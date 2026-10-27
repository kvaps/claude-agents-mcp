package agents

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// errNotStarted is returned when a prompt was delivered but no turn started: the
// text is sitting unsubmitted in the input box (typically collapsed to a
// "[Pasted text]" bracketed paste that still needs an Enter).
//
// It is now narrow: the two other states that used to reach it — blocked on a
// dialog, and queued behind a running turn — are detected first and reported as
// themselves, because the right response differs in each case (answer the
// dialog, wait, press Enter) and one message advising Enter was wrong for two of
// the three. By the time this is returned, Enter has already been sent twice and
// did not take, so it does not suggest sending another.
var errNotStarted = errors.New("prompt delivered but the session did not start a turn — no dialog has focus and no turn is running, so the text is genuinely stuck unsubmitted in the input box. Enter was already sent twice without effect; read_screen to see what state the session is in rather than sending more keystrokes blind")

// goalMaxLen is the maximum length, in characters, that Claude Code's /goal
// command accepts for a goal condition. A longer condition is rejected with
// "Goal condition is limited to 4000 characters (got N)" and the prompt is left
// unsubmitted in the input box, so SubmitPrompt must not hand /goal a body over
// this length — it falls back to a plain prompt instead (see SubmitPrompt).
const goalMaxLen = 4000

// readyMarkers are strings that appear once a freshly-booted session has
// rendered its REPL prompt and can accept input.
var readyMarkers = []string{"❯", "for shortcuts", "auto mode"}

// WaitReady blocks until a session has booted far enough to accept input (its
// prompt is on screen), or the timeout elapses.
func (c *Client) WaitReady(short string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if screen, err := c.ReadScreen(short, 60); err == nil {
			for _, m := range readyMarkers {
				if strings.Contains(screen, m) {
					return nil
				}
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("session %s did not become ready within %s", short, timeout)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// WaitInteractive blocks until a session has finished booting and is waiting on
// us — either sitting at its REPL prompt or holding a dialog — or the timeout
// elapses.
//
// A resumed session does not always land at its prompt: the CLI can put up the
// resume dialog instead, and while it is up the prompt markers WaitReady looks
// for are not on screen. Waiting only for those would burn the whole timeout on
// precisely the case that needs attention, and then report the session as never
// having settled. Either state ends the wait; the caller settles the dialog if
// there is one.
func (c *Client) WaitInteractive(short string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if screen, err := c.ReadScreen(short, 60); err == nil {
			if DetectDialog(screen).Blocking() {
				return nil
			}
			for _, m := range readyMarkers {
				if strings.Contains(screen, m) {
					return nil
				}
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("session %s did not become ready within %s", short, timeout)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// turnStarted reports whether the session has begun a turn relative to a
// baseline captured just before submitting. A normal conversational turn keeps
// state=="done" and only flips tempo to "active" and updates detail (state only
// reads "working" for goal/loop sessions), so any of those signals counts.
//
// It is weak evidence — tempo flips on any repaint, including the one the text
// landing in the input box causes — so it is only consulted for a session whose
// transcript cannot be located at all.
func turnStarted(base, cur Session) bool {
	return cur.Busy() ||
		cur.Detail != base.Detail ||
		cur.State != base.State ||
		(cur.Tempo == "active" && base.Tempo != "active")
}

// delivery is the evidence baseline for one prompt delivery, captured before the
// text is sent: the session's roster state (for the TUI heuristics), the byte
// offsets of its transcripts (for ground truth) and when it was sent.
type delivery struct {
	base Session
	mark transcriptMark
	body string
	at   time.Time
}

// beginDelivery snapshots the baseline a delivery is later judged against. It
// must be called before the text is sent — the native op:reply can submit the
// turn itself, so a baseline taken afterwards would already reflect the started
// state and hide the very change we look for.
func (c *Client) beginDelivery(short, body string) delivery {
	base, _ := c.Resolve(short)
	return delivery{base: base, mark: markTranscript(base.SessionID), body: body, at: time.Now()}
}

// Confirmations returned by SubmitPrompt, reported up to the caller so a success
// is honest about what it is based on.
const (
	// confirmedTranscript: the prompt was found as a user record in the session
	// transcript. This is ground truth — the text reached the conversation.
	confirmedTranscript = "prompt confirmed in the session transcript"
	// confirmedUnrecorded: the prompt left the input box and the session is
	// working, but the CLI had not written it to the transcript by the end of
	// the confirmation window. The CLI records a prompt only after processing it
	// (hooks, attachments) and flushes on a timer, so a loaded machine can
	// take longer than the window. Treated as delivered.
	confirmedUnrecorded = "prompt submitted and the session is working on it, but it was not in the session transcript yet when the confirmation window ran out — treat it as delivered: do not resubmit (that would deliver it twice); get_session or read_screen if in doubt"
	// confirmedState: the session's transcript could not be located (no session
	// id), so the only evidence is the roster changing as a turn starting would.
	// It says the session started doing something, not that it was our text.
	confirmedState = "turn started (roster state only; the session's transcript could not be located to confirm against)"
	// confirmedQueued: the session was already running a turn, so the prompt is
	// sitting in the input box and the REPL will consume it when that turn ends.
	// This is a success, not a failure, and it specifically must not be retried:
	// a second submit_prompt would deliver the body twice.
	confirmedQueued = "prompt queued behind the turn the session is already running; it will be consumed when that turn ends — do not retry and do not send Enter (either would deliver it twice or interrupt the running turn)"
)

// errNotDelivered is returned when, at the end of the confirmation window,
// nothing shows the prompt was taken: it is not in the transcript or the CLI's
// queue, the input box does not hold it, and the session is not working.
var errNotDelivered = errors.New("prompt not delivered: it is not in the session transcript, the input box does not hold it, and the session is not working. Enter was not the problem. read_screen before resubmitting — if the screen shows the prompt being worked on after all, it was delivered and resubmitting would deliver it twice")

// Default timings for confirming a delivery against the transcript.
const (
	// defaultConfirmWindow bounds the whole wait for the prompt to appear in
	// the transcript, from the moment it was sent. CLAUDE_AGENTS_CONFIRM_TIMEOUT
	// overrides it.
	defaultConfirmWindow = 15 * time.Second
	// rescueAfter is how long the transcript is given before the session is
	// inspected for text stuck unsubmitted in its input box.
	rescueAfter = 4 * time.Second
	// enterWait is how long the transcript is given after each rescue Enter.
	enterWait = 6 * time.Second
)

// confirmTimeoutEnv names the variable that overrides defaultConfirmWindow: a Go
// duration ("20s") or a whole number of seconds, clamped to 1s–120s.
const confirmTimeoutEnv = "CLAUDE_AGENTS_CONFIRM_TIMEOUT"

// confirmTimings are the waits one delivery is confirmed within.
type confirmTimings struct {
	window, rescue, enter time.Duration
}

// timings returns the client's confirmation waits: the test override where one
// is set, the defaults and the environment otherwise.
func (c *Client) timings() confirmTimings {
	t := c.confirm
	if t.window == 0 {
		t.window = confirmWindowFromEnv()
	}
	if t.rescue == 0 {
		t.rescue = rescueAfter
	}
	if t.enter == 0 {
		t.enter = enterWait
	}
	t.rescue = min(t.rescue, t.window)
	return t
}

func confirmWindowFromEnv() time.Duration {
	v := strings.TrimSpace(os.Getenv(confirmTimeoutEnv))
	if v == "" {
		return defaultConfirmWindow
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		n, nerr := strconv.Atoi(v)
		if nerr != nil {
			return defaultConfirmWindow
		}
		d = time.Duration(n) * time.Second
	}
	return min(max(d, time.Second), 2*time.Minute)
}

// delivered reports how, if at all, the prompt has been confirmed as landed.
//
// Wherever the transcript can be located it is the only thing consulted: it is
// the one signal that identifies *our* text, and the roster changes before the
// CLI writes the prompt down, so letting the roster answer first would turn
// most confirmations into roster-only ones. The roster heuristic is the
// fallback for a session whose transcript cannot be located at all.
func (c *Client) delivered(short string, d *delivery) string {
	if !d.mark.available() {
		// A session that had no session id at the baseline may have one now.
		if cur, err := c.Resolve(short); err == nil && cur.SessionID != "" {
			d.mark.adopt(cur.SessionID, d.at)
		}
	}
	if d.mark.available() {
		if d.mark.Landed(d.body) {
			return confirmedTranscript
		}
		return ""
	}
	if cur, err := c.Resolve(short); err == nil && turnStarted(d.base, cur) {
		return confirmedState
	}
	return ""
}

// waitDelivered polls until the prompt is confirmed as landed, or timeout,
// backing off from 100ms to 1s between reads. It returns the confirmation, or ""
// if none was reached.
func (c *Client) waitDelivered(short string, d *delivery, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	pause := 100 * time.Millisecond
	for {
		if how := c.delivered(short, d); how != "" {
			return how
		}
		left := time.Until(deadline)
		if left <= 0 {
			return ""
		}
		time.Sleep(min(pause, left))
		pause = min(pause*3/2, time.Second)
	}
}

// sessionState is what can be seen of a delivery that the transcript has not
// confirmed: the transcript since the mark, the screen and the roster.
type sessionState struct {
	scan    transcriptScan
	pending bool // the input box still holds the prompt
	working bool // the screen shows a turn running
	busy    bool // the roster reports state=="working"
	seen    bool // the screen could be read, so pending is known
}

// underway reports whether the prompt has evidently been taken: it is no longer
// in the input box and the session is doing something with it. Roster detail
// and tempo are left out on purpose — both change on a repaint alone.
func (s sessionState) underway() bool {
	return !s.pending && (s.working || s.busy || s.scan.Grew || s.scan.Enqueued)
}

func (c *Client) observe(short string, d delivery) sessionState {
	st := sessionState{scan: d.mark.Scan(d.body)}
	if cur, err := c.Resolve(short); err == nil {
		st.busy = cur.Busy()
	}
	before, err := c.ReadScreen(short, 60)
	if err != nil {
		return st
	}
	time.Sleep(screenSampleGap)
	after, err := c.ReadScreen(short, 60)
	if err != nil {
		return st
	}
	st.seen = true
	st.pending = inputHolds(after, d.body)
	st.working = strings.Contains(newOutput(before, after), "esc to interrupt")
	return st
}

// screenSampleGap separates the two screen reads observe compares. A running
// turn repaints its spinner several times a second; an idle session draws
// nothing.
const screenSampleGap = 600 * time.Millisecond

// newOutput returns what the second of two reads of a session's output tail
// added to the first. The tail is raw output, so a spinner from a turn that has
// since ended can still be in it; only output that is still arriving says a
// turn is running now.
func newOutput(before, after string) string {
	if before == after {
		return ""
	}
	anchor := before[max(0, len(before)-200):]
	if i := strings.LastIndex(after, anchor); anchor != "" && i >= 0 {
		return after[i+len(anchor):]
	}
	return after
}

// inputHolds reports whether the input box on screen still holds the prompt.
// The input box is drawn below the conversation, so the last line that starts
// with the prompt marker is it; it holds the prompt when it shows the collapsed
// "[Pasted text" placeholder or the start of the prompt's first line.
func inputHolds(screen, body string) bool {
	lines := strings.Split(normaliseScreen(screen), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		rest, ok := strings.CutPrefix(trimBox(lines[i]), "❯")
		if !ok {
			continue
		}
		rest = normaliseText(rest)
		if rest == "" {
			return false
		}
		if strings.Contains(rest, "[Pasted text") {
			return true
		}
		first, _, _ := strings.Cut(strings.TrimSpace(body), "\n")
		head := normaliseText(first)
		if len(head) > 24 {
			head = strings.ToValidUTF8(head[:24], "")
		}
		return head != "" && strings.HasPrefix(rest, head)
	}
	return false
}

// buildSubmitBody produces the exact text to deliver for a submission. A plain
// prompt is delivered verbatim (only trailing newlines trimmed). A goal is
// delivered as a "/goal <condition>" command — but only when the condition fits
// Claude Code's goalMaxLen limit; a longer condition would be rejected outright
// ("Goal condition is limited to 4000 characters") and left unsubmitted, so it
// falls back to the plain body, delivering the full task as an ordinary prompt.
// The overlay is lost but the whole task reaches the agent and the turn starts.
// Length is measured in characters (runes), matching Claude Code's own count, so
// a multi-byte (e.g. Cyrillic) goal is not needlessly downgraded.
func buildSubmitBody(text string, goal bool) string {
	body := strings.TrimRight(text, "\r\n")
	if goal {
		if g := strings.TrimSpace(text); utf8.RuneCountInString(g) <= goalMaxLen {
			body = "/goal " + g
		}
	}
	return body
}

// SubmitPrompt delivers text to a session and submits it as a turn, then always
// verifies the turn actually started before reporting success. It is the single
// delivery path for both plain prompts and /goal commands (goal just prefixes
// "/goal "), so both get the same guarantee.
//
// Preferred delivery: the daemon's native op:reply (see Reply), which hands the
// text straight to the worker's REPL — no PTY typing. But a successful op:reply
// ack only means the text reached the REPL, NOT that a turn started: for a long
// or multi-line body the REPL collapses it into an unsubmitted bracketed paste
// ("[Pasted text]") that still needs an Enter, so the ack alone is not
// trustworthy. So after a successful ack we still verify the prompt landed; if
// it hasn't, we press Enter to rescue the unsubmitted paste, retrying once
// before reporting failure. When the native op is unavailable (an older daemon,
// a peer backend, or a session not accepting replies) it falls back to the
// keystroke-emulation path, submitViaPTY, which carries the same verify+retry
// guarantee.
//
// "Landed" is checked against the session transcript wherever it can be located:
// the prompt appearing as a user record under
// ~/.claude/projects/<project>/<sessionId>.jsonl is the only evidence that our
// text actually reached the conversation, as opposed to the roster heuristics,
// which merely observe that the session started doing something. The heuristics
// remain as the fallback for sessions whose transcript cannot be located. The
// returned string reports which of the two confirmed the delivery.
func (c *Client) SubmitPrompt(short, text string, goal bool) (string, error) {
	body := buildSubmitBody(text, goal)
	if strings.TrimSpace(body) == "" {
		return "", fmt.Errorf("empty prompt")
	}

	d := c.beginDelivery(short, body)

	// On a native-op error, fall back to the PTY paste path (self-contained: it
	// pastes, settles, and verifies with its own baseline).
	if err := c.Reply(short, body); err != nil {
		return c.submitViaPTY(short, body)
	}

	// Native op acked. Don't trust it: verify the prompt landed, and if it hasn't
	// (the body is sitting as an unsubmitted paste), press Enter to submit it,
	// retrying once before reporting failure.
	if how := c.waitDelivered(short, &d, c.timings().rescue); how != "" {
		return how, nil
	}
	return c.submitEnterUntilStarted(short, &d)
}

// submitEnterUntilStarted presses Enter to submit a prompt still sitting in the
// input box and verifies it landed, retrying once. It is shared by the
// native-reply rescue and the PTY paste path so both converge on the same
// verify+retry-Enter guarantee.
//
// Enter is only sent after the screen has been checked for a dialog. Enter is a
// confirmation keystroke, not a neutral one: whatever holds focus consumes it.
// A resumed session can be sitting on the CLI's startup dialog offering to
// compact the conversation, with the compacting option preselected — an Enter
// aimed at the input box then accepts it instead, so the conversation is
// compacted and the prompt still sitting in the box is discarded, unsent. So a
// dialog on screen ends the delivery with a *DialogBlockedError naming the
// dialog and its options, and the caller decides what to answer.
//
// Nor is Enter sent into a session that is already running a turn. There, the
// prompt is simply queued in the input box and the REPL consumes it when the
// turn ends — nothing is wrong and nothing needs rescuing, so the delivery
// reports confirmedQueued and stops. These are the three states an unstarted
// delivery can be in, and they want opposite responses: answer the dialog, wait
// out the turn, or press Enter. Only the third is handled here by pressing a
// key; the other two are reported so the caller does not have to guess.
//
// A prompt that has already left the input box and is being worked on gets no
// Enter either: it is only waiting to be written down. Whatever has not reached
// the transcript by the end of the confirmation window is reported by
// unconfirmed.
func (c *Client) submitEnterUntilStarted(short string, d *delivery) (string, error) {
	t := c.timings()
	for attempt := 0; attempt < 2; attempt++ {
		if dlg := c.dialogOnScreen(short); dlg.Blocking() {
			return "", &DialogBlockedError{Dialog: dlg}
		}
		if c.midTurn(short, *d) {
			return confirmedQueued, nil
		}
		if c.observe(short, *d).underway() {
			break
		}
		if _, err := c.send(short, []byte("\r"), true, 800*time.Millisecond); err != nil {
			return "", err
		}
		if how := c.waitDelivered(short, d, t.enter); how != "" {
			return how, nil
		}
	}
	if how := c.waitDelivered(short, d, time.Until(d.at.Add(t.window))); how != "" {
		return how, nil
	}
	return c.unconfirmed(short, *d)
}

// unconfirmed classifies a delivery the transcript never confirmed within the
// window. Each verdict asks the caller for something different: nothing
// (queued, or delivered and not yet recorded), Enter having failed, or a
// resubmission once the screen has been read.
func (c *Client) unconfirmed(short string, d delivery) (string, error) {
	st := c.observe(short, d)
	switch {
	case st.scan.Foreign || st.scan.Enqueued:
		return confirmedQueued, nil
	case st.underway():
		return confirmedUnrecorded, nil
	case st.pending || !st.seen:
		return "", errNotStarted
	default:
		return "", errNotDelivered
	}
}

// midTurn reports whether the session is busy running a turn that is not ours,
// which is what makes an undelivered prompt "queued" rather than "stuck".
//
// The roster alone cannot answer this: state=="working" is only reported for
// goal/loop sessions, and an ordinary session runs a turn as state=="running",
// tempo=="active" — indistinguishable from one that has just booted and is
// sitting idle at its prompt. So activity is taken from the transcript instead:
// a running turn appends assistant and tool records continuously, while an idle
// session writes nothing. Conversation records appearing without our prompt
// mean the session is busy with something else; other bookkeeping the CLI
// writes at the start of our own turn does not count.
func (c *Client) midTurn(short string, d delivery) bool {
	if cur, err := c.Resolve(short); err == nil && cur.Busy() {
		return true
	}
	return d.mark.Scan(d.body).Foreign
}

// submitViaPTY is the keystroke-emulation fallback for SubmitPrompt: it types the
// prompt into the session's PTY and verifies the turn started.
//
// The body is delivered as a bracketed paste so multi-line input is treated as
// one literal block (newlines don't submit early). The paste is sent in wait
// mode so the call drains the session's current redraw and only returns once the
// paste has actually landed and the screen is quiet — rather than firing the
// paste off and guessing with a fixed delay. The old fixed 300ms guess could be
// outrun by a long multi-line paste, or by a freshly created session still
// painting its boot UI (create_session submits the moment the prompt appears),
// so the submit Enter raced the not-yet-settled input and the text was left
// sitting unsubmitted in the box — most visible on long /goal pastes, where the
// leading slash also spins up the command menu and adds render latency. After
// the paste has settled a distinct Enter submits it (via submitEnterUntilStarted,
// shared with the native path); the delivery is verified against the transcript
// (falling back to the daemon roster) and Enter is retried once before reporting
// failure, so a session is never left silently idle holding an unsubmitted
// prompt.
func (c *Client) submitViaPTY(short, body string) (string, error) {
	// Deliver as a bracketed paste (the session enables paste mode), so the
	// whole multi-line body lands in the input box without submitting per line.
	// wait=true drains the paste's redraw so we only proceed once it has fully
	// landed — not after a fixed guess that a long paste can outrun.
	paste := append([]byte("\x1b[200~"), body...)
	paste = append(paste, "\x1b[201~"...)
	if _, err := c.send(short, paste, true, 4*time.Second); err != nil {
		return "", err
	}
	// A short extra guard so any residual redraw (e.g. the slash-command menu)
	// settles past the quiet window before the submit keystroke, then snapshot
	// the delivery baseline. It is taken *after* the paste on purpose: the paste
	// is only screen activity, so it writes no transcript record, but it can flip
	// the roster's tempo — a baseline taken before it would let that redraw pass
	// for a started turn.
	time.Sleep(300 * time.Millisecond)
	d := c.beginDelivery(short, body)

	return c.submitEnterUntilStarted(short, &d)
}
