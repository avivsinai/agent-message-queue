package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/lock"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/ipc"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/registry"
)

// attachReadyTimeout bounds the wait for an endpoint that attach started.
var attachReadyTimeout = 20 * time.Second

// attach binds the per-user Buzz agent to the session the owner is typing
// in (bead agent-message-queue-611.31). Typing the command is the sharing
// choice, so the invoking session is found exactly, never guessed from a
// discovery list: Claude by its process ancestry, Codex by CODEX_THREAD_ID.
func attach(args []string, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := addCommon(fs)
	self := fs.Bool("self", false, "bind the session this command runs in")
	if err := fs.Parse(args); err != nil {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "%v", err)
	}
	if !*self {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "attach needs --self")
	}
	stateDir, err := c.stateDir()
	if err != nil {
		return protocol.ExitUsage, err
	}
	cand, err := selfCandidate(c.root, stateDir)
	if err != nil {
		return protocol.ExitActionRequired, err
	}
	reg := ipc.RegisterRequest{Kind: cand.Kind, Target: cand.Target, Config: cand.Config}
	session, err := registerTarget(stateDir, reg)
	if isEndpointUnreachable(err) {
		// No endpoint yet: record the target where startup reads it, then
		// start a supervised endpoint and register again once it answers.
		if err = persistAdapter(stateDir, manifest.Adapter{Kind: reg.Kind, Target: reg.Target, Config: reg.Config}); err == nil {
			if err = startEndpoint(c.root, stateDir); err == nil {
				session, err = awaitRegister(stateDir, reg)
			}
		}
	}
	if err != nil {
		return protocol.ExitActionRequired, err
	}
	native, err := nativeSessionOf(stateDir, cand.Target)
	if err != nil {
		return protocol.ExitActionRequired, err
	}
	display := cand.Display
	if display == "" {
		display = session.DisplayName
	}
	if err := binding.Write(binding.Binding{Root: c.root, Target: cand.Target, NativeSession: native, Display: display}); err != nil {
		return protocol.ExitActionRequired, err
	}
	say(stdout, "Connected: %s (%s). DM your AMQ Remote agent from Buzz.", nonEmpty(display, cand.Target), cand.Target)
	return 0, nil
}

// detach ends the binding. With --self it unbinds only when the binding
// names this session, so one session's off never unbinds another.
func detach(args []string, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("detach", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := addCommon(fs)
	self := fs.Bool("self", false, "unbind only if this session is the bound one")
	if err := fs.Parse(args); err != nil {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "%v", err)
	}
	target := ""
	if *self {
		stateDir, err := c.stateDir()
		if err != nil {
			return protocol.ExitUsage, err
		}
		cand, err := selfCandidate(c.root, stateDir)
		if err != nil {
			return protocol.ExitActionRequired, err
		}
		target = cand.Target
	}
	removed, err := binding.Remove(target)
	if err != nil {
		return protocol.ExitActionRequired, err
	}
	if !removed {
		say(stdout, "Not connected.")
		return 0, nil
	}
	say(stdout, "Disconnected. The session keeps running; Buzz DMs now answer \"Not connected\".")
	return 0, nil
}

// selfCandidate finds the invoking native session among discovered ones.
func selfCandidate(root, stateDir string) (registry.Candidate, error) {
	cands, _ := registry.Discover(context.Background(), registry.DiscoverRequest{Root: root, StateDir: stateDir})
	if thread := strings.TrimSpace(os.Getenv("CODEX_THREAD_ID")); thread != "" {
		for _, cand := range cands {
			if cand.Kind != "codex" {
				continue
			}
			var cfg struct {
				Thread string `json:"thread"`
			}
			if json.Unmarshal(cand.Config, &cfg) == nil && cfg.Thread == thread {
				return cand, nil
			}
		}
		return registry.Candidate{}, fmt.Errorf("codex thread %s is not loaded in the app-server daemon; start Codex with `codex --remote unix://`", thread)
	}
	ancestors, err := ancestorPIDs(os.Getpid())
	if err != nil {
		return registry.Candidate{}, err
	}
	for _, pid := range ancestors {
		for _, cand := range cands {
			if cand.Kind != "claude" {
				continue
			}
			var cfg struct {
				PID int `json:"pid"`
			}
			if json.Unmarshal(cand.Config, &cfg) == nil && cfg.PID == pid {
				return cand, nil
			}
		}
	}
	return registry.Candidate{}, errors.New("cannot identify the session this runs in; run it from inside a Claude Code or Codex session")
}

// ancestorPIDs lists the parent chain of pid, nearest first.
func ancestorPIDs(pid int) ([]int, error) {
	var out []int
	for i := 0; i < 64 && pid > 1; i++ {
		raw, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			return out, nil
		}
		parent, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil || parent <= 1 {
			return out, nil
		}
		out = append(out, parent)
		pid = parent
	}
	return out, nil
}

func registerTarget(stateDir string, reg ipc.RegisterRequest) (protocol.Session, error) {
	resp, err := ipc.Call(stateDir, ipc.Request{Register: &reg})
	if err != nil {
		return protocol.Session{}, err
	}
	if err := resp.AsError(); err != nil {
		return protocol.Session{}, err
	}
	var s protocol.Session
	if err := json.Unmarshal(resp.Reply, &s); err != nil {
		return protocol.Session{}, err
	}
	return s, nil
}

func awaitRegister(stateDir string, reg ipc.RegisterRequest) (protocol.Session, error) {
	deadline := time.Now().Add(attachReadyTimeout)
	for {
		s, err := registerTarget(stateDir, reg)
		if !isEndpointUnreachable(err) || time.Now().After(deadline) {
			return s, err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func nativeSessionOf(stateDir, target string) (string, error) {
	resp, err := ipc.Call(stateDir, ipc.Request{Native: &ipc.NativeQuery{TargetID: target}})
	if err != nil {
		return "", err
	}
	if err := resp.AsError(); err != nil {
		return "", err
	}
	var reply ipc.NativeReply
	if err := json.Unmarshal(resp.Reply, &reply); err != nil || reply.NativeSession == "" {
		return "", fmt.Errorf("target %s reports no native session", target)
	}
	return reply.NativeSession, nil
}

// persistAdapter adds a manifest entry for a so a restart attaches it again.
// An existing entry for the target is kept as it is.
func persistAdapter(stateDir string, a manifest.Adapter) error {
	if !lock.AdvisoryLockAvailable() {
		return errors.New("refusing to update the manifest without an advisory file lock")
	}
	path := manifest.DefaultPath(stateDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return lock.WithExclusiveFileLock(path+".lock", func() error {
		f, err := manifest.Load(path)
		if err != nil {
			return err
		}
		for _, existing := range f.Adapters {
			if existing.Target == a.Target {
				return nil
			}
		}
		f.Adapters = append(f.Adapters, a)
		if err := manifest.Validate(f); err != nil {
			return err
		}
		return manifest.Write(path, f)
	})
}

// liveRegistrar attaches one adapter to the running endpoint the way startup
// attaches manifest entries, after persisting it for restarts.
func liveRegistrar(root, stateDir string, ep *core.Endpoint) ipc.Registrar {
	var mu sync.Mutex
	return func(r ipc.RegisterRequest) (protocol.Session, error) {
		mu.Lock()
		defer mu.Unlock()
		a := manifest.Adapter{Kind: r.Kind, Target: r.Target, Config: r.Config, Epoch: r.Epoch}
		if err := persistAdapter(stateDir, a); err != nil {
			return protocol.Session{}, err
		}
		if _, ok := ep.AttachmentOf(r.Target); !ok {
			out := registry.Build(context.Background(), root, stateDir, manifest.File{Adapters: []manifest.Adapter{a}})
			if len(out) != 1 || out[0].Refusal != nil {
				reason := "no adapter built"
				if len(out) == 1 {
					reason = out[0].Refusal.Error()
				}
				return protocol.Session{}, protocol.Refuse(protocol.CodeUnsupported, "attach %s: %s", r.Target, reason)
			}
			ep.Register(out[0].Attachment)
		}
		for _, s := range ep.Sessions() {
			if s.TargetID == r.Target {
				return s, nil
			}
		}
		return protocol.Session{}, protocol.Refuse(protocol.CodeNotFound, "target %s did not attach", r.Target)
	}
}

func nonEmpty(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}
