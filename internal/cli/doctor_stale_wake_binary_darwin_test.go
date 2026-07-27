//go:build darwin

package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDarwinWakeBinaryComparisonUsesStrictStartedMTimeHeuristic(t *testing.T) {
	started := time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "amq")
	if err := os.WriteFile(path, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name  string
		mtime time.Time
		stale bool
	}{
		{name: "before started", mtime: started.Add(-time.Second)},
		{name: "equal started", mtime: started},
		{name: "inside second precision window", mtime: started.Add(999 * time.Millisecond)},
		{name: "equal uncertainty boundary", mtime: started.Add(time.Second)},
		{name: "strictly after uncertainty boundary", mtime: started.Add(time.Second + time.Nanosecond), stale: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.Chtimes(path, tc.mtime, tc.mtime); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			got, err := inspectWakeBinaryStalenessPlatform(
				wakeLockInspection{Lock: wakeLock{Started: started.Format(time.RFC3339)}},
				resolvedWakeBinary{Info: info},
			)
			if err != nil {
				t.Fatalf("compare timestamps: %v", err)
			}
			if got.Stale != tc.stale {
				t.Fatalf("stale = %v, want %v: %#v", got.Stale, tc.stale, got)
			}
			if got.Stale && got.Method != wakeBinaryComparisonStartedMTime {
				t.Fatalf("stale method = %q", got.Method)
			}
		})
	}
}

func TestDarwinWakeBinaryComparisonReturnsUnknownForInvalidStarted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "amq")
	if err := os.WriteFile(path, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := inspectWakeBinaryStalenessPlatform(
		wakeLockInspection{Lock: wakeLock{Started: "not-a-timestamp"}},
		resolvedWakeBinary{Info: info},
	)
	if err == nil {
		t.Fatal("invalid Started returned nil error")
	}
	if got.Stale {
		t.Fatalf("invalid Started reported stale: %#v", got)
	}
}

func TestDarwinWakeBinaryComparisonStatsResolvedHomebrewTargetNotSymlink(t *testing.T) {
	started := time.Now().UTC().Add(-5 * time.Second).Truncate(time.Second)
	dir := t.TempDir()
	target := filepath.Join(dir, "Cellar", "amq", "0.49.6", "bin", "amq")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	targetMTime := started.Add(-time.Minute)
	if err := os.Chtimes(target, targetMTime, targetMTime); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "bin", "amq")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	linkInfo, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if !linkInfo.ModTime().After(started.Add(wakeStartedTimestampUncertainty)) {
		t.Fatalf("test requires symlink mtime after wake uncertainty boundary: %v", linkInfo.ModTime())
	}

	old := resolveWakeExecutablePath
	resolveWakeExecutablePath = func() (string, string, error) {
		return link, target, nil
	}
	t.Cleanup(func() { resolveWakeExecutablePath = old })

	got, err := inspectWakeBinaryStalenessDefault(wakeLockInspection{
		Lock: wakeLock{Started: started.Format(time.RFC3339)},
	})
	if err != nil {
		t.Fatalf("compare resolved Homebrew target: %v", err)
	}
	if got.Stale {
		t.Fatalf("newer Homebrew symlink mtime produced stale hint: %#v", got)
	}
}
