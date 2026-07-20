// Package mcpserver exposes the claude agents daemon as MCP tools.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kvaps/claude-agents-mcp/internal/agents"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// New builds the MCP server exposing claude agents control + attach actions.
func New(version string, a *agents.Client) *server.MCPServer {
	s := server.NewMCPServer("claude-agents-mcp", version)

	// ---- session management ----

	s.AddTool(mcp.NewTool("list_sessions",
		mcp.WithDescription("List sessions exactly as the agents view shows them — including not-running ones (`live:false`). Running sessions carry live state (state, tempo, detail, needs) and a short id; pass live_only=true to return only running sessions. For not-running sessions, `resumable:true` means the session is exited-but-resumable: it can be continued with its full history via resume_session or simply by submit_prompt/send_text (which auto-resume it in place) — prefer continuing such a session over forking or starting a fresh one. `resumable:false` on a not-running session means it is really dead (no job state, or its working directory is gone)."),
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
		mcp.WithDescription("Get one session's details by short id, session id (prefix) or name. For a not-running session (`live:false`), `resumable:true` means it can be continued in place with its full history (via resume_session, or automatically by submit_prompt/send_text); `resumable:false` means it is really dead."),
		mcp.WithString("session", mcp.Required(), mcp.Description("short id, session id, or name")),
	), func(_ context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		sess, err := a.Resolve(r.GetString("session", ""))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return jsonResult(sess)
	})

	s.AddTool(mcp.NewTool("create_session",
		mcp.WithDescription("Create a new background session in a directory (runs `claude --bg`). With model set, the session runs on that model (passed as `--model`; alias like sonnet/opus/haiku or a full model id — the claude CLI validates it). With prompt set, the task is delivered and reliably submitted so the agent starts immediately (no separate send_text+Enter, no getting stuck idle). goal=true sends it as /goal."),
		mcp.WithString("cwd", mcp.Required(), mcp.Description("working directory for the session")),
		mcp.WithString("name", mcp.Description("display name for the session")),
		mcp.WithString("model", mcp.Description("model for the session: an alias (sonnet, opus, haiku) or a full model id; omit for the default")),
		mcp.WithBoolean("dangerous", mcp.Description("pass --dangerously-skip-permissions")),
		mcp.WithString("prompt", mcp.Description("task to deliver and submit once the session is up (may be long/multi-line)")),
		mcp.WithBoolean("goal", mcp.Description("submit the prompt as a /goal command")),
	), func(_ context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		out, err := agents.Create(r.GetString("cwd", ""), r.GetString("name", ""), r.GetString("model", ""), r.GetBool("dangerous", false))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		prompt := r.GetString("prompt", "")
		if strings.TrimSpace(prompt) == "" {
			return mcp.NewToolResultText("created: " + out), nil
		}
		short := agents.ParseShortID(out)
		if short == "" {
			return mcp.NewToolResultError("created session but could not parse its id from output; prompt NOT delivered:\n" + out), nil
		}
		if err := a.WaitReady(short, 20*time.Second); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("created %s but it never became ready; prompt NOT delivered: %v", short, err)), nil
		}
		how, err := a.SubmitPrompt(short, prompt, r.GetBool("goal", false))
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("created %s but %v", short, err)), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("created %s and started the task — %s", short, how)), nil
	})

	s.AddTool(mcp.NewTool("resume_session",
		mcp.WithDescription("Bring a not-running session back to life and only return once the worker is verified live — never a job that 'already exited'. It resumes the session IN PLACE via the daemon, the same way the agents view does: under the session's own short, same session id, with no fork and no duplicate entry in the list (unlike a raw `claude --bg --resume`, which spawns a worker under a fresh short and leaves the original behind). It validates the session's saved working directory first — a deleted worktree is the most common resume crash — and returns a clear error instead of spawning a doomed worker, and on any failure it cleans up the worker it started so no crashed/idle session is left as garbage. It refuses to resume a session that is already live. Accepts a name, short id, or full session id (a full id still works for sessions no longer in the agents list, via a CLI fallback that forks to a fresh short). With model set, the session is resumed on that model (passed as `--model`, replacing any model it was originally launched with; alias or full model id — the claude CLI validates it). With prompt set, the task is delivered and submitted once the resumed session settles at its prompt (best-effort; goal=true sends it as /goal)."),
		mcp.WithString("session", mcp.Required(), mcp.Description("session id, short id, or name to resume")),
		mcp.WithString("prompt", mcp.Description("task to deliver and submit once the session is ready (best-effort; optional)")),
		mcp.WithBoolean("goal", mcp.Description("submit the prompt as a /goal command")),
		mcp.WithString("model", mcp.Description("model to resume on: an alias (sonnet, opus, haiku) or a full model id; overrides the session's original model, omit to keep it")),
		mcp.WithBoolean("dangerous", mcp.Description("pass --dangerously-skip-permissions")),
		mcp.WithString("on_resume_dialog", mcp.Description(onResumeDialogDesc)),
	), func(_ context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		dialog, derr := resumeDialogChoice(r)
		if derr != nil {
			return mcp.NewToolResultError(derr.Error()), nil
		}
		ref := r.GetString("session", "")
		model := r.GetString("model", "")
		dangerous := r.GetBool("dangerous", false)
		sess, rerr := a.Resolve(ref)
		if rerr == nil && sess.Live {
			return mcp.NewToolResultText(fmt.Sprintf("session %s is already live (state=%s); not resuming to avoid a duplicate worker — use submit_prompt to (re)seed it", sess.Short, sess.State)), nil
		}

		// Preferred path: resume in place by the session's short (daemon dispatch,
		// no fork/duplicate). Fall back to the CLI resume only when the session has
		// no on-disk job state (e.g. it is no longer in the agents list) or when
		// only a full session id was given. Both paths verify liveness and clean up
		// after themselves on failure, so no orphan is left behind.
		var out agents.ResumeOutcome
		var err error
		switch {
		case rerr == nil && sess.Short != "":
			out, err = a.ResumeInPlace(sess.Short, model, dangerous)
			if errors.Is(err, agents.ErrNoJobState) {
				out, err = a.ResumeByCLI(sess.SessionID, model, dangerous)
			}
		case agents.IsFullSessionID(ref):
			out, err = a.ResumeByCLI(ref, model, dangerous)
		default:
			return mcp.NewToolResultError(rerr.Error()), nil
		}
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("resume failed: %v. The session is intact on disk — drive it from the agents view or start a fresh session", err)), nil
		}

		short := out.Short
		if err := a.WaitInteractive(short, 20*time.Second); err != nil {
			return mcp.NewToolResultText(fmt.Sprintf("resumed %s (live, state=%s) but it never settled at a prompt — read_screen and drive it manually", short, out.State)), nil
		}
		// Settle the CLI's resume dialog before anything else: while it is up it
		// owns the keyboard, so a prompt typed underneath it goes nowhere and a
		// stray Enter answers it with the preselected (compacting) option.
		dnote, err := a.SettleResumeDialog(short, dialog)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("resumed %s (live, state=%s) but %v", short, out.State, err)), nil
		}
		prompt := r.GetString("prompt", "")
		if strings.TrimSpace(prompt) == "" {
			return mcp.NewToolResultText(fmt.Sprintf("resumed %s (live, state=%s); %s", short, out.State, dnote)), nil
		}
		how, err := a.SubmitPrompt(short, prompt, r.GetBool("goal", false))
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("resumed %s; %sbut %v", short, dnote, err)), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("resumed %s; %sstarted the task — %s", short, dnote, how)), nil
	})

	s.AddTool(mcp.NewTool("fork_session",
		mcp.WithDescription("Fork a session into a new, independent background session that carries ALL of the source's history up to the moment of the fork. The fork shows up in `claude agents` as its own entry (new short id, new session id) and can be driven immediately; the source session is never touched. Uses Claude Code's native --fork-session, so the full transcript is forked correctly (not a shallow copy). The fork inherits the source's working directory. Accepts a name, short id, or full session id for the source. With model set, the fork runs on that model (passed as `--model`; alias or full model id — the claude CLI validates it), independent of the source's model. With prompt set, the task is delivered and submitted once the fork settles at its prompt (best-effort; goal=true sends it as /goal)."),
		mcp.WithString("session", mcp.Required(), mcp.Description("source session to fork: short id, session id, or name")),
		mcp.WithString("name", mcp.Description("display name for the new forked session")),
		mcp.WithString("model", mcp.Description("model for the fork: an alias (sonnet, opus, haiku) or a full model id; omit to keep the default")),
		mcp.WithString("prompt", mcp.Description("task to deliver and submit once the fork is ready (best-effort; optional)")),
		mcp.WithBoolean("goal", mcp.Description("submit the prompt as a /goal command")),
		mcp.WithBoolean("dangerous", mcp.Description("pass --dangerously-skip-permissions")),
		mcp.WithString("on_resume_dialog", mcp.Description(onResumeDialogDesc)),
	), func(_ context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		dialog, derr := resumeDialogChoice(r)
		if derr != nil {
			return mcp.NewToolResultError(derr.Error()), nil
		}
		sess, err := a.Resolve(r.GetString("session", ""))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if sess.SessionID == "" {
			return mcp.NewToolResultError("source session has no session id on disk; cannot fork"), nil
		}
		out, err := a.ForkSession(sess.SessionID, sess.Cwd, r.GetString("name", ""), r.GetString("model", ""), r.GetBool("dangerous", false))
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("fork failed: %v. The source session %s is intact", err, sess.Short)), nil
		}
		short := out.Short
		if err := a.WaitInteractive(short, 20*time.Second); err != nil {
			return mcp.NewToolResultText(fmt.Sprintf("forked %s -> %s (live, state=%s) but it never settled at a prompt — read_screen and drive it manually", sess.Short, short, out.State)), nil
		}
		// A fork replays the source's history, so it can come up on the same
		// resume dialog a plain resume does.
		dnote, err := a.SettleResumeDialog(short, dialog)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("forked %s -> %s but %v", sess.Short, short, err)), nil
		}
		prompt := r.GetString("prompt", "")
		if strings.TrimSpace(prompt) == "" {
			return mcp.NewToolResultText(fmt.Sprintf("forked %s -> %s (live, state=%s, session id %s); carries the source's full history; %s", sess.Short, short, out.State, out.SessionID, dnote)), nil
		}
		how, err := a.SubmitPrompt(short, prompt, r.GetBool("goal", false))
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("forked %s -> %s; %sbut %v", sess.Short, short, dnote, err)), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("forked %s -> %s; %sstarted the task — %s", sess.Short, short, dnote, how)), nil
	})

	s.AddTool(mcp.NewTool("submit_prompt",
		mcp.WithDescription("Deliver a prompt to a session and reliably submit it in one call (handles bracketed-paste for long/multi-line text, then verifies the turn actually started, retrying Enter once). Use this to (re)seed a session's task instead of send_text+send_keys. A session that is not running but resumable is transparently resumed in place first, keeping its full history — like typing into an exited session in the app — so there is no need to check liveness or call resume_session before continuing a conversation. goal=true sends it as /goal."),
		mcp.WithString("session", mcp.Required(), mcp.Description("short id, session id, or name")),
		mcp.WithString("text", mcp.Required(), mcp.Description("prompt text to deliver and submit (may be long/multi-line)")),
		mcp.WithBoolean("goal", mcp.Description("submit the prompt as a /goal command")),
		mcp.WithString("on_resume_dialog", mcp.Description(onResumeDialogDesc)),
	), func(_ context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		dialog, err := resumeDialogChoice(r)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		sess, err := a.Resolve(r.GetString("session", ""))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		sess, note, err := ensureLive(a, sess, dialog)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		how, err := a.SubmitPrompt(sess.Short, r.GetString("text", ""), r.GetBool("goal", false))
		if err != nil {
			return mcp.NewToolResultError(note + err.Error()), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("%ssubmitted to %s; %s", note, sess.Short, how)), nil
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
		if err := a.Rename(sess.Short, sess.SessionID, sess.Cwd, title); err != nil {
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
		mcp.WithDescription("Type text into a session (e.g. a prompt). submit=true presses Enter. A session that is not running but resumable is transparently resumed in place first (full history kept), like typing into an exited session in the app. Fire-and-forget by default (returns immediately); pass wait=true to block until the screen settles and return it — otherwise use read_screen to see output."),
		mcp.WithString("session", mcp.Required(), mcp.Description("short id, session id, or name")),
		mcp.WithString("text", mcp.Required(), mcp.Description("text to type")),
		mcp.WithBoolean("submit", mcp.Description("press Enter after typing (default true)")),
		mcp.WithBoolean("wait", mcp.Description("block and return the resulting screen (default false: return immediately)")),
		mcp.WithString("on_resume_dialog", mcp.Description(onResumeDialogDesc)),
	), func(_ context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		dialog, err := resumeDialogChoice(r)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		sess, err := a.Resolve(r.GetString("session", ""))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		sess, note, err := ensureLive(a, sess, dialog)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		wait := r.GetBool("wait", false)
		screen, err := a.SendText(sess.Short, r.GetString("text", ""), r.GetBool("submit", true), wait)
		if err != nil {
			return mcp.NewToolResultError(note + err.Error()), nil
		}
		return mcp.NewToolResultText(note + sentResult(screen, wait)), nil
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
		mcp.WithDescription("Run a slash command in a session reliably: clears modals, waits for idle, types and submits. e.g. /remote-control, /goal, /compact, /clear. A session that is not running but resumable is transparently resumed in place first (full history kept)."),
		mcp.WithString("session", mcp.Required(), mcp.Description("short id, session id, or name")),
		mcp.WithString("command", mcp.Required(), mcp.Description("slash command, with or without the leading /")),
		mcp.WithString("on_resume_dialog", mcp.Description(onResumeDialogDesc)),
	), func(_ context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		dialog, err := resumeDialogChoice(r)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		sess, err := a.Resolve(r.GetString("session", ""))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		sess, note, err := ensureLive(a, sess, dialog)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		screen, err := a.SendCommand(sess.Short, r.GetString("command", ""))
		if err != nil {
			return mcp.NewToolResultError(note + err.Error()), nil
		}
		return mcp.NewToolResultText(note + screen), nil
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

// onResumeDialogDesc documents the on_resume_dialog parameter, shared by every
// tool that can trigger a resume.
const onResumeDialogDesc = "what to do if the resumed session comes up on the CLI's resume dialog (\"Resume from summary\" / \"Resume full session as-is\"), which the CLI shows for a session that is old and large enough and which preselects the summary option: " +
	"\"keep\" (default) answers \"Resume full session as-is\", keeping the full conversation; " +
	"\"compact\" accepts the summary, which discards the conversation's detail; " +
	"\"ask\" leaves the dialog up and reports its options so you can decide. " +
	"Never answer this dialog by sending a bare Enter — Enter takes the preselected option, which compacts the session and discards whatever prompt was pending"

// resumeDialogChoice reads the on_resume_dialog parameter. Absent or empty means
// keep: delivering a prompt must never spend a session's context as a side
// effect, so compaction is opt-in.
func resumeDialogChoice(r mcp.CallToolRequest) (agents.ResumeDialogChoice, error) {
	return agents.ParseResumeDialogChoice(r.GetString("on_resume_dialog", ""))
}

// ensureLive brings a delivery target live before input is sent to it: a live
// session is returned as-is; a not-running-but-resumable one is transparently
// resumed in place (mirroring the app, where typing into an exited session
// brings it back with its history), and any resume dialog it comes up on is
// settled per the caller's choice before anything is typed. The returned note,
// prepended to the tool result, tells the caller a resume happened and what was
// done about the dialog.
func ensureLive(a *agents.Client, sess agents.Session, dialog agents.ResumeDialogChoice) (agents.Session, string, error) {
	if sess.Live {
		return sess, "", nil
	}
	live, dnote, err := a.EnsureLive(sess, dialog)
	if err != nil {
		return agents.Session{}, "", err
	}
	return live, fmt.Sprintf("session %s was not running — auto-resumed in place; %s", live.Short, dnote), nil
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
