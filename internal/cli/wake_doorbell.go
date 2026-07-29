package cli

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"
)

type wakeDoorbellPhase uint8

const (
	wakeDoorbellIdle wakeDoorbellPhase = iota
	wakeDoorbellAwaitingToken
	wakeDoorbellAwaitingObservation
	wakeDoorbellObserved
)

const (
	wakeDoorbellRetryBase       = 5 * time.Second
	wakeDoorbellRetryMax        = 2 * time.Minute
	wakeDoorbellObservationHold = 2 * time.Minute
)

type wakeDoorbellState struct {
	phase            wakeDoorbellPhase
	token            string
	cohort           map[string]wakeFileIdentity
	tokenFailures    uint
	attempts         uint
	nextAttempt      time.Time
	observationUntil time.Time
}

type wakeDoorbellPlan struct {
	attempt bool
	retry   bool
	prompt  string
}

func (state *wakeDoorbellState) plan(
	now time.Time,
	current map[string]os.FileInfo,
	newToken func() (string, error),
) (wakeDoorbellPlan, error) {
	if len(current) == 0 {
		state.reset()
		return wakeDoorbellPlan{}, nil
	}

	if state.phase != wakeDoorbellIdle && wakeCohortProgressed(state.cohort, current) {
		state.reset()
	}
	if state.phase == wakeDoorbellObserved && !now.Before(state.observationUntil) {
		state.reset()
	}
	if state.phase == wakeDoorbellAwaitingToken && now.Before(state.nextAttempt) {
		return wakeDoorbellPlan{}, nil
	}
	if state.phase == wakeDoorbellIdle || state.phase == wakeDoorbellAwaitingToken {
		token, err := newToken()
		if err != nil {
			if state.phase == wakeDoorbellIdle {
				state.cohort = snapshotWakeFileIdentities(current)
			}
			state.phase = wakeDoorbellAwaitingToken
			state.tokenFailures++
			state.nextAttempt = now.Add(wakeDoorbellRetryDelay(state.tokenFailures))
			return wakeDoorbellPlan{}, err
		}
		state.phase = wakeDoorbellAwaitingObservation
		state.token = token
		state.cohort = snapshotWakeFileIdentities(current)
		state.tokenFailures = 0
		state.nextAttempt = time.Time{}
	}
	if state.phase == wakeDoorbellObserved || (state.attempts > 0 && now.Before(state.nextAttempt)) {
		return wakeDoorbellPlan{}, nil
	}

	return wakeDoorbellPlan{
		attempt: true,
		retry:   state.attempts > 0,
		prompt:  buildCoopWakeDoorbell(state.token),
	}, nil
}

func (state *wakeDoorbellState) recordAttempt(now time.Time) {
	state.attempts++
	state.nextAttempt = now.Add(wakeDoorbellRetryDelay(state.attempts))
}

func (state *wakeDoorbellState) observe(token string, now time.Time) bool {
	if state.phase != wakeDoorbellAwaitingObservation || token != state.token {
		return false
	}
	state.phase = wakeDoorbellObserved
	state.observationUntil = now.Add(wakeDoorbellObservationHold)
	return true
}

func (state *wakeDoorbellState) nextDeadline() (time.Time, bool) {
	switch state.phase {
	case wakeDoorbellAwaitingToken:
		return state.nextAttempt, !state.nextAttempt.IsZero()
	case wakeDoorbellAwaitingObservation:
		return state.nextAttempt, !state.nextAttempt.IsZero()
	case wakeDoorbellObserved:
		return state.observationUntil, !state.observationUntil.IsZero()
	default:
		return time.Time{}, false
	}
}

func (state *wakeDoorbellState) reset() {
	*state = wakeDoorbellState{}
}

func wakeDoorbellRetryDelay(attempt uint) time.Duration {
	delay := wakeDoorbellRetryBase
	for i := uint(1); i < attempt && delay < wakeDoorbellRetryMax; i++ {
		delay *= 2
		if delay >= wakeDoorbellRetryMax {
			return wakeDoorbellRetryMax
		}
	}
	return delay
}

func wakeCohortProgressed(
	cohort map[string]wakeFileIdentity,
	current map[string]os.FileInfo,
) bool {
	for name, identity := range cohort {
		info, ok := current[name]
		if !ok || !matchesWakeFileIdentity(identity, info) {
			return true
		}
	}
	return false
}

func generateWakeDoorbellToken() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", fmt.Errorf("generate wake doorbell token: %w", err)
	}
	return hex.EncodeToString(token[:]), nil
}

func validWakeDoorbellToken(token string) bool {
	if len(token) != 32 || token != strings.ToLower(token) {
		return false
	}
	_, err := hex.DecodeString(token)
	return err == nil
}
