package agents

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestConfigDirPrefersEnvironment(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/opt/agent/.claude")
	got, err := configDir()
	if err != nil {
		t.Fatalf("configDir: %v", err)
	}
	if want := "/opt/agent/.claude"; got != want {
		t.Fatalf("configDir = %q, want %q", got, want)
	}
}

func TestConfigDirFallsBackToHome(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	got, err := configDir()
	if err != nil {
		t.Fatalf("configDir: %v", err)
	}
	if want := filepath.Join(home, ".claude"); got != want {
		t.Fatalf("configDir = %q, want %q", got, want)
	}
}

// A session whose daemon runs under a custom configuration directory keeps its
// jobs and transcripts there; resolving them under the home directory finds an
// unrelated daemon, or nothing at all.
func TestDerivedPathsFollowConfigDir(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/opt/agent/.claude")

	jobs, err := jobsDir()
	if err != nil {
		t.Fatalf("jobsDir: %v", err)
	}
	if want := "/opt/agent/.claude/jobs"; jobs != want {
		t.Fatalf("jobsDir = %q, want %q", jobs, want)
	}

	projects, err := projectsDir()
	if err != nil {
		t.Fatalf("projectsDir: %v", err)
	}
	if want := "/opt/agent/.claude/projects"; projects != want {
		t.Fatalf("projectsDir = %q, want %q", projects, want)
	}
}

// Two configuration directories mean two daemons, each with its own socket
// directory under /tmp. Picking the freshest socket reaches whichever daemon
// was restarted last, not the one this session belongs to.
func TestDaemonSocketDirFollowsConfigDir(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/Users/someone/.claude")
	got, err := daemonSocketDir()
	if err != nil {
		t.Fatalf("daemonSocketDir: %v", err)
	}
	sum := sha256.Sum256([]byte("/Users/someone/.claude"))
	want := filepath.Join(fmt.Sprintf("/tmp/cc-daemon-%d", os.Getuid()), hex.EncodeToString(sum[:])[:8])
	if got != want {
		t.Fatalf("daemonSocketDir = %q, want %q", got, want)
	}
}

func TestFindSocketPrefersOwnDaemon(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	own, err := daemonSocketDir()
	if err != nil {
		t.Fatalf("daemonSocketDir: %v", err)
	}
	// A second daemon, restarted later than ours: picking by freshness lands here.
	other := filepath.Join(filepath.Dir(own), "00000000")
	for _, dir := range []string{own, other} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Skipf("cannot create socket dir: %v", err)
		}
		t.Cleanup(func() { os.RemoveAll(dir) })
		if err := os.WriteFile(filepath.Join(dir, "control.sock"), nil, 0o600); err != nil {
			t.Fatalf("write socket placeholder: %v", err)
		}
	}
	stale := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(own, "control.sock"), stale, stale); err != nil {
		t.Fatalf("age our socket: %v", err)
	}

	got, err := FindSocket()
	if err != nil {
		t.Fatalf("FindSocket: %v", err)
	}
	if want := filepath.Join(own, "control.sock"); got != want {
		t.Fatalf("FindSocket = %q, want own daemon socket %q", got, want)
	}
}
