package agents

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// bgShortRe extracts the new session's short id from `claude --bg` output,
// which reads "backgrounded · <short> · <name> (idle — …)".
var bgShortRe = regexp.MustCompile(`backgrounded[^\n]*?([0-9a-f]{8})`)

// ParseShortID pulls the new session's short id out of `claude --bg` output
// (ANSI colours and all). Returns "" if it can't be found.
func ParseShortID(out string) string {
	if m := bgShortRe.FindStringSubmatch(StripANSI(out)); m != nil {
		return m[1]
	}
	return ""
}

// AgentInfo is the public `claude agents --json` view of a session. It carries
// the display name, the short id, and the real (worktree) cwd, which the
// daemon's op:list omits. With --all it also includes not-running sessions.
type AgentInfo struct {
	ID        string `json:"id"` // short id (also valid for not-running sessions)
	SessionID string `json:"sessionId"`
	Name      string `json:"name"`
	Cwd       string `json:"cwd"`
	Kind      string `json:"kind"`
	Status    string `json:"status"`
	State     string `json:"state"`
}

// StatusStr returns whichever status field the CLI populated.
func (a AgentInfo) StatusStr() string {
	if a.Status != "" {
		return a.Status
	}
	return a.State
}

// AgentsJSON returns `claude agents --json` as an ordered slice. When all is
// true it passes --all to include not-running sessions (the full agents view).
func AgentsJSON(all bool) ([]AgentInfo, error) {
	bin, err := claudePath()
	if err != nil {
		return nil, err
	}
	args := []string{"agents", "--json"}
	if all {
		args = append(args, "--all")
	}
	out, err := exec.Command(bin, args...).Output()
	if err != nil {
		return nil, err
	}
	var arr []AgentInfo
	if err := json.Unmarshal(out, &arr); err != nil {
		return nil, err
	}
	return arr, nil
}

// claudePath returns the path to the claude CLI.
func claudePath() (string, error) {
	if p, err := exec.LookPath("claude"); err == nil {
		return p, nil
	}
	if home, err := os.UserHomeDir(); err == nil {
		cand := filepath.Join(home, ".local", "bin", "claude")
		if _, err := os.Stat(cand); err == nil {
			return cand, nil
		}
	}
	return "", fmt.Errorf("claude CLI not found in PATH")
}

// Create dispatches a new background session in cwd with an optional name.
// It returns the command output (which includes the new session id).
func Create(cwd, name string, dangerous bool) (string, error) {
	if cwd == "" {
		return "", fmt.Errorf("cwd is required")
	}
	if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("cwd %q is not a directory", cwd)
	}
	bin, err := claudePath()
	if err != nil {
		return "", err
	}
	args := []string{"--bg"}
	if name != "" {
		args = append(args, "--name", name)
	}
	if dangerous {
		args = append(args, "--dangerously-skip-permissions")
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = cwd
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("claude --bg failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// Resume brings a not-running session back as a background worker (runs
// `claude --bg --resume <sessionID>`) and returns the command output, which
// carries the freshly-allocated short id. Resuming a session that is already
// live spawns a second worker the daemon then retires (it keeps one worker per
// session), so the returned short can be dead on arrival — callers must verify
// liveness first (resume_session checks the roster and waits via WaitLive).
func Resume(sessionID string, dangerous bool) (string, error) {
	if strings.TrimSpace(sessionID) == "" {
		return "", fmt.Errorf("session id is required")
	}
	bin, err := claudePath()
	if err != nil {
		return "", err
	}
	args := []string{"--bg", "--resume", sessionID}
	if dangerous {
		args = append(args, "--dangerously-skip-permissions")
	}
	out, err := exec.Command(bin, args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("claude --bg --resume failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// Stop gracefully stops a session (it stays in the list, idle).
func Stop(short string) error { return runClaude("stop", short) }

// Remove permanently deletes a session (tombstone, no respawn).
func Remove(short string) error { return runClaude("rm", short) }

func runClaude(sub, short string) error {
	if short == "" {
		return fmt.Errorf("empty session id")
	}
	bin, err := claudePath()
	if err != nil {
		return err
	}
	cmd := exec.Command(bin, sub, short)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("claude %s %s: %w: %s", sub, short, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Rename sets a session's display name natively, in both name systems the
// `claude` CLI uses:
//
//   - The agents view (`claude agents`) reads the daemon job state
//     (~/.claude/jobs/<short>/state.json) `name` field — written by RenameJobState.
//   - The resume picker / `claude` session list read the session transcript: the
//     CLI's /rename appends `custom-title` and `agent-name` events to the
//     <sessionID>.jsonl, and readers scan those out of its head/tail — written by
//     writeSessionTitle.
//
// Both are best-effort and independent: a live session updates the job state
// (and its transcript if present), a not-running session with only a transcript
// still gets renamed there. It errors only when neither store could be touched.
func Rename(short, sessionID, cwd, title string) error {
	if title == "" {
		return fmt.Errorf("title is required")
	}
	wroteState, stateErr := RenameJobState(short, title)
	if stateErr != nil {
		return stateErr
	}
	wroteTitle, titleErr := writeSessionTitle(sessionID, cwd, title)
	if titleErr != nil && !wroteState {
		return titleErr // job state untouched and the transcript write failed
	}
	if !wroteState && !wroteTitle {
		return fmt.Errorf("could not rename %q: no daemon job state and no transcript at its cwd", short)
	}
	return nil
}

// writeSessionTitle records the rename in the session transcript the way the
// CLI's /rename does: it appends a `custom-title` event (the title the resume
// picker and `claude` session list read) and an `agent-name` event (the
// prompt-bar display name) to ~/.claude/projects/<sanitized-cwd>/<sessionID>.jsonl.
// Readers scan these out of the transcript head/tail; the CLI never reads the
// .meta.json sidecar our earlier code wrote, so that write was inert.
//
// It is best-effort: it appends only to an already-existing transcript (so it
// never creates an orphan metadata-only file at a guessed path) and is a no-op
// when the session id or cwd is unknown. Returns whether it appended.
func writeSessionTitle(sessionID, cwd, title string) (bool, error) {
	if sessionID == "" || cwd == "" {
		return false, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false, err
	}
	path := filepath.Join(home, ".claude", "projects", sanitizeProjectPath(cwd), sessionID+".jsonl")
	if _, err := os.Stat(path); err != nil {
		return false, nil // no transcript at the guessed path; the job-state name still applies
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	for _, entry := range []map[string]any{
		{"type": "custom-title", "customTitle": title, "sessionId": sessionID},
		{"type": "agent-name", "agentName": title, "sessionId": sessionID},
	} {
		b, err := json.Marshal(entry)
		if err != nil {
			return false, err
		}
		if _, err := f.Write(append(b, '\n')); err != nil {
			return false, err
		}
	}
	return true, nil
}

// nonAlnum matches every character the CLI's sanitizePath rewrites.
var nonAlnum = regexp.MustCompile(`[^a-zA-Z0-9]`)

// sanitizeProjectPath maps a working directory to the project directory name
// Claude Code uses under ~/.claude/projects, matching the CLI's sanitizePath:
// every non-alphanumeric character becomes a dash. The earlier encoder replaced
// only "/" and ".", so a cwd containing "_", a space, etc. resolved to a phantom
// directory the CLI never reads.
//
// The CLI appends a hash suffix when the sanitized name exceeds 200 chars (very
// deep paths); that rare edge case is not reproduced here.
func sanitizeProjectPath(cwd string) string {
	return nonAlnum.ReplaceAllString(cwd, "-")
}
