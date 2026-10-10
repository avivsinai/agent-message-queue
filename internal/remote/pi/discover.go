package pi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
	"github.com/avivsinai/agent-message-queue/internal/remote/registry"
)

// maxDiscoverHandles bounds the scan of <root>/agents: one entry per handle.
const maxDiscoverHandles = 512

// discoverer lists the live pi chats under the root: every handle whose
// pi-bridge extension directory holds a live bridge.liveness (protocol:
// liveness). It reads liveness only, through the same classifier as the
// attachment, and never attaches. A candidate carries the manifest entry
// that attaches it and the chat's pi session id when the bridge publishes
// one (protocol: session identity).
type discoverer struct {
	// now overrides the clock for tests; nil = time.Now.
	now func() time.Time
}

func (d discoverer) Discover(_ context.Context, req registry.DiscoverRequest) ([]registry.Candidate, error) {
	if req.Root == "" {
		return nil, nil
	}
	agents := filepath.Join(req.Root, "agents")
	dir, err := os.Open(agents)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil // no agents under the root: nothing to discover
		}
		return nil, fmt.Errorf("read %s: %w", agents, err)
	}
	defer func() { _ = dir.Close() }()
	var handles []string
	scanned, incomplete := 0, false
	for !incomplete {
		entries, rerr := dir.ReadDir(128)
		for _, e := range entries {
			if scanned == maxDiscoverHandles {
				incomplete = true
				break
			}
			scanned++
			if e.IsDir() && fsq.ValidateHandle(e.Name()) == nil && protocol.ValidTargetID("pi:"+e.Name()) {
				handles = append(handles, e.Name())
			}
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				return nil, fmt.Errorf("read %s: %w", agents, rerr)
			}
			break
		}
	}
	sort.Strings(handles)
	now := time.Now
	if d.now != nil {
		now = d.now
	}
	var out []registry.Candidate
	for _, h := range handles {
		live := bridgeDir{dir: bridgePath(req.Root, h, piWire), names: piWire}.liveness(now())
		if !live.live {
			continue
		}
		native := ""
		if validSessionID(live.sessionID) {
			native = live.sessionID
		}
		cfg, _ := json.Marshal(config{Handle: h})
		out = append(out, registry.Candidate{Kind: "pi", Target: "pi:" + h, Display: h, Config: cfg, NativeSession: native, PID: live.pid})
	}
	if incomplete {
		return out, fmt.Errorf("%w: examined the first %d entries of %s", registry.ErrIncomplete, maxDiscoverHandles, agents)
	}
	return out, nil
}

func init() {
	registry.RegisterDiscoverer("pi", discoverer{})
}
