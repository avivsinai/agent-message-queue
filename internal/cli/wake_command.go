package cli

import (
	"github.com/avivsinai/agent-message-queue/internal/harness"
	"github.com/avivsinai/agent-message-queue/internal/wakecmd"
)

// serveWakeCommands types the owner's oldest queued slash command (wakecmd)
// into the terminal, as its own line followed by Enter, by the
// doorbell's injection mechanism and mode. A command waits, queued and never
// dropped, while a dialog may be open or the terminal still owes earlier
// input; the harness's composer queues typed input during a turn, so a turn
// in progress does not hold it. A seat that cannot take commands answers
// each one Refused.
func serveWakeCommands(cfg *wakeConfig) {
	if cfg.commandDir == "" {
		return
	}
	reqs, err := wakecmd.Pending(cfg.commandDir)
	if err != nil {
		_ = writeWakeDiagnostic(cfg, "amq wake: read the command queue: %v\n", err)
		return
	}
	if len(reqs) == 0 {
		return
	}
	if why := wakeCommandRefusal(cfg); why != "" {
		for _, req := range reqs {
			if wakecmd.Claim(cfg.commandDir, req.ID) {
				wakecmd.Finish(cfg.commandDir, req.ID, wakecmd.Refused, why)
			}
		}
		return
	}
	for i, req := range reqs {
		if why := wakeCommandWait(cfg); why != "" {
			for _, later := range reqs[i:] {
				wakecmd.Mark(cfg.commandDir, later.ID, wakecmd.Queued, why)
			}
			return
		}
		if !wakecmd.Claim(cfg.commandDir, req.ID) {
			continue
		}
		notice := wakeNotification{
			input:  wakePayload{text: req.Text, provenance: wakePayloadOwnerCommand},
			output: wakePayload{text: "AMQ: the owner's command " + req.Text, provenance: wakePayloadSystemFixed},
		}
		deliveryErr := deliverWakeNotification(cfg, notice, true)
		switch {
		case wakeInputAttemptConfirmed(cfg, deliveryErr):
			wakecmd.Finish(cfg.commandDir, req.ID, wakecmd.Typed, "")
			// One command per tick: the next one is typed after the
			// harness has read this one.
			for _, later := range reqs[i+1:] {
				wakecmd.Mark(cfg.commandDir, later.ID, wakecmd.Queued, "an earlier command is being typed")
			}
			return
		case deliveryErr == nil && cfg.lastAttemptTransientAttention,
			cfg.lastInjectorOutcome == wakeInjectorOutcomeDeferred:
			wakecmd.Release(cfg.commandDir, req.ID)
			wakecmd.Mark(cfg.commandDir, req.ID, wakecmd.Queued, "the terminal is in use")
			return
		default:
			why := "the wake could not type into the terminal"
			if deliveryErr != nil {
				why += " (" + deliveryErr.Error() + ")"
			}
			_ = writeWakeDiagnostic(cfg, "amq wake: owner command not typed: %s\n", why)
			wakecmd.Finish(cfg.commandDir, req.ID, wakecmd.Refused, why)
			return
		}
	}
}

// wakeCommandRefusal says why this seat takes no commands, or "".
func wakeCommandRefusal(cfg *wakeConfig) string {
	switch {
	case cfg.injectMode == wakeInjectModeNone:
		return "the wake's inject mode is none, so it types nothing"
	case cfg.injectMode == wakeInjectModeClaudeNative:
		return "the wake delivers natively and types nothing"
	case cfg.harness != harness.Claude:
		return "only a Claude Code seat takes commands from Buzz"
	}
	return ""
}

// wakeCommandWait says why the next command must wait, or "".
func wakeCommandWait(cfg *wakeConfig) string {
	switch {
	case cfg.inputRecoveryRequired || cfg.inputDelivery.pending():
		return "the terminal still owes earlier input"
	}
	if state, _ := cfg.dialogState(); state != wakeDialogClosed {
		return "a dialog is open in the terminal"
	}
	return ""
}
