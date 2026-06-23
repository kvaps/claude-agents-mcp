package agents

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Client talks to the local `claude agents` daemon over its control socket.
// The socket is resolved on every call, so the client survives daemon restarts.
type Client struct{}

// NewClient returns a daemon client. It does not require the daemon to be
// running yet; the control socket is located lazily on each call.
func NewClient() *Client { return &Client{} }

// FindSocket locates the freshest control.sock for the current user.
func FindSocket() (string, error) {
	pattern := fmt.Sprintf("/tmp/cc-daemon-%d/*/control.sock", os.Getuid())
	matches, _ := filepath.Glob(pattern)
	if len(matches) == 0 {
		return "", fmt.Errorf("no control.sock matching %s — is `claude agents` running?", pattern)
	}
	sort.Slice(matches, func(i, j int) bool {
		fi, ei := os.Stat(matches[i])
		fj, ej := os.Stat(matches[j])
		if ei != nil || ej != nil {
			return false
		}
		return fi.ModTime().After(fj.ModTime())
	})
	return matches[0], nil
}

// request sends one newline-framed JSON request and returns the first reply line.
func (c *Client) request(payload map[string]any) ([]byte, error) {
	sock, err := FindSocket()
	if err != nil {
		return nil, err
	}
	conn, err := net.DialTimeout("unix", sock, 3*time.Second)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return nil, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(append(body, '\n')); err != nil {
		return nil, err
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil && len(line) == 0 {
		return nil, err
	}
	return []byte(line), nil
}

// listDaemon returns the live daemon roster (op:list) with rich state. Not-
// running sessions are not included here.
func (c *Client) listDaemon() ([]Session, error) {
	raw, err := c.request(map[string]any{"proto": 1, "op": "list"})
	if err != nil {
		return nil, err
	}
	var resp struct {
		OK    bool      `json:"ok"`
		Error string    `json:"error"`
		Jobs  []Session `json:"jobs"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("parse list reply: %w", err)
	}
	if !resp.OK {
		return nil, fmt.Errorf("daemon error: %s", resp.Error)
	}
	return resp.Jobs, nil
}

// List returns every session shown in the agents view — including not-running
// ones (via `claude agents --json --all`) — enriched with the live daemon state
// (state/tempo/detail/needs) and short id for the running ones. The daemon's
// op:list omits the display name and reports the repo cwd; the agents view
// supplies the name, short id and the real worktree cwd.
func (c *Client) List() ([]Session, error) {
	live, lerr := c.listDaemon()
	all, aerr := AgentsJSON(true)
	if lerr != nil && aerr != nil {
		return nil, lerr
	}

	rich := make(map[string]Session, len(live))
	for _, s := range live {
		rich[s.SessionID] = s
	}

	// No agents-view list available: return the live roster as-is.
	if aerr != nil {
		out := make([]Session, 0, len(live))
		for _, s := range live {
			s.Live = true
			out = append(out, s)
		}
		return markPinned(out), nil
	}

	out := make([]Session, 0, len(all))
	for _, a := range all {
		if r, ok := rich[a.SessionID]; ok {
			r.Live = true
			if r.Name == "" {
				r.Name = a.Name
			}
			if a.Cwd != "" {
				r.Cwd = a.Cwd
			}
			out = append(out, r)
			delete(rich, a.SessionID)
			continue
		}
		out = append(out, Session{
			Short:     a.ID,
			SessionID: a.SessionID,
			Name:      a.Name,
			Cwd:       a.Cwd,
			State:     a.StatusStr(),
			Live:      false,
		})
	}
	// Any live sessions that the agents view did not list (rare): append them.
	for _, s := range live {
		if _, leftover := rich[s.SessionID]; leftover {
			s.Live = true
			out = append(out, s)
		}
	}
	return markPinned(out), nil
}

// markPinned flags sessions present in the agents-view pin set (pins.json).
func markPinned(sessions []Session) []Session {
	pins := PinnedSet()
	if len(pins) == 0 {
		return sessions
	}
	for i := range sessions {
		if pins[sessions[i].Short] {
			sessions[i].Pinned = true
		}
	}
	return sessions
}

// idRef matches an 8-hex short id or a full session UUID — references that can
// be resolved from the daemon roster alone, without the slower CLI enrichment.
var idRef = regexp.MustCompile(`^[0-9a-fA-F]{8}(-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})?$`)

// Resolve finds a session by exact short id / session id / name, then falls
// back to a short-id or session-id prefix match.
func (c *Client) Resolve(ref string) (Session, error) {
	if ref == "" {
		return Session{}, fmt.Errorf("empty session reference")
	}
	// Fast path: a short id or full session id is resolvable from the daemon
	// roster alone (one socket round-trip), skipping `claude agents --json`.
	// Names and not-running sessions still need the full, enriched list below.
	if idRef.MatchString(ref) {
		if live, lerr := c.listDaemon(); lerr == nil {
			for _, j := range live {
				if j.Short == ref || j.SessionID == ref {
					j.Live = true
					return j, nil
				}
			}
		}
	}
	jobs, err := c.List()
	if err != nil {
		return Session{}, err
	}
	for _, j := range jobs {
		if j.Short == ref || j.SessionID == ref || j.Name == ref {
			return j, nil
		}
	}
	for _, j := range jobs {
		if j.Short != "" && strings.HasPrefix(j.Short, ref) {
			return j, nil
		}
		if strings.HasPrefix(j.SessionID, ref) {
			return j, nil
		}
	}
	return Session{}, fmt.Errorf("no session matching %q", ref)
}

// fullSessionID matches a complete session UUID — resumable directly via
// `claude --bg --resume` even when the session is not in the agents roster.
var fullSessionID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// IsFullSessionID reports whether ref is a complete session UUID.
func IsFullSessionID(ref string) bool { return fullSessionID.MatchString(ref) }

// WaitLive polls the daemon roster until a freshly-resumed worker reaches and
// holds a usable state for a settle window. `claude --bg --resume` hands back a
// short id immediately, but the worker then boots: it may sit in "resuming"
// (replaying history) or flicker through "crashed" while it runs its first turn
// before settling into a normal state — and it can also simply be retired by the
// daemon (the failure behind a resume that has "already exited" by the time you
// attach). WaitLive treats resuming/crashed as transient states to wait through,
// resetting the settle timer until the worker is genuinely usable. It returns:
//   - the live session once it has held a usable state through the settle window;
//   - a "retired" error if the worker left the roster after appearing — the
//     caller should retry the resume from a clean slate;
//   - a "never registered" / "did not stabilize" error otherwise.
//
// If the worker is usable but simply hasn't completed the settle window when the
// overall timeout hits, it is returned without error as a best-effort live result.
func (c *Client) WaitLive(short string, timeout, settle time.Duration) (Session, error) {
	deadline := time.Now().Add(timeout)
	seen := false
	var last Session
	var bootedAt time.Time
	for {
		if live, err := c.listDaemon(); err == nil {
			var cur Session
			found := false
			for _, j := range live {
				if j.Short == short {
					j.Live = true
					cur, found = j, true
					break
				}
			}
			switch {
			case found:
				seen, last = true, cur
				if cur.Usable() {
					if bootedAt.IsZero() {
						bootedAt = time.Now()
					}
					if time.Since(bootedAt) >= settle {
						return cur, nil // held a usable state through settle
					}
				} else {
					bootedAt = time.Time{} // resuming/crashed: still booting, restart settle
				}
			case seen:
				return Session{}, fmt.Errorf("resumed worker %s registered then was retired by the daemon — `claude --bg --resume` does this intermittently; retry the resume from a clean slate", short)
			}
		}
		if time.Now().After(deadline) {
			switch {
			case seen && last.Usable():
				return last, nil // usable, just not settled yet
			case seen:
				return Session{}, fmt.Errorf("resumed worker %s never stabilized within %s (last state=%q)", short, timeout, last.State)
			default:
				return Session{}, fmt.Errorf("resumed worker %s never registered with the daemon within %s", short, timeout)
			}
		}
		time.Sleep(400 * time.Millisecond)
	}
}

// EnsureNotLive stops any live workers for a session id and waits until none
// remain in the daemon roster, so a (re)resume starts from a clean slate and
// cannot collide with a leftover worker — which is what makes the daemon retire
// one of the pair. Best-effort: it returns once the roster is clear of this
// session or the timeout elapses.
func (c *Client) EnsureNotLive(sessionID string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for {
		live, err := c.listDaemon()
		if err != nil {
			return
		}
		any := false
		for _, j := range live {
			if j.SessionID == sessionID {
				_ = Stop(j.Short)
				any = true
			}
		}
		if !any || time.Now().After(deadline) {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// WaitIdle polls until the session is no longer actively working, or timeout.
func (c *Client) WaitIdle(short string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		s, err := c.Resolve(short)
		if err != nil {
			return err
		}
		if !s.Busy() {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("session %s still busy after %s", short, timeout)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// Snapshot opens a read-only subscription and returns the session record plus
// the current screen contents (raw PTY tail joined).
func (c *Client) Snapshot(short string, tail int) (Session, string, error) {
	if strings.TrimSpace(short) == "" {
		return Session{}, "", fmt.Errorf("session is not running (no live attach target)")
	}
	sock, err := FindSocket()
	if err != nil {
		return Session{}, "", err
	}
	conn, err := net.DialTimeout("unix", sock, 3*time.Second)
	if err != nil {
		return Session{}, "", err
	}
	defer func() { _ = conn.Close() }()
	req, err := json.Marshal(map[string]any{"proto": 1, "op": "subscribe", "short": short, "tail": tail})
	if err != nil {
		return Session{}, "", err
	}
	if _, err := conn.Write(append(req, '\n')); err != nil {
		return Session{}, "", err
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var f struct {
			Type       string   `json:"type"`
			Record     Session  `json:"record"`
			StreamTail []string `json:"streamTail"`
			Error      string   `json:"error"`
		}
		if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
			continue
		}
		if f.Error != "" {
			return Session{}, "", fmt.Errorf("daemon error: %s", f.Error)
		}
		if f.Type == "snapshot" {
			return f.Record, strings.Join(f.StreamTail, ""), nil
		}
	}
	if err := sc.Err(); err != nil {
		return Session{}, "", err
	}
	return Session{}, "", fmt.Errorf("no snapshot received for %s", short)
}
