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
	Live      bool   `json:"live"` // true if a live daemon worker (attachable); false for not-running sessions
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
