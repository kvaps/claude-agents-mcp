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
			got, err := pickTarget(c.ref, fleet)
			if err != nil {
				t.Fatalf("pickTarget(%q) failed: %v", c.ref, err)
			}
			if got.Short != c.wantShort {
				t.Fatalf("pickTarget(%q) = %s, want %s", c.ref, got.Short, c.wantShort)
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
			_, err := pickTarget(ref, fleet)
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
			if got, err := pickTarget(ref, fleet); err == nil {
				t.Fatalf("pickTarget(%q) = %s, want an error", ref, got.Short)
			}
		})
	}
}

// TestPickTargetPrefersIDsOverNames: a session named after another session's
// short id must not shadow it. Ids are the unambiguous half of the address
// space, so they are matched first.
func TestPickTargetPrefersIDsOverNames(t *testing.T) {
	sessions := append([]Session{{Short: "11112222", SessionID: "11112222-aaaa-bbbb-cccc-dddddddddddd", Name: "a1b2c3d4", Live: true}}, fleet...)
	got, err := pickTarget("a1b2c3d4", sessions)
	if err != nil {
		t.Fatalf("pickTarget: %v", err)
	}
	if got.Short != "a1b2c3d4" {
		t.Fatalf("pickTarget(\"a1b2c3d4\") = %s, want the session whose short id it is", got.Short)
	}
}
