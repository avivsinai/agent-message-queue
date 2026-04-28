package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	if fs.NArg() > 0 {
		return UsageError("route explain does not accept positional arguments (got %q)", strings.Join(fs.Args(), " "))
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

	deliveryRoot, normalizedTargetSession, err := resolveRouteDelivery(sourceRoot, targetProject, targetSession, target)
	if err != nil {
		result.TargetSession = normalizedTargetSession
		result.Error = err.Error()
		return result
	}

	result.Routable = true
	result.DeliveryRoot = deliveryRoot
	result.TargetSession = normalizedTargetSession
	if result.TargetProject == "" {
		result.TargetProject = targetProject
	}
	if result.TargetSession == "" {
		result.TargetSession = result.SourceSession
	}
	result.Argv = buildRouteArgv(sourceRoot, me, target, targetProject, normalizedTargetSession)
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

func resolveRouteDelivery(sourceRoot, targetProject, targetSession, target string) (deliveryRoot, normalizedTargetSession string, err error) {
	deliveryRoot = sourceRoot
	normalizedTargetSession = strings.TrimSpace(targetSession)

	if targetProject != "" {
		peerBaseRoot, err := resolvePeer(sourceRoot, targetProject)
		if err != nil {
			return "", normalizedTargetSession, err
		}
		if !dirExists(peerBaseRoot) {
			return "", normalizedTargetSession, fmt.Errorf("peer root for %q does not exist: %s", targetProject, peerBaseRoot)
		}

		if normalizedTargetSession != "" {
			normalized, err := normalizeHandle(normalizedTargetSession)
			if err != nil {
				return "", normalizedTargetSession, fmt.Errorf("--session: %v", err)
			}
			normalizedTargetSession = normalized
			deliveryRoot = filepath.Join(peerBaseRoot, normalizedTargetSession)
		} else if classifyRoot(sourceRoot) != "" {
			normalizedTargetSession = sessionName(sourceRoot)
			deliveryRoot = filepath.Join(peerBaseRoot, normalizedTargetSession)
		} else {
			deliveryRoot = peerBaseRoot
		}

		if !dirExists(deliveryRoot) {
			if normalizedTargetSession != "" {
				return "", normalizedTargetSession, fmt.Errorf("session %q not found in peer %q at %s", normalizedTargetSession, targetProject, deliveryRoot)
			}
			return "", normalizedTargetSession, fmt.Errorf("peer %q root does not exist at %s", targetProject, deliveryRoot)
		}
		inbox := filepath.Join(deliveryRoot, "agents", target, "inbox")
		if !dirExists(inbox) {
			if normalizedTargetSession != "" {
				return "", normalizedTargetSession, fmt.Errorf("agent %q not found in peer %q session %q", target, targetProject, normalizedTargetSession)
			}
			return "", normalizedTargetSession, fmt.Errorf("agent %q not found in peer %q", target, targetProject)
		}
		return deliveryRoot, normalizedTargetSession, nil
	}

	if normalizedTargetSession != "" {
		normalized, err := normalizeHandle(normalizedTargetSession)
		if err != nil {
			return "", normalizedTargetSession, fmt.Errorf("--session: %v", err)
		}
		normalizedTargetSession = normalized
		baseRoot := classifyRoot(sourceRoot)
		if baseRoot == "" {
			return "", normalizedTargetSession, fmt.Errorf("--session requires a session context: run from inside 'amq coop exec --session <name>'")
		}
		deliveryRoot = filepath.Join(baseRoot, normalizedTargetSession)
		if !dirExists(deliveryRoot) {
			return "", normalizedTargetSession, fmt.Errorf("session %q not found at %s", normalizedTargetSession, deliveryRoot)
		}
		inbox := filepath.Join(deliveryRoot, "agents", target, "inbox")
		if !dirExists(inbox) {
			return "", normalizedTargetSession, fmt.Errorf("agent %q not found in session %q", target, normalizedTargetSession)
		}
		return deliveryRoot, normalizedTargetSession, nil
	}

	return deliveryRoot, "", nil
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
