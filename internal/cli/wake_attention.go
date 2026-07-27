package cli

import (
	"fmt"
	"os"
	"strings"
)

const (
	wakeAttentionEffectOutput = "stderr_output"
	wakeAttentionEffectBell   = "bell"
	wakeAttentionEffectTitle  = "title"
	wakeAttentionEffectOSC9   = "osc9_notification"
	wakeAttentionEffectOSC777 = "osc777_notification"

	wakeAttentionTitle = "AMQ attention"
)

// emitWakeAttention is the non-input destination for a wake notification.
// Effects record only bytes emitted by AMQ; they do not assert that a terminal
// displayed them or that a person observed them.
func emitWakeAttention(cfg *wakeConfig, text string) {
	effects := []string{
		wakeAttentionEffectOutput,
		wakeAttentionEffectBell,
		wakeAttentionEffectTitle,
	}
	var output strings.Builder

	// OSC 0 and a standalone BEL form the portable attention floor. The BEL
	// terminating OSC 0 is not relied on to ring.
	fmt.Fprintf(&output, "\x1b]0;%s\a\a", wakeAttentionTitle)

	safeText := sanitizeForTTY(text)
	oscText := strings.ReplaceAll(safeText, ";", ",")
	switch supportedWakeAttentionNotification(cfg) {
	case wakeAttentionEffectOSC9:
		fmt.Fprintf(&output, "\x1b]9;%s\a", oscText)
		effects = append(effects, wakeAttentionEffectOSC9)
	case wakeAttentionEffectOSC777:
		fmt.Fprintf(&output, "\x1b]777;notify;AMQ;%s\a", oscText)
		effects = append(effects, wakeAttentionEffectOSC777)
	}
	output.WriteString(safeText)
	output.WriteByte('\n')
	_, _ = fmt.Fprint(os.Stderr, output.String())

	if cfg != nil && cfg.recordEffects != nil {
		if err := cfg.recordEffects(effects); err != nil {
			_ = writeStderr("amq wake: record emitted attention effects: %v\n", err)
		}
	}
}

func supportedWakeAttentionNotification(cfg *wakeConfig) string {
	lookup := os.Getenv
	if cfg != nil && cfg.attentionEnv != nil {
		lookup = cfg.attentionEnv
	}
	if lookup("TERM_PROGRAM") == "iTerm.app" {
		return wakeAttentionEffectOSC9
	}
	if isPositiveDecimal(lookup("VTE_VERSION")) {
		return wakeAttentionEffectOSC777
	}
	return ""
}

func isPositiveDecimal(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return strings.TrimLeft(value, "0") != ""
}
