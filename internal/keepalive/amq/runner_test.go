package amq

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
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
printf ready > "$ready"
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
printf ready > "$ready"
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
			checkOutput:  `{"schema":1,"live_wake":true,"image_status":"different"}`,
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
			checkOutput:   `{"schema":1,"live_wake":true,"image_status":"different"}`,
			retireOutput:  `{"status":"refused","reason":"target identity changed"}`,
			retireExit:    "1",
			wantCalls:     []string{"check", "retire"},
			wantErrIs:     ErrWakeRetireNotConfirmed,
			wantErrString: "target identity changed",
		},
		{
			name:          "retired output with failed exit does not start",
			checkOutput:   `{"schema":1,"live_wake":true,"image_status":"different"}`,
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
printf ready > "$ready"
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
}

func TestStartWakeKeepsSharedReadyDirectoryForPostReadyWork(t *testing.T) {
	dir := t.TempDir()
	readyPathLog := filepath.Join(dir, "ready-path.log")
	postReady := filepath.Join(dir, "post-ready")
	release := filepath.Join(dir, "release")
	t.Setenv("AMQ_KEEPALIVE_READY_PATH_LOG", readyPathLog)
	t.Setenv("AMQ_KEEPALIVE_POST_READY", postReady)
	t.Setenv("AMQ_KEEPALIVE_RELEASE", release)
	fakeAMQ := writeStartWakeExecutable(t, filepath.Join(dir, "amq"), `#!/bin/sh
ready=""
previous=""
for arg in "$@"; do
  if [ "$previous" = "-ready-file" ]; then ready="$arg"; fi
  previous="$arg"
done
[ -n "$ready" ] || exit 11
printf '%s' "$ready" > "$AMQ_KEEPALIVE_READY_PATH_LOG"
printf ready > "$ready"
while [ ! -f "$AMQ_KEEPALIVE_RELEASE" ]; do sleep 0.01; done
[ -d "${ready%/*}" ] || exit 12
: > "$AMQ_KEEPALIVE_POST_READY"
`)

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
printf ready > "$ready"
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
printf ready > "$ready"
: > "$AMQ_KEEPALIVE_LATE_READY"
while [ ! -f "$AMQ_KEEPALIVE_RELEASE" ]; do sleep 0.01; done
: > "$AMQ_KEEPALIVE_EXITED"
`)
	registerDetachedWakeCleanup(t, pidFile, allowReady, release)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		done <- NewCLI(fakeAMQ).StartWake(ctx, StartWakeRequest{
			Root: "/tmp/amq-root", Me: "codex", InjectVia: "/tmp/amq-keepalive",
			Adapter: "cmux", Target: "cmux:surface:F901D722-6789-4BBB-9818-C4E97F20BEB3", Timeout: 5 * time.Second,
		})
	}()
	waitForFile(t, started, 2*time.Second)
	cancel()
	err := <-done
	if !errors.Is(err, ErrWakeReadinessUncertain) || !errors.Is(err, context.Canceled) {
		t.Fatalf("StartWake() error = %v, want canceled uncertain readiness", err)
	}
	if err := os.WriteFile(allowReady, nil, 0o600); err != nil {
		t.Fatalf("allow late readiness: %v", err)
	}
	waitForFile(t, lateReady, 2*time.Second)
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatalf("release child: %v", err)
	}
	waitForFile(t, exited, 2*time.Second)
}

func TestStartWakeTimesOutWhenReadyFileNeverAppears(t *testing.T) {
	dir := t.TempDir()
	fakeAMQ := writeStartWakeExecutable(t, filepath.Join(dir, "amq"), `#!/bin/sh
sleep 0.2
`)

	start := time.Now()
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
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("StartWake took %s, want timeout branch to return promptly", elapsed)
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
	for _, path := range []string{oldMarker, recentMarker} {
		if err := os.WriteFile(path, []byte("ready"), 0o600); err != nil {
			t.Fatalf("write marker: %v", err)
		}
	}
	oldTime := time.Now().Add(-staleWakeReadyMarkerAge - time.Hour)
	if err := os.Chtimes(oldMarker, oldTime, oldTime); err != nil {
		t.Fatalf("age old marker: %v", err)
	}
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
	if _, err := os.Stat(recentMarker); err != nil {
		t.Fatalf("recent marker was scavenged: %v", err)
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
		Root:      "/tmp/amq-root",
		Me:        "codex",
		InjectVia: "/tmp/amq-keepalive",
		Adapter:   "cmux",
		Target:    "cmux:surface:F901D722-6789-4BBB-9818-C4E97F20BEB3",
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
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			err := syscall.Kill(pid, 0)
			if errors.Is(err, syscall.ESRCH) {
				return
			}
			if err != nil {
				t.Errorf("inspect detached fake wake pid %d: %v", pid, err)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Errorf("detached fake wake pid %d did not exit after cleanup", pid)
	})
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
