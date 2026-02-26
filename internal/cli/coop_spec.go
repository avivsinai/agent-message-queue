package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func runCoopSpec(args []string) error {
	if len(args) == 0 || isHelp(args[0]) {
		return printCoopSpecUsage()
	}

	switch args[0] {
	case "start":
		return runCoopSpecStart(args[1:])
	case "status":
		return runCoopSpecStatus(args[1:])
	case "submit":
		return runCoopSpecSubmit(args[1:])
	case "present":
		return runCoopSpecPresent(args[1:])
	default:
		return fmt.Errorf("unknown coop spec subcommand: %s\nRun 'amq coop spec --help' for usage", args[0])
	}
}

func printCoopSpecUsage() error {
	lines := []string{
		"amq coop spec - collaborative specification workflow",
		"",
		"Subcommands:",
		"  start    Start a new spec topic with a partner agent",
		"  status   Show current phase and submissions for a topic",
		"  submit   Submit research, draft, review, or final spec",
		"  present  Output the final spec to stdout",
		"",
		"Workflow phases: research → exchange → draft → review → converge → done",
		"",
		"Quick start:",
		"  amq coop spec start --topic auth-redesign --partner codex --body \"Problem description\"",
		"  amq coop spec submit --topic auth-redesign --phase research --body \"Findings...\"",
		"  amq coop spec status --topic auth-redesign",
		"  amq coop spec present --topic auth-redesign",
		"",
		"Run 'amq coop spec <subcommand> --help' for details.",
	}
	for _, line := range lines {
		if err := writeStdoutLine(line); err != nil {
			return err
		}
	}
	return nil
}

func runCoopSpecStart(args []string) error {
	fs := flag.NewFlagSet("coop spec start", flag.ContinueOnError)
	common := addCommonFlags(fs)
	topicFlag := fs.String("topic", "", "Topic name (lowercase, [a-z0-9_-]+)")
	partnerFlag := fs.String("partner", "", "Partner agent handle")
	bodyFlag := fs.String("body", "", "Problem description (string, @file, or stdin)")

	usage := usageWithFlags(fs, "amq coop spec start --topic <name> --partner <agent> [--body <text>]",
		"Start a new collaborative spec workflow.",
		"Creates the spec directory, initial state, and sends a research message to the partner.")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := common.validate(); err != nil {
		return err
	}
	if err := requireMe(common.Me); err != nil {
		return err
	}
	me, err := normalizeHandle(common.Me)
	if err != nil {
		return UsageError("--me: %v", err)
	}
	common.Me = me
	root := resolveRoot(common.Root)

	topic := strings.TrimSpace(*topicFlag)
	if topic == "" {
		return UsageError("--topic is required")
	}
	if err := fsq.ValidateTopicName(topic); err != nil {
		return UsageError("--topic: %v", err)
	}

	partner := strings.TrimSpace(*partnerFlag)
	if partner == "" {
		return UsageError("--partner is required")
	}
	partner, err = normalizeHandle(partner)
	if err != nil {
		return UsageError("--partner: %v", err)
	}
	if partner == me {
		return UsageError("--partner cannot be the same as --me")
	}

	// Validate handles against config.json
	if err := validateKnownHandles(root, common.Strict, me, partner); err != nil {
		return err
	}

	body, err := readBody(*bodyFlag)
	if err != nil {
		return err
	}

	var startResult struct {
		threadID string
		agents   []string
		msgID    string
		started  string
	}

	err = withSpecLock(root, topic, func() error {
		// Check if spec already exists (inside lock to prevent TOCTOU race)
		statePath := specStatePath(root, topic)
		if _, err := os.Stat(statePath); err == nil {
			return fmt.Errorf("spec %q already exists; use 'amq coop spec status --topic %s' to check progress", topic, topic)
		}

		// Create spec directory
		if err := fsq.EnsureSpecDirs(root, topic); err != nil {
			return fmt.Errorf("create spec directory: %w", err)
		}

		startResult.agents = []string{me, partner}
		startResult.threadID = "spec/" + topic
		now := time.Now().UTC()

		state := specState{
			Topic:       topic,
			Phase:       specPhaseResearch,
			Started:     now.Format(time.RFC3339Nano),
			StartedBy:   me,
			Agents:      startResult.agents,
			Thread:      startResult.threadID,
			Submissions: make(map[string]map[string]specSub),
		}
		startResult.started = state.Started

		if err := saveSpecState(root, topic, state); err != nil {
			return err
		}

		// Send research message to partner
		msgID, err := format.NewMessageID(now)
		if err != nil {
			return err
		}
		startResult.msgID = msgID

		subject := fmt.Sprintf("Spec: %s — research phase", topic)
		if body == "" {
			body = fmt.Sprintf("Starting collaborative spec for topic %q. Please research and submit your findings with:\n  amq coop spec submit --topic %s --phase research --body \"Your findings...\"", topic, topic)
		}

		msg := format.Message{
			Header: format.Header{
				Schema:   format.CurrentSchema,
				ID:       msgID,
				From:     me,
				To:       []string{partner},
				Thread:   startResult.threadID,
				Subject:  subject,
				Created:  now.Format(time.RFC3339Nano),
				Priority: format.PriorityNormal,
				Kind:     format.KindSpecResearch,
				Labels:   []string{"spec"},
			},
			Body: body,
		}

		data, err := msg.Marshal()
		if err != nil {
			return err
		}

		filename := msgID + ".md"
		if _, err := fsq.DeliverToInboxes(root, []string{partner}, filename, data); err != nil {
			return err
		}

		// Copy to sender outbox
		outboxDir := fsq.AgentOutboxSent(root, me)
		_, _ = fsq.WriteFileAtomic(outboxDir, filename, data, 0o600)

		return nil
	})
	if err != nil {
		return err
	}

	if common.JSON {
		return writeJSON(os.Stdout, map[string]any{
			"topic":   topic,
			"phase":   specPhaseResearch,
			"thread":  startResult.threadID,
			"agents":  startResult.agents,
			"msg_id":  startResult.msgID,
			"started": startResult.started,
		})
	}

	if err := writeStdout("Started spec %q (thread: %s)\n", topic, startResult.threadID); err != nil {
		return err
	}
	if err := writeStdout("  Phase: %s\n", specPhaseResearch); err != nil {
		return err
	}
	if err := writeStdout("  Sent research message to %s (%s)\n", partner, startResult.msgID); err != nil {
		return err
	}
	return printSpecNextSteps(me, partner, topic, specPhaseResearch, false)
}

func runCoopSpecStatus(args []string) error {
	fs := flag.NewFlagSet("coop spec status", flag.ContinueOnError)
	common := addCommonFlags(fs)
	topicFlag := fs.String("topic", "", "Topic name")

	usage := usageWithFlags(fs, "amq coop spec status --topic <name>",
		"Show current phase and submissions for a spec topic.")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := common.validate(); err != nil {
		return err
	}
	root := resolveRoot(common.Root)

	topic := strings.TrimSpace(*topicFlag)
	if topic == "" {
		return UsageError("--topic is required")
	}
	if err := fsq.ValidateTopicName(topic); err != nil {
		return UsageError("--topic: %v", err)
	}

	state, err := loadSpecState(root, topic)
	if err != nil {
		return err
	}

	if common.JSON {
		return writeJSON(os.Stdout, state)
	}

	if err := writeStdout("Spec: %s\n", state.Topic); err != nil {
		return err
	}
	if err := writeStdout("  Phase:      %s\n", state.Phase); err != nil {
		return err
	}
	if err := writeStdout("  Thread:     %s\n", state.Thread); err != nil {
		return err
	}
	if err := writeStdout("  Agents:     %s\n", strings.Join(state.Agents, ", ")); err != nil {
		return err
	}
	if err := writeStdout("  Started by: %s (%s)\n", state.StartedBy, state.Started); err != nil {
		return err
	}

	if len(state.Submissions) > 0 {
		if err := writeStdout("  Submissions:\n"); err != nil {
			return err
		}
		for _, agent := range state.Agents {
			subs, ok := state.Submissions[agent]
			if !ok || len(subs) == 0 {
				if err := writeStdout("    %s: (none)\n", agent); err != nil {
					return err
				}
				continue
			}
			for phase, sub := range subs {
				if err := writeStdout("    %s/%s: %s (%s)\n", agent, phase, sub.File, sub.Submitted); err != nil {
					return err
				}
			}
		}
	}

	if state.FinalSpec != "" {
		if err := writeStdout("  Final spec: %s\n", state.FinalSpec); err != nil {
			return err
		}
	}
	if state.Completed != "" {
		if err := writeStdout("  Completed:  %s\n", state.Completed); err != nil {
			return err
		}
	}

	// Show next steps based on current phase and who has submitted
	me := common.Me
	if me == "" {
		me = defaultMe()
	}
	if me != "" {
		var partner string
		for _, a := range state.Agents {
			if a != me {
				partner = a
				break
			}
		}
		mySubmitted := hasSubmittedPhase(&state, me)
		return printSpecNextSteps(me, partner, state.Topic, state.Phase, mySubmitted)
	}
	return nil
}

func runCoopSpecSubmit(args []string) error {
	fs := flag.NewFlagSet("coop spec submit", flag.ContinueOnError)
	common := addCommonFlags(fs)
	topicFlag := fs.String("topic", "", "Topic name")
	phaseFlag := fs.String("phase", "", "Submission phase: research, draft, review, or final")
	bodyFlag := fs.String("body", "", "Submission content (string, @file, or stdin)")

	usage := usageWithFlags(fs, "amq coop spec submit --topic <name> --phase <phase> [--body <text>]",
		"Submit research, draft, review, or final spec.",
		"Phase auto-advances when all agents have submitted.")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := common.validate(); err != nil {
		return err
	}
	if err := requireMe(common.Me); err != nil {
		return err
	}
	me, err := normalizeHandle(common.Me)
	if err != nil {
		return UsageError("--me: %v", err)
	}
	common.Me = me
	root := resolveRoot(common.Root)

	topic := strings.TrimSpace(*topicFlag)
	if topic == "" {
		return UsageError("--topic is required")
	}
	if err := fsq.ValidateTopicName(topic); err != nil {
		return UsageError("--topic: %v", err)
	}

	submitPhase := strings.TrimSpace(*phaseFlag)
	if submitPhase == "" {
		return UsageError("--phase is required")
	}
	validPhases := []string{specPhaseResearch, specPhaseDraft, specPhaseReview, "final"}
	phaseValid := false
	for _, vp := range validPhases {
		if submitPhase == vp {
			phaseValid = true
			break
		}
	}
	if !phaseValid {
		return UsageError("--phase must be one of: %s", strings.Join(validPhases, ", "))
	}

	body, err := readBody(*bodyFlag)
	if err != nil {
		return err
	}
	if body == "" {
		return UsageError("--body is required for submissions")
	}

	var result struct {
		Phase    string `json:"phase"`
		OldPhase string `json:"old_phase"`
		Advanced bool   `json:"advanced"`
		File     string `json:"file"`
		MsgID    string `json:"msg_id"`
	}

	err = withSpecLock(root, topic, func() error {
		state, err := loadSpecState(root, topic)
		if err != nil {
			return err
		}

		// Validate agent is participant
		isAgent := false
		for _, a := range state.Agents {
			if a == me {
				isAgent = true
				break
			}
		}
		if !isAgent {
			return fmt.Errorf("agent %q is not a participant in spec %q (agents: %s)", me, topic, strings.Join(state.Agents, ", "))
		}

		// Validate phase transition
		if err := validSubmitPhase(state.Phase, submitPhase); err != nil {
			return err
		}

		now := time.Now().UTC()

		// Write artifact file
		var artifactFile string
		if submitPhase == "final" {
			artifactFile = "final.md"
		} else {
			artifactFile = fmt.Sprintf("%s-%s.md", me, submitPhase)
		}
		artifactPath := filepath.Join(fsq.SpecTopicDir(root, topic), artifactFile)
		if err := os.WriteFile(artifactPath, []byte(body), 0o600); err != nil {
			return fmt.Errorf("write artifact: %w", err)
		}

		// Send message to partner(s)
		msgID, err := format.NewMessageID(now)
		if err != nil {
			return err
		}

		var recipients []string
		for _, a := range state.Agents {
			if a != me {
				recipients = append(recipients, a)
			}
		}

		// Determine message kind
		var kind string
		switch submitPhase {
		case specPhaseResearch:
			kind = format.KindSpecResearch
		case specPhaseDraft:
			kind = format.KindSpecDraft
		case specPhaseReview:
			kind = format.KindSpecReview
		case "final":
			kind = format.KindSpecDecision
		}

		subject := fmt.Sprintf("Spec: %s — %s submission", topic, submitPhase)
		msg := format.Message{
			Header: format.Header{
				Schema:   format.CurrentSchema,
				ID:       msgID,
				From:     me,
				To:       recipients,
				Thread:   state.Thread,
				Subject:  subject,
				Created:  now.Format(time.RFC3339Nano),
				Priority: format.PriorityNormal,
				Kind:     kind,
				Labels:   []string{"spec"},
				Context:  map[string]any{"paths": []string{artifactPath}},
			},
			Body: body,
		}

		data, err := msg.Marshal()
		if err != nil {
			return err
		}

		filename := msgID + ".md"
		if _, err := fsq.DeliverToInboxes(root, recipients, filename, data); err != nil {
			return err
		}
		// Copy to sender outbox
		outboxDir := fsq.AgentOutboxSent(root, me)
		_, _ = fsq.WriteFileAtomic(outboxDir, filename, data, 0o600)

		// Record submission
		if state.Submissions == nil {
			state.Submissions = make(map[string]map[string]specSub)
		}
		if state.Submissions[me] == nil {
			state.Submissions[me] = make(map[string]specSub)
		}
		state.Submissions[me][submitPhase] = specSub{
			Submitted: now.Format(time.RFC3339Nano),
			MsgID:     msgID,
			File:      artifactFile,
		}

		// Handle final spec
		if submitPhase == "final" {
			state.FinalSpec = artifactFile
		}

		// Try to advance phase
		oldPhase := state.Phase
		advanced := advancePhase(&state)

		// For exchange phase: submitting a draft auto-advances to draft
		if !advanced && oldPhase == specPhaseExchange && submitPhase == specPhaseDraft {
			state.Phase = specPhaseDraft
			advanced = true
			// Check if we should advance further (both drafts submitted)
			advancePhase(&state)
		}

		if err := saveSpecState(root, topic, state); err != nil {
			return err
		}

		result.Phase = state.Phase
		result.OldPhase = oldPhase
		result.Advanced = advanced
		result.File = artifactFile
		result.MsgID = msgID

		// Send phase transition status message if advanced
		if advanced && state.Phase != specPhaseDone {
			statusMsgID, err := format.NewMessageID(time.Now())
			if err != nil {
				return nil // Non-fatal: submission succeeded
			}
			statusMsg := format.Message{
				Header: format.Header{
					Schema:   format.CurrentSchema,
					ID:       statusMsgID,
					From:     me,
					To:       recipients,
					Thread:   state.Thread,
					Subject:  fmt.Sprintf("Spec: %s — phase advanced to %s", topic, state.Phase),
					Created:  time.Now().UTC().Format(time.RFC3339Nano),
					Priority: format.PriorityNormal,
					Kind:     format.KindStatus,
					Labels:   []string{"spec", "phase-transition"},
				},
				Body: fmt.Sprintf("Phase advanced from %s to %s.", oldPhase, state.Phase),
			}
			if statusData, err := statusMsg.Marshal(); err == nil {
				statusFilename := statusMsgID + ".md"
				_, _ = fsq.DeliverToInboxes(root, recipients, statusFilename, statusData)
			}
		}

		return nil
	})

	if err != nil {
		return err
	}

	if common.JSON {
		return writeJSON(os.Stdout, map[string]any{
			"topic":    topic,
			"phase":    result.Phase,
			"advanced": result.Advanced,
			"file":     result.File,
			"msg_id":   result.MsgID,
		})
	}

	if err := writeStdout("Submitted %s for spec %q (%s)\n", submitPhase, topic, result.MsgID); err != nil {
		return err
	}
	if result.Advanced {
		if err := writeStdout("  Phase advanced: %s → %s\n", result.OldPhase, result.Phase); err != nil {
			return err
		}
	}

	// Determine partner and submission state for next-step guidance
	var partner string
	var mySubmitted bool
	err = withSpecLock(root, topic, func() error {
		state, loadErr := loadSpecState(root, topic)
		if loadErr != nil {
			return loadErr
		}
		for _, a := range state.Agents {
			if a != me {
				partner = a
				break
			}
		}
		mySubmitted = hasSubmittedPhase(&state, me)
		return nil
	})
	if err != nil {
		return nil // Non-fatal: submission succeeded
	}
	return printSpecNextSteps(me, partner, topic, result.Phase, mySubmitted)
}

func runCoopSpecPresent(args []string) error {
	fs := flag.NewFlagSet("coop spec present", flag.ContinueOnError)
	common := addCommonFlags(fs)
	topicFlag := fs.String("topic", "", "Topic name")

	usage := usageWithFlags(fs, "amq coop spec present --topic <name>",
		"Output the final spec to stdout.")
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if err := common.validate(); err != nil {
		return err
	}
	root := resolveRoot(common.Root)

	topic := strings.TrimSpace(*topicFlag)
	if topic == "" {
		return UsageError("--topic is required")
	}
	if err := fsq.ValidateTopicName(topic); err != nil {
		return UsageError("--topic: %v", err)
	}

	state, err := loadSpecState(root, topic)
	if err != nil {
		return err
	}

	if state.FinalSpec == "" {
		return fmt.Errorf("spec %q has no final spec yet (phase: %s)", topic, state.Phase)
	}

	finalPath := filepath.Join(fsq.SpecTopicDir(root, topic), state.FinalSpec)
	data, err := os.ReadFile(finalPath)
	if err != nil {
		return fmt.Errorf("read final spec: %w", err)
	}

	if common.JSON {
		return writeJSON(os.Stdout, map[string]any{
			"topic": topic,
			"phase": state.Phase,
			"file":  state.FinalSpec,
			"body":  string(data),
		})
	}

	_, err = os.Stdout.Write(data)
	return err
}

// hasSubmittedPhase checks if agent has submitted for the given current phase.
func hasSubmittedPhase(state *specState, agent string) bool {
	subs, ok := state.Submissions[agent]
	if !ok {
		return false
	}
	phase := state.Phase
	// Map current phase to what submission would be expected
	switch phase {
	case specPhaseResearch:
		_, ok = subs[specPhaseResearch]
	case specPhaseExchange, specPhaseDraft:
		_, ok = subs[specPhaseDraft]
	case specPhaseReview:
		_, ok = subs[specPhaseReview]
	case specPhaseConverge:
		// Check if final submitted
		_, ok = subs["final"]
	default:
		return false
	}
	return ok
}

// printSpecNextSteps prints contextual guidance based on current phase.
// This output is critical for agent-guided workflows — agents read command
// output and follow the printed next steps.
func printSpecNextSteps(me, partner, topic, phase string, iSubmitted bool) error {
	if err := writeStdout("\n"); err != nil {
		return err
	}
	switch phase {
	case specPhaseResearch:
		if !iSubmitted {
			return writeStdout(
				"NEXT STEP: Research the codebase NOW, independently.\n"+
					"  Do NOT read %s's findings. Do NOT wait for %s.\n"+
					"  When done, run:\n"+
					"    amq coop spec submit --topic %s --phase research --body \"Your findings...\"\n",
				partner, partner, topic)
		}
		return writeStdout(
			"WAITING: You already submitted research. %s has not yet.\n"+
				"  Check progress:\n"+
				"    amq coop spec status --topic %s\n"+
				"  Phase advances automatically when %s also submits.\n",
			partner, topic, partner)
	case specPhaseExchange:
		return writeStdout(
			"NEXT STEP: Phase is now EXCHANGE.\n"+
				"  1. Read %s's research (check the spec/%s thread or artifacts)\n"+
				"  2. Discuss differences via: amq send --to %s --body \"...\" --thread spec/%s\n"+
				"  3. When ready, submit your draft:\n"+
				"    amq coop spec submit --topic %s --phase draft --body \"Your draft...\"\n",
			partner, topic, partner, topic, topic)
	case specPhaseDraft:
		if !iSubmitted {
			return writeStdout(
				"NEXT STEP: Submit your spec draft.\n"+
					"    amq coop spec submit --topic %s --phase draft --body \"Your draft...\"\n",
				topic)
		}
		return writeStdout(
			"WAITING: You already submitted a draft. %s has not yet.\n"+
				"  Check progress:\n"+
				"    amq coop spec status --topic %s\n",
			partner, topic)
	case specPhaseReview:
		if !iSubmitted {
			return writeStdout(
				"NEXT STEP: Read %s's draft and submit your review.\n"+
					"    amq coop spec submit --topic %s --phase review --body \"Your review...\"\n",
				partner, topic)
		}
		return writeStdout(
			"WAITING: You already submitted a review. %s has not yet.\n"+
				"  Check progress:\n"+
				"    amq coop spec status --topic %s\n",
			partner, topic)
	case specPhaseConverge:
		return writeStdout(
			"NEXT STEP: Synthesize both drafts and reviews into a final spec.\n"+
				"    amq coop spec submit --topic %s --phase final --body @final-spec.md\n",
			topic)
	case specPhaseDone:
		return writeStdout(
			"DONE: Spec %q is complete.\n"+
				"  View: amq coop spec present --topic %s\n",
			topic, topic)
	}
	return nil
}
