package board

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/avivsinai/agent-message-queue/internal/board/decision"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/board/usage"
)

// ConfigFile is the board configuration's name, under ~/.amq for the user
// file and under <project>/.amq for project overrides.
const ConfigFile = "board.json"

// Provider is an external provider the owner registered in the user file.
// Only the user file may define its executable and arguments.
type Provider struct {
	Name    string
	Exec    string
	Args    []string
	TTL     time.Duration
	Enabled bool
}

// FileConfig is the merged user and project configuration.
type FileConfig struct {
	Suggestions        decision.Config
	Board              Config
	BeadsAssignees     []string
	GitHubLogins       []string
	GitHubIdentities   map[string]string // login -> AMQ handle
	ObligationMaxAge   string            // passed to amq thread open --max-age
	SourceBeads        bool
	SourceGitHub       bool
	GitHubRepos        []string // owner/repo
	GitHubTTL          time.Duration
	TokenOnce          bool
	ReadyToMergeWaitsH bool
	Providers          []Provider
	// Usage is the "usage" key of the user file, loaded with the rest so
	// that serve keeps it with the last good configuration.
	Usage usage.Settings
}

// DefaultFileConfig is the configuration with no file.
func DefaultFileConfig() FileConfig {
	return FileConfig{
		Board:              DefaultConfig(),
		GitHubIdentities:   map[string]string{},
		SourceBeads:        true,
		SourceGitHub:       true,
		GitHubTTL:          10 * time.Minute,
		TokenOnce:          true,
		ReadyToMergeWaitsH: true,
	}
}

type rawConfig struct {
	Suggestions *struct {
		Endpoint string `json:"endpoint"`
		Model    string `json:"model"`
		Digest   string `json:"digest"`
	} `json:"suggestions"`
	Humans *struct {
		BeadsAssignees []string `json:"beads_assignees"`
		GitHubLogins   []string `json:"github_logins"`
	} `json:"humans"`
	Identities *struct {
		GitHub map[string]string `json:"github"`
	} `json:"identities"`
	LivenessWindow   *string `json:"liveness_window"`
	StaleAfter       *string `json:"stale_after"`
	ObligationMaxAge *string `json:"obligation_max_age"`
	Sources          *struct {
		Beads  *bool `json:"beads"`
		GitHub *bool `json:"github"`
	} `json:"sources"`
	Forges *struct {
		GitHub *rawGitHub `json:"github"`
	} `json:"forges"`
	Providers []rawProvider `json:"providers"`
}

type rawGitHub struct {
	Repos                    []string `json:"repos"`
	TTL                      *string  `json:"ttl"`
	TokenOnce                *bool    `json:"token_once"`
	ReadyToMergeWaitsOnHuman *bool    `json:"ready_to_merge_waits_on_human"`
}

type rawProvider struct {
	Name    string   `json:"name"`
	Exec    *string  `json:"exec"`
	Args    []string `json:"args"`
	TTL     *string  `json:"ttl"`
	Enabled *bool    `json:"enabled"`
}

var (
	providerNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	repoSlugRe     = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

// LoadConfig reads the user file and then the project file. A missing file
// is not an error. The project file may set only display and scope keys and
// enable or disable providers the user file registers; anything else in it
// is refused with one warning line and dropped. An unreadable or invalid
// file is reported as a warning and leaves the defaults in place.
func LoadConfig(userPath, projectPath string) (FileConfig, []string) {
	cfg := DefaultFileConfig()
	var warnings []string
	warn := func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }

	if raw, ok := readRawConfig(userPath, warn); ok {
		applyUser(&cfg, raw, userPath, warn)
	}
	if raw, ok := readRawConfig(projectPath, warn); ok {
		applyProject(&cfg, raw, projectPath, warn)
	}
	return cfg, warnings
}

func readRawConfig(path string, warn func(string, ...any)) (rawConfig, bool) {
	var raw rawConfig
	if path == "" {
		return raw, false
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return raw, false
	}
	if err != nil {
		warn("%s: %v; using defaults", path, err)
		return raw, false
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		warn("%s: %v; using defaults", path, err)
		return raw, false
	}
	return raw, true
}

func applyUser(cfg *FileConfig, raw rawConfig, path string, warn func(string, ...any)) {
	if raw.Suggestions != nil {
		p := raw.Suggestions
		u, err := url.Parse(p.Endpoint)
		_, digestErr := hex.DecodeString(p.Digest)
		if err != nil || u.Scheme != "http" || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback() || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || p.Model != "qwen3.5:4b" || len(p.Digest) != 64 || digestErr != nil {
			warn("%s: suggestions need literal loopback HTTP, qwen3.5:4b and a full digest; refused", path)
		} else {
			cfg.Suggestions = decision.Config{Endpoint: p.Endpoint, Model: p.Model, Digest: p.Digest}
		}
	}
	if h := raw.Humans; h != nil {
		cfg.BeadsAssignees = append([]string(nil), h.BeadsAssignees...)
		cfg.GitHubLogins = append([]string(nil), h.GitHubLogins...)
	}
	if id := raw.Identities; id != nil {
		for login, handle := range id.GitHub {
			if !validHandle(handle) {
				warn("%s: identities.github[%q]: invalid handle %q; ignored", path, login, handle)
				continue
			}
			cfg.GitHubIdentities[strings.ToLower(login)] = handle
		}
	}
	applyShared(cfg, raw, path, warn)
	if g := githubOf(raw); g != nil && g.TokenOnce != nil {
		cfg.TokenOnce = *g.TokenOnce
	}
	if g := githubOf(raw); g != nil && g.TTL != nil {
		if d, err := ParseDuration(*g.TTL); err == nil && d > 0 {
			cfg.GitHubTTL = d
		} else {
			warn("%s: forges.github.ttl %q is not a duration; ignored", path, *g.TTL)
		}
	}
	seen := map[string]bool{}
	for _, p := range raw.Providers {
		switch {
		case !providerNameRe.MatchString(p.Name):
			warn("%s: provider name %q is invalid; ignored", path, p.Name)
			continue
		case seen[p.Name]:
			warn("%s: provider %q is listed twice; the first entry wins", path, p.Name)
			continue
		case p.Exec == nil || !filepath.IsAbs(*p.Exec):
			warn("%s: provider %q needs an absolute exec path; ignored", path, p.Name)
			continue
		}
		seen[p.Name] = true
		prov := Provider{Name: p.Name, Exec: *p.Exec, Args: append([]string(nil), p.Args...), TTL: 10 * time.Minute, Enabled: true}
		if p.TTL != nil {
			if d, err := ParseDuration(*p.TTL); err == nil {
				prov.TTL = max(d, time.Minute)
			} else {
				warn("%s: provider %q ttl %q is not a duration; using 10m", path, p.Name, *p.TTL)
			}
		}
		if p.Enabled != nil {
			prov.Enabled = *p.Enabled
		}
		cfg.Providers = append(cfg.Providers, prov)
	}
}

func applyProject(cfg *FileConfig, raw rawConfig, path string, warn func(string, ...any)) {
	if raw.Suggestions != nil {
		warn("%s: suggestions provider pins may be set only in the user file; refused", path)
	}
	if raw.Humans != nil {
		warn("%s: humans may be set only in the user file; ignored", path)
	}
	if raw.Identities != nil {
		warn("%s: identities may be set only in the user file; ignored", path)
	}
	if g := githubOf(raw); g != nil {
		if g.TokenOnce != nil {
			warn("%s: forges.github.token_once may be set only in the user file; ignored", path)
		}
		if g.TTL != nil {
			warn("%s: forges.github.ttl may be set only in the user file; ignored", path)
		}
	}
	applyShared(cfg, raw, path, warn)
	for _, p := range raw.Providers {
		if p.Exec != nil || p.Args != nil || p.TTL != nil {
			warn("%s: provider %q: exec, args, and ttl may be set only in the user file; refused", path, p.Name)
			continue
		}
		i := providerIndex(cfg.Providers, p.Name)
		if i < 0 {
			warn("%s: provider %q is not registered in the user file; refused", path, p.Name)
			continue
		}
		if p.Enabled != nil {
			cfg.Providers[i].Enabled = *p.Enabled
		}
	}
}

// applyShared applies the keys both files may set.
func applyShared(cfg *FileConfig, raw rawConfig, path string, warn func(string, ...any)) {
	setDuration := func(name string, v *string, dst *time.Duration) {
		if v == nil {
			return
		}
		d, err := ParseDuration(*v)
		if err != nil || d <= 0 {
			warn("%s: %s %q is not a positive duration; ignored", path, name, *v)
			return
		}
		*dst = d
	}
	setDuration("liveness_window", raw.LivenessWindow, &cfg.Board.LivenessWindow)
	setDuration("stale_after", raw.StaleAfter, &cfg.Board.StaleAfter)
	if v := raw.ObligationMaxAge; v != nil {
		if _, err := ParseDuration(*v); err != nil {
			warn("%s: obligation_max_age %q is not a duration; ignored", path, *v)
		} else {
			cfg.ObligationMaxAge = *v
		}
	}
	if s := raw.Sources; s != nil {
		if s.Beads != nil {
			cfg.SourceBeads = *s.Beads
		}
		if s.GitHub != nil {
			cfg.SourceGitHub = *s.GitHub
		}
	}
	if g := githubOf(raw); g != nil {
		if g.Repos != nil {
			cfg.GitHubRepos = nil
			for _, r := range g.Repos {
				if !repoSlugRe.MatchString(r) {
					warn("%s: forges.github.repos entry %q is not owner/repo; ignored", path, r)
					continue
				}
				cfg.GitHubRepos = append(cfg.GitHubRepos, r)
			}
		}
		if g.ReadyToMergeWaitsOnHuman != nil {
			cfg.ReadyToMergeWaitsH = *g.ReadyToMergeWaitsOnHuman
		}
	}
}

func githubOf(raw rawConfig) *rawGitHub {
	if raw.Forges == nil {
		return nil
	}
	return raw.Forges.GitHub
}

func providerIndex(ps []Provider, name string) int {
	for i, p := range ps {
		if p.Name == name {
			return i
		}
	}
	return -1
}

// ParseDuration accepts a Go duration or a whole number of days ("14d"),
// the forms `amq thread open --max-age` accepts. "0" is zero.
func ParseDuration(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if days, ok := strings.CutSuffix(raw, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("invalid duration %q", raw)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	if raw == "0" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("invalid duration %q", raw)
	}
	return d, nil
}

// NormalizeRepos lowercases, validates, and sorts a repo set, dropping
// duplicates.
func NormalizeRepos(repos []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range repos {
		r = strings.ToLower(r)
		if repoSlugRe.MatchString(r) && !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	sort.Strings(out)
	return out
}
