package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/board"
	"github.com/avivsinai/agent-message-queue/internal/harness"
)

// CodexGoals reads the versioned Codex goals store in place. Objectives are
// selected only after the thread has passed project eligibility.
type CodexGoals struct{ Home string }

func (CodexGoals) Name() string { return "codex-goals" }

func (CodexGoals) Discover(env board.Env) (bool, string) {
	if _, err := env.LookPath("sqlite3"); err != nil {
		return false, "sqlite3 not found"
	}
	return true, ""
}

var codexGoalsFeeds = []board.Column{board.ColumnInFlight, board.ColumnBlocked}

func (source CodexGoals) Read(ctx context.Context, env board.Env) board.Result {
	res := board.Result{Status: board.StatusDegraded, ObservedAt: env.Now(), Version: "goals_1.sqlite", Feeds: codexGoalsFeeds}
	fail := func(err error) board.Result {
		res.Status = board.StatusDegraded
		res.Detail = err.Error()
		res.Coverage.Unresolved++
		return res
	}
	home := source.Home
	if home == "" {
		dir, err := harness.UserCodexHome()
		if err != nil {
			return fail(err)
		}
		home = dir
	}
	home, err := filepath.Abs(home)
	if err != nil {
		return fail(err)
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		return fail(err)
	}
	res.Namespace = home
	if override := os.Getenv("CODEX_SQLITE_HOME"); override != "" {
		real, err := filepath.EvalSymlinks(override)
		if err != nil || real != home {
			return fail(fmt.Errorf("codex sqlite home override is outside the supported home scope"))
		}
	}
	db := filepath.Join(home, "goals_1.sqlite")
	fresh, _, err := codexGoalsReadable(db)
	if os.IsNotExist(err) {
		res.Status, res.Detail = board.StatusUnavailable, "goals_1.sqlite not found"
		res.Coverage.Unresolved++
		if versions, _ := filepath.Glob(filepath.Join(home, "goals_*.sqlite")); len(versions) > 0 {
			res.Status, res.Detail = board.StatusDegraded, "format "+filepath.Base(versions[0])+" not supported"
		}
		return res
	}
	if err != nil {
		return fail(err)
	}
	res.Detail = "database freshness " + fresh.UTC().Format(time.RFC3339Nano)
	owners, known, err := codexGoalOwners(ctx, env)
	if err != nil {
		return fail(err)
	}
	eligible, coverage, err := codexGoalSessions(ctx, env, home, owners, known)
	res.Coverage = coverage
	if err != nil {
		return fail(err)
	}
	res.Status = board.StatusOK
	if len(eligible) == 0 {
		res.Coverage.Unresolved++
		res.Coverage.Detail = "no eligible native goal containers; payload scope not assessed"
		res.NativeScopes = map[string]board.NativeScope{home: {Complete: false}}
		return res
	}
	ids := make([]string, 0, len(eligible))
	for id := range eligible {
		ids = append(ids, "'"+id+"'")
	}
	sort.Strings(ids)
	// IDs came through CodexGoal's UUID shape check. No payload field, path,
	// or free text is interpolated into SQL.
	query := "PRAGMA query_only=ON; BEGIN; SELECT thread_id, goal_id, objective, status, updated_at_ms FROM thread_goals WHERE thread_id IN (" + strings.Join(ids, ",") + ") ORDER BY thread_id; COMMIT;"
	// Decide the open mode and stamp the main file again just before the
	// query, so a checkpoint during the session walk is not a failed read.
	_, checkpointed, err := codexGoalsReadable(db)
	if err != nil {
		return fail(err)
	}
	mode := "mode=ro"
	if checkpointed != nil {
		mode += "&immutable=1"
	}
	uri := (&url.URL{Scheme: "file", Path: db, RawQuery: mode}).String()
	qctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := env.Run.Run(qctx, env.Dir, []string{"sqlite3", "-batch", "-init", os.DevNull, "-readonly", "-json", "-cmd", ".timeout 500", uri, query})
	if err != nil {
		return fail(fmt.Errorf("could not read the Codex goals database (%s); the next sync reads it again", errLine(err)))
	}
	if checkpointed != nil && !checkpointed.unchanged(db) {
		return fail(fmt.Errorf("the goals database changed during the read; the next sync reads it again"))
	}
	items, err := parseCodexGoals(out, eligible)
	if err != nil {
		return fail(err)
	}
	for i := range items {
		items[i].Namespace = home
	}
	res.Items = items
	res.NativeScopes = map[string]board.NativeScope{home: {Complete: res.Status == board.StatusOK && !res.Coverage.Partial()}}
	return res
}

// codexGoalsReadable checks the file header before any SQLite connection
// and returns the database freshness. A read-only connection to a WAL
// database creates the -wal and -shm sidecars when they are missing, so a
// live read requires both to exist. A WAL database whose -wal is absent is
// fully checkpointed: Codex closed it, and every committed page is in the
// main file. It is read with immutable, which takes no lock and creates no
// sidecar; the returned stamp lets the caller prove after the query that
// no checkpoint or write touched the main file during the unlocked read.
func codexGoalsReadable(path string) (time.Time, *codexGoalsStamp, error) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return time.Time{}, nil, err
	}
	if !info.Mode().IsRegular() {
		return time.Time{}, nil, fmt.Errorf("the Codex goals database is not a regular file")
	}
	var header [100]byte
	if _, err := io.ReadFull(f, header[:]); err != nil {
		return time.Time{}, nil, fmt.Errorf("could not read the Codex goals database header: %w", err)
	}
	if string(header[:16]) != "SQLite format 3\x00" {
		return time.Time{}, nil, fmt.Errorf("the Codex goals database has an unsupported format")
	}
	fresh := info.ModTime()
	if header[18] != 2 && header[19] != 2 {
		return fresh, nil, nil
	}
	if _, err := os.Lstat(path + "-wal"); errors.Is(err, fs.ErrNotExist) {
		return fresh, &codexGoalsStamp{size: info.Size(), mod: info.ModTime()}, nil
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		sidecar, err := os.Open(path + suffix)
		if err != nil {
			return time.Time{}, nil, fmt.Errorf("could not open the Codex goals database %s file: %w", suffix, err)
		}
		stat, statErr := sidecar.Stat()
		_ = sidecar.Close()
		if statErr != nil {
			return time.Time{}, nil, statErr
		}
		if !stat.Mode().IsRegular() {
			return time.Time{}, nil, fmt.Errorf("the Codex goals database %s file is not a regular file", suffix)
		}
		if suffix == "-wal" && stat.ModTime().After(fresh) {
			fresh = stat.ModTime()
		}
	}
	return fresh, nil, nil
}

// codexGoalsStamp is the main file of a checkpointed WAL database before an
// immutable read.
type codexGoalsStamp struct {
	size int64
	mod  time.Time
}

// unchanged reports that the main file kept its size and time through the
// read. A WAL writer appends to -wal and leaves the main file alone, so a
// -wal that appeared during the read leaves the read a consistent earlier
// state; only a checkpoint writes the main file, and then the unlocked read
// may have seen part of it.
func (s codexGoalsStamp) unchanged(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Size() == s.size && info.ModTime().Equal(s.mod)
}

type codexGoalSession struct {
	Owner      board.Owner
	Structural bool
}

func parseCodexGoals(out []byte, eligible map[string]codexGoalSession) ([]board.Item, error) {
	if len(strings.TrimSpace(string(out))) == 0 {
		return nil, nil
	}
	var rows []struct {
		Thread    string `json:"thread_id"`
		Goal      string `json:"goal_id"`
		Objective string `json:"objective"`
		Status    string `json:"status"`
		Updated   int64  `json:"updated_at_ms"`
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("goals_1.sqlite query output unreadable: %w", err)
	}
	items := make([]board.Item, 0, len(rows))
	for _, row := range rows {
		session, ok := eligible[row.Thread]
		if !ok || !board.ValidHarnessID(row.Thread) {
			return nil, fmt.Errorf("goals query returned an ineligible thread")
		}
		ref, ok := board.CodexGoal(row.Goal)
		if !ok {
			return nil, fmt.Errorf("goals query returned an invalid goal UUID")
		}
		state := board.State("")
		switch row.Status {
		case "active":
			state = board.StateActive
		case "blocked", "usage_limited", "budget_limited":
			state = board.StateBlocked
		case "paused":
			state = board.StatePlanned
		case "complete":
			state = board.StateDone
		default:
			return nil, fmt.Errorf("goals_1.sqlite status %q not supported", row.Status)
		}
		at := time.UnixMilli(row.Updated).UTC()
		items = append(items, board.Item{Container: row.Thread, Ref: ref, Title: row.Objective, State: state, Owner: session.Owner, Structural: session.Structural,
			UpdatedAt: at, Reason: board.Reason{Field: "status", Text: "Codex goal " + row.Status, Since: at},
			WaitsOn: []board.Owner{}, Relations: []board.Relation{}, Evidence: []board.Evidence{{Field: "status", Value: row.Status, At: at}}})
	}
	return items, nil
}
