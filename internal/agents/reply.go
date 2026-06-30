package agents

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Reply submits text to a running session as a turn via the daemon's native
// op:reply — the same control-socket path the `claude` CLI uses to message a
// background session — instead of emulating keystrokes into its PTY. The op is
// authenticated with the daemon control key; the daemon hands the text to the
// worker's REPL, which submits it as a turn (plain prompts and slash commands
// such as /goal alike).
//
// It mirrors the CLI's own reply sender: it retries the transient
// booting/non-interactive states the daemon reports (ESTARTING while the worker
// is still coming up, ENOREPLY while it is momentarily not accepting input) and
// refreshes the auth key once on EAUTH (the daemon was restarted and rotated its
// key). A missing job (ENOJOB) or any other code is returned as an error so the
// caller can fall back to the PTY path.
func (c *Client) Reply(short, text string) error {
	if strings.TrimSpace(short) == "" {
		return fmt.Errorf("session is not running (no reply target)")
	}
	key, err := controlKey()
	if err != nil {
		return err
	}
	const maxAttempts = 12
	refreshedAuth := false
	for attempt := 0; attempt < maxAttempts; attempt++ {
		raw, reqErr := c.request(map[string]any{
			"proto": 1, "op": "reply", "short": short, "text": text, "auth": key,
		})
		if reqErr != nil {
			return reqErr
		}
		var resp struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
			Code  string `json:"code"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			return fmt.Errorf("parse reply response: %w", err)
		}
		if resp.OK {
			return nil
		}
		switch resp.Code {
		case "ESTARTING", "ENOREPLY":
			time.Sleep(200 * time.Millisecond)
		case "EAUTH":
			if refreshedAuth {
				return fmt.Errorf("reply rejected (daemon control key mismatch): %s", resp.Error)
			}
			k, kerr := controlKey()
			if kerr != nil {
				return kerr
			}
			key, refreshedAuth = k, true
		default:
			return fmt.Errorf("reply rejected: %s", resp.Error)
		}
	}
	return fmt.Errorf("session %s did not accept the prompt after %d attempts", short, maxAttempts)
}
