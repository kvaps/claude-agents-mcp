package agents

// Session is one background agent session as reported by the daemon control
// protocol (op:list).
type Session struct {
	Short     string `json:"short"`
	SessionID string `json:"sessionId"`
	PID       int    `json:"pid"`
	Cwd       string `json:"cwd"`
	Backend   string `json:"backend"`
	Tempo     string `json:"tempo"`  // idle | active | blocked
	State     string `json:"state"`  // running | working | blocked | done
	Detail    string `json:"detail"` // live human-readable status
	Intent    string `json:"intent"`
	Name      string `json:"name"`
	Agent     string `json:"agent"`
	Needs     string `json:"needs"` // what a blocked session is waiting on
	StartedAt int64  `json:"startedAt"`
	Live      bool   `json:"live"`      // true if a live daemon worker (attachable); false for not-running sessions
	Resumable bool   `json:"resumable"` // for a not-running session: true when it can be resumed in place (exited-but-resumable, vs really dead); always false while live
	Pinned    bool   `json:"pinned"`    // true if the session is pinned in the agents view (ctrl+t)
}

// Busy reports whether the session is actively processing a turn, so input
// should wait. It keys off state, not tempo: tempo=="active" only means the
// session produced output recently and is set even when it is idle at the
// prompt — a freshly-booted session sits there with tempo=active/state=running,
// and a just-finished one reports tempo=active/state=done. Neither is busy;
// only state=="working" means a turn is actually in flight.
func (s Session) Busy() bool {
	return s.State == "working"
}

// Resuming reports whether the session is still replaying its history after a
// `--bg --resume` and is not yet ready to accept input. A large session can sit
// in this state for several seconds; attaching during this window races with the
// worker finishing its boot.
func (s Session) Resuming() bool {
	return s.State == "resuming"
}

// Crashed reports whether the worker is flagged crashed. This can show up
// transiently while a resumed worker replays/runs its first turn and then
// recovers, so it is treated as a not-yet-usable state to wait through, not as a
// terminal failure (only leaving the roster — being retired — is terminal).
func (s Session) Crashed() bool {
	return s.State == "crashed"
}

// Usable reports whether the session is in a normal, attachable state — booted
// and not in a transient startup state (resuming/crashed). A resume is only
// trustworthy once the worker reaches and holds a usable state.
func (s Session) Usable() bool {
	return !s.Resuming() && !s.Crashed() && s.State != ""
}
