package agents

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParsePermissionMode(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"default", ModeDefault},
		{"acceptEdits", ModeAcceptEdits},
		{"accept-edits", ModeAcceptEdits},
		{"Accept_Edits", ModeAcceptEdits},
		{" plan ", ModePlan},
		{"bypassPermissions", ModeBypass},
		{"bypass", ModeBypass},
		{"dangerous", ModeBypass},
		{"dontAsk", ModeDontAsk},
		{"auto", ModeAuto},
	}
	for _, c := range cases {
		got, err := ParsePermissionMode(c.in)
		if err != nil {
			t.Errorf("ParsePermissionMode(%q) errored: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParsePermissionMode(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if _, err := ParsePermissionMode("bypassy"); err == nil {
		t.Error("expected an error for an unknown mode")
	}
}

// footer reproduces the prompt-footer line Claude Code paints for a mode:
// symbol, the lowercased mode title, " on", and the cycle hint.
func footer(title string) string {
	return "⏵⏵ " + title + " on (shift+tab to cycle) · ← 15 agents"
}

func TestScreenPermissionMode(t *testing.T) {
	cases := []struct {
		name         string
		screen       string
		want         string
		wantExplicit bool
	}{
		{"bypass", footer("bypass permissions"), ModeBypass, true},
		{"auto", footer("auto mode"), ModeAuto, true},
		{"accept edits", footer("accept edits"), ModeAcceptEdits, true},
		{"plan", "⏸ " + footer("plan mode"), ModePlan, true},
		{"dont ask", footer("don't ask"), ModeDontAsk, true},
		// The default mode labels itself "manual" in the footer of current builds.
		{"manual is the default mode", footer("manual mode"), ModeDefault, true},
		// The cycle hint is dropped when the footer carries other items, so only
		// the "<title> on" part can be matched on.
		{"indicator without the cycle hint", "⏵⏵ bypass permissions on · ← 15 agents", ModeBypass, true},
		{"no indicator is default but not explicit", "❯ just a prompt\n? for shortcuts", ModeDefault, false},
		{"empty screen", "", ModeDefault, false},
		{
			// The screen is a stream tail carrying every footer the session has
			// painted, so the freshest one — the last — is the current mode.
			"last footer wins",
			footer("auto mode") + "\nsome work\n" + footer("bypass permissions"),
			ModeBypass,
			true,
		},
		{
			"earlier bypass does not shadow a later plan",
			footer("bypass permissions") + "\nwork\n" + footer("plan mode"),
			ModePlan,
			true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, explicit := ScreenPermissionMode(c.screen)
			if got != c.want || explicit != c.wantExplicit {
				t.Fatalf("ScreenPermissionMode() = (%q, %v), want (%q, %v)", got, explicit, c.want, c.wantExplicit)
			}
		})
	}
}

func TestBypassAvailableFromFlags(t *testing.T) {
	cases := []struct {
		name  string
		flags []string
		want  bool
	}{
		{"dangerously-skip-permissions", []string{"--name", "x", "--dangerously-skip-permissions"}, true},
		{"allow-dangerously-skip-permissions", []string{"--permission-mode", "auto", "--allow-dangerously-skip-permissions"}, true},
		{"permission-mode bypassPermissions", []string{"--permission-mode", "bypassPermissions"}, true},
		{"permission-mode=bypassPermissions", []string{"--permission-mode=bypassPermissions"}, true},
		// The launch that produced the reported failure: a worker started in auto
		// can never reach bypass, so a "dangerous" resume of it must respawn.
		{"permission-mode auto", []string{"--permission-mode", "auto"}, false},
		{"acceptEdits", []string{"--permission-mode", "acceptEdits"}, false},
		{"no permission flags", []string{"--name", "x", "--model", "opus"}, false},
		{"empty", nil, false},
		// A dangling --permission-mode must not read the flag that follows it as
		// its value by accident.
		{"trailing permission-mode", []string{"--permission-mode"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := BypassAvailableFromFlags(c.flags); got != c.want {
				t.Fatalf("BypassAvailableFromFlags(%v) = %v, want %v", c.flags, got, c.want)
			}
		})
	}
}

func TestApplyModeToFlags(t *testing.T) {
	cases := []struct {
		name        string
		flags       []string
		mode        string
		allowBypass bool
		want        []string
	}{
		{
			"bypass replaces a saved permission mode",
			[]string{"--permission-mode", "auto"}, ModeBypass, false,
			[]string{"--dangerously-skip-permissions"},
		},
		{
			"bypass keeps unrelated flags in order",
			[]string{"--name", "x", "--permission-mode", "auto", "--model", "opus"}, ModeBypass, false,
			[]string{"--name", "x", "--model", "opus", "--dangerously-skip-permissions"},
		},
		{
			"bypass does not duplicate an existing bypass flag",
			[]string{"--dangerously-skip-permissions", "--name", "x"}, ModeBypass, false,
			[]string{"--name", "x", "--dangerously-skip-permissions"},
		},
		{
			"stepping down from bypass drops the bypass flag",
			[]string{"--name", "x", "--dangerously-skip-permissions"}, ModeAcceptEdits, false,
			[]string{"--name", "x", "--permission-mode", "acceptEdits"},
		},
		{
			"the --permission-mode=value form is replaced too",
			[]string{"--permission-mode=auto", "--name", "x"}, ModePlan, false,
			[]string{"--name", "x", "--permission-mode", "plan"},
		},
		{
			"allowBypass makes bypass reachable without activating it",
			[]string{"--name", "x"}, ModeAuto, true,
			[]string{"--name", "x", "--permission-mode", "auto", "--allow-dangerously-skip-permissions"},
		},
		{
			"allowBypass is redundant in bypass and is not added",
			[]string{"--name", "x"}, ModeBypass, true,
			[]string{"--name", "x", "--dangerously-skip-permissions"},
		},
		{
			"a stale allow flag is dropped when not asked for",
			[]string{"--allow-dangerously-skip-permissions", "--name", "x"}, ModeDefault, false,
			[]string{"--name", "x", "--permission-mode", "default"},
		},
		{
			"empty flags",
			nil, ModePlan, false,
			[]string{"--permission-mode", "plan"},
		},
		{
			"a trailing --permission-mode with no value is dropped cleanly",
			[]string{"--name", "x", "--permission-mode"}, ModeAcceptEdits, false,
			[]string{"--name", "x", "--permission-mode", "acceptEdits"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ApplyModeToFlags(c.flags, c.mode, c.allowBypass)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("ApplyModeToFlags(%v, %q, %v) = %v, want %v", c.flags, c.mode, c.allowBypass, got, c.want)
			}
		})
	}
}

func TestApplyModeToFlagsDoesNotMutateInput(t *testing.T) {
	in := []string{"--name", "x", "--permission-mode", "auto"}
	_ = ApplyModeToFlags(in, ModeBypass, true)
	if !reflect.DeepEqual(in, []string{"--name", "x", "--permission-mode", "auto"}) {
		t.Fatalf("ApplyModeToFlags mutated its input: %v", in)
	}
}

// TestApplyModeToFlagsIsIdempotent guards the restart path: rewriting the same
// mode twice must not grow the flag list, or a session restarted repeatedly
// would accumulate duplicate permission flags.
func TestApplyModeToFlagsIsIdempotent(t *testing.T) {
	for _, mode := range []string{ModeDefault, ModeAcceptEdits, ModePlan, ModeBypass, ModeAuto} {
		once := ApplyModeToFlags([]string{"--name", "x"}, mode, true)
		twice := ApplyModeToFlags(once, mode, true)
		if !reflect.DeepEqual(once, twice) {
			t.Errorf("ApplyModeToFlags is not idempotent for %s: %v then %v", mode, once, twice)
		}
	}
}

func TestKeyBytesShiftTab(t *testing.T) {
	// CSI Z: Claude Code's key parser maps '[Z' to tab and flags it as shift,
	// which is the chord its chat:cycleMode binding listens for.
	for _, name := range []string{"shift-tab", "shifttab", "backtab"} {
		got, err := KeyBytes(name)
		if err != nil {
			t.Fatalf("KeyBytes(%q) errored: %v", name, err)
		}
		if got != "\x1b[Z" {
			t.Errorf("KeyBytes(%q) = %q, want ESC[Z", name, got)
		}
	}
}

// writeJobState lays down a minimal ~/.claude/jobs/<short>/state.json under a
// temporary HOME so the on-disk rewrite can be exercised for real.
func writeJobState(t *testing.T, home, short string, state map[string]any) string {
	t.Helper()
	dir := filepath.Join(home, ".claude", "jobs", short)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "state.json")
	b, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSetJobPermissionMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := writeJobState(t, home, "abcd1234", map[string]any{
		"sessionId":    "11111111-2222-3333-4444-555555555555",
		"name":         "aeman-dev",
		"cwd":          "/tmp/x",
		"respawnFlags": []string{"--permission-mode", "auto"},
	})

	flags, err := SetJobPermissionMode("abcd1234", ModeBypass, false)
	if err != nil {
		t.Fatalf("SetJobPermissionMode: %v", err)
	}
	want := []string{"--dangerously-skip-permissions"}
	if !reflect.DeepEqual(flags, want) {
		t.Fatalf("returned flags = %v, want %v", flags, want)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	// Fields the daemon owns must survive the rewrite untouched.
	if got["sessionId"] != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("sessionId was lost: %v", got["sessionId"])
	}
	if got["name"] != "aeman-dev" {
		t.Errorf("name was lost: %v", got["name"])
	}
	raw, ok := got["respawnFlags"].([]any)
	if !ok || len(raw) != 1 || raw[0] != "--dangerously-skip-permissions" {
		t.Fatalf("respawnFlags on disk = %v, want [--dangerously-skip-permissions]", got["respawnFlags"])
	}
	if got["updatedAt"] == nil {
		t.Error("updatedAt was not refreshed")
	}
}

func TestSetJobPermissionModeMissingStateIsSentinel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	_, err := SetJobPermissionMode("nosuchjob", ModeBypass, false)
	if err != ErrNoJobState {
		t.Fatalf("expected ErrNoJobState for a session with no job state, got %v", err)
	}
}

// TestCyclePermissionModeIntegration drives a real session's shift+tab carousel
// and asserts the footer follows. It is gated behind PERMMODE_IT_SHORT (the
// short of a throwaway LIVE session — the test changes its mode) so `go test`
// stays hermetic by default. The session is returned to the mode it was found
// in.
func TestCyclePermissionModeIntegration(t *testing.T) {
	short := os.Getenv("PERMMODE_IT_SHORT")
	if short == "" {
		t.Skip("set PERMMODE_IT_SHORT=<throwaway live session short> to run the live carousel test")
	}
	c := NewClient()
	before, explicit := c.readMode(short, "")
	t.Logf("session %s starts in %s (indicator seen: %v)", short, before, explicit)
	t.Cleanup(func() {
		if _, err := c.CyclePermissionMode(short, before); err != nil {
			t.Logf("could not restore %s to %s: %v", short, before, err)
		}
	})

	// plan is in every carousel, whatever the worker was launched with, so it is
	// the one target that is always a valid assertion.
	path, err := c.CyclePermissionMode(short, ModePlan)
	if err != nil {
		t.Fatalf("CyclePermissionMode(%s, plan): %v (path %v)", short, err, path)
	}
	t.Logf("cycled: %v", path)
	got, seen := c.readMode(short, "")
	if !seen || got != ModePlan {
		t.Fatalf("after cycling to plan the footer reports %q (indicator seen: %v)", got, seen)
	}
}

// TestBypassUnreachableWithoutRestartIntegration is the negative result, checked
// against a real worker: a session launched WITHOUT bypass capability cannot be
// talked into bypassPermissions, however many times shift+tab is pressed.
// Claude Code fixes isBypassPermissionsModeAvailable at startup, so the carousel
// skips from plan straight back to default. Gated behind PERMMODE_IT_AUTO_SHORT
// (a throwaway live session started with e.g. `--permission-mode auto`).
func TestBypassUnreachableWithoutRestartIntegration(t *testing.T) {
	short := os.Getenv("PERMMODE_IT_AUTO_SHORT")
	if short == "" {
		t.Skip("set PERMMODE_IT_AUTO_SHORT=<throwaway live session launched without bypass> to run")
	}
	c := NewClient()
	sess, err := c.Resolve(short)
	if err != nil {
		t.Fatalf("resolve %s: %v", short, err)
	}
	if c.BypassAvailable(sess) {
		t.Skipf("session %s was launched bypass-capable; this test needs one that was not", short)
	}
	before, _ := c.readMode(short, "")
	t.Cleanup(func() { _, _ = c.CyclePermissionMode(short, before) })

	path, err := c.CyclePermissionMode(short, ModeBypass)
	if err == nil {
		t.Fatalf("expected bypassPermissions to be unreachable, but the carousel reached it: %v", path)
	}
	t.Logf("bypass correctly unreachable, carousel walked %v: %v", path, err)
	// The session must be left where it was found, not parked mid-carousel.
	after, _ := c.readMode(short, "")
	if after != before {
		t.Errorf("a failed cycle left the session in %s, want it back in %s", after, before)
	}
}

// TestSetPermissionModeRestartIntegration is the positive counterpart: the same
// bypass-incapable session IS switched into bypassPermissions when a restart is
// allowed — by rewriting its saved launch flags and dispatching it back under
// its own short, with no `claude rm` and no touched worktree. Gated behind
// PERMMODE_IT_RESTART_SHORT.
func TestSetPermissionModeRestartIntegration(t *testing.T) {
	short := os.Getenv("PERMMODE_IT_RESTART_SHORT")
	if short == "" {
		t.Skip("set PERMMODE_IT_RESTART_SHORT=<throwaway live session short> to run the live restart test")
	}
	c := NewClient()
	sess, err := c.Resolve(short)
	if err != nil {
		t.Fatalf("resolve %s: %v", short, err)
	}
	sid := sess.SessionID

	out, err := c.SetPermissionMode(sess, ModeBypass, true, false)
	if err != nil {
		t.Fatalf("SetPermissionMode(%s, bypassPermissions, restart): %v", short, err)
	}
	if !out.Restarted {
		t.Errorf("expected a restart for a bypass-incapable session, got Restarted=false")
	}
	if !BypassAvailableFromFlags(out.Flags) {
		t.Errorf("launch flags after the switch do not grant bypass: %v", out.Flags)
	}
	after, rerr := c.Resolve(short)
	if rerr != nil {
		t.Fatalf("session %s is gone after the restart: %v", short, rerr)
	}
	if !after.Live {
		t.Errorf("session %s did not come back live (state=%q)", short, after.State)
	}
	// The point of the restart path: the session survives it as itself.
	if after.SessionID != sid {
		t.Errorf("session id changed across the restart: %s → %s (that is a fork, not a restart)", sid, after.SessionID)
	}
	mode, seen := c.readMode(short, "")
	if !seen || mode != ModeBypass {
		t.Errorf("restarted session reports mode %q (indicator seen: %v), want bypassPermissions", mode, seen)
	}
}

// TestAllowBypassMakesLaterSwitchesFreeIntegration checks the point of
// allow_bypass: a session restarted with --allow-dangerously-skip-permissions
// runs in an ordinary mode but carries bypassPermissions in its carousel, so the
// NEXT switch into bypass needs no restart at all. Gated behind
// PERMMODE_IT_ALLOW_SHORT (a throwaway live session with a transcript).
func TestAllowBypassMakesLaterSwitchesFreeIntegration(t *testing.T) {
	short := os.Getenv("PERMMODE_IT_ALLOW_SHORT")
	if short == "" {
		t.Skip("set PERMMODE_IT_ALLOW_SHORT=<throwaway live session short> to run")
	}
	c := NewClient()
	sess, err := c.Resolve(short)
	if err != nil {
		t.Fatalf("resolve %s: %v", short, err)
	}

	// Restart it in acceptEdits, but bypass-capable.
	out, err := c.SetPermissionMode(sess, ModeAcceptEdits, true, true)
	if err != nil {
		t.Fatalf("SetPermissionMode(acceptEdits, restart, allowBypass): %v", err)
	}
	if !BypassAvailableFromFlags(out.Flags) {
		t.Fatalf("allow_bypass did not make the session bypass-capable: %v", out.Flags)
	}

	// Now the expensive step must not be needed: no restart, mode still reached.
	sess, err = c.Resolve(short)
	if err != nil {
		t.Fatalf("resolve after restart: %v", err)
	}
	if !c.BypassAvailable(sess) {
		t.Fatal("session is not reported bypass-capable after the allow_bypass restart")
	}
	out, err = c.SetPermissionMode(sess, ModeBypass, false, false)
	if err != nil {
		t.Fatalf("in-place switch to bypass failed on a bypass-capable session: %v", err)
	}
	if out.Restarted {
		t.Error("the switch respawned the worker; a bypass-capable session must switch in place")
	}
	mode, seen := c.readMode(short, "")
	if !seen || mode != ModeBypass {
		t.Errorf("footer reports %q (seen: %v) after the in-place switch, want bypassPermissions", mode, seen)
	}
	t.Logf("switched in place through %v — no restart", out.Path)
}
