package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kvaps/claude-agents-mcp/internal/agents"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// The identities the round trip runs under. A real deployment has them in two
// different processes (Codex's server and a session's server) sharing the state
// directory; here one test process switches its environment between them, which
// is the same thing as far as the store is concerned, because identity is read
// from the environment on every call.
const (
	orchShort = "a1b2c3d4"
	orchSID   = "a1b2c3d4-1111-2222-3333-444455556666"
)

var testFleet = []agents.Session{
	{Short: orchShort, SessionID: orchSID, Name: "orchestrator", Live: true},
	{Short: "b2c3d4e5", SessionID: "b2c3d4e5-1111-2222-3333-444455556666", Name: "reviewer", Live: true},
}

// newServer builds the MCP server over a fake fleet and connects an in-process
// client to it, returning a call helper that fails the test on protocol errors
// and returns the tool's text payload plus whether the tool reported an error.
func newServer(t *testing.T) func(name string, args map[string]any) (string, bool) {
	t.Helper()
	a := agents.NewTestClient(func() ([]agents.Session, error) { return testFleet, nil })
	srv := New("test", a)
	c, err := client.NewInProcessClient(srv)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Initialize(ctx, mcp.InitializeRequest{}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return func(name string, args map[string]any) (string, bool) {
		t.Helper()
		req := mcp.CallToolRequest{}
		req.Params.Name = name
		req.Params.Arguments = args
		res, err := c.CallTool(ctx, req)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var sb strings.Builder
		for _, content := range res.Content {
			if tc, ok := content.(mcp.TextContent); ok {
				sb.WriteString(tc.Text)
			}
		}
		return sb.String(), res.IsError
	}
}

func asClient(t *testing.T, mailbox string) {
	t.Helper()
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("CLAUDE_JOB_DIR", "")
	t.Setenv("CLAUDE_AGENTS_MAILBOX", mailbox)
}

func asSession(t *testing.T, short, sid string) {
	t.Helper()
	t.Setenv("CLAUDE_AGENTS_MAILBOX", "")
	t.Setenv("CLAUDE_JOB_DIR", "/Users/x/.claude/jobs/"+short)
	t.Setenv("CLAUDE_CODE_SESSION_ID", sid)
}

func decode(t *testing.T, raw string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(raw), v); err != nil {
		t.Fatalf("result is not JSON: %v\n%s", err, raw)
	}
}

// TestMailboxRoundTripOverMCP drives the agent → client half of the exchange
// through the tools themselves: a client comes up and finds its mailbox empty,
// a session sends to it (and retries, idempotently), the client reads and
// acknowledges, and the session sees the status move to read. The client →
// session half needs a live daemon and is exercised by the live test.
func TestMailboxRoundTripOverMCP(t *testing.T) {
	t.Setenv("CLAUDE_AGENTS_MCP_STATE_DIR", t.TempDir())

	// The client's server process starts: its mailbox exists from that moment.
	asClient(t, "codex")
	call := newServer(t)
	raw, isErr := call("whoami", nil)
	var self agents.Self
	decode(t, raw, &self)
	if isErr || self.Kind != agents.KindClient || self.Mailbox != "codex" || !self.Addressable {
		t.Fatalf("client whoami = %s (err=%v)", raw, isErr)
	}

	// Nothing there yet: a short wait times out honestly.
	start := time.Now()
	raw, isErr = call("wait_for_messages", map[string]any{"timeout_seconds": 0.4})
	var waited readResult
	decode(t, raw, &waited)
	if isErr || !waited.TimedOut || len(waited.Messages) != 0 {
		t.Fatalf("quiet wait = %s", raw)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("quiet wait overran: %s", time.Since(start))
	}

	// A session writes to the client — twice with the same key, which is once.
	asSession(t, orchShort, orchSID)
	raw, isErr = call("send_message", map[string]any{"to": "codex", "message": "the build is green", "idempotency_key": "build-1"})
	var sent sendResult
	decode(t, raw, &sent)
	if isErr || sent.Status != agents.StatusQueued || sent.ToKind != agents.RecipientMailbox || sent.From != "orchestrator [a1b2c3d4]" || sent.Duplicate {
		t.Fatalf("send = %s", raw)
	}
	raw, _ = call("send_message", map[string]any{"to": "codex", "message": "the build is green", "idempotency_key": "build-1"})
	var again sendResult
	decode(t, raw, &again)
	if !again.Duplicate || again.ID != sent.ID {
		t.Fatalf("retry = %s, want duplicate of %s", raw, sent.ID)
	}
	raw, isErr = call("list_mailboxes", nil)
	var boxes []agents.MailboxStatus
	decode(t, raw, &boxes)
	if isErr || len(boxes) != 1 || boxes[0].Name != "codex" || boxes[0].Total != 1 || boxes[0].Queued != 1 {
		t.Fatalf("list_mailboxes = %s", raw)
	}

	// The client reads: one message, marked untrusted, with a return address.
	asClient(t, "codex")
	raw, isErr = call("read_messages", nil)
	var got readResult
	decode(t, raw, &got)
	if isErr || len(got.Messages) != 1 {
		t.Fatalf("read = %s", raw)
	}
	msg := got.Messages[0]
	if msg.ID != sent.ID || msg.Text != "the build is green" || !msg.Untrusted || msg.ReplyTo != orchShort || msg.From != "orchestrator [a1b2c3d4]" {
		t.Fatalf("message = %+v", msg)
	}
	if !strings.Contains(got.Note, "untrusted") || !strings.Contains(got.Note, got.Cursor) {
		t.Fatalf("note does not warn or name the cursor: %q", got.Note)
	}
	// A second cursor-less read returns nothing: the message was handed out once.
	raw, _ = call("read_messages", nil)
	var empty readResult
	decode(t, raw, &empty)
	if len(empty.Messages) != 0 {
		t.Fatalf("message handed out twice: %s", raw)
	}
	// An explicit cursor replays it.
	raw, _ = call("read_messages", map[string]any{"cursor": "0"})
	var replay readResult
	decode(t, raw, &replay)
	if len(replay.Messages) != 1 || replay.Messages[0].ID != sent.ID {
		t.Fatalf("replay = %s", raw)
	}

	// The sender sees delivered, then read once the client acknowledges.
	asSession(t, orchShort, orchSID)
	raw, _ = call("message_status", map[string]any{"id": sent.ID})
	var rec agents.SentRecord
	decode(t, raw, &rec)
	if rec.Status != agents.StatusDelivered {
		t.Fatalf("status after fetch = %s", raw)
	}
	asClient(t, "codex")
	raw, isErr = call("ack_messages", map[string]any{"cursor": got.Cursor})
	var st agents.MailboxStatus
	decode(t, raw, &st)
	if isErr || st.Unread != 0 || st.ReadSeq != 1 {
		t.Fatalf("ack = %s", raw)
	}
	asSession(t, orchShort, orchSID)
	raw, _ = call("message_status", map[string]any{"id": sent.ID})
	decode(t, raw, &rec)
	if rec.Status != agents.StatusRead {
		t.Fatalf("status after ack = %s", raw)
	}

	// A session has no mailbox to read, and says so instead of inventing one.
	if raw, isErr := call("read_messages", nil); !isErr || !strings.Contains(raw, "conversation") {
		t.Fatalf("session read_messages = %s (err=%v)", raw, isErr)
	}
	if _, isErr := call("message_status", map[string]any{"id": "m-nothing"}); !isErr {
		t.Fatal("unknown message id did not error")
	}
}

// TestWaitReturnsOnArrivalOverMCP: a client parked in wait_for_messages is
// released by a send from a session, with the message, well before the timeout.
func TestWaitReturnsOnArrivalOverMCP(t *testing.T) {
	t.Setenv("CLAUDE_AGENTS_MCP_STATE_DIR", t.TempDir())
	asClient(t, "codex")
	call := newServer(t)
	call("whoami", nil) // creates the mailbox via the server's startup hook

	// The sender runs in its own process in reality; here a goroutine with a
	// second client, writing straight to the store the way its tool would.
	go func() {
		time.Sleep(300 * time.Millisecond)
		mb, _ := agents.OpenMailbox("codex")
		_, _, _ = mb.Append(agents.Message{ID: "m-arrival", From: "reviewer [b2c3d4e5]", ReplyTo: "b2c3d4e5", Text: "done"})
	}()
	start := time.Now()
	raw, isErr := call("wait_for_messages", map[string]any{"timeout_seconds": 10})
	var got readResult
	decode(t, raw, &got)
	if isErr || got.TimedOut || len(got.Messages) != 1 || got.Messages[0].ID != "m-arrival" || !got.Messages[0].Untrusted {
		t.Fatalf("wait = %s", raw)
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("wait took %s to notice an arrival", el)
	}
}

// TestRegisterMailboxOverMCP: a client with no configured mailbox is told how to
// get one, registers, and is addressable from then on.
func TestRegisterMailboxOverMCP(t *testing.T) {
	t.Setenv("CLAUDE_AGENTS_MCP_STATE_DIR", t.TempDir())
	asClient(t, "")
	call := newServer(t)
	if raw, isErr := call("read_messages", nil); !isErr || !strings.Contains(raw, "register_mailbox") {
		t.Fatalf("unconfigured client read_messages = %s (err=%v)", raw, isErr)
	}
	raw, isErr := call("register_mailbox", map[string]any{"name": "Codex"})
	var self agents.Self
	decode(t, raw, &self)
	if isErr || self.Mailbox != "codex" || !self.Addressable {
		t.Fatalf("register = %s", raw)
	}
	if raw, isErr := call("register_mailbox", map[string]any{"name": "deadbeef"}); !isErr || !strings.Contains(raw, "short id") {
		t.Fatalf("hex name accepted: %s", raw)
	}
}

// TestCreateSessionTrustNeedsTheOperator: trust_workspace is refused, before
// anything is launched, unless the operator's trust roots cover the folder.
func TestCreateSessionTrustNeedsTheOperator(t *testing.T) {
	t.Setenv("CLAUDE_AGENTS_MCP_STATE_DIR", t.TempDir())
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("PATH", "") // a launch attempt would fail differently: no claude CLI
	asClient(t, "codex")
	call := newServer(t)

	t.Setenv("CLAUDE_AGENTS_TRUST_ROOTS", "")
	raw, isErr := call("create_session", map[string]any{"cwd": dir, "trust_workspace": true})
	if !isErr || !strings.Contains(raw, "CLAUDE_AGENTS_TRUST_ROOTS") || strings.Contains(raw, "claude CLI") {
		t.Fatalf("without trust roots: %s (err=%v)", raw, isErr)
	}
	t.Setenv("CLAUDE_AGENTS_TRUST_ROOTS", t.TempDir())
	raw, isErr = call("create_session", map[string]any{"cwd": dir, "trust_workspace": true})
	if !isErr || !strings.Contains(raw, "not inside") {
		t.Fatalf("outside the trust roots: %s (err=%v)", raw, isErr)
	}
}
