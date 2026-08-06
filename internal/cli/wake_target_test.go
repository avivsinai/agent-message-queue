package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWakeTargetSerializedBytesAndDigestGoldens(t *testing.T) {
	tests := []struct {
		name       string
		target     wakeTarget
		wantBytes  string
		wantDigest string
	}{
		{
			name: "canonical",
			target: wakeTarget{
				Schema:    wakeTargetSchema,
				Mode:      wakeTargetInjectVia,
				Root:      "/var/lib/amq",
				Agent:     "codex",
				Created:   "2026-08-04T00:00:00Z",
				InjectVia: "/usr/local/bin/injector",
			},
			wantBytes:  `{"schema":1,"mode":"inject-via","root":"/var/lib/amq","agent":"codex","created":"2026-08-04T00:00:00Z","inject_via":"/usr/local/bin/injector"}`,
			wantDigest: "sha256:d2cc3155136d4963718df60e3f86247cf3c738a6432d68c6823b884303113e01",
		},
		{
			name: "optional fields",
			target: wakeTarget{
				Schema:     wakeTargetSchema,
				Mode:       wakeTargetInjectVia,
				Root:       "/var/lib/amq",
				Agent:      "codex",
				Created:    "2026-08-04T00:00:00Z",
				InjectVia:  "/usr/local/bin/injector",
				InjectArgs: []string{"exec", "tab-42"},
				Owner: &wakeOwner{
					PID:          4242,
					ProcessStart: "12345",
					BootID:       "boot-1",
					SessionID:    99,
				},
			},
			wantBytes:  `{"schema":1,"mode":"inject-via","root":"/var/lib/amq","agent":"codex","created":"2026-08-04T00:00:00Z","inject_via":"/usr/local/bin/injector","inject_args":["exec","tab-42"],"owner":{"pid":4242,"process_start":"12345","boot_id":"boot-1","session_id":99}}`,
			wantDigest: "sha256:eb6bc4ee799825b47aa06d6b7367ec971291adc39b5a6d55d965ee0313aef88a",
		},
		{
			// Distinct from canonical: explicit injected policy must serialize and
			// digest differently while omitted/default remains byte-compatible.
			name: "retry_until injected",
			target: wakeTarget{
				Schema:     wakeTargetSchema,
				Mode:       wakeTargetInjectVia,
				Root:       "/var/lib/amq",
				Agent:      "codex",
				Created:    "2026-08-04T00:00:00Z",
				InjectVia:  "/usr/local/bin/injector",
				RetryUntil: wakeRetryUntilInjected,
			},
			wantBytes:  `{"schema":1,"mode":"inject-via","root":"/var/lib/amq","agent":"codex","created":"2026-08-04T00:00:00Z","inject_via":"/usr/local/bin/injector","retry_until":"injected"}`,
			wantDigest: "sha256:436792b393eae1deb9dbb231c5258ca69f971744b188ed189129ac850a8344f8",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			serialized, err := json.Marshal(test.target)
			if err != nil {
				t.Fatal(err)
			}
			if got := string(serialized); got != test.wantBytes {
				t.Fatalf("serialized wake target = %q, want %q", got, test.wantBytes)
			}

			digest, err := wakeTargetDigest(test.target)
			if err != nil {
				t.Fatal(err)
			}
			if digest != test.wantDigest {
				t.Fatalf("wake target digest = %q, want %q", digest, test.wantDigest)
			}
		})
	}
}

func TestValidateWakeTargetRejectsNoncanonicalRetryUntil(t *testing.T) {
	root := secureTempDirForTest(t)
	injector := filepath.Join(root, "injector")
	if err := os.WriteFile(injector, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	base := wakeTarget{
		Schema:    wakeTargetSchema,
		Mode:      wakeTargetInjectVia,
		Root:      root,
		Agent:     "codex",
		Created:   "2026-08-04T00:00:00Z",
		InjectVia: injector,
	}
	for _, ok := range []string{"", wakeRetryUntilDrained, wakeRetryUntilInjected} {
		target := base
		target.RetryUntil = ok
		if err := validateWakeTarget(target, root, "codex"); err != nil {
			t.Fatalf("canonical RetryUntil %q refused: %v", ok, err)
		}
	}
	for _, invalid := range []string{"replied", " INJECTED ", "Injected", " drained "} {
		target := base
		target.RetryUntil = invalid
		err := validateWakeTarget(target, root, "codex")
		if err == nil || !strings.Contains(err.Error(), "retry-until") {
			t.Fatalf("RetryUntil %q error = %v, want noncanonical refusal", invalid, err)
		}
	}
}

func TestNewWakeStateRoundTripPreservesRetryUntil(t *testing.T) {
	target := wakeTarget{
		Schema:     wakeTargetSchema,
		Mode:       wakeTargetInjectVia,
		Root:       "/var/lib/amq",
		Agent:      "codex",
		Created:    "2026-08-04T00:00:00Z",
		InjectVia:  "/usr/local/bin/injector",
		RetryUntil: wakeRetryUntilInjected,
	}
	raw, err := json.Marshal(target)
	if err != nil {
		t.Fatal(err)
	}
	state, err := newWakeState(wakeStateLegacy{Target: &target, TargetRaw: raw})
	if err != nil {
		t.Fatalf("newWakeState: %v", err)
	}
	if state.Target.RetryUntil != wakeRetryUntilInjected {
		t.Fatalf("state target RetryUntil = %q, want %q", state.Target.RetryUntil, wakeRetryUntilInjected)
	}
	got := state.Target.wakeTarget()
	if got.RetryUntil != wakeRetryUntilInjected {
		t.Fatalf("round-trip RetryUntil = %q, want %q", got.RetryUntil, wakeRetryUntilInjected)
	}
	if !sameWakeTarget(got, target) {
		t.Fatal("wakeTarget -> newWakeState -> wakeTarget changed target identity")
	}
}
