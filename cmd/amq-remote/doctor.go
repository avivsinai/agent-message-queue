package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/relay"
	"github.com/avivsinai/agent-message-queue/internal/remote/amqio"
	"github.com/avivsinai/agent-message-queue/internal/remote/buzzio"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"github.com/avivsinai/agent-message-queue/internal/remote/ipc"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
)

func doctor(args []string, probe ...*jsonProbe) (any, int, error) {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := addCommon(fs, probe...)
	me := fs.String("me", amqio.DefaultHandle, "endpoint mailbox handle in the root")
	if err := fs.Parse(args); err != nil {
		return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "%v", err)
	}
	if err := fsq.ValidateHandle(*me); err != nil {
		return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "invalid handle %q: %v", *me, err)
	}
	stateDir, err := c.stateDir()
	if err != nil {
		return nil, protocol.ExitUsage, err
	}
	report := map[string]any{"root": c.root, "state_dir": stateDir, "socket": ipc.SocketPath(stateDir)}
	// failing names each broken boundary between the owner and the native
	// session: AMQ route, endpoint, registration, native capability,
	// body key, tag expiry, relay auth, DM surface, publication, presence,
	// discovery.
	// Doctor exits 6 exactly when it is not empty.
	var failing []boundaryFailure
	fail := func(boundary, subject, detail, remedy string) {
		failing = append(failing, boundaryFailure{Boundary: boundary, Subject: subject, Detail: detail, Remedy: remedy})
	}
	finish := func() (any, int, error) {
		report["failing"] = failing
		if len(failing) > 0 {
			return report, protocol.ExitActionRequired, nil
		}
		report["failing"] = []boundaryFailure{}
		return report, 0, nil
	}
	// notes carry non-failing advice in the same shape as failing entries;
	// doctor exits 0 when only notes are present.
	var notes []boundaryFailure
	note := func(boundary, subject, detail, remedy string) {
		notes = append(notes, boundaryFailure{Boundary: boundary, Subject: subject, Detail: detail, Remedy: remedy})
	}
	if _, err := os.Stat(stateDir); err != nil {
		report["endpoint"] = "no state directory; run `amq-remote serve` once"
		fail("endpoint", "", "no state directory", "run `amq-remote serve` once")
		noteStaleACP(note)
		if len(notes) > 0 {
			report["notes"] = notes
		}
		return finish()
	}
	if detail := amqRouteDetail(c.root, *me); detail != "" {
		fail("amq_route", *me, detail, "run `amq-remote serve` once so it registers this handle in config.json")
	}
	var sessions []protocol.Session
	resp, err := ipc.Call(stateDir, ipc.Request{Command: &protocol.Command{Schema: protocol.SchemaCommand, Op: protocol.OpSessionList}})
	switch {
	case err != nil:
		report["endpoint"] = "not reachable: " + err.Error()
		fail("endpoint", "", "not reachable: "+err.Error(), "start `amq-remote serve` (or `up`) for this root")
	case resp.AsError() != nil:
		report["endpoint"] = "refused session.list: " + resp.AsError().Error()
		fail("endpoint", "", "refused session.list: "+resp.AsError().Error(), "check the endpoint's stderr, then restart serve")
	default:
		_ = json.Unmarshal(resp.Reply, &sessions)
		report["endpoint"] = "reachable"
		report["targets"] = len(sessions)
	}
	doctorRequestStore(stateDir, report, fail)
	// Print adapter refusals persisted by serve, so doctor shows them.
	refusals, loadErr := loadRefusals(stateDir)
	switch {
	case loadErr == nil && len(refusals) > 0:
		report["refusals"] = refusals
		for _, r := range refusals {
			fail("registration", r.Target, r.Kind+": "+r.Error, "fix the manifest entry for "+r.Target+", then restart serve")
		}
	case loadErr != nil && !errors.Is(loadErr, os.ErrNotExist):
		// A truncated or unreadable refusals.json must not be dropped
		// silently: doctor reports it and does not exit 0.
		report["refusals_error"] = loadErr.Error()
		fail("registration", "refusals.json", loadErr.Error(), "restart serve; it rewrites refusals.json on every start")
	}
	// Native capability: a Claude target completes a request only through
	// the Stop hook.
	for _, s := range sessions {
		if s.Harness != "claude_code" {
			continue
		}
		home, herr := os.UserHomeDir()
		state, serr := "", herr
		if herr == nil {
			state, serr = claude.StopHookState(home)
		}
		switch {
		case serr != nil:
			fail("native_capability", s.TargetID, "cannot read ~/.claude/settings.json: "+serr.Error(), "repair ~/.claude/settings.json")
		case state == claude.StopHookMissing:
			fail("native_capability", s.TargetID, "the Claude Stop hook is not installed, so requests are admitted but never complete", "run `amq-remote claude install-stop-hook`")
		case state == claude.StopHookDisabled:
			// Only the user file was read; a project or managed setting can
			// override disableAllHooks, so the effective state is unknown and
			// is reported, not failed.
			report["claude_stop_hook"] = "~/.claude/settings.json holds the Stop hook and sets disableAllHooks; project or managed settings decide whether it runs"
		}
		break
	}
	// A Codex target with approve whose thread runs Codex's automatic
	// approvals reviewer gets few or no DM approvals: the reviewer answers
	// them first. That is the owner's choice, so it is reported,
	// not failed.
	reviewers := map[string]string{}
	for _, s := range sessions {
		if s.Harness == "codex" && s.Capabilities.ApproveTool && s.ApprovalReviewer != "" {
			reviewers[s.TargetID] = "Codex's approvals reviewer " + s.ApprovalReviewer + " answers this thread's approvals itself, so few or none reach a remote client; for approvals from Buzz, start a new Codex thread with approvals_reviewer = \"user\" (config.toml or -c)"
		}
	}
	if len(reviewers) > 0 {
		report["approval_reviewer"] = reviewers
	}
	// DM approvals of a Claude target with approve need the PermissionRequest
	// hook; without it every approval is answered in the terminal. A target
	// with observe_approvals needs it too: the hook raises the approvals it
	// shows.
	observing := observeOnlyTargets(stateDir)
	for _, s := range sessions {
		if s.Harness != "claude_code" || !s.Capabilities.ApproveTool && !observing[s.TargetID] {
			continue
		}
		home, herr := os.UserHomeDir()
		state, serr := "", herr
		if herr == nil {
			state, serr = claude.PermissionHookState(home)
		}
		switch {
		case serr != nil:
			fail("native_capability", s.TargetID, "cannot read ~/.claude/settings.json: "+serr.Error(), "repair ~/.claude/settings.json")
		case state == claude.StopHookMissing && !s.Capabilities.ApproveTool:
			fail("native_capability", s.TargetID, "the PermissionRequest hook is not installed, so a shared source never sees a tool approval", "run `amq-remote claude install-approval-hook`")
		case state == claude.StopHookMissing:
			fail("native_capability", s.TargetID, "the PermissionRequest hook is not installed, so Buzz cannot block a tool call; the terminal decides every approval", "run `amq-remote claude install-approval-hook`")
		case state == claude.StopHookDisabled:
			report["claude_approval_hook"] = "~/.claude/settings.json holds the PermissionRequest hook and sets disableAllHooks; project or managed settings decide whether it runs"
		}
		// Allow from Buzz needs the owner pinned on the hook's command line,
		// and the pin holds only while Claude cannot change it unprompted.
		if serr == nil && state != claude.StopHookMissing && s.Capabilities.ApproveTool {
			if pin := claude.PermissionHookPin(home); pin.DesktopComplete() {
				report["claude_approval_pin"] = map[string]string{"surface": pin.Surface, "owner": pin.Owner, "agent": pin.Body, "channel": pin.Channel}
			} else if !pin.Complete() {
				report["claude_approval_pin"] = "the PermissionRequest hook pins no complete owner and share, so Buzz can only deny a tool call and allow stays in the terminal; for a relay share, run `amq-remote claude install-approval-hook --owner <pubkey>` to allow from Buzz; for Buzz Desktop, run the install-approval-hook --desktop command the agent's approval message shows"
			} else {
				report["claude_approval_pin"] = map[string]string{"owner": pin.Owner, "session": pin.Session, "relay": pin.Relay, "channel": pin.Channel, "target": pin.Target}
			}
			if warns, werr := claude.PinWarnings(home); werr == nil && len(warns) > 0 {
				report["claude_approval_pin_warnings"] = warns
			}
		}
		break
	}
	// Body identity: per-session body keys, attestations, expiry
	// warnings (7-day horizon enforced inside the inspection). Absent keys
	// dir reports nothing — sessions without remote sharing are normal.
	doctorBodyKeys(c.root, report, fail)
	// Relay connections, as serve last reported them. Each state
	// is distinct: auth_pending is not authenticated, and authenticated is
	// not proof the relay materialized ownership.
	doctorRelay(stateDir, report, fail, note)
	// Stale harness (bbn): a Buzz agent can keep running an amq-acp that an
	// upgrade replaced, so it answers from the removed binding contract.
	// A different identity can also be a byte-identical copy or another
	// binary with the same name, so this is advice, not a failure: it lands
	// in notes and never makes doctor exit non-zero.
	noteStaleACP(note)
	noteNoService(note, c.root, stateDir)
	if len(notes) > 0 {
		report["notes"] = notes
	}
	return finish()
}

// doctorRequestStore counts the request records and fails each one that
// cannot be read. It reads the store as admission does: an unreadable
// record makes its target's reservation state unknown, so admission
// refuses every submit to that target until the record is repaired.
func doctorRequestStore(stateDir string, report map[string]any, fail func(boundary, subject, detail, remedy string)) {
	store, err := requests.OpenReadOnly(stateDir)
	var refusal *protocol.Refusal
	if errors.As(err, &refusal) && refusal.Code == protocol.CodeNotFound {
		return // no request was ever stored
	}
	if err != nil {
		fail("request_store", "", "cannot open the request store: "+err.Error(), "check the permissions of "+filepath.Join(stateDir, "v1"))
		return
	}
	recs, poison, err := store.ListWithPoison()
	if err != nil {
		report["records_error"] = err.Error()
		fail("request_store", "", "cannot list the request store: "+err.Error(), "check the permissions of "+filepath.Join(stateDir, "v1"))
		return
	}
	counts := map[string]int{}
	for _, r := range recs {
		counts[string(r.State)]++
	}
	report["records"] = counts
	for _, p := range poison {
		subject, refused := p.Key.TargetID, "submits to "+p.Key.TargetID+" are refused"
		if subject == "" {
			subject, refused = p.Path, "submits to every target are refused"
		}
		fail("request_store", subject, fmt.Sprintf("request record %s is unreadable (%s); %s while it stays unreadable", p.Path, p.Error, refused),
			"repair or restore the file by hand; a restart does not repair it. Do not delete it: it may be the only record that stops its request from running twice")
	}
}

// doctorBodyKeys reports the session body keys and fails each share whose
// key, attestation or grants cannot authenticate.
func doctorBodyKeys(root string, report map[string]any, fail func(boundary, subject, detail, remedy string)) {
	if bodyKeys := doctorShareInspection(root); bodyKeys != nil {
		report["body_keys"] = bodyKeys
		sessionsWithKeys := make([]string, 0, len(bodyKeys))
		for name := range bodyKeys {
			sessionsWithKeys = append(sessionsWithKeys, name)
		}
		sort.Strings(sessionsWithKeys)
		for _, name := range sessionsWithKeys {
			info, _ := bodyKeys[name].(map[string]any)
			remedy, _ := info["remedy"].(string)
			if remedy == "" {
				remedy = "run `amq-remote share --session " + name + "` and follow its output"
			}
			for _, key := range []string{"attestation_error", "key_error", "body_pub_error", "staged_error"} {
				if msg, ok := info[key].(string); ok {
					fail("body_key", name, msg, remedy)
				}
			}
			// A body with no enrolled generation, or only a pending one,
			// cannot authenticate: that is a failing share.
			if att, ok := info["attestation"].(string); ok {
				switch {
				case strings.HasPrefix(att, "missing"):
					fail("body_key", name, "no owner-signed generation is enrolled", "run `amq-remote share --session "+name+"` and have the owner sign the printed preimages")
				case strings.HasPrefix(att, "pending"):
					fail("body_key", name, att, "have the owner sign the printed preimages, then run `amq-remote share --session "+name+"`")
				}
			}
			if msg, ok := info["expiry_warning"].(string); ok && strings.HasPrefix(msg, "expired") {
				fail("tag_expiry", name, "the enrolled grants have expired", strings.TrimPrefix(msg, "expired: "))
			}
		}
	}
}

// doctorRelay reports the relay connections and fails each share whose
// relay, DM, publication, presence, activity or discovery boundary is broken.
func doctorRelay(stateDir string, report map[string]any, fail, note func(boundary, subject, detail, remedy string)) {
	switch rs, rerr := loadRelayStatus(stateDir); {
	case rerr == nil:
		report["relay"] = rs
		for _, sh := range rs.Shares {
			if sh.State != string(relay.StateAuthenticated) {
				fail("relay_auth", sh.Session, sh.State+errSuffix(sh.Error), "check the relay URL and the share's enrollment (`amq-remote share --session "+sh.Session+"`)")
			}
			// A commands share also needs its DM surface open.
			switch {
			case sh.Commands == "" || sh.Commands == "subscription_active":
			case strings.HasPrefix(sh.Commands, "publish_pending"):
				fail("publication", sh.Session, sh.Commands, "output stays owed and is resent; check the relay")
			default:
				fail("dm_surface", sh.Session, sh.Commands, "check the DM channel membership, the native_session_id pin and the buzz-dm enrollment")
			}
			// A presence share needs a published status and the owner's
			// 30177 policy, or Desktop cannot list the body as owned.
			if sh.Presence != "" && sh.Presence != "online" && sh.Presence != "away" {
				fail("presence", sh.Session, sh.Presence, "enroll the profile kind with `amq-remote share --session "+sh.Session+" --enable buzz-profile`")
			}
			if sh.Activity != "" && sh.Activity != "exporting" {
				fail("activity", sh.Session, sh.Activity, "check the native_session_id pin and that the target is an attached Codex thread")
			}
			if sh.Discovery != "" && sh.Discovery != "policy_present" {
				fail("discovery", sh.Session, sh.Discovery, "the owner publishes the kind 30177 policy for this body from their Buzz client")
			}
			noteRefusedRows(note, stateDir, sh.Session)
			failDamagedRows(fail, stateDir, sh.Session)
		}
	case !errors.Is(rerr, os.ErrNotExist):
		report["relay_error"] = rerr.Error()
		fail("relay_auth", "relay-status.json", rerr.Error(), "restart serve; it rewrites relay-status.json")
	}
}

// maxRefusedNotes bounds the refused rows doctor lists per share.
const maxRefusedNotes = 5

// noteRefusedRows names the share's owed outputs the relay refused on
// their last attempt, with the relay's reason. They stay owed and are
// retried, and other requests' outputs still go out, so they are advice, not
// a failure.
func noteRefusedRows(note func(boundary, subject, detail, remedy string), stateDir, session string) {
	refused, err := buzzio.RefusedOutputs(filepath.Join(stateDir, "shares", session))
	if err != nil {
		note("publication_refused", session, "cannot read the outbox: "+err.Error(), "")
		return
	}
	for i, o := range refused {
		if i == maxRefusedNotes {
			note("publication_refused", session, fmt.Sprintf("%d more refused outputs in the outbox", len(refused)-i), "each stays owed and is retried")
			return
		}
		r := o.Refused
		detail := fmt.Sprintf("%s: the relay refused %d attempt(s), last with %q; next try %s", o.Key, r.Attempts, r.Reason, time.Unix(r.NextTry, 0).UTC().Format(time.RFC3339))
		see := "check the session in the terminal"
		if rest, ok := strings.CutPrefix(o.Key, "row/"); ok {
			ref, _, _ := strings.Cut(rest, "/")
			see = "see the request's state with `amq-remote status " + ref + "` or in the terminal"
		}
		note("publication_refused", session, detail, "the output stays owed and is retried; "+see+". Do not send the message again from Buzz: that would run the request a second time")
	}
}

// failDamagedRows fails each outbox record of the share that cannot be
// read. The other outputs still go out, but the damaged one is never sent,
// and whether it was already accepted cannot be read back.
func failDamagedRows(fail func(boundary, subject, detail, remedy string), stateDir, session string) {
	damaged, err := buzzio.DamagedOutputs(filepath.Join(stateDir, "shares", session))
	if err != nil {
		return // noteRefusedRows already reports an unreadable outbox
	}
	for _, d := range damaged {
		fail("publication", session, fmt.Sprintf("outbox record %s is unreadable (%s); the output it holds is never sent", d.Path, d.Error),
			"check the session's requests with `amq-remote requests`, then repair or restore the file by hand, or move it out of the outbox once its output is not owed; a restart does not repair it")
	}
}

// amqRouteDetail reports why other agents cannot route to the endpoint
// handle. serve registers that handle in config.json; an empty result means
// the handle is listed.
func amqRouteDetail(root, handle string) string {
	cfg, err := config.LoadConfig(filepath.Join(root, "meta", "config.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return "config.json does not exist, so no agent can route to " + handle
		}
		return "cannot read config.json: " + err.Error()
	}
	for _, agent := range cfg.Agents {
		if agent == handle {
			return ""
		}
	}
	return fmt.Sprintf("handle %q is not in config.json agents", handle)
}

// boundaryFailure is one broken boundary doctor names, with what to do.
type boundaryFailure struct {
	Boundary string `json:"boundary"`
	Subject  string `json:"subject,omitempty"`
	Detail   string `json:"detail"`
	Remedy   string `json:"remedy,omitempty"`
}

func errSuffix(e string) string {
	if e == "" {
		return ""
	}
	return ": " + e
}
