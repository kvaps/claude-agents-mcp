package agents

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrNoJobState is returned by ReadJobState/ResumeInPlace when a session has no
// on-disk job state (e.g. it is not in the agents list). The caller should fall
// back to the CLI resume path.
var ErrNoJobState = errors.New("no on-disk job state for session")

// ResumeOutcome reports how a resume completed.
type ResumeOutcome struct {
	Short   string
	State   string
	InPlace bool // resumed under its own short via dispatch (no fork / duplicate)
}

// JobState is the subset of a session's on-disk job state
// (~/.claude/jobs/<short>/state.json) that we need to resume it in place via the
// daemon dispatch op, the same way the agents-view picker does.
type JobState struct {
	SessionID       string   `json:"sessionId"`
	ResumeSessionID string   `json:"resumeSessionId"`
	RespawnFlags    []string `json:"respawnFlags"`
	Cwd             string   `json:"cwd"`
	LinkScanPath    string   `json:"linkScanPath"`
	Name            string   `json:"name"`
	Intent          string   `json:"intent"`
	DaemonShort     string   `json:"daemonShort"`
	State           string   `json:"state"`
	Detail          string   `json:"detail"`
}

// ReadJobState loads the on-disk job state for a session short. A missing job
// dir / state.json (e.g. a session that is not in the agents list) returns an
// error so the caller can fall back to the CLI resume path.
func ReadJobState(short string) (*JobState, error) {
	if short == "" {
		return nil, fmt.Errorf("empty short id")
	}
	dir, err := jobsDir()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(dir, short, "state.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNoJobState
		}
		return nil, err
	}
	var js JobState
	if err := json.Unmarshal(b, &js); err != nil {
		return nil, fmt.Errorf("parse job state for %s: %w", short, err)
	}
	return &js, nil
}

// controlKey reads the daemon control-socket auth key. The dispatch op is
// authenticated; attach/list/subscribe are not.
func controlKey() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(filepath.Join(home, ".claude", "daemon", "control.key"))
	if err != nil {
		return "", fmt.Errorf("read daemon control key: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

// resumeFlags derives the launch flags for a resume from the session's saved
// respawnFlags, adding bypass-permissions when the caller asked for it and the
// session was not already launched with a permission mode, and overriding the
// model when the caller picked one explicitly.
func resumeFlags(js *JobState, model string, dangerous bool) []string {
	flags := withModelOverride(js.RespawnFlags, model)
	if !dangerous {
		return flags
	}
	for _, f := range flags {
		if f == "--dangerously-skip-permissions" || f == "--permission-mode" {
			return flags // already carries a permission mode
		}
	}
	return append(flags, "--dangerously-skip-permissions")
}

// withModelOverride returns a copy of flags with the model applied: any saved
// --model flag (both the "--model X" and "--model=X" forms) is dropped and the
// caller's model appended, so an explicit choice wins over whatever the session
// was originally launched with. An empty model keeps the flags untouched.
func withModelOverride(flags []string, model string) []string {
	out := make([]string, 0, len(flags)+2)
	if model == "" {
		return append(out, flags...)
	}
	for i := 0; i < len(flags); i++ {
		if flags[i] == "--model" {
			i++ // skip the value too
			continue
		}
		if strings.HasPrefix(flags[i], "--model=") {
			continue
		}
		out = append(out, flags[i])
	}
	return append(out, "--model", model)
}

// findTranscript locates the on-disk transcript (~/.claude/projects/<project>/
// <sid>.jsonl) for the session being resumed. The resumed worker's `--resume
// <sessionId>` lookup only searches the project directory derived from the
// launch cwd, which misses transcripts living under another project dir — e.g. a
// session that switched into a worktree mid-run, so its job cwd no longer maps
// to where its transcript is kept. Such a resume exits with "No conversation
// found" (exit 1, exit_with_message) and crash-loops. The agents-view picker
// avoids this by passing launch.transcriptPath explicitly; this derives the same
// path: the job state's linkScanPath (the transcript the daemon itself scans)
// when it matches the session id, else a search across ~/.claude/projects.
// Returns "" when no transcript exists (e.g. a never-prompted session) — the
// dispatch then proceeds without the hint and the worker falls back to its own
// cwd-derived lookup.
func findTranscript(js *JobState, sid string) string {
	if p := js.LinkScanPath; p != "" && filepath.Base(p) == sid+".jsonl" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	matches := transcriptFiles(sid)
	if len(matches) == 0 {
		return ""
	}
	newest := matches[0]
	var newestMod time.Time
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil && fi.ModTime().After(newestMod) {
			newest, newestMod = m, fi.ModTime()
		}
	}
	return newest
}

// resumeDescriptor builds the op:dispatch descriptor for an in-place resume,
// mirroring what the agents-view picker sends (see the roster.json `dispatch`
// field of any live picker-resumed worker). launch.transcriptPath is included
// whenever the transcript can be located — without it a worker whose transcript
// lives outside its cwd-derived project dir cannot find its conversation and
// exits at startup.
func resumeDescriptor(short string, js *JobState, model string, dangerous bool) (map[string]any, error) {
	sid := js.ResumeSessionID
	if sid == "" {
		sid = js.SessionID
	}
	if sid == "" {
		return nil, fmt.Errorf("session %s has no sessionId on disk", short)
	}
	flags := resumeFlags(js, model, dangerous)
	launch := map[string]any{
		"mode":      "resume",
		"sessionId": sid,
		"fork":      false,
		"flagArgs":  flags,
	}
	if p := findTranscript(js, sid); p != "" {
		launch["transcriptPath"] = p
	}
	return map[string]any{
		"proto":        1,
		"short":        short,
		"nonce":        randID()[:8],
		"sessionId":    js.SessionID,
		"createdAt":    time.Now().UnixMilli(),
		"source":       "fleet",
		"cwd":          js.Cwd,
		"launch":       launch,
		"env":          map[string]any{},
		"isolation":    "none",
		"respawnFlags": flags,
		"seed":         map[string]any{"intent": js.Intent, "name": js.Name},
	}, nil
}

// DispatchResume asks the daemon to resume a not-running session in place: under
// its own short, with the same sessionId, no fork and no duplicate roster entry
// — exactly what pressing Enter on a session in the agents view does (it sends
// op:dispatch with launch.mode:"resume"). Unlike `claude --bg --resume`, which
// spawns a worker under a fresh short (a fork that duplicates the session in the
// list), this keeps the session as a single entry that simply goes live.
//
// It returns the short the worker registered under (the session's own short). A
// reply of ok with the same short means the daemon claimed/spawned the worker;
// the caller must still verify it reaches a usable state (a resume can crash
// during replay, e.g. when the saved cwd is gone or there is no transcript).
func (c *Client) DispatchResume(short string, js *JobState, model string, dangerous bool) (string, error) {
	key, err := controlKey()
	if err != nil {
		return "", err
	}
	desc, err := resumeDescriptor(short, js, model, dangerous)
	if err != nil {
		return "", err
	}
	raw, err := c.request(map[string]any{
		"proto": 1, "op": "dispatch", "d": desc, "timeoutMs": 5000, "auth": key,
	})
	if err != nil {
		return "", fmt.Errorf("dispatch resume: %w", err)
	}
	var resp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		Short string `json:"short"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", fmt.Errorf("parse dispatch reply: %w", err)
	}
	if !resp.OK {
		return "", fmt.Errorf("daemon refused resume: %s", resp.Error)
	}
	if resp.Short != "" {
		return resp.Short, nil
	}
	return short, nil
}

// awaitResumed polls the daemon roster until the resumed worker holds a usable
// state for the settle window, or fails. Unlike WaitLive (which waits through a
// crash as a transient state for the racey CLI path), a dispatch resume that
// crashes is terminal — the daemon does not respawn a cwd-gone / no-transcript
// crash — so this returns promptly with the captured reason instead of waiting
// it out. It returns the live session on success, or an error describing why the
// resume did not come up.
func (c *Client) awaitResumed(short string, timeout, settle time.Duration) (Session, error) {
	deadline := time.Now().Add(timeout)
	seen := false
	var last Session
	var reason string
	var bootedAt time.Time
	for {
		if live, err := c.listDaemon(); err == nil {
			cur, found := Session{}, false
			for _, j := range live {
				if j.Short == short {
					j.Live, cur, found = true, j, true
					break
				}
			}
			switch {
			case found:
				seen, last = true, cur
				if d := strings.TrimSpace(cur.Detail); d != "" {
					reason = d
				}
				switch {
				case cur.Crashed():
					return Session{}, fmt.Errorf("resumed worker crashed during startup%s", reasonSuffix(reason))
				case cur.Usable():
					if bootedAt.IsZero() {
						bootedAt = time.Now()
					}
					if time.Since(bootedAt) >= settle {
						return cur, nil
					}
				default: // resuming: still replaying history, restart settle
					bootedAt = time.Time{}
				}
			case seen:
				// Left the roster after appearing: the worker exited/settled. For
				// a dispatch resume this is a terminal crash, not the CLI race.
				if reason == "" {
					if js, e := ReadJobState(short); e == nil && strings.TrimSpace(js.Detail) != "" {
						reason = strings.TrimSpace(js.Detail)
					}
				}
				return Session{}, fmt.Errorf("resumed worker exited during startup%s", reasonSuffix(reason))
			}
		}
		if time.Now().After(deadline) {
			switch {
			case seen && last.Usable():
				return last, nil // usable, just not settled yet
			case seen:
				return Session{}, fmt.Errorf("resumed worker %s did not stabilize within %s (last state=%q)", short, timeout, last.State)
			default:
				return Session{}, fmt.Errorf("resumed worker %s never registered with the daemon within %s", short, timeout)
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func reasonSuffix(reason string) string {
	if strings.TrimSpace(reason) == "" {
		return ""
	}
	return ": " + reason
}

// cwdMissing reports whether a session's saved cwd is gone (the most common
// resume crash: a deleted .claude/worktrees/... dir). An empty cwd is treated as
// present (the daemon will fall back to a default).
func cwdMissing(cwd string) bool {
	if strings.TrimSpace(cwd) == "" {
		return false
	}
	fi, err := os.Stat(cwd)
	return err != nil || !fi.IsDir()
}

// ResumeInPlace resumes a not-running session by its short via the daemon
// dispatch op: under its own short, same sessionId, no fork and no duplicate
// entry. It validates the saved cwd up front (a deleted worktree is the most
// common crash and is not worth a spawn), verifies the worker comes up usable,
// and on failure stops the worker so no crashed/idle orphan is left behind. A
// non-empty model relaunches the worker on that model, overriding any --model
// the session was originally started with; empty keeps the saved flags as-is.
// It returns ErrNoJobState when the session has no on-disk state (caller should
// fall back to ResumeByCLI).
func (c *Client) ResumeInPlace(short, model string, dangerous bool) (ResumeOutcome, error) {
	js, err := ReadJobState(short)
	if err != nil {
		return ResumeOutcome{}, err // ErrNoJobState or a read/parse error
	}
	if cwdMissing(js.Cwd) {
		return ResumeOutcome{}, fmt.Errorf("the session's working directory no longer exists (%s) — likely a deleted worktree; cannot resume, start a fresh session instead", js.Cwd)
	}

	const maxAttempts = 2
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			time.Sleep(1500 * time.Millisecond)
		}
		rshort, derr := c.DispatchResume(short, js, model, dangerous)
		if derr != nil {
			lastErr = derr // a refused dispatch (e.g. ESTALE) is worth one retry
			continue
		}
		live, werr := c.awaitResumed(rshort, 30*time.Second, 3*time.Second)
		if werr == nil {
			return ResumeOutcome{Short: rshort, State: live.State, InPlace: true}, nil
		}
		lastErr = werr
		_ = Stop(rshort) // normalise the failed worker back to not-running; no orphan
	}
	return ResumeOutcome{}, lastErr
}

// Resumable reports whether a not-running session can be brought back live in
// place: it has on-disk job state carrying a session id and its saved working
// directory still exists. This is what distinguishes an exited-but-resumable
// session (continue it, keeping its context) from a really-dead one (fork it or
// start fresh). A transcript is deliberately not required — the dispatch path
// resumes even never-prompted sessions.
func Resumable(short string) bool {
	js, err := ReadJobState(short)
	if err != nil {
		return false
	}
	sid := js.ResumeSessionID
	if sid == "" {
		sid = js.SessionID
	}
	return sid != "" && !cwdMissing(js.Cwd)
}

// EnsureLive returns the session ready to receive input, transparently resuming
// it in place first when it is not running — mirroring the app, where typing
// into an exited session brings it back with its history. A live session is
// returned as-is. The resumed session is additionally waited to its REPL prompt
// so a follow-up submit/keystroke lands in a ready input box, not a booting
// screen.
//
// A resume can come up on the CLI's resume-return dialog instead of at the
// prompt, and that dialog owns the keyboard: anything typed next answers it
// rather than reaching the input box. So the dialog is settled here, per the
// caller's choice, before the session is handed back as ready — see
// SettleResumeDialog. The returned note describes what was done to it, for the
// tool result.
func (c *Client) EnsureLive(sess Session, dialog ResumeDialogChoice) (Session, string, error) {
	if sess.Live {
		return sess, "", nil
	}
	out, err := c.resumeAny(sess)
	if err != nil {
		return Session{}, "", err
	}
	if err := c.WaitInteractive(out.Short, 20*time.Second); err != nil {
		return Session{}, "", fmt.Errorf("session %s was resumed but never settled at its prompt: %w", out.Short, err)
	}
	note, derr := c.SettleResumeDialog(out.Short, dialog)
	if derr != nil {
		return Session{}, "", fmt.Errorf("session %s was resumed but %w", out.Short, derr)
	}
	live, rerr := c.Resolve(out.Short)
	if rerr != nil || !live.Live {
		// The roster listed it moments ago (ResumeInPlace verified that); fall
		// back to the outcome rather than failing the delivery.
		live = sess
		live.Live, live.State = true, out.State
	}
	return live, note, nil
}

// resumeAny brings a not-running session back by whichever route its on-disk
// state allows, so callers do not have to know which kind of session they hold:
//
//   - listed, with job state: resumed in place via the daemon (own short, same
//     session id, no fork or duplicate entry);
//   - listed but without job state, or not listed at all — an orphan found by
//     its transcript alone: resurrected by session id through the CLI, launched
//     in the session's own working directory and registered under its recovered
//     name.
func (c *Client) resumeAny(sess Session) (ResumeOutcome, error) {
	if sess.Short != "" {
		out, err := c.ResumeInPlace(sess.Short, "", false)
		if !errors.Is(err, ErrNoJobState) {
			if err != nil {
				return ResumeOutcome{}, fmt.Errorf("session %s is not running and auto-resume failed: %w", sess.Short, err)
			}
			return out, nil
		}
	}
	if sess.SessionID == "" {
		return ResumeOutcome{}, fmt.Errorf("session is not running and has neither job state nor a session id to resume by")
	}
	out, err := c.ResumeByCLI(sess.SessionID, sess.Cwd, "", sess.Name, false)
	if err != nil {
		return ResumeOutcome{}, fmt.Errorf("session %s is not running and could not be resurrected from its transcript: %w", sess.SessionID, err)
	}
	return out, nil
}

// ResumeByCLI is the fallback for sessions with no on-disk job state — ones no
// longer in the agents list, or never in it: it runs `claude --bg --resume
// <sessionID>` in the session's own working directory, verifies liveness, and
// removes the worker on failure so nothing is left behind.
//
// name is the display name to register for the resurrected session. Without it
// a session resumed this way carries no name in the store: while its process
// runs the view derives a title from the transcript so it looks fine, but once
// it exits the entry falls back to showing the session kind ("bg") — a session
// with hundreds of records of real history, listed as nothing. Recovering the
// name from the transcript and registering it is what makes a resurrected
// session indistinguishable from one this server created.
func (c *Client) ResumeByCLI(sessionID, cwd, model, name string, dangerous bool) (ResumeOutcome, error) {
	// A transcript is keyed to the directory its session ran in, so the missing
	// directory is worth naming plainly. Left to the CLI it surfaces as a worker
	// that crashes at startup and an entry to clean up by hand.
	switch {
	case strings.TrimSpace(cwd) == "":
		return ResumeOutcome{}, fmt.Errorf("session %s has no recoverable working directory, and `--resume` looks a conversation up relative to the launch directory — it cannot be resumed without one", sessionID)
	case cwdMissing(cwd):
		return ResumeOutcome{}, fmt.Errorf("session %s cannot be resumed: its working directory no longer exists (%s) — recreate that directory (a deleted worktree is the usual cause) and resume again, or fork the session instead", sessionID, cwd)
	}
	out, err := Resume(sessionID, cwd, model, name, dangerous)
	if err != nil {
		return ResumeOutcome{}, err
	}
	short := ParseShortID(out)
	if short == "" {
		return ResumeOutcome{}, fmt.Errorf("resumed but could not parse the new short id from output:\n%s", out)
	}
	live, werr := c.WaitLive(short, 45*time.Second, 5*time.Second)
	if werr != nil {
		// A resume that never came up leaves an entry behind — nameless, in a
		// failed state, and with no transcript of its own to recover it from.
		// Remove it rather than leaving junk to be cleaned up by hand.
		_ = Remove(short)
		return ResumeOutcome{}, fmt.Errorf("%w. The spawned worker %s was removed so it is not left behind as a nameless failed entry; the session's transcript on disk is untouched", werr, short)
	}
	if name != "" {
		// Best-effort: the session is live and usable either way, and a missing
		// name is not worth failing a recovered conversation over.
		_ = c.Rename(short, sessionID, cwd, name)
	}
	return ResumeOutcome{Short: short, State: live.State, InPlace: false}, nil
}
