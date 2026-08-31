//go:build windows

package adapter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsWriterLockInspectorDistinguishesHeldIdleAndMissing(t *testing.T) {
	inspector := platformWriterLockInspector{}
	path := filepath.Join(t.TempDir(), "thread.lock")

	if held, err := inspector.Held(context.Background(), path); err != nil || held {
		t.Fatalf("missing lock held = %v, %v; want false", held, err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if held, err := inspector.Held(context.Background(), path); err != nil || held {
		t.Fatalf("idle lock held = %v, %v; want false", held, err)
	}

	unlock, err := tryFlockExclusive(path)
	if err != nil {
		t.Fatalf("hold writer lock: %v", err)
	}
	if held, err := inspector.Held(context.Background(), path); err != nil || !held {
		t.Fatalf("active lock held = %v, %v; want true", held, err)
	}
	unlock()
	if held, err := inspector.Held(context.Background(), path); err != nil || held {
		t.Fatalf("released lock held = %v, %v; want false", held, err)
	}
}

func TestWindowsWriterLockInspectorHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (platformWriterLockInspector{}).Held(ctx, filepath.Join(t.TempDir(), "thread.lock"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Held() error = %v, want context.Canceled", err)
	}
}
