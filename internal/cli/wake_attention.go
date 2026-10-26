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

type wakeAttentionDeliveryError struct {
	written int
	total   int
	err     error
}

func (err *wakeAttentionDeliveryError) Error() string {
	return fmt.Sprintf(
		"output attention write failed after %d/%d bytes: %v",
		err.written,
		err.total,
		err.err,
	)
}

func (err *wakeAttentionDeliveryError) Unwrap() error {
	return err.err
}

// writeWakeDiagnostic keeps human-readable diagnostics in a log destination
// (the coop .wake.log). A terminal gets them only under wakeMayPrintToTerminal;
// otherwise a harness TUI owns that terminal and they are dropped.
func writeWakeDiagnostic(cfg *wakeConfig, format string, args ...any) error {
	if wakeDiagnosticIsTerminal(cfg) &&
		!wakeMayPrintToTerminal(cfg, wakeDiagnosticIsForeground) {
		return nil
	}
	return writeStderr(format, args...)
}

func wakeDiagnosticIsTerminal(cfg *wakeConfig) bool {
	if cfg != nil && cfg.diagnosticIsTTY != nil {
		return cfg.diagnosticIsTTY()
	}
	return term.IsTerminal(int(os.Stderr.Fd()))
}

// emitWakeAttention is the non-input destination for a wake notification.
// Effects record only a complete write by AMQ; they do not assert that a
// terminal rendered them or that a person observed them.
func emitWakeAttention(cfg *wakeConfig, payload wakePayload) error {
	isTerminal := wakeAttentionIsTerminal(cfg)
	writePlainOutput := !isTerminal || wakeMayPrintToTerminal(cfg, wakeAttentionIsForeground)
	var effects []string
	if writePlainOutput {
		effects = append(effects, wakeAttentionEffectOutput)
	}
	var output strings.Builder

	safeText := sanitizeForTTY(payload.text)
	if isTerminal {
		effects = append(effects, wakeAttentionEffectBell, wakeAttentionEffectTitle)
		// OSC 0 and a standalone BEL form the portable terminal-attention floor.
		// The BEL terminating OSC 0 is not relied on to ring. Plain text goes to
		// a terminal only under wakeMayPrintToTerminal: otherwise a harness TUI
		// owns the screen and printed text lands in it.
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
	if writePlainOutput {
		output.WriteString(safeText)
		if isTerminal {
			// A terminal in raw mode does not map LF to CRLF.
			output.WriteByte('\r')
		}
		output.WriteByte('\n')
	}

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
		_ = writeWakeDiagnostic(cfg, "amq wake: output attention write failed after %d/%d bytes: %v\n", n, len(data), err)
		return &wakeAttentionDeliveryError{
			written: n,
			total:   len(data),
			err:     err,
		}
	}

	if cfg != nil && cfg.recordAttention != nil {
		emission := wakeAttentionEmission{
			Effects:          append([]string(nil), effects...),
			OutputProvenance: payload.provenance,
		}
		if err := cfg.recordAttention(emission); err != nil {
			_ = writeWakeDiagnostic(cfg, "amq wake: record output attention write: %v\n", err)
		}
	}
	return nil
}

// wakeMayPrintToTerminal reports whether the wake may print text to a
// terminal destination: only as that terminal's foreground process group, so
// a wake running behind a harness (a background job, or a coop child that the
// harness replaced in the foreground) never prints into its screen. A wake
// under coop exec never prints, because it may share the harness's group.
func wakeMayPrintToTerminal(cfg *wakeConfig, isForeground func(*wakeConfig) bool) bool {
	return (cfg == nil || cfg.wakeOwner == nil) && isForeground(cfg)
}

func wakeAttentionIsForeground(cfg *wakeConfig) bool {
	if cfg != nil && cfg.attentionIsForeground != nil {
		return cfg.attentionIsForeground()
	}
	return wakeFileIsForeground(os.Stderr)
}

func wakeDiagnosticIsForeground(cfg *wakeConfig) bool {
	if cfg != nil && cfg.diagnosticIsForeground != nil {
		return cfg.diagnosticIsForeground()
	}
	return wakeFileIsForeground(os.Stderr)
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
	// Keep this as a verified-upstream allowlist. Unknown terminals receive the
	// portable bell/title floor plus plain output when it is safe for the target.
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
