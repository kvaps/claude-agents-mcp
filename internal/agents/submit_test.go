package agents

import (
	"os"
	"testing"
	"time"
)

// longMultilinePrompt is a long, multi-line body that reproduces the periodic
// submit race — a bare \r swallowed into a still-settling bracketed paste, which
// leaves the text sitting unsubmitted in the input box. SubmitPrompt prefixes
// "/goal " itself for the goal path, so this body carries no command prefix.
const longMultilinePrompt = "This is a deliberately long multi-line prompt used to exercise reliable submission.\n" +
	"Line two adds context so the paste spans several rendered lines in the REPL input box.\n" +
	"Line three: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n" +
	"Line four:  bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n" +
	"Line five:  cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc\n" +
	"Instruction: reply with the single word ACKNOWLEDGED and then stop; do not modify any files."

// settleIdle interrupts any running turn, waits for the session to go idle, and
// clears the input box so the next delivery starts from a clean slate.
func settleIdle(t *testing.T, c *Client, short string) {
	t.Helper()
	_, _ = c.Cancel(short, false) // Esc: interrupt any running turn
	_ = c.WaitIdle(short, 30*time.Second)
	time.Sleep(700 * time.Millisecond)
	_, _ = c.send(short, []byte("\x15"), false, 0) // Ctrl-U: clear the input box
	time.Sleep(200 * time.Millisecond)
}

// TestSubmitPromptStartsTurnIntegration drives the real daemon: it delivers a
// long multi-line prompt with SubmitPrompt and asserts the turn actually starts
// — without a manual Enter — for both the plain and /goal paths, several times
// each. This guards the periodic "text stuck in the input box" submit race. It
// is gated behind SUBMIT_IT_SHORT (the short of a live, idle session) so
// `go test` stays hermetic by default, and it returns the session to idle after
// each delivery so it leaves no running turn behind.
func TestSubmitPromptStartsTurnIntegration(t *testing.T) {
	short := os.Getenv("SUBMIT_IT_SHORT")
	if short == "" {
		t.Skip("set SUBMIT_IT_SHORT=<live idle session short> to run the live submit test")
	}
	c := NewClient()
	if _, err := c.Resolve(short); err != nil {
		t.Fatalf("resolve %s: %v", short, err)
	}
	t.Cleanup(func() { settleIdle(t, c, short) })

	cases := []struct {
		name string
		goal bool
	}{
		{"plain", false},
		{"goal", true},
	}
	const itersPerCase = 3
	for _, tc := range cases {
		for i := 0; i < itersPerCase; i++ {
			settleIdle(t, c, short)
			base, _ := c.Resolve(short)
			if _, err := c.SubmitPrompt(short, longMultilinePrompt, tc.goal); err != nil {
				t.Errorf("%s iter %d: SubmitPrompt failed: %v", tc.name, i, err)
				continue
			}
			// Verify the turn actually started, independent of which path
			// SubmitPrompt took (native op:reply or the PTY fallback), so the
			// guarantee holds either way.
			if !c.waitStarted(short, base, 8*time.Second) {
				t.Errorf("%s iter %d: SubmitPrompt returned ok but no turn started", tc.name, i)
				continue
			}
			t.Logf("%s iter %d: turn started", tc.name, i)
		}
	}
}
