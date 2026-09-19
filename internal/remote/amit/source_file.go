package amit

import (
	"encoding/json"
	"os"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
)

// Compile-time seam checks.
var (
	_ SessionSource   = (*fileSource)(nil)
	_ core.Attachment = (*Attachment)(nil)
)

// logEvent is one JSON line in the extension event log.
type logEvent struct {
	Kind      string `json:"kind"`
	ClientRef string `json:"client_ref,omitempty"`
	Text      string `json:"text,omitempty"`
	Status    string `json:"status,omitempty"`
}

// Entries implements SessionSource: parse the JSON-line log, oldest first.
func (f *fileSource) Entries() []ExtEntry {
	data, err := os.ReadFile(f.path)
	if err != nil {
		return nil
	}
	var out []ExtEntry
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		var ev logEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev.Kind != "user_message" && ev.Kind != "agent_message" {
			continue
		}
		out = append(out, ExtEntry{Kind: ev.Kind, ClientRef: ev.ClientRef, Text: ev.Text})
	}
	return out
}

// Status implements SessionSource: the last "status" line wins.
func (f *fileSource) Status() string {
	data, err := os.ReadFile(f.path)
	if err != nil {
		return "offline"
	}
	status := "idle"
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		var ev logEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev.Kind == "status" && ev.Status != "" {
			status = ev.Status
		}
	}
	return status
}

// Subscribe implements SessionSource: poll the log on a timer until stop.
func (f *fileSource) Subscribe(fn func(ExtEvent)) func() {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-f.stop:
				return
			case <-ticker.C:
				// Poll: deliver new user_message entries as events. Tail
				// tracking is per-subscription.
				entries := f.Entries()
				f.subMu.Lock()
				for ; f.subTail < len(entries); f.subTail++ {
					e := entries[f.subTail]
					f.subMu.Unlock()
					fn(ExtEvent{Type: e.Kind, ClientRef: e.ClientRef, Text: e.Text})
					f.subMu.Lock()
				}
				f.subMu.Unlock()
			}
		}
	}()
	return func() {
		f.stopOnce.Do(func() { close(f.stop) })
		<-done
	}
}
