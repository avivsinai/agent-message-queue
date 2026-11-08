package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/route"
	"github.com/avivsinai/agent-message-queue/internal/thread"
)

const (
	threadOpenSchema          = "amq/thread-open/v1"
	threadOpenUnsettledParent = thread.UnsettledParent
)

// The reduction's records appear in thread open output as they are.
type (
	participant            = thread.Participant
	threadOpenSkipped      = thread.Skipped
	threadOpenUnresolved   = thread.Unresolved
	threadOpenAmbiguous    = thread.Ambiguous
	threadOpenRoutedOpener = thread.RoutedOpener
)

type threadOpenResult struct {
	Schema      string                 `json:"schema"`
	GeneratedAt string                 `json:"generated_at"`
	BaseRoot    string                 `json:"base_root"`
	Sessions    []string               `json:"sessions"`
	Obligations []threadOpenObligation `json:"obligations"`
	Coverage    threadOpenCoverage     `json:"coverage"`
}

type threadOpenObligation struct {
	Key                   string                 `json:"key"`
	Thread                string                 `json:"thread"`
	Opener                threadOpenOpener       `json:"opener"`
	OwedBy                participant            `json:"owed_by"`
	OwedByKind            string                 `json:"owed_by_kind"`
	OwedTo                participant            `json:"owed_to"`
	ReplyDestination      *threadOpenDestination `json:"reply_destination"`
	ReplyDestinationError string                 `json:"reply_destination_error,omitempty"`
	State                 string                 `json:"state"`
	Completion            threadOpenRecipe       `json:"completion"`
	Acknowledge           threadOpenRecipe       `json:"acknowledge"`
	LastActivity          *threadOpenActivity    `json:"last_activity,omitempty"`
	AgeSeconds            int64                  `json:"age_seconds"`
}

type threadOpenOpener struct {
	ID           string   `json:"id"`
	Kind         string   `json:"kind"`
	From         string   `json:"from"`
	To           []string `json:"to"`
	ReplyTo      string   `json:"reply_to"`
	ReplyProject string   `json:"reply_project"`
	FromProject  string   `json:"from_project"`
	Subject      string   `json:"subject"`
	Created      string   `json:"created"`
	Priority     string   `json:"priority"`
	Labels       []string `json:"labels"`
}

type threadOpenDestination struct {
	Handle  string `json:"handle"`
	Session string `json:"session"`
	Project string `json:"project"`
	Source  string `json:"source"`
}

type threadOpenRecipe struct {
	Kind   string   `json:"kind"`
	Labels []string `json:"labels"`
}

type threadOpenActivity struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	From    string `json:"from"`
	Created string `json:"created"`
}

type threadOpenCoverage struct {
	MaxAgeSeconds     int64                     `json:"max_age_seconds"`
	AssessedSince     string                    `json:"assessed_since,omitempty"`
	PrunedFiles       []threadOpenCount         `json:"pruned_files"`
	Expired           []threadOpenExpired       `json:"expired"`
	Skipped           []threadOpenSkipped       `json:"skipped"`
	UnresolvedLineage []threadOpenUnresolved    `json:"unresolved_lineage"`
	Ambiguous         []threadOpenAmbiguous     `json:"ambiguous"`
	OutboundRouted    []threadOpenCount         `json:"outbound_routed"`
	OutboundOpeners   []threadOpenRoutedOpener  `json:"outbound_routed_openers"`
	ActionOutcomes    []threadOpenActionOutcome `json:"action_outcomes"`
	SkewDeferred      []threadOpenCount         `json:"skew_deferred"`
	TruncatedByLimit  threadOpenTruncated       `json:"truncated_by_limit"`
}

type threadOpenCount struct {
	Session string `json:"session"`
	Count   int    `json:"count"`
}

type threadOpenActionOutcome struct {
	OpenerID string      `json:"opener_id"`
	Key      string      `json:"key"`
	OwedBy   participant `json:"owed_by"`
	State    string      `json:"state"`
}

type threadOpenExpired struct {
	Session string `json:"session"`
	OwedBy  string `json:"owed_by"`
	Count   int    `json:"count"`
}

type threadOpenTruncated struct {
	Limit   int `json:"limit"`
	Omitted int `json:"omitted"`
}

type threadOpenFilter struct {
	owedBy, owedTo, kinds []string
}

func (f threadOpenFilter) match(ob *thread.Obligation) bool {
	return (len(f.kinds) == 0 || slices.Contains(f.kinds, ob.Opener.Kind)) &&
		(len(f.owedBy) == 0 || slices.Contains(f.owedBy, ob.Owner.Handle)) &&
		(len(f.owedTo) == 0 || slices.Contains(f.owedTo, ob.Requester.Handle))
}

func runThreadOpen(args []string) error {
	fs := flag.NewFlagSet("thread open", flag.ContinueOnError)
	common := addCommonFlags(fs)
	allSessions := fs.Bool("all-sessions", false, "Scan every canonical session under the base root")
	includeBase := fs.Bool("include-base", false, "Include base-root mailboxes with --all-sessions")
	owedByFlag := fs.String("owed-by", "", "Comma-separated handles that owe a reply")
	owedToFlag := fs.String("owed-to", "", "Comma-separated handles that are owed a reply")
	kindFlag := fs.String("kind", "", "Comma-separated opener kinds: review_request, question, todo")
	maxAgeFlag := fs.String("max-age", "14d", "Ignore openers older than this (e.g. 14d, 36h; 0 = read every file)")
	limitFlag := fs.Int("limit", 0, "List at most N obligations, oldest first, after the whole window is reduced (0 = no limit)")
	schemaFlag := fs.Int("json-schema", 1, "JSON schema version: 1 (requires --json)")

	usage := usageWithFlags(fs, "amq thread open [--all-sessions] [--include-base] [--owed-by <handles>] [--owed-to <handles>] [--kind <kinds>] [--max-age <dur>] [options]",
		"List open review requests, questions, and todos per thread.",
		"An obligation closes when its owner replies with the expected kind or a closing label.",
		"Read-only: it never moves a message, writes a receipt, or reads a body.")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if flagWasVisited(fs, "json-schema") && !common.JSON {
		return UsageError("--json-schema requires --json")
	}
	if *schemaFlag != 1 {
		return UsageError("--json-schema must be 1")
	}
	if *limitFlag < 0 {
		return UsageError("--limit must be >= 0")
	}
	if *includeBase && !*allSessions {
		return UsageError("--include-base requires --all-sessions")
	}
	maxAge, err := parseMaxAge(*maxAgeFlag)
	if err != nil {
		return UsageError("--max-age: %v", err)
	}
	var filter threadOpenFilter
	if filter.owedBy, err = parseHandles(*owedByFlag); err != nil {
		return UsageError("--owed-by: %v", err)
	}
	if filter.owedTo, err = parseHandles(*owedToFlag); err != nil {
		return UsageError("--owed-to: %v", err)
	}
	for _, kind := range splitList(*kindFlag) {
		if !slices.Contains(thread.OpenerKinds, kind) {
			return UsageError("--kind must be one of: %s", strings.Join(thread.OpenerKinds, ", "))
		}
		filter.kinds = append(filter.kinds, kind)
	}

	root := absPath(resolveRoot(common.Root))
	if !dirExists(root) {
		return NotFoundError("root not found at %s", root)
	}
	// Inspection path: a session pin mismatch is reported and the scan goes on.
	if mismatch, err := sessionPinMismatch(root); err != nil {
		_ = writeStderr("warning: %v\n", err)
	} else if mismatch != nil && !isPinnedBaseRoot(root) {
		_ = writeStderr("warning: %s\n", mismatch.Error())
	}

	baseRoot := absPath(baseRootOf(root))
	scanRoots := []thread.ScanRoot{threadOpenScanRoot(root)}
	if *allSessions {
		if baseRoot, err = sessionBaseRoot(root); err != nil {
			return err
		}
		if scanRoots, err = canonicalSessionRoots(baseRoot); err != nil {
			return err
		}
		if *includeBase {
			scanRoots = append([]thread.ScanRoot{{Root: baseRoot}}, scanRoots...)
		}
	}

	now := time.Now().UTC()
	var cutoff time.Time
	if maxAge > 0 {
		cutoff = now.Add(-maxAge)
	}
	result := threadOpenResult{
		Schema:      threadOpenSchema,
		GeneratedAt: now.Format(time.RFC3339),
		BaseRoot:    baseRoot,
		Sessions:    []string{},
		Obligations: []threadOpenObligation{},
		Coverage: threadOpenCoverage{
			MaxAgeSeconds: int64(maxAge / time.Second),
			PrunedFiles:   []threadOpenCount{},
			Skipped:       []threadOpenSkipped{},
		},
	}
	if !cutoff.IsZero() {
		result.Coverage.AssessedSince = cutoff.Format(time.RFC3339)
	}
	for _, scanRoot := range scanRoots {
		result.Sessions = append(result.Sessions, scanRoot.Session)
	}

	scan, red, err := reduceObligations(scanRoots, cutoff, func(path string, parseErr error) error {
		result.Coverage.Skipped = append(result.Coverage.Skipped, threadOpenSkipped{Path: path, Error: parseErr.Error()})
		return nil
	})
	if err != nil {
		return err
	}
	result.Coverage.PrunedFiles = countsBySession(result.Sessions, scan.Pruned)
	result.Coverage.Skipped = append(result.Coverage.Skipped, red.Skipped...)
	result.Coverage.UnresolvedLineage = red.Unresolved
	result.Coverage.Ambiguous = red.Ambiguous
	result.Coverage.OutboundRouted = countsBySession(result.Sessions, red.Outbound)
	result.Coverage.OutboundOpeners = red.OutboundOpeners
	result.Coverage.ActionOutcomes = make([]threadOpenActionOutcome, 0, len(red.Obligations))
	for _, ob := range red.Obligations {
		state := "open"
		if ob.Closed {
			state = "closed"
		}
		result.Coverage.ActionOutcomes = append(result.Coverage.ActionOutcomes, threadOpenActionOutcome{
			OpenerID: ob.Opener.ID,
			Key:      ob.Key,
			OwedBy:   ob.Owner,
			State:    state,
		})
	}
	result.Coverage.SkewDeferred = countsBySession(result.Sessions, red.Deferred)

	open, expired := report(red, filter)
	result.Coverage.Expired = expired
	result.Coverage.TruncatedByLimit.Limit = *limitFlag
	if *limitFlag > 0 && len(open) > *limitFlag {
		result.Coverage.TruncatedByLimit.Omitted = len(open) - *limitFlag
		open = open[:*limitFlag]
	}
	for _, ob := range open {
		result.Obligations = append(result.Obligations, obligationJSON(ob, now))
	}

	if common.JSON {
		return writeJSON(os.Stdout, result)
	}
	return writeThreadOpenText(result)
}

// threadOpenScanRoot is the root thread open scans without --all-sessions.
func threadOpenScanRoot(root string) thread.ScanRoot {
	session := ""
	if root != absPath(baseRootOf(root)) {
		session = sessionName(root)
	}
	return thread.ScanRoot{Session: session, Root: root}
}

// reduceObligations reads the message headers under scanRoots, skipping
// files stamped before a nonzero cutoff, and reduces them to obligations.
func reduceObligations(scanRoots []thread.ScanRoot, cutoff time.Time, onError func(string, error) error) (thread.ScanResult, *thread.Reduction, error) {
	scan, err := thread.CollectAll(scanRoots, cutoff, onError)
	if err != nil {
		return thread.ScanResult{}, nil, err
	}
	return scan, thread.ReduceObligations(scanRoots, scan.Entries, cutoff, nil), nil
}

// openObligationIDs are the ids of the openers that `thread open --max-age 0`
// lists as open under root, and of every message whose refs name one: a
// reply that acknowledges an opener decides its state. A message whose header
// does not parse is included too: its obligation is unknown and it belongs to
// dead-letter handling. closers maps the id of each message on the path that
// closed an obligation under root (the closing message and its direct
// parents up to the opener) to the ids of those openers.
func openObligationIDs(root string) (ids map[string]bool, closers map[string][]string, err error) {
	ids, closers = map[string]bool{}, map[string][]string{}
	scan, red, err := reduceObligations([]thread.ScanRoot{threadOpenScanRoot(root)}, time.Time{}, func(path string, _ error) error {
		ids[strings.TrimSuffix(filepath.Base(path), ".md")] = true
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	parent := map[string]string{}
	for _, entry := range scan.Entries {
		if n := len(entry.Refs); n > 0 {
			parent[entry.ID] = entry.Refs[n-1]
		}
	}
	for _, ob := range red.Obligations {
		if ob.Open() {
			ids[ob.Opener.ID] = true
			continue
		}
		if !ob.Closed || ob.ClosedBy == nil {
			continue
		}
		seen := map[string]bool{}
		for id := ob.ClosedBy.ID; id != "" && id != ob.Opener.ID && !seen[id]; id = parent[id] {
			seen[id] = true
			closers[id] = append(closers[id], ob.Opener.ID)
		}
	}
	var replies []string
	for _, entry := range scan.Entries {
		for _, ref := range entry.Refs {
			if ids[ref] {
				replies = append(replies, entry.ID)
				break
			}
		}
	}
	for _, id := range replies {
		ids[id] = true
	}
	return ids, closers, nil
}

// report returns the obligations filter keeps that are still open, oldest
// opener first, and the expired ones counted per owner.
func report(red *thread.Reduction, filter threadOpenFilter) ([]*thread.Obligation, []threadOpenExpired) {
	open := []*thread.Obligation{}
	expiredCount := map[[2]string]int{}
	for _, ob := range red.Obligations {
		if !filter.match(ob) {
			continue
		}
		if ob.Expired {
			expiredCount[[2]string{ob.Owner.Session, ob.Owner.Handle}]++
			continue
		}
		if ob.Open() {
			open = append(open, ob)
		}
	}
	sort.SliceStable(open, func(i, j int) bool {
		a, b := open[i].Opener.RawTime, open[j].Opener.RawTime
		if !a.Equal(b) {
			return a.Before(b)
		}
		return open[i].Key < open[j].Key
	})
	expired := []threadOpenExpired{}
	for k, count := range expiredCount {
		expired = append(expired, threadOpenExpired{Session: k[0], OwedBy: k[1], Count: count})
	}
	sort.Slice(expired, func(i, j int) bool {
		if expired[i].Session != expired[j].Session {
			return expired[i].Session < expired[j].Session
		}
		return expired[i].OwedBy < expired[j].OwedBy
	})
	return open, expired
}

// completionRecipe is the owner message that closes an opener of the kind.
func completionRecipe(openerKind string) threadOpenRecipe {
	switch openerKind {
	case format.KindReviewRequest:
		return threadOpenRecipe{Kind: format.KindReviewResponse, Labels: []string{}}
	case format.KindQuestion:
		return threadOpenRecipe{Kind: format.KindAnswer, Labels: []string{}}
	}
	return threadOpenRecipe{Kind: format.KindStatus, Labels: []string{"done"}}
}

// replyDestination derives the route back to the requester by the rules amq
// reply uses: reply_project, then reply_to, then the sender in the owner's
// session.
func replyDestination(ob *thread.Obligation) (*threadOpenDestination, error) {
	opener := ob.Opener
	if opener.ReplyProject != "" {
		if strings.TrimSpace(opener.ReplyProject) != opener.ReplyProject {
			return nil, fmt.Errorf("invalid reply_project %q", opener.ReplyProject)
		}
		handle, session, err := route.ParseReplyTo(opener.ReplyTo, true)
		if err != nil {
			return nil, err
		}
		return &threadOpenDestination{Handle: handle, Session: session, Project: opener.ReplyProject, Source: "reply_project"}, nil
	}
	if opener.ReplyTo != "" {
		handle, session, err := route.ParseReplyTo(opener.ReplyTo, false)
		if err != nil {
			return nil, err
		}
		return &threadOpenDestination{Handle: handle, Session: session, Source: "reply_to"}, nil
	}
	if handle, err := normalizeHandle(opener.From); err != nil || handle != opener.From {
		return nil, fmt.Errorf("invalid sender handle %q", opener.From)
	}
	return &threadOpenDestination{Handle: opener.From, Session: ob.Owner.Session, Source: "local"}, nil
}

func obligationJSON(ob *thread.Obligation, now time.Time) threadOpenObligation {
	opener := ob.Opener
	owedByKind := "agent"
	if ob.Owner.Handle == reservedHumanHandle {
		owedByKind = "human"
	}
	out := threadOpenObligation{
		Key:    ob.Key,
		Thread: opener.Thread,
		Opener: threadOpenOpener{
			ID:           opener.ID,
			Kind:         opener.Kind,
			From:         opener.From,
			To:           nonNil(opener.To),
			ReplyTo:      opener.ReplyTo,
			ReplyProject: opener.ReplyProject,
			FromProject:  opener.FromProject,
			Subject:      opener.Subject,
			Created:      opener.Created,
			Priority:     opener.Priority,
			Labels:       nonNil(opener.Labels),
		},
		OwedBy:      ob.Owner,
		OwedByKind:  owedByKind,
		OwedTo:      ob.Requester,
		State:       ob.State,
		Completion:  completionRecipe(opener.Kind),
		Acknowledge: threadOpenRecipe{Kind: format.KindStatus, Labels: []string{}},
		AgeSeconds:  int64(now.Sub(opener.RawTime) / time.Second),
	}
	if dest, err := replyDestination(ob); err != nil {
		out.ReplyDestinationError = err.Error()
	} else {
		out.ReplyDestination = dest
	}
	if ob.Last != nil {
		out.LastActivity = &threadOpenActivity{ID: ob.Last.ID, Kind: ob.Last.Kind, From: ob.Last.From, Created: ob.Last.Created}
	}
	return out
}

func writeThreadOpenText(result threadOpenResult) error {
	if len(result.Obligations) == 0 {
		if err := writeStdoutLine("No open obligations."); err != nil {
			return err
		}
	}
	for _, ob := range result.Obligations {
		subject := ob.Opener.Subject
		if subject == "" {
			subject = "(no subject)"
		}
		age := (time.Duration(ob.AgeSeconds) * time.Second).Round(time.Minute)
		if err := writeStdout("%s  %s owes %s  %s  %s  %s\n", age, participantLabel(ob.OwedBy), participantLabel(ob.OwedTo), ob.Opener.Kind, ob.State, subject); err != nil {
			return err
		}
	}
	cov := result.Coverage
	for _, exp := range cov.Expired {
		if err := writeStdout("expired: %s owes %d older than the cutoff\n", participantLabel(participant{Session: exp.Session, Handle: exp.OwedBy}), exp.Count); err != nil {
			return err
		}
	}
	for _, pruned := range cov.PrunedFiles {
		if err := writeStdout("pruned: %d files in %s older than the cutoff were not read\n", pruned.Count, sessionLabel(pruned.Session)); err != nil {
			return err
		}
	}
	for _, routed := range cov.OutboundRouted {
		if err := writeStdout("routed: %d requests from %s went to a root outside the scan\n", routed.Count, sessionLabel(routed.Session)); err != nil {
			return err
		}
	}
	for _, un := range cov.UnresolvedLineage {
		if err := writeStdout("unresolved: %s from %s refers to missing %s\n", un.ID, un.From, un.MissingRef); err != nil {
			return err
		}
	}
	for _, amb := range cov.Ambiguous {
		if err := writeStdout("ambiguous: %s from %s (%s)\n", amb.ID, amb.From, amb.Reason); err != nil {
			return err
		}
	}
	if cov.TruncatedByLimit.Omitted > 0 {
		if err := writeStdout("limit: %d more not shown\n", cov.TruncatedByLimit.Omitted); err != nil {
			return err
		}
	}
	for _, skip := range cov.Skipped {
		if err := writeStderr("warning: skipping unreadable message %s: %s\n", skip.Path, skip.Error); err != nil {
			return err
		}
	}
	return nil
}

func participantLabel(p participant) string {
	label := p.Handle
	if p.Session != "" {
		label += "@" + p.Session
	}
	if p.Project != "" {
		label += " (" + p.Project + ")"
	}
	return label
}

func sessionLabel(session string) string {
	if session == "" {
		return "the base root"
	}
	return session
}

func countsBySession(sessions []string, counts map[string]int) []threadOpenCount {
	out := []threadOpenCount{}
	for _, session := range sessions {
		if counts[session] > 0 {
			out = append(out, threadOpenCount{Session: session, Count: counts[session]})
		}
	}
	return out
}

// canonicalSessionRoots lists the canonical sessions under base, sorted by
// name, as amq session list reports them.
func canonicalSessionRoots(base string) ([]thread.ScanRoot, error) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil, err
	}
	roots := []thread.ScanRoot{}
	for _, entry := range entries {
		item, _, skip := classifySessionChild(base, entry.Name())
		if skip || item.Kind != sessionKindCanonical {
			continue
		}
		roots = append(roots, thread.ScanRoot{Session: item.Name, Root: item.Path})
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].Session < roots[j].Session })
	return roots, nil
}

// parseMaxAge accepts a Go duration or a whole number of days ("14d"). Zero
// disables the cutoff.
func parseMaxAge(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "0" {
		return 0, nil
	}
	var d time.Duration
	if days, ok := strings.CutSuffix(raw, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", raw)
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		var err error
		if d, err = time.ParseDuration(raw); err != nil {
			return 0, err
		}
	}
	if d < 0 {
		return 0, fmt.Errorf("must be >= 0")
	}
	return d, nil
}

func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
