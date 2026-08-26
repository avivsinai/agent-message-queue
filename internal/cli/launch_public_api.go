package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/avivsinai/agent-message-queue/launchapi"
)

func runPublicLaunch(common *commonFlags, session, launcher, planSource string, prepareOnly bool, applySource, placementJSON, requestSource string, fs *flag.FlagSet) error {
	planExplicit, applyExplicit := flagWasVisited(fs, "plan"), flagWasVisited(fs, "apply")
	requestExplicit := flagWasVisited(fs, "request")
	if prepareOnly && !common.JSON {
		return UsageError("--prepare requires --json")
	}
	if applyExplicit && !common.JSON {
		return UsageError("--apply requires --json")
	}
	if requestExplicit && !common.JSON {
		return UsageError("--request requires --json")
	}
	if applyExplicit && (planExplicit || prepareOnly) {
		return UsageError("--apply is mutually exclusive with --plan and --prepare")
	}
	if requestExplicit && (planExplicit || applyExplicit) {
		return UsageError("--request is mutually exclusive with --plan and --apply")
	}
	if requestExplicit && flagWasVisited(fs, "placement") {
		return UsageError("--request is mutually exclusive with --placement")
	}
	if requestExplicit && (flagWasVisited(fs, "session") || flagWasVisited(fs, "launcher") || common.rootExplicit()) {
		return UsageError("--request takes its target and launcher from PrepareRequestV1")
	}
	if prepareOnly && !planExplicit && !requestExplicit {
		return UsageError("--prepare requires --plan or --request")
	}
	if applyExplicit && (flagWasVisited(fs, "session") || flagWasVisited(fs, "launcher") || common.rootExplicit()) {
		return UsageError("--apply takes its target and launcher from ApplyRequestV1")
	}
	if applyExplicit && flagWasVisited(fs, "placement") {
		return UsageError("--apply takes placement from ApplyRequestV1")
	}
	if flagWasVisited(fs, "placement") && !planExplicit {
		return UsageError("--placement requires --plan")
	}
	if flagWasVisited(fs, "placement") && strings.TrimSpace(placementJSON) == "" {
		return UsageError("--placement must be a PlacementV1 JSON object")
	}
	if planExplicit && strings.TrimSpace(planSource) == "" {
		return UsageError("--plan must name a file or -")
	}
	if applyExplicit && strings.TrimSpace(applySource) == "" {
		return UsageError("--apply must name a file or -")
	}
	if requestExplicit && strings.TrimSpace(requestSource) == "" {
		return UsageError("--request must name a file or -")
	}

	ctx := context.Background()
	if applyExplicit {
		data, err := readLaunchAPIDocument(applySource)
		if err != nil {
			return err
		}
		request, err := launchapi.DecodeApplyRequestV1(data)
		if err != nil {
			return UsageError("%v", err)
		}
		result, err := launchapi.Apply(ctx, request)
		if err != nil {
			return err
		}
		if err := outputPublicLaunchResult(result); err != nil {
			return err
		}
		return publicApplyExit(result)
	}

	if requestExplicit {
		data, err := readLaunchAPIDocument(requestSource)
		if err != nil {
			return err
		}
		prepareRequest, err := launchapi.DecodePrepareRequestV1(data)
		if err != nil {
			return UsageError("%v", err)
		}
		return runPublicPrepareThenApply(ctx, prepareRequest, prepareOnly)
	}

	target, err := resolvePublicLaunchTarget(common, session)
	if err != nil {
		return err
	}
	data, err := readLaunchAPIDocument(planSource)
	if err != nil {
		return err
	}
	intent, err := launchapi.DecodeLaunchIntentV1(data)
	if err != nil {
		return UsageError("%v", err)
	}
	request := launchapi.PrepareRequestV1{
		RequestVersion: launchapi.RequestVersionV1,
		Target:         target,
		Launcher:       strings.TrimSpace(launcher),
		Intent:         intent,
	}
	if flagWasVisited(fs, "placement") {
		placement, err := decodePublicPlacement(placementJSON)
		if err != nil {
			return err
		}
		request.Placement = placement
	}
	return runPublicPrepareThenApply(ctx, request, prepareOnly)
}

func outputPublicLaunchResult(result any) error {
	return launchapi.EncodeResultV1(os.Stdout, result)
}

// runPublicPrepareThenApply runs the shared Prepare (and, unless prepareOnly,
// Apply) flow for a PrepareRequestV1. The --plan and --request paths both
// decode their document into a PrepareRequestV1 and delegate here, so the
// outcome handling, action-required mapping, and Apply construction live in
// one place. With prepareOnly, the result is emitted with no mutation.
func runPublicPrepareThenApply(ctx context.Context, request launchapi.PrepareRequestV1, prepareOnly bool) error {
	prepared, err := launchapi.Prepare(ctx, request)
	if err != nil {
		return err
	}
	if prepareOnly || prepared.Outcome != launchapi.PrepareOutcomeReady || len(prepared.RequiredActions) != 0 {
		if err := outputPublicLaunchResult(prepared); err != nil {
			return err
		}
		if prepared.Outcome == launchapi.PrepareOutcomeActionRequired || len(prepared.RequiredActions) != 0 {
			return ActionRequiredError("%s", publicPrepareReason(prepared))
		}
		if prepared.Outcome == launchapi.PrepareOutcomeUnsupported {
			return ActionRequiredError("%s", publicPrepareReason(prepared))
		}
		return nil
	}
	result, err := launchapi.Apply(ctx, launchapi.ApplyRequestV1{
		RequestVersion: launchapi.RequestVersionV1,
		Prepare:        request,
		SubjectSchema:  prepared.SubjectSchema,
		SubjectDigest:  prepared.SubjectDigest,
		Decisions:      []launchapi.DecisionV1{},
	})
	if err != nil {
		return err
	}
	if err := outputPublicLaunchResult(result); err != nil {
		return err
	}
	return publicApplyExit(result)
}

func resolvePublicLaunchTarget(common *commonFlags, session string) (launchapi.TargetV1, error) {
	cfg, present, err := loadProjectLaunchConfig()
	if err != nil {
		return launchapi.TargetV1{}, err
	}
	if !present {
		return launchapi.TargetV1{}, NotFoundError("committed launch config not found; run 'amq setup'")
	}
	configPath, _, err := findProjectLaunchJSONPath()
	if err != nil {
		return launchapi.TargetV1{}, err
	}
	projectRoot := filepath.Dir(filepath.Dir(configPath))
	if resolved, resolveErr := filepath.EvalSymlinks(projectRoot); resolveErr == nil {
		projectRoot = resolved
	}
	projectRoot, err = filepath.Abs(projectRoot)
	if err != nil {
		return launchapi.TargetV1{}, err
	}
	session = strings.TrimSpace(session)
	if session == "" && !common.rootExplicit() {
		session = cfg.DefaultSession
	}
	if common.rootExplicit() {
		if flagWasVisited(common.flagSet, "session") {
			return launchapi.TargetV1{}, UsageError("--session and --root are mutually exclusive")
		}
		session = resolveSessionName(common.Root)
		if session == "" {
			return launchapi.TargetV1{}, UsageError("--root must select a named session root")
		}
	}
	if err := validateSessionName(session); err != nil {
		return launchapi.TargetV1{}, UsageError("--session: %v", err)
	}
	routeSession := session
	if common.rootExplicit() {
		routeSession = ""
	}
	root, routed, err := resolveMailboxRoot(common, routeSession)
	if err != nil {
		return launchapi.TargetV1{}, err
	}
	if err := guardMailboxContext("launch", root, routed, false, common.rootExplicit()); err != nil {
		return launchapi.TargetV1{}, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return launchapi.TargetV1{}, err
	}
	return launchapi.TargetV1{ProjectRoot: filepath.Clean(projectRoot), SessionRoot: filepath.Clean(root), Session: session}, nil
}

func decodePublicPlacement(raw string) (*launchapi.PlacementV1, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var placement launchapi.PlacementV1
	if err := decoder.Decode(&placement); err != nil {
		return nil, UsageError("placement: %v", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, UsageError("placement: multiple JSON values")
		}
		return nil, UsageError("placement: %v", err)
	}
	if err := placement.Validate(); err != nil {
		return nil, UsageError("placement: %v", err)
	}
	return &placement, nil
}

func readLaunchAPIDocument(source string) ([]byte, error) {
	var reader io.Reader
	var file *os.File
	if source == "-" {
		reader = os.Stdin
	} else {
		var err error
		file, err = os.Open(source)
		if err != nil {
			return nil, fmt.Errorf("open launch document: %w", err)
		}
		defer func() { _ = file.Close() }()
		reader = file
	}
	data, err := io.ReadAll(io.LimitReader(reader, maxLaunchAPIDocumentBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read launch document: %w", err)
	}
	if len(data) > maxLaunchAPIDocumentBytes {
		return nil, UsageError("launch document exceeds %d bytes", maxLaunchAPIDocumentBytes)
	}
	return data, nil
}

func publicPrepareReason(result launchapi.PrepareResultV1) string {
	if result.Reason != "" {
		return result.Reason
	}
	if len(result.RequiredActions) != 0 {
		return result.RequiredActions[0].ReasonCode
	}
	return "launch prepare requires action"
}

func publicApplyExit(result launchapi.ApplyResultV1) error {
	if result.Outcome != "action_required" {
		return nil
	}
	if result.FailureDetail != "" {
		if err := writeStderr("amq launch: %s\n", result.FailureDetail); err != nil {
			return err
		}
	}
	reason := result.ReasonCode
	if reason == "" {
		reason = "launch apply requires action"
	}
	return ActionRequiredError("%s", reason)
}
