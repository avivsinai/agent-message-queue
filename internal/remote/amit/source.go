package amit

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/avivsinai/agent-message-queue/internal/remote/registry"
)

// sourcePath returns the extension event log for one Amit host root:
// <root>/agents/<extension>/extensions/remote-events.jsonl. The Amit-side
// amq-bridge extension appends JSON lines there; this adapter reads them.
// The log is owned by the extension; the adapter never writes it.
func sourcePath(root, extension string) string {
	return filepath.Join(root, "agents", extension, "extensions", "remote-events.jsonl")
}

// fileSource is a SessionSource over the extension's JSON-line event log.
// It polls the log on each Entries/Status call; Subscribe polls on a timer.
// The Amit host runs the companion; this file seam keeps the adapter
// process-free (no socket spawn, no Amit ownership).
type fileSource struct {
	path string
	stop chan struct{}
	// subTail is how many entries the (single) subscription has delivered.
	// Guarded by subMu: the poll goroutine is the only writer.
	subTail  int
	subMu    sync.Mutex
	stopOnce sync.Once
}

// Dial opens the SessionSource for a manifest config. Production seam: the
// extension event log under the companion root. A missing log is an error —
// the manifest entry names an extension that is not running.
func Dial(ctx context.Context, cfg registry.FactoryConfig, extension string) (SessionSource, error) {
	path := sourcePath(cfg.Root, extension)
	if st, err := os.Stat(path); err != nil || st.IsDir() {
		return nil, fmt.Errorf("amit: extension %q event log not found at %s (is the amq-bridge extension running?)", extension, path)
	}
	fs := &fileSource{path: path, stop: make(chan struct{})}
	return fs, nil
}
