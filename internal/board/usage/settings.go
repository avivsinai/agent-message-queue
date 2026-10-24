package usage

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Settings is the "usage" key of the board's user configuration
// (~/.amq/board.json). Enabled false reads nothing.
type Settings struct {
	Enabled bool
	Config  Config
}

// LoadSettings reads the "usage" key of the board configuration at path.
// A missing file or key gives the defaults under home. Each harness listed
// under "homes" replaces that harness's default homes (an empty list reads
// none of it); a home is absolute or starts with "~/". The budget takes a
// Go duration ("1s") and a byte size ("300MB"; KB, MB and GB are 1024-based,
// so the default 300MB is 300 << 20 bytes).
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
	for harness, list := range u.Homes {
		if harness != Claude && harness != Codex && harness != Pi {
			return st, fmt.Errorf("%s: usage.homes: unknown harness %q (want claude, codex or pi)", path, harness)
		}
		homes := []string{}
		for _, h := range list {
			p, err := expandHome(h, home)
			if err != nil {
				return st, fmt.Errorf("%s: usage.homes.%s: %w", path, harness, err)
			}
			homes = append(homes, p)
		}
		st.Config.Homes[harness] = homes
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
