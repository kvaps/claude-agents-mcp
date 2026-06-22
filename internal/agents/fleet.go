package agents

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

// The agents view (FleetView) persists pin and ordering state on disk under
// ~/.claude/jobs, independent of the daemon control protocol:
//
//   - pins.json            — a JSON array of pinned short ids (the pin set)
//   - <short>/order        — this session's sortOrder (a number, flat ordering)
//   - <short>/stateOrder   — this session's stateSortOrder (within a state group)
//   - <short>/state.json   — per-session meta (name/color); must exist for the
//     picker to honour the order files
//
// Sorting puts pinned sessions first, then orders by `sortOrder ?? createdAt`
// (lower = higher in the list). pin_session and reorder_session below write
// exactly these files, matching what ctrl+t and shift+↑/↓ do in the picker.

// jobsDir returns ~/.claude/jobs, where the agents view keeps its state.
func jobsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "jobs"), nil
}

func pinsPath() (string, error) {
	dir, err := jobsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "pins.json"), nil
}

// acquireLock takes a proper-lockfile-compatible lock on target by creating a
// sibling "<target>.lock" directory (an atomic mkdir), so it mutually excludes
// a concurrently-running picker. A lock whose mtime is older than the stale
// window is assumed abandoned and stolen.
func acquireLock(target string) (release func(), err error) {
	lock := target + ".lock"
	const stale = 10 * time.Second
	deadline := time.Now().Add(3 * time.Second)
	for {
		if mkErr := os.Mkdir(lock, 0o700); mkErr == nil {
			return func() { _ = os.Remove(lock) }, nil
		} else if !os.IsExist(mkErr) {
			return nil, mkErr
		}
		if fi, statErr := os.Stat(lock); statErr == nil && time.Since(fi.ModTime()) > stale {
			_ = os.Remove(lock)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("could not lock %s (held by another process)", filepath.Base(target))
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// readPins reads the pin set as an ordered, de-duplicated slice of short ids.
// A missing file is an empty set.
func readPins(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var arr []string
	if err := json.Unmarshal(b, &arr); err != nil {
		return nil, fmt.Errorf("parse %s: %w", filepath.Base(path), err)
	}
	seen := make(map[string]bool, len(arr))
	out := make([]string, 0, len(arr))
	for _, id := range arr {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, nil
}

// PinnedSet returns the set of pinned short ids (best-effort; empty on any
// error so it never blocks listing).
func PinnedSet() map[string]bool {
	path, err := pinsPath()
	if err != nil {
		return nil
	}
	ids, err := readPins(path)
	if err != nil {
		return nil
	}
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return set
}

// writeJSONAtomic writes v as 2-space-indented JSON via a temp file + rename,
// matching the picker's JSON.stringify(value, null, 2) on-disk format.
func writeJSONAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp." + randID()
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// Pin adds (pinned=true) or removes (pinned=false) a session's short id from
// the agents-view pin set — the same effect as ctrl+t in the picker.
func (c *Client) Pin(short string, pinned bool) error {
	if short == "" {
		return fmt.Errorf("empty session short id")
	}
	path, err := pinsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	release, err := acquireLock(path)
	if err != nil {
		return err
	}
	defer release()

	ids, err := readPins(path)
	if err != nil {
		return err
	}
	has := false
	for _, id := range ids {
		if id == short {
			has = true
			break
		}
	}
	switch {
	case pinned && has, !pinned && !has:
		return nil // already in the desired state
	case pinned:
		ids = append(ids, short)
	default:
		kept := make([]string, 0, len(ids))
		for _, id := range ids {
			if id != short {
				kept = append(kept, id)
			}
		}
		ids = kept
	}
	if err := writeJSONAtomic(path, ids); err != nil {
		return err
	}
	touchState(short)
	return nil
}

// orderFile reads a session's numeric sortOrder file ("order" or "stateOrder"),
// returning nil when it is absent or unparseable.
func orderFile(short, name string) *float64 {
	dir, err := jobsDir()
	if err != nil {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(dir, short, name))
	if err != nil {
		return nil
	}
	v, err := strconv.ParseFloat(string(b), 64)
	if err != nil {
		return nil
	}
	return &v
}

// writeOrderFiles writes both order (sortOrder) and stateOrder for a session,
// so the new position is honoured in flat and state-grouped views alike.
func writeOrderFiles(short string, val float64) error {
	dir, err := jobsDir()
	if err != nil {
		return err
	}
	sdir := filepath.Join(dir, short)
	if err := os.MkdirAll(sdir, 0o755); err != nil {
		return err
	}
	s := strconv.FormatFloat(val, 'f', -1, 64)
	for _, name := range []string{"order", "stateOrder"} {
		if err := os.WriteFile(filepath.Join(sdir, name), []byte(s), 0o644); err != nil {
			return err
		}
	}
	touchState(short)
	return nil
}

// touchState bumps the session's state.json mtime so a live picker, which polls
// it, reloads and repaints. Best-effort: absent state.json is fine.
func touchState(short string) {
	dir, err := jobsDir()
	if err != nil {
		return
	}
	p := filepath.Join(dir, short, "state.json")
	now := time.Now()
	_ = os.Chtimes(p, now, now)
}

// effRow is a daemon session with its effective flat sort key.
type effRow struct {
	short string
	eff   float64
}

// Reorder moves a session within the agents view's flat ordering. With
// direction "up"/"down" it steps one slot; with a position it jumps to that
// 0-based index. It writes the moved session's order files to a value between
// its new neighbours, leaving every other session untouched.
func (c *Client) Reorder(target, direction string, position int, hasPosition bool) error {
	jobs, err := c.listDaemon()
	if err != nil {
		return err
	}
	rows := make([]effRow, 0, len(jobs))
	found := false
	for _, s := range jobs {
		if s.Backend != "daemon" {
			continue
		}
		eff := float64(s.StartedAt)
		if v := orderFile(s.Short, "order"); v != nil {
			eff = *v
		}
		rows = append(rows, effRow{s.Short, eff})
		if s.Short == target {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("session %s is not a running daemon session (cannot reorder)", target)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].eff != rows[j].eff {
			return rows[i].eff < rows[j].eff
		}
		return rows[i].short < rows[j].short
	})
	n := len(rows)
	cur := 0
	for i, r := range rows {
		if r.short == target {
			cur = i
			break
		}
	}

	var desired int
	switch {
	case hasPosition:
		desired = position
	case direction == "up":
		desired = cur - 1
	case direction == "down":
		desired = cur + 1
	default:
		return fmt.Errorf("reorder needs direction \"up\"/\"down\" or a position")
	}
	if desired < 0 {
		desired = 0
	}
	if desired > n-1 {
		desired = n - 1
	}
	if desired == cur {
		return nil // already there
	}

	// Neighbours at the destination slot, excluding the target itself.
	rest := make([]effRow, 0, n-1)
	for _, r := range rows {
		if r.short != target {
			rest = append(rest, r)
		}
	}
	var above, below *float64
	if desired > 0 {
		above = &rest[desired-1].eff
	}
	if desired <= n-2 {
		below = &rest[desired].eff
	}

	var newVal float64
	switch {
	case above != nil && below != nil:
		newVal = (*above + *below) / 2
	case below != nil: // new top
		newVal = *below - 1
	case above != nil: // new bottom
		newVal = *above + 1
	}
	return writeOrderFiles(target, newVal)
}
