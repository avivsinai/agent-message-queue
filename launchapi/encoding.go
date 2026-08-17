package launchapi

import (
	"encoding/json"
	"fmt"
	"io"
)

// MarshalResultV1 is the canonical JSON encoder for all V1 result DTOs.
func MarshalResultV1(result any) ([]byte, error) {
	switch result.(type) {
	case PrepareResultV1, ApplyResultV1, LifecycleResultV1:
	default:
		return nil, fmt.Errorf("unsupported launch result type %T", result)
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// EncodeResultV1 writes the exact bytes returned by MarshalResultV1.
func EncodeResultV1(writer io.Writer, result any) error {
	data, err := MarshalResultV1(result)
	if err != nil {
		return err
	}
	_, err = writer.Write(data)
	return err
}
