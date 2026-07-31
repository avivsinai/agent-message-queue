package cli

import "github.com/avivsinai/agent-message-queue/internal/fsq"

type doctorResultV2 struct {
	Checks               []doctorCheck               `json:"checks"`
	Mailboxes            []fsq.MailboxInspection     `json:"mailboxes,omitempty"`
	MailboxRepair        *fsq.MailboxRepairResult    `json:"mailbox_repair,omitempty"`
	ExtensionManifests   []doctorExtensionManifest   `json:"extension_manifests,omitempty"`
	ExtensionDiagnostics []doctorExtensionDiagnostic `json:"extension_diagnostics,omitempty"`
	Summary              doctorSummaryV2             `json:"summary"`
	Ops                  *doctorOpsResultV2          `json:"ops,omitempty"`
}

type doctorSummaryV2 struct {
	OK    int `json:"ok"`
	Warn  int `json:"warn"`
	Error int `json:"error"`
}

type doctorOpsResultV2 struct {
	Root         opsRoot          `json:"root"`
	Agents       []opsAgent       `json:"agents"`
	OperatorGate *opsOperatorGate `json:"operator_gate,omitempty"`
	WakeLocks    []opsWakeLockV2  `json:"wake_locks,omitempty"`
	Hints        []opsHint        `json:"hints"`
}

type opsWakeLockV2 struct {
	Status        string             `json:"status"`
	Agent         string             `json:"agent"`
	Root          string             `json:"root"`
	Lock          string             `json:"lock"`
	PID           int                `json:"pid,omitempty"`
	TTY           string             `json:"tty,omitempty"`
	Started       string             `json:"started,omitempty"`
	Reason        string             `json:"reason,omitempty"`
	Removed       bool               `json:"removed,omitempty"`
	Target        string             `json:"target,omitempty"`
	TargetPresent bool               `json:"target_present,omitempty"`
	TargetReason  string             `json:"target_reason,omitempty"`
	WakeCheck     *wakeCheckResultV2 `json:"wake_check"`
}

func renderDoctorResultV2(result doctorResult) doctorResultV2 {
	v2 := doctorResultV2{
		Checks:               result.Checks,
		Mailboxes:            result.Mailboxes,
		MailboxRepair:        result.MailboxRepair,
		ExtensionManifests:   result.ExtensionManifests,
		ExtensionDiagnostics: result.ExtensionDiagnostics,
		Summary: doctorSummaryV2{
			OK:    result.Summary.OK,
			Warn:  result.Summary.Warn,
			Error: result.Summary.Error,
		},
	}
	if result.Ops == nil {
		return v2
	}
	v2.Ops = &doctorOpsResultV2{
		Root:         result.Ops.Root,
		Agents:       result.Ops.Agents,
		OperatorGate: result.Ops.OperatorGate,
		Hints:        result.Ops.Hints,
		WakeLocks:    make([]opsWakeLockV2, 0, len(result.Ops.WakeLocks)),
	}
	for _, lock := range result.Ops.WakeLocks {
		v2Lock := opsWakeLockV2{
			Status:        lock.Status,
			Agent:         lock.Agent,
			Root:          lock.Root,
			Lock:          lock.Lock,
			PID:           lock.PID,
			TTY:           lock.TTY,
			Started:       lock.Started,
			Reason:        lock.Reason,
			Removed:       lock.Removed,
			Target:        lock.Target,
			TargetPresent: lock.TargetPresent,
			TargetReason:  lock.TargetReason,
		}
		if lock.WakeCheckDecision != nil {
			wakeCheck := renderWakeCheckV2(*lock.WakeCheckDecision)
			v2Lock.WakeCheck = &wakeCheck
		}
		v2.Ops.WakeLocks = append(v2.Ops.WakeLocks, v2Lock)
	}
	return v2
}
