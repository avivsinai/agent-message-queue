package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/amqhome"
	"github.com/avivsinai/agent-message-queue/internal/board"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/lock"
)

type boardContext struct {
	Root       string `json:"root"`
	RootID     string `json:"root_id"`
	BaseRoot   string `json:"base_root"`
	BaseRootID string `json:"base_root_id"`
	Session    string `json:"session_name"`
	Me         string `json:"me"`
}

func readBoardContext(ctx context.Context, env board.Env) (boardContext, error) {
	raw, err := env.Run.Run(ctx, env.Dir, []string{"amq", "env", "--json"})
	if err != nil {
		return boardContext{}, err
	}
	var result boardContext
	if err = json.Unmarshal(raw, &result); err != nil {
		return result, err
	}
	if result.Root == "" || result.RootID == "" || result.BaseRoot == "" || result.BaseRootID == "" {
		return result, errors.New("AMQ full authenticated context unavailable")
	}
	return result, nil
}

func resolveStore(ctx context.Context, env board.Env) (board.Store, error) {
	store, _, err := resolveBoardContext(ctx, env)
	return store, err
}
func resolveBoardContext(ctx context.Context, env board.Env) (board.Store, board.Env, error) {
	initial, err := readBoardContext(ctx, env)
	if err != nil {
		return board.Store{}, env, err
	}
	host, err := boardHostID(ctx)
	if err != nil {
		return board.Store{}, env, err
	}
	runner, ok := env.Run.(execRunner)
	if !ok {
		return board.Store{}, env, errors.New("board context requires the production command runner")
	}
	pin := map[string]string{"AM_ROOT": initial.Root, "AM_ROOT_ID": initial.RootID, "AM_BASE_ROOT": initial.BaseRoot, "AM_BASE_ROOT_ID": initial.BaseRootID, "AM_SESSION": initial.Session, "AM_ME": initial.Me}
	runner.amqEnv = []string{}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, replace := pin[key]; !replace {
			runner.amqEnv = append(runner.amqEnv, entry)
		}
	}
	for key, value := range pin {
		runner.amqEnv = append(runner.amqEnv, key+"="+value)
	}
	pinned := env
	pinned.Run = runner
	validate := func(ctx context.Context) error {
		// The original resolver detects a changed project route. The pinned resolver
		// separately checks the identities on disk rather than trusting stored IDs.
		for _, candidate := range []board.Env{env, pinned} {
			actual, err := readBoardContext(ctx, candidate)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(actual, initial) {
				return errors.New("AMQ context or root identity changed; restart required")
			}
		}
		return nil
	}
	store, err := board.NewStore(initial.BaseRoot, initial.BaseRootID, host)
	runner.amqValidate = validate
	pinned.Run = runner
	return store, pinned, err
}

// boardHostID belongs to the AMQ home, independent of any project store.
func boardHostID(ctx context.Context) (string, error) {
	home, err := amqhome.EnsureDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(home, "board-host-id")
	var identity string
	err = lock.WithExclusiveFileLockUntil(path+".lock", func() bool { return ctx.Err() != nil }, func() error {
		info, err := os.Lstat(path)
		if err == nil {
			if !info.Mode().IsRegular() {
				return errors.New("unsafe board host identity")
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			identity = strings.TrimSpace(string(raw))
			decoded, decodeErr := hex.DecodeString(identity)
			if len(identity) != 32 || decodeErr != nil || len(decoded) != 16 {
				return errors.New("board host identity corrupt; recovery required")
			}
			return nil
		}
		if !os.IsNotExist(err) {
			return err
		}
		identity, err = randomToken()
		if err != nil {
			return err
		}
		_, err = fsq.WriteFileAtomic(home, "board-host-id", []byte(identity+"\n"), 0o600)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("board host identity: %w", err)
	}
	return identity, nil
}
