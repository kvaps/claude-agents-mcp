package agents

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
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
// later scan reads only what was appended past those offsets, so an identical
// prompt delivered earlier in the conversation cannot be mistaken for the
// current one. since, when set, additionally drops records stamped before it;
// it is what makes a mark adopted after delivery (see adopt) safe.
type transcriptMark struct {
	sid   string
	sizes map[string]int64
	since time.Time
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

// adopt gives a mark taken before the session id was known one for sid. No
// offsets can be recovered by then, so the files are read whole and records
// stamped before since are ignored instead.
func (m *transcriptMark) adopt(sid string, since time.Time) {
	m.sid, m.sizes, m.since = sid, map[string]int64{}, since
}

// available reports whether the mark can produce ground truth at all — i.e. the
// session has a session id, so its transcript can be located.
func (m transcriptMark) available() bool { return m.sid != "" }

// Grew reports whether any of the session's transcripts has been appended to
// since the mark was taken.
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

// transcriptScan is what the records appended since a mark say about one
// delivery.
type transcriptScan struct {
	// Landed: the prompt is in a user record — it reached the conversation.
	Landed bool
	// Enqueued: the CLI recorded the prompt in its command queue, so the REPL
	// took it as submitted input rather than leaving it in the input box.
	Enqueued bool
	// Foreign: conversation records (assistant output, tool results, other
	// prompts) that are not the prompt. Before the prompt lands they can only
	// belong to a turn that was already running, so the prompt is queued behind
	// it.
	Foreign bool
	// Grew: anything at all was appended.
	Grew bool
}

// Scan reads the records appended since the mark and reports what they say
// about body. A record still being written is skipped and picked up whole by
// the next scan.
func (m transcriptMark) Scan(body string) transcriptScan {
	var s transcriptScan
	if !m.available() {
		return s
	}
	needle := promptNeedle(body)
	// Re-glob rather than reusing the marked paths: a resumed worker can begin a
	// new transcript file, which would not have existed at mark time.
	for _, p := range transcriptFiles(m.sid) {
		f, err := os.Open(p) // #nosec G304 -- path comes from our own projects glob
		if err != nil {
			continue
		}
		chunk, rerr := readFrom(f, m.sizes[p])
		_ = f.Close()
		if rerr != nil || chunk == "" {
			continue
		}
		// An adopted mark reads whole files; there only records stamped since
		// the delivery count as growth (scanChunk sets it).
		s.Grew = s.Grew || m.since.IsZero()
		m.scanChunk(chunk, needle, &s)
	}
	return s
}

// Landed reports whether the prompt body has appeared as a user record in the
// session's transcript since the mark was taken — ground truth that the text
// actually reached the conversation, as opposed to the TUI-state heuristics,
// which only observe that *something* changed on screen.
func (m transcriptMark) Landed(body string) bool { return m.Scan(body).Landed }

// transcriptRecord is the part of a transcript line the delivery checks read.
type transcriptRecord struct {
	Type      string          `json:"type"`
	Operation string          `json:"operation"`
	Content   json.RawMessage `json:"content"`
	Timestamp string          `json:"timestamp"`
	IsMeta    bool            `json:"isMeta"`
	Message   struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

func (m transcriptMark) scanChunk(chunk, needle string, s *transcriptScan) {
	for _, line := range strings.Split(chunk, "\n") {
		// Most of a transcript is records no check reads; skip them unparsed.
		if !strings.Contains(line, `"type":"user"`) && !strings.Contains(line, `"type":"assistant"`) &&
			!strings.Contains(line, `"type":"queue-operation"`) {
			continue
		}
		var r transcriptRecord
		if json.Unmarshal([]byte(line), &r) != nil {
			continue
		}
		if m.before(r.Timestamp) {
			continue
		}
		s.Grew = true
		switch r.Type {
		case "user":
			if text, ok := recordText(r.Message.Content); ok && containsNormalised(text, needle) {
				s.Landed = true
			} else if !r.IsMeta {
				s.Foreign = true
			}
		case "assistant":
			s.Foreign = true
		case "queue-operation":
			if text, ok := recordText(r.Content); r.Operation == "enqueue" && ok && containsNormalised(text, needle) {
				s.Enqueued = true
			}
		}
	}
}

// before reports whether a record cannot be shown to postdate an adopted mark:
// it is stamped earlier, or not stamped at all. The second of slack absorbs the
// CLI stamping a record a moment before the server took its own reading of the
// clock.
func (m transcriptMark) before(stamp string) bool {
	if m.since.IsZero() {
		return false
	}
	t, err := time.Parse(time.RFC3339Nano, stamp)
	return err != nil || t.Before(m.since.Add(-time.Second))
}

// recordText returns the typed text of a message's content: the string itself,
// or the text blocks of a block list. Tool results and images are not text a
// user typed, so a record made only of them has none.
func recordText(raw json.RawMessage) (string, bool) {
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return str, true
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return "", false
	}
	var b strings.Builder
	for _, bl := range blocks {
		if bl.Type == "text" {
			b.WriteString(bl.Text)
			b.WriteByte('\n')
		}
	}
	return b.String(), b.Len() > 0
}

// promptNeedle picks the most distinctive slice of a prompt body to search the
// transcript for: the longest single line, whitespace-normalised and capped so
// the needle stays a reasonable size.
//
// A slash command with arguments is matched on its arguments: the CLI records
// "/goal <condition>" as <command-name>/goal</command-name> and
// <command-args><condition></command-args>, so the line as typed never appears.
func promptNeedle(body string) string {
	if strings.HasPrefix(body, "/") {
		if _, args, ok := strings.Cut(body, " "); ok && strings.TrimSpace(args) != "" {
			body = args
		}
	}
	best := ""
	for _, ln := range strings.Split(body, "\n") {
		if ln = normaliseText(ln); len(ln) > len(best) {
			best = ln
		}
	}
	const maxNeedle = 160
	if len(best) > maxNeedle {
		// Cut on a rune boundary so the needle is still valid UTF-8.
		best = strings.ToValidUTF8(best[:maxNeedle], "")
	}
	return best
}

// normaliseText collapses every run of whitespace — tabs, CR/LF, non-breaking
// spaces — to one space and trims the ends, so a needle still matches a prompt
// the REPL reflowed on its way into the transcript.
func normaliseText(s string) string { return strings.Join(strings.Fields(s), " ") }

func containsNormalised(text, needle string) bool {
	return needle != "" && strings.Contains(normaliseText(text), needle)
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
