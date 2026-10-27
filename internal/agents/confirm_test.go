package agents

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// The roster changes the moment a prompt is submitted, but the CLI writes the
// prompt to the transcript only after processing it and on its next flush. The
// roster must not answer for the transcript in that gap.
func TestSubmitWaitsForSlowTranscript(t *testing.T) {
	f, c := newFakeDaemon(t)
	f.onReply = func(f *fakeDaemon, text string) { f.startTurn(text, 1200*time.Millisecond) }

	how, err := c.SubmitPrompt(fakeShort, "Review the address controller and report what you find.", false)
	if err != nil {
		t.Fatal(err)
	}
	if how != confirmedTranscript {
		t.Errorf("got %q, want %q", how, confirmedTranscript)
	}
	if _, enters := f.counts(); enters != 0 {
		t.Errorf("sent %d Enter(s) into a session already working on the prompt", enters)
	}
}

// The text landing in the input box repaints the TUI, which flips the roster's
// tempo with no turn started. That must not pass for delivery: the paste is
// rescued with Enter.
func TestSubmitRescuesHeldPaste(t *testing.T) {
	f, c := newFakeDaemon(t)
	f.onReply = func(f *fakeDaemon, text string) {
		f.holdInput(text)
		f.onEnter = func(f *fakeDaemon) { f.startTurn(text, 100*time.Millisecond) }
	}

	how, err := c.SubmitPrompt(fakeShort, longMultilinePrompt, false)
	if err != nil {
		t.Fatal(err)
	}
	if how != confirmedTranscript {
		t.Errorf("got %q, want %q", how, confirmedTranscript)
	}
	if _, enters := f.counts(); enters != 1 {
		t.Errorf("sent %d Enters, want 1", enters)
	}
}

// Submitted and being worked on, but still not written down when the window
// ends: delivered, and said to be unrecorded rather than roster-confirmed.
func TestSubmitReportsUnrecordedDelivery(t *testing.T) {
	f, c := newFakeDaemon(t)
	f.onReply = func(f *fakeDaemon, text string) { f.startTurn(text, -1) }

	how, err := c.SubmitPrompt(fakeShort, "Summarise the open pull requests.", false)
	if err != nil {
		t.Fatal(err)
	}
	if how != confirmedUnrecorded {
		t.Errorf("got %q, want %q", how, confirmedUnrecorded)
	}
	if _, enters := f.counts(); enters != 0 {
		t.Errorf("sent %d Enter(s) into a session already working on the prompt", enters)
	}
}

// Text that stays in the input box through both Enters is stuck.
func TestSubmitReportsStuckInput(t *testing.T) {
	f, c := newFakeDaemon(t)
	f.onReply = func(f *fakeDaemon, text string) { f.holdInput(text) }

	_, err := c.SubmitPrompt(fakeShort, longMultilinePrompt, false)
	if !errors.Is(err, errNotStarted) {
		t.Errorf("got %v, want errNotStarted", err)
	}
	if _, enters := f.counts(); enters != 2 {
		t.Errorf("sent %d Enters, want 2", enters)
	}
}

// Text that vanished — not in the input box, not in the transcript, the
// session idle — was not delivered, and says so distinctly.
func TestSubmitReportsNotDelivered(t *testing.T) {
	f, c := newFakeDaemon(t)
	f.onReply = func(*fakeDaemon, string) {}

	_, err := c.SubmitPrompt(fakeShort, "Rebase the branch onto master.", false)
	if !errors.Is(err, errNotDelivered) {
		t.Errorf("got %v, want errNotDelivered", err)
	}
}

// A session in the middle of a turn keeps writing its own records; the prompt
// waits in the input box until the turn ends.
func TestSubmitReportsQueuedBehindRunningTurn(t *testing.T) {
	f, c := newFakeDaemon(t)
	f.mu.Lock()
	f.working = true
	f.mu.Unlock()
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(100 * time.Millisecond):
				f.append(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"still on the earlier task"}]}}`)
			}
		}
	}()
	f.onReply = func(f *fakeDaemon, text string) { f.holdInput(text) }

	how, err := c.SubmitPrompt(fakeShort, "Then update the changelog.", false)
	if err != nil {
		t.Fatal(err)
	}
	if how != confirmedQueued {
		t.Errorf("got %q, want %q", how, confirmedQueued)
	}
	if _, enters := f.counts(); enters != 0 {
		t.Errorf("sent %d Enter(s) into a running turn", enters)
	}
}

// The CLI records "/goal <condition>" in its command form, so the typed line
// never appears; the condition does.
func TestSubmitConfirmsGoal(t *testing.T) {
	f, c := newFakeDaemon(t)
	f.onReply = func(f *fakeDaemon, text string) { f.startTurn(text, 200*time.Millisecond) }

	how, err := c.SubmitPrompt(fakeShort, "all tests in ./internal pass", true)
	if err != nil {
		t.Fatal(err)
	}
	if how != confirmedTranscript {
		t.Errorf("got %q, want %q", how, confirmedTranscript)
	}
}

// A session whose id the roster did not report at the baseline is confirmed
// once the id shows up.
func TestSubmitAdoptsLateSessionID(t *testing.T) {
	f, c := newFakeDaemon(t)
	f.mu.Lock()
	sid := f.listSID
	f.listSID = ""
	f.mu.Unlock()
	f.onReply = func(f *fakeDaemon, text string) {
		f.listSID = sid
		f.startTurn(text, 500*time.Millisecond)
	}

	how, err := c.SubmitPrompt(fakeShort, "Draft the release notes.", false)
	if err != nil {
		t.Fatal(err)
	}
	if how != confirmedTranscript {
		t.Errorf("got %q, want %q", how, confirmedTranscript)
	}
}

// Without a session id there is no transcript to consult, and the roster is the
// only evidence there is.
func TestSubmitFallsBackToRosterWithoutTranscript(t *testing.T) {
	f, c := newFakeDaemon(t)
	f.mu.Lock()
	f.listSID = ""
	f.mu.Unlock()
	f.onReply = func(f *fakeDaemon, text string) { f.startTurn(text, -1) }

	how, err := c.SubmitPrompt(fakeShort, "Draft the release notes.", false)
	if err != nil {
		t.Fatal(err)
	}
	if how != confirmedState {
		t.Errorf("got %q, want %q", how, confirmedState)
	}
}

func TestConfirmWindowFromEnv(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
	}{
		{"", defaultConfirmWindow},
		{"30s", 30 * time.Second},
		{"20", 20 * time.Second},
		{"10ms", time.Second},
		{"1h", 2 * time.Minute},
		{"soon", defaultConfirmWindow},
	} {
		t.Setenv(confirmTimeoutEnv, tc.in)
		if got := confirmWindowFromEnv(); got != tc.want {
			t.Errorf("%s=%q: got %s, want %s", confirmTimeoutEnv, tc.in, got, tc.want)
		}
	}
}

func TestInputHolds(t *testing.T) {
	const body = "Review the address controller\nand report."
	for _, tc := range []struct {
		name, screen string
		want         bool
	}{
		{"empty input", "❯ Review the address controller\n  ⎿ ok\n────\n❯ \n────\n", false},
		{"typed text", "────\n❯ Review the address controller\n────\n", true},
		{"paste placeholder", "│ ❯ [Pasted text #1 +1 lines] │\n", true},
		{"other text", "❯ something else entirely\n", false},
		{"no prompt", "loading…\n", false},
	} {
		if got := inputHolds(tc.screen, body); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestNewOutput(t *testing.T) {
	before := strings.Repeat("x", 300) + "✻ Thinking (esc to interrupt)"
	if got := newOutput(before, before); got != "" {
		t.Errorf("unchanged output: got %q", got)
	}
	if got := newOutput(before, before+"frame"); got != "frame" {
		t.Errorf("appended output: got %q", got)
	}
	// The tail window slid past the old content entirely.
	if got := newOutput("old", "brand new"); got != "brand new" {
		t.Errorf("replaced output: got %q", got)
	}
}
