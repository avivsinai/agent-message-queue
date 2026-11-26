package usage

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Settings is the "usage" key of the board's user configuration
// (~/.amq/board.json). Enabled false reads nothing.
type Settings struct {
	Enabled bool
	Config  Config
	// Warnings name the refused price entries, which are ignored.
	Warnings []string
}

// LoadSettings reads the "usage" key of the board configuration at path.
// A missing file or key gives the defaults under home. Homes listed under
// "homes" are read in addition to the harness's default home, each as its
// own account; a home listed twice, or the default listed again, is read
// once. A home is absolute or starts with "~/". The budget takes a
// Go duration ("5s") and a byte size ("512MB"; KB, MB and GB are 1024-based,
// so the default 512MB is 512 << 20 bytes). Entries under "prices"."models"
// replace the shipped entry with the same match or add one; a refused entry
// is a warning and is ignored, and never turns usage off.
func LoadSettings(path, home string) (Settings, error) {
	st := Settings{Enabled: true, Config: DefaultConfig(home)}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	var file struct {
		Usage *struct {
			Enabled *bool               `json:"enabled"`
			Homes   map[string][]string `json:"homes"`
			Budget  *struct {
				Wall  string `json:"wall"`
				Bytes string `json:"bytes"`
			} `json:"budget"`
			Prices json.RawMessage `json:"prices"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return st, fmt.Errorf("%s: %w", path, err)
	}
	u := file.Usage
	if u == nil {
		return st, nil
	}
	if u.Enabled != nil {
		st.Enabled = *u.Enabled
	}
	if len(u.Prices) > 0 {
		var user []Price
		user, st.Warnings = userPrices(path, u.Prices)
		st.Config.Prices = st.Config.Prices.with(user)
	}
	for harness, list := range u.Homes {
		if harness != Claude && harness != Codex && harness != Pi {
			return st, fmt.Errorf("%s: usage.homes: unknown harness %q (want claude, codex or pi)", path, harness)
		}
		for _, h := range list {
			p, err := expandHome(h, home)
			if err != nil {
				return st, fmt.Errorf("%s: usage.homes.%s: %w", path, harness, err)
			}
			if !slices.Contains(st.Config.Homes[harness], p) {
				st.Config.Homes[harness] = append(st.Config.Homes[harness], p)
			}
		}
	}
	if b := u.Budget; b != nil {
		if b.Wall != "" {
			d, err := time.ParseDuration(b.Wall)
			if err != nil || d <= 0 {
				return st, fmt.Errorf("%s: usage.budget.wall: %q is not a positive duration", path, b.Wall)
			}
			st.Config.BudgetWall = d
		}
		if b.Bytes != "" {
			n, err := parseBytes(b.Bytes)
			if err != nil {
				return st, fmt.Errorf("%s: usage.budget.bytes: %w", path, err)
			}
			st.Config.BudgetBytes = n
		}
	}
	return st, nil
}

// userPrices reads the user's price entries, each on its own: a refused
// entry is named in a warning and left out.
func userPrices(path string, raw json.RawMessage) ([]Price, []string) {
	var p struct {
		Models []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, []string{fmt.Sprintf("%s: usage.prices: %v; ignored", path, err)}
	}
	var out []Price
	var warnings []string
	seen := map[string]bool{}
	for i, m := range p.Models {
		e, err := parsePrice(m)
		if err == nil && seen[e.Match] {
			err = fmt.Errorf("match %q is listed earlier; the first entry stands", e.Match)
		}
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: usage.prices.models[%d] %s: %v; ignored", path, i, strings.TrimSpace(string(m)), err))
			continue
		}
		seen[e.Match] = true
		out = append(out, e)
	}
	return out, warnings
}

func expandHome(p, home string) (string, error) {
	switch {
	case p == "~":
		return home, nil
	case strings.HasPrefix(p, "~/"):
		return filepath.Join(home, filepath.FromSlash(p[2:])), nil
	case filepath.IsAbs(p):
		return filepath.Clean(p), nil
	}
	return "", fmt.Errorf("%q is neither absolute nor under ~/", p)
}

// parseBytes reads a positive size such as "300MB", "64KB" or "4096B".
func parseBytes(s string) (int64, error) {
	t := strings.ToUpper(strings.TrimSpace(s))
	shift := 0
	for _, u := range []struct {
		suffix string
		shift  int
	}{{"GB", 30}, {"MB", 20}, {"KB", 10}, {"B", 0}} {
		if num, ok := strings.CutSuffix(t, u.suffix); ok {
			t, shift = strings.TrimSpace(num), u.shift
			break
		}
	}
	n, err := strconv.ParseInt(t, 10, 64)
	if err != nil || n <= 0 || n > (1<<62)>>shift {
		return 0, fmt.Errorf("%q is not a positive size such as 300MB", s)
	}
	return n << shift, nil
}
