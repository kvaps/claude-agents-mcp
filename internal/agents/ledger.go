package agents

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// The sent ledger records every message this server (any process of it) has
// sent: one JSON file per message id under <state dir>/sent, plus an index from
// a sender's idempotency key to the id. It answers two questions a sender has
// after the call returned: "what became of message X" (message_status), and
// "did my retry send it twice" (it did not — a repeated key returns the first
// record).
//
// Message ids are minted by the server, so they are unique across senders. An
// idempotency key is the sender's own, so it is scoped to the sender: two
// agents may both use key "1" without colliding.

// SentRecord is the ledger's view of one message.
type SentRecord struct {
	ID        string    `json:"id"`
	Key       string    `json:"idempotency_key,omitempty"` // the sender's key, when it gave one
	From      string    `json:"from"`                      // sender label
	To        string    `json:"to"`                        // recipient label
	ToAddress string    `json:"to_address"`                // what to write in `to` to reach the same recipient
	ToKind    string    `json:"to_kind"`                   // session or mailbox
	Status    string    `json:"status"`                    // queued, delivered, read or failed
	Reason    string    `json:"reason,omitempty"`          // why it failed, or what queued means right now
	Delivery  string    `json:"delivery,omitempty"`        // how a session delivery was confirmed
	At        time.Time `json:"at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// idempotencyKey constrains a sender-supplied key to something that is safe as
// a file name and short enough to be one the sender actually typed.
var idempotencyKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)

// ValidateIdempotencyKey reports why a key cannot be used.
func ValidateIdempotencyKey(key string) error {
	if !idempotencyKey.MatchString(key) {
		return fmt.Errorf("idempotency key %q is invalid: use 1-64 letters, digits, dots, dashes, colons or underscores", key)
	}
	return nil
}

func sentDir() (string, error) {
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "sent"), nil
}

func sentPath(id string) (string, error) {
	dir, err := sentDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, id+".json"), nil
}

// keyPath is where a sender's idempotency key is mapped to a message id. The
// sender is part of the name so keys are scoped per sender; an unidentified
// sender shares the "anonymous" scope.
func keyPath(sender, key string) (string, error) {
	dir, err := sentDir()
	if err != nil {
		return "", err
	}
	if sender == "" {
		sender = "anonymous"
	}
	return filepath.Join(dir, "keys", sender+"__"+key), nil
}

// readSent returns the ledger record for an id, if there is one.
func readSent(id string) (SentRecord, bool, error) {
	p, err := sentPath(id)
	if err != nil {
		return SentRecord{}, false, err
	}
	var rec SentRecord
	if err := readJSON(p, &rec); err != nil {
		if os.IsNotExist(err) {
			return SentRecord{}, false, nil
		}
		return SentRecord{}, false, err
	}
	return rec, true, nil
}

// writeSent stores a ledger record, creating the directory on first use.
func writeSent(rec SentRecord) error {
	p, err := sentPath(rec.ID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	rec.UpdatedAt = time.Now()
	return writeJSONAtomic(p, rec)
}

// claimKey records that key belongs to id, unless the key is already taken, in
// which case the id it belongs to is returned with claimed=false. The check and
// the write happen under the ledger lock, so two processes retrying the same
// send at the same moment cannot both claim it.
func claimKey(sender, key, id string) (string, bool, error) {
	p, err := keyPath(sender, key)
	if err != nil {
		return "", false, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", false, err
	}
	release, err := acquireLock(filepath.Dir(p))
	if err != nil {
		return "", false, err
	}
	defer release()
	if b, rerr := os.ReadFile(p); rerr == nil {
		if existing := strings.TrimSpace(string(b)); existing != "" {
			return existing, false, nil
		}
	}
	if err := os.WriteFile(p, []byte(id+"\n"), 0o600); err != nil {
		return "", false, err
	}
	return id, true, nil
}

// readJSON reads a JSON file into v.
func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
