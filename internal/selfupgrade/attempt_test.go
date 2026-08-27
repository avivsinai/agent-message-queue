package selfupgrade

import (
	"testing"
	"time"
)

func TestAttemptIsFreshAllowsBoundedFutureClockSkew(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	attempt := Attempt{UnixTime: now.Add(time.Hour).Unix()}
	if !attempt.IsFresh(now) {
		t.Fatal("future-dated attempt within the skew bound is not fresh")
	}
	if attempt.IsFresh(now.Add(-26 * time.Hour)) {
		t.Fatal("future-dated attempt beyond the skew bound is fresh")
	}
}
