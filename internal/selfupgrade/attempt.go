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

func (attempt Attempt) Matches(evidence ImageEvidence) bool {
	return attempt.Candidate == RefusedCandidateFromEvidence(evidence)
}

func (attempt Attempt) RefusalReason() string {
	return fmt.Sprintf(
		"candidate was exec'd at %s and did not settle; refusing for 24h",
		time.Unix(attempt.UnixTime, 0).UTC().Format(time.RFC3339),
	)
}

func (attempt Attempt) IsFresh(now time.Time) bool {
	if attempt.UnixTime <= 0 {
		return false
	}
	recordedAt := time.Unix(attempt.UnixTime, 0)
	age := now.UTC().Sub(recordedAt)
	return age > -AttemptMaxAge && age < AttemptMaxAge
}
