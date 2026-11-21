package agents

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// configDir returns the Claude Code configuration directory: CLAUDE_CONFIG_DIR
// when the CLI was pointed elsewhere, ~/.claude otherwise.
//
// Claude Code exports the variable into every session it starts, and a daemon
// launched that way keeps its roster, job state and transcripts under it. A
// machine can therefore run two daemons at once — one per configuration
// directory — with different control keys, so resolving these paths from the
// home directory reaches the wrong daemon rather than none.
func configDir() (string, error) {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude"), nil
}

// projectsDir returns the directory holding session transcripts, one
// subdirectory per project path.
func projectsDir() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "projects"), nil
}

// daemonSocketDir returns the directory the daemon of this configuration
// directory keeps its sockets in. The name is the first eight hex digits of the
// SHA-256 of the configuration directory path, which is how the CLI derives it —
// so two configuration directories never share a socket directory.
func daemonSocketDir() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(dir))
	name := hex.EncodeToString(sum[:])[:8]
	return filepath.Join(fmt.Sprintf("/tmp/cc-daemon-%d", os.Getuid()), name), nil
}
