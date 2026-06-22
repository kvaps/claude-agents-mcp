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
	State     string `json:"state"`  // working | blocked | done | ...
	Detail    string `json:"detail"` // live human-readable status
	Intent    string `json:"intent"`
	Name      string `json:"name"`
	Agent     string `json:"agent"`
	Needs     string `json:"needs"` // what a blocked session is waiting on
	StartedAt int64  `json:"startedAt"`
	Live      bool   `json:"live"` // true if a live daemon worker (attachable); false for not-running sessions
}

// Busy reports whether the session is actively working (so input should wait).
func (s Session) Busy() bool {
	return s.Tempo == "active" || s.State == "working"
}
