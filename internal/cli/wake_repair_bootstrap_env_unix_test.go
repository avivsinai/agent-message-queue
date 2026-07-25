//go:build darwin || linux

package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const wakeRepairBootstrapEnvHelper = "AMQ_TEST_WAKE_REPAIR_BOOTSTRAP_ENV_HELPER"

func TestWakeRepairBootstrapEnvironmentScrubbedBeforeHandoffValidation(t *testing.T) {
	t.Setenv(envWakeRepairHandoffReadFD, "3")
	t.Setenv(envWakeRepairHandoffWriteFD, "")
	t.Setenv(envWakeRepairAgentDirFD, "")
	t.Setenv(envWakeRepairInboxDirFD, "")

	loopCalled := false
	err := runWakeWithLoop(nil, func(wakeConfig) error {
		loopCalled = true
		return nil
	})
	if err == nil {
		t.Fatal("incomplete repair handoff unexpectedly succeeded")
	}
	if loopCalled {
		t.Fatal("wake loop ran with an invalid repair bootstrap")
	}
	for _, name := range []string{
		envWakeRepairHandoffReadFD,
		envWakeRepairHandoffWriteFD,
		envWakeRepairAgentDirFD,
		envWakeRepairInboxDirFD,
	} {
		if value, present := os.LookupEnv(name); present {
			t.Errorf("%s remained in the process environment as %q", name, value)
		}
	}
}

func TestWakePrivateStopEnvironmentScrubbedOnEveryPlatform(t *testing.T) {
	t.Setenv(envWakePrivateStopFD, "not-a-descriptor")

	loopCalled := false
	err := runWakeWithLoop(nil, func(wakeConfig) error {
		loopCalled = true
		return nil
	})
	if err == nil {
		t.Fatal("unsupported or malformed private stop bootstrap unexpectedly succeeded")
	}
	if loopCalled {
		t.Fatal("wake loop ran with an invalid private stop bootstrap")
	}
	if value, present := os.LookupEnv(envWakePrivateStopFD); present {
		t.Fatalf("%s remained in the process environment as %q", envWakePrivateStopFD, value)
	}
}

func TestWakeRepairBootstrapEnvironmentDoesNotReachInjectorOrNestedProcess(t *testing.T) {
	fds := make([]int, 4)
	for index := range fds {
		reader, writer, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		fd, err := unix.Dup(int(reader.Fd()))
		_ = reader.Close()
		if err != nil {
			_ = writer.Close()
			t.Fatalf("duplicate inherited handoff fd: %v", err)
		}
		t.Cleanup(func() { _ = writer.Close() })
		fds[index] = fd
	}
	t.Setenv(envWakeRepairHandoffReadFD, strconv.Itoa(fds[0]))
	t.Setenv(envWakeRepairHandoffWriteFD, strconv.Itoa(fds[1]))
	t.Setenv(envWakeRepairAgentDirFD, strconv.Itoa(fds[2]))
	t.Setenv(envWakeRepairInboxDirFD, strconv.Itoa(fds[3]))

	handoff, present, err := wakeRepairChildHandoffFromEnv()
	if err != nil {
		for _, fd := range fds {
			_ = unix.Close(fd)
		}
		t.Fatalf("initialize inherited child handoff: %v", err)
	}
	if !present {
		t.Fatal("inherited child handoff was not detected")
	}
	defer func() { _ = handoff.Close() }()

	assertWakeRepairBootstrapEnvironmentAbsent(t)
	assertWakeRepairDescriptorsClosedInInjector(t, fds)
	assertWakeRepairBootstrapEnvironmentAbsentInInjectorAndNestedProcess(t)
}

func TestWakeRepairBootstrapUnsetFailureFailsClosedAndAttemptsEveryScrub(t *testing.T) {
	names := wakeRepairBootstrapEnvNames()
	originalUnset := unsetWakeRepairBootstrapEnv
	t.Cleanup(func() { unsetWakeRepairBootstrapEnv = originalUnset })
	for _, failedName := range names {
		t.Run(failedName, func(t *testing.T) {
			for _, name := range names {
				t.Setenv(name, "9")
			}

			calls := make(map[string]int)
			unsetWakeRepairBootstrapEnv = func(name string) error {
				calls[name]++
				if name == failedName {
					return errors.New("injected unset failure")
				}
				return os.Unsetenv(name)
			}
			t.Cleanup(func() { unsetWakeRepairBootstrapEnv = originalUnset })

			loopCalled := false
			err := runWakeWithLoop(nil, func(wakeConfig) error {
				loopCalled = true
				return nil
			})
			if err == nil {
				t.Fatal("wake bootstrap proceeded after an environment scrub failure")
			}
			if !strings.Contains(err.Error(), failedName) {
				t.Fatalf("scrub failure did not identify %s: %v", failedName, err)
			}
			if loopCalled {
				t.Fatal("wake loop ran after an environment scrub failure")
			}
			for _, name := range names {
				if calls[name] != 1 {
					t.Errorf("unset %s called %d times, want 1", name, calls[name])
				}
				if name == failedName {
					continue
				}
				if value, present := os.LookupEnv(name); present {
					t.Errorf("%s remained after another variable failed to unset: %q", name, value)
				}
			}
		})
		unsetWakeRepairBootstrapEnv = originalUnset
	}
}

func assertWakeRepairBootstrapEnvironmentAbsent(t *testing.T) {
	t.Helper()
	for _, name := range wakeRepairBootstrapEnvNames() {
		if value, present := os.LookupEnv(name); present {
			t.Errorf("%s remained in the process environment as %q", name, value)
		}
	}
}

func assertWakeRepairBootstrapEnvironmentAbsentInInjectorAndNestedProcess(t *testing.T) {
	t.Helper()
	dir := secureTempDirForTest(t)
	output := filepath.Join(dir, "inherited-repair-environment")
	if err := os.WriteFile(output, nil, 0o600); err != nil {
		t.Fatalf("create repair environment inspection output: %v", err)
	}
	t.Setenv(injectViaHelperEnv, "1")
	t.Setenv(wakeRepairBootstrapEnvHelper, "injector")
	if err := injectVia(&wakeConfig{
		injectVia: copyTestBinaryForInjectVia(t),
		injectArgs: []string{
			"-test.run=^TestWakeRepairBootstrapEnvironmentInspectorHelperProcess$",
			"--",
			output,
		},
		injectTimeout: 10 * time.Second,
	}, "ignored wake payload"); err != nil {
		t.Fatalf("run repair environment inspector: %v", err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("read repair environment inspection: %v", err)
	}
	if len(data) != 0 {
		t.Fatalf("private repair bootstrap environment leaked to a descendant:\n%s", data)
	}
}

func TestWakeRepairBootstrapEnvironmentInspectorHelperProcess(t *testing.T) {
	mode := os.Getenv(wakeRepairBootstrapEnvHelper)
	if mode == "" {
		return
	}
	separator := -1
	for index, arg := range os.Args {
		if arg == "--" {
			separator = index
			break
		}
	}
	if separator < 0 || separator+1 >= len(os.Args) {
		_, _ = os.Stderr.WriteString("missing repair environment inspector output\n")
		os.Exit(2)
	}
	output := os.Args[separator+1]
	var leaked []string
	for _, name := range wakeRepairBootstrapEnvNames() {
		if _, present := os.LookupEnv(name); present {
			leaked = append(leaked, mode+":"+name)
		}
	}
	if len(leaked) > 0 {
		file, err := os.OpenFile(output, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			_, _ = os.Stderr.WriteString("open repair environment inspection: " + err.Error() + "\n")
			os.Exit(3)
		}
		_, writeErr := file.WriteString(strings.Join(leaked, "\n") + "\n")
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			_, _ = os.Stderr.WriteString("write repair environment inspection\n")
			os.Exit(3)
		}
	}
	if mode == "injector" {
		cmd := exec.Command(
			os.Args[0],
			"-test.run=^TestWakeRepairBootstrapEnvironmentInspectorHelperProcess$",
			"--",
			output,
		)
		cmd.Env = setEnvVar(os.Environ(), wakeRepairBootstrapEnvHelper, "nested")
		if out, err := cmd.CombinedOutput(); err != nil {
			_, _ = os.Stderr.WriteString("run nested repair environment inspector: " + err.Error() + ": " + string(out) + "\n")
			os.Exit(4)
		}
	}
	os.Exit(0)
}
