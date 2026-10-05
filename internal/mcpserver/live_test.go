package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kvaps/claude-agents-mcp/internal/agents"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// The live tests run the built binary over stdio against the real daemon and a
// disposable background session the operator of the test created for them.
// They are gated behind two variables so `go test ./...` stays hermetic:
//
//	MAILBOX_IT_BINARY  path to a freshly built claude-agents-mcp
//	MAILBOX_IT_TARGET  short id of a session that may receive test traffic
//
// The target session must have been told to answer an <agent-message> with
// `pong <id>` via send_message to the envelope's reply address, and it must be
// running a build of this server that knows about mailboxes.
const liveMailbox = "codex-it"

func liveEnv(t *testing.T) (bin, target string) {
	t.Helper()
	bin, target = os.Getenv("MAILBOX_IT_BINARY"), os.Getenv("MAILBOX_IT_TARGET")
	if bin == "" || target == "" {
		t.Skip("set MAILBOX_IT_BINARY and MAILBOX_IT_TARGET to run the live stdio tests")
	}
	return bin, target
}

// stdioClient starts the binary as a client-identity process (the way Codex
// would, with CLAUDE_AGENTS_MAILBOX in its environment) and returns a call
// helper. Session variables are blanked so the test's own session identity does
// not leak into the child.
func stdioClient(t *testing.T, bin string) func(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	t.Helper()
	c, err := client.NewStdioMCPClient(bin, []string{
		"CLAUDE_AGENTS_MAILBOX=" + liveMailbox,
		"CLAUDE_CODE_SESSION_ID=",
		"CLAUDE_JOB_DIR=",
	})
	if err != nil {
		t.Fatalf("start %s: %v", bin, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := c.Initialize(ctx, mcp.InitializeRequest{}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return func(ctx context.Context, name string, args map[string]any) (string, bool, error) {
		req := mcp.CallToolRequest{}
		req.Params.Name = name
		req.Params.Arguments = args
		res, err := c.CallTool(ctx, req)
		if err != nil {
			return "", false, err
		}
		var sb strings.Builder
		for _, content := range res.Content {
			if tc, ok := content.(mcp.TextContent); ok {
				sb.WriteString(tc.Text)
			}
		}
		return sb.String(), res.IsError, nil
	}
}

// TestLiveSmoke runs the pre-existing scenarios the installed binary is relied
// on for — list_sessions, get_session, set_permission_mode, submit_prompt —
// against the new build, so an upgrade of the installed binary is known not to
// regress them before it happens.
func TestLiveSmoke(t *testing.T) {
	bin, target := liveEnv(t)
	call := stdioClient(t, bin)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	raw, isErr, err := call(ctx, "list_sessions", map[string]any{"live_only": true})
	if err != nil || isErr {
		t.Fatalf("list_sessions: %v / %s", err, raw)
	}
	var sessions []agents.Session
	if err := json.Unmarshal([]byte(raw), &sessions); err != nil {
		t.Fatalf("list_sessions is not JSON: %v", err)
	}
	found := false
	for _, s := range sessions {
		if s.Short == target {
			found = true
		}
	}
	if !found {
		t.Fatalf("target %s not among %d live sessions", target, len(sessions))
	}
	t.Logf("list_sessions: %d live sessions, target %s present", len(sessions), target)

	raw, isErr, err = call(ctx, "get_session", map[string]any{"session": target})
	if err != nil || isErr || !strings.Contains(raw, target) {
		t.Fatalf("get_session: %v / %s", err, raw)
	}

	// Modes reachable in place from any worker. bypassPermissions is left out on
	// purpose: switching away from it rewrites the saved launch flags, after
	// which the tool treats the worker as not bypass-capable and demands a
	// restart — the restart path is exercised separately when the session is
	// respawned onto the installed binary.
	waitIdle(t, ctx, call, target)
	for _, mode := range []string{"auto", "plan", "acceptEdits", "auto"} {
		raw, isErr, err = call(ctx, "set_permission_mode", map[string]any{"session": target, "mode": mode})
		switched := strings.Contains(raw, "switched in place") || strings.Contains(raw, "was already in")
		if err != nil || isErr || !switched {
			t.Fatalf("set_permission_mode %s: %v / %s", mode, err, raw)
		}
		t.Logf("set_permission_mode %s: %s", mode, raw)
	}

	raw, isErr, err = call(ctx, "submit_prompt", map[string]any{"session": target, "text": "Smoke test from claude-agents-mcp: reply with the single word ok and keep waiting for messages."})
	if err != nil || isErr || !strings.Contains(raw, "submitted to "+target) {
		t.Fatalf("submit_prompt: %v / %s", err, raw)
	}
	t.Logf("submit_prompt: %s", raw)
	waitIdle(t, ctx, call, target)
}

// waitIdle blocks until the target session has produced no output for a while,
// so the next delivery lands at its prompt rather than queueing. It goes by the
// daemon-observed tempo, not by state: state is what the agent reports about
// itself ("working", "waiting for …") and an idle agent may well say it is
// working on waiting.
func waitIdle(t *testing.T, ctx context.Context, call func(context.Context, string, map[string]any) (string, bool, error), target string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		raw, _, err := call(ctx, "get_session", map[string]any{"session": target})
		var s agents.Session
		if err == nil && json.Unmarshal([]byte(raw), &s) == nil && s.Live && s.Tempo != "active" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("session %s never went idle: %s", target, raw)
		}
		time.Sleep(time.Second)
	}
}

// TestLiveRoundTrip is the two-way exchange over the real transport: the
// client-identity binary writes to the session, the session's own server (the
// installed binary) writes back to the client's mailbox, and a long-poll in the
// client picks the answer up. Along the way it checks that a blocked long-poll
// does not stall other tool calls on the same stdio connection.
func TestLiveRoundTrip(t *testing.T) {
	bin, target := liveEnv(t)
	call := stdioClient(t, bin)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	raw, isErr, err := call(ctx, "whoami", nil)
	var self agents.Self
	if err != nil || isErr || json.Unmarshal([]byte(raw), &self) != nil || self.Kind != agents.KindClient || self.Mailbox != liveMailbox || !self.Addressable {
		t.Fatalf("whoami: %v / %s", err, raw)
	}

	// Concurrency on one stdio connection: a long-poll in flight must not delay
	// an unrelated call.
	var wg sync.WaitGroup
	wg.Add(1)
	var waitElapsed time.Duration
	go func() {
		defer wg.Done()
		start := time.Now()
		_, _, _ = call(ctx, "wait_for_messages", map[string]any{"timeout_seconds": 6})
		waitElapsed = time.Since(start)
	}()
	time.Sleep(500 * time.Millisecond)
	start := time.Now()
	if raw, isErr, err := call(ctx, "list_mailboxes", nil); err != nil || isErr || !strings.Contains(raw, liveMailbox) {
		t.Fatalf("list_mailboxes during a wait: %v / %s", err, raw)
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("list_mailboxes took %s while a wait_for_messages was in flight — stdio is serialising tool calls", el)
	}
	wg.Wait()
	if waitElapsed < 5*time.Second {
		t.Fatalf("the concurrent wait returned after %s, before its timeout", waitElapsed)
	}
	t.Logf("concurrency: list_mailboxes answered in %s while wait_for_messages ran for %s", time.Since(start), waitElapsed)

	// Drain anything a previous run left in the mailbox so the pong we wait for
	// is ours.
	if _, _, err := call(ctx, "read_messages", map[string]any{"limit": 500}); err != nil {
		t.Fatal(err)
	}

	waitIdle(t, ctx, call, target)
	key := fmt.Sprintf("it-%d", time.Now().UnixNano())
	raw, isErr, err = call(ctx, "send_message", map[string]any{
		"to":              target,
		"message":         "ping from the claude-agents-mcp live test. Answer exactly as your instructions say: send_message to the reply address with `pong <id>`.",
		"idempotency_key": key,
	})
	var sent sendResult
	if err != nil || isErr || json.Unmarshal([]byte(raw), &sent) != nil {
		t.Fatalf("send_message: %v / %s", err, raw)
	}
	if sent.ToKind != agents.RecipientSession || sent.ToAddress != target || (sent.Status != agents.StatusDelivered && sent.Status != agents.StatusQueued) || sent.From != liveMailbox {
		t.Fatalf("send_message result: %s", raw)
	}
	t.Logf("sent %s to %s: status=%s delivery=%q", sent.ID, sent.To, sent.Status, sent.Delivery)

	// The retry is not a second delivery.
	raw, _, _ = call(ctx, "send_message", map[string]any{"to": target, "message": "ping again", "idempotency_key": key})
	var dup sendResult
	if json.Unmarshal([]byte(raw), &dup) != nil || !dup.Duplicate || dup.ID != sent.ID {
		t.Fatalf("retry with the same key: %s", raw)
	}

	// Long-poll for the answer; the agent needs a turn to read and reply.
	var pong *agents.Message
	deadline := time.Now().Add(4 * time.Minute)
	for pong == nil {
		if time.Now().After(deadline) {
			t.Fatalf("no pong for %s within 4 minutes", sent.ID)
		}
		raw, isErr, err = call(ctx, "wait_for_messages", map[string]any{"timeout_seconds": 60})
		var got readResult
		if err != nil || isErr || json.Unmarshal([]byte(raw), &got) != nil {
			t.Fatalf("wait_for_messages: %v / %s", err, raw)
		}
		for i := range got.Messages {
			m := got.Messages[i]
			t.Logf("received %s from %s (reply_to=%s): %q", m.ID, m.From, m.ReplyTo, m.Text)
			if strings.Contains(m.Text, sent.ID) {
				pong = &m
			}
		}
		if pong == nil && len(got.Messages) == 0 {
			t.Logf("wait timed out (%v), listening again", got.TimedOut)
		}
		if pong != nil {
			if _, isErr, err := call(ctx, "ack_messages", map[string]any{"cursor": got.Cursor}); err != nil || isErr {
				t.Fatalf("ack_messages: %v", err)
			}
		}
	}
	if !pong.Untrusted || pong.ReplyTo != target || !strings.Contains(pong.From, target) {
		t.Fatalf("pong metadata: %+v", pong)
	}

	raw, _, _ = call(ctx, "message_status", map[string]any{"id": sent.ID})
	var rec agents.SentRecord
	if json.Unmarshal([]byte(raw), &rec) != nil || rec.Status != agents.StatusDelivered {
		t.Fatalf("message_status of our ping: %s", raw)
	}
	raw, _, _ = call(ctx, "message_status", map[string]any{"id": pong.ID})
	if json.Unmarshal([]byte(raw), &rec) != nil || rec.Status != agents.StatusRead || rec.ToKind != agents.RecipientMailbox {
		t.Fatalf("message_status of the pong: %s", raw)
	}
	t.Logf("round trip complete: ping %s delivered to %s, pong %s read in mailbox %s", sent.ID, target, pong.ID, liveMailbox)
}
