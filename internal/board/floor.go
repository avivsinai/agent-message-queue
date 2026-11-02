package board

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/board/usage"
)

// StationState is a seat's state on the Floor. The board cannot see a
// process, so no state says a worker runs or has stopped: each one names the
// evidence it rests on, and missing evidence is Unknown.
type StationState string

const (
	StationLimit   StationState = "limit_reached"
	StationStalled StationState = "stalled"
	StationWorking StationState = "working"
	StationIdle    StationState = "idle"
	StationOff     StationState = "off"
	StationUnknown StationState = "unknown"
	// stationDormant folds a mailbox into the dormant count; it is never a
	// station's state.
	stationDormant StationState = "dormant"
)

// StationStates is the Floor's order: the rule order of the state table.
var StationStates = []StationState{StationLimit, StationStalled, StationWorking, StationIdle, StationOff, StationUnknown}

// Title is the state word.
func (s StationState) Title() string {
	switch s {
	case StationLimit:
		return "Limit reached"
	case StationStalled:
		return "Stalled"
	case StationWorking:
		return "Working"
	case StationIdle:
		return "Idle"
	case StationOff:
		return "Off"
	case StationUnknown:
		return "Unknown"
	}
	return ""
}

// Glyph is the state's shape, shown beside its word.
func (s StationState) Glyph() string {
	switch s {
	case StationLimit:
		return "■"
	case StationStalled:
		return "▲"
	case StationWorking:
		return "●"
	case StationIdle:
		return "○"
	case StationOff:
		return "–"
	}
	return "?"
}

// Station is one agent seat: a mailbox of a session with recent evidence.
type Station struct {
	Session  string       `json:"session"`
	Handle   string       `json:"handle"`
	Harness  string       `json:"harness,omitempty"`
	State    StationState `json:"state"`
	StateWhy string       `json:"state_why"`
	// Now names the current cards the handle owns, most advanced stage
	// first.
	Now []string `json:"now"`
	// HandleSessions is set when a card names the handle without a session
	// and the handle has a mailbox in that many sessions: the card is listed
	// under each of them, and the board does not guess which one works on it.
	HandleSessions int `json:"handle_sessions,omitempty"`
	// Says is the agent's presence note and Status its free-text presence
	// status: self-reports, quoted, never facts.
	Says       string `json:"says,omitempty"`
	Status     string `json:"status,omitempty"`
	Counted    bool   `json:"counted"`
	CountError string `json:"count_error,omitempty"`
	Unread     int    `json:"unread"`
	// OldestUnreadAgeSeconds is the oldest unread message's age at the
	// projection time, dated from when AMQ was read.
	OldestUnreadAgeSeconds float64   `json:"oldest_unread_age_seconds,omitempty"`
	LastActivity           time.Time `json:"last_activity,omitzero"`
	// ActivitySource names the fact behind LastActivity: usage, or presence
	// when no wake is live.
	ActivitySource string      `json:"activity_source,omitempty"`
	Wake           StationWake `json:"wake"`
	// Limit is the account's reached limit while its reset is ahead. When
	// the seat, or any session of the account, was active after the limit
	// was read, LimitNote says so and the state comes from the other rules.
	Limit     *usage.Limit `json:"limit,omitempty"`
	LimitNote string       `json:"limit_note,omitempty"`
	Spend5h   *Usage       `json:"spend_5h,omitempty"`
}

// StationWake is the seat's wake as presence and doctor report it.
type StationWake struct {
	Live   bool   `json:"live"`
	Mode   string `json:"mode,omitempty"`
	Reason string `json:"reason,omitempty"`
	Parked bool   `json:"doorbell_parked,omitempty"`
}

// DormantMailboxes counts one session's mailboxes with no recent evidence.
type DormantMailboxes struct {
	Session string `json:"session"`
	Count   int    `json:"count"`
}

// nowStages are the stages a seat's current work is listed from, most
// advanced first.
var nowStages = map[Stage]int{StageInProgress: 0, StageInReview: 1, StageReady: 2, StageBlocked: 3}

// buildFloor makes one station per seat with recent evidence and folds every
// other agent mailbox into a per-session dormant count. It reads the roster
// with its usage attached and the placed Flow cards; observed is when the
// AMQ source read presence.
func buildFloor(m *Model, observed time.Time, cfg Config, now time.Time) {
	m.Stations, m.Dormant = []Station{}, []DormantMailboxes{}
	type owned struct {
		id    string
		rank  int
		since time.Time
	}
	bySeat := map[[2]string][]owned{}
	byHandle := map[string][]owned{}
	for _, col := range m.Columns {
		for _, c := range col.Cards {
			rank, ok := nowStages[c.Stage]
			if !ok {
				continue
			}
			for _, o := range c.Owners {
				if o.Kind != OwnerAgent || o.Handle == "" {
					continue
				}
				w := owned{c.ID, rank, c.StageSince}
				if o.Session != "" {
					bySeat[[2]string{o.Session, o.Handle}] = append(bySeat[[2]string{o.Session, o.Handle}], w)
				} else {
					byHandle[o.Handle] = append(byHandle[o.Handle], w)
				}
			}
		}
	}
	handleSessions := map[string]int{}
	for _, rs := range m.Roster {
		for _, a := range append(append([]RosterAgent{}, rs.Agents...), rs.Quiet...) {
			handleSessions[a.Handle]++
		}
	}
	for _, rs := range m.Roster {
		var stations []Station
		dormant := 0
		for _, a := range append(append([]RosterAgent{}, rs.Agents...), rs.Quiet...) {
			if a.Human {
				continue
			}
			key := [2]string{a.Session, a.Handle}
			work := append([]owned{}, bySeat[key]...)
			ownsWork := len(work) > 0
			if shared := byHandle[a.Handle]; len(shared) > 0 {
				work = append(work, shared...)
				// A card that names only the handle is this seat's work
				// when no other session has the handle.
				ownsWork = ownsWork || handleSessions[a.Handle] == 1
			}
			sort.SliceStable(work, func(i, j int) bool {
				if work[i].rank != work[j].rank {
					return work[i].rank < work[j].rank
				}
				if !work[i].since.Equal(work[j].since) {
					return work[i].since.Before(work[j].since)
				}
				return work[i].id < work[j].id
			})
			st := newStation(a, ownsWork, observed, cfg, now)
			if st.State == stationDormant {
				dormant++
				continue
			}
			for _, w := range work {
				st.Now = append(st.Now, w.id)
			}
			if len(byHandle[a.Handle]) > 0 && handleSessions[a.Handle] > 1 {
				st.HandleSessions = handleSessions[a.Handle]
			}
			stations = append(stations, st)
		}
		rank := map[StationState]int{}
		for i, s := range StationStates {
			rank[s] = i
		}
		sort.SliceStable(stations, func(i, j int) bool {
			if ri, rj := rank[stations[i].State], rank[stations[j].State]; ri != rj {
				return ri < rj
			}
			return stations[i].Handle < stations[j].Handle
		})
		m.Stations = append(m.Stations, stations...)
		if dormant > 0 {
			m.Dormant = append(m.Dormant, DormantMailboxes{Session: rs.Session, Count: dormant})
		}
	}
}

// newStation applies the seat table to one mailbox, first match wins. A
// mailbox with no live wake, no owned work, no activity inside the liveness
// window, and no unread mail newer than stale_after is dormant; so is one
// whose mail is fresh but whose last activity is older than stale_after,
// so an abandoned mailbox that receives mail does not alarm. Then:
// Limit reached (the account reports a reached limit whose reset is ahead
// and neither the seat nor any session of the account was active after it
// was read), Stalled (mail older than the
// liveness window waits and the last known activity is older than it),
// Working (activity inside the liveness window), Unknown (inbox not
// counted, or unread mail with no activity fact), Idle (wake live), and Off.
// ownsWork is whether a current card names this seat; observed is when AMQ
// was read, which dates the unread ages.
func newStation(a RosterAgent, ownsWork bool, observed time.Time, cfg Config, now time.Time) Station {
	st := Station{
		Session: a.Session, Handle: a.Handle, Harness: a.Harness, Now: []string{},
		Says: a.Note, Counted: a.Counted, CountError: a.CountError,
		Unread: a.UnreadCount,
		Wake:   StationWake{Live: a.PresenceSource == "notifier_live", Mode: a.NotifierMode, Reason: a.NotifierReason, Parked: a.DoorbellParked},
	}
	switch a.PresenceStatus {
	case "", "active", "idle", "unknown":
	default:
		st.Status = a.PresenceStatus
	}
	for i := range a.Usage {
		if a.Usage[i].Window == usage.Window5h {
			u := a.Usage[i]
			st.Spend5h = &u
		}
	}
	st.LastActivity, st.ActivitySource = lastActivity(a, observed)
	last := st.LastActivity
	known := !last.IsZero()
	recent := known && now.Sub(last) < cfg.LivenessWindow
	staleActivity := known && now.Sub(last) > cfg.StaleAfter
	// The oldest unread age was measured when AMQ was read; the mail
	// arrived then, and its age grows with the render time.
	read := observed
	if read.IsZero() {
		read = now
	}
	arrived := read.Add(-seconds(a.OldestUnreadAgeSeconds))
	oldest := now.Sub(arrived)
	if a.UnreadCount > 0 {
		st.OldestUnreadAgeSeconds = oldest.Seconds()
	}
	freshMail := a.Counted && a.UnreadCount > 0 && oldest < cfg.StaleAfter && !staleActivity
	if !a.ValidHandle || !st.Wake.Live && !ownsWork && !recent && !freshMail {
		st.State = stationDormant
		return st
	}

	if l := a.LimitReached; l != nil && (l.ResetsAt == nil || l.ResetsAt.After(now)) {
		st.Limit = l
		seatActive := known && last.After(l.AsOf)
		if !seatActive && !a.AccountActiveAt.After(l.AsOf) {
			st.State, st.StateWhy = StationLimit, limitWhy(*l, now)
			return st
		}
		st.LimitNote = "limit reported"
		if !l.AsOf.IsZero() {
			st.LimitNote += " " + age(now.Sub(l.AsOf)) + " ago"
		}
		if seatActive {
			st.LimitNote += "; active since then"
		} else {
			st.LimitNote += "; the account was active since then"
		}
	}
	unread := pluralize(a.UnreadCount, "unread message") + ", oldest " + age(oldest)
	switch {
	case a.Counted && a.UnreadCount > 0 && oldest > cfg.LivenessWindow && known && last.Before(arrived):
		st.State, st.StateWhy = StationStalled, unread+"; last activity "+age(now.Sub(last))+" ago"
	case recent:
		st.State, st.StateWhy = StationWorking, "active "+age(now.Sub(last))+" ago ("+st.ActivitySource+")"
	case !a.Counted:
		st.State, st.StateWhy = StationUnknown, "inbox not counted"
		if a.CountError != "" {
			st.StateWhy += ": " + a.CountError
		}
	case a.UnreadCount > 0 && !known:
		st.State, st.StateWhy = StationUnknown, unread+"; activity not observed"
	case st.Wake.Live && a.UnreadCount == 0:
		st.State, st.StateWhy = StationIdle, "wake live, waiting for mail"
	case st.Wake.Live:
		st.State, st.StateWhy = StationIdle, "wake live; "+unread
	case known:
		st.State, st.StateWhy = StationOff, "no wake; last activity "+age(now.Sub(last))+" ago"
	default:
		st.State, st.StateWhy = StationOff, "no wake; activity not observed"
	}
	return st
}

// lastActivity is the newest activity fact: the worker's newest usage
// record, or presence last seen when no wake is live. A live wake refreshes
// presence on its own, so with one presence proves only the wake. Doctor
// --ops reports presence age; a mailbox only who lists has none.
func lastActivity(a RosterAgent, observed time.Time) (time.Time, string) {
	var at time.Time
	source := ""
	doctorReported := a.Counted || a.CountError != ""
	wakeLive := a.PresenceSource == "notifier_live"
	if doctorReported && !wakeLive && !observed.IsZero() && a.PresenceStatus != "" && a.PresenceStatus != "unknown" {
		at, source = observed.Add(-seconds(a.PresenceAgeSeconds)), "presence"
	}
	for _, u := range a.Usage {
		if u.AsOf.After(at) {
			at, source = u.AsOf, "usage"
		}
	}
	return at, source
}

// limitWhy names a reached limit and its reset.
func limitWhy(l usage.Limit, now time.Time) string {
	s := limitName(l.Name) + " limit"
	if l.Name == "" {
		s = "limit"
	}
	if l.ResetsAt != nil {
		return s + ", resets " + weekdayTime(*l.ResetsAt, now)
	}
	s += ", reset not reported"
	if !l.AsOf.IsZero() {
		s += ", read " + age(now.Sub(l.AsOf)) + " ago"
	}
	return s
}

// weekdayTime prints a time inside the next day as a clock time, inside the
// next week with its weekday, and later with its date.
func weekdayTime(t, now time.Time) string {
	switch d := t.Sub(now); {
	case d >= 0 && d < 24*time.Hour:
		return t.Local().Format("15:04")
	case d >= 0 && d < 7*24*time.Hour:
		return t.Local().Format("Mon 15:04")
	}
	return t.Local().Format("Jan 2 15:04")
}

// harnessName is the harness as the owner knows it.
func harnessName(h string) string {
	switch h {
	case usage.Claude:
		return "Claude Code"
	case usage.Codex:
		return "Codex"
	}
	return h
}

// floorCounts is the Floor's headline: each state with seats, in table
// order, then the dormant count.
func floorCounts(m Model) (states []string, dormant string) {
	n := map[StationState]int{}
	for _, s := range m.Stations {
		n[s.State]++
	}
	for _, s := range StationStates {
		if n[s] > 0 {
			states = append(states, strconv.Itoa(n[s])+" "+strings.ToLower(s.Title()))
		}
	}
	total := 0
	for _, d := range m.Dormant {
		total += d.Count
	}
	if total > 0 {
		dormant = strconv.Itoa(total) + " dormant mailbox"
		if total != 1 {
			dormant += "es"
		}
		dormant += " in " + pluralize(len(m.Dormant), "session")
	}
	return states, dormant
}

// stationSummary is what a station shows on every face. Every value is
// cleaned for display.
type stationSummary struct {
	Harness, Now, NowStage, NowMore, Shared, Inbox, Activity, Wake, Says, Status, Limit, Spend string
}

func summarizeStation(st Station, cards map[string]Card, now time.Time) stationSummary {
	s := stationSummary{Harness: clean(harnessName(st.Harness)), Now: "no owned work", Says: cleanTitle(st.Says), Status: cleanTitle(st.Status), Limit: clean(st.LimitNote)}
	if len(st.Now) > 0 {
		c := cards[st.Now[0]]
		s.Now, s.NowStage = cleanTitle(c.Title), c.Stage.Title()
		if s.Now == "" {
			s.Now = clean(c.ID)
		}
		if n := len(st.Now) - 1; n > 0 {
			s.NowMore = "+" + strconv.Itoa(n) + " more"
		}
	}
	if st.HandleSessions > 0 {
		s.Shared = "handle in " + strconv.Itoa(st.HandleSessions) + " sessions"
	}
	switch {
	case !st.Counted:
		s.Inbox = "not counted"
	case st.Unread == 0:
		s.Inbox = "empty"
	default:
		s.Inbox = strconv.Itoa(st.Unread) + " unread · oldest " + age(seconds(st.OldestUnreadAgeSeconds))
	}
	s.Activity = "activity not observed"
	if !st.LastActivity.IsZero() {
		s.Activity = "active " + age(now.Sub(st.LastActivity)) + " ago (" + st.ActivitySource + ")"
	}
	s.Wake = "no wake"
	if st.Wake.Live {
		s.Wake = "wake live"
		if st.Wake.Mode != "" {
			s.Wake += " (" + clean(st.Wake.Mode) + ")"
		}
	}
	if st.Wake.Parked {
		s.Wake += " · doorbell parked"
	}
	if st.Spend5h != nil {
		s.Spend = clean(spendLine([]Usage{*st.Spend5h}, now, false))
	}
	return s
}
