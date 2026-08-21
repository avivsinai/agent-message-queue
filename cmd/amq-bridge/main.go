package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

var version = "dev"

type cliOptions struct {
	cfg      Config
	mode     Mode
	once     bool
	interval time.Duration
}

func main() {
	args := os.Args[1:]
	if isVersionArgument(args) {
		fmt.Println(version)
		return
	}
	if len(args) > 0 && args[0] == "identity" {
		if err := runIdentity(args[1:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			fmt.Fprintln(os.Stderr, "amq-bridge identity:", err)
			if isUsageError(err) {
				os.Exit(2)
			}
			os.Exit(1)
		}
		return
	}
	if len(args) > 0 && args[0] == "enqueue" {
		if err := runEnqueue(args[1:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			fmt.Fprintln(os.Stderr, "amq-bridge enqueue:", err)
			if isUsageError(err) {
				os.Exit(2)
			}
			os.Exit(1)
		}
		return
	}
	if len(args) > 0 && args[0] == "apply-file" {
		if err := runApplyFile(args[1:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			fmt.Fprintln(os.Stderr, "amq-bridge apply-file:", err)
			if isUsageError(err) {
				os.Exit(2)
			}
			os.Exit(1)
		}
		return
	}
	opts, err := parseFlags(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "amq-bridge:", err)
		os.Exit(2)
	}
	courier, err := NewCourier(opts.cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-bridge:", err)
		os.Exit(2)
	}
	if opts.interval <= 0 {
		fmt.Fprintln(os.Stderr, "amq-bridge: interval must be positive")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if opts.once {
		result, runErr := courier.RunOnce(ctx, opts.mode)
		if err := writeRunResult(result); err != nil {
			fmt.Fprintln(os.Stderr, "amq-bridge:", err)
			os.Exit(1)
		}
		if runErr != nil {
			fmt.Fprintln(os.Stderr, "amq-bridge:", runErr)
			os.Exit(1)
		}
		return
	}

	if err := runLoop(ctx, courier, opts.mode, opts.interval); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "amq-bridge:", err)
		os.Exit(1)
	}
}

func parseFlags(args []string) (cliOptions, error) {
	fs := flag.NewFlagSet("amq-bridge", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	root := fs.String("root", os.Getenv("AM_ROOT"), "local AMQ root")
	rendezvous := fs.String("rendezvous", "", "HTTPS rendezvous base URL")
	sourceHost := fs.String("source-host", "", "authenticated local host alias used on outbound envelopes")
	sourceHandle := fs.String("source-handle", "", "local allowlisted handle whose bridge spool is drained")
	destAlias := fs.String("dest-alias", "", "receiver-owned destination alias host/agent")
	receiveAlias := fs.String("receive-alias", "", "local receive alias host/agent for poll (defaults to --dest-alias)")
	localHost := fs.String("local-host", "", "local host identity; defaults to receive-alias host")
	allowDest := fs.String("allow-dest", "", "comma-separated receiver-owned destination aliases")
	allowSource := fs.String("allow-source-host", "", "inbound source host allowlist (required for poll)")
	spool := fs.String("spool", "", "bridge spool new directory (default: <root>/bridge/outbox/<source-handle>/new)")
	receipts := fs.String("receipt-dir", "", "bridge receipt directory below the AMQ root")
	keyGeneration := fs.String("key-generation", defaultKeyGeneration, "envelope key generation label")
	batch := fs.Int("batch-size", defaultBridgeBatchSize, "maximum envelopes per cycle")
	mode := fs.String("mode", string(ModeBoth), "cycle mode: both, push, or poll")
	once := fs.Bool("once", true, "run one bounded cycle and exit")
	interval := fs.Duration("interval", 30*time.Second, "delay between cycles when --once=false")
	if err := fs.Parse(args); err != nil {
		return cliOptions{}, err
	}
	if fs.NArg() != 0 {
		return cliOptions{}, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	parsedMode := Mode(strings.TrimSpace(*mode))
	if parsedMode != ModeBoth && parsedMode != ModePush && parsedMode != ModePoll {
		return cliOptions{}, fmt.Errorf("invalid --mode %q; want both, push, or poll", *mode)
	}
	return cliOptions{
		cfg: Config{
			Root:               *root,
			RendezvousURL:      *rendezvous,
			SourceHost:         *sourceHost,
			SourceHandle:       *sourceHandle,
			DestAlias:          strings.TrimSpace(*destAlias),
			ReceiveAlias:       strings.TrimSpace(*receiveAlias),
			LocalHost:          strings.TrimSpace(*localHost),
			AllowedDestAliases: splitCSV(*allowDest),
			AllowedSourceHosts: splitCSV(*allowSource),
			SpoolDir:           *spool,
			ReceiptDir:         *receipts,
			KeyGeneration:      *keyGeneration,
			BatchSize:          *batch,
		},
		mode:     parsedMode,
		once:     *once,
		interval: *interval,
	}, nil
}

func splitCSV(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	return strings.Split(raw, ",")
}

func isVersionArgument(args []string) bool {
	return len(args) == 1 && (args[0] == "-v" || args[0] == "--version" || args[0] == "version")
}

func isUsageError(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "unexpected arguments") ||
		strings.Contains(msg, "is required") ||
		strings.Contains(msg, "not allowed with enqueue")
}

func runLoop(ctx context.Context, courier *Courier, mode Mode, interval time.Duration) error {
	for {
		result, err := courier.RunOnce(ctx, mode)
		if writeErr := writeRunResult(result); writeErr != nil {
			return writeErr
		}
		if err != nil {
			return err
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func writeRunResult(result RunResult) error {
	encoder := json.NewEncoder(os.Stdout)
	for _, receipt := range result.Push.Receipts {
		if err := encoder.Encode(receipt); err != nil {
			return err
		}
	}
	for _, receipt := range result.Poll.Receipts {
		if err := encoder.Encode(receipt); err != nil {
			return err
		}
	}
	return nil
}
