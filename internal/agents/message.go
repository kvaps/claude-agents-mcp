package agents

import (
	"fmt"
	"strings"
	"time"
)

// MessageOptions tunes one send: whether a sleeping recipient may be woken, and
// what to do about the resume dialog if it is.
type MessageOptions struct {
	// Resume allows a not-running-but-resumable recipient to be resumed in place
	// before the message is delivered (the default, mirroring submit_prompt).
	// With it off, a sleeping recipient is reported as undelivered rather than
	// woken — a fleet of sleeping agents should not be started by a broadcast.
	Resume bool
	// Dialog decides how a resumed recipient's resume dialog is answered.
	Dialog ResumeDialogChoice
}

// MessageOutcome describes a delivered message: who it was from, who it reached,
// how the delivery was confirmed, and what had to happen to the recipient first.
type MessageOutcome struct {
	ID         string // the message id both ends see, for referring to it later
	From       Self
	To         Session
	Delivery   string // one of the confirmed* strings from submit.go
	ResumeNote string // non-empty when the recipient had to be resumed
	Body       string // the envelope as delivered
}

// SendMessage delivers a message from the calling session to another session.
//
// It is addressing and identification on top of the existing delivery path, not
// a second channel: the envelope is submitted with SubmitPrompt, so it travels
// over the daemon's native op:reply exactly like a prompt does (falling back to
// the PTY the same way) and inherits its guarantee — the delivery is verified
// against the recipient's transcript, and a recipient that is mid-turn queues
// the message instead of losing it.
//
// What it adds over submit_prompt is that the recipient can tell who is talking
// and how to answer. A prompt arrives indistinguishable from something the user
// typed; a message arrives wrapped in an envelope naming the sender and its
// address (see envelope). The sender is not a parameter — it is read from the
// environment Claude Code gave this server process, so an agent cannot claim to
// be another one.
//
// A recipient that is not running is resumed in place first (opts.Resume),
// keeping its full history, the same way typing into an exited session in the
// app brings it back.
func (c *Client) SendMessage(to, text string, opts MessageOptions) (MessageOutcome, error) {
	body := strings.TrimSpace(text)
	if body == "" {
		return MessageOutcome{}, fmt.Errorf("empty message")
	}
	target, err := c.ResolveTarget(to)
	if err != nil {
		return MessageOutcome{}, err
	}
	self := c.Whoami()
	if isSelf(self, target) {
		return MessageOutcome{}, fmt.Errorf("%q is this session itself — a session cannot message itself; address another agent (list_sessions shows them, whoami shows you)", to)
	}

	note := ""
	if !target.Live {
		if !opts.Resume {
			return MessageOutcome{}, fmt.Errorf("recipient %s is not running, so the message was NOT delivered (resume=false). %s",
				target.Label(), resumeHint(target))
		}
		live, dnote, lerr := c.EnsureLive(target, opts.Dialog)
		if lerr != nil {
			return MessageOutcome{}, fmt.Errorf("recipient %s is not running and could not be woken to receive the message: %w",
				target.Label(), lerr)
		}
		target, note = live, dnote
	}

	id := newMessageID()
	msg := envelope(id, self, target, body, time.Now())
	how, serr := c.SubmitPrompt(target.Short, msg, false)
	if serr != nil {
		return MessageOutcome{}, fmt.Errorf("message %s to %s was NOT delivered: %w", id, target.Label(), serr)
	}
	return MessageOutcome{ID: id, From: self, To: target, Delivery: how, ResumeNote: note, Body: msg}, nil
}

// isSelf reports whether a resolved target is the calling session. Messaging
// yourself delivers a turn to your own REPL that arrives as if a peer wrote it,
// which is never what was meant.
func isSelf(self Self, target Session) bool {
	if !self.Known() {
		return false
	}
	return (self.Short != "" && strings.EqualFold(self.Short, target.Short)) ||
		(self.SessionID != "" && strings.EqualFold(self.SessionID, target.SessionID))
}

// resumeHint explains what can be done about a sleeping recipient, which differs
// by whether it is exited-but-resumable or really dead.
func resumeHint(target Session) string {
	if target.Resumable {
		return "It is exited-but-resumable: pass resume=true (the default) to wake it in place with its full history and deliver."
	}
	return "It is not resumable either (no job state, or its working directory is gone), so nothing can receive this message."
}

// newMessageID mints the identifier one message is known by at both ends. It is
// short enough to quote in a reply and unique enough to tell two deliveries
// apart — which the delivery check depends on, see envelope.
func newMessageID() string { return "m-" + randID()[:6] }

// envelope renders a message as the text the recipient's session receives.
//
// The recipient has to be able to answer three questions from the delivery
// alone, because nothing else will tell it: who wrote this, is anyone waiting,
// and how do I reply. So the body is wrapped in a tag naming both ends by their
// address, and followed by a short instruction. Without the instruction an agent
// answers into its own session, where the sender cannot see it — the mistake
// this envelope exists to prevent.
//
// The reply address is the sender's short id rather than its name: names are not
// unique, and a message that cannot be answered reliably is barely a message.
// A sender that has no address at all (this server started outside a background
// session) says so, instead of inviting a reply that would go nowhere.
//
// The message id leads the instruction line, not only the tag, for a mechanical
// reason: SubmitPrompt confirms a delivery by looking for the body's longest
// line in the recipient's transcript, and for a short message that line is this
// instruction — identical in every message ever sent. Two agents writing to the
// same session at once would then confirm each other's deliveries. Leading with
// the id keeps the line unique per message, so a confirmation is evidence about
// this message and no other.
func envelope(id string, from Self, to Session, body string, at time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<agent-message id=%s from=%s to=%s at=%s>\n",
		attr(id), attr(from.Label()), attr(to.Label()), attr(at.Format(time.RFC3339)))
	b.WriteString(body)
	b.WriteString("\n</agent-message>\n\n")
	fmt.Fprintf(&b, "[%s] This is a message from another agent, not from your user. It did not interrupt anything and nobody is blocked on it — answer when the work you are doing allows. ", id)
	if from.Addressable {
		fmt.Fprintf(&b, "To answer, call this MCP server's send_message tool (usually mcp__claude-agents__send_message) with to:%s — your own output is not visible to the sender, only a message is; if you do not have that tool, say so in your own session rather than answering into the void.", attr(from.Address()))
	} else {
		b.WriteString("The sender is not a background session and cannot receive a reply through send_message, so do not try to answer it that way — act on the message, and report to your user if an answer is needed.")
	}
	return b.String()
}

// attr renders a value as a double-quoted tag attribute. Quotes inside the value
// become single quotes rather than being escaped: a display name is free text,
// and a stray backslash-escape in what the model reads is noisier than the
// substitution.
func attr(v string) string {
	return `"` + strings.ReplaceAll(v, `"`, `'`) + `"`
}
