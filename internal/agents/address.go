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

// AmbiguousRefError is returned when a reference names more than one session and
// no rule can prefer one of them — two live sessions sharing a display name, or
// a prefix matching several ids. Delivering to whichever matched first would put
// a message in front of the wrong agent, and a message is not something to
// silently misroute, so the candidates are reported instead and the caller picks
// by short id.
type AmbiguousRefError struct {
	Ref        string
	Candidates []Session
}

func (e *AmbiguousRefError) Error() string {
	names := make([]string, 0, len(e.Candidates))
	for _, s := range e.Candidates {
		state := "not running"
		if s.Live {
			state = "live"
		}
		names = append(names, fmt.Sprintf("%s (%s)", s.Label(), state))
	}
	return fmt.Sprintf("%q matches %d sessions: %s — address the one you mean by its short id",
		e.Ref, len(e.Candidates), strings.Join(names, ", "))
}

// pickTarget resolves a reference against a session list for messaging, with the
// precedence that makes addressing predictable: an exact short id, then an exact
// session id, then an exact display name, then a prefix of an id.
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
func pickTarget(ref string, sessions []Session) (Session, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return Session{}, fmt.Errorf("empty session reference")
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
	if len(byName) > 0 {
		return one(ref, byName)
	}
	byPrefix := matching(sessions, func(s Session) bool {
		return hasPrefixFold(s.Short, ref) || hasPrefixFold(s.SessionID, ref)
	})
	if len(byPrefix) > 0 {
		return one(ref, byPrefix)
	}
	return Session{}, fmt.Errorf("no session matching %q", ref)
}

// one reduces a candidate set to the single session a message should go to: the
// only match, or — when a display name is shared — the only live one. Anything
// else is ambiguous and is reported rather than guessed.
func one(ref string, candidates []Session) (Session, error) {
	if len(candidates) == 1 {
		return candidates[0], nil
	}
	live := matching(candidates, func(s Session) bool { return s.Live })
	if len(live) == 1 {
		return live[0], nil
	}
	return Session{}, &AmbiguousRefError{Ref: ref, Candidates: candidates}
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

// ResolveTarget finds the session a message is addressed to, reporting an
// ambiguous reference as such instead of picking one (see pickTarget). Like
// ResolveAny it falls back to the transcript on disk for a session the agents
// list no longer has, so a conversation that lost its list entry is still
// reachable by id.
func (c *Client) ResolveTarget(ref string) (Session, error) {
	sessions, err := c.List()
	if err != nil {
		return Session{}, err
	}
	sess, perr := pickTarget(ref, sessions)
	if perr == nil {
		return sess, nil
	}
	var ambiguous *AmbiguousRefError
	if errors.As(perr, &ambiguous) {
		return Session{}, perr
	}
	if o := FindOrphan(ref); o != nil {
		return o.Session(), nil
	}
	return Session{}, perr
}
