package agents

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ForkOutcome reports the session a fork produced. Unlike a resume, a fork is
// always a brand-new entry in the agents view, so it carries the freshly-minted
// short and session id (distinct from the source) rather than an InPlace flag.
type ForkOutcome struct {
	Short     string
	SessionID string
	State     string
}

// Fork starts a background worker that forks an existing session: it runs
// `claude --bg --resume <sessionID> --fork-session`, which replays the source
// session's transcript into a NEW session id and continues there, leaving the
// source untouched. --fork-session is Claude Code's own forking primitive, so
// the full history (parent linkage, summaries, file-history snapshots) is copied
// correctly — there is no need to hand-copy the .jsonl transcript.
//
// It runs in the source session's cwd (cmd.Dir = cwd) so the fork inherits the
// same working directory and lands in the same ~/.claude/projects/<proj> dir as
// the source; without that the fork would be created under the server's own cwd,
// in the wrong project and with the wrong working directory. The `--bg` flag is
// the same daemon-registration path create_session uses, so the fork shows up in
// `claude agents` like any other background session.
//
// An optional model is passed through as `--model` so the fork can run on a
// different model than the source (the claude CLI validates the value).
//
// It returns the command output, which carries the new session's short id.
func Fork(sessionID, cwd, name, model string, dangerous bool) (string, error) {
	if strings.TrimSpace(sessionID) == "" {
		return "", fmt.Errorf("source session id is required")
	}
	if cwd == "" {
		return "", fmt.Errorf("source session cwd is required to place the fork in its project")
	}
	if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("source session cwd %q is not a directory; cannot fork", cwd)
	}
	bin, err := claudePath()
	if err != nil {
		return "", err
	}
	args := []string{"--bg", "--resume", sessionID, "--fork-session"}
	if name != "" {
		args = append(args, "--name", name)
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	if dangerous {
		args = append(args, "--dangerously-skip-permissions")
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = cwd
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("claude --bg --resume --fork-session failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// ForkSession forks a source session and returns only once the new worker is
// verified live — so the caller never gets a fork that "already exited". It
// parses the new short from the `--bg` output, waits for the worker to boot and
// settle (a fork replays the whole source transcript, so it sits in "resuming"
// for a moment), and on failure removes the half-spawned worker so no
// crashed/idle orphan is left behind. The source session is never modified.
func (c *Client) ForkSession(srcSessionID, srcCwd, name, model string, dangerous bool) (ForkOutcome, error) {
	out, err := Fork(srcSessionID, srcCwd, name, model, dangerous)
	if err != nil {
		return ForkOutcome{}, err
	}
	short := ParseShortID(out)
	if short == "" {
		return ForkOutcome{}, fmt.Errorf("forked but could not parse the new short id from output:\n%s", out)
	}
	live, werr := c.WaitLive(short, 45*time.Second, 5*time.Second)
	if werr != nil {
		_ = Remove(short) // the fork is throwaway if it never came up; remove it so it is not left as garbage
		return ForkOutcome{}, werr
	}
	return ForkOutcome{Short: short, SessionID: live.SessionID, State: live.State}, nil
}
