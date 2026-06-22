package cli

import (
	"fmt"
	"io"
	"os"
)

const maxWakeMetadataFileBytes = 64 * 1024

func readWakeMetadata(file *os.File, label, path string) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(file, maxWakeMetadataFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", label, err)
	}
	if len(data) > maxWakeMetadataFileBytes {
		return nil, fmt.Errorf("%s %s is too large", label, path)
	}
	return data, nil
}
