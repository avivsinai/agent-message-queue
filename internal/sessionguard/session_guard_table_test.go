package sessionguard

import (
	"strings"
	"testing"
)

func TestSessionGuardCanonicalRows(t *testing.T) {
	want := []string{
		RowR01UnboundAllow,
		RowR02MatchAllow,
		RowR03MailboxRouted,
		RowR04IgnorePin,
		RowR05SourceFromSession,
		RowR06ListOwnBase,
		RowR07DoctorOwnBase,
		RowR08MismatchExit5,
		RowR09InvalidExit5,
		RowR10ListMismatchWarn,
		RowR11ListInvalidWarn,
		RowR12DoctorMismatchErr,
		RowR13DoctorInvalidErr,
		RowR14RouteMismatchJSON,
		RowR15RouteInvalidJSON,
	}
	if len(canonicalRows) != len(want) {
		t.Fatalf("canonical row count = %d, want %d", len(canonicalRows), len(want))
	}
	for i, row := range want {
		if got := canonicalRows[i]; got != row {
			t.Errorf("canonical row %d = %q, want %q", i, got, row)
		}
	}
}

func TestSessionGuardDecisionRows(t *testing.T) {
	tests := []struct {
		name    string
		input   Input
		row     string
		verdict Verdict
	}{
		{
			name: "R01 unbound allows every policy",
			input: Input{
				Kind: KindEnv, Relation: TargetUnbound,
				Flags: Flags{ExplicitContext: true},
			},
			row: RowR01UnboundAllow, verdict: Allow,
		},
		{
			name: "R02 matching identity pin allows",
			input: Input{
				Kind: KindMailbox, Pin: PinIdentity, Relation: TargetMatch,
			},
			row: RowR02MatchAllow, verdict: Allow,
		},
		{
			name: "R03 mailbox routed after valid preflight",
			input: Input{
				Kind: KindMailbox, Pin: PinAbsent, Relation: TargetMismatch,
				Flags: Flags{Routed: true},
			},
			row: RowR03MailboxRouted, verdict: Allow,
		},
		{
			name: "R09 invalid pin is not hidden by routed preflight",
			input: Input{
				Kind: KindMailbox, Pin: PinInvalid, Relation: TargetMismatch,
				Flags: Flags{Routed: true},
			},
			row: RowR09InvalidExit5, verdict: RefuseExit5,
		},
		{
			name: "R04 ignore pin is an early policy bypass",
			input: Input{
				Kind: KindDoctorRepair, Pin: PinInvalid, Relation: TargetMismatch,
				Flags: Flags{ExplicitRoot: true, IgnorePin: true},
			},
			row: RowR04IgnorePin, verdict: Allow,
		},
		{
			name: "R05 send from-session bypass follows identity preflight",
			input: Input{
				Kind: KindSource, Pin: PinIdentity, Relation: TargetMismatch,
				Flags: Flags{FromSession: true},
			},
			row: RowR05SourceFromSession, verdict: Allow,
		},
		{
			name: "R09 invalid pin is not hidden by from-session preflight",
			input: Input{
				Kind: KindSource, Pin: PinInvalid, Relation: TargetMismatch,
				Flags: Flags{FromSession: true},
			},
			row: RowR09InvalidExit5, verdict: RefuseExit5,
		},
		{
			name: "R06 list own pinned base suppresses warning",
			input: Input{
				Kind: KindList, Pin: PinIdentity, Relation: TargetOwnPinnedBase,
				Flags: Flags{ExplicitRoot: true},
			},
			row: RowR06ListOwnBase, verdict: Allow,
		},
		{
			name: "R07 doctor own pinned base allows repair",
			input: Input{
				Kind: KindDoctorRepair, Pin: PinIdentity, Relation: TargetOwnPinnedBase,
			},
			row: RowR07DoctorOwnBase, verdict: Allow,
		},
		{
			name: "R08 valid mismatch refuses mailbox",
			input: Input{
				Kind: KindMailbox, Pin: PinLegacy, Relation: TargetMismatch,
			},
			row: RowR08MismatchExit5, verdict: RefuseExit5,
		},
		{
			name: "R08 cross-project still refuses source mismatch",
			input: Input{
				Kind: KindSource, Pin: PinIdentity, Relation: TargetMismatch,
				Flags: Flags{CrossProject: true},
			},
			row: RowR08MismatchExit5, verdict: RefuseExit5,
		},
		{
			name: "R08 explicit root does not waive pin mismatch",
			input: Input{
				Kind: KindMailbox, Pin: PinIdentity, Relation: TargetMismatch,
				Flags: Flags{ExplicitRoot: true},
			},
			row: RowR08MismatchExit5, verdict: RefuseExit5,
		},
		{
			name: "R08 source explicit root does not waive pin mismatch",
			input: Input{
				Kind: KindSource, Pin: PinIdentity, Relation: TargetMismatch,
				Flags: Flags{ExplicitRoot: true},
			},
			row: RowR08MismatchExit5, verdict: RefuseExit5,
		},
		{
			name: "R09 invalid pin refuses exit five",
			input: Input{
				Kind: KindEnv, Pin: PinInvalid, Relation: TargetMismatch,
			},
			row: RowR09InvalidExit5, verdict: RefuseExit5,
		},
		{
			name: "R09 invalid pin wins over unbound relation",
			input: Input{
				Kind: KindMailbox, Pin: PinInvalid, Relation: TargetUnbound,
			},
			row: RowR09InvalidExit5, verdict: RefuseExit5,
		},
		{
			name: "R10 list mismatch warns",
			input: Input{
				Kind: KindList, Pin: PinLegacy, Relation: TargetMismatch,
			},
			row: RowR10ListMismatchWarn, verdict: WarnContinue,
		},
		{
			name: "R11 list invalid warns",
			input: Input{
				Kind: KindList, Pin: PinInvalid, Relation: TargetMismatch,
			},
			row: RowR11ListInvalidWarn, verdict: WarnContinue,
		},
		{
			name: "R12 doctor mismatch is structured error continue",
			input: Input{
				Kind: KindDoctorRepair, Pin: PinIdentity, Relation: TargetMismatch,
			},
			row: RowR12DoctorMismatchErr, verdict: StructuredErrorContinueExit0,
		},
		{
			name: "R13 doctor invalid is structured error continue",
			input: Input{
				Kind: KindDoctorRepair, Pin: PinInvalid, Relation: TargetMismatch,
			},
			row: RowR13DoctorInvalidErr, verdict: StructuredErrorContinueExit0,
		},
		{
			name: "R14 route mismatch is json error exit zero",
			input: Input{
				Kind: KindSource, Channel: ChannelJSON,
				Pin: PinLegacy, Relation: TargetMismatch,
			},
			row: RowR14RouteMismatchJSON, verdict: JSONErrorExit0,
		},
		{
			name: "R15 route invalid is json error exit zero",
			input: Input{
				Kind: KindSource, Channel: ChannelJSON,
				Pin: PinInvalid, Relation: TargetMismatch,
			},
			row: RowR15RouteInvalidJSON, verdict: JSONErrorExit0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := Decide(test.input)
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
	for _, kind := range []Kind{KindMailbox} {
		for _, relation := range []TargetRelation{TargetMatch, TargetMismatch} {
			entry := Decide(Input{Kind: kind, Pin: PinIdentity, Relation: relation})
			revalidated := Decide(Input{
				Kind: kind, Pin: PinIdentity, Relation: relation,
				Flags: Flags{Revalidation: true},
			})
			if revalidated != entry {
				t.Errorf("revalidation relation %d decision %#v, want entry %#v", relation, revalidated, entry)
			}
		}
	}
}

type sessionGuardSiteMapping struct {
	site          string
	kind          Kind
	channel       Channel
	rows          []string
	phase         string
	evaluatorOnly bool
}

// R1 has 19 decision-site mappings plus one evaluator-only diagnostic.
// Revalidation callbacks and both send branches are textual consumers of
// existing rows; doctor:437 intentionally maps to no decision row.
var sessionGuardSiteMappings = []sessionGuardSiteMapping{
	{site: "internal/cli/read.go:50", kind: KindMailbox, rows: []string{RowR01UnboundAllow, RowR02MatchAllow, RowR03MailboxRouted, RowR04IgnorePin, RowR08MismatchExit5, RowR09InvalidExit5}, phase: "entry"},
	{site: "internal/cli/drain.go:51", kind: KindMailbox, rows: []string{RowR01UnboundAllow, RowR02MatchAllow, RowR03MailboxRouted, RowR04IgnorePin, RowR08MismatchExit5, RowR09InvalidExit5}, phase: "entry"},
	{site: "internal/cli/watch.go:73", kind: KindMailbox, rows: []string{RowR01UnboundAllow, RowR02MatchAllow, RowR03MailboxRouted, RowR04IgnorePin, RowR08MismatchExit5, RowR09InvalidExit5}, phase: "entry"},
	{site: "internal/cli/watch.go:101", kind: KindMailbox, rows: []string{RowR01UnboundAllow, RowR02MatchAllow, RowR03MailboxRouted, RowR04IgnorePin, RowR08MismatchExit5, RowR09InvalidExit5}, phase: "revalidation"},
	{site: "internal/cli/monitor.go:72", kind: KindMailbox, rows: []string{RowR01UnboundAllow, RowR02MatchAllow, RowR03MailboxRouted, RowR04IgnorePin, RowR08MismatchExit5, RowR09InvalidExit5}, phase: "entry"},
	{site: "internal/cli/monitor.go:99", kind: KindMailbox, rows: []string{RowR01UnboundAllow, RowR02MatchAllow, RowR03MailboxRouted, RowR04IgnorePin, RowR08MismatchExit5, RowR09InvalidExit5}, phase: "revalidation"},
	{site: "internal/cli/list.go:86", kind: KindList, rows: []string{RowR10ListMismatchWarn}, phase: "cwd warning"},
	{site: "internal/cli/list.go:102", kind: KindList, rows: []string{RowR01UnboundAllow, RowR02MatchAllow, RowR06ListOwnBase, RowR10ListMismatchWarn, RowR11ListInvalidWarn}, phase: "warning"},
	{site: "internal/cli/dlq.go:101", kind: KindMailbox, rows: []string{RowR01UnboundAllow, RowR02MatchAllow, RowR03MailboxRouted, RowR04IgnorePin, RowR08MismatchExit5, RowR09InvalidExit5}, phase: "entry"},
	{site: "internal/cli/dlq.go:288", kind: KindMailbox, rows: []string{RowR01UnboundAllow, RowR02MatchAllow, RowR03MailboxRouted, RowR04IgnorePin, RowR08MismatchExit5, RowR09InvalidExit5}, phase: "entry"},
	{site: "internal/cli/dlq.go:419", kind: KindMailbox, rows: []string{RowR01UnboundAllow, RowR02MatchAllow, RowR03MailboxRouted, RowR04IgnorePin, RowR08MismatchExit5, RowR09InvalidExit5}, phase: "entry"},
	{site: "internal/cli/dlq.go:601", kind: KindMailbox, rows: []string{RowR01UnboundAllow, RowR02MatchAllow, RowR03MailboxRouted, RowR04IgnorePin, RowR08MismatchExit5, RowR09InvalidExit5}, phase: "entry"},
	{site: "internal/cli/send.go:171", kind: KindSource, rows: []string{RowR01UnboundAllow, RowR02MatchAllow, RowR04IgnorePin, RowR05SourceFromSession, RowR08MismatchExit5, RowR09InvalidExit5}, phase: "identity-pin branch"},
	{site: "internal/cli/send.go:187", kind: KindSource, rows: []string{RowR01UnboundAllow, RowR02MatchAllow, RowR04IgnorePin, RowR05SourceFromSession, RowR08MismatchExit5, RowR09InvalidExit5}, phase: "legacy branch"},
	{site: "internal/cli/reply.go:60", kind: KindSource, rows: []string{RowR01UnboundAllow, RowR02MatchAllow, RowR04IgnorePin, RowR08MismatchExit5, RowR09InvalidExit5}, phase: "entry"},
	{site: "internal/cli/route.go:147", kind: KindSource, channel: ChannelJSON, rows: []string{RowR01UnboundAllow, RowR02MatchAllow, RowR14RouteMismatchJSON, RowR15RouteInvalidJSON}, phase: "json probe"},
	{site: "internal/cli/env.go:162", kind: KindEnv, rows: []string{RowR01UnboundAllow, RowR02MatchAllow, RowR08MismatchExit5, RowR09InvalidExit5}, phase: "ambient resolution"},
	{site: "internal/cli/doctor.go:176", kind: KindDoctorRepair, rows: []string{RowR01UnboundAllow, RowR02MatchAllow, RowR04IgnorePin, RowR07DoctorOwnBase, RowR12DoctorMismatchErr, RowR13DoctorInvalidErr}, phase: "wake-lock repair"},
	{site: "internal/cli/doctor.go:437", rows: nil, phase: "evaluator-only diagnostic", evaluatorOnly: true},
	{site: "internal/cli/doctor.go:561", kind: KindDoctorRepair, rows: []string{RowR01UnboundAllow, RowR02MatchAllow, RowR04IgnorePin, RowR07DoctorOwnBase, RowR12DoctorMismatchErr, RowR13DoctorInvalidErr}, phase: "mailbox repair"},
}

func TestSessionGuardSiteMappingCoversNineteenDecisionSitesPlusDiagnostic(t *testing.T) {
	if got, want := len(sessionGuardSiteMappings), 20; got != want {
		t.Fatalf("site mapping count = %d, want %d (19 decision sites plus diagnostic)", got, want)
	}
	rowSet := make(map[string]struct{}, len(canonicalRows))
	for _, row := range canonicalRows {
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
				got := Decide(input)
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
			entry := Decide(Input{
				Kind: KindMailbox, Pin: PinIdentity,
				Relation: TargetMatch,
			})
			if entry.Row != RowR02MatchAllow || entry.Verdict != Allow {
				t.Fatalf("entry decision = %#v, want matching allow row", entry)
			}

			// A repin to a different root during the live wait must abort the
			// callback with the same refusal row as a mismatched entry guard.
			repinned := Decide(Input{
				Kind: KindMailbox, Pin: PinIdentity,
				Relation: TargetMismatch,
				Flags:    Flags{Revalidation: true},
			})
			if repinned.Row != RowR08MismatchExit5 || repinned.Verdict != RefuseExit5 {
				t.Fatalf("mid-wait repin decision = %#v, want mismatch exit-5 row", repinned)
			}
		})
	}
}

func sessionGuardSiteReplayCase(site sessionGuardSiteMapping, row string) (string, Input) {
	input := Input{Kind: site.kind, Channel: site.channel, Pin: PinIdentity, Relation: TargetMatch}
	input.Flags.Revalidation = site.phase == "revalidation"
	switch row {
	case RowR01UnboundAllow:
		input.Pin, input.Relation = PinAbsent, TargetUnbound
		input.Flags.ExplicitContext = true
	case RowR02MatchAllow:
		// Defaults above.
	case RowR03MailboxRouted:
		input.Pin, input.Relation = PinAbsent, TargetMismatch
		input.Flags.Routed = true
	case RowR04IgnorePin:
		// Send loads the pin before reaching the ignore bypass, so a malformed
		// pin is a preflight R09 there. Other guard callers can bypass the
		// malformed pin itself; model that distinction per textual site.
		input.Pin, input.Relation = PinInvalid, TargetMismatch
		if strings.HasPrefix(site.site, "internal/cli/send.go:") {
			input.Pin = PinIdentity
		}
		input.Flags.ExplicitRoot = true
		input.Flags.IgnorePin = true
	case RowR05SourceFromSession:
		input.Kind, input.Pin, input.Relation = KindSource, PinIdentity, TargetMismatch
		input.Flags.FromSession = true
	case RowR06ListOwnBase:
		input.Kind, input.Pin, input.Relation = KindList, PinIdentity, TargetOwnPinnedBase
		input.Flags.ExplicitRoot = true
	case RowR07DoctorOwnBase:
		input.Kind, input.Pin, input.Relation = KindDoctorRepair, PinIdentity, TargetOwnPinnedBase
	case RowR08MismatchExit5:
		input.Pin, input.Relation = PinIdentity, TargetMismatch
		if site.kind == KindSource && site.channel == ChannelJSON {
			input.Channel = ChannelExit5
		}
	case RowR09InvalidExit5:
		input.Pin, input.Relation = PinInvalid, TargetMismatch
	case RowR10ListMismatchWarn:
		input.Kind, input.Pin, input.Relation = KindList, PinIdentity, TargetMismatch
	case RowR11ListInvalidWarn:
		input.Kind, input.Pin, input.Relation = KindList, PinInvalid, TargetMismatch
	case RowR12DoctorMismatchErr:
		input.Kind, input.Pin, input.Relation = KindDoctorRepair, PinIdentity, TargetMismatch
	case RowR13DoctorInvalidErr:
		input.Kind, input.Pin, input.Relation = KindDoctorRepair, PinInvalid, TargetMismatch
	case RowR14RouteMismatchJSON:
		input.Kind, input.Channel, input.Pin, input.Relation = KindSource, ChannelJSON, PinIdentity, TargetMismatch
	case RowR15RouteInvalidJSON:
		input.Kind, input.Channel, input.Pin, input.Relation = KindSource, ChannelJSON, PinInvalid, TargetMismatch
	default:
		panic("unknown session guard row")
	}
	return row, input
}
