package selfupgrade

import (
	"runtime"
	"strconv"
	"testing"
	"time"
)

func TestAttemptIsFreshAllowsBoundedFutureClockSkew(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	attempt := Attempt{UnixTime: now.Add(AttemptFutureSkew - time.Second).Unix()}
	if !attempt.IsFresh(now) {
		t.Fatal("future-dated attempt within the skew bound is not fresh")
	}
	uncertain := Attempt{UnixTime: now.Add(AttemptFutureSkew).Unix()}
	if uncertain.IsFresh(now) {
		t.Fatal("future-dated attempt at the skew boundary is fresh")
	}
	if uncertain.IsExpired(now) {
		t.Fatal("future-dated attempt is expired")
	}
	if !uncertain.IsFutureUncertain(now) {
		t.Fatal("future-dated attempt is not marked uncertain")
	}
}

func TestAddAttemptDoesNotEvictFreshUnresolvedEntry(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	incumbent := ImageEvidence{
		Platform:        runtime.GOOS,
		Device:          1,
		Inode:           1,
		Size:            1,
		SHA256:          "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		EmbeddedVersion: "1.0.0",
	}
	attempts := make([]Attempt, 0, AttemptLimit)
	for index := 0; index < AttemptLimit; index++ {
		candidate := incumbent
		candidate.Inode = uint64(index + 1)
		candidate.EmbeddedVersion = "1.0." + strconv.Itoa(index+1)
		attempts = append(attempts, NewAttempt(candidate, now))
	}
	addition := incumbent
	addition.Inode = AttemptLimit + 1
	addition.EmbeddedVersion = "2.0.0"
	if _, err := AddAttempt(attempts, NewAttempt(addition, now), now); err == nil {
		t.Fatal("AddAttempt() succeeded with a full fresh unresolved ledger")
	}
	if len(attempts) != AttemptLimit {
		t.Fatalf("input ledger length = %d, want %d", len(attempts), AttemptLimit)
	}
}

func TestAddAttemptDoesNotEvictFutureUncertainEntry(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	incumbent := ImageEvidence{
		Platform:        runtime.GOOS,
		Device:          1,
		Inode:           1,
		Size:            1,
		SHA256:          "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		EmbeddedVersion: "1.0.0",
	}
	attempts := make([]Attempt, 0, AttemptLimit)
	for index := 0; index < AttemptLimit; index++ {
		candidate := incumbent
		candidate.Inode = uint64(index + 1)
		candidate.EmbeddedVersion = "1.0." + strconv.Itoa(index+1)
		when := now
		if index == 0 {
			when = now.Add(AttemptFutureSkew + time.Second)
		}
		attempts = append(attempts, NewAttempt(candidate, when))
	}
	addition := incumbent
	addition.Inode = AttemptLimit + 1
	addition.EmbeddedVersion = "2.0.0"
	if _, err := AddAttempt(attempts, NewAttempt(addition, now), now); err == nil {
		t.Fatal("AddAttempt() succeeded with a full ledger containing a future-uncertain entry")
	}
	if len(attempts) != AttemptLimit {
		t.Fatalf("input ledger length = %d, want %d", len(attempts), AttemptLimit)
	}
	if !attempts[0].IsFutureUncertain(now) {
		t.Fatalf("future-uncertain entry = %#v, want preserved", attempts[0])
	}
}

func TestAddAttemptDoesNotDeleteMatchingFutureUncertainEntry(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	candidate := ImageEvidence{
		Platform:        runtime.GOOS,
		Device:          1,
		Inode:           1,
		Size:            1,
		SHA256:          "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		EmbeddedVersion: "1.0.0",
	}
	future := NewAttempt(candidate, now.Add(AttemptFutureSkew+time.Second))
	addition := NewAttempt(candidate, now)
	attempts := []Attempt{future}
	if _, err := AddAttempt(attempts, addition, now); err == nil {
		t.Fatal("AddAttempt() succeeded with a matching future-uncertain entry")
	}
	if len(attempts) != 1 || attempts[0] != future {
		t.Fatalf("input ledger = %#v, want matching future entry preserved", attempts)
	}
}

func TestMergeAttemptsDoesNotReopenSettledEntry(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	evidence := ImageEvidence{
		Platform:        runtime.GOOS,
		Device:          1,
		Inode:           1,
		Size:            1,
		SHA256:          "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		EmbeddedVersion: "1.1.0",
	}
	settled := NewAttempt(evidence, now.Add(-time.Minute))
	settled.Status = AttemptStatusSettled
	stale := NewAttempt(evidence, now)
	merged, err := MergeAttempts([]Attempt{settled}, []Attempt{stale}, now)
	if err != nil {
		t.Fatalf("MergeAttempts() error = %v", err)
	}
	if len(merged) != 1 || merged[0].Status != AttemptStatusSettled {
		t.Fatalf("merged attempts = %#v, want one settled entry", merged)
	}
}

func TestMergeAttemptsPreservesNewerUnresolvedEntryAgainstOlderSettledView(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	evidence := ImageEvidence{
		Platform:        runtime.GOOS,
		Device:          1,
		Inode:           1,
		Size:            1,
		SHA256:          "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		EmbeddedVersion: "1.1.0",
	}
	current := NewAttempt(evidence, now)
	olderSettled := NewAttempt(evidence, now.Add(-time.Minute))
	olderSettled.Status = AttemptStatusSettled
	merged, err := MergeAttempts([]Attempt{current}, []Attempt{olderSettled}, now)
	if err != nil {
		t.Fatalf("MergeAttempts() error = %v", err)
	}
	if len(merged) != 1 || merged[0] != current {
		t.Fatalf("merged attempts = %#v, want newer unresolved entry", merged)
	}
}

func TestMergeAttemptsKeepsNewestUnresolvedEntry(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	evidence := ImageEvidence{
		Platform:        runtime.GOOS,
		Device:          1,
		Inode:           1,
		Size:            1,
		SHA256:          "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		EmbeddedVersion: "1.1.0",
	}
	older := NewAttempt(evidence, now.Add(-time.Minute))
	newer := NewAttempt(evidence, now)
	merged, err := MergeAttempts([]Attempt{older}, []Attempt{newer}, now)
	if err != nil {
		t.Fatalf("MergeAttempts() error = %v", err)
	}
	if len(merged) != 1 || merged[0] != newer {
		t.Fatalf("merged attempts = %#v, want newest unresolved entry", merged)
	}
}

func TestMergeAttemptsPreservesFutureUncertainty(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	evidence := ImageEvidence{
		Platform:        runtime.GOOS,
		Device:          1,
		Inode:           1,
		Size:            1,
		SHA256:          "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		EmbeddedVersion: "1.1.0",
	}
	current := NewAttempt(evidence, now)
	future := NewAttempt(evidence, now.Add(AttemptFutureSkew+time.Second))
	merged, err := MergeAttempts([]Attempt{current}, []Attempt{future}, now)
	if err != nil {
		t.Fatalf("MergeAttempts() error = %v", err)
	}
	if len(merged) != 1 || merged[0] != future || !merged[0].IsFutureUncertain(now) {
		t.Fatalf("merged attempts = %#v, want future-uncertain entry preserved", merged)
	}
}
