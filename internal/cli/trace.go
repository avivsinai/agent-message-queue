package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/notificationattempt"
	"github.com/avivsinai/agent-message-queue/internal/receipt"
)

const traceSchema = "amq/trace/v1"

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
}

type traceLeg struct {
	Status   string          `json:"status"`
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
	root         string
	deliveryRoot *fsq.DeliveryRoot
	messageID    string
	agents       []string
	headers      []traceLocatedHeader
	targets      []traceLocatedHeader
	legs         map[string]traceLeg
	legErrors    map[string][]string
	seenRoutes   map[string]bool
	seenThread   map[string]bool
}

func runTrace(args []string) error {
	fs := flag.NewFlagSet("trace", flag.ContinueOnError)
	common := &commonFlags{flagSet: fs}
	registerImplicitRootFlag(fs, &common.Root, "Root directory for the queue")
	fs.BoolVar(&common.JSON, "json", false, "Emit JSON output")
	usage := usageWithFlags(fs, "amq trace <message-id> [options]",
		"Join current on-disk evidence for one message without mutating the queue.",
		"",
		"Phase A reports message copies, route fields, visible delivery artifacts, DLQ entries,",
		"delivery receipts, thread references, and durable agent-owned notification write attempts.",
		"Notification evidence never proves that a TUI or person saw, displayed, or submitted it.",
	)

	messageID := ""
	flagArgs := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		messageID = strings.TrimSpace(args[0])
		flagArgs = args[1:]
	}
	if handled, err := parseFlags(fs, flagArgs, usage); err != nil {
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
	if strings.ContainsAny(messageID, `/\`) || messageID == "." || messageID == ".." {
		return UsageError("invalid message id %q", messageID)
	}

	root := resolveRoot(common.Root)
	common.warnRootOverride()
	result := collectTrace(root, messageID)
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

func collectTrace(root, messageID string) traceResult {
	collector := &traceCollector{
		root:       root,
		messageID:  messageID,
		legs:       make(map[string]traceLeg, len(traceLegOrder)),
		legErrors:  make(map[string][]string),
		seenRoutes: map[string]bool{},
		seenThread: map[string]bool{},
	}
	for _, name := range traceLegOrder {
		collector.legs[name] = traceLeg{Evidence: []traceEvidence{}}
	}

	identity, err := fsq.SnapshotDeliveryRoot(root)
	if err != nil {
		collector.addRootError(err)
		collector.finishLegs()
		return collector.result()
	}
	deliveryRoot, err := fsq.OpenDeliveryRoot(root, identity)
	if err != nil {
		collector.addRootError(err)
		collector.finishLegs()
		return collector.result()
	}
	collector.deliveryRoot = deliveryRoot
	defer func() { _ = deliveryRoot.Close() }()

	collector.agents = collector.listAgents()
	collector.scanMessages()
	collector.scanDLQ()
	collector.scanReceipts()
	collector.scanNotificationAttempts()
	collector.joinHeaders()
	collector.finishLegs()
	return collector.result()
}

func (c *traceCollector) result() traceResult {
	status := "found"
	hasEvidence := false
	hasErrors := false
	for _, leg := range c.legs {
		if len(leg.Evidence) > 0 {
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
		entries, err := c.readDir(dir)
		if err != nil {
			c.addError("receipts", fmt.Sprintf("scan %s: %v", c.relative(dir), err))
			continue
		}
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
		}
	}
}

func (c *traceCollector) scanNotificationAttempts() {
	for _, agent := range c.agents {
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

func notificationAttemptLimitation(state string) string {
	switch state {
	case notificationattempt.StateIndeterminate:
		return "prepared without a durable result is indeterminate; it does not prove that notification bytes were written"
	case notificationattempt.OutcomeWritten:
		return "written means only that notifier bytes were accepted; it does not prove seen, displayed, or submitted"
	default:
		return "failed records a notifier write failure; it does not establish any terminal or user-visible state"
	}
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
			next:   "locate a parsable message copy; Phase A does not persist send-time route resolver decisions",
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
		leg := c.legs[name]
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
				leg.Detail = "durable notifier write-attempt evidence found; this never proves human or TUI observation"
				leg.NextStep = "treat prepared-without-result as indeterminate and written only as a byte-write outcome"
			}
		} else {
			leg.Status = "no_evidence"
			leg.Detail = noEvidence[name].detail
			leg.NextStep = noEvidence[name].next
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
		c.addError(name, fmt.Sprintf("open root: %v", err))
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
		leg := result.Legs[name]
		line := fmt.Sprintf("  %-12s %s", name, leg.Status)
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
