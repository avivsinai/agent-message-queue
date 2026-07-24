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
	"time"
)

const (
	cmuxSurfaceTargetPrefix = "cmux:surface:"
	defaultCmuxSettleDelay  = 150 * time.Millisecond
)

type Cmux struct {
	Runner       CommandRunner
	Path         string
	Getenv       func(string) string
	LookPath     func(string) (string, error)
	UserHomeDir  func() (string, error)
	IsExecutable func(string) bool
	Sleep        func(context.Context, time.Duration) error
	SettleDelay  time.Duration
}

type cmuxSystemTree struct {
	Windows *[]cmuxWindow `json:"windows"`
}

type cmuxWindow struct {
	Workspaces *[]cmuxWorkspace `json:"workspaces"`
}

type cmuxWorkspace struct {
	Panes *[]cmuxPane `json:"panes"`
}

type cmuxPane struct {
	Surfaces *[]cmuxSurface `json:"surfaces"`
}

type cmuxSurface struct {
	ID  string `json:"id"`
	TTY string `json:"tty"`
}

type cmuxSurfaceIdentity struct {
	TTY string
}

type cmuxTargetInventory struct {
	surfaces  map[string]cmuxSurfaceIdentity
	ttyOwners map[string][]string
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
	inventory, err := c.Inventory(ctx)
	if err != nil {
		return err
	}
	return inventory.Probe(target)
}

func (c Cmux) Inventory(ctx context.Context) (TargetInventory, error) {
	if err := requireCmuxPlatform(); err != nil {
		return nil, err
	}
	path, err := c.executable()
	if err != nil {
		return nil, err
	}
	out, err := c.runner().Run(ctx, path, "rpc", "system.tree", "{}")
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
	inventory := cmuxTargetInventory{
		surfaces:  map[string]cmuxSurfaceIdentity{},
		ttyOwners: map[string][]string{},
	}
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
					identity := cmuxSurfaceIdentity{TTY: strings.TrimSpace(surface.TTY)}
					if previous, exists := inventory.surfaces[id]; exists && !sameCmuxSurfaceIdentity(previous, identity) {
						return nil, fmt.Errorf("parse cmux system.tree: surface %q has conflicting tty identities %q and %q", id, previous.TTY, identity.TTY)
					}
					inventory.surfaces[id] = identity
				}
			}
		}
	}
	for id, surface := range inventory.surfaces {
		tty, ttyErr := canonicalCmuxTTY(surface.TTY)
		if ttyErr != nil {
			continue
		}
		inventory.ttyOwners[tty] = append(inventory.ttyOwners[tty], id)
	}
	for tty := range inventory.ttyOwners {
		sort.Strings(inventory.ttyOwners[tty])
	}
	return inventory, nil
}

func (i cmuxTargetInventory) Probe(target string) error {
	_, err := i.OwnershipKey(target)
	return err
}

func (i cmuxTargetInventory) OwnershipKey(target string) (string, error) {
	_, surface, err := i.lookup(target)
	if err != nil {
		return "", err
	}
	tty, err := canonicalCmuxTTY(surface.TTY)
	if err != nil {
		return "", fmt.Errorf("cmux target %q physical identity is ambiguous: %w", target, err)
	}
	owners := i.ttyOwners[tty]
	if len(owners) != 1 {
		return "", fmt.Errorf(
			"cmux target %q physical identity is ambiguous: tty %q has %d live surface aliases: %s; inspect cmux aliases and existing wakes manually",
			target, tty, len(owners), strings.Join(owners, ", "),
		)
	}
	return "tty:" + tty, nil
}

func canonicalCmuxTTY(value string) (string, error) {
	tty := strings.TrimSpace(value)
	if tty == "" {
		return "", errors.New("system.tree surface tty is missing or blank")
	}
	if !filepath.IsAbs(tty) {
		if filepath.Base(tty) != tty {
			return "", fmt.Errorf("relative tty %q is not a device basename", value)
		}
		tty = filepath.Join("/dev", tty)
	}
	return filepath.Clean(tty), nil
}

func sameCmuxSurfaceIdentity(left, right cmuxSurfaceIdentity) bool {
	if left == right {
		return true
	}
	leftTTY, leftErr := canonicalCmuxTTY(left.TTY)
	rightTTY, rightErr := canonicalCmuxTTY(right.TTY)
	return leftErr == nil && rightErr == nil && leftTTY == rightTTY
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
	inventory, err := c.Inventory(ctx)
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
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return "", fmt.Errorf("cmux surface id %q is not a UUID", id)
		}
	}
	return id, nil
}

func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0
}
