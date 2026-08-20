package agents

import "testing"

// TestEnvSelf covers how the calling session is identified from the environment
// Claude Code hands its child processes. This is the whole basis of message
// identification — the sender is never a parameter — so each shape of that
// environment is pinned: a background session (both variables), a session id
// alone, a job dir alone, and an environment that says nothing at all.
func TestEnvSelf(t *testing.T) {
	const sid = "58ce5347-abfd-474f-973b-29bd4821769a"
	cases := []struct {
		name      string
		sessionID string
		jobDir    string
		wantShort string
		wantSID   string
		wantKnown bool
	}{
		{
			name:      "background session: job dir names the short",
			sessionID: sid,
			jobDir:    "/Users/x/.claude/jobs/58ce5347",
			wantShort: "58ce5347",
			wantSID:   sid,
			wantKnown: true,
		},
		{
			name:      "session id alone: short is its first 8 hex digits",
			sessionID: sid,
			wantShort: "58ce5347",
			wantSID:   sid,
			wantKnown: true,
		},
		{
			name:      "job dir alone still yields an address",
			jobDir:    "/Users/x/.claude/jobs/a1b2c3d4/",
			wantShort: "a1b2c3d4",
			wantKnown: true,
		},
		{
			name:      "job dir that is not a short id is not an address",
			jobDir:    "/tmp/somewhere-else",
			wantKnown: false,
		},
		{
			name:      "session id that is not a UUID yields no short",
			sessionID: "not-a-session-id",
			wantSID:   "not-a-session-id",
			wantKnown: true,
		},
		{
			name:      "no environment: nothing is claimed",
			wantKnown: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(envSessionID, c.sessionID)
			t.Setenv(envJobDir, c.jobDir)
			got := envSelf()
			if got.Short != c.wantShort {
				t.Errorf("Short = %q, want %q", got.Short, c.wantShort)
			}
			if got.SessionID != c.wantSID {
				t.Errorf("SessionID = %q, want %q", got.SessionID, c.wantSID)
			}
			if got.Known() != c.wantKnown {
				t.Errorf("Known() = %v, want %v", got.Known(), c.wantKnown)
			}
		})
	}
}

// TestSelfAddressLabel pins what other agents are told to write to: the short id
// when there is one, the session id otherwise, and never a display name on its
// own — names are not unique, and a reply that resolves to the wrong session is
// worse than one that does not resolve at all.
func TestSelfAddressLabel(t *testing.T) {
	cases := []struct {
		name        string
		self        Self
		wantAddress string
		wantLabel   string
	}{
		{
			name:        "named background session",
			self:        Self{Short: "a1b2c3d4", SessionID: "a1b2c3d4-1111-2222-3333-444455556666", Name: "orchestrator"},
			wantAddress: "a1b2c3d4",
			wantLabel:   "orchestrator [a1b2c3d4]",
		},
		{
			name:        "unnamed session shows its address alone",
			self:        Self{Short: "a1b2c3d4"},
			wantAddress: "a1b2c3d4",
			wantLabel:   "a1b2c3d4",
		},
		{
			name:        "no short: the session id is the address",
			self:        Self{SessionID: "a1b2c3d4-1111-2222-3333-444455556666", Name: "desktop"},
			wantAddress: "a1b2c3d4-1111-2222-3333-444455556666",
			wantLabel:   "desktop [a1b2c3d4-1111-2222-3333-444455556666]",
		},
		{
			name:        "nothing known",
			self:        Self{},
			wantAddress: "",
			wantLabel:   "unknown",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.self.Address(); got != c.wantAddress {
				t.Errorf("Address() = %q, want %q", got, c.wantAddress)
			}
			if got := c.self.Label(); got != c.wantLabel {
				t.Errorf("Label() = %q, want %q", got, c.wantLabel)
			}
		})
	}
}

// TestWhoamiUnknownEnvironment asserts that a server started outside a session
// reports no identity and no address, rather than inventing one: an agent that
// cannot be reached must not invite replies. It touches the daemon only through
// Resolve, which is never called for an unidentified caller, so it is hermetic.
func TestWhoamiUnknownEnvironment(t *testing.T) {
	t.Setenv(envSessionID, "")
	t.Setenv(envJobDir, "")
	self := NewClient().Whoami()
	if self.Known() {
		t.Fatalf("Whoami() = %+v, want an unidentified caller", self)
	}
	if self.Addressable {
		t.Error("an unidentified caller must not be reported as addressable")
	}
}
