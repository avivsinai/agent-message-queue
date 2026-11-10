package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/notificationattempt"
	"github.com/avivsinai/agent-message-queue/internal/receipt"
)

const traceSchema = "amq/trace/v1"

const (
	traceNotificationLedgerAbsent          = "ledger_absent"
	traceNotificationLedgerPresentNoRecord = "ledger_present_no_record"
	traceNotificationRecordPresent         = "record_present"
	traceNotificationLedgerUnsupported     = "ledger_unsupported"
)

var traceLegOrder = []string{
	"message",
	"route",
	"delivery",
	"dlq",
	"receipts",
	"thread",
	"notification",
}

type traceResult struct {
	Schema       string              `json:"schema"`
	MessageID    string              `json:"message_id"`
	Status       string              `json:"status"`
	Root         string              `json:"root"`
	RootIdentity string              `json:"root_identity,omitempty"`
	Legs         map[string]traceLeg `json:"legs"`
	Follow       *traceFollow        `json:"follow,omitempty"`
}

type traceLeg struct {
	Status   string          `json:"status"`
	State    string          `json:"state,omitempty"`
	Evidence []traceEvidence `json:"evidence"`
	Detail   string          `json:"detail,omitempty"`
	NextStep string          `json:"next_step,omitempty"`
}

type traceEvidence struct {
	Authority    string                       `json:"authority"`
	Path         string                       `json:"path,omitempty"`
	Agent        string                       `json:"agent,omitempty"`
	Area         string                       `json:"area,omitempty"`
	Box          string                       `json:"box,omitempty"`
	Message      *traceMessage                `json:"message,omitempty"`
	Route        *traceRouteEvidence          `json:"route,omitempty"`
	DLQ          *fsq.DLQEnvelope             `json:"dlq,omitempty"`
	Receipt      *receipt.Receipt             `json:"receipt,omitempty"`
	Relation     *traceRelation               `json:"relation,omitempty"`
	Notification *notificationattempt.Attempt `json:"notification,omitempty"`
	State        string                       `json:"state,omitempty"`
	Stage        string                       `json:"stage,omitempty"`
	Durability   string                       `json:"durability,omitempty"`
	Limitation   string                       `json:"limitation,omitempty"`
}

type traceMessage struct {
	ID      string   `json:"id"`
	From    string   `json:"from"`
	To      []string `json:"to"`
	Thread  string   `json:"thread"`
	Created string   `json:"created"`
	Refs    []string `json:"refs"`
}

type traceRouteEvidence struct {
	From         string   `json:"from"`
	To           []string `json:"to"`
	Thread       string   `json:"thread"`
	ReplyTo      string   `json:"reply_to,omitempty"`
	ReplyProject string   `json:"reply_project,omitempty"`
	FromProject  string   `json:"from_project,omitempty"`
}

type traceRelation struct {
	MessageID string `json:"message_id"`
	Relation  string `json:"relation"`
	Thread    string `json:"thread,omitempty"`
}

type traceLocatedHeader struct {
	header format.Header
	path   string
	agent  string
	area   string
	box    string
}

type traceCollector struct {
	root                      string
	deliveryRoot              *fsq.DeliveryRoot
	messageID                 string
	agents                    []string
	headers                   []traceLocatedHeader
	targets                   []traceLocatedHeader
	legs                      map[string]traceLeg
	legErrors                 map[string][]string
	seenRoutes                map[string]bool
	seenThread                map[string]bool
	notificationLedgerPresent bool
}

func runTrace(args []string) error {
	fs := flag.NewFlagSet("trace", flag.ContinueOnError)
	common := &commonFlags{flagSet: fs}
	registerImplicitRootFlag(fs, &common.Root, "Root directory for the queue")
	fs.BoolVar(&common.JSON, "json", false, "Emit JSON output")
	follow := fs.Bool("follow", false, "Also find each recipient's copy and receipt in the roots the header routes to")
	from := fs.String("from", "", "With --follow: read only this sender's outbox copy instead of scanning the root")
	usage := usageWithFlags(fs, "amq trace <message-id> [--follow [--from <sender>]] [options]",
		"Join current on-disk evidence for one message without mutating the queue.",
		"",
		"It reports message copies, route fields, visible delivery artifacts, DLQ entries,",
		"delivery receipts, owed receipts (consumed, receipt not written yet; stage \"owed\"),",
		"thread references, and durable agent-owned notification write attempts.",
		"Notification evidence never proves that a TUI or person saw, displayed, or submitted it.",
		"",
		"--follow reads the sender's outbox copy and searches the roots its header routes to:",
		"the sender's root, the sessions of its project (reply_to), or the peer projects of",
		"its .amqrc and their sessions (reply_project). For each recipient it reports the",
		"root and session, the delivery (new, cur, dlq) and the receipt (drained, dlq, owed,",
		"or unknown when a receipt file cannot be read as a receipt).",
		"The session is the one whose root holds the receipt. A root that cannot be read is",
		"reported, and a recipient it might hold is unknown. --from <sender> reads",
		"agents/<sender>/outbox/sent/<id>.md directly and skips the scan of the root.",
		"For a request (review_request, question, todo) --follow also finds the replies whose",
		"refs name it, in the recipient's outbox or the requester's inbox, and reports whether",
		"each reply was delivered to the requester and drained.",
	)

	messageID := ""
	flagArgs := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		messageID = strings.TrimSpace(args[0])
		flagArgs = args[1:]
	}
	if handled, err := parseFlagsAllowPositionals(fs, flagArgs, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if messageID == "" {
		if fs.NArg() != 1 {
			return UsageError("exactly one message id is required")
		}
		messageID = strings.TrimSpace(fs.Arg(0))
	} else if fs.NArg() != 0 {
		return UsageError("trace accepts exactly one message id")
	}
	if messageID == "" {
		return UsageError("message id must not be blank")
	}
	if *from != "" && !*follow {
		return UsageError("--from requires --follow")
	}
	if *from != "" {
		if err := fsq.ValidateHandle(*from); err != nil {
			return UsageError("--from: %v", err)
		}
	}
	if strings.ContainsAny(messageID, `/\`) || messageID == "." || messageID == ".." {
		return UsageError("invalid message id %q", messageID)
	}

	root := resolveRoot(common.Root)
	common.warnRootOverride()
	var result traceResult
	var targets []traceLocatedHeader
	if *from != "" {
		result, targets = collectTraceFrom(root, messageID, *from)
	} else {
		result, targets = collectTrace(root, messageID)
	}
	if *follow {
		result.Follow = followTraceTargets(root, messageID, targets)
	}
	if identity, err := resolveTreeIdentityToken(root); err == nil {
		result.RootIdentity = identity
	}

	if common.JSON {
		if err := writeJSON(os.Stdout, result); err != nil {
			return err
		}
	} else if err := writeTraceText(result); err != nil {
		return err
	}
	if result.Status == "not_found" {
		return NotFoundError("message evidence not found: %s; verify the message id and AM_ROOT", messageID)
	}
	return nil
}

func collectTrace(root, messageID string) (traceResult, []traceLocatedHeader) {
	return runTraceCollector(root, messageID, traceLegOrder, func(c *traceCollector) {
		c.agents = c.listAgents()
		c.scanMessages()
		c.scanDLQ()
		c.scanReceipts()
		c.scanNotificationAttempts()
		c.joinHeaders()
	})
}

// collectTraceFrom reads only sender's outbox copy. Its legs are message and
// route: the legs a root scan would fill are absent, never no_evidence.
func collectTraceFrom(root, messageID, sender string) (traceResult, []traceLocatedHeader) {
	return runTraceCollector(root, messageID, []string{"message", "route"}, func(c *traceCollector) {
		path := filepath.Join("agents", sender, "outbox", "sent", messageID+".md")
		header, err := c.readHeader(path)
		switch {
		case os.IsNotExist(err):
		case err != nil:
			c.addError("message", fmt.Sprintf("read %s: %v", c.relative(path), err))
		case header.ID == messageID:
			c.addTarget(traceLocatedHeader{header: header, path: c.relative(path), agent: sender, area: "outbox", box: "sent"}, "message_file")
		}
	})
}

func runTraceCollector(root, messageID string, legs []string, scan func(*traceCollector)) (traceResult, []traceLocatedHeader) {
	collector := &traceCollector{
		root:       root,
		messageID:  messageID,
		legs:       make(map[string]traceLeg, len(legs)),
		legErrors:  make(map[string][]string),
		seenRoutes: map[string]bool{},
		seenThread: map[string]bool{},
	}
	for _, name := range legs {
		collector.legs[name] = traceLeg{Evidence: []traceEvidence{}}
	}

	identity, err := fsq.SnapshotDeliveryRoot(root)
	if err != nil {
		collector.addRootError(err)
		collector.finishLegs()
		return collector.result(), nil
	}
	deliveryRoot, err := fsq.OpenDeliveryRoot(root, identity)
	if err != nil {
		collector.addRootError(err)
		collector.finishLegs()
		return collector.result(), nil
	}
	collector.deliveryRoot = deliveryRoot
	defer func() { _ = deliveryRoot.Close() }()

	scan(collector)
	collector.finishLegs()
	return collector.result(), collector.targets
}

// followTraceTargets follows the message from its sender's outbox copy; the
// root that holds that copy is the root the header's route starts from.
func followTraceTargets(root, messageID string, targets []traceLocatedHeader) *traceFollow {
	for _, target := range targets {
		if target.area != "outbox" || target.agent != target.header.From {
			continue
		}
		h := target.header
		f := newFollower()
		defer f.Close()
		follow := f.follow(followedMessage{
			ID: h.ID, From: h.From, Kind: h.Kind, To: h.To, ReplyTo: h.ReplyTo, ReplyProject: h.ReplyProject, Root: root,
		})
		return &follow
	}
	return &traceFollow{
		MessageID:  messageID,
		Status:     "unknown",
		Detail:     "no sender outbox copy in this root; rerun in the sender's root with --from <sender>",
		Searched:   []followRoot{},
		Recipients: []followRecipient{},
	}
}

func (c *traceCollector) result() traceResult {
	status := "found"
	hasEvidence := false
	hasErrors := false
	for name, leg := range c.legs {
		// A notification attempt never proves the message exists.
		if name != "notification" && len(leg.Evidence) > 0 {
			hasEvidence = true
		}
		if leg.Status == "error" {
			hasErrors = true
		}
	}
	if !hasEvidence {
		status = "not_found"
	} else if hasErrors {
		status = "partial"
	}

	return traceResult{
		Schema:    traceSchema,
		MessageID: c.messageID,
		Status:    status,
		Root:      c.root,
		Legs:      c.legs,
	}
}

func (c *traceCollector) listAgents() []string {
	dir := "agents"
	entries, err := c.readDir(dir)
	if err != nil {
		c.addError("message", fmt.Sprintf("scan agents: %v", err))
		c.addError("dlq", fmt.Sprintf("scan agents: %v", err))
		c.addError("receipts", fmt.Sprintf("scan agents: %v", err))
		c.addError("thread", fmt.Sprintf("scan agents: %v", err))
		c.addError("notification", fmt.Sprintf("scan agents: %v", err))
		return []string{}
	}
	agents := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			agents = append(agents, entry.Name())
		}
	}
	sort.Strings(agents)
	return agents
}

func (c *traceCollector) scanMessages() {
	for _, agent := range c.agents {
		locations := []struct {
			area string
			box  string
			dir  string
		}{
			{area: "inbox", box: "new", dir: filepath.Join("agents", agent, "inbox", "new")},
			{area: "inbox", box: "cur", dir: filepath.Join("agents", agent, "inbox", "cur")},
			{area: "outbox", box: "sent", dir: filepath.Join("agents", agent, "outbox", "sent")},
		}
		for _, location := range locations {
			entries, err := c.readDir(location.dir)
			if err != nil {
				c.addError("message", fmt.Sprintf("scan %s: %v", c.relative(location.dir), err))
				c.addError("thread", fmt.Sprintf("scan %s: %v", c.relative(location.dir), err))
				continue
			}
			for _, entry := range entries {
				if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || !strings.HasSuffix(entry.Name(), ".md") {
					continue
				}
				path := filepath.Join(location.dir, entry.Name())
				header, err := c.readHeader(path)
				if err != nil {
					c.addError("thread", fmt.Sprintf("parse %s: %v", c.relative(path), err))
					if entry.Name() == c.messageID+".md" {
						c.addError("message", fmt.Sprintf("parse candidate %s: %v", c.relative(path), err))
						if location.area == "inbox" {
							c.addEvidence("delivery", traceEvidence{
								Authority:  "filesystem",
								Path:       c.relative(path),
								Agent:      agent,
								Area:       location.area,
								Box:        location.box,
								State:      "present",
								Durability: "no_evidence",
								Limitation: "file presence does not establish the original directory sync result",
							})
						}
					}
					continue
				}
				located := traceLocatedHeader{
					header: header,
					path:   c.relative(path),
					agent:  agent,
					area:   location.area,
					box:    location.box,
				}
				c.headers = append(c.headers, located)
				if header.ID == c.messageID {
					c.addTarget(located, "message_file")
				}
			}
		}
	}
}

func (c *traceCollector) scanDLQ() {
	for _, agent := range c.agents {
		for _, box := range []string{"new", "cur"} {
			dir := filepath.Join("agents", agent, "dlq", "new")
			if box == "cur" {
				dir = filepath.Join("agents", agent, "dlq", "cur")
			}
			entries, err := c.readDir(dir)
			if err != nil {
				c.addError("dlq", fmt.Sprintf("scan %s: %v", c.relative(dir), err))
				continue
			}
			for _, entry := range entries {
				if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || !strings.HasSuffix(entry.Name(), ".md") {
					continue
				}
				path := filepath.Join(dir, entry.Name())
				envelope, original, err := fsq.ReadDLQEnvelope(c.deliveryRoot, path)
				if err != nil {
					c.addError("dlq", fmt.Sprintf("parse %s: %v", c.relative(path), err))
					continue
				}
				if envelope.OriginalID != c.messageID {
					continue
				}
				envelopeCopy := *envelope
				relative := c.relative(path)
				c.addEvidence("dlq", traceEvidence{
					Authority: "dlq_envelope",
					Path:      relative,
					Agent:     agent,
					Area:      "dlq",
					Box:       box,
					DLQ:       &envelopeCopy,
				})
				c.addEvidence("delivery", traceEvidence{
					Authority:  "dlq_envelope",
					Path:       relative,
					Agent:      agent,
					Area:       "dlq",
					Box:        box,
					State:      "present",
					Durability: "no_evidence",
					Limitation: "DLQ presence does not establish the original directory sync result",
				})
				header, parseErr := format.ParseHeader(original)
				if parseErr != nil {
					continue
				}
				if header.ID == c.messageID {
					located := traceLocatedHeader{
						header: header,
						path:   relative,
						agent:  agent,
						area:   "dlq",
						box:    box,
					}
					c.headers = append(c.headers, located)
					c.addTarget(located, "dlq_original")
				}
			}
		}
	}
}

func (c *traceCollector) scanReceipts() {
	for _, agent := range c.agents {
		dir := filepath.Join("agents", agent, "receipts")
		// An unreadable receipts/ is reported, and the owed receipts are
		// still read: a failed receipt write is exactly when one is owed.
		entries, err := c.readDir(dir)
		if err != nil {
			c.addError("receipts", fmt.Sprintf("scan %s: %v", c.relative(dir), err))
		}
		written := map[string]bool{}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasPrefix(entry.Name(), c.messageID+"__") || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			item, err := receipt.ReadDeliveryRoot(c.deliveryRoot, path)
			if err != nil {
				c.addError("receipts", fmt.Sprintf("parse candidate %s: %v", c.relative(path), err))
				continue
			}
			if item.MsgID != c.messageID {
				continue
			}
			itemCopy := item
			c.addEvidence("receipts", traceEvidence{
				Authority: "delivery_receipt",
				Path:      c.relative(path),
				Agent:     agent,
				Receipt:   &itemCopy,
			})
			written[entry.Name()] = true
		}
		owed, err := receipt.ListPending(c.deliveryRoot, agent)
		if err != nil {
			c.addError("receipts", fmt.Sprintf("scan agent %s owed receipts: %v", agent, err))
			continue
		}
		for _, item := range owed {
			path := receipt.PendingPath(item)
			if item.MsgID != c.messageID || written[filepath.Base(path)] {
				continue
			}
			itemCopy := item
			c.addEvidence("receipts", traceEvidence{
				Authority: "owed_receipt",
				Path:      c.relative(path),
				Agent:     agent,
				Receipt:   &itemCopy,
				Stage:     "owed",
			})
		}
	}
}

func (c *traceCollector) scanNotificationAttempts() {
	for _, agent := range c.agents {
		ledgerPresent, err := c.notificationAttemptLedgerPresent(agent)
		if err != nil {
			c.addError("notification", fmt.Sprintf("scan agent %s notification attempt ledger: %v", agent, err))
			continue
		}
		c.notificationLedgerPresent = c.notificationLedgerPresent || ledgerPresent

		attempts, err := notificationattempt.ListDeliveryRoot(c.deliveryRoot, agent, c.messageID)
		if err != nil {
			c.addError("notification", fmt.Sprintf("scan agent %s notification attempts: %v", agent, err))
			continue
		}
		for i := range attempts {
			attempt := attempts[i]
			c.addEvidence("notification", traceEvidence{
				Authority:    "notification_attempt",
				Path:         filepath.ToSlash(filepath.Join("agents", agent, "receipts")),
				Agent:        agent,
				State:        attempt.State,
				Notification: &attempt,
				Limitation:   notificationAttemptLimitation(attempt.State),
			})
		}
	}
}

// notificationAttemptLedgerPresent checks both the active journal and its one
// retained rotation. It uses the pinned DeliveryRoot rather than reopening
// the base path, and filepath.Join keeps the relative path valid on Windows.
func (c *traceCollector) notificationAttemptLedgerPresent(agent string) (bool, error) {
	for _, filename := range []string{
		notificationattempt.LogFilename,
		notificationattempt.LogFilename + notificationattempt.RotatedSuffix,
	} {
		path := filepath.Join("agents", agent, "receipts", filename)
		if _, err := c.deliveryRoot.Stat(path); err == nil {
			return true, nil
		} else if !os.IsNotExist(err) {
			return false, fmt.Errorf("stat %s: %w", c.relative(path), err)
		}
	}
	return false, nil
}

func notificationAttemptTraceState(ledgerPresent, supported bool) string {
	switch {
	case ledgerPresent:
		return traceNotificationLedgerPresentNoRecord
	case !supported:
		return traceNotificationLedgerUnsupported
	default:
		return traceNotificationLedgerAbsent
	}
}

func notificationAttemptLimitation(state string) string {
	switch state {
	case notificationattempt.StateIndeterminate:
		return "prepared without a durable result is indeterminate; it does not prove that notification bytes were written"
	case notificationattempt.OutcomeWritten:
		return "written means only that notifier bytes were accepted; it does not prove seen, displayed, or submitted"
	case notificationattempt.StateAccepted:
		return "accepted means the external injector declared provider dispatch; it does not prove that the provider displayed, submitted, or consumed the doorbell"
	case notificationattempt.StateDeferred, notificationattempt.StateRetried:
		return "deferred/retried means the provider reported pre-dispatch busy or transition; the unread cohort remains pending and no provider acceptance is claimed"
	case notificationattempt.StateInvalid:
		return "invalid lifecycle ordering is ignored for acceptance; inspect the append-only notification journal"
	default:
		return "failed is terminal injector failure or uncertainty; it does not establish any terminal or user-visible state and is not silently replayed"
	}
}

// hasWriteFailedNotification reports whether any notification evidence
// carries StateWriteFailed — the prepared record could not be persisted, but
// the wake injected anyway. trace uses this to print "recording failed"
// rather than "no attempt recorded" (Requirement 3: the two facts must not
// print the same sentence).
func hasWriteFailedNotification(evidence []traceEvidence) bool {
	for _, ev := range evidence {
		if ev.Notification != nil && ev.Notification.State == notificationattempt.StateWriteFailed {
			return true
		}
	}
	return false
}

func (c *traceCollector) addTarget(located traceLocatedHeader, authority string) {
	c.targets = append(c.targets, located)
	header := located.header
	c.addEvidence("message", traceEvidence{
		Authority: authority,
		Path:      located.path,
		Agent:     located.agent,
		Area:      located.area,
		Box:       located.box,
		Message: &traceMessage{
			ID:      header.ID,
			From:    header.From,
			To:      append([]string{}, header.To...),
			Thread:  header.Thread,
			Created: header.Created,
			Refs:    append([]string{}, header.Refs...),
		},
	})
	if located.area == "inbox" {
		c.addEvidence("delivery", traceEvidence{
			Authority:  "message_file",
			Path:       located.path,
			Agent:      located.agent,
			Area:       located.area,
			Box:        located.box,
			State:      "present",
			Durability: "no_evidence",
			Limitation: "file presence proves current visibility, not the original directory sync result",
		})
	}

	routeKey := strings.Join([]string{
		header.From,
		strings.Join(header.To, ","),
		header.Thread,
		header.ReplyTo,
		header.ReplyProject,
		header.FromProject,
	}, "\x00")
	if !c.seenRoutes[routeKey] {
		c.seenRoutes[routeKey] = true
		c.addEvidence("route", traceEvidence{
			Authority: "message_header",
			Path:      located.path,
			Route: &traceRouteEvidence{
				From:         header.From,
				To:           append([]string{}, header.To...),
				Thread:       header.Thread,
				ReplyTo:      header.ReplyTo,
				ReplyProject: header.ReplyProject,
				FromProject:  header.FromProject,
			},
			Limitation: "header fields record the resulting addressing metadata, not a persisted send-time resolver decision",
		})
	}
}

func (c *traceCollector) joinHeaders() {
	targetRefs := map[string]bool{}
	for _, target := range c.targets {
		for _, ref := range target.header.Refs {
			targetRefs[ref] = true
			key := "target_references\x00" + ref
			if c.seenThread[key] {
				continue
			}
			c.seenThread[key] = true
			c.addEvidence("thread", traceEvidence{
				Authority: "message_refs",
				Path:      target.path,
				Agent:     target.agent,
				Area:      target.area,
				Box:       target.box,
				Relation: &traceRelation{
					MessageID: ref,
					Relation:  "target_references",
					Thread:    target.header.Thread,
				},
			})
		}
	}
	for _, located := range c.headers {
		relation := ""
		switch {
		case located.header.ID != c.messageID && containsTraceString(located.header.Refs, c.messageID):
			relation = "references_target"
		case located.header.ID != c.messageID && targetRefs[located.header.ID]:
			relation = "referenced_by_target"
		}
		if relation == "" {
			continue
		}
		key := relation + "\x00" + located.header.ID
		if c.seenThread[key] {
			continue
		}
		c.seenThread[key] = true
		c.addEvidence("thread", traceEvidence{
			Authority: "message_refs",
			Path:      located.path,
			Agent:     located.agent,
			Area:      located.area,
			Box:       located.box,
			Relation: &traceRelation{
				MessageID: located.header.ID,
				Relation:  relation,
				Thread:    located.header.Thread,
			},
		})
	}
}

func (c *traceCollector) finishLegs() {
	noEvidence := map[string]struct {
		detail string
		next   string
	}{
		"message": {
			detail: "no parsable message copy or matching DLQ original was found",
			next:   "verify the message id and AM_ROOT; run 'amq who --json' to inspect available sessions",
		},
		"route": {
			detail: "no message header is available to establish addressing metadata",
			next:   "locate a parsable message copy; AMQ does not persist send-time route resolver decisions",
		},
		"delivery": {
			detail: "no current message or DLQ file establishes visible delivery state",
			next:   "verify the target root and inspect sender output if it was retained; do not retry from this trace alone",
		},
		"dlq": {
			detail: "no matching DLQ envelope was found",
			next:   "if a DLQ transition was expected, run 'amq dlq list --me <consumer> --json' in the target root",
		},
		"receipts": {
			detail: "no drained or dlq delivery receipt was found",
			next:   "run 'amq receipts list --me <consumer> --msg-id " + c.messageID + " --json' for the expected consumer",
		},
		"thread": {
			detail: "no message refs connect another message to this id",
			next:   "inspect the message thread with 'amq thread --id <thread-id> --json' when a parsable header is available",
		},
		"notification": {
			detail: "no durable notification attempt record was found",
			next:   "run 'amq doctor --ops' for current wake health; do not infer historical notification success",
		},
	}
	for _, name := range traceLegOrder {
		leg, ok := c.legs[name]
		if !ok {
			continue
		}
		if errorsForLeg := c.legErrors[name]; len(errorsForLeg) > 0 {
			leg.Status = "error"
			leg.Detail = strings.Join(errorsForLeg, "; ")
			leg.NextStep = "fix the reported path or permission problem, then rerun the trace"
		} else if len(leg.Evidence) > 0 {
			leg.Status = "evidence"
			switch name {
			case "route":
				leg.Detail = "addressing metadata found; persisted send-time resolver decisions are no_evidence"
				leg.NextStep = "use 'amq route explain' only to inspect current routing; do not treat it as historical evidence"
			case "delivery":
				leg.Detail = "current file visibility found; original directory sync durability is no_evidence"
				leg.NextStep = "inspect retained send output before retrying; do not infer retry safety from current file presence"
			case "notification":
				leg.State = traceNotificationRecordPresent
				// Requirement 3: distinguish "no attempt recorded" from "recording
				// failed". If any evidence has StateWriteFailed, the prepared write
				// itself errored (the ledger could not persist the record) but the
				// wake injected anyway — the ledger never blocks delivery. An
				// operator debugging a missed doorbell must not conclude the wake
				// never tried.
				if hasWriteFailedNotification(leg.Evidence) {
					leg.Detail = "notification attempt was made but the attempt record could not be persisted; the wake injects regardless of ledger write success"
					leg.NextStep = "the notification was attempted — investigate the injection path, not the ledger; the missing record is a persistence gap, not a missed attempt"
				} else {
					leg.Detail = "durable notifier write-attempt evidence found; this never proves human or TUI observation"
					leg.NextStep = "treat prepared-without-result as indeterminate and written only as a byte-write outcome"
				}
			}
		} else {
			leg.Status = "no_evidence"
			leg.Detail = noEvidence[name].detail
			leg.NextStep = noEvidence[name].next
			if name == "notification" {
				leg.State = notificationAttemptTraceState(
					c.notificationLedgerPresent,
					notificationattempt.LedgerSupported,
				)
				switch leg.State {
				case traceNotificationLedgerAbsent:
					leg.Detail = "no notification attempt ledger file was found"
				case traceNotificationLedgerPresentNoRecord:
					leg.Detail = "notification attempt ledger exists, but no record for this message was found"
				case traceNotificationLedgerUnsupported:
					leg.Detail = fmt.Sprintf("notification attempt ledger is not supported on %s; delivery is not recorded", runtime.GOOS)
					leg.NextStep = "inspect provider or terminal output; do not infer notification outcome from this trace"
				}
			}
		}
		c.legs[name] = leg
	}
}

func (c *traceCollector) addEvidence(legName string, evidence traceEvidence) {
	leg := c.legs[legName]
	leg.Evidence = append(leg.Evidence, evidence)
	c.legs[legName] = leg
}

func (c *traceCollector) addError(legName, detail string) {
	c.legErrors[legName] = append(c.legErrors[legName], detail)
}

func (c *traceCollector) addRootError(err error) {
	for _, name := range []string{"message", "dlq", "receipts", "thread", "notification"} {
		if _, ok := c.legs[name]; ok {
			c.addError(name, fmt.Sprintf("open root: %v", err))
		}
	}
}

func (c *traceCollector) relative(path string) string {
	return filepath.ToSlash(path)
}

func (c *traceCollector) readDir(path string) ([]os.DirEntry, error) {
	return c.deliveryRoot.ReadDir(path)
}

func (c *traceCollector) readHeader(path string) (format.Header, error) {
	file, _, err := c.deliveryRoot.OpenRegularNoFollow(path)
	if err != nil {
		return format.Header{}, err
	}
	defer func() { _ = file.Close() }()
	return format.ReadHeader(file)
}

func containsTraceString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func writeTraceText(result traceResult) error {
	if err := writeStdout("Trace %s: %s\n", result.MessageID, result.Status); err != nil {
		return err
	}
	for _, name := range traceLegOrder {
		leg, ok := result.Legs[name]
		if !ok {
			continue
		}
		line := fmt.Sprintf("  %-12s %s", name, leg.Status)
		if leg.State != "" {
			line += " [" + leg.State + "]"
		}
		if len(leg.Evidence) > 0 {
			line += fmt.Sprintf(" (%d)", len(leg.Evidence))
		}
		if leg.Detail != "" {
			line += ": " + leg.Detail
		}
		if err := writeStdoutLine(line); err != nil {
			return err
		}
		for _, evidence := range leg.Evidence {
			if summary := traceEvidenceText(name, evidence); summary != "" {
				if err := writeStdout("    - %s\n", summary); err != nil {
					return err
				}
			}
		}
		if leg.NextStep != "" {
			if err := writeStdout("    next: %s\n", leg.NextStep); err != nil {
				return err
			}
		}
	}
	if result.Follow != nil {
		return writeFollowText(*result.Follow)
	}
	return nil
}

func traceEvidenceText(legName string, evidence traceEvidence) string {
	switch legName {
	case "message":
		if evidence.Message != nil {
			return fmt.Sprintf("%s: %s -> %s", evidence.Path, evidence.Message.From, strings.Join(evidence.Message.To, ","))
		}
	case "route":
		if evidence.Route != nil {
			return fmt.Sprintf("%s -> %s; thread %s", evidence.Route.From, strings.Join(evidence.Route.To, ","), evidence.Route.Thread)
		}
	case "delivery":
		return fmt.Sprintf("%s visible; durability %s", evidence.Path, evidence.Durability)
	case "dlq":
		if evidence.DLQ != nil {
			return fmt.Sprintf("%s: %s (%s)", evidence.Path, evidence.DLQ.FailureReason, evidence.DLQ.FailureDetail)
		}
	case "receipts":
		if evidence.Receipt != nil && evidence.Stage == "owed" {
			return fmt.Sprintf("%s owed by %s (consumed at %s; receipt not written yet)", evidence.Receipt.Stage, evidence.Receipt.Consumer, evidence.Receipt.EmittedAt)
		}
		if evidence.Receipt != nil {
			return fmt.Sprintf("%s by %s at %s", evidence.Receipt.Stage, evidence.Receipt.Consumer, evidence.Receipt.EmittedAt)
		}
	case "thread":
		if evidence.Relation != nil {
			return fmt.Sprintf("%s %s", evidence.Relation.Relation, evidence.Relation.MessageID)
		}
	case "notification":
		if evidence.Notification != nil {
			return fmt.Sprintf("%s by %s at %s", evidence.State, evidence.Agent, evidence.Notification.Prepared.RecordedAt)
		}
	}
	return ""
}
