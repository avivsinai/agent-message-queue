package board

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

// BindClaudeList grants or removes project-local admission for an exact native
// home/list. It shares the same serialized publication path as owner corrections.
func (s Store) BindClaudeList(ctx context.Context, home, list string, remove bool) (string, error) {
	if !nativeName.MatchString(list) || len(list) > 256 {
		return "", errors.New("invalid Claude list ID")
	}
	real, key, err := CanonicalClaudeList(home, list)
	if err != nil && remove {
		real, err = canonicalMissingHome(home)
		key = ClaudeListKey(real, list)
	}
	if err != nil {
		return "", err
	}
	err = s.update(ctx, func(state *Snapshot) error {
		if state.ClaudeLists == nil {
			state.ClaudeLists = map[string]ClaudeListBinding{}
		}
		if remove {
			if _, ok := state.ClaudeLists[key]; !ok {
				return errors.New("claude list binding not found; use its exact stored binding key")
			}
			delete(state.ClaudeLists, key)
			return nil
		}
		if _, ok := state.ClaudeLists[key]; ok {
			return nil
		}
		revision, err := randomID("binding-")
		if err != nil {
			return err
		}
		state.ClaudeLists[key] = ClaudeListBinding{Home: real, List: list, Revision: revision}
		return nil
	})
	return key, err
}

// canonicalMissingHome resolves the surviving parent chain, so a missing home
// beneath /var retains the same /private/var identity it had while present.
func canonicalMissingHome(home string) (string, error) {
	absolute, err := filepath.Abs(home)
	if err != nil {
		return "", err
	}
	path := absolute
	var suffix []string
	for {
		real, err := filepath.EvalSymlinks(path)
		if err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				real = filepath.Join(real, suffix[i])
			}
			return real, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", err
		}
		suffix = append(suffix, filepath.Base(path))
		path = parent
	}
}

// UnbindClaudeListKey revokes the exact stored key even if a path alias that
// was used to create it no longer exists.
func (s Store) UnbindClaudeListKey(ctx context.Context, key string) error {
	return s.update(ctx, func(state *Snapshot) error {
		if _, ok := state.ClaudeLists[key]; !ok {
			return errors.New("claude list binding key not found")
		}
		delete(state.ClaudeLists, key)
		return nil
	})
}
