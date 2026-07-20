package agents

import (
	"strings"
	"testing"
)

// resumeReturnScreen is the CLI's resume dialog as it reaches DetectDialog:
// the daemon's PTY tail, ANSI-stripped. Its wording and option order are the
// ones the CLI ships — the first option, which compacts, is the preselected one,
// which is why a bare Enter here is destructive.
const resumeReturnScreen = `╭──────────────────────────────────────────────────────────────╮
│ This session is 2h 15m old and 187k tokens.                  │
│                                                              │
│ Resuming the full session will consume a substantial portion │
│ of your usage limits. We recommend resuming from a summary.  │
│                                                              │
│ ❯ Resume from summary (recommended)                          │
│   Resume full session as-is                                  │
│   Don't ask me again                                         │
╰──────────────────────────────────────────────────────────────╯`

func TestDetectResumeDialog(t *testing.T) {
	d := DetectDialog(resumeReturnScreen)
	if d == nil {
		t.Fatal("DetectDialog found no dialog on the resume screen")
	}
	if !d.Recognised() {
		t.Fatalf("resume dialog was not recognised: kind=%q", d.Kind)
	}
	if !d.Blocking() {
		t.Error("a dialog with options must be reported as blocking")
	}
	if len(d.Options) != 3 {
		t.Fatalf("got %d options, want 3: %+v", len(d.Options), d.Options)
	}

	want := []struct {
		effect   string
		selected bool
	}{
		{effectCompact, true}, // preselected: what a bare Enter would pick
		{effectKeep, false},
		{"", false}, // "Don't ask me again" continues, but persists a setting
	}
	for i, w := range want {
		if got := d.Options[i]; got.Effect != w.effect || got.Selected != w.selected {
			t.Errorf("option %d (%q): effect=%q selected=%v, want effect=%q selected=%v",
				i+1, got.Label, got.Effect, got.Selected, w.effect, w.selected)
		}
	}
}

// TestResumeDialogDefaultsToKeeping is the regression test for the bug this
// detection exists for: the option chosen by default must be the one that keeps
// the conversation, never the preselected one that compacts it.
func TestResumeDialogDefaultsToKeeping(t *testing.T) {
	d := DetectDialog(resumeReturnScreen)
	if d == nil {
		t.Fatal("no dialog detected")
	}

	choice, err := ParseResumeDialogChoice("")
	if err != nil {
		t.Fatalf("unspecified choice rejected: %v", err)
	}
	if choice != DialogKeep {
		t.Fatalf("unspecified on_resume_dialog resolved to %q, want %q — compaction must be opt-in", choice, DialogKeep)
	}

	keep, ok := d.keepOption()
	if !ok {
		t.Fatal("no keep option found in the resume dialog")
	}
	if keep.Label != "Resume full session as-is" {
		t.Errorf("keep option is %q, want %q", keep.Label, "Resume full session as-is")
	}
	if keep.Selected {
		t.Error("the keep option must not already be selected — the point is that it has to be navigated to")
	}

	// The default choice must never land on the preselected option: picking that
	// one is what compacted conversations and discarded the pending prompt.
	compact, ok := d.option(effectCompact)
	if !ok {
		t.Fatal("no compact option found in the resume dialog")
	}
	if keep.Number == compact.Number {
		t.Fatal("the keep option and the compact option resolved to the same entry")
	}
}

// TestKeepOptionAvoidsPersistentSuppression checks that "keep" does not answer
// with the option that also stops the dialog appearing ever again: continuing
// this one resume must not change a persistent setting behind the user's back.
func TestKeepOptionAvoidsPersistentSuppression(t *testing.T) {
	d := DetectDialog(resumeReturnScreen)
	keep, ok := d.keepOption()
	if !ok {
		t.Fatal("no keep option")
	}
	if strings.Contains(strings.ToLower(keep.Label), "ask me again") {
		t.Errorf("keep resolved to the suppress-forever option %q", keep.Label)
	}
}

func TestDetectDialogIgnoresOrdinaryScreens(t *testing.T) {
	cases := map[string]string{
		"idle prompt": "✻ Worked for 16m 10s\n\n❯ \n\n? for shortcuts",
		"echoed slash command": "❯ /compact\n  ⎿  Compacted (ctrl+o to see full summary)\n" +
			"  ⎿  Read ../metallb-iad/README.md (99 lines)\n❯ ",
		"prose mentioning the dialog": "I resumed the session and it offered to resume from summary, " +
			"but I chose to resume the full session as-is instead.",
		"numbered prose": "Steps:\nfirst, do a thing\nthen do another",
	}
	for name, screen := range cases {
		if d := DetectDialog(screen); d.Blocking() {
			t.Errorf("%s: reported a dialog (%s)", name, d.Describe())
		}
	}
}

// A dialog whose wording is not the one we know must be reported, never
// answered: a wrong match that answers an unknown question is worse than no
// match at all.
func TestUnknownDialogIsReportedNotAnswered(t *testing.T) {
	screen := "Do you want to proceed?\n❯ 1. Yes\n  2. Yes, and don't ask again\n  3. No"
	d := DetectDialog(screen)
	if d == nil {
		t.Fatal("numbered dialog not detected")
	}
	if !d.Blocking() {
		t.Error("a numbered dialog must be reported as blocking")
	}
	if d.Recognised() {
		t.Errorf("an unknown dialog must not be recognised: kind=%q", d.Kind)
	}
	if d.Kind != dialogUnknown {
		t.Errorf("kind=%q, want %q", d.Kind, dialogUnknown)
	}
	if _, ok := d.selectedNumber(); !ok {
		t.Error("the selection marker was not picked up")
	}
}

// The dialog is recognised by its labels, so it is found whether or not the CLI
// renders the options with numbers, and whichever apostrophe it draws.
func TestDetectResumeDialogVariants(t *testing.T) {
	variants := map[string]string{
		"numbered": "This session is 3d 4h old and 606k tokens.\n" +
			"❯ 1. Resume from summary (recommended)\n  2. Resume full session as-is\n  3. Don't ask me again",
		"typographic apostrophe": "This session is 45m old and 120k tokens.\n" +
			"❯ Resume from summary (recommended)\n  Resume full session as-is\n  Don’t ask me again",
		"selection moved": "This session is 45m old and 120k tokens.\n" +
			"  Resume from summary (recommended)\n❯ Resume full session as-is\n  Don't ask me again",
	}
	for name, screen := range variants {
		d := DetectDialog(screen)
		if d == nil || !d.Recognised() {
			t.Errorf("%s: resume dialog not recognised (%v)", name, d)
			continue
		}
		if _, ok := d.keepOption(); !ok {
			t.Errorf("%s: no keep option", name)
		}
	}
	// In the "selection moved" variant the highlight sits on the keep option,
	// which is what answerDialog looks for before it confirms.
	d := DetectDialog(variants["selection moved"])
	keep, _ := d.keepOption()
	sel, ok := d.selectedNumber()
	if !ok || sel != keep.Number {
		t.Errorf("selection is %d (found=%v), want the keep option %d", sel, ok, keep.Number)
	}
}

// A half-rendered dialog — only one of the two option labels on screen — must
// not be claimed as the known kind. Recognition is what licenses answering it.
func TestPartialResumeDialogIsNotRecognised(t *testing.T) {
	screen := "This session is 2h old and 187k tokens.\n❯ Resume from summary (recommended)"
	if d := DetectDialog(screen); d.Recognised() {
		t.Errorf("a partially rendered dialog was recognised: %s", d.Describe())
	}
}

func TestParseResumeDialogChoice(t *testing.T) {
	ok := map[string]ResumeDialogChoice{
		"":        DialogKeep,
		"  ":      DialogKeep,
		"keep":    DialogKeep,
		"KEEP":    DialogKeep,
		"compact": DialogCompact,
		"ask":     DialogAsk,
	}
	for in, want := range ok {
		got, err := ParseResumeDialogChoice(in)
		if err != nil || got != want {
			t.Errorf("ParseResumeDialogChoice(%q) = %q, %v; want %q, nil", in, got, err, want)
		}
	}
	if _, err := ParseResumeDialogChoice("summarise"); err == nil {
		t.Error("an unknown choice must be rejected rather than silently defaulted")
	}
}

func TestDialogBlockedErrorDoesNotAdviseEnter(t *testing.T) {
	d := DetectDialog(resumeReturnScreen)
	msg := (&DialogBlockedError{Dialog: d}).Error()
	for _, want := range []string{"Resume full session as-is", "on_resume_dialog", "Do NOT send a bare Enter"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message is missing %q:\n%s", want, msg)
		}
	}
}
