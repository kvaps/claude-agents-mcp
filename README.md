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
- `send_keys` — named keys (fire-and-forget; `wait=true` to block): `enter esc tab shift-tab space backspace delete up down left right home end pageup pagedown ctrl-c ctrl-d ctrl-u ctrl-l ctrl-z ctrl-r`. **`enter` is a confirmation keystroke, not a neutral one** — whatever holds focus consumes it. Never send it to recover a delivery without reading the screen first; see [The resume dialog](#the-resume-dialog)
- `set_permission_mode` — change a session's permission mode (`default` / `acceptEdits` / `plan` / `bypassPermissions` / `dontAsk` / `auto`). A live session is switched **in place** through its shift+tab carousel — no restart, no lost turn. `bypassPermissions` is the exception and needs a bypass-capable worker; `restart=true` respawns one with rewritten launch flags, and `allow_bypass=true` makes every later switch free. See [Permission modes](#permission-modes)
- `send_command` — run a slash command reliably (clears modals → waits for idle → types → submits): `/remote-control`, `/goal`, `/compact`, …; auto-resumes a not-running-but-resumable session in place first, honouring `on_resume_dialog`
- `cancel` — interrupt the current task (Esc, or Ctrl-C with `hard=true`)

Agents writing to each other:

- `send_message` — write to another agent. Same delivery path as `submit_prompt`, but the text arrives wrapped in an envelope naming the sender and its return address, so the recipient knows it is talking to an agent and can answer with a message of its own instead of replying into its own session where nobody sees it. The sender is not a parameter — it is read from the session this server was started by, so it cannot be spoofed. A recipient mid-turn queues the message instead of being interrupted; a not-running-but-resumable one is resumed in place first (`resume=false` refuses instead, reporting the message as undelivered); a name matching several sessions is refused with the candidates listed rather than delivered to the first match. See [Agents writing to each other](#agents-writing-to-each-other)
- `whoami` — who *you* are in the fleet: short id, session id, display name, working directory, and whether other agents can reach you (`addressable`). Read from the environment your own session gave this server, so it is you, not a guess — use it to tell a peer how to reach you, or to recognise yourself in `list_sessions`

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

## Permission modes

`set_permission_mode` changes the mode a session runs under. A live session is switched **in place** by driving its shift+tab carousel: that is not a status-bar toggle — Claude Code's `chat:cycleMode` handler runs `cyclePermissionMode` and writes the result into the tool permission context every tool call is authorised against, then rechecks the queued permission prompts. Nothing is restarted and no context is lost. The keystroke is CSI Z (`ESC [ Z`), which its key parser reads as tab-with-shift.

Verified against a live 2.1.259 worker, the carousel is:

```text
bypass permissions → auto mode → manual mode → accept edits → plan mode → (back to the top)
```

`manual mode` is what the default mode calls itself in the footer. **The `bypass permissions` step is present only for a worker that was launched bypass-capable** — a session started `--permission-mode auto` cycles the same ring with that one step missing.

### Why bypass is different

Claude Code decides bypass availability once, at startup:

```js
// permissionSetup.ts
const isBypassPermissionsModeAvailable =
    (permissionMode === 'bypassPermissions' || allowDangerouslySkipPermissions)
    && !growthBookDisableBypassPermissionsMode && !settingsDisableBypassPermissionsMode
```

From then on the flag only ever goes *false* (`createDisabledBypassPermissionsContext`); nothing raises it. Every route into bypass tests that same flag and refuses without it — the shift+tab carousel (`getNextPermissionMode`), the SDK `set_permission_mode` control request (`print.ts`), and the claude.ai bridge (`useReplBridge`), the last two with the message *"Cannot set permission mode to bypassPermissions because the session was not launched with `--dangerously-skip-permissions`"*. There is no slash command for it either: `/config` deliberately offers only `default`, `plan`, `acceptEdits`, `dontAsk`, `auto`. So a session started in `auto` or `acceptEdits` **cannot** be talked into bypass, by keystroke or by protocol.

A launch is bypass-capable when it carries `--dangerously-skip-permissions`, `--permission-mode bypassPermissions`, or `--allow-dangerously-skip-permissions` — the last one puts bypass in the carousel *without* switching it on, which is the flag worth passing to any agent whose mode might need raising later.

### Restarting instead of removing

When the mode is out of carousel reach, `restart=true` stops the worker, rewrites the permission flags in `~/.claude/jobs/<short>/state.json` (`respawnFlags` — where the daemon reads launch flags from), and dispatches it back **under its own short**. Same session id, same history, same worktree. `claude rm` is never involved, so its worktree guards ("worktree has uncommitted changes", "worktree has commits that are not pushed anywhere") — which have no force flag and can leave a session unfixable — never come up. The cost is the turn in flight, which is why the restart is opt-in.

`ps` cannot answer the "what mode is this worker in" question, in either direction: workers are claimed from a pool of pre-warmed spare processes, so every one of them shows the same `claude bg-pty-host … --bg-spare …` command line whatever mode it runs in. The permission flags live in the job state, not on the command line.

## What "the turn started" means

`submit_prompt` confirms a delivery against the session transcript where it can: the prompt appearing as a user record in `~/.claude/projects/<project>/<sessionId>.jsonl` is the only evidence that the text reached the conversation. The daemon roster heuristics remain as a fallback for sessions whose transcript cannot be located, and the tool result says which of the two confirmed it.

A prompt that has not (yet) started a turn is reported as one of three distinct states, because the right response differs in each:

| state | what happened | what to do |
| --- | --- | --- |
| blocked on a dialog | a dialog has focus; the prompt never reached the input box | answer the dialog — `on_resume_dialog`, or explicitly |
| queued | the session was already running a turn; the prompt is in the input box and the REPL will consume it when that turn ends | nothing. Do not retry (it would deliver twice) and do not send `Enter` |
| stuck | no dialog, no running turn, text unsubmitted in the box | `Enter` is the right recovery — and has already been retried twice by the time this is reported |

## Agents writing to each other

`submit_prompt` delivers text as if the user had typed it. That is right for seeding a task and wrong for a conversation: the recipient cannot tell that another agent is talking, cannot tell whether anyone is waiting, and has nowhere to answer — its reply goes into its own session, where the sender never sees it. `send_message` is the same delivery path with the two things a conversation needs, identity and a return address.

**The sender is not a parameter.** Claude Code gives every process it starts inside a session — MCP servers included — `CLAUDE_CODE_SESSION_ID` and `CLAUDE_JOB_DIR` (whose base name *is* the short id), so this server knows which session is calling it without being told. An agent therefore cannot claim to be another one, and does not have to know its own name to write to someone. `whoami` reports that same identity back to the caller, including `addressable:false` for a caller nothing can deliver to (a server not started by a background session) — which the envelope then says out loud instead of inviting a reply into the void.

**The envelope** is what the recipient reads:

```text
<agent-message id="m-8f3a1c" from="orchestrator [a1b2c3d4]" to="reviewer [b2c3d4e5]" at="2026-08-17T21:40:03Z">
check whether the tests pass
</agent-message>

[m-8f3a1c] This is a message from another agent, not from your user. It did not interrupt anything and nobody is blocked on it — answer when the work you are doing allows. To answer, call this MCP server's send_message tool (usually mcp__claude-agents__send_message) with to:"a1b2c3d4" — …
```

The return address is the sender's short id, not its display name: names are not unique, and a message that cannot be answered reliably is barely a message. The message id leads the instruction line for a mechanical reason as well — a delivery is confirmed by finding the body's longest line in the recipient's transcript, and for a short message that line is this boilerplate, identical in every message ever sent; leading with the id keeps two agents writing to the same session at the same moment from confirming each other's deliveries.

**Nothing is interrupted, nothing is silently lost.** A recipient running a turn is not cancelled and not raced: the message lands in its input box and the REPL consumes it when that turn ends, reported to the sender as `queued` — do not resend, that delivers it twice. A recipient holding a dialog is reported as blocked, with the dialog named, rather than rescued by a blind `Enter`. Delivery is confirmed against the recipient's transcript exactly as `submit_prompt` confirms a prompt (see [What "the turn started" means](#what-the-turn-started-means)).

**A sleeping recipient is woken, not skipped.** Not running but resumable means resumed in place first, with its full history, like typing into an exited session in the app. `resume=false` refuses instead and says so — an undelivered message reported as undelivered, never a message dropped on the floor.

**One name, one recipient.** Addressing accepts a short id, a session id or a display name, in that order of precedence, and a name that matches several sessions is refused with the candidates listed unless exactly one of them is live. Every other tool here resolves a reference to the first match, which is fine for actions a human retries; a message put in front of the wrong agent has already been read by the time the mistake shows.

**Delivered is not read.** The call returns when the message has landed in the recipient's conversation, not when it has been read, acted on or answered. Replies arrive later as messages of their own — or not at all. Nothing blocks.

**No second channel.** The message travels over `op:reply`, the daemon control-socket op the `claude` CLI itself uses to hand text to a background session, via the same `SubmitPrompt` that every other delivery here goes through (PTY fallback, verification and all). Recent Claude Code versions do have their own cross-session inbox (`CLAUDE_CODE_MESSAGING_SOCKET`, a per-worker socket under `/tmp/cc-socks`) behind the agent-teams flag; this server deliberately does not speak that undocumented protocol — and it reaches sessions that one does not, since only this path can wake a session that is not running.

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
- [x] Inter-agent messaging (`send_message`): sender identity taken from the environment (not a spoofable parameter), an envelope carrying who is writing and how to answer, queue-don't-interrupt for a busy recipient, auto-resume for a sleeping one, ambiguous names refused with candidates instead of misrouted
- [x] Self-identification (`whoami`): a session can find out its own short id, name, working directory and whether peers can reach it
- [x] Rename a session (`ctrl+r`; custom title via `.meta.json` sidecar)
- [x] Pin / unpin a session (`ctrl+t`; agents-view pin set in `~/.claude/jobs/pins.json`)
- [x] Reorder a session up/down or to an absolute slot (`shift+↑/↓`; sort keys in `~/.claude/jobs/<id>/order`)
- [x] Delete a session (`ctrl+x` remove / graceful stop)
- [x] Read a session's screen
- [x] Type text / submit prompts
- [x] Send named keys (arrows, Esc, shift+tab, Ctrl-C, …)
- [x] Change a session's permission mode (`set_permission_mode`): in place via the shift+tab carousel for a live session, or by rewriting `respawnFlags` and re-dispatching under the same short when the target is out of carousel reach — see [Permission modes](#permission-modes)
- [x] Run slash commands reliably (Esc → wait-idle → type → submit)
- [x] Cancel the current task (Esc / Ctrl-C)
- [x] Apache-2.0 license, mandatory `golangci-lint` step

### Not yet — wanted

- [ ] Full VT terminal emulation for `read_screen` (today it ANSI-strips the PTY tail, so wrapped/redrawn TUI screens render imperfectly — not a true cell grid)
- [ ] Live streaming / subscribe tool (push updates as a session changes; today `read_screen` is a pull/snapshot)
- [ ] Structured detection of permission prompts + a high-level "answer the prompt" tool (the resume dialog is recognised and answerable today; permission prompts are only reported as an unknown dialog)
- [ ] High-level "answer the session's `needs` question" tool
- [ ] Read receipts and threading for `send_message` (today a message is confirmed as landed in the recipient's conversation; there is no signal that it was read, and a reply is tied to what it answers only by the message id the recipient quotes)
- [ ] Broadcast: one message to several recipients, or to a named group (today each recipient is a separate call)
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
- `send_message` adds no channel of its own: it renders the envelope and hands it to the same `SubmitPrompt` used for every other delivery (daemon `op:reply`, PTY fallback, transcript verification). What it adds is identity — the sender is read from the environment Claude Code gives this server process (`CLAUDE_CODE_SESSION_ID`, and `CLAUDE_JOB_DIR`, whose base name is the short id), which is also what `whoami` reports, so it is neither a parameter nor a guess.
- pin / reorder are **not** daemon ops — the agents-view picker keeps them on disk under `~/.claude/jobs`: the pin set in `pins.json` (a JSON array of short ids, written under a lock) and per-session sort keys in `<id>/order` and `<id>/stateOrder`. `pin_session` / `reorder_session` write exactly those files, so the change is durable and any picker reflects it.

Slash commands only work over the raw PTY (`op:attach`): they are REPL input, not conversation messages, so they cannot be delivered through any message/dispatch channel.

## License

Apache-2.0. See [LICENSE](LICENSE).
