package agents

import (
	"errors"
	"strings"
	"testing"
)

// fakeFleet installs a fixed address book on a client so messaging can be
// exercised without a daemon, and gives the test a session identity that is
// part of that fleet.
func fakeFleet(t *testing.T, sessions []Session, selfShort string) *Client {
	t.Helper()
	isolateIdentity(t)
	if selfShort != "" {
		t.Setenv(envJobDir, "/Users/x/.claude/jobs/"+selfShort)
		for _, s := range sessions {
			if s.Short == selfShort {
				t.Setenv(envSessionID, s.SessionID)
			}
		}
	}
	return &Client{listFn: func() ([]Session, error) { return sessions, nil }}
}

func createMailbox(t *testing.T, name string) *Mailbox {
	t.Helper()
	mb, err := OpenMailbox(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := mb.Create(); err != nil {
		t.Fatal(err)
	}
	return mb
}

// TestSendMessageToMailbox is the agent → client half of the round trip: a
// session sends to a mailbox, the message is stored with the sender's label and
// return address, and its status follows the reader through queued, delivered
// and read.
func TestSendMessageToMailbox(t *testing.T) {
	c := fakeFleet(t, fleet, "a1b2c3d4")
	mb := createMailbox(t, "codex")

	out, err := c.SendMessage("codex", "  the tests pass  ", MessageOptions{})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if out.Status != StatusQueued || !out.To.IsMailbox() || out.To.Mailbox != "codex" || out.Duplicate {
		t.Fatalf("outcome = %+v", out)
	}
	if out.From.Label() != "orchestrator [a1b2c3d4]" || !out.From.Addressable {
		t.Fatalf("sender = %+v", out.From)
	}

	res, err := mb.ReadAfter(0, 0)
	if err != nil || len(res.Messages) != 1 {
		t.Fatalf("mailbox holds %d messages (%v)", len(res.Messages), err)
	}
	msg := res.Messages[0]
	if msg.ID != out.ID || msg.Text != "the tests pass" || msg.From != "orchestrator [a1b2c3d4]" || msg.ReplyTo != "a1b2c3d4" || !msg.Untrusted || msg.To != "codex" {
		t.Fatalf("stored message = %+v", msg)
	}

	status := func() string {
		rec, found, err := c.MessageStatus(out.ID)
		if err != nil || !found {
			t.Fatalf("MessageStatus(%s): found=%v err=%v", out.ID, found, err)
		}
		if rec.ToKind != RecipientMailbox || rec.ToAddress != "codex" || rec.From != "orchestrator [a1b2c3d4]" {
			t.Fatalf("record = %+v", rec)
		}
		return rec.Status
	}
	if got := status(); got != StatusQueued {
		t.Fatalf("status before fetch = %s", got)
	}
	if _, err := mb.Fetch(0); err != nil {
		t.Fatal(err)
	}
	if got := status(); got != StatusDelivered {
		t.Fatalf("status after fetch = %s", got)
	}
	if _, err := mb.Ack(msg.Seq); err != nil {
		t.Fatal(err)
	}
	if got := status(); got != StatusRead {
		t.Fatalf("status after ack = %s", got)
	}
}

// TestSendMessageFromClient is the client → ... half as far as it goes without
// a daemon: a client's messages carry its mailbox as sender and return address,
// so a session that receives one knows where to answer.
func TestSendMessageFromClient(t *testing.T) {
	c := fakeFleet(t, fleet, "")
	t.Setenv(envMailbox, "codex")
	createMailbox(t, "codex")
	createMailbox(t, "voice")

	out, err := c.SendMessage("voice", "ping", MessageOptions{})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if out.From.Kind != KindClient || out.From.Label() != "codex" {
		t.Fatalf("sender = %+v", out.From)
	}
	voice, _ := OpenMailbox("voice")
	res, _ := voice.ReadAfter(0, 0)
	if len(res.Messages) != 1 || res.Messages[0].From != "codex" || res.Messages[0].ReplyTo != "codex" {
		t.Fatalf("stored = %+v", res.Messages)
	}
}

// TestSendMessageIdempotencyKey: the same key from the same sender is one
// message, whatever the body says the second time; a different sender's same
// key is unrelated.
func TestSendMessageIdempotencyKey(t *testing.T) {
	c := fakeFleet(t, fleet, "a1b2c3d4")
	mb := createMailbox(t, "codex")

	first, err := c.SendMessage("codex", "once", MessageOptions{Key: "req-1"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.SendMessage("codex", "twice", MessageOptions{Key: "req-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Duplicate || second.ID != first.ID || second.Status != StatusQueued || second.To.Mailbox != "codex" {
		t.Fatalf("retry = %+v, want a duplicate of %s", second, first.ID)
	}
	st, _ := mb.Status()
	if st.Total != 1 {
		t.Fatalf("mailbox holds %d messages after a retried send", st.Total)
	}

	// Another sender, same key: its own message.
	t.Setenv(envJobDir, "/Users/x/.claude/jobs/b2c3d4e5")
	t.Setenv(envSessionID, "b2c3d4e5-1111-2222-3333-444455556666")
	third, err := c.SendMessage("codex", "from the reviewer", MessageOptions{Key: "req-1"})
	if err != nil {
		t.Fatal(err)
	}
	if third.Duplicate || third.ID == first.ID {
		t.Fatalf("another sender's key collided: %+v", third)
	}
	if _, err := c.SendMessage("codex", "x", MessageOptions{Key: "has spaces"}); err == nil {
		t.Fatal("an invalid idempotency key was accepted")
	}
}

// TestSendMessageRefusals: the refusals that need no daemon — self-addressed,
// unknown, ambiguous — are reported before anything is stored.
func TestSendMessageRefusals(t *testing.T) {
	sessions := append([]Session{{Short: "11112222", SessionID: "11112222-aaaa-bbbb-cccc-dddddddddddd", Name: "codex", Live: true}}, fleet...)
	c := fakeFleet(t, sessions, "")
	t.Setenv(envMailbox, "codex")
	mb := createMailbox(t, "codex")
	createMailbox(t, "voice")

	// A client naming its own mailbox is ambiguous here (a session is also called
	// codex), so address the mailbox through the ambiguity check first.
	_, err := c.SendMessage("codex", "hi", MessageOptions{})
	var ambiguous *AmbiguousRefError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("mailbox named like a session: err = %v, want ambiguity", err)
	}
	if _, err := c.SendMessage("nobody", "hi", MessageOptions{}); err == nil || !strings.Contains(err.Error(), "no session or mailbox") {
		t.Fatalf("unknown recipient: err = %v", err)
	}
	if _, err := c.SendMessage("voice", "   ", MessageOptions{}); err == nil {
		t.Fatal("empty body accepted")
	}
	if st, _ := mb.Status(); st.Total != 0 {
		t.Fatalf("a refused send stored something: %+v", st)
	}

	// Self-addressed: a client whose mailbox is unambiguous.
	t.Setenv(envMailbox, "voice")
	if _, err := c.SendMessage("voice", "hi", MessageOptions{}); err == nil || !strings.Contains(err.Error(), "itself") {
		t.Fatalf("self-addressed: err = %v", err)
	}
}

// TestSendMessageToSessionUnaddressedSender: a client without a mailbox can
// still write (to a mailbox here, since a session needs the daemon), but the
// stored message carries no return address rather than a made-up one.
func TestSendMessageUnidentifiedSender(t *testing.T) {
	c := fakeFleet(t, fleet, "")
	createMailbox(t, "codex")
	out, err := c.SendMessage("codex", "anonymous note", MessageOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if out.From.Known() || out.From.Addressable {
		t.Fatalf("sender = %+v, want unidentified", out.From)
	}
	mb, _ := OpenMailbox("codex")
	res, _ := mb.ReadAfter(0, 0)
	if len(res.Messages) != 1 || res.Messages[0].From != "unknown" || res.Messages[0].ReplyTo != "" {
		t.Fatalf("stored = %+v", res.Messages)
	}
}

func TestMessageStatusUnknownID(t *testing.T) {
	isolateIdentity(t)
	if _, found, err := NewClient().MessageStatus("m-nothing"); err != nil || found {
		t.Fatalf("MessageStatus(unknown) = found=%v err=%v", found, err)
	}
}
