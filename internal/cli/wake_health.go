package cli

// wakeHealthDecision describes a response from the notification loop, separate
// from the operating system's proof that the lock's process still exists.
type wakeHealthDecision struct {
	Status string `json:"status"`
	Detail string `json:"detail"`
}

type wakeHealthRequest struct {
	reply chan string
}

const (
	wakeHealthSchema       = 1
	wakeHealthHealthy      = "healthy"
	wakeHealthDegraded     = "degraded"
	wakeHealthUnknown      = "unknown"
	wakeHealthUnresponsive = "unresponsive"
)
