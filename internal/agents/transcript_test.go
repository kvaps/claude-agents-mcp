package agents

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeSession points ~/.claude/projects at a temp dir and returns a session id
// whose transcript lives there, so the transcript checks can be exercised
// without touching a real session.
func fakeSession(t *testing.T) (sid, path string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	sid = "11111111-2222-3333-4444-555555555555"
	dir := filepath.Join(home, ".claude", "projects", "-tmp-project")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(dir, sid+".jsonl")
	if err := os.WriteFile(path, []byte(userRecord("an earlier, unrelated prompt")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return sid, path
}

// userRecord is a transcript line shaped like the ones the CLI writes for a
// submitted prompt.
func userRecord(text string) string {
	return `{"type":"user","message":{"role":"user","content":` + jsonQuote(text) + `},"sessionId":"x"}`
}

func jsonQuote(s string) string { return `"` + jsonNeedle(s) + `"` }

func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}

// The point of the transcript check: "the turn started" should mean the prompt
// reached the conversation, not that the TUI looked busy.
func TestTranscriptMarkLanded(t *testing.T) {
	sid, path := fakeSession(t)
	const prompt = "Please review the address controller and report what you find."

	m := markTranscript(sid)
	if m.Landed(prompt) {
		t.Fatal("reported the prompt as landed before it was delivered")
	}
	if m.Grew() {
		t.Fatal("reported transcript growth before anything was written")
	}

	// A turn running in the session — records appended, but not our prompt. That
	// is the "queued behind a running turn" signal, not delivery.
	appendLine(t, path, `{"type":"assistant","message":{"role":"assistant","content":"working on the previous task"}}`)
	if m.Landed(prompt) {
		t.Error("unrelated records were mistaken for the prompt landing")
	}
	if !m.Grew() {
		t.Error("appended records were not seen as transcript growth")
	}

	appendLine(t, path, userRecord(prompt))
	if !m.Landed(prompt) {
		t.Error("the delivered prompt was not found in the transcript")
	}
}

// An identical prompt sent earlier in the same conversation must not be mistaken
// for this delivery — that is what the pre-delivery offset is for.
func TestTranscriptMarkIgnoresEarlierIdenticalPrompt(t *testing.T) {
	sid, path := fakeSession(t)
	const prompt = "Run the tests and report the failures."

	appendLine(t, path, userRecord(prompt))
	m := markTranscript(sid) // marked after the earlier copy
	if m.Landed(prompt) {
		t.Error("an identical prompt from before the mark counted as this delivery")
	}
	appendLine(t, path, userRecord(prompt))
	if !m.Landed(prompt) {
		t.Error("the new copy was not found")
	}
}

// Tool results are user records too, so a record has to carry the prompt text to
// count.
func TestTranscriptMarkIgnoresToolResults(t *testing.T) {
	sid, path := fakeSession(t)
	m := markTranscript(sid)
	appendLine(t, path, `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"ok"}]}}`)
	if m.Landed("Please summarise the design document.") {
		t.Error("a tool result counted as prompt delivery")
	}
}

// A multi-line prompt is matched on its most distinctive line, so the REPL
// normalising the edges of a paste does not break the check.
func TestTranscriptMarkMultilinePrompt(t *testing.T) {
	sid, path := fakeSession(t)
	prompt := "Do this:\n" +
		"and the long distinctive line that identifies this particular prompt beyond doubt\nthanks"

	m := markTranscript(sid)
	appendLine(t, path, userRecord(prompt))
	if !m.Landed(prompt) {
		t.Error("a multi-line prompt was not matched")
	}
}

// A session with no session id has no ground truth available; that must read as
// "cannot tell", and the caller falls back to the roster heuristics rather than
// treating it as a failed delivery.
func TestTranscriptMarkWithoutSessionID(t *testing.T) {
	m := markTranscript("")
	if m.available() {
		t.Error("a mark with no session id claimed to be available")
	}
	if m.Landed("anything") || m.Grew() {
		t.Error("an unavailable mark must report neither delivery nor growth")
	}
}

func TestPromptFragment(t *testing.T) {
	if got := promptFragment("short\nthe longest line here\nmid"); got != "the longest line here" {
		t.Errorf("promptFragment picked %q", got)
	}
	if got := promptFragment("   "); got != "" {
		t.Errorf("promptFragment of blank text is %q, want empty", got)
	}
	long := "x" + string(make([]byte, 0)) + repeat("ю", 300)
	if got := promptFragment(long); len(got) > 200 || got == "" {
		t.Errorf("promptFragment did not cap a long line: len=%d", len(got))
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
