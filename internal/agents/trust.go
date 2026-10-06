package agents

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// envTrustRoots lists the folders under which create_session may record
// workspace trust, separated like PATH. Unset or empty disables pre-trusting.
const envTrustRoots = "CLAUDE_AGENTS_TRUST_ROOTS"

// configLockWait bounds how long a trust write waits for Claude Code's config
// lock. Claude Code holds it for one read-modify-write, and retries its own
// acquisition for about ten seconds, so a lock still held after that is stale.
var configLockWait = 12 * time.Second

// lockHoldLimit is how long a trust write may hold the config lock before it
// gives up instead of renaming: half the stale window, after which another
// process may legitimately consider the lock abandoned and steal it.
const lockHoldLimit = 5 * time.Second

// trustProjectDefaults is the entry Claude Code writes for a project it has no
// entry for when it records trust (its default project config with
// hasTrustDialogAccepted set), so a pre-trusted project looks like one trusted
// through the dialog.
var trustProjectDefaults = []struct {
	key string
	val string
}{
	{"allowedTools", `[]`},
	{"mcpContextUris", `[]`},
	{"mcpServers", `{}`},
	{"enabledMcpjsonServers", `[]`},
	{"disabledMcpjsonServers", `[]`},
	{"hasTrustDialogAccepted", `true`},
	{"hasClaudeMdExternalIncludesApproved", `false`},
	{"hasClaudeMdExternalIncludesWarningShown", `false`},
}

// TrustResult reports what PreTrust did.
type TrustResult struct {
	Project string // the directory whose trust Claude Code checks for cwd
	Already bool   // trust was already recorded; nothing was written
}

// TrustRoots returns the operator-configured trust roots from
// CLAUDE_AGENTS_TRUST_ROOTS, each resolved to its real path. Roots that do not
// exist are skipped: they cannot contain anything to trust.
func TrustRoots() ([]string, error) {
	var roots []string
	for _, r := range filepath.SplitList(os.Getenv(envTrustRoots)) {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if r == "~" || strings.HasPrefix(r, "~/") {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, err
			}
			r = filepath.Join(home, r[1:])
		}
		if !filepath.IsAbs(r) {
			return nil, fmt.Errorf("%s: %q is not an absolute path", envTrustRoots, r)
		}
		real, err := filepath.EvalSymlinks(r)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		roots = append(roots, real)
	}
	return roots, nil
}

// PreTrust records Claude Code's workspace trust for the project cwd belongs
// to, so `claude --bg` starts there without the interactive trust dialog. It
// refuses unless that project lies strictly inside one of the trust roots.
func PreTrust(cwd string) (TrustResult, error) {
	roots, err := TrustRoots()
	if err != nil {
		return TrustResult{}, err
	}
	if len(roots) == 0 {
		return TrustResult{}, fmt.Errorf("pre-trusting a workspace is disabled: the server's operator has not set %s", envTrustRoots)
	}
	project, err := trustProject(cwd)
	if err != nil {
		return TrustResult{}, err
	}
	if !insideAny(project, roots) {
		return TrustResult{Project: project}, fmt.Errorf("refusing to trust %s: it is not inside any of the folders in %s (%s)", project, envTrustRoots, strings.Join(roots, string(filepath.ListSeparator)))
	}
	if home, err := os.UserHomeDir(); err == nil {
		if real, err := filepath.EvalSymlinks(home); err == nil && real == project {
			return TrustResult{Project: project}, fmt.Errorf("refusing to trust the home directory %s", project)
		}
	}
	path, err := globalConfigPath()
	if err != nil {
		return TrustResult{Project: project}, err
	}
	already, err := markTrusted(path, project)
	return TrustResult{Project: project, Already: already}, err
}

// insideAny reports whether p lies strictly below one of roots. A root itself
// is not trustable: trust on a directory outside any repository also covers
// every directory below it that is not a repository either.
func insideAny(p string, roots []string) bool {
	for _, r := range roots {
		if rel, err := filepath.Rel(r, p); err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// globalConfigPath is the file Claude Code keeps per-project trust in:
// $CLAUDE_CONFIG_DIR/.claude.json, or ~/.claude.json without the variable.
func globalConfigPath() (string, error) {
	if d := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); d != "" {
		return filepath.Join(d, ".claude.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude.json"), nil
}

// trustProject returns the directory Claude Code keys cwd's trust under: the
// canonical root of the git repository cwd is in — the main checkout for a
// linked worktree, the repository directory for a worktree of a bare clone —
// or cwd itself outside a repository. Paths are resolved first, as the CLI
// resolves them before it checks.
func trustProject(cwd string) (string, error) {
	if !filepath.IsAbs(cwd) {
		return "", fmt.Errorf("cwd %q is not an absolute path", cwd)
	}
	real, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return "", err
	}
	if fi, err := os.Stat(real); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("cwd %q is not a directory", cwd)
	}
	for dir := real; ; {
		if fi, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			if fi.IsDir() {
				return dir, nil
			}
			return canonicalWorktreeRoot(dir), nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return real, nil
		}
		dir = parent
	}
}

// canonicalWorktreeRoot follows a `.git` file the way the CLI does: only a
// linked worktree whose admin directory sits in <common>/worktrees and points
// back at this checkout resolves to its repository; anything else (a
// submodule, a hand-written gitdir) is its own root.
func canonicalWorktreeRoot(root string) string {
	b, err := os.ReadFile(filepath.Join(root, ".git"))
	if err != nil {
		return root
	}
	line := strings.TrimSpace(string(b))
	if !strings.HasPrefix(line, "gitdir:") {
		return root
	}
	admin := resolveFrom(root, strings.TrimSpace(strings.TrimPrefix(line, "gitdir:")))
	common, err := os.ReadFile(filepath.Join(admin, "commondir"))
	if err != nil {
		return root
	}
	commonDir := resolveFrom(admin, strings.TrimSpace(string(common)))
	if filepath.Dir(admin) != filepath.Join(commonDir, "worktrees") {
		return root
	}
	back, err := os.ReadFile(filepath.Join(admin, "gitdir"))
	if err != nil {
		return root
	}
	if !samePath(resolveFrom(admin, strings.TrimSpace(string(back))), filepath.Join(root, ".git")) {
		return root
	}
	if filepath.Base(commonDir) != ".git" {
		if _, err := os.Stat(filepath.Join(commonDir, ".git")); err == nil {
			return root
		}
		return commonDir // a bare repository
	}
	return filepath.Dir(commonDir)
}

func resolveFrom(base, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(base, p)
}

func samePath(a, b string) bool {
	if a == b {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

// markTrusted sets projects[project].hasTrustDialogAccepted in Claude Code's
// global config, which every running Claude process rewrites too. It takes the
// same "<file>.lock" directory lock the CLI takes for its own read-modify-write,
// re-reads the file under it, changes that one field, and replaces the file by
// rename, so other keys and other writers' edits made under the lock survive.
// A write that cannot be confirmed afterwards is retried. Returns true when
// trust was already recorded and nothing was written.
func markTrusted(path, project string) (bool, error) {
	var lastErr error
	for attempt := range 5 {
		already, err := markTrustedOnce(path, project)
		if err != nil {
			if errors.Is(err, errConfigMissing) || errors.Is(err, errConfigLocked) {
				return false, err
			}
			lastErr = err
			time.Sleep(time.Duration(50*(attempt+1)) * time.Millisecond)
			continue
		}
		if already {
			return true, nil
		}
		// Every writer that honours the lock re-reads under it and keeps
		// projects it did not change. Confirm nothing that skipped the lock
		// replaced the file from an older copy.
		if ok, err := isTrusted(path, project); err == nil && ok {
			return false, nil
		} else if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("trust for %s did not persist in %s", project, path)
		}
	}
	return false, fmt.Errorf("could not record trust for %s: %w", project, lastErr)
}

var (
	errConfigMissing = errors.New("claude code config not found")
	errConfigLocked  = errors.New("claude code config is locked")
)

func markTrustedOnce(path, project string) (bool, error) {
	release, err := acquireLockWait(path, configLockWait)
	if err != nil {
		return false, fmt.Errorf("%w: %v", errConfigLocked, err)
	}
	defer release()
	acquired := time.Now()

	target := path
	if real, err := filepath.EvalSymlinks(path); err == nil {
		target = real // Claude Code writes through a symlinked config, so do we
	}
	before, err := os.Stat(target)
	if err != nil {
		if os.IsNotExist(err) {
			return false, fmt.Errorf("%w at %s: run claude once first", errConfigMissing, path)
		}
		return false, err
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return false, err
	}
	out, changed, err := withTrust(data, project)
	if err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	if !changed {
		return true, nil
	}

	tmp, err := os.CreateTemp(filepath.Dir(target), filepath.Base(target)+".tmp.")
	if err != nil {
		return false, err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(out); err != nil {
		_ = tmp.Close()
		return false, err
	}
	if err := tmp.Chmod(before.Mode().Perm()); err != nil {
		_ = tmp.Close()
		return false, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if time.Since(acquired) > lockHoldLimit {
		return false, fmt.Errorf("held the lock on %s too long to write safely", path)
	}
	// A writer that skips the lock (the CLI does when its own lock attempt
	// fails) shows up as a changed file: start over from what it wrote.
	if now, err := os.Stat(target); err != nil || !now.ModTime().Equal(before.ModTime()) || now.Size() != before.Size() {
		return false, fmt.Errorf("%s changed while it was being updated", path)
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		return false, err
	}
	return false, nil
}

func isTrusted(path, project string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	var cfg struct {
		Projects map[string]struct {
			HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return false, err
	}
	return cfg.Projects[project].HasTrustDialogAccepted, nil
}

// withTrust returns data with projects[project].hasTrustDialogAccepted set to
// true, every other key and value carried over in its original order. changed
// is false when the field was already true.
func withTrust(data []byte, project string) (out []byte, changed bool, err error) {
	top, err := parseObject(data)
	if err != nil {
		return nil, false, err
	}
	projects := &object{}
	if raw, ok := top.get("projects"); ok && string(raw) != "null" {
		if projects, err = parseObject(raw); err != nil {
			return nil, false, fmt.Errorf("projects: %w", err)
		}
	}
	entry := &object{}
	if raw, ok := projects.get(project); ok && string(raw) != "null" {
		if entry, err = parseObject(raw); err != nil {
			return nil, false, fmt.Errorf("projects[%q]: %w", project, err)
		}
		if v, _ := entry.get("hasTrustDialogAccepted"); string(v) == "true" {
			return data, false, nil
		}
		entry.set("hasTrustDialogAccepted", json.RawMessage("true"))
	} else {
		for _, d := range trustProjectDefaults {
			entry.set(d.key, json.RawMessage(d.val))
		}
	}
	if err := projects.setObject(project, entry); err != nil {
		return nil, false, err
	}
	if err := top.setObject("projects", projects); err != nil {
		return nil, false, err
	}
	compact, err := top.marshal()
	if err != nil {
		return nil, false, err
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, compact, "", "  "); err != nil {
		return nil, false, err
	}
	return buf.Bytes(), true, nil
}

// object is a JSON object that keeps its keys in document order and its
// values verbatim, so a rewrite changes only what it sets.
type object struct {
	keys []string
	vals map[string]json.RawMessage
}

func parseObject(data []byte) (*object, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("not a JSON object")
	}
	o := &object{vals: map[string]json.RawMessage{}}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := tok.(string)
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return nil, err
		}
		o.set(key, val)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err == nil {
		return nil, fmt.Errorf("trailing data after the JSON object")
	}
	return o, nil
}

func (o *object) get(key string) (json.RawMessage, bool) {
	v, ok := o.vals[key]
	return v, ok
}

func (o *object) set(key string, val json.RawMessage) {
	if o.vals == nil {
		o.vals = map[string]json.RawMessage{}
	}
	if _, ok := o.vals[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = val
}

func (o *object) setObject(key string, val *object) error {
	b, err := val.marshal()
	if err != nil {
		return err
	}
	o.set(key, b)
	return nil
}

func (o *object) marshal() (json.RawMessage, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		var kb bytes.Buffer
		enc := json.NewEncoder(&kb)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(k); err != nil {
			return nil, err
		}
		buf.Write(bytes.TrimRight(kb.Bytes(), "\n"))
		buf.WriteByte(':')
		var vb bytes.Buffer
		if err := json.Compact(&vb, o.vals[k]); err != nil {
			return nil, err
		}
		buf.Write(vb.Bytes())
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}
