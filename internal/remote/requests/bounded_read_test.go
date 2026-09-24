package requests

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestRequestStoreReadRejectsFIFOAndOversized is the agent-message-queue-qgc
// regression (codex owner review 2026-09-24, finding 3). List used to
// os.ReadFile every record before the MaxRecordBytes check, so a FIFO blocked
// the endpoint and a huge file was allocated. Both are poison for that record
// only; a sibling target is still returned, and neither is reported as absent.
func TestRequestStoreReadRejectsFIFOAndOversized(t *testing.T) {
	s, err := Open(t.TempDir(), WithClock(fixedClock))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	good := newRecord("11111111-1111-4111-8111-1111111110a1")
	if err := s.Create(good); err != nil {
		t.Fatal(err)
	}
	hostDir := filepath.Join(s.Dir(), requestsDir, good.CreatorHost)
	over := filepath.Join(hostDir, "t_over__11111111-1111-4111-8111-1111111110a2.json")
	f, err := os.OpenFile(over, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(int64(MaxRecordBytes) + 1); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	var fifo string
	if runtime.GOOS != "windows" {
		fifo = filepath.Join(hostDir, "t_fifo__11111111-1111-4111-8111-1111111110a3.json")
		if err := plantNamedPipe(fifo); err != nil {
			t.Fatal(err)
		}
	}

	type listed struct {
		recs   []*Record
		poison []Poison
		err    error
	}
	done := make(chan listed, 1)
	go func() {
		recs, poison, err := s.ListWithPoison()
		done <- listed{recs, poison, err}
	}()
	var got listed
	select {
	case got = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("list blocked on a non-regular or oversized record")
	}
	if got.err != nil {
		t.Fatal(got.err)
	}
	if len(got.recs) != 1 || got.recs[0].RequestID != good.RequestID {
		t.Fatalf("sibling record: %+v", got.recs)
	}
	if !poisonNames(got.poison, filepath.Base(over)) {
		t.Fatalf("oversized record was not reported: %+v", got.poison)
	}
	if _, ok, err := s.Get(Key{CreatorHost: good.CreatorHost, TargetID: "t_over", RequestID: "11111111-1111-4111-8111-1111111110a2"}); err == nil {
		t.Fatalf("oversized Get err=nil exists=%v; a bad record must not look absent", ok)
	}
	if fifo == "" {
		return
	}
	if !poisonNames(got.poison, filepath.Base(fifo)) {
		t.Fatalf("fifo was not reported: %+v", got.poison)
	}
	if _, ok, err := s.Get(Key{CreatorHost: good.CreatorHost, TargetID: "t_fifo", RequestID: "11111111-1111-4111-8111-1111111110a3"}); err == nil {
		t.Fatalf("fifo Get err=nil exists=%v; a bad record must not look absent", ok)
	}
}

func poisonNames(poison []Poison, base string) bool {
	for _, p := range poison {
		if filepath.Base(p.Path) == base && p.Error != "" {
			return true
		}
	}
	return false
}
