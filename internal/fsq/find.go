package fsq

import (
	"os"
	"path/filepath"
)

const (
	BoxNew = "new"
	BoxCur = "cur"
)

func FindMessage(root, agent, filename string) (string, string, error) {
	newPath := filepath.Join(root, "agents", agent, "inbox", "new", filename)
	if _, err := os.Stat(newPath); err == nil {
		return newPath, BoxNew, nil
	} else if err != nil && !os.IsNotExist(err) {
		return "", "", err
	}
	curPath := filepath.Join(root, "agents", agent, "inbox", "cur", filename)
	if _, err := os.Stat(curPath); err == nil {
		return curPath, BoxCur, nil
	} else if err != nil && !os.IsNotExist(err) {
		return "", "", err
	}
	return "", "", os.ErrNotExist
}

func MoveNewToCur(root, agent, filename string) error {
	newPath := filepath.Join(root, "agents", agent, "inbox", "new", filename)
	curDir := filepath.Join(root, "agents", agent, "inbox", "cur")
	curPath := filepath.Join(curDir, filename)
	if err := os.MkdirAll(curDir, 0o700); err != nil {
		return err
	}
	if err := os.Rename(newPath, curPath); err != nil {
		return err
	}
	if err := SyncDir(filepath.Dir(newPath)); err != nil {
		return err
	}
	return SyncDir(curDir)
}
