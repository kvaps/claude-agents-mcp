# claude-agents-mcp

An MCP server that lets an agent (or you) drive local **`claude agents`** background sessions programmatically — everything a human can do via `attach`, plus session management.

It talks to the native Claude Code daemon over its control socket (`/tmp/cc-daemon-<uid>/*/control.sock`) and the public `claude` CLI. There is no separate daemon — it's the same one `claude agents` and `claude --bg` already use.

## Why

When you run many `claude agents` sessions, a human can attach to any of them and read the screen, type, run slash commands (`/remote-control`, `/goal`, …), cancel a task, and manage the fleet. This server exposes those same actions as MCP tools so an orchestrating agent can do them too.

## Build

```sh
make all        # tidy + lint + build  (golangci-lint is a mandatory step)
# or
go build -o claude-agents-mcp ./cmd/claude-agents-mcp
```

Requirements: Go 1.24+, `golangci-lint`, and the `claude` CLI in `PATH`.

## Use with Claude Code

```sh
claude mcp add claude-agents -- /path/to/claude-agents-mcp
```

The server speaks MCP over stdio.

## Tools

Session management:

- `list_sessions` — every session the agents view shows, **including not-running ones** (`live:false`); running ones carry live state (`state`, `tempo`, `detail`, `needs`), short id, `cwd`, `name`, and a `pinned` flag. `live_only=true` filters to running sessions
- `get_session` — one session by short id / session id / name
- `create_session` — `claude --bg` in a directory (optional name, dangerous mode); with `prompt` it delivers and reliably submits the task so the agent starts immediately (`goal=true` sends it as `/goal`)
- `submit_prompt` — deliver a prompt to a session and reliably submit it in one call (handles long/multi-line bracketed-paste, verifies the turn started, retries Enter once); `goal=true` sends `/goal`
- `resume_session` — bring a not-running session back to life **in place** (the same way the agents view does it) and **return only once the worker is verified live**, so you never attach to a job that "already exited". It resumes under the session's own short with the same session id — no fork, no duplicate entry — unlike a raw `claude --bg --resume`, which spawns a worker under a fresh short and leaves the original behind. It validates the saved working directory first (a deleted worktree is the most common resume crash) and cleans up the worker it started on any failure, so nothing is left as garbage. Refuses to resume an already-live session. Accepts a name, short id, or full session id (a full id still works for sessions no longer in the agents list, via a CLI fallback that forks to a fresh short); optional `prompt` is delivered once it settles (`goal=true` sends `/goal`)
- `fork_session` — fork a session into a **new, independent** background session that carries **all of the source's history up to the moment of the fork**. The fork shows up in the agents view as its own entry (new short, new session id) and can be driven immediately; the source is never touched. It uses Claude Code's native `--fork-session` (`claude --bg --resume <id> --fork-session`), so the whole transcript is forked correctly — not a shallow copy — and the fork inherits the source's working directory. Accepts a name, short id, or session id for the source; optional `name` for the fork; optional `prompt` is delivered once it settles (`goal=true` sends `/goal`)
- `rename_session` — set a session's custom title (`ctrl+r` in the agents view)
- `pin_session` — pin / unpin a session so it sorts to the top (`ctrl+t` in the agents view)
- `reorder_session` — move a running session up/down or to an absolute slot (`shift+↑/↓` in the agents view)
- `delete_session` — `claude rm` (permanent, `ctrl+x` in the agents view) or `claude stop` (graceful)

Attach — everything a human can do inside a session:

- `read_screen` — current screen as plain text
- `send_text` — type text into a session (fire-and-forget by default; `wait=true` blocks and returns the settled screen)
- `send_keys` — named keys (fire-and-forget; `wait=true` to block): `enter esc tab space backspace delete up down left right home end pageup pagedown ctrl-c ctrl-d ctrl-u ctrl-l ctrl-z ctrl-r`
- `send_command` — run a slash command reliably (clears modals → waits for idle → types → submits): `/remote-control`, `/goal`, `/compact`, …
- `cancel` — interrupt the current task (Esc, or Ctrl-C with `hard=true`)

## Status

### Implemented

- [x] List sessions, including not-running ones (`--all`), with a `live` flag and live state (`state`/`tempo`/`detail`/`needs`/`cwd`/`name`)
- [x] Get a single session
- [x] Create a session (`claude --bg`), optionally delivering + submitting a starting prompt (or `/goal`)
- [x] Reliably deliver + submit a prompt (`submit_prompt`): bracketed-paste for long/multi-line, verify the turn started
- [x] Resume a not-running session (`resume_session`): in-place daemon dispatch (own short, same session id, no fork/duplicate), validate the saved cwd first, verify liveness before returning, clean up the worker on any failure
- [x] Fork a session (`fork_session`): native `--fork-session` into a new entry (new short + session id) carrying the source's full history, source untouched, verify liveness before returning, clean up the worker on any failure
- [x] Rename a session (`ctrl+r`; custom title via `.meta.json` sidecar)
- [x] Pin / unpin a session (`ctrl+t`; agents-view pin set in `~/.claude/jobs/pins.json`)
- [x] Reorder a session up/down or to an absolute slot (`shift+↑/↓`; sort keys in `~/.claude/jobs/<id>/order`)
- [x] Delete a session (`ctrl+x` remove / graceful stop)
- [x] Read a session's screen
- [x] Type text / submit prompts
- [x] Send named keys (arrows, Esc, Ctrl-C, …)
- [x] Run slash commands reliably (Esc → wait-idle → type → submit)
- [x] Cancel the current task (Esc / Ctrl-C)
- [x] Apache-2.0 license, mandatory `golangci-lint` step

### Not yet — wanted

- [ ] Full VT terminal emulation for `read_screen` (today it ANSI-strips the PTY tail, so wrapped/redrawn TUI screens render imperfectly — not a true cell grid)
- [ ] Live streaming / subscribe tool (push updates as a session changes; today `read_screen` is a pull/snapshot)
- [ ] Structured detection of permission prompts + a high-level "answer the prompt" tool
- [ ] High-level "answer the session's `needs` question" tool
- [ ] Real-time bidirectional interactive bridge (hand a live session to a human/agent)
- [ ] Rename reflected in the live daemon roster `name` (today it sets the custom title; the roster name stays the spawn name)
- [ ] Multi-attacher resize / repaint coordination
- [ ] `op:dispatch` create with agent/model/effort overrides (today create goes through `claude --bg`; a starting prompt is delivered over the PTY rather than seeded at dispatch)

## How it works

- `list` uses the daemon control op `list` for rich state, enriched with `claude agents --json` for the display name and worktree `cwd` (which `op:list` omits).
- attach actions open the daemon's `op:attach` raw PTY stream and write keystrokes — the exact same channel as the human keyboard. Reads come back from the same stream (or `op:subscribe` for `read_screen`).
- create / stop / remove shell out to the stable public `claude` CLI.
- resume goes through the daemon, not the CLI. `claude --bg --resume` is the wrong tool here: it forks the session — spawning a worker under a fresh short with a new session id and leaving the original as a duplicate not-running entry — and it crashes deterministically (the daemon does not retry) when the session has no transcript ("No conversation found") or its saved cwd is gone ("working directory no longer exists", e.g. a deleted worktree). Instead `resume_session` does exactly what pressing Enter on a session in the agents view does: it sends the daemon an `op:dispatch` with `launch.mode:"resume"` under the session's **own** short, so the session simply goes live in place (same id, single entry). It reconstructs the dispatch descriptor from the session's on-disk job state (`~/.claude/jobs/<short>/state.json`) and authenticates with the daemon control key, validates the saved cwd up front, polls the roster until the worker holds a usable state, and stops the worker on any failure so no crashed/idle session is left behind. Sessions with no on-disk job state (no longer in the agents list) fall back to the CLI resume.
- pin / reorder are **not** daemon ops — the agents-view picker keeps them on disk under `~/.claude/jobs`: the pin set in `pins.json` (a JSON array of short ids, written under a lock) and per-session sort keys in `<id>/order` and `<id>/stateOrder`. `pin_session` / `reorder_session` write exactly those files, so the change is durable and any picker reflects it.

Slash commands only work over the raw PTY (`op:attach`): they are REPL input, not conversation messages, so they cannot be delivered through any message/dispatch channel.

## License

Apache-2.0. See [LICENSE](LICENSE).
