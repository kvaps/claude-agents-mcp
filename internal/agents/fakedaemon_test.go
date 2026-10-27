package agents

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDaemon serves the parts of the daemon's control protocol a delivery uses —
// list, reply, subscribe and attach — for one scripted session, so SubmitPrompt
// runs end to end over a real socket with no daemon and no CLI. HOME is a temp
// dir holding the control key and the session's transcript, and PATH is emptied
// so nothing can reach a real `claude`.
type fakeDaemon struct {
	t          *testing.T
	transcript string

	mu      sync.Mutex
	sess    Session
	listSID string // session id op:list reports; differs from sess.SessionID only to test adoption
	input   string // what the input box shows
	working bool   // a turn is running: every screen read gets a fresh spinner frame
	output  []string
	replies int
	enters  int

	// onReply and onEnter script the session. They run under mu.
	onReply func(f *fakeDaemon, text string)
	onEnter func(f *fakeDaemon)
}

const fakeShort = "fa4e0001"

func newFakeDaemon(t *testing.T) (*fakeDaemon, *Client) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	if err := os.MkdirAll(filepath.Join(home, ".claude", "daemon"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "daemon", "control.key"), []byte("k\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sid := "fa4e0001-0000-4000-8000-000000000001"
	dir := filepath.Join(home, ".claude", "projects", "-tmp-project")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A short directory: a socket path under t.TempDir() can exceed the limit.
	sockDir, err := os.MkdirTemp("", "cam")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	ln, err := net.Listen("unix", filepath.Join(sockDir, "control.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	f := &fakeDaemon{
		t:          t,
		transcript: filepath.Join(dir, sid+".jsonl"),
		sess:       Session{Short: fakeShort, SessionID: sid, State: "running", Tempo: "idle", Detail: "idle"},
		listSID:    sid,
	}
	f.paint()
	go f.serve(ln)
	return f, &Client{sock: ln.Addr().String(), confirm: confirmTimings{
		window: 2500 * time.Millisecond, rescue: 300 * time.Millisecond, enter: 400 * time.Millisecond,
	}}
}

func (f *fakeDaemon) serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go f.handle(conn)
	}
}

func (f *fakeDaemon) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	line, err := r.ReadBytes('\n')
	if err != nil {
		return
	}
	var req struct {
		Op   string `json:"op"`
		Text string `json:"text"`
	}
	if json.Unmarshal(line, &req) != nil {
		return
	}
	reply := func(v any) {
		b, _ := json.Marshal(v)
		_, _ = conn.Write(append(b, '\n'))
	}
	f.mu.Lock()
	switch req.Op {
	case "list":
		s := f.sess
		s.SessionID = f.listSID
		f.mu.Unlock()
		reply(map[string]any{"ok": true, "jobs": []Session{s}})
	case "reply":
		f.replies++
		if f.onReply != nil {
			f.onReply(f, req.Text)
		}
		f.mu.Unlock()
		reply(map[string]any{"ok": true})
	case "subscribe":
		if f.working {
			f.paint()
		}
		tail := strings.Join(f.output, "")
		s := f.sess
		f.mu.Unlock()
		reply(map[string]any{"type": "snapshot", "record": s, "streamTail": []string{tail}})
	case "attach":
		f.mu.Unlock()
		reply(map[string]any{"ok": true})
		buf := make([]byte, 4096)
		for {
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			n, err := conn.Read(buf)
			if n > 0 && strings.Contains(string(buf[:n]), "\r") {
				f.mu.Lock()
				f.enters++
				if f.onEnter != nil {
					f.onEnter(f)
				}
				f.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	default:
		f.mu.Unlock()
		reply(map[string]any{"ok": false, "error": "unsupported op"})
	}
}

// paint appends one frame of the session's live area to its output, the way the
// TUI redraws: a spinner while a turn runs, then the input box and footer.
// Callers hold mu.
func (f *fakeDaemon) paint() {
	frame := "\n"
	if f.working {
		frame += "✻ Thinking… (" + time.Now().Format("05.000") + " · esc to interrupt)\n"
	}
	frame += "────────\n❯ " + f.input + "\n────────\n  ? for shortcuts\n"
	f.output = append(f.output, frame)
	if len(f.output) > 40 {
		f.output = f.output[len(f.output)-40:]
	}
}

// startTurn is what submitting does to the session: the input box empties, the
// spinner starts, the roster changes at once, and the CLI writes the prompt to
// the transcript after recordAfter (a negative delay never writes it).
func (f *fakeDaemon) startTurn(text string, recordAfter time.Duration) {
	f.input, f.working = "", true
	f.sess.Tempo, f.sess.Detail = "active", "Thinking"
	f.paint()
	if recordAfter >= 0 {
		time.AfterFunc(recordAfter, func() { f.append(cliRecord(text)) })
	}
}

// holdInput is the REPL taking the text without submitting it: a long body is
// collapsed into a paste placeholder. The repaint alone flips the roster's
// tempo, as the real daemon's does.
func (f *fakeDaemon) holdInput(text string) {
	f.input = "[Pasted text #1 +" + itoa(strings.Count(text, "\n")) + " lines]"
	if !strings.Contains(text, "\n") {
		f.input = text
	}
	f.sess.Tempo = "active"
	f.paint()
}

func (f *fakeDaemon) append(line string) {
	fh, err := os.OpenFile(f.transcript, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		f.t.Error(err)
		return
	}
	defer func() { _ = fh.Close() }()
	_, _ = fh.WriteString(line + "\n")
}

func (f *fakeDaemon) counts() (replies, enters int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.replies, f.enters
}

// cliRecord renders a submitted prompt the way the CLI records it: a slash
// command with arguments as its command-name/command-args form, anything else as
// the text itself.
func cliRecord(text string) string {
	content := text
	if name, args, ok := strings.Cut(text, " "); ok && strings.HasPrefix(text, "/") {
		content = "<command-message>" + name[1:] + "</command-message>\n<command-name>" + name +
			"</command-name>\n<command-args>" + args + "</command-args>"
	}
	return `{"type":"user","message":{"role":"user","content":` + jsonQuote(content) + `},"timestamp":"` +
		time.Now().UTC().Format(time.RFC3339Nano) + `","sessionId":"x"}`
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
