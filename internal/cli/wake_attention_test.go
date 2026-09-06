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
