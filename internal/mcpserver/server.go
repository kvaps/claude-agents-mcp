// Package mcpserver exposes the claude agents daemon as MCP tools.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kvaps/claude-agents-mcp/internal/agents"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// New builds the MCP server exposing claude agents control + attach actions.
func New(version string, a *agents.Client) *server.MCPServer {
	s := server.NewMCPServer("claude-agents-mcp", version)

	// ---- session management ----

	s.AddTool(mcp.NewTool("list_sessions",
		mcp.WithDescription("List sessions exactly as the agents view shows them — including not-running ones (`live:false`). Running sessions carry live state (state, tempo, detail, needs) and a short id; pass live_only=true to return only running sessions."),
		mcp.WithBoolean("live_only", mcp.Description("return only running (attachable) sessions")),
	), func(_ context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		jobs, err := a.List()
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if r.GetBool("live_only", false) {
			filtered := make([]agents.Session, 0, len(jobs))
			for _, j := range jobs {
				if j.Live {
					filtered = append(filtered, j)
				}
			}
			jobs = filtered
		}
		return jsonResult(jobs)
	})

	s.AddTool(mcp.NewTool("get_session",
		mcp.WithDescription("Get one session's details by short id, session id (prefix) or name."),
		mcp.WithString("session", mcp.Required(), mcp.Description("short id, session id, or name")),
	), func(_ context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sess, err := a.Resolve(r.GetString("session", ""))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return jsonResult(sess)
	})

	s.AddTool(mcp.NewTool("create_session",
		mcp.WithDescription("Create a new background session in a directory (runs `claude --bg`). Returns the new session id."),
		mcp.WithString("cwd", mcp.Required(), mcp.Description("working directory for the session")),
		mcp.WithString("name", mcp.Description("display name for the session")),
		mcp.WithBoolean("dangerous", mcp.Description("pass --dangerously-skip-permissions")),
	), func(_ context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		out, err := agents.Create(r.GetString("cwd", ""), r.GetString("name", ""), r.GetBool("dangerous", false))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText("created: " + out), nil
	})

	s.AddTool(mcp.NewTool("rename_session",
		mcp.WithDescription("Rename a session (sets its custom title, same effect as renaming in the agents view)."),
		mcp.WithString("session", mcp.Required(), mcp.Description("short id, session id, or name")),
		mcp.WithString("title", mcp.Required(), mcp.Description("new title")),
	), func(_ context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sess, err := a.Resolve(r.GetString("session", ""))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		title := r.GetString("title", "")
		if err := agents.Rename(sess.SessionID, sess.Cwd, title); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("renamed %s -> %q", sess.Short, title)), nil
	})

	s.AddTool(mcp.NewTool("delete_session",
		mcp.WithDescription("Delete a session. permanent=true removes it (claude rm, like ctrl+x in the agents view); permanent=false stops it gracefully (claude stop)."),
		mcp.WithString("session", mcp.Required(), mcp.Description("short id, session id, or name")),
		mcp.WithBoolean("permanent", mcp.Description("remove permanently (default true)")),
	), func(_ context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sess, err := a.Resolve(r.GetString("session", ""))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if r.GetBool("permanent", true) {
			err = agents.Remove(sess.Short)
		} else {
			err = agents.Stop(sess.Short)
		}
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText("closed " + sess.Short), nil
	})

	s.AddTool(mcp.NewTool("pin_session",
		mcp.WithDescription("Pin or unpin a session in the agents view (ctrl+t). Pinned sessions sort to the top of the list. pinned=true pins, pinned=false unpins."),
		mcp.WithString("session", mcp.Required(), mcp.Description("short id, session id, or name")),
		mcp.WithBoolean("pinned", mcp.Description("true to pin (default), false to unpin")),
	), func(_ context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sess, err := a.Resolve(r.GetString("session", ""))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		pinned := r.GetBool("pinned", true)
		if err := a.Pin(sess.Short, pinned); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		verb := "pinned"
		if !pinned {
			verb = "unpinned"
		}
		return mcp.NewToolResultText(verb + " " + sess.Short), nil
	})

	s.AddTool(mcp.NewTool("reorder_session",
		mcp.WithDescription("Reorder a running session in the agents view (shift+up/down). Use direction=up/down to move one slot, or position for a 0-based absolute slot. Only running daemon sessions can be reordered; pinning takes precedence over ordering."),
		mcp.WithString("session", mcp.Required(), mcp.Description("short id, session id, or name")),
		mcp.WithString("direction", mcp.Description("\"up\" or \"down\" (move one slot)")),
		mcp.WithNumber("position", mcp.Description("0-based target slot (overrides direction)")),
	), func(_ context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sess, err := a.Resolve(r.GetString("session", ""))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		direction := strings.ToLower(strings.TrimSpace(r.GetString("direction", "")))
		_, hasPosition := r.GetArguments()["position"]
		position := r.GetInt("position", 0)
		if !hasPosition && direction == "" {
			return mcp.NewToolResultError("provide direction (\"up\"/\"down\") or position"), nil
		}
		if err := a.Reorder(sess.Short, direction, position, hasPosition); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		where := direction
		if hasPosition {
			where = fmt.Sprintf("position %d", position)
		}
		return mcp.NewToolResultText(fmt.Sprintf("reordered %s (%s)", sess.Short, where)), nil
	})

	// ---- attach: everything a human can do inside a session ----

	s.AddTool(mcp.NewTool("read_screen",
		mcp.WithDescription("Read the current screen of a session as plain text (what a human would see)."),
		mcp.WithString("session", mcp.Required(), mcp.Description("short id, session id, or name")),
		mcp.WithNumber("tail", mcp.Description("lines of scrollback to include (default 200)")),
	), func(_ context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sess, err := a.Resolve(r.GetString("session", ""))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		screen, err := a.ReadScreen(sess.Short, r.GetInt("tail", 200))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(screen), nil
	})

	s.AddTool(mcp.NewTool("send_text",
		mcp.WithDescription("Type text into a session (e.g. a prompt). submit=true presses Enter. Fire-and-forget by default (returns immediately); pass wait=true to block until the screen settles and return it — otherwise use read_screen to see output."),
		mcp.WithString("session", mcp.Required(), mcp.Description("short id, session id, or name")),
		mcp.WithString("text", mcp.Required(), mcp.Description("text to type")),
		mcp.WithBoolean("submit", mcp.Description("press Enter after typing (default true)")),
		mcp.WithBoolean("wait", mcp.Description("block and return the resulting screen (default false: return immediately)")),
	), func(_ context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sess, err := a.Resolve(r.GetString("session", ""))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		wait := r.GetBool("wait", false)
		screen, err := a.SendText(sess.Short, r.GetString("text", ""), r.GetBool("submit", true), wait)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(sentResult(screen, wait)), nil
	})

	s.AddTool(mcp.NewTool("send_keys",
		mcp.WithDescription("Send a sequence of named keys, e.g. \"esc down enter\" or \"ctrl-c\". Supported: enter, esc, tab, space, backspace, delete, up, down, left, right, home, end, pageup, pagedown, ctrl-c/d/u/l/z/r. Fire-and-forget by default; pass wait=true to block and return the screen."),
		mcp.WithString("session", mcp.Required(), mcp.Description("short id, session id, or name")),
		mcp.WithString("keys", mcp.Required(), mcp.Description("comma- or space-separated key names")),
		mcp.WithBoolean("wait", mcp.Description("block and return the resulting screen (default false: return immediately)")),
	), func(_ context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sess, err := a.Resolve(r.GetString("session", ""))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		wait := r.GetBool("wait", false)
		screen, err := a.SendKeys(sess.Short, splitKeys(r.GetString("keys", "")), wait)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(sentResult(screen, wait)), nil
	})

	s.AddTool(mcp.NewTool("send_command",
		mcp.WithDescription("Run a slash command in a session reliably: clears modals, waits for idle, types and submits. e.g. /remote-control, /goal, /compact, /clear."),
		mcp.WithString("session", mcp.Required(), mcp.Description("short id, session id, or name")),
		mcp.WithString("command", mcp.Required(), mcp.Description("slash command, with or without the leading /")),
	), func(_ context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sess, err := a.Resolve(r.GetString("session", ""))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		screen, err := a.SendCommand(sess.Short, r.GetString("command", ""))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(screen), nil
	})

	s.AddTool(mcp.NewTool("cancel",
		mcp.WithDescription("Cancel the current task in a session. hard=false sends Esc; hard=true sends Ctrl-C."),
		mcp.WithString("session", mcp.Required(), mcp.Description("short id, session id, or name")),
		mcp.WithBoolean("hard", mcp.Description("use Ctrl-C instead of Esc")),
	), func(_ context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sess, err := a.Resolve(r.GetString("session", ""))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		screen, err := a.Cancel(sess.Short, r.GetBool("hard", false))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return mcp.NewToolResultText(screen), nil
	})

	return s
}

func jsonResult(v any) (*mcp.CallToolResult, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(string(b)), nil
}

// sentResult formats a send response: the screen when waiting, otherwise the
// little that the grace read captured — or a hint to read_screen when empty.
func sentResult(screen string, wait bool) string {
	if wait || strings.TrimSpace(screen) != "" {
		return screen
	}
	return "sent (fire-and-forget; call read_screen to see output, or pass wait=true)"
}

func splitKeys(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t'
	})
}
