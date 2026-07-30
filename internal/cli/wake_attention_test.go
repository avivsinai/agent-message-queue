package cli

import (
	"errors"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
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
		name        string
		setup       func(*testing.T, *wakeConfig)
		wantBlocked bool
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
			name:        "generic failure with uncertain acceptance",
			wantBlocked: true,
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

			var deliveryErr error
			stderr := captureWakeStderr(t, func() {
				deliveryErr = deliverWakeNotification(cfg, notice, true)
			})

			if tc.wantBlocked {
				var blocked *wakeInputDemotionBlockedError
				if !errors.As(deliveryErr, &blocked) {
					t.Fatalf("delivery error = %v, want blocked uncertain-input demotion", deliveryErr)
				}
				if strings.Contains(stderr, "message from peer - output only") {
					t.Fatalf("uncertain input emitted output fallback: %q", stderr)
				}
				if !strings.Contains(stderr, "terminal input acceptance is uncertain") {
					t.Fatalf("uncertain input diagnostic missing: %q", stderr)
				}
				if emission.OutputProvenance != "" || emission.Effects != nil {
					t.Fatalf("uncertain input recorded output delivery: %#v", emission)
				}
				return
			}
			if deliveryErr != nil {
				t.Fatalf("deliverWakeNotification: %v", deliveryErr)
			}
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
		if err := emitWakeAttention(cfg, payload); err != nil {
			t.Fatalf("emit complete attention: %v", err)
		}
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
	var writeErr error
	stderr = captureWakeStderr(t, func() {
		writeErr = emitWakeAttention(cfg, payload)
	})
	var deliveryErr *wakeAttentionDeliveryError
	if !errors.As(writeErr, &deliveryErr) {
		t.Fatalf("partial write error = %v, want typed attention delivery failure", writeErr)
	}
	if recorded.OutputProvenance != "" || recorded.Effects != nil {
		t.Fatalf("partial write recorded emission: %#v", recorded)
	}
	if !strings.Contains(stderr, "output attention write failed") {
		t.Fatalf("partial-write diagnostic missing: %q", stderr)
	}

	sinkErr := errors.New("attention sink unavailable")
	cfg.attentionWrite = func([]byte) (int, error) {
		return 0, sinkErr
	}
	stderr = captureWakeStderr(t, func() {
		writeErr = emitWakeAttention(cfg, payload)
	})
	if !errors.As(writeErr, &deliveryErr) || !errors.Is(writeErr, sinkErr) {
		t.Fatalf("failed write error = %v, want typed attention failure wrapping sink error", writeErr)
	}
	if recorded.OutputProvenance != "" || recorded.Effects != nil {
		t.Fatalf("failed write recorded emission: %#v", recorded)
	}
	if !strings.Contains(stderr, "attention sink unavailable") {
		t.Fatalf("failed-write diagnostic missing: %q", stderr)
	}
}

func TestWakeAttentionAlternateScreenAgentOmitsPlainTerminalOutput(t *testing.T) {
	for _, me := range []string{"codex", "codex2", "claude"} {
		t.Run(me, func(t *testing.T) {
			var recorded wakeAttentionEmission
			var written strings.Builder
			cfg := &wakeConfig{
				me:             me,
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
				text:       "AMQ [session1]: message from peer",
				provenance: wakePayloadPeerHeaders,
			}

			if err := emitWakeAttention(cfg, payload); err != nil {
				t.Fatalf("emit alternate-screen attention: %v", err)
			}

			if got := written.String(); got != "\x1b]0;AMQ attention\a\a" {
				t.Fatalf("alternate-screen attention = %q, want title and bell only", got)
			}
			wantEffects := []string{
				wakeAttentionEffectBell,
				wakeAttentionEffectTitle,
			}
			if !reflect.DeepEqual(recorded.Effects, wantEffects) {
				t.Fatalf("effects = %#v, want %#v", recorded.Effects, wantEffects)
			}
			if recorded.OutputProvenance != wakePayloadPeerHeaders {
				t.Fatalf("provenance = %q, want peer headers", recorded.OutputProvenance)
			}
		})
	}
}

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

func TestWakeDiagnosticAvoidsAlternateScreenTTYAndKeepsRedirectedLogs(t *testing.T) {
	cfg := &wakeConfig{
		me:              "codex",
		diagnosticIsTTY: func() bool { return true },
	}
	stderr := captureWakeStderr(t, func() {
		if err := writeWakeDiagnostic(cfg, "amq wake: warning: %s\n", "unsafe"); err != nil {
			t.Fatalf("terminal diagnostic: %v", err)
		}
	})
	if stderr != "" {
		t.Fatalf("alternate-screen diagnostic wrote plain text: %q", stderr)
	}

	cfg.diagnosticIsTTY = func() bool { return false }
	stderr = captureWakeStderr(t, func() {
		if err := writeWakeDiagnostic(cfg, "amq wake: warning: %s\n", "logged"); err != nil {
			t.Fatalf("redirected diagnostic: %v", err)
		}
	})
	if stderr != "amq wake: warning: logged\n" {
		t.Fatalf("redirected diagnostic = %q", stderr)
	}
}

func TestWakeDiagnosticUsesItsOwnSinkInsteadOfAttentionTerminal(t *testing.T) {
	tests := []struct {
		name            string
		attentionIsTTY  bool
		diagnosticIsTTY bool
		wantDiagnostic  string
	}{
		{
			name:            "terminal attention with durable diagnostic log",
			attentionIsTTY:  true,
			diagnosticIsTTY: false,
			wantDiagnostic:  "durable diagnostic\n",
		},
		{
			name:            "redirected attention with shared diagnostic terminal",
			attentionIsTTY:  false,
			diagnosticIsTTY: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &wakeConfig{
				me:              "codex",
				attentionIsTTY:  func() bool { return tc.attentionIsTTY },
				diagnosticIsTTY: func() bool { return tc.diagnosticIsTTY },
			}
			stderr := captureWakeStderr(t, func() {
				if err := writeWakeDiagnostic(cfg, "durable diagnostic\n"); err != nil {
					t.Fatalf("writeWakeDiagnostic: %v", err)
				}
			})
			if stderr != tc.wantDiagnostic {
				t.Fatalf("diagnostic = %q, want %q", stderr, tc.wantDiagnostic)
			}
		})
	}
}

func TestNotifyNewMessagesCodexMaxHoldUsesTerminalSafeAttention(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	for index, sender := range []string{"codex2", "peer"} {
		message := format.Message{
			Header: format.Header{
				Schema:  1,
				ID:      "attention-max-hold-" + sender,
				From:    sender,
				To:      []string{"codex"},
				Thread:  "p2p/codex__" + sender,
				Subject: "pending",
				Created: "2026-07-30T08:00:00Z",
			},
		}
		data, err := message.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		name := string(rune('a'+index)) + ".md"
		if _, err := deliverToInboxForTest(t, root, "codex", name, data); err != nil {
			t.Fatal(err)
		}
	}

	oldWait := waitForWakeInputQuiet
	waitForWakeInputQuiet = func(*wakeConfig) bool { return false }
	t.Cleanup(func() { waitForWakeInputQuiet = oldWait })

	var written strings.Builder
	cfg := &wakeConfig{
		me:              "codex",
		root:            root,
		session:         "session1",
		wakeOwner:       &wakeOwner{},
		injectMode:      wakeInjectModeRaw,
		deferWhileInput: true,
		attentionEnv:    func(string) string { return "" },
		attentionIsTTY:  func() bool { return true },
		attentionWrite: func(data []byte) (int, error) {
			return written.Write(data)
		},
	}
	if err := notifyNewMessages(cfg); err != nil {
		t.Fatalf("notify max-hold messages: %v", err)
	}
	if got := written.String(); got != "\x1b]0;AMQ attention\a\a" {
		t.Fatalf("max-hold attention = %q, want title and bell only", got)
	}
}

func TestNotifyNewMessagesCodexInterruptFallbackUsesTerminalSafeAttention(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	message := format.Message{
		Header: format.Header{
			Schema:   1,
			ID:       "attention-interrupt",
			From:     "peer",
			To:       []string{"codex"},
			Thread:   "p2p/codex__peer",
			Subject:  "urgent",
			Created:  "2026-07-30T08:00:00Z",
			Priority: "urgent",
			Labels:   []string{"interrupt"},
		},
	}
	data, err := message.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deliverToInboxForTest(t, root, "codex", "urgent.md", data); err != nil {
		t.Fatal(err)
	}

	var written strings.Builder
	cfg := &wakeConfig{
		me:                "codex",
		root:              root,
		session:           "session1",
		wakeOwner:         &wakeOwner{},
		injectMode:        wakeInjectModeRaw,
		fallbackWarn:      true,
		interrupt:         true,
		interruptLabel:    "interrupt",
		interruptPriority: "urgent",
		terminalWrite: func(string) error {
			return newWakeInjectorUnsupportedError(syscall.EIO)
		},
		attentionEnv:   func(string) string { return "" },
		attentionIsTTY: func() bool { return true },
		diagnosticIsTTY: func() bool {
			return true
		},
		attentionWrite: func(data []byte) (int, error) {
			return written.Write(data)
		},
	}
	stderr := captureWakeStderr(t, func() {
		if err := notifyNewMessages(cfg); err != nil {
			t.Fatalf("notify interrupt fallback: %v", err)
		}
	})
	if stderr != "" {
		t.Fatalf("interrupt fallback wrote plain diagnostic: %q", stderr)
	}
	if got := written.String(); got != "\x1b]0;AMQ attention\a\a" {
		t.Fatalf("interrupt attention = %q, want title and bell only", got)
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
				if err := emitWakeAttention(cfg, wakePayload{
					text:       "safe;notice",
					provenance: wakePayloadPeerHeaders,
				}); err != nil {
					t.Fatalf("emit optional OSC attention: %v", err)
				}
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
		if err := emitWakeAttention(cfg, payload); err != nil {
			t.Fatalf("emit operator-provenance attention: %v", err)
		}
	})
	if stderr != coopWakeDoorbell+"\n" {
		t.Fatalf("operator output was content-normalized: %q", stderr)
	}
	if recorded.OutputProvenance != wakePayloadOperatorFlag {
		t.Fatalf("provenance = %q, want operator", recorded.OutputProvenance)
	}
}
