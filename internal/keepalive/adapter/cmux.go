package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	cmuxSurfaceTargetPrefix = "cmux:surface:"
	cmuxSystemTreeParams    = `{"all_windows":true}`
	defaultCmuxSettleDelay  = 150 * time.Millisecond

	// cmuxEvictedTTYName is the sentinel tty_name written to a corpse surface
	// via surface.report_tty to retract its stale PTY alias. report_tty rejects
	// an empty tty_name, so retraction needs a non-empty value; this basename
	// canonicalizes to /dev/amq-evicted-corpse, which can never collide with a
	// real macOS PTY (those are /dev/ttysNNN). Keepalive treats any surface
	// carrying this tty as retired (a non-claimant, absent from ownership).
	cmuxEvictedTTYName = "amq-evicted-corpse"
)

// cmuxEvictedTTY is the canonical device path form of the eviction sentinel.
var cmuxEvictedTTY = filepath.Join("/dev", cmuxEvictedTTYName)

type Cmux struct {
	Runner       CommandRunner
	Path         string
	Getenv       func(string) string
	LookPath     func(string) (string, error)
	UserHomeDir  func() (string, error)
	IsExecutable func(string) bool
	Sleep        func(context.Context, time.Duration) error
	SettleDelay  time.Duration
	// LiveTTYOwnerCount reports how many live (non-zombie) processes hold the
	// device at devPath as their controlling terminal. Defaults to the darwin
	// sysctl implementation; tests inject a fake so they never run a real
	// sysctl against fixture tty names.
	LiveTTYOwnerCount func(devPath string) (int, error)
	// Logf receives non-fatal diagnostics (evictions, degraded fail-closed
	// ttys). Nil is a no-op.
	Logf func(format string, args ...any)
	// recorded remembers the last successful OwnershipKey per surface UUID so
	// a blank-tty surface:<uuid> key may promote to tty:<pty> once, while a
	// later tty:<other> for the same UUID stays fail-closed.
	recorded *cmuxOwnershipRecord
}

type cmuxSystemTree struct {
	Windows *[]cmuxWindow `json:"windows"`
}

type cmuxWindow struct {
	Workspaces *[]cmuxWorkspace `json:"workspaces"`
}

type cmuxWorkspace struct {
	ID    string      `json:"id"`
	Panes *[]cmuxPane `json:"panes"`
}

type cmuxPane struct {
	Surfaces *[]cmuxSurface `json:"surfaces"`
}

type cmuxSurface struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	TTY  string `json:"tty"`
	// ProcessAlive is forward-compat: a future cmux may report per-surface
	// process liveness directly. Absent (nil) means unknown -> no effect.
	// Explicit false marks the surface a corpse (a non-claimant, evictable).
	ProcessAlive *bool `json:"process_alive"`
}

type cmuxSurfaceIdentity struct {
	Type         string
	TTY          string
	WorkspaceID  string
	ProcessAlive *bool
}

// cmuxOwnershipRecord is the process-local last-key map for one Cmux adapter.
type cmuxOwnershipRecord struct {
	mu   sync.Mutex
	keys map[string]string
}

func newCmuxOwnershipRecord() *cmuxOwnershipRecord {
	return &cmuxOwnershipRecord{keys: map[string]string{}}
}

// Keep the token non-zero-sized so distinct snapshots cannot legally share an
// address under Go's zero-size allocation rules.
type cmuxOwnershipToken struct{ _ byte }

// cmuxDegradedOwnershipError reports uncertain ownership while retaining a
// physical identity established by one immutable cmux inventory snapshot.
// Both the type and its snapshot token stay package-private.
type cmuxDegradedOwnershipError struct {
	inventoryToken *cmuxOwnershipToken
	ownershipKey   string
	detail         string
}

func (e *cmuxDegradedOwnershipError) Error() string {
	if e.detail == "" {
		return ErrTargetDegraded.Error()
	}
	return ErrTargetDegraded.Error() + ": " + e.detail
}

func (*cmuxDegradedOwnershipError) Unwrap() error {
	return ErrTargetDegraded
}

// cmuxTargetInventory is a post-resolution snapshot. Physical ownership is
// resolved once during the inventory build (liveness probes, corpse eviction,
// and a single tree rebuild all happen there); OwnershipKey only reads the
// resolved state below.
type cmuxTargetInventory struct {
	ownershipToken *cmuxOwnershipToken
	surfaces       map[string]cmuxSurfaceIdentity
	// claimants maps a canonical tty to its non-sentinel claimant surface ids
	// after resolution (sorted). Used for ownership and for ambiguity messages.
	claimants map[string][]string
	// owner maps a canonical tty to its single resolved live owner surface id.
	owner map[string]string
	// degraded marks a canonical tty as fail-closed: liveness was unavailable
	// or ownership stayed ambiguous, so OwnershipKey refuses to pick an owner.
	degraded map[string]bool
	// notFound marks a canonical tty proven to have zero live owners; its
	// surfaces resolve to ErrTargetNotFound.
	notFound map[string]bool
	recorded *cmuxOwnershipRecord
}

func (Cmux) Name() string {
	return "cmux"
}

func (c Cmux) Discover(_ context.Context) (string, error) {
	if err := requireCmuxPlatform(); err != nil {
		return "", err
	}
	id, err := normalizeCmuxSurfaceID(c.getenv("CMUX_SURFACE_ID"))
	if err != nil {
		return "", fmt.Errorf("discover cmux surface from CMUX_SURFACE_ID: %w", err)
	}
	return cmuxSurfaceTargetPrefix + id, nil
}

func (Cmux) NormalizeTarget(target string) (string, error) {
	id, err := parseCmuxSurfaceTarget(target)
	if err != nil {
		return "", err
	}
	return cmuxSurfaceTargetPrefix + id, nil
}

func (c Cmux) Probe(ctx context.Context, target string) error {
	inventory, err := c.Inventory(ctx, OwnershipContext{})
	if err != nil {
		return err
	}
	return inventory.Probe(target)
}

func (c Cmux) Inventory(ctx context.Context, _ OwnershipContext) (TargetInventory, error) {
	if err := requireCmuxPlatform(); err != nil {
		return nil, err
	}
	path, err := c.executable()
	if err != nil {
		return nil, err
	}
	inventory, err := c.buildInventory(ctx, path)
	if err != nil {
		return nil, err
	}
	return inventory, nil
}

// buildInventory owns all physical-ownership resolution. It parses the tree,
// resolves owners (probing tty liveness and eviting provable corpses for
// contested ttys), and on any successful eviction rebuilds the tree exactly
// once so the returned snapshot reflects the retracted aliases.
func (c Cmux) buildInventory(ctx context.Context, path string) (cmuxTargetInventory, error) {
	surfaces, err := c.fetchSurfaces(ctx, path)
	if err != nil {
		return cmuxTargetInventory{}, err
	}
	res := c.resolveOwners(surfaces, true)
	if len(res.evictions) == 0 {
		return c.snapshotInventory(surfaces, res), nil
	}

	failedTTYs, evicted := c.evict(ctx, path, res.evictions)
	if !evicted {
		// No eviction landed. Keep the pre-eviction fail-closed state: every
		// tty we intended to clean stays ambiguous/degraded, and we do not
		// rebuild (nothing changed). Retry naturally on the next pass.
		for tty := range failedTTYs {
			res.forceDegraded(tty)
		}
		return c.snapshotInventory(surfaces, res), nil
	}

	// At least one alias was retracted; rebuild once so the snapshot reflects
	// the current tree. The rebuild does not probe or evict again: cleaned
	// corpses now carry the sentinel tty (excluded), so a previously contested
	// tty collapses to its single live owner. A tty whose eviction failed is
	// still contested in the rebuilt tree, so it stays degraded here too.
	rebuilt, err := c.fetchSurfaces(ctx, path)
	if err != nil {
		return cmuxTargetInventory{}, err
	}
	res2 := c.resolveOwners(rebuilt, false)
	for tty := range res2.degraded {
		c.logf("WARN cmux tty ownership remained ambiguous after corpse eviction, degraded to fail-closed: tty=%s", tty)
	}
	for tty := range failedTTYs {
		res2.forceDegraded(tty)
		c.logf("WARN cmux eviction incomplete, degraded to fail-closed: tty=%s", tty)
	}
	return c.snapshotInventory(rebuilt, res2), nil
}

// fetchSurfaces runs one system.tree RPC and parses it into a surface identity
// map. It fails ambiguously (never ErrTargetNotFound) on transport or schema
// errors so absence is only ever inferred from a well-formed tree.
func (c Cmux) fetchSurfaces(ctx context.Context, path string) (map[string]cmuxSurfaceIdentity, error) {
	out, err := c.runner().Run(ctx, path, "rpc", "system.tree", cmuxSystemTreeParams)
	if err != nil {
		return nil, fmt.Errorf("inventory cmux surfaces: %w: %s", err, strings.TrimSpace(string(out)))
	}
	var tree cmuxSystemTree
	if err := json.Unmarshal(out, &tree); err != nil {
		return nil, fmt.Errorf("parse cmux system.tree: %w", err)
	}
	if tree.Windows == nil {
		return nil, errors.New("parse cmux system.tree: required windows field is missing or null")
	}
	surfaces := map[string]cmuxSurfaceIdentity{}
	for _, window := range *tree.Windows {
		if window.Workspaces == nil {
			return nil, errors.New("parse cmux system.tree: window workspaces field is missing or null")
		}
		for _, workspace := range *window.Workspaces {
			if workspace.Panes == nil {
				return nil, errors.New("parse cmux system.tree: workspace panes field is missing or null")
			}
			for _, pane := range *workspace.Panes {
				if pane.Surfaces == nil {
					return nil, errors.New("parse cmux system.tree: pane surfaces field is missing or null")
				}
				for _, surface := range *pane.Surfaces {
					id, err := normalizeCmuxSurfaceID(surface.ID)
					if err != nil {
						return nil, fmt.Errorf("parse cmux system.tree surface id: %w", err)
					}
					id = strings.ToUpper(id)
					typ := strings.TrimSpace(surface.Type)
					if typ == "" {
						typ = "terminal"
					}
					identity := cmuxSurfaceIdentity{
						Type:         typ,
						TTY:          strings.TrimSpace(surface.TTY),
						WorkspaceID:  strings.TrimSpace(workspace.ID),
						ProcessAlive: surface.ProcessAlive,
					}
					if previous, exists := surfaces[id]; exists && !sameCmuxSurfaceIdentity(previous, identity) {
						return nil, fmt.Errorf("parse cmux system.tree: surface %q has conflicting tty identities %q and %q", id, previous.TTY, identity.TTY)
					}
					surfaces[id] = identity
				}
			}
		}
	}
	return surfaces, nil
}

// cmuxEviction is one corpse alias to retract via surface.report_tty.
type cmuxEviction struct {
	surfaceID   string
	workspaceID string
	tty         string // canonical tty the corpse claimed (for fail-closed fallback)
}

// cmuxResolution is the outcome of resolving physical ownership over one tree.
type cmuxResolution struct {
	claimants map[string][]string
	owner     map[string]string
	degraded  map[string]bool
	notFound  map[string]bool
	evictions []cmuxEviction
}

func (r cmuxResolution) forceDegraded(tty string) {
	r.degraded[tty] = true
	delete(r.owner, tty)
	delete(r.notFound, tty)
}

// resolveOwners groups surfaces by canonical tty and decides ownership. When
// probe is true it runs the liveness seam for every claimant set, including an
// uncontested one; unknown liveness is never treated as ownership. When false
// (the post-eviction rebuild) it only reads the tree and marks any still-
// contested tty degraded.
func (c Cmux) resolveOwners(surfaces map[string]cmuxSurfaceIdentity, probe bool) cmuxResolution {
	res := cmuxResolution{
		claimants: map[string][]string{},
		owner:     map[string]string{},
		degraded:  map[string]bool{},
		notFound:  map[string]bool{},
	}
	// Group live claimants per canonical tty, excluding sentinel-tty surfaces
	// (already retired) and explicit process_alive:false corpses. A corpse is
	// evictable regardless of contention.
	grouped := map[string][]string{}
	workspaceOf := map[string]string{}
	for id, surface := range surfaces {
		workspaceOf[id] = surface.WorkspaceID
		tty, ttyErr := canonicalCmuxTTY(surface.TTY)
		if ttyErr != nil {
			continue
		}
		if tty == cmuxEvictedTTY {
			continue
		}
		if surface.ProcessAlive != nil && !*surface.ProcessAlive {
			if probe {
				res.evictions = append(res.evictions, cmuxEviction{surfaceID: id, workspaceID: surface.WorkspaceID, tty: tty})
			}
			continue
		}
		grouped[tty] = append(grouped[tty], id)
	}

	for tty, ids := range grouped {
		sort.Strings(ids)
		res.claimants[tty] = ids
		if !probe {
			// Rebuild pass: a tty still contested after eviction cannot be
			// resolved without another probe round, which we do not do.
			if len(ids) == 1 {
				res.owner[tty] = ids[0]
			} else {
				res.degraded[tty] = true
			}
			continue
		}
		c.resolveClaimants(&res, tty, ids, workspaceOf)
	}
	return res
}

// resolveClaimants applies kernel liveness to every claimant set, including a
// single unknown-liveness surface. A probe error degrades; zero live owners
// marks the tty absent and queues eviction; a live uncontested claimant owns
// the tty; two or more live claimants stay ambiguous.
func (c Cmux) resolveClaimants(res *cmuxResolution, tty string, ids []string, workspaceOf map[string]string) {
	count, err := c.liveTTYOwnerCount(tty)
	if err != nil {
		res.degraded[tty] = true
		c.logf("WARN cmux tty-liveness unavailable, degraded to fail-closed: tty=%s reason=%v", tty, err)
		return
	}
	if count == 0 {
		res.notFound[tty] = true
		for _, id := range ids {
			res.evictions = append(res.evictions, cmuxEviction{surfaceID: id, workspaceID: workspaceOf[id], tty: tty})
		}
		return
	}
	if len(ids) == 1 {
		res.owner[tty] = ids[0]
		return
	}
	res.degraded[tty] = true
	c.logf(
		"WARN cmux tty ownership ambiguous, degraded to fail-closed: tty=%s claimants=%s; inspect cmux aliases and existing wakes manually",
		tty,
		strings.Join(ids, ","),
	)
}

func (i cmuxTargetInventory) Probe(target string) error {
	_, err := i.OwnershipKey(target)
	return err
}

func (i cmuxTargetInventory) OwnershipKey(target string) (string, error) {
	id, surface, err := i.lookup(target)
	if err != nil {
		return "", err
	}
	if !isCmuxTerminalType(surface.Type) {
		return "", fmt.Errorf("%w: cmux target %q has type %q, want terminal", ErrTargetDegraded, target, surface.Type)
	}
	tty, err := canonicalCmuxTTY(surface.TTY)
	if err != nil {
		if strings.TrimSpace(surface.TTY) != "" {
			return "", fmt.Errorf("%w: cmux target %q physical identity is ambiguous: %v", ErrTargetDegraded, target, err)
		}
		return i.commitOwnershipKey(id, "surface:"+id)
	}
	if tty == cmuxEvictedTTY {
		return "", fmt.Errorf("%w: cmux target %q was retired as a stale tty alias", ErrTargetNotFound, target)
	}
	if i.notFound[tty] {
		return "", fmt.Errorf("%w: cmux target %q tty %q has no live surface owner", ErrTargetNotFound, target, tty)
	}
	if i.degraded[tty] {
		return "", i.ambiguityError(target, tty)
	}
	if owner, ok := i.owner[tty]; !ok || owner != id {
		return "", i.ambiguityError(target, tty)
	}
	return i.commitOwnershipKey(id, "tty:"+tty)
}

func (i cmuxTargetInventory) commitOwnershipKey(id, key string) (string, error) {
	if err := i.recorded.accept(id, key); err != nil {
		return "", err
	}
	return key, nil
}

func (r *cmuxOwnershipRecord) accept(id, key string) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.keys == nil {
		r.keys = map[string]string{}
	}
	prev, ok := r.keys[id]
	if !ok || prev == key {
		r.keys[id] = key
		return nil
	}
	if strings.HasPrefix(prev, "surface:") && strings.HasPrefix(key, "tty:") {
		r.keys[id] = key
		return nil
	}
	return fmt.Errorf("%w: cmux surface %s ownership key conflict: recorded %q now %q", ErrTargetDegraded, id, prev, key)
}

// WithOwnershipRecord returns a copy whose last-key map survives Inventory
// rebuilds. Production DefaultRegistry already includes one; tests that
// exercise UUID→TTY drift across supervise ticks must use this so
// RememberOwnership can seed keys persisted on a registration.
func (c Cmux) WithOwnershipRecord() Cmux {
	if c.recorded == nil {
		c.recorded = newCmuxOwnershipRecord()
	}
	return c
}

// RememberOwnership seeds the last-known physical key for a surface. The
// supervisor loads keys persisted on registry entries so a restart still
// refuses UUID→TTY drift. Empty keys are ignored.
func (c Cmux) RememberOwnership(target, key string) {
	if c.recorded == nil {
		return
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}
	id, err := parseCmuxSurfaceTarget(target)
	if err != nil {
		return
	}
	c.recorded.mu.Lock()
	defer c.recorded.mu.Unlock()
	if c.recorded.keys == nil {
		c.recorded.keys = map[string]string{}
	}
	c.recorded.keys[id] = key
}

func (i cmuxTargetInventory) ambiguityError(target, tty string) error {
	owners := i.claimants[tty]
	return &cmuxDegradedOwnershipError{
		inventoryToken: i.ownershipToken,
		ownershipKey:   "tty:" + tty,
		detail: fmt.Sprintf(
			"cmux target %q physical identity is ambiguous: tty %q has %d live surface aliases: %s; inspect cmux aliases and existing wakes manually",
			target, tty, len(owners), strings.Join(owners, ", "),
		),
	}
}

// CmuxDegradedOwnershipKey returns a physical key only when the error came
// from the same concrete cmux inventory type used for the candidate lookup.
// This prevents generic or third-party TargetInventory implementations from
// turning ErrTargetDegraded into permission to skip an uncertain owner.
func CmuxDegradedOwnershipKey(inventory TargetInventory, err error) (string, bool) {
	var inventoryToken *cmuxOwnershipToken
	switch typed := inventory.(type) {
	case cmuxTargetInventory:
		inventoryToken = typed.ownershipToken
	case *cmuxTargetInventory:
		inventoryToken = typed.ownershipToken
	default:
		return "", false
	}
	var degraded *cmuxDegradedOwnershipError
	if inventoryToken == nil || !errors.As(err, &degraded) || degraded.inventoryToken != inventoryToken || degraded.ownershipKey == "" {
		return "", false
	}
	return degraded.ownershipKey, true
}

func canonicalCmuxTTY(value string) (string, error) {
	tty := strings.TrimSpace(value)
	if tty == "" {
		return "", errors.New("system.tree surface tty is missing or blank")
	}
	if tty == cmuxEvictedTTYName || tty == cmuxEvictedTTY {
		return cmuxEvictedTTY, nil
	}
	if filepath.IsAbs(tty) {
		if filepath.Clean(tty) != tty {
			return "", fmt.Errorf("system.tree surface tty %q is not a canonical device path", value)
		}
	} else {
		if filepath.Base(tty) != tty {
			return "", fmt.Errorf("relative tty %q is not a device basename", value)
		}
		tty = filepath.Join("/dev", tty)
	}
	if !isCmuxPTY(tty) {
		return "", fmt.Errorf("system.tree surface tty %q is not a macOS PTY under /dev/ttys<digits>", value)
	}
	return tty, nil
}

func isCmuxPTY(tty string) bool {
	if filepath.Dir(tty) != "/dev" {
		return false
	}
	suffix, ok := strings.CutPrefix(filepath.Base(tty), "ttys")
	if !ok || suffix == "" {
		return false
	}
	for _, char := range suffix {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func sameCmuxSurfaceIdentity(left, right cmuxSurfaceIdentity) bool {
	if left.WorkspaceID != right.WorkspaceID || left.Type != right.Type || !sameOptionalBool(left.ProcessAlive, right.ProcessAlive) {
		return false
	}
	leftTTY, leftErr := canonicalCmuxTTY(left.TTY)
	rightTTY, rightErr := canonicalCmuxTTY(right.TTY)
	if leftErr == nil && rightErr == nil {
		return leftTTY == rightTTY
	}
	return strings.TrimSpace(left.TTY) == strings.TrimSpace(right.TTY)
}

func sameOptionalBool(left, right *bool) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func (i cmuxTargetInventory) lookup(target string) (string, cmuxSurfaceIdentity, error) {
	id, err := parseCmuxSurfaceTarget(target)
	if err != nil {
		return "", cmuxSurfaceIdentity{}, err
	}
	surface, ok := i.surfaces[id]
	if !ok {
		return "", cmuxSurfaceIdentity{}, fmt.Errorf("%w: cmux target %q is absent from system.tree", ErrTargetNotFound, target)
	}
	return id, surface, nil
}

func (c Cmux) Inject(ctx context.Context, target string, payload string) error {
	if err := requireCmuxPlatform(); err != nil {
		return err
	}
	id, err := parseCmuxSurfaceTarget(target)
	if err != nil {
		return err
	}
	inventory, err := c.Inventory(ctx, OwnershipContext{})
	if err != nil {
		return fmt.Errorf("verify cmux target before injection: %w", err)
	}
	if err := inventory.Probe(target); err != nil {
		return fmt.Errorf("verify cmux target before injection: %w", err)
	}
	path, err := c.executable()
	if err != nil {
		return err
	}

	payload = sanitizePayloadForSubmit(payload)
	textParams, err := json.Marshal(map[string]string{
		"surface_id": id,
		"text":       payload,
	})
	if err != nil {
		return fmt.Errorf("encode cmux text parameters: %w", err)
	}
	if out, err := c.runner().Run(ctx, path, "rpc", "surface.send_text", string(textParams)); err != nil {
		return fmt.Errorf("inject text into cmux target %q: %w: %s", target, err, strings.TrimSpace(string(out)))
	}

	if err := c.sleep(ctx, c.settleDelay()); err != nil {
		return fmt.Errorf("wait before submitting cmux target %q: %w", target, err)
	}

	keyParams, err := json.Marshal(map[string]string{
		"key":        "enter",
		"surface_id": id,
	})
	if err != nil {
		return fmt.Errorf("encode cmux key parameters: %w", err)
	}
	if out, err := c.runner().Run(ctx, path, "rpc", "surface.send_key", string(keyParams)); err != nil {
		return fmt.Errorf("submit cmux target %q: %w: %s", target, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func newCmuxInventory(surfaces map[string]cmuxSurfaceIdentity, res cmuxResolution) cmuxTargetInventory {
	return cmuxTargetInventory{
		ownershipToken: &cmuxOwnershipToken{},
		surfaces:       surfaces,
		claimants:      res.claimants,
		owner:          res.owner,
		degraded:       res.degraded,
		notFound:       res.notFound,
	}
}

func (c Cmux) snapshotInventory(surfaces map[string]cmuxSurfaceIdentity, res cmuxResolution) cmuxTargetInventory {
	inventory := newCmuxInventory(surfaces, res)
	inventory.recorded = c.recorded
	return inventory
}

func isCmuxTerminalType(value string) bool {
	return strings.EqualFold(strings.TrimSpace(value), "terminal")
}

// evict retracts each corpse alias via surface.report_tty, writing the sentinel
// tty name. It returns the set of canonical ttys whose eviction did not land and
// whether at least one eviction succeeded. There is no CAS RPC, so a fresh
// system.tree snapshot must still show the same stale identity before
// report_tty; a live rebind aborts without overwrite and degrades. A failed
// report_tty never crashes the pass and never mutates the registry; the
// affected tty keeps its fail-closed state and is retried on the next pass.
func (c Cmux) evict(ctx context.Context, path string, evictions []cmuxEviction) (map[string]bool, bool) {
	failed := map[string]bool{}
	succeeded := 0
	current, err := c.fetchSurfaces(ctx, path)
	if err != nil {
		for _, ev := range evictions {
			failed[ev.tty] = true
		}
		c.logf("WARN cmux eviction CAS snapshot failed: %v", err)
		return failed, false
	}
	for _, ev := range evictions {
		if ev.workspaceID == "" {
			failed[ev.tty] = true
			c.logf("WARN cmux eviction failed: tty=%s surface=%s reason=%v", ev.tty, ev.surfaceID, errors.New("missing workspace id"))
			continue
		}
		if !c.stillEvictable(current, ev) {
			failed[ev.tty] = true
			c.logf("WARN cmux eviction aborted, surface rebinding or identity drifted: tty=%s surface=%s", ev.tty, ev.surfaceID)
			continue
		}
		params, err := json.Marshal(map[string]string{
			"workspace_id": ev.workspaceID,
			"surface_id":   ev.surfaceID,
			"tty_name":     cmuxEvictedTTYName,
		})
		if err != nil {
			failed[ev.tty] = true
			c.logf("WARN cmux eviction failed: tty=%s surface=%s reason=%v", ev.tty, ev.surfaceID, err)
			continue
		}
		if out, err := c.runner().Run(ctx, path, "rpc", "surface.report_tty", string(params)); err != nil {
			failed[ev.tty] = true
			c.logf("WARN cmux eviction failed: tty=%s surface=%s reason=%v output=%s", ev.tty, ev.surfaceID, err, strings.TrimSpace(string(out)))
			continue
		}
		succeeded++
	}
	if succeeded > 0 {
		c.logf("INFO cmux evicted %d corpse alias(es)", succeeded)
	}
	return failed, succeeded > 0
}

// stillEvictable is the pre-report_tty CAS: the surface must still claim the
// same tty in the same workspace, and it must still be a proven corpse
// (explicit process_alive:false or zero live kernel owners). A live rebind
// changes tty or liveness and must not be overwritten.
func (c Cmux) stillEvictable(surfaces map[string]cmuxSurfaceIdentity, ev cmuxEviction) bool {
	surface, ok := surfaces[ev.surfaceID]
	if !ok || surface.WorkspaceID == "" || surface.WorkspaceID != ev.workspaceID {
		return false
	}
	tty, err := canonicalCmuxTTY(surface.TTY)
	if err != nil || tty != ev.tty || tty == cmuxEvictedTTY {
		return false
	}
	if surface.ProcessAlive != nil && !*surface.ProcessAlive {
		return true
	}
	count, err := c.liveTTYOwnerCount(ev.tty)
	return err == nil && count == 0
}

func (c Cmux) liveTTYOwnerCount(devPath string) (int, error) {
	if c.LiveTTYOwnerCount != nil {
		return c.LiveTTYOwnerCount(devPath)
	}
	return ttyLiveOwnerCount(devPath)
}

func (c Cmux) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

func (c Cmux) runner() CommandRunner {
	if c.Runner != nil {
		return c.Runner
	}
	return ExecRunner{}
}

func (c Cmux) executable() (string, error) {
	if strings.TrimSpace(c.Path) != "" {
		return filepath.Clean(strings.TrimSpace(c.Path)), nil
	}
	if override := strings.TrimSpace(c.getenv("AMQ_KEEPALIVE_CMUX")); override != "" {
		path, err := c.resolveCandidate(override)
		if err != nil {
			return "", fmt.Errorf("resolve AMQ_KEEPALIVE_CMUX: %w", err)
		}
		return path, nil
	}
	if bundled := strings.TrimSpace(c.getenv("CMUX_BUNDLED_CLI_PATH")); bundled != "" && c.isExecutable(bundled) {
		return filepath.Clean(bundled), nil
	}
	if path, err := c.lookPath("cmux"); err == nil {
		return filepath.Clean(path), nil
	}

	candidates := []string{"/Applications/cmux.app/Contents/Resources/bin/cmux"}
	if home, err := c.userHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		candidates = append(candidates, filepath.Join(home, "Applications", "cmux.app", "Contents", "Resources", "bin", "cmux"))
	}
	for _, candidate := range candidates {
		if c.isExecutable(candidate) {
			return filepath.Clean(candidate), nil
		}
	}
	return "", fmt.Errorf("cmux CLI not found; set AMQ_KEEPALIVE_CMUX or install the bundled CLI at %s", candidates[0])
}

func (c Cmux) resolveCandidate(candidate string) (string, error) {
	if filepath.IsAbs(candidate) || strings.ContainsRune(candidate, filepath.Separator) {
		if !c.isExecutable(candidate) {
			return "", fmt.Errorf("%q is not an executable file", candidate)
		}
		return filepath.Clean(candidate), nil
	}
	path, err := c.lookPath(candidate)
	if err != nil {
		return "", err
	}
	return filepath.Clean(path), nil
}

func (c Cmux) getenv(key string) string {
	if c.Getenv != nil {
		return c.Getenv(key)
	}
	return os.Getenv(key)
}

func (c Cmux) lookPath(file string) (string, error) {
	if c.LookPath != nil {
		return c.LookPath(file)
	}
	return exec.LookPath(file)
}

func (c Cmux) userHomeDir() (string, error) {
	if c.UserHomeDir != nil {
		return c.UserHomeDir()
	}
	return os.UserHomeDir()
}

func (c Cmux) isExecutable(path string) bool {
	if c.IsExecutable != nil {
		return c.IsExecutable(path)
	}
	return isExecutableFile(path)
}

func (c Cmux) sleep(ctx context.Context, delay time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, delay)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c Cmux) settleDelay() time.Duration {
	if c.SettleDelay < 0 {
		return 0
	}
	if c.SettleDelay == 0 {
		return defaultCmuxSettleDelay
	}
	return c.SettleDelay
}

func requireCmuxPlatform() error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("cmux adapter requires macOS, got %s", runtime.GOOS)
	}
	return nil
}

func parseCmuxSurfaceTarget(target string) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", errors.New("cmux adapter target is required")
	}
	id, ok := strings.CutPrefix(target, cmuxSurfaceTargetPrefix)
	if !ok {
		return "", fmt.Errorf("unsupported cmux target %q; reattach required: run reattach --adapter cmux from the target surface", target)
	}
	id, err := normalizeCmuxSurfaceID(id)
	if err != nil {
		return "", fmt.Errorf("invalid cmux surface target: %w", err)
	}
	return strings.ToUpper(id), nil
}

func normalizeCmuxSurfaceID(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", errors.New("cmux surface id is empty")
	}
	if len(id) != 36 {
		return "", fmt.Errorf("cmux surface id %q is not a UUID", id)
	}
	for i, r := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if r != '-' {
				return "", fmt.Errorf("cmux surface id %q is not a UUID", id)
			}
			continue
		}
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return "", fmt.Errorf("cmux surface id %q is not a UUID", id)
		}
	}
	return id, nil
}

func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0
}
