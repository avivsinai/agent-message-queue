package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func stubInspectWakeProcess(t *testing.T, fn func(pid int) wakeProcessInfo) {
	t.Helper()
	old := inspectWakeProcess
	inspectWakeProcess = fn
	t.Cleanup(func() {
		inspectWakeProcess = old
	})
}

func secureTempDirForTest(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(cliSecureTempRoot, "amq-test-")
	if err != nil {
		t.Fatalf("create secure test temp directory: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove secure test temp directory: %v", err)
		}
	})
	if resolved, resolveErr := filepath.EvalSymlinks(dir); resolveErr == nil {
		return resolved
	}
	return dir
}

func mustNewWakeTargetForTest(t *testing.T, root, me, injectVia string, injectArgs []string) wakeTarget {
	t.Helper()
	target, err := newWakeTarget(root, me, injectVia, injectArgs)
	if err != nil {
		t.Fatalf("newWakeTarget: %v", err)
	}
	return target
}

func writeWakeLockForTest(t *testing.T, root, agent string, lock wakeLock) string {
	t.Helper()
	if lock.Root == "" {
		lock.Root = canonicalWakeRoot(root)
	}
	if lock.Agent == "" {
		lock.Agent = agent
	}
	return writeWakeLockExactForTest(t, root, agent, lock)
}

func writeWakeLockExactForTest(t *testing.T, root, agent string, lock wakeLock) string {
	t.Helper()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("EnsureRootDirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, agent); err != nil {
		t.Fatalf("EnsureAgentDirs: %v", err)
	}
	if lock.Started == "" {
		lock.Started = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	}
	data, err := json.Marshal(lock)
	if err != nil {
		t.Fatalf("marshal wake lock: %v", err)
	}
	lockPath := filepath.Join(fsq.AgentBase(root, agent), ".wake.lock")
	if err := os.WriteFile(lockPath, data, 0o600); err != nil {
		t.Fatalf("write wake lock: %v", err)
	}
	return lockPath
}

func bindWakeLockToTarget(lock wakeLock, target wakeTarget) wakeLock {
	lock.WakeMode = wakeTargetInjectVia
	lock.TargetDigest = mustWakeTargetDigest(target)
	return lock
}

func mustWakeTargetDigest(target wakeTarget) string {
	digest, err := wakeTargetDigest(target)
	if err != nil {
		panic(err)
	}
	return digest
}

func TestWakeLockStateBindingRawABIGoldens(t *testing.T) {
	const (
		generation = "0123456789abcdef0123456789abcdef"
		digest     = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	)

	tests := []struct {
		name string
		lock wakeLock
		want string
	}{
		{
			name: "bound generic target lock",
			lock: wakeLock{
				PID:             1,
				TTY:             "tty",
				Root:            "/queue",
				Agent:           "codex",
				Started:         "2026-08-02T00:00:00Z",
				WakeMode:        wakeTargetInjectVia,
				TargetDigest:    digest,
				Generation:      generation,
				StateGeneration: generation,
				StateDigest:     digest,
			},
			want: `{"pid":1,"tty":"tty","root":"/queue","agent":"codex","started":"2026-08-02T00:00:00Z","wake_mode":"inject-via","target_digest":"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","generation":"0123456789abcdef0123456789abcdef","state_generation":"0123456789abcdef0123456789abcdef","state_digest":"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}`,
		},
		{
			name: "bound authoritative target lock",
			lock: wakeLock{
				PID:             1,
				TTY:             "tty",
				Root:            "/queue",
				Agent:           "codex",
				Started:         "2026-08-02T00:00:00Z",
				WakeMode:        wakeOwnerWakeMode,
				TargetDigest:    digest,
				Generation:      generation,
				StateGeneration: generation,
				StateDigest:     digest,
				OwnerSchema:     wakeOwnerLockSchema,
				Owner:           &wakeOwner{PID: 1},
			},
			want: `{"pid":1,"tty":"tty","root":"/queue","agent":"codex","started":"2026-08-02T00:00:00Z","wake_mode":"owner-inject-via-v1","target_digest":"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","generation":"0123456789abcdef0123456789abcdef","state_generation":"0123456789abcdef0123456789abcdef","state_digest":"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","owner_schema":1,"owner":{"pid":1}}`,
		},
		{
			name: "unbound generic target lock preserves legacy bytes",
			lock: wakeLock{
				PID:          1,
				TTY:          "tty",
				Root:         "/queue",
				Agent:        "codex",
				Started:      "2026-08-02T00:00:00Z",
				WakeMode:     wakeTargetInjectVia,
				TargetDigest: digest,
				Generation:   generation,
			},
			want: `{"pid":1,"tty":"tty","root":"/queue","agent":"codex","started":"2026-08-02T00:00:00Z","wake_mode":"inject-via","target_digest":"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","generation":"0123456789abcdef0123456789abcdef"}`,
		},
		{
			name: "unbound authoritative target lock preserves legacy bytes",
			lock: wakeLock{
				PID:          1,
				TTY:          "tty",
				Root:         "/queue",
				Agent:        "codex",
				Started:      "2026-08-02T00:00:00Z",
				WakeMode:     wakeOwnerWakeMode,
				TargetDigest: digest,
				Generation:   generation,
				OwnerSchema:  wakeOwnerLockSchema,
				Owner:        &wakeOwner{PID: 1},
			},
			want: `{"pid":1,"tty":"tty","root":"/queue","agent":"codex","started":"2026-08-02T00:00:00Z","wake_mode":"owner-inject-via-v1","target_digest":"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","generation":"0123456789abcdef0123456789abcdef","owner_schema":1,"owner":{"pid":1}}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.lock)
			if err != nil {
				t.Fatalf("marshal wake lock: %v", err)
			}
			if got := string(raw); got != tc.want {
				t.Fatalf("wake lock raw bytes = %s\nwant = %s", got, tc.want)
			}
		})
	}
}

func TestWakeLockStateBindingValidation(t *testing.T) {
	const (
		generation = "0123456789abcdef0123456789abcdef"
		digest     = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	)
	bound := wakeLock{
		WakeMode:        wakeTargetInjectVia,
		TargetDigest:    digest,
		Generation:      generation,
		StateGeneration: generation,
		StateDigest:     digest,
	}

	tests := []struct {
		name    string
		mutate  func(*wakeLock)
		wantErr string
	}{
		{name: "unbound legacy target lock remains valid", mutate: func(lock *wakeLock) {
			lock.StateGeneration = ""
			lock.StateDigest = ""
		}},
		{name: "generation without digest", mutate: func(lock *wakeLock) { lock.StateDigest = "" }, wantErr: "must be present together"},
		{name: "digest without generation", mutate: func(lock *wakeLock) { lock.StateGeneration = "" }, wantErr: "must be present together"},
		{name: "malformed state generation", mutate: func(lock *wakeLock) { lock.StateGeneration = "not-a-generation" }, wantErr: "state generation is invalid"},
		{name: "generation differs from lock", mutate: func(lock *wakeLock) { lock.StateGeneration = "fedcba9876543210fedcba9876543210" }, wantErr: "state generation does not match lock generation"},
		{name: "malformed state digest", mutate: func(lock *wakeLock) { lock.StateDigest = "sha256:bad" }, wantErr: "state digest is invalid"},
		{name: "digest differs from target", mutate: func(lock *wakeLock) {
			lock.StateDigest = "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
		}, wantErr: "state digest does not match lock target digest"},
		{name: "targetless lock cannot bind state", mutate: func(lock *wakeLock) { lock.TargetDigest = "" }, wantErr: "state binding requires a target digest"},
		{name: "non-target wake mode cannot bind state", mutate: func(lock *wakeLock) { lock.WakeMode = "none" }, wantErr: "state binding requires a target-bearing lock"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lock := bound
			tc.mutate(&lock)
			err := validateWakeLockStateBinding(lock)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validate state binding: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validate state binding error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestWakeLockStateBindingIsToleratedByLegacyJSONReader(t *testing.T) {
	const (
		generation = "0123456789abcdef0123456789abcdef"
		digest     = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	)
	bound := wakeLock{
		PID:             1,
		TTY:             "tty",
		Root:            "/queue",
		Agent:           "codex",
		Started:         "2026-08-02T00:00:00Z",
		WakeMode:        wakeTargetInjectVia,
		TargetDigest:    digest,
		Generation:      generation,
		StateGeneration: generation,
		StateDigest:     digest,
	}
	raw, err := json.Marshal(bound)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"state_generation"`) || !strings.Contains(string(raw), `"state_digest"`) {
		t.Fatalf("bound lock omitted state ABI fields: %s", raw)
	}
	var legacy struct {
		PID          int    `json:"pid"`
		Root         string `json:"root"`
		Agent        string `json:"agent"`
		TargetDigest string `json:"target_digest"`
		Generation   string `json:"generation"`
	}
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatalf("legacy JSON reader rejected additive state fields: %v", err)
	}
	if legacy.PID != bound.PID || legacy.Root != bound.Root || legacy.Agent != bound.Agent ||
		legacy.TargetDigest != bound.TargetDigest || legacy.Generation != bound.Generation {
		t.Fatalf("legacy JSON reader changed known lock fields: %#v", legacy)
	}
}

func TestInspectWakeLockRejectsMalformedStateBindingFields(t *testing.T) {
	const (
		generation = "0123456789abcdef0123456789abcdef"
		digest     = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	)
	tests := []struct {
		name   string
		fields string
	}{
		{name: "both null", fields: `"state_generation":null,"state_digest":null`},
		{name: "both empty", fields: `"state_generation":"","state_digest":""`},
		{name: "both wrong type", fields: `"state_generation":42,"state_digest":42`},
		{name: "partial null generation", fields: `"state_generation":null`},
		{name: "partial null digest", fields: `"state_digest":null`},
		{name: "partial string generation", fields: `"state_generation":"0123456789abcdef0123456789abcdef"`},
	}

	for _, mode := range []os.FileMode{0o600, wakeOwnerLockFileMode} {
		for _, tc := range tests {
			t.Run(tc.name+"-"+mode.String(), func(t *testing.T) {
				root := secureTempDirForTest(t)
				lock := wakeLock{
					PID:          4242,
					TTY:          "tty",
					Root:         canonicalWakeRoot(root),
					Agent:        "codex",
					Started:      "2026-08-02T00:00:00Z",
					WakeMode:     wakeTargetInjectVia,
					TargetDigest: digest,
					Generation:   generation,
				}
				base, err := json.Marshal(lock)
				if err != nil {
					t.Fatal(err)
				}
				raw := append(append([]byte{}, base[:len(base)-1]...), append([]byte(","+tc.fields), '}')...)
				path := writeWakeLockForTest(t, root, "codex", lock)
				if err := os.WriteFile(path, raw, mode); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, mode); err != nil {
					t.Fatal(err)
				}
				inspection := inspectWakeLock(root, "codex")
				if inspection.Status != wakeLockUnverified {
					t.Fatalf("inspection status = %q reason %q, want unverified", inspection.Status, inspection.Reason)
				}
			})
		}
	}
}

func TestInspectWakeLockRejectsMalformedWholeJSON(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
	}{
		{name: "empty", raw: []byte{}},
		{name: "whitespace", raw: []byte(" \n\t")},
		{name: "empty object", raw: []byte(`{}`)},
		{name: "null required fields", raw: []byte(`{"pid":null,"tty":null,"root":null,"started":null}`)},
		{name: "pid wrong type", raw: []byte(`{"pid":"4242","tty":"tty","root":"/tmp","started":"now"}`)},
		{name: "tty wrong type", raw: []byte(`{"pid":4242,"tty":42,"root":"/tmp","started":"now"}`)},
		{name: "root wrong type", raw: []byte(`{"pid":4242,"tty":"tty","root":42,"started":"now"}`)},
		{name: "started wrong type", raw: []byte(`{"pid":4242,"tty":"tty","root":"/tmp","started":42}`)},
		{name: "truncated object", raw: []byte(`{"pid":`)},
		{name: "null", raw: []byte(`null`)},
		{name: "array", raw: []byte(`[]`)},
	}

	for _, mode := range []os.FileMode{0o600, wakeOwnerLockFileMode} {
		for _, tc := range tests {
			t.Run(tc.name+"-"+mode.String(), func(t *testing.T) {
				root := secureTempDirForTest(t)
				path := writeWakeLockForTest(t, root, "codex", wakeLock{PID: 4242})
				if err := os.WriteFile(path, tc.raw, mode); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, mode); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(path, time.Now().Add(-3*time.Second), time.Now().Add(-3*time.Second)); err != nil {
					t.Fatal(err)
				}

				inspection := inspectWakeLock(root, "codex")
				if inspection.Status != wakeLockUnverified {
					t.Fatalf("inspection status = %q reason %q, want unverified", inspection.Status, inspection.Reason)
				}
				if claim := classifyWakeClaimForGenericTransition(inspection); claim != wakeClaimInvalid {
					t.Fatalf("claim = %v, want invalid", claim)
				}
				if err := validateGenericWakeLifecycleTransition(inspection, wakeGenericRequestMutate); err == nil {
					t.Fatal("generic mutation accepted malformed whole lock JSON")
				}
			})
		}
	}
}

func TestValidUnboundP2aWakeLockRemainsGeneric(t *testing.T) {
	root := secureTempDirForTest(t)
	path := writeWakeLockForTest(t, root, "codex", wakeLock{PID: 4242})
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateWakeLockStateBindingJSON(raw); err != nil {
		t.Fatalf("unbound P2a wake lock rejected: %v", err)
	}
	inspection := inspectWakeLock(root, "codex")
	if claim := classifyWakeClaimForGenericTransition(inspection); claim != wakeClaimGeneric {
		t.Fatalf("claim = %v, want generic", claim)
	}
	if err := validateGenericWakeLifecycleTransition(inspection, wakeGenericRequestMutate); err != nil {
		t.Fatalf("generic P2a mutation rejected: %v", err)
	}
}

func TestSameWakeInjectorIdentityUsesOnlyPathAndOrderedArgs(t *testing.T) {
	first := wakeTarget{
		InjectVia:  "/opt/amq/injector",
		InjectArgs: []string{"exec", "target"},
		Created:    "2026-01-01T00:00:00Z",
		Owner:      &wakeOwner{PID: 101, ProcessStart: "first"},
	}
	second := first
	second.Created = "2026-07-20T00:00:00Z"
	second.Owner = &wakeOwner{PID: 202, ProcessStart: "second"}
	if !sameWakeInjectorIdentity(first, second) {
		t.Fatal("Created/owner metadata changed semantic injector identity")
	}

	second.InjectArgs = []string{"target", "exec"}
	if sameWakeInjectorIdentity(first, second) {
		t.Fatal("ordered fixed arguments were treated as interchangeable")
	}
	second = first
	second.InjectVia = "/opt/amq/other-injector"
	if sameWakeInjectorIdentity(first, second) {
		t.Fatal("different injector paths were treated as the same identity")
	}
}

func TestWakeBootIDMismatchAcceptsDarwinLegacyMigration(t *testing.T) {
	tests := []struct {
		name     string
		recorded string
		process  wakeProcessInfo
		mismatch bool
	}{
		{
			name:     "current boot session uuid",
			recorded: "9C0682F4-901B-4243-8B5C-287FAFB9AD0E",
			process:  wakeProcessInfo{BootID: "9C0682F4-901B-4243-8B5C-287FAFB9AD0E"},
		},
		{
			name:     "legacy boot time with macOS clock correction",
			recorded: "1783327533.465308000",
			process: wakeProcessInfo{
				BootID:       "9C0682F4-901B-4243-8B5C-287FAFB9AD0E",
				LegacyBootID: "1783327533.407566000",
			},
		},
		{
			name:     "boot time fallback with macOS clock correction",
			recorded: "1783327533.465308000",
			process:  wakeProcessInfo{BootID: "1783327533.407566000"},
		},
		{
			name:     "different legacy boot",
			recorded: "1783327533.465308000",
			process: wakeProcessInfo{
				BootID:       "9C0682F4-901B-4243-8B5C-287FAFB9AD0E",
				LegacyBootID: "1783327535.407566000",
			},
			mismatch: false,
		},
		{
			name:     "differing legacy with current UUID is unknown",
			recorded: "100.000000000",
			process: wakeProcessInfo{
				BootID:       "9C0682F4-901B-4243-8B5C-287FAFB9AD0E",
				LegacyBootID: "200.000000000",
			},
			mismatch: false,
		},
		{
			name:     "different boot session uuid",
			recorded: "AAAAAAAA-AAAA-AAAA-AAAA-AAAAAAAAAAAA",
			process:  wakeProcessInfo{BootID: "BBBBBBBB-BBBB-BBBB-BBBB-BBBBBBBBBBBB"},
			mismatch: true,
		},
		{
			name:     "same boot session uuid different case",
			recorded: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
			process:  wakeProcessInfo{BootID: "AAAAAAAA-AAAA-AAAA-AAAA-AAAAAAAAAAAA"},
		},
		{
			name:     "recorded boot with unavailable current identity",
			recorded: "AAAAAAAA-AAAA-AAAA-AAAA-AAAAAAAAAAAA",
			process:  wakeProcessInfo{},
			mismatch: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := wakeBootIDMismatch(tc.recorded, tc.process); got != tc.mismatch {
				t.Fatalf("wakeBootIDMismatch() = %v, want %v", got, tc.mismatch)
			}
		})
	}
}

func TestInspectWakeLockAcceptsLegacyDarwinBootIDForProvenWake(t *testing.T) {
	const wakePID = 4242
	root := secureTempDirForTest(t)
	writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:          wakePID,
		TTY:          "tty",
		ProcessStart: "start-1",
		BootID:       "1783327533.465308000",
		Executable:   "/opt/homebrew/bin/amq",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		if pid == wakePID {
			return wakeProcessInfo{
				PID:          pid,
				Running:      true,
				StartToken:   "start-1",
				BootID:       "9C0682F4-901B-4243-8B5C-287FAFB9AD0E",
				LegacyBootID: "1783327533.407566000",
				Executable:   "/opt/homebrew/bin/amq",
				Args:         []string{"/opt/homebrew/bin/amq", "wake", "--root", root, "--me", "codex"},
			}
		}
		return wakeProcessInfo{PID: pid}
	})

	inspection := inspectWakeLock(root, "codex")
	if inspection.Status != wakeLockValid || !inspection.IdentityConfirmed {
		t.Fatalf("inspection = status %q reason %q confirmed %v", inspection.Status, inspection.Reason, inspection.IdentityConfirmed)
	}
}

func TestInspectWakeLockDoesNotProveLiveRenamedBinaryStale(t *testing.T) {
	const wakePID = 4242
	root := secureTempDirForTest(t)
	writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:          wakePID,
		TTY:          "tty",
		ProcessStart: "start-1",
		BootID:       "boot-1",
		Executable:   "/opt/amq-dev",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: "start-1",
			BootID:     "boot-1",
			Executable: "/opt/amq-dev",
			Args: []string{
				"amq",
				"wake",
				"--root",
				root,
				"--me",
				"codex",
			},
		}
	})

	inspection := inspectWakeLock(root, "codex")
	if inspection.Status != wakeLockUnverified {
		t.Fatalf(
			"matching live renamed binary status = %s (%s), want unverified",
			inspection.Status,
			inspection.Reason,
		)
	}
}

func TestInspectWakeLockTreatsUnavailableCurrentBootIdentityAsUnverified(t *testing.T) {
	const wakePID = 4343
	root := secureTempDirForTest(t)
	writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:          wakePID,
		TTY:          "tty",
		ProcessStart: "start-1",
		BootID:       "recorded-boot",
		Executable:   "/opt/homebrew/bin/amq",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			StartToken: "start-1",
			Executable: "/opt/homebrew/bin/amq",
			Args:       []string{"/opt/homebrew/bin/amq", "wake", "--root", root, "--me", "codex"},
		}
	})

	inspection := inspectWakeLock(root, "codex")
	if inspection.Status != wakeLockUnverified {
		t.Fatalf("inspection status = %q, want unverified (reason %q)", inspection.Status, inspection.Reason)
	}
}

func TestInspectWakeLockRejectsBootIDWithoutProcessStart(t *testing.T) {
	const wakePID = 4444
	root := secureTempDirForTest(t)
	writeWakeLockForTest(t, root, "codex", wakeLock{
		PID:        wakePID,
		TTY:        "tty",
		BootID:     "recorded-boot",
		Executable: "/opt/homebrew/bin/amq",
	})
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{
			PID:        pid,
			Running:    true,
			Executable: "/opt/homebrew/bin/amq",
			Args:       []string{"/opt/homebrew/bin/amq", "wake", "--root", root, "--me", "codex"},
		}
	})

	inspection := inspectWakeLock(root, "codex")
	if inspection.Status != wakeLockUnverified || inspection.IdentityConfirmed {
		t.Fatalf("inspection = status %q reason %q confirmed %v; want unverified and unconfirmed",
			inspection.Status, inspection.Reason, inspection.IdentityConfirmed)
	}
	if inspection.Reason != "boot id requires process start metadata" {
		t.Fatalf("inspection reason = %q", inspection.Reason)
	}
}
