package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/thread"
)

const threadSettleSchema = "amq/thread-settle/v1"

type threadSettleGroup struct {
	Thread       string
	OwnerProject string
	OwnerSession string
	OwnerHandle  string
	Anchor       *thread.Obligation
	Openers      []*thread.Obligation
}

type threadSettleResult struct {
	Schema           string                  `json:"schema"`
	GeneratedAt      string                  `json:"generated_at"`
	DryRun           bool                    `json:"dry_run"`
	OlderThanSeconds int64                   `json:"older_than_seconds"`
	Settled          []threadSettleEntryJSON `json:"settled"`
	Kept             int                     `json:"kept"`
}

type threadSettleEntryJSON struct {
	Thread    string               `json:"thread"`
	OwedBy    *reportParticipant   `json:"owed_by"`
	Through   threadOpenObligation `json:"through"`
	Count     int                  `json:"count"`
	OpenerIDs []string             `json:"opener_ids"`
	MessageID string               `json:"message_id"`
}

// runThreadSettle implements `amq thread settle`: the requester sends one
// decision per (thread, owner) named by SettledThrough, closing its own open
// requests to that owner in that thread that are at least --older-than old.
func runThreadSettle(args []string) error {
	fs := flag.NewFlagSet("thread settle", flag.ContinueOnError)
	common := addCommonFlags(fs)
	olderThanFlag := fs.String("older-than", "", "")
	owedByFlag := fs.String("owed-by", "", "Restrict to these owner handles")
	threadFlag := fs.String("thread", "", "Restrict to one thread id")
	dryRunFlag := fs.Bool("dry-run", false, "Report but do not send markers")
	jsonSchemaFlag := fs.Int("json-schema", 0, "JSON schema 1")
	// --me is provided by addCommonFlags.
	fsUsage := usageWithFlags(fs, "amq thread settle --older-than <dur> [--owed-by <handles>] [--thread <id>] [--dry-run] [--json] [--me <h>] [--root <path>]",
		"Send one decision labelled settled-through from me to each owner of my open requests older than the bound, per (thread, owner). Only the requester of those obligations can settle them. Owners in another session are not settled. A follow-up timer on the anchor cancels with the marker; a timer on an older settled request may still fire one true notice.")
	if handled, err := parseFlags(fs, args, fsUsage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if *jsonSchemaFlag != 0 && !common.JSON {
		return UsageError("--json-schema requires --json")
	}
	if *jsonSchemaFlag != 0 && *jsonSchemaFlag != 1 {
		return UsageError("--json-schema must be 1")
	}
	if strings.TrimSpace(*olderThanFlag) == "" {
		return UsageError("--older-than is required; 0 means every open request")
	}
	bound, err := parseMaxAge(*olderThanFlag)
	if err != nil {
		return UsageError("--older-than: %v", err)
	}
	allowedOwners := map[string]bool{}
	if *owedByFlag != "" {
		parsed, err := parseHandles(*owedByFlag)
		if err != nil {
			return UsageError("--owed-by: %v", err)
		}
		for _, h := range parsed {
			allowedOwners[h] = true
		}
	}
	var threadMatch = func(t string) bool { return true }
	if *threadFlag != "" {
		threadMatch = func(t string) bool { return t == *threadFlag }
	}
	me := common.Me
	root := absPath(resolveRoot(common.Root))
	if !dirExists(root) {
		return NotFoundError("root not found at %s", root)
	}
	scanRoots := []thread.ScanRoot{threadOpenScanRoot(root)}
	// The caller as a participant of the scanned root. A request is the
	// caller's only when its requester is this whole value: an agent of
	// another project or session with the same handle is someone else.
	mine := thread.Participant{Session: scanRoots[0].Session, Handle: me}
	now := time.Now().UTC()
	// Same window as `thread open` uses on this root (336h cutoff).
	_, red, err := reduceObligations(scanRoots, now.Add(-336*time.Hour), func(path string, _ error) error { return nil })
	if err != nil {
		return err
	}
	identity, err := fsq.SnapshotDeliveryRoot(root)
	if err != nil {
		return err
	}
	deliveryFS, err := fsq.OpenDeliveryRoot(root, identity)
	if err != nil {
		return err
	}
	defer func() { _ = deliveryFS.Close() }()
	pruneOpenRows := func(ob *thread.Obligation) bool {
		if !ob.Open() {
			return false
		}
		// The marker is delivered into this root, so an owner outside it
		// is not reached and its rows are not selected.
		if ob.Requester != mine || ob.Owner.Project != mine.Project || ob.Owner.Session != mine.Session || !threadMatch(ob.Opener.Thread) {
			return false
		}
		if len(allowedOwners) > 0 && !allowedOwners[ob.Owner.Handle] {
			return false
		}
		return true
	}
	// One pass: bucket matching rows by (thread, owner) and count the kept
	// ones (newer than the bound).
	groups := map[string]*threadSettleGroup{}
	var result threadSettleResult
	result.Schema = threadSettleSchema
	result.GeneratedAt = now.Format(time.RFC3339)
	result.DryRun = *dryRunFlag
	result.OlderThanSeconds = int64(bound / time.Second)
	result.Settled = []threadSettleEntryJSON{}
	for _, ob := range red.Obligations {
		if !pruneOpenRows(ob) {
			continue
		}
		if bound > 0 && now.Sub(ob.Opener.RawTime) < bound {
			result.Kept++
			continue
		}
		key := ob.Opener.Thread + "\x00" + ob.Owner.Project + "\x00" + ob.Owner.Session + "\x00" + ob.Owner.Handle
		if groups[key] == nil {
			groups[key] = &threadSettleGroup{
				Thread:       ob.Opener.Thread,
				OwnerProject: ob.Owner.Project,
				OwnerSession: ob.Owner.Session,
				OwnerHandle:  ob.Owner.Handle,
			}
		}
		g := groups[key]
		if g.Anchor == nil || lessEntries(g.Anchor.Opener, ob.Opener) {
			g.Anchor = ob
		}
		g.Openers = append(g.Openers, ob)
	}
	sortingKeys := make([]string, 0, len(groups))
	for key := range groups {
		sortingKeys = append(sortingKeys, key)
	}
	sort.Strings(sortingKeys)
	var firstErr error
	for _, key := range sortingKeys {
		g := groups[key]
		anc := g.Anchor
		// `anc.Opener` is the newest row at least the bound; that's the anchor.
		openerIDs := make([]string, 0, len(g.Openers))
		slices.SortFunc(g.Openers, func(a, b *thread.Obligation) int {
			if lessEntries(a.Opener, b.Opener) {
				return -1
			}
			if lessEntries(b.Opener, a.Opener) {
				return 1
			}
			return 0
		})
		for _, ob := range g.Openers {
			openerIDs = append(openerIDs, ob.Opener.ID)
		}
		subject := fmt.Sprintf("Settled: %d %s through %q", len(g.Openers), pluralRequest(len(g.Openers)), anc.Opener.Subject)
		body := fmt.Sprintf("These requests from %s need nothing more from you; each one in this thread up to and including %s is closed.\n", me, anc.Opener.ID)
		for _, ob := range g.Openers {
			body += fmt.Sprintf("%s  %s  %s\n", ob.Opener.ID, ob.Opener.Kind, ob.Opener.Subject)
		}
		to := participantLabel(participant{Session: g.OwnerSession, Handle: g.OwnerHandle})
		text := fmt.Sprintf("settled %d %s to %s in %s through %s (%q) as %%s",
			len(g.Openers), pluralRequest(len(g.Openers)), to, anc.Opener.Thread, anc.Opener.ID, anc.Opener.Subject)
		if *dryRunFlag {
			if !common.JSON {
				_ = writeStdout("would settle %d %s to %s in %s through %s (%q)\n",
					len(g.Openers), pluralRequest(len(g.Openers)), to, anc.Opener.Thread, anc.Opener.ID, anc.Opener.Subject)
			}
			result.Settled = append(result.Settled, threadSettleEntryJSON{
				Thread:    g.Thread,
				OwedBy:    &reportParticipant{Project: g.OwnerProject, Session: g.OwnerSession, Handle: g.OwnerHandle},
				Through:   obligationJSON(anc, time.Now()),
				Count:     len(g.Openers),
				OpenerIDs: openerIDs,
				MessageID: "",
			})
			continue
		}
		msgID := ""
		err := deliverOutgoing(outgoingMessage{
			deliveryFS:   deliveryFS,
			sourceFS:     deliveryFS,
			deliveryRoot: root,
			verb:         "settled",
			me:           me,
			recipients:   []string{anc.Owner.Handle},
			thread:       anc.Opener.Thread,
			subject:      subject,
			body:         body,
			refs:         append(append([]string{}, anc.Opener.Refs...), anc.Opener.ID),
			kind:         format.KindDecision,
			priority:     format.PriorityLow,
			labels:       []string{thread.SettledThrough},
			outID:        &msgID,
			silent:       true,
		})

		result.Settled = append(result.Settled, threadSettleEntryJSON{
			Thread:    g.Thread,
			OwedBy:    &reportParticipant{Project: g.OwnerProject, Session: g.OwnerSession, Handle: g.OwnerHandle},
			Through:   obligationJSON(anc, time.Now()),
			Count:     len(g.Openers),
			OpenerIDs: openerIDs,
			MessageID: msgID,
		})
		if err != nil {
			_ = writeStderr("settle %s: %v\n", text, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if !common.JSON {
			_ = writeStdout("%s\n", fmt.Sprintf(text, msgID))
		}
	}
	if len(result.Settled) == 0 && firstErr == nil {
		if !common.JSON {
			_ = writeStdout("Nothing to settle.\n")
		}
	}
	if common.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(result); err != nil {
			if firstErr != nil {
				return errors.Join(firstErr, err)
			}
			return err
		}
		return firstErr
	}
	if result.Kept > 0 {
		_ = writeStdout("kept: %d %s newer than %s\n", result.Kept, pluralRequest(result.Kept), *olderThanFlag)
	}
	return firstErr
}

// pluralRequest is `request`/`requests` for the printed case lines.
func pluralRequest(n int) string {
	if n == 1 {
		return "request"
	}
	return "requests"
}

// lessEntries is the (created, id) ordering planner used by Reduction.
func lessEntries(a, b *thread.Entry) bool {
	if !a.RawTime.Equal(b.RawTime) {
		return a.RawTime.Before(b.RawTime)
	}
	return a.ID < b.ID
}

var _ = errors.Is // used where errors.As wraps callers

// reportParticipant mirrors thread.Open's participant shape (only fields we print).
type reportParticipant struct {
	Project string `json:"project,omitempty"`
	Session string `json:"session"`
	Handle  string `json:"handle"`
}
