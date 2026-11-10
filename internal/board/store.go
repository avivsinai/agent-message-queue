package board

import (
	"bytes"
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
	"sync"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/lock"
	"github.com/avivsinai/agent-message-queue/internal/thread"
)

// SnapshotSchema v3 is v2 published as a base snapshot plus a journal; a
// binary that reads only the base refuses it instead of publishing over it.
const SnapshotSchema = "amq/board-state/v3"
const v2SnapshotSchema = "amq/board-state/v2"
const legacySnapshotSchema = "amq/board-state/v1"
const snapshotLimit = 64 << 20
const snapshotReserve = 1 << 20

// A publication past snapshotLimit-snapshotReserve first prunes the oldest
// unreferenced history down to pruneTarget, so ingestion continues.
const pruneTarget = (snapshotLimit - snapshotReserve) / 4 * 3

// journalName holds one record per publication since the base snapshot.
const journalName = "journal.jsonl"

var ErrProjectChanged = errors.New("board project identity changed; restart required")

var errSnapshotBound = errors.New("board snapshot capacity reached; ingestion stopped")

// errUnchanged from an update's change ends it without a publication.
var errUnchanged = errors.New("board state unchanged")

const evidenceLimit = 256 << 20

var errEvidenceBound = errors.New("board evidence bound reached; ingestion stopped")

// evidenceBoundError is errEvidenceBound with the bytes by which staging
// would pass the bound.
type evidenceBoundError struct{ excess int64 }

func (e evidenceBoundError) Error() string        { return errEvidenceBound.Error() }
func (e evidenceBoundError) Is(target error) bool { return target == errEvidenceBound }

// errEvidenceSwept reports that evidence staged for an acquisition was
// removed by a new base before the acquisition was published. Staging again
// recovers it.
var errEvidenceSwept = errors.New("board evidence staged for this refresh was removed before publication; refresh again")

// Store is bound to the authenticated canonical base root for its lifetime.
// It never repairs an existing store or restores a backup implicitly.
type Store struct {
	Dir, BaseRoot, BaseRootID, HostID string
	// ProjectID pins a published project for a long-lived reader or acquisition.
	ProjectID string
}

type Snapshot struct {
	HistoryBaseline   time.Time                     `json:"history_baseline,omitzero"`
	Observations      map[string]MessageObservation `json:"message_observations,omitempty"`
	Suggestions       SuggestionState               `json:"suggestions"`
	SuggestionIgnored map[string]bool               `json:"suggestion_ignored,omitempty"`
	SuggestionAdopted map[string]string             `json:"suggestion_adopted,omitempty"`
	Facts             map[string]AliasFact          `json:"source_facts"`
	ClaudeLists       map[string]ClaudeListBinding  `json:"claude_lists,omitempty"`
	Schema            string                        `json:"schema"`
	BaseRoot          string                        `json:"base_root"`
	BaseRootID        string                        `json:"base_root_id"`
	ProjectID         string                        `json:"project_id"`
	HostID            string                        `json:"host_id"`
	Generation        uint64                        `json:"generation"`
	PublishedAt       time.Time                     `json:"published_at"`
	Input             Input                         `json:"input"`
	Usage             *UsageView                    `json:"usage,omitempty"`
	Tasks             map[string]Task               `json:"tasks"`
	Anchors           map[string]string             `json:"admission_anchors"`
	Aliases           map[string]string             `json:"aliases"`
	Messages          map[string]Message            `json:"messages"`
	Dismissed         map[string]bool               `json:"dismissed"`
	ActionCancelled   map[string]bool               `json:"action_cancelled"`
	Withdrawals       map[string]string             `json:"withdrawal_adoptions"`
	// RequestLabels are classifier opinions keyed by RequestLabelKey.
	RequestLabels map[string]RequestLabel `json:"request_labels,omitempty"`
	// PrunedIDs maps the id of each pruned message created at or after
	// PrunedBefore to its creation time; acquisition does not take these
	// messages again. A message created before PrunedBefore is not taken even
	// when it was never ingested (see prunedIDHorizon).
	PrunedIDs    map[string]time.Time `json:"pruned_ids,omitempty"`
	PrunedBefore time.Time            `json:"pruned_before,omitzero"`
	// RetiredEvidence maps the evidence hash of each pruned message to the
	// generation that pruned it. The file goes once neither the base snapshot
	// nor its backup can name the message.
	RetiredEvidence map[string]uint64 `json:"retired_evidence,omitempty"`
	// newBase asks the next publication for a new base, which sweeps
	// evidence that nothing names. It is never stored.
	newBase bool
}

type Task struct {
	Inferred         bool              `json:"inferred,omitempty"`
	ModelAttachments []ModelAttachment `json:"model_attachments,omitempty"`
	ID               string            `json:"id"`
	Title            string            `json:"title"`
	Authority        string            `json:"authority"`
	Fact             Item              `json:"-"`
	OwnerFact        Item              `json:"owner_fact,omitzero"`
	RedirectTo       string            `json:"redirect_to,omitempty"`
	AdmissionAnchors []string          `json:"admission_anchors,omitempty"`
	Aliases          []string          `json:"aliases"`
	Attachments      []string          `json:"attachments"`
	Cancelled        bool              `json:"owner_cancelled"`
	ObservedAt       time.Time         `json:"-"`
	SourceMissing    bool              `json:"-"`
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
	ProjectID string
	Input     Input
	Usage     *UsageView
	Messages  []Message
	Evidence  [][]byte
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
		return s.publish(&state, document{})
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
		state, _, err = s.readUnlocked(ctx)
		return err
	})
	return state, err
}

// document is the published state as readers reconstruct it: the base
// snapshot with the journal replayed over it.
type document struct {
	data           []byte
	baseSchema     string
	baseGeneration uint64
	baseBytes      int64
	journalBytes   int64
}

func (s Store) readDocument() (document, error) {
	base, err := readBounded(filepath.Join(s.Dir, "snapshot.json"), snapshotLimit)
	if err != nil {
		return document{}, err
	}
	var head struct {
		Schema     string `json:"schema"`
		Generation uint64 `json:"generation"`
	}
	if err = json.Unmarshal(base, &head); err != nil {
		return document{}, err
	}
	doc := document{data: base, baseSchema: head.Schema, baseGeneration: head.Generation, baseBytes: int64(len(base))}
	journal, err := readBounded(filepath.Join(s.Dir, journalName), snapshotLimit)
	if os.IsNotExist(err) {
		return doc, nil
	} else if err != nil {
		return document{}, err
	}
	doc.journalBytes = int64(len(journal))
	var records []journalRecord
	for rest := journal; len(rest) > 0; {
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			return document{}, errors.New("board journal ends in a partial record")
		}
		var r journalRecord
		if err = json.Unmarshal(rest[:i], &r); err != nil {
			return document{}, fmt.Errorf("board journal record unreadable: %w", err)
		}
		records = append(records, r)
		rest = rest[i+1:]
	}
	if doc.data, err = replayJournal(base, head.Generation, records); err != nil {
		return document{}, err
	}
	if len(doc.data) > snapshotLimit {
		return document{}, errors.New("board snapshot bound exceeded")
	}
	return doc, nil
}

func (s Store) readUnlocked(ctx context.Context) (Snapshot, document, error) {
	var state Snapshot
	if err := s.safeDirectory(); err != nil {
		return state, document{}, err
	}
	if _, err := os.Lstat(filepath.Join(s.Dir, "write-pending")); !os.IsNotExist(err) {
		return state, document{}, errors.New("board publication interrupted; recovery required")
	}
	doc, err := s.readDocument()
	if err != nil {
		return state, document{}, fmt.Errorf("board snapshot recovery required: %w", err)
	}
	if err = json.Unmarshal(doc.data, &state); err != nil {
		return state, document{}, fmt.Errorf("board snapshot recovery required: %w", err)
	}
	if state.Schema == legacySnapshotSchema {
		if err := state.migrateLegacy(doc.data); err != nil {
			return Snapshot{}, document{}, err
		}
	}
	if state.Schema == v2SnapshotSchema {
		state.Schema = SnapshotSchema
	}
	if state.Schema != SnapshotSchema || state.BaseRoot != s.BaseRoot || !fsq.SamePersistedTreeIdentity(state.BaseRootID, s.BaseRootID) || state.ProjectID == "" || state.HostID != s.HostID || state.Generation == 0 || state.Tasks == nil || state.Anchors == nil || state.Aliases == nil || state.Messages == nil || state.Dismissed == nil || state.ActionCancelled == nil || state.Withdrawals == nil || state.Facts == nil {
		return Snapshot{}, document{}, errors.New("board identity or state invalid; recovery required")
	}
	// A reboot can renumber the base root's device; the next publish records
	// the current token.
	state.BaseRootID = s.BaseRootID
	if s.ProjectID != "" && state.ProjectID != s.ProjectID {
		return Snapshot{}, document{}, ErrProjectChanged
	}
	if state.ClaudeLists == nil {
		state.ClaudeLists = map[string]ClaudeListBinding{}
	}
	for key, binding := range state.ClaudeLists {
		if key != ClaudeListKey(binding.Home, binding.List) || binding.Revision == "" {
			return Snapshot{}, document{}, errors.New("claude list binding invalid; recovery required")
		}
	}
	for anchor, id := range state.Anchors {
		if _, ok := state.Tasks[id]; !ok || anchor == "" {
			return Snapshot{}, document{}, errors.New("board admission identity invalid; recovery required")
		}
	}
	for alias, id := range state.Aliases {
		if _, ok := state.Tasks[id]; !ok || alias == "" {
			return Snapshot{}, document{}, errors.New("board alias identity invalid; recovery required")
		}
	}
	for id, task := range state.Tasks {
		if task.ID != id || task.Title == "" || task.Authority == "" {
			return Snapshot{}, document{}, errors.New("board task identity invalid; recovery required")
		}
		for _, key := range task.Attachments {
			if _, ok := state.Messages[key]; !ok {
				return Snapshot{}, document{}, errors.New("board attachment missing; recovery required")
			}
		}
	}
	for key, msg := range state.Messages {
		if key != msg.Key || msg.ID == "" || msg.Root == "" {
			return Snapshot{}, document{}, errors.New("board message identity invalid; recovery required")
		}
	}
	for key, label := range state.RequestLabels {
		if key != RequestLabelKey(label.MessageKey, label.EvidenceDigest, label.Classifier) {
			return Snapshot{}, document{}, errors.New("board request label identity invalid; recovery required")
		}
	}
	if err := state.validateIdentityRecords(); err != nil {
		return Snapshot{}, document{}, err
	}
	state.hydrateAuthorities()
	if err := s.verifyMessages(ctx, &state); err != nil {
		return Snapshot{}, document{}, err
	}
	if err := state.validateModelBindings(); err != nil {
		return Snapshot{}, document{}, err
	}
	return state, doc, nil
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
		state, doc, err := s.readUnlocked(ctx)
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
		if err = state.validateModelBindings(); err != nil {
			return err
		}
		data, err := stamp(&state)
		if err != nil {
			return err
		}
		if len(data) > snapshotLimit-snapshotReserve && state.pruneHistory(len(data)-pruneTarget, nil) {
			if data, err = snapshotBytes(&state); err != nil {
				return err
			}
		}
		if len(data) > snapshotLimit-snapshotReserve {
			prior, doc, err := s.readUnlocked(ctx)
			if err != nil {
				return err
			}
			markCapacity(&prior, "board-capacity", errSnapshotBound.Error())
			if err = s.publish(&prior, doc); err != nil {
				return err
			}
			return errSnapshotBound
		}
		return s.write(&state, data, doc)
	})
}

// stamp gives state its next generation and publication time and returns
// the bytes that publish it.
func stamp(state *Snapshot) ([]byte, error) {
	state.Generation++
	state.PublishedAt = time.Now().UTC()
	return snapshotBytes(state)
}

// publish writes state as the generation after prev, the document it was
// read from; an empty prev starts a new base.
func (s Store) publish(state *Snapshot, prev document) error {
	data, err := stamp(state)
	if err != nil {
		return err
	}
	return s.write(state, data, prev)
}

// write publishes data, the encoding of state, as the generation after prev.
// It appends the leaf changes from prev to the journal, or writes a new base
// when prev is none or an older schema, when the journal would pass a quarter
// of the base, or while pruned evidence waits to be removed. A new base keeps
// the old base and its journal as the recovery copy (keepBackup) without
// moving them, and journal.jsonl goes only after the new base is durable, so
// at every crash point snapshot.json with journal.jsonl, or the new base
// alone, is the last published generation. The write-pending marker names
// which publication was interrupted.
func (s Store) write(state *Snapshot, data []byte, prev document) error {
	if len(data) > snapshotLimit {
		return errors.New("board snapshot reached 64 MiB; ingestion stopped, existing state preserved")
	}
	compact := prev.data == nil || prev.baseSchema != SnapshotSchema || len(state.RetiredEvidence) > 0 || state.newBase
	var record []byte
	if !compact {
		ops, err := diffRaw(prev.data, data, nil, nil)
		if err != nil {
			return err
		}
		if record, err = json.Marshal(journalRecord{Generation: state.Generation, Ops: ops}); err != nil {
			return err
		}
		record = append(record, '\n')
		compact = prev.journalBytes+int64(len(record)) > prev.baseBytes/4
	}
	// A retired hash is stamped with the generation that first publishes it.
	// Retired evidence the old base no longer names goes with this base: the
	// old base becomes the backup, so no kept generation names it.
	changed := false
	for hash, gen := range state.RetiredEvidence {
		if gen == 0 {
			state.RetiredEvidence[hash] = state.Generation
			changed = true
		} else if prev.data != nil && gen <= prev.baseGeneration {
			delete(state.RetiredEvidence, hash)
			changed = true
		}
	}
	if changed {
		var err error
		if data, err = snapshotBytes(state); err != nil {
			return err
		}
	}
	// A durable marker makes an interrupted or uncertain rename fail closed on
	// the next writer. Readers never fall back to source reads or a backup.
	marker := "journal append pending\n"
	if compact {
		marker = fmt.Sprintf("new base pending: generation %d\n", state.Generation)
	}
	if _, err := s.writeFile("write-pending", []byte(marker), 0o600); err != nil {
		return err
	}
	if compact {
		if err := s.keepBackup(); err != nil {
			return err
		}
		if _, err := s.writeFile("snapshot.json", append(data, '\n'), 0o600); err != nil {
			return fmt.Errorf("board durability uncertain; stop writes and recover: %w", err)
		}
		if err := os.Remove(filepath.Join(s.Dir, journalName)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("board durability uncertain; stop writes and recover: %w", err)
		}
	} else if err := s.appendJournal(record); err != nil {
		return fmt.Errorf("board durability uncertain; stop writes and recover: %w", err)
	}
	if err := fsq.SyncDir(s.Dir); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(s.Dir, "write-pending")); err != nil {
		return err
	}
	if err := fsq.SyncDir(s.Dir); err != nil {
		_, _ = s.writeFile("write-pending", []byte("durability uncertain\n"), 0o600)
		return fmt.Errorf("board durability uncertain; stop writes: %w", err)
	}
	if compact {
		// Leftover evidence is immutable and safe, so a crash or a failed
		// sweep only leaks it until the next new base.
		_ = s.sweepEvidence(state, prev)
	}
	return nil
}

// namedEvidence is the evidence file names a new base keeps: those state
// and the recovery copy (prev, the document the new base replaces) name, and
// the pending retirements.
func namedEvidence(state *Snapshot, prev document) (map[string]bool, error) {
	named := map[string]bool{}
	for _, m := range state.Messages {
		named[m.EvidenceHash+".json"] = true
	}
	for hash := range state.RetiredEvidence {
		named[hash+".json"] = true
	}
	if prev.data != nil {
		var backup struct {
			Messages map[string]struct {
				EvidenceHash string `json:"evidence_hash"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(prev.data, &backup); err != nil {
			return nil, err
		}
		for _, m := range backup.Messages {
			named[m.EvidenceHash+".json"] = true
		}
	}
	return named, nil
}

// holdsUnnamedEvidence reports whether the evidence directory holds bytes a
// new base would sweep: named by neither namedEvidence of the published
// state nor staged, the files this acquisition just wrote.
func (s Store) holdsUnnamedEvidence(ctx context.Context, staged map[string]bool) (bool, error) {
	var named map[string]bool
	err := lock.WithExclusiveFileLockUntil(filepath.Join(s.Dir, "writer.lock"), func() bool { return ctx.Err() != nil }, func() error {
		if err := s.validateIdentity(ctx); err != nil {
			return err
		}
		state, doc, err := s.readUnlocked(ctx)
		if err != nil {
			return err
		}
		named, err = namedEvidence(&state, doc)
		return err
	})
	if err != nil {
		return false, err
	}
	entries, err := os.ReadDir(filepath.Join(s.Dir, "evidence"))
	if os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	for _, e := range entries {
		if named[e.Name()] || staged[e.Name()] || !e.Type().IsRegular() {
			continue
		}
		if info, err := e.Info(); err == nil && info.Size() > 0 {
			return true, nil
		}
	}
	return false, nil
}

// sweepEvidence removes every evidence file that the published state, the
// recovery copy (prev) and RetiredEvidence leave unnamed. It holds the
// evidence lock, so no staging measures the bound while it runs; evidence
// staged for a publication that has not happened yet goes too, and that
// publication's evidence check refuses it until it is staged again.
func (s Store) sweepEvidence(state *Snapshot, prev document) error {
	named, err := namedEvidence(state, prev)
	if err != nil {
		return err
	}
	dir := filepath.Join(s.Dir, "evidence")
	return lock.WithExclusiveFileLockUntil(filepath.Join(s.Dir, "evidence.lock"), func() bool { return false }, func() error {
		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			return nil
		} else if err != nil {
			return err
		}
		for _, e := range entries {
			if !named[e.Name()] && e.Type().IsRegular() {
				if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !os.IsNotExist(err) {
					return err
				}
			}
		}
		return nil
	})
}

// backupJournalName replays over snapshot.backup.json to the generation
// before the current base.
const backupJournalName = "snapshot.backup.journal.jsonl"

// keepBackup makes the current base and its journal snapshot.backup.json and
// snapshot.backup.journal.jsonl, which replay to the current generation. It
// never moves or changes snapshot.json or journal.jsonl.
func (s Store) keepBackup() error {
	if _, err := os.Lstat(filepath.Join(s.Dir, "snapshot.json")); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if err := s.linkBackup("snapshot.json", "snapshot.backup.json"); err != nil {
		return err
	}
	err := s.linkBackup(journalName, backupJournalName)
	if os.IsNotExist(err) {
		err = os.Remove(filepath.Join(s.Dir, backupJournalName))
		if os.IsNotExist(err) {
			return nil
		}
	}
	return err
}

// linkBackup makes backup hold the bytes of name, a hard link where the file
// system allows one and a durable copy otherwise. name is not changed.
func (s Store) linkBackup(name, backup string) error {
	source := filepath.Join(s.Dir, name)
	staged := filepath.Join(s.Dir, backup+".tmp")
	if err := os.Remove(staged); err != nil && !os.IsNotExist(err) {
		return err
	}
	err := os.Link(source, staged)
	if err == nil {
		// Renaming a link onto a link of the same file leaves staged behind.
		if err = os.Rename(staged, filepath.Join(s.Dir, backup)); err != nil {
			return err
		}
		if err = os.Remove(staged); os.IsNotExist(err) {
			return nil
		}
		return err
	} else if os.IsNotExist(err) {
		return err
	}
	data, err := readBounded(source, snapshotLimit)
	if err != nil {
		return err
	}
	_, err = s.writeFile(backup, data, 0o600)
	return err
}

func (s Store) appendJournal(record []byte) error {
	f, err := os.OpenFile(filepath.Join(s.Dir, journalName), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(record); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// saveEvidence writes the blobs that are new and returns the evidence file
// names it wrote.
func (s Store) saveEvidence(ctx context.Context, blobs [][]byte, pruned map[string]bool) (map[string]bool, error) {
	written := map[string]bool{}
	dir := filepath.Join(s.Dir, "evidence")
	if info, err := os.Lstat(dir); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return written, errors.New("unsafe board evidence directory")
	} else if err != nil && !os.IsNotExist(err) {
		return written, err
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
		return written, err
	}
	// The budget scan stats every evidence file, so it runs only when a blob
	// is new.
	used := int64(-1)
	measure := func() error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		used = 0
		for _, e := range entries {
			info, err := e.Info()
			if err != nil {
				return err
			}
			used += info.Size()
		}
		return nil
	}
	for i, b := range blobs {
		if ctx.Err() != nil {
			return written, ctx.Err()
		}
		if err := s.safeDirectory(); err != nil {
			return written, err
		}
		sum := sha256.Sum256(b)
		hash := hex.EncodeToString(sum[:])
		if pruned[hash] {
			continue
		}
		name := hash + ".json"
		// The name is the content hash and a read re-hashes the file whenever it changes, so
		// an existing regular file of the same size holds this blob.
		if info, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			if !info.Mode().IsRegular() {
				return written, fmt.Errorf("unsafe board file: %s", filepath.Join(dir, name))
			}
			if info.Size() != int64(len(b)) {
				return written, errors.New("board evidence hash conflict")
			}
			continue
		} else if !os.IsNotExist(err) {
			return written, err
		}
		if used < 0 {
			if err := measure(); err != nil {
				return written, err
			}
		}
		if len(b) > snapshotLimit || used+int64(len(b)) > evidenceLimit {
			// The excess counts only blobs this batch would add: not on
			// disk, not pruned, not repeated.
			excess := used - evidenceLimit
			counted := map[string]bool{}
			for _, rest := range blobs[i:] {
				sum := sha256.Sum256(rest)
				h := hex.EncodeToString(sum[:])
				if pruned[h] || counted[h] {
					continue
				}
				counted[h] = true
				if _, err := os.Lstat(filepath.Join(dir, h+".json")); err == nil {
					continue
				}
				excess += int64(len(rest))
			}
			return written, evidenceBoundError{excess: excess}
		}
		if _, err := s.writeFile(filepath.Join("evidence", name), b, 0o600); err != nil {
			return written, err
		}
		written[name] = true
		used += int64(len(b))
	}
	return written, fsq.SyncDir(s.Dir)
}

func (s Store) Commit(ctx context.Context, a Acquisition) error {
	if a.ProjectID == "" {
		return errors.New("acquisition requires the published project identity captured before source reads")
	}
	if s.ProjectID != "" && s.ProjectID != a.ProjectID {
		return ErrProjectChanged
	}
	s.ProjectID = a.ProjectID
	// A new base between staging and publication can remove the staged
	// evidence; staging once more recovers it.
	err := s.commit(ctx, a)
	if errors.Is(err, errEvidenceSwept) {
		err = s.commit(ctx, a)
	}
	return err
}

func (s Store) commit(ctx context.Context, a Acquisition) error {
	staged, stageErr := s.stageEvidence(ctx, a.Evidence, a.Messages)
	if errors.Is(stageErr, errEvidenceBound) {
		// A new base removes the evidence nothing names; only what is
		// still past the bound after it is pressure.
		unnamed, err := s.holdsUnnamedEvidence(ctx, staged)
		if err != nil {
			return err
		}
		if unnamed {
			if err := s.update(ctx, func(state *Snapshot) error {
				state.newBase = true
				return nil
			}); err != nil {
				return err
			}
			_, stageErr = s.stageEvidence(ctx, a.Evidence, a.Messages)
		}
	}
	if stageErr != nil && !errors.Is(stageErr, errEvidenceBound) {
		return stageErr
	}
	stopped := false
	err := s.update(ctx, func(state *Snapshot) error {
		for _, nr := range a.Input.Results {
			if nr.Name == "beads" && nr.Result.Namespace == "" && len(nr.Result.Items) > 0 {
				return errors.New("beads facts require a bound source store")
			}
		}
		if err := stageErr; err != nil {
			if !errors.Is(err, errEvidenceBound) {
				return err
			}
			stopped = true
			// Named evidence past the bound prunes the oldest unheld history
			// by the bytes still over it, so a later publication removes
			// those files and ingestion resumes. While a retirement is
			// pending, its removal comes first and nothing more is pruned.
			var over evidenceBoundError
			if len(state.RetiredEvidence) == 0 && errors.As(err, &over) {
				state.pruneHistory(int(over.excess), func(m Message) int {
					info, err := os.Stat(filepath.Join(s.Dir, "evidence", m.EvidenceHash+".json"))
					if err != nil {
						return 0
					}
					return int(info.Size())
				})
			}
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
						nr.Result.Detail = "a Claude task list binding changed during the read; the next sync reads it again"
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
		checked := make(map[string]time.Time, len(state.Facts))
		for alias, fact := range state.Facts {
			checked[alias] = state.checkedAt(fact)
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
		// Evidence staged before a concurrent prune can belong to a message
		// that is now pruned; it is retired like the prune's own evidence.
		var skipped []string
		defer func() { state.retireEvidence(skipped) }()
		for _, m := range a.Messages {
			if state.Pruned(m.ID) {
				skipped = append(skipped, m.EvidenceHash)
				continue
			}
			if _, published := state.Messages[m.Key]; !published {
				if _, err := os.Lstat(filepath.Join(s.Dir, "evidence", m.EvidenceHash+".json")); os.IsNotExist(err) {
					return errEvidenceSwept
				}
			}
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
		state.observeMessages(time.Now().UTC())
		for sourceIndex, nr := range a.Input.Results {
			if nr.Name != "beads" && nr.Name != "codex-goals" && nr.Name != "claude-tasks" || stale[nr.Name] {
				continue
			}
			scopes := nr.Result.NativeScopes
			if nr.Name == "beads" {
				scopes = map[string]NativeScope{nr.Result.Namespace: {Complete: beadsComplete(nr.Result)}}
			}
			present := map[string]bool{}
			for _, it := range nr.Result.Items {
				it.Source = nr.Name
				if nr.Name == "beads" {
					it.Namespace = nr.Result.Namespace
				}
				alias, ok := nativeAlias(nr.Name, it)
				_, scoped := scopes[it.Namespace]
				if !ok || !scoped {
					a.Input.Results[sourceIndex].Result.Status = StatusDegraded
					a.Input.Results[sourceIndex].Result.Detail = "a task record had no valid identity or scope and was skipped"
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
				// An unchanged fact keeps its time; a fact in a complete
				// scope reads as checked with its source (checkedAt).
				old, seen := state.Facts[alias]
				if seen && !old.Missing && sameItem(old.Fact, it) {
					continue
				}
				if !nr.Result.ObservedAt.Before(old.ObservedAt) {
					state.Facts[alias] = AliasFact{Alias: alias, Fact: it, ObservedAt: nr.Result.ObservedAt}
				}
			}
			// A complete scope marks what it no longer reports missing. An
			// incomplete scope stamps what it reported, since checkedAt
			// cannot tell its reported facts from the rest.
			for namespace, scope := range scopes {
				for alias, fact := range state.Facts {
					if fact.Fact.Source != nr.Name || fact.Fact.Namespace != namespace {
						continue
					}
					switch {
					case scope.Complete && !present[alias]:
						fact.Missing = true
					case !scope.Complete && present[alias] && fact.ObservedAt.Before(nr.Result.ObservedAt):
						fact.ObservedAt = nr.Result.ObservedAt
					default:
						continue
					}
					state.Facts[alias] = fact
				}
			}
		}
		results := append([]NamedResult(nil), state.Input.Results...)
		for i := range results {
			results[i].Result.Items = storedItems(results[i].Result.Items)
		}
		state.Input.Results = results
		// A fact's checked time never moves back: when the new results no
		// longer show a later check, that check is stored on the fact.
		for alias, fact := range state.Facts {
			if at := checked[alias]; at.After(state.checkedAt(fact)) {
				fact.ObservedAt = at
				state.Facts[alias] = fact
			}
		}
		return nil
	})
	if err == nil && stopped {
		return errEvidenceBound
	}
	return err
}

// storedItems is the part of a result's items that a published state keeps:
// Project reads only forge items back, and source facts hold the native
// ones. They are ordered by ref, so a refresh that reorders a source's items
// publishes no change.
func storedItems(items []Item) []Item {
	var forge []Item
	for _, it := range items {
		if it.Ref.Kind == RefForge {
			forge = append(forge, it)
		}
	}
	sort.SliceStable(forge, func(i, j int) bool { return forge[i].Ref.String() < forge[j].Ref.String() })
	return forge
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
				var keptBindings []ModelAttachment
				for _, b := range task.ModelAttachments {
					if b.Unit.MessageKey != a.MessageKey {
						keptBindings = append(keptBindings, b)
					}
				}
				task.ModelAttachments = keptBindings
				return task
			}
			transferWholeMessage := a.Kind == "attach"
			if a.Kind == "move" {
				from, ok := state.Tasks[a.FromTaskID]
				if !ok || a.FromTaskID == id {
					return errors.New("move needs a different existing --from-task")
				}
				hasBinding := false
				for _, key := range from.Attachments {
					if key == a.MessageKey {
						hasBinding = true
						transferWholeMessage = true
					}
				}
				for _, b := range from.ModelAttachments {
					if b.Unit.MessageKey == a.MessageKey {
						hasBinding = true
						task.ModelAttachments = appendModelAttachment(task.ModelAttachments, b)
					}
				}
				if !hasBinding {
					return errors.New("source task does not contain this attachment")
				}
				state.Tasks[a.FromTaskID] = remove(from)
			}
			if a.Kind == "detach" {
				task = remove(task)
			} else if transferWholeMessage {
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
// link graphs. Requests are projected separately and stay outside task and
// WIP counts. Forge items that no task claims as the same work become Flow
// cards, never tasks.
func (s Snapshot) Project(cfg Config, now time.Time, version string) Model {
	s = s.projectionSnapshot()
	input := s.Input
	input.Results = append([]NamedResult(nil), s.Input.Results...)
	input.Config = cfg
	input.Now = now
	input.Version = version
	var forge []Item
	observed := map[string]time.Time{}
	for i := range input.Results {
		nr := input.Results[i]
		observed[nr.Name] = nr.Result.ObservedAt
		for _, it := range nr.Result.Items {
			if it.Ref.Kind == RefForge {
				it.Source = nr.Name
				forge = append(forge, it)
			}
		}
		input.Results[i].Result.Items = nil
	}
	m := Build(input)
	m.Usage = s.Usage
	attachUsage(&m)
	m.Generation = s.Generation
	m.PublishedAt = s.PublishedAt
	for i := range m.Columns {
		m.Columns[i].Cards = []Card{}
	}
	m.Historical = []Card{}
	m.Catalogue = s.Catalogue()
	m.Redirects = s.redirects()
	m.Suggestions = s.SuggestionViews()
	m.SuggestionDiagnostic = s.Suggestions.Attempt
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
			col = ColumnHistorical
		}
		if task.Authority == "operator" && col != ColumnHistorical {
			col = ColumnUnclaimed
		}
		if task.Cancelled {
			col = ColumnHistorical
		}
		card := Card{Inferred: task.Inferred, Cancelled: task.Cancelled, ID: id, Title: task.Title, Column: col, Reason: it.Reason, ReasonSource: it.Source, Items: []Item{it}, Owners: []Owner{}, WaitsOn: it.WaitsOn, Marks: []string{}, Actions: []Action{}, Refs: []Ref{}, UpdatedAt: it.UpdatedAt, Aliases: task.Aliases}
		card.SourceObservedAt = task.ObservedAt
		card.Summary = cardSummary(card.Items)
		if card.Summary == "" && it.Obligation != nil {
			card.Summary = s.openerSummary(*it.Obligation)
		}
		card.Authority = task.Authority
		card.current = !task.Cancelled && !task.SourceMissing
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
		if task.Inferred {
			card.Marks = append(card.Marks, "owner-adopted inferred/requested outcome")
		}
		card.ModelAttachments = task.ModelAttachments
		if col == ColumnHistorical {
			card.Historical = true
			card.HistoryStatus = "recorded " + string(it.State)
		}
		if it.State == StateDone {
			card.HistoryStatus = "completed"
		}
		if task.Cancelled {
			card.HistoryStatus = "owner-cancelled"
			card.Marks = append(card.Marks, "owner-cancelled; retained outside current work")
		}
		if task.SourceMissing {
			card.Historical = true
			if !task.Cancelled {
				card.HistoryStatus = "not reported"
			}
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
		taskOperation := "cancel"
		if task.Cancelled {
			taskOperation = "reopen"
		}
		card.Actions = append(card.Actions, newOwnerAction(taskOperation+" this task", "amq-board", taskOperation, "--scope", "task", "--task", id))
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
	m.Requests = s.RequestView(now)
	var unattached []Message
	for _, list := range [][]Request{m.Requests.Recent, m.Requests.Older, m.Requests.Undated} {
		for _, r := range list {
			if !attached[r.Message.Key] {
				unattached = append(unattached, r.Message)
			}
		}
	}
	relateAsks(m.Columns, unattached, input.Known)
	for i := range m.Columns {
		sortCards(m.Columns[i].ID, m.Columns[i].Cards)
	}
	m.Next = selectNext(m.Columns)
	buildFlow(&m, forge, input, observed)
	buildFloor(&m, observed["amq"], cfg, now)
	return m
}

// openerSummary is the first sentence of the request a thread obligation
// answers, or empty when the snapshot does not hold that message.
func (s Snapshot) openerSummary(o ObligationFacts) string {
	for _, m := range s.Messages {
		if m.ID == o.OpenerID && m.Thread == o.Thread {
			return Sentence(m.Body)
		}
	}
	return ""
}

// relateAsks adds one related token to each current card per request that
// no task holds and whose subject, body, or refs name the card's bead id,
// newest request first.
// The longest candidate in the bead vocabulary wins, so a mention of a
// longer known id never names its prefix. A related ask is context only:
// placement, counts, attachments, actions, and lifecycle stay as projected.
func relateAsks(columns []ColumnView, asks []Message, known map[string]string) {
	type position struct{ column, card int }
	byBead := map[string][]position{}
	for i := range columns {
		for j, card := range columns[i].Cards {
			for _, ref := range card.Refs {
				if ref.Kind == RefBead {
					byBead[ref.ID] = append(byBead[ref.ID], position{i, j})
				}
			}
		}
	}
	if len(byBead) == 0 {
		return
	}
	newest := append([]Message(nil), asks...)
	sort.SliceStable(newest, func(i, j int) bool { return askNewer(newest[i], newest[j]) })
	for _, ask := range newest {
		ref, ok := AMQMessage(ask.Key)
		if !ok {
			continue
		}
		linked := map[position]bool{}
		text := ask.Subject + "\n" + ask.Body + "\n" + strings.Join(ask.Refs, "\n")
		for _, token := range BeadTokens(text) {
			for _, id := range BeadCandidates(token) {
				positions, current := byBead[id]
				if _, ok := known[id]; !ok && !current {
					continue
				}
				for _, p := range positions {
					if linked[p] {
						continue
					}
					linked[p] = true
					t := Token{Ref: ref, Relation: string(RelationRelated), Basis: strings.ReplaceAll(ask.Kind, "_", " ") + ": " + cleanTitle(ask.Subject), Inferred: true}
					if actions := messageActions(ask); len(actions) > 0 {
						t.Action = &actions[0]
					}
					card := &columns[p.column].Cards[p.card]
					card.Related = append(card.Related, t)
				}
				break
			}
		}
	}
}

// askNewer orders asks newest first by creation time, then by key.
func askNewer(a, b Message) bool {
	at, aerr := time.Parse(time.RFC3339Nano, a.Created)
	bt, berr := time.Parse(time.RFC3339Nano, b.Created)
	if aerr == nil && berr == nil {
		if !at.Equal(bt) {
			return at.After(bt)
		}
	} else if a.Created != b.Created {
		return a.Created > b.Created
	}
	return a.Key < b.Key
}

// Attachment is an owner binding of an exact message to a task.
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
			entry, err = readEvidenceEntry(filepath.Join(s.Dir, "evidence", hash+".json"), hash, key)
			if err != nil {
				return err
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

// evidenceMemo keeps the verified decode of an evidence file, keyed by path
// and validated by size, mtime, and file identity. The file name is its
// content hash and the file is written once by rename, so an unchanged stat
// means unchanged content; any change re-reads and re-hashes.
var evidenceMemo = struct {
	sync.Mutex
	m map[string]evidenceMemoEntry
}{m: map[string]evidenceMemoEntry{}}

type evidenceMemoEntry struct {
	size  int64
	mtime time.Time
	id    string
	entry thread.Entry
}

func readEvidenceEntry(path, hash, key string) (thread.Entry, error) {
	var stat evidenceMemoEntry
	if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() {
		if id, idErr := fsq.StableFileIdentityInfo(info); idErr == nil {
			stat = evidenceMemoEntry{size: info.Size(), mtime: info.ModTime(), id: id}
			evidenceMemo.Lock()
			hit, ok := evidenceMemo.m[path]
			evidenceMemo.Unlock()
			if ok && hit.size == stat.size && hit.mtime.Equal(stat.mtime) && hit.id == stat.id {
				return hit.entry, nil
			}
		}
	}
	data, err := readBounded(path, snapshotLimit)
	if err != nil {
		return thread.Entry{}, fmt.Errorf("message evidence recovery required: %w", err)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != hash {
		return thread.Entry{}, fmt.Errorf("message evidence hash mismatch: %s", key)
	}
	var entry thread.Entry
	if err = json.Unmarshal(data, &entry); err != nil {
		return thread.Entry{}, fmt.Errorf("message evidence unreadable: %w", err)
	}
	if stat.id != "" {
		evidenceMemo.Lock()
		if len(evidenceMemo.m) >= 8192 {
			clear(evidenceMemo.m)
		}
		evidenceMemo.m[path] = evidenceMemoEntry{size: stat.size, mtime: stat.mtime, id: stat.id, entry: entry}
		evidenceMemo.Unlock()
	}
	return entry, nil
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
	return json.Marshal(published)
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

// writeFile atomically replaces rel below the store directory. The parent of
// the store directory is the trust anchor, so a symlink at the store
// directory or below it is refused.
func (s Store) writeFile(rel string, data []byte, perm os.FileMode) (string, error) {
	return fsq.WriteStateFile(filepath.Dir(s.Dir), filepath.Join(filepath.Base(s.Dir), rel), data, perm)
}
