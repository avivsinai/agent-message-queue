//go:build darwin || linux

package lock

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestWithExclusiveFileLockSerializesAndReleases(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "resource.lock")
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- WithExclusiveFileLock(lockPath, func() error {
			close(firstEntered)
			<-releaseFirst
			return nil
		})
	}()
	select {
	case <-firstEntered:
	case err := <-firstDone:
		t.Fatalf("first callback did not enter: %v", err)
	case <-time.After(time.Second):
		t.Fatal("first callback did not enter within timeout")
	}

	secondEntered := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- WithExclusiveFileLock(lockPath, func() error {
			close(secondEntered)
			return nil
		})
	}()
	select {
	case <-secondEntered:
		t.Fatal("second callback entered while first lock was held")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("first lock: %v", err)
	}
	select {
	case <-secondEntered:
	case <-time.After(time.Second):
		t.Fatal("second callback did not enter after first lock released")
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second lock: %v", err)
	}

	wantErr := errors.New("callback failed")
	if err := WithExclusiveFileLock(lockPath, func() error { return wantErr }); !errors.Is(err, wantErr) {
		t.Fatalf("callback error = %v, want %v", err, wantErr)
	}
	if err := WithExclusiveFileLock(lockPath, func() error { return nil }); err != nil {
		t.Fatalf("lock was not reusable after callback error: %v", err)
	}
}
