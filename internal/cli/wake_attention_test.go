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
		me:             "codex",
		attentionEnv:   func(key string) string { return map[string]string{"TERM_PROGRAM": "ghostty"}[key] },
		attentionIsTTY: func() bool { return true },
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

// field: agy TUI showed staircased doorbell text (2026-10-26). A coop wake
// shares the harness terminal, whatever the harness, so only non-printing
// signals reach it.
func TestWakeAttentionCoopHarnessTerminalWritesNoText(t *testing.T) {
	var recorded wakeAttentionEmission
	var written strings.Builder
	cfg := &wakeConfig{
		me:             "agy",
		harness:        "agy",
		injectMode:     wakeInjectModePaste,
		wakeOwner:      &wakeOwner{PID: 1},
		attentionEnv:   func(string) string { return "" },
		attentionIsTTY: func() bool { return true },
		attentionWrite: func(data []byte) (int, error) {
			return written.Write(data)
		},
		recordAttention: func(emission wakeAttentionEmission) error {
			recorded = emission
			return nil
		},
	}
	payload := wakePayload{
		text:       "AMQ [collab]: message from claude - W2",
		provenance: wakePayloadPeerHeaders,
	}

	if err := emitWakeAttention(cfg, payload); err != nil {
		t.Fatalf("emit coop attention: %v", err)
	}

	if got := written.String(); got != "\x1b]0;AMQ attention\a\a" {
		t.Fatalf("coop harness terminal attention = %q, want title + bell only", got)
	}
	if !reflect.DeepEqual(recorded.Effects, []string{
		wakeAttentionEffectBell,
		wakeAttentionEffectTitle,
	}) {
		t.Fatalf("effects = %#v", recorded.Effects)
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
