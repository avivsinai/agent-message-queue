package presence

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// Presence captures the current presence for an agent handle.
type Presence struct {
	Schema   int    `json:"schema"`
	Handle   string `json:"handle"`
	Status   string `json:"status"`
	LastSeen string `json:"last_seen"`
	Note     string `json:"note,omitempty"`
}

func New(handle, status, note string, now time.Time) Presence {
	return Presence{
		Schema:   1,
		Handle:   handle,
		Status:   status,
		LastSeen: now.UTC().Format(time.RFC3339Nano),
		Note:     note,
	}
}

func Write(root string, p Presence) error {
	path := filepath.Join(root, "agents", p.Handle, "presence.json")
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = fsq.WriteFileAtomic(filepath.Dir(path), filepath.Base(path), data, 0o600)
	return err
}

func Read(root, handle string) (Presence, error) {
	path := filepath.Join(root, "agents", handle, "presence.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return Presence{}, err
	}
	var p Presence
	if err := json.Unmarshal(data, &p); err != nil {
		return Presence{}, err
	}
	return p, nil
}
