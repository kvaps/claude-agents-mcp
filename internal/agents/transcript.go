package agents

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// transcriptFiles returns every on-disk transcript for a session id. There is
// normally exactly one (~/.claude/projects/<project>/<sid>.jsonl), but a session
// that changed directory mid-run can have its records under another project dir,
// and a resume can start writing a fresh file — so all of them are considered.
func transcriptFiles(sid string) []string {
	if sid == "" {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	matches, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", sid+".jsonl"))
	return matches
}

// transcriptMark is a snapshot of a session's transcripts taken just before a
// prompt is delivered: the byte length of every file that already exists. A
// later Landed scan reads only what was appended past those offsets, so an
// identical prompt delivered earlier in the conversation cannot be mistaken for
// the current one.
type transcriptMark struct {
	sid   string
	sizes map[string]int64
}

// markTranscript captures the pre-delivery transcript offsets for a session. A
// session with no session id or no transcript yet yields an empty mark, whose
// Landed always reports false — callers must treat that as "no ground truth
// available" rather than as evidence of a failed delivery.
func markTranscript(sid string) transcriptMark {
	m := transcriptMark{sid: sid, sizes: map[string]int64{}}
	for _, p := range transcriptFiles(sid) {
		if fi, err := os.Stat(p); err == nil {
			m.sizes[p] = fi.Size()
		}
	}
	return m
}

// available reports whether the mark can produce ground truth at all — i.e. the
// session has a session id, so its transcript can be located.
func (m transcriptMark) available() bool { return m.sid != "" }

// Grew reports whether any of the session's transcripts has been appended to
// since the mark was taken. It is evidence that the session is doing something
// — a turn writes assistant and tool records continuously — which, when the
// prompt itself has not appeared, means the prompt is queued behind a turn that
// was already running rather than stuck unsubmitted in the input box.
func (m transcriptMark) Grew() bool {
	if !m.available() {
		return false
	}
	for _, p := range transcriptFiles(m.sid) {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if fi.Size() > m.sizes[p] {
			return true
		}
	}
	return false
}

// promptFragment picks the most distinctive slice of a prompt body to search the
// transcript for: the longest single line, capped so the needle stays a
// reasonable size. Matching a fragment rather than the whole body keeps the
// check robust against the REPL normalising whitespace at the edges of a
// multi-line paste.
func promptFragment(body string) string {
	best := ""
	for _, ln := range strings.Split(body, "\n") {
		if ln = strings.TrimSpace(ln); len(ln) > len(best) {
			best = ln
		}
	}
	if best == "" {
		return ""
	}
	const maxNeedle = 160
	if len(best) > maxNeedle {
		// Cut on a rune boundary so the needle is still valid UTF-8 and escapes
		// the same way the transcript encoded it.
		best = strings.ToValidUTF8(best[:maxNeedle], "")
	}
	return best
}

// jsonNeedle renders a fragment the way it appears inside a transcript record —
// JSON-escaped, without the surrounding quotes — so a raw line scan can find it.
//
// The transcript is written by the CLI's JavaScript (JSON.stringify), which
// leaves <, > and & as literal characters. Go's json.Marshal instead escapes
// them to < / > / & by default, so a marshalled needle would never
// match a prompt whose distinctive line contains them — shell redirects (`>`,
// `&&`), JSX/HTML (`<Component>`) and the like, a common enough class of prompts
// that the transcript check would silently fall back to the weaker roster
// heuristic exactly there. Encoding with HTML escaping off matches what the CLI
// actually wrote.
func jsonNeedle(fragment string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(fragment); err != nil {
		return ""
	}
	// Encode quotes the string and appends a newline; drop the newline and the
	// surrounding quotes, leaving the escaped body as a record carries it.
	s := strings.TrimRight(buf.String(), "\n")
	if len(s) < 2 {
		return ""
	}
	return s[1 : len(s)-1]
}

// landedIn reports whether a chunk of transcript records contains the prompt as
// a user record. Tool results are user records too, so the fragment match is
// what identifies the prompt; requiring both on the same record keeps an
// assistant echoing the text back from counting as delivery.
func landedIn(chunk, needle string) bool {
	if needle == "" {
		return false
	}
	for _, line := range strings.Split(chunk, "\n") {
		if strings.Contains(line, `"type":"user"`) && strings.Contains(line, needle) {
			return true
		}
	}
	return false
}

// Landed reports whether the prompt body has appeared as a user record in the
// session's transcript since the mark was taken — ground truth that the text
// actually reached the conversation, as opposed to the TUI-state heuristics,
// which only observe that *something* changed on screen.
func (m transcriptMark) Landed(body string) bool {
	needle := jsonNeedle(promptFragment(body))
	if needle == "" || !m.available() {
		return false
	}
	// Re-glob rather than reusing the marked paths: a resumed worker can begin a
	// new transcript file, which would not have existed at mark time.
	for _, p := range transcriptFiles(m.sid) {
		f, err := os.Open(p) // #nosec G304 -- path comes from our own projects glob
		if err != nil {
			continue
		}
		chunk, rerr := readFrom(f, m.sizes[p])
		_ = f.Close()
		if rerr == nil && landedIn(chunk, needle) {
			return true
		}
	}
	return false
}

// maxTailBytes bounds how much of a transcript a single scan reads. The scan
// runs in a poll loop, and a long-lived session's transcript can be tens of
// megabytes; a prompt appended since the mark is always well inside this tail.
const maxTailBytes = 4 << 20

// readFrom reads a file from a byte offset to its end, never more than
// maxTailBytes. A file that shrank below the offset (rotated or rewritten) is
// read from its own tail instead.
func readFrom(f *os.File, off int64) (string, error) {
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	if off > fi.Size() {
		off = 0
	}
	if tail := fi.Size() - maxTailBytes; tail > off {
		off = tail
	}
	if off > 0 {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return "", err
		}
	}
	b, err := io.ReadAll(f)
	return string(b), err
}
