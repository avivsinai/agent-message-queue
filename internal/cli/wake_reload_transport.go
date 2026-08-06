//go:build darwin || linux

package cli

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const (
	wakeReloadTransportSchemaV1        = 1
	wakeReloadTransportOperation       = "reload"
	wakeReloadTransportMaxRequestBytes = 16 * 1024
)

type wakeReloadTransportRequest struct {
	Schema     int                 `json:"schema"`
	Operation  string              `json:"operation"`
	Root       string              `json:"root"`
	Agent      string              `json:"agent"`
	Generation string              `json:"generation"`
	Owner      wakeOwner           `json:"owner"`
	Candidate  wakeImageEvidenceV1 `json:"candidate"`
}

type wakeReloadTransportResponse struct {
	Status     string `json:"status"`
	ReasonCode string `json:"reason_code"`
}

// wakeReloadTransportUnavailableError marks failures that occur only while
// creating the optional endpoint. Authority and lifecycle failures remain
// ordinary errors and must still stop wake startup.
type wakeReloadTransportUnavailableError struct {
	err error
}

func (err *wakeReloadTransportUnavailableError) Error() string {
	if err == nil || err.err == nil {
		return "wake reload transport unavailable"
	}
	return err.err.Error()
}

func (err *wakeReloadTransportUnavailableError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.err
}

func decodeWakeReloadTransportRequest(payload []byte) (wakeReloadTransportRequest, error) {
	if len(payload) == 0 || len(payload) > wakeReloadTransportMaxRequestBytes {
		return wakeReloadTransportRequest{}, fmt.Errorf("wake reload request size is invalid")
	}
	if payload[len(payload)-1] != '\n' || bytes.ContainsAny(payload[:len(payload)-1], "\r\n") {
		return wakeReloadTransportRequest{}, fmt.Errorf("wake reload request must be one terminated line")
	}

	var request wakeReloadTransportRequest
	if err := json.Unmarshal(payload[:len(payload)-1], &request); err != nil {
		return wakeReloadTransportRequest{}, fmt.Errorf("decode wake reload request: %w", err)
	}
	canonical, err := json.Marshal(request)
	if err != nil {
		return wakeReloadTransportRequest{}, fmt.Errorf("re-encode wake reload request: %w", err)
	}
	if !bytes.Equal(canonical, payload[:len(payload)-1]) {
		return wakeReloadTransportRequest{}, fmt.Errorf("wake reload request is not canonical")
	}
	if err := validateWakeReloadTransportRequest(request); err != nil {
		return wakeReloadTransportRequest{}, err
	}
	return request, nil
}

func validateWakeReloadTransportRequest(request wakeReloadTransportRequest) error {
	if request.Schema != wakeReloadTransportSchemaV1 {
		return fmt.Errorf("wake reload request schema is unsupported")
	}
	if request.Operation != wakeReloadTransportOperation {
		return fmt.Errorf("wake reload operation is unsupported")
	}
	if request.Root == "" || request.Root != strings.TrimSpace(request.Root) ||
		strings.ContainsRune(request.Root, 0) || !filepath.IsAbs(request.Root) ||
		filepath.Clean(request.Root) != request.Root || canonicalWakeRoot(request.Root) != request.Root {
		return fmt.Errorf("wake reload root is not canonical")
	}
	if err := fsq.ValidateHandle(request.Agent); err != nil {
		return fmt.Errorf("wake reload agent is invalid: %w", err)
	}
	if !validWakeReloadTransportGeneration(request.Generation) {
		return fmt.Errorf("wake reload generation is invalid")
	}
	if err := validateAuthoritativeWakeOwner(request.Owner); err != nil {
		return fmt.Errorf("wake reload owner is invalid: %w", err)
	}
	if err := validateWakeImageEvidence(request.Candidate); err != nil {
		return fmt.Errorf("wake reload candidate is invalid: %w", err)
	}
	return nil
}

func validWakeReloadTransportGeneration(generation string) bool {
	if len(generation) != 32 || generation != strings.ToLower(generation) {
		return false
	}
	decoded, err := hex.DecodeString(generation)
	return err == nil && len(decoded) == 16
}

func wakeReloadTransportUnavailableResponse() wakeReloadTransportResponse {
	return wakeReloadTransportResponse{
		Status:     wakeReloadUnavailable,
		ReasonCode: wakeReloadReasonCommandUnavailable,
	}
}

func encodeWakeReloadTransportResponse(response wakeReloadTransportResponse) ([]byte, error) {
	if response != wakeReloadTransportUnavailableResponse() {
		return nil, fmt.Errorf("wake reload transport response is not a closed refusal")
	}
	payload, err := json.Marshal(response)
	if err != nil {
		return nil, err
	}
	return append(payload, '\n'), nil
}
