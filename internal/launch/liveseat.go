package launch

const (
	SeatKept    = "kept"
	SeatCreated = "created"
	SeatRefused = "refused"
)

const (
	ReasonLiveParticipantRefused = "live_participant_refused"
	ReasonCohortRefused          = "cohort_refused"
)

const (
	StartModeResumed = "resumed"
	StartModeFresh   = "fresh"
)

const (
	OnLiveRefuse = "refuse"
	OnLiveKeep   = "keep"
)

// SeatFacts is the observation for one desired handle. Keep eligibility is
// the conjunction of managed, owned, attached, and live; hostile facts
// (foreign, stale, profile mismatch, unknown inspect) fail closed.
type SeatFacts struct {
	Handle       string
	Managed      bool
	Owned        bool
	Attached     bool
	Live         bool
	Inspect      InspectStatus
	Foreign      bool
	Stale        bool
	ProfileMatch bool
	Missing      bool
	OnLive       string
}

// SeatDisposition is the Apply preflight row for one handle.
type SeatDisposition struct {
	Handle     string
	Decision   string
	ReasonCode string
	StartMode  string
}

func EligibleToKeep(facts SeatFacts) bool {
	return facts.Managed && facts.Owned && facts.Attached && facts.Live &&
		facts.Inspect == InspectPresent && facts.ProfileMatch && !facts.Foreign && !facts.Stale && !facts.Missing
}

// ClassifyLiveSeats applies the cohort rule as a pure function: any refused
// live seat turns remaining missing seats into cohort_refused; if every live
// seat is kept, missing seats are created.
func ClassifyLiveSeats(roster []SeatFacts) []SeatDisposition {
	rows := make([]SeatDisposition, 0, len(roster))
	refused := false
	for _, facts := range roster {
		row := classifyOneLiveSeat(facts)
		if row.Decision == SeatRefused {
			refused = true
		}
		rows = append(rows, row)
	}
	if !refused {
		return rows
	}
	for i := range rows {
		if rows[i].Decision == SeatCreated {
			rows[i] = SeatDisposition{
				Handle: rows[i].Handle, Decision: SeatRefused,
				ReasonCode: ReasonCohortRefused,
			}
		}
	}
	return rows
}

func classifyOneLiveSeat(facts SeatFacts) SeatDisposition {
	if EligibleToKeep(facts) {
		if facts.OnLive == OnLiveKeep {
			return SeatDisposition{Handle: facts.Handle, Decision: SeatKept, StartMode: StartModeResumed}
		}
		return SeatDisposition{Handle: facts.Handle, Decision: SeatRefused, ReasonCode: ReasonLiveParticipantRefused}
	}
	if facts.Missing && facts.Inspect == InspectAbsent && !facts.Foreign && !facts.Stale {
		return SeatDisposition{Handle: facts.Handle, Decision: SeatCreated, StartMode: StartModeFresh}
	}
	return SeatDisposition{Handle: facts.Handle, Decision: SeatRefused, ReasonCode: ReasonLiveParticipantRefused}
}

func tmuxOmittedPlacementJoinSupported(backend string, placement *Placement) bool {
	return backend == LauncherTMux && placement == nil
}

func onLiveKeepHandles(participants []PrepareParticipant) []string {
	handles := make([]string, 0)
	for _, participant := range participants {
		if participant.OnLive == OnLiveKeep {
			handles = append(handles, participant.Handle)
		}
	}
	return handles
}

func ticketAgentsForCreate(planned []plannedAgent, joinBinding *BindingRecord) []plannedAgent {
	if joinBinding == nil {
		return planned
	}
	filtered := make([]plannedAgent, 0, len(planned))
	for _, agent := range planned {
		if agent.write {
			filtered = append(filtered, agent)
		}
	}
	return filtered
}

func reconcileLiveSeats(request ReconcileRequest, binding BindingRecord) ([]SeatDisposition, bool, string) {
	anyKeep := false
	for _, policy := range request.OnLive {
		if policy == OnLiveKeep {
			anyKeep = true
			break
		}
	}
	if !anyKeep {
		return nil, false, ""
	}
	backend := request.Backends[binding.Backend]
	if backend == nil {
		return nil, false, ReasonLiveParticipantRefused
	}
	inspection, err := backend.Inspect(InspectRequest{Binding: binding, Root: request.Root})
	if err != nil {
		return nil, false, ReasonLiveParticipantRefused
	}
	foreign := request.HostIdentity != "" && binding.HostIdentity != request.HostIdentity
	detect := backend.Detect()
	if detect.InstanceIdentity != "" && binding.InstanceIdentity != detect.InstanceIdentity {
		foreign = true
	}
	participants := make([]PrepareParticipant, 0, len(request.Config.Agents))
	for _, cfg := range request.Config.Agents {
		participants = append(participants, PrepareParticipant{
			Handle: cfg.Handle, Runnable: true, OnLive: request.OnLive[cfg.Handle],
		})
	}
	seats := ClassifyLiveSeats(seatFactsFromBinding(participants, &binding, inspection.Status, detect.Profile.Identity(), foreign))
	kept, created := 0, 0
	reason := ""
	for _, seat := range seats {
		switch seat.Decision {
		case SeatRefused:
			if reason == "" {
				reason = seat.ReasonCode
			}
		case SeatKept:
			kept++
		case SeatCreated:
			created++
		}
	}
	if reason != "" {
		return seats, false, reason
	}
	if kept > 0 && created > 0 {
		if !tmuxOmittedPlacementJoinSupported(binding.Backend, request.Placement) {
			return seats, false, PlacementUnsupportedReason
		}
		return seats, true, ""
	}
	return seats, false, ""
}

func seatFactsFromBinding(participants []PrepareParticipant, binding *BindingRecord, inspection InspectStatus, profile string, foreign bool) []SeatFacts {
	owned := map[string]bool{}
	if binding != nil {
		for _, resource := range binding.Resources.Resources {
			if resource.Agent != "" {
				owned[resource.Agent] = true
			}
		}
	}
	profileMatch := binding != nil && binding.Profile == profile
	facts := make([]SeatFacts, 0, len(participants))
	for _, participant := range participants {
		if !participant.Runnable {
			continue
		}
		hasResource := owned[participant.Handle]
		row := SeatFacts{
			Handle: participant.Handle, Managed: true, OnLive: participant.OnLive,
			ProfileMatch: profileMatch, Foreign: foreign,
		}
		if !hasResource {
			row.Missing = true
			row.Inspect = InspectAbsent
			facts = append(facts, row)
			continue
		}
		row.Owned = true
		row.Inspect = inspection
		row.Stale = inspection == InspectUnknown
		row.Attached = inspection == InspectPresent
		row.Live = inspection == InspectPresent
		facts = append(facts, row)
	}
	return facts
}
