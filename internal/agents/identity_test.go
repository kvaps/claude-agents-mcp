package agents

import (
	"strings"
	"testing"
)

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
			isolateIdentity(t)
			t.Setenv(envSessionID, c.sessionID)
			t.Setenv(envJobDir, c.jobDir)
			got := envSelf()
			if c.wantKnown && got.Kind != KindSession {
				t.Errorf("Kind = %q, want %q", got.Kind, KindSession)
			}
			if !c.wantKnown && got.Kind != KindUnknown {
				t.Errorf("Kind = %q, want %q", got.Kind, KindUnknown)
			}
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
		{
			name:        "client: the mailbox is both address and label",
			self:        Self{Kind: KindClient, Mailbox: "codex"},
			wantAddress: "codex",
			wantLabel:   "codex",
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
	isolateIdentity(t)
	self := NewClient().Whoami()
	if self.Known() {
		t.Fatalf("Whoami() = %+v, want an unidentified caller", self)
	}
	if self.Addressable {
		t.Error("an unidentified caller must not be reported as addressable")
	}
}

// isolateIdentity clears every environment variable and on-disk default the
// identity is read from, so a test sees only what it sets itself.
func isolateIdentity(t *testing.T) {
	t.Helper()
	t.Setenv(envSessionID, "")
	t.Setenv(envJobDir, "")
	t.Setenv(envMailbox, "")
	t.Setenv(envStateDir, t.TempDir())
}

// TestEnvSelfClient covers the caller that is not a session: an MCP client. Its
// mailbox comes from the environment the client's configuration gave the server,
// which wins over everything else because it is explicit; without it, a process
// that is not a session falls back to the registered default; an invalid name in
// the environment is ignored rather than becoming an unusable identity.
func TestEnvSelfClient(t *testing.T) {
	isolateIdentity(t)
	t.Setenv(envMailbox, "Codex")
	got := envSelf()
	if got.Kind != KindClient || got.Mailbox != "codex" || !got.IsClient() || !got.Known() {
		t.Fatalf("envSelf with %s set = %+v", envMailbox, got)
	}

	// Explicit mailbox beats a session environment.
	t.Setenv(envSessionID, "58ce5347-abfd-474f-973b-29bd4821769a")
	if got := envSelf(); got.Kind != KindClient || got.Mailbox != "codex" {
		t.Fatalf("session env overrode an explicit mailbox: %+v", got)
	}

	// An invalid name is not an identity; the session wins.
	t.Setenv(envMailbox, "Not A Mailbox")
	if got := envSelf(); got.Kind != KindSession || got.Short != "58ce5347" {
		t.Fatalf("invalid mailbox name produced %+v", got)
	}

	// No environment at all: the registered default, when there is one.
	isolateIdentity(t)
	if err := SetDefaultClientMailbox("voice"); err != nil {
		t.Fatal(err)
	}
	if got := envSelf(); got.Kind != KindClient || got.Mailbox != "voice" {
		t.Fatalf("default client mailbox not picked up: %+v", got)
	}
}

// TestWhoamiClient: a client is addressable once its mailbox exists, and
// OwnMailbox is what makes it exist.
func TestWhoamiClient(t *testing.T) {
	isolateIdentity(t)
	t.Setenv(envMailbox, "codex")
	c := NewClient()
	if self := c.Whoami(); self.Addressable {
		t.Fatalf("client reported addressable before its mailbox exists: %+v", self)
	}
	mb, err := c.OwnMailbox()
	if err != nil || mb.Name != "codex" || !mb.Exists() {
		t.Fatalf("OwnMailbox: %v (%+v)", err, mb)
	}
	if self := c.Whoami(); !self.Addressable || self.Address() != "codex" {
		t.Fatalf("client not addressable after OwnMailbox: %+v", self)
	}
}

// TestOwnMailboxRefusesSessionsAndStrangers: a session has a conversation to
// receive in, and an unidentified caller has to say who it is first.
func TestOwnMailboxRefusesSessionsAndStrangers(t *testing.T) {
	isolateIdentity(t)
	c := NewClient()
	if _, err := c.OwnMailbox(); err == nil || !strings.Contains(err.Error(), envMailbox) {
		t.Fatalf("unidentified caller: err = %v, want a hint naming %s", err, envMailbox)
	}
	t.Setenv(envJobDir, "/Users/x/.claude/jobs/a1b2c3d4")
	if _, err := c.OwnMailbox(); err == nil || !strings.Contains(err.Error(), "conversation") {
		t.Fatalf("session caller: err = %v, want a refusal", err)
	}
}

// TestRegisterMailbox: registering fixes the default for every later client
// process, is refused for a session, and cannot contradict an explicit
// environment.
func TestRegisterMailbox(t *testing.T) {
	isolateIdentity(t)
	c := NewClient()
	mb, err := c.RegisterMailbox(" Codex ")
	if err != nil || mb.Name != "codex" || !mb.Exists() {
		t.Fatalf("RegisterMailbox: %v (%+v)", err, mb)
	}
	if DefaultClientMailbox() != "codex" {
		t.Fatalf("default after register = %q", DefaultClientMailbox())
	}
	if self := c.Whoami(); !self.IsClient() || !self.Addressable || self.Mailbox != "codex" {
		t.Fatalf("Whoami after register = %+v", self)
	}
	t.Setenv(envMailbox, "voice")
	if _, err := c.RegisterMailbox("codex"); err == nil {
		t.Fatal("registering a name that contradicts the environment succeeded")
	}
	if _, err := c.RegisterMailbox("voice"); err != nil {
		t.Fatalf("registering the environment's own name failed: %v", err)
	}
	isolateIdentity(t)
	t.Setenv(envSessionID, "58ce5347-abfd-474f-973b-29bd4821769a")
	if _, err := c.RegisterMailbox("codex"); err == nil {
		t.Fatal("a session registered a mailbox")
	}
}
