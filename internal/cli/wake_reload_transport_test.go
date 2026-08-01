//go:build darwin || linux

package cli

import (
	"encoding/json"
	"runtime"
	"strings"
	"testing"
)

func validWakeReloadTransportRequestForTest() wakeReloadTransportRequest {
	method := wakeImageMethodFDExec
	if runtime.GOOS == "darwin" {
		method = wakeImageMethodPathnameExecVerified
	}
	return wakeReloadTransportRequest{
		Schema:     wakeReloadTransportSchemaV1,
		Operation:  wakeReloadTransportOperation,
		Root:       canonicalWakeRoot("/queue"),
		Agent:      "codex",
		Generation: "0123456789abcdef0123456789abcdef",
		Owner: wakeOwner{
			PID:          4242,
			ProcessStart: "12345",
			BootID:       "11111111-1111-1111-1111-111111111111",
			SessionID:    99,
		},
		Candidate: wakeImageEvidenceV1{
			Schema:          wakeImageEvidenceSchemaV1,
			Platform:        runtime.GOOS,
			Method:          method,
			ExecutionPath:   "/usr/local/bin/amq",
			Device:          1,
			Inode:           2,
			Size:            3,
			CTimeNS:         4,
			SHA256:          "sha256:" + strings.Repeat("0", 64),
			EmbeddedVersion: "0.0.0-test",
		},
	}
}

func encodeWakeReloadTransportRequestForTest(t *testing.T, request wakeReloadTransportRequest) []byte {
	t.Helper()
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return append(payload, '\n')
}

func TestDecodeWakeReloadTransportRequestIsStrictAndReloadOnly(t *testing.T) {
	valid := validWakeReloadTransportRequestForTest()
	if got, err := decodeWakeReloadTransportRequest(
		encodeWakeReloadTransportRequestForTest(t, valid),
	); err != nil || got != valid {
		t.Fatalf("valid request = %#v, %v", got, err)
	}
	for _, schema := range []int{0, 2} {
		request := valid
		request.Schema = schema
		if _, err := decodeWakeReloadTransportRequest(
			encodeWakeReloadTransportRequestForTest(t, request),
		); err == nil {
			t.Fatalf("schema %d was accepted", schema)
		}
	}

	for _, operation := range []string{"", "stop", "recover", "Reload", "reload ", "arbitrary-unknown-operation"} {
		t.Run("operation_"+operation, func(t *testing.T) {
			request := valid
			request.Operation = operation
			if _, err := decodeWakeReloadTransportRequest(
				encodeWakeReloadTransportRequestForTest(t, request),
			); err == nil {
				t.Fatalf("operation %q was accepted", operation)
			}
		})
	}

	unknown := []byte(`{"schema":1,"operation":"reload","root":"/queue","agent":"codex","generation":"0123456789abcdef0123456789abcdef","owner":{"pid":4242},"candidate":{},"unknown":true}` + "\n")
	if _, err := decodeWakeReloadTransportRequest(unknown); err == nil {
		t.Fatal("unknown request field was accepted")
	}
	duplicateSchema := encodeWakeReloadTransportRequestForTest(t, valid)
	duplicateSchema = []byte(strings.Replace(
		string(duplicateSchema),
		`"schema":1,`,
		`"schema":1,"schema":1,`,
		1,
	))
	if _, err := decodeWakeReloadTransportRequest(duplicateSchema); err == nil {
		t.Fatal("duplicate request field was accepted")
	}
	if _, err := decodeWakeReloadTransportRequest(
		append(encodeWakeReloadTransportRequestForTest(t, valid), []byte("{}\n")...),
	); err == nil {
		t.Fatal("trailing request object was accepted")
	}
	withoutTerminator := encodeWakeReloadTransportRequestForTest(t, valid)
	withoutTerminator = withoutTerminator[:len(withoutTerminator)-1]
	if _, err := decodeWakeReloadTransportRequest(withoutTerminator); err == nil {
		t.Fatal("unterminated request was accepted")
	}
	oversized := make([]byte, wakeReloadTransportMaxRequestBytes+1)
	if _, err := decodeWakeReloadTransportRequest(oversized); err == nil {
		t.Fatal("oversized request was accepted")
	}
}

func TestWakeReloadTransportResponseIsOnlyUnavailable(t *testing.T) {
	response := wakeReloadTransportUnavailableResponse()
	if response.Status != wakeReloadUnavailable ||
		response.ReasonCode != wakeReloadReasonCommandUnavailable {
		t.Fatalf("response = %#v", response)
	}
	payload, err := encodeWakeReloadTransportResponse(response)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(payload), "{\"status\":\"unavailable\",\"reason_code\":\"reload_command_unavailable\"}\n"; got != want {
		t.Fatalf("response wire = %q, want %q", got, want)
	}
}
