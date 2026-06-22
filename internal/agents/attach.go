package agents

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"
)

func randID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// readLine reads bytes from conn until a newline, returning the line (without
// the newline) and any bytes already read past it.
func readLine(conn net.Conn, timeout time.Duration) (line, rest []byte, err error) {
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	var buf []byte
	tmp := make([]byte, 4096)
	for {
		n, e := conn.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if i := bytes.IndexByte(buf, '\n'); i >= 0 {
				return buf[:i], buf[i+1:], nil
			}
		}
		if e != nil {
			return buf, nil, e
		}
	}
}

// settleGap is how long the PTY must be silent before output is considered
// settled. Reads return one gap after the last byte, so a redraw that finishes
// quickly is returned promptly instead of waiting out a fixed window.
const settleGap = 100 * time.Millisecond

// readSettle reads bytes until the stream stays quiet for idle, or until
// maxWait elapses as a hard backstop, returning everything read. This lets a
// call return as soon as the terminal stops drawing (the common case) while
// still bounding the wait when a session keeps repainting (e.g. a spinner).
func readSettle(conn net.Conn, seed []byte, idle, maxWait time.Duration) []byte {
	out := append([]byte{}, seed...)
	deadline := time.Now().Add(maxWait)
	tmp := make([]byte, 8192)
	for {
		now := time.Now()
		if !now.Before(deadline) {
			return out
		}
		wait := idle
		if d := deadline.Sub(now); d < wait {
			wait = d
		}
		_ = conn.SetReadDeadline(now.Add(wait))
		n, e := conn.Read(tmp)
		if n > 0 {
			out = append(out, tmp[:n]...)
			continue
		}
		if e != nil {
			if ne, ok := e.(net.Error); ok && ne.Timeout() {
				if wait >= idle {
					return out // a full idle gap with no data: settled
				}
				continue // gap was clipped by the backstop; next loop hits it
			}
			return out // EOF or a real error
		}
		return out // no data, no error (unusual): don't spin
	}
}

// SendBytes attaches to a session's PTY, writes data as keystrokes, reads the
// resulting output until it settles (capped at maxWait), and returns the screen
// as plain text. Attach is additive (co-attach), so it does not disturb other
// viewers of the session.
func (c *Client) SendBytes(short string, data []byte, maxWait time.Duration) (string, error) {
	if strings.TrimSpace(short) == "" {
		return "", fmt.Errorf("session is not running (no live attach target)")
	}
	sock, err := FindSocket()
	if err != nil {
		return "", err
	}
	conn, err := net.DialTimeout("unix", sock, 3*time.Second)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()

	attach := map[string]any{
		"proto": 1, "op": "attach", "short": short,
		"cols": 120, "rows": 40, "attachId": randID(),
		"caps": map[string]any{"terminal": "xterm-256color", "mux": nil, "ssh": false},
	}
	req, err := json.Marshal(attach)
	if err != nil {
		return "", err
	}
	if _, err := conn.Write(append(req, '\n')); err != nil {
		return "", err
	}

	ackLine, rest, err := readLine(conn, 5*time.Second)
	if err != nil && len(ackLine) == 0 {
		return "", fmt.Errorf("attach: no ack: %w", err)
	}
	var ack struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(ackLine, &ack)
	if !ack.OK {
		return "", fmt.Errorf("attach rejected: %s", ack.Error)
	}

	// Drain the initial screen repaint (rest holds any bytes after the ack)
	// until it settles, so it isn't mixed into the response below.
	_ = readSettle(conn, rest, settleGap, 350*time.Millisecond)

	if len(data) > 0 {
		if _, err := conn.Write(data); err != nil {
			return "", err
		}
	}
	out := readSettle(conn, nil, settleGap, maxWait)
	return StripANSI(string(out)), nil
}

// ReadScreen returns the current screen of a session as plain text.
func (c *Client) ReadScreen(short string, tail int) (string, error) {
	if tail <= 0 {
		tail = 200
	}
	_, screen, err := c.Snapshot(short, tail)
	if err != nil {
		return "", err
	}
	return StripANSI(screen), nil
}

// SendText types text into the session, optionally submitting with Enter.
func (c *Client) SendText(short, text string, submit bool) (string, error) {
	data := []byte(text)
	if submit {
		data = append(data, '\r')
	}
	return c.SendBytes(short, data, 1000*time.Millisecond)
}

// SendKeys sends a sequence of named keys (e.g. "esc", "down", "ctrl-c").
func (c *Client) SendKeys(short string, keys []string) (string, error) {
	var data []byte
	for _, k := range keys {
		b, err := KeyBytes(k)
		if err != nil {
			return "", err
		}
		data = append(data, b...)
	}
	return c.SendBytes(short, data, 600*time.Millisecond)
}

// SendCommand runs a slash command reliably: it clears any open modal, waits
// for the session to be idle, then types the command and submits it.
func (c *Client) SendCommand(short, command string) (string, error) {
	cmd := strings.TrimSpace(command)
	if !strings.HasPrefix(cmd, "/") {
		cmd = "/" + cmd
	}
	if _, err := c.SendBytes(short, []byte("\x1b"), 250*time.Millisecond); err != nil {
		return "", err
	}
	if err := c.WaitIdle(short, 30*time.Second); err != nil {
		return "", err
	}
	return c.SendBytes(short, append([]byte(cmd), '\r'), 1500*time.Millisecond)
}

// Cancel interrupts the current task: Esc by default, Ctrl-C when hard.
func (c *Client) Cancel(short string, hard bool) (string, error) {
	key := []byte("\x1b")
	if hard {
		key = []byte("\x03")
	}
	return c.SendBytes(short, key, 600*time.Millisecond)
}
