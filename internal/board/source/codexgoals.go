package source

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/board"
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
		home = os.Getenv("CODEX_HOME")
	}
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return fail(err)
		}
		home = filepath.Join(userHome, ".codex")
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
	fresh, err := codexGoalsReadable(db)
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
	uri := (&url.URL{Scheme: "file", Path: db, RawQuery: "mode=ro"}).String()
	qctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := env.Run.Run(qctx, env.Dir, []string{"sqlite3", "-batch", "-init", os.DevNull, "-readonly", "-json", "-cmd", ".timeout 500", uri, query})
	if err != nil {
		return fail(fmt.Errorf("read Codex goals: %w", err))
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

// codexGoalsReadable checks the file header before any SQLite connection.
// Read-only WAL connections can create sidecars; require both to exist and
// be readable before opening a WAL database. Never use immutable on live data.
func codexGoalsReadable(path string) (time.Time, error) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return time.Time{}, err
	}
	if !info.Mode().IsRegular() {
		return time.Time{}, fmt.Errorf("goals database is not a regular file")
	}
	var header [100]byte
	if _, err := io.ReadFull(f, header[:]); err != nil {
		return time.Time{}, fmt.Errorf("read goals database header: %w", err)
	}
	if string(header[:16]) != "SQLite format 3\x00" {
		return time.Time{}, fmt.Errorf("goals database has an unsupported format")
	}
	fresh := info.ModTime()
	if header[18] != 2 && header[19] != 2 {
		return fresh, nil
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		sidecar, err := os.Open(path + suffix)
		if err != nil {
			return time.Time{}, fmt.Errorf("WAL database requires readable existing %s: %w", suffix, err)
		}
		stat, statErr := sidecar.Stat()
		_ = sidecar.Close()
		if statErr != nil {
			return time.Time{}, statErr
		}
		if !stat.Mode().IsRegular() {
			return time.Time{}, fmt.Errorf("goals database %s is not a regular file", suffix)
		}
		if suffix == "-wal" && stat.ModTime().After(fresh) {
			fresh = stat.ModTime()
		}
	}
	return fresh, nil
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
