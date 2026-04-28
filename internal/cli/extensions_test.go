package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/config"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

func TestIsValidExtensionLayerName(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"io.github.omriariav.amq-squad", true},
		{"amq_squad-1.2", true},
		{"", false},
		{".", false},
		{"..", false},
		{"a..b", false},
		{"..layer", false},
		{"layer..", false},
		{"Upper", false},
		{"has space", false},
		{"slash/name", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isValidExtensionLayerName(tt.name); got != tt.want {
				t.Fatalf("isValidExtensionLayerName(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestRunDoctorJSONReportsExtensionManifestsAndDiagnostics(t *testing.T) {
	root := t.TempDir()
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatalf("ensure root dirs: %v", err)
	}
	if err := fsq.EnsureAgentDirs(root, "alice"); err != nil {
		t.Fatalf("ensure alice dirs: %v", err)
	}
	if err := config.WriteConfig(filepath.Join(root, "meta", "config.json"), config.Config{
		Version: 1,
		Agents:  []string{"alice"},
	}, true); err != nil {
		t.Fatalf("write config: %v", err)
	}

	layer := "io.github.omriariav.amq-squad"
	manifestDir := filepath.Join(root, "extensions", layer)
	if err := os.MkdirAll(manifestDir, 0o700); err != nil {
		t.Fatalf("mkdir manifest dir: %v", err)
	}
	manifest := map[string]any{
		"schema_version": 1,
		"layer":          layer,
		"version":        "0.3.1",
		"owns": []string{
			"agents/*/extensions/io.github.omriariav.amq-squad/launch.json",
			"agents/*/extensions/io.github.omriariav.amq-squad/role.md",
		},
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(manifestDir, "manifest.json"), manifestData, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	badJSONDir := filepath.Join(root, "extensions", "bad-json")
	if err := os.MkdirAll(badJSONDir, 0o700); err != nil {
		t.Fatalf("mkdir bad json dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(badJSONDir, "manifest.json"), []byte("{"), 0o600); err != nil {
		t.Fatalf("write bad manifest: %v", err)
	}

	invalidRootLayer := filepath.Join(root, "extensions", "BadLayer")
	if err := os.MkdirAll(invalidRootLayer, 0o700); err != nil {
		t.Fatalf("mkdir invalid root layer: %v", err)
	}
	invalidAgentLayer := filepath.Join(root, "agents", "alice", "extensions", "BadLayer")
	if err := os.MkdirAll(invalidAgentLayer, 0o700); err != nil {
		t.Fatalf("mkdir invalid agent layer: %v", err)
	}

	// Legacy direct-agent-root files from the migration overlap are not part of
	// the reserved namespace and should not produce extension diagnostics.
	if err := os.WriteFile(filepath.Join(root, "agents", "alice", "launch.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("write legacy launch file: %v", err)
	}

	t.Setenv("AM_ROOT", root)
	t.Setenv("AM_ME", "")

	output, err := captureEnvStdout(t, func() error {
		return runDoctor([]string{"--json"})
	})
	if err != nil {
		t.Fatalf("runDoctor: %v", err)
	}

	var result doctorResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal doctor output: %v, output was: %s", err, output)
	}
	if len(result.ExtensionManifests) != 1 {
		t.Fatalf("expected 1 extension manifest, got %d: %+v", len(result.ExtensionManifests), result.ExtensionManifests)
	}
	gotManifest := result.ExtensionManifests[0]
	if gotManifest.Layer != layer {
		t.Errorf("manifest layer = %q, want %q", gotManifest.Layer, layer)
	}
	if gotManifest.Version != "0.3.1" {
		t.Errorf("manifest version = %q, want 0.3.1", gotManifest.Version)
	}
	if gotManifest.Path != "extensions/io.github.omriariav.amq-squad/manifest.json" {
		t.Errorf("manifest path = %q", gotManifest.Path)
	}
	if len(gotManifest.Owns) != 2 {
		t.Errorf("manifest owns length = %d, want 2", len(gotManifest.Owns))
	}

	if len(result.ExtensionDiagnostics) != 3 {
		t.Fatalf("expected 3 extension diagnostics, got %d: %+v", len(result.ExtensionDiagnostics), result.ExtensionDiagnostics)
	}
	for _, diag := range result.ExtensionDiagnostics {
		if diag.Path == "agents/alice/launch.json" {
			t.Fatalf("legacy direct-agent-root file should not be diagnosed: %+v", diag)
		}
	}
	if !hasExtensionDiagnostic(result.ExtensionDiagnostics, "root", "", "BadLayer", "invalid extension layer name") {
		t.Fatalf("expected invalid root layer diagnostic, got %+v", result.ExtensionDiagnostics)
	}
	if !hasExtensionDiagnostic(result.ExtensionDiagnostics, "agent", "alice", "BadLayer", "invalid extension layer name") {
		t.Fatalf("expected invalid agent layer diagnostic, got %+v", result.ExtensionDiagnostics)
	}
	if !hasExtensionDiagnostic(result.ExtensionDiagnostics, "root", "", "bad-json", "malformed manifest") {
		t.Fatalf("expected malformed manifest diagnostic, got %+v", result.ExtensionDiagnostics)
	}
}

func TestReadPassiveExtensionManifestRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	layer := "symlink-manifest"
	layerDir := filepath.Join(root, "extensions", layer)
	if err := os.MkdirAll(layerDir, 0o700); err != nil {
		t.Fatalf("mkdir layer dir: %v", err)
	}
	target := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(target, []byte(`{"schema_version":1,"layer":"symlink-manifest"}`), 0o600); err != nil {
		t.Fatalf("write target manifest: %v", err)
	}
	manifestPath := filepath.Join(layerDir, "manifest.json")
	if err := os.Symlink(target, manifestPath); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	_, diag, ok := readPassiveExtensionManifest(root, layer, manifestPath)
	if ok {
		t.Fatal("expected symlink manifest to be rejected")
	}
	if diag == nil || !strings.Contains(diag.Message, "not a regular file") {
		t.Fatalf("diagnostic = %+v, want not a regular file", diag)
	}
}

func TestReadPassiveExtensionManifestRejectsOversizedFile(t *testing.T) {
	root := t.TempDir()
	layer := "oversized-manifest"
	layerDir := filepath.Join(root, "extensions", layer)
	if err := os.MkdirAll(layerDir, 0o700); err != nil {
		t.Fatalf("mkdir layer dir: %v", err)
	}
	manifestPath := filepath.Join(layerDir, "manifest.json")
	if err := os.WriteFile(manifestPath, []byte(strings.Repeat("x", 300*1024)), 0o600); err != nil {
		t.Fatalf("write oversized manifest: %v", err)
	}

	_, diag, ok := readPassiveExtensionManifest(root, layer, manifestPath)
	if ok {
		t.Fatal("expected oversized manifest to be rejected")
	}
	if diag == nil || !strings.Contains(diag.Message, "manifest is too large") {
		t.Fatalf("diagnostic = %+v, want manifest is too large", diag)
	}
}

func hasExtensionDiagnostic(diagnostics []doctorExtensionDiagnostic, scope, agent, layer, messagePrefix string) bool {
	for _, diag := range diagnostics {
		if diag.Scope != scope || diag.Agent != agent || diag.Layer != layer {
			continue
		}
		if len(diag.Message) >= len(messagePrefix) && diag.Message[:len(messagePrefix)] == messagePrefix {
			return true
		}
	}
	return false
}
