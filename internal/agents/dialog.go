package agents

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ResumeDialogChoice is what a caller wants done about the startup dialog the
// CLI shows when a large conversation is resumed. The zero value is the safe
// one: keep the conversation.
type ResumeDialogChoice string

const (
	// DialogKeep answers the dialog with its non-destructive option — continue
	// with the conversation as it is — and then proceeds with the delivery.
	DialogKeep ResumeDialogChoice = "keep"
	// DialogCompact answers it with the compacting option. Opt-in only: it
	// spends the session's context.
	DialogCompact ResumeDialogChoice = "compact"
	// DialogAsk leaves the dialog untouched and reports it back to the caller.
	DialogAsk ResumeDialogChoice = "ask"
)

// ParseResumeDialogChoice validates a caller-supplied choice. An empty value
// means "unspecified" and resolves to DialogKeep, so a caller that never heard
// of this parameter can never lose a conversation to it.
func ParseResumeDialogChoice(s string) (ResumeDialogChoice, error) {
	switch c := ResumeDialogChoice(strings.ToLower(strings.TrimSpace(s))); c {
	case "":
		return DialogKeep, nil
	case DialogKeep, DialogCompact, DialogAsk:
		return c, nil
	default:
		return "", fmt.Errorf("invalid on_resume_dialog %q: use keep, compact or ask", s)
	}
}

// DialogOption is one entry of an on-screen multiple-choice dialog.
type DialogOption struct {
	Number   int    `json:"number"`   // as rendered, 1-based
	Label    string `json:"label"`    // the option text, without its number
	Selected bool   `json:"selected"` // currently highlighted (what a bare Enter would pick)
	Effect   string `json:"effect"`   // "keep" / "compact" / "" when unclassified
}

// Dialog is a multiple-choice dialog detected on a session's screen.
type Dialog struct {
	// Kind is "resume_compact" for the recognised startup dialog offering to
	// compact a resumed conversation, and "unknown" for any other numbered
	// choice dialog. Only a recognised kind may be answered automatically.
	Kind    string         `json:"kind"`
	Title   string         `json:"title"`
	Options []DialogOption `json:"options"`
}

// Recognised reports whether the dialog was positively identified, so answering
// it is a deliberate act rather than a guess.
func (d *Dialog) Recognised() bool { return d != nil && d.Kind == dialogResumeCompact }

const (
	dialogResumeCompact = "resume_compact"
	dialogUnknown       = "unknown"
)

// optionRe matches one rendered option line of a select dialog, e.g.
//
//	❯ 1. Compact the conversation
//	  2. Continue without compacting
//
// The selection marker varies between builds (❯, >, ›) and may be absent, so it
// is optional and only used to report which option a bare Enter would pick.
var optionRe = regexp.MustCompile(`^\s*([❯>›→*]?)\s*(\d{1,2})[.)]\s+(\S.*?)\s*$`)

// DetectDialog parses a multiple-choice dialog out of a session's screen text.
//
// Two passes, in order of confidence. First the resume-return dialog is looked
// for by its own option labels — that one is recognised, so it may be answered
// automatically, and identifying it must not rest on guesswork. Failing that, a
// run of consecutively numbered option lines *drawn inside a box* is reported as
// an unknown dialog: enough to know that something owns the keyboard and a
// keystroke aimed at the input box would go to it instead, without pretending to
// know what answering it would do.
//
// The box requirement is what keeps this from firing on ordinary output: a
// session constantly prints bare numbered lists ("1. Do X\n2. Do Y"), and one of
// those sitting on screen right after a resume must not be mistaken for a dialog
// — that would wedge every resume/fork/auto-resume behind a DialogBlockedError
// no on_resume_dialog choice can clear. A real CLI choice dialog is always
// framed (each row wrapped in box borders); printed lists are not.
//
// nil means no dialog, which is the normal case for a session at its prompt.
func DetectDialog(screen string) *Dialog {
	if d := detectResumeReturn(screen); d != nil {
		return d
	}
	return detectNumbered(screen)
}

// resumeReturnOptions are the options of the CLI's resume-return dialog, in the
// order it renders them, matched case-insensitively against a normalised screen.
//
// The dialog is shown when a resumed session is old and large enough (the CLI
// gates it on ~70 minutes since the last message and ~100k estimated tokens),
// and it preselects the first option — the one that compacts. Its exact labels
// are pinned here because recognising it is what earns the right to answer it;
// a looser match risks answering some other question. When the CLI rewords the
// dialog this stops recognising it, and the fallback is to report an unknown
// dialog and leave it alone, never to press a key on spec.
var resumeReturnOptions = []struct{ match, effect string }{
	{"resume from summary", effectCompact},
	{"resume full session as-is", effectKeep},
	{"don't ask me again", ""}, // continues, but also suppresses the dialog for good
}

// detectResumeReturn finds the resume-return dialog by its option labels. It
// requires both a compacting and a non-compacting option to be present: one
// label alone could be an echo in scrollback, whereas the pair rendered on the
// same screen is the dialog itself.
func detectResumeReturn(screen string) *Dialog {
	lines := strings.Split(normaliseScreen(screen), "\n")
	var opts []DialogOption
	first := len(lines)
	for i, ln := range lines {
		low := strings.ToLower(ln)
		for _, want := range resumeReturnOptions {
			if !strings.Contains(low, want.match) {
				continue
			}
			if i < first {
				first = i
			}
			// The dialog is drawn inside a box, so the option text arrives wrapped
			// in border characters and padding; strip those before reading the
			// selection marker off the front.
			body := trimBox(ln)
			opts = append(opts, DialogOption{
				Number:   len(opts) + 1,
				Label:    strings.TrimSpace(strings.TrimLeft(body, "❯>›→* ")),
				Selected: selectionMarked(body),
				Effect:   want.effect,
			})
			break
		}
	}
	var hasCompact, hasKeep bool
	for _, o := range opts {
		hasCompact = hasCompact || o.Effect == effectCompact
		hasKeep = hasKeep || o.Effect == effectKeep
	}
	if !hasCompact || !hasKeep {
		return nil
	}
	return &Dialog{Kind: dialogResumeCompact, Title: dialogTitle(lines[:first]), Options: opts}
}

// normaliseScreen folds the typographic characters a TUI may render into their
// plain equivalents, so a matcher written with an ASCII apostrophe still matches
// "Don't ask me again" however the terminal drew it.
func normaliseScreen(screen string) string {
	return strings.NewReplacer("’", "'", "‘", "'", " ", " ").Replace(screen)
}

// boxChars are the border and padding characters a framed TUI dialog draws
// around its text.
const boxChars = " \t│┃|╭╮╰╯┌┐└┘─━═"

// trimBox strips a framed dialog's border and padding from one rendered line,
// leaving the text it contains.
func trimBox(line string) string { return strings.Trim(line, boxChars) }

// selectionMarked reports whether a line carries the select cursor.
func selectionMarked(line string) bool {
	t := strings.TrimLeft(line, " \t")
	return t != "" && strings.ContainsRune("❯>›→*", []rune(t)[0])
}

// detectNumbered looks for a run of consecutively numbered option lines starting
// at 1 and framed inside a box — the shape a numbered select dialog renders. It
// is the generic guard: what it finds is reported, never answered.
//
// Option lines are matched after their box border and padding are trimmed, so a
// framed "│ ❯ 1. Yes │" row matches, and the block is only accepted when it is
// actually framed. That box requirement is the whole point: without it a bare
// printed list ("1. …\n2. …") reads as a blocking dialog, and since bare rows
// carry no leading border the old raw-line match caught *only* those — false
// positives — while missing every real (framed) dialog.
func detectNumbered(screen string) *Dialog {
	lines := strings.Split(screen, "\n")
	// Scan from the bottom: a dialog is on the current screen, while an old one
	// may still be sitting in scrollback above it.
	for start := len(lines) - 1; start >= 0; start-- {
		m := optionRe.FindStringSubmatch(trimBox(lines[start]))
		if m == nil || m[2] != "1" {
			continue
		}
		opts, boxed := collectOptions(lines[start:])
		if len(opts) < 2 || !boxed {
			continue
		}
		d := &Dialog{Title: dialogTitle(lines[:start]), Options: opts}
		annotate(d)
		return d
	}
	return nil
}

// collectOptions reads consecutively numbered option lines (1, 2, 3, …) from the
// start of lines, tolerating blank lines and wrapped continuation text between
// them, and stops at the first non-blank line that is neither. It matches each
// line with its box framing trimmed off, and reports whether the block was
// framed at all (any option row carried a box border) — the caller rejects an
// unframed block as ordinary printed output rather than a dialog.
func collectOptions(lines []string) (opts []DialogOption, boxed bool) {
	want := 1
	for _, ln := range lines {
		m := optionRe.FindStringSubmatch(trimBox(ln))
		if m == nil {
			if strings.TrimSpace(ln) == "" || len(opts) == 0 {
				continue
			}
			// A wrapped continuation of the previous option: keep scanning, but
			// do not fold it into the label (indentation is unreliable here).
			continue
		}
		n, err := strconv.Atoi(m[2])
		if err != nil || n != want {
			break
		}
		if lineFramed(ln) {
			boxed = true
		}
		opts = append(opts, DialogOption{
			Number:   n,
			Label:    strings.TrimSpace(m[3]),
			Selected: m[1] != "",
		})
		want++
	}
	return opts, boxed
}

// lineFramed reports whether a rendered line is part of a drawn box — it carries
// a vertical border or a box corner. Only genuine box-drawing runes count; the
// ASCII pipe is excluded because it appears in ordinary text (e.g. "a | b") and
// would let a bare list masquerade as framed.
func lineFramed(line string) bool {
	return strings.ContainsAny(line, "│┃╭╮╰╯┌┐└┘")
}

// dialogTitle returns the last non-blank line above the options — the dialog's
// question — with any box-drawing decoration trimmed off.
func dialogTitle(above []string) string {
	for i := len(above) - 1; i >= 0; i-- {
		if t := trimBox(above[i]); t != "" {
			return t
		}
	}
	return ""
}

// Blocking reports whether a detected dialog is holding keyboard focus, so a
// keystroke aimed at the input box would be answering the dialog instead.
func (d *Dialog) Blocking() bool { return d != nil && len(d.Options) > 0 }

// Describe renders a dialog for a tool result: the kind, the question, and every
// option with its number and classified effect, so the caller can decide what to
// do instead of being told to press a key blind.
func (d *Dialog) Describe() string {
	if d == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "dialog=%s", d.Kind)
	if d.Title != "" {
		fmt.Fprintf(&b, " question=%q", d.Title)
	}
	b.WriteString(" options=[")
	for i, o := range d.Options {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%d:%q", o.Number, o.Label)
		if o.Effect != "" {
			fmt.Fprintf(&b, " (%s)", o.Effect)
		}
		if o.Selected {
			b.WriteString(" [default]")
		}
	}
	b.WriteString("]")
	return b.String()
}

const (
	effectKeep    = "keep"
	effectCompact = "compact"
)

// annotate labels each option of an unknown dialog with the effect its wording
// suggests, purely so the report back to the caller is more useful. It never
// promotes the dialog to a recognised kind: a dialog is only answered
// automatically when it was identified by its own labels, because reading intent
// out of arbitrary wording is the guesswork this package exists to avoid.
func annotate(d *Dialog) {
	d.Kind = dialogUnknown
	for i := range d.Options {
		d.Options[i].Effect = optionEffect(d.Options[i].Label)
	}
}

// optionEffect reads one option label for what choosing it would do. Compaction
// is only claimed when the label is not negating it — "continue without
// compacting" names compaction but is the option that avoids it — so the
// negation is tested first and wins.
func optionEffect(label string) string {
	l := strings.ToLower(label)
	compacting := containsAny(l, "compact", "summary")
	negated := containsAny(l, "without", "don't", "dont", "do not", "skip")
	switch {
	case compacting && !negated:
		return effectCompact
	case compacting && negated:
		return effectKeep
	case containsAny(l, "continue", "keep", "as is", "as-is", "leave", "full session"):
		return effectKeep
	default:
		return ""
	}
}

func containsAny(hay string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(hay, n) {
			return true
		}
	}
	return false
}

// keepOption returns the option to pick for DialogKeep, preferring one that only
// continues this resume over one that also suppresses the dialog permanently —
// changing a persistent setting is not part of delivering a prompt.
func (d *Dialog) keepOption() (DialogOption, bool) {
	if d == nil {
		return DialogOption{}, false
	}
	for _, o := range d.Options {
		if o.Effect == effectKeep && !containsAny(strings.ToLower(o.Label), "ask again", "always", "remember") {
			return o, true
		}
	}
	return d.option(effectKeep)
}

// option returns the first option classified with the given effect.
func (d *Dialog) option(effect string) (DialogOption, bool) {
	if d == nil {
		return DialogOption{}, false
	}
	for _, o := range d.Options {
		if o.Effect == effect {
			return o, true
		}
	}
	return DialogOption{}, false
}
