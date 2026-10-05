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
//
// envMailbox is the counterpart for a caller that is not a Claude Code session
// at all — an MCP client such as Codex. Set in that client's server
// configuration, it names the mailbox the process answers to. It is still the
// server process's own environment, not a tool parameter, so the identity is
// fixed by whoever configured the client rather than claimed per call.
const (
	envSessionID = "CLAUDE_CODE_SESSION_ID"
	envJobDir    = "CLAUDE_JOB_DIR"
	envMailbox   = "CLAUDE_AGENTS_MAILBOX"
)

// Kinds of caller. A session receives messages in its conversation; a client
// receives them in a mailbox it reads; an unknown caller receives nothing.
const (
	KindSession = "session"
	KindClient  = "client"
	KindUnknown = "unknown"
)

// Self is the identity of the process this MCP server was started by: who "I"
// am when an agent asks, and the address other agents use to reach me.
type Self struct {
	Kind      string `json:"kind"`                 // session, client or unknown — see the Kind constants
	Short     string `json:"short,omitempty"`      // short id, when the caller is a background (fleet) session
	SessionID string `json:"session_id,omitempty"` // full session UUID, from the environment
	Name      string `json:"name,omitempty"`       // display name in the agents view, when it has one
	Cwd       string `json:"cwd,omitempty"`        // working directory, when it could be resolved
	Mailbox   string `json:"mailbox,omitempty"`    // the mailbox a client caller reads
	Live      bool   `json:"live"`                 // present in the daemon roster right now (sessions only)
	// Addressable reports whether other agents can send messages back here:
	// a fleet session with a short id can receive them in its conversation, a
	// client with an existing mailbox can read them there, anything else cannot.
	Addressable bool `json:"addressable"`
}

// Known reports whether the caller could be identified at all. It is false when
// the server was started outside a Claude Code session and no mailbox was
// configured or registered (a bare shell, a test), where there is nobody to
// speak for.
func (s Self) Known() bool { return s.SessionID != "" || s.Short != "" || s.Mailbox != "" }

// IsClient reports whether the caller is an MCP client with a mailbox rather
// than a Claude Code session.
func (s Self) IsClient() bool { return s.Mailbox != "" }

// Address is how another agent addresses this caller in send_message: the
// mailbox name for a client; for a session the short id, which is unique,
// falling back to the session id for a session that has no short (not a
// background session). Empty when the caller is unidentified.
func (s Self) Address() string {
	switch {
	case s.Mailbox != "":
		return s.Mailbox
	case s.Short != "":
		return s.Short
	}
	return s.SessionID
}

// Label renders the identity the way it appears in a message envelope:
// `name [short]` for a session, or just the address when the session has no
// display name; a client is its mailbox name.
func (s Self) Label() string {
	if s.Mailbox != "" {
		return s.Mailbox
	}
	return label(s.Name, s.Address())
}

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

// envSelf reads the raw identity out of the environment, with no daemon lookup.
//
// An explicit mailbox (envMailbox) wins: it is configuration written by the
// person who set the client up, and configuration beats inference. Otherwise a
// Claude Code session is recognised by its session id and job dir — a
// background session has both; a session id without a job dir still yields the
// session id, which is enough to look the rest up. A process with neither is a
// client without configuration, and answers to the registered default mailbox
// if register_mailbox ever wrote one.
func envSelf() Self {
	if mb := strings.ToLower(strings.TrimSpace(os.Getenv(envMailbox))); mb != "" && ValidateMailboxName(mb) == nil {
		return Self{Kind: KindClient, Mailbox: mb}
	}
	s := Self{Kind: KindSession, SessionID: strings.TrimSpace(os.Getenv(envSessionID))}
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
	if s.Known() {
		return s
	}
	if def := DefaultClientMailbox(); def != "" {
		return Self{Kind: KindClient, Mailbox: def}
	}
	return Self{Kind: KindUnknown}
}

// Whoami identifies the caller of this server.
//
// For a session: its short id, session id, display name and working directory,
// plus whether it is reachable by other agents. The identity comes from the
// environment Claude Code gave the server process (see envSessionID/envJobDir)
// and is enriched from the agents list, so the name is the one the agents view
// shows. A caller the daemon does not list (an interactive session, or one that
// is not a background job) keeps whatever the environment said and is reported
// as not addressable: nothing can deliver a message to a session with no live
// worker and no job state, and claiming otherwise would invite replies into a
// void.
//
// For a client: its mailbox name, addressable once the mailbox exists on disk.
func (c *Client) Whoami() Self {
	self := envSelf()
	if self.IsClient() {
		mb, err := OpenMailbox(self.Mailbox)
		self.Addressable = err == nil && mb.Exists()
		return self
	}
	if !self.Known() {
		return self
	}
	if sessions, err := c.sessions(); err == nil {
		for _, sess := range sessions {
			byShort := self.Short != "" && strings.EqualFold(sess.Short, self.Short)
			bySID := self.SessionID != "" && strings.EqualFold(sess.SessionID, self.SessionID)
			if !byShort && !bySID {
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
	}
	// Not in the agents list: keep the environment's view, but do not advertise
	// an address for it.
	self.Addressable = false
	return self
}

// EnsureOwnMailbox creates the caller's mailbox if the caller is a client, so
// agents can address it from the moment the client's server process starts,
// before the client has read anything. It never touches the daemon and does
// nothing for a session or an unidentified caller.
func (c *Client) EnsureOwnMailbox() {
	if self := envSelf(); self.IsClient() {
		if mb, err := OpenMailbox(self.Mailbox); err == nil {
			_ = mb.Create()
		}
	}
}

// OwnMailbox returns the caller's mailbox, creating it if it does not exist
// yet, or an error explaining why this caller has none: a session receives
// messages in its conversation and an unidentified caller has to register one.
func (c *Client) OwnMailbox() (*Mailbox, error) {
	self := envSelf()
	switch {
	case self.IsClient():
		mb, err := OpenMailbox(self.Mailbox)
		if err != nil {
			return nil, err
		}
		if err := mb.Create(); err != nil {
			return nil, err
		}
		return mb, nil
	case self.Known():
		return nil, fmt.Errorf("this server was started by Claude Code session %s, which receives messages in its own conversation, not in a mailbox — there is nothing to read here; if you are an MCP client, set %s in the server's environment", self.Label(), envMailbox)
	default:
		return nil, fmt.Errorf("this server has no identity: it was not started by a Claude Code session and no mailbox is configured — set %s=<name> in the MCP server's environment, or call register_mailbox once to pick a default", envMailbox)
	}
}

// RegisterMailbox makes name the mailbox this process (and every later client
// process without an explicit CLAUDE_AGENTS_MAILBOX) answers to, creating it.
// A session cannot register one: it already has a conversation to receive in,
// and a mailbox nobody reads would swallow messages meant for it. A client whose
// mailbox is fixed by the environment can only register that same name.
func (c *Client) RegisterMailbox(name string) (*Mailbox, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if err := ValidateMailboxName(name); err != nil {
		return nil, err
	}
	self := envSelf()
	if self.Kind == KindSession && self.Known() {
		return nil, fmt.Errorf("this server was started by Claude Code session %s, which receives messages in its conversation; a session does not register a mailbox", self.Label())
	}
	if env := strings.ToLower(strings.TrimSpace(os.Getenv(envMailbox))); env != "" && env != name {
		return nil, fmt.Errorf("this process's mailbox is fixed to %q by %s; change the environment instead of registering %q", env, envMailbox, name)
	}
	mb, err := OpenMailbox(name)
	if err != nil {
		return nil, err
	}
	if err := mb.Create(); err != nil {
		return nil, err
	}
	if err := SetDefaultClientMailbox(name); err != nil {
		return nil, err
	}
	return mb, nil
}
