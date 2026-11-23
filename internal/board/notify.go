package board

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/lock"
)

// InputRunner supplies input to one command while retaining the pinned
// environment and command bounds. Every send obtains a fresh text reader.
type InputRunner interface {
	WithInput(io.Reader) Runner
}

// NotifyPlan carries an independently resolved live route and the snapshot's
// store. Sender and Recipients are explicit handles, not ambient seats.
type NotifyPlan struct {
	Sender, Session string
	Recipients      []string
	Roster          []RosterEntry
	Env             Env
	Store           Store
}

type deliveredEntry struct {
	Incident string `json:"incident"`
	To       string `json:"to"`
}

type deliveredSet struct {
	Sent []deliveredEntry `json:"sent"`
}

// Notify validates every handle before its first send, under the live route,
// not under a stale snapshot. The store lock covers the entire run. Each pair
// is written atomically after its send succeeds. A crash between a send and
// its record sends that one notice again on the next run.
func Notify(ctx context.Context, plan NotifyPlan, stale bool, problems []Problem) error {
	unknown := map[string]bool{}
	for _, handle := range append([]string{plan.Sender}, plan.Recipients...) {
		if unknown[handle] {
			return fmt.Errorf("floor identities must be distinct: %s", handle)
		}
		unknown[handle] = true
	}
	if len(unknown) < 2 {
		return errors.New("--as and --notify are required together")
	}
	for _, entry := range plan.Roster {
		if entry.Session == plan.Session && entry.ValidHandle {
			delete(unknown, entry.Handle)
		}
	}
	if len(unknown) > 0 {
		return fmt.Errorf("notify handle is not in the board's session %q: %s", plan.Session, strings.Join(notifySortedKeys(unknown), ", "))
	}
	if err := plan.Store.ensureDeliveredDir(); err != nil {
		return err
	}
	return lock.WithExclusiveFileLockUntil(filepath.Join(plan.Store.Dir, "notify.lock"), func() bool { return ctx.Err() != nil }, func() error {
		delivered, err := plan.Store.readDelivered()
		if err != nil {
			return err
		}
		if stale {
			problems = []Problem{{Kind: ProblemBoardStale, Text: "The board snapshot is absent or stale; run amq-board sync."}}
		}
		var failures []error
		for _, problem := range problems {
			incident := problem.Incident()
			if incident == "" {
				return fmt.Errorf("floor problem has no incident identity: %s", problem.Kind)
			}
			for _, recipient := range plan.Recipients {
				pair := deliveredPair(incident, recipient)
				if delivered[pair] {
					continue
				}
				if err := notifySend(ctx, plan, recipient, problem); err != nil {
					failures = append(failures, err)
					continue
				}
				delivered[pair] = true
				if err := plan.Store.writeDelivered(delivered); err != nil {
					failures = append(failures, err)
				}
			}
		}
		if !stale {
			active := map[string]bool{}
			for _, p := range problems {
				active[p.Incident()] = true
			}
			for entry := range delivered {
				if !active[entry.Incident] {
					delete(delivered, entry)
				}
			}
			if err := plan.Store.writeDelivered(delivered); err != nil {
				failures = append(failures, err)
			}
		}
		return errors.Join(failures...)
	})
}

func notifySend(ctx context.Context, plan NotifyPlan, recipient string, problem Problem) error {
	in, ok := plan.Env.Run.(InputRunner)
	if !ok {
		return errors.New("floor notice runner does not support stdin")
	}
	argv := []string{"amq", "send", "--me", plan.Sender, "--to", recipient,
		"--kind", "status", "--labels", "floor", "--subject", "Floor: " + string(problem.Kind), "--body", "-"}
	if _, err := in.WithInput(strings.NewReader(problem.Text)).Run(ctx, plan.Env.Dir, argv); err != nil {
		return fmt.Errorf("floor notice to %s: %w", recipient, err)
	}
	return nil
}

func deliveredPair(incident, recipient string) deliveredEntry {
	return deliveredEntry{Incident: incident, To: recipient}
}

func notifySortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func (s Store) ensureDeliveredDir() error {
	if err := os.MkdirAll(filepath.Dir(s.Dir), 0o700); err != nil {
		return err
	}
	if err := os.Mkdir(s.Dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	if err := s.safeDirectory(); err != nil {
		return err
	}
	if info, err := os.Lstat(filepath.Join(s.Dir, "notify.lock")); err == nil && !info.Mode().IsRegular() {
		return errors.New("floor notification lock is not a regular file")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s Store) readDelivered() (map[deliveredEntry]bool, error) {
	raw, err := fsq.ReadRegularNoFollowBounded(filepath.Join(s.Dir, "floor-notices.json"), 64<<20)
	if errors.Is(err, os.ErrNotExist) {
		return map[deliveredEntry]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	var doc deliveredSet
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("floor notification delivery set: %w", err)
	}
	set := make(map[deliveredEntry]bool, len(doc.Sent))
	for _, pair := range doc.Sent {
		if pair.Incident == "" || fsq.ValidateHandle(pair.To) != nil {
			return nil, errors.New("floor notification delivery set has an invalid pair")
		}
		set[pair] = true
	}
	return set, nil
}

func (s Store) writeDelivered(set map[deliveredEntry]bool) error {
	entries := make([]deliveredEntry, 0, len(set))
	for entry := range set {
		entries = append(entries, entry)
	}
	slices.SortFunc(entries, func(a, b deliveredEntry) int {
		if d := strings.Compare(a.Incident, b.Incident); d != 0 {
			return d
		}
		return strings.Compare(a.To, b.To)
	})
	raw, err := json.MarshalIndent(deliveredSet{Sent: entries}, "", "  ")
	if err != nil {
		return err
	}
	if _, err := s.writeFile("floor-notices.json", append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("floor notification delivery set: %w", err)
	}
	return nil
}
