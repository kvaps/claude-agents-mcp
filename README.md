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
- `create_session` — `claude --bg` in a directory (optional name, dangerous mode)
- `rename_session` — set a session's custom title (`ctrl+r` in the agents view)
- `pin_session` — pin / unpin a session so it sorts to the top (`ctrl+t` in the agents view)
- `reorder_session` — move a running session up/down or to an absolute slot (`shift+↑/↓` in the agents view)
- `delete_session` — `claude rm` (permanent, `ctrl+x` in the agents view) or `claude stop` (graceful)

Attach — everything a human can do inside a session:

- `read_screen` — current screen as plain text
- `send_text` — type text / submit a prompt
- `send_keys` — named keys: `enter esc tab space backspace delete up down left right home end pageup pagedown ctrl-c ctrl-d ctrl-u ctrl-l ctrl-z ctrl-r`
- `send_command` — run a slash command reliably (clears modals → waits for idle → types → submits): `/remote-control`, `/goal`, `/compact`, …
- `cancel` — interrupt the current task (Esc, or Ctrl-C with `hard=true`)

## Status

### Implemented

- [x] List sessions, including not-running ones (`--all`), with a `live` flag and live state (`state`/`tempo`/`detail`/`needs`/`cwd`/`name`)
- [x] Get a single session
- [x] Create a session (`claude --bg`)
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
- [ ] `op:dispatch` create with a starting prompt and agent/model/effort overrides (today create goes through `claude --bg`)

## How it works

- `list` uses the daemon control op `list` for rich state, enriched with `claude agents --json` for the display name and worktree `cwd` (which `op:list` omits).
- attach actions open the daemon's `op:attach` raw PTY stream and write keystrokes — the exact same channel as the human keyboard. Reads come back from the same stream (or `op:subscribe` for `read_screen`).
- create / stop / remove shell out to the stable public `claude` CLI.
- pin / reorder are **not** daemon ops — the agents-view picker keeps them on disk under `~/.claude/jobs`: the pin set in `pins.json` (a JSON array of short ids, written under a lock) and per-session sort keys in `<id>/order` and `<id>/stateOrder`. `pin_session` / `reorder_session` write exactly those files, so the change is durable and any picker reflects it.

Slash commands only work over the raw PTY (`op:attach`): they are REPL input, not conversation messages, so they cannot be delivered through any message/dispatch channel.

## License

Apache-2.0. See [LICENSE](LICENSE).
