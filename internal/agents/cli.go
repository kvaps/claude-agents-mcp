package agents

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// AgentInfo is the public `claude agents --json` view of a session. It carries
// the display name and the real (worktree) cwd, which the daemon's op:list omits.
type AgentInfo struct {
	SessionID string `json:"sessionId"`
	Name      string `json:"name"`
	Cwd       string `json:"cwd"`
	Kind      string `json:"kind"`
	Status    string `json:"status"`
}

// AgentsJSON returns `claude agents --json` keyed by session id.
func AgentsJSON() (map[string]AgentInfo, error) {
	bin, err := claudePath()
	if err != nil {
		return nil, err
	}
	out, err := exec.Command(bin, "agents", "--json").Output()
	if err != nil {
		return nil, err
	}
	var arr []AgentInfo
	if err := json.Unmarshal(out, &arr); err != nil {
		return nil, err
	}
	m := make(map[string]AgentInfo, len(arr))
	for _, a := range arr {
		m[a.SessionID] = a
	}
	return m, nil
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

// Stop gracefully stops a session (it stays in the list, idle).
func Stop(short string) error { return runClaude("stop", short) }

// Remove permanently removes a session (tombstone, no respawn).
func Remove(short string) error { return runClaude("rm", short) }

func runClaude(sub, short string) error {
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

// Rename sets the custom title of a session — the same effect as renaming a
// session in the agents view. It writes customTitle into the session's
// .meta.json sidecar under ~/.claude/projects/<encoded-cwd>/.
func Rename(sessionID, cwd, title string) error {
	if sessionID == "" || title == "" {
		return fmt.Errorf("session id and title are required")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".claude", "projects", encodeCwd(cwd))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, sessionID+".meta.json")
	meta := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &meta)
	}
	meta["customTitle"] = title
	meta["updatedAt"] = time.Now().UTC().Format(time.RFC3339)
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// encodeCwd maps a working directory to the project directory name Claude Code
// uses under ~/.claude/projects (slashes and dots become dashes).
func encodeCwd(cwd string) string {
	return strings.NewReplacer("/", "-", ".", "-").Replace(cwd)
}
