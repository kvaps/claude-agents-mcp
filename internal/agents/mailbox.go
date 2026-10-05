package agents

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Delivery statuses. They are the same four words for every kind of recipient,
// so a sender never has to know what it wrote to in order to read the answer:
//
//   - queued:    accepted and stored, but not yet in front of the recipient — a
//     session recipient has it in its input box behind a running turn; a
//     mailbox recipient has not fetched it yet.
//   - delivered: in front of the recipient — a session recipient has it in its
//     conversation (confirmed against the transcript); a mailbox recipient has
//     fetched it with read_messages / wait_for_messages.
//   - read:      the mailbox recipient acknowledged it (ack_messages). Only a
//     mailbox can say this; a session gives no read receipt.
//   - failed:    not delivered, with a reason.
const (
	StatusQueued    = "queued"
	StatusDelivered = "delivered"
	StatusRead      = "read"
	StatusFailed    = "failed"
)

// envStateDir overrides where this server keeps its own state (mailboxes and
// the sent ledger). The default is ~/.claude/claude-agents-mcp, next to the
// Claude Code state it works alongside, so one `ls ~/.claude` finds it.
const envStateDir = "CLAUDE_AGENTS_MCP_STATE_DIR"

// StateDir returns the directory this server's own state lives in. It is a
// filesystem directory rather than anything in-process on purpose: every MCP
// client starts its own server process (Codex starts one, every Claude session
// starts one), so a mailbox written by one process has to be readable by
// another, and it has to survive all of them restarting.
func StateDir() (string, error) {
	if d := strings.TrimSpace(os.Getenv(envStateDir)); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "claude-agents-mcp"), nil
}

func mailboxesDir() (string, error) {
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "mailboxes"), nil
}

// mailboxName is what a mailbox may be called: lowercase, starts with a letter
// or digit, dots/dashes/underscores inside, at most 32 characters. Lowercase
// because addresses are matched case-insensitively anyway and a name that
// differs from another only by case would be two spellings of one address.
var mailboxName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)

// ValidateMailboxName reports why a name cannot be a mailbox address. Besides
// the character rules, a name that reads as a session short id is refused: an
// address is resolved against sessions and mailboxes together, and an 8-hex
// mailbox name would shadow or be shadowed by a session.
func ValidateMailboxName(name string) error {
	if !mailboxName.MatchString(name) {
		return fmt.Errorf("mailbox name %q is invalid: use 1-32 lowercase letters, digits, dots, dashes or underscores, starting with a letter or digit", name)
	}
	if shortID.MatchString(name) {
		return fmt.Errorf("mailbox name %q looks like a session short id (8 hex digits) and would be ambiguous as an address", name)
	}
	return nil
}

// Message is one entry in a mailbox: who wrote it, to which mailbox, what, and
// where a reply goes. Untrusted is always true on the way out and exists so a
// reader cannot miss what the text is: another agent's words, not an
// instruction or an approval from the reader's own user.
type Message struct {
	Seq       int64     `json:"seq"`                // position in the mailbox log; cursors are sequence numbers
	ID        string    `json:"id"`                 // the message id both ends refer to it by
	From      string    `json:"from"`               // sender label, e.g. `reviewer [b2c3d4e5]` or `codex`
	ReplyTo   string    `json:"reply_to,omitempty"` // address to answer to; empty when the sender cannot receive messages
	To        string    `json:"to"`                 // mailbox name
	Text      string    `json:"text"`               // the message body, verbatim
	At        time.Time `json:"at"`                 // when it was stored
	Untrusted bool      `json:"untrusted"`          // always true: the text is not from the reader's user
}

// mailboxState is the mutable part of a mailbox: two watermarks over the
// immutable log. Everything up to DeliveredSeq has been handed to a reader;
// everything up to ReadSeq has been acknowledged by one. A message's status is
// derived from where its seq falls, so the log never has to be rewritten.
type mailboxState struct {
	DeliveredSeq int64     `json:"delivered_seq"`
	ReadSeq      int64     `json:"read_seq"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Mailbox is one named inbox on disk: an append-only log of messages plus the
// watermarks that say how far a reader has got.
//
// The layout is <state dir>/mailboxes/<name>/log.jsonl and state.json. Appends
// and watermark moves happen under a directory lock (the same mkdir lock the
// agents view uses for its pin file), so several server processes — one per
// MCP client — can write to the same mailbox without interleaving lines or
// handing one message to two readers.
type Mailbox struct {
	Name string
	dir  string
}

// OpenMailbox returns a handle to a mailbox by name. It validates the name and
// locates the directory; it does not create anything — see Exists and Create.
func OpenMailbox(name string) (*Mailbox, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if err := ValidateMailboxName(name); err != nil {
		return nil, err
	}
	root, err := mailboxesDir()
	if err != nil {
		return nil, err
	}
	return &Mailbox{Name: name, dir: filepath.Join(root, name)}, nil
}

// Exists reports whether the mailbox has been created.
func (m *Mailbox) Exists() bool {
	fi, err := os.Stat(m.dir)
	return err == nil && fi.IsDir()
}

// Create makes the mailbox exist. Creating an existing mailbox is a no-op.
func (m *Mailbox) Create() error {
	return os.MkdirAll(m.dir, 0o700)
}

func (m *Mailbox) logPath() string   { return filepath.Join(m.dir, "log.jsonl") }
func (m *Mailbox) statePath() string { return filepath.Join(m.dir, "state.json") }

// lock serialises writers to this mailbox across processes.
func (m *Mailbox) lock() (func(), error) { return acquireLock(m.logPath()) }

// readAll returns every message in the log in order. A line that does not parse
// — the tail of a write that was cut off — is skipped rather than failing the
// read; Append repairs the line ending before writing after it.
func (m *Mailbox) readAll() ([]Message, error) {
	f, err := os.Open(m.logPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []Message
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var msg Message
		if err := json.Unmarshal([]byte(line), &msg); err != nil || msg.Seq <= 0 {
			continue
		}
		out = append(out, msg)
	}
	if err := sc.Err(); err != nil {
		return out, err
	}
	return out, nil
}

func (m *Mailbox) readState() (mailboxState, error) {
	var st mailboxState
	b, err := os.ReadFile(m.statePath())
	if err != nil {
		if os.IsNotExist(err) {
			return st, nil
		}
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("parse %s: %w", m.statePath(), err)
	}
	return st, nil
}

func (m *Mailbox) writeState(st mailboxState) error {
	st.UpdatedAt = time.Now()
	return writeJSONAtomic(m.statePath(), st)
}

// Append stores a message and returns it with its sequence number. It is
// idempotent on the message id: a message whose id is already in the log is not
// stored again, and the stored one comes back with created=false — so a sender
// that retries after a timeout cannot deliver twice, and a reader never sees the
// same id under two sequence numbers.
func (m *Mailbox) Append(msg Message) (Message, bool, error) {
	if !m.Exists() {
		return Message{}, false, fmt.Errorf("mailbox %q does not exist", m.Name)
	}
	if strings.TrimSpace(msg.ID) == "" {
		return Message{}, false, errors.New("message has no id")
	}
	release, err := m.lock()
	if err != nil {
		return Message{}, false, err
	}
	defer release()

	existing, err := m.readAll()
	if err != nil {
		return Message{}, false, err
	}
	var last int64
	for _, e := range existing {
		if e.ID == msg.ID {
			return e, false, nil
		}
		if e.Seq > last {
			last = e.Seq
		}
	}
	msg.Seq = last + 1
	msg.To = m.Name
	msg.Untrusted = true
	if msg.At.IsZero() {
		msg.At = time.Now()
	}
	line, err := json.Marshal(msg)
	if err != nil {
		return Message{}, false, err
	}

	f, err := os.OpenFile(m.logPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return Message{}, false, err
	}
	defer func() { _ = f.Close() }()
	// A previous writer that died mid-line leaves the file without a trailing
	// newline; gluing our record onto that fragment would lose both.
	if fi, serr := f.Stat(); serr == nil && fi.Size() > 0 {
		if tail, rerr := lastByte(m.logPath()); rerr == nil && tail != '\n' {
			if _, werr := f.Write([]byte{'\n'}); werr != nil {
				return Message{}, false, werr
			}
		}
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		return Message{}, false, err
	}
	if err := f.Sync(); err != nil {
		return Message{}, false, err
	}
	return msg, true, nil
}

// lastByte returns the final byte of a file.
func lastByte(path string) (byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil || fi.Size() == 0 {
		return 0, errors.New("empty")
	}
	b := make([]byte, 1)
	if _, err := f.ReadAt(b, fi.Size()-1); err != nil {
		return 0, err
	}
	return b[0], nil
}

// ReadResult is what a reader gets back: the messages, the cursor to continue
// from, and how many more are waiting beyond what the limit allowed.
type ReadResult struct {
	Mailbox  string    `json:"mailbox"`
	Messages []Message `json:"messages"`
	// Cursor is the sequence number of the last message returned (or the cursor
	// the read started from when nothing was). Pass it back to continue from
	// here, or to ack_messages to acknowledge everything up to and including it.
	Cursor string `json:"cursor"`
	// Pending counts messages after Cursor that were not returned because of
	// the limit. Zero means the reader has seen everything so far.
	Pending int `json:"pending"`
	// TimedOut is set by Wait when the timeout passed with nothing to return.
	TimedOut bool `json:"timed_out,omitempty"`
}

// Read limits. A read without a limit returns up to defaultReadLimit messages;
// a limit above maxReadLimit is clamped, because a tool result is read by a
// model and a thousand messages in one result is not a delivery, it is a dump.
const (
	defaultReadLimit = 50
	maxReadLimit     = 500
)

func clampLimit(limit int) int {
	switch {
	case limit <= 0:
		return defaultReadLimit
	case limit > maxReadLimit:
		return maxReadLimit
	}
	return limit
}

// after returns the messages with seq > cursor, at most limit of them, and how
// many were left over.
func after(msgs []Message, cursor int64, limit int) ([]Message, int) {
	out := make([]Message, 0, limit)
	pending := 0
	for _, msg := range msgs {
		if msg.Seq <= cursor {
			continue
		}
		if len(out) < limit {
			msg.Untrusted = true
			out = append(out, msg)
		} else {
			pending++
		}
	}
	return out, pending
}

func formatCursor(seq int64) string { return strconv.FormatInt(seq, 10) }

// ParseCursor reads a cursor as returned in ReadResult.Cursor. "0" (or an
// empty string) means the beginning of the mailbox.
func ParseCursor(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("cursor %q is not a sequence number returned by a previous read", s)
	}
	return n, nil
}

// ReadAfter returns the messages after a cursor without changing anything: it
// is a replay, for a reader that remembers where it was or wants to see a
// message again. Two readers calling it with the same cursor get the same
// messages — that is what an explicit cursor means.
func (m *Mailbox) ReadAfter(cursor int64, limit int) (ReadResult, error) {
	limit = clampLimit(limit)
	msgs, err := m.readAll()
	if err != nil {
		return ReadResult{}, err
	}
	out, pending := after(msgs, cursor, limit)
	res := ReadResult{Mailbox: m.Name, Messages: out, Cursor: formatCursor(cursor), Pending: pending}
	if n := len(out); n > 0 {
		res.Cursor = formatCursor(out[n-1].Seq)
	}
	return res, nil
}

// Fetch returns the messages no reader has fetched yet and marks them delivered
// by moving the delivered watermark past them. It is the call for a reader that
// does not keep a cursor of its own: each message comes out of Fetch exactly
// once, however many readers share the mailbox, because the watermark moves
// under the lock. A reader that wants to see a fetched message again uses
// ReadAfter with an explicit cursor.
func (m *Mailbox) Fetch(limit int) (ReadResult, error) {
	limit = clampLimit(limit)
	release, err := m.lock()
	if err != nil {
		return ReadResult{}, err
	}
	defer release()

	msgs, err := m.readAll()
	if err != nil {
		return ReadResult{}, err
	}
	st, err := m.readState()
	if err != nil {
		return ReadResult{}, err
	}
	out, pending := after(msgs, st.DeliveredSeq, limit)
	res := ReadResult{Mailbox: m.Name, Messages: out, Cursor: formatCursor(st.DeliveredSeq), Pending: pending}
	if n := len(out); n > 0 {
		st.DeliveredSeq = out[n-1].Seq
		if err := m.writeState(st); err != nil {
			return ReadResult{}, err
		}
		res.Cursor = formatCursor(st.DeliveredSeq)
	}
	return res, nil
}

// Ack acknowledges every message up to and including cursor: their status
// becomes read. Acknowledging implies having been handed the message, so the
// delivered watermark is raised too if it was behind. A cursor past the end of
// the log is clamped to the last message; a cursor behind the current watermark
// changes nothing (acks never move backwards).
func (m *Mailbox) Ack(cursor int64) (MailboxStatus, error) {
	release, err := m.lock()
	if err != nil {
		return MailboxStatus{}, err
	}
	defer release()

	msgs, err := m.readAll()
	if err != nil {
		return MailboxStatus{}, err
	}
	st, err := m.readState()
	if err != nil {
		return MailboxStatus{}, err
	}
	var last int64
	if n := len(msgs); n > 0 {
		last = msgs[n-1].Seq
	}
	if cursor > last {
		cursor = last
	}
	changed := false
	if cursor > st.ReadSeq {
		st.ReadSeq, changed = cursor, true
	}
	if st.ReadSeq > st.DeliveredSeq {
		st.DeliveredSeq, changed = st.ReadSeq, true
	}
	if changed {
		if err := m.writeState(st); err != nil {
			return MailboxStatus{}, err
		}
	}
	return statusOf(m.Name, msgs, st), nil
}

// WaitOptions tunes one Wait. Cursor nil means "whatever has not been fetched
// yet" (Fetch semantics, which marks what it returns delivered); a non-nil
// cursor means "everything after this" (ReadAfter semantics, read-only).
type WaitOptions struct {
	Cursor  *int64
	Limit   int
	Timeout time.Duration
}

// Wait timing. The poll interval bounds how late a message can be noticed. The
// default timeout sits under every MCP client's default tool-call timeout seen
// so far (Codex documents 60 s and ships 300 s; Claude Code's is configurable),
// and the cap keeps one call from outliving the client that made it: Codex's
// current default tool timeout is 300 s, so a wait never exceeds it, and a
// client that wants to listen longer calls again — the cursor makes that free.
const (
	waitPollInterval   = 200 * time.Millisecond
	DefaultWaitTimeout = 30 * time.Second
	MaxWaitTimeout     = 5 * time.Minute
)

// Wait blocks until the mailbox has at least one message to return, the timeout
// passes, or the context is cancelled. It returns what Fetch or ReadAfter would
// (see WaitOptions), with TimedOut set when nothing arrived in time — an empty
// result with TimedOut is the normal outcome of a quiet mailbox, not an error.
//
// It watches the log file's size and re-reads only when it changes, so a
// hundred waiting readers cost a stat each per interval, not a lock each.
func (m *Mailbox) Wait(ctx context.Context, opts WaitOptions) (ReadResult, error) {
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultWaitTimeout
	}
	if opts.Timeout > MaxWaitTimeout {
		opts.Timeout = MaxWaitTimeout
	}
	read := func() (ReadResult, error) {
		if opts.Cursor != nil {
			return m.ReadAfter(*opts.Cursor, opts.Limit)
		}
		return m.Fetch(opts.Limit)
	}
	deadline := time.Now().Add(opts.Timeout)
	var (
		lastSize int64 = -1
		last     ReadResult
	)
	for {
		if size := m.logSize(); size != lastSize {
			lastSize = size
			res, err := read()
			if err != nil {
				return res, err
			}
			if len(res.Messages) > 0 {
				return res, nil
			}
			last = res
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			last.TimedOut = true
			return last, nil
		}
		sleep := waitPollInterval
		if remaining < sleep {
			sleep = remaining
		}
		select {
		case <-ctx.Done():
			last.TimedOut = true
			return last, ctx.Err()
		case <-time.After(sleep):
		}
	}
}

// logSize is the size of the log, 0 when it does not exist yet.
func (m *Mailbox) logSize() int64 {
	fi, err := os.Stat(m.logPath())
	if err != nil {
		return 0
	}
	return fi.Size()
}

// MailboxStatus summarises a mailbox: how much is in it and how far its reader
// has got, in the same terms a message's status uses.
type MailboxStatus struct {
	Name         string    `json:"name"`
	Total        int       `json:"total"`         // messages in the log
	Queued       int       `json:"queued"`        // not yet fetched by any reader
	Unread       int       `json:"unread"`        // not yet acknowledged
	LastSeq      int64     `json:"last_seq"`      // cursor of the newest message
	DeliveredSeq int64     `json:"delivered_seq"` // fetched up to here
	ReadSeq      int64     `json:"read_seq"`      // acknowledged up to here
	LastAt       time.Time `json:"last_at,omitempty"`
}

func statusOf(name string, msgs []Message, st mailboxState) MailboxStatus {
	s := MailboxStatus{Name: name, Total: len(msgs), DeliveredSeq: st.DeliveredSeq, ReadSeq: st.ReadSeq}
	for _, msg := range msgs {
		if msg.Seq > st.DeliveredSeq {
			s.Queued++
		}
		if msg.Seq > st.ReadSeq {
			s.Unread++
		}
		if msg.Seq > s.LastSeq {
			s.LastSeq, s.LastAt = msg.Seq, msg.At
		}
	}
	return s
}

// Status reports the mailbox's counters without changing anything.
func (m *Mailbox) Status() (MailboxStatus, error) {
	msgs, err := m.readAll()
	if err != nil {
		return MailboxStatus{}, err
	}
	st, err := m.readState()
	if err != nil {
		return MailboxStatus{}, err
	}
	return statusOf(m.Name, msgs, st), nil
}

// Find looks a message up by id and reports its current status, derived from
// the watermarks: read if acknowledged, delivered if fetched, queued otherwise.
func (m *Mailbox) Find(id string) (Message, string, bool, error) {
	msgs, err := m.readAll()
	if err != nil {
		return Message{}, "", false, err
	}
	st, err := m.readState()
	if err != nil {
		return Message{}, "", false, err
	}
	for _, msg := range msgs {
		if msg.ID != id {
			continue
		}
		msg.Untrusted = true
		switch {
		case msg.Seq <= st.ReadSeq:
			return msg, StatusRead, true, nil
		case msg.Seq <= st.DeliveredSeq:
			return msg, StatusDelivered, true, nil
		default:
			return msg, StatusQueued, true, nil
		}
	}
	return Message{}, "", false, nil
}

// ListMailboxes returns every mailbox on disk with its counters, sorted by name.
func ListMailboxes() ([]MailboxStatus, error) {
	root, err := mailboxesDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]MailboxStatus, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() || ValidateMailboxName(e.Name()) != nil {
			continue
		}
		mb := &Mailbox{Name: e.Name(), dir: filepath.Join(root, e.Name())}
		st, serr := mb.Status()
		if serr != nil {
			return nil, fmt.Errorf("mailbox %s: %w", e.Name(), serr)
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// MailboxNames returns the names of every mailbox on disk, for address
// resolution.
func MailboxNames() ([]string, error) {
	root, err := mailboxesDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() && ValidateMailboxName(e.Name()) == nil {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// The default client mailbox: the name a server process started by something
// other than a Claude Code session answers to when CLAUDE_AGENTS_MAILBOX is not
// set. register_mailbox writes it once; every later client process reads it.
func clientMailboxPath() (string, error) {
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "client-mailbox"), nil
}

// DefaultClientMailbox returns the registered default client mailbox name, or
// "" when none was registered (or the registered name is no longer valid).
func DefaultClientMailbox() string {
	p, err := clientMailboxPath()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	name := strings.ToLower(strings.TrimSpace(string(b)))
	if ValidateMailboxName(name) != nil {
		return ""
	}
	return name
}

// SetDefaultClientMailbox records name as the default client mailbox.
func SetDefaultClientMailbox(name string) error {
	if err := ValidateMailboxName(name); err != nil {
		return err
	}
	p, err := clientMailboxPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp := p + ".tmp." + randID()
	if err := os.WriteFile(tmp, []byte(name+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
