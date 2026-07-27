package cli

import (
	"errors"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func testWakeNotification(input, output string, provenance wakePayloadProvenance) wakeNotification {
	return wakeNotification{
		input: wakePayload{
			text:       input,
			provenance: wakePayloadSystemFixed,
		},
		output: wakePayload{
			text:       output,
			provenance: provenance,
		},
	}
}

func testOutputAttentionConfig(recorded *wakeAttentionEmission) *wakeConfig {
	return &wakeConfig{
		attentionEnv:   func(string) string { return "" },
		attentionIsTTY: func() bool { return true },
		recordAttention: func(emission wakeAttentionEmission) error {
			*recorded = emission
			return nil
		},
	}
}

func TestDeliverWakeNotificationSuccessUsesOnlyInput(t *testing.T) {
	var writes []string
	cfg := &wakeConfig{
		injectMode: wakeInjectModePaste,
		terminalWrite: func(chunk string) error {
			writes = append(writes, chunk)
			return nil
		},
	}
	notice := testWakeNotification(
		coopWakeDoorbell,
		"AMQ: message from peer - output only",
		wakePayloadPeerHeaders,
	)

	stderr := captureWakeStderr(t, func() {
		if err := deliverWakeNotification(cfg, notice, false); err != nil {
			t.Fatalf("deliverWakeNotification: %v", err)
		}
	})

	if stderr != "" {
		t.Fatalf("successful input delivery wrote output: %q", stderr)
	}
	got := strings.Join(writes, "|")
	if !strings.Contains(got, coopWakeDoorbell) {
		t.Fatalf("input chunks = %#v, missing fixed doorbell", writes)
	}
	if strings.Contains(got, "peer") || strings.Contains(got, "output only") {
		t.Fatalf("peer-derived output entered terminal input: %#v", writes)
	}
}

func TestDeliverWakeNotificationNonInputOutcomesUseOnlyOutput(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, *wakeConfig)
	}{
		{
			name: "none",
			setup: func(_ *testing.T, cfg *wakeConfig) {
				cfg.injectMode = wakeInjectModeNone
			},
		},
		{
			name: "max hold",
			setup: func(t *testing.T, cfg *wakeConfig) {
				old := waitForWakeInputQuiet
				waitForWakeInputQuiet = func(*wakeConfig) bool { return false }
				t.Cleanup(func() { waitForWakeInputQuiet = old })
				cfg.injectMode = wakeInjectModeRaw
				cfg.deferWhileInput = true
			},
		},
		{
			name: "unsupported",
			setup: func(_ *testing.T, cfg *wakeConfig) {
				cfg.injectMode = wakeInjectModeRaw
				cfg.terminalWrite = func(string) error {
					return newWakeInjectorUnsupportedError(syscall.EIO)
				}
			},
		},
		{
			name: "generic failure",
			setup: func(t *testing.T, cfg *wakeConfig) {
				cfg.injectMode = wakeInjectModeRaw
				stubTIOCSTIInject(t, func(string) error {
					return errors.New("terminal write failed")
				})
			},
		},
		{
			name: "inject via failure",
			setup: func(_ *testing.T, cfg *wakeConfig) {
				cfg.injectVia = "/nonexistent/amq-output-attention-injector"
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var emission wakeAttentionEmission
			cfg := testOutputAttentionConfig(&emission)
			tc.setup(t, cfg)
			notice := testWakeNotification(
				coopWakeDoorbell,
				"AMQ: message from peer - output only",
				wakePayloadPeerHeaders,
			)

			stderr := captureWakeStderr(t, func() {
				if err := deliverWakeNotification(cfg, notice, true); err != nil {
					t.Fatalf("deliverWakeNotification: %v", err)
				}
			})

			if !strings.Contains(stderr, "message from peer - output only") {
				t.Fatalf("fallback output = %q, missing peer output", stderr)
			}
			if strings.Contains(stderr, coopWakeDoorbell) {
				t.Fatalf("fallback output leaked input payload: %q", stderr)
			}
			if emission.OutputProvenance != wakePayloadPeerHeaders {
				t.Fatalf("output provenance = %q, want peer headers", emission.OutputProvenance)
			}
		})
	}
}

func TestDeliverWakeNotificationOwnerBoundForcesOnlyInput(t *testing.T) {
	stop := make(chan struct{})
	var writes []string
	cfg := &wakeConfig{
		injectMode:  wakeInjectModePaste,
		controlStop: stop,
		beforeTerminalWrite: func() error {
			return nil
		},
		terminalWrite: func(chunk string) error {
			writes = append(writes, chunk)
			return nil
		},
	}
	notice := wakeNotification{
		input: wakePayload{
			text:       "operator input",
			provenance: wakePayloadOperatorFlag,
		},
		output: wakePayload{
			text:       "operator output",
			provenance: wakePayloadOperatorFlag,
		},
	}

	if err := deliverWakeNotification(cfg, notice, false); err != nil {
		t.Fatalf("deliverWakeNotification: %v", err)
	}
	if got := strings.Join(writes, "|"); !strings.Contains(got, coopWakeDoorbell) ||
		strings.Contains(got, "operator input") ||
		strings.Contains(got, "operator output") {
		t.Fatalf("owner-bound input chunks = %#v", writes)
	}

	cfg.injectMode = wakeInjectModeNone
	var emission wakeAttentionEmission
	cfg.attentionEnv = func(string) string { return "" }
	cfg.attentionIsTTY = func() bool { return true }
	cfg.recordAttention = func(got wakeAttentionEmission) error {
		emission = got
		return nil
	}
	stderr := captureWakeStderr(t, func() {
		if err := deliverWakeNotification(cfg, notice, false); err != nil {
			t.Fatalf("none-mode deliverWakeNotification: %v", err)
		}
	})
	if !strings.Contains(stderr, "operator output") || strings.Contains(stderr, coopWakeDoorbell) {
		t.Fatalf("owner-bound output = %q", stderr)
	}
	if emission.OutputProvenance != wakePayloadOperatorFlag {
		t.Fatalf("output provenance = %q, want operator", emission.OutputProvenance)
	}

	emission = wakeAttentionEmission{}
	peerNotice := peerWakeNotification("peer output")
	stderr = captureWakeStderr(t, func() {
		if err := deliverWakeNotification(cfg, peerNotice, false); err != nil {
			t.Fatalf("peer none-mode deliverWakeNotification: %v", err)
		}
	})
	if !strings.Contains(stderr, "peer output") || strings.Contains(stderr, coopWakeDoorbell) {
		t.Fatalf("owner-bound peer output = %q", stderr)
	}
	if emission.OutputProvenance != wakePayloadPeerHeaders {
		t.Fatalf("peer output provenance = %q, want peer headers", emission.OutputProvenance)
	}
}

func TestDeliverWakeNotificationUnsupportedDemotionUsesEachCurrentOutput(t *testing.T) {
	writes := 0
	var provenances []wakePayloadProvenance
	cfg := &wakeConfig{
		injectMode: wakeInjectModeRaw,
		terminalWrite: func(string) error {
			writes++
			return newWakeInjectorUnsupportedError(syscall.EIO)
		},
		attentionIsTTY: func() bool { return false },
		recordAttention: func(emission wakeAttentionEmission) error {
			provenances = append(provenances, emission.OutputProvenance)
			return nil
		},
	}

	first := captureWakeStderr(t, func() {
		if err := deliverWakeNotification(
			cfg,
			peerWakeNotification("first peer output"),
			false,
		); err != nil {
			t.Fatalf("first delivery: %v", err)
		}
	})
	second := captureWakeStderr(t, func() {
		if err := deliverWakeNotification(
			cfg,
			peerWakeNotification("second peer output"),
			false,
		); err != nil {
			t.Fatalf("second delivery: %v", err)
		}
	})

	if writes != 1 {
		t.Fatalf("terminal writes = %d, want one failed attempt before demotion", writes)
	}
	if !strings.Contains(first, "first peer output") ||
		strings.Contains(first, "second peer output") {
		t.Fatalf("first output = %q", first)
	}
	if !strings.Contains(second, "second peer output") ||
		strings.Contains(second, "first peer output") {
		t.Fatalf("second output = %q", second)
	}
	if !reflect.DeepEqual(provenances, []wakePayloadProvenance{
		wakePayloadPeerHeaders,
		wakePayloadPeerHeaders,
	}) {
		t.Fatalf("output provenances = %#v", provenances)
	}
}

func TestWakeAttentionReportsOnlyFullyWrittenOutputEffectsAndProvenance(t *testing.T) {
	payload := wakePayload{
		text:       "safe notice",
		provenance: wakePayloadPeerHeaders,
	}
	var recorded wakeAttentionEmission
	cfg := testOutputAttentionConfig(&recorded)

	stderr := captureWakeStderr(t, func() {
		emitWakeAttention(cfg, payload)
	})
	for _, want := range []string{
		"\x1b]0;AMQ attention\a",
		"\a",
		"safe notice\n",
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr %q does not contain %q", stderr, want)
		}
	}
	if recorded.OutputProvenance != wakePayloadPeerHeaders {
		t.Fatalf("provenance = %q, want peer headers", recorded.OutputProvenance)
	}
	wantEffects := []string{
		wakeAttentionEffectOutput,
		wakeAttentionEffectBell,
		wakeAttentionEffectTitle,
	}
	if !reflect.DeepEqual(recorded.Effects, wantEffects) {
		t.Fatalf("effects = %#v, want %#v", recorded.Effects, wantEffects)
	}

	recorded = wakeAttentionEmission{}
	cfg.attentionWrite = func(data []byte) (int, error) {
		return len(data) - 1, nil
	}
	stderr = captureWakeStderr(t, func() {
		emitWakeAttention(cfg, payload)
	})
	if recorded.OutputProvenance != "" || recorded.Effects != nil {
		t.Fatalf("partial write recorded emission: %#v", recorded)
	}
	if !strings.Contains(stderr, "output attention write failed") {
		t.Fatalf("partial-write diagnostic missing: %q", stderr)
	}
}

func TestWakeAttentionRedirectedOutputOmitsControls(t *testing.T) {
	var recorded wakeAttentionEmission
	cfg := &wakeConfig{
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
		emitWakeAttention(cfg, payload)
	})
	if stderr != "peer ]2;spoof \n" {
		t.Fatalf("redirected output = %q", stderr)
	}
	if !reflect.DeepEqual(recorded.Effects, []string{wakeAttentionEffectOutput}) {
		t.Fatalf("redirected effects = %#v", recorded.Effects)
	}
}

func TestWakeAttentionOptionalOSCRequiresPositiveTerminalSupport(t *testing.T) {
	tests := []struct {
		name       string
		env        map[string]string
		wantEffect string
		wantBytes  string
	}{
		{
			name: "unknown terminal",
			env:  map[string]string{"TERM_PROGRAM": "unknown"},
		},
		{
			name:       "iterm osc9",
			env:        map[string]string{"TERM_PROGRAM": "iTerm.app"},
			wantEffect: wakeAttentionEffectOSC9,
			wantBytes:  "\x1b]9;safe,notice\a",
		},
		{
			name:       "ghostty osc9",
			env:        map[string]string{"TERM_PROGRAM": "ghostty"},
			wantEffect: wakeAttentionEffectOSC9,
			wantBytes:  "\x1b]9;safe,notice\a",
		},
		{
			name:       "vte osc777",
			env:        map[string]string{"VTE_VERSION": "7600"},
			wantEffect: wakeAttentionEffectOSC777,
			wantBytes:  "\x1b]777;notify;AMQ;safe,notice\a",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var recorded wakeAttentionEmission
			cfg := &wakeConfig{
				attentionIsTTY: func() bool { return true },
				attentionEnv: func(key string) string {
					return tc.env[key]
				},
				recordAttention: func(emission wakeAttentionEmission) error {
					recorded = emission
					return nil
				},
			}
			stderr := captureWakeStderr(t, func() {
				emitWakeAttention(cfg, wakePayload{
					text:       "safe;notice",
					provenance: wakePayloadPeerHeaders,
				})
			})
			if tc.wantBytes == "" {
				if strings.Contains(stderr, "\x1b]9;") ||
					strings.Contains(stderr, "\x1b]777;") {
					t.Fatalf("unknown terminal received optional OSC: %q", stderr)
				}
				for _, effect := range recorded.Effects {
					if effect == wakeAttentionEffectOSC9 ||
						effect == wakeAttentionEffectOSC777 {
						t.Fatalf("unknown terminal recorded optional OSC effect: %#v", recorded.Effects)
					}
				}
			} else if !strings.Contains(stderr, tc.wantBytes) {
				t.Fatalf("stderr %q does not contain %q", stderr, tc.wantBytes)
			}
			if tc.wantEffect != "" &&
				recorded.Effects[len(recorded.Effects)-1] != tc.wantEffect {
				t.Fatalf("effects = %#v, want final %q", recorded.Effects, tc.wantEffect)
			}
		})
	}
}

func TestWakeAttentionDoesNotInferOperatorProvenanceFromPayloadText(t *testing.T) {
	var recorded wakeAttentionEmission
	cfg := &wakeConfig{
		attentionIsTTY: func() bool { return false },
		recordAttention: func(emission wakeAttentionEmission) error {
			recorded = emission
			return nil
		},
	}
	payload := wakePayload{
		text:       coopWakeDoorbell,
		provenance: wakePayloadOperatorFlag,
	}

	stderr := captureWakeStderr(t, func() {
		emitWakeAttention(cfg, payload)
	})
	if stderr != coopWakeDoorbell+"\n" {
		t.Fatalf("operator output was content-normalized: %q", stderr)
	}
	if recorded.OutputProvenance != wakePayloadOperatorFlag {
		t.Fatalf("provenance = %q, want operator", recorded.OutputProvenance)
	}
}
