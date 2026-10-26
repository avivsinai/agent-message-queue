package cli

import (
	"reflect"
	"strings"
	"testing"
)

func TestWakeAttentionAlternateScreenAgentKeepsSupportedOSCNotification(t *testing.T) {
	var recorded wakeAttentionEmission
	var written strings.Builder
	cfg := &wakeConfig{
		me:                    "codex",
		attentionEnv:          func(key string) string { return map[string]string{"TERM_PROGRAM": "ghostty"}[key] },
		attentionIsTTY:        func() bool { return true },
		attentionIsForeground: func() bool { return false },
		attentionWrite: func(data []byte) (int, error) {
			return written.Write(data)
		},
		recordAttention: func(emission wakeAttentionEmission) error {
			recorded = emission
			return nil
		},
	}
	payload := wakePayload{
		text:       "AMQ [session1]: safe;notice",
		provenance: wakePayloadPeerHeaders,
	}

	if err := emitWakeAttention(cfg, payload); err != nil {
		t.Fatalf("emit supported OSC attention: %v", err)
	}

	got := written.String()
	if !strings.Contains(got, "\x1b]9;AMQ [session1]: safe,notice\a") {
		t.Fatalf("supported OSC notification missing: %q", got)
	}
	if strings.HasSuffix(got, payload.text+"\n") {
		t.Fatalf("alternate-screen attention appended plain text: %q", got)
	}
	if !reflect.DeepEqual(recorded.Effects, []string{
		wakeAttentionEffectBell,
		wakeAttentionEffectTitle,
		wakeAttentionEffectOSC9,
	}) {
		t.Fatalf("effects = %#v", recorded.Effects)
	}
}

// A wake prints doorbell text into a terminal only as its foreground process
// group and never under coop exec; otherwise only non-printing signals go out.
func TestWakeAttentionTerminalTextNeedsForegroundOwnership(t *testing.T) {
	tests := []struct {
		name       string
		cfg        wakeConfig
		foreground bool
		want       string
	}{
		{
			// field: agy TUI showed staircased doorbell text (2026-10-26).
			// The coop child may share the harness's foreground group.
			name:       "coop-agy",
			cfg:        wakeConfig{me: "agy", harness: "agy", injectMode: wakeInjectModePaste, wakeOwner: &wakeOwner{PID: 1}},
			foreground: true,
			want:       "\x1b]0;AMQ attention\a\a",
		},
		{
			// field: agy TUI showed staircased doorbell text (2026-10-26);
			// review: the COOP.md notify-only recipe runs as a background job
			// behind the Claude composer.
			name:       "background-notify-only-claude",
			cfg:        wakeConfig{me: "claude", injectMode: wakeInjectModeNone, requestedInjectMode: wakeInjectModeNone},
			foreground: false,
			want:       "\x1b]0;AMQ attention\a\a",
		},
		{
			name:       "foreground-notify-only",
			cfg:        wakeConfig{me: "claude", injectMode: wakeInjectModeNone, requestedInjectMode: wakeInjectModeNone},
			foreground: true,
			want:       "\x1b]0;AMQ attention\a\aAMQ [collab]: message from claude - W2\r\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var written strings.Builder
			cfg := tt.cfg
			cfg.attentionEnv = func(string) string { return "" }
			cfg.attentionIsTTY = func() bool { return true }
			cfg.attentionIsForeground = func() bool { return tt.foreground }
			cfg.attentionWrite = func(data []byte) (int, error) { return written.Write(data) }
			payload := wakePayload{
				text:       "AMQ [collab]: message from claude - W2",
				provenance: wakePayloadPeerHeaders,
			}
			if err := emitWakeAttention(&cfg, payload); err != nil {
				t.Fatalf("emit attention: %v", err)
			}
			if got := written.String(); got != tt.want {
				t.Fatalf("terminal attention = %q, want %q", got, tt.want)
			}
		})
	}
}

// review: a foreground --inject-via wake in its own pane lost its failure
// diagnostics, which have no other destination there.
func TestWakeDiagnosticForegroundInjectViaWakePrints(t *testing.T) {
	cfg := &wakeConfig{
		me:                     "pi",
		injectVia:              "/bin/true",
		injectMode:             wakeInjectModeAuto,
		requestedInjectMode:    wakeInjectModeAuto,
		diagnosticIsTTY:        func() bool { return true },
		diagnosticIsForeground: func() bool { return true },
	}
	stderr := captureWakeStderr(t, func() {
		if err := writeWakeDiagnostic(cfg, "amq wake: --inject-via failed: boom\n"); err != nil {
			t.Fatal(err)
		}
	})
	if stderr != "amq wake: --inject-via failed: boom\n" {
		t.Fatalf("foreground diagnostic = %q", stderr)
	}
}

func TestWakeAttentionRedirectedOutputOmitsControls(t *testing.T) {
	var recorded wakeAttentionEmission
	cfg := &wakeConfig{
		me:             "codex",
		attentionIsTTY: func() bool { return false },
		recordAttention: func(emission wakeAttentionEmission) error {
			recorded = emission
			return nil
		},
	}
	payload := wakePayload{
		text:       "peer\x1b]2;spoof\a",
		provenance: wakePayloadPeerHeaders,
	}

	stderr := captureWakeStderr(t, func() {
		if err := emitWakeAttention(cfg, payload); err != nil {
			t.Fatalf("emit redirected attention: %v", err)
		}
	})
	if stderr != "peer ]2;spoof \n" {
		t.Fatalf("redirected output = %q", stderr)
	}
	if !reflect.DeepEqual(recorded.Effects, []string{wakeAttentionEffectOutput}) {
		t.Fatalf("redirected effects = %#v", recorded.Effects)
	}
}

// The held-doorbell notice builds an AppleScript literal: a quote or
// backslash in the body must not end the string, and controls are removed.
func TestAppleScriptStringEscapesQuotesAndBackslashes(t *testing.T) {
	got := appleScriptString("say \"hi\" C:\\tmp\x07\nnext")
	want := `"say \"hi\" C:\\tmp  next"`
	if got != want {
		t.Fatalf("appleScriptString = %s, want %s", got, want)
	}
}
