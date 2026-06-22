// Command claude-agents-mcp is an MCP server that controls local `claude agents`
// background sessions: it exposes the full set of actions a human can perform
// via attach (read screen, type, send keys, run slash commands, cancel) plus
// session management (list, create, rename, close).
package main

import (
	"fmt"
	"os"

	"github.com/kvaps/claude-agents-mcp/internal/agents"
	"github.com/kvaps/claude-agents-mcp/internal/mcpserver"
	"github.com/mark3labs/mcp-go/server"
)

// version is the server version reported to MCP clients.
var version = "0.1.0"

func main() {
	s := mcpserver.New(version, agents.NewClient())
	if err := server.ServeStdio(s); err != nil {
		fmt.Fprintln(os.Stderr, "claude-agents-mcp:", err)
		os.Exit(1)
	}
}
