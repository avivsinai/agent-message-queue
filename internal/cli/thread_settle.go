package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
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
	// A shared delivery root is opened for the whole command; the deliverer
	// does not open it for me. `deliverOutgoing` treats the struct's delivery
	// fs as owned here, so this command holds it once and closes once.
	// The reduction reads the whole mailbox. The age bound is applied
	// afterwards when selecting the open rows closer to now than --older-than;
	// reducing with that cutoff would skip the very obligations settle finds.
	_, red, err := reduceObligations(scanRoots, time.Time{}, func(path string, _ error) error { return nil })
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
	// Group open obligations where I am the requester, by thread + owner.
	groups := map[string]*threadSettleGroup{}
	var kept int
	for _, ob := range red.Obligations {
		if !ob.Open() {
			continue
		}
		if ob.Requester.Handle != me || !threadMatch(ob.Opener.Thread) {
			continue
		}
		if len(allowedOwners) > 0 && !allowedOwners[ob.Owner.Handle] {
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
			kept++
		}
	}
	want := map[string]bool{}
	for _, ob := range red.Obligations {
		if !ob.Open() {
			continue
		}
		if ob.Requester.Handle != me || !threadMatch(ob.Opener.Thread) {
			continue
		}
		if len(allowedOwners) > 0 && !allowedOwners[ob.Owner.Handle] {
			continue
		}
		key := ob.Opener.Thread + "\x00" + ob.Owner.Project + "\x00" + ob.Owner.Session + "\x00" + ob.Owner.Handle
		g := groups[key]
		if g == nil {
			continue
		}
		if bound > 0 && time.Since(ob.Opener.RawTime) < bound {
			continue
		}
		g.Openers = append(g.Openers, ob)
		want[key] = true
	}
	// Any group without an anchor (no old obligations) drops out.
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var result threadSettleResult
	result.Schema = threadSettleSchema
	result.GeneratedAt = time.Now().UTC().Format(time.RFC3339)
	result.DryRun = *dryRunFlag
	result.OlderThanSeconds = int64(bound / time.Second)
	result.Settled = []threadSettleEntryJSON{}
	var firstErr error
	for _, key := range keys {
		g := groups[key]
		if !want[key] || len(g.Openers) == 0 {
			continue
		}
		// The newest old obligation is the anchor.
		newest := g.Openers[0]
		for _, ob := range g.Openers[1:] {
			if lessEntries(newest.Opener, ob.Opener) {
				newest = ob
			}
		}
		g.Anchor = newest
	}
	for _, key := range keys {
		g := groups[key]
		if g.Anchor == nil {
			continue
		}
		anc := g.Anchor
		openerIDs := make([]string, 0, len(g.Openers))
		for _, ob := range g.Openers {
			openerIDs = append(openerIDs, ob.Opener.ID)
		}
		sortOpeners(g.Openers)
		subject := fmt.Sprintf("Settled: %d requests through %q", len(g.Openers), anc.Opener.Subject)
		body := fmt.Sprintf("These requests from %s need nothing more from you; each one in this thread up to and including %s is closed.\n", me, anc.Opener.ID)
		for _, ob := range g.Openers {
			body += fmt.Sprintf("%s  %s  %s\n", ob.Opener.ID, ob.Opener.Kind, ob.Opener.Subject)
		}
		to := participantLabel(participant{Session: g.OwnerSession, Handle: g.OwnerHandle})
		text := fmt.Sprintf("settled %d requests to %s in %s through %s (%q) as %%s",
			len(g.Openers), to, anc.Opener.Thread, anc.Opener.ID, anc.Opener.Subject)
		if *dryRunFlag {
			if !common.JSON {
				_ = writeStdout("would settle %d requests to %s in %s through %s (%q)\n",
					len(g.Openers), to, anc.Opener.Thread, anc.Opener.ID, anc.Opener.Subject)
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
		_ = writeStdout("%s\n", fmt.Sprintf(text, msgID))
	}
	if len(result.Settled) == 0 && firstErr == nil {
		if !common.JSON {
			_ = writeStdout("Nothing to settle.\n")
		}
	}
	result.Kept = 0
	for _, ob := range red.Obligations {
		if ob.Open() {
			result.Kept++
		}
	}
	if common.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}
	_ = writeStdout("kept: %d open requests outside the bound\n", result.Kept)
	return firstErr
}

func sortOpeners(obs []*thread.Obligation) {
	// oldest-first matches the source report order
	for i := 1; i < len(obs); i++ {
		for j := i; j > 0; j-- {
			if lessEntries(obs[j-1].Opener, obs[j].Opener) {
				break
			}
			obs[j-1], obs[j] = obs[j], obs[j-1]
		}
	}
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
