package agents

import (
	"errors"
	"strings"
	"testing"
)

// fleet is the address book the addressing tests resolve against: two live
// sessions sharing a display name, a not-running session sharing a name with a
// live one, and an unnamed session — the shapes that make "address by name"
// interesting.
var fleet = []Session{
	{Short: "a1b2c3d4", SessionID: "a1b2c3d4-1111-2222-3333-444455556666", Name: "orchestrator", Live: true},
	{Short: "b2c3d4e5", SessionID: "b2c3d4e5-1111-2222-3333-444455556666", Name: "reviewer", Live: true},
	{Short: "c3d4e5f6", SessionID: "c3d4e5f6-1111-2222-3333-444455556666", Name: "reviewer", Live: true},
	{Short: "d4e5f6a7", SessionID: "d4e5f6a7-1111-2222-3333-444455556666", Name: "builder", Live: false, Resumable: true},
	{Short: "e5f6a7b8", SessionID: "e5f6a7b8-1111-2222-3333-444455556666", Name: "builder", Live: true},
	{Short: "f6a7b8c9", SessionID: "f6a7b8c9-1111-2222-3333-444455556666", Live: true},
}

func TestPickTarget(t *testing.T) {
	cases := []struct {
		name      string
		ref       string
		wantShort string
	}{
		{"exact short id", "a1b2c3d4", "a1b2c3d4"},
		{"exact short id, upper case", "A1B2C3D4", "a1b2c3d4"},
		{"exact session id", "b2c3d4e5-1111-2222-3333-444455556666", "b2c3d4e5"},
		{"unique display name", "orchestrator", "a1b2c3d4"},
		{"display name, different case", "Orchestrator", "a1b2c3d4"},
		{"display name with surrounding space", "  orchestrator  ", "a1b2c3d4"},
		{"shared name resolves to the live session", "builder", "e5f6a7b8"},
		{"id prefix", "f6a7", "f6a7b8c9"},
		{"unnamed session by its short id", "f6a7b8c9", "f6a7b8c9"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := pickTarget(c.ref, fleet, nil)
			if err != nil {
				t.Fatalf("pickTarget(%q) failed: %v", c.ref, err)
			}
			if got.Session.Short != c.wantShort {
				t.Fatalf("pickTarget(%q) = %s, want %s", c.ref, got.Session.Short, c.wantShort)
			}
		})
	}
}

// TestPickTargetAmbiguous pins the rule that separates messaging from every
// other tool here: a reference naming several sessions is refused with the
// candidates listed, instead of quietly delivering to whichever came first in
// the list. A message put in front of the wrong agent is not recoverable by
// retrying — it has already been read.
func TestPickTargetAmbiguous(t *testing.T) {
	for _, ref := range []string{"reviewer", "REVIEWER"} {
		t.Run(ref, func(t *testing.T) {
			_, err := pickTarget(ref, fleet, nil)
			var ambiguous *AmbiguousRefError
			if !errors.As(err, &ambiguous) {
				t.Fatalf("pickTarget(%q) error = %v, want *AmbiguousRefError", ref, err)
			}
			if len(ambiguous.Candidates) != 2 {
				t.Fatalf("candidates = %d, want 2", len(ambiguous.Candidates))
			}
			// The error has to be actionable: it names both shorts so the caller
			// can immediately re-address by one of them.
			for _, want := range []string{"b2c3d4e5", "c3d4e5f6"} {
				if !strings.Contains(ambiguous.Error(), want) {
					t.Errorf("error %q does not name candidate %s", ambiguous.Error(), want)
				}
			}
		})
	}
}

func TestPickTargetNotFound(t *testing.T) {
	for _, ref := range []string{"", "   ", "nobody", "99999999"} {
		t.Run(ref, func(t *testing.T) {
			if got, err := pickTarget(ref, fleet, []string{"codex"}); err == nil {
				t.Fatalf("pickTarget(%q) = %s, want an error", ref, got.Label())
			}
		})
	}
}

// TestPickTargetPrefersIDsOverNames: a session named after another session's
// short id must not shadow it. Ids are the unambiguous half of the address
// space, so they are matched first.
func TestPickTargetPrefersIDsOverNames(t *testing.T) {
	sessions := append([]Session{{Short: "11112222", SessionID: "11112222-aaaa-bbbb-cccc-dddddddddddd", Name: "a1b2c3d4", Live: true}}, fleet...)
	got, err := pickTarget("a1b2c3d4", sessions, nil)
	if err != nil {
		t.Fatalf("pickTarget: %v", err)
	}
	if got.Session.Short != "a1b2c3d4" {
		t.Fatalf("pickTarget(\"a1b2c3d4\") = %s, want the session whose short id it is", got.Session.Short)
	}
}

// TestPickTargetMailbox: a mailbox is addressed by its name like a session by
// its display name, case-insensitively, and resolves to a mailbox recipient.
func TestPickTargetMailbox(t *testing.T) {
	for _, ref := range []string{"codex", "Codex", " codex "} {
		got, err := pickTarget(ref, fleet, []string{"codex", "voice"})
		if err != nil {
			t.Fatalf("pickTarget(%q): %v", ref, err)
		}
		if !got.IsMailbox() || got.Mailbox != "codex" || got.Kind() != RecipientMailbox || got.Address() != "codex" || got.Label() != "codex" {
			t.Fatalf("pickTarget(%q) = %+v, want the codex mailbox", ref, got)
		}
	}
}

// TestPickTargetMailboxVersusName: a mailbox and a session sharing a name is
// not resolved by preferring either — the message would reach the wrong one —
// but reported with both candidates named.
func TestPickTargetMailboxVersusName(t *testing.T) {
	sessions := append([]Session{{Short: "11112222", SessionID: "11112222-aaaa-bbbb-cccc-dddddddddddd", Name: "codex", Live: true}}, fleet...)
	_, err := pickTarget("codex", sessions, []string{"codex"})
	var ambiguous *AmbiguousRefError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("error = %v, want *AmbiguousRefError", err)
	}
	for _, want := range []string{"11112222", "codex (mailbox)"} {
		if !strings.Contains(ambiguous.Error(), want) {
			t.Errorf("error %q does not name %s", ambiguous.Error(), want)
		}
	}
	// An id still wins over a mailbox named like it would be impossible (ids are
	// refused as mailbox names), but a mailbox never shadows a short id lookup.
	got, err := pickTarget("11112222", sessions, []string{"codex"})
	if err != nil || got.Session.Short != "11112222" {
		t.Fatalf("pickTarget by short id with mailboxes present = %+v, %v", got, err)
	}
}
