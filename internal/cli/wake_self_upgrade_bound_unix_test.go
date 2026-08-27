//go:build darwin || linux

package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	wakeSelfUpgradeBoundProbeVersionEnv = "AMQ_TEST_WAKE_SELF_UPGRADE_BOUND_PROBE_VERSION"
	wakeSelfUpgradeBoundProbeModeEnv    = "AMQ_TEST_WAKE_SELF_UPGRADE_BOUND_PROBE_MODE"
	wakeSelfUpgradeBoundProbeFailMode   = "fail"
	wakeSelfUpgradeBoundProbeMutateMode = "mutate-after-version"
	wakeSelfUpgradeBoundProbeForkMode   = "fork-with-stdout"
)

func TestProbeBoundWakeSelfUpgradeVersion(t *testing.T) {
	binary := buildWakeSelfUpgradeBoundProbeBinary(t)
	const incumbentVersion = "1.0.0"

	tests := []struct {
		name             string
		tentativeVersion string
		probeVersion     string
		mode             string
		wantError        string
	}{
		{name: "exact newer version", tentativeVersion: "1.1.0", probeVersion: "1.1.0"},
		{name: "tentative version mismatch", tentativeVersion: "1.1.0", probeVersion: "1.1.1", wantError: "does not match tentative version"},
		{name: "equal version", tentativeVersion: incumbentVersion, probeVersion: incumbentVersion, wantError: "not strictly newer"},
		{name: "downgrade", tentativeVersion: "0.9.0", probeVersion: "0.9.0", wantError: "not strictly newer"},
		{name: "invalid semantic version", tentativeVersion: "not-a-semantic-version", probeVersion: "not-a-semantic-version", wantError: "not strictly newer"},
		{name: "probe failure", tentativeVersion: "1.2.0", probeVersion: "1.2.0", mode: wakeSelfUpgradeBoundProbeFailMode, wantError: "version probe failed"},
		{name: "post probe revalidation", tentativeVersion: "1.3.0", probeVersion: "1.3.0", mode: wakeSelfUpgradeBoundProbeMutateMode, wantError: "changed during preflight"},
		{name: "inherited stdout process", tentativeVersion: "1.4.0", probeVersion: "1.4.0", mode: wakeSelfUpgradeBoundProbeForkMode, wantError: "timed out"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate, err := captureWakeImageEvidence(binary, test.tentativeVersion)
			if err != nil {
				t.Fatal(err)
			}
			record := validWakeSelfUpgradeRecordForBoundProbeTest(t, candidate)
			bound, err := bindWakeRestartCandidateForRecord(record)
			if err != nil {
				t.Fatal(err)
			}
			defer closeWakeSelfUpgradeBoundProbeImage(t, bound, test.mode == wakeSelfUpgradeBoundProbeMutateMode)

			t.Setenv(wakeSelfUpgradeBoundProbeVersionEnv, test.probeVersion)
			t.Setenv(wakeSelfUpgradeBoundProbeModeEnv, test.mode)
			started := time.Now()
			err = probeBoundWakeSelfUpgradeVersion(bound, record, incumbentVersion)
			if test.mode == wakeSelfUpgradeBoundProbeForkMode &&
				time.Since(started) > wakeResumePreflightTimeout+2*time.Second {
				t.Fatalf("bounded probe took too long: %v", time.Since(started))
			}
			if test.wantError == "" {
				if err != nil {
					t.Fatalf("probe bound newer candidate: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("probe error = %v, want substring %q", err, test.wantError)
			}
		})
	}
}

func TestValidateWakeRestartRecordSelfUpgradeSources(t *testing.T) {
	binary := buildWakeSelfUpgradeBoundProbeBinary(t)
	candidate, err := captureWakeImageEvidence(binary, "1.1.0")
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name      string
		source    string
		wantError string
	}{
		{name: "absent defaults to foreign", source: ""},
		{name: "foreign", source: wakeRestartSourceForeign},
		{name: "self", source: wakeRestartSourceSelf},
		{name: "invalid", source: "operator", wantError: "source is invalid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			record := validWakeSelfUpgradeRecordForBoundProbeTest(t, candidate)
			record.Source = test.source
			err := validateWakeRestartRecord(record)
			if test.wantError == "" {
				if err != nil {
					t.Fatalf("validate source %q: %v", test.source, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("validate source %q error = %v, want substring %q", test.source, err, test.wantError)
			}
		})
	}
}

func validWakeSelfUpgradeRecordForBoundProbeTest(
	t *testing.T,
	candidate wakeImageEvidenceV1,
) wakeRestartRecord {
	t.Helper()
	return wakeRestartRecord{
		Schema:     wakeRestartSchemaV1,
		Source:     wakeRestartSourceSelf,
		RequestID:  "0123456789abcdef0123456789abcdef",
		Status:     wakeRestartPending,
		Root:       canonicalWakeRoot(secureTempDirForTest(t)),
		Agent:      "codex",
		Generation: "fedcba9876543210fedcba9876543210",
		Owner:      validWakeResumeOwnerForTest(),
		Candidate:  candidate,
	}
}

func closeWakeSelfUpgradeBoundProbeImage(
	t *testing.T,
	bound *wakeRestartBoundImage,
	stageWasReplaced bool,
) {
	t.Helper()
	if bound == nil {
		return
	}
	if stageWasReplaced && runtime.GOOS == "darwin" {
		if bound.file != nil {
			if err := bound.file.Close(); err != nil {
				t.Error(err)
			}
			bound.file = nil
		}
		if err := os.RemoveAll(filepath.Dir(bound.executionPath)); err != nil {
			t.Error(err)
		}
		return
	}
	if err := bound.close(); err != nil {
		t.Error(err)
	}
}

func buildWakeSelfUpgradeBoundProbeBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "bound-probe-helper.go")
	binary := filepath.Join(dir, "amq-bound-probe-helper")
	program := `package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

func main() {
	if len(os.Args) != 3 || os.Args[1] != "--no-update-check" || os.Args[2] != "--version" {
		os.Exit(91)
	}
	if os.Getenv("AMQ_TEST_WAKE_SELF_UPGRADE_BOUND_PROBE_MODE") == "fail" {
		os.Exit(92)
	}
	version := os.Getenv("AMQ_TEST_WAKE_SELF_UPGRADE_BOUND_PROBE_VERSION")
	mode := os.Getenv("AMQ_TEST_WAKE_SELF_UPGRADE_BOUND_PROBE_MODE")
	if mode == "mutate-after-version" {
		path, err := os.Executable()
		if err != nil {
			os.Exit(93)
		}
		if runtime.GOOS == "darwin" {
			replacement := filepath.Join(filepath.Dir(path), ".replacement")
			if err := os.WriteFile(replacement, []byte("replacement"), 0o700); err != nil {
				os.Exit(94)
			}
			if err := os.Rename(replacement, path); err != nil {
				os.Exit(95)
			}
		} else if err := os.Chmod(path, 0o500); err != nil {
			os.Exit(96)
		}
	}
	if mode == "fork-with-stdout" {
		child := exec.Command("sleep", "60")
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(97)
		}
		fmt.Println(version)
		if err := child.Wait(); err != nil {
			os.Exit(98)
		}
		return
	}
	fmt.Println(version)
}
`
	if err := os.WriteFile(source, []byte(program), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", binary, source)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("build bound probe helper timed out: %v\n%s", ctx.Err(), output)
	}
	if err != nil {
		t.Fatalf("build bound probe helper: %v\n%s", err, output)
	}
	signTestAMQ(t, binary)
	if err := os.Chmod(binary, 0o700); err != nil {
		t.Fatal(err)
	}
	return binary
}
