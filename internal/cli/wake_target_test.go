package cli

import (
	"encoding/json"
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
