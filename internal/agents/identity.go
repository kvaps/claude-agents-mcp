package agents

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Environment variables Claude Code sets for every process it spawns inside a
// session — including the MCP servers it starts over stdio. They are how this
// server knows which session is calling it:
//
//   - envSessionID carries the full session UUID of the calling session;
//   - envJobDir points at ~/.claude/jobs/<short>, so its base name is the short
//     id of a background session (a short id is the first 8 hex digits of the
//     session id, which is also this directory's name).
//
// The env is inherited from the session's own worker process, so it identifies
// the caller without a parameter to pass — and without a parameter to get wrong:
// an agent cannot claim to be a session it is not.
const (
	envSessionID = "CLAUDE_CODE_SESSION_ID"
	envJobDir    = "CLAUDE_JOB_DIR"
)

// Self is the identity of the session this MCP server was started by: who "I"
// am when an agent asks, and the address other agents use to reach me.
type Self struct {
	Short     string `json:"short"`      // short id, when the caller is a background (fleet) session
	SessionID string `json:"session_id"` // full session UUID, from the environment
	Name      string `json:"name"`       // display name in the agents view, when it has one
	Cwd       string `json:"cwd"`        // working directory, when it could be resolved
	Live      bool   `json:"live"`       // present in the daemon roster right now
	// Addressable reports whether other agents can send messages back here:
	// a fleet session with a short id can receive them, anything else cannot.
	Addressable bool `json:"addressable"`
}

// Known reports whether the caller could be identified at all. It is false when
// the server was started outside a Claude Code session (a bare shell, a test),
// where there is no session to speak for.
func (s Self) Known() bool { return s.SessionID != "" || s.Short != "" }

// Address is how another agent addresses this session in send_message: the short
// id, which is unique, falling back to the session id for a session that has no
// short (not a background session). Empty when the caller is unidentified.
func (s Self) Address() string {
	if s.Short != "" {
		return s.Short
	}
	return s.SessionID
}

// Label renders the identity the way it appears in a message envelope:
// `name [short]`, or just the address when the session has no display name.
func (s Self) Label() string { return label(s.Name, s.Address()) }

// label formats a `name [address]` pair, degrading to whichever half exists.
func label(name, address string) string {
	name = strings.TrimSpace(name)
	switch {
	case name == "" && address == "":
		return "unknown"
	case name == "":
		return address
	case address == "":
		return name
	}
	return fmt.Sprintf("%s [%s]", name, address)
}

// envSelf reads the raw identity out of the environment, with no daemon lookup:
// the session id as given, and the short id from the job directory's name. A
// background session has both; a session id without a job dir still yields the
// session id, which is enough to look the rest up.
func envSelf() Self {
	s := Self{SessionID: strings.TrimSpace(os.Getenv(envSessionID))}
	if dir := strings.TrimSpace(os.Getenv(envJobDir)); dir != "" {
		s.Short = filepath.Base(filepath.Clean(dir))
	}
	// A job dir base that is not a short id (an unusual layout) is not a usable
	// address; the session id remains.
	if s.Short != "" && !shortID.MatchString(s.Short) {
		s.Short = ""
	}
	if s.Short == "" && IsFullSessionID(s.SessionID) {
		// Every background session's short is the first 8 hex digits of its
		// session id; whether one actually exists is settled by the roster below.
		s.Short = s.SessionID[:8]
	}
	return s
}

// Whoami identifies the session that called this server: its short id, session
// id, display name and working directory, plus whether it is reachable by other
// agents. The identity comes from the environment Claude Code gave the server
// process (see envSessionID/envJobDir) and is enriched from the agents list, so
// the name is the one the agents view shows.
//
// A caller the daemon does not list (an interactive session, or one that is not
// a background job) keeps whatever the environment said and is reported as not
// addressable: nothing can deliver a message to a session with no live worker
// and no job state, and claiming otherwise would invite replies into a void.
func (c *Client) Whoami() Self {
	self := envSelf()
	if !self.Known() {
		return self
	}
	for _, ref := range []string{self.Short, self.SessionID} {
		if ref == "" {
			continue
		}
		sess, err := c.Resolve(ref)
		if err != nil {
			continue
		}
		self.Short = sess.Short
		if sess.SessionID != "" {
			self.SessionID = sess.SessionID
		}
		self.Name, self.Cwd, self.Live = sess.Name, sess.Cwd, sess.Live
		self.Addressable = sess.Short != "" && (sess.Live || sess.Resumable)
		return self
	}
	// Not in the agents list: keep the environment's view, but do not advertise
	// an address for it.
	self.Addressable = false
	return self
}
