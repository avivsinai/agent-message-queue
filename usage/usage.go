// Package usage holds the facts an account-usage collection observes: plan
// windows, money figures, plan buckets and account identities, each with the
// account it belongs to and the evidence it came from, plus failures as
// values. Facts are plain values: a pure function never mutates its inputs,
// and a returned slice owns its storage. Status (fresh, stale, partial,
// unknown) is derived from facts for an explicit time; it is never stored.
package usage

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"time"
)

// ProviderCodex names the Codex (ChatGPT plan) provider.
const ProviderCodex = "codex"

// Presence says whether a source reported a field and how.
type Presence uint8

const (
	// Absent: the field was not in the response.
	Absent Presence = iota
	// Null: the response carried the field as null.
	Null
	// Present: the response carried a valid value.
	Present
	// Invalid: the response carried a value of the wrong shape.
	Invalid
)

var presenceNames = [...]string{"absent", "null", "present", "invalid"}

func (p Presence) String() string {
	if int(p) < len(presenceNames) {
		return presenceNames[p]
	}
	return "invalid"
}

// MarshalText writes the presence as its name.
func (p Presence) MarshalText() ([]byte, error) { return []byte(p.String()), nil }

// UnmarshalText reads a presence name.
func (p *Presence) UnmarshalText(b []byte) error {
	for i, n := range presenceNames {
		if string(b) == n {
			*p = Presence(i)
			return nil
		}
	}
	return fmt.Errorf("usage: unknown presence %q", b)
}

// Field is a value with its presence, so a reported zero, a null and a
// missing field stay distinct. Value is meaningful only when Presence is
// Present.
type Field[T any] struct {
	Presence Presence `json:"presence"`
	Value    T        `json:"value,omitzero"`
}

// Some is a present field.
func Some[T any](v T) Field[T] { return Field[T]{Presence: Present, Value: v} }

// Get is the value and whether it is present.
func (f Field[T]) Get() (T, bool) { return f.Value, f.Presence == Present }

// Account names one account as one source instance saw it: the provider,
// the home the instance ran against, and the remote account that instance
// proved it was logged in to. Remote is empty while unproven; it is a hash,
// never the provider's raw account id, so persisted facts carry no account
// identifier in the clear.
type Account struct {
	Provider string `json:"provider"`
	Home     string `json:"home"`
	Remote   string `json:"remote,omitempty"`
}

// RemoteKey hashes a provider's remote account id with a versioned,
// length-prefixed encoding, so two providers' ids never collide and the
// same id always gives the same key.
func RemoteKey(provider, id string) string {
	h := sha256.New()
	for _, part := range []string{"usage-remote-v1", provider, id} {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(part)))
		h.Write(n[:])
		h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// Evidence is where and when a fact was observed.
type Evidence struct {
	// Source names the source instance kind, for example "codex-app-server".
	Source string `json:"source"`
	// Contract names the wire contract the response was parsed against.
	Contract string `json:"contract"`
	// ProducerVersion is the producer's version as it reported it.
	ProducerVersion Field[string] `json:"producer_version"`
	// ObservedAt is the collector's wall time when the response was read.
	ObservedAt time.Time `json:"observed_at"`
}

// Fact is one observed value of one account.
type Fact[T any] struct {
	Account  Account  `json:"account"`
	Value    T        `json:"value"`
	Evidence Evidence `json:"evidence"`
}

// Window kinds, named from the window's duration.
const (
	Window5h     = "5h"
	WindowWeekly = "weekly"
)

// Window is one plan window as reported. Reported is Null when the response
// carried the window slot as null: it says only that this response held no
// window there, never that the plan has none.
type Window struct {
	// LimitID is the metered bucket the window belongs to.
	LimitID string `json:"limit_id"`
	// Slot is the position the source reported it in; it carries no meaning.
	Slot     string   `json:"slot"`
	Reported Presence `json:"reported"`
	// Kind is Window5h or WindowWeekly when the duration says so, else empty.
	Kind            string           `json:"kind,omitempty"`
	DurationMinutes Field[int64]     `json:"duration_minutes"`
	UsedPercent     Field[float64]   `json:"used_percent"`
	ResetsAt        Field[time.Time] `json:"resets_at"`
}

// Money kinds.
const (
	// MoneyBalance is a remaining balance.
	MoneyBalance = "balance"
	// MoneyCap is a spend cap: an amount used of a limit.
	MoneyCap = "cap"
)

// Money is one money-like figure with its unit as the source established
// it. Amounts are exact decimal strings as reported, never floats; Unit is
// empty when the source does not say what the amounts count.
type Money struct {
	LimitID string `json:"limit_id"`
	Kind    string `json:"kind"`
	Unit    string `json:"unit,omitempty"`
	// Amount is the balance, or the amount used of a cap.
	Amount           Field[string]    `json:"amount"`
	Limit            Field[string]    `json:"limit"`
	RemainingPercent Field[float64]   `json:"remaining_percent"`
	Unlimited        Field[bool]      `json:"unlimited"`
	ResetsAt         Field[time.Time] `json:"resets_at"`
}

// Bucket is one metered bucket's plan and limit state.
type Bucket struct {
	LimitID string        `json:"limit_id"`
	Plan    Field[string] `json:"plan"`
	// Reached names the limit the source says is reached, if any.
	Reached Field[string] `json:"reached"`
}

// Identity is what a source instance proved about the account it ran as.
type Identity struct {
	// Type is the account type the source reported, for example "chatgpt".
	Type Field[string] `json:"type"`
	Plan Field[string] `json:"plan"`
}

// Failure is one collection failure, as a value. Code is stable; Detail is
// safe to show and carries no response body or credential.
type Failure struct {
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

// Failure codes.
const (
	FailNotInstalled        = "not_installed"
	FailUnsupportedPlatform = "unsupported_platform"
	FailUnsupportedVersion  = "unsupported_version"
	FailTimeout             = "timeout"
	FailOutputLimit         = "output_limit"
	FailExit                = "exit"
	FailMalformed           = "malformed"
	FailRPC                 = "rpc_error"
	FailNotLoggedIn         = "not_logged_in"
	FailHomeMismatch        = "home_mismatch"
	FailJournal             = "journal_unreadable"
	// FailJournalRecovered is a diagnostic, not a stop: the pass set a
	// corrupt journal aside or dropped attempts it rejected, and went on.
	FailJournalRecovered = "journal_recovered"
	FailReservation      = "reservation_failed"
	FailCacheRead        = "cache_unreadable"
	FailCacheWrite       = "cache_write_failed"
	FailLock             = "lock_unavailable"
)

// Result is the facts of one read or collection pass.
type Result struct {
	Windows    []Fact[Window]   `json:"windows"`
	Money      []Fact[Money]    `json:"money"`
	Buckets    []Fact[Bucket]   `json:"buckets"`
	Identities []Fact[Identity] `json:"identities"`
	Failures   []Fact[Failure]  `json:"failures"`
}

// Attempt is one reserved collection dispatch. An attempt is written
// durably before its child process starts; FinishedAt stays absent until it
// completes, so a crash leaves a reservation that still holds.
type Attempt struct {
	ID        string  `json:"id"`
	Operation string  `json:"operation"`
	Key       Account `json:"key"`
	// ReservedAt and DeadlineAt bound the dispatch; Interval is the minimum
	// time between dispatches that conflict with it.
	ReservedAt time.Time        `json:"reserved_at"`
	DeadlineAt time.Time        `json:"deadline_at"`
	Interval   time.Duration    `json:"interval"`
	StartedAt  Field[time.Time] `json:"started_at"`
	FinishedAt Field[time.Time] `json:"finished_at"`
	ExitCode   Field[int]       `json:"exit_code"`
	// FailureCode is empty for a successful attempt.
	FailureCode string `json:"failure_code,omitempty"`
	// Lease names the credential state Key.Remote was proven for, as file
	// metadata and never content; empty when the attempt proved no account
	// or the credential changed during the run.
	Lease string `json:"lease,omitempty"`
}

// ForHome is the facts of one provider home, in r's order.
func (r Result) ForHome(provider, home string) Result {
	var out Result
	match := func(a Account) bool { return a.Provider == provider && a.Home == home }
	out.Windows = filter(r.Windows, match)
	out.Money = filter(r.Money, match)
	out.Buckets = filter(r.Buckets, match)
	out.Identities = filter(r.Identities, match)
	out.Failures = filter(r.Failures, match)
	return out
}

// WithoutHome is r without the facts of one provider home.
func (r Result) WithoutHome(provider, home string) Result {
	var out Result
	other := func(a Account) bool { return a.Provider != provider || a.Home != home }
	out.Windows = filter(r.Windows, other)
	out.Money = filter(r.Money, other)
	out.Buckets = filter(r.Buckets, other)
	out.Identities = filter(r.Identities, other)
	out.Failures = filter(r.Failures, other)
	return out
}

// Merge is a followed by b.
func Merge(a, b Result) Result {
	return Result{
		Windows:    append(append([]Fact[Window]{}, a.Windows...), b.Windows...),
		Money:      append(append([]Fact[Money]{}, a.Money...), b.Money...),
		Buckets:    append(append([]Fact[Bucket]{}, a.Buckets...), b.Buckets...),
		Identities: append(append([]Fact[Identity]{}, a.Identities...), b.Identities...),
		Failures:   append(append([]Fact[Failure]{}, a.Failures...), b.Failures...),
	}
}

func filter[T any](in []Fact[T], keep func(Account) bool) []Fact[T] {
	var out []Fact[T]
	for _, f := range in {
		if keep(f.Account) {
			out = append(out, f)
		}
	}
	return out
}
