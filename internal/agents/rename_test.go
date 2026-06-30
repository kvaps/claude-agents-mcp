package agents

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRenameRespawnFlag(t *testing.T) {
	cases := []struct {
		name  string
		in    []any
		title string
		want  []any
	}{
		{
			name:  "replaces value after --name",
			in:    []any{"--name", "old", "--model", "opus"},
			title: "new",
			want:  []any{"--name", "new", "--model", "opus"},
		},
		{
			name:  "replaces --name= form",
			in:    []any{"--name=old", "--model", "opus"},
			title: "new",
			want:  []any{"--name=new", "--model", "opus"},
		},
		{
			name:  "no --name token leaves flags untouched",
			in:    []any{"--model", "opus", "--dangerously-skip-permissions"},
			title: "new",
			want:  []any{"--model", "opus", "--dangerously-skip-permissions"},
		},
		{
			name:  "trailing --name with no value is left alone",
			in:    []any{"--model", "opus", "--name"},
			title: "new",
			want:  []any{"--model", "opus", "--name"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			renameRespawnFlag(c.in, c.title)
			if !reflect.DeepEqual(c.in, c.want) {
				t.Fatalf("renameRespawnFlag = %v, want %v", c.in, c.want)
			}
		})
	}
}

// TestRenameJobState writes a synthetic job state.json into a uniquely-named
// fixture under the real jobs dir, renames it, and asserts the authoritative
// fields changed while every other field the daemon owns is preserved. The
// fixture is removed in cleanup so it leaves nothing behind.
func TestRenameJobState(t *testing.T) {
	dir, err := jobsDir()
	if err != nil {
		t.Skip("no home dir")
	}
	short := "rename-test-fixture"
	sdir := filepath.Join(dir, short)
	if err := os.MkdirAll(sdir, 0o755); err != nil {
		t.Fatalf("mkdir fixture: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sdir) })

	orig := map[string]any{
		"state":        "blocked",
		"tokens":       float64(352861),
		"name":         "old-name",
		"nameSource":   "user",
		"sessionId":    "b8068003-c88a-4f94-9b65-f124631fafce",
		"respawnFlags": []any{"--name", "old-name", "--dangerously-skip-permissions"},
	}
	b, _ := json.MarshalIndent(orig, "", "  ")
	statePath := filepath.Join(sdir, "state.json")
	if err := os.WriteFile(statePath, b, 0o644); err != nil {
		t.Fatalf("write fixture state: %v", err)
	}

	wrote, err := RenameJobState(short, "▶ cozyllm")
	if err != nil {
		t.Fatalf("RenameJobState: %v", err)
	}
	if !wrote {
		t.Fatal("RenameJobState reported no write for an existing job state")
	}

	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read back state: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("parse state: %v", err)
	}
	if got["name"] != "▶ cozyllm" {
		t.Errorf("name = %v, want %q", got["name"], "▶ cozyllm")
	}
	if got["nameSource"] != "user" {
		t.Errorf("nameSource = %v, want user", got["nameSource"])
	}
	// Untouched fields survive the rewrite.
	if got["state"] != "blocked" || got["tokens"] != float64(352861) {
		t.Errorf("rename clobbered unrelated fields: state=%v tokens=%v", got["state"], got["tokens"])
	}
	// The --name token in respawnFlags now carries the new title, so a respawn
	// re-applies it instead of the original name.
	wantFlags := []any{"--name", "▶ cozyllm", "--dangerously-skip-permissions"}
	if !reflect.DeepEqual(got["respawnFlags"], wantFlags) {
		t.Errorf("respawnFlags = %v, want %v", got["respawnFlags"], wantFlags)
	}
}

// TestRenameJobStateMissingIsNoOp asserts that renaming a session with no on-disk
// job state reports (false, nil) so the caller can fall back to the sidecar.
func TestRenameJobStateMissingIsNoOp(t *testing.T) {
	wrote, err := RenameJobState("zzzz-no-such-short", "x")
	if err != nil {
		t.Fatalf("RenameJobState on missing state errored: %v", err)
	}
	if wrote {
		t.Error("expected wrote=false for a session with no job dir")
	}
}

func TestSanitizeProjectPath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/Users/kvaps/git/cozystack", "-Users-kvaps-git-cozystack"},
		{"/Users/kvaps/git/freedom_cloud_docs", "-Users-kvaps-git-freedom-cloud-docs"},
		{"/tmp/a.b/c d", "-tmp-a-b-c-d"},
		{"plugin:name:server", "plugin-name-server"},
	}
	for _, c := range cases {
		if got := sanitizeProjectPath(c.in); got != c.want {
			t.Errorf("sanitizeProjectPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestWriteSessionTitle appends title events to a fixture transcript and asserts
// the canonical custom-title / agent-name JSONL lines are written, and that a
// missing transcript is a no-op (no orphan file created).
func TestWriteSessionTitle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sid := "11111111-2222-3333-4444-555555555555"
	cwd := "/Users/x/proj_one"
	projDir := filepath.Join(home, ".claude", "projects", sanitizeProjectPath(cwd))
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(projDir, sid+".jsonl")

	// Missing transcript: no-op, no file created.
	wrote, err := writeSessionTitle(sid, cwd, "x")
	if err != nil || wrote {
		t.Fatalf("missing transcript: got wrote=%v err=%v, want false,nil", wrote, err)
	}
	if _, statErr := os.Stat(transcript); statErr == nil {
		t.Fatal("writeSessionTitle created an orphan transcript for a missing file")
	}

	// Existing transcript: appends both events, preserving prior content.
	if err := os.WriteFile(transcript, []byte("{\"type\":\"user\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	wrote, err = writeSessionTitle(sid, cwd, "▶ my title")
	if err != nil || !wrote {
		t.Fatalf("existing transcript: got wrote=%v err=%v, want true,nil", wrote, err)
	}
	b, err := os.ReadFile(transcript)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines (1 original + 2 events), got %d: %q", len(lines), string(b))
	}
	var ct, an map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &ct); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[2]), &an); err != nil {
		t.Fatal(err)
	}
	if ct["type"] != "custom-title" || ct["customTitle"] != "▶ my title" || ct["sessionId"] != sid {
		t.Errorf("custom-title line wrong: %v", ct)
	}
	if an["type"] != "agent-name" || an["agentName"] != "▶ my title" || an["sessionId"] != sid {
		t.Errorf("agent-name line wrong: %v", an)
	}
}

// agentsViewName polls `claude agents --json --all` and returns the display name
// the agents view shows for a session id (empty if not listed).
func agentsViewName(t *testing.T, sessionID string) string {
	t.Helper()
	infos, err := AgentsJSON(true)
	if err != nil {
		t.Fatalf("AgentsJSON: %v", err)
	}
	for _, a := range infos {
		if a.SessionID == sessionID {
			return a.Name
		}
	}
	return ""
}

// TestRenameLiveIntegration renames a real running session and asserts the new
// name lands in the authoritative job state, is what `claude agents` reports, and
// survives a sync window (the daemon's classification cycles do not revert it).
// Gated behind RENAME_IT_SHORT (the short of a throwaway running session) so
// `go test` stays hermetic by default. The caller owns the fixture's lifecycle
// (create it before, `claude rm` it after).
func TestRenameLiveIntegration(t *testing.T) {
	short := os.Getenv("RENAME_IT_SHORT")
	if short == "" {
		t.Skip("set RENAME_IT_SHORT=<throwaway running session short> to run the live rename test")
	}
	c := NewClient()
	sess, err := c.Resolve(short)
	if err != nil {
		t.Fatalf("resolve %s: %v", short, err)
	}
	newTitle := "rename-it-" + randID()[:6]

	if err := c.Rename(sess.Short, sess.SessionID, sess.Cwd, newTitle); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	// Authoritative store: jobs/<short>/state.json carries the new name + user source.
	js, err := ReadJobState(short)
	if err != nil {
		t.Fatalf("ReadJobState: %v", err)
	}
	if js.Name != newTitle {
		t.Errorf("job state name = %q, want %q", js.Name, newTitle)
	}

	// Agents view reports the new name, and it stays put across a sync window
	// (the original bug reverted it within seconds).
	for _, when := range []string{"immediately", "after a 6s sync window"} {
		if got := agentsViewName(t, sess.SessionID); got != newTitle {
			t.Errorf("`claude agents` name %s = %q, want %q", when, got, newTitle)
		}
		if when == "immediately" {
			time.Sleep(6 * time.Second)
		}
	}
}
