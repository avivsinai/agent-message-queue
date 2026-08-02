//go:build darwin || linux

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWakeStateDualReadPrefersExactState(t *testing.T) {
	fixture := newWakeStateUnixFixture(t, "legacy")
	publishWakeStateForDualReadTest(t, fixture)

	selection, err := readWakeStateSelectionForTest(t, fixture)
	if err != nil {
		t.Fatal(err)
	}
	if !selection.StatePreferred {
		t.Fatal("exact state was not preferred")
	}
	if got := selection.Target.InjectArgs; len(got) != 1 || got[0] != "legacy" {
		t.Fatalf("target args = %v", got)
	}
}

func TestWakeStateDualReadPrefersDocumentWithoutExtraLegacyParse(t *testing.T) {
	fixture := newWakeStateUnixFixture(t, "legacy")
	publishWakeStateForDualReadTest(t, fixture)
	original := afterWakeTargetSnapshotDataRead
	captures := 0
	afterWakeTargetSnapshotDataRead = func() { captures++ }
	t.Cleanup(func() { afterWakeTargetSnapshotDataRead = original })

	selection, err := readWakeStateSelectionForTest(t, fixture)
	if err != nil {
		t.Fatal(err)
	}
	if !selection.StatePreferred {
		t.Fatal("exact state was not preferred")
	}
	if captures != 2 {
		t.Fatalf("legacy target reads = %d, want exactly 2", captures)
	}
}

func TestWakeStateDualReadFallsBackWithoutMutatingDocument(t *testing.T) {
	tests := []struct {
		name   string
		absent bool
		mutate func(t *testing.T, raw []byte) []byte
	}{
		{name: "absent", absent: true},
		{name: "malformed", mutate: func(_ *testing.T, _ []byte) []byte { return []byte("{") }},
		{name: "noncanonical", mutate: func(_ *testing.T, raw []byte) []byte { return append(raw, '\n') }},
		{name: "newer", mutate: func(t *testing.T, raw []byte) []byte {
			return newerWakeStateRawForTest(t, raw, "document")
		}},
		{name: "newer target", mutate: func(t *testing.T, raw []byte) []byte {
			return newerWakeStateRawForTest(t, raw, "target")
		}},
		{name: "schema less", mutate: func(t *testing.T, raw []byte) []byte {
			var document map[string]json.RawMessage
			if err := json.Unmarshal(raw, &document); err != nil {
				t.Fatal(err)
			}
			delete(document, "schema")
			changed, err := json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			return changed
		}},
		{name: "target schema less", mutate: func(t *testing.T, raw []byte) []byte {
			var document map[string]json.RawMessage
			if err := json.Unmarshal(raw, &document); err != nil {
				t.Fatal(err)
			}
			var target map[string]json.RawMessage
			if err := json.Unmarshal(document["target"], &target); err != nil {
				t.Fatal(err)
			}
			delete(target, "schema")
			document["target"], _ = json.Marshal(target)
			changed, err := json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			return changed
		}},
		{name: "target missing from state", mutate: func(t *testing.T, raw []byte) []byte {
			var document map[string]json.RawMessage
			if err := json.Unmarshal(raw, &document); err != nil {
				t.Fatal(err)
			}
			delete(document, "target")
			changed, err := json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			return changed
		}},
		{name: "prepared exists only in state", mutate: func(t *testing.T, raw []byte) []byte {
			var state wakeState
			if err := json.Unmarshal(raw, &state); err != nil {
				t.Fatal(err)
			}
			state.Prepared = &wakeStatePrepared{
				Schema:        wakeStatePreparedSchema,
				Generation:    "11111111111111111111111111111111",
				LegacyPresent: true,
				TargetDigest:  state.Target.TargetDigest,
				LegacyDigest:  "sha256:" + string(bytes.Repeat([]byte{'0'}, 64)),
			}
			changed, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			return changed
		}},
		{name: "legacy digest mismatch", mutate: func(t *testing.T, raw []byte) []byte {
			var state wakeState
			if err := json.Unmarshal(raw, &state); err != nil {
				t.Fatal(err)
			}
			state.Target.LegacyDigest = "sha256:" + string(bytes.Repeat([]byte{'0'}, 64))
			changed, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			return changed
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newWakeStateUnixFixture(t, "legacy")
			publishWakeStateForDualReadTest(t, fixture)
			path := filepath.Join(fixture.agentDir.path, wakeStateFileName)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var installed []byte
			if test.absent {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else {
				installed = test.mutate(t, raw)
				if err := os.WriteFile(path, installed, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before := snapshotWakeCheckTree(t, fixture.root)
			var selection wakeStateReadSelection
			var readErr error
			stderr := captureWakeStderr(t, func() {
				selection, readErr = readWakeStateSelectionForTest(t, fixture)
			})
			if stderr != "" {
				t.Fatalf("stderr = %q, want silent fallback", stderr)
			}
			if readErr != nil {
				t.Fatal(readErr)
			}
			if selection.StatePreferred {
				t.Fatal("invalid state was preferred")
			}
			assertWakeCheckTreeUnchanged(t, fixture.root, before)
			if !test.absent {
				got, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, installed) {
					t.Fatalf("read mutated state: got %q want %q", got, installed)
				}
			}
		})
	}
}

func TestWakePreparedDualReadFourWay(t *testing.T) {
	tests := []struct {
		name      string
		configure func(t *testing.T, fixture *authoritativeWakePreparedCleanupFixture)
		wantReady bool
		wantError bool
	}{
		{name: "absent", configure: func(t *testing.T, fixture *authoritativeWakePreparedCleanupFixture) {
			if err := os.Remove(fixture.preparedPath); err != nil {
				t.Fatal(err)
			}
			republishWakeStateForPreparedTest(t, fixture)
		}},
		{name: "stale generation", configure: func(t *testing.T, fixture *authoritativeWakePreparedCleanupFixture) {
			marker := fixture.preparedMarker
			marker.Generation = "11111111111111111111111111111111"
			if err := writeWakeGenerationFile(fixture.preparedPath, "wake prepared marker", marker); err != nil {
				t.Fatal(err)
			}
			republishWakeStateForPreparedTest(t, fixture)
		}},
		{name: "current", wantReady: true},
		{name: "wrong target digest", wantError: true, configure: func(t *testing.T, fixture *authoritativeWakePreparedCleanupFixture) {
			marker := fixture.preparedMarker
			marker.TargetDigest = "sha256:" + string(bytes.Repeat([]byte{'0'}, 64))
			if err := writeWakeGenerationFile(fixture.preparedPath, "wake prepared marker", marker); err != nil {
				t.Fatal(err)
			}
			republishWakeStateForPreparedTest(t, fixture)
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAuthoritativeWakePreparedCleanupFixture(t)
			if test.configure != nil {
				test.configure(t, fixture)
			}
			before := snapshotWakeCheckTree(t, fixture.root)
			ready, err := validateWakePreparedFileAgainstInspection(
				fixture.root,
				fixture.me,
				fixture.inspection,
			)
			assertPreparedDualReadResult(t, ready, err, test.wantReady, test.wantError)

			var readyAt bool
			errAt := fixture.agentDir.withFD(func(dirfd int) error {
				var err error
				readyAt, err = validateWakePreparedFileAgainstInspectionAt(
					dirfd,
					fixture.agentDir,
					fixture.root,
					fixture.me,
					fixture.inspection,
				)
				return err
			})
			assertPreparedDualReadResult(t, readyAt, errAt, test.wantReady, test.wantError)
			assertWakeCheckTreeUnchanged(t, fixture.root, before)
		})
	}
}

func TestWakePreparedDualReadFallsBackFromIneligiblePreparedState(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, raw []byte) []byte
	}{
		{name: "newer prepared", mutate: func(t *testing.T, raw []byte) []byte {
			return newerWakeStateRawForTest(t, raw, "prepared")
		}},
		{name: "prepared missing from state", mutate: func(t *testing.T, raw []byte) []byte {
			var state wakeState
			if err := json.Unmarshal(raw, &state); err != nil {
				t.Fatal(err)
			}
			state.Prepared = nil
			changed, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			return changed
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAuthoritativeWakePreparedCleanupFixture(t)
			statePath := filepath.Join(fixture.agentDir.path, wakeStateFileName)
			raw, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatal(err)
			}
			installed := test.mutate(t, raw)
			if err := os.WriteFile(statePath, installed, 0o600); err != nil {
				t.Fatal(err)
			}
			before := snapshotWakeCheckTree(t, fixture.root)
			var ready bool
			var readErr error
			stderr := captureWakeStderr(t, func() {
				ready, readErr = validateWakePreparedFileAgainstInspection(
					fixture.root,
					fixture.me,
					fixture.inspection,
				)
			})
			if readErr != nil || !ready {
				t.Fatalf("legacy prepared fallback ready=%v err=%v", ready, readErr)
			}
			if stderr != "" {
				t.Fatalf("stderr = %q, want silent fallback", stderr)
			}
			assertWakeCheckTreeUnchanged(t, fixture.root, before)
		})
	}
}

func TestWakeStateDualReadPreservesPreparedErrorWithoutBreakingTargetRead(t *testing.T) {
	fixture := newAuthoritativeWakePreparedCleanupFixture(t)
	if err := os.WriteFile(fixture.preparedPath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapshotWakeCheckTree(t, fixture.root)
	target, exists, err := readWakeTargetFromState(fixture.root, fixture.me)
	if err != nil || !exists || !sameWakeTarget(target, fixture.target) {
		t.Fatalf("target read exists=%v target=%#v err=%v", exists, target, err)
	}
	ready, err := validateWakePreparedFileAgainstInspection(
		fixture.root,
		fixture.me,
		fixture.inspection,
	)
	if err == nil || ready {
		t.Fatalf("prepared read ready=%v err=%v, want legacy parse error", ready, err)
	}
	assertWakeCheckTreeUnchanged(t, fixture.root, before)
}

func TestWakeStateDualReadPreservesTargetForStableInvalidPrepared(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{
			name: "mode-0640",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Chmod(path, 0o640); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "symlink",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				target := path + ".symlink-target"
				if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "oversized",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, bytes.Repeat([]byte("x"), maxWakeMetadataFileBytes+1), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAuthoritativeWakePreparedCleanupFixture(t)
			test.mutate(t, fixture.preparedPath)
			before := snapshotWakeCheckTree(t, fixture.root)

			selection, err := readWakeStateSelection(fixture.root, fixture.me)
			if err != nil {
				var changed *wakeSnapshotReadChangedError
				if errors.As(err, &changed) {
					t.Fatalf("stable invalid prepared was typed as changed: %v", err)
				}
				t.Fatal(err)
			}
			if !selection.TargetPresent || !sameWakeTarget(selection.Target, fixture.target) {
				t.Fatalf("selection target present=%v target=%#v", selection.TargetPresent, selection.Target)
			}
			if selection.PreparedErr == nil {
				t.Fatal("selection lost stable prepared error")
			}
			if selection.StatePreferred {
				t.Fatal("state was preferred despite invalid legacy prepared evidence")
			}
			if selection.legacy.Prepared.Failure == nil ||
				!selection.legacy.Prepared.Failure.IdentityKnown {
				t.Fatalf("stable prepared failure fingerprint = %#v", selection.legacy.Prepared.Failure)
			}

			target, exists, err := readWakeTargetFromState(fixture.root, fixture.me)
			if err != nil || !exists || !sameWakeTarget(target, fixture.target) {
				t.Fatalf("target read exists=%v target=%#v err=%v", exists, target, err)
			}
			ready, err := validateWakePreparedFileAgainstInspection(
				fixture.root,
				fixture.me,
				fixture.inspection,
			)
			if err == nil || ready {
				t.Fatalf("prepared read ready=%v err=%v, want stable validation error", ready, err)
			}
			assertWakeCheckTreeUnchanged(t, fixture.root, before)
		})
	}
}

func TestWakeCheckAndDoctorGoldenBytesUnaffectedByWakeStatePresence(t *testing.T) {
	fixture := newAuthoritativeWakePreparedCleanupFixture(t)
	wakeBefore := wakeCheckPublicBytesForDualReadTest(t, fixture.root, fixture.me)
	locksBefore, hintsBefore := checkWakeLocksWithHintsSchema(
		fixture.root,
		[]string{fixture.me},
		false,
		wakeCheckSchemaV2,
	)
	doctorBefore, err := json.Marshal(struct {
		Locks []opsWakeLock `json:"locks"`
		Hints []opsHint     `json:"hints"`
	}{locksBefore, hintsBefore})
	if err != nil {
		t.Fatal(err)
	}

	statePath := filepath.Join(fixture.agentDir.path, wakeStateFileName)
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	absentTree := snapshotWakeCheckTree(t, fixture.root)
	wakeAfter := wakeCheckPublicBytesForDualReadTest(t, fixture.root, fixture.me)
	locksAfter, hintsAfter := checkWakeLocksWithHintsSchema(
		fixture.root,
		[]string{fixture.me},
		false,
		wakeCheckSchemaV2,
	)
	doctorAfter, err := json.Marshal(struct {
		Locks []opsWakeLock `json:"locks"`
		Hints []opsHint     `json:"hints"`
	}{locksAfter, hintsAfter})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(wakeBefore, wakeAfter) {
		t.Fatalf("wake-check decision drifted:\nbefore=%s\nafter=%s", wakeBefore, wakeAfter)
	}
	if !bytes.Equal(doctorBefore, doctorAfter) {
		t.Fatalf("doctor decision drifted:\nbefore=%s\nafter=%s", doctorBefore, doctorAfter)
	}
	assertWakeCheckTreeUnchanged(t, fixture.root, absentTree)
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("read-only diagnostics recreated state: %v", err)
	}
}

func wakeCheckPublicBytesForDualReadTest(t *testing.T, root, me string) []byte {
	t.Helper()
	decision := inspectWakeCheckDecision(root, me)
	v1, err := json.Marshal(renderWakeCheckV1(decision))
	if err != nil {
		t.Fatal(err)
	}
	v2, err := json.Marshal(renderWakeCheckV2(decision))
	if err != nil {
		t.Fatal(err)
	}
	return append(append(v1, '\n'), v2...)
}

func TestNewCLIDualReadNeverCreatesStateFromReadOnlyPath(t *testing.T) {
	fixture := newWakeStateUnixFixture(t, "legacy-only")
	statePath := filepath.Join(fixture.agentDir.path, wakeStateFileName)
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("fixture unexpectedly has state: %v", err)
	}
	before := snapshotWakeCheckTree(t, fixture.root)
	if _, exists, err := readWakeTargetFromState(fixture.root, fixture.agent); err != nil || !exists {
		t.Fatalf("semantic target read exists=%v err=%v", exists, err)
	}
	_ = inspectWakeCheckDecision(fixture.root, fixture.agent)
	_, _ = checkWakeLocksWithHintsSchema(
		fixture.root,
		[]string{fixture.agent},
		false,
		wakeCheckSchemaV2,
	)
	assertWakeCheckTreeUnchanged(t, fixture.root, before)
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("read-only path created state: %v", err)
	}
}

func TestWakeStateDualReadMissingAgentDirectoryIsReadOnly(t *testing.T) {
	root := secureTempDirForTest(t)
	agentPath := filepath.Join(root, "agents", "codex")
	before := snapshotWakeCheckTree(t, root)
	_, exists, err := readWakeTargetFromState(root, "codex")
	if err != nil || exists {
		t.Fatalf("missing-agent target exists=%v err=%v", exists, err)
	}
	assertWakeCheckTreeUnchanged(t, root, before)
	if _, err := os.Stat(agentPath); !os.IsNotExist(err) {
		t.Fatalf("read-only selector created agent directory: %v", err)
	}
}

func TestWakeCheckFingerprintAnchoredToLegacyNotState(t *testing.T) {
	fixture := newAuthoritativeWakePreparedCleanupFixture(t)
	selection, err := readWakeStateSelection(fixture.root, fixture.me)
	if err != nil {
		t.Fatal(err)
	}
	if !selection.StatePreferred {
		t.Fatal("fixture state was not preferred")
	}
	want, err := newWakeCheckMetadataFingerprint(
		selection.legacy.TargetPresent,
		selection.legacy.Target.Raw,
		selection.legacy.Target.FileInfo,
	)
	if err != nil {
		t.Fatal(err)
	}
	first, err := observeWakeCheck(fixture.root, fixture.me)
	if err != nil {
		t.Fatal(err)
	}
	if first.Target != want {
		t.Fatalf("target fingerprint = %#v, want legacy %#v", first.Target, want)
	}

	statePath := filepath.Join(fixture.agentDir.path, wakeStateFileName)
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	replacement := statePath + ".replacement"
	if err := os.WriteFile(replacement, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, statePath); err != nil {
		t.Fatal(err)
	}
	second, err := observeWakeCheck(fixture.root, fixture.me)
	if err != nil {
		t.Fatal(err)
	}
	if second.Target != want {
		t.Fatalf("state replacement changed target fingerprint: got %#v want %#v", second.Target, want)
	}
}

func TestWakeStateDualReadFallsBackWhenStateIsReplaced(t *testing.T) {
	fixture := newWakeStateUnixFixture(t, "legacy")
	publishWakeStateForDualReadTest(t, fixture)
	path := filepath.Join(fixture.agentDir.path, wakeStateFileName)
	replacement := filepath.Join(fixture.agentDir.path, ".wake-state-replacement")
	if err := os.WriteFile(replacement, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	original := afterWakeStateSnapshotRead
	afterWakeStateSnapshotRead = func() {
		afterWakeStateSnapshotRead = func() {}
		if err := os.Rename(replacement, path); err != nil {
			t.Errorf("replace state: %v", err)
		}
	}
	t.Cleanup(func() { afterWakeStateSnapshotRead = original })

	selection, err := readWakeStateSelectionForTest(t, fixture)
	if err != nil {
		t.Fatal(err)
	}
	if selection.StatePreferred {
		t.Fatal("replaced state was preferred")
	}
}

func TestWakeStateDualReadReturnsTypedErrorWhenLegacyChanges(t *testing.T) {
	fixture := newWakeStateUnixFixture(t, "before")
	publishWakeStateForDualReadTest(t, fixture)
	original := afterWakeStateDualReadDocument
	afterWakeStateDualReadDocument = func() {
		afterWakeStateDualReadDocument = func() {}
		target, exists, err := readWakeTarget(fixture.root, fixture.agent)
		if err != nil || !exists {
			t.Errorf("read replacement target: exists=%v err=%v", exists, err)
			return
		}
		target.InjectArgs = []string{"after"}
		data, err := json.MarshalIndent(target, "", "  ")
		if err != nil {
			t.Errorf("marshal replacement target: %v", err)
			return
		}
		if err := os.WriteFile(wakeTargetPath(fixture.root, fixture.agent), append(data, '\n'), 0o600); err != nil {
			t.Errorf("replace legacy target: %v", err)
		}
	}
	t.Cleanup(func() { afterWakeStateDualReadDocument = original })

	_, err := readWakeStateSelectionForTest(t, fixture)
	var changed *wakeSnapshotReadChangedError
	if !errors.As(err, &changed) {
		t.Fatalf("error = %v, want wakeSnapshotReadChangedError", err)
	}
}

func TestWakeStateDualReadReturnsTypedErrorWhenClosingLegacyIsMalformed(t *testing.T) {
	fixture := newWakeStateUnixFixture(t, "before")
	publishWakeStateForDualReadTest(t, fixture)
	original := afterWakeStateDualReadDocument
	afterWakeStateDualReadDocument = func() {
		afterWakeStateDualReadDocument = func() {}
		if err := os.WriteFile(wakeTargetPath(fixture.root, fixture.agent), []byte("{"), 0o600); err != nil {
			t.Errorf("replace legacy target: %v", err)
		}
	}
	t.Cleanup(func() { afterWakeStateDualReadDocument = original })

	_, err := readWakeStateSelectionForTest(t, fixture)
	var changed *wakeSnapshotReadChangedError
	if !errors.As(err, &changed) {
		t.Fatalf("error = %v, want wakeSnapshotReadChangedError", err)
	}
}

func TestWakeStateDualReadReturnsTypedErrorWhenPreparedMarkerChanges(t *testing.T) {
	fixture := newAuthoritativeWakePreparedCleanupFixture(t)
	original := afterWakeStateDualReadDocument
	afterWakeStateDualReadDocument = func() {
		afterWakeStateDualReadDocument = func() {}
		marker := fixture.preparedMarker
		marker.Generation = "11111111111111111111111111111111"
		if err := writeWakeGenerationFile(fixture.preparedPath, "wake prepared marker", marker); err != nil {
			t.Errorf("replace prepared marker: %v", err)
		}
	}
	t.Cleanup(func() { afterWakeStateDualReadDocument = original })

	_, err := readWakeStateSelection(fixture.root, fixture.me)
	var changed *wakeSnapshotReadChangedError
	if !errors.As(err, &changed) {
		t.Fatalf("error = %v, want wakeSnapshotReadChangedError", err)
	}
}

func TestWakeStateDualReadReturnsTypedErrorWhenInvalidPreparedMarkerIsReplaced(t *testing.T) {
	fixture := newAuthoritativeWakePreparedCleanupFixture(t)
	if err := os.Chmod(fixture.preparedPath, 0o640); err != nil {
		t.Fatal(err)
	}
	original := afterWakeStateDualReadDocument
	afterWakeStateDualReadDocument = func() {
		afterWakeStateDualReadDocument = func() {}
		replacement := fixture.preparedPath + ".replacement"
		if err := os.WriteFile(replacement, []byte("replacement"), 0o640); err != nil {
			t.Errorf("write invalid replacement prepared marker: %v", err)
			return
		}
		if err := os.Rename(replacement, fixture.preparedPath); err != nil {
			t.Errorf("replace invalid prepared marker: %v", err)
		}
	}
	t.Cleanup(func() { afterWakeStateDualReadDocument = original })

	_, err := readWakeStateSelection(fixture.root, fixture.me)
	var changed *wakeSnapshotReadChangedError
	if !errors.As(err, &changed) {
		t.Fatalf("error = %v, want wakeSnapshotReadChangedError", err)
	}
}

func TestWakeTargetAndPreparedReopenFailuresAreTypedSnapshotChanges(t *testing.T) {
	t.Run("target", func(t *testing.T) {
		fixture := newWakeStateUnixFixture(t, "legacy")
		original := afterWakeTargetSnapshotDataRead
		afterWakeTargetSnapshotDataRead = func() {
			afterWakeTargetSnapshotDataRead = func() {}
			if err := os.Remove(wakeTargetPath(fixture.root, fixture.agent)); err != nil {
				t.Errorf("remove target: %v", err)
			}
		}
		t.Cleanup(func() { afterWakeTargetSnapshotDataRead = original })

		var readErr error
		if err := fixture.agentDir.withFD(func(dirfd int) error {
			_, _, readErr = readWakeTargetSnapshotAt(
				dirfd,
				fixture.agentDir,
				fixture.root,
				fixture.agent,
			)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		var changed *wakeSnapshotReadChangedError
		if !errors.As(readErr, &changed) {
			t.Fatalf("error = %v, want wakeSnapshotReadChangedError", readErr)
		}
	})

	t.Run("prepared", func(t *testing.T) {
		fixture := newAuthoritativeWakePreparedCleanupFixture(t)
		original := afterWakeGenerationFileSnapshotDataRead
		afterWakeGenerationFileSnapshotDataRead = func(name string) {
			if name != wakePreparedFileName {
				return
			}
			afterWakeGenerationFileSnapshotDataRead = func(string) {}
			if err := os.Remove(fixture.preparedPath); err != nil {
				t.Errorf("remove prepared marker: %v", err)
			}
		}
		t.Cleanup(func() { afterWakeGenerationFileSnapshotDataRead = original })

		var readErr error
		if err := fixture.agentDir.withFD(func(dirfd int) error {
			_, _, readErr = readWakeGenerationFileSnapshotAt(
				dirfd,
				fixture.agentDir,
				wakePreparedFileName,
				"wake prepared marker",
			)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		var changed *wakeSnapshotReadChangedError
		if !errors.As(readErr, &changed) {
			t.Fatalf("error = %v, want wakeSnapshotReadChangedError", readErr)
		}
	})

	t.Run("prepared mode change", func(t *testing.T) {
		fixture := newAuthoritativeWakePreparedCleanupFixture(t)
		original := afterWakeGenerationFileSnapshotDataRead
		afterWakeGenerationFileSnapshotDataRead = func(name string) {
			if name != wakePreparedFileName {
				return
			}
			afterWakeGenerationFileSnapshotDataRead = func(string) {}
			if err := os.Chmod(fixture.preparedPath, 0o640); err != nil {
				t.Errorf("chmod prepared marker: %v", err)
			}
		}
		t.Cleanup(func() {
			afterWakeGenerationFileSnapshotDataRead = original
			_ = os.Chmod(fixture.preparedPath, 0o600)
		})

		var readErr error
		if err := fixture.agentDir.withFD(func(dirfd int) error {
			_, _, readErr = readWakeGenerationFileSnapshotAt(
				dirfd,
				fixture.agentDir,
				wakePreparedFileName,
				"wake prepared marker",
			)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		var changed *wakeSnapshotReadChangedError
		if !errors.As(readErr, &changed) {
			t.Fatalf("error = %v, want wakeSnapshotReadChangedError", readErr)
		}
	})
}

func readWakeStateSelectionForTest(t *testing.T, fixture wakeStateUnixFixture) (wakeStateReadSelection, error) {
	t.Helper()
	var selection wakeStateReadSelection
	err := fixture.agentDir.withFD(func(dirfd int) error {
		var err error
		selection, err = readWakeStateSelectionAt(
			dirfd,
			fixture.agentDir,
			fixture.root,
			fixture.agent,
		)
		return err
	})
	return selection, err
}

func publishWakeStateForDualReadTest(t *testing.T, fixture wakeStateUnixFixture) {
	t.Helper()
	expected := captureWakeStateLegacyForTest(t, fixture)
	if _, err := publishWakeStateForTest(fixture, expected); err != nil {
		t.Fatal(err)
	}
}

func republishWakeStateForPreparedTest(t *testing.T, fixture *authoritativeWakePreparedCleanupFixture) {
	t.Helper()
	stateFixture := wakeStateUnixFixture{
		root:     fixture.root,
		agent:    fixture.me,
		agentDir: fixture.agentDir,
	}
	publishWakeStateForDualReadTest(t, stateFixture)
}

func assertPreparedDualReadResult(t *testing.T, ready bool, err error, wantReady, wantError bool) {
	t.Helper()
	if (err != nil) != wantError {
		t.Fatalf("ready=%v error=%v, wantReady=%v wantError=%v", ready, err, wantReady, wantError)
	}
	if ready != wantReady {
		t.Fatalf("ready=%v error=%v, wantReady=%v", ready, err, wantReady)
	}
}
