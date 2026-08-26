//go:build darwin || linux

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// state_5 is Codex's current state schema generation. A different file is
	// an unknown store and must not be used for automatic naming.
	codexStateFilename           = "state_5.sqlite"
	codexThreadsQuery            = "SELECT cwd, name, title, rollout_path FROM threads"
	coopNamedTUIReadbackInterval = 500 * time.Millisecond
)

var (
	coopNamedTUIReadbackTimeout = 5 * time.Second
	coopNamedReadbackSleep      = time.Sleep
	runCodexSQLiteQuery         = runCodexSQLiteQueryProcess
)

type coopNamedStoreCandidate struct {
	storePath string
	key       string
	name      string
	modTime   time.Time
}

type coopNamedStoreReader interface {
	locate(cwd string, execStart time.Time) (coopNamedStoreCandidate, error)
	readName(candidate coopNamedStoreCandidate) (string, error)
}

func coopNamedStoreReaderFor(binary string) (coopNamedStoreReader, bool) {
	switch strings.ToLower(filepath.Base(binary)) {
	case "codex":
		return codexNamedStoreReader{}, true
	case "agent":
		return cursorNamedStoreReader{}, true
	default:
		return nil, false
	}
}

func selectCoopNamedStoreCandidate(candidates []coopNamedStoreCandidate, description string) (coopNamedStoreCandidate, error) {
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].modTime.After(candidates[j].modTime)
	})
	switch len(candidates) {
	case 0:
		return coopNamedStoreCandidate{}, fmt.Errorf("no new %s store candidate", description)
	case 1:
		return candidates[0], nil
	default:
		return coopNamedStoreCandidate{}, fmt.Errorf("%d new %s store candidates are ambiguous", len(candidates), description)
	}
}

type codexThreadRow struct {
	CWD         string `json:"cwd"`
	Name        string `json:"name"`
	Title       string `json:"title"`
	RolloutPath string `json:"rollout_path"`
}

type codexNamedStoreReader struct{}

func (codexNamedStoreReader) locate(cwd string, execStart time.Time) (coopNamedStoreCandidate, error) {
	dbPath, err := codexStatePath()
	if err != nil {
		return coopNamedStoreCandidate{}, err
	}
	rows, err := readCodexThreadRows(dbPath)
	if err != nil {
		return coopNamedStoreCandidate{}, err
	}
	candidates := make([]coopNamedStoreCandidate, 0, 1)
	for _, row := range rows {
		if row.CWD != cwd || row.RolloutPath == "" {
			continue
		}
		info, statErr := os.Stat(row.RolloutPath)
		if statErr != nil {
			if errors.Is(statErr, fs.ErrNotExist) {
				continue
			}
			return coopNamedStoreCandidate{}, fmt.Errorf("stat Codex rollout %q: %w", row.RolloutPath, statErr)
		}
		if info.ModTime().Before(execStart) {
			continue
		}
		candidates = append(candidates, coopNamedStoreCandidate{
			storePath: dbPath,
			key:       row.RolloutPath,
			name:      row.Name,
			modTime:   info.ModTime(),
		})
	}
	return selectCoopNamedStoreCandidate(candidates, "Codex thread")
}

func (codexNamedStoreReader) readName(candidate coopNamedStoreCandidate) (string, error) {
	rows, err := readCodexThreadRows(candidate.storePath)
	if err != nil {
		return "", err
	}
	var name string
	found := 0
	for _, row := range rows {
		if row.RolloutPath != candidate.key {
			continue
		}
		name = row.Name
		found++
	}
	if found != 1 {
		return "", fmt.Errorf("codex rollout %q has %d matching rows", candidate.key, found)
	}
	if strings.TrimSpace(name) != "" {
		return name, nil
	}
	// Codex versions before the explicit thread-name field persisted /rename
	// in title. Keep that legacy store readable without treating generated
	// titles as an existing explicit name before injection.
	for _, row := range rows {
		if row.RolloutPath == candidate.key {
			return row.Title, nil
		}
	}
	return "", nil
}

func codexStatePath() (string, error) {
	home := strings.TrimSpace(os.Getenv("CODEX_HOME"))
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve Codex home: %w", err)
		}
	}
	if home == "" {
		return "", errors.New("codex home is empty")
	}
	return filepath.Join(home, codexStateFilename), nil
}

func readCodexThreadRows(dbPath string) ([]codexThreadRow, error) {
	data, err := runCodexSQLiteQuery(dbPath)
	if err != nil {
		return nil, err
	}
	var rows []codexThreadRow
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&rows); err != nil {
		return nil, fmt.Errorf("decode Codex thread store: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, errors.New("decode Codex thread store: multiple JSON values")
		}
		return nil, fmt.Errorf("decode Codex thread store: %w", err)
	}
	return rows, nil
}

func runCodexSQLiteQueryProcess(dbPath string) ([]byte, error) {
	cmd := exec.Command("sqlite3", "-readonly", "-json", dbPath, codexThreadsQuery)
	data, err := cmd.Output()
	if err == nil {
		return data, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(exitErr.Stderr) != 0 {
		return nil, fmt.Errorf("sqlite3 query failed: %s", strings.TrimSpace(string(exitErr.Stderr)))
	}
	return nil, fmt.Errorf("run sqlite3 query: %w", err)
}

type cursorChatMeta struct {
	Title string `json:"title"`
	CWD   string `json:"cwd"`
}

type cursorNamedStoreReader struct{}

func (cursorNamedStoreReader) locate(cwd string, execStart time.Time) (coopNamedStoreCandidate, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return coopNamedStoreCandidate{}, fmt.Errorf("resolve Cursor home: %w", err)
	}
	root := filepath.Join(home, ".cursor", "chats")
	var candidates []coopNamedStoreCandidate
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || entry.Name() != "meta.json" || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		if !info.Mode().IsRegular() || info.ModTime().Before(execStart) {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		var meta cursorChatMeta
		if err := json.Unmarshal(data, &meta); err != nil {
			return fmt.Errorf("decode Cursor metadata %q: %w", path, err)
		}
		if meta.CWD != cwd {
			return nil
		}
		candidates = append(candidates, coopNamedStoreCandidate{
			storePath: path,
			name:      meta.Title,
			modTime:   info.ModTime(),
		})
		return nil
	})
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return coopNamedStoreCandidate{}, errors.New("cursor chat store is absent")
		}
		return coopNamedStoreCandidate{}, fmt.Errorf("scan Cursor chat store: %w", err)
	}
	return selectCoopNamedStoreCandidate(candidates, "Cursor chat")
}

func (cursorNamedStoreReader) readName(candidate coopNamedStoreCandidate) (string, error) {
	data, err := os.ReadFile(candidate.storePath)
	if err != nil {
		return "", err
	}
	var meta cursorChatMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return "", fmt.Errorf("decode Cursor metadata %q: %w", candidate.storePath, err)
	}
	return meta.Title, nil
}
