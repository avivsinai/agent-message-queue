package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/board"
)

// labelFileLimit bounds one label import.
const labelFileLimit = 16 << 20

// runClassify exports Recent requests for an outside classifier, or imports
// the labels it returned. The board runs no model and schedules nothing.
func runClassify(args []string) int {
	flags := flag.NewFlagSet("amq-board classify", flag.ContinueOnError)
	classifier := flags.String("classifier", "", "classifier name to export for: the model and prompt revision")
	next := flags.Int("next", 8, "with --json, the most requests to export")
	asJSON := flags.Bool("json", false, "export the next requests without a current label as JSON")
	file := flags.String("file", "", "import a label file")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	if flags.NArg() != 0 || *asJSON == (*file != "") || *asJSON && *classifier == "" || *file != "" && *classifier != "" {
		fmt.Fprintln(os.Stderr, "usage: amq-board classify --classifier NAME --next N --json | amq-board classify --file LABELS.json")
		return exitUsage
	}
	dir, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-board:", err)
		return exitGeneral
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	store, err := resolveStore(ctx, newEnv(dir, execRunner{}))
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-board:", err)
		return exitGeneral
	}
	if *asJSON {
		err = exportRequests(ctx, store, *classifier, *next, os.Stdout)
	} else {
		err = importLabels(ctx, store, *file, os.Stdout)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "amq-board:", err)
		return exitGeneral
	}
	return 0
}

func exportRequests(ctx context.Context, store board.Store, classifier string, next int, w io.Writer) error {
	state, err := store.ReadContext(ctx)
	if err != nil {
		return err
	}
	batch, err := state.Classify(classifier, next, time.Now())
	if err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(batch)
}

func importLabels(ctx context.Context, store board.Store, path string, w io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, labelFileLimit+1))
	if err != nil {
		return err
	}
	if len(raw) > labelFileLimit {
		return errors.New("label file exceeds 16 MiB")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var batch board.LabelBatch
	if err = dec.Decode(&batch); err != nil {
		return fmt.Errorf("label file: %w", err)
	}
	added, err := store.PutRequestLabels(ctx, batch)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "imported %d new of %d labels from %s\n", added, len(batch.Labels), batch.Classifier)
	return err
}
