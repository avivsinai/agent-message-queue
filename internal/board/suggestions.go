package board

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/avivsinai/agent-message-queue/internal/board/decision"
	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/lock"
)

type MessageObservation struct {
	AttemptFingerprint string    `json:"suggestion_attempt_fingerprint,omitempty"`
	FirstSeen          time.Time `json:"first_seen"`
	Bootstrap          bool      `json:"bootstrap"`
}
type EvidenceUnit struct {
	ID         string `json:"id"`
	MessageKey string `json:"message_key"`
	Revision   string `json:"revision"`
	Field      string `json:"field"`
	Start      int    `json:"start"`
	End        int    `json:"end"`
	Quote      string `json:"quote"`
}
type SuggestionChoice struct {
	ID         string        `json:"id"`
	Kind       string        `json:"kind"`
	TaskID     string        `json:"task_id,omitempty"`
	Role       string        `json:"role,omitempty"`
	Outcome    *EvidenceUnit `json:"outcome,omitempty"`
	CurrentAsk *EvidenceUnit `json:"current_ask,omitempty"`
}
type SuggestionCandidate struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	State       State    `json:"recorded_state"`
	Cancelled   bool     `json:"cancelled"`
	NotReported bool     `json:"not_reported"`
	Aliases     []string `json:"aliases"`
}

type Suggestion struct {
	Stale      bool                  `json:"stale"`
	Candidates []SuggestionCandidate `json:"candidates"`
	ID         string                `json:"id"`
	Unit       EvidenceUnit          `json:"unit"`
	Choices    []SuggestionChoice    `json:"choices"`
	Selected   []string              `json:"selected"`
	Ignored    bool                  `json:"ignored"`
	Adopted    bool                  `json:"adopted"`
	Commands   []Action              `json:"commands"`
}
type SuggestionAttempt struct {
	Context     *EpisodeContext  `json:"context,omitempty"`
	Fingerprint string           `json:"fingerprint"`
	MessageKey  string           `json:"message_key,omitempty"`
	Status      string           `json:"status"`
	Detail      string           `json:"detail"`
	At          time.Time        `json:"at"`
	Runtime     *decision.Result `json:"runtime,omitempty"`
}
type SuggestionState struct {
	UserConfigured bool                  `json:"user_configured"`
	Candidates     []SuggestionCandidate `json:"candidates"`
	Units          []EvidenceUnit        `json:"units"`
	Expanded       bool                  `json:"expanded"`
	Config         decision.Config       `json:"config"`
	Fingerprint    string                `json:"fingerprint"`
	Proposals      []Suggestion          `json:"proposals"`
	Attempt        SuggestionAttempt     `json:"latest_attempt"`
}
type ModelAttachment struct {
	Unit         EvidenceUnit `json:"unit"`
	Role         string       `json:"role"`
	SuggestionID string       `json:"suggestion_id"`
}
type SuggestionAdoption struct {
	ID                 string
	Choices            []string
	ExpectedGeneration uint64
	Title              string
	ConfirmIndependent bool
}

func digestValue(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
func historyComplete(s Snapshot) bool {
	for _, nr := range s.Input.Results {
		if nr.Name == "amq-history" {
			return nr.Result.Status == StatusOK && !nr.Result.Coverage.Partial()
		}
	}
	return false
}
func (s *Snapshot) observeMessages(now time.Time) {
	if s.Observations == nil {
		s.Observations = map[string]MessageObservation{}
	}
	for key := range s.Messages {
		if _, ok := s.Observations[key]; !ok {
			s.Observations[key] = MessageObservation{FirstSeen: now}
		}
	}
	if s.HistoryBaseline.IsZero() && historyComplete(*s) {
		s.HistoryBaseline = now
		for key, o := range s.Observations {
			o.Bootstrap = true
			s.Observations[key] = o
		}
	}
}

// SuggestionFingerprint excludes proposal state and acquisition-only clocks.
func (s Snapshot) SuggestionFingerprint(cfg decision.Config) string {
	facts := map[string]AliasFact{}
	for k, v := range s.Facts {
		v.ObservedAt = time.Time{}
		v.Fact.Reason.Since = time.Time{}
		facts[k] = v
	}
	tasks := map[string]Task{}
	for k, v := range s.Tasks {
		v.ObservedAt = time.Time{}
		v.OwnerFact.Reason.Since = time.Time{}
		tasks[k] = v
	}
	type coverage struct {
		Name     string
		Status   SourceStatus
		Coverage Coverage
		Detail   string
		Actions  map[string]string
		Scopes   map[string]NativeScope
	}
	sources := []coverage{}
	for _, nr := range s.Input.Results {
		sources = append(sources, coverage{nr.Name, nr.Result.Status, nr.Result.Coverage, nr.Result.Detail, nr.Result.ActionStates, nr.Result.NativeScopes})
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].Name < sources[j].Name })
	type observedEligibility struct {
		Bootstrap                bool
		PostBaseline             bool
		CreatedBeforeObservation bool
	}
	bootstrap := map[string]observedEligibility{}
	for key, o := range s.Observations {
		created, err := time.Parse(time.RFC3339Nano, s.Messages[key].Created)
		bootstrap[key] = observedEligibility{o.Bootstrap, o.FirstSeen.After(s.HistoryBaseline), err == nil && !created.After(o.FirstSeen)}
	}
	return digestValue(struct {
		Project              string
		Config               decision.Config
		Messages             map[string]Message
		Facts                map[string]AliasFact
		Tasks                map[string]Task
		Anchors, Aliases     map[string]string
		Dismissed, Cancelled map[string]bool
		Withdrawals          map[string]string
		Baseline             time.Time
		Bootstrap            map[string]observedEligibility
		Sources              []coverage
		Grants               map[string]ClaudeListBinding
		Suppressed           map[string]bool
		Adopted              map[string]string
	}{s.ProjectID, cfg, s.Messages, facts, tasks, s.Anchors, s.Aliases, s.Dismissed, s.ActionCancelled, s.Withdrawals, s.HistoryBaseline, bootstrap, sources, s.ClaudeLists, s.SuggestionIgnored, s.SuggestionAdopted})
}
func unitValid(s Snapshot, u EvidenceUnit) bool {
	m, ok := s.Messages[u.MessageKey]
	if !ok || m.EvidenceHash != u.Revision || u.Field != "body" || u.Start < 0 || u.End <= u.Start || u.End > len(m.Body) {
		return false
	}
	return utf8.ValidString(m.Body[:u.Start]) && utf8.ValidString(m.Body[:u.End]) && m.Body[u.Start:u.End] == u.Quote && u.ID == "unit-"+digestValue(struct {
		Key, Revision, Field string
		Start, End           int
	}{u.MessageKey, u.Revision, u.Field, u.Start, u.End})[:32]
}
func makeUnit(m Message, start, end int) EvidenceUnit {
	u := EvidenceUnit{MessageKey: m.Key, Revision: m.EvidenceHash, Field: "body", Start: start, End: end, Quote: m.Body[start:end]}
	u.ID = "unit-" + digestValue(struct {
		Key, Revision, Field string
		Start, End           int
	}{u.MessageKey, u.Revision, u.Field, u.Start, u.End})[:32]
	return u
}
func actionState(s Snapshot, key string) string {
	for _, nr := range s.Input.Results {
		if state := nr.Result.ActionStates[key]; state != "" {
			return state
		}
	}
	return "unknown"
}
func unitsFor(s Snapshot, key string) ([]EvidenceUnit, error) {
	keys := []string{}
	fp := s.SuggestionFingerprint(s.Suggestions.Config)
	anchored := exactAnchors(s)
	for k, m := range s.Messages {
		if key == "" && s.Observations[k].AttemptFingerprint == fp {
			continue
		}
		if (key == "" || key == k) && format.IsValidKind(m.Kind) && !s.Dismissed[k] && !s.ActionCancelled[k] && anchored[k] == nil {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := s.Messages[keys[i]], s.Messages[keys[j]]
		aCurrent, bCurrent := currentObservation(s, a), currentObservation(s, b)
		if aCurrent != bCurrent {
			return aCurrent
		}
		last, hasLast := s.Messages[s.Suggestions.Attempt.MessageKey]
		if key == "" && hasLast && aCurrent == currentObservation(s, last) {
			aAfter, bAfter := a.Created > last.Created || a.Created == last.Created && a.Key > last.Key, b.Created > last.Created || b.Created == last.Created && b.Key > last.Key
			if aAfter != bAfter {
				return aAfter
			}
		}
		if a.Created != b.Created {
			return a.Created < b.Created
		}
		return keys[i] < keys[j]
	})
	if key != "" && len(keys) == 0 {
		return nil, errors.New("exact message is absent, has an owner/resolved disposition, or is linked by exact id")
	}
	units := []EvidenceUnit{}
	adoptedUnits := map[string]bool{}
	for _, task := range s.Tasks {
		if task.RedirectTo != "" {
			continue
		}
		for _, binding := range task.ModelAttachments {
			adoptedUnits[binding.Unit.ID] = true
		}
	}
	for _, k := range keys {
		m := s.Messages[k]
		offset := 0
		for _, p := range strings.Split(m.Body, "\n\n") {
			start := offset
			offset += len(p) + 2
			trimmed := strings.TrimSpace(p)
			if trimmed == "" {
				continue
			}
			start += strings.Index(p, trimmed)
			u := makeUnit(m, start, start+len(trimmed))
			if s.SuggestionIgnored[u.ID] || adoptedUnits[u.ID] {
				continue
			}
			if len(units) == 8 {
				return units, errors.New("episode exceeds eight units; no text was truncated")
			}
			units = append(units, u)
		}
		if len(units) > 0 {
			return units, nil
		}
	}
	return units, nil
}

// NEW needs positive current intent in addition to structural outcome text.
func newEligible(s Snapshot, u EvidenceUnit, now time.Time) bool {
	m := s.Messages[u.MessageKey]
	episode, episodeErr := buildEpisode(s, []EvidenceUnit{u})
	if episodeErr != nil || episode.Partial {
		return false
	}
	o, ok := s.Observations[m.Key]
	created, err := time.Parse(time.RFC3339Nano, m.Created)
	if !ok || err != nil || s.HistoryBaseline.IsZero() || o.Bootstrap || !o.FirstSeen.After(s.HistoryBaseline) || !created.After(s.HistoryBaseline) || created.After(now) || created.After(o.FirstSeen) || !historyComplete(s) || actionState(s, m.Key) != "open" || (m.Kind != "todo" && m.Kind != "question") || s.Dismissed[m.Key] || s.ActionCancelled[m.Key] {
		return false
	}
	for _, prior := range s.Messages {
		if referencesMessage(m, prior.ID) && (s.ActionCancelled[prior.Key] || s.Withdrawals[prior.Key] != "") {
			return false
		}
	}
	for _, task := range s.Tasks {
		if task.RedirectTo != "" || !task.Cancelled {
			continue
		}
		for _, binding := range task.ModelAttachments {
			if binding.Unit.ID == u.ID || binding.Unit.MessageKey == m.Key || referencesMessage(m, s.Messages[binding.Unit.MessageKey].ID) {
				return false
			}
		}
		for _, key := range task.Attachments {
			if key == m.Key || referencesMessage(m, s.Messages[key].ID) {
				return false
			}
		}
		for _, alias := range task.Aliases {
			for _, ref := range m.Refs {
				if ref == s.Facts[alias].Fact.Ref.ID {
					return false
				}
			}
		}
	}
	text := strings.ToLower(strings.TrimSpace(u.Quote))
	if len(text) < 24 || len(text) > 1024 || strings.ContainsAny(text, "\n;•") || strings.Count(m.Body, "\n\n") > 0 {
		return false
	}
	for _, bad := range []string{" and ", " then ", "continue", "resume", "same as", "ping", "pong", "probe", "test message", "are you there", "this task", "that task", "above", "previous", "withdraw", "cancel", "review"} {
		if strings.Contains(text, bad) {
			return false
		}
	}
	for _, prefix := range []string{"build ", "implement ", "add ", "fix ", "create ", "deliver ", "please build ", "please implement ", "please add ", "please fix ", "can you build ", "can you implement ", "can you add ", "can you fix "} {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
}
func words(text string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if len(w) > 2 {
			out[w] = true
		}
	}
	return out
}
func candidates(s Snapshot, units []EvidenceUnit, limit int) []TaskCatalogueEntry {
	type ranked struct {
		Task              TaskCatalogueEntry
		Explicit, Context bool
		Score             int
	}
	textWords, directIDs, contextIDs := map[string]bool{}, map[string]bool{}, map[string]bool{}
	directRefs, contextRefs := map[string]bool{}, map[string]bool{}
	directMessages := map[string]bool{}
	addText := func(text string, ids map[string]bool) {
		for word := range words(text) {
			textWords[word] = true
		}
		for id := range exactBeadTokens(text) {
			ids[id] = true
		}
	}
	for _, u := range units {
		m := s.Messages[u.MessageKey]
		directMessages[m.Key] = true
		addText(u.Quote+" "+m.Subject, directIDs)
		for _, ref := range m.Refs {
			directRefs[ref] = true
		}
	}
	if episode, err := buildEpisode(s, units); err == nil {
		for _, m := range episode.Messages {
			if directMessages[m.Key] {
				continue
			}
			addText(m.Body+" "+m.Subject, contextIDs)
			for _, ref := range m.Refs {
				contextRefs[ref] = true
			}
		}
	}
	rows := []ranked{}
	for _, task := range s.Catalogue() {
		score := 0
		explicit, context := false, false
		tw := words(task.Title)
		for _, fact := range task.Facts {
			for w := range words(fact.Fact.Title) {
				tw[w] = true
			}
			if fact.Fact.Ref.Kind != "claude-task" {
				explicit = explicit || directRefs[fact.Fact.Ref.ID]
				context = context || contextRefs[fact.Fact.Ref.ID]
			}
			if fact.Fact.Ref.Kind == RefBead {
				explicit = explicit || directIDs[fact.Fact.Ref.ID]
				context = context || contextIDs[fact.Fact.Ref.ID]
			}
		}
		for w := range textWords {
			if tw[w] {
				score++
			}
		}
		for _, u := range units {
			m := s.Messages[u.MessageKey]
			for _, a := range task.Attachments {
				am := s.Messages[a]
				if a == m.Key {
					score += 100
				}
				if am.Thread != "" && am.Thread == m.Thread {
					score += 20
				}
				if referencesMessage(m, am.ID) {
					score += 40
				}
			}
		}
		rows = append(rows, ranked{task, explicit, context, score})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Explicit != rows[j].Explicit {
			return rows[i].Explicit
		}
		if rows[i].Context != rows[j].Context {
			return rows[i].Context
		}
		if rows[i].Score != rows[j].Score {
			return rows[i].Score > rows[j].Score
		}
		return rows[i].Task.ID < rows[j].Task.ID
	})
	out := []TaskCatalogueEntry{}
	for i := 0; i < len(rows) && i < limit; i++ {
		out = append(out, rows[i].Task)
	}
	return out
}

func buildDecision(s Snapshot, units []EvidenceUnit, expanded bool, now time.Time) (decision.Request, map[string]SuggestionChoice) {
	limit := 8
	if expanded {
		limit = 24
	}
	tasks := candidates(s, units, limit)
	req := decision.Request{Context: "Suggestions only; AMQ quality is not measured. Native gaps remain unknown. Search is bounded, not uniqueness proof. Status updates, answers, review responses and resolved asks can attach to existing tasks as updates or evidence; attachment does not require a new ask. Current, closed and cancelled candidates follow: " + candidateContext(tasks) + "\nSource coverage: " + sourceContext(s) + "\nExact episode context: " + episodeContext(s, units), Interpretations: []decision.Interpretation{{ID: "non_actionable", Description: "No supported task attachment or actionable outcome in this unit, such as unrelated chatter or a delivery probe", Exclusive: true}, {ID: "unresolved", Description: "Insufficient context, compound/continuation ambiguity, or none of the supplied choices", Exclusive: true}}}
	choices := map[string]SuggestionChoice{"non_actionable": {ID: "non_actionable", Kind: "non_actionable"}, "unresolved": {ID: "unresolved", Kind: "unresolved"}}
	for ti, t := range tasks {
		for _, role := range []string{"request", "review", "blocker", "update", "evidence"} {
			id := fmt.Sprintf("t%d_%s", ti, role)
			choices[id] = SuggestionChoice{ID: id, Kind: "attach", TaskID: t.ID, Role: role}
			req.Interpretations = append(req.Interpretations, decision.Interpretation{ID: id, Description: t.ID + " " + role})
		}
	}
	attachmentIDs := []string{}
	for _, it := range req.Interpretations[2:] {
		attachmentIDs = append(attachmentIDs, it.ID)
	}
	for ui, u := range units {
		allowed := []string{"non_actionable", "unresolved"}
		allowed = append(allowed, attachmentIDs...)
		if newEligible(s, u, now) {
			id := fmt.Sprintf("new_%d", ui)
			copy := u
			choices[id] = SuggestionChoice{ID: id, Kind: "new", Outcome: &copy, CurrentAsk: &copy}
			req.Interpretations = append(req.Interpretations, decision.Interpretation{ID: id, Description: "Independent new outcome exactly quoted in " + u.ID + "; current ask is that same exact passage. Owner must confirm uniqueness.", Exclusive: true})
			allowed = append(allowed, id)
		}
		m := s.Messages[u.MessageKey]
		req.Evidence = append(req.Evidence, decision.Unit{ID: u.ID, Text: u.Quote, Context: "Subject: " + m.Subject + "; kind: " + m.Kind + "; thread: " + m.Thread + "; reply_to: " + m.ReplyTo + "; refs: " + strings.Join(m.Refs, ",") + "; expanded search: " + fmt.Sprint(expanded), Allowed: allowed})
	}
	return req, choices
}
func stringJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

// Suggest uses the production decision client outside the publication lock.
func (s Store) Suggest(ctx context.Context, cfg decision.Config, key string, manual bool, currentConfig func() decision.Config) error {
	budget, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	initial, err := s.ReadContext(budget)
	if err != nil {
		return err
	}
	s.ProjectID = initial.ProjectID
	return lock.WithExclusiveFileLockUntil(filepath.Join(s.Dir, "inference.lock"), func() bool { return budget.Err() != nil }, func() error {
		state, err := s.ReadContext(budget)
		if err != nil {
			return err
		}
		if state.Suggestions.Config != cfg || state.Suggestions.UserConfigured != !manual {
			if err = s.update(budget, func(current *Snapshot) error {
				current.Suggestions.Config = cfg
				current.Suggestions.UserConfigured = !manual
				return nil
			}); err != nil {
				return err
			}
			state, err = s.ReadContext(budget)
			if err != nil {
				return err
			}
		}
		fp := state.SuggestionFingerprint(cfg)
		units, planErr := unitsFor(state, key)
		if planErr == nil && len(units) > 0 {
			_, planErr = buildEpisode(state, units)
		}
		if !manual && len(units) == 0 && planErr == nil && state.Suggestions.Attempt.Status == "idle" && state.Suggestions.Attempt.Fingerprint == fp {
			return nil
		}
		attemptKey := key
		if len(units) > 0 {
			attemptKey = units[0].MessageKey
		}
		attempt := SuggestionAttempt{Fingerprint: fp, MessageKey: attemptKey, At: time.Now().UTC(), Status: "unavailable"}
		if len(units) > 0 {
			episode, episodeErr := buildEpisode(state, units)
			if episodeErr != nil {
				required := episode.Included
				episode.Messages = nil
				episode.Included = 0
				episode.Omitted = episode.RetainedThreadMessages
				episode.Partial = true
				episode.Detail = fmt.Sprintf("Refused required set of %d whole messages; no context submitted: %s", required, episodeErr)
			}
			attempt.Context = &episode
		}
		proposals := []Suggestion{}
		var result decision.Result
		expanded := false
		if planErr == nil && len(units) > 0 {
			request, choices := buildDecision(state, units, false, time.Now())
			result, planErr = decision.Decide(budget, cfg, request)
			expand := false
			if planErr == nil {
				for _, selection := range result.Selections {
					for _, id := range selection.ChoiceIDs {
						if choices[id].Kind == "new" {
							expand = true
						}
					}
				}
				if expand {
					expanded = true
					request, choices = buildDecision(state, units, true, time.Now())
					result, planErr = decision.Decide(budget, cfg, request)
				}
			}
			if planErr == nil {
				for _, selection := range result.Selections {
					var unit EvidenceUnit
					for _, u := range units {
						if u.ID == selection.UnitID {
							unit = u
						}
					}
					p := makeSuggestion(fp, unit, selection.ChoiceIDs, choices)
					proposals = append(proposals, p)
				}
			}
		}
		if planErr != nil {
			attempt.Detail = planErr.Error()
		} else if len(units) == 0 {
			attempt.Status = "idle"
			attempt.Detail = "No unsuppressed actionable units"
		} else {
			attempt.Status = "ready"
			attempt.Detail = "Provisional; AMQ quality not yet measured; server reports cloud disabled"
			if attempt.Context != nil {
				attempt.Detail += fmt.Sprintf("; exact context %d included, %d same-thread omitted; %s", attempt.Context.Included, attempt.Context.Omitted, attempt.Context.Detail)
			}
			attempt.Runtime = &result
		}
		if currentConfig != nil && currentConfig() != cfg {
			return errors.New("suggestion provider configuration changed; result discarded")
		}
		err = s.publishSuggestions(ctx, state, cfg, units, expanded, proposals, attempt)
		if err != nil {
			return err
		}
		return planErr
	})
}

func (s Snapshot) SuggestionViews() []Suggestion {
	result := append([]Suggestion{}, s.Suggestions.Proposals...)
	fp := s.SuggestionFingerprint(s.Suggestions.Config)
	for i := range result {
		p := &result[i]
		p.Stale = fp != s.Suggestions.Fingerprint
		p.Candidates = s.Suggestions.Candidates
		p.Ignored = s.SuggestionIgnored[p.Unit.ID]
		p.Adopted = s.SuggestionAdopted[p.ID] != ""
		p.Commands = []Action{}
		if p.Ignored || p.Adopted || fp != s.Suggestions.Fingerprint || validateProposal(s, *p) != nil {
			continue
		}
		mutates := false
		newTask := false
		for _, c := range p.Choices {
			mutates = mutates || c.Kind == "attach" || c.Kind == "new"
			newTask = newTask || c.Kind == "new"
		}
		if mutates {
			args := []string{"amq-board", "adopt-suggestion", "--suggestion", p.ID, "--generation", fmt.Sprint(s.Generation)}
			for _, id := range p.Selected {
				args = append(args, "--choice", id)
			}
			if newTask {
				args = append(args, "--confirm-independent")
			}
			label := "Adopt selected attachment interpretations"
			if newTask {
				label = "Adopt as new task; confirm independent outcome"
			}
			p.Commands = append(p.Commands, newOwnerAction(label, args...))
		}
		p.Commands = append(p.Commands, newOwnerAction("Ignore suggestion; keep source evidence", "amq-board", "ignore-suggestion", "--suggestion", p.ID, "--generation", fmt.Sprint(s.Generation)))
	}
	return result
}
func (s Store) IgnoreSuggestion(ctx context.Context, id string, generation uint64) error {
	return s.update(ctx, func(state *Snapshot) error {
		if err := requireGeneration(state, generation); err != nil {
			return err
		}
		for _, p := range state.Suggestions.Proposals {
			if p.ID == id {
				if !unitValid(*state, p.Unit) {
					return errors.New("suggestion source unit changed")
				}
				if state.SuggestionIgnored == nil {
					state.SuggestionIgnored = map[string]bool{}
				}
				state.SuggestionIgnored[p.Unit.ID] = true
				return nil
			}
		}
		return errors.New("suggestion not found")
	})
}
func (s Store) AdoptSuggestion(ctx context.Context, a SuggestionAdoption) ([]string, error) {
	ids := []string{}
	err := s.update(ctx, func(state *Snapshot) error {
		if err := requireGeneration(state, a.ExpectedGeneration); err != nil {
			return err
		}
		if previous := state.SuggestionAdopted[a.ID]; previous != "" {
			for _, copied := range strings.Split(previous, ",") {
				canonical, err := state.ResolveTaskID(copied)
				if err != nil {
					return err
				}
				ids = appendUnique(ids, canonical)
			}
			return nil
		}
		if state.SuggestionFingerprint(state.Suggestions.Config) != state.Suggestions.Fingerprint {
			return errors.New("suggestion input changed; run suggest again")
		}
		var p *Suggestion
		for i := range state.Suggestions.Proposals {
			if state.Suggestions.Proposals[i].ID == a.ID {
				p = &state.Suggestions.Proposals[i]
			}
		}
		if p == nil || state.SuggestionIgnored[p.Unit.ID] || !unitValid(*state, p.Unit) || state.Dismissed[p.Unit.MessageKey] || state.ActionCancelled[p.Unit.MessageKey] {
			return errors.New("suggestion absent, suppressed or source evidence changed")
		}
		if err := validateProposal(*state, *p); err != nil {
			return err
		}
		want := append([]string{}, p.Selected...)
		got := append([]string{}, a.Choices...)
		sort.Strings(want)
		sort.Strings(got)
		if stringJSON(want) != stringJSON(got) {
			return errors.New("adoption must name exactly the published finite selected IDs")
		}
		for _, c := range p.Choices {
			switch c.Kind {
			case "attach":
				id, err := state.ResolveTaskID(c.TaskID)
				if err != nil || id != c.TaskID {
					return errors.New("suggestion target is no longer canonical")
				}
				task := state.Tasks[id]
				task.ModelAttachments = appendModelAttachment(task.ModelAttachments, ModelAttachment{Unit: p.Unit, Role: c.Role, SuggestionID: p.ID})
				state.Tasks[id] = task
				ids = appendUnique(ids, id)
			case "new":
				if !a.ConfirmIndependent || c.Outcome == nil || c.CurrentAsk == nil || !unitValid(*state, *c.Outcome) || !unitValid(*state, *c.CurrentAsk) || !newEligible(*state, *c.CurrentAsk, time.Now()) {
					return errors.New("NEW requires exact eligible outcome/current ask and owner confirmation of independence")
				}
				anchor := "inferred:" + c.Outcome.ID
				id := state.Anchors[anchor]
				if id != "" {
					var err error
					id, err = state.ResolveTaskID(id)
					if err != nil {
						return err
					}
				} else {
					var err error
					id, err = randomID("task-")
					if err != nil {
						return err
					}
					title := strings.TrimSpace(c.Outcome.Quote)
					if a.Title != "" {
						title = strings.TrimSpace(a.Title)
					}
					if title == "" || hasControl(title) || len(title) > 4096 {
						return errors.New("invalid outcome title")
					}
					state.Anchors[anchor] = id
					state.Tasks[id] = Task{ID: id, Title: title, Authority: "operator", Inferred: true, AdmissionAnchors: []string{anchor}, OwnerFact: Item{Source: "operator", Title: title, State: StateRequested, Structural: true, Owner: Owner{Kind: OwnerNone}, Reason: Reason{Field: "owner_adopted_inference", Text: "Owner-adopted inferred/requested outcome; execution not observed"}}}
				}
				task := state.Tasks[id]
				task.ModelAttachments = appendModelAttachment(task.ModelAttachments, ModelAttachment{Unit: p.Unit, Role: "request", SuggestionID: p.ID})
				state.Tasks[id] = task
				ids = appendUnique(ids, id)
			default:
				return errors.New("NON_ACTIONABLE can be ignored; UNRESOLVED has no adopt mutation")
			}
		}
		if state.SuggestionAdopted == nil {
			state.SuggestionAdopted = map[string]string{}
		}
		state.SuggestionAdopted[p.ID] = strings.Join(ids, ",")
		return nil
	})
	return ids, err
}
func appendModelAttachment(items []ModelAttachment, value ModelAttachment) []ModelAttachment {
	for _, old := range items {
		if old.Unit.ID == value.Unit.ID && old.Role == value.Role {
			return items
		}
	}
	return append(items, value)
}

func (s Snapshot) validateModelBindings() error {
	for _, task := range s.Tasks {
		for _, b := range task.ModelAttachments {
			if !unitValid(s, b.Unit) || !validAttachmentRole(b.Role) {
				return errors.New("model attachment evidence invalid; recovery required")
			}
		}
	}
	return nil
}
func validAttachmentRole(role string) bool {
	switch role {
	case "request", "review", "blocker", "update", "evidence":
		return true
	}
	return false
}

func referencesMessage(m Message, id string) bool {
	if id == "" {
		return false
	}
	for _, ref := range m.Refs {
		if ref == id {
			return true
		}
	}
	return false
}
func candidateRecords(tasks []TaskCatalogueEntry) []SuggestionCandidate {
	out := []SuggestionCandidate{}
	for _, t := range tasks {
		out = append(out, SuggestionCandidate{t.ID, t.Title, t.State, t.Cancelled, t.Historical, t.Aliases})
	}
	return out
}
func candidateContext(tasks []TaskCatalogueEntry) string { return stringJSON(candidateRecords(tasks)) }

func sourceContext(s Snapshot) string {
	type status struct {
		Name     string
		Status   SourceStatus
		Coverage Coverage
	}
	out := []status{}
	for _, nr := range s.Input.Results {
		out = append(out, status{nr.Name, nr.Result.Status, nr.Result.Coverage})
	}
	return stringJSON(out)
}

type EpisodeContext struct {
	Messages               []Message `json:"messages"`
	RetainedThreadMessages int       `json:"retained_thread_messages"`
	Included               int       `json:"included"`
	Omitted                int       `json:"omitted"`
	Partial                bool      `json:"partial"`
	MissingRefs            []string  `json:"missing_refs"`
	Detail                 string    `json:"detail"`
}

func buildEpisode(s Snapshot, units []EvidenceUnit) (EpisodeContext, error) {
	episode := EpisodeContext{Messages: []Message{}, MissingRefs: []string{}, Detail: "Exact-reference context; not proof of full causal context"}
	required := map[string]bool{}
	threads := map[string]bool{}
	sources := map[string]Message{}
	for _, u := range units {
		m, ok := s.Messages[u.MessageKey]
		if !ok {
			return episode, errors.New("source message missing from episode")
		}
		required[m.Key] = true
		sources[m.Key] = m
		if m.Thread != "" {
			threads[m.Namespace+"#"+m.Thread] = true
		}
	}
	for _, source := range sources {
		for _, ref := range source.Refs {
			matches := []string{}
			for key, m := range s.Messages {
				if m.Namespace == source.Namespace && m.ID == ref {
					matches = append(matches, key)
				}
			}
			if len(matches) != 1 {
				episode.MissingRefs = append(episode.MissingRefs, ref)
				episode.Partial = true
			} else {
				required[matches[0]] = true
			}
		}
		for key, m := range s.Messages {
			if m.Namespace == source.Namespace && referencesMessage(m, source.ID) {
				required[key] = true
			}
		}
	}
	for key, m := range s.Messages {
		if threads[m.Namespace+"#"+m.Thread] {
			episode.RetainedThreadMessages++
			if !required[key] {
				episode.Omitted++
			}
		}
		if required[key] {
			episode.Messages = append(episode.Messages, m)
		}
	}
	sort.Slice(episode.Messages, func(i, j int) bool { return episode.Messages[i].Key < episode.Messages[j].Key })
	sort.Strings(episode.MissingRefs)
	episode.Included = len(episode.Messages)
	if episode.Omitted > 0 {
		episode.Partial = true
		episode.Detail += "; thread context is partial: same-thread messages omitted"
	}
	if len(episode.MissingRefs) > 0 {
		episode.Detail += "; exact message refs missing or ambiguous"
	}
	if episode.Included > 8 {
		return episode, errors.New("exact-reference episode exceeds eight whole messages; no context was truncated")
	}
	if len([]byte(stringJSON(episode))) > 16<<10 {
		return episode, errors.New("exact-reference episode exceeds 16 KiB; no context was truncated")
	}
	return episode, nil
}
func episodeContext(s Snapshot, units []EvidenceUnit) string {
	episode, err := buildEpisode(s, units)
	if err != nil {
		return stringJSON(struct {
			Error   string
			Episode EpisodeContext
		}{err.Error(), episode})
	}
	return stringJSON(episode)
}

func validateProposal(s Snapshot, p Suggestion) error {
	if _, err := buildEpisode(s, s.Suggestions.Units); err != nil {
		return err
	}
	if len(s.Suggestions.Units) == 0 || len(s.Suggestions.Units) > 8 || !unitValid(s, p.Unit) {
		return errors.New("invalid source units")
	}
	for _, u := range s.Suggestions.Units {
		if !unitValid(s, u) {
			return errors.New("source unit changed")
		}
	}
	request, choices := buildDecision(s, s.Suggestions.Units, s.Suggestions.Expanded, time.Now())
	allowed := map[string]bool{}
	for _, u := range request.Evidence {
		if u.ID == p.Unit.ID {
			for _, id := range u.Allowed {
				allowed[id] = true
			}
		}
	}
	if len(p.Choices) != len(p.Selected) || len(p.Selected) == 0 || len(p.Selected) > 8 {
		return errors.New("invalid finite selections")
	}
	seen := map[string]bool{}
	for i, c := range p.Choices {
		if seen[c.ID] || c.ID != p.Selected[i] || !allowed[c.ID] || digestValue(c) != digestValue(choices[c.ID]) || (len(p.Choices) > 1 && c.Kind != "attach") || (c.Kind == "new" && !s.Suggestions.Expanded) {
			return errors.New("selected interpretation is not valid for this exact source unit")
		}
		seen[c.ID] = true
	}
	want := "suggestion-" + digestValue(struct {
		Fingerprint, Unit string
		Selected          []string
	}{s.Suggestions.Fingerprint, p.Unit.ID, p.Selected})[:32]
	if p.ID != want {
		return errors.New("suggestion identity invalid")
	}
	return nil
}

func makeSuggestion(fp string, unit EvidenceUnit, selected []string, choices map[string]SuggestionChoice) Suggestion {
	p := Suggestion{Unit: unit, Selected: selected, Choices: []SuggestionChoice{}}
	for _, id := range selected {
		p.Choices = append(p.Choices, choices[id])
	}
	p.ID = "suggestion-" + digestValue(struct {
		Fingerprint, Unit string
		Selected          []string
	}{fp, unit.ID, p.Selected})[:32]
	return p
}
func (s Store) publishSuggestions(ctx context.Context, expected Snapshot, cfg decision.Config, units []EvidenceUnit, expanded bool, proposals []Suggestion, attempt SuggestionAttempt) error {
	s.ProjectID = expected.ProjectID
	fp := expected.SuggestionFingerprint(cfg)
	return s.update(ctx, func(current *Snapshot) error {
		if current.Generation != expected.Generation || current.SuggestionFingerprint(cfg) != fp || current.Suggestions.Config != cfg {
			return errors.New("suggestion input changed; result discarded")
		}
		limit := 8
		if expanded {
			limit = 24
		}
		if attempt.MessageKey == "" && len(units) > 0 {
			attempt.MessageKey = units[0].MessageKey
		}
		if _, ok := current.Messages[attempt.MessageKey]; ok {
			if current.Observations == nil {
				current.Observations = map[string]MessageObservation{}
			}
			observation := current.Observations[attempt.MessageKey]
			observation.AttemptFingerprint = fp
			current.Observations[attempt.MessageKey] = observation
		}
		current.Suggestions = SuggestionState{UserConfigured: expected.Suggestions.UserConfigured, Candidates: candidateRecords(candidates(expected, units, limit)), Units: units, Expanded: expanded, Config: cfg, Fingerprint: fp, Proposals: proposals, Attempt: attempt}
		for _, p := range proposals {
			if err := validateProposal(*current, p); err != nil {
				return err
			}
		}
		return nil
	})
}

func currentObservation(s Snapshot, m Message) bool {
	o, ok := s.Observations[m.Key]
	created, err := time.Parse(time.RFC3339Nano, m.Created)
	return ok && !o.Bootstrap && !s.HistoryBaseline.IsZero() && o.FirstSeen.After(s.HistoryBaseline) && err == nil && created.After(s.HistoryBaseline) && !created.After(o.FirstSeen)
}
