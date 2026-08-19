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

func TestStartWakeRefusesWhenStderrCaptureIsUnavailable(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	t.Setenv("AMQ_KEEPALIVE_STARTED", started)
	original := newWakeStartupStderrForStart
	newWakeStartupStderrForStart = func(string) (*wakeStartupStderr, error) {
		return nil, errors.New("simulated capture setup failure")
	}
	t.Cleanup(func() { newWakeStartupStderrForStart = original })
	fakeAMQ := writeStartWakeExecutable(t, filepath.Join(dir, "amq"), `#!/bin/sh
printf started > "$AMQ_KEEPALIVE_STARTED"
`)

	err := NewCLI(fakeAMQ).StartWake(context.Background(), StartWakeRequest{
		Root: "/tmp/amq-root", Me: "codex", InjectVia: "/tmp/amq-keepalive",
		Adapter: "cmux", Target: "cmux:surface:ABC", Timeout: 5 * time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "prepare wake startup stderr capture: simulated capture setup failure") {
		t.Fatalf("StartWake() error = %v, want fail-closed capture setup error", err)
	}
	if _, statErr := os.Stat(started); !os.IsNotExist(statErr) {
		t.Fatalf("wake started without diagnostic capture: %v", statErr)
	}
}

func TestStartWakeBaselinesExistingWhenOptedIn(t *testing.T) {
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "args.log")
	t.Setenv("AMQ_KEEPALIVE_ARGS_LOG", argsLog)
	t.Setenv("AMQ_KEEPALIVE_BASELINE_EXISTING", "1")
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
	args := string(data)
	// The escape hatch restores the old suppressing behavior, and the flag must
	// still precede -inject-via to match buildCoopWakeArgs/buildRepairWakeArgs.
	idx1, idx2 := strings.Index(args, "--baseline-existing\n"), strings.Index(args, "-inject-via\n")
	if idx1 < 0 {
		t.Fatalf("args log missing --baseline-existing under opt-in:\n%s", args)
	}
	if idx2 < 0 || idx1 > idx2 {
		t.Fatalf("args log has wrong ordering, want --baseline-existing before -inject-via:\n%s", args)
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

func TestStartWakeKeepsSharedReadyDirectoryForPostReadyWork(t *testing.T) {
	dir := t.TempDir()
	readyPathLog := filepath.Join(dir, "ready-path.log")
	postReady := filepath.Join(dir, "post-ready")
	release := filepath.Join(dir, "release")
	pidFile := filepath.Join(dir, "pid")
	t.Setenv("AMQ_KEEPALIVE_READY_PATH_LOG", readyPathLog)
	t.Setenv("AMQ_KEEPALIVE_POST_READY", postReady)
	t.Setenv("AMQ_KEEPALIVE_RELEASE", release)
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
while [ ! -f "$AMQ_KEEPALIVE_RELEASE" ]; do sleep 0.01; done
[ -d "${ready%/*}" ] || exit 12
: > "$AMQ_KEEPALIVE_POST_READY"
`)
	registerDetachedWakeCleanup(t, pidFile, release)

	err := NewCLI(fakeAMQ).StartWake(context.Background(), StartWakeRequest{
		Root: "/tmp/amq-root", Me: "codex", InjectVia: "/tmp/amq-keepalive",
		Adapter: "cmux", Target: "cmux:surface:F901D722-6789-4BBB-9818-C4E97F20BEB3", Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("StartWake() error = %v", err)
	}
	data, err := os.ReadFile(readyPathLog)
	if err != nil {
		t.Fatalf("read ready path log: %v", err)
	}
	readyPath := string(data)
	readyDir := filepath.Dir(readyPath)
	if info, err := os.Stat(readyDir); err != nil || !info.IsDir() {
		t.Fatalf("ready directory was removed before wake post-ready work: dir=%q err=%v", readyDir, err)
	}
	if _, err := os.Stat(readyPath); !os.IsNotExist(err) {
		t.Fatalf("acknowledged ready marker still exists: path=%q err=%v", readyPath, err)
	}
	if err := os.WriteFile(release, []byte("release"), 0o600); err != nil {
		t.Fatalf("release wake: %v", err)
	}
	waitForFile(t, postReady, 2*time.Second)
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

func TestStartWakeCancelBeforeReadyLeavesChildUnsignaled(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	allowReady := filepath.Join(dir, "allow-ready")
	lateReady := filepath.Join(dir, "late-ready")
	release := filepath.Join(dir, "release")
	exited := filepath.Join(dir, "exited")
	pidFile := filepath.Join(dir, "pid")
	t.Setenv("AMQ_KEEPALIVE_STARTED", started)
	t.Setenv("AMQ_KEEPALIVE_ALLOW_READY", allowReady)
	t.Setenv("AMQ_KEEPALIVE_LATE_READY", lateReady)
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
: > "$AMQ_KEEPALIVE_STARTED"
while [ ! -f "$AMQ_KEEPALIVE_ALLOW_READY" ]; do sleep 0.01; done
umask 077
printf '%s\n' '{"schema":1,"generation":"test-generation","target_digest":"test-digest"}' > "$ready"
: > "$AMQ_KEEPALIVE_LATE_READY"
while [ ! -f "$AMQ_KEEPALIVE_RELEASE" ]; do sleep 0.01; done
: > "$AMQ_KEEPALIVE_EXITED"
`)
	registerDetachedWakeCleanup(t, pidFile, allowReady, release)
	budget := testWaitBudget(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		done <- NewCLI(fakeAMQ).StartWake(ctx, StartWakeRequest{
			Root: "/tmp/amq-root", Me: "codex", InjectVia: "/tmp/amq-keepalive",
			Adapter: "cmux", Target: "cmux:surface:F901D722-6789-4BBB-9818-C4E97F20BEB3", Timeout: budget,
		})
	}()
	waitForFile(t, started, testWaitBudget(t))
	cancel()
	err := <-done
	if !errors.Is(err, ErrWakeReadinessUncertain) || !errors.Is(err, context.Canceled) {
		t.Fatalf("StartWake() error = %v, want canceled uncertain readiness", err)
	}
	if err := os.WriteFile(allowReady, nil, 0o600); err != nil {
		t.Fatalf("allow late readiness: %v", err)
	}
	waitForFile(t, lateReady, testWaitBudget(t))
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatalf("release child: %v", err)
	}
	waitForFile(t, exited, testWaitBudget(t))
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

func TestWaitForWakeReadyUsesRequestedTimeoutDeterministically(t *testing.T) {
	timeoutSignal := make(chan time.Time, 1)
	timeoutSignal <- time.Time{}
	var requestedTimeout time.Duration
	var observedGrace time.Duration
	processDone, err := waitForWakeReadyWith(
		context.Background(),
		make(chan wakeProcessResult),
		filepath.Join(t.TempDir(), "missing"),
		50*time.Millisecond,
		func(timeout time.Duration) wakeReadyTimer {
			requestedTimeout = timeout
			return wakeReadyTimer{channel: timeoutSignal, stop: func() {}}
		},
		func(_ <-chan wakeProcessResult, _ string, grace time.Duration) (bool, bool, error) {
			observedGrace = grace
			return false, false, nil
		},
	)
	if processDone {
		t.Fatal("waitForWakeReadyWith() processDone = true, want uncertain timeout")
	}
	if err == nil || !strings.Contains(err.Error(), "timed out after 50ms") {
		t.Fatalf("waitForWakeReadyWith() error = %v, want exact timeout", err)
	}
	if requestedTimeout != 50*time.Millisecond {
		t.Fatalf("timer duration = %s, want 50ms", requestedTimeout)
	}
	if observedGrace != wakeProcessExitGrace {
		t.Fatalf("observation grace = %s, want %s", observedGrace, wakeProcessExitGrace)
	}
}

func TestWaitForWakeReadyDefaultsNonPositiveTimeoutDeterministically(t *testing.T) {
	for _, timeout := range []time.Duration{0, -time.Second} {
		t.Run(timeout.String(), func(t *testing.T) {
			timeoutSignal := make(chan time.Time, 1)
			timeoutSignal <- time.Time{}
			var requested time.Duration
			_, err := waitForWakeReadyWith(
				context.Background(),
				make(chan wakeProcessResult),
				filepath.Join(t.TempDir(), "missing"),
				timeout,
				func(got time.Duration) wakeReadyTimer {
					requested = got
					return wakeReadyTimer{channel: timeoutSignal, stop: func() {}}
				},
				func(_ <-chan wakeProcessResult, _ string, _ time.Duration) (bool, bool, error) {
					return false, false, nil
				},
			)
			if err == nil || !strings.Contains(err.Error(), defaultWakeReadyTimeout.String()) {
				t.Fatalf("waitForWakeReadyWith() error = %v, want default timeout", err)
			}
			if requested != defaultWakeReadyTimeout {
				t.Fatalf("timer duration = %s, want %s", requested, defaultWakeReadyTimeout)
			}
		})
	}
}

func TestStartWakeTimeoutIncludesCapturedStderr(t *testing.T) {
	dir := t.TempDir()
	release := filepath.Join(dir, "release")
	pidFile := filepath.Join(dir, "pid")
	t.Setenv("AMQ_KEEPALIVE_RELEASE", release)
	t.Setenv("AMQ_KEEPALIVE_PID", pidFile)
	fakeAMQ := writeStartWakeExecutable(t, filepath.Join(dir, "amq"), `#!/bin/sh
printf '%s' "$$" > "$AMQ_KEEPALIVE_PID"
printf 'wake directory temporarily unavailable: inbox parent directory: no such file or directory\n' >&2
while [ ! -f "$AMQ_KEEPALIVE_RELEASE" ]; do sleep 0.01; done
`)
	registerDetachedWakeCleanup(t, pidFile, release)

	err := NewCLI(fakeAMQ).StartWake(context.Background(), StartWakeRequest{
		Root: "/tmp/amq-root", Me: "codex", InjectVia: "/tmp/amq-keepalive",
		Adapter: "cmux", Target: "cmux:surface:ABC", Timeout: 50 * time.Millisecond,
	})
	if !errors.Is(err, ErrWakeReadinessUncertain) {
		t.Fatalf("StartWake() error = %v, want uncertain readiness", err)
	}
	if !strings.Contains(err.Error(), "inbox parent directory: no such file or directory") {
		t.Fatalf("StartWake() error = %v, want captured timeout diagnostic", err)
	}
}

func TestStartWakeCanceledContextDoesNotInvokeAMQ(t *testing.T) {
	dir := t.TempDir()
	called := filepath.Join(dir, "called")
	t.Setenv("AMQ_KEEPALIVE_CALLED", called)
	fakeAMQ := writeExecutable(t, filepath.Join(dir, "amq"), `#!/bin/sh
: > "$AMQ_KEEPALIVE_CALLED"
exit 99
`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := NewCLI(fakeAMQ).StartWake(ctx, StartWakeRequest{
		Root: "/tmp/amq-root", Me: "codex", InjectVia: "/tmp/amq-keepalive",
		Adapter: "cmux", Target: "cmux:surface:ABC", Timeout: time.Second,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("StartWake() error = %v, want context.Canceled", err)
	}
	if _, statErr := os.Stat(called); !os.IsNotExist(statErr) {
		t.Fatalf("AMQ was invoked for an already-canceled context: %v", statErr)
	}
}

func TestStartWakeCancellationDuringCaptureSetupDoesNotStartWake(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	t.Setenv("AMQ_KEEPALIVE_STARTED", started)
	fakeAMQ := writeStartWakeExecutable(t, filepath.Join(dir, "amq"), `#!/bin/sh
: > "$AMQ_KEEPALIVE_STARTED"
exit 97
`)
	original := newWakeStartupStderrForStart
	setupEntered := make(chan struct{})
	setupRelease := make(chan struct{})
	newWakeStartupStderrForStart = func(readyDir string) (*wakeStartupStderr, error) {
		close(setupEntered)
		<-setupRelease
		return newWakeStartupStderr(readyDir)
	}
	t.Cleanup(func() { newWakeStartupStderrForStart = original })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- NewCLI(fakeAMQ).StartWake(ctx, StartWakeRequest{
			Root: "/tmp/amq-root", Me: "codex", InjectVia: "/tmp/amq-keepalive",
			Adapter: "cmux", Target: "cmux:surface:ABC", Timeout: time.Second,
		})
	}()
	<-setupEntered
	cancel()
	close(setupRelease)

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("StartWake() error = %v, want context.Canceled", err)
	}
	if _, statErr := os.Stat(started); !os.IsNotExist(statErr) {
		t.Fatalf("wake started after mid-setup cancellation: %v", statErr)
	}
}

func TestStartWakeReportsProcessStartFailure(t *testing.T) {
	dir := t.TempDir()
	fakeAMQ := writeExecutable(t, filepath.Join(dir, "amq"), `#!/bin/sh
if [ "$1" = "wake" ] && [ "$2" = "check" ]; then
  rm "$0"
  printf '%s\n' '{"schema":1,"live_wake":false,"image_status":"unknown"}'
  exit 0
fi
exit 98
`)

	err := NewCLI(fakeAMQ).StartWake(context.Background(), StartWakeRequest{
		Root: "/tmp/amq-root", Me: "codex", InjectVia: "/tmp/amq-keepalive",
		Adapter: "cmux", Target: "cmux:surface:ABC", Timeout: time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "no such file or directory") {
		t.Fatalf("StartWake() error = %v, want concrete process start failure", err)
	}
}

func TestStartWakeKeepsLongLivedStderrDrained(t *testing.T) {
	dir := t.TempDir()
	trigger := filepath.Join(dir, "trigger")
	survived := filepath.Join(dir, "survived")
	release := filepath.Join(dir, "release")
	pidFile := filepath.Join(dir, "pid")
	t.Setenv("AMQ_KEEPALIVE_TRIGGER", trigger)
	t.Setenv("AMQ_KEEPALIVE_SURVIVED", survived)
	t.Setenv("AMQ_KEEPALIVE_RELEASE", release)
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
umask 077
printf '%s\n' '{"schema":1,"generation":"test-generation","target_digest":"test-digest"}' > "$ready"
while [ ! -f "$AMQ_KEEPALIVE_TRIGGER" ]; do sleep 0.01; done
printf 'post-launch diagnostic\n' >&2
: > "$AMQ_KEEPALIVE_SURVIVED"
while [ ! -f "$AMQ_KEEPALIVE_RELEASE" ]; do sleep 0.01; done
`)
	registerDetachedWakeCleanup(t, pidFile, release)

	if err := NewCLI(fakeAMQ).StartWake(context.Background(), StartWakeRequest{
		Root: "/tmp/amq-root", Me: "codex", InjectVia: "/tmp/amq-keepalive",
		Adapter: "cmux", Target: "cmux:surface:F901D722-6789-4BBB-9818-C4E97F20BEB3", Timeout: 5 * time.Second,
	}); err != nil {
		t.Fatalf("StartWake() error = %v", err)
	}
	if err := os.WriteFile(trigger, nil, 0o600); err != nil {
		t.Fatalf("trigger post-launch stderr: %v", err)
	}
	waitForFile(t, survived, 2*time.Second)
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

func TestWaitForWakeReadyPrefersExitedChildOverCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan wakeProcessResult, 1)
	go func() {
		time.Sleep(20 * time.Millisecond)
		done <- wakeProcessResult{Err: errors.New("exit status 9"), Stderr: "specific wake refusal"}
	}()

	processDone, err := waitForWakeReady(ctx, done, filepath.Join(t.TempDir(), "missing"), time.Second)
	if !processDone {
		t.Fatal("waitForWakeReady() processDone = false, want true")
	}
	if err == nil || !strings.Contains(err.Error(), "exit status 9") || !strings.Contains(err.Error(), "specific wake refusal") {
		t.Fatalf("waitForWakeReady() error = %v, want actual child result", err)
	}
}

func TestWaitForWakeReadyPrefersExitedChildOverTimeout(t *testing.T) {
	done := make(chan wakeProcessResult, 1)
	go func() {
		time.Sleep(20 * time.Millisecond)
		done <- wakeProcessResult{Err: errors.New("exit status 8"), Stderr: "timeout-edge refusal"}
	}()

	processDone, err := waitForWakeReady(context.Background(), done, filepath.Join(t.TempDir(), "missing"), time.Nanosecond)
	if !processDone {
		t.Fatal("waitForWakeReady() processDone = false, want true")
	}
	if err == nil || !strings.Contains(err.Error(), "exit status 8") || !strings.Contains(err.Error(), "timeout-edge refusal") {
		t.Fatalf("waitForWakeReady() error = %v, want actual child result", err)
	}
}

func TestWaitForWakeReadyAllowsBoundedStderrDrainBeforeReportingTimeout(t *testing.T) {
	done := make(chan wakeProcessResult, 1)
	go func() {
		// The child waiter publishes only after its bounded stderr drain. This
		// delay is longer than the old 100ms process grace but remains within
		// the drain bound used by StartWake.
		time.Sleep(150 * time.Millisecond)
		done <- wakeProcessResult{Err: errors.New("exit status 6"), Stderr: "late concrete refusal"}
	}()

	processDone, err := waitForWakeReady(context.Background(), done, filepath.Join(t.TempDir(), "missing"), time.Nanosecond)
	if !processDone {
		t.Fatal("waitForWakeReady() processDone = false, want concrete child exit after bounded drain")
	}
	if err == nil || !strings.Contains(err.Error(), "exit status 6") || !strings.Contains(err.Error(), "late concrete refusal") {
		t.Fatalf("waitForWakeReady() error = %v, want delayed child exit and stderr", err)
	}
}

func TestWakeStartupStderrReportsTruncationAndRemovesFile(t *testing.T) {
	capture, err := newWakeStartupStderr(t.TempDir())
	if err != nil {
		t.Fatalf("newWakeStartupStderr() error = %v", err)
	}
	path := capture.file.Name()
	diagnosticPath := capture.diagnosticFile.Name()
	data := strings.Repeat("x", maxWakeStartupStderrBytes+8)
	if _, err := capture.writer.WriteString(data); err != nil {
		t.Fatalf("write startup stderr: %v", err)
	}
	capture.closeWriter()
	if err := capture.waitForDrain(2 * time.Second); err != nil {
		t.Fatalf("wait for stderr drain: %v", err)
	}
	info, err := capture.file.Stat()
	if err != nil {
		t.Fatalf("stat startup stderr capture: %v", err)
	}
	if info.Size() > maxWakeStartupStderrBytes {
		t.Fatalf("startup stderr storage = %d bytes, want at most %d", info.Size(), maxWakeStartupStderrBytes)
	}
	got := capture.String()
	if len(got) < maxWakeStartupStderrBytes ||
		!strings.Contains(got, "stderr truncated after 16384 bytes") {
		t.Fatalf("String() = %q, want bounded prefix and truncation marker", got)
	}
	capture.Close()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("wake startup stderr file still exists after Close: %v", err)
	}
	if _, err := os.Stat(diagnosticPath); !os.IsNotExist(err) {
		t.Fatalf("wake stderr drain diagnostic file still exists after Close: %v", err)
	}
}

func TestWakeStartupStderrReportsReadFailure(t *testing.T) {
	capture, err := newWakeStartupStderr(t.TempDir())
	if err != nil {
		t.Fatalf("newWakeStartupStderr() error = %v", err)
	}
	t.Cleanup(capture.Close)
	capture.closeWriter()
	if err := capture.waitForDrain(2 * time.Second); err != nil {
		t.Fatalf("wait for stderr drain: %v", err)
	}
	if err := capture.file.Close(); err != nil {
		t.Fatalf("close capture file: %v", err)
	}
	if got := capture.String(); !strings.Contains(got, "stderr unavailable") {
		t.Fatalf("String() = %q, want visible read failure", got)
	}
}

func TestWakeStartupStderrWaitForDrainAcceptsAbsentCapture(t *testing.T) {
	var absent *wakeStartupStderr
	if err := absent.waitForDrain(time.Second); err != nil {
		t.Fatalf("nil capture waitForDrain() error = %v", err)
	}
	if err := (&wakeStartupStderr{}).waitForDrain(time.Second); err != nil {
		t.Fatalf("nil drain result waitForDrain() error = %v", err)
	}
}

func TestReadBoundedWakeDiagnosticMarksOnlyBytesBeyondLimit(t *testing.T) {
	const limit = 8
	for _, tc := range []struct {
		name       string
		data       string
		want       string
		wantMarker bool
	}{
		{name: "exact limit", data: strings.Repeat("x", limit)},
		{name: "one byte beyond", data: strings.Repeat("x", limit+1), wantMarker: true},
		{
			name:       "whitespace beyond limit",
			data:       strings.Repeat(" ", limit+1),
			want:       "[test diagnostic truncated after 8 bytes]",
			wantMarker: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, err := os.CreateTemp(t.TempDir(), "diagnostic-*")
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := file.Close(); err != nil {
					t.Errorf("close diagnostic file: %v", err)
				}
			}()
			if _, err := file.WriteString(tc.data); err != nil {
				t.Fatal(err)
			}
			got := readBoundedWakeDiagnostic(file, limit, "test diagnostic")
			marker := "[test diagnostic truncated after 8 bytes]"
			if strings.Contains(got, marker) != tc.wantMarker {
				t.Fatalf("diagnostic = %q, marker presence want %v", got, tc.wantMarker)
			}
			if tc.want != "" && got != tc.want {
				t.Fatalf("diagnostic = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNewWakeStartupStderrReportsMissingDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing", "readiness")
	if _, err := newWakeStartupStderr(missing); err == nil ||
		!strings.Contains(err.Error(), "create wake startup stderr file") {
		t.Fatalf("newWakeStartupStderr() error = %v, want create failure", err)
	}
}

func TestNewWakeStartupStderrCleansEverySetupFailure(t *testing.T) {
	cases := []struct {
		name    string
		wantErr string
		mutate  func(*wakeStderrSetup)
	}{
		{
			name:    "secure startup capture",
			wantErr: "secure wake startup stderr file",
			mutate: func(setup *wakeStderrSetup) {
				setup.chmod = func(*os.File, os.FileMode) error { return errors.New("chmod failed") }
			},
		},
		{
			name:    "create pipe",
			wantErr: "create wake startup stderr pipe",
			mutate: func(setup *wakeStderrSetup) {
				setup.pipe = func() (*os.File, *os.File, error) {
					return nil, nil, errors.New("pipe failed")
				}
			},
		},
		{
			name:    "create drain diagnostic",
			wantErr: "create wake stderr drain diagnostic file",
			mutate: func(setup *wakeStderrSetup) {
				calls := 0
				setup.createTemp = func(dir, pattern string) (*os.File, error) {
					calls++
					if calls == 2 {
						return nil, errors.New("diagnostic create failed")
					}
					return os.CreateTemp(dir, pattern)
				}
			},
		},
		{
			name:    "secure drain diagnostic",
			wantErr: "secure wake stderr drain diagnostic file",
			mutate: func(setup *wakeStderrSetup) {
				calls := 0
				setup.chmod = func(file *os.File, mode os.FileMode) error {
					calls++
					if calls == 2 {
						return errors.New("diagnostic chmod failed")
					}
					return file.Chmod(mode)
				}
			},
		},
		{
			name:    "resolve executable",
			wantErr: "resolve wake stderr drain executable",
			mutate: func(setup *wakeStderrSetup) {
				setup.executable = func() (string, error) { return "", errors.New("executable failed") }
			},
		},
		{
			name:    "start drain",
			wantErr: "start wake stderr drain",
			mutate: func(setup *wakeStderrSetup) {
				setup.start = func(*exec.Cmd) error { return errors.New("start failed") }
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			setup := wakeStderrSetup{
				createTemp: os.CreateTemp,
				chmod:      func(file *os.File, mode os.FileMode) error { return file.Chmod(mode) },
				pipe:       os.Pipe,
				executable: os.Executable,
				start:      func(cmd *exec.Cmd) error { return cmd.Start() },
			}
			tc.mutate(&setup)
			capture, err := newWakeStartupStderrWithSetup(dir, setup)
			if capture != nil || err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("newWakeStartupStderrWithSetup() = (%v, %v), want nil and %q", capture, err, tc.wantErr)
			}
			entries, readErr := os.ReadDir(dir)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 0 {
				t.Fatalf("setup failure left diagnostic files: %v", entries)
			}
		})
	}
}

func TestWakeStartupStderrDetailReportsDrainFailure(t *testing.T) {
	capture, err := newWakeStartupStderr(t.TempDir())
	if err != nil {
		t.Fatalf("newWakeStartupStderr() error = %v", err)
	}
	t.Cleanup(capture.Close)
	if _, err := capture.file.WriteAt([]byte("startup failure"), 0); err != nil {
		t.Fatal(err)
	}
	got := wakeStartupStderrDetail(capture, errors.New("drain failed"))
	want := "startup failure\n[stderr capture incomplete: drain failed]"
	if got != want {
		t.Fatalf("wakeStartupStderrDetail() = %q, want %q", got, want)
	}
}

func TestNewWakeReadyPathScavengesOnlyStaleMarkers(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("AMQ_KEEPALIVE_CACHE_DIR", cache)
	dir := filepath.Join(cache, "amq-keepalive", "readiness")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create readiness dir: %v", err)
	}
	oldMarker := filepath.Join(dir, "wake-old")
	recentMarker := filepath.Join(dir, "wake-recent")
	boundaryMarker := filepath.Join(dir, "wake-boundary")
	unrelatedFile := filepath.Join(dir, "notes")
	unrelatedDir := filepath.Join(dir, "wake-directory")
	for _, path := range []string{oldMarker, recentMarker, boundaryMarker, unrelatedFile} {
		if err := os.WriteFile(path, []byte("ready"), 0o600); err != nil {
			t.Fatalf("write marker: %v", err)
		}
	}
	if err := os.Mkdir(unrelatedDir, 0o700); err != nil {
		t.Fatalf("create unrelated directory: %v", err)
	}
	boundaryTime := time.Now().Add(-staleWakeReadyMarkerAge)
	if err := os.Chtimes(boundaryMarker, boundaryTime, boundaryTime); err != nil {
		t.Fatalf("age boundary marker: %v", err)
	}
	boundaryInfo, err := os.Stat(boundaryMarker)
	if err != nil {
		t.Fatal(err)
	}
	now := boundaryInfo.ModTime().Add(staleWakeReadyMarkerAge)
	oldTime := now.Add(-staleWakeReadyMarkerAge - time.Hour)
	for _, path := range []string{oldMarker, unrelatedFile, unrelatedDir} {
		if err := os.Chtimes(path, oldTime, oldTime); err != nil {
			t.Fatalf("age readiness entry %q: %v", path, err)
		}
	}
	scavengeStaleWakeReadyMarkers(dir, now)
	_, reserved, err := newWakeReadyPath()
	if err != nil {
		t.Fatalf("newWakeReadyPath: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Remove(reserved); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("remove reserved readiness path: %v", err)
		}
	})
	if _, err := os.Stat(oldMarker); !os.IsNotExist(err) {
		t.Fatalf("old marker was not scavenged: %v", err)
	}
	if _, err := os.Stat(boundaryMarker); !os.IsNotExist(err) {
		t.Fatalf("boundary marker was not scavenged: %v", err)
	}
	if _, err := os.Stat(recentMarker); err != nil {
		t.Fatalf("recent marker was scavenged: %v", err)
	}
	for _, path := range []string{unrelatedFile, unrelatedDir} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("unrelated readiness entry %q was scavenged: %v", path, err)
		}
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

func TestRetireWakeOutcomeMapping(t *testing.T) {
	cases := []struct {
		name            string
		script          string
		wantRetired     bool
		wantStatus      string
		wantErrContains string
	}{
		{
			name:        "retired",
			script:      `printf '%s\n' '{"status":"retired","agent":"codex","pid":7}'`,
			wantRetired: true,
			wantStatus:  "retired",
		},
		{
			name:        "retired with residue",
			script:      `printf '%s\n' '{"status":"retired_with_residue","agent":"codex","pid":7,"reason":"retirement succeeded; preserved residue: .wake.target"}'`,
			wantRetired: true,
			wantStatus:  "retired_with_residue",
		},
		{
			name: "refused",
			script: `printf '%s\n' '{"status":"refused","reason":"owner-bound wake claims require recover-owner"}'
exit 1`,
			wantStatus:      "refused",
			wantErrContains: "owner-bound",
		},
		{
			name:            "invalid-json",
			script:          `printf '%s\n' 'not json at all'`,
			wantErrContains: "parse amq wake retire output",
		},
		{
			name: "non-zero-exit",
			script: `echo boom >&2
exit 7`,
			wantErrContains: "amq wake retire failed",
		},
		{
			name: "retired-non-zero-exit",
			script: `printf '%s\n' '{"status":"retired","agent":"codex","pid":7}'
exit 7`,
			wantErrContains: "exited unsuccessfully",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			fakeAMQ := writeExecutable(t, filepath.Join(dir, "amq"), "#!/bin/sh\n"+tc.script+"\n")
			result, err := NewCLI(fakeAMQ).RetireWake(context.Background(), RetireWakeRequest{
				Root: "/tmp/root", Me: "codex", InjectVia: "/tmp/keepalive",
				Adapter: "cmux", Target: "cmux:surface:ABC",
			})
			if tc.wantRetired {
				if err != nil {
					t.Fatalf("RetireWake() error = %v, want nil", err)
				}
				if !result.Retired() || result.Status != tc.wantStatus {
					t.Fatalf("result = %+v, want retired", result)
				}
				return
			}
			if err == nil {
				t.Fatalf("RetireWake() error = nil, want not-confirmed; result = %+v", result)
			}
			if !errors.Is(err, ErrWakeRetireNotConfirmed) {
				t.Fatalf("error = %v, want wrapped ErrWakeRetireNotConfirmed", err)
			}
			if result.Retired() {
				t.Fatalf("result marked retired on failure: %+v", result)
			}
			if tc.wantStatus != "" && result.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", result.Status, tc.wantStatus)
			}
			if tc.wantErrContains != "" && !strings.Contains(err.Error(), tc.wantErrContains) {
				t.Fatalf("error = %v, want contains %q", err, tc.wantErrContains)
			}
		})
	}
}

func TestRetireWakeValidatesRequiredFields(t *testing.T) {
	cases := map[string]RetireWakeRequest{
		"missing me":         {InjectVia: "/x", Adapter: "cmux", Target: "t"},
		"missing inject-via": {Me: "codex", Adapter: "cmux", Target: "t"},
		"missing adapter":    {Me: "codex", InjectVia: "/x", Target: "t"},
		"missing target":     {Me: "codex", InjectVia: "/x", Adapter: "cmux"},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewCLI("/does/not/matter").RetireWake(context.Background(), req); err == nil {
				t.Fatalf("RetireWake(%+v) error = nil, want validation error", req)
			}
		})
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

func testWaitBudget(t *testing.T) time.Duration {
	t.Helper()
	const reserve = 2 * time.Second
	deadline, ok := t.Deadline()
	if !ok {
		return 30 * time.Second
	}
	remain := time.Until(deadline)
	if remain > reserve {
		return remain - reserve
	}
	if remain > 0 {
		return remain
	}
	return time.Millisecond
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
