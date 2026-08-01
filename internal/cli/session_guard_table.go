package cli

// sessionGuardKind identifies the policy surface consuming the shared pin
// evaluator. Route explain is a source policy with a different output channel;
// it is intentionally not a second source policy.
type sessionGuardKind uint8

const (
	sessionGuardMailbox sessionGuardKind = iota
	sessionGuardSource
	sessionGuardList
	sessionGuardEnv
	sessionGuardDoctorRepair
)

type sessionGuardChannel uint8

const (
	sessionGuardChannelExit5 sessionGuardChannel = iota
	sessionGuardChannelJSON
)

type sessionGuardPinState uint8

const (
	sessionGuardPinAbsent sessionGuardPinState = iota
	sessionGuardPinLegacy
	sessionGuardPinIdentity
	sessionGuardPinInvalid
)

type sessionGuardTargetRelation uint8

const (
	// The caller has already normalized the evaluator facts. Unbound includes
	// no pin and no established conflicting tree; CWD conflicts normalize to
	// Mismatch before this policy table is consulted.
	sessionGuardTargetUnbound sessionGuardTargetRelation = iota
	sessionGuardTargetMatch
	sessionGuardTargetMismatch
	sessionGuardTargetOwnPinnedBase
)

// These are the only public outcome channels in the policy table.
type sessionGuardVerdict uint8

const (
	sessionGuardAllow sessionGuardVerdict = iota
	sessionGuardRefuseExit5
	sessionGuardWarnContinue
	sessionGuardStructuredErrorContinueExit0
	sessionGuardJSONErrorExit0
)

// The 15 stable rows are policy decisions, not call sites. Keep their IDs
// reviewable because the site-to-row map and equivalence tests refer to them.
const (
	sessionGuardRowR01UnboundAllow      = "R01_unbound_allow"
	sessionGuardRowR02MatchAllow        = "R02_match_allow"
	sessionGuardRowR03MailboxRouted     = "R03_mailbox_routed_allow"
	sessionGuardRowR04IgnorePin         = "R04_ignore_pin_allow"
	sessionGuardRowR05SourceFromSession = "R05_source_from_session_allow"
	sessionGuardRowR06ListOwnBase       = "R06_list_own_base_allow"
	sessionGuardRowR07DoctorOwnBase     = "R07_doctor_own_base_allow"
	sessionGuardRowR08MismatchExit5     = "R08_mismatch_exit5"
	sessionGuardRowR09InvalidExit5      = "R09_invalid_pin_exit5"
	sessionGuardRowR10ListMismatchWarn  = "R10_list_mismatch_warn"
	sessionGuardRowR11ListInvalidWarn   = "R11_list_invalid_warn"
	sessionGuardRowR12DoctorMismatchErr = "R12_doctor_mismatch_structured_error"
	sessionGuardRowR13DoctorInvalidErr  = "R13_doctor_invalid_structured_error"
	sessionGuardRowR14RouteMismatchJSON = "R14_route_mismatch_json_error"
	sessionGuardRowR15RouteInvalidJSON  = "R15_route_invalid_json_error"
)

var sessionGuardCanonicalRows = [...]string{
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

type sessionGuardFlags struct {
	Routed          bool // participating mailbox target was --session-routed
	IgnorePin       bool // --ignore-session-pin passed caller preflight
	ExplicitRoot    bool // caller supplied an explicit root
	ExplicitContext bool // env --root/--session was visited
	CrossProject    bool // source guard serves a --project route
	FromSession     bool // send used --from-session source routing
	Revalidation    bool // watch/monitor callback phase; policy is unchanged
}

type sessionGuardInput struct {
	Kind     sessionGuardKind
	Channel  sessionGuardChannel
	Pin      sessionGuardPinState
	Relation sessionGuardTargetRelation
	Flags    sessionGuardFlags
}

type sessionGuardDecision struct {
	Row     string
	Verdict sessionGuardVerdict
}

func sessionGuardValidPin(pin sessionGuardPinState) bool {
	return pin == sessionGuardPinAbsent || pin == sessionGuardPinLegacy || pin == sessionGuardPinIdentity
}

func sessionGuardPinStateFor(pin sessionPin) sessionGuardPinState {
	if !pin.Present {
		return sessionGuardPinAbsent
	}
	if pin.IdentityPin {
		return sessionGuardPinIdentity
	}
	return sessionGuardPinLegacy
}

func sessionGuardInputForContext(kind sessionGuardKind, channel sessionGuardChannel, pin sessionGuardPinState, mismatch *SessionContextError, flags sessionGuardFlags) sessionGuardInput {
	relation := sessionGuardTargetUnbound
	if mismatch != nil {
		relation = sessionGuardTargetMismatch
	} else if pin != sessionGuardPinAbsent {
		relation = sessionGuardTargetMatch
	}
	return sessionGuardInput{
		Kind: kind, Channel: channel, Pin: pin, Relation: relation, Flags: flags,
	}
}

func sessionGuardDecisionForContext(kind sessionGuardKind, channel sessionGuardChannel, pin sessionGuardPinState, mismatch *SessionContextError, flags sessionGuardFlags) sessionGuardDecision {
	return decideSessionGuard(sessionGuardInputForContext(kind, channel, pin, mismatch, flags))
}

func sessionGuardDecisionFor(row string, verdict sessionGuardVerdict) sessionGuardDecision {
	return sessionGuardDecision{Row: row, Verdict: verdict}
}

// decideSessionGuard is the pure W2a policy table. Filesystem resolution,
// identity authentication, preflight validation, error construction, and
// output channels remain at the current call sites until Step 3 rewiring.
//
// TargetRelationMismatch includes either a pin mismatch or a CWD conflict
// after the caller's evaluator has normalized those facts. Explicit roots and
// cross-project routing must not turn a pin mismatch into a match; they only
// affect the pre-table CWD check.
func decideSessionGuard(in sessionGuardInput) sessionGuardDecision {
	// Early policy bypasses are reachable only after each caller's preflight.
	// Keeping them before pin/relation handling makes their authority explicit.
	if in.Kind == sessionGuardMailbox && in.Flags.Routed && sessionGuardValidPin(in.Pin) {
		return sessionGuardDecisionFor(sessionGuardRowR03MailboxRouted, sessionGuardAllow)
	}
	if in.Flags.IgnorePin && (in.Kind == sessionGuardMailbox || in.Kind == sessionGuardSource || in.Kind == sessionGuardDoctorRepair) {
		return sessionGuardDecisionFor(sessionGuardRowR04IgnorePin, sessionGuardAllow)
	}
	if in.Kind == sessionGuardSource && in.Flags.FromSession && sessionGuardValidPin(in.Pin) {
		return sessionGuardDecisionFor(sessionGuardRowR05SourceFromSession, sessionGuardAllow)
	}

	invalid := in.Pin == sessionGuardPinInvalid
	if invalid {
		switch in.Kind {
		case sessionGuardList:
			return sessionGuardDecisionFor(sessionGuardRowR11ListInvalidWarn, sessionGuardWarnContinue)
		case sessionGuardDoctorRepair:
			return sessionGuardDecisionFor(sessionGuardRowR13DoctorInvalidErr, sessionGuardStructuredErrorContinueExit0)
		case sessionGuardSource:
			if in.Channel == sessionGuardChannelJSON {
				return sessionGuardDecisionFor(sessionGuardRowR15RouteInvalidJSON, sessionGuardJSONErrorExit0)
			}
			return sessionGuardDecisionFor(sessionGuardRowR09InvalidExit5, sessionGuardRefuseExit5)
		default:
			return sessionGuardDecisionFor(sessionGuardRowR09InvalidExit5, sessionGuardRefuseExit5)
		}
	}

	if in.Relation == sessionGuardTargetUnbound {
		return sessionGuardDecisionFor(sessionGuardRowR01UnboundAllow, sessionGuardAllow)
	}
	if in.Relation == sessionGuardTargetMatch && sessionGuardValidPin(in.Pin) {
		return sessionGuardDecisionFor(sessionGuardRowR02MatchAllow, sessionGuardAllow)
	}

	if in.Relation == sessionGuardTargetOwnPinnedBase {
		switch in.Kind {
		case sessionGuardList:
			if in.Flags.ExplicitRoot && sessionGuardValidPin(in.Pin) {
				return sessionGuardDecisionFor(sessionGuardRowR06ListOwnBase, sessionGuardAllow)
			}
		case sessionGuardDoctorRepair:
			if sessionGuardValidPin(in.Pin) {
				return sessionGuardDecisionFor(sessionGuardRowR07DoctorOwnBase, sessionGuardAllow)
			}
		}
	}

	if in.Kind == sessionGuardList {
		return sessionGuardDecisionFor(sessionGuardRowR10ListMismatchWarn, sessionGuardWarnContinue)
	}
	if in.Kind == sessionGuardDoctorRepair {
		return sessionGuardDecisionFor(sessionGuardRowR12DoctorMismatchErr, sessionGuardStructuredErrorContinueExit0)
	}
	if in.Kind == sessionGuardSource && in.Channel == sessionGuardChannelJSON {
		return sessionGuardDecisionFor(sessionGuardRowR14RouteMismatchJSON, sessionGuardJSONErrorExit0)
	}
	return sessionGuardDecisionFor(sessionGuardRowR08MismatchExit5, sessionGuardRefuseExit5)
}
