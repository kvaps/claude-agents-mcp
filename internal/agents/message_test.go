package agents

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

var (
	testSender    = Self{Short: "a1b2c3d4", SessionID: "a1b2c3d4-1111-2222-3333-444455556666", Name: "orchestrator", Live: true, Addressable: true}
	testRecipient = Session{Short: "b2c3d4e5", SessionID: "b2c3d4e5-1111-2222-3333-444455556666", Name: "reviewer", Live: true}
	testAt        = time.Date(2026, 8, 17, 21, 40, 3, 0, time.UTC)
)

// TestEnvelope pins what the recipient actually reads. Every part of it is there
// to answer a question the recipient cannot answer any other way, so each is
// asserted: who wrote this, who is it addressed to, when, and how to reply.
func TestEnvelope(t *testing.T) {
	got := envelope("m-abc123", testSender, testRecipient, "check whether the tests pass", testAt)
	for _, want := range []string{
		`<agent-message id="m-abc123" from="orchestrator [a1b2c3d4]" to="reviewer [b2c3d4e5]" at="2026-08-17T21:40:03Z">`,
		"check whether the tests pass",
		"</agent-message>",
		"message from another agent, not from your user",
		"nobody is blocked on it",
		`send_message`,
		`to:"a1b2c3d4"`, // the sender's short id, not its name
	} {
		if !strings.Contains(got, want) {
			t.Errorf("envelope is missing %q:\n%s", want, got)
		}
	}
}

// TestEnvelopeUnaddressableSender: a server started outside a background session
// has no address, so the envelope must say the sender cannot be replied to
// rather than pointing the recipient at a send_message that would fail.
func TestEnvelopeUnaddressableSender(t *testing.T) {
	got := envelope("m-abc123", Self{Name: "desktop"}, testRecipient, "fyi", testAt)
	if strings.Contains(got, "To answer, call") {
		t.Errorf("envelope invites a reply to a sender with no address:\n%s", got)
	}
	if !strings.Contains(got, "cannot receive a reply") {
		t.Errorf("envelope does not say the sender is unreachable:\n%s", got)
	}
}

// TestEnvelopeFromClient: a message relayed by an MCP client says so, names the
// mailbox to answer to, and still says it is not the recipient's user.
func TestEnvelopeFromClient(t *testing.T) {
	got := envelope("m-abc123", Self{Kind: KindClient, Mailbox: "codex", Addressable: true}, testRecipient, "status?", testAt)
	for _, want := range []string{`from="codex"`, "an MCP client (codex)", "not from your user", "untrusted", `to:"codex"`} {
		if !strings.Contains(got, want) {
			t.Errorf("client envelope is missing %q:\n%s", want, got)
		}
	}
}

// TestEnvelopeQuotesInNames: a display name is free text and can contain the
// quote character that delimits the envelope's attributes. It must not break the
// tag it sits in.
func TestEnvelopeQuotesInNames(t *testing.T) {
	sender := Self{Short: "a1b2c3d4", Name: `the "boss"`, Addressable: true}
	got := envelope("m-abc123", sender, testRecipient, "hi", testAt)
	head := strings.SplitN(got, "\n", 2)[0]
	if strings.Count(head, `"`) != 8 { // four attributes, two quotes each
		t.Errorf("attribute quoting broken by a quoted name:\n%s", head)
	}
	if !strings.Contains(head, "the 'boss'") {
		t.Errorf("quoted name not rendered safely:\n%s", head)
	}
}

// TestEnvelopeConfirmationIsPerMessage guards the mechanical property the
// delivery check rests on. SubmitPrompt confirms a delivery by finding the
// body's longest line in the recipient's transcript; for a short message that
// line is the envelope's own boilerplate, which is identical in every message
// ever sent. Leading it with the message id keeps the needle unique, so two
// agents writing to the same session at the same moment cannot confirm each
// other's deliveries.
func TestEnvelopeConfirmationIsPerMessage(t *testing.T) {
	for _, body := range []string{"ok", "please rerun the failing test and tell me what it says"} {
		t.Run(body[:2], func(t *testing.T) {
			first := promptFragment(envelope("m-aaaaaa", testSender, testRecipient, body, testAt))
			second := promptFragment(envelope("m-bbbbbb", testSender, testRecipient, body, testAt))
			if first == second {
				t.Fatalf("two distinct messages share a confirmation needle: %q", first)
			}
		})
	}
}

// TestNewMessageIDUnique: ids are what both ends refer to a message by, and what
// separates one delivery from another in a transcript.
func TestNewMessageIDUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := newMessageID()
		if !strings.HasPrefix(id, "m-") || len(id) != 14 {
			t.Fatalf("message id %q is not of the form m-xxxxxxxxxxxx", id)
		}
		if seen[id] {
			t.Fatalf("duplicate message id %q", id)
		}
		seen[id] = true
	}
}

func TestIsSelf(t *testing.T) {
	cases := []struct {
		name   string
		self   Self
		target Recipient
		want   bool
	}{
		{"same short", testSender, Recipient{Session: Session{Short: "a1b2c3d4"}}, true},
		{"same session id, no short yet", testSender, Recipient{Session: Session{SessionID: testSender.SessionID}}, true},
		{"another session", testSender, Recipient{Session: testRecipient}, false},
		{"unidentified caller never matches", Self{}, Recipient{Session: Session{Short: "", SessionID: ""}}, false},
		{"client writing to its own mailbox", Self{Kind: KindClient, Mailbox: "codex"}, Recipient{Mailbox: "codex"}, true},
		{"client writing to another mailbox", Self{Kind: KindClient, Mailbox: "codex"}, Recipient{Mailbox: "voice"}, false},
		{"session writing to a mailbox", testSender, Recipient{Mailbox: "codex"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isSelf(c.self, c.target); got != c.want {
				t.Fatalf("isSelf = %v, want %v", got, c.want)
			}
		})
	}
}

// TestSendMessageEmptyBody: an empty message is refused before anything is
// resolved or delivered, so this needs no daemon.
func TestSendMessageEmptyBody(t *testing.T) {
	for _, body := range []string{"", "   ", "\n\t "} {
		if _, err := NewClient().SendMessage("whoever", body, MessageOptions{}); err == nil {
			t.Fatalf("SendMessage with body %q = nil error, want a refusal", body)
		}
	}
}

// TestSendMessageIntegration drives the real daemon: it sends a message to a
// live session and asserts the envelope actually landed in that session's
// conversation — the sender's identity, the recipient's own name, and the
// message id all present in the transcript, not merely acknowledged by the
// daemon. It is gated behind MESSAGE_IT_TARGET (the short id, name or session id
// of a session that may receive a test message) so `go test` stays hermetic, and
// the message it sends says plainly that it is a test and asks for nothing, so a
// real agent reading it does no work.
func TestSendMessageIntegration(t *testing.T) {
	ref := os.Getenv("MESSAGE_IT_TARGET")
	if ref == "" {
		t.Skip("set MESSAGE_IT_TARGET=<session ref that may receive a test message> to run the live messaging test")
	}
	c := NewClient()
	target, err := c.ResolveTarget(ref)
	if err != nil {
		t.Fatalf("resolve %q: %v", ref, err)
	}
	mark := markTranscript(target.Session.SessionID)

	body := fmt.Sprintf("This is an automated delivery test from claude-agents-mcp (%s). No action is needed and no answer is expected — ignore it and carry on.", time.Now().Format(time.RFC3339))
	out, err := c.SendMessage(ref, body, MessageOptions{Resume: true, Dialog: DialogKeep})
	if err != nil {
		t.Fatalf("SendMessage(%q): %v", ref, err)
	}
	t.Logf("message %s from %s (addressable=%v) to %s: %s", out.ID, out.From.Label(), out.From.Addressable, out.To.Label(), out.Delivery)

	if out.From.Known() && isSelf(out.From, out.To) {
		t.Errorf("message was delivered to the sender itself (%s)", out.To.Label())
	}
	if !strings.Contains(out.Body, out.ID) {
		t.Errorf("envelope does not carry its own message id %s:\n%s", out.ID, out.Body)
	}

	// Ground truth: the envelope has to appear as a user record in the
	// recipient's transcript. A daemon ack alone is not delivery.
	deadline := time.Now().Add(10 * time.Second)
	for !mark.Landed(out.Body) {
		if time.Now().After(deadline) {
			t.Fatalf("message %s reported as %q but never appeared in %s's transcript", out.ID, out.Delivery, out.To.Label())
		}
		time.Sleep(250 * time.Millisecond)
	}
}
