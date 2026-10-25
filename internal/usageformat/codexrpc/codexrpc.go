// SPDX-License-Identifier: MIT
// The app-server call sequence and response fields follow CodexBar's Codex
// app-server client; see third_party/codexbar/NOTICE.md.

// Package codexrpc is the wire format of the Codex app-server JSON-RPC
// interface over stdio, as far as plan limits need it: the request lines,
// when the replies are complete, and a presence-aware parse of the
// initialize, account/read and account/rateLimits/read replies. It parses
// syntax and field presence only; meaning and units belong to the
// normalizer. The account email is never parsed.
package codexrpc

import (
	"bytes"
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/avivsinai/agent-message-queue/usage"
)

// Contract names this wire contract in fact evidence.
const Contract = "codex-app-server-v2"

// Args starts the app-server over stdio with a read-only sandbox and no
// approvals, so the child can run no model-generated command.
var Args = []string{"-s", "read-only", "-a", "never", "app-server"}

// VersionArgs prints the CLI version.
var VersionArgs = []string{"--version"}

// verified is the CLI version range this parser was checked against: from
// the oldest version whose replies were read for real, up to the next major
// version, which may change the protocol.
var verified = struct{ min, below [3]int }{min: [3]int{0, 163, 0}, below: [3]int{1, 0, 0}}

// Version reads `codex --version` output ("codex-cli 0.163.0").
func Version(out []byte) (string, bool) {
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	fields := strings.Fields(line)
	if len(fields) != 2 || fields[0] != "codex-cli" {
		return "", false
	}
	if _, ok := parseVersion(fields[1]); !ok {
		return "", false
	}
	return fields[1], true
}

// Verified reports whether v lies in the checked range. Any pre-release or
// build suffix is ignored.
func Verified(v string) bool {
	n, ok := parseVersion(v)
	return ok && !less(n, verified.min) && less(n, verified.below)
}

func parseVersion(v string) ([3]int, bool) {
	var n [3]int
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return n, false
	}
	for i, p := range parts {
		x, err := strconv.Atoi(p)
		if err != nil || x < 0 {
			return n, false
		}
		n[i] = x
	}
	return n, true
}

func less(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// Request ids. Replies are matched by id: the server answers out of order.
const (
	idInitialize = 1
	idAccount    = 2
	idRateLimits = 3
)

// Requests is the stdin of one run: initialize, the initialized
// notification, account/read and account/rateLimits/read. account/read
// sends no refreshToken, so the server refreshes nothing for it, and the
// rate limit read sets only excludeResetCreditDetails, which skips a detail
// lookup a background poll does not need.
func Requests(clientName, clientVersion string) []byte {
	msgs := []any{
		map[string]any{"jsonrpc": "2.0", "id": idInitialize, "method": "initialize",
			"params": map[string]any{"clientInfo": map[string]any{"name": clientName, "version": clientVersion}}},
		map[string]any{"jsonrpc": "2.0", "method": "initialized"},
		map[string]any{"jsonrpc": "2.0", "id": idAccount, "method": "account/read", "params": map[string]any{}},
		map[string]any{"jsonrpc": "2.0", "id": idRateLimits, "method": "account/rateLimits/read",
			"params": map[string]any{"excludeResetCreditDetails": true}},
	}
	var b bytes.Buffer
	for _, m := range msgs {
		line, _ := json.Marshal(m) // maps of strings and ints always marshal
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// envelope is one JSON-RPC line. A line with a method is a notification or
// a server request and is not a reply.
type envelope struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code int `json:"code"`
	} `json:"error"`
}

// replies indexes the reply lines in out by id; a later reply to an id
// replaces an earlier one.
func replies(out []byte) map[int]envelope {
	got := map[int]envelope{}
	for _, line := range bytes.Split(out, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var e envelope
		if json.Unmarshal(line, &e) != nil || e.Method != "" || len(e.ID) == 0 {
			continue
		}
		id, err := strconv.Atoi(string(e.ID))
		if err != nil {
			continue
		}
		got[id] = e
	}
	return got
}

// Done reports whether out holds every reply a run waits for: account/read
// and account/rateLimits/read, or an initialize error, after which nothing
// else will answer.
func Done(out []byte) bool {
	got := replies(out)
	if e, ok := got[idInitialize]; ok && e.Error != nil {
		return true
	}
	_, acct := got[idAccount]
	_, limits := got[idRateLimits]
	return acct && limits
}

// RPCError is a JSON-RPC error reply. Only the code is kept: the message is
// free text from the server.
type RPCError struct{ Code int }

// Reply is one parsed reply. Presence is Absent when no reply came, Null
// when the result was null, Invalid when it was not understood, and Present
// with Value otherwise. Err is set for an error reply.
type Reply[T any] struct {
	Presence usage.Presence
	Value    T
	Err      *RPCError
}

// Initialize is the initialize result.
type Initialize struct {
	CodexHome usage.Field[string]
}

// Account is the account/read result. Account is Null when the server has
// no login.
type Account struct {
	Account usage.Field[AccountInfo]
}

// AccountInfo is the logged-in account, without its email.
type AccountInfo struct {
	Type usage.Field[string]
	Plan usage.Field[string]
}

// RateLimits is the account/rateLimits/read result.
type RateLimits struct {
	AccountID usage.Field[string]
	// Snapshots holds one entry per metered bucket: rateLimitsByLimitId by
	// key when it has any, else the single rateLimits view.
	Snapshots []Snapshot
}

// Snapshot is one bucket's rate limit snapshot.
type Snapshot struct {
	LimitID    string
	Plan       usage.Field[string]
	Reached    usage.Field[string]
	Primary    usage.Field[Window]
	Secondary  usage.Field[Window]
	Credits    usage.Field[Credits]
	Individual usage.Field[SpendLimit]
}

// Window is one rate limit window.
type Window struct {
	UsedPercent        usage.Field[float64]
	WindowDurationMins usage.Field[int64]
	ResetsAt           usage.Field[int64] // Unix seconds
}

// Credits is the credits snapshot. Balance is a decimal string.
type Credits struct {
	HasCredits usage.Field[bool]
	Unlimited  usage.Field[bool]
	Balance    usage.Field[string]
}

// SpendLimit is the individual spend-control limit. Limit and Used are
// decimal strings.
type SpendLimit struct {
	Limit            usage.Field[string]
	Used             usage.Field[string]
	RemainingPercent usage.Field[float64]
	ResetsAt         usage.Field[int64] // Unix seconds
}

// Replies is everything one run's stdout answered.
type Replies struct {
	Initialize Reply[Initialize]
	Account    Reply[Account]
	RateLimits Reply[RateLimits]
}

// Parse reads the replies in out, matched by id; notifications and
// unrelated lines are skipped.
func Parse(out []byte) Replies {
	got := replies(out)
	return Replies{
		Initialize: reply(got, idInitialize, parseInitialize),
		Account:    reply(got, idAccount, parseAccount),
		RateLimits: reply(got, idRateLimits, parseRateLimits),
	}
}

func reply[T any](got map[int]envelope, id int, parse func(json.RawMessage) (T, bool)) Reply[T] {
	e, ok := got[id]
	switch {
	case !ok:
		return Reply[T]{}
	case e.Error != nil:
		return Reply[T]{Presence: usage.Invalid, Err: &RPCError{Code: e.Error.Code}}
	case len(e.Result) == 0:
		return Reply[T]{Presence: usage.Invalid}
	case string(e.Result) == "null":
		return Reply[T]{Presence: usage.Null}
	}
	v, ok := parse(e.Result)
	if !ok {
		return Reply[T]{Presence: usage.Invalid}
	}
	return Reply[T]{Presence: usage.Present, Value: v}
}

func parseInitialize(raw json.RawMessage) (Initialize, bool) {
	var w struct {
		CodexHome json.RawMessage `json:"codexHome"`
	}
	if json.Unmarshal(raw, &w) != nil {
		return Initialize{}, false
	}
	return Initialize{CodexHome: scalar[string](w.CodexHome, nonEmpty)}, true
}

func parseAccount(raw json.RawMessage) (Account, bool) {
	var w struct {
		Account json.RawMessage `json:"account"`
	}
	if json.Unmarshal(raw, &w) != nil {
		return Account{}, false
	}
	return Account{Account: object(w.Account, func(w struct {
		Type     json.RawMessage `json:"type"`
		PlanType json.RawMessage `json:"planType"`
	}) AccountInfo {
		return AccountInfo{Type: scalar[string](w.Type, nonEmpty), Plan: scalar[string](w.PlanType, nonEmpty)}
	})}, true
}

type snapshotWire struct {
	LimitID              json.RawMessage `json:"limitId"`
	PlanType             json.RawMessage `json:"planType"`
	RateLimitReachedType json.RawMessage `json:"rateLimitReachedType"`
	Primary              json.RawMessage `json:"primary"`
	Secondary            json.RawMessage `json:"secondary"`
	Credits              json.RawMessage `json:"credits"`
	IndividualLimit      json.RawMessage `json:"individualLimit"`
}

func parseRateLimits(raw json.RawMessage) (RateLimits, bool) {
	var w struct {
		AccountID  json.RawMessage `json:"accountId"`
		RateLimits json.RawMessage `json:"rateLimits"`
		ByLimitID  json.RawMessage `json:"rateLimitsByLimitId"`
	}
	if json.Unmarshal(raw, &w) != nil {
		return RateLimits{}, false
	}
	out := RateLimits{AccountID: scalar[string](w.AccountID, nonEmpty)}
	var by map[string]snapshotWire
	if len(w.ByLimitID) > 0 && string(w.ByLimitID) != "null" {
		if json.Unmarshal(w.ByLimitID, &by) != nil {
			return RateLimits{}, false
		}
	}
	if len(by) > 0 {
		keys := make([]string, 0, len(by))
		for k := range by {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			out.Snapshots = append(out.Snapshots, snapshot(k, by[k]))
		}
		return out, true
	}
	var single snapshotWire
	if len(w.RateLimits) == 0 || json.Unmarshal(w.RateLimits, &single) != nil {
		return RateLimits{}, false
	}
	id, _ := scalar[string](single.LimitID, nonEmpty).Get()
	out.Snapshots = []Snapshot{snapshot(id, single)}
	return out, true
}

func snapshot(id string, w snapshotWire) Snapshot {
	return Snapshot{
		LimitID:    id,
		Plan:       scalar[string](w.PlanType, nonEmpty),
		Reached:    scalar[string](w.RateLimitReachedType, nonEmpty),
		Primary:    object(w.Primary, window),
		Secondary:  object(w.Secondary, window),
		Credits:    object(w.Credits, credits),
		Individual: object(w.IndividualLimit, spendLimit),
	}
}

func window(w struct {
	UsedPercent        json.RawMessage `json:"usedPercent"`
	WindowDurationMins json.RawMessage `json:"windowDurationMins"`
	ResetsAt           json.RawMessage `json:"resetsAt"`
}) Window {
	return Window{
		UsedPercent:        scalar(w.UsedPercent, func(v float64) bool { return v >= 0 }),
		WindowDurationMins: scalar(w.WindowDurationMins, func(v int64) bool { return v > 0 }),
		ResetsAt:           scalar(w.ResetsAt, func(v int64) bool { return v > 0 }),
	}
}

func credits(w struct {
	HasCredits json.RawMessage `json:"hasCredits"`
	Unlimited  json.RawMessage `json:"unlimited"`
	Balance    json.RawMessage `json:"balance"`
}) Credits {
	return Credits{
		HasCredits: scalar[bool](w.HasCredits, nil),
		Unlimited:  scalar[bool](w.Unlimited, nil),
		Balance:    scalar(w.Balance, isDecimal),
	}
}

func spendLimit(w struct {
	Limit            json.RawMessage `json:"limit"`
	Used             json.RawMessage `json:"used"`
	RemainingPercent json.RawMessage `json:"remainingPercent"`
	ResetsAt         json.RawMessage `json:"resetsAt"`
}) SpendLimit {
	return SpendLimit{
		Limit:            scalar(w.Limit, isDecimal),
		Used:             scalar(w.Used, isDecimal),
		RemainingPercent: scalar[float64](w.RemainingPercent, nil),
		ResetsAt:         scalar(w.ResetsAt, func(v int64) bool { return v > 0 }),
	}
}

var decimal = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?$`)

func isDecimal(s string) bool { return decimal.MatchString(s) }

func nonEmpty(s string) bool { return s != "" }

// scalar reads one JSON value with its presence; valid, when set, rejects
// a well-typed value outside the contract.
func scalar[T any](raw json.RawMessage, valid func(T) bool) usage.Field[T] {
	switch {
	case len(raw) == 0:
		return usage.Field[T]{}
	case string(raw) == "null":
		return usage.Field[T]{Presence: usage.Null}
	}
	var v T
	if json.Unmarshal(raw, &v) != nil || (valid != nil && !valid(v)) {
		return usage.Field[T]{Presence: usage.Invalid}
	}
	return usage.Some(v)
}

// object reads one JSON object with its presence and converts it.
func object[W, T any](raw json.RawMessage, conv func(W) T) usage.Field[T] {
	switch {
	case len(raw) == 0:
		return usage.Field[T]{}
	case string(raw) == "null":
		return usage.Field[T]{Presence: usage.Null}
	}
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		return usage.Field[T]{Presence: usage.Invalid}
	}
	var w W
	if json.Unmarshal(raw, &w) != nil {
		return usage.Field[T]{Presence: usage.Invalid}
	}
	return usage.Some(conv(w))
}
