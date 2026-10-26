package board

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/lock"
	"github.com/avivsinai/agent-message-queue/internal/thread"
)

const SnapshotSchema = "amq/board-state/v2"
const legacySnapshotSchema = "amq/board-state/v1"
const snapshotLimit = 64 << 20
const snapshotReserve = 1 << 20

var ErrProjectChanged = errors.New("board project identity changed; restart required")

var errSnapshotBound = errors.New("board snapshot capacity reached; ingestion stopped")

const evidenceLimit = 256 << 20

var errEvidenceBound = errors.New("board evidence bound reached; ingestion stopped")

// Store is bound to the authenticated canonical base root for its lifetime.
// It never repairs an existing store or restores a backup implicitly.
type Store struct {
	Dir, BaseRoot, BaseRootID, HostID string
	// ProjectID pins a published project for a long-lived reader or acquisition.
	ProjectID string
}

type Snapshot struct {
	Facts           map[string]AliasFact         `json:"source_facts"`
	ClaudeLists     map[string]ClaudeListBinding `json:"claude_lists,omitempty"`
	Schema          string                       `json:"schema"`
	BaseRoot        string                       `json:"base_root"`
	BaseRootID      string                       `json:"base_root_id"`
	ProjectID       string                       `json:"project_id"`
	HostID          string                       `json:"host_id"`
	Generation      uint64                       `json:"generation"`
	PublishedAt     time.Time                    `json:"published_at"`
	Input           Input                        `json:"input"`
	Usage           *UsageView                   `json:"usage,omitempty"`
	Tasks           map[string]Task              `json:"tasks"`
	Anchors         map[string]string            `json:"admission_anchors"`
	Aliases         map[string]string            `json:"aliases"`
	Messages        map[string]Message           `json:"messages"`
	Dismissed       map[string]bool              `json:"dismissed"`
	ActionCancelled map[string]bool              `json:"action_cancelled"`
	Withdrawals     map[string]string            `json:"withdrawal_adoptions"`
}

type Task struct {
	ID               string    `json:"id"`
	Title            string    `json:"title"`
	Authority        string    `json:"authority"`
	Fact             Item      `json:"-"`
	OwnerFact        Item      `json:"owner_fact,omitzero"`
	RedirectTo       string    `json:"redirect_to,omitempty"`
	AdmissionAnchors []string  `json:"admission_anchors,omitempty"`
	Aliases          []string  `json:"aliases"`
	Attachments      []string  `json:"attachments"`
	Cancelled        bool      `json:"owner_cancelled"`
	ObservedAt       time.Time `json:"-"`
	SourceMissing    bool      `json:"-"`
}

// Message is one exact exported message, scoped to its source root.
type MessageCopy struct {
	Root    string   `json:"root"`
	Session string   `json:"session"`
	Agents  []string `json:"agents"`
}

type Message struct {
	Namespace    string         `json:"namespace"`
	Copies       []MessageCopy  `json:"observed_copies"`
	Key          string         `json:"key"`
	Root         string         `json:"root"`
	Session      string         `json:"session"`
	ID           string         `json:"id"`
	Thread       string         `json:"thread"`
	From         string         `json:"from"`
	To           []string       `json:"to"`
	Subject      string         `json:"subject"`
	Kind         string         `json:"kind"`
	Body         string         `json:"body,omitempty"`
	Priority     string         `json:"priority,omitempty"`
	Labels       []string       `json:"labels,omitempty"`
	Refs         []string       `json:"refs,omitempty"`
	ReplyTo      string         `json:"reply_to,omitempty"`
	ReplyProject string         `json:"reply_project,omitempty"`
	FromProject  string         `json:"from_project,omitempty"`
	Context      map[string]any `json:"context,omitempty"`
	Created      string         `json:"created"`
	EvidenceHash string         `json:"evidence_hash"`
}

// Acquisition is fetched without a store lock. Commit reloads the current
// generation so an owner correction made during acquisition is retained.
type Acquisition struct {
	ProjectID  string
	Input      Input
	Usage      *UsageView
	Messages   []Message
	Evidence   [][]byte
	BeadsStore string
}

func NewStore(base, identity, hostID string) (Store, error) {
	if base == "" || identity == "" || hostID == "" {
		return Store{}, errors.New("AMQ canonical base_root and base_root_id are required")
	}
	real, err := filepath.EvalSymlinks(base)
	if err != nil {
		return Store{}, err
	}
	real, err = filepath.Abs(real)
	if err != nil {
		return Store{}, err
	}
	return Store{Dir: filepath.Join(real, "extensions", "board"), BaseRoot: real, BaseRootID: identity, HostID: hostID}, nil
}

func randomID(prefix string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b[:]), nil
}

func (s Store) Init(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := s.validateIdentity(ctx); err != nil {
		return err
	}
	parent := filepath.Dir(s.Dir)
	if info, err := os.Lstat(parent); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return errors.New("unsafe board extension parent")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	// An existing directory includes interrupted initialization. Never replace it.
	if err := os.Mkdir(s.Dir, 0o700); err != nil {
		return fmt.Errorf("board initialization refused; existing or interrupted store needs recovery: %w", err)
	}
	if err := fsq.SyncDir(parent); err != nil {
		return err
	}
	return lock.WithExclusiveFileLockUntil(filepath.Join(s.Dir, "writer.lock"), func() bool { return ctx.Err() != nil }, func() error {
		project, err := randomID("project-")
		if err != nil {
			return err
		}

		state := Snapshot{Facts: map[string]AliasFact{}, Schema: SnapshotSchema, BaseRoot: s.BaseRoot, BaseRootID: s.BaseRootID, ProjectID: project, HostID: s.HostID, Tasks: map[string]Task{}, Anchors: map[string]string{}, Aliases: map[string]string{}, Messages: map[string]Message{}, Dismissed: map[string]bool{}, ActionCancelled: map[string]bool{}, Withdrawals: map[string]string{}}
		return s.publish(&state)
	})
}

func readBounded(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("unsafe board file: %s", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > limit {
		return nil, fmt.Errorf("invalid or oversized board file: %s", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("board snapshot bound exceeded")
	}
	return b, nil
}

func (s Store) safeDirectory() error {
	for _, dir := range []string{filepath.Dir(s.Dir), s.Dir} {
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("board directory missing or unsafe; recovery required")
		}
	}
	if info, err := os.Lstat(filepath.Join(s.Dir, "writer.lock")); err == nil && !info.Mode().IsRegular() {
		return errors.New("board writer lock unsafe")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s Store) Read() (Snapshot, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return s.ReadContext(ctx)
}

func (s Store) ReadContext(ctx context.Context) (Snapshot, error) {
	var state Snapshot
	if err := s.validateIdentity(ctx); err != nil {
		return state, err
	}
	if err := s.safeDirectory(); err != nil {
		return state, err
	}
	err := lock.WithExclusiveFileLockUntil(filepath.Join(s.Dir, "writer.lock"), func() bool { return ctx.Err() != nil }, func() error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var err error
		if err = s.validateIdentity(ctx); err != nil {
			return err
		}
		state, err = s.readUnlocked(ctx)
		return err
	})
	return state, err
}

func (s Store) readUnlocked(ctx context.Context) (Snapshot, error) {
	var state Snapshot
	if err := s.safeDirectory(); err != nil {
		return state, err
	}
	if _, err := os.Lstat(filepath.Join(s.Dir, "write-pending")); !os.IsNotExist(err) {
		return state, errors.New("board publication interrupted; recovery required")
	}
	data, err := readBounded(filepath.Join(s.Dir, "snapshot.json"), snapshotLimit)
	if err != nil {
		return state, fmt.Errorf("board snapshot recovery required: %w", err)
	}
	if err = json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("board snapshot recovery required: %w", err)
	}
	if state.Schema == legacySnapshotSchema {
		if err := state.migrateLegacy(data); err != nil {
			return Snapshot{}, err
		}
	}
	if state.Schema != SnapshotSchema || state.BaseRoot != s.BaseRoot || state.BaseRootID != s.BaseRootID || state.ProjectID == "" || state.HostID != s.HostID || state.Generation == 0 || state.Tasks == nil || state.Anchors == nil || state.Aliases == nil || state.Messages == nil || state.Dismissed == nil || state.ActionCancelled == nil || state.Withdrawals == nil || state.Facts == nil {
		return Snapshot{}, errors.New("board identity or state invalid; recovery required")
	}
	if s.ProjectID != "" && state.ProjectID != s.ProjectID {
		return Snapshot{}, ErrProjectChanged
	}
	if state.ClaudeLists == nil {
		state.ClaudeLists = map[string]ClaudeListBinding{}
	}
	for key, binding := range state.ClaudeLists {
		if key != ClaudeListKey(binding.Home, binding.List) || binding.Revision == "" {
			return Snapshot{}, errors.New("claude list binding invalid; recovery required")
		}
	}
	for anchor, id := range state.Anchors {
		if _, ok := state.Tasks[id]; !ok || anchor == "" {
			return Snapshot{}, errors.New("board admission identity invalid; recovery required")
		}
	}
	for alias, id := range state.Aliases {
		if _, ok := state.Tasks[id]; !ok || alias == "" {
			return Snapshot{}, errors.New("board alias identity invalid; recovery required")
		}
	}
	for id, task := range state.Tasks {
		if task.ID != id || task.Title == "" || task.Authority == "" {
			return Snapshot{}, errors.New("board task identity invalid; recovery required")
		}
		for _, key := range task.Attachments {
			if _, ok := state.Messages[key]; !ok {
				return Snapshot{}, errors.New("board attachment missing; recovery required")
			}
		}
	}
	for key, msg := range state.Messages {
		if key != msg.Key || msg.ID == "" || msg.Root == "" {
			return Snapshot{}, errors.New("board message identity invalid; recovery required")
		}
	}
	if err := state.validateIdentityRecords(); err != nil {
		return Snapshot{}, err
	}
	state.hydrateAuthorities()
	if err := s.verifyMessages(ctx, &state); err != nil {
		return Snapshot{}, err
	}
	return state, nil
}

func (s Store) update(ctx context.Context, change func(*Snapshot) error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := s.safeDirectory(); err != nil {
		return err
	}
	return lock.WithExclusiveFileLockUntil(filepath.Join(s.Dir, "writer.lock"), func() bool { return ctx.Err() != nil }, func() error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := s.validateIdentity(ctx); err != nil {
			return err
		}
		state, err := s.readUnlocked(ctx)
		if err != nil {
			return err
		}
		if err = change(&state); err != nil {
			return err
		}
		if err = state.validateIdentityRecords(); err != nil {
			return err
		}
		state.hydrateAuthorities()
		if err = s.verifyMessages(ctx, &state); err != nil {
			return err
		}
		data, err := snapshotBytes(&state)
		if err != nil {
			return err
		}
		if len(data) > snapshotLimit-snapshotReserve {
			prior, err := s.readUnlocked(ctx)
			if err != nil {
				return err
			}
			markCapacity(&prior, "board-capacity", errSnapshotBound.Error())
			if err = s.publish(&prior); err != nil {
				return err
			}
			return errSnapshotBound
		}
		return s.publish(&state)
	})
}

func (s Store) publish(state *Snapshot) error {
	state.Generation++
	state.PublishedAt = time.Now().UTC()
	data, err := snapshotBytes(state)
	if err != nil {
		return err
	}
	if len(data) > snapshotLimit {
		return errors.New("board snapshot reached 64 MiB; ingestion stopped, existing state preserved")
	}
	// A durable marker makes an interrupted or uncertain rename fail closed on
	// the next writer. Readers never fall back to source reads or a backup.
	if _, err = fsq.WriteFileAtomic(s.Dir, "write-pending", []byte("publication pending\n"), 0o600); err != nil {
		return err
	}
	old, err := readBounded(filepath.Join(s.Dir, "snapshot.json"), snapshotLimit)
	if err == nil {
		if _, err = fsq.WriteFileAtomic(s.Dir, "snapshot.backup.json", old, 0o600); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, err = fsq.WriteFileAtomic(s.Dir, "snapshot.json", append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("board durability uncertain; stop writes and recover: %w", err)
	}
	if err = os.Remove(filepath.Join(s.Dir, "write-pending")); err != nil {
		return err
	}
	if err = fsq.SyncDir(s.Dir); err != nil {
		_, _ = fsq.WriteFileAtomic(s.Dir, "write-pending", []byte("durability uncertain\n"), 0o600)
		return fmt.Errorf("board durability uncertain; stop writes: %w", err)
	}
	return nil
}

func (s Store) saveEvidence(ctx context.Context, blobs [][]byte) error {
	dir := filepath.Join(s.Dir, "evidence")
	if info, err := os.Lstat(dir); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return errors.New("unsafe board evidence directory")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var used int64
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			return err
		}
		used += info.Size()
	}
	for _, b := range blobs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := s.safeDirectory(); err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		name := hex.EncodeToString(sum[:]) + ".json"
		if previous, err := readBounded(filepath.Join(dir, name), snapshotLimit); err == nil {
			if string(previous) != string(b) {
				return errors.New("board evidence hash conflict")
			}
			continue
		} else if !os.IsNotExist(err) {
			return err
		}
		if len(b) > snapshotLimit || used+int64(len(b)) > evidenceLimit {
			return errEvidenceBound
		}
		if _, err = fsq.WriteFileAtomic(dir, name, b, 0o600); err != nil {
			return err
		}
		used += int64(len(b))
	}
	return fsq.SyncDir(s.Dir)
}

func (s Store) Commit(ctx context.Context, a Acquisition) error {
	if a.ProjectID == "" {
		return errors.New("acquisition requires the published project identity captured before source reads")
	}
	if s.ProjectID != "" && s.ProjectID != a.ProjectID {
		return ErrProjectChanged
	}
	s.ProjectID = a.ProjectID
	stageErr := s.stageEvidence(ctx, a.Evidence)
	if stageErr != nil && !errors.Is(stageErr, errEvidenceBound) {
		return stageErr
	}
	stopped := false
	err := s.update(ctx, func(state *Snapshot) error {
		if a.BeadsStore == "" {
			for _, nr := range a.Input.Results {
				if nr.Name == "beads" && len(nr.Result.Items) > 0 {
					return errors.New("beads facts require a bound source store")
				}
			}
		}
		if err := stageErr; err != nil {
			if !errors.Is(err, errEvidenceBound) {
				return err
			}
			stopped = true
			// Do not publish references to any partially ingested evidence. Keep prior
			// facts and identities, and publish the failed acquisition coverage itself.
			found := false
			for i := range state.Input.Results {
				if state.Input.Results[i].Name == "amq-history" {
					r := &state.Input.Results[i].Result
					r.Status = StatusDegraded
					r.ObservedAt = time.Now().UTC()
					r.Detail = err.Error()
					r.Coverage.Unresolved++
					r.Coverage.Detail = err.Error()
					found = true
				}
			}
			if !found {
				state.Input.Results = append(state.Input.Results, NamedResult{Name: "amq-history", Result: Result{Status: StatusDegraded, ObservedAt: time.Now().UTC(), Detail: err.Error(), Coverage: Coverage{Unresolved: 1, Detail: err.Error()}}})
			}
			return nil
		}
		for i := range a.Input.Results {
			nr := &a.Input.Results[i]
			if nr.Name != "claude-tasks" {
				continue
			}
			var admitted []Item
			for _, it := range nr.Result.Items {
				if it.BindingRevision != "" {
					binding, ok := state.ClaudeLists[it.Namespace]
					if !ok || binding.Revision != it.BindingRevision {
						nr.Result.Status = StatusDegraded
						nr.Result.Coverage.Unresolved++
						nr.Result.Coverage.Detail = "Claude list binding changed during acquisition"
						scope := nr.Result.NativeScopes[it.Namespace]
						scope.Complete = false
						nr.Result.NativeScopes[it.Namespace] = scope
						continue
					}
				}
				admitted = append(admitted, it)
			}
			for namespace, scope := range nr.Result.NativeScopes {
				if scope.BindingRevision != "" {
					binding, ok := state.ClaudeLists[namespace]
					if !ok || binding.Revision != scope.BindingRevision {
						scope.Complete = false
						nr.Result.NativeScopes[namespace] = scope
					}
				}
			}
			nr.Result.Items = admitted
		}
		previous := map[string]NamedResult{}
		for _, nr := range state.Input.Results {
			previous[nr.Name] = nr
		}
		stale := map[string]bool{}
		for i, nr := range a.Input.Results {
			if old, ok := previous[nr.Name]; ok && nr.Result.ObservedAt.Before(old.Result.ObservedAt) {
				a.Input.Results[i] = old
				stale[nr.Name] = true
			}
		}
		known := map[string]string{}
		for id, status := range state.Input.Known {
			known[id] = status
		}
		for id, status := range a.Input.Known {
			known[id] = status
		}
		a.Input.Known = known
		state.Input = a.Input
		state.Usage = a.Usage
		for _, m := range a.Messages {
			if previous, ok := state.Messages[m.Key]; ok {
				before, after := previous, m
				before.EvidenceHash = ""
				after.EvidenceHash = ""
				before.Root = ""
				after.Root = ""
				before.Session = ""
				after.Session = ""
				before.Copies = nil
				after.Copies = nil
				a, _ := json.Marshal(before)
				b, _ := json.Marshal(after)
				if string(a) != string(b) {
					return fmt.Errorf("published message changed content: %s", m.Key)
				}
			}
			if previous, ok := state.Messages[m.Key]; ok {
				seen := map[string]bool{}
				for _, copy := range m.Copies {
					seen[copy.Root+"#"+copy.Session] = true
				}
				for _, copy := range previous.Copies {
					if !seen[copy.Root+"#"+copy.Session] {
						m.Copies = append(m.Copies, copy)
					}
				}
			}
			state.Messages[m.Key] = m
		}
		for sourceIndex, nr := range a.Input.Results {
			if nr.Name != "beads" && nr.Name != "codex-goals" && nr.Name != "claude-tasks" || stale[nr.Name] {
				continue
			}
			scopes := nr.Result.NativeScopes
			if nr.Name == "beads" {
				scopes = map[string]NativeScope{a.BeadsStore: {Complete: nr.Result.Status == StatusOK && !nr.Result.Coverage.Partial()}}
			}
			present := map[string]bool{}
			for _, it := range nr.Result.Items {
				it.Source = nr.Name
				if nr.Name == "beads" {
					it.Namespace = a.BeadsStore
				}
				alias, ok := nativeAlias(nr.Name, it)
				_, scoped := scopes[it.Namespace]
				if !ok || !scoped {
					a.Input.Results[sourceIndex].Result.Status = StatusDegraded
					a.Input.Results[sourceIndex].Result.Coverage.Skipped++
					a.Input.Results[sourceIndex].Result.Coverage.Detail = "native fact identity or scope rejected"
					scope := scopes[it.Namespace]
					scope.Complete = false
					scopes[it.Namespace] = scope
					continue
				}
				present[alias] = true
				id := state.Aliases[alias]
				if id == "" {
					var err error
					id, err = randomID("task-")
					if err != nil {
						return err
					}
					state.Anchors[alias] = id
					state.Aliases[alias] = id
					state.Tasks[id] = Task{ID: id, Title: it.Title, Authority: alias, Aliases: []string{alias}, AdmissionAnchors: []string{alias}}
				}
				canonical, err := state.ResolveTaskID(id)
				if err != nil {
					return err
				}
				state.Aliases[alias] = canonical
				old := state.Facts[alias]
				if !nr.Result.ObservedAt.Before(old.ObservedAt) {
					state.Facts[alias] = AliasFact{Alias: alias, Fact: it, ObservedAt: nr.Result.ObservedAt}
				}

			}
			for namespace, scope := range scopes {
				if !scope.Complete {
					continue
				}
				for alias, fact := range state.Facts {
					if fact.Fact.Source == nr.Name && fact.Fact.Namespace == namespace && !present[alias] {
						fact.Missing = true
						state.Facts[alias] = fact
					}
				}

			}
		}
		return nil
	})
	if err == nil && stopped {
		return errEvidenceBound
	}
	return err
}

// OwnerAction applies one exact local operator correction. Source reports
// cannot clear cancellations or override these bindings.
type OwnerAction struct{ Kind, TaskID, FromTaskID, MessageKey, Anchor, Title, Scope string }

func (s Store) Apply(ctx context.Context, a OwnerAction) (string, error) {
	id := a.TaskID
	err := s.update(ctx, func(state *Snapshot) error {
		if id != "" {
			var err error
			id, err = state.ResolveTaskID(id)
			if err != nil {
				return err
			}
		}
		if a.FromTaskID != "" {
			var err error
			a.FromTaskID, err = state.ResolveTaskID(a.FromTaskID)
			if err != nil {
				return err
			}
		}
		switch a.Kind {
		case "admit":
			m, ok := state.Messages[a.MessageKey]
			if !ok {
				return errors.New("exact message key not found in published history")
			}
			anchor := a.Anchor
			if anchor == "" {
				anchor = a.MessageKey
			}
			anchor = "operator:" + anchor
			if existing := state.Anchors[anchor]; existing != "" {
				var err error
				id, err = state.ResolveTaskID(existing)
				return err
			}
			if strings.TrimSpace(a.Title) == "" || hasControl(a.Title) || len(a.Title) > 4096 || hasControl(anchor) || len(anchor) > 4096 {
				return errors.New("admission needs an explicit outcome title")
			}
			var err error
			id, err = randomID("task-")
			if err != nil {
				return err
			}
			state.Anchors[anchor] = id
			state.Tasks[id] = Task{ID: id, Title: a.Title, Authority: "operator", AdmissionAnchors: []string{anchor}, OwnerFact: Item{Title: a.Title, State: StateRequested, Owner: Owner{Kind: OwnerNone}, Structural: true, Source: "operator", UpdatedAt: time.Now().UTC(), Reason: Reason{Field: "admission", Text: "Explicitly admitted; next action needs owner assignment", Since: time.Now().UTC()}}, Attachments: []string{m.Key}}
			state.Dismissed[m.Key] = false
		case "attach", "detach", "move":
			task, ok := state.Tasks[id]
			if !ok {
				return errors.New("task not found")
			}
			if _, ok := state.Messages[a.MessageKey]; !ok {
				return errors.New("message not found")
			}
			remove := func(task Task) Task {
				var kept []string
				for _, key := range task.Attachments {
					if key != a.MessageKey {
						kept = append(kept, key)
					}
				}
				task.Attachments = kept
				return task
			}
			if a.Kind == "move" {
				from, ok := state.Tasks[a.FromTaskID]
				if !ok || a.FromTaskID == id {
					return errors.New("move needs a different existing --from-task")
				}
				hasBinding := false
				for _, key := range from.Attachments {
					if key == a.MessageKey {
						hasBinding = true
					}
				}
				if !hasBinding {
					return errors.New("source task does not contain this attachment")
				}
				state.Tasks[a.FromTaskID] = remove(from)
			}
			if a.Kind == "detach" {
				task = remove(task)
			} else {
				found := false
				for _, key := range task.Attachments {
					if key == a.MessageKey {
						found = true
					}
				}
				if !found {
					task.Attachments = append(task.Attachments, a.MessageKey)
				}
			}
			state.Tasks[id] = task
			if a.Kind != "detach" {
				state.Dismissed[a.MessageKey] = false
			}

		case "dismiss":
			if _, ok := state.Messages[a.MessageKey]; !ok {
				return errors.New("message not found")
			}
			state.Dismissed[a.MessageKey] = true
		case "cancel", "reopen":
			cancelled := a.Kind == "cancel"
			switch a.Scope {
			case "task":
				task, ok := state.Tasks[id]
				if !ok {
					return errors.New("task not found")
				}
				task.Cancelled = cancelled
				state.Tasks[id] = task
			case "action":
				if _, ok := state.Messages[a.MessageKey]; !ok {
					return errors.New("message not found")
				}
				state.ActionCancelled[a.MessageKey] = cancelled
			default:
				return errors.New("cancel/reopen needs --scope task or --scope action")
			}
		default:
			return errors.New("unknown owner action")
		}
		return nil
	})
	return id, err
}

// Project is pure: one admitted task produces one card, regardless of source
// link graphs. Unadmitted asks remain outside task and WIP counts.
func (s Snapshot) Project(cfg Config, now time.Time, version string) Model {
	s = s.projectionSnapshot()
	input := s.Input
	input.Results = append([]NamedResult(nil), s.Input.Results...)
	input.Config = cfg
	input.Now = now
	input.Version = version
	for i := range input.Results {
		input.Results[i].Result.Items = nil
	}
	m := Build(input)
	m.Usage = s.Usage
	m.Generation = s.Generation
	m.PublishedAt = s.PublishedAt
	m.GeneratedAt = s.PublishedAt
	for i := range m.Columns {
		m.Columns[i].Cards = []Card{}
	}
	m.Unresolved = []Message{}
	m.Historical = []Card{}
	m.Catalogue = s.Catalogue()
	m.Redirects = s.redirects()
	attached := map[string]bool{}
	ids := make([]string, 0, len(s.Tasks))
	for id := range s.Tasks {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		task := s.Tasks[id]
		if task.RedirectTo != "" {
			continue
		}
		for _, key := range task.Attachments {
			attached[key] = true
		}
		if task.Cancelled {
			continue
		}
		it := task.Fact
		it.Title = task.Title
		if task.Authority == "operator" {
			it.Ref = Ref{Kind: "task", ID: id}
		}
		it.Mentions = nil
		col, _ := Place([]Item{it})
		if task.SourceMissing {
			col = ColumnHistorical
		}
		if col == ColumnHidden {
			continue
		}
		if task.Authority == "operator" {
			col = ColumnUnclaimed
		}
		card := Card{ID: id, Title: task.Title, Column: col, Reason: it.Reason, ReasonSource: it.Source, Items: []Item{it}, Owners: []Owner{}, WaitsOn: it.WaitsOn, Marks: []string{}, Actions: []Action{}, Refs: []Ref{}, UpdatedAt: it.UpdatedAt, Aliases: task.Aliases}
		card.SourceObservedAt = task.ObservedAt
		card.Authority = task.Authority
		for _, alias := range task.Aliases {
			fact := s.Facts[alias]
			card.SourceFacts = append(card.SourceFacts, fact)
			if alias != task.Authority {
				secondary := fact.Fact
				secondary.Evidence = append(append([]Evidence{}, secondary.Evidence...), Evidence{Field: "source_title", Value: secondary.Title}, Evidence{Field: "authority", Value: "secondary alias", Text: alias})
				card.Items = append(card.Items, secondary)
				if fact.Fact.State != it.State {
					card.Marks = append(card.Marks, "source disagreement: "+alias+" reports "+string(fact.Fact.State))
				}
			}
		}
		if task.SourceMissing {
			card.Historical = true
			card.Reason.Text = "Historical; source no longer reports this task. Last recorded: " + card.Reason.Text
			card.Marks = append(card.Marks, "source no longer reports this task; last recorded state retained")
		}
		if it.Owner.Kind != OwnerNone {
			card.Owners = append(card.Owners, it.Owner)
		}
		if it.HostAttribution != nil {
			card.Marks = append(card.Marks, "container attribution: "+ownerLabel(*it.HostAttribution, nil))
		}
		if it.NativeOwner != "" {
			card.Marks = append(card.Marks, "native owner: "+it.NativeOwner)
		}
		if it.Container != "" {
			card.Related = append(card.Related, Token{Ref: Ref{Kind: "container", ID: it.Container}, Basis: "native context"})
		}
		if it.State == StateActive {
			card.Marks = append(card.Marks, "tracker progress; execution not observed")
		}
		for _, alias := range task.Aliases {
			if strings.HasPrefix(alias, "beads:") {
				fact := s.Facts[alias].Fact
				card.Refs = append(card.Refs, fact.Ref)
				if action, ok := ShowBead(fact.Ref.ID, input.Known); ok {
					if fact.Namespace != "" && !hasControl(fact.Namespace) {
						action = newAction("show bead", "bd", "--readonly", "--sandbox", "--db", fact.Namespace, "show", fact.Ref.ID)
					}
					card.Actions = append(card.Actions, action)
				}
			}
		}
		for _, key := range task.Attachments {
			message, ok := s.Messages[key]
			if !ok {
				continue
			}
			card.Attachments = append(card.Attachments, Attachment{Message: message, Cancelled: s.ActionCancelled[key]})
			card.Actions = append(card.Actions, messageActions(message)...)
			card.Actions = append(card.Actions, newOwnerAction("Detach this evidence from this task", "amq-board", "detach", "--task", id, "--message", key))
			operation := "cancel"
			if s.ActionCancelled[key] {
				operation = "reopen"
			}
			card.Actions = append(card.Actions, newOwnerAction(operation+" this request action", "amq-board", operation, "--scope", "action", "--message", key))
		}
		card.Actions = append(card.Actions, newOwnerAction("Cancel this task", "amq-board", "cancel", "--scope", "task", "--task", id))
		if card.Historical {
			m.Historical = append(m.Historical, card)
			continue
		}
		for i := range m.Columns {
			if m.Columns[i].ID == col {
				m.Columns[i].Cards = append(m.Columns[i].Cards, card)
			}
		}
	}
	actionStates := map[string]string{}
	for _, nr := range s.Input.Results {
		for key, status := range nr.Result.ActionStates {
			actionStates[key] = status
		}
	}
	m.ActionStates = actionStates
	for key, msg := range s.Messages {
		if actionStates[key] == "resolved" {
			continue
		}
		if !attached[key] && !s.Dismissed[key] && !s.ActionCancelled[key] && (msg.Kind == "question" || msg.Kind == "review_request" || msg.Kind == "todo") {
			m.Unresolved = append(m.Unresolved, msg)
		}
	}
	sort.Slice(m.Unresolved, func(i, j int) bool { return m.Unresolved[i].Key < m.Unresolved[j].Key })
	for i := range m.Columns {
		sortCards(m.Columns[i].ID, m.Columns[i].Cards)
	}
	m.Next = selectNext(m.Columns)
	return m
}

type Attachment struct {
	Message   Message `json:"message"`
	Cancelled bool    `json:"owner_cancelled"`
}

// WithdrawalAdoption names the exact target and the exact withdrawal. The
// current local operator grants authority by explicitly importing this pair.
type WithdrawalAdoption struct {
	MessageKey    string `json:"message_key"`
	WithdrawalKey string `json:"withdrawal_key"`
}

func (s Store) Adopt(ctx context.Context, entries []WithdrawalAdoption) error {
	return s.update(ctx, func(state *Snapshot) error {
		for _, entry := range entries {
			if _, ok := state.Messages[entry.MessageKey]; !ok {
				return fmt.Errorf("withdrawal target not found: %s", entry.MessageKey)
			}
			if _, ok := state.Messages[entry.WithdrawalKey]; !ok {
				return fmt.Errorf("withdrawal evidence not found: %s", entry.WithdrawalKey)
			}
			if entry.MessageKey == entry.WithdrawalKey {
				return errors.New("withdrawal evidence must differ from its target")
			}
		}
		for _, entry := range entries {
			state.ActionCancelled[entry.MessageKey] = true
			state.Withdrawals[entry.MessageKey] = entry.WithdrawalKey
		}
		return nil
	})
}

// messageActions preserves exact routing while refusing unsafe terminal text.
func messageActions(message Message) []Action {
	if !validThread(message.Thread) || !validMessageID(message.ID) || hasControl(message.Root) || hasControl(message.Key) {
		return nil
	}
	return []Action{newAction("Read exact thread", "amq", "thread", "--id", message.Thread, "--include-body", "--json", "--root", message.Root)}
}

// MessageFromEntry preserves the complete exported source facts. Root/session
// scope belongs to acquisition; the content itself remains immutable.
func MessageFromEntry(base, root, session string, entry thread.Entry, hash string) Message {
	return Message{Namespace: base, Copies: []MessageCopy{{Root: root, Session: session}}, Key: base + "#" + entry.ID, Root: root, Session: session, ID: entry.ID, Thread: entry.Thread, From: entry.From, To: entry.To, Subject: entry.Subject, Kind: entry.Kind, Body: entry.Body, Created: entry.Created, Priority: entry.Priority, Labels: entry.Labels, Refs: entry.Refs, ReplyTo: entry.ReplyTo, ReplyProject: entry.ReplyProject, FromProject: entry.FromProject, Context: entry.Context, EvidenceHash: hash}
}

func (s Store) verifyMessages(ctx context.Context, state *Snapshot) error {
	if len(state.Messages) > 0 {
		info, err := os.Lstat(filepath.Join(s.Dir, "evidence"))
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("board evidence directory missing or unsafe; recovery required")
		}
	}
	entries := map[string]thread.Entry{}
	for key, message := range state.Messages {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		hash := message.EvidenceHash
		if len(hash) != 64 {
			return fmt.Errorf("message evidence identity invalid: %s", key)
		}
		decoded, err := hex.DecodeString(hash)
		if err != nil || len(decoded) != sha256.Size {
			return fmt.Errorf("message evidence identity invalid: %s", key)
		}
		entry, ok := entries[hash]
		if !ok {
			data, err := readBounded(filepath.Join(s.Dir, "evidence", hash+".json"), snapshotLimit)
			if err != nil {
				return fmt.Errorf("message evidence recovery required: %w", err)
			}
			sum := sha256.Sum256(data)
			if hex.EncodeToString(sum[:]) != hash {
				return fmt.Errorf("message evidence hash mismatch: %s", key)
			}
			if err = json.Unmarshal(data, &entry); err != nil {
				return fmt.Errorf("message evidence unreadable: %w", err)
			}
			entries[hash] = entry
		}
		expected := MessageFromEntry(s.BaseRoot, message.Root, message.Session, entry, hash)
		expected.Copies = message.Copies
		metadata := message
		metadata.Body = ""
		expectedMetadata := expected
		expectedMetadata.Body = ""
		a, _ := json.Marshal(metadata)
		b, _ := json.Marshal(expectedMetadata)
		if string(a) != string(b) || (message.Body != "" && message.Body != entry.Body) {
			return fmt.Errorf("message evidence does not match snapshot: %s", key)
		}
		state.Messages[key] = expected
	}
	return nil
}

func (s Store) validateIdentity(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	actual, err := fsq.StableTreeIdentity(s.BaseRoot)
	if err != nil {
		return err
	}
	if actual != s.BaseRootID {
		return errors.New("AMQ base root identity changed; restart or recovery required")
	}
	return nil
}

func snapshotBytes(state *Snapshot) ([]byte, error) {
	published := *state
	published.Messages = make(map[string]Message, len(state.Messages))
	for key, message := range state.Messages {
		message.Body = ""
		published.Messages[key] = message
	}
	return json.MarshalIndent(published, "", "  ")
}

func markCapacity(state *Snapshot, name, detail string) {
	for i := range state.Input.Results {
		if state.Input.Results[i].Name == name {
			r := &state.Input.Results[i].Result
			r.Status = StatusDegraded
			r.Detail = detail
			r.Coverage = Coverage{Unresolved: 1, Detail: detail}
			r.ObservedAt = time.Now().UTC()
			return
		}
	}
	state.Input.Results = append(state.Input.Results, NamedResult{Name: name, Result: Result{Status: StatusDegraded, Detail: detail, ObservedAt: time.Now().UTC(), Coverage: Coverage{Unresolved: 1, Detail: detail}}})
}
