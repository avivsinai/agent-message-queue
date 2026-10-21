package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/keepalive/adapter"
	"github.com/avivsinai/agent-message-queue/internal/keepalive/amq"
	"github.com/avivsinai/agent-message-queue/internal/keepalive/registry"
	"github.com/avivsinai/agent-message-queue/internal/keepalive/supervisor"
)

type App struct {
	Stdout   io.Writer
	Stderr   io.Writer
	Adapters *adapter.Registry
	Wake     supervisor.WakeRunner
	Now      func() time.Time
	// SelfVersion is the version embedded in the running keepalive binary.
	// The command entry point supplies it; tests and library callers may leave
	// it empty to keep self-upgrade disabled.
	SelfVersion string

	adapterLogState *adapterLogState
}

// wakeRetirer stops an identity-confirmed live inject-via wake (or removes its
// exactly-bound proven-stale lock) whose saved target matches exactly. The
// concrete implementation is amq.CLI; the interface keeps the app callers
// testable with the same fake amq executables used across this package.
type wakeRetirer interface {
	CheckWake(ctx context.Context, req amq.StartWakeRequest) (amq.WakeCheckResult, error)
	RetireWake(ctx context.Context, req amq.RetireWakeRequest) (amq.RetireWakeResult, error)
}

func observeWakeGeneration(ctx context.Context, retirer wakeRetirer, root, me string) (string, error) {
	check, err := retirer.CheckWake(ctx, amq.StartWakeRequest{Root: root, Me: me})
	if err != nil {
		return "", fmt.Errorf("observe wake generation: %w", err)
	}
	generation := strings.TrimSpace(check.Generation)
	if check.LiveWake && generation == "" {
		return "", errors.New("live wake omitted generation")
	}
	return generation, nil
}

func (a App) Run(ctx context.Context, args []string) int {
	if a.Stdout == nil {
		a.Stdout = os.Stdout
	}
	if a.Stderr == nil {
		a.Stderr = os.Stderr
	}
	if a.adapterLogState == nil {
		a.adapterLogState = &adapterLogState{last: map[string]time.Time{}}
	}
	if len(args) == 0 {
		a.usage(a.Stderr)
		return 2
	}

	var err error
	switch args[0] {
	case "-h", "--help", "help":
		a.usage(a.Stdout)
		return 0
	case "attach":
		err = a.attach(ctx, args[1:])
	case "reattach":
		err = a.reattach(ctx, args[1:])
	case "supervise":
		err = a.supervise(ctx, args[1:])
	case "inject":
		err = a.inject(ctx, args[1:])
	case "doctor":
		err = a.doctor(args[1:])
	case "gc":
		err = a.gc(ctx, args[1:])
	case "retire-session":
		err = a.retireSession(ctx, args[1:])
	case "forget":
		err = a.forget(ctx, args[1:])
	case "install-launchd":
		err = a.installLaunchd(ctx, args[1:])
	case "install-hook":
		err = a.installHook(args[1:])
	case "uninstall":
		err = a.uninstallLaunchd(ctx, args[1:])
	default:
		_, _ = fmt.Fprintf(a.Stderr, "unknown command %q\n", args[0])
		a.usage(a.Stderr)
		return 2
	}
	if err != nil {
		writeCommandError(a.Stderr, err)
		return 1
	}
	return 0
}

func writeCommandError(w io.Writer, err error) {
	if !errors.Is(err, adapter.ErrInjectUncertain) {
		_, _ = fmt.Fprintln(w, err)
		return
	}

	// Keep the machine-readable marker on its own line. The wrapped error is
	// still useful to an operator, but its diagnostics must not look like a
	// second progress marker to a strict line parser.
	marker := adapter.ErrInjectUncertain.Error()
	_, _ = fmt.Fprintln(w, marker)
	detail := strings.TrimSpace(strings.ReplaceAll(err.Error(), marker, ""))
	detail = strings.TrimLeft(detail, ": ")
	if detail != "" {
		_, _ = fmt.Fprintln(w, detail)
	}
}

func (a App) wakeRunner(fallback supervisor.WakeRunner) supervisor.WakeRunner {
	if a.Wake != nil {
		return a.Wake
	}
	return fallback
}

func (a App) now() time.Time {
	if a.Now != nil {
		return a.Now().UTC()
	}
	return time.Now().UTC()
}

func (a App) adapterRegistry() adapter.Registry {
	if a.Adapters != nil {
		return *a.Adapters
	}
	state := a.adapterLogState
	if state == nil {
		state = &adapterLogState{last: map[string]time.Time{}}
	}
	return adapter.DefaultRegistry(a.rateLimitedAdapterLogf(state))
}

func (a App) inject(ctx context.Context, args []string) error {
	if len(args) != 3 {
		return errors.New("usage: amq-keepalive inject <adapter> <target> <payload>")
	}
	adapters := a.adapterRegistry()
	selected, err := adapters.Get(args[0])
	if err != nil {
		return err
	}
	if err := selected.Inject(ctx, args[1], args[2]); err != nil {
		return err
	}
	if _, ok := selected.(adapter.ProviderAcceptanceReporter); ok {
		_, _ = fmt.Fprintln(a.Stderr, "AMQ_INJECT_PROGRESS=accepted")
	}
	return nil
}

func (a App) usage(writer io.Writer) {
	_, _ = fmt.Fprintln(writer, "usage: amq-keepalive <attach|reattach|supervise|inject|doctor|gc|retire-session|forget|install-launchd|install-hook|uninstall|version> [options]")
	_, _ = fmt.Fprintln(writer, "       amq-keepalive <-v|--version>")
}

func mustDefaultRegistryPath() string {
	path, err := registry.DefaultPath()
	if err != nil {
		return ""
	}
	return path
}

// resolveDefaultRegistry fills an empty --registry with the default path.
// mustDefaultRegistryPath gives "" when the default refuses (for example,
// both keepalive homes exist), and this returns that refusal instead of
// "registry path is required".
func resolveDefaultRegistry(path *string) error {
	if *path != "" {
		return nil
	}
	resolved, err := registry.DefaultPath()
	if err != nil {
		return err
	}
	*path = resolved
	return nil
}

func executablePath() string {
	path, err := os.Executable()
	if err != nil {
		return os.Args[0]
	}
	return path
}

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
