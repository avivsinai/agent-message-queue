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

func TestWakeLockJSONTrustMatrix(t *testing.T) {
	tests := []struct {
		name    string
		invalid bool
		aged    bool
		mutate  func(*testing.T, []byte) []byte
	}{
		{name: "raw parse failure", invalid: true, aged: true, mutate: func(*testing.T, []byte) []byte { return []byte(`{"pid":`) }},
		{name: "non-object null", invalid: true, mutate: func(*testing.T, []byte) []byte { return []byte(`null`) }},
		{name: "non-object array", invalid: true, mutate: func(*testing.T, []byte) []byte { return []byte(`[]`) }},
		{name: "non-object scalar", invalid: true, mutate: func(*testing.T, []byte) []byte { return []byte(`42`) }},
		{name: "known wrong type early", invalid: true, mutate: func(t *testing.T, raw []byte) []byte {
			return replaceWakeLockJSONFieldForTest(t, raw, "agent", `42`)
		}},
		{name: "known wrong type late", invalid: true, mutate: func(t *testing.T, raw []byte) []byte {
			return replaceWakeLockJSONFieldForTest(t, raw, "running_image_evidence", `42`)
		}},
		{name: "known null direct", invalid: true, mutate: func(t *testing.T, raw []byte) []byte {
			return replaceWakeLockJSONFieldForTest(t, raw, "wake_mode", `null`)
		}},
		{name: "known null unicode-fold alias", invalid: true, mutate: func(t *testing.T, raw []byte) []byte {
			return replaceWakeLockJSONFieldNameAndValueForTest(t, raw, "process_start", "proceſs_start", `null`)
		}},
		{name: "duplicate known null then valid", invalid: true, mutate: func(t *testing.T, raw []byte) []byte {
			return prependWakeLockJSONFieldForTest(t, raw, "pid", `null`)
		}},
		{name: "duplicate known valid then null", invalid: true, mutate: func(t *testing.T, raw []byte) []byte {
			return appendWakeLockJSONFieldForTest(t, raw, "pid", `null`)
		}},
		{name: "duplicate known fold aliases", invalid: true, mutate: func(t *testing.T, raw []byte) []byte {
			return prependWakeLockJSONFieldForTest(t, raw, "PID", `1`)
		}},
		{name: "duplicate known escaped alias", invalid: true, mutate: func(t *testing.T, raw []byte) []byte {
			return prependWakeLockJSONFieldForTest(t, raw, `ag\u0065nt`, `"shadow"`)
		}},
		{name: "nested colliding keys", mutate: func(t *testing.T, raw []byte) []byte {
			return appendWakeLockJSONFieldForTest(t, raw, "future_lock_field", `{"pid":null,"pid":42}`)
		}},
		{name: "unknown single", mutate: func(t *testing.T, raw []byte) []byte {
			return appendWakeLockJSONFieldForTest(t, raw, "future_lock_field", `{"schema":99}`)
		}},
		{name: "unknown duplicated", mutate: func(t *testing.T, raw []byte) []byte {
			raw = appendWakeLockJSONFieldForTest(t, raw, "future_lock_field", `1`)
			return appendWakeLockJSONFieldForTest(t, raw, "future_lock_field", `2`)
		}},
		{name: "unknown null", mutate: func(t *testing.T, raw []byte) []byte {
			return appendWakeLockJSONFieldForTest(t, raw, "future_lock_field", `null`)
		}},
		{name: "valid control", mutate: func(_ *testing.T, raw []byte) []byte { return raw }},
		{name: "valid case-folded required field", mutate: func(t *testing.T, raw []byte) []byte {
			return renameWakeLockJSONFieldForTest(t, raw, "pid", "PID")
		}},
	}
	mutationSurfaces := []string{"acquisition", "stale cleanup", "doctor fix", "release", "recovery"}

	for _, test := range tests {
		for _, bound := range []bool{false, true} {
			operations := []string{"inspection"}
			if test.invalid {
				operations = mutationSurfaces
			}
			for _, operation := range operations {
				name := test.name + "/" + map[bool]string{false: "unbound", true: "bound"}[bound] + "/" + operation
				t.Run(name, func(t *testing.T) {
					fixture := newGenericWakePreparedCleanupFixture(t, bound)
					path := fixture.created.LockPath
					raw, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					raw = test.mutate(t, raw)
					if err := os.WriteFile(path, raw, 0o600); err != nil {
						t.Fatal(err)
					}
					if test.aged {
						past := time.Now().Add(-3 * time.Second)
						if err := os.Chtimes(path, past, past); err != nil {
							t.Fatal(err)
						}
					}
					if operation == "stale cleanup" {
						stubInspectWakeProcess(t, func(pid int) wakeProcessInfo { return wakeProcessInfo{PID: pid} })
					} else if !test.invalid {
						lock := fixture.created.Lock
						stubInspectWakeProcess(t, func(pid int) wakeProcessInfo {
							return wakeProcessInfo{
								PID:        pid,
								Running:    true,
								StartToken: lock.ProcessStart,
								BootID:     lock.BootID,
								Executable: "amq",
								Args:       []string{"amq", "wake", "--root", fixture.root, "--me", fixture.me},
							}
						})
					}

					inspection := inspectWakeLock(fixture.root, fixture.me)
					claim := classifyPersistedWakeClaim(inspection)
					if !test.invalid {
						if inspection.Status != wakeLockValid || inspection.decodeErr != nil || claim != wakeClaimGeneric {
							t.Fatalf("compatible lock inspection = %#v claim=%v, want valid generic", inspection, claim)
						}
						return
					}
					if inspection.Status != wakeLockUnverified || claim != wakeClaimInvalid {
						t.Fatalf("invalid lock inspection = %#v claim=%v, want invalid unverified", inspection, claim)
					}
					before := snapshotWakeCheckTree(t, fixture.root)

					switch operation {
					case "acquisition":
						cleanup, err := acquireWakeLockWithOptions(fixture.root, fixture.me, wakeLockAcquireOptions{})
						if cleanup != nil {
							cleanup()
						}
						if err == nil {
							t.Fatal("acquisition accepted invalid lock JSON")
						}
					case "stale cleanup":
						if err := cleanupTerminatedWakeLock(inspection); err == nil {
							t.Fatal("stale cleanup accepted invalid lock JSON")
						}
					case "doctor fix":
						locks, _ := checkWakeLocksWithHints(fixture.root, []string{fixture.me}, true)
						if len(locks) != 1 || locks[0].Removed || locks[0].Status != string(wakeLockUnverified) {
							t.Fatalf("doctor fix result = %#v, want preserved unverified lock", locks)
						}
					case "release":
						_ = withWakeLifecycleGuardInDir(fixture.agentDir, func(dirfd int) error {
							return cleanupGenericWakeGenerationAt(
								dirfd,
								fixture.agentDir,
								fixture.root,
								fixture.me,
								inspection,
								fixture.options,
							)
						})
					case "recovery":
						if _, err := recoverOwnerWake(fixture.root, fixture.me); err == nil {
							t.Fatal("recovery accepted invalid lock JSON")
						}
					}
					assertWakeCheckTreeUnchanged(t, fixture.root, before)
				})
			}
			if !test.invalid || !bound {
				continue
			}
			for _, operation := range []string{"authoritative release", "authoritative recovery"} {
				t.Run(test.name+"/bound/"+operation, func(t *testing.T) {
					fixture := newAuthoritativeWakePreparedCleanupFixture(t)
					raw, err := os.ReadFile(fixture.lockPath)
					if err != nil {
						t.Fatal(err)
					}
					raw = test.mutate(t, raw)
					if err := os.Chmod(fixture.lockPath, 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(fixture.lockPath, raw, 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.Chmod(fixture.lockPath, wakeOwnerLockFileMode); err != nil {
						t.Fatal(err)
					}
					inspection := inspectWakeLock(fixture.root, fixture.me)
					if inspection.Status != wakeLockUnverified || classifyPersistedWakeClaim(inspection) != wakeClaimInvalid {
						t.Fatalf("invalid authoritative inspection = %#v, want invalid unverified", inspection)
					}
					before := snapshotWakeCheckTree(t, fixture.root)
					switch operation {
					case "authoritative release":
						err := withWakeLifecycleGuardInDir(fixture.agentDir, func(dirfd int) error {
							return removeAuthoritativeWakeClaimAt(
								dirfd,
								fixture.agentDir,
								inspection,
								&fixture.target,
							)
						})
						if err == nil {
							t.Fatal("authoritative release accepted invalid lock JSON")
						}
					case "authoritative recovery":
						originalObserve := observeAuthoritativeWakeOwner
						observeCalls := 0
						observeAuthoritativeWakeOwner = func(wakeOwner) (wakeOwnerObservation, error) {
							observeCalls++
							return deadWakeOwnerObservation("unexpected owner observation"), nil
						}
						t.Cleanup(func() { observeAuthoritativeWakeOwner = originalObserve })
						result, err := recoverOwnerWake(fixture.root, fixture.me)
						if err == nil || result.Status != "refused" || observeCalls != 0 {
							t.Fatal("authoritative recovery accepted invalid lock JSON")
						}
					}
					assertWakeCheckTreeUnchanged(t, fixture.root, before)
				})
			}
		}
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

func replaceWakeLockJSONFieldNameAndValueForTest(t *testing.T, raw []byte, oldName, newName, replacement string) []byte {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	current, exists := fields[oldName]
	if !exists {
		t.Fatalf("wake lock field %q is absent", oldName)
	}
	needle := append([]byte(`"`+oldName+`":`), current...)
	replaced := bytes.Replace(raw, needle, []byte(`"`+newName+`":`+replacement), 1)
	if bytes.Equal(replaced, raw) {
		t.Fatalf("wake lock field %q was not replaced", oldName)
	}
	return replaced
}

func renameWakeLockJSONFieldForTest(t *testing.T, raw []byte, oldName, newName string) []byte {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	current, exists := fields[oldName]
	if !exists {
		t.Fatalf("wake lock field %q is absent", oldName)
	}
	needle := append([]byte(`"`+oldName+`":`), current...)
	replacement := append([]byte(`"`+newName+`":`), current...)
	renamed := bytes.Replace(raw, needle, replacement, 1)
	if bytes.Equal(renamed, raw) {
		t.Fatalf("wake lock field %q was not renamed", oldName)
	}
	return renamed
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

func prependWakeLockJSONFieldForTest(t *testing.T, raw []byte, name, value string) []byte {
	t.Helper()
	if len(raw) == 0 || raw[0] != '{' {
		t.Fatalf("wake lock JSON has no opening object delimiter: %q", raw)
	}
	result := append([]byte(`{"`+name+`":`+value+`,`), raw[1:]...)
	return result
}
