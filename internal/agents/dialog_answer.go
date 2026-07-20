package agents

import (
	"fmt"
	"time"
)

// DialogBlockedError reports that a multiple-choice dialog holds the session's
// keyboard focus, so nothing can be typed into it until the dialog is answered.
// It carries the dialog itself so a caller can act on the actual options instead
// of guessing at a keystroke.
type DialogBlockedError struct {
	Dialog *Dialog
}

func (e *DialogBlockedError) Error() string {
	return "session is showing a dialog and cannot accept a prompt until it is answered (" + e.Dialog.Describe() +
		"). Do NOT send a bare Enter: it picks whichever option is highlighted — on the resume dialog that is compacting, which spends the conversation and discards the pending prompt. " +
		"Re-run with on_resume_dialog=keep to continue the conversation as it is, or on_resume_dialog=compact to accept compaction."
}

// dialogOnScreen reads a session's current screen and returns the multiple-
// choice dialog holding focus, or nil when there is none. A read failure is
// reported as "no dialog": the callers use this as a guard before pressing a
// key, and every one of them has a safe fallback that does not press it.
func (c *Client) dialogOnScreen(short string) *Dialog {
	screen, err := c.ReadScreen(short, 60)
	if err != nil {
		return nil
	}
	return DetectDialog(screen)
}

// dialogNavSteps bounds how many keystrokes answerDialog will spend moving the
// selection. It is generously above any real dialog's option count, and exists
// only so a screen we are misreading cannot make the loop run forever.
const dialogNavSteps = 12

// answerDialog selects a specific option and confirms it. It never sends Enter
// on faith: before each keystroke it re-reads the screen, and it only confirms
// once the target option is the highlighted one. If the selection cannot be
// moved onto the target, it gives up with the dialog untouched — moving a
// highlight has no effect until something is confirmed.
func (c *Client) answerDialog(short string, target DialogOption) error {
	for step := 0; step < dialogNavSteps; step++ {
		cur := c.dialogOnScreen(short)
		if cur == nil {
			return fmt.Errorf("the dialog disappeared from %s's screen before it could be answered", short)
		}
		sel, ok := cur.selectedNumber()
		switch {
		case ok && sel == target.Number:
			if _, err := c.send(short, []byte("\r"), true, 1500*time.Millisecond); err != nil {
				return err
			}
			return nil
		case !ok:
			// No highlight is rendered (or it did not survive the ANSI strip): a
			// single arrow both moves and reveals the selection, and moving it is
			// harmless because nothing is confirmed until Enter.
			if _, err := c.send(short, []byte(keyBytes["down"]), true, 600*time.Millisecond); err != nil {
				return err
			}
		case sel < target.Number:
			if _, err := c.send(short, []byte(keyBytes["down"]), true, 600*time.Millisecond); err != nil {
				return err
			}
		default:
			if _, err := c.send(short, []byte(keyBytes["up"]), true, 600*time.Millisecond); err != nil {
				return err
			}
		}
	}
	return fmt.Errorf("could not move %s's dialog selection onto option %d (%q) within %d keystrokes; nothing was confirmed", short, target.Number, target.Label, dialogNavSteps)
}

// selectedNumber returns the number of the currently highlighted option.
func (d *Dialog) selectedNumber() (int, bool) {
	if d == nil {
		return 0, false
	}
	for _, o := range d.Options {
		if o.Selected {
			return o.Number, true
		}
	}
	return 0, false
}

// SettleResumeDialog deals with the startup dialog the CLI shows when a large
// conversation is resumed — the one offering to compact it, with compacting
// preselected. It is called after a resume and before anything is typed, so the
// dialog is answered deliberately rather than by whatever keystroke arrives
// first.
//
// choice decides what happens:
//   - DialogKeep (the default) answers with the option that continues the
//     conversation as it is. Compaction is destructive to the pending prompt and
//     to the session's context, so it is never the automatic answer.
//   - DialogCompact answers with the compacting option.
//   - DialogAsk leaves the dialog alone and reports it.
//
// A dialog that is not positively recognised is never answered, whatever the
// choice: answering an unknown question by matching it loosely is exactly the
// failure this guards against. It returns a note for the tool result (empty when
// there was no dialog) and a *DialogBlockedError when the dialog was left up.
func (c *Client) SettleResumeDialog(short string, choice ResumeDialogChoice) (string, error) {
	d := c.dialogOnScreen(short)
	if !d.Blocking() {
		return "", nil
	}
	if !d.Recognised() {
		return "", &DialogBlockedError{Dialog: d}
	}
	if choice == DialogAsk {
		return "", &DialogBlockedError{Dialog: d}
	}

	var target DialogOption
	var ok bool
	var did string
	if choice == DialogCompact {
		target, ok = d.option(effectCompact)
		did = "compacted the conversation as requested"
	} else {
		target, ok = d.keepOption()
		did = "kept the conversation (no compaction)"
	}
	if !ok {
		return "", &DialogBlockedError{Dialog: d}
	}
	if err := c.answerDialog(short, target); err != nil {
		return "", fmt.Errorf("%s is showing the resume dialog and it could not be answered: %w (%s)", short, err, d.Describe())
	}
	// Compaction runs a model turn and can take minutes; keeping is immediate.
	// Either way the session must be back at its prompt before anything is typed.
	wait := 30 * time.Second
	if choice == DialogCompact {
		wait = 10 * time.Minute
	}
	if err := c.waitDialogCleared(short, wait); err != nil {
		return "", err
	}
	return fmt.Sprintf("resume dialog answered — %s; ", did), nil
}

// waitDialogCleared blocks until no dialog is on the session's screen and it is
// back at its prompt.
func (c *Client) waitDialogCleared(short string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if d := c.dialogOnScreen(short); !d.Blocking() {
			return c.WaitReady(short, 20*time.Second)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("session %s still showed a dialog %s after it was answered", short, timeout)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
