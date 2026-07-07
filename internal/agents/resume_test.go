package agents

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestResumeFlags(t *testing.T) {
	cases := []struct {
		name      string
		flags     []string
		dangerous bool
		want      []string
	}{
		{"not dangerous keeps flags", []string{"--model", "opus"}, false, []string{"--model", "opus"}},
		{"dangerous adds bypass", []string{"--name", "x"}, true, []string{"--name", "x", "--dangerously-skip-permissions"}},
		{"dangerous no dup when already bypass", []string{"--dangerously-skip-permissions"}, true, []string{"--dangerously-skip-permissions"}},
		{"dangerous no dup when perm-mode set", []string{"--permission-mode", "bypassPermissions"}, true, []string{"--permission-mode", "bypassPermissions"}},
		{"dangerous on empty", nil, true, []string{"--dangerously-skip-permissions"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resumeFlags(&JobState{RespawnFlags: c.flags}, c.dangerous)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("resumeFlags(%v, %v) = %v, want %v", c.flags, c.dangerous, got, c.want)
			}
		})
	}
}

func TestResumeFlagsDoesNotMutateInput(t *testing.T) {
	in := []string{"--name", "x"}
	_ = resumeFlags(&JobState{RespawnFlags: in}, true)
	if !reflect.DeepEqual(in, []string{"--name", "x"}) {
		t.Fatalf("resumeFlags mutated its input: %v", in)
	}
}

func TestCwdMissing(t *testing.T) {
	dir := t.TempDir()
	if cwdMissing(dir) {
		t.Errorf("existing dir reported missing: %s", dir)
	}
	if cwdMissing("") {
		t.Errorf("empty cwd should be treated as present")
	}
	gone := filepath.Join(dir, "deleted-worktree")
	if !cwdMissing(gone) {
		t.Errorf("nonexistent dir reported present: %s", gone)
	}
	file := filepath.Join(dir, "afile")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !cwdMissing(file) {
		t.Errorf("a regular file is not a usable cwd: %s", file)
	}
}

func TestReadJobStateMissingReturnsSentinel(t *testing.T) {
	// A short with no job dir must report ErrNoJobState so the caller falls back
	// to the CLI resume path rather than treating it as a hard error.
	_, err := ReadJobState("zzzznope")
	if err == nil {
		t.Fatal("expected an error for a missing job dir")
	}
	// HOME may or may not contain the dir; only assert the sentinel when the dir
	// genuinely does not exist (the common case in a clean test env).
	if _, statErr := os.Stat(jobStatePath(t, "zzzznope")); os.IsNotExist(statErr) && err != ErrNoJobState {
		t.Fatalf("missing job state: got %v, want ErrNoJobState", err)
	}
}

func TestFindTranscript(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sid := "11111111-2222-3333-4444-555555555555"
	proj := filepath.Join(home, ".claude", "projects", "-some-worktree-project")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(proj, sid+".jsonl")
	if err := os.WriteFile(transcript, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("linkScanPath matching the sid wins", func(t *testing.T) {
		got := findTranscript(&JobState{LinkScanPath: transcript}, sid)
		if got != transcript {
			t.Errorf("got %q, want %q", got, transcript)
		}
	})
	t.Run("linkScanPath for another sid is ignored, search still finds it", func(t *testing.T) {
		stale := filepath.Join(proj, "99999999-8888-7777-6666-555555555555.jsonl")
		got := findTranscript(&JobState{LinkScanPath: stale}, sid)
		if got != transcript {
			t.Errorf("got %q, want %q", got, transcript)
		}
	})
	t.Run("no transcript anywhere returns empty", func(t *testing.T) {
		got := findTranscript(&JobState{}, "00000000-0000-0000-0000-000000000000")
		if got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}

func TestResumeDescriptorCarriesTranscriptPath(t *testing.T) {
	// The regression this guards: a dispatch descriptor without
	// launch.transcriptPath makes the resumed worker look its conversation up in
	// the project dir derived from the launch cwd, which fails (exit 1,
	// exit_with_message, crash loop) for any session whose transcript lives under
	// a different project dir — e.g. one that switched into a worktree.
	home := t.TempDir()
	t.Setenv("HOME", home)
	sid := "11111111-2222-3333-4444-555555555555"
	proj := filepath.Join(home, ".claude", "projects", "-elsewhere")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(proj, sid+".jsonl")
	if err := os.WriteFile(transcript, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	js := &JobState{SessionID: sid, Cwd: home, LinkScanPath: transcript}
	desc, err := resumeDescriptor("11111111", js, false)
	if err != nil {
		t.Fatal(err)
	}
	launch, ok := desc["launch"].(map[string]any)
	if !ok {
		t.Fatalf("descriptor has no launch map: %v", desc)
	}
	if got := launch["transcriptPath"]; got != transcript {
		t.Errorf("launch.transcriptPath = %v, want %q", got, transcript)
	}
	if got := launch["sessionId"]; got != sid {
		t.Errorf("launch.sessionId = %v, want %q", got, sid)
	}

	t.Run("omitted when no transcript exists", func(t *testing.T) {
		js := &JobState{SessionID: "00000000-0000-0000-0000-000000000000", Cwd: home}
		desc, err := resumeDescriptor("00000000", js, false)
		if err != nil {
			t.Fatal(err)
		}
		launch := desc["launch"].(map[string]any)
		if _, present := launch["transcriptPath"]; present {
			t.Errorf("transcriptPath should be omitted when no transcript is found: %v", launch)
		}
	})
	t.Run("errors without a session id", func(t *testing.T) {
		if _, err := resumeDescriptor("shortid0", &JobState{}, false); err == nil {
			t.Error("expected an error for a job state with no session id")
		}
	})
}

func jobStatePath(t *testing.T, short string) string {
	t.Helper()
	dir, err := jobsDir()
	if err != nil {
		t.Skip("no home dir")
	}
	return filepath.Join(dir, short, "state.json")
}

// TestResumeInPlaceIntegration drives the real daemon: it resumes a not-running
// session in place and asserts it comes up live under its own short, then stops
// it back to not-running so it leaves no live worker behind. It is gated behind
// RESUME_IT_SHORT (the short of a not-running session that has a transcript and a
// valid cwd) so `go test` stays hermetic by default.
func TestResumeInPlaceIntegration(t *testing.T) {
	short := os.Getenv("RESUME_IT_SHORT")
	if short == "" {
		t.Skip("set RESUME_IT_SHORT=<not-running session short> to run the live resume test")
	}
	c := NewClient()
	out, err := c.ResumeInPlace(short, true)
	if err != nil {
		t.Fatalf("ResumeInPlace(%s): %v", short, err)
	}
	t.Cleanup(func() { _ = Stop(out.Short) }) // return the fixture to not-running
	if !out.InPlace {
		t.Errorf("expected an in-place resume, got InPlace=false (short=%s)", out.Short)
	}
	if out.Short != short {
		t.Errorf("resumed under a different short: got %s, want %s (a fork/duplicate)", out.Short, short)
	}
	// Confirm the daemon roster actually carries it live under that short.
	live, lerr := c.listDaemon()
	if lerr != nil {
		t.Fatalf("listDaemon: %v", lerr)
	}
	found := false
	for _, j := range live {
		if j.Short == short {
			found = true
			if !j.Usable() {
				t.Errorf("resumed worker not usable: state=%q", j.State)
			}
		}
	}
	if !found {
		t.Errorf("resumed session %s not present in the live roster", short)
	}
	// Give it a beat so Stop in cleanup targets a settled worker.
	time.Sleep(500 * time.Millisecond)
}

// assertNoLiveOrphan fails if a usable worker is still present under short.
func assertNoLiveOrphan(t *testing.T, c *Client, short string) {
	t.Helper()
	time.Sleep(1 * time.Second)
	live, err := c.listDaemon()
	if err != nil {
		t.Fatalf("listDaemon: %v", err)
	}
	for _, j := range live {
		if j.Short == short && j.Usable() {
			t.Errorf("a live worker for %s was left behind after a failed resume (state=%q)", short, j.State)
		}
	}
}

// TestResumeInPlaceCwdGone drives the most common failure: the session's saved
// working directory is gone (a deleted worktree). ResumeInPlace must reject it
// with a clear error and never spawn a worker. Gated behind
// RESUME_IT_CWDGONE_SHORT.
func TestResumeInPlaceCwdGone(t *testing.T) {
	short := os.Getenv("RESUME_IT_CWDGONE_SHORT")
	if short == "" {
		t.Skip("set RESUME_IT_CWDGONE_SHORT=<session whose cwd was deleted> to run")
	}
	c := NewClient()
	_, err := c.ResumeInPlace(short, true)
	if err == nil {
		t.Fatalf("expected a cwd-gone error for %s, got success", short)
	}
	if !strings.Contains(err.Error(), "working directory no longer exists") {
		t.Errorf("error did not mention the missing cwd: %v", err)
	}
	t.Logf("got expected error: %v", err)
	assertNoLiveOrphan(t, c, short)
}

// TestResumeInPlaceCrashCleanup drives a resume that gets past the cwd check but
// crashes during replay (e.g. its transcript was removed): ResumeInPlace must
// return an error AND stop the failed worker so nothing is left live under the
// short. Gated behind RESUME_IT_CRASH_SHORT.
func TestResumeInPlaceCrashCleanup(t *testing.T) {
	short := os.Getenv("RESUME_IT_CRASH_SHORT")
	if short == "" {
		t.Skip("set RESUME_IT_CRASH_SHORT=<session whose resume crashes> to run the cleanup test")
	}
	c := NewClient()
	out, err := c.ResumeInPlace(short, true)
	if err == nil {
		// The in-place dispatch path is robust and may resume where a raw CLI
		// resume would crash (e.g. a missing transcript). Don't leave it live, and
		// skip rather than fail — this fixture simply did not crash.
		_ = Stop(out.Short)
		t.Skipf("resume of %s unexpectedly succeeded; cannot exercise crash cleanup with this fixture", short)
	}
	t.Logf("got expected resume error: %v", err)
	assertNoLiveOrphan(t, c, short)
}
