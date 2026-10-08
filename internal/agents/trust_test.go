package agents

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// sandbox points every path PreTrust could touch at a temporary directory and
// returns it resolved, with a minimal Claude Code config in place.
func sandbox(t *testing.T) (dir, config string) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", filepath.Join(dir, "home"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, "config"))
	t.Setenv(envTrustRoots, "")
	for _, d := range []string{"home", "config"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	config = filepath.Join(dir, "config", ".claude.json")
	writeFile(t, config, `{"userID": "u1", "projects": {}}`)
	return dir, config
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func mustTrusted(t *testing.T, config, project string, want bool) {
	t.Helper()
	got, err := isTrusted(config, project)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("trusted(%s) = %v, want %v", project, got, want)
	}
}

func TestTrustProjectFollowsTheRepositoryRoot(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir, _ := sandbox(t)

	plain := filepath.Join(dir, "plain", "nested")
	repo := filepath.Join(dir, "repo")
	bare := filepath.Join(dir, "bare")
	for _, d := range []string{plain, filepath.Join(repo, "sub", "deeper"), bare} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	git(t, repo, "init", "-q")
	git(t, repo, "commit", "-q", "--allow-empty", "-m", "init")
	git(t, repo, "worktree", "add", "-q", filepath.Join(dir, "linked"))
	git(t, bare, "init", "-q", "--bare")
	git(t, repo, "push", "-q", bare, "HEAD:refs/heads/main")
	git(t, bare, "worktree", "add", "-q", filepath.Join(bare, "trees", "main"), "main")
	if err := os.Symlink(filepath.Join(repo, "sub"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	// A .git file that is not a linked worktree (a submodule looks like this)
	// is its own root.
	other := filepath.Join(dir, "module")
	writeFile(t, filepath.Join(other, ".git"), "gitdir: "+filepath.Join(repo, ".git", "modules", "x")+"\n")

	for _, tc := range []struct{ cwd, want string }{
		{plain, plain},
		{filepath.Join(repo, "sub", "deeper"), repo},
		{filepath.Join(dir, "linked"), repo},
		{filepath.Join(bare, "trees", "main"), bare},
		{filepath.Join(dir, "link"), repo},
		{other, other},
	} {
		got, err := trustProject(tc.cwd)
		if err != nil {
			t.Fatalf("%s: %v", tc.cwd, err)
		}
		if got != tc.want {
			t.Errorf("trustProject(%s) = %s, want %s", tc.cwd, got, tc.want)
		}
	}
	if _, err := trustProject("relative/path"); err == nil {
		t.Error("a relative cwd was accepted")
	}
}

func TestPreTrustStaysInsideTheTrustRoots(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir, config := sandbox(t)
	root := filepath.Join(dir, "dev")
	inside := filepath.Join(root, "proj")
	outside := filepath.Join(dir, "elsewhere")
	for _, d := range []string{inside, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A repository whose top is above the root: a cwd inside the root still
	// resolves to the repository, which the root does not cover.
	wide := filepath.Join(dir, "wide")
	if err := os.MkdirAll(filepath.Join(wide, "narrow"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, wide, "init", "-q")
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	orig, _ := os.ReadFile(config)

	if _, err := PreTrust(inside); err == nil || !strings.Contains(err.Error(), envTrustRoots) {
		t.Fatalf("without trust roots: err = %v, want it disabled", err)
	}

	t.Setenv(envTrustRoots, root+string(filepath.ListSeparator)+filepath.Join(wide, "narrow"))
	for _, cwd := range []string{
		outside,
		root,                              // the root itself
		filepath.Join(root, "escape"),     // a symlink out of the root
		filepath.Join(wide, "narrow"),     // inside a root, but the repository is not
		filepath.Join(root, "proj/../.."), // dot-dot out of the root
	} {
		if res, err := PreTrust(cwd); err == nil {
			t.Errorf("PreTrust(%s) trusted %s", cwd, res.Project)
		}
	}
	if b, _ := os.ReadFile(config); string(b) != string(orig) {
		t.Fatalf("a refused PreTrust wrote the config:\n%s", b)
	}

	res, err := PreTrust(inside)
	if err != nil {
		t.Fatal(err)
	}
	if res.Project != inside || res.Already {
		t.Fatalf("PreTrust = %+v", res)
	}
	mustTrusted(t, config, inside, true)
	if res, err = PreTrust(inside); err != nil || !res.Already {
		t.Fatalf("second PreTrust = %+v, %v; want already trusted", res, err)
	}
}

func TestPreTrustRefusesTheHomeDirectory(t *testing.T) {
	dir, _ := sandbox(t)
	home := filepath.Join(dir, "home")
	t.Setenv(envTrustRoots, dir)
	if _, err := PreTrust(home); err == nil || !strings.Contains(err.Error(), "home directory") {
		t.Fatalf("err = %v, want the home directory refused", err)
	}
}

func TestWithTrustChangesOnlyTheTrustField(t *testing.T) {
	in := `{
  "zeta": 12345678901234567890,
  "projects": {
    "/a": {"allowedTools": ["Bash(<x> && y)"], "hasTrustDialogAccepted": false, "custom": {"n": 1.50}},
    "/b": {"hasTrustDialogAccepted": true}
  },
  "alpha": "<&>",
  "oauthAccount": {"emailAddress": "x@y"}
}`
	out, changed, err := withTrust([]byte(in), "/a")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	want := `{
  "zeta": 12345678901234567890,
  "projects": {
    "/a": {
      "allowedTools": [
        "Bash(<x> && y)"
      ],
      "hasTrustDialogAccepted": true,
      "custom": {
        "n": 1.50
      }
    },
    "/b": {
      "hasTrustDialogAccepted": true
    }
  },
  "alpha": "<&>",
  "oauthAccount": {
    "emailAddress": "x@y"
  }
}`
	if string(out) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}

	out, changed, err = withTrust([]byte(`{"userID":"u"}`), "/new")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	var cfg struct {
		UserID   string                    `json:"userID"`
		Projects map[string]map[string]any `json:"projects"`
	}
	if err := json.Unmarshal(out, &cfg); err != nil {
		t.Fatal(err)
	}
	entry := cfg.Projects["/new"]
	if cfg.UserID != "u" || entry["hasTrustDialogAccepted"] != true || entry["hasClaudeMdExternalIncludesApproved"] != false || entry["mcpServers"] == nil {
		t.Fatalf("new project entry = %v, want the CLI's defaults with trust set", entry)
	}

	if _, changed, _ := withTrust([]byte(`{"projects":{"/b":{"hasTrustDialogAccepted":true}}}`), "/b"); changed {
		t.Fatal("already-trusted project reported as changed")
	}
	for _, bad := range []string{``, `[]`, `{"projects": []}`, `{"a":1} {"b":2}`} {
		if _, _, err := withTrust([]byte(bad), "/a"); err == nil {
			t.Errorf("withTrust(%q) accepted malformed input", bad)
		}
	}
}

func TestMarkTrustedLeavesAnAlreadyTrustedFileAlone(t *testing.T) {
	_, config := sandbox(t)
	writeFile(t, config, `{"projects":{"/p":{"hasTrustDialogAccepted":true}}}`)
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(config, old, old); err != nil {
		t.Fatal(err)
	}
	already, err := markTrusted(config, "/p")
	if err != nil || !already {
		t.Fatalf("already=%v err=%v", already, err)
	}
	if fi, _ := os.Stat(config); !fi.ModTime().Equal(old) {
		t.Fatal("an already-trusted config was rewritten")
	}
}

func TestMarkTrustedNeedsAnExistingConfig(t *testing.T) {
	dir, config := sandbox(t)
	if err := os.Remove(config); err != nil {
		t.Fatal(err)
	}
	if _, err := markTrusted(config, filepath.Join(dir, "p")); err == nil {
		t.Fatal("created a config from nothing")
	}
	if _, err := os.Stat(config); !os.IsNotExist(err) {
		t.Fatal("a config file appeared")
	}
}

func TestMarkTrustedWritesThroughASymlink(t *testing.T) {
	dir, config := sandbox(t)
	real := filepath.Join(dir, "dotfiles", "claude.json")
	writeFile(t, real, `{"projects":{}}`)
	if err := os.Remove(config); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, config); err != nil {
		t.Fatal(err)
	}
	if _, err := markTrusted(config, "/p"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(config); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlink was replaced by a file")
	}
	mustTrusted(t, real, "/p", true)
	if fi, _ := os.Stat(real); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want the original 0600", fi.Mode().Perm())
	}
}

func TestMarkTrustedWaitsForTheConfigLock(t *testing.T) {
	_, config := sandbox(t)
	lock := config + ".lock"
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := markTrusted(config, "/p"); done <- err }()

	time.Sleep(300 * time.Millisecond)
	mustTrusted(t, config, "/p", false) // nothing written while another process holds the lock
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("never acquired the released lock")
	}
	mustTrusted(t, config, "/p", true)
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatal("the lock was not released")
	}

	// A lock left by a crashed process goes stale and is taken over.
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := markTrusted(config, "/q"); err != nil {
		t.Fatal(err)
	}
	mustTrusted(t, config, "/q", true)
}

func TestMarkTrustedGivesUpOnAHeldLock(t *testing.T) {
	_, config := sandbox(t)
	old := configLockWait
	configLockWait = 200 * time.Millisecond
	t.Cleanup(func() { configLockWait = old })
	if err := os.Mkdir(config+".lock", 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := markTrusted(config, "/p"); err == nil {
		t.Fatal("wrote without the lock")
	}
	mustTrusted(t, config, "/p", false)
}

// claudeStyleWrite is what the CLI does to save its config: take the lock,
// re-read, apply its change, and replace the file — here, adding a project
// and bumping a top-level counter.
func claudeStyleWrite(path, project string) error {
	release, err := acquireLockWait(path, 30*time.Second)
	if err != nil {
		return err
	}
	defer release()
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		return err
	}
	projects, _ := cfg["projects"].(map[string]any)
	if projects == nil {
		projects = map[string]any{}
	}
	projects[project] = map[string]any{"lastCost": 1}
	cfg["projects"] = projects
	n, _ := cfg["numStartups"].(float64)
	cfg["numStartups"] = n + 1
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp.claude"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func TestMarkTrustedLosesNoConcurrentEdits(t *testing.T) {
	_, config := sandbox(t)
	writeFile(t, config, `{"oauthAccount":{"emailAddress":"x@y"},"numStartups":0,"projects":{}}`)
	const n = 12
	var wg sync.WaitGroup
	errs := make(chan error, 2*n)
	for i := range n {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := markTrusted(config, fmt.Sprintf("/trusted/%d", i)); err != nil {
				errs <- err
			}
		}()
		go func() {
			defer wg.Done()
			if err := claudeStyleWrite(config, fmt.Sprintf("/claude/%d", i)); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	b, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		OAuth       map[string]any            `json:"oauthAccount"`
		NumStartups float64                   `json:"numStartups"`
		Projects    map[string]map[string]any `json:"projects"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.OAuth["emailAddress"] != "x@y" || cfg.NumStartups != n {
		t.Fatalf("other keys lost: oauth=%v numStartups=%v", cfg.OAuth, cfg.NumStartups)
	}
	for i := range n {
		if cfg.Projects[fmt.Sprintf("/trusted/%d", i)]["hasTrustDialogAccepted"] != true {
			t.Errorf("trust for /trusted/%d lost", i)
		}
		if cfg.Projects[fmt.Sprintf("/claude/%d", i)]["lastCost"] == nil {
			t.Errorf("the CLI's edit to /claude/%d lost", i)
		}
	}
	if _, err := os.Stat(config + ".lock"); !os.IsNotExist(err) {
		t.Fatal("the lock was left behind")
	}
}
