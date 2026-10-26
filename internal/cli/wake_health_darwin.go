//go:build darwin

package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func probeWakeHealthPlatform(root, me string, inspection wakeLockInspection) wakeHealthDecision {
	unknown := func(detail string) wakeHealthDecision {
		return wakeHealthDecision{Status: wakeHealthUnknown, Detail: detail}
	}
	unresponsive := func(detail string) wakeHealthDecision {
		return wakeHealthDecision{Status: wakeHealthUnresponsive, Detail: detail}
	}
	if !inspection.Exists || inspection.Status != wakeLockValid ||
		!inspection.IdentityConfirmed || !inspection.Process.Running {
		return unknown("wake process is not live and identity-confirmed")
	}
	if inspection.Lock.HealthSchema != wakeHealthSchema {
		return unknown("wake does not advertise the health protocol")
	}
	expectedPath := wakeControlSocketPath(root, me, inspection.Lock.Generation)
	if inspection.Lock.ControlSocket != expectedPath {
		return unresponsive("wake health endpoint does not match the generation")
	}
	agentDir, err := openWakeDirectory(
		fsq.AgentBase(root, me),
		"wake health agent directory",
	)
	if err != nil {
		return unresponsive("wake health endpoint is unavailable")
	}
	defer func() { _ = agentDir.Close() }()
	name, err := darwinControlSocketName(agentDir, expectedPath)
	if err != nil {
		return unresponsive("wake health endpoint is unavailable")
	}
	conn, err := dialDarwinUnixAt(agentDir, name, wakeControlHealthTimeout)
	if err != nil {
		return unresponsive("wake health endpoint is unresponsive")
	}
	defer func() { _ = conn.Close() }()
	request := wakeControlHealthRequest{
		Generation: inspection.Lock.Generation,
		Operation:  wakeControlHealthOperation,
	}
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return unresponsive("wake health endpoint is unresponsive")
	}
	line, err := bufio.NewReader(io.LimitReader(conn, 4097)).ReadString('\n')
	if err != nil || len(line) > 4096 {
		return unresponsive("wake health endpoint is unresponsive")
	}
	var response wakeControlHealthResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &response); err != nil ||
		response.Generation != inspection.Lock.Generation ||
		(response.Status != "healthy" && response.Status != "degraded") {
		return unresponsive("wake health endpoint returned an invalid response")
	}
	detail := fmt.Sprintf("wake loop reports %s", response.Status)
	return wakeHealthDecision{Status: response.Status, Detail: detail}
}
