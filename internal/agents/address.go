package agents

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// shortID matches an 8-hex short id — the id the agents view shows and the one
// message addressing prefers, because it is unique where a display name is not.
var shortID = regexp.MustCompile(`^[0-9a-fA-F]{8}$`)

// Recipient is a resolved address: a session or a mailbox, never both. A
// session receives in its conversation; a mailbox is read by a client.
type Recipient struct {
	Session Session `json:"session,omitempty"`
	Mailbox string  `json:"mailbox,omitempty"`
}

// Recipient kinds, as reported in delivery results.
const (
	RecipientSession = "session"
	RecipientMailbox = "mailbox"
)

// IsMailbox reports whether the recipient is a mailbox rather than a session.
func (r Recipient) IsMailbox() bool { return r.Mailbox != "" }

// Kind names the recipient kind for results.
func (r Recipient) Kind() string {
	if r.IsMailbox() {
		return RecipientMailbox
	}
	return RecipientSession
}

// Address is what a sender writes in `to` to reach this recipient again.
func (r Recipient) Address() string {
	switch {
	case r.IsMailbox():
		return r.Mailbox
	case r.Session.Short != "":
		return r.Session.Short
	}
	return r.Session.SessionID
}

// Label renders the recipient the way an envelope shows it.
func (r Recipient) Label() string {
	if r.IsMailbox() {
		return r.Mailbox
	}
	return r.Session.Label()
}

// AmbiguousRefError is returned when a reference names more than one recipient
// and no rule can prefer one of them — two live sessions sharing a display name,
// a mailbox named like a session, or a prefix matching several ids. Delivering
// to whichever matched first would put a message in front of the wrong agent,
// and a message is not something to silently misroute, so the candidates are
// reported instead and the caller picks by short id (or mailbox name).
type AmbiguousRefError struct {
	Ref        string
	Candidates []Session
	Mailboxes  []string
}

func (e *AmbiguousRefError) Error() string {
	names := make([]string, 0, len(e.Candidates)+len(e.Mailboxes))
	for _, s := range e.Candidates {
		state := "not running"
		if s.Live {
			state = "live"
		}
		names = append(names, fmt.Sprintf("%s (%s)", s.Label(), state))
	}
	for _, m := range e.Mailboxes {
		names = append(names, fmt.Sprintf("%s (mailbox)", m))
	}
	return fmt.Sprintf("%q matches %d recipients: %s — address the one you mean by its short id (a mailbox cannot share a name with a session; rename one of them)",
		e.Ref, len(names), strings.Join(names, ", "))
}

// pickTarget resolves a reference against the sessions and mailboxes for
// messaging, with the precedence that makes addressing predictable: an exact
// short id, then an exact session id, then an exact name — a mailbox name or a
// session display name, which must not collide — then a prefix of an id.
//
// It differs from Resolve on purpose. Resolve returns the first match, which is
// fine for the tools a human drives by short id; a message is addressed by name
// far more often, and names are not unique — an orchestrator that names two
// workers "reviewer" would otherwise have its messages routed by list order. So
// several exact name matches are only resolved when exactly one of them is live
// (the live one is the agent that can act on the message); otherwise the
// ambiguity is reported.
//
// Matching is case-insensitive: ids are hex and names are typed from memory by a
// model, so "Reviewer" must reach "reviewer" rather than mysteriously not exist.
func pickTarget(ref string, sessions []Session, mailboxes []string) (Recipient, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return Recipient{}, fmt.Errorf("empty recipient")
	}
	byShort := matching(sessions, func(s Session) bool { return strings.EqualFold(s.Short, ref) })
	if len(byShort) > 0 {
		return one(ref, byShort)
	}
	bySID := matching(sessions, func(s Session) bool { return strings.EqualFold(s.SessionID, ref) })
	if len(bySID) > 0 {
		return one(ref, bySID)
	}
	byName := matching(sessions, func(s Session) bool { return strings.EqualFold(strings.TrimSpace(s.Name), ref) })
	var byMailbox []string
	for _, m := range mailboxes {
		if strings.EqualFold(m, ref) {
			byMailbox = append(byMailbox, m)
		}
	}
	switch {
	case len(byMailbox) == 1 && len(byName) == 0:
		return Recipient{Mailbox: byMailbox[0]}, nil
	case len(byMailbox) == 0 && len(byName) > 0:
		return one(ref, byName)
	case len(byMailbox) > 0:
		return Recipient{}, &AmbiguousRefError{Ref: ref, Candidates: byName, Mailboxes: byMailbox}
	}
	byPrefix := matching(sessions, func(s Session) bool {
		return hasPrefixFold(s.Short, ref) || hasPrefixFold(s.SessionID, ref)
	})
	if len(byPrefix) > 0 {
		return one(ref, byPrefix)
	}
	return Recipient{}, fmt.Errorf("no session or mailbox matching %q", ref)
}

// one reduces a candidate set to the single session a message should go to: the
// only match, or — when a display name is shared — the only live one. Anything
// else is ambiguous and is reported rather than guessed.
func one(ref string, candidates []Session) (Recipient, error) {
	if len(candidates) == 1 {
		return Recipient{Session: candidates[0]}, nil
	}
	live := matching(candidates, func(s Session) bool { return s.Live })
	if len(live) == 1 {
		return Recipient{Session: live[0]}, nil
	}
	return Recipient{}, &AmbiguousRefError{Ref: ref, Candidates: candidates}
}

func matching(sessions []Session, pred func(Session) bool) []Session {
	out := make([]Session, 0, 1)
	for _, s := range sessions {
		if pred(s) {
			out = append(out, s)
		}
	}
	return out
}

func hasPrefixFold(s, prefix string) bool {
	return s != "" && len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// ResolveTarget finds the recipient a message is addressed to — a session or a
// mailbox — reporting an ambiguous reference as such instead of picking one
// (see pickTarget). Like ResolveAny it falls back to the transcript on disk for
// a session the agents list no longer has, so a conversation that lost its list
// entry is still reachable by id.
func (c *Client) ResolveTarget(ref string) (Recipient, error) {
	sessions, err := c.sessions()
	if err != nil {
		return Recipient{}, err
	}
	mailboxes, err := MailboxNames()
	if err != nil {
		return Recipient{}, err
	}
	target, perr := pickTarget(ref, sessions, mailboxes)
	if perr == nil {
		return target, nil
	}
	var ambiguous *AmbiguousRefError
	if errors.As(perr, &ambiguous) {
		return Recipient{}, perr
	}
	if o := FindOrphan(ref); o != nil {
		return Recipient{Session: o.Session()}, nil
	}
	return Recipient{}, perr
}

// sessions is the address book: the full agents list, or the test double a
// hermetic test installed in its place.
func (c *Client) sessions() ([]Session, error) {
	if c.listFn != nil {
		return c.listFn()
	}
	return c.List()
}
