package launch

import (
	"reflect"
	"testing"
)

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

func keepable(handle string) SeatFacts {
	return SeatFacts{
		Handle: handle, Managed: true, Owned: true, Attached: true, Live: true,
		Inspect: InspectPresent, ProfileMatch: true, OnLive: OnLiveKeep,
	}
}

func missing(handle string) SeatFacts {
	return SeatFacts{Handle: handle, Missing: true, Inspect: InspectAbsent, ProfileMatch: true, Managed: true}
}
