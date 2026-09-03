package agents

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Permission modes, spelled the way Claude Code spells them (its
// EXTERNAL_PERMISSION_MODES, plus the ant-only "auto").
const (
	ModeDefault     = "default"
	ModeAcceptEdits = "acceptEdits"
	ModePlan        = "plan"
	ModeBypass      = "bypassPermissions"
	ModeDontAsk     = "dontAsk"
	ModeAuto        = "auto"
)

// modeAliases maps everything a caller might reasonably type to the canonical
// mode name the CLI accepts after --permission-mode.
var modeAliases = map[string]string{
	"default":            ModeDefault,
	"normal":             ModeDefault,
	"ask":                ModeDefault,
	"acceptedits":        ModeAcceptEdits,
	"accept-edits":       ModeAcceptEdits,
	"accept_edits":       ModeAcceptEdits,
	"accept":             ModeAcceptEdits,
	"edits":              ModeAcceptEdits,
	"plan":               ModePlan,
	"bypasspermissions":  ModeBypass,
	"bypass":             ModeBypass,
	"bypass-permissions": ModeBypass,
	"dangerous":          ModeBypass,
	"yolo":               ModeBypass,
	"dontask":            ModeDontAsk,
	"dont-ask":           ModeDontAsk,
	"auto":               ModeAuto,
}

// ParsePermissionMode resolves a caller-supplied mode name to its canonical
// spelling. It is deliberately lenient about case, dashes and underscores: the
// CLI itself is not, and a rejected --permission-mode value costs a respawn.
func ParsePermissionMode(s string) (string, error) {
	key := strings.ToLower(strings.TrimSpace(s))
	key = strings.ReplaceAll(key, " ", "")
	if m, ok := modeAliases[key]; ok {
		return m, nil
	}
	return "", fmt.Errorf("unknown permission mode %q (want one of: default, acceptEdits, plan, bypassPermissions, dontAsk, auto)", s)
}

// modeIndicator maps a permission mode to the text Claude Code prints in its
// prompt footer for that mode. The footer renders
// `{symbol} {permissionModeTitle(mode).toLowerCase()} on (shift+tab to cycle)`;
// the hint in parentheses is dropped when the footer is carrying other items, so
// only the "<title> on" part can be relied on.
//
// The default mode calls itself "manual" in the footer — verified against a live
// 2.1.259 worker, whose carousel reads
//
//	bypass permissions → auto mode → manual mode → accept edits → plan mode → …
//
// with the bypass step present only for a bypass-capable worker (a session
// launched `--permission-mode auto` cycles the same ring with that one step
// missing). Older builds printed nothing at all in the default mode, so a screen
// with no indicator is reported as "not seen" rather than assumed to be default.
var modeIndicator = map[string]string{
	ModeDefault:     "manual mode on",
	ModeAcceptEdits: "accept edits on",
	ModePlan:        "plan mode on",
	ModeBypass:      "bypass permissions on",
	ModeDontAsk:     "don't ask on",
	ModeAuto:        "auto mode on",
}

// ScreenPermissionMode reads the permission mode off a session's screen.
//
// The screen we get is a stream tail, not a single repaint, so it holds every
// footer the session has drawn — the current mode is the LAST indicator in it,
// not the first, and a long tail is actively misleading: it is full of stale
// footers, which is why readMode asks for a short one.
//
// explicit reports whether an indicator was actually found. A screen with none
// is not evidence of the default mode: current builds label that mode "manual"
// in the footer, so no indicator means the footer was not in the captured tail
// (or this is an older build that printed nothing in default). Callers must
// treat !explicit as "unknown" and read again rather than act on the mode.
func ScreenPermissionMode(screen string) (mode string, explicit bool) {
	lower := strings.ToLower(screen)
	best := -1
	found := ModeDefault
	for m, indicator := range modeIndicator {
		if i := strings.LastIndex(lower, indicator); i > best {
			best, found = i, m
		}
	}
	if best < 0 {
		return ModeDefault, false
	}
	return found, true
}

// BypassAvailableFromFlags reports whether a worker launched with these flags
// can reach bypassPermissions at runtime — by shift+tab, or over the SDK /
// bridge control channel.
//
// Claude Code computes this once at startup (permissionSetup.ts, the
// isBypassPermissionsModeAvailable assignment) as
//
//	(permissionMode === 'bypassPermissions' || allowDangerouslySkipPermissions)
//	  && !disabledByGate && !disabledBySettings
//
// and from then on the flag only ever goes false (createDisabledBypassPermissions
// Context). So the launch flags decide it for the whole life of the worker:
// --dangerously-skip-permissions and --permission-mode bypassPermissions start
// IN bypass, --allow-dangerously-skip-permissions makes bypass reachable
// without starting in it. Any other launch — --permission-mode auto included —
// can never reach bypass, whatever is typed at it.
//
// The org-policy kill switches (the Statsig gate and
// permissions.disableBypassPermissionsMode) are not visible from here; they can
// only make the answer more restrictive, never less.
func BypassAvailableFromFlags(flags []string) bool {
	for i := 0; i < len(flags); i++ {
		switch flags[i] {
		case "--dangerously-skip-permissions", "--allow-dangerously-skip-permissions",
			"--permission-mode=" + ModeBypass:
			return true
		case "--permission-mode":
			if i+1 < len(flags) && flags[i+1] == ModeBypass {
				return true
			}
		}
	}
	return false
}

// ApplyModeToFlags rewrites a launch-flag list so a worker started with it comes
// up in mode. Every permission flag already present is dropped first, so the
// result never carries two conflicting ones — the bug that made a "dangerous"
// resume of a session launched with `--permission-mode auto` come back up in
// auto, because the old code saw a --permission-mode token and left it alone.
//
// allowBypass additionally passes --allow-dangerously-skip-permissions, which
// puts bypassPermissions in the worker's shift+tab carousel without activating
// it — the difference between a session whose mode can be raised later without a
// restart and one whose mode is fixed until it is respawned. It is redundant for
// mode == bypassPermissions (already in bypass) and skipped there.
//
// The input slice is never mutated.
func ApplyModeToFlags(flags []string, mode string, allowBypass bool) []string {
	out := make([]string, 0, len(flags)+2)
	for i := 0; i < len(flags); i++ {
		f := flags[i]
		if f == "--permission-mode" {
			i++ // drop the value too
			continue
		}
		if strings.HasPrefix(f, "--permission-mode=") {
			continue
		}
		if f == "--dangerously-skip-permissions" || f == "--allow-dangerously-skip-permissions" {
			continue
		}
		out = append(out, f)
	}
	if mode == ModeBypass {
		return append(out, "--dangerously-skip-permissions")
	}
	out = append(out, "--permission-mode", mode)
	if allowBypass {
		out = append(out, "--allow-dangerously-skip-permissions")
	}
	return out
}

// SetJobPermissionMode rewrites the permission flags in a session's on-disk job
// state (~/.claude/jobs/<short>/state.json `respawnFlags`), which is where the
// daemon reads a worker's launch flags from when it (re)spawns it. Changing them
// is what makes a restart come up in a different mode without `claude rm`: the
// session keeps its short, its session id, its history and its worktree.
//
// It returns the flag list that was written. A session with no job state on disk
// reports ErrNoJobState so the caller can say so plainly rather than silently
// doing nothing.
func SetJobPermissionMode(short, mode string, allowBypass bool) ([]string, error) {
	if short == "" {
		return nil, fmt.Errorf("empty session id")
	}
	dir, err := jobsDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, short, "state.json")
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNoJobState
		}
		return nil, err
	}
	// Decode into a generic map so every field the daemon owns survives the
	// write; only respawnFlags and updatedAt are ours to touch.
	var state map[string]any
	if err := json.Unmarshal(b, &state); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var current []string
	if raw, ok := state["respawnFlags"].([]any); ok {
		for _, v := range raw {
			if s, ok := v.(string); ok {
				current = append(current, s)
			}
		}
	}
	next := ApplyModeToFlags(current, mode, allowBypass)
	state["respawnFlags"] = next
	state["updatedAt"] = time.Now().UTC().Format(time.RFC3339)
	if err := writeJSONAtomic(path, state); err != nil {
		return nil, err
	}
	touchState(short)
	return next, nil
}

// maxCycleSteps caps how far the shift+tab carousel is driven before giving up.
// The carousel is at most five modes long (default, acceptEdits, plan,
// bypassPermissions, auto), so anything beyond a full lap means the keystroke is
// not landing where we think it is.
const maxCycleSteps = 6

// cycleSettle is how long to wait after a shift+tab before reading the mode
// back. The keystroke is delivered fire-and-forget and the worker then repaints
// its footer; reading too early reports the mode from before the press, which
// walks the carousel one step behind and lands the session in the wrong mode.
const cycleSettle = 1200 * time.Millisecond

// screenSettleReads is how many times the screen is re-read while it still fails
// to show a mode indicator (or still shows the pre-keystroke one) before giving
// up. A session that is mid-turn keeps painting output, so the repaint carrying
// the footer can take a moment to land.
const screenSettleReads = 4

// readTails are the stream-tail sizes tried in order when reading the mode. A
// short tail is the accurate one — it holds only the freshest repaints — but it
// can miss the footer entirely on a session that is busy printing, so longer
// tails follow as a fallback.
var readTails = []int{6, 30, 120}

// CyclePermissionMode drives a live session's shift+tab carousel until it sits
// in target, and returns the modes it passed through.
//
// shift+tab is not a UI-only toggle: its handler (PromptInput's handleCycleMode)
// runs cyclePermissionMode and writes the result to both the app state and the
// tool permission context, then rechecks every queued permission prompt — the
// same context every tool call is authorised against. The keystroke is CSI Z
// ("\x1b[Z"), which Claude Code's key parser reads as tab with shift set.
//
// The carousel is walked one step at a time, reading the mode back off the
// screen after each press, rather than computing a step count up front: the step
// count depends on runtime facts we cannot see from here (whether bypass and
// auto are in this worker's carousel at all), and a miscount would leave the
// session in a mode nobody asked for. If target turns out to be unreachable the
// session is walked back to where it started before the error is returned.
func (c *Client) CyclePermissionMode(short, target string) ([]string, error) {
	start, explicit := c.readMode(short, "")
	if !explicit {
		return nil, fmt.Errorf("could not read %s's current permission mode from its screen, so the carousel cannot be walked safely — read_screen to see what it is showing (a modal owns the keyboard until it is cleared)", short)
	}
	if start == target {
		return []string{start}, nil
	}
	visited := []string{start}
	cur := start
	for step := 0; step < maxCycleSteps; step++ {
		next, err := c.pressCycle(short, cur)
		if err != nil {
			return visited, err
		}
		visited = append(visited, next)
		if next == target {
			return visited, nil
		}
		if next == cur {
			return visited, fmt.Errorf("shift+tab did not change the mode (still %s) — the session may be showing a dialog that owns the keyboard; read_screen and clear it first", cur)
		}
		cur = next
		if cur == start {
			break // a full lap without hitting target
		}
	}
	// Unreachable. Walk back to where we started so the session is left as found.
	for step := 0; step < maxCycleSteps && cur != start; step++ {
		next, err := c.pressCycle(short, cur)
		if err != nil {
			break
		}
		cur = next
	}
	return visited, fmt.Errorf("%s is not in this session's shift+tab carousel (it cycles %s); left it in %s", target, strings.Join(visited, " → "), cur)
}

// pressCycle sends one shift+tab and reports the mode the session lands in.
func (c *Client) pressCycle(short, from string) (string, error) {
	if _, err := c.SendKeys(short, []string{"shift-tab"}, false); err != nil {
		return "", err
	}
	time.Sleep(cycleSettle)
	mode, explicit := c.readMode(short, from)
	if !explicit {
		return "", fmt.Errorf("could not read %s's permission mode back after shift+tab — its footer never showed one; read_screen to see what it is doing", short)
	}
	return mode, nil
}

// readMode reads the current mode off the screen, retrying while the footer is
// missing from the captured tail or still reports differsFrom (pass "" to accept
// the first mode read). It returns whether a mode indicator was actually seen —
// a false here means the mode is unknown, not that it is default.
func (c *Client) readMode(short, differsFrom string) (string, bool) {
	var mode string
	var explicit bool
	for i := 0; i < screenSettleReads; i++ {
		if i > 0 {
			time.Sleep(500 * time.Millisecond)
		}
		for _, tail := range readTails {
			screen, err := c.ReadScreen(short, tail)
			if err != nil {
				continue
			}
			m, seen := ScreenPermissionMode(screen)
			if !seen {
				continue // footer not in this tail; try a longer one
			}
			mode, explicit = m, true
			if differsFrom == "" || mode != differsFrom {
				return mode, true
			}
			break // a stale read: wait and take the whole ladder again
		}
	}
	return mode, explicit
}

// PermissionOutcome describes what SetPermissionMode did to a session.
type PermissionOutcome struct {
	Mode      string   // the mode the session is in now (or will start in)
	Previous  string   // the mode it was in before, "" when it was not running
	Path      []string // modes walked through when the carousel was used
	Restarted bool     // the worker was respawned to take the new mode
	Live      bool     // the session is running now
	Flags     []string // launch flags left in the session's job state
}

// settingsDefaultModeBypass reports whether ~/.claude/settings.json asks for
// bypassPermissions by default. That path grants bypass availability at startup
// exactly like the CLI flag does (initialPermissionModeFromCLI folds
// settings.permissions.defaultMode into the same ordered mode list), so a
// session can be bypass-capable with no permission flag on its command line.
func settingsDefaultModeBypass() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	b, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		return false
	}
	var s struct {
		Permissions struct {
			DefaultMode string `json:"defaultMode"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return false
	}
	return s.Permissions.DefaultMode == ModeBypass
}

// BypassAvailable reports whether a session can be switched into
// bypassPermissions without respawning it.
//
// The answer comes from the session's saved respawnFlags, NOT from the worker's
// command line. A background worker is not exec'd with its own flags at all: the
// daemon keeps a pool of pre-warmed processes and hands one over per session, so
// every worker's command line reads
//
//	claude bg-pty-host --bg-pty-host …/<id>.pty.sock 200 50 -- <binary> --bg-spare …/<id>.claim.sock
//
// whatever mode it is running in — a bypass session and an auto session look
// identical there. Checking with `ps | grep` therefore proves nothing in either
// direction; the flags the daemon actually applies when it claims a spare are
// the ones in the job state.
func (c *Client) BypassAvailable(sess Session) bool {
	if js, err := ReadJobState(sess.Short); err == nil && BypassAvailableFromFlags(js.RespawnFlags) {
		return true
	}
	return settingsDefaultModeBypass()
}

// waitNotLive polls until the session has left the daemon roster, so its job
// state can be rewritten without the daemon writing over it and so a dispatch
// resume is not refused for a session that is still running.
func (c *Client) waitNotLive(short string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		live, err := c.listDaemon()
		if err == nil {
			running := false
			for _, j := range live {
				if j.Short == short {
					running = true
					break
				}
			}
			if !running {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("session %s was still running %s after stop", short, timeout)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// SetPermissionMode puts a session into mode.
//
// A live session is switched in place through its shift+tab carousel, which
// changes the permission contract itself and costs nothing — no restart, no lost
// context. That only reaches the modes the worker's carousel actually holds:
// bypassPermissions is in it only when the worker was LAUNCHED bypass-capable,
// because Claude Code fixes that at startup and never grants it later (see
// BypassAvailableFromFlags). dontAsk is never in it — the carousel skips it.
//
// When the target is out of carousel reach, restart says whether to respawn the
// worker with rewritten launch flags: stop, rewrite respawnFlags in the job
// state, dispatch it back in place. The session keeps its short, session id,
// history and worktree — `claude rm` is not involved, so its worktree guards
// ("uncommitted changes", "commits that are not pushed anywhere") never come up.
// The turn in flight is lost, which is why this is opt-in.
//
// allowBypass adds --allow-dangerously-skip-permissions to the rewritten flags:
// bypass lands in the carousel without being switched on, so the NEXT change of
// mode on this session needs no restart at all.
func (c *Client) SetPermissionMode(sess Session, mode string, restart, allowBypass bool) (PermissionOutcome, error) {
	out := PermissionOutcome{Mode: mode, Live: sess.Live}
	if sess.Short == "" {
		return out, fmt.Errorf("session has no short id; cannot address its job state or its keyboard")
	}

	if !sess.Live {
		flags, err := SetJobPermissionMode(sess.Short, mode, allowBypass)
		if err != nil {
			return out, err
		}
		out.Flags = flags
		if !restart {
			return out, nil
		}
		res, err := c.ResumeInPlace(sess.Short, "", false)
		if err != nil {
			return out, fmt.Errorf("wrote the new mode to the session's launch flags but resuming it failed: %w", err)
		}
		out.Restarted, out.Live = true, true
		_ = res
		return out, nil
	}

	current, explicit := c.readMode(sess.Short, "")
	if !explicit {
		return out, fmt.Errorf("could not read %s's current permission mode from its screen — read_screen to see what it is showing; a modal dialog owns the keyboard until it is cleared", sess.Short)
	}
	out.Previous = current
	if current == mode {
		out.Path = []string{current}
		// Still persist, so a daemon respawn does not silently revert the mode.
		if flags, err := SetJobPermissionMode(sess.Short, mode, allowBypass); err == nil {
			out.Flags = flags
		}
		return out, nil
	}

	inCarousel := mode != ModeDontAsk && (mode != ModeBypass || c.BypassAvailable(sess))
	if inCarousel {
		path, err := c.CyclePermissionMode(sess.Short, mode)
		out.Path = path
		if err == nil {
			if flags, ferr := SetJobPermissionMode(sess.Short, mode, allowBypass); ferr == nil {
				out.Flags = flags
			}
			return out, nil
		}
		if !restart {
			return out, err
		}
	} else if !restart {
		return out, fmt.Errorf("this session cannot reach %s without a restart: it was not launched bypass-capable, and Claude Code fixes that at startup (isBypassPermissionsModeAvailable) — shift+tab skips straight from plan to default, and the SDK and bridge channels both reject the switch with \"the session was not launched with --dangerously-skip-permissions\". Re-run with restart=true to respawn the worker in %s (keeps the session, its history and its worktree; the turn in flight is lost)", mode, mode)
	}

	// Restart path: stop, rewrite the launch flags, dispatch it back in place.
	if err := Stop(sess.Short); err != nil {
		return out, fmt.Errorf("could not stop %s to restart it in %s: %w", sess.Short, mode, err)
	}
	if err := c.waitNotLive(sess.Short, 20*time.Second); err != nil {
		return out, err
	}
	flags, err := SetJobPermissionMode(sess.Short, mode, allowBypass)
	if err != nil {
		return out, fmt.Errorf("stopped %s but could not rewrite its launch flags: %w", sess.Short, err)
	}
	out.Flags = flags
	res, err := c.ResumeInPlace(sess.Short, "", false)
	if err != nil {
		return out, fmt.Errorf("stopped %s and set its launch flags to %s, but bringing it back failed: %w. Its history is intact — resume_session retries it", sess.Short, mode, err)
	}
	out.Restarted, out.Live = true, true
	_ = res
	return out, nil
}
