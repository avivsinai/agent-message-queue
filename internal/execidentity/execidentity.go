// Package execidentity keeps the AMQ identity of each harness process that
// amq coop exec became. coop exec replaces itself with the harness, so the
// harness runs as coop exec's own pid; recording that pid and its process
// start before the exec lets a reader join harness-owned per-process state
// (for example Claude Code's sessions/<pid>.json) to an AMQ handle exactly,
// without changing the harness argv.
//
// The store is per user ($HOME/.amq/exec-records, mode 0700): one record per
// process under records/, and under current/ the process that the newest
// coop exec gave each (root identity, handle). A record whose process is no
// longer current for its handle is revoked. Records are never deleted by
// AMQ. The store keeps the owner user's trust boundary; it is no defense
// against another process running as the same user.
package execidentity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/amqhome"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/lock"
)

// Record is the identity coop exec ran under when it became Harness.
type Record struct {
	Schema  int                   `json:"schema"`
	Harness string                `json:"harness"`
	Holder  launch.HolderIdentity `json:"holder"`
	// StartUnix is the process start in whole seconds since the epoch.
	StartUnix  int64  `json:"start_unix"`
	Root       string `json:"root"`
	BaseRoot   string `json:"base_root"`
	Session    string `json:"session,omitempty"`
	Me         string `json:"me"`
	RootID     string `json:"root_id"`
	BaseRootID string `json:"base_root_id"`
}

// Entry is one recorded process with the time its record was written.
type Entry struct {
	Record     Record
	RecordedAt time.Time
	// Current is true when the handle's current marker names this process.
	Current bool
}

const (
	schema   = 1
	maxBytes = 4096
)

// Publish durably records r, then makes it the current process of its
// (root identity, handle), under the store lock so the record and its
// current marker change together.
func Publish(r Record) error {
	r.Schema = schema
	if err := r.check(); err != nil {
		return err
	}
	dir, err := storeDir(true)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	current, err := json.Marshal(currentRecord{Schema: schema, Process: processKey(r)})
	if err != nil {
		return err
	}
	return lock.WithExclusiveFileLock(filepath.Join(dir, ".lock"), func() error {
		for _, d := range []string{filepath.Dir(filepath.Dir(dir)), filepath.Dir(dir), dir} {
			if err := fsq.SyncDir(d); err != nil {
				return fmt.Errorf("sync %s: %w", d, err)
			}
		}
		if _, err := fsq.WriteFileAtomic(filepath.Join(dir, "records"), processKey(r)+".json", append(raw, '\n'), 0o600); err != nil {
			return fmt.Errorf("record the exec identity: %w", err)
		}
		if _, err := fsq.WriteFileAtomic(filepath.Join(dir, "current"), currentKey(r)+".json", append(current, '\n'), 0o600); err != nil {
			return fmt.Errorf("record the current exec identity: %w", err)
		}
		return nil
	})
}

// List returns every valid record in the store. It never creates the store;
// a missing store lists nothing, and a damaged record is skipped.
func List() ([]Entry, error) {
	dir, err := storeDir(false)
	if err != nil || dir == "" {
		return nil, err
	}
	names, err := os.ReadDir(filepath.Join(dir, "records"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var entries []Entry
	for _, name := range names {
		key, ok := strings.CutSuffix(name.Name(), ".json")
		if !ok {
			continue
		}
		var r Record
		info, err := readStrict(filepath.Join(dir, "records", name.Name()), &r)
		if err != nil || r.check() != nil || processKey(r) != key {
			continue
		}
		entry := Entry{Record: r, RecordedAt: info.ModTime()}
		var cur currentRecord
		if _, err := readStrict(filepath.Join(dir, "current", currentKey(r)+".json"), &cur); err == nil {
			entry.Current = cur.Process == key
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

type currentRecord struct {
	Schema  int    `json:"schema"`
	Process string `json:"process"`
}

func (r Record) check() error {
	switch {
	case r.Schema != schema:
		return fmt.Errorf("unknown schema %d", r.Schema)
	case strings.TrimSpace(r.Harness) == "":
		return errors.New("the harness is required")
	case r.Holder.PID <= 0 || r.Holder.ProcessStart == "" || r.StartUnix <= 0:
		return errors.New("the process identity is required")
	case !filepath.IsAbs(r.Root) || !filepath.IsAbs(r.BaseRoot):
		return errors.New("both roots must be absolute")
	case strings.TrimSpace(r.Me) == "" || strings.TrimSpace(r.RootID) == "" || strings.TrimSpace(r.BaseRootID) == "":
		return errors.New("the handle and both root identity tokens are required")
	}
	return nil
}

// processKey names one process by its pid, start, and boot.
func processKey(r Record) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s\x00%s", r.Holder.PID, r.Holder.ProcessStart, r.Holder.BootID)))
	return hex.EncodeToString(sum[:16])
}

// currentKey names the current-process record of one handle in one root, by
// the root's identity token so a recreated root starts a new lineage.
func currentKey(r Record) string {
	sum := sha256.Sum256([]byte(r.RootID + "\x00" + r.Me))
	return hex.EncodeToString(sum[:16])
}

// storeDir is $HOME/.amq/exec-records with its records/ and current/
// directories, each a plain directory. With create, missing directories are
// made with mode 0700; without, a missing store returns "".
func storeDir(create bool) (string, error) {
	amqDir, err := amqhome.Dir()
	if create {
		amqDir, err = amqhome.EnsureDir()
	}
	if err != nil {
		return "", err
	}
	store := filepath.Join(amqDir, "exec-records")
	for _, cur := range []string{filepath.Dir(store), store, filepath.Join(store, "records"), filepath.Join(store, "current")} {
		fi, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			if !create {
				if cur == filepath.Dir(store) || cur == store {
					return "", nil
				}
				return store, nil
			}
			if err := os.Mkdir(cur, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return "", err
			}
			fi, err = os.Lstat(cur)
		}
		if err != nil {
			return "", err
		}
		if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%s is not a plain directory; refusing", cur)
		}
	}
	return store, nil
}

// readStrict decodes a small regular 0600 file at path into v, opened
// without following a symlink.
func readStrict(path string, v any) (os.FileInfo, error) {
	f, fi, err := fsq.OpenRegularNoFollowBounded(path, maxBytes)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if fi.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("%s is not a small regular 0600 file; refusing", path)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return fi, nil
}
