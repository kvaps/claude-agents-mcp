package agents

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Orphan is a session that exists on disk as a transcript but has no entry in
// the agents list. `claude rm` clears the entry, and the daemon drops sessions
// it no longer tracks, but neither touches ~/.claude/projects/<project>/
// <sessionId>.jsonl — and that file is the session: everything needed to bring
// it back with its full history is in it. Treating a missing list entry as "no
// such session" makes recoverable conversations look deleted.
type Orphan struct {
	SessionID string // from the transcript file name
	Path      string // the transcript itself
	Cwd       string // recovered from the transcript's records
	Title     string // recovered from its custom-title / agent-name records
	Records   int    // rough size signal, for reporting
}

// Short is the id the agents view would show for this session: a short id is the
// first 8 hex digits of the session id, which is what makes a short-id lookup by
// transcript file name possible at all.
func (o *Orphan) Short() string {
	if len(o.SessionID) < 8 {
		return o.SessionID
	}
	return o.SessionID[:8]
}

// hexRef matches a reference that could name a transcript file: a short id, a
// full session UUID, or a prefix of either. Anything shorter than 8 hex digits
// is refused — a two-character "prefix" would match dozens of unrelated
// sessions, and resuming the wrong conversation is worse than not finding one.
var hexRef = regexp.MustCompile(`^[0-9a-fA-F]{8}[0-9a-fA-F-]*$`)

// FindOrphan locates a session by its transcript when the agents list does not
// have it. ref may be a short id, a full session id, or a longer prefix of one.
//
// It returns nil (no error) when ref cannot name a transcript or none matches,
// so callers can keep their existing "no session matching …" error for the case
// where the session really does not exist anywhere.
func FindOrphan(ref string) *Orphan {
	ref = strings.ToLower(strings.TrimSpace(ref))
	if !hexRef.MatchString(ref) {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	matches, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", ref+"*.jsonl"))
	if len(matches) == 0 {
		return nil
	}
	// A session that moved between project directories has a transcript in each;
	// the freshest one is the conversation to resume.
	sort.Slice(matches, func(i, j int) bool {
		fi, ei := os.Stat(matches[i])
		fj, ej := os.Stat(matches[j])
		if ei != nil || ej != nil {
			return false
		}
		return fi.ModTime().After(fj.ModTime())
	})
	path := matches[0]
	sid := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if !IsFullSessionID(sid) {
		return nil
	}
	o := &Orphan{SessionID: sid, Path: path}
	o.Cwd, o.Title, o.Records = readOrphanMeta(path)
	return o
}

// orphanScanBytes is how much of each end of a transcript is scanned for the
// metadata below. Transcripts run to tens of megabytes and the fields wanted
// here sit at the ends: the launch cwd in the first records, the current cwd and
// the latest title near the last.
const orphanScanBytes = 256 << 10

var (
	cwdRe   = regexp.MustCompile(`"cwd":"((?:[^"\\]|\\.)*)"`)
	titleRe = regexp.MustCompile(`"customTitle":"((?:[^"\\]|\\.)*)"`)
	nameRe  = regexp.MustCompile(`"agentName":"((?:[^"\\]|\\.)*)"`)
)

// readOrphanMeta recovers what is needed to resurrect a session from its
// transcript: the working directory it ran in and the display name it was given.
//
// The tail is preferred over the head for both. A session can change directory
// mid-run, and the title is whatever the last `custom-title` record says; the
// head is the fallback, which for cwd is the directory it was launched in.
func readOrphanMeta(path string) (cwd, title string, records int) {
	head, tail := readEnds(path, orphanScanBytes)
	records = strings.Count(head, "\n")
	if tail != head {
		records += strings.Count(tail, "\n")
	}
	cwd = firstExisting(lastMatches(tail, cwdRe), lastMatches(head, cwdRe))
	title = lastMatch(tail, titleRe)
	if title == "" {
		title = lastMatch(head, titleRe)
	}
	if title == "" {
		title = firstNonEmpty(lastMatch(tail, nameRe), lastMatch(head, nameRe))
	}
	return cwd, title, records
}

// readEnds returns up to n bytes from the start and from the end of a file. A
// file smaller than n yields the same content twice.
func readEnds(path string, n int64) (head, tail string) {
	f, err := os.Open(path) // #nosec G304 -- path comes from our own projects glob
	if err != nil {
		return "", ""
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return "", ""
	}
	buf := make([]byte, min64(n, fi.Size()))
	if _, err := f.ReadAt(buf, 0); err != nil && len(buf) > 0 {
		return "", ""
	}
	head = string(buf)
	if fi.Size() <= n {
		return head, head
	}
	tbuf := make([]byte, n)
	if _, err := f.ReadAt(tbuf, fi.Size()-n); err != nil {
		return head, head
	}
	return head, string(tbuf)
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// lastMatches returns every capture of re in s, latest first.
func lastMatches(s string, re *regexp.Regexp) []string {
	ms := re.FindAllStringSubmatch(s, -1)
	out := make([]string, 0, len(ms))
	for i := len(ms) - 1; i >= 0; i-- {
		if v := unescape(ms[i][1]); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func lastMatch(s string, re *regexp.Regexp) string {
	if ms := lastMatches(s, re); len(ms) > 0 {
		return ms[0]
	}
	return ""
}

// firstExisting returns the first candidate directory that still exists. A
// session that moved into a worktree since deleted should fall back to where it
// started rather than reporting a directory that is gone.
func firstExisting(groups ...[]string) string {
	var first string
	for _, g := range groups {
		for _, cand := range g {
			if first == "" {
				first = cand
			}
			if !cwdMissing(cand) {
				return cand
			}
		}
	}
	return first // nothing exists; return one so the error can name it
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// unescape undoes the JSON string escaping of a captured field. Only the escapes
// a path or a title can realistically carry are handled; anything else is left
// as-is rather than failing the whole recovery.
func unescape(s string) string {
	return strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\/`, `/`, `\n`, "\n", `\t`, "\t").Replace(s)
}

// Session renders an orphan as the not-running session it is, so it flows
// through the same code paths as a listed one. It has no short id: nothing has
// allocated one, and one appears only when a worker is spawned.
func (o *Orphan) Session() Session {
	return Session{
		SessionID: o.SessionID,
		Name:      o.Title,
		Cwd:       o.Cwd,
		State:     "exited",
		Live:      false,
		Resumable: o.Cwd != "" && !cwdMissing(o.Cwd),
	}
}

// ResolveAny finds a session the way Resolve does, and falls back to the
// transcript on disk when the agents list has no entry for it.
//
// Losing the list entry — `claude rm`, a daemon that no longer tracks it — does
// not lose the conversation, so it must not make the session unreachable. The
// fallback only accepts references that can name a transcript file (a short id
// or session id, or a prefix of one); a name that is not in the list still
// reports not-found, because names live in the list, not in a file name.
func (c *Client) ResolveAny(ref string) (Session, error) {
	sess, err := c.Resolve(ref)
	if err == nil {
		return sess, nil
	}
	o := FindOrphan(ref)
	if o == nil {
		return Session{}, err // genuinely nothing on disk either
	}
	return o.Session(), nil
}
