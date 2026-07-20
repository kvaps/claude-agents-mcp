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

- `list_sessions` — every session the agents view shows, **including not-running ones** (`live:false`); running ones carry live state (`state`, `tempo`, `detail`, `needs`), short id, `cwd`, `name`, and a `pinned` flag. Not-running ones carry a `resumable` flag distinguishing **exited-but-resumable** (can be continued in place with full history — prefer that over forking or starting fresh) from **really dead** (no job state, or its working directory is gone). `live_only=true` filters to running sessions
- `get_session` — one session by short id / session id / name (same `resumable` flag as `list_sessions`)
- `create_session` — `claude --bg` in a directory (optional name, dangerous mode); optional `model` runs the session on a specific model (passed as `--model`: an alias like `sonnet`/`opus`/`haiku` or a full model id — the claude CLI validates it); with `prompt` it delivers and reliably submits the task so the agent starts immediately (`goal=true` sends it as `/goal`)
- `submit_prompt` — deliver a prompt to a session and reliably submit it in one call (handles long/multi-line bracketed-paste, then confirms the prompt actually landed — against the session transcript where possible — and retries Enter once); a not-running-but-resumable session is transparently resumed in place first, like typing into an exited session in the app; `goal=true` sends `/goal`. `on_resume_dialog` (`keep` (default) / `compact` / `ask`) decides what to do if the resumed session comes up on the CLI's resume dialog — see [The resume dialog](#the-resume-dialog)
- `resume_session` — bring a not-running session back to life **in place** (the same way the agents view does it) and **return only once the worker is verified live**, so you never attach to a job that "already exited". It resumes under the session's own short with the same session id — no fork, no duplicate entry — unlike a raw `claude --bg --resume`, which spawns a worker under a fresh short and leaves the original behind. It validates the saved working directory first (a deleted worktree is the most common resume crash) and cleans up the worker it started on any failure, so nothing is left as garbage. Refuses to resume an already-live session. Accepts a name, short id, or full session id — including for sessions that have dropped off the agents list entirely, which are found by their transcript and resurrected in their own working directory under their recovered name (see [Sessions that dropped off the list](#sessions-that-dropped-off-the-list)); optional `model` resumes the session on a different model, **replacing** any `--model` it was originally launched with (omit to keep it); optional `prompt` is delivered once it settles (`goal=true` sends `/goal`); `on_resume_dialog` decides how the CLI's resume dialog is answered — see [The resume dialog](#the-resume-dialog)
- `fork_session` — fork a session into a **new, independent** background session that carries **all of the source's history up to the moment of the fork**. The fork shows up in the agents view as its own entry (new short, new session id) and can be driven immediately; the source is never touched. It uses Claude Code's native `--fork-session` (`claude --bg --resume <id> --fork-session`), so the whole transcript is forked correctly — not a shallow copy — and the fork inherits the source's working directory. Accepts a name, short id, or session id for the source; optional `name` for the fork; optional `model` runs the fork on a specific model, independent of the source's; optional `prompt` is delivered once it settles (`goal=true` sends `/goal`); `on_resume_dialog` decides how the CLI's resume dialog is answered — a fork replays the source's history, so it can hit the same dialog
- `rename_session` — set a session's custom title (`ctrl+r` in the agents view)
- `pin_session` — pin / unpin a session so it sorts to the top (`ctrl+t` in the agents view)
- `reorder_session` — move a running session up/down or to an absolute slot (`shift+↑/↓` in the agents view)
- `delete_session` — `claude rm` (permanent, `ctrl+x` in the agents view) or `claude stop` (graceful)

Attach — everything a human can do inside a session:

- `read_screen` — current screen as plain text
- `send_text` — type text into a session (fire-and-forget by default; `wait=true` blocks and returns the settled screen); auto-resumes a not-running-but-resumable session in place first, honouring `on_resume_dialog`
- `send_keys` — named keys (fire-and-forget; `wait=true` to block): `enter esc tab space backspace delete up down left right home end pageup pagedown ctrl-c ctrl-d ctrl-u ctrl-l ctrl-z ctrl-r`. **`enter` is a confirmation keystroke, not a neutral one** — whatever holds focus consumes it. Never send it to recover a delivery without reading the screen first; see [The resume dialog](#the-resume-dialog)
- `send_command` — run a slash command reliably (clears modals → waits for idle → types → submits): `/remote-control`, `/goal`, `/compact`, …; auto-resumes a not-running-but-resumable session in place first, honouring `on_resume_dialog`
- `cancel` — interrupt the current task (Esc, or Ctrl-C with `hard=true`)

## Sessions that dropped off the list

The list entry and the conversation are different artifacts with different lifetimes. `claude rm` clears the entry; the daemon stops tracking sessions it no longer runs. Neither touches `~/.claude/projects/<project>/<sessionId>.jsonl`, and that file *is* the session — everything needed to bring it back with its full history is in it. So a missing list entry is not a missing session.

`resume_session` (and the auto-resume in `submit_prompt` / `send_text` / `send_command`) falls back to the transcript when the list has no entry:

- **Lookup by transcript.** A short id is the first 8 hex digits of the session id, which is also the transcript's file name, so both resolve to the same file. References shorter than 8 hex digits are refused rather than matched loosely. Only a session with no transcript anywhere is reported as not found.
- **Working directory recovered from the records.** `--resume` resolves a conversation relative to the launch directory, so the worker has to start in the session's own `cwd` — launching from anywhere else fails to find a transcript sitting right there on disk. It is read from the transcript's `cwd` fields, preferring the most recent one that still exists.
- **Name recovered and registered.** A session resurrected without one carries no name in the store: while its process runs the view derives a title from the transcript so it looks fine, but once it exits the entry falls back to showing the session kind (`bg`) — hundreds of records of real history, listed as nothing. The title comes from the transcript's `custom-title` (or `agent-name`) records and is registered the same way `create_session` registers one.
- **A missing directory is named, not crashed into.** A transcript is keyed to the directory its session ran in; if that directory is gone (a deleted worktree is the usual cause) the error says so and says to recreate it, instead of spawning a worker that exits at startup.
- **No phantoms.** A resume that never comes up leaves an entry with no name, no transcript of its own and a `failed` state. The spawned worker is removed on failure, so there is nothing to clean up by hand; the session's own transcript is never touched.

## The resume dialog

When a session that is old enough and large enough is resumed, the Claude Code CLI opens a startup dialog before handing the keyboard to the REPL:

```
This session is 2h 15m old and 187k tokens.

Resuming the full session will consume a substantial portion of your usage
limits. We recommend resuming from a summary.

❯ Resume from summary (recommended)
  Resume full session as-is
  Don't ask me again
```

Two things about it matter to anything driving a session programmatically:

- **The preselected option compacts the conversation.** Choosing it submits `/compact`; the session's detail is replaced by a summary, and any prompt sitting unsubmitted in the input box is discarded with it.
- **The dialog owns the keyboard.** Text delivered while it is up does not reach the input box, and an `Enter` aimed at the input box answers the dialog instead — with the preselected option.

So the tools never send a bare `Enter` to rescue a delivery without first reading the screen, and they never tell you to. Instead:

- After a resume, the screen is inspected. The dialog is matched by its own option labels; anything else that looks like a choice dialog is reported as `unknown` and left strictly alone, because answering a question you have not identified is worse than not answering it.
- `on_resume_dialog` decides what happens: **`keep` (the default)** navigates to *Resume full session as-is* and confirms it, keeping the conversation; `compact` accepts the summary; `ask` leaves the dialog up and returns its kind and options so the caller can choose. **Compaction is opt-in** — delivering a prompt should never spend a session's context as a side effect.
- Answering is verified, not fired blind: the selection is moved with arrow keys and the screen re-read until the intended option is the highlighted one, and only then is `Enter` sent. If the selection cannot be put where it belongs, nothing is confirmed and the dialog is reported instead.
- When a delivery is blocked, the error names the dialog and its options rather than saying "the turn did not start".

The CLI gates the dialog on roughly 70 minutes since the last message and ~100k estimated tokens (`CLAUDE_CODE_RESUME_THRESHOLD_MINUTES` / `CLAUDE_CODE_RESUME_TOKEN_THRESHOLD` override the thresholds), so it is a long-running-fleet problem specifically: exactly the sessions with the most context to lose.

## What "the turn started" means

`submit_prompt` confirms a delivery against the session transcript where it can: the prompt appearing as a user record in `~/.claude/projects/<project>/<sessionId>.jsonl` is the only evidence that the text reached the conversation. The daemon roster heuristics remain as a fallback for sessions whose transcript cannot be located, and the tool result says which of the two confirmed it.

A prompt that has not (yet) started a turn is reported as one of three distinct states, because the right response differs in each:

| state | what happened | what to do |
| --- | --- | --- |
| blocked on a dialog | a dialog has focus; the prompt never reached the input box | answer the dialog — `on_resume_dialog`, or explicitly |
| queued | the session was already running a turn; the prompt is in the input box and the REPL will consume it when that turn ends | nothing. Do not retry (it would deliver twice) and do not send `Enter` |
| stuck | no dialog, no running turn, text unsubmitted in the box | `Enter` is the right recovery — and has already been retried twice by the time this is reported |

## Status

### Implemented

- [x] List sessions, including not-running ones (`--all`), with a `live` flag and live state (`state`/`tempo`/`detail`/`needs`/`cwd`/`name`)
- [x] Get a single session
- [x] Create a session (`claude --bg`), optionally delivering + submitting a starting prompt (or `/goal`)
- [x] Reliably deliver + submit a prompt (`submit_prompt`): bracketed-paste for long/multi-line, verify the prompt landed against the session transcript
- [x] Handle the CLI's resume dialog (`on_resume_dialog`: `keep` (default) / `compact` / `ask`) instead of answering it with a blind `Enter` that compacts the conversation and discards the prompt
- [x] Distinguish blocked-on-a-dialog / queued-behind-a-running-turn / genuinely-stuck instead of reporting all three as "the turn did not start"
- [x] Resurrect sessions that dropped off the agents list, by transcript: recover the working directory and display name from the records, resume in that directory, register the name, and remove the worker if the spawn fails
- [x] Resume a not-running session (`resume_session`): in-place daemon dispatch (own short, same session id, no fork/duplicate), validate the saved cwd first, pass the transcript path so the worker finds its conversation regardless of cwd, verify liveness before returning, clean up the worker on any failure
- [x] Auto-resume on input: `submit_prompt` / `send_text` / `send_command` into an exited-but-resumable session transparently resume it in place first (like typing into an exited session in the app)
- [x] `resumable` flag on not-running sessions (`list_sessions` / `get_session`): exited-but-resumable vs really dead, so an orchestrator continues instead of forking
- [x] Fork a session (`fork_session`): native `--fork-session` into a new entry (new short + session id) carrying the source's full history, source untouched, verify liveness before returning, clean up the worker on any failure
- [x] Model selection on `create_session` / `fork_session` / `resume_session`: optional `model` (alias or full model id, passed as `--model`, validated by the claude CLI); on resume an explicit model replaces the one the session was launched with
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
- [ ] Structured detection of permission prompts + a high-level "answer the prompt" tool (the resume dialog is recognised and answerable today; permission prompts are only reported as an unknown dialog)
- [ ] High-level "answer the session's `needs` question" tool
- [ ] Real-time bidirectional interactive bridge (hand a live session to a human/agent)
- [ ] Rename reflected in the live daemon roster `name` (today it sets the custom title; the roster name stays the spawn name)
- [ ] Multi-attacher resize / repaint coordination
- [ ] `op:dispatch` create with agent/effort overrides (today create goes through `claude --bg` — the model is already selectable via `--model` — and a starting prompt is delivered over the PTY rather than seeded at dispatch)

## How it works

- `list` uses the daemon control op `list` for rich state, enriched with `claude agents --json` for the display name and worktree `cwd` (which `op:list` omits).
- attach actions open the daemon's `op:attach` raw PTY stream and write keystrokes — the exact same channel as the human keyboard. Reads come back from the same stream (or `op:subscribe` for `read_screen`).
- create / stop / remove shell out to the stable public `claude` CLI.
- resume goes through the daemon, not the CLI. `claude --bg --resume` is the wrong tool here: it forks the session — spawning a worker under a fresh short with a new session id and leaving the original as a duplicate not-running entry — and it crashes deterministically (the daemon does not retry) when the session has no transcript ("No conversation found") or its saved cwd is gone ("working directory no longer exists", e.g. a deleted worktree). Instead `resume_session` does exactly what pressing Enter on a session in the agents view does: it sends the daemon an `op:dispatch` with `launch.mode:"resume"` under the session's **own** short, so the session simply goes live in place (same id, single entry). It reconstructs the dispatch descriptor from the session's on-disk job state (`~/.claude/jobs/<short>/state.json`) and authenticates with the daemon control key, validates the saved cwd up front, polls the roster until the worker holds a usable state, and stops the worker on any failure so no crashed/idle session is left behind. Sessions with no on-disk job state (no longer in the agents list) fall back to the CLI resume.
- the dispatch descriptor must carry `launch.transcriptPath`. The resumed worker's `--resume <sessionId>` lookup only searches the project directory derived from the launch `cwd` (`~/.claude/projects/<sanitized-cwd>/`), so a session whose transcript lives under a different project dir — typically one that switched into a worktree mid-run — exits at startup with "No conversation found" (`exit 1`, `exit_with_message`) and crash-loops, even though the same session resumes fine from the agents view. The picker avoids this by passing the transcript path explicitly in the descriptor; `resume_session` derives the same path from the job state's `linkScanPath` (falling back to a `~/.claude/projects/*/<sessionId>.jsonl` search) and omits it only when no transcript exists yet.
- pin / reorder are **not** daemon ops — the agents-view picker keeps them on disk under `~/.claude/jobs`: the pin set in `pins.json` (a JSON array of short ids, written under a lock) and per-session sort keys in `<id>/order` and `<id>/stateOrder`. `pin_session` / `reorder_session` write exactly those files, so the change is durable and any picker reflects it.

Slash commands only work over the raw PTY (`op:attach`): they are REPL input, not conversation messages, so they cannot be delivered through any message/dispatch channel.

## License

Apache-2.0. See [LICENSE](LICENSE).
