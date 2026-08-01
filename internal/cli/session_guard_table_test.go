package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionGuardRouteExplainSessionPinABIIsJSONExitZero(t *testing.T) {
	parent := t.TempDir()
	globalProject := filepath.Join(parent, "global")
	foreignProject := filepath.Join(parent, "foreign")
	globalBase := filepath.Join(globalProject, ".agent-mail")
	globalRoot := sessionRoot(t, globalProject, "session1", "alice", "bob")
	foreignRoot := sessionRoot(t, foreignProject, "session1", "alice", "bob")
	pinSendSessionForTest(t, globalBase, globalRoot, "session1")

	// runRouteExplainJSONForTest fails the test if runRouteExplain returns an
	// error. A mismatched source therefore proves the route probe's promised
	// JSON-error/exit-0 channel rather than an exit-5 process failure.
	result := runRouteExplainJSONForTest(t,
		"--from-root", foreignRoot,
		"--me", "alice",
		"--to", "bob",
	)
	if result.Routable {
		t.Fatalf("mismatched pinned source was reported routable: %#v", result)
	}
	if result.Error == "" || !strings.Contains(result.Error, "mismatched source") {
		t.Fatalf("route JSON error = %q, want pinned-source mismatch", result.Error)
	}
}

func TestSessionGuardRouteExplainInvalidPinABIIsJSONExitZero(t *testing.T) {
	parent := t.TempDir()
	globalProject := filepath.Join(parent, "global")
	foreignProject := filepath.Join(parent, "foreign")
	globalBase := filepath.Join(globalProject, ".agent-mail")
	globalRoot := sessionRoot(t, globalProject, "session1", "alice", "bob")
	foreignRoot := sessionRoot(t, foreignProject, "session1", "alice", "bob")
	t.Setenv(envRoot, globalRoot)
	t.Setenv(envBaseRoot, globalBase)
	t.Setenv(envSession, "session1")
	t.Setenv(envRootID, "malformed-root-token")
	t.Setenv(envBaseRootID, "malformed-base-token")

	// The route helper converts guard errors into its JSON result and returns
	// nil, so malformed pin evidence must remain a probe error with exit 0.
	result := runRouteExplainJSONForTest(t,
		"--from-root", foreignRoot,
		"--me", "alice",
		"--to", "bob",
	)
	if result.Routable {
		t.Fatalf("invalid pinned source was reported routable: %#v", result)
	}
	if result.Error == "" || !strings.Contains(result.Error, "identity pin") {
		t.Fatalf("route JSON error = %q, want invalid identity-pin diagnostic", result.Error)
	}
}

func TestSessionGuardCanonicalRows(t *testing.T) {
	want := []string{
		sessionGuardRowR01UnboundAllow,
		sessionGuardRowR02MatchAllow,
		sessionGuardRowR03MailboxRouted,
		sessionGuardRowR04IgnorePin,
		sessionGuardRowR05SourceFromSession,
		sessionGuardRowR06ListOwnBase,
		sessionGuardRowR07DoctorOwnBase,
		sessionGuardRowR08MismatchExit5,
		sessionGuardRowR09InvalidExit5,
		sessionGuardRowR10ListMismatchWarn,
		sessionGuardRowR11ListInvalidWarn,
		sessionGuardRowR12DoctorMismatchErr,
		sessionGuardRowR13DoctorInvalidErr,
		sessionGuardRowR14RouteMismatchJSON,
		sessionGuardRowR15RouteInvalidJSON,
	}
	if len(sessionGuardCanonicalRows) != len(want) {
		t.Fatalf("canonical row count = %d, want %d", len(sessionGuardCanonicalRows), len(want))
	}
	for i, row := range want {
		if got := sessionGuardCanonicalRows[i]; got != row {
			t.Errorf("canonical row %d = %q, want %q", i, got, row)
		}
	}
}

func TestSessionGuardDecisionRows(t *testing.T) {
	tests := []struct {
		name    string
		input   sessionGuardInput
		row     string
		verdict sessionGuardVerdict
	}{
		{
			name: "R01 unbound allows every policy",
			input: sessionGuardInput{
				Kind: sessionGuardEnv, Relation: sessionGuardTargetUnbound,
				Flags: sessionGuardFlags{ExplicitContext: true},
			},
			row: sessionGuardRowR01UnboundAllow, verdict: sessionGuardAllow,
		},
		{
			name: "R02 matching identity pin allows",
			input: sessionGuardInput{
				Kind: sessionGuardMailbox, Pin: sessionGuardPinIdentity, Relation: sessionGuardTargetMatch,
			},
			row: sessionGuardRowR02MatchAllow, verdict: sessionGuardAllow,
		},
		{
			name: "R03 mailbox routed after valid preflight",
			input: sessionGuardInput{
				Kind: sessionGuardMailbox, Pin: sessionGuardPinAbsent, Relation: sessionGuardTargetMismatch,
				Flags: sessionGuardFlags{Routed: true},
			},
			row: sessionGuardRowR03MailboxRouted, verdict: sessionGuardAllow,
		},
		{
			name: "R09 invalid pin is not hidden by routed preflight",
			input: sessionGuardInput{
				Kind: sessionGuardMailbox, Pin: sessionGuardPinInvalid, Relation: sessionGuardTargetMismatch,
				Flags: sessionGuardFlags{Routed: true},
			},
			row: sessionGuardRowR09InvalidExit5, verdict: sessionGuardRefuseExit5,
		},
		{
			name: "R04 ignore pin is an early policy bypass",
			input: sessionGuardInput{
				Kind: sessionGuardDoctorRepair, Pin: sessionGuardPinInvalid, Relation: sessionGuardTargetMismatch,
				Flags: sessionGuardFlags{ExplicitRoot: true, IgnorePin: true},
			},
			row: sessionGuardRowR04IgnorePin, verdict: sessionGuardAllow,
		},
		{
			name: "R05 send from-session bypass follows identity preflight",
			input: sessionGuardInput{
				Kind: sessionGuardSource, Pin: sessionGuardPinIdentity, Relation: sessionGuardTargetMismatch,
				Flags: sessionGuardFlags{FromSession: true},
			},
			row: sessionGuardRowR05SourceFromSession, verdict: sessionGuardAllow,
		},
		{
			name: "R09 invalid pin is not hidden by from-session preflight",
			input: sessionGuardInput{
				Kind: sessionGuardSource, Pin: sessionGuardPinInvalid, Relation: sessionGuardTargetMismatch,
				Flags: sessionGuardFlags{FromSession: true},
			},
			row: sessionGuardRowR09InvalidExit5, verdict: sessionGuardRefuseExit5,
		},
		{
			name: "R06 list own pinned base suppresses warning",
			input: sessionGuardInput{
				Kind: sessionGuardList, Pin: sessionGuardPinIdentity, Relation: sessionGuardTargetOwnPinnedBase,
				Flags: sessionGuardFlags{ExplicitRoot: true},
			},
			row: sessionGuardRowR06ListOwnBase, verdict: sessionGuardAllow,
		},
		{
			name: "R07 doctor own pinned base allows repair",
			input: sessionGuardInput{
				Kind: sessionGuardDoctorRepair, Pin: sessionGuardPinIdentity, Relation: sessionGuardTargetOwnPinnedBase,
			},
			row: sessionGuardRowR07DoctorOwnBase, verdict: sessionGuardAllow,
		},
		{
			name: "R08 valid mismatch refuses mailbox",
			input: sessionGuardInput{
				Kind: sessionGuardMailbox, Pin: sessionGuardPinLegacy, Relation: sessionGuardTargetMismatch,
			},
			row: sessionGuardRowR08MismatchExit5, verdict: sessionGuardRefuseExit5,
		},
		{
			name: "R08 cross-project still refuses source mismatch",
			input: sessionGuardInput{
				Kind: sessionGuardSource, Pin: sessionGuardPinIdentity, Relation: sessionGuardTargetMismatch,
				Flags: sessionGuardFlags{CrossProject: true},
			},
			row: sessionGuardRowR08MismatchExit5, verdict: sessionGuardRefuseExit5,
		},
		{
			name: "R08 explicit root does not waive pin mismatch",
			input: sessionGuardInput{
				Kind: sessionGuardMailbox, Pin: sessionGuardPinIdentity, Relation: sessionGuardTargetMismatch,
				Flags: sessionGuardFlags{ExplicitRoot: true},
			},
			row: sessionGuardRowR08MismatchExit5, verdict: sessionGuardRefuseExit5,
		},
		{
			name: "R08 source explicit root does not waive pin mismatch",
			input: sessionGuardInput{
				Kind: sessionGuardSource, Pin: sessionGuardPinIdentity, Relation: sessionGuardTargetMismatch,
				Flags: sessionGuardFlags{ExplicitRoot: true},
			},
			row: sessionGuardRowR08MismatchExit5, verdict: sessionGuardRefuseExit5,
		},
		{
			name: "R09 invalid pin refuses exit five",
			input: sessionGuardInput{
				Kind: sessionGuardEnv, Pin: sessionGuardPinInvalid, Relation: sessionGuardTargetMismatch,
			},
			row: sessionGuardRowR09InvalidExit5, verdict: sessionGuardRefuseExit5,
		},
		{
			name: "R09 invalid pin wins over unbound relation",
			input: sessionGuardInput{
				Kind: sessionGuardMailbox, Pin: sessionGuardPinInvalid, Relation: sessionGuardTargetUnbound,
			},
			row: sessionGuardRowR09InvalidExit5, verdict: sessionGuardRefuseExit5,
		},
		{
			name: "R10 list mismatch warns",
			input: sessionGuardInput{
				Kind: sessionGuardList, Pin: sessionGuardPinLegacy, Relation: sessionGuardTargetMismatch,
			},
			row: sessionGuardRowR10ListMismatchWarn, verdict: sessionGuardWarnContinue,
		},
		{
			name: "R11 list invalid warns",
			input: sessionGuardInput{
				Kind: sessionGuardList, Pin: sessionGuardPinInvalid, Relation: sessionGuardTargetMismatch,
			},
			row: sessionGuardRowR11ListInvalidWarn, verdict: sessionGuardWarnContinue,
		},
		{
			name: "R12 doctor mismatch is structured error continue",
			input: sessionGuardInput{
				Kind: sessionGuardDoctorRepair, Pin: sessionGuardPinIdentity, Relation: sessionGuardTargetMismatch,
			},
			row: sessionGuardRowR12DoctorMismatchErr, verdict: sessionGuardStructuredErrorContinueExit0,
		},
		{
			name: "R13 doctor invalid is structured error continue",
			input: sessionGuardInput{
				Kind: sessionGuardDoctorRepair, Pin: sessionGuardPinInvalid, Relation: sessionGuardTargetMismatch,
			},
			row: sessionGuardRowR13DoctorInvalidErr, verdict: sessionGuardStructuredErrorContinueExit0,
		},
		{
			name: "R14 route mismatch is json error exit zero",
			input: sessionGuardInput{
				Kind: sessionGuardSource, Channel: sessionGuardChannelJSON,
				Pin: sessionGuardPinLegacy, Relation: sessionGuardTargetMismatch,
			},
			row: sessionGuardRowR14RouteMismatchJSON, verdict: sessionGuardJSONErrorExit0,
		},
		{
			name: "R15 route invalid is json error exit zero",
			input: sessionGuardInput{
				Kind: sessionGuardSource, Channel: sessionGuardChannelJSON,
				Pin: sessionGuardPinInvalid, Relation: sessionGuardTargetMismatch,
			},
			row: sessionGuardRowR15RouteInvalidJSON, verdict: sessionGuardJSONErrorExit0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := decideSessionGuard(test.input)
			if got.Row != test.row {
				t.Fatalf("row = %q, want %q", got.Row, test.row)
			}
			if got.Verdict != test.verdict {
				t.Fatalf("verdict = %d, want %d", got.Verdict, test.verdict)
			}
		})
	}
}

func TestSessionGuardRevalidationUsesEntryRows(t *testing.T) {
	for _, kind := range []sessionGuardKind{sessionGuardMailbox} {
		for _, relation := range []sessionGuardTargetRelation{sessionGuardTargetMatch, sessionGuardTargetMismatch} {
			entry := decideSessionGuard(sessionGuardInput{Kind: kind, Pin: sessionGuardPinIdentity, Relation: relation})
			revalidated := decideSessionGuard(sessionGuardInput{
				Kind: kind, Pin: sessionGuardPinIdentity, Relation: relation,
				Flags: sessionGuardFlags{Revalidation: true},
			})
			if revalidated != entry {
				t.Errorf("revalidation relation %d decision %#v, want entry %#v", relation, revalidated, entry)
			}
		}
	}
}

type sessionGuardSiteMapping struct {
	site          string
	kind          sessionGuardKind
	channel       sessionGuardChannel
	rows          []string
	phase         string
	evaluatorOnly bool
}

// R1 has 19 decision-site mappings plus one evaluator-only diagnostic.
// Revalidation callbacks and both send branches are textual consumers of
// existing rows; doctor:437 intentionally maps to no decision row.
var sessionGuardSiteMappings = []sessionGuardSiteMapping{
	{site: "internal/cli/read.go:50", kind: sessionGuardMailbox, rows: []string{sessionGuardRowR01UnboundAllow, sessionGuardRowR02MatchAllow, sessionGuardRowR03MailboxRouted, sessionGuardRowR04IgnorePin, sessionGuardRowR08MismatchExit5, sessionGuardRowR09InvalidExit5}, phase: "entry"},
	{site: "internal/cli/drain.go:51", kind: sessionGuardMailbox, rows: []string{sessionGuardRowR01UnboundAllow, sessionGuardRowR02MatchAllow, sessionGuardRowR03MailboxRouted, sessionGuardRowR04IgnorePin, sessionGuardRowR08MismatchExit5, sessionGuardRowR09InvalidExit5}, phase: "entry"},
	{site: "internal/cli/watch.go:73", kind: sessionGuardMailbox, rows: []string{sessionGuardRowR01UnboundAllow, sessionGuardRowR02MatchAllow, sessionGuardRowR03MailboxRouted, sessionGuardRowR04IgnorePin, sessionGuardRowR08MismatchExit5, sessionGuardRowR09InvalidExit5}, phase: "entry"},
	{site: "internal/cli/watch.go:101", kind: sessionGuardMailbox, rows: []string{sessionGuardRowR01UnboundAllow, sessionGuardRowR02MatchAllow, sessionGuardRowR03MailboxRouted, sessionGuardRowR04IgnorePin, sessionGuardRowR08MismatchExit5, sessionGuardRowR09InvalidExit5}, phase: "revalidation"},
	{site: "internal/cli/monitor.go:72", kind: sessionGuardMailbox, rows: []string{sessionGuardRowR01UnboundAllow, sessionGuardRowR02MatchAllow, sessionGuardRowR03MailboxRouted, sessionGuardRowR04IgnorePin, sessionGuardRowR08MismatchExit5, sessionGuardRowR09InvalidExit5}, phase: "entry"},
	{site: "internal/cli/monitor.go:99", kind: sessionGuardMailbox, rows: []string{sessionGuardRowR01UnboundAllow, sessionGuardRowR02MatchAllow, sessionGuardRowR03MailboxRouted, sessionGuardRowR04IgnorePin, sessionGuardRowR08MismatchExit5, sessionGuardRowR09InvalidExit5}, phase: "revalidation"},
	{site: "internal/cli/list.go:86", kind: sessionGuardList, rows: []string{sessionGuardRowR10ListMismatchWarn}, phase: "cwd warning"},
	{site: "internal/cli/list.go:102", kind: sessionGuardList, rows: []string{sessionGuardRowR01UnboundAllow, sessionGuardRowR02MatchAllow, sessionGuardRowR06ListOwnBase, sessionGuardRowR10ListMismatchWarn, sessionGuardRowR11ListInvalidWarn}, phase: "warning"},
	{site: "internal/cli/dlq.go:101", kind: sessionGuardMailbox, rows: []string{sessionGuardRowR01UnboundAllow, sessionGuardRowR02MatchAllow, sessionGuardRowR03MailboxRouted, sessionGuardRowR04IgnorePin, sessionGuardRowR08MismatchExit5, sessionGuardRowR09InvalidExit5}, phase: "entry"},
	{site: "internal/cli/dlq.go:288", kind: sessionGuardMailbox, rows: []string{sessionGuardRowR01UnboundAllow, sessionGuardRowR02MatchAllow, sessionGuardRowR03MailboxRouted, sessionGuardRowR04IgnorePin, sessionGuardRowR08MismatchExit5, sessionGuardRowR09InvalidExit5}, phase: "entry"},
	{site: "internal/cli/dlq.go:419", kind: sessionGuardMailbox, rows: []string{sessionGuardRowR01UnboundAllow, sessionGuardRowR02MatchAllow, sessionGuardRowR03MailboxRouted, sessionGuardRowR04IgnorePin, sessionGuardRowR08MismatchExit5, sessionGuardRowR09InvalidExit5}, phase: "entry"},
	{site: "internal/cli/dlq.go:601", kind: sessionGuardMailbox, rows: []string{sessionGuardRowR01UnboundAllow, sessionGuardRowR02MatchAllow, sessionGuardRowR03MailboxRouted, sessionGuardRowR04IgnorePin, sessionGuardRowR08MismatchExit5, sessionGuardRowR09InvalidExit5}, phase: "entry"},
	{site: "internal/cli/send.go:171", kind: sessionGuardSource, rows: []string{sessionGuardRowR01UnboundAllow, sessionGuardRowR02MatchAllow, sessionGuardRowR04IgnorePin, sessionGuardRowR05SourceFromSession, sessionGuardRowR08MismatchExit5, sessionGuardRowR09InvalidExit5}, phase: "identity-pin branch"},
	{site: "internal/cli/send.go:187", kind: sessionGuardSource, rows: []string{sessionGuardRowR01UnboundAllow, sessionGuardRowR02MatchAllow, sessionGuardRowR04IgnorePin, sessionGuardRowR05SourceFromSession, sessionGuardRowR08MismatchExit5, sessionGuardRowR09InvalidExit5}, phase: "legacy branch"},
	{site: "internal/cli/reply.go:60", kind: sessionGuardSource, rows: []string{sessionGuardRowR01UnboundAllow, sessionGuardRowR02MatchAllow, sessionGuardRowR04IgnorePin, sessionGuardRowR08MismatchExit5, sessionGuardRowR09InvalidExit5}, phase: "entry"},
	{site: "internal/cli/route.go:147", kind: sessionGuardSource, channel: sessionGuardChannelJSON, rows: []string{sessionGuardRowR01UnboundAllow, sessionGuardRowR02MatchAllow, sessionGuardRowR14RouteMismatchJSON, sessionGuardRowR15RouteInvalidJSON}, phase: "json probe"},
	{site: "internal/cli/env.go:162", kind: sessionGuardEnv, rows: []string{sessionGuardRowR01UnboundAllow, sessionGuardRowR02MatchAllow, sessionGuardRowR08MismatchExit5, sessionGuardRowR09InvalidExit5}, phase: "ambient resolution"},
	{site: "internal/cli/doctor.go:176", kind: sessionGuardDoctorRepair, rows: []string{sessionGuardRowR01UnboundAllow, sessionGuardRowR02MatchAllow, sessionGuardRowR04IgnorePin, sessionGuardRowR07DoctorOwnBase, sessionGuardRowR12DoctorMismatchErr, sessionGuardRowR13DoctorInvalidErr}, phase: "wake-lock repair"},
	{site: "internal/cli/doctor.go:437", rows: nil, phase: "evaluator-only diagnostic", evaluatorOnly: true},
	{site: "internal/cli/doctor.go:561", kind: sessionGuardDoctorRepair, rows: []string{sessionGuardRowR01UnboundAllow, sessionGuardRowR02MatchAllow, sessionGuardRowR04IgnorePin, sessionGuardRowR07DoctorOwnBase, sessionGuardRowR12DoctorMismatchErr, sessionGuardRowR13DoctorInvalidErr}, phase: "mailbox repair"},
}

func TestSessionGuardSiteMappingCoversNineteenDecisionSitesPlusDiagnostic(t *testing.T) {
	if got, want := len(sessionGuardSiteMappings), 20; got != want {
		t.Fatalf("site mapping count = %d, want %d (19 decision sites plus diagnostic)", got, want)
	}
	rowSet := make(map[string]struct{}, len(sessionGuardCanonicalRows))
	for _, row := range sessionGuardCanonicalRows {
		rowSet[row] = struct{}{}
	}
	decisionSites, evaluatorOnlySites := 0, 0
	for _, site := range sessionGuardSiteMappings {
		if site.evaluatorOnly {
			evaluatorOnlySites++
			if len(site.rows) != 0 {
				t.Errorf("evaluator-only %s has decision rows: %v", site.site, site.rows)
			}
			continue
		}
		decisionSites++
		if len(site.rows) == 0 {
			t.Errorf("decision site %s has no mapped rows", site.site)
		}
		for _, row := range site.rows {
			if _, ok := rowSet[row]; !ok {
				t.Errorf("site %s maps unknown row %q", site.site, row)
			}
		}
	}
	if decisionSites != 19 {
		t.Errorf("decision-site mapping count = %d, want 19", decisionSites)
	}
	if evaluatorOnlySites != 1 {
		t.Errorf("evaluator-only site count = %d, want 1", evaluatorOnlySites)
	}
}

func TestSessionGuardSiteCasesReplayTheirMappedRows(t *testing.T) {
	for _, site := range sessionGuardSiteMappings {
		if site.evaluatorOnly {
			continue
		}
		for _, row := range site.rows {
			row, input := sessionGuardSiteReplayCase(site, row)
			t.Run(site.site+"/"+row, func(t *testing.T) {
				got := decideSessionGuard(input)
				if got.Row != row {
					t.Fatalf("row = %q, want %q for phase %s", got.Row, row, site.phase)
				}
			})
		}
	}
}

func TestSessionGuardWatchMonitorMidWaitRepinRefuses(t *testing.T) {
	for _, site := range []string{"internal/cli/watch.go:101", "internal/cli/monitor.go:99"} {
		t.Run(site, func(t *testing.T) {
			entry := decideSessionGuard(sessionGuardInput{
				Kind: sessionGuardMailbox, Pin: sessionGuardPinIdentity,
				Relation: sessionGuardTargetMatch,
			})
			if entry.Row != sessionGuardRowR02MatchAllow || entry.Verdict != sessionGuardAllow {
				t.Fatalf("entry decision = %#v, want matching allow row", entry)
			}

			// A repin to a different root during the live wait must abort the
			// callback with the same refusal row as a mismatched entry guard.
			repinned := decideSessionGuard(sessionGuardInput{
				Kind: sessionGuardMailbox, Pin: sessionGuardPinIdentity,
				Relation: sessionGuardTargetMismatch,
				Flags:    sessionGuardFlags{Revalidation: true},
			})
			if repinned.Row != sessionGuardRowR08MismatchExit5 || repinned.Verdict != sessionGuardRefuseExit5 {
				t.Fatalf("mid-wait repin decision = %#v, want mismatch exit-5 row", repinned)
			}
		})
	}
}

func sessionGuardSiteReplayCase(site sessionGuardSiteMapping, row string) (string, sessionGuardInput) {
	input := sessionGuardInput{Kind: site.kind, Channel: site.channel, Pin: sessionGuardPinIdentity, Relation: sessionGuardTargetMatch}
	input.Flags.Revalidation = site.phase == "revalidation"
	switch row {
	case sessionGuardRowR01UnboundAllow:
		input.Pin, input.Relation = sessionGuardPinAbsent, sessionGuardTargetUnbound
		input.Flags.ExplicitContext = true
	case sessionGuardRowR02MatchAllow:
		// Defaults above.
	case sessionGuardRowR03MailboxRouted:
		input.Pin, input.Relation = sessionGuardPinAbsent, sessionGuardTargetMismatch
		input.Flags.Routed = true
	case sessionGuardRowR04IgnorePin:
		// Send loads the pin before reaching the ignore bypass, so a malformed
		// pin is a preflight R09 there. Other guard callers can bypass the
		// malformed pin itself; model that distinction per textual site.
		input.Pin, input.Relation = sessionGuardPinInvalid, sessionGuardTargetMismatch
		if strings.HasPrefix(site.site, "internal/cli/send.go:") {
			input.Pin = sessionGuardPinIdentity
		}
		input.Flags.ExplicitRoot = true
		input.Flags.IgnorePin = true
	case sessionGuardRowR05SourceFromSession:
		input.Kind, input.Pin, input.Relation = sessionGuardSource, sessionGuardPinIdentity, sessionGuardTargetMismatch
		input.Flags.FromSession = true
	case sessionGuardRowR06ListOwnBase:
		input.Kind, input.Pin, input.Relation = sessionGuardList, sessionGuardPinIdentity, sessionGuardTargetOwnPinnedBase
		input.Flags.ExplicitRoot = true
	case sessionGuardRowR07DoctorOwnBase:
		input.Kind, input.Pin, input.Relation = sessionGuardDoctorRepair, sessionGuardPinIdentity, sessionGuardTargetOwnPinnedBase
	case sessionGuardRowR08MismatchExit5:
		input.Pin, input.Relation = sessionGuardPinIdentity, sessionGuardTargetMismatch
		if site.kind == sessionGuardSource && site.channel == sessionGuardChannelJSON {
			input.Channel = sessionGuardChannelExit5
		}
	case sessionGuardRowR09InvalidExit5:
		input.Pin, input.Relation = sessionGuardPinInvalid, sessionGuardTargetMismatch
	case sessionGuardRowR10ListMismatchWarn:
		input.Kind, input.Pin, input.Relation = sessionGuardList, sessionGuardPinIdentity, sessionGuardTargetMismatch
	case sessionGuardRowR11ListInvalidWarn:
		input.Kind, input.Pin, input.Relation = sessionGuardList, sessionGuardPinInvalid, sessionGuardTargetMismatch
	case sessionGuardRowR12DoctorMismatchErr:
		input.Kind, input.Pin, input.Relation = sessionGuardDoctorRepair, sessionGuardPinIdentity, sessionGuardTargetMismatch
	case sessionGuardRowR13DoctorInvalidErr:
		input.Kind, input.Pin, input.Relation = sessionGuardDoctorRepair, sessionGuardPinInvalid, sessionGuardTargetMismatch
	case sessionGuardRowR14RouteMismatchJSON:
		input.Kind, input.Channel, input.Pin, input.Relation = sessionGuardSource, sessionGuardChannelJSON, sessionGuardPinIdentity, sessionGuardTargetMismatch
	case sessionGuardRowR15RouteInvalidJSON:
		input.Kind, input.Channel, input.Pin, input.Relation = sessionGuardSource, sessionGuardChannelJSON, sessionGuardPinInvalid, sessionGuardTargetMismatch
	default:
		panic("unknown session guard row")
	}
	return row, input
}
