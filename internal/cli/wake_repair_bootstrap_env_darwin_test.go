//go:build darwin

package cli

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestDarwinWakeRepairBootstrapRejectsCrossCapabilityAliasesAtomically(t *testing.T) {
	commonNames := []string{
		envWakeRepairHandoffReadFD,
		envWakeRepairHandoffWriteFD,
		envWakeRepairAgentDirFD,
		envWakeRepairInboxDirFD,
	}
	for aliasIndex, aliasName := range commonNames {
		t.Run(aliasName, func(t *testing.T) {
			fds := openWakeRepairBootstrapDescriptors(t, 4)
			setWakeRepairBootstrapDescriptors(t, commonNames, fds)
			t.Setenv(envWakeRepairChildControlFD, strconv.Itoa(fds[aliasIndex]))

			bootstrap, err := captureAndScrubWakeRepairBootstrapEnv()
			if err != nil {
				t.Fatalf("capture repair bootstrap: %v", err)
			}
			handoff, present, stop, cleanup, err := wakeRepairChildCapabilitiesFromBootstrap(bootstrap)
			if cleanup != nil {
				cleanup()
			}
			if handoff != nil || present || stop != nil {
				t.Fatalf("alias returned partial capabilities: handoff=%v present=%v stop=%v", handoff, present, stop)
			}
			want := fmt.Sprintf(
				"%s aliases %s",
				envWakeRepairChildControlFD,
				aliasName,
			)
			if err == nil || err.Error() != want {
				t.Fatalf("alias error = %v, want %q", err, want)
			}
			assertWakeRepairBootstrapDescriptorsUnchanged(t, fds)
			assertWakeRepairBootstrapEnvironmentAbsent(t)
		})
	}
}

func TestDarwinWakeRepairBootstrapRejectsEveryCommonAliasPairBeforeMutation(t *testing.T) {
	commonNames := []string{
		envWakeRepairHandoffReadFD,
		envWakeRepairHandoffWriteFD,
		envWakeRepairAgentDirFD,
		envWakeRepairInboxDirFD,
	}
	pairs := []struct {
		first  int
		second int
	}{
		{first: 0, second: 1},
		{first: 0, second: 2},
		{first: 0, second: 3},
		{first: 1, second: 2},
		{first: 1, second: 3},
		{first: 2, second: 3},
	}
	for _, pair := range pairs {
		name := commonNames[pair.first] + "__" + commonNames[pair.second]
		t.Run(name, func(t *testing.T) {
			fds := openWakeRepairBootstrapDescriptors(t, 5)
			commonFDs := make([]int, len(commonNames))
			nextUnique := 1
			for index := range commonFDs {
				if index == pair.first || index == pair.second {
					commonFDs[index] = fds[0]
					continue
				}
				commonFDs[index] = fds[nextUnique]
				nextUnique++
			}
			setWakeRepairBootstrapDescriptors(t, commonNames, commonFDs)
			t.Setenv(envWakeRepairChildControlFD, strconv.Itoa(fds[3]))
			t.Setenv(envWakePrivateStopFD, strconv.Itoa(fds[4]))

			bootstrap, err := captureAndScrubWakeRepairBootstrapEnv()
			if err != nil {
				t.Fatalf("capture aliased private bootstrap: %v", err)
			}
			handoff, present, stop, cleanup, err :=
				wakeRepairChildCapabilitiesFromBootstrap(bootstrap)
			if cleanup != nil {
				cleanup()
			}
			if handoff != nil {
				_ = handoff.Close()
			}
			if handoff != nil || present || stop != nil {
				t.Fatalf(
					"common alias returned capabilities: handoff=%v present=%v stop=%v",
					handoff,
					present,
					stop,
				)
			}
			const want = "wake repair handoff descriptors must be distinct"
			if err == nil || err.Error() != want {
				t.Fatalf("common alias error = %v, want %q", err, want)
			}
			assertWakeRepairBootstrapDescriptorsUnchanged(t, fds)
			assertWakeRepairBootstrapEnvironmentAbsent(t)

			setWakeRepairBootstrapDescriptors(t, commonNames, commonFDs)
			t.Setenv(envWakeRepairChildControlFD, strconv.Itoa(fds[3]))
			t.Setenv(envWakePrivateStopFD, strconv.Itoa(fds[4]))
			loopCalled := false
			err = runWakeWithLoop(nil, func(wakeConfig) error {
				loopCalled = true
				return nil
			})
			if err == nil || err.Error() != want {
				t.Fatalf("common alias loop error = %v, want %q", err, want)
			}
			if loopCalled {
				t.Fatal("wake loop ran with common bootstrap aliases")
			}
			assertWakeRepairBootstrapDescriptorsUnchanged(t, fds)
			assertWakeRepairBootstrapEnvironmentAbsent(t)
		})
	}
}

func TestDarwinWakePrivateBootstrapPreparationFailureRollsBackWholeTuple(t *testing.T) {
	commonNames := []string{
		envWakeRepairHandoffReadFD,
		envWakeRepairHandoffWriteFD,
		envWakeRepairAgentDirFD,
		envWakeRepairInboxDirFD,
	}
	t.Run("transaction capabilities", func(t *testing.T) {
		fds := openWakeRepairBootstrapDescriptors(t, 6)
		setWakeRepairBootstrapDescriptors(t, commonNames, fds[:4])
		t.Setenv(envWakeRepairChildControlFD, strconv.Itoa(fds[4]))
		t.Setenv(envWakePrivateStopFD, strconv.Itoa(fds[5]))
		if err := unix.Close(fds[4]); err != nil {
			t.Fatalf("close later child control descriptor: %v", err)
		}

		bootstrap, err := captureAndScrubWakeRepairBootstrapEnv()
		if err != nil {
			t.Fatalf("capture preparation-failure bootstrap: %v", err)
		}
		handoff, present, stop, cleanup, err :=
			wakeRepairChildCapabilitiesFromBootstrap(bootstrap)
		if cleanup != nil {
			cleanup()
		}
		if handoff != nil {
			_ = handoff.Close()
		}
		if handoff != nil || present || stop != nil {
			t.Fatalf(
				"preparation failure returned capabilities: handoff=%v present=%v stop=%v",
				handoff,
				present,
				stop,
			)
		}
		if err == nil || !strings.HasPrefix(
			err.Error(),
			"make wake repair child control fd nonblocking:",
		) {
			t.Fatalf("preparation failure error = %v", err)
		}
		assertWakeRepairBootstrapDescriptorsClosed(t, fds)
		assertWakeRepairBootstrapEnvironmentAbsent(t)
	})

	t.Run("wake loop", func(t *testing.T) {
		fds := openWakeRepairBootstrapDescriptors(t, 6)
		setWakeRepairBootstrapDescriptors(t, commonNames, fds[:4])
		t.Setenv(envWakeRepairChildControlFD, strconv.Itoa(fds[4]))
		t.Setenv(envWakePrivateStopFD, strconv.Itoa(fds[5]))
		if err := unix.Close(fds[4]); err != nil {
			t.Fatalf("close later child control descriptor: %v", err)
		}

		loopCalled := false
		err := runWakeWithLoop(nil, func(wakeConfig) error {
			loopCalled = true
			return nil
		})
		if err == nil || !strings.HasPrefix(
			err.Error(),
			"make wake repair child control fd nonblocking:",
		) {
			t.Fatalf("preparation failure loop error = %v", err)
		}
		if loopCalled {
			t.Fatal("wake loop ran after private bootstrap preparation failed")
		}
		assertWakeRepairBootstrapDescriptorsClosed(t, fds)
		assertWakeRepairBootstrapEnvironmentAbsent(t)
	})
}

func TestDarwinWakePrivateStopRejectsEveryBootstrapAliasAtomically(t *testing.T) {
	otherNames := []string{
		envWakeRepairHandoffReadFD,
		envWakeRepairHandoffWriteFD,
		envWakeRepairAgentDirFD,
		envWakeRepairInboxDirFD,
		envWakeRepairChildControlFD,
	}
	commonNames := otherNames[:4]
	for aliasIndex, aliasName := range otherNames {
		t.Run(aliasName, func(t *testing.T) {
			fds := openWakeRepairBootstrapDescriptors(t, 5)
			setWakeRepairBootstrapDescriptors(t, commonNames, fds[:4])
			t.Setenv(envWakeRepairChildControlFD, strconv.Itoa(fds[4]))
			t.Setenv(envWakePrivateStopFD, strconv.Itoa(fds[aliasIndex]))

			bootstrap, err := captureAndScrubWakeRepairBootstrapEnv()
			if err != nil {
				t.Fatalf("capture private bootstrap: %v", err)
			}
			handoff, present, stop, cleanup, err :=
				wakeRepairChildCapabilitiesFromBootstrap(bootstrap)
			if cleanup != nil {
				cleanup()
			}
			if handoff != nil {
				_ = handoff.Close()
			}
			if handoff != nil || present || stop != nil {
				t.Fatalf(
					"owner alias returned partial capabilities: handoff=%v present=%v stop=%v",
					handoff,
					present,
					stop,
				)
			}
			want := fmt.Sprintf("%s aliases %s", envWakePrivateStopFD, aliasName)
			if err == nil || err.Error() != want {
				t.Fatalf("owner alias error = %v, want %q", err, want)
			}
			assertWakeRepairBootstrapDescriptorsUnchanged(t, fds)
			assertWakeRepairBootstrapEnvironmentAbsent(t)
		})
	}
}

func TestDarwinWakePrivateStopMalformedFailsBeforeAdoption(t *testing.T) {
	t.Setenv(envWakePrivateStopFD, "not-a-descriptor")

	bootstrap, err := captureAndScrubWakeRepairBootstrapEnv()
	if err != nil {
		t.Fatalf("capture private bootstrap: %v", err)
	}
	handoff, present, stop, cleanup, err :=
		wakeRepairChildCapabilitiesFromBootstrap(bootstrap)
	if cleanup != nil {
		cleanup()
	}
	if handoff != nil {
		_ = handoff.Close()
	}
	if handoff != nil || present || stop != nil {
		t.Fatalf(
			"malformed owner returned partial capabilities: handoff=%v present=%v stop=%v",
			handoff,
			present,
			stop,
		)
	}
	want := envWakePrivateStopFD + " is invalid"
	if err == nil || err.Error() != want {
		t.Fatalf("malformed owner error = %v, want %q", err, want)
	}
	if value, present := os.LookupEnv(envWakePrivateStopFD); present {
		t.Fatalf("%s remained in the process environment as %q", envWakePrivateStopFD, value)
	}
}

func TestDarwinWakePrivateBootstrapAdoptsDistinctSixDescriptorTuple(t *testing.T) {
	commonNames := []string{
		envWakeRepairHandoffReadFD,
		envWakeRepairHandoffWriteFD,
		envWakeRepairAgentDirFD,
		envWakeRepairInboxDirFD,
	}
	fds := openWakeRepairBootstrapDescriptors(t, 6)
	setWakeRepairBootstrapDescriptors(t, commonNames, fds[:4])
	t.Setenv(envWakeRepairChildControlFD, strconv.Itoa(fds[4]))
	t.Setenv(envWakePrivateStopFD, strconv.Itoa(fds[5]))

	bootstrap, err := captureAndScrubWakeRepairBootstrapEnv()
	if err != nil {
		t.Fatalf("capture private bootstrap: %v", err)
	}
	handoff, present, stop, cleanup, err :=
		wakeRepairChildCapabilitiesFromBootstrap(bootstrap)
	if err != nil {
		t.Fatalf("adopt distinct private bootstrap tuple: %v", err)
	}
	if handoff == nil || !present || stop == nil || cleanup == nil {
		t.Fatalf(
			"complete tuple capabilities: handoff=%v present=%v stop=%v cleanup-present=%v",
			handoff,
			present,
			stop,
			cleanup != nil,
		)
	}
	defer cleanup()
	defer func() { _ = handoff.Close() }()
	assertWakeRepairBootstrapEnvironmentAbsent(t)
	select {
	case <-stop:
		t.Fatal("complete private bootstrap stopped before either writer closed")
	default:
	}
}

func TestDarwinWakePrivateStopBootstrapDoesNotReachDescendants(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	fd, err := unix.Dup(int(reader.Fd()))
	_ = reader.Close()
	if err != nil {
		t.Fatalf("duplicate inherited private stop fd: %v", err)
	}
	t.Setenv(envWakePrivateStopFD, strconv.Itoa(fd))

	bootstrap, err := captureAndScrubWakeRepairBootstrapEnv()
	if err != nil {
		_ = unix.Close(fd)
		t.Fatalf("capture private bootstrap: %v", err)
	}
	handoff, present, stop, cleanup, err :=
		wakeRepairChildCapabilitiesFromBootstrap(bootstrap)
	if err != nil {
		_ = unix.Close(fd)
		t.Fatalf("initialize private stop bootstrap: %v", err)
	}
	if handoff != nil || present {
		t.Fatalf("owner-only bootstrap returned repair handoff: handoff=%v present=%v", handoff, present)
	}
	defer cleanup()
	if stop == nil {
		t.Fatal("owner-only bootstrap did not return a stop channel")
	}
	assertWakeRepairBootstrapEnvironmentAbsent(t)
	assertWakeRepairDescriptorsClosedInInjector(t, []int{fd})
	assertWakeRepairBootstrapEnvironmentAbsentInInjectorAndNestedProcess(t)
	select {
	case <-stop:
		t.Fatal("private owner stop fired before its writer closed")
	default:
	}
}

func TestDarwinWakeRepairBootstrapRejectsMalformedTupleBeforeAdoption(t *testing.T) {
	commonNames := []string{
		envWakeRepairHandoffReadFD,
		envWakeRepairHandoffWriteFD,
		envWakeRepairAgentDirFD,
		envWakeRepairInboxDirFD,
	}
	tests := []struct {
		name        string
		common      []string
		control     string
		owner       string
		wantError   string
		expectedFDs int
	}{
		{
			name:        "incomplete handoff with valid control",
			common:      []string{"fd", "", "", ""},
			control:     "fd",
			owner:       "fd",
			wantError:   "wake repair handoff descriptors are incomplete",
			expectedFDs: 3,
		},
		{
			name:        "malformed handoff with valid control",
			common:      []string{"invalid", "fd", "fd", "fd"},
			control:     "fd",
			owner:       "fd",
			wantError:   envWakeRepairHandoffReadFD + " is invalid",
			expectedFDs: 5,
		},
		{
			name:        "valid handoff with malformed control",
			common:      []string{"fd", "fd", "fd", "fd"},
			control:     "invalid",
			owner:       "fd",
			wantError:   envWakeRepairChildControlFD + " is invalid",
			expectedFDs: 5,
		},
		{
			name:        "valid repair tuple with malformed owner stop",
			common:      []string{"fd", "fd", "fd", "fd"},
			control:     "fd",
			owner:       "invalid",
			wantError:   envWakePrivateStopFD + " is invalid",
			expectedFDs: 5,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fds := openWakeRepairBootstrapDescriptors(t, test.expectedFDs)
			nextFD := 0
			for index, value := range test.common {
				switch value {
				case "":
					t.Setenv(commonNames[index], "")
				case "invalid":
					t.Setenv(commonNames[index], "not-a-descriptor")
				case "fd":
					t.Setenv(commonNames[index], strconv.Itoa(fds[nextFD]))
					nextFD++
				}
			}
			switch test.control {
			case "invalid":
				t.Setenv(envWakeRepairChildControlFD, "not-a-descriptor")
			case "fd":
				t.Setenv(envWakeRepairChildControlFD, strconv.Itoa(fds[nextFD]))
				nextFD++
			}
			switch test.owner {
			case "fd":
				t.Setenv(envWakePrivateStopFD, strconv.Itoa(fds[nextFD]))
			case "invalid":
				t.Setenv(envWakePrivateStopFD, "not-a-descriptor")
			}

			bootstrap, err := captureAndScrubWakeRepairBootstrapEnv()
			if err != nil {
				t.Fatalf("capture repair bootstrap: %v", err)
			}
			handoff, present, stop, cleanup, err := wakeRepairChildCapabilitiesFromBootstrap(bootstrap)
			if cleanup != nil {
				cleanup()
			}
			if handoff != nil || present || stop != nil {
				t.Fatalf("invalid tuple returned partial capabilities: handoff=%v present=%v stop=%v", handoff, present, stop)
			}
			if err == nil || err.Error() != test.wantError {
				t.Fatalf("bootstrap error = %v, want %q", err, test.wantError)
			}
			assertWakeRepairBootstrapDescriptorsUnchanged(t, fds)
			assertWakeRepairBootstrapEnvironmentAbsent(t)
		})
	}
}

func TestDarwinWakeRepairBootstrapRequiresCompletePlatformTuple(t *testing.T) {
	commonNames := []string{
		envWakeRepairHandoffReadFD,
		envWakeRepairHandoffWriteFD,
		envWakeRepairAgentDirFD,
		envWakeRepairInboxDirFD,
	}
	tests := []struct {
		name           string
		includeHandoff bool
		includeControl bool
	}{
		{
			name:           "handoff without control",
			includeHandoff: true,
		},
		{
			name:           "control without handoff",
			includeControl: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			count := 1
			if test.includeHandoff {
				count = 4
			}
			fds := openWakeRepairBootstrapDescriptors(t, count)
			if test.includeHandoff {
				setWakeRepairBootstrapDescriptors(t, commonNames, fds)
			}
			if test.includeControl {
				t.Setenv(envWakeRepairChildControlFD, strconv.Itoa(fds[0]))
			}

			bootstrap, err := captureAndScrubWakeRepairBootstrapEnv()
			if err != nil {
				t.Fatalf("capture repair bootstrap: %v", err)
			}
			handoff, present, stop, cleanup, err :=
				wakeRepairChildCapabilitiesFromBootstrap(bootstrap)
			if cleanup != nil {
				cleanup()
			}
			if handoff != nil || present || stop != nil {
				t.Fatalf(
					"incomplete tuple returned partial capabilities: handoff=%v present=%v stop=%v",
					handoff,
					present,
					stop,
				)
			}
			const want = "wake repair bootstrap descriptors are incomplete"
			if err == nil || err.Error() != want {
				t.Fatalf("tuple validation error = %v, want %q", err, want)
			}
			assertWakeRepairBootstrapDescriptorsUnchanged(t, fds)
			assertWakeRepairBootstrapEnvironmentAbsent(t)
		})
	}
}

func TestDarwinWakeRepairBootstrapAliasDoesNotEnterWakeLoop(t *testing.T) {
	commonNames := []string{
		envWakeRepairHandoffReadFD,
		envWakeRepairHandoffWriteFD,
		envWakeRepairAgentDirFD,
		envWakeRepairInboxDirFD,
	}
	fds := openWakeRepairBootstrapDescriptors(t, 4)
	setWakeRepairBootstrapDescriptors(t, commonNames, fds)
	t.Setenv(envWakeRepairChildControlFD, strconv.Itoa(fds[0]))

	loopCalled := false
	err := runWakeWithLoop(nil, func(wakeConfig) error {
		loopCalled = true
		return nil
	})
	want := envWakeRepairChildControlFD + " aliases " + envWakeRepairHandoffReadFD
	if err == nil || err.Error() != want {
		t.Fatalf("alias error = %v, want %q", err, want)
	}
	if loopCalled {
		t.Fatal("wake loop ran with an aliased repair bootstrap")
	}
	assertWakeRepairBootstrapDescriptorsUnchanged(t, fds)
	assertWakeRepairBootstrapEnvironmentAbsent(t)
}

func openWakeRepairBootstrapDescriptors(t *testing.T, count int) []int {
	t.Helper()
	fds := make([]int, 0, count)
	for range count {
		reader, writer, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		fd, err := unix.Dup(int(reader.Fd()))
		_ = reader.Close()
		if err != nil {
			_ = writer.Close()
			t.Fatalf("duplicate repair bootstrap descriptor: %v", err)
		}
		t.Cleanup(func() {
			_ = unix.Close(fd)
			_ = writer.Close()
		})
		fds = append(fds, fd)
	}
	return fds
}

func setWakeRepairBootstrapDescriptors(t *testing.T, names []string, fds []int) {
	t.Helper()
	for index, name := range names {
		t.Setenv(name, strconv.Itoa(fds[index]))
	}
}

func assertWakeRepairBootstrapDescriptorsUnchanged(t *testing.T, fds []int) {
	t.Helper()
	for _, fd := range fds {
		fdFlags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
		if err != nil {
			t.Errorf("descriptor %d was closed before bootstrap admission: %v", fd, err)
			continue
		}
		if fdFlags&unix.FD_CLOEXEC != 0 {
			t.Errorf("descriptor %d became close-on-exec before bootstrap admission", fd)
		}
		statusFlags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
		if err != nil {
			t.Errorf("inspect descriptor %d status: %v", fd, err)
			continue
		}
		if statusFlags&unix.O_NONBLOCK != 0 {
			t.Errorf("descriptor %d became nonblocking before bootstrap admission", fd)
		}
	}
}

func assertWakeRepairBootstrapDescriptorsClosed(t *testing.T, fds []int) {
	t.Helper()
	for _, fd := range fds {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
			t.Errorf("descriptor %d remained open after transaction rollback: %v", fd, err)
		}
	}
}

func TestDarwinWakeRepairControlEnvironmentScrubbedBeforeHandoffValidation(t *testing.T) {
	t.Setenv(envWakeRepairHandoffReadFD, "3")
	t.Setenv(envWakeRepairHandoffWriteFD, "")
	t.Setenv(envWakeRepairAgentDirFD, "")
	t.Setenv(envWakeRepairInboxDirFD, "")
	t.Setenv(envWakeRepairChildControlFD, "not-a-descriptor")

	err := runWakeWithLoop(nil, func(wakeConfig) error {
		t.Fatal("wake loop ran with an invalid repair bootstrap")
		return nil
	})
	if err == nil {
		t.Fatal("invalid repair bootstrap unexpectedly succeeded")
	}
	if value, present := os.LookupEnv(envWakeRepairChildControlFD); present {
		t.Fatalf("%s remained in the process environment as %q", envWakeRepairChildControlFD, value)
	}
}

func TestDarwinWakeRepairEnvironmentScrubbedBeforeOwnerValidation(t *testing.T) {
	t.Setenv(envWakePrivateStopFD, "not-a-descriptor")
	t.Setenv(envWakeRepairChildControlFD, "also-not-a-descriptor")

	err := runWakeWithLoop(nil, func(wakeConfig) error {
		t.Fatal("wake loop ran with an invalid private owner bootstrap")
		return nil
	})
	if err == nil {
		t.Fatal("invalid private owner bootstrap unexpectedly succeeded")
	}
	if value, present := os.LookupEnv(envWakeRepairChildControlFD); present {
		t.Fatalf("%s remained in the process environment as %q", envWakeRepairChildControlFD, value)
	}
}

func TestDarwinWakeRepairControlEnvironmentDoesNotReachDescendants(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	childFD, err := unix.Dup(int(reader.Fd()))
	_ = reader.Close()
	if err != nil {
		t.Fatalf("duplicate inherited child control fd: %v", err)
	}
	t.Setenv(envWakeRepairChildControlFD, strconv.Itoa(childFD))

	stop, cleanup, err := wakeRepairChildStopFromEnv()
	if err != nil {
		_ = unix.Close(childFD)
		t.Fatalf("initialize child control: %v", err)
	}
	defer cleanup()
	assertWakeRepairBootstrapEnvironmentAbsent(t)
	assertWakeRepairDescriptorsClosedInInjector(t, []int{childFD})
	assertWakeRepairBootstrapEnvironmentAbsentInInjectorAndNestedProcess(t)
	select {
	case <-stop:
		t.Fatal("child control stopped before its writer closed")
	default:
	}
}
