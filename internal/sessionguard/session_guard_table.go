package sessionguard

// Kind identifies the policy surface consuming the shared pin
// evaluator. Route explain is a source policy with a different output channel;
// it is intentionally not a second source policy.
type Kind uint8

const (
	KindMailbox Kind = iota
	KindSource
	KindList
	KindEnv
	KindDoctorRepair
)

type Channel uint8

const (
	ChannelExit5 Channel = iota
	ChannelJSON
)

type PinState uint8

const (
	PinAbsent PinState = iota
	PinLegacy
	PinIdentity
	PinInvalid
)

type TargetRelation uint8

const (
	// The caller has already normalized the evaluator facts. Unbound includes
	// no pin and no established conflicting tree; CWD conflicts normalize to
	// Mismatch before this policy table is consulted.
	TargetUnbound TargetRelation = iota
	TargetMatch
	TargetMismatch
	TargetOwnPinnedBase
)

// These are the only public outcome channels in the policy table.
type Verdict uint8

const (
	Allow Verdict = iota
	RefuseExit5
	WarnContinue
	StructuredErrorContinueExit0
	JSONErrorExit0
)

// The 15 stable rows are policy decisions, not call sites. Keep their IDs
// reviewable because the site-to-row map and equivalence tests refer to them.
const (
	RowR01UnboundAllow      = "R01_unbound_allow"
	RowR02MatchAllow        = "R02_match_allow"
	RowR03MailboxRouted     = "R03_mailbox_routed_allow"
	RowR04IgnorePin         = "R04_ignore_pin_allow"
	RowR05SourceFromSession = "R05_source_from_session_allow"
	RowR06ListOwnBase       = "R06_list_own_base_allow"
	RowR07DoctorOwnBase     = "R07_doctor_own_base_allow"
	RowR08MismatchExit5     = "R08_mismatch_exit5"
	RowR09InvalidExit5      = "R09_invalid_pin_exit5"
	RowR10ListMismatchWarn  = "R10_list_mismatch_warn"
	RowR11ListInvalidWarn   = "R11_list_invalid_warn"
	RowR12DoctorMismatchErr = "R12_doctor_mismatch_structured_error"
	RowR13DoctorInvalidErr  = "R13_doctor_invalid_structured_error"
	RowR14RouteMismatchJSON = "R14_route_mismatch_json_error"
	RowR15RouteInvalidJSON  = "R15_route_invalid_json_error"
)

var canonicalRows = [...]string{
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

type Flags struct {
	Routed          bool // participating mailbox target was --session-routed
	IgnorePin       bool // --ignore-session-pin passed caller preflight
	ExplicitRoot    bool // caller supplied an explicit root
	ExplicitContext bool // env --root/--session was visited
	CrossProject    bool // source guard serves a --project route
	FromSession     bool // send used --from-session source routing
	Revalidation    bool // watch/monitor callback phase; policy is unchanged
}

type Input struct {
	Kind     Kind
	Channel  Channel
	Pin      PinState
	Relation TargetRelation
	Flags    Flags
}

type Decision struct {
	Row     string
	Verdict Verdict
}

func validPin(pin PinState) bool {
	return pin == PinAbsent || pin == PinLegacy || pin == PinIdentity
}

func decisionFor(row string, verdict Verdict) Decision {
	return Decision{Row: row, Verdict: verdict}
}

// Decide is the pure W2a policy table. Filesystem resolution,
// identity authentication, preflight validation, error construction, and
// output channels remain at the current call sites until Step 3 rewiring.
//
// TargetRelationMismatch includes either a pin mismatch or a CWD conflict
// after the caller's evaluator has normalized those facts. Explicit roots and
// cross-project routing must not turn a pin mismatch into a match; they only
// affect the pre-table CWD check.
func Decide(in Input) Decision {
	// Early policy bypasses are reachable only after each caller's preflight.
	// Keeping them before pin/relation handling makes their authority explicit.
	if in.Kind == KindMailbox && in.Flags.Routed && validPin(in.Pin) {
		return decisionFor(RowR03MailboxRouted, Allow)
	}
	if in.Flags.IgnorePin && (in.Kind == KindMailbox || in.Kind == KindSource || in.Kind == KindDoctorRepair) {
		return decisionFor(RowR04IgnorePin, Allow)
	}
	if in.Kind == KindSource && in.Flags.FromSession && validPin(in.Pin) {
		return decisionFor(RowR05SourceFromSession, Allow)
	}

	invalid := in.Pin == PinInvalid
	if invalid {
		switch in.Kind {
		case KindList:
			return decisionFor(RowR11ListInvalidWarn, WarnContinue)
		case KindDoctorRepair:
			return decisionFor(RowR13DoctorInvalidErr, StructuredErrorContinueExit0)
		case KindSource:
			if in.Channel == ChannelJSON {
				return decisionFor(RowR15RouteInvalidJSON, JSONErrorExit0)
			}
			return decisionFor(RowR09InvalidExit5, RefuseExit5)
		default:
			return decisionFor(RowR09InvalidExit5, RefuseExit5)
		}
	}

	if in.Relation == TargetUnbound {
		return decisionFor(RowR01UnboundAllow, Allow)
	}
	if in.Relation == TargetMatch && validPin(in.Pin) {
		return decisionFor(RowR02MatchAllow, Allow)
	}

	if in.Relation == TargetOwnPinnedBase {
		switch in.Kind {
		case KindList:
			if in.Flags.ExplicitRoot && validPin(in.Pin) {
				return decisionFor(RowR06ListOwnBase, Allow)
			}
		case KindDoctorRepair:
			if validPin(in.Pin) {
				return decisionFor(RowR07DoctorOwnBase, Allow)
			}
		}
	}

	if in.Kind == KindList {
		return decisionFor(RowR10ListMismatchWarn, WarnContinue)
	}
	if in.Kind == KindDoctorRepair {
		return decisionFor(RowR12DoctorMismatchErr, StructuredErrorContinueExit0)
	}
	if in.Kind == KindSource && in.Channel == ChannelJSON {
		return decisionFor(RowR14RouteMismatchJSON, JSONErrorExit0)
	}
	return decisionFor(RowR08MismatchExit5, RefuseExit5)
}
