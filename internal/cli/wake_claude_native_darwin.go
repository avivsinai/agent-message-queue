//go:build darwin

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/claudepeer"
	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/receipt"
	"golang.org/x/sys/unix"
)

const claudeNativeFrameLimit = 4096
const claudeNativeCohortLimit = 512

type claudeNativeAttempt struct {
	Schema     int                   `json:"schema"`
	Root       string                `json:"root"`
	Agent      string                `json:"agent"`
	Generation string                `json:"generation"`
	Wake       launch.HolderIdentity `json:"wake"`
	Binding    claudeNativeLaunch    `json:"binding"`
	Socket     string                `json:"socket,omitempty"`
	Phase      string                `json:"phase"`
	MessageID  string                `json:"message_id,omitempty"`
	CreatedAt  time.Time             `json:"created_at,omitempty"`
	Cohort     []string              `json:"cohort,omitempty"`
	Disabled   string                `json:"disabled,omitempty"`
}

type claudeNativeNotifier struct {
	root      string
	agent     string
	dir       *wakeAgentDir
	claim     wakeLockInspection
	owner     wakeOwner
	binding   claudeNativeLaunch
	state     claudeNativeAttempt
	stop      <-chan struct{}
	closed    chan struct{}
	closeOnce sync.Once
}

func newWakeClaudeNative(s *wakeStart, cfg *wakeConfig) (wakeNativeNotifier, error) {
	if s.nativeLaunch == nil || s.requestedOwner == nil || s.resumeContext != nil || s.repairHandoffPresent ||
		s.startupHold == nil || os.Getppid() != s.requestedOwner.PID || s.nativeLaunch.Launch.PID != s.requestedOwner.PID {
		return nil, fmt.Errorf("claude-native requires its original managed launch child")
	}
	identity, err := fsq.SnapshotDeliveryRoot(s.root)
	if err != nil {
		return nil, err
	}
	root, err := fsq.OpenDeliveryRoot(s.root, identity)
	if err != nil {
		return nil, err
	}
	ticket, ticketErr := launch.LoadExecutionTicket(root, s.me)
	closeErr := root.Close()
	if err := errors.Join(ticketErr, closeErr); err != nil {
		return nil, err
	}
	if ticket.State != launch.ExecutionPending || ticket.ConversationID != s.nativeLaunch.SessionID ||
		ticket.ConversationID != ticket.LaunchNonce || ticket.Provider != launch.ClaudeProvider ||
		ticket.ProviderVersion != "2.1.311" || ticket.Mode != launch.AdapterModeMint || ticket.Wrapper != nil ||
		ticket.Cwd != s.nativeLaunch.Cwd || ticket.Execution == nil || ticket.Execution.InjectorMode != wakeInjectModeClaudeNative {
		return nil, fmt.Errorf("native launch does not match a fresh pending Claude ticket")
	}
	wake, err := launch.HolderOf(os.Getpid())
	if err != nil {
		return nil, err
	}
	n := &claudeNativeNotifier{root: canonicalWakeRoot(s.root), agent: s.me, dir: s.activeAgentDir,
		claim: s.currentWake, owner: *s.requestedOwner, binding: *s.nativeLaunch,
		stop: cfg.controlStop, closed: make(chan struct{})}
	n.state = claudeNativeAttempt{Schema: 1, Root: n.root, Agent: n.agent, Generation: s.currentWake.Lock.Generation,
		Wake: wake, Binding: n.binding, Phase: "selected"}
	if err := n.authority(); err != nil {
		return nil, err
	}
	var previous claudeNativeAttempt
	exists, err := n.readState(&previous)
	if err != nil {
		return nil, err
	}
	if exists {
		if previous.Schema != 1 || previous.Root != n.root || previous.Agent != n.agent ||
			(previous.Phase != "selected" && previous.Phase != "unconfirmed" && previous.Phase != "consumed") ||
			len(previous.Cohort) > claudeNativeCohortLimit {
			return nil, fmt.Errorf("native attempt state requires recovery")
		}
		// A replacement wake cannot inherit a live launch's posting authority.
		if state, _ := launch.InspectHolder(previous.Binding.Launch); state != launch.LeaseStale {
			return nil, fmt.Errorf("previous native launch is not proven retired")
		}
		if previous.Phase == "unconfirmed" {
			n.state = previous
			if _, err := n.Observe(); err != nil {
				return nil, err
			}
			if n.state.Phase != "consumed" {
				return nil, fmt.Errorf("previous native attempt is unconfirmed; consume its mail before a new native launch")
			}
		}
		n.state = claudeNativeAttempt{Schema: 1, Root: n.root, Agent: n.agent, Generation: s.currentWake.Lock.Generation,
			Wake: wake, Binding: n.binding, Phase: "selected"}
	}
	if err := n.save(); err != nil {
		return nil, err
	}
	return n, nil
}

func decodeClaudeNativeMetadata(raw []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("native metadata has trailing content")
	}
	return nil
}

func (n *claudeNativeNotifier) readState(value *claudeNativeAttempt) (bool, error) {
	var exists bool
	err := n.dir.withFD(func(fd int) error {
		raw, _, present, err := readWakeRepairMetadataAt(fd, claudeNativeStateName, "native attempt", filepath.Join(n.dir.path, claudeNativeStateName), claudeNativeMetadataLimit)
		exists = present
		if err != nil || !present {
			return err
		}
		return decodeClaudeNativeMetadata(raw, value)
	})
	return exists, err
}

func (n *claudeNativeNotifier) authority() error {
	select {
	case <-n.stop:
		return fmt.Errorf("native wake ownership ended")
	case <-n.closed:
		return fmt.Errorf("native wake closed")
	default:
	}
	if os.Getppid() != n.owner.PID || n.binding.Launch.PID != n.owner.PID {
		return fmt.Errorf("native wake is no longer the original direct child")
	}
	if state, _ := launch.InspectHolder(n.binding.Launch); state != launch.LeaseValid {
		return fmt.Errorf("native parent identity is uncertain")
	}
	observation, err := observeLiveWakeOwner(n.owner, "native parent identity")
	if err != nil {
		return err
	}
	if err := observation.Close(); err != nil {
		return err
	}
	if err := validateCanonicalWakeAgentDir(n.dir); err != nil {
		return err
	}
	return n.dir.withFD(func(fd int) error {
		current := inspectWakeLockAt(fd, n.dir, n.root, n.agent)
		if !sameWakeLockGeneration(n.claim, current) || current.PID != os.Getpid() {
			return fmt.Errorf("native wake generation changed")
		}
		return nil
	})
}

func (n *claudeNativeNotifier) save() error {
	if err := n.authority(); err != nil {
		return err
	}
	raw, err := json.Marshal(n.state)
	if err != nil {
		return err
	}
	if len(raw)+1 > claudeNativeMetadataLimit {
		return fmt.Errorf("native attempt exceeds its state bound")
	}
	return withWakeLifecycleGuardInDir(n.dir, wakeGuardOptions{existing: true, noWait: true}, func(fd int) error {
		select {
		case <-n.stop:
			return fmt.Errorf("native wake stopped before persistence")
		case <-n.closed:
			return fmt.Errorf("native wake closed before persistence")
		default:
		}
		current := inspectWakeLockAt(fd, n.dir, n.root, n.agent)
		if !sameWakeLockGeneration(n.claim, current) || current.PID != os.Getpid() {
			return fmt.Errorf("native wake generation changed before persistence")
		}
		return writeWakeRepairMetadataAt(fd, n.dir, claudeNativeStateName, "native attempt", append(raw, '\n'), claudeNativeMetadataLimit)
	})
}

func (n *claudeNativeNotifier) ready() error {
	return n.dir.withFD(func(fd int) error {
		raw, _, exists, err := readWakeRepairMetadataAt(fd, claudeNativeReadyName, "native startup acknowledgement", filepath.Join(n.dir.path, claudeNativeReadyName), claudeNativeMetadataLimit)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("native startup acknowledgement unavailable; installed dialog hooks are required")
		}
		var ready claudeNativeReady
		if err := decodeClaudeNativeMetadata(raw, &ready); err != nil {
			return err
		}
		if ready.Schema != 1 || ready.SessionID != n.binding.SessionID || ready.Launch != n.binding.Launch {
			return fmt.Errorf("native startup acknowledgement belongs to another launch")
		}
		raw, info, exists, err := readWakeRepairMetadataAt(fd, wakeDialogFileName, "wake dialog marker", filepath.Join(n.dir.path, wakeDialogFileName), maxWakeMetadataFileBytes)
		if err != nil || !exists {
			return err
		}
		var marker wakeDialogMarker
		if json.Unmarshal(raw, &marker) != nil || marker.Schema != 1 || marker.SessionID != n.binding.SessionID || info.ModTime().After(time.Now().Add(wakeDialogRecheck)) {
			return fmt.Errorf("native delivery retains an uncertain dialog hold")
		}
		if marker.Event == wakeDialogStartupEvent || marker.Kind == claudeNativeStartupKind {
			return fmt.Errorf("native delivery retains the startup hold")
		}
		if marker.Event == "PermissionRequest" || marker.Event == "PreToolUse" && (marker.Tool == "AskUserQuestion" || marker.Tool == "ExitPlanMode") {
			return nil
		}
		return fmt.Errorf("native delivery retains an unknown-purpose hold")
	})
}

func nativeDoorbellFrame(id string) ([]byte, error) {
	envelope, err := claudepeer.BuildCrossSessionEnvelope("amq-doorbell", "amq", coopWakeDoorbell, "", "amq-wake", "code")
	if err != nil {
		return nil, err
	}
	frame := claudepeer.Frame{MsgV: 1, MsgID: id, Type: "user", Priority: "next", From: "amq-doorbell",
		Message: claudepeer.MessageBody{Role: "user", Content: envelope}}
	raw, err := json.Marshal(frame)
	if err != nil {
		return nil, err
	}
	raw = append(raw, '\n')
	if len(raw) > claudeNativeFrameLimit {
		return nil, fmt.Errorf("native doorbell exceeds 4 KiB")
	}
	return raw, nil
}

func (n *claudeNativeNotifier) Notify(pending map[string]os.FileInfo, observedAt time.Time) (bool, error) {
	if n.state.Disabled != "" {
		return false, fmt.Errorf("native wake disabled until relaunch: %s", n.state.Disabled)
	}
	if n.state.Phase == "unconfirmed" {
		return true, nil
	}
	if err := n.authority(); err != nil {
		return false, err
	}
	if err := n.ready(); err != nil {
		return false, err
	}
	if len(pending) > claudeNativeCohortLimit {
		return false, fmt.Errorf("native cohort exceeds 512 messages; drain manually")
	}
	registry, err := readClaudeNativeRegistry(n.binding)
	if err != nil {
		if errors.Is(err, errClaudeNativeRegistryMismatch) {
			return false, n.disable(err.Error())
		}
		return false, err
	}
	if n.state.Socket != "" && n.state.Socket != registry.Socket {
		return false, n.disable("native socket changed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-n.stop:
			cancel()
		case <-n.closed:
			cancel()
		case <-ctx.Done():
		}
	}()
	defer func() { cancel(); <-watchDone }()
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", registry.Socket)
	if err != nil {
		return false, err
	}
	defer func() { _ = connection.Close() }()
	conn, ok := connection.(*net.UnixConn)
	if !ok {
		return false, fmt.Errorf("native endpoint is not a Unix socket")
	}
	peer, err := darwinPeerPID(conn)
	if err != nil || peer != n.owner.PID {
		return false, n.disable("native connected peer is not the bound parent")
	}
	if err := n.authority(); err != nil {
		return false, err
	}
	if n.state.Socket == "" {
		n.state.Socket = registry.Socket
		if err := n.save(); err != nil {
			return false, err
		}
	}
	current, err := readClaudeNativeRegistry(n.binding)
	if err != nil {
		if errors.Is(err, errClaudeNativeRegistryMismatch) {
			return false, n.disable(err.Error())
		}
		return false, err
	}
	if current != registry {
		return false, n.disable("native session changed before payload")
	}
	if err := n.ready(); err != nil {
		return false, err
	}
	id, err := claudepeer.NewMsgID()
	if err != nil {
		return false, err
	}
	wire, err := nativeDoorbellFrame(id)
	if err != nil {
		return false, err
	}
	cohort := make([]string, 0, len(pending))
	for name := range pending {
		cohort = append(cohort, name)
	}
	sort.Strings(cohort)
	n.state.Socket, n.state.MessageID, n.state.Cohort = registry.Socket, id, cohort
	// A consumer can drain between the scan and the socket connection.
	// Its receipt must count even when it precedes this persistence step.
	n.state.CreatedAt, n.state.Phase = observedAt.UTC(), "unconfirmed"
	// This durable uncertainty fence precedes every possible payload byte.
	if err := n.save(); err != nil {
		return true, err
	}
	if err := n.authority(); err != nil {
		return true, err
	}
	if err := ctx.Err(); err != nil {
		return true, err
	}
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return true, err
	}
	closed := make(chan struct{})
	go func() { defer close(closed); <-ctx.Done(); _ = conn.Close() }()
	defer func() { cancel(); <-closed }()
	for len(wire) > 0 {
		written, err := conn.Write(wire)
		wire = wire[written:]
		if err != nil {
			return true, fmt.Errorf("native write unconfirmed: %w", err)
		}
		if written == 0 {
			return true, io.ErrShortWrite
		}
	}
	return true, nil
}

func (n *claudeNativeNotifier) disable(reason string) error {
	n.state.Disabled = reason
	return errors.Join(fmt.Errorf("native wake disabled until relaunch: %s", reason), n.save())
}

// Observe closes only mail obligations with actual consumer-local receipts.
// Inbox disappearance and a successful socket write are not consumption.
func (n *claudeNativeNotifier) Observe() (bool, error) {
	if n.state.Phase != "unconfirmed" {
		return false, nil
	}
	if len(n.state.Cohort) == 0 || n.state.CreatedAt.IsZero() {
		return false, fmt.Errorf("native unconfirmed state has no bounded cohort")
	}
	complete := true
	err := n.dir.withFD(func(fd int) error {
		receiptsFD, err := unix.Openat(fd, "receipts", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		defer func() { _ = unix.Close(receiptsFD) }()
		for _, name := range n.state.Cohort {
			if filepath.Base(name) != name || !strings.HasSuffix(name, ".md") {
				return fmt.Errorf("native cohort contains an invalid message name")
			}
			id := strings.TrimSuffix(name, ".md")
			found := false
			for _, stage := range []string{"drained", "dlq"} {
				leaf := id + "__" + n.agent + "__" + stage + ".json"
				raw, _, exists, err := readWakeRepairMetadataAt(receiptsFD, leaf, "native consumption receipt", filepath.Join(n.dir.path, "receipts", leaf), claudeNativeMetadataLimit)
				if err != nil {
					return err
				}
				if !exists {
					continue
				}
				var rec receipt.Receipt
				if json.Unmarshal(raw, &rec) != nil {
					return fmt.Errorf("native consumption receipt is invalid")
				}
				at, err := time.Parse(time.RFC3339Nano, rec.EmittedAt)
				if err != nil || rec.Schema != format.CurrentSchema || rec.MsgID != id || rec.Consumer != n.agent || rec.Stage != stage || at.Before(n.state.CreatedAt) {
					continue
				}
				found = true
			}
			if !found {
				complete = false
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	if complete {
		n.state.Phase = "consumed"
		return true, n.save()
	}
	return false, nil
}

func (n *claudeNativeNotifier) Close() error {
	n.closeOnce.Do(func() { close(n.closed) })
	return nil
}
