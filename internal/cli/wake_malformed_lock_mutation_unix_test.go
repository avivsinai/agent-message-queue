//go:build darwin || linux

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
	"time"
)

func writeMalformedPreviouslyBoundWakeLockForTest(t *testing.T, root, me string) (string, []byte, os.FileInfo) {
	t.Helper()
	target := mustNewWakeTargetForTest(t, root, me, writeExecutableForTest(t, "malformed-lock-injector"), []string{"exec"})
	lock := bindWakeLockToTarget(wakeLock{
		PID:        4242,
		Generation: "0123456789abcdef0123456789abcdef",
	}, target)
	lock.StateGeneration = lock.Generation
	lock.StateDigest = lock.TargetDigest
	path := writeWakeLockForTest(t, root, me, lock)
	if err := os.WriteFile(path, []byte(`{"pid":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, time.Now().Add(-3*time.Second), time.Now().Add(-3*time.Second)); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, raw, info
}

func assertMalformedWakeLockPreserved(t *testing.T, path string, wantRaw []byte, wantInfo os.FileInfo) {
	t.Helper()
	gotRaw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read malformed lock after refusal: %v", err)
	}
	gotInfo, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat malformed lock after refusal: %v", err)
	}
	if !bytes.Equal(gotRaw, wantRaw) || !sameWakeFileIdentity(gotInfo, wantInfo) {
		t.Fatal("malformed previously-bound wake lock changed after refusal")
	}
}

func TestMalformedPreviouslyBoundWakeLockRefusesGenericAcquire(t *testing.T) {
	root := secureTempDirForTest(t)
	path, raw, info := writeMalformedPreviouslyBoundWakeLockForTest(t, root, "codex")

	cleanup, err := acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{})
	if err == nil {
		if cleanup != nil {
			cleanup()
		}
		t.Fatal("generic acquisition accepted malformed previously-bound wake lock")
	}
	assertMalformedWakeLockPreserved(t, path, raw, info)
}

func TestValidUnboundP2aStaleWakeLockIsReplaced(t *testing.T) {
	root := secureTempDirForTest(t)
	path := writeWakeLockForTest(t, root, "codex", wakeLock{PID: 4242})
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
		return wakeProcessInfo{PID: pid}
	})

	cleanup, err := acquireWakeLockWithOptions(root, "codex", wakeLockAcquireOptions{})
	if err != nil {
		t.Fatalf("acquire after valid unbound P2a stale lock: %v", err)
	}
	t.Cleanup(cleanup)

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(after, before) {
		t.Fatal("valid unbound P2a stale lock was not replaced")
	}
	var replacement wakeLock
	if err := json.Unmarshal(after, &replacement); err != nil {
		t.Fatalf("decode replacement lock: %v", err)
	}
	if replacement.PID != os.Getpid() || replacement.StateGeneration != "" || replacement.StateDigest != "" {
		t.Fatalf("replacement = %#v, want a new unbound P2a lock", replacement)
	}
}

func TestMalformedPreviouslyBoundWakeLockDoctorFixPreservesIt(t *testing.T) {
	root := secureTempDirForTest(t)
	path, raw, info := writeMalformedPreviouslyBoundWakeLockForTest(t, root, "codex")

	locks, _ := checkWakeLocksWithHints(root, []string{"codex"}, true)
	if len(locks) != 1 {
		t.Fatalf("wake lock count = %d, want 1", len(locks))
	}
	if locks[0].Removed || locks[0].Status != string(wakeLockUnverified) {
		t.Fatalf("doctor fix result = %#v, want unverified preserved lock", locks[0])
	}
	assertMalformedWakeLockPreserved(t, path, raw, info)
}

func TestWrongTypedKnownWakeLockFieldsRefuseGenericMutation(t *testing.T) {
	tests := []struct {
		name        string
		bound       bool
		field       string
		replacement string
		appendField bool
	}{
		{name: "bound early agent", bound: true, field: "agent", replacement: `42`},
		{name: "bound wake mode", bound: true, field: "wake_mode", replacement: `42`},
		{name: "bound late image evidence", bound: true, field: "running_image_evidence", replacement: `42`, appendField: true},
		{name: "bound null target digest", bound: true, field: "target_digest", replacement: `null`},
		{name: "bound uppercase null wake mode", bound: true, field: "WAKE_MODE", replacement: `null`, appendField: true},
		{name: "bound unicode-fold null process start", bound: true, field: "proceſs_start", replacement: `null`, appendField: true},
		{name: "unbound early agent", field: "agent", replacement: `42`},
		{name: "unbound generation", field: "generation", replacement: `42`},
		{name: "unbound late image evidence", field: "running_image_evidence", replacement: `42`, appendField: true},
		{name: "unbound null wake mode", field: "wake_mode", replacement: `null`},
		{name: "unbound mixed-case null wake mode", field: "WaKe_MoDe", replacement: `null`, appendField: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newGenericWakePreparedCleanupFixture(t, test.bound)
			path := fixture.created.LockPath
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if test.appendField {
				raw = appendWakeLockJSONFieldForTest(t, raw, test.field, test.replacement)
			} else {
				raw = replaceWakeLockJSONFieldForTest(t, raw, test.field, test.replacement)
			}
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			info, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}

			inspection := inspectWakeLock(fixture.root, fixture.me)
			if inspection.Status != wakeLockUnverified {
				t.Fatalf("inspection status = %q reason %q, want unverified", inspection.Status, inspection.Reason)
			}
			if claim := classifyPersistedWakeClaim(inspection); claim != wakeClaimInvalid {
				t.Fatalf("claim = %v, want invalid", claim)
			}
			cleanup, err := acquireWakeLockWithOptions(fixture.root, fixture.me, wakeLockAcquireOptions{})
			if err == nil {
				if cleanup != nil {
					cleanup()
				}
				t.Fatal("generic acquisition accepted wrong-typed known wake lock field")
			}
			assertMalformedWakeLockPreserved(t, path, raw, info)
		})
	}
}

func TestUnknownFutureWakeLockFieldRemainsCompatible(t *testing.T) {
	for _, bound := range []bool{false, true} {
		t.Run(map[bool]string{false: "unbound", true: "bound"}[bound], func(t *testing.T) {
			fixture := newGenericWakePreparedCleanupFixture(t, bound)
			raw, err := os.ReadFile(fixture.created.LockPath)
			if err != nil {
				t.Fatal(err)
			}
			raw = appendWakeLockJSONFieldForTest(t, raw, "future_lock_field", `{"schema":99}`)
			if err := os.WriteFile(fixture.created.LockPath, raw, 0o600); err != nil {
				t.Fatal(err)
			}

			inspection := inspectWakeLock(fixture.root, fixture.me)
			if inspection.decodeErr != nil {
				t.Fatalf("unknown field decode error = %v", inspection.decodeErr)
			}
			if claim := classifyPersistedWakeClaim(inspection); claim != wakeClaimGeneric {
				t.Fatalf("unknown field claim = %v, want generic", claim)
			}
		})
	}
}

func replaceWakeLockJSONFieldForTest(t *testing.T, raw []byte, name, replacement string) []byte {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	current, exists := fields[name]
	if !exists {
		t.Fatalf("wake lock field %q is absent", name)
	}
	needle := append([]byte(`"`+name+`":`), current...)
	replaced := bytes.Replace(raw, needle, []byte(`"`+name+`":`+replacement), 1)
	if bytes.Equal(replaced, raw) {
		t.Fatalf("wake lock field %q was not replaced", name)
	}
	return replaced
}

func appendWakeLockJSONFieldForTest(t *testing.T, raw []byte, name, value string) []byte {
	t.Helper()
	if len(raw) == 0 || raw[len(raw)-1] != '}' {
		t.Fatalf("wake lock JSON has no closing object delimiter: %q", raw)
	}
	result := append([]byte{}, raw[:len(raw)-1]...)
	result = append(result, []byte(`,"`+name+`":`+value+`}`)...)
	return result
}
