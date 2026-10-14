package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/avivsinai/agent-message-queue/internal/bridge/xferv3"
)

func runUp(args []string) error {
	fs := flag.NewFlagSet("amq-bridge up", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	root := fs.String("root", os.Getenv("AM_ROOT"), "local AMQ root")
	ensure := fs.Bool("ensure", false, "write the launchd template and exit")
	ack := fs.Bool("acknowledge-empty-ledger", false, "allow start when mail exists and the v3 ledger is empty")
	once := fs.Bool("once", false, "run one status tick and exit")
	interval := fs.Duration("interval", 15*time.Minute, "owed-mail timer")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if strings.TrimSpace(*root) == "" {
		return fmt.Errorf("bridge root is required")
	}
	if err := xferv3.CheckEmptyLedger(*root, *ack); err != nil {
		return err
	}
	if *ensure {
		bin, err := os.Executable()
		if err != nil {
			return err
		}
		fmt.Print(xferv3.LaunchdPlist(bin, *root))
		return nil
	}
	return xferv3.HoldPID(*root, func() error {
		if *once {
			return printStatus(*root)
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return watchUp(ctx, *root, *interval)
	})
}

func runStatus(args []string) error {
	fs := flag.NewFlagSet("amq-bridge status", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	root := fs.String("root", os.Getenv("AM_ROOT"), "local AMQ root")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if strings.TrimSpace(*root) == "" {
		return fmt.Errorf("bridge root is required")
	}
	return printStatus(*root)
}

func runDiscard(args []string) error {
	fs := flag.NewFlagSet("amq-bridge discard", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	root := fs.String("root", os.Getenv("AM_ROOT"), "local AMQ root")
	peerName := fs.String("peer", "", "peer host")
	idText := fs.String("id", "", "transfer id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if strings.TrimSpace(*root) == "" {
		return fmt.Errorf("bridge root is required")
	}
	peer, err := xferv3.ParseHost(*peerName)
	if err != nil {
		return err
	}
	id, err := xferv3.ParseID(*idText)
	if err != nil {
		return err
	}
	if err := xferv3.Discard(*root, peer, id); err != nil {
		return err
	}
	fmt.Printf("discarded peer=%s id=%s\n", peer.String(), id.String())
	return nil
}

func printStatus(root string) error {
	report, err := xferv3.CollectStatus(root)
	if err != nil {
		return err
	}
	fmt.Printf("owed=%d oldest=%s ledger_bytes=%d uncertain=%d\n", len(report.Owed), report.OldestAge.Round(time.Second), report.LedgerBytes, report.Uncertain)
	for _, item := range report.Owed {
		reason := item.Reason
		if reason == "" {
			reason = "owed"
		}
		fmt.Printf("peer=%s id=%s reason=%s age=%s\n", item.Peer, item.ID, reason, item.Age.Round(time.Second))
	}
	if report.Unhealthy {
		return fmt.Errorf("xferv3: status is unhealthy")
	}
	return nil
}

func watchUp(ctx context.Context, root string, interval time.Duration) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer func() { _ = watcher.Close() }()
	tx := filepath.Join(root, "bridge", "tx")
	if err := os.MkdirAll(tx, 0o700); err != nil {
		return err
	}
	if err := watcher.Add(tx); err != nil {
		return err
	}
	timer := time.NewTimer(jitter(interval))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-watcher.Events:
			_ = tryWake(root)
			resetTimer(timer, interval)
		case <-timer.C:
			_ = tryWake(root)
			resetTimer(timer, interval)
		case err := <-watcher.Errors:
			if err != nil {
				return err
			}
		}
	}
}

func resetTimer(timer *time.Timer, interval time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(jitter(interval))
}

func jitter(interval time.Duration) time.Duration {
	if interval <= 0 {
		return 15 * time.Minute
	}
	return interval + time.Duration(os.Getpid()%17)*time.Second
}

func tryWake(root string) error {
	amq, err := exec.LookPath("amq")
	if err != nil {
		return nil
	}
	cmd := exec.Command(amq, "wake", "--root", root)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
