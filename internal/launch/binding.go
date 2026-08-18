package launch

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const (
	BindingVersion     = 1
	ResourceSetVersion = 1
	bindingDirectory   = "meta/launch"
	bindingFilename    = "binding.json"
)

// BindingRecord is disposable runtime state. It never identifies an AMQ
// session and never grants authority to execute a plan.
type BindingRecord struct {
	Version          int                 `json:"version"`
	Backend          string              `json:"backend"`
	HostIdentity     string              `json:"host_identity"`
	InstanceIdentity string              `json:"instance_identity"`
	Profile          string              `json:"profile"`
	LaunchNonce      string              `json:"launch_nonce"`
	Resources        ResourceIdentitySet `json:"resources"`
	Placement        PlacementPreview    `json:"placement,omitempty"`
}

type ResourceIdentitySet struct {
	Version   int                `json:"version"`
	Resources []ResourceIdentity `json:"resources"`
}

type ResourceIdentity struct {
	OpaqueID string `json:"opaque_id"`
	Agent    string `json:"agent,omitempty"`
}

func (record BindingRecord) Validate() error {
	if record.Version != BindingVersion {
		return fmt.Errorf("unsupported binding version %d", record.Version)
	}
	for name, value := range map[string]string{
		"backend": record.Backend, "host identity": record.HostIdentity,
		"instance identity": record.InstanceIdentity, "profile": record.Profile,
		"launch nonce": record.LaunchNonce,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	if record.Resources.Version != ResourceSetVersion {
		return fmt.Errorf("unsupported resource identity set version %d", record.Resources.Version)
	}
	seen := make(map[string]struct{}, len(record.Resources.Resources))
	for i, resource := range record.Resources.Resources {
		if strings.TrimSpace(resource.OpaqueID) == "" {
			return fmt.Errorf("resources[%d]: opaque identity is required", i)
		}
		if resource.Agent != "" {
			if err := fsq.ValidateHandle(resource.Agent); err != nil {
				return fmt.Errorf("resources[%d]: invalid agent association: %w", i, err)
			}
		}
		if _, ok := seen[resource.OpaqueID]; ok {
			return fmt.Errorf("duplicate resource identity %q", resource.OpaqueID)
		}
		seen[resource.OpaqueID] = struct{}{}
	}
	if pane := strings.TrimSpace(record.Placement.Effective.LauncherPane); pane != "" {
		owned := tmuxPaneResource(pane)
		if _, ok := seen[owned]; ok {
			return fmt.Errorf("launcher pane %q must not be an owned resource", pane)
		}
	}
	return nil
}

// WriteBinding replaces the session binding. A live *Lease is required;
// there is no lease-free write path.
func WriteBinding(root *fsq.DeliveryRoot, lease *Lease, record BindingRecord) error {
	if err := lease.authorizeWrite(root); err != nil {
		return err
	}
	if err := record.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = root.WriteFileAtomic(bindingDirectory, bindingFilename, data, 0o600)
	return err
}

func LoadBinding(root *fsq.DeliveryRoot) (BindingRecord, error) {
	if root == nil {
		return BindingRecord{}, fmt.Errorf("missing pinned session root")
	}
	file, info, err := root.OpenRegularNoFollow(filepath.Join(bindingDirectory, bindingFilename))
	if err != nil {
		return BindingRecord{}, err
	}
	defer func() { _ = file.Close() }()
	if info.Mode().Perm() != 0o600 {
		return BindingRecord{}, fmt.Errorf("binding permissions are %04o, want 0600", info.Mode().Perm())
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return BindingRecord{}, err
	}
	var record BindingRecord
	if err := decodeStrict(data, &record); err != nil {
		return BindingRecord{}, fmt.Errorf("decode launch binding: %w", err)
	}
	if err := record.Validate(); err != nil {
		return BindingRecord{}, err
	}
	return record, nil
}

// BindingPath is for diagnostics and tests only. I/O must use a pinned root.
func BindingPath(sessionRoot string) string {
	return filepath.Join(sessionRoot, bindingDirectory, bindingFilename)
}
