package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

const (
	wakeAttentionEffectOutput = "stderr_output_written"
	wakeAttentionEffectBell   = "bell_byte_written"
	wakeAttentionEffectTitle  = "title_sequence_written"
	wakeAttentionEffectOSC9   = "osc9_sequence_written"
	wakeAttentionEffectOSC777 = "osc777_sequence_written"

	wakeAttentionTitle = "AMQ attention"
)

type wakeAttentionEmission struct {
	Effects          []string
	OutputProvenance wakePayloadProvenance
}

// emitWakeAttention is the non-input destination for a wake notification.
// Effects record only a complete write by AMQ; they do not assert that a
// terminal rendered them or that a person observed them.
func emitWakeAttention(cfg *wakeConfig, payload wakePayload) {
	if cfg != nil && cfg.suppressAttention {
		return
	}
	effects := []string{wakeAttentionEffectOutput}
	var output strings.Builder

	safeText := sanitizeForTTY(payload.text)
	if wakeAttentionIsTerminal(cfg) {
		effects = append(effects, wakeAttentionEffectBell, wakeAttentionEffectTitle)
		// OSC 0 and a standalone BEL form the portable terminal-attention floor.
		// The BEL terminating OSC 0 is not relied on to ring.
		fmt.Fprintf(&output, "\x1b]0;%s\a\a", wakeAttentionTitle)

		oscText := strings.ReplaceAll(safeText, ";", ",")
		switch supportedWakeAttentionNotification(cfg) {
		case wakeAttentionEffectOSC9:
			fmt.Fprintf(&output, "\x1b]9;%s\a", oscText)
			effects = append(effects, wakeAttentionEffectOSC9)
		case wakeAttentionEffectOSC777:
			fmt.Fprintf(&output, "\x1b]777;notify;AMQ;%s\a", oscText)
			effects = append(effects, wakeAttentionEffectOSC777)
		}
	}
	output.WriteString(safeText)
	output.WriteByte('\n')

	write := func(data []byte) (int, error) {
		return os.Stderr.Write(data)
	}
	if cfg != nil && cfg.attentionWrite != nil {
		write = cfg.attentionWrite
	}
	data := []byte(output.String())
	n, err := write(data)
	if err != nil || n != len(data) {
		if err == nil {
			err = io.ErrShortWrite
		}
		_ = writeStderr("amq wake: output attention write failed after %d/%d bytes: %v\n", n, len(data), err)
		return
	}

	if cfg != nil && cfg.recordAttention != nil {
		emission := wakeAttentionEmission{
			Effects:          append([]string(nil), effects...),
			OutputProvenance: payload.provenance,
		}
		if err := cfg.recordAttention(emission); err != nil {
			_ = writeStderr("amq wake: record output attention write: %v\n", err)
		}
	}
}

func wakeAttentionIsTerminal(cfg *wakeConfig) bool {
	if cfg != nil && cfg.attentionIsTTY != nil {
		return cfg.attentionIsTTY()
	}
	return term.IsTerminal(int(os.Stderr.Fd()))
}

func supportedWakeAttentionNotification(cfg *wakeConfig) string {
	lookup := os.Getenv
	if cfg != nil && cfg.attentionEnv != nil {
		lookup = cfg.attentionEnv
	}
	// Keep this as a verified-upstream allowlist. Unknown terminals
	// deliberately receive only the portable bell/title/output floor.
	switch lookup("TERM_PROGRAM") {
	case "iTerm.app", "ghostty":
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
