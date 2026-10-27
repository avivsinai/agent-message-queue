package source

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/board"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// ClaudeTasks supports the filesystem task producer shipped in 2.1.310.
// Other storage backends are not inferred from an empty filesystem listing.
type ClaudeTasks struct {
	Home     string
	Bindings map[string]board.ClaudeListBinding
}

func (ClaudeTasks) Name() string                      { return "claude-tasks" }
func (ClaudeTasks) Discover(board.Env) (bool, string) { return true, "" }
func (source ClaudeTasks) readHome(ctx context.Context, env board.Env, budget *claudeReadBudget) board.Result {
	result := board.Result{Status: board.StatusOK, ObservedAt: env.Now(), Version: "Claude Code 2.1.310 filesystem", Feeds: []board.Column{board.ColumnUnclaimed, board.ColumnInFlight, board.ColumnBlocked}, NativeScopes: map[string]board.NativeScope{}}
	gap := func(text string) {
		result.Status = board.StatusDegraded
		result.Coverage.Unresolved++
		if result.Coverage.Detail != "" {
			result.Coverage.Detail += "; "
		}
		if len(result.Coverage.Detail) < 4096 {
			result.Coverage.Detail += text
		}
	}
	home := source.Home
	if home == "" {
		home = os.Getenv("CLAUDE_CONFIG_DIR")
	}
	if home == "" {
		user, err := os.UserHomeDir()
		if err != nil {
			gap(err.Error())
			return result
		}
		home = filepath.Join(user, ".claude")
	}
	absolute, err := filepath.Abs(home)
	if err != nil {
		gap("Claude home path invalid")
		return result
	}
	real, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		gap("Claude home unavailable")
		return result
	}
	home = real
	result.Namespace = home
	version, err := env.Run.Run(ctx, env.Dir, []string{"claude", "--version"})
	if err != nil || strings.TrimSpace(string(version)) != "2.1.310 (Claude Code)" {
		gap("Claude task producer version unsupported")
		return result
	}
	worktrees, err := projectWorktrees(ctx, env)
	if err != nil {
		gap("project scope unavailable: " + err.Error())
		return result
	}
	entries, err := os.ReadDir(filepath.Join(home, "tasks"))
	if err != nil {
		gap("filesystem task lists unavailable; storageV5 coverage unknown")
		return result
	}
	hosts, hostErr := claudeTaskHosts(ctx, env)
	if hostErr != nil {
		gap("host attribution unavailable: " + hostErr.Error())
	}
	count := 0
	for _, entry := range entries {
		if budget.stopped {
			gap("aggregate native acquisition byte budget reached")
			break
		}
		if ctx.Err() != nil {
			gap(ctx.Err().Error())
			break
		}
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		list := entry.Name()
		_, key, err := board.CanonicalClaudeList(home, list)
		if err != nil {
			gap("unsupported list filename")
			continue
		}
		binding, bound := source.Bindings[key]
		if !bound && !claudeDefaultList(ctx, home, list, worktrees, budget) {
			gap("unbound native task list " + list)
			continue
		}
		revision := ""
		if bound {
			revision = binding.Revision
		}
		scope := board.NativeScope{Complete: true, BindingRevision: revision}
		files, err := os.ReadDir(filepath.Join(home, "tasks", list))
		if err != nil {
			scope.Complete = false
			result.NativeScopes[key] = scope
			gap("native list unreadable " + list)
			continue
		}
		for _, file := range files {
			if budget.stopped {
				scope.Complete = false
				gap("aggregate native acquisition byte budget reached")
				break
			}
			if !strings.HasSuffix(file.Name(), ".json") {
				continue
			}
			count++
			if count > 10000 {
				scope.Complete = false
				gap("native task file bound reached")
				break
			}
			path := filepath.Join(home, "tasks", list, file.Name())
			f, info, err := fsq.OpenRegularNoFollowBounded(path, 1<<20)
			if err != nil {
				scope.Complete = false
				gap("native task unreadable " + list + "/" + file.Name())
				continue
			}
			raw, err := io.ReadAll(io.LimitReader(claudeBudgetReader{reader: f, budget: budget}, (1<<20)+1))
			_ = f.Close()
			if err != nil || len(raw) > 1<<20 {
				scope.Complete = false
				gap("native task incomplete " + list + "/" + file.Name())
				continue
			}
			var task struct {
				ID          string         `json:"id"`
				Subject     string         `json:"subject"`
				Description string         `json:"description"`
				ActiveForm  string         `json:"activeForm"`
				Owner       string         `json:"owner"`
				Status      string         `json:"status"`
				Blocks      []string       `json:"blocks"`
				BlockedBy   []string       `json:"blockedBy"`
				Metadata    map[string]any `json:"metadata"`
			}
			var fields map[string]json.RawMessage
			if json.Unmarshal(raw, &task) != nil || json.Unmarshal(raw, &fields) != nil || task.ID != strings.TrimSuffix(file.Name(), ".json") || task.Subject == "" || fields["description"] == nil || fields["blocks"] == nil || fields["blockedBy"] == nil || len(fields["description"]) == 0 || fields["description"][0] != '"' || len(fields["blocks"]) == 0 || fields["blocks"][0] != '[' || len(fields["blockedBy"]) == 0 || fields["blockedBy"][0] != '[' {
				scope.Complete = false
				gap("native task schema unsupported " + list + "/" + file.Name())
				continue
			}
			_, _, err = board.CanonicalClaudeList(home, task.ID)
			if err != nil {
				scope.Complete = false
				gap("native task ID unsupported")
				continue
			}
			state := board.State("")
			switch task.Status {
			case "pending":
				state = board.StateOpenUnowned
			case "in_progress":
				state = board.StateActive
			case "completed":
				state = board.StateDone
			default:
				scope.Complete = false
				gap("native task status unsupported")
				continue
			}
			owner := board.Owner{Kind: board.OwnerNone}
			if task.Owner != "" {
				owner = board.Owner{Kind: board.OwnerUnmapped, Display: task.Owner}
			}
			item := board.Item{Source: "claude-tasks", Namespace: key, Container: list, BindingRevision: revision, NativeOwner: task.Owner, Ref: board.Ref{Kind: "claude-task", ID: task.ID}, Title: task.Subject, State: state, Owner: owner, Structural: true, UpdatedAt: info.ModTime().UTC(), Reason: board.Reason{Field: "status", Text: "Claude task " + task.Status, Since: info.ModTime().UTC()}, Evidence: []board.Evidence{{Field: "status", Value: task.Status, Text: task.Description, At: info.ModTime().UTC()}}}
			if !bound {
				if host, ok := hosts[list]; ok {
					item.HostAttribution = &host
				}
			}
			if task.ActiveForm != "" {
				item.Evidence = append(item.Evidence, board.Evidence{Field: "activeForm", Value: task.ActiveForm})
			}
			if len(task.Metadata) > 0 {
				metadata, _ := json.Marshal(task.Metadata)
				item.Evidence = append(item.Evidence, board.Evidence{Field: "metadata", Value: string(metadata)})
			}
			for _, id := range task.Blocks {
				item.Relations = append(item.Relations, board.Relation{Kind: board.RelationRelated, To: board.Ref{Kind: "claude-task", ID: id}, Basis: "blocks"})
			}
			for _, id := range task.BlockedBy {
				item.Relations = append(item.Relations, board.Relation{Kind: board.RelationRelated, To: board.Ref{Kind: "claude-task", ID: id}, Basis: "blockedBy"})
			}
			result.Items = append(result.Items, item)
		}
		result.NativeScopes[key] = scope
	}
	for key, binding := range source.Bindings {
		if binding.Home == home {
			if _, ok := result.NativeScopes[key]; !ok {
				result.NativeScopes[key] = board.NativeScope{Complete: false, BindingRevision: binding.Revision}
				gap("bound native list missing or unsupported: " + binding.List)
			}
		}
	}
	if budget.stopped {
		gap("aggregate native acquisition byte budget reached")
	}
	// The producer can use an injected storageV5 backend. This adapter supports
	// filesystem records only and cannot certify that other backends are empty.
	gap("filesystem tasks only; storageV5 scopes are not inspected")
	sort.Slice(result.Items, func(i, j int) bool {
		return result.Items[i].Namespace+result.Items[i].Ref.ID < result.Items[j].Namespace+result.Items[j].Ref.ID
	})
	return result
}

// A default list must match a real session in this exact home and a project
// cwd from its transcript. Team/custom names require an owner binding.
func claudeDefaultList(ctx context.Context, home, list string, worktrees map[string]bool, budget *claudeReadBudget) bool {
	if !board.ValidHarnessID(list) {
		return false
	}
	paths, err := filepath.Glob(filepath.Join(home, "projects", "*", list+".jsonl"))
	if err != nil {
		return false
	}
	found := false
	for _, path := range paths {
		f, _, err := fsq.OpenRegularNoFollowBounded(path, 1<<40)
		if err != nil {
			return false
		}
		scan := bufio.NewScanner(io.LimitReader(claudeBudgetReader{reader: f, budget: budget}, 1<<20))
		scan.Buffer(make([]byte, 4096), 1<<20)
		n := 0
		for scan.Scan() && n < 100 {
			n++
			if ctx.Err() != nil {
				_ = f.Close()
				return false
			}
			var record struct {
				SessionID string `json:"sessionId"`
				Cwd       string `json:"cwd"`
				Version   string `json:"version"`
			}
			if json.Unmarshal(scan.Bytes(), &record) != nil {
				continue
			}
			if record.SessionID == list && record.Cwd != "" {
				cwd, err := filepath.EvalSymlinks(record.Cwd)
				if err != nil || !worktrees[cwd] || record.Version != "2.1.310" {
					_ = f.Close()
					return false
				}
				found = true
			}
		}
		_ = f.Close()
	}
	return found
}

func (source ClaudeTasks) Read(ctx context.Context, env board.Env) board.Result {
	budget := &claudeReadBudget{remaining: claudeAcquisitionByteLimit}
	result := source.readHome(ctx, env, budget)
	seen := map[string]bool{result.Namespace: true}
	var homes []string
	for _, binding := range source.Bindings {
		if !seen[binding.Home] {
			seen[binding.Home] = true
			homes = append(homes, binding.Home)
		}
	}
	sort.Strings(homes)
	for i, home := range homes {
		if budget.stopped {
			result.Status = board.StatusDegraded
			result.Coverage.Truncated += len(homes) - i
			result.Coverage.Detail += "; aggregate native acquisition byte budget reached"
			break
		}
		if i >= 16 {
			result.Status = board.StatusDegraded
			result.Coverage.Truncated += len(homes) - i
			result.Coverage.Detail += "; native home bound reached"
			break
		}
		next := (ClaudeTasks{Home: home, Bindings: source.Bindings}).readHome(ctx, env, budget)
		result.Items = append(result.Items, next.Items...)
		for key, scope := range next.NativeScopes {
			result.NativeScopes[key] = scope
		}
		result.Coverage.Skipped += next.Coverage.Skipped
		result.Coverage.Unresolved += next.Coverage.Unresolved
		result.Coverage.Truncated += next.Coverage.Truncated
		if next.Status != board.StatusOK {
			result.Status = board.StatusDegraded
		}
		if next.Coverage.Detail != "" {
			result.Coverage.Detail += "; " + next.Coverage.Detail
		}
	}
	return result
}

func claudeTaskHosts(ctx context.Context, env board.Env) (map[string]board.Owner, error) {
	raw, err := env.Run.Run(ctx, env.Dir, []string{"amq", "session", "list", "--json"})
	if err != nil {
		return nil, err
	}
	var listing struct {
		Sessions []struct {
			Name          string                                         `json:"name"`
			Conversations []struct{ Handle, Provider, ID, State string } `json:"conversations"`
		} `json:"sessions"`
	}
	if err = json.Unmarshal(raw, &listing); err != nil {
		return nil, err
	}
	owners := map[string]board.Owner{}
	for _, session := range listing.Sessions {
		for _, conversation := range session.Conversations {
			if conversation.Provider != "claude" || conversation.State != "ready" || !board.ValidHarnessID(conversation.ID) || fsq.ValidateHandle(conversation.Handle) != nil {
				continue
			}
			owner := board.Owner{Kind: board.OwnerAgent, Handle: conversation.Handle, Session: session.Name}
			if prior, ok := owners[conversation.ID]; ok && prior != owner {
				owner = board.Owner{Kind: board.OwnerUnmapped, Display: "ambiguous native container attribution"}
			}
			owners[conversation.ID] = owner
		}
	}
	return owners, nil
}

const claudeAcquisitionByteLimit = 16 << 20

var errClaudeBudget = errors.New("aggregate native acquisition byte budget reached")

type claudeReadBudget struct {
	remaining int64
	stopped   bool
}
type claudeBudgetReader struct {
	reader io.Reader
	budget *claudeReadBudget
}

func (r claudeBudgetReader) Read(p []byte) (int, error) {
	if r.budget.remaining == 0 {
		r.budget.stopped = true
		return 0, errClaudeBudget
	}
	if int64(len(p)) > r.budget.remaining {
		p = p[:r.budget.remaining]
	}
	n, err := r.reader.Read(p)
	r.budget.remaining -= int64(n)
	return n, err
}
