package cli

import (
	"reflect"
	"strings"
	"testing"
)

func TestInjectNotificationNoneEmitsAttentionFloorAndRecordsEffects(t *testing.T) {
	var recorded []string
	cfg := &wakeConfig{
		injectMode: wakeInjectModeNone,
		attentionEnv: func(string) string {
			return ""
		},
		recordEffects: func(effects []string) error {
			recorded = append([]string(nil), effects...)
			return nil
		},
	}

	stderr := captureWakeStderr(t, func() {
		if err := injectNotification(cfg, "safe notice", true); err != nil {
			t.Fatalf("injectNotification: %v", err)
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
	wantEffects := []string{
		wakeAttentionEffectOutput,
		wakeAttentionEffectBell,
		wakeAttentionEffectTitle,
	}
	if !reflect.DeepEqual(recorded, wantEffects) {
		t.Fatalf("recorded effects = %#v, want %#v", recorded, wantEffects)
	}
}

func TestWakeAttentionOptionalNotificationRequiresPositiveSupport(t *testing.T) {
	tests := []struct {
		name       string
		env        map[string]string
		wantEffect string
		wantBytes  string
	}{
		{
			name: "unknown terminal gets floor only",
			env:  map[string]string{"TERM_PROGRAM": "unknown"},
		},
		{
			name:       "iterm supports osc9",
			env:        map[string]string{"TERM_PROGRAM": "iTerm.app"},
			wantEffect: wakeAttentionEffectOSC9,
			wantBytes:  "\x1b]9;safe notice\a",
		},
		{
			name:       "vte supports osc777",
			env:        map[string]string{"VTE_VERSION": "7600"},
			wantEffect: wakeAttentionEffectOSC777,
			wantBytes:  "\x1b]777;notify;AMQ;safe notice\a",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &wakeConfig{
				attentionEnv: func(key string) string {
					return tc.env[key]
				},
			}
			var recorded []string
			cfg.recordEffects = func(effects []string) error {
				recorded = append([]string(nil), effects...)
				return nil
			}

			stderr := captureWakeStderr(t, func() {
				emitWakeAttention(cfg, "safe notice")
			})

			if tc.wantBytes == "" {
				if strings.Contains(stderr, "\x1b]9;") || strings.Contains(stderr, "\x1b]777;") {
					t.Fatalf("unknown terminal received optional notification: %q", stderr)
				}
			} else if !strings.Contains(stderr, tc.wantBytes) {
				t.Fatalf("stderr %q does not contain %q", stderr, tc.wantBytes)
			}
			if tc.wantEffect == "" {
				if len(recorded) != 3 {
					t.Fatalf("recorded effects = %#v, want floor only", recorded)
				}
			} else if recorded[len(recorded)-1] != tc.wantEffect {
				t.Fatalf("recorded effects = %#v, want final effect %q", recorded, tc.wantEffect)
			}
		})
	}
}

func TestWakeAttentionSanitizesOptionalNotificationPayload(t *testing.T) {
	cfg := &wakeConfig{
		attentionEnv: func(key string) string {
			if key == "TERM_PROGRAM" {
				return "iTerm.app"
			}
			return ""
		},
	}
	stderr := captureWakeStderr(t, func() {
		emitWakeAttention(cfg, "unsafe\x1b]2;spoof\a")
	})

	if strings.Contains(stderr, "spoof\a\a") || strings.Contains(stderr, "\x1b]2;") {
		t.Fatalf("unsafe control bytes survived notification sanitization: %q", stderr)
	}
	if !strings.Contains(stderr, "\x1b]9;unsafe ]2,spoof \a") {
		t.Fatalf("sanitized optional notification missing: %q", stderr)
	}
}
