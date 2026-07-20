package agents

import (
	"strings"
	"testing"
)

// TestBuildSubmitBody covers the pure body-selection logic, in particular the
// /goal length fallback: a goal condition over Claude Code's goalMaxLen limit
// must be delivered as a plain prompt (whole task preserved) rather than a
// "/goal …" command that Claude Code would reject and leave unsubmitted.
func TestBuildSubmitBody(t *testing.T) {
	atLimitASCII := strings.Repeat("a", goalMaxLen)     // exactly 4000 chars → fits
	overLimitASCII := strings.Repeat("a", goalMaxLen+1) // 4001 chars → too long
	// A Cyrillic goal at the character limit is ~2 bytes/rune (~8000 bytes) but
	// only goalMaxLen runes, so it must still go out as /goal — this guards the
	// rune-count (not byte-count) measurement against a needless downgrade.
	atLimitCyrillic := strings.Repeat("я", goalMaxLen)

	cases := []struct {
		name string
		text string
		goal bool
		want string
	}{
		{"plain trims trailing newlines", "do the thing\n\n", false, "do the thing"},
		{"plain keeps internal newlines", "line one\nline two", false, "line one\nline two"},
		{"short goal gets prefix and is trimmed", "  ship it  ", true, "/goal ship it"},
		{"goal at limit still uses /goal", atLimitASCII, true, "/goal " + atLimitASCII},
		{"goal over limit falls back to plain", overLimitASCII, true, overLimitASCII},
		{"cyrillic goal at rune limit still uses /goal", atLimitCyrillic, true, "/goal " + atLimitCyrillic},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildSubmitBody(tc.text, tc.goal); got != tc.want {
				t.Errorf("buildSubmitBody(len=%d, goal=%v): got len=%d, want len=%d",
					len(tc.text), tc.goal, len(got), len(tc.want))
			}
		})
	}
}
