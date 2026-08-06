package cli

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/sessionguard"
)

type listItem struct {
	ID       string    `json:"id"`
	From     string    `json:"from"`
	Subject  string    `json:"subject"`
	Thread   string    `json:"thread"`
	Created  string    `json:"created"`
	Box      string    `json:"box"`
	Path     string    `json:"path"`
	Priority string    `json:"priority,omitempty"`
	Kind     string    `json:"kind,omitempty"`
	Labels   []string  `json:"labels,omitempty"`
	SortKey  time.Time `json:"-"`
}

func (l listItem) GetCreated() string {
	return l.Created
}

func (l listItem) GetID() string {
	return l.ID
}

func (l listItem) GetRawTime() time.Time {
	return l.SortKey
}

func runList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	common := addCommonFlags(fs)
	newFlag := fs.Bool("new", false, "List messages in inbox/new")
	curFlag := fs.Bool("cur", false, "List messages in inbox/cur")
	limitFlag := fs.Int("limit", 0, "Limit number of messages (0 = no limit)")
	offsetFlag := fs.Int("offset", 0, "Offset into sorted results (0 = start)")
	sessionFlag := fs.String("session", "", "Target session under the resolved base root")

	// Filter flags
	priorityFlag := fs.String("priority", "", "Filter by priority (urgent, normal, low)")
	fromFlag := fs.String("from", "", "Filter by sender handle")
	kindFlag := fs.String("kind", "", "Filter by message kind")
	var labelFlags multiStringFlag
	fs.Var(&labelFlags, "label", "Filter by label (can be repeated)")

	usage := usageWithFlags(fs, "amq list --me <agent> [--session <name>] [--new | --cur] [options]")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := requireMe(common.Me); err != nil {
		return err
	}
	me, err := normalizeHandle(common.Me)
	legacyInspection := false
	if err != nil {
		legacy := strings.TrimSpace(common.Me)
		if !strings.HasPrefix(legacy, "-") ||
			fsq.ValidateLegacyHandleForInspection(legacy) != nil {
			return UsageError("--me: %v", err)
		}
		me = legacy
		legacyInspection = true
	}
	common.Me = me
	root, routed, err := resolveMailboxRoot(common, *sessionFlag)
	if err != nil {
		if GetExitCode(err) != ExitContextMismatch {
			return err
		}
		pin, pinErr := loadSessionPin()
		pinState := sessionGuardPinStateFor(pin)
		if pinErr != nil {
			pinState = sessionguard.PinInvalid
		}
		decision := sessionGuardDecisionForContext(
			sessionguard.KindList,
			sessionguard.ChannelExit5,
			pinState,
			&SessionContextError{Message: err.Error()},
			sessionguard.Flags{},
		)
		if decision.Verdict != sessionguard.WarnContinue {
			return err
		}
		_ = writeStderr("warning: %v\n", err)
		root, routed = absPath(resolveRoot(common.Root)), false
	}
	if !routed && !common.rootExplicit() {
		localRoot, ok, checkErr := cwdLocalMailboxRoot(root)
		if checkErr != nil && GetExitCode(checkErr) != ExitContextMismatch {
			return checkErr
		}
		if checkErr == nil && ok && !sameBaseTree(localRoot, root) {
			pin, pinErr := loadSessionPin()
			pinState := sessionGuardPinStateFor(pin)
			if pinErr != nil {
				pinState = sessionguard.PinInvalid
			}
			decision := sessionGuardDecisionForContext(
				sessionguard.KindList,
				sessionguard.ChannelExit5,
				pinState,
				&SessionContextError{Message: "active root conflicts with initialized repo-local root"},
				sessionguard.Flags{},
			)
			if decision.Verdict != sessionguard.WarnContinue {
				return ContextMismatchError("list context warning was not authorized")
			}
			if err := writeStderr(
				"warning: active root %s conflicts with initialized repo-local root %s detected from cwd; list is read-only and will inspect the active root. Pass explicit --root %s to confirm it, or repin to the repo-local root.\n",
				absPath(resolveRoot(root)),
				localRoot,
				shellQuotePosix(absPath(resolveRoot(root))),
			); err != nil {
				return err
			}
		}
	}
	if !routed {
		pin, pinErr := loadSessionPin()
		if pinErr != nil {
			if GetExitCode(pinErr) != ExitContextMismatch {
				return pinErr
			}
			decision := sessionGuardDecisionForContext(
				sessionguard.KindList,
				sessionguard.ChannelExit5,
				sessionguard.PinInvalid,
				&SessionContextError{Message: pinErr.Error()},
				sessionguard.Flags{},
			)
			if decision.Verdict != sessionguard.WarnContinue {
				return pinErr
			}
			_ = writeStderr("warning: %v\n", pinErr)
		} else {
			mismatch, checkErr := sessionPinMismatchWithPin(root, pin)
			if checkErr != nil {
				if GetExitCode(checkErr) != ExitContextMismatch {
					return checkErr
				}
				decision := sessionGuardDecisionForContext(
					sessionguard.KindList,
					sessionguard.ChannelExit5,
					sessionguard.PinInvalid,
					&SessionContextError{Message: checkErr.Error()},
					sessionguard.Flags{},
				)
				if decision.Verdict != sessionguard.WarnContinue {
					return checkErr
				}
				_ = writeStderr("warning: %v\n", checkErr)
			} else if mismatch != nil {
				if isExplicitOwnBaseRootInspectionWithPin(common, root, pin) {
					decision := sessionguard.Decide(sessionguard.Input{
						Kind: sessionguard.KindList,
						Pin:  sessionGuardPinStateFor(pin), Relation: sessionguard.TargetOwnPinnedBase,
						Flags: sessionguard.Flags{ExplicitRoot: true},
					})
					if decision.Verdict != sessionguard.Allow {
						return ContextMismatchError("list pinned-base inspection was not authorized")
					}
				} else {
					decision := sessionGuardDecisionForContext(
						sessionguard.KindList,
						sessionguard.ChannelExit5,
						sessionGuardPinStateFor(pin),
						mismatch,
						sessionguard.Flags{},
					)
					if decision.Verdict != sessionguard.WarnContinue {
						return ContextMismatchError("list context warning was not authorized")
					}
					_ = writeStderr("warning: %s\n", mismatch.Error())
				}
			}
		}
	}
	if err := requireMailbox(root, me); err != nil {
		emitSiblingBacklogHintsIfInboxEmpty(root, me)
		return err
	}

	// Validate handle against config.json
	if err := validateKnownHandles(root, common.Strict, me); err != nil {
		return err
	}
	validator, err := newHeaderValidator(root, common.Strict)
	if err != nil {
		return err
	}
	validator.allowLegacyFlagHandles = legacyInspection

	box := "new"
	if *newFlag && *curFlag {
		return UsageError("use only one of --new or --cur")
	}
	if *curFlag {
		box = "cur"
	}
	if *limitFlag < 0 {
		return UsageError("--limit must be >= 0")
	}
	if *offsetFlag < 0 {
		return UsageError("--offset must be >= 0")
	}

	// Validate filter values (allow empty, but reject invalid non-empty values)
	if *priorityFlag != "" && !format.IsValidPriority(*priorityFlag) {
		return UsageError("--priority must be one of: urgent, normal, low")
	}
	if *kindFlag != "" && !format.IsValidKind(*kindFlag) {
		return UsageError("--kind must be one of: %s", format.ValidKindsList())
	}
	if *fromFlag != "" {
		if _, err := normalizeHandle(*fromFlag); err != nil {
			return UsageError("--from: %v", err)
		}
	}

	var dir string
	if box == "new" {
		dir = fsq.AgentInboxNew(root, common.Me)
	} else {
		dir = fsq.AgentInboxCur(root, common.Me)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return NotFoundError("mailbox for %q disappeared while listing %s", common.Me, dir)
		}
		return err
	}

	items := make([]listItem, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".md") {
			continue
		}
		path := filepath.Join(dir, name)
		header, err := format.ReadHeaderFile(path)
		if err != nil {
			if err := writeStderr("warning: skipping corrupt message %s: %v\n", entry.Name(), err); err != nil {
				return err
			}
			continue
		}
		if err := validator.validate(header); err != nil {
			if err := writeStderr("warning: skipping invalid message %s: %v\n", entry.Name(), err); err != nil {
				return err
			}
			continue
		}
		item := listItem{
			ID:       header.ID,
			From:     header.From,
			Subject:  header.Subject,
			Thread:   header.Thread,
			Created:  header.Created,
			Box:      box,
			Path:     path,
			Priority: header.Priority,
			Kind:     header.Kind,
			Labels:   header.Labels,
		}
		if ts, err := time.Parse(time.RFC3339Nano, header.Created); err == nil {
			item.SortKey = ts
		}
		items = append(items, item)
	}

	// Apply filters
	filterOpts := FilterOptions{
		Priority: *priorityFlag,
		From:     *fromFlag,
		Kind:     *kindFlag,
		Labels:   labelFlags,
	}
	items = FilterMessages(items, filterOpts)

	format.SortByTimestamp(items)

	if *offsetFlag > 0 {
		if *offsetFlag >= len(items) {
			items = []listItem{}
		} else {
			items = items[*offsetFlag:]
		}
	}
	if *limitFlag > 0 && len(items) > *limitFlag {
		items = items[:*limitFlag]
	}

	if len(items) == 0 && box == "new" {
		emitSiblingBacklogHintsIfInboxEmpty(root, common.Me)
	}
	if common.JSON {
		return writeJSON(os.Stdout, items)
	}

	if len(items) == 0 {
		if err := writeStdoutLine("No messages."); err != nil {
			return err
		}
		return nil
	}
	for _, item := range items {
		subject := item.Subject
		if subject == "" {
			subject = "(no subject)"
		}
		priority := item.Priority
		if priority == "" {
			priority = "-"
		}
		if err := writeStdout("%s  %-6s  %s  %s  %s\n", item.Created, priority, item.From, item.ID, strings.TrimSpace(subject)); err != nil {
			return err
		}
	}
	return nil
}
