package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type doctorExtensionManifest struct {
	Scope         string   `json:"scope"`
	Layer         string   `json:"layer"`
	Path          string   `json:"path"`
	SchemaVersion int      `json:"schema_version"`
	Version       string   `json:"version"`
	Owns          []string `json:"owns"`
}

type doctorExtensionDiagnostic struct {
	Scope   string `json:"scope"`
	Agent   string `json:"agent,omitempty"`
	Layer   string `json:"layer,omitempty"`
	Path    string `json:"path"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

type passiveExtensionManifest struct {
	SchemaVersion int      `json:"schema_version"`
	Layer         string   `json:"layer"`
	Version       string   `json:"version"`
	Owns          []string `json:"owns"`
}

func isValidExtensionLayerName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') ||
			c == '-' ||
			c == '_' ||
			c == '.' {
			continue
		}
		return false
	}
	return true
}

func scanExtensionMetadata(root string) ([]doctorExtensionManifest, []doctorExtensionDiagnostic) {
	var manifests []doctorExtensionManifest
	var diagnostics []doctorExtensionDiagnostic

	rootExtensionsDir := filepath.Join(root, "extensions")
	scanRootExtensions(root, rootExtensionsDir, &manifests, &diagnostics)
	scanAgentExtensions(root, &diagnostics)

	return manifests, diagnostics
}

func scanRootExtensions(root, extensionsDir string, manifests *[]doctorExtensionManifest, diagnostics *[]doctorExtensionDiagnostic) {
	entries, err := os.ReadDir(extensionsDir)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		*diagnostics = append(*diagnostics, doctorExtensionDiagnostic{
			Scope:   "root",
			Path:    rootRelativePath(root, extensionsDir),
			Status:  "error",
			Message: fmt.Sprintf("cannot read extension directory: %v", err),
		})
		return
	}

	for _, entry := range entries {
		layer := entry.Name()
		layerPath := filepath.Join(extensionsDir, layer)
		if !entry.IsDir() {
			*diagnostics = append(*diagnostics, doctorExtensionDiagnostic{
				Scope:   "root",
				Layer:   layer,
				Path:    rootRelativePath(root, layerPath),
				Status:  "warn",
				Message: "extension entry is not a directory",
			})
			continue
		}
		if !isValidExtensionLayerName(layer) {
			*diagnostics = append(*diagnostics, invalidLayerDiagnostic(root, "root", "", layer, layerPath))
			continue
		}

		manifestPath := filepath.Join(layerPath, "manifest.json")
		manifest, diag, ok := readPassiveExtensionManifest(root, layer, manifestPath)
		if diag != nil {
			*diagnostics = append(*diagnostics, *diag)
		}
		if ok {
			*manifests = append(*manifests, manifest)
		}
	}
}

func scanAgentExtensions(root string, diagnostics *[]doctorExtensionDiagnostic) {
	agentsDir := filepath.Join(root, "agents")
	agents, err := os.ReadDir(agentsDir)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		*diagnostics = append(*diagnostics, doctorExtensionDiagnostic{
			Scope:   "agent",
			Path:    rootRelativePath(root, agentsDir),
			Status:  "error",
			Message: fmt.Sprintf("cannot read agents directory for extensions: %v", err),
		})
		return
	}

	for _, agentEntry := range agents {
		if !agentEntry.IsDir() {
			continue
		}
		agent := agentEntry.Name()
		extensionsDir := filepath.Join(agentsDir, agent, "extensions")
		entries, err := os.ReadDir(extensionsDir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			*diagnostics = append(*diagnostics, doctorExtensionDiagnostic{
				Scope:   "agent",
				Agent:   agent,
				Path:    rootRelativePath(root, extensionsDir),
				Status:  "error",
				Message: fmt.Sprintf("cannot read agent extension directory: %v", err),
			})
			continue
		}
		for _, entry := range entries {
			layer := entry.Name()
			layerPath := filepath.Join(extensionsDir, layer)
			if !entry.IsDir() {
				*diagnostics = append(*diagnostics, doctorExtensionDiagnostic{
					Scope:   "agent",
					Agent:   agent,
					Layer:   layer,
					Path:    rootRelativePath(root, layerPath),
					Status:  "warn",
					Message: "extension entry is not a directory",
				})
				continue
			}
			if !isValidExtensionLayerName(layer) {
				*diagnostics = append(*diagnostics, invalidLayerDiagnostic(root, "agent", agent, layer, layerPath))
			}
		}
	}
}

func readPassiveExtensionManifest(root, layer, manifestPath string) (doctorExtensionManifest, *doctorExtensionDiagnostic, bool) {
	data, err := os.ReadFile(manifestPath)
	if os.IsNotExist(err) {
		return doctorExtensionManifest{}, nil, false
	}
	if err != nil {
		return doctorExtensionManifest{}, &doctorExtensionDiagnostic{
			Scope:   "root",
			Layer:   layer,
			Path:    rootRelativePath(root, manifestPath),
			Status:  "warn",
			Message: fmt.Sprintf("cannot read manifest: %v", err),
		}, false
	}

	var manifest passiveExtensionManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return doctorExtensionManifest{}, &doctorExtensionDiagnostic{
			Scope:   "root",
			Layer:   layer,
			Path:    rootRelativePath(root, manifestPath),
			Status:  "warn",
			Message: fmt.Sprintf("malformed manifest: %v", err),
		}, false
	}
	if manifest.SchemaVersion != 1 {
		return doctorExtensionManifest{}, &doctorExtensionDiagnostic{
			Scope:   "root",
			Layer:   layer,
			Path:    rootRelativePath(root, manifestPath),
			Status:  "warn",
			Message: fmt.Sprintf("unsupported manifest schema_version %d", manifest.SchemaVersion),
		}, false
	}
	if !isValidExtensionLayerName(manifest.Layer) {
		return doctorExtensionManifest{}, &doctorExtensionDiagnostic{
			Scope:   "root",
			Layer:   layer,
			Path:    rootRelativePath(root, manifestPath),
			Status:  "warn",
			Message: "manifest layer is not a valid extension layer name",
		}, false
	}
	if manifest.Layer != layer {
		return doctorExtensionManifest{}, &doctorExtensionDiagnostic{
			Scope:   "root",
			Layer:   layer,
			Path:    rootRelativePath(root, manifestPath),
			Status:  "warn",
			Message: fmt.Sprintf("manifest layer %q does not match directory %q", manifest.Layer, layer),
		}, false
	}
	if manifest.Owns == nil {
		manifest.Owns = []string{}
	}

	return doctorExtensionManifest{
		Scope:         "root",
		Layer:         manifest.Layer,
		Path:          rootRelativePath(root, manifestPath),
		SchemaVersion: manifest.SchemaVersion,
		Version:       manifest.Version,
		Owns:          manifest.Owns,
	}, nil, true
}

func invalidLayerDiagnostic(root, scope, agent, layer, path string) doctorExtensionDiagnostic {
	return doctorExtensionDiagnostic{
		Scope:   scope,
		Agent:   agent,
		Layer:   layer,
		Path:    rootRelativePath(root, path),
		Status:  "warn",
		Message: "invalid extension layer name",
	}
}

func checkExtensions(manifests []doctorExtensionManifest, diagnostics []doctorExtensionDiagnostic) doctorCheck {
	check := doctorCheck{Name: "Extensions"}
	if len(diagnostics) == 0 {
		check.Status = "ok"
		check.Message = fmt.Sprintf("%d manifest(s)", len(manifests))
		return check
	}

	check.Status = "warn"
	for _, diag := range diagnostics {
		if diag.Status == "error" {
			check.Status = "error"
			break
		}
	}
	check.Message = fmt.Sprintf("%d manifest(s), %d diagnostic(s)", len(manifests), len(diagnostics))
	return check
}

func rootRelativePath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}
