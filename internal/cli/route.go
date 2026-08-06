package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

type routeExplainResult struct {
	SchemaVersion  int      `json:"schema_version"`
	Routable       bool     `json:"routable"`
	Argv           []string `json:"argv"`
	DisplayCommand string   `json:"display_command"`
	SourceRoot     string   `json:"source_root"`
	DeliveryRoot   string   `json:"delivery_root"`
	SourceProject  string   `json:"source_project"`
	TargetProject  string   `json:"target_project"`
	SourceSession  string   `json:"source_session"`
	TargetSession  string   `json:"target_session"`
	Error          string   `json:"error,omitempty"`
}

func runRoute(args []string) error {
	if len(args) == 0 || isHelp(args[0]) {
		return printGroupUsage(findCommand("route"))
	}

	switch args[0] {
	case "explain":
		return runRouteExplain(args[1:])
	default:
		return formatUnknownSubcommand("route", args[0])
	}
}

func runRouteExplain(args []string) error {
	fs := flag.NewFlagSet("route explain", flag.ContinueOnError)
	toFlag := fs.String("to", "", "Receiver handle")
	projectFlag := fs.String("project", "", "Target peer project name")
	sessionFlag := fs.String("session", "", "Target session")
	fromRootFlag := fs.String("from-root", "", "Source AMQ root to explain from")
	rootFlag := fs.String("root", "", "Source AMQ root to explain from (alias for --from-root)")
	fromCwdFlag := fs.String("from-cwd", "", "Working directory to resolve .amqrc and auto-detection from")
	meFlag := fs.String("me", defaultMe(), "Sender handle (or AM_ME)")
	jsonFlag := fs.Bool("json", false, "Emit JSON output")

	usage := usageWithFlags(fs, "amq route explain --to <handle> [--project <project>] [--session <session>] --json",
		"Explains canonical AMQ routing without sending a message.",
		"",
		"Examples:",
		"  amq route explain --to codex --json",
		"  amq route explain --to qa --project project-b --session qa --json",
	)
	if handled, err := parseFlags(fs, args, usage); err != nil {
		return err
	} else if handled {
		return nil
	}
	if !*jsonFlag {
		return UsageError("route explain requires --json")
	}
	if strings.TrimSpace(*fromRootFlag) != "" && strings.TrimSpace(*rootFlag) != "" {
		return UsageError("--from-root and --root are mutually exclusive")
	}

	restore, err := chdirForRouteExplain(*fromCwdFlag)
	if err != nil {
		result := newRouteExplainResult()
		result.Error = err.Error()
		return writeJSON(os.Stdout, result)
	}
	defer restore()

	result := explainRoute(routeExplainOptions{
		To:       *toFlag,
		Project:  *projectFlag,
		Session:  *sessionFlag,
		FromRoot: firstNonEmpty(*fromRootFlag, *rootFlag),
		Me:       *meFlag,
	})
	return writeJSON(os.Stdout, result)
}

type routeExplainOptions struct {
	To       string
	Project  string
	Session  string
	FromRoot string
	Me       string
}

func explainRoute(opts routeExplainOptions) routeExplainResult {
	result := newRouteExplainResult()

	sourceRoot, me, err := resolveRouteSource(opts.FromRoot, opts.Me)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.SourceRoot = sourceRoot
	result.SourceProject = resolveProject(sourceRoot)
	result.SourceSession = resolveSessionName(sourceRoot)

	me, err = normalizeHandle(me)
	if err != nil {
		result.Error = fmt.Sprintf("--me: %v", err)
		return result
	}

	targetProject := strings.TrimSpace(opts.Project)
	targetSession := strings.TrimSpace(opts.Session)
	rawTo := strings.TrimSpace(opts.To)
	if targetProject == "" && rawTo != "" && strings.Contains(rawTo, "@") {
		if handle, project, session, ok := parseInlineRecipient(rawTo); ok {
			rawTo = handle
			targetProject = project
			if targetSession == "" {
				targetSession = session
			}
		}
	}
	result.TargetProject = targetProject
	result.TargetSession = targetSession

	recipients, err := splitRecipients(rawTo)
	if err != nil {
		result.Error = fmt.Sprintf("--to: %v", err)
		return result
	}
	recipients = dedupeStrings(recipients)
	if len(recipients) != 1 {
		result.Error = fmt.Sprintf("--to requires exactly one recipient for route explain (got %d)", len(recipients))
		return result
	}
	target := recipients[0]

	// Validate the source context exactly as the emitted send will. An explicit
	// --from-root confirms the cwd choice, but it does not waive an active pin or
	// cross-tree mismatch. Cross-project routing disambiguates the cwd boundary;
	// a target session never authorizes a mismatched source.
	explicitRoot := strings.TrimSpace(opts.FromRoot) != ""
	crossProject := targetProject != ""
	if err := guardPinnedSourceContextJSON("send", sourceRoot, crossProject, explicitRoot); err != nil {
		result.Error = err.Error()
		return result
	}

	plan, err := planDeliveryRoute(sourceRoot, targetProject, targetSession, deliveryRouteOptions{
		MirrorPeerSession: true,
	})
	if err != nil {
		result.TargetSession = plan.TargetSession
		result.Error = err.Error()
		return result
	}
	if err := validatePlannedMailbox(plan, target); err != nil {
		result.TargetSession = plan.TargetSession
		result.Error = err.Error()
		return result
	}

	result.Routable = true
	result.DeliveryRoot = plan.DeliveryRoot
	if plan.SourceProject != "" {
		result.SourceProject = plan.SourceProject
	}
	result.TargetSession = plan.TargetSession
	if result.TargetProject == "" {
		result.TargetProject = targetProject
	}
	if result.TargetSession == "" {
		result.TargetSession = result.SourceSession
	}
	result.Argv = buildRouteArgv(sourceRoot, me, target, targetProject, plan.TargetSession)
	result.DisplayCommand = displayCommand(result.Argv)
	return result
}

func newRouteExplainResult() routeExplainResult {
	return routeExplainResult{
		SchemaVersion: 1,
		Argv:          []string{},
	}
}

func resolveRouteSource(fromRoot, meFlag string) (sourceRoot, me string, err error) {
	if strings.TrimSpace(fromRoot) != "" {
		sourceRoot = resolveRoot(fromRoot)
		me = strings.TrimSpace(meFlag)
		if me == "" {
			return sourceRoot, "", fmt.Errorf("--me is required (or set AM_ME)")
		}
		return sourceRoot, me, nil
	}

	root, _, resolvedMe, err := resolveEnvConfigWithSource("", meFlag)
	if err != nil {
		return "", "", err
	}
	sourceRoot = resolveRoot(root)
	if strings.TrimSpace(resolvedMe) == "" {
		return sourceRoot, "", fmt.Errorf("--me is required (or set AM_ME)")
	}
	return sourceRoot, resolvedMe, nil
}

type deliveryRoutePlan struct {
	DeliveryRoot  string
	PeerBaseRoot  string
	SourceProject string
	TargetProject string
	TargetSession string
}

type deliveryRouteOptions struct {
	MirrorPeerSession bool
}

var findDeliveryRouteAmqrc = findAmqrcForRoot

func planDeliveryRoute(sourceRoot, targetProject, targetSession string, opts deliveryRouteOptions) (deliveryRoutePlan, error) {
	plan := deliveryRoutePlan{
		DeliveryRoot:  sourceRoot,
		TargetProject: strings.TrimSpace(targetProject),
		TargetSession: strings.TrimSpace(targetSession),
	}

	if plan.TargetProject != "" {
		routeConfig, err := findDeliveryRouteAmqrc(sourceRoot)
		if err != nil {
			return plan, federationSourceProjectError(sourceRoot)
		}
		sourceProject, err := requireFederationSourceProject(routeConfig, sourceRoot)
		if err != nil {
			return plan, err
		}
		plan.SourceProject = sourceProject

		peerBaseRoot, err := resolvePeerFromAmqrcResult(routeConfig, targetProject)
		if err != nil {
			return plan, err
		}
		plan.PeerBaseRoot = peerBaseRoot

		if plan.TargetSession != "" {
			normalized, err := normalizeHandle(plan.TargetSession)
			if err != nil {
				return plan, fmt.Errorf("--session: %v", err)
			}
			plan.TargetSession = normalized
			plan.DeliveryRoot, err = resolveSessionRoot(peerBaseRoot, plan.TargetSession)
			if err != nil {
				return plan, err
			}
		} else if opts.MirrorPeerSession && classifyRoot(sourceRoot) != "" {
			plan.TargetSession = sessionName(sourceRoot)
			plan.DeliveryRoot, err = resolveSessionRoot(peerBaseRoot, plan.TargetSession)
			if err != nil {
				return plan, err
			}
		} else {
			plan.DeliveryRoot = peerBaseRoot
		}
		return plan, nil
	}

	if plan.TargetSession != "" {
		normalized, err := normalizeHandle(plan.TargetSession)
		if err != nil {
			return plan, fmt.Errorf("--session: %v", err)
		}
		plan.TargetSession = normalized
		baseRoot := classifyRoot(sourceRoot)
		if baseRoot == "" {
			return plan, fmt.Errorf("--session requires a session context: run from inside 'amq coop exec --session <name>'")
		}
		plan.DeliveryRoot, err = resolveSessionRoot(baseRoot, plan.TargetSession)
		if err != nil {
			return plan, err
		}
	}

	return plan, nil
}

func requireFederationSourceProject(result amqrcResult, sourceRoot string) (string, error) {
	project := strings.TrimSpace(projectFromAmqrcResult(result))
	if project == "" {
		return "", federationSourceProjectError(sourceRoot)
	}
	configuredRoot := strings.TrimSpace(result.Config.Root)
	if configuredRoot == "" {
		return "", federationSourceOwnershipError(result, sourceRoot, "")
	}
	configuredBase, err := resolvePeerPath(result, configuredRoot)
	if err != nil {
		return "", fmt.Errorf("resolve configured source root: %w", err)
	}
	sourceRoot = absPath(resolveRoot(sourceRoot))
	configuredBase = absPath(resolveRoot(configuredBase))
	if sourceRoot != configuredBase {
		if filepath.Dir(sourceRoot) != configuredBase {
			return "", federationSourceOwnershipError(result, sourceRoot, configuredBase)
		}
		ownedSession, err := resolveSessionRoot(configuredBase, filepath.Base(sourceRoot))
		if err != nil || ownedSession != sourceRoot {
			return "", federationSourceOwnershipError(result, sourceRoot, configuredBase)
		}
	}
	return project, nil
}

func federationSourceProjectError(sourceRoot string) error {
	return fmt.Errorf(
		"cross-project routing requires a non-empty source project identity; set \"project\" in the .amqrc that owns source root %s",
		sourceRoot,
	)
}

func federationSourceOwnershipError(result amqrcResult, sourceRoot, configuredRoot string) error {
	if configuredRoot == "" {
		configuredRoot = "<empty>"
	}
	return fmt.Errorf(
		"configured .amqrc at %s does not own source root %s (configured root: %s)",
		result.Path,
		sourceRoot,
		configuredRoot,
	)
}

func validatePlannedMailbox(plan deliveryRoutePlan, target string) error {
	identity, err := fsq.SnapshotDeliveryRoot(plan.DeliveryRoot)
	if err != nil {
		return err
	}
	root, err := fsq.OpenDeliveryRoot(plan.DeliveryRoot, identity)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	layoutErr := fsq.ValidateExistingMailboxLayout(root, target)
	if plan.TargetProject != "" {
		return layoutErr
	}

	pin, err := loadSessionPin()
	if err != nil {
		return err
	}
	authorization, cleanup, err := openLocalMailboxAuthorization(root, plan.DeliveryRoot, pin, false, false)
	if err != nil {
		return err
	}
	defer cleanup()
	if authorization == nil {
		return layoutErr
	}
	if layoutErr == nil {
		return nil
	}
	return validateAuthorizedLocalMailbox(root, authorization, target)
}

func buildRouteArgv(sourceRoot, me, target, targetProject, targetSession string) []string {
	// Keep argv self-contained for tooling; display_command is presentation only.
	argv := []string{"amq", "send", "--root", sourceRoot, "--me", me, "--to", target}
	if targetProject != "" {
		argv = append(argv, "--project", targetProject)
	}
	if targetSession != "" {
		argv = append(argv, "--session", targetSession)
	}
	return argv
}

func displayCommand(argv []string) string {
	parts := make([]string, 0, len(argv))
	for _, arg := range argv {
		parts = append(parts, shellQuotePosix(arg))
	}
	return strings.Join(parts, " ")
}

func chdirForRouteExplain(fromCwd string) (func(), error) {
	fromCwd = strings.TrimSpace(fromCwd)
	if fromCwd == "" {
		return func() {}, nil
	}
	info, err := os.Stat(fromCwd)
	if err != nil {
		return nil, fmt.Errorf("--from-cwd: %v", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("--from-cwd is not a directory: %s", fromCwd)
	}
	oldWd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	if err := os.Chdir(fromCwd); err != nil {
		return nil, fmt.Errorf("--from-cwd: %v", err)
	}
	return func() { _ = os.Chdir(oldWd) }, nil
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
