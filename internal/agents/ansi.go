package agents

import (
	"regexp"
	"strings"
)

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[()][AB012]|[\x00-\x08\x0b-\x1f\x7f]`)

// StripANSI converts raw PTY output into readable plain text: it removes ANSI
// escape sequences and control characters and trims surrounding blank lines.
func StripANSI(raw string) string {
	s := strings.ReplaceAll(raw, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = ansiRe.ReplaceAllString(s, "")
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		out = append(out, strings.TrimRight(ln, " \t"))
	}
	return strings.Trim(strings.Join(out, "\n"), "\n")
}
