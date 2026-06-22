package agents

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
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
		return out, nil
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
	return out, nil
}

// Resolve finds a session by exact short id / session id / name, then falls
// back to a short-id or session-id prefix match.
func (c *Client) Resolve(ref string) (Session, error) {
	if ref == "" {
		return Session{}, fmt.Errorf("empty session reference")
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
