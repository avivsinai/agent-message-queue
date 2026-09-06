package amq

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestStartWakeWaitsForReadyFileAndPassesTarget(t *testing.T) {
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "args.log")
	t.Setenv("AMQ_KEEPALIVE_ARGS_LOG", argsLog)
	fakeAMQ := writeStartWakeExecutable(t, filepath.Join(dir, "amq"), `#!/bin/sh
printf '%s\n' "$@" > "$AMQ_KEEPALIVE_ARGS_LOG"
ready=""
previous=""
for arg in "$@"; do
  if [ "$previous" = "-ready-file" ]; then
    ready="$arg"
  fi
  previous="$arg"
done
if [ -z "$ready" ]; then
  exit 11
fi
umask 077
printf '%s\n' '{"schema":1,"generation":"test-generation","target_digest":"test-digest"}' > "$ready"
`)

	err := NewCLI(fakeAMQ).StartWake(context.Background(), StartWakeRequest{
		Root:      "/tmp/amq-root",
		Me:        "codex",
		InjectVia: "/tmp/amq-keepalive",
		Adapter:   "ghostty",
		Target:    "ghostty:terminal:abc",
		Timeout:   5 * time.Second,
	})
	if err != nil {
		t.Fatalf("StartWake() error = %v", err)
	}
	data, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatalf("read args log: %v", err)
	}
	got := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	readyIndex := -1
	for index, arg := range got {
		if arg == "-ready-file" {
			readyIndex = index
			break
		}
	}
	if readyIndex < 0 || readyIndex+1 >= len(got) {
		t.Fatalf("argv has no ready-file value: %#v", got)
	}
	if !filepath.IsAbs(got[readyIndex+1]) {
		t.Fatalf("ready-file path = %q, want absolute", got[readyIndex+1])
	}
	got[readyIndex+1] = "<ready-file>"
	want := []string{
		"wake",
		"-root", "/tmp/amq-root",
		"-me", "codex",
		"-inject-via", "/tmp/amq-keepalive",
		"-inject-arg", "inject",
		"-inject-arg", "ghostty",
		"-inject-arg", "ghostty:terminal:abc",
		"--retry-until", "injected",
		"--accept-existing-wake",
		"-ready-file", "<ready-file>",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %#v\nwant %#v", got, want)
	}
	// keepalive starts a wake only when none is live, so mail already waiting in
	// inbox/new arrived while nothing was notifying. Baselining it here made those
	// downtime arrivals permanently silent, so the flag must be absent by default.
	if strings.Contains(string(data), "--baseline-existing\n") {
		t.Fatalf("args log must not baseline downtime arrivals by default:\n%s", data)
	}
}

func TestStartWakeDoesNotInheritCoopOwnerToken(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AMQ_WAKE_OWNER", "owner-for-the-calling-session")
	fakeAMQ := writeStartWakeExecutable(t, filepath.Join(dir, "amq"), `#!/bin/sh
if [ "${AMQ_WAKE_OWNER+x}" = x ]; then
  printf 'inherited AMQ_WAKE_OWNER\n' >&2
  exit 12
fi
ready=""
previous=""
for arg in "$@"; do
  if [ "$previous" = "-ready-file" ]; then ready="$arg"; fi
  previous="$arg"
done
umask 077
printf '%s\n' '{"schema":1,"generation":"test-generation","target_digest":"test-digest"}' > "$ready"
`)

	err := NewCLI(fakeAMQ).StartWake(context.Background(), StartWakeRequest{
		Root:      "/tmp/amq-root",
		Me:        "claude",
		InjectVia: "/tmp/amq-keepalive",
		Adapter:   "cmux",
		Target:    "cmux:surface:B8A8C4A7-3C88-4DAD-93BE-97E9701D07D2",
		Timeout:   5 * time.Second,
	})
	if err != nil {
		t.Fatalf("StartWake() error = %v", err)
	}
}

func TestStartWakeRefreshesOnlyConclusivelyDifferentLiveImages(t *testing.T) {
	cases := []struct {
		name          string
		checkOutput   string
		checkExit     string
		retireOutput  string
		retireExit    string
		wantCalls     []string
		wantErrIs     error
		wantErrString string
	}{
		{
			name:         "different image retires then starts",
			checkOutput:  `{"schema":1,"live_wake":true,"image_status":"different","wake_generation":"0123456789abcdef0123456789abcdef"}`,
			retireOutput: `{"status":"retired","agent":"codex","pid":4242}`,
			wantCalls:    []string{"check", "retire", "start"},
		},
		{
			name:          "different image without generation preserves wake",
			checkOutput:   `{"schema":1,"live_wake":true,"image_status":"different"}`,
			wantCalls:     []string{"check"},
			wantErrIs:     ErrWakeImageIdentityInconclusive,
			wantErrString: "omitted generation",
		},
		{
			name:        "current image starts without retire",
			checkOutput: `{"schema":1,"live_wake":true,"image_status":"current"}`,
			wantCalls:   []string{"check", "start"},
		},
		{
			name:        "no live wake starts",
			checkOutput: `{"schema":1,"live_wake":false,"image_status":"unknown"}`,
			wantCalls:   []string{"check", "start"},
		},
		{
			name:        "unknown live image preserves wake",
			checkOutput: `{"schema":1,"live_wake":true,"image_status":"unknown"}`,
			wantCalls:   []string{"check"},
			wantErrIs:   ErrWakeImageIdentityInconclusive,
		},
		{
			name:        "unrecognized live image status preserves wake",
			checkOutput: `{"schema":1,"live_wake":true,"image_status":"future"}`,
			wantCalls:   []string{"check"},
			wantErrIs:   ErrWakeImageIdentityInconclusive,
		},
		{
			name:        "schema mismatch is inconclusive",
			checkOutput: `{"schema":2,"live_wake":true,"image_status":"different"}`,
			wantCalls:   []string{"check"},
			wantErrIs:   ErrWakeImageIdentityInconclusive,
		},
		{
			name:          "missing schema is inconclusive",
			checkOutput:   `{"live_wake":false,"image_status":"unknown"}`,
			wantCalls:     []string{"check"},
			wantErrIs:     ErrWakeImageIdentityInconclusive,
			wantErrString: "omitted required",
		},
		{
			name:          "missing live wake is inconclusive",
			checkOutput:   `{"schema":1,"image_status":"unknown"}`,
			wantCalls:     []string{"check"},
			wantErrIs:     ErrWakeImageIdentityInconclusive,
			wantErrString: "omitted required",
		},
		{
			name:          "missing image status is inconclusive",
			checkOutput:   `{"schema":1,"live_wake":false}`,
			wantCalls:     []string{"check"},
			wantErrIs:     ErrWakeImageIdentityInconclusive,
			wantErrString: "omitted required",
		},
		{
			name:          "malformed check is inconclusive",
			checkOutput:   `not-json`,
			wantCalls:     []string{"check"},
			wantErrIs:     ErrWakeImageIdentityInconclusive,
			wantErrString: "parse amq wake check output",
		},
		{
			name:          "failed check is inconclusive",
			checkExit:     "7",
			wantCalls:     []string{"check"},
			wantErrIs:     ErrWakeImageIdentityInconclusive,
			wantErrString: "amq wake check failed",
		},
		{
			name:          "retirement refusal does not start",
			checkOutput:   `{"schema":1,"live_wake":true,"image_status":"different","wake_generation":"0123456789abcdef0123456789abcdef"}`,
			retireOutput:  `{"status":"refused","reason":"target identity changed"}`,
			retireExit:    "1",
			wantCalls:     []string{"check", "retire"},
			wantErrIs:     ErrWakeRetireNotConfirmed,
			wantErrString: "target identity changed",
		},
		{
			name:          "retired output with failed exit does not start",
			checkOutput:   `{"schema":1,"live_wake":true,"image_status":"different","wake_generation":"0123456789abcdef0123456789abcdef"}`,
			retireOutput:  `{"status":"retired","agent":"codex","pid":4242}`,
			retireExit:    "1",
			wantCalls:     []string{"check", "retire"},
			wantErrIs:     ErrWakeRetireNotConfirmed,
			wantErrString: "exited unsuccessfully",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			callsLog := filepath.Join(dir, "calls.log")
			checkExit := tc.checkExit
			if checkExit == "" {
				checkExit = "0"
			}
			retireExit := tc.retireExit
			if retireExit == "" {
				retireExit = "0"
			}
			t.Setenv("AMQ_KEEPALIVE_CALLS_LOG", callsLog)
			t.Setenv("AMQ_KEEPALIVE_CHECK_OUTPUT", tc.checkOutput)
			t.Setenv("AMQ_KEEPALIVE_CHECK_EXIT", checkExit)
			t.Setenv("AMQ_KEEPALIVE_RETIRE_OUTPUT", tc.retireOutput)
			t.Setenv("AMQ_KEEPALIVE_RETIRE_EXIT", retireExit)
			fakeAMQ := writeExecutable(t, filepath.Join(dir, "amq"), `#!/bin/sh
operation=start
if [ "$1" = "wake" ] && [ "$2" = "check" ]; then
  operation=check
elif [ "$1" = "wake" ] && [ "$2" = "retire" ]; then
  operation=retire
fi
printf '%s\t%s\n' "$operation" "$*" >> "$AMQ_KEEPALIVE_CALLS_LOG"
case "$operation" in
  check)
    printf '%s\n' "$AMQ_KEEPALIVE_CHECK_OUTPUT"
    exit "$AMQ_KEEPALIVE_CHECK_EXIT"
    ;;
  retire)
    printf '%s\n' "$AMQ_KEEPALIVE_RETIRE_OUTPUT"
    exit "$AMQ_KEEPALIVE_RETIRE_EXIT"
    ;;
esac
ready=""
previous=""
for arg in "$@"; do
  if [ "$previous" = "-ready-file" ]; then ready="$arg"; fi
  previous="$arg"
done
[ -n "$ready" ] || exit 11
umask 077
printf '%s\n' '{"schema":1,"generation":"test-generation","target_digest":"test-digest"}' > "$ready"
`)

			err := NewCLI(fakeAMQ).StartWake(context.Background(), StartWakeRequest{
				Root:      "/tmp/amq-root",
				Me:        "codex",
				InjectVia: "/tmp/amq-keepalive",
				Adapter:   "ghostty",
				Target:    "ghostty:terminal:abc",
				Timeout:   5 * time.Second,
			})
			if tc.wantErrIs == nil && err != nil {
				t.Fatalf("StartWake() error = %v, want nil", err)
			}
			if tc.wantErrIs != nil && !errors.Is(err, tc.wantErrIs) {
				t.Fatalf("StartWake() error = %v, want errors.Is(%v)", err, tc.wantErrIs)
			}
			if tc.wantErrString != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErrString)) {
				t.Fatalf("StartWake() error = %v, want contains %q", err, tc.wantErrString)
			}

			data, readErr := os.ReadFile(callsLog)
			if readErr != nil {
				t.Fatalf("read calls log: %v", readErr)
			}
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			gotCalls := make([]string, 0, len(lines))
			for _, line := range lines {
				operation, _, ok := strings.Cut(line, "\t")
				if !ok {
					t.Fatalf("malformed call log line %q", line)
				}
				gotCalls = append(gotCalls, operation)
			}
			if !reflect.DeepEqual(gotCalls, tc.wantCalls) {
				t.Fatalf("call order = %#v, want %#v\n%s", gotCalls, tc.wantCalls, data)
			}
			wantCheck := "check\twake check --me codex --root /tmp/amq-root --json --json-schema 1"
			if lines[0] != wantCheck {
				t.Fatalf("wake check argv = %q, want %q", lines[0], wantCheck)
			}
		})
	}
}

func TestStartWakeFailsWhenProcessExitsBeforeReady(t *testing.T) {
	dir := t.TempDir()
	fakeAMQ := writeStartWakeExecutable(t, filepath.Join(dir, "amq"), `#!/bin/sh
printf 'invalid wake target for this surface\n' >&2
exit 7
`)

	err := NewCLI(fakeAMQ).StartWake(context.Background(), StartWakeRequest{
		Root:      "/tmp/amq-root",
		Me:        "codex",
		InjectVia: "/tmp/amq-keepalive",
		Adapter:   "ghostty",
		Target:    "ghostty:terminal:abc",
		Timeout:   5 * time.Second,
	})
	if err == nil {
		t.Fatal("StartWake() error = nil, want readiness failure")
	}
	if !strings.Contains(err.Error(), "amq wake exited before becoming ready") {
		t.Fatalf("error = %v, want readiness failure", err)
	}
	if !strings.Contains(err.Error(), "invalid wake target for this surface") {
		t.Fatalf("error = %v, want actionable child stderr", err)
	}
}

func TestStartWakePreservesAlreadyTextWithoutRelabelingFailure(t *testing.T) {
	dir := t.TempDir()
	fakeAMQ := writeStartWakeExecutable(t, filepath.Join(dir, "amq"), `#!/bin/sh
printf 'configuration was already migrated but target is invalid\n' >&2
exit 7
`)

	err := NewCLI(fakeAMQ).StartWake(context.Background(), StartWakeRequest{
		Root: "/tmp/amq-root", Me: "codex", InjectVia: "/tmp/amq-keepalive",
		Adapter: "ghostty", Target: "ghostty:terminal:abc", Timeout: 5 * time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "target is invalid") {
		t.Fatalf("StartWake() error = %v, want concrete stderr", err)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("error = %v, want concrete child exit error preserved", err)
	}
}

func TestStartWakeCancelAfterReadyDoesNotKillEstablishedWake(t *testing.T) {
	dir := t.TempDir()
	readyPathLog := filepath.Join(dir, "ready-path.log")
	check := filepath.Join(dir, "check")
	alive := filepath.Join(dir, "alive")
	release := filepath.Join(dir, "release")
	exited := filepath.Join(dir, "exited")
	pidFile := filepath.Join(dir, "pid")
	t.Setenv("AMQ_KEEPALIVE_READY_PATH_LOG", readyPathLog)
	t.Setenv("AMQ_KEEPALIVE_CHECK", check)
	t.Setenv("AMQ_KEEPALIVE_ALIVE", alive)
	t.Setenv("AMQ_KEEPALIVE_RELEASE", release)
	t.Setenv("AMQ_KEEPALIVE_EXITED", exited)
	t.Setenv("AMQ_KEEPALIVE_PID", pidFile)
	fakeAMQ := writeStartWakeExecutable(t, filepath.Join(dir, "amq"), `#!/bin/sh
printf '%s' "$$" > "$AMQ_KEEPALIVE_PID"
ready=""
previous=""
for arg in "$@"; do
  if [ "$previous" = "-ready-file" ]; then ready="$arg"; fi
  previous="$arg"
done
[ -n "$ready" ] || exit 11
printf '%s' "$ready" > "$AMQ_KEEPALIVE_READY_PATH_LOG"
umask 077
printf '%s\n' '{"schema":1,"generation":"test-generation","target_digest":"test-digest"}' > "$ready"
while [ ! -f "$AMQ_KEEPALIVE_RELEASE" ]; do
  if [ -f "$AMQ_KEEPALIVE_CHECK" ]; then : > "$AMQ_KEEPALIVE_ALIVE"; fi
  sleep 0.01
done
: > "$AMQ_KEEPALIVE_EXITED"
`)
	registerDetachedWakeCleanup(t, pidFile, release)

	ctx, cancel := context.WithCancel(context.Background())
	err := NewCLI(fakeAMQ).StartWake(ctx, StartWakeRequest{
		Root: "/tmp/amq-root", Me: "codex", InjectVia: "/tmp/amq-keepalive",
		Adapter: "cmux", Target: "cmux:surface:F901D722-6789-4BBB-9818-C4E97F20BEB3", Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("StartWake() error = %v", err)
	}
	cancel()
	if err := os.WriteFile(check, []byte("check"), 0o600); err != nil {
		t.Fatalf("request liveness check: %v", err)
	}
	waitForFile(t, alive, 2*time.Second)
	if err := os.WriteFile(release, []byte("release"), 0o600); err != nil {
		t.Fatalf("release wake: %v", err)
	}
	waitForFile(t, exited, 2*time.Second)
	data, err := os.ReadFile(readyPathLog)
	if err != nil {
		t.Fatalf("read ready path log: %v", err)
	}
	waitForMissingFile(t, string(data), 2*time.Second)
}

func TestStartWakeTimesOutWhenReadyFileNeverAppears(t *testing.T) {
	dir := t.TempDir()
	release := filepath.Join(dir, "release")
	pidFile := filepath.Join(dir, "pid")
	t.Setenv("AMQ_KEEPALIVE_RELEASE", release)
	t.Setenv("AMQ_KEEPALIVE_PID", pidFile)
	fakeAMQ := writeStartWakeExecutable(t, filepath.Join(dir, "amq"), `#!/bin/sh
printf '%s' "$$" > "$AMQ_KEEPALIVE_PID"
while [ ! -f "$AMQ_KEEPALIVE_RELEASE" ]; do sleep 0.01; done
`)
	registerDetachedWakeCleanup(t, pidFile, release)

	err := NewCLI(fakeAMQ).StartWake(context.Background(), StartWakeRequest{
		Root:      "/tmp/amq-root",
		Me:        "codex",
		InjectVia: "/tmp/amq-keepalive",
		Adapter:   "ghostty",
		Target:    "ghostty:terminal:abc",
		Timeout:   50 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("StartWake() error = nil, want readiness timeout")
	}
	if !strings.Contains(err.Error(), "timed out") || !errors.Is(err, ErrWakeReadinessUncertain) {
		t.Fatalf("error = %v, want uncertain timeout", err)
	}
}

func TestStartWakeDrainSurvivesLauncherProcessExit(t *testing.T) {
	dir := t.TempDir()
	trigger := filepath.Join(dir, "trigger")
	survived := filepath.Join(dir, "survived")
	release := filepath.Join(dir, "release")
	pidFile := filepath.Join(dir, "pid")
	fakeAMQ := writeStartWakeExecutable(t, filepath.Join(dir, "amq"), `#!/bin/sh
printf '%s' "$$" > "$AMQ_KEEPALIVE_PID"
ready=""
previous=""
for arg in "$@"; do
  if [ "$previous" = "-ready-file" ]; then ready="$arg"; fi
  previous="$arg"
done
[ -n "$ready" ] || exit 11
umask 077
printf '%s\n' '{"schema":1,"generation":"test-generation","target_digest":"test-digest"}' > "$ready"
while [ ! -f "$AMQ_KEEPALIVE_TRIGGER" ]; do sleep 0.01; done
set -e
dd if=/dev/zero bs=65536 count=4 >&2 2>/dev/null
: > "$AMQ_KEEPALIVE_SURVIVED"
while [ ! -f "$AMQ_KEEPALIVE_RELEASE" ]; do sleep 0.01; done
`)
	registerDetachedWakeCleanup(t, pidFile, release)

	t.Setenv("AMQ_KEEPALIVE_TEST_LAUNCHER_HELPER", "1")
	t.Setenv("AMQ_KEEPALIVE_TEST_FAKE_AMQ", fakeAMQ)
	t.Setenv("AMQ_KEEPALIVE_CACHE_DIR", filepath.Join(dir, "cache"))
	t.Setenv("AMQ_KEEPALIVE_TRIGGER", trigger)
	t.Setenv("AMQ_KEEPALIVE_SURVIVED", survived)
	t.Setenv("AMQ_KEEPALIVE_RELEASE", release)
	t.Setenv("AMQ_KEEPALIVE_PID", pidFile)
	launcher, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(launcher, "-test.run=^TestStartWakeDetachedLauncherHelper$")
	cmd.Env = os.Environ()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("launcher helper failed: %v\n%s", err, output)
	}

	if err := os.WriteFile(trigger, nil, 0o600); err != nil {
		t.Fatalf("trigger post-launch stderr: %v", err)
	}
	waitForFile(t, survived, 3*time.Second)
}

func TestStartWakeDetachedLauncherHelper(t *testing.T) {
	if os.Getenv("AMQ_KEEPALIVE_TEST_LAUNCHER_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	err := NewCLI(os.Getenv("AMQ_KEEPALIVE_TEST_FAKE_AMQ")).StartWake(
		context.Background(),
		StartWakeRequest{
			Root: "/tmp/amq-root", Me: "codex", InjectVia: "/tmp/amq-keepalive",
			Adapter: "cmux", Target: "cmux:surface:ABC", Timeout: 5 * time.Second,
		},
	)
	if err != nil {
		t.Fatalf("StartWake() error = %v", err)
	}
}

func TestNewWakeReadyPathRejectsSymlinkAndRegularFile(t *testing.T) {
	for _, tc := range []struct {
		name string
		make func(*testing.T, string)
	}{
		{
			name: "symlink",
			make: func(t *testing.T, path string) {
				target := t.TempDir()
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "regular file",
			make: func(t *testing.T, path string) {
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := t.TempDir()
			t.Setenv("AMQ_KEEPALIVE_CACHE_DIR", cache)
			parent := filepath.Join(cache, "amq-keepalive")
			if err := os.Mkdir(parent, 0o700); err != nil {
				t.Fatal(err)
			}
			tc.make(t, filepath.Join(parent, "readiness"))
			if _, _, err := newWakeReadyPath(); err == nil {
				t.Fatal("newWakeReadyPath() accepted non-directory readiness path")
			}
		})
	}
}

func TestRetireWakeEmitsExactArgvAndParsesRetired(t *testing.T) {
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "args.log")
	t.Setenv("AMQ_KEEPALIVE_ARGS_LOG", argsLog)
	fakeAMQ := writeExecutable(t, filepath.Join(dir, "amq"), `#!/bin/sh
printf '%s\n' "$@" > "$AMQ_KEEPALIVE_ARGS_LOG"
printf '%s\n' '{"status":"retired","agent":"codex","pid":4242}'
`)

	result, err := NewCLI(fakeAMQ).RetireWake(context.Background(), RetireWakeRequest{
		Root:       "/tmp/amq-root",
		Me:         "codex",
		InjectVia:  "/tmp/amq-keepalive",
		Adapter:    "cmux",
		Target:     "cmux:surface:F901D722-6789-4BBB-9818-C4E97F20BEB3",
		Generation: "0123456789abcdef0123456789abcdef",
	})
	if err != nil {
		t.Fatalf("RetireWake() error = %v", err)
	}
	if !result.Retired() || result.Status != "retired" || result.PID != 4242 {
		t.Fatalf("result = %+v, want retired pid=4242", result)
	}

	data, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatalf("read args log: %v", err)
	}
	got := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	want := []string{
		"wake", "retire",
		"--me", "codex",
		"--root", "/tmp/amq-root",
		"--inject-via", "/tmp/amq-keepalive",
		"--inject-arg", "inject",
		"--inject-arg", "cmux",
		"--inject-arg", "cmux:surface:F901D722-6789-4BBB-9818-C4E97F20BEB3",
		"--if-generation", "0123456789abcdef0123456789abcdef",
		"--retry-until", "injected",
		"-json",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %#v\nwant %#v", got, want)
	}
}

func writeStartWakeExecutable(t *testing.T, path string, body string) string {
	t.Helper()
	body = strings.TrimPrefix(body, "#!/bin/sh\n")
	return writeExecutable(t, path, `#!/bin/sh
if [ "$1" = "wake" ] && [ "$2" = "check" ]; then
  printf '%s\n' '{"schema":1,"live_wake":false,"image_status":"unknown"}'
  exit 0
fi
`+body)
}

func writeExecutable(t *testing.T, path string, body string) string {
	t.Helper()
	t.Setenv("AMQ_KEEPALIVE_CACHE_DIR", filepath.Join(filepath.Dir(path), "cache"))
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatalf("write executable: %v", err)
	}
	return path
}

func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("file %q did not appear within %s", path, timeout)
}

func waitForMissingFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("file %q still exists after %s", path, timeout)
}

func registerDetachedWakeCleanup(t *testing.T, pidFile string, gates ...string) {
	t.Helper()
	t.Cleanup(func() {
		for _, gate := range gates {
			if err := os.WriteFile(gate, nil, 0o600); err != nil {
				t.Errorf("release detached fake wake through %q: %v", gate, err)
			}
		}
		pid, ok := waitForFakeWakePID(pidFile, 2*time.Second)
		if !ok {
			return
		}
		if exited, err := waitForFakeWakeExit(pid, 2*time.Second); err != nil {
			t.Errorf("inspect detached fake wake pid %d: %v", pid, err)
			return
		} else if exited {
			return
		}
		forceStopDetachedWakeForTest(t, pid)
	})
}

func forceStopDetachedWakeForTest(t *testing.T, pid int) {
	t.Helper()
	t.Errorf("detached fake wake pid %d did not exit through cooperative cleanup", pid)
	// StartWake intentionally detaches its child into a new session. Kill the
	// complete test-owned session so a mutant cannot strand the shell or one
	// of its sleep children after bypassing the cooperative release gates.
	_ = exec.Command("kill", "-TERM", "-"+strconv.Itoa(pid)).Run()
	if exited, err := waitForFakeWakeExit(pid, 500*time.Millisecond); err != nil {
		t.Errorf("inspect detached fake wake pid %d after SIGTERM: %v", pid, err)
		return
	} else if exited {
		return
	}
	_ = exec.Command("kill", "-KILL", "-"+strconv.Itoa(pid)).Run()
	if exited, err := waitForFakeWakeExit(pid, 2*time.Second); err != nil {
		t.Errorf("inspect detached fake wake pid %d after SIGKILL: %v", pid, err)
	} else if !exited {
		t.Errorf("detached fake wake pid %d survived unconditional cleanup", pid)
	}
}

func waitForFakeWakeExit(pid int, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		running, err := fakeWakeProcessRunning(pid)
		if err != nil {
			return false, err
		}
		if !running {
			return true, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false, nil
}

func fakeWakeProcessRunning(pid int) (bool, error) {
	err := exec.Command("kill", "-0", strconv.Itoa(pid)).Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return false, nil
	}
	return false, err
}

func waitForFakeWakePID(path string, timeout time.Duration) (int, bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr == nil && pid > 0 {
				return pid, true
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return 0, false
}
