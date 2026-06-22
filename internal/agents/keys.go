package agents

import (
	"fmt"
	"strings"
)

// keyBytes maps human key names to the byte sequence a terminal would send.
var keyBytes = map[string]string{
	"enter":     "\r",
	"return":    "\r",
	"esc":       "\x1b",
	"escape":    "\x1b",
	"tab":       "\t",
	"space":     " ",
	"backspace": "\x7f",
	"delete":    "\x1b[3~",
	"up":        "\x1b[A",
	"down":      "\x1b[B",
	"right":     "\x1b[C",
	"left":      "\x1b[D",
	"home":      "\x1b[H",
	"end":       "\x1b[F",
	"pageup":    "\x1b[5~",
	"pagedown":  "\x1b[6~",
	"ctrl-c":    "\x03",
	"ctrl-d":    "\x04",
	"ctrl-u":    "\x15",
	"ctrl-l":    "\x0c",
	"ctrl-z":    "\x1a",
	"ctrl-r":    "\x12",
}

// KeyBytes resolves a key name (e.g. "enter", "ctrl-c", "down") to its byte
// sequence. A single printable character is sent as-is.
func KeyBytes(name string) (string, error) {
	if b, ok := keyBytes[strings.ToLower(strings.TrimSpace(name))]; ok {
		return b, nil
	}
	if len([]rune(name)) == 1 {
		return name, nil
	}
	return "", fmt.Errorf("unknown key %q", name)
}
