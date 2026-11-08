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

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/relay"
	"github.com/avivsinai/agent-message-queue/internal/remote/amqio"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/buzzio"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"github.com/avivsinai/agent-message-queue/internal/remote/ipc"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/requests"
	"github.com/avivsinai/agent-message-queue/internal/roster"
	"github.com/avivsinai/agent-message-queue/internal/route"
)

func doctor(args []string, probe ...*jsonProbe) (any, int, error) {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := addCommon(fs, probe...)
	me := fs.String("me", amqio.DefaultHandle, "endpoint mailbox handle in the root")
	fix := fs.Bool("fix", false, "repair what doctor can repair (hooks, the machine agent's import file, the login service), then check")
	if err := fs.Parse(args); err != nil {
		return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "%v", err)
	}
	if err := fsq.ValidateHandle(*me); err != nil {
		return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "invalid handle %q: %v", *me, err)
	}
	if c.root == "" {
		// The onboarding shell has no AM_ROOT: check the machine root, the
		// one the login service and native sessions use.
		root, err := binding.MachineRoot()
		if err != nil {
			return nil, protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "--root or AM_ROOT is required: %v", err)
		}
		c.root = root
	}
	stateDir, err := c.stateDir()
	if err != nil {
		return nil, protocol.ExitUsage, err
	}
	report := map[string]any{"root": c.root, "state_dir": stateDir, "socket": ipc.SocketPath(stateDir)}
	// failing names each broken link between the owner and a session:
	// hooks, machine agent, channel, mailbox, stale harness, AMQ route,
	// endpoint, registration, body key, tag expiry, relay auth, DM surface,
	// publication, presence, discovery.
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
	all, err := binding.List()
	if err != nil {
		fail("bindings", "", "cannot list the bindings: "+err.Error(), "check the permissions of the AMQ home's remote directory")
	}
	if *fix {
		report["fixed"] = doctorFix(c.root, stateDir, all, fail)
	}
	doctorMachine(stateDir, all, report, fail, note)
	// A Buzz agent can keep running an amq-acp that an upgrade replaced, so
	// it answers from the removed binding contract.
	noteStaleACP(fail, note)
	if _, err := os.Stat(stateDir); err != nil {
		report["endpoint"] = "no state directory; run `amq-remote serve` once"
		// Only a native session needs the endpoint: a mailbox binding
		// delivers through the AMQ queue.
		if nativeThrough(all, c.root) {
			fail("endpoint", "", "no state directory", "run `amq-remote serve` once")
		} else {
			note("endpoint", "", "no state directory: no native session is attached through this root", "a native session (/amq-remote --native) starts the endpoint")
		}
		if len(notes) > 0 {
			report["notes"] = notes
		}
		return finish()
	}
	doctorAMQRoute(c.root, *me, fail, note)
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
	// A native_capability row repeats a hooks row of the same file for
	// older clients; the text output prints that link once.
	repeat := func(boundary, subject, detail, remedy string) {
		failing = append(failing, boundaryFailure{Boundary: boundary, Subject: subject, Detail: detail, Remedy: remedy, repeat: true})
	}
	hooksFailed := func(settings string) bool {
		for _, f := range failing {
			if f.Boundary == "hooks" && f.Subject != "" && canonicalDir(filepath.Dir(f.Subject)) == canonicalDir(filepath.Dir(settings)) {
				return true
			}
		}
		return false
	}
	doctorNativeCapability(stateDir, sessions, report, fail, repeat, hooksFailed)
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
	// A Codex thread that runs Codex's automatic approvals reviewer shows
	// few or no dialogs in Buzz: the reviewer answers them first. That is
	// the owner's choice, so it is reported, not failed.
	reviewers := map[string]string{}
	for _, s := range sessions {
		if s.Harness == "codex" && s.ApprovalReviewer != "" {
			reviewers[s.TargetID] = "Codex's approvals reviewer " + s.ApprovalReviewer + " answers this thread's approvals itself, so few or none reach a remote client; for approvals from Buzz, start a new Codex thread with approvals_reviewer = \"user\" (config.toml or -c)"
		}
	}
	if len(reviewers) > 0 {
		report["approval_reviewer"] = reviewers
	}
	// Body identity: per-session body keys, attestations, expiry
	// warnings (7-day horizon enforced inside the inspection). Absent keys
	// dir reports nothing — sessions without remote sharing are normal.
	doctorBodyKeys(c.root, report, fail)
	// Relay connections, as serve last reported them. Each state
	// is distinct: auth_pending is not authenticated, and authenticated is
	// not proof the relay materialized ownership.
	doctorRelay(stateDir, report, fail, note)
	noteNoService(note, c.root, stateDir)
	if len(notes) > 0 {
		report["notes"] = notes
	}
	return finish()
}

// hookFormFinding reports an installed Claude hook that is not the form its
// installer writes now. An ungated hook still works and costs an amq-remote
// start at every event in every session, so it is advice. A stale gate can
// skip a state the receiver reads, so it fails.
func hookFormFinding(form, subject, hook, remedy string, fail, note reportFunc) {
	switch form {
	case claude.HookFormUngated:
		note("hook_form", subject, "the "+hook+" hook is an older form that starts amq-remote in every Claude session on this machine", remedy)
	case claude.HookFormStale:
		fail("hooks", subject, "the "+hook+" hook's gate is stale: it names other paths than this machine resolves now, or an older timeout, so it can skip the state it guards", remedy)
	}
}

// doctorNativeCapability keeps the per-target rows and keys a client of
// an older doctor reads (native_capability, claude_stop_hook,
// claude_approval_hook) next to the hooks rows: a Claude target completes
// a request only through the Stop hook, and Buzz shows a dialog only
// through the PermissionRequest hook.
func doctorNativeCapability(stateDir string, sessions []protocol.Session, report map[string]any, failNew, repeat reportFunc, hooksFailed func(settings string) bool) {
	settingsOf := claudeTargetSettings(stateDir)
	type hookRow struct {
		state     func(string) (string, error)
		form      func(home, settings string) (string, error)
		key       string
		installer string
		missing   func(protocol.Session) string
	}
	rows := []hookRow{
		{claude.StopHookStateAt, claude.StopHookForm, "claude_stop_hook", "install-stop-hook", func(protocol.Session) string {
			return "the Claude Stop hook is not installed, so requests are admitted but never complete"
		}},
		{claude.PermissionHookState, claude.PermissionHookForm, "claude_approval_hook", "install-approval-hook", func(s protocol.Session) string {
			if !s.Capabilities.ApproveTool {
				return "the PermissionRequest hook is not installed, so a shared source never sees a tool approval"
			}
			return "the PermissionRequest hook is not installed, so Buzz cannot block a tool call; the terminal decides every approval"
		}},
	}
	for _, row := range rows {
		checked := map[string]bool{}
		for _, s := range sessions {
			if s.Harness != "claude_code" {
				continue
			}
			home, settings, err := settingsOf.lookup(s.TargetID)
			if err == nil && checked[settings] {
				continue
			}
			checked[settings] = true
			fail := failNew
			if err == nil && hooksFailed(settings) {
				fail = repeat
			}
			state := ""
			if err == nil {
				state, err = row.state(settings)
			}
			switch {
			case err != nil:
				fail("native_capability", s.TargetID, "cannot read the Claude user settings.json: "+err.Error(), "repair "+settings)
			case state == claude.HookMissing:
				fail("native_capability", s.TargetID, row.missing(s), "run `amq-remote claude "+row.installer+"`")
			case state == claude.HookDisabled:
				report[row.key] = settings + " holds the hook and sets disableAllHooks; project or managed settings decide whether it runs"
			}
			if err == nil && state != claude.HookMissing {
				if form, _ := row.form(home, settings); form == claude.HookFormStale {
					fail("native_capability", s.TargetID, "the hook's gate is stale: it names other paths than this machine resolves now, or an older timeout, so it can skip the state it guards", "run `amq-remote claude "+row.installer+"`")
				}
			}
		}
	}
}

// nativeThrough reports whether a native binding names root, so its
// session needs root's endpoint.
func nativeThrough(all []binding.Binding, root string) bool {
	for _, b := range all {
		if !b.Mailbox() && sameRoot(b.Root, root) {
			return true
		}
	}
	return false
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

// doctorAMQRoute reports how senders see the endpoint handle. serve
// registers that handle in config.json. A handle no readable roster declares
// fails: unregistered, or known only from its identity record while a roster
// cannot be read, senders are warned and --strict sends refuse. A declared
// handle whose registration still has diagnostics (an invalid identity
// record) routes, so each diagnostic is a note.
func doctorAMQRoute(root, handle string, fail, note func(boundary, subject, detail, remedy string)) {
	status, diagnostics := roster.AgentRegistration(route.Walker{}, root, handle)
	const remedy = "run `amq-remote serve` once so it registers this handle in config.json"
	switch {
	case status == "unregistered":
		detail := fmt.Sprintf("handle %q is unregistered: no roster of this root or its base lists it and its mailbox has no valid identity record, so senders are warned and --strict sends refuse", handle)
		if len(diagnostics) > 0 {
			detail += " (" + strings.Join(diagnostics, "; ") + ")"
		}
		fail("amq_route", handle, detail, remedy)
	case status != "declared" && len(diagnostics) > 0:
		fail("amq_route", handle, fmt.Sprintf("handle %q is %s, but no readable roster declares it, so senders cannot classify it and --strict sends refuse (%s)", handle, status, strings.Join(diagnostics, "; ")),
			"repair the roster named above, then "+remedy)
	default:
		for _, diagnostic := range diagnostics {
			note("amq_route", handle, fmt.Sprintf("handle %q is %s; its registration has a diagnostic: %s", handle, status, diagnostic), "")
		}
	}
}

// doctorReport is the doctor report; JSON prints it as it is, and text
// prints one line per failing link and per note.
type doctorReport map[string]any

// printDoctor prints each failing link as `FAIL <link> <subject>: <detail>;
// fix: <remedy>`, then each note the same way with NOTE, or one OK line when
// nothing fails.
func printDoctor(w io.Writer, r doctorReport) {
	line := func(kind string, f boundaryFailure) {
		head := kind + " " + f.Boundary
		if f.Subject != "" {
			head += " " + f.Subject
		}
		text := head + ": " + f.Detail
		if f.Remedy != "" {
			text += "; fix: " + f.Remedy
		}
		say(w, "%s", text)
	}
	fixed, _ := r["fixed"].([]string)
	for _, f := range fixed {
		say(w, "FIXED %s", f)
	}
	failing, _ := r["failing"].([]boundaryFailure)
	for _, f := range failing {
		if !f.repeat {
			line("FAIL", f)
		}
	}
	notes, _ := r["notes"].([]boundaryFailure)
	for _, n := range notes {
		line("NOTE", n)
	}
	// The fields that are reported, not failed, one line each.
	keys := make([]string, 0, len(r))
	for k := range r {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch v := r[k].(type) {
		case string:
			say(w, "INFO %s: %s", k, v)
		case map[string]string:
			subjects := make([]string, 0, len(v))
			for s := range v {
				subjects = append(subjects, s)
			}
			sort.Strings(subjects)
			for _, s := range subjects {
				say(w, "INFO %s %s: %s", k, s, v[s])
			}
		default:
			switch k {
			case "failing", "notes", "fixed", "hooks", "machine_agent", "channels", "mailboxes":
				continue
			}
			if raw, err := json.Marshal(v); err == nil {
				say(w, "INFO %s: %s", k, raw)
			}
		}
	}
	if len(failing) == 0 {
		say(w, "OK: no failing link")
	}
}

// boundaryFailure is one broken boundary doctor names, with what to do.
type boundaryFailure struct {
	Boundary string `json:"boundary"`
	Subject  string `json:"subject,omitempty"`
	Detail   string `json:"detail"`
	Remedy   string `json:"remedy,omitempty"`
	// repeat marks a row that repeats another link's row for older JSON
	// clients; the text output leaves it out.
	repeat bool
}

func errSuffix(e string) string {
	if e == "" {
		return ""
	}
	return ": " + e
}

// claudeTargets is the Claude entries of a manifest and a lookup of each
// one's home and the settings.json of its session: the entry's recorded
// config_dir, else home/.claude. Like the endpoint, it never reads
// CLAUDE_CONFIG_DIR.
type claudeTargets struct {
	targets []string
	configs map[string]json.RawMessage
}

func claudeTargetSettings(stateDir string) claudeTargets {
	t := claudeTargets{configs: map[string]json.RawMessage{}}
	if mf, err := manifest.Load(manifest.DefaultPath(stateDir)); err == nil {
		for _, ad := range mf.Adapters {
			if ad.Kind == "claude" {
				t.targets = append(t.targets, ad.Target)
				t.configs[ad.Target] = ad.Config
			}
		}
	}
	return t
}

func (t claudeTargets) lookup(target string) (home, settings string, err error) {
	home, err = os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	settings, err = claude.TargetSettingsPath(t.configs[target])
	return home, settings, err
}
