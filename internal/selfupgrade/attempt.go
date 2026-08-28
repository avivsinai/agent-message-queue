package selfupgrade

import (
	"fmt"
	"runtime"
	"strings"
	"time"
)

const (
	AttemptStatusAttempt = "attempt"
	AttemptStatusSettled = "settled"
	AttemptMaxAge        = 24 * time.Hour
	AttemptFutureSkew    = time.Hour
	AttemptLimit         = 8
)

// Attempt records the stable identity of an image that was about to replace
// the current process. It deliberately excludes paths and observation methods.
type Attempt struct {
	Status    string           `json:"status"`
	Candidate RefusedCandidate `json:"candidate"`
	UnixTime  int64            `json:"unix_time"`
}

func NewAttempt(evidence ImageEvidence, now time.Time) Attempt {
	return Attempt{
		Status:    AttemptStatusAttempt,
		Candidate: RefusedCandidateFromEvidence(evidence),
		UnixTime:  now.UTC().Unix(),
	}
}

func ValidateAttempt(attempt Attempt) error {
	return ValidateAttemptForPlatform(attempt, runtime.GOOS)
}

func ValidateAttemptForPlatform(attempt Attempt, platform string) error {
	if attempt.Status != AttemptStatusAttempt && attempt.Status != AttemptStatusSettled {
		return fmt.Errorf("self-upgrade attempt status %q is invalid", attempt.Status)
	}
	candidate := attempt.Candidate
	if candidate.Platform != platform || candidate.Device == 0 || candidate.Inode == 0 ||
		candidate.Size <= 0 || !ValidSHA256(candidate.SHA256) {
		return fmt.Errorf("self-upgrade attempt identity is invalid")
	}
	version := strings.TrimSpace(candidate.EmbeddedVersion)
	if version == "" || version != candidate.EmbeddedVersion || strings.ContainsRune(version, 0) {
		return fmt.Errorf("self-upgrade attempt version is invalid")
	}
	if attempt.UnixTime <= 0 {
		return fmt.Errorf("self-upgrade attempt time is invalid")
	}
	return nil
}

// ValidateAttempts validates the bounded ledger and rejects duplicate image
// identities. A duplicate would make settlement and refusal ownership
// ambiguous after a concurrent publication.
func ValidateAttempts(attempts []Attempt) error {
	if len(attempts) > AttemptLimit {
		return fmt.Errorf("self-upgrade attempt ledger exceeds the limit of %d", AttemptLimit)
	}
	seen := make(map[RefusedCandidate]struct{}, len(attempts))
	for _, attempt := range attempts {
		if err := ValidateAttempt(attempt); err != nil {
			return err
		}
		if _, exists := seen[attempt.Candidate]; exists {
			return fmt.Errorf("self-upgrade attempt ledger contains a duplicate candidate")
		}
		seen[attempt.Candidate] = struct{}{}
	}
	return nil
}

func (attempt Attempt) Matches(evidence ImageEvidence) bool {
	return attempt.Candidate == RefusedCandidateFromEvidence(evidence)
}

func (attempt Attempt) RefusalReason() string {
	return fmt.Sprintf(
		"replacement attempt was armed at %s and did not settle; refusing while the attempt is fresh relative to the recorded timestamp",
		time.Unix(attempt.UnixTime, 0).UTC().Format(time.RFC3339),
	)
}

func (attempt Attempt) IsFresh(now time.Time) bool {
	if attempt.UnixTime <= 0 {
		return false
	}
	recordedAt := time.Unix(attempt.UnixTime, 0)
	age := now.UTC().Sub(recordedAt)
	return age > -AttemptFutureSkew && age < AttemptMaxAge
}

func (attempt Attempt) IsExpired(now time.Time) bool {
	if attempt.Status != AttemptStatusAttempt || attempt.UnixTime <= 0 {
		return false
	}
	return now.UTC().Sub(time.Unix(attempt.UnixTime, 0)) >= AttemptMaxAge
}

func (attempt Attempt) IsFutureUncertain(now time.Time) bool {
	if attempt.UnixTime <= 0 {
		return false
	}
	return now.UTC().Sub(time.Unix(attempt.UnixTime, 0)) <= -AttemptFutureSkew
}

// PruneExpiredAttempts removes only unresolved attempts that are past the
// refusal window. Settled entries remain available for bounded ledger merge
// and audit until they are evicted by a newer attempt.
func PruneExpiredAttempts(attempts []Attempt, now time.Time) []Attempt {
	pruned := make([]Attempt, 0, len(attempts))
	for _, attempt := range attempts {
		if attempt.IsExpired(now) {
			continue
		}
		pruned = append(pruned, attempt)
	}
	return pruned
}

// AddAttempt appends a new attempt without evicting a fresh unresolved
// attempt. It returns an error when the bounded ledger contains no safe entry
// to evict.
func AddAttempt(attempts []Attempt, addition Attempt, now time.Time) ([]Attempt, error) {
	if err := ValidateAttempts(attempts); err != nil {
		return nil, err
	}
	if err := ValidateAttempt(addition); err != nil {
		return nil, err
	}
	current := PruneExpiredAttempts(attempts, now)
	for index, attempt := range current {
		if attempt.Candidate == addition.Candidate {
			if attempt.IsFutureUncertain(now) {
				return nil, fmt.Errorf("self-upgrade attempt timestamp is uncertain")
			}
			current = append(current[:index], current[index+1:]...)
			break
		}
	}
	if len(current) >= AttemptLimit {
		evict := -1
		for index, attempt := range current {
			if attempt.Status != AttemptStatusSettled {
				continue
			}
			if evict == -1 || attempt.UnixTime < current[evict].UnixTime {
				evict = index
			}
		}
		if evict == -1 {
			for index, attempt := range current {
				if attempt.Status != AttemptStatusAttempt || attempt.IsFresh(now) || attempt.IsFutureUncertain(now) {
					continue
				}
				if evict == -1 || attempt.UnixTime < current[evict].UnixTime {
					evict = index
				}
			}
		}
		if evict == -1 {
			return nil, fmt.Errorf("self-upgrade attempt ledger is full of fresh unresolved attempts")
		}
		current = append(current[:evict], current[evict+1:]...)
	}
	current = append(current, addition)
	return current, ValidateAttempts(current)
}

// MergeAttempts unions two controller views while preserving the bounded
// ledger invariant. Settled entries are terminal, so a stale unresolved view
// cannot reopen an entry that another controller settled.
func MergeAttempts(current, desired []Attempt, now time.Time) ([]Attempt, error) {
	if err := ValidateAttempts(current); err != nil {
		return nil, err
	}
	if err := ValidateAttempts(desired); err != nil {
		return nil, err
	}
	merged := PruneExpiredAttempts(current, now)
	for _, attempt := range desired {
		mergedExisting := false
		for index, existing := range merged {
			if existing.Candidate == attempt.Candidate {
				switch {
				case existing.IsFutureUncertain(now):
					if !attempt.IsFutureUncertain(now) || attempt.UnixTime <= existing.UnixTime {
						attempt = existing
					}
				case attempt.IsFutureUncertain(now):
					// Preserve an uncertain timestamp so the caller can fail closed.
				case existing.Status == AttemptStatusSettled ||
					(attempt.Status == AttemptStatusSettled && attempt.UnixTime < existing.UnixTime):
					attempt = existing
				case attempt.Status != AttemptStatusSettled && attempt.UnixTime <= existing.UnixTime:
					attempt = existing
				}
				merged[index] = attempt
				mergedExisting = true
				break
			}
		}
		if mergedExisting {
			continue
		}
		var err error
		merged, err = AddAttempt(merged, attempt, now)
		if err != nil {
			return nil, err
		}
	}
	return merged, nil
}
