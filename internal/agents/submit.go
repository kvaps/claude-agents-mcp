package agents

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// errNotStarted is returned when a prompt was delivered but no turn started: the
// text is sitting unsubmitted in the input box (typically collapsed to a
// "[Pasted text]" bracketed paste that still needs an Enter).
var errNotStarted = errors.New("prompt delivered but the session did not start a turn — the text is in the input box; retry submit_prompt or send Enter")

// goalMaxLen is the maximum length, in characters, that Claude Code's /goal
// command accepts for a goal condition. A longer condition is rejected with
// "Goal condition is limited to 4000 characters (got N)" and the prompt is left
// unsubmitted in the input box, so SubmitPrompt must not hand /goal a body over
// this length — it falls back to a plain prompt instead (see SubmitPrompt).
const goalMaxLen = 4000

// readyMarkers are strings that appear once a freshly-booted session has
// rendered its REPL prompt and can accept input.
var readyMarkers = []string{"❯", "for shortcuts", "auto mode"}

// WaitReady blocks until a session has booted far enough to accept input (its
// prompt is on screen), or the timeout elapses.
func (c *Client) WaitReady(short string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if screen, err := c.ReadScreen(short, 60); err == nil {
			for _, m := range readyMarkers {
				if strings.Contains(screen, m) {
					return nil
				}
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("session %s did not become ready within %s", short, timeout)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// turnStarted reports whether the session has begun a turn relative to a
// baseline captured just before submitting. A normal conversational turn keeps
// state=="done" and only flips tempo to "active" and updates detail (state only
// reads "working" for goal/loop sessions), so any of those signals counts.
func turnStarted(base, cur Session) bool {
	return cur.Busy() ||
		cur.Detail != base.Detail ||
		cur.State != base.State ||
		(cur.Tempo == "active" && base.Tempo != "active")
}

// waitStarted polls until the session begins a turn (relative to base) or
// timeout.
func (c *Client) waitStarted(short string, base Session, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if cur, err := c.Resolve(short); err == nil && turnStarted(base, cur) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// buildSubmitBody produces the exact text to deliver for a submission. A plain
// prompt is delivered verbatim (only trailing newlines trimmed). A goal is
// delivered as a "/goal <condition>" command — but only when the condition fits
// Claude Code's goalMaxLen limit; a longer condition would be rejected outright
// ("Goal condition is limited to 4000 characters") and left unsubmitted, so it
// falls back to the plain body, delivering the full task as an ordinary prompt.
// The overlay is lost but the whole task reaches the agent and the turn starts.
// Length is measured in characters (runes), matching Claude Code's own count, so
// a multi-byte (e.g. Cyrillic) goal is not needlessly downgraded.
func buildSubmitBody(text string, goal bool) string {
	body := strings.TrimRight(text, "\r\n")
	if goal {
		if g := strings.TrimSpace(text); utf8.RuneCountInString(g) <= goalMaxLen {
			body = "/goal " + g
		}
	}
	return body
}

// SubmitPrompt delivers text to a session and submits it as a turn, then always
// verifies the turn actually started before reporting success. It is the single
// delivery path for both plain prompts and /goal commands (goal just prefixes
// "/goal "), so both get the same guarantee.
//
// Preferred delivery: the daemon's native op:reply (see Reply), which hands the
// text straight to the worker's REPL — no PTY typing. But a successful op:reply
// ack only means the text reached the REPL, NOT that a turn started: for a long
// or multi-line body the REPL collapses it into an unsubmitted bracketed paste
// ("[Pasted text]") that still needs an Enter, so the ack alone is not
// trustworthy. So after a successful ack we still verify the turn started via the
// daemon roster (op:list); if it hasn't, we press Enter to rescue the
// unsubmitted paste, retrying once before reporting failure. When the native op
// is unavailable (an older daemon, a peer backend, or a session not accepting
// replies) it falls back to the keystroke-emulation path, submitViaPTY, which
// carries the same verify+retry guarantee.
func (c *Client) SubmitPrompt(short, text string, goal bool) (string, error) {
	body := buildSubmitBody(text, goal)
	if strings.TrimSpace(body) == "" {
		return "", fmt.Errorf("empty prompt")
	}

	// Snapshot the baseline BEFORE delivery so a started turn is detectable as a
	// change afterwards. The native op:reply can itself submit the turn, so the
	// baseline must predate it — capturing after would already reflect the started
	// state and hide the change.
	base, _ := c.Resolve(short)

	// On a native-op error, fall back to the PTY paste path (self-contained: it
	// pastes, settles, and verifies with its own baseline).
	if err := c.Reply(short, body); err != nil {
		return c.submitViaPTY(short, body)
	}

	// Native op acked. Don't trust it: verify the turn started, and if it hasn't
	// (the body is sitting as an unsubmitted paste), press Enter to submit it,
	// retrying once before reporting failure.
	if c.waitStarted(short, base, 4*time.Second) {
		return "", nil
	}
	screen, ok := c.submitEnterUntilStarted(short, base)
	if ok {
		return screen, nil
	}
	return screen, errNotStarted
}

// submitEnterUntilStarted presses Enter to submit a prompt still sitting in the
// input box and verifies the turn started, retrying once. It is shared by the
// native-reply rescue and the PTY paste path so both converge on the same
// verify+retry-Enter guarantee. Pressing Enter is safe even if a turn is already
// running (the input box is empty, so the keystroke is a no-op) — the leading
// waitStarted in SubmitPrompt avoids that case anyway. Returns the last screen
// and whether the turn started.
func (c *Client) submitEnterUntilStarted(short string, base Session) (string, bool) {
	var screen string
	for attempt := 0; attempt < 2; attempt++ {
		screen, _ = c.send(short, []byte("\r"), true, 800*time.Millisecond)
		if c.waitStarted(short, base, 6*time.Second) {
			return screen, true
		}
	}
	return screen, false
}

// submitViaPTY is the keystroke-emulation fallback for SubmitPrompt: it types the
// prompt into the session's PTY and verifies the turn started.
//
// The body is delivered as a bracketed paste so multi-line input is treated as
// one literal block (newlines don't submit early). The paste is sent in wait
// mode so the call drains the session's current redraw and only returns once the
// paste has actually landed and the screen is quiet — rather than firing the
// paste off and guessing with a fixed delay. The old fixed 300ms guess could be
// outrun by a long multi-line paste, or by a freshly created session still
// painting its boot UI (create_session submits the moment the prompt appears),
// so the submit Enter raced the not-yet-settled input and the text was left
// sitting unsubmitted in the box — most visible on long /goal pastes, where the
// leading slash also spins up the command menu and adds render latency. After
// the paste has settled a distinct Enter submits it (via submitEnterUntilStarted,
// shared with the native path); the turn is verified via the daemon roster
// (op:list) and Enter is retried once before reporting failure, so a session is
// never left silently idle holding an unsubmitted prompt.
func (c *Client) submitViaPTY(short, body string) (string, error) {
	// Deliver as a bracketed paste (the session enables paste mode), so the
	// whole multi-line body lands in the input box without submitting per line.
	// wait=true drains the paste's redraw so we only proceed once it has fully
	// landed — not after a fixed guess that a long paste can outrun.
	paste := append([]byte("\x1b[200~"), body...)
	paste = append(paste, "\x1b[201~"...)
	if _, err := c.send(short, paste, true, 4*time.Second); err != nil {
		return "", err
	}
	// A short extra guard so any residual redraw (e.g. the slash-command menu)
	// settles past the quiet window before the submit keystroke, then snapshot
	// the baseline so a started turn is detectable as a change.
	time.Sleep(300 * time.Millisecond)
	base, _ := c.Resolve(short)

	screen, ok := c.submitEnterUntilStarted(short, base)
	if ok {
		return screen, nil
	}
	return screen, errNotStarted
}
