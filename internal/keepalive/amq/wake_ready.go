package amq

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

const wakeReadySchema = 1
const maxWakeReadyFileBytes = 64 * 1024

type wakeReadyMarker struct {
	Schema       int    `json:"schema"`
	Generation   string `json:"generation"`
	TargetDigest string `json:"target_digest,omitempty"`
}

func wakeReadyFileExists(path string) bool {
	_, err := readWakeReadyFile(path)
	return err == nil
}

func decodeWakeReady(data []byte) (wakeReadyMarker, error) {
	var ready wakeReadyMarker
	if err := json.Unmarshal(data, &ready); err != nil {
		return wakeReadyMarker{}, fmt.Errorf("legacy wake ready file refused")
	}
	if ready.Schema != wakeReadySchema || ready.Generation == "" {
		return wakeReadyMarker{}, fmt.Errorf("legacy wake ready file refused")
	}
	return ready, nil
}

func readWakeReadyBytes(file *os.File, path string) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(file, maxWakeReadyFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read wake ready file: %w", err)
	}
	if len(data) > maxWakeReadyFileBytes {
		return nil, fmt.Errorf("wake ready file %s is too large", path)
	}
	return data, nil
}
