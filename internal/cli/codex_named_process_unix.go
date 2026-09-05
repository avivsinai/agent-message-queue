//go:build darwin || linux

package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	codexNamedProbeTimeout  = 10 * time.Second
	codexNamedRPCTimeout    = 10 * time.Second
	codexNamedStopGrace     = 750 * time.Millisecond
	codexNamedMaxMetadata   = 256 << 10
	codexNamedMaxToolOutput = 4 << 20
	codexNamedMaxRPCMessage = 1 << 20
)

var (
	errCodexThreadNotReady    = errors.New("codex thread is not yet persisted")
	errCodexNamingTargetEnded = errors.New("spawned Codex process ended")
)

var (
	codexNamedDiscoveryPoll         = time.Second
	inspectCodexNamingProcess       = inspectWakeProcessPlatform
	locateCodexProcessThreadForWait = locateCodexProcessThread
	startCodexNamingSidecar         = startCodexSidecar
)

type codexNamingTarget struct {
	PID               int
	ProcessStart      string
	BootID            string
	InitialExecutable string
	InitialIdentity   codexFileIdentity
	ProviderBinary    string
	ProviderIdentity  codexFileIdentity
}

type codexFileIdentity struct {
	Device uint64
	Inode  uint64
}

type codexOpenRollout struct {
	HolderPID int
	Path      string
	Identity  codexFileIdentity
}

type codexProcessThread struct {
	ThreadID    string
	RolloutPath string
	Identity    codexFileIdentity
}

type codexSessionMeta struct {
	Type    string `json:"type"`
	Payload struct {
		ID           string          `json:"id"`
		Source       json.RawMessage `json:"source"`
		ThreadSource string          `json:"thread_source"`
	} `json:"payload"`
}

func captureCodexNamingTarget(providerBinary string) (codexNamingTarget, error) {
	providerBinary, err := filepath.Abs(providerBinary)
	if err != nil {
		return codexNamingTarget{}, fmt.Errorf("resolve Codex executable: %w", err)
	}
	providerBinary = filepath.Clean(providerBinary)
	providerIdentity, err := fileIdentity(providerBinary)
	if err != nil {
		return codexNamingTarget{}, fmt.Errorf("inspect Codex executable %q: %w", providerBinary, err)
	}
	info := inspectCodexNamingProcess(os.Getpid())
	if !info.Running || info.InspectError != nil || info.StartToken == "" || info.BootID == "" {
		return codexNamingTarget{}, fmt.Errorf("capture coop exec process identity: %s", processInspectionReason(info))
	}
	initialExecutable := info.ExecutablePath
	if initialExecutable == "" {
		initialExecutable = info.Executable
	}
	if initialExecutable == "" {
		return codexNamingTarget{}, errors.New("capture coop exec executable: path is unavailable")
	}
	initialExecutable, err = filepath.Abs(initialExecutable)
	if err != nil {
		return codexNamingTarget{}, fmt.Errorf("resolve coop exec executable: %w", err)
	}
	initialIdentity, err := fileIdentity(initialExecutable)
	if err != nil {
		return codexNamingTarget{}, fmt.Errorf("inspect coop exec executable %q: %w", initialExecutable, err)
	}
	return codexNamingTarget{
		PID:               os.Getpid(),
		ProcessStart:      info.StartToken,
		BootID:            info.BootID,
		InitialExecutable: filepath.Clean(initialExecutable),
		InitialIdentity:   initialIdentity,
		ProviderBinary:    providerBinary,
		ProviderIdentity:  providerIdentity,
	}, nil
}

func processInspectionReason(info wakeProcessInfo) string {
	if !info.Running {
		return "process is not running"
	}
	if info.InspectError != nil {
		return info.InspectError.Error()
	}
	if info.StartToken == "" {
		return "process start is unavailable"
	}
	if info.BootID == "" {
		return "boot identity is unavailable"
	}
	return "process identity is incomplete"
}

func validateCodexNamingTarget(target codexNamingTarget) (bool, error) {
	info := inspectCodexNamingProcess(target.PID)
	if !info.Running && info.InspectError == nil {
		return false, errCodexNamingTargetEnded
	}
	if !info.Running || info.InspectError != nil || info.StartToken == "" || info.BootID == "" {
		return false, fmt.Errorf("spawned Codex process is unavailable: %s", processInspectionReason(info))
	}
	if info.StartToken != target.ProcessStart || info.BootID != target.BootID {
		return false, fmt.Errorf("%w: process identity changed", errCodexNamingTargetEnded)
	}
	executable := info.ExecutablePath
	if executable == "" {
		executable = info.Executable
	}
	actualID, err := fileIdentity(executable)
	if err != nil {
		return false, fmt.Errorf("inspect spawned Codex executable: %w", err)
	}
	if actualID == target.InitialIdentity {
		return false, nil
	}
	providerID, err := fileIdentity(target.ProviderBinary)
	if err != nil {
		return false, fmt.Errorf("reinspect Codex executable: %w", err)
	}
	if providerID != target.ProviderIdentity {
		return false, errors.New("pinned Codex executable identity changed")
	}
	if actualID == target.ProviderIdentity {
		return true, nil
	}
	base := strings.ToLower(filepath.Base(executable))
	if base != "node" && base != "nodejs" {
		return false, fmt.Errorf("spawned process executable %q is not the pinned Codex binary", executable)
	}
	args, err := readCodexProcessArgs(target.PID)
	if err != nil {
		return false, fmt.Errorf("inspect Codex Node launcher arguments: %w", err)
	}
	for _, arg := range args[1:] {
		if !filepath.IsAbs(arg) {
			continue
		}
		argID, statErr := fileIdentity(arg)
		if statErr == nil && argID == target.ProviderIdentity {
			return true, nil
		}
	}
	return false, errors.New("codex Node launcher does not contain the pinned executable path")
}

func locateCodexProcessThread(ctx context.Context, target codexNamingTarget) (codexProcessThread, error) {
	ready, err := validateCodexNamingTarget(target)
	if err != nil {
		return codexProcessThread{}, err
	}
	if !ready {
		return codexProcessThread{}, errCodexThreadNotReady
	}
	parents, err := codexProcessParents(ctx)
	if err != nil {
		return codexProcessThread{}, err
	}
	pids := codexDescendantPIDs(target.PID, parents)
	seen := make(map[codexFileIdentity]codexProcessThread)
	for _, pid := range pids {
		before := inspectCodexNamingProcess(pid)
		if !before.Running || before.InspectError != nil || before.StartToken == "" || before.BootID == "" {
			continue
		}
		rollouts, err := codexOpenRolloutsForPID(ctx, pid)
		if err != nil {
			return codexProcessThread{}, err
		}
		afterParents, err := codexProcessParents(ctx)
		if err != nil {
			return codexProcessThread{}, err
		}
		after := inspectCodexNamingProcess(pid)
		parentChanged := pid != target.PID && parents[pid] != afterParents[pid]
		if !after.Running || after.InspectError != nil || after.StartToken != before.StartToken || after.BootID != before.BootID || parentChanged || !codexPIDDescendsFrom(pid, target.PID, afterParents) {
			continue
		}
		for _, rollout := range rollouts {
			meta, err := readCodexSessionMeta(rollout.Path, rollout.Identity)
			if err != nil || !isCodexRootUserThread(meta) {
				continue
			}
			candidate := codexProcessThread{ThreadID: meta.Payload.ID, RolloutPath: rollout.Path, Identity: rollout.Identity}
			if prior, ok := seen[rollout.Identity]; ok && prior != candidate {
				return codexProcessThread{}, errors.New("one open Codex rollout produced inconsistent metadata")
			}
			seen[rollout.Identity] = candidate
		}
	}
	if readyAgain, err := validateCodexNamingTarget(target); err != nil || !readyAgain {
		if err != nil {
			return codexProcessThread{}, err
		}
		return codexProcessThread{}, errors.New("spawned Codex process changed during rollout discovery")
	}
	candidates := make([]codexProcessThread, 0, len(seen))
	for _, candidate := range seen {
		candidates = append(candidates, candidate)
	}
	switch len(candidates) {
	case 0:
		return codexProcessThread{}, errCodexThreadNotReady
	case 1:
		return candidates[0], nil
	default:
		return codexProcessThread{}, fmt.Errorf("%d root user Codex rollouts are open in the spawned process tree", len(candidates))
	}
}

func waitForCodexProcessThread(ctx context.Context, target codexNamingTarget) (codexProcessThread, error) {
	for {
		probeCtx, cancelProbe := context.WithTimeout(ctx, codexNamedProbeTimeout)
		thread, err := locateCodexProcessThreadForWait(probeCtx, target)
		cancelProbe()
		if err == nil {
			return thread, nil
		}
		if !errors.Is(err, errCodexThreadNotReady) {
			return codexProcessThread{}, err
		}
		timer := time.NewTimer(codexNamedDiscoveryPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return codexProcessThread{}, fmt.Errorf("wait for spawned Codex rollout: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func revalidateCodexProcessThread(ctx context.Context, target codexNamingTarget, expected codexProcessThread) error {
	actual, err := locateCodexProcessThread(ctx, target)
	if err != nil {
		return fmt.Errorf("revalidate spawned Codex thread ownership: %w", err)
	}
	if actual != expected {
		return errors.New("spawned Codex thread ownership changed")
	}
	return nil
}

func isCodexRootUserThread(meta codexSessionMeta) bool {
	if meta.Type != "session_meta" || strings.TrimSpace(meta.Payload.ID) == "" || meta.Payload.ThreadSource != "user" {
		return false
	}
	var source string
	return json.Unmarshal(meta.Payload.Source, &source) == nil && source == "cli"
}

func readCodexSessionMeta(path string, expected codexFileIdentity) (codexSessionMeta, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return codexSessionMeta{}, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return codexSessionMeta{}, err
	}
	if !info.Mode().IsRegular() {
		return codexSessionMeta{}, errors.New("codex rollout is not a regular file")
	}
	actual, err := fileIdentityFromInfo(info)
	if err != nil || actual != expected {
		return codexSessionMeta{}, errors.New("codex rollout path no longer names the open file")
	}
	limited := io.LimitReader(file, codexNamedMaxMetadata+1)
	line, err := bufio.NewReader(limited).ReadBytes('\n')
	if len(line) > codexNamedMaxMetadata {
		return codexSessionMeta{}, errors.New("codex session metadata exceeds the size limit")
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return codexSessionMeta{}, err
	}
	var meta codexSessionMeta
	if err := json.Unmarshal(bytes.TrimSpace(line), &meta); err != nil || meta.Type != "session_meta" {
		return codexSessionMeta{}, errors.New("invalid Codex session metadata")
	}
	return meta, nil
}

func codexProcessParents(ctx context.Context) (map[int]int, error) {
	cmd := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=")
	var output boundedOutput
	output.limit = codexNamedMaxToolOutput
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("inspect Codex process ancestry: %w", ctx.Err())
		}
		return nil, fmt.Errorf("inspect Codex process ancestry: %w", err)
	}
	parents := make(map[int]int)
	for _, line := range strings.Split(output.String(), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		ppid, ppidErr := strconv.Atoi(fields[1])
		if pidErr == nil && ppidErr == nil && pid > 0 && ppid >= 0 {
			parents[pid] = ppid
		}
	}
	return parents, nil
}

func codexDescendantPIDs(rootPID int, parents map[int]int) []int {
	children := make(map[int][]int)
	for pid, ppid := range parents {
		children[ppid] = append(children[ppid], pid)
	}
	result := []int{rootPID}
	seen := map[int]bool{rootPID: true}
	for index := 0; index < len(result); index++ {
		for _, child := range children[result[index]] {
			if !seen[child] {
				seen[child] = true
				result = append(result, child)
			}
		}
	}
	return result
}

func codexPIDDescendsFrom(pid, rootPID int, parents map[int]int) bool {
	if pid == rootPID {
		return true
	}
	seen := make(map[int]bool)
	for pid > 0 && !seen[pid] {
		seen[pid] = true
		pid = parents[pid]
		if pid == rootPID {
			return true
		}
	}
	return false
}

func codexOpenRolloutsForPID(ctx context.Context, pid int) ([]codexOpenRollout, error) {
	if runtime.GOOS == "linux" {
		return codexLinuxOpenRollouts(pid)
	}
	return codexDarwinOpenRollouts(ctx, pid)
}

func codexLinuxOpenRollouts(pid int) ([]codexOpenRollout, error) {
	fdRoot := filepath.Join("/proc", strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(fdRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("scan Codex process descriptors: %w", err)
	}
	if len(entries) > 4096 {
		return nil, errors.New("codex process descriptor count exceeds the safety limit")
	}
	var rollouts []codexOpenRollout
	for _, entry := range entries {
		fdPath := filepath.Join(fdRoot, entry.Name())
		path, err := os.Readlink(fdPath)
		if err != nil || !isCodexRolloutPath(path) {
			continue
		}
		fdInfo, err := os.Stat(fdPath)
		if err != nil {
			continue
		}
		fdID, err := fileIdentityFromInfo(fdInfo)
		if err != nil {
			continue
		}
		pathID, err := fileIdentity(path)
		if err != nil || pathID != fdID {
			continue
		}
		rollouts = append(rollouts, codexOpenRollout{HolderPID: pid, Path: path, Identity: fdID})
	}
	return rollouts, nil
}

func codexDarwinOpenRollouts(ctx context.Context, pid int) ([]codexOpenRollout, error) {
	cmd := exec.CommandContext(ctx, "lsof", "-a", "-p", strconv.Itoa(pid), "-FnfDi")
	var output boundedOutput
	output.limit = codexNamedMaxToolOutput
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return nil, nil
		}
		return nil, fmt.Errorf("inspect Codex process descriptors with lsof: %w", err)
	}
	var rollouts []codexOpenRollout
	var fd, path string
	var device, inode uint64
	flush := func() {
		if fd == "" || path == "" || device == 0 || inode == 0 || !isCodexRolloutPath(path) {
			return
		}
		if _, err := strconv.Atoi(strings.TrimRight(fd, "rwu")); err != nil {
			return
		}
		identity, err := fileIdentity(path)
		if err == nil && identity == (codexFileIdentity{Device: device, Inode: inode}) {
			rollouts = append(rollouts, codexOpenRollout{HolderPID: pid, Path: path, Identity: identity})
		}
	}
	for _, line := range strings.Split(output.String(), "\n") {
		if strings.HasPrefix(line, "f") {
			flush()
			fd, path, device, inode = strings.TrimPrefix(line, "f"), "", 0, 0
			continue
		}
		if len(line) < 2 {
			continue
		}
		switch line[0] {
		case 'D':
			device, _ = strconv.ParseUint(line[1:], 0, 64)
		case 'i':
			inode, _ = strconv.ParseUint(line[1:], 10, 64)
		case 'n':
			path = line[1:]
		}
	}
	flush()
	return rollouts, nil
}

func isCodexRolloutPath(path string) bool {
	return filepath.IsAbs(path) && strings.HasPrefix(filepath.Base(path), "rollout-") && strings.HasSuffix(path, ".jsonl")
}

func fileIdentity(path string) (codexFileIdentity, error) {
	info, err := os.Stat(path)
	if err != nil {
		return codexFileIdentity{}, err
	}
	return fileIdentityFromInfo(info)
}

func fileIdentityFromInfo(info os.FileInfo) (codexFileIdentity, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil || stat.Ino == 0 {
		return codexFileIdentity{}, errors.New("file identity is unavailable")
	}
	return codexFileIdentity{Device: uint64(stat.Dev), Inode: uint64(stat.Ino)}, nil
}

type boundedOutput struct {
	bytes.Buffer
	limit int
}

func (output *boundedOutput) Write(data []byte) (int, error) {
	if output.Len()+len(data) > output.limit {
		remaining := output.limit - output.Len()
		if remaining > 0 {
			_, _ = output.Buffer.Write(data[:remaining])
		}
		return remaining, errors.New("command output exceeds the safety limit")
	}
	return output.Buffer.Write(data)
}

type codexSidecarMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type codexSidecar struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	responses <-chan codexSidecarMessage
	readErr   <-chan error
	wait      <-chan error
	done      chan struct{}
	doneOnce  sync.Once
	closeOnce sync.Once
}

func startCodexSidecar(binary string) (*codexSidecar, error) {
	cmd := exec.Command(binary, "app-server", "--listen", "stdio://")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stderr = io.Discard
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open Codex sidecar input: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open Codex sidecar output: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start Codex naming sidecar: %w", err)
	}
	responses := make(chan codexSidecarMessage)
	readErr := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(responses)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 64<<10), codexNamedMaxRPCMessage)
		for scanner.Scan() {
			var message codexSidecarMessage
			if json.Unmarshal(scanner.Bytes(), &message) == nil {
				select {
				case responses <- message:
				case <-done:
					return
				}
			}
		}
		readErr <- scanner.Err()
	}()
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	return &codexSidecar{cmd: cmd, stdin: stdin, responses: responses, readErr: readErr, wait: wait, done: done}, nil
}

func (sidecar *codexSidecar) close() {
	sidecar.doneOnce.Do(func() { close(sidecar.done) })
	sidecar.closeOnce.Do(func() {
		_ = sidecar.stdin.Close()
		select {
		case <-sidecar.wait:
			return
		case <-time.After(codexNamedStopGrace):
		}
		_ = syscall.Kill(-sidecar.cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-sidecar.wait:
			return
		case <-time.After(codexNamedStopGrace):
		}
		_ = syscall.Kill(-sidecar.cmd.Process.Pid, syscall.SIGKILL)
		select {
		case <-sidecar.wait:
		case <-time.After(codexNamedStopGrace):
		}
	})
}

func (sidecar *codexSidecar) send(ctx context.Context, request codexSidecarMessage) error {
	stopClose := context.AfterFunc(ctx, sidecar.close)
	defer stopClose()
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	data, err := json.Marshal(request)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(sidecar.stdin, "%s\n", data); err != nil {
		return fmt.Errorf("send Codex naming request: %w", err)
	}
	return nil
}

func (sidecar *codexSidecar) call(ctx context.Context, request codexSidecarMessage) (json.RawMessage, error) {
	stopClose := context.AfterFunc(ctx, sidecar.close)
	defer stopClose()
	if err := sidecar.send(ctx, request); err != nil {
		return nil, err
	}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case err := <-sidecar.readErr:
			if err != nil {
				return nil, fmt.Errorf("read Codex naming response: %w", err)
			}
			return nil, errors.New("codex naming sidecar closed its output")
		case response, ok := <-sidecar.responses:
			if !ok {
				return nil, errors.New("codex naming sidecar closed its output")
			}
			if response.ID != request.ID {
				continue
			}
			if response.Error != nil {
				return nil, errors.New(response.Error.Message)
			}
			return response.Result, nil
		}
	}
}

func runCodexNamedSidecar(name string, target codexNamingTarget) error {
	// Codex defers persistence until the first user turn. An idle composer has
	// no naming target yet, regardless of how long it has been open. Each probe
	// is bounded, and process identity checks end the wait when this CLI exits.
	thread, err := waitForCodexProcessThread(context.Background(), target)
	if errors.Is(err, errCodexNamingTargetEnded) {
		return nil
	}
	if err != nil {
		return err
	}
	rpcCtx, cancelRPC := context.WithTimeout(context.Background(), codexNamedRPCTimeout)
	defer cancelRPC()
	sidecar, err := startCodexNamingSidecar(target.ProviderBinary)
	if err != nil {
		return err
	}
	defer sidecar.close()
	initialize := codexSidecarMessage{JSONRPC: "2.0", ID: 1, Method: "initialize", Params: json.RawMessage(`{"clientInfo":{"name":"amq","title":"AMQ","version":"1"},"capabilities":{}}`)}
	if _, err := sidecar.call(rpcCtx, initialize); err != nil {
		return fmt.Errorf("initialize Codex naming sidecar: %w", err)
	}
	if err := sidecar.send(rpcCtx, codexSidecarMessage{JSONRPC: "2.0", Method: "initialized", Params: json.RawMessage(`{}`)}); err != nil {
		return err
	}
	return setCodexThreadNameIfEmpty(rpcCtx, sidecar, thread, name, func() error {
		return revalidateCodexProcessThread(rpcCtx, target, thread)
	})
}

func setCodexThreadNameIfEmpty(ctx context.Context, sidecar *codexSidecar, thread codexProcessThread, name string, revalidate func() error) error {
	if err := revalidate(); err != nil {
		return err
	}
	currentName, err := readCodexThreadName(ctx, sidecar, thread, 2)
	if err != nil {
		return err
	}
	if strings.TrimSpace(currentName) != "" {
		return nil
	}
	if err := revalidate(); err != nil {
		return err
	}
	// Ownership discovery can be slow. Preserve a name set during that probe
	// by reading again immediately before mutation. Codex has no conditional
	// set-name operation, so the final read/set pair is still best effort.
	currentName, err = readCodexThreadName(ctx, sidecar, thread, 3)
	if err != nil {
		return err
	}
	if strings.TrimSpace(currentName) != "" {
		return nil
	}
	params, _ := json.Marshal(map[string]string{"threadId": thread.ThreadID, "name": name})
	if _, err := sidecar.call(ctx, codexSidecarMessage{JSONRPC: "2.0", ID: 4, Method: "thread/name/set", Params: params}); err != nil {
		return fmt.Errorf("set Codex thread name: %w", err)
	}
	if err := revalidate(); err != nil {
		return err
	}
	actual, err := readCodexThreadName(ctx, sidecar, thread, 5)
	if err != nil {
		return err
	}
	if strings.TrimSpace(actual) != name {
		return errors.New("codex naming sidecar did not confirm the requested name")
	}
	return nil
}

func readCodexThreadName(ctx context.Context, sidecar *codexSidecar, thread codexProcessThread, requestID int) (string, error) {
	params, _ := json.Marshal(map[string]any{"threadId": thread.ThreadID, "includeTurns": false})
	result, err := sidecar.call(ctx, codexSidecarMessage{JSONRPC: "2.0", ID: requestID, Method: "thread/read", Params: params})
	if err != nil {
		return "", fmt.Errorf("read Codex thread: %w", err)
	}
	var response struct {
		Thread struct {
			ID   string  `json:"id"`
			Name *string `json:"name"`
			Path *string `json:"path"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(result, &response); err != nil {
		return "", fmt.Errorf("decode Codex thread: %w", err)
	}
	if response.Thread.ID != thread.ThreadID {
		return "", errors.New("codex sidecar returned a different thread")
	}
	if response.Thread.Path == nil || *response.Thread.Path != thread.RolloutPath {
		return "", errors.New("codex sidecar returned a different rollout path")
	}
	identity, err := fileIdentity(*response.Thread.Path)
	if err != nil || identity != thread.Identity {
		return "", errors.New("codex sidecar rollout path identity changed")
	}
	if response.Thread.Name == nil {
		return "", nil
	}
	return *response.Thread.Name, nil
}
