//go:build !unix

package adapter

import (
	"context"
	"fmt"
	"runtime"
)

type platformProcessSpawner struct{}

func (platformProcessSpawner) Start(context.Context, processSpec) (startedProcess, error) {
	return nil, fmt.Errorf("claude-print spawn is unsupported on %s", runtime.GOOS)
}

func pidAlive(pid int) (bool, error) {
	return false, fmt.Errorf("claude-print live-owner inspect is unsupported on %s", runtime.GOOS)
}

func tryFlockExclusive(path string) (func(), error) {
	return nil, fmt.Errorf("claude-print inject lock is unsupported on %s", runtime.GOOS)
}
