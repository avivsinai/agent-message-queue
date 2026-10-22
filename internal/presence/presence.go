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
	Schema           int    `json:"schema"`
	Handle           string `json:"handle"`
	Status           string `json:"status"`
	LastSeen         string `json:"last_seen"`
	Note             string `json:"note,omitempty"`
	NotifierStatus   string `json:"notifier_status,omitempty"`
	NotifierMode     string `json:"notifier_mode,omitempty"`
	NotifierReason   string `json:"notifier_reason,omitempty"`
	DoorbellParked   bool   `json:"doorbell_parked,omitempty"`
	DoorbellAttempts uint   `json:"doorbell_attempts,omitempty"`
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

// WriteDeliveryRoot writes presence through an already authorized root
// capability. Send/reply use this form so a path alias change cannot redirect a
// best-effort presence update outside the authorized tree.
func WriteDeliveryRoot(root *fsq.DeliveryRoot, p Presence) error {
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Join("agents", p.Handle)
	_, err = root.WriteFileAtomic(dir, "presence.json", data, 0o600)
	return err
}

// Update applies change to the stored presence, or to a default "active"
// presence when none exists, refreshes LastSeen, and writes the result. Each
// writer changes only the fields it owns, so the agent's status and note and
// the wake's notifier and doorbell state survive each other's writes.
func Update(root, handle string, change func(*Presence)) (Presence, error) {
	data, err := os.ReadFile(filepath.Join(root, "agents", handle, "presence.json"))
	p, err := updated(handle, data, err, change)
	if err != nil {
		return Presence{}, err
	}
	return p, Write(root, p)
}

// UpdateDeliveryRoot is Update through a pinned root capability.
func UpdateDeliveryRoot(root *fsq.DeliveryRoot, handle string, change func(*Presence)) (Presence, error) {
	data, err := root.ReadFile(filepath.Join("agents", handle, "presence.json"))
	p, err := updated(handle, data, err, change)
	if err != nil {
		return Presence{}, err
	}
	return p, WriteDeliveryRoot(root, p)
}

func updated(handle string, data []byte, readErr error, change func(*Presence)) (Presence, error) {
	now := time.Now()
	p := New(handle, "active", "", now)
	switch {
	case readErr == nil:
		if err := json.Unmarshal(data, &p); err != nil {
			return Presence{}, err
		}
		p.LastSeen = now.UTC().Format(time.RFC3339Nano)
	case !os.IsNotExist(readErr):
		return Presence{}, readErr
	}
	if change != nil {
		change(&p)
	}
	return p, nil
}

// SetStatus records the agent's own status and note.
func SetStatus(root, handle, status, note string) (Presence, error) {
	return Update(root, handle, statusChange(status, note))
}

// SetStatusDeliveryRoot is SetStatus through a pinned root capability.
func SetStatusDeliveryRoot(root *fsq.DeliveryRoot, handle, status, note string) (Presence, error) {
	return UpdateDeliveryRoot(root, handle, statusChange(status, note))
}

func statusChange(status, note string) func(*Presence) {
	return func(p *Presence) {
		p.Status = status
		p.Note = note
	}
}

// Touch updates the LastSeen timestamp for an agent's presence.
func Touch(root, handle string) error {
	_, err := Update(root, handle, nil)
	return err
}

// SetNotifierStatus records notifier capability. Empty values clear a stale
// notifier diagnosis when a later wake starts with a usable injector.
func SetNotifierStatus(root, handle, status, mode, reason string) error {
	_, err := Update(root, handle, func(p *Presence) {
		p.NotifierStatus = status
		p.NotifierMode = mode
		p.NotifierReason = reason
	})
	return err
}

// TouchDeliveryRoot updates presence through a pinned root capability.
func TouchDeliveryRoot(root *fsq.DeliveryRoot, handle string) error {
	_, err := UpdateDeliveryRoot(root, handle, nil)
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
