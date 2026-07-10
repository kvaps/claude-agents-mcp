package agents

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestForkValidation covers the guards Fork applies before it ever shells out to
// claude: a fork needs both a source session id and a real source cwd (the cwd
// is where the fork lands, so a missing one would put the fork in the wrong
// project). These all return before claudePath/exec, so the test is hermetic.
func TestForkValidation(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "afile")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name      string
		sessionID string
		cwd       string
	}{
		{"empty session id", "", dir},
		{"blank session id", "   ", dir},
		{"empty cwd", "abc-123", ""},
		{"missing cwd", "abc-123", filepath.Join(dir, "gone")},
		{"file as cwd", "abc-123", file},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Fork(c.sessionID, c.cwd, "", "", false); err == nil {
				t.Fatalf("Fork(%q, %q) = nil error, want a validation error", c.sessionID, c.cwd)
			}
		})
	}
}

// TestForkSessionIntegration drives the real daemon: it forks a live or
// not-running source session and asserts the fork comes up live under a NEW
// short and a NEW session id (a fork, not the source), then removes the fork so
// it leaves nothing behind. The source is never touched. Gated behind
// FORK_IT_SOURCE (a short id / session id / name of a forkable source session).
func TestForkSessionIntegration(t *testing.T) {
	ref := os.Getenv("FORK_IT_SOURCE")
	if ref == "" {
		t.Skip("set FORK_IT_SOURCE=<source session ref> to run the live fork test")
	}
	c := NewClient()
	src, err := c.Resolve(ref)
	if err != nil {
		t.Fatalf("resolve source %q: %v", ref, err)
	}
	out, err := c.ForkSession(src.SessionID, src.Cwd, "fork-it-test", "", true)
	if err != nil {
		t.Fatalf("ForkSession(%s): %v", src.Short, err)
	}
	t.Cleanup(func() { _ = Remove(out.Short) }) // forks created by the test are throwaway
	if out.Short == src.Short {
		t.Errorf("fork reused the source short %s (not a distinct entry)", src.Short)
	}
	if out.SessionID == "" || out.SessionID == src.SessionID {
		t.Errorf("fork did not get a new session id: got %q, source %q", out.SessionID, src.SessionID)
	}
	// The fork must be present and usable in the live roster under its own short.
	live, lerr := c.listDaemon()
	if lerr != nil {
		t.Fatalf("listDaemon: %v", lerr)
	}
	found := false
	for _, j := range live {
		if j.Short == out.Short {
			found = true
			if !j.Usable() {
				t.Errorf("forked worker not usable: state=%q", j.State)
			}
		}
	}
	if !found {
		t.Errorf("forked session %s not present in the live roster", out.Short)
	}
	time.Sleep(500 * time.Millisecond) // let it settle before cleanup Remove
}
