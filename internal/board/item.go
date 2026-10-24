// Package board builds the fleet board: work items from read-only sources,
// linked into cards over explicit same-work relations, placed into columns
// from structured fields only, and rendered as text or JSON.
//
// The core (link, column table, model) is pure: it takes values and returns
// values, with the clock passed in. Sources run commands; the core never
// touches the file system or starts a process.
package board

import (
	"context"
	"time"
)

// State is the closed set every source maps into (design 3.2).
type State string

const (
	StateNeedsHuman  State = "needs_human"
	StateActive      State = "active"
	StateBlocked     State = "blocked"
	StateOpenUnowned State = "open_unowned"
	StatePlanned     State = "planned"
	StateDone        State = "done"
)

// OwnerKind says who holds the work. Unmapped means the source names someone
// the board cannot map; it is never "no owner".
type OwnerKind string

const (
	OwnerHuman    OwnerKind = "human"
	OwnerAgent    OwnerKind = "agent"
	OwnerUnmapped OwnerKind = "unmapped"
	OwnerNone     OwnerKind = "none"
)

// RelationKind separates the one relation that merges cards from references
// that are only shown.
type RelationKind string

const (
	RelationSameWork RelationKind = "same_work"
	RelationRelated  RelationKind = "related"
)

// Item is one unit of work as a source reports it.
type Item struct {
	Source     string     `json:"source"`
	Ref        Ref        `json:"ref"`
	Title      string     `json:"title"`
	State      State      `json:"state"`
	Owner      Owner      `json:"owner"`
	Requester  *Owner     `json:"requester,omitempty"`
	WaitsOn    []Owner    `json:"waits_on"`
	Reason     Reason     `json:"reason"`
	Relations  []Relation `json:"relations"`
	Evidence   []Evidence `json:"evidence"`
	UpdatedAt  time.Time  `json:"updated_at"`
	Structural bool       `json:"structural"`

	// Mentions are candidate tokens from free text. The core keeps only those
	// that resolve to a verified ref, and only as related references.
	Mentions []Mention `json:"-"`
	// Thread groups AMQ obligations of one session and thread as related.
	Thread *Ref `json:"-"`
	// Obligation carries the typed fields AMQ actions are built from.
	Obligation *ObligationFacts `json:"-"`
}

// Ref is a canonical identity; build it only with the constructors in ref.go.
type Ref struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// String is the canonical form, for example "bead:demo-a1b".
func (r Ref) String() string { return r.Kind + ":" + r.ID }

// Owner is a participant in an item.
type Owner struct {
	Kind    OwnerKind `json:"kind"`
	Handle  string    `json:"handle,omitempty"`
	Session string    `json:"session,omitempty"`
	Display string    `json:"display,omitempty"`
}

// Reason is why an item is in its state, and since when.
type Reason struct {
	Field string    `json:"field"`
	Text  string    `json:"text"`
	Since time.Time `json:"since"`
}

// Relation links an item to another ref.
type Relation struct {
	Kind     RelationKind `json:"kind"`
	To       Ref          `json:"to"`
	Basis    string       `json:"basis"`
	Inferred bool         `json:"inferred"`
}

// Evidence is one structured fact behind an item.
type Evidence struct {
	Source string    `json:"source"`
	Field  string    `json:"field"`
	Value  string    `json:"value"`
	Text   string    `json:"text,omitempty"`
	At     time.Time `json:"at"`
}

// Mention is a candidate token found in free text.
type Mention struct {
	Token string
	Basis string
}

// ObligationFacts are the validated-later inputs of the AMQ answer actions.
type ObligationFacts struct {
	OpenerID         string
	Thread           string
	OwnerSession     string
	Destination      *Destination
	CompletionKind   string
	CompletionLabels []string
	AcknowledgeKind  string
}

// Destination is the route back to the requester, as AMQ reports it.
type Destination struct {
	Handle  string
	Session string
	Project string
}

// SourceStatus is the health of one source read.
type SourceStatus string

const (
	StatusOK          SourceStatus = "ok"
	StatusDegraded    SourceStatus = "degraded"
	StatusUnavailable SourceStatus = "unavailable"
	StatusDisabled    SourceStatus = "disabled"
)

// Coverage is what a result did not assess, even when its status is ok.
type Coverage struct {
	Window     string `json:"window,omitempty"`
	Truncated  int    `json:"truncated"`
	Skipped    int    `json:"skipped"`
	Unresolved int    `json:"unresolved"`
	Detail     string `json:"detail,omitempty"`
}

// Partial reports whether anything was left unassessed.
func (c Coverage) Partial() bool {
	return c.Window != "" || c.Truncated > 0 || c.Skipped > 0 || c.Unresolved > 0
}

// RosterEntry is one AMQ mailbox, keyed by (session, handle).
type RosterEntry struct {
	Session     string `json:"session"`
	Handle      string `json:"handle"`
	Human       bool   `json:"human"`
	ValidHandle bool   `json:"valid_handle"`
	// Counted is false when the unread and DLQ counts are unknown: doctor
	// --ops does not report the mailbox (not in the session's config.json),
	// or it reports and could not read it, and CountError says why. An
	// unknown count is never zero.
	Counted                bool    `json:"counted"`
	CountError             string  `json:"count_error,omitempty"`
	UnreadCount            int     `json:"unread_count"`
	OldestUnreadAgeSeconds float64 `json:"oldest_unread_age_seconds"`
	DLQCount               int     `json:"dlq_count"`
	PresenceStatus         string  `json:"presence_status,omitempty"`
	PresenceAgeSeconds     float64 `json:"presence_age_seconds"`
	PresenceSource         string  `json:"presence_source,omitempty"`
	NotifierStatus         string  `json:"notifier_status,omitempty"`
}

// SessionInfo is one AMQ session of the project.
type SessionInfo struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// Gate is the per-session count of unread messages in the reserved user
// inbox. It overlaps obligations and is never added to a card count.
type Gate struct {
	Session          string  `json:"session"`
	OpenCount        int     `json:"open_count"`
	OldestAgeSeconds float64 `json:"oldest_age_seconds"`
}

// AMQContext is what the AMQ source learns about the project itself.
type AMQContext struct {
	Project  string        `json:"project,omitempty"`
	Peers    []string      `json:"peers,omitempty"`
	Sessions []SessionInfo `json:"sessions"`
	Gates    []Gate        `json:"gates,omitempty"`
}

// Result is one source read.
type Result struct {
	Items      []Item
	Roster     []RosterEntry
	AMQ        *AMQContext
	Status     SourceStatus
	Detail     string
	Coverage   Coverage
	ObservedAt time.Time
	Version    string
	// BeadIDs is the bead vocabulary (beads source only): every non-closed
	// bead id -> its stored status.
	BeadIDs map[string]string
	// Feeds lists the columns this source can fill, so an empty column names
	// the sources that did not fully assess it.
	Feeds []Column
}

// Runner runs one argv in dir and returns stdout. It never uses a shell.
type Runner interface {
	Run(ctx context.Context, dir string, argv []string) ([]byte, error)
}

// Env is what a source may use.
type Env struct {
	Dir      string
	Now      func() time.Time
	Run      Runner
	LookPath func(string) (string, error)
	IsDir    func(string) bool
}

// Source is one read-only provider of items.
type Source interface {
	Name() string
	Discover(env Env) (bool, string)
	Read(ctx context.Context, env Env) Result
}

// BeadResolver resolves bead tokens that are not in the open set (closed
// beads keep their identity).
type BeadResolver interface {
	ResolveBeads(ctx context.Context, env Env, ids []string) (map[string]string, error)
}
