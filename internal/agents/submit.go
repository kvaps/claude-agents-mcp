package agents

import (
	"fmt"
	"strings"
	"time"
)

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

// SubmitPrompt delivers text to a session's prompt and reliably submits it,
// then verifies the turn actually started. If goal is true the text is sent as
// a /goal command. The text is delivered as a bracketed paste so multi-line
// input is treated as one literal block (newlines don't submit early); a
// separate Enter then submits it. If the turn hasn't started shortly after,
// Enter is retried once before reporting failure — so a session is never left
// silently idle holding an unsubmitted prompt.
func (c *Client) SubmitPrompt(short, text string, goal bool) (string, error) {
	body := strings.TrimRight(text, "\r\n")
	if goal {
		body = "/goal " + strings.TrimSpace(text)
	}
	if strings.TrimSpace(body) == "" {
		return "", fmt.Errorf("empty prompt")
	}

	// Deliver as a bracketed paste (the session enables paste mode), so the
	// whole multi-line body lands in the input box without submitting per line.
	paste := append([]byte("\x1b[200~"), body...)
	paste = append(paste, "\x1b[201~"...)
	if _, err := c.send(short, paste, false, 0); err != nil {
		return "", err
	}
	// Let the paste register and its echo settle before the submit keystroke,
	// then snapshot the baseline so a started turn is detectable as a change.
	time.Sleep(300 * time.Millisecond)
	base, _ := c.Resolve(short)

	// Submit with a distinct Enter, then verify the turn started; retry once.
	var screen string
	for attempt := 0; attempt < 2; attempt++ {
		screen, _ = c.send(short, []byte("\r"), true, 600*time.Millisecond)
		if c.waitStarted(short, base, 6*time.Second) {
			return screen, nil
		}
	}
	return screen, fmt.Errorf("prompt delivered but the session did not start a turn — the text is in the input box; retry submit_prompt or send Enter")
}
