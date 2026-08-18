package launch

import (
	"reflect"
	"testing"
)

func TestEligibleToKeepRequiresManagedOwnedAttachedLive(t *testing.T) {
	ok := keepable("claude")
	if !EligibleToKeep(ok) {
		t.Fatal("complete facts must be eligible")
	}
	for _, mutate := range []func(*SeatFacts){
		func(f *SeatFacts) { f.Managed = false },
		func(f *SeatFacts) { f.Owned = false },
		func(f *SeatFacts) { f.Attached = false },
		func(f *SeatFacts) { f.Live = false },
		func(f *SeatFacts) { f.Inspect = InspectUnknown },
		func(f *SeatFacts) { f.Foreign = true },
		func(f *SeatFacts) { f.Stale = true },
		func(f *SeatFacts) { f.ProfileMatch = false },
		func(f *SeatFacts) { f.Missing = true },
	} {
		facts := ok
		mutate(&facts)
		if EligibleToKeep(facts) {
			t.Fatalf("still eligible after hostile mutate: %#v", facts)
		}
	}
}

func TestClassifyLiveSeatsKeepsLiveAndCreatesMissing(t *testing.T) {
	rows := ClassifyLiveSeats([]SeatFacts{
		keepable("claude"),
		missing("codex"),
	})
	want := []SeatDisposition{
		{Handle: "claude", Decision: SeatKept, StartMode: StartModeResumed},
		{Handle: "codex", Decision: SeatCreated, StartMode: StartModeFresh},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows = %#v", rows)
	}
}

func TestClassifyLiveSeatsForeignRefusesAndCohortBlocksMissing(t *testing.T) {
	foreign := keepable("claude")
	foreign.Foreign = true
	rows := ClassifyLiveSeats([]SeatFacts{foreign, missing("codex")})
	want := []SeatDisposition{
		{Handle: "claude", Decision: SeatRefused, ReasonCode: ReasonLiveParticipantRefused},
		{Handle: "codex", Decision: SeatRefused, ReasonCode: ReasonCohortRefused},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("foreign cohort = %#v", rows)
	}
}

func TestClassifyLiveSeatsStaleAndProfileMismatchRefuse(t *testing.T) {
	stale := keepable("claude")
	stale.Stale = true
	mismatch := keepable("codex")
	mismatch.ProfileMatch = false
	rows := ClassifyLiveSeats([]SeatFacts{stale, mismatch, missing("reviewer")})
	if rows[0].Decision != SeatRefused || rows[0].ReasonCode != ReasonLiveParticipantRefused {
		t.Fatalf("stale = %#v", rows[0])
	}
	if rows[1].Decision != SeatRefused || rows[1].ReasonCode != ReasonLiveParticipantRefused {
		t.Fatalf("profile mismatch = %#v", rows[1])
	}
	if rows[2].Decision != SeatRefused || rows[2].ReasonCode != ReasonCohortRefused {
		t.Fatalf("cohort missing = %#v", rows[2])
	}
}

func TestClassifyLiveSeatsUnknownInspectRefuses(t *testing.T) {
	unknown := keepable("claude")
	unknown.Inspect = InspectUnknown
	unknown.Live = false
	rows := ClassifyLiveSeats([]SeatFacts{unknown, missing("codex")})
	if rows[0].ReasonCode != ReasonLiveParticipantRefused || rows[1].ReasonCode != ReasonCohortRefused {
		t.Fatalf("unknown inspect = %#v", rows)
	}
}

func TestClassifyLiveSeatsDefaultRefuseBlocksMissing(t *testing.T) {
	live := keepable("claude")
	live.OnLive = ""
	rows := ClassifyLiveSeats([]SeatFacts{live, missing("codex")})
	want := []SeatDisposition{
		{Handle: "claude", Decision: SeatRefused, ReasonCode: ReasonLiveParticipantRefused},
		{Handle: "codex", Decision: SeatRefused, ReasonCode: ReasonCohortRefused},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("default refuse = %#v", rows)
	}
}

func keepable(handle string) SeatFacts {
	return SeatFacts{
		Handle: handle, Managed: true, Owned: true, Attached: true, Live: true,
		Inspect: InspectPresent, ProfileMatch: true, OnLive: OnLiveKeep,
	}
}

func missing(handle string) SeatFacts {
	return SeatFacts{Handle: handle, Missing: true, Inspect: InspectAbsent, ProfileMatch: true, Managed: true}
}
