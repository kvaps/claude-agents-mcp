package agents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTranscript lays down a transcript under a fake ~/.claude/projects for a
// session that has no entry in the agents list — the shape left behind by
// `claude rm`, which clears the entry and nothing else.
func writeTranscript(t *testing.T, home, project, sid string, lines ...string) string {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, sid+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func record(cwd, extra string) string {
	return `{"type":"user","message":{"role":"user","content":"hi"},"cwd":"` + cwd + `"` + extra + `}`
}

// The core of the bug: a session with a valid transcript but no list entry must
// be findable by its short id, which is the first 8 hex of its session id.
func TestFindOrphanByShortAndSessionID(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	const sid = "abcd1234-1111-2222-3333-444455556666"
	writeTranscript(t, home, "-tmp-repo", sid,
		record(cwd, ""),
		`{"type":"custom-title","customTitle":"20.07/recovered-session","sessionId":"`+sid+`"}`,
		record(cwd, ""),
	)

	for _, ref := range []string{"abcd1234", sid, "ABCD1234", "abcd1234-1111"} {
		o := FindOrphan(ref)
		if o == nil {
			t.Errorf("FindOrphan(%q) found nothing", ref)
			continue
		}
		if o.SessionID != sid {
			t.Errorf("FindOrphan(%q).SessionID = %q, want %q", ref, o.SessionID, sid)
		}
		if o.Short() != "abcd1234" {
			t.Errorf("FindOrphan(%q).Short() = %q", ref, o.Short())
		}
		if o.Cwd != cwd {
			t.Errorf("FindOrphan(%q).Cwd = %q, want %q", ref, o.Cwd, cwd)
		}
		if o.Title != "20.07/recovered-session" {
			t.Errorf("FindOrphan(%q).Title = %q — the name must be recovered so the session does not display as its kind once it exits", ref, o.Title)
		}
	}
}

// A reference too short to identify a session must not match: resuming the
// wrong conversation is worse than reporting not-found.
func TestFindOrphanRejectsVagueReferences(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeTranscript(t, home, "-tmp-repo", "abcd1234-1111-2222-3333-444455556666", record(t.TempDir(), ""))

	for _, ref := range []string{"", "ab", "abcd", "abcd123", "some-session-name", "zzzzzzzz"} {
		if o := FindOrphan(ref); o != nil {
			t.Errorf("FindOrphan(%q) matched %s", ref, o.SessionID)
		}
	}
}

// A session that moved between project directories has a transcript in each;
// the freshest is the conversation to resume.
func TestFindOrphanPrefersFreshestTranscript(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	oldCwd, newCwd := t.TempDir(), t.TempDir()
	const sid = "beef0000-1111-2222-3333-444455556666"
	old := writeTranscript(t, home, "-tmp-old", sid, record(oldCwd, ""))
	fresh := writeTranscript(t, home, "-tmp-new", sid, record(newCwd, ""))
	stale := mustStat(t, old).ModTime().Add(-2 * 60 * 1e9)
	if err := os.Chtimes(old, stale, stale); err != nil {
		t.Fatal(err)
	}

	o := FindOrphan(sid)
	if o == nil {
		t.Fatal("no orphan found")
	}
	if o.Path != fresh {
		t.Errorf("picked %s, want the freshest transcript %s", o.Path, fresh)
	}
	if o.Cwd != newCwd {
		t.Errorf("Cwd = %q, want %q", o.Cwd, newCwd)
	}
}

// A session that moved into a directory since deleted should fall back to one
// that still exists rather than being declared unresumable.
func TestOrphanCwdFallsBackToAnExistingDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	live := t.TempDir()
	gone := filepath.Join(t.TempDir(), "deleted-worktree")
	const sid = "cafe0000-1111-2222-3333-444455556666"
	writeTranscript(t, home, "-tmp-repo", sid, record(live, ""), record(gone, ""))

	o := FindOrphan(sid)
	if o == nil {
		t.Fatal("no orphan found")
	}
	if o.Cwd != live {
		t.Errorf("Cwd = %q, want the directory that still exists %q", o.Cwd, live)
	}
	if !o.Session().Resumable {
		t.Error("an orphan with an existing cwd must report resumable")
	}
}

// When every recorded directory is gone the orphan is still returned, carrying
// the missing directory, so the error can name it instead of the resume
// crashing at startup.
func TestOrphanWithNoSurvivingCwdIsReportedNotResumable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	gone := filepath.Join(t.TempDir(), "deleted-worktree")
	const sid = "dead0000-1111-2222-3333-444455556666"
	writeTranscript(t, home, "-tmp-repo", sid, record(gone, ""))

	o := FindOrphan(sid)
	if o == nil {
		t.Fatal("no orphan found")
	}
	if o.Cwd != gone {
		t.Errorf("Cwd = %q, want the missing directory %q so the error can name it", o.Cwd, gone)
	}
	sess := o.Session()
	if sess.Resumable {
		t.Error("a session whose working directory is gone must not report resumable")
	}

	c := NewClient()
	_, err := c.ResumeByCLI(sid, gone, "", "", false)
	if err == nil {
		t.Fatal("resuming into a missing directory should fail before spawning anything")
	}
	if !strings.Contains(err.Error(), gone) {
		t.Errorf("the error does not name the missing directory: %v", err)
	}
}

// An empty working directory must fail before a worker is spawned: `--resume`
// resolves the conversation relative to the launch directory, so launching from
// wherever the server happens to run finds nothing.
func TestResumeByCLIRefusesWithoutCwd(t *testing.T) {
	c := NewClient()
	if _, err := c.ResumeByCLI("dead0000-1111-2222-3333-444455556666", "  ", "", "", false); err == nil {
		t.Error("resuming with no working directory should fail")
	}
}

// The agent-name record is the fallback when no custom-title was ever written.
func TestOrphanTitleFallsBackToAgentName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	const sid = "f00d0000-1111-2222-3333-444455556666"
	writeTranscript(t, home, "-tmp-repo", sid,
		record(cwd, ""),
		`{"type":"agent-name","agentName":"derived name","sessionId":"`+sid+`"}`,
	)
	o := FindOrphan(sid)
	if o == nil || o.Title != "derived name" {
		t.Errorf("title = %q, want %q", o.Title, "derived name")
	}
}

func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi
}
