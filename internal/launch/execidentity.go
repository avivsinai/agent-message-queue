package launch

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	ExecutableTypeFile      = "file"
	ExecutableTypeSymlink   = "symlink"
	ExecutableTypeDirectory = "directory"
	ExecutableTypeOther     = "other"
)

// ExecutableIdentity is the freeze tuple for amq-xgc. Field names and decimal
// integer encoding are part of the contract. Schema-v2 subjects embed
// MarshalExecutableIdentity bytes as executable_identity.identity.
type ExecutableIdentity struct {
	CanonicalPath string               `json:"canonical_path"`
	Type          string               `json:"type"`
	Dev           uint64               `json:"dev"`
	Inode         uint64               `json:"inode"`
	VolumeID      uint64               `json:"volume_id"`
	FileID        uint64               `json:"file_id"`
	Size          int64                `json:"size"`
	MtimeNS       int64                `json:"mtime_ns"`
	SymlinkChain  []ExecutableIdentity `json:"symlink_chain"`
}

// ProbeExecutableIdentity records the on-disk identity of path, including
// every symlink hop. It does not consult PATH; the caller resolves names.
func ProbeExecutableIdentity(path string) (ExecutableIdentity, error) {
	if strings.TrimSpace(path) == "" {
		return ExecutableIdentity{}, fmt.Errorf("executable path is required")
	}
	current, err := filepath.Abs(path)
	if err != nil {
		return ExecutableIdentity{}, fmt.Errorf("absolute executable path: %w", err)
	}
	var chain []ExecutableIdentity
	seen := make(map[string]struct{}, 8)
	for {
		if _, dup := seen[current]; dup {
			return ExecutableIdentity{}, fmt.Errorf("symlink loop at %s", current)
		}
		seen[current] = struct{}{}
		node, err := probeExecutableNode(current)
		if err != nil {
			return ExecutableIdentity{}, err
		}
		if node.Type != ExecutableTypeSymlink {
			node.CanonicalPath = current
			if resolved, resolveErr := filepath.EvalSymlinks(current); resolveErr == nil {
				node.CanonicalPath = resolved
			}
			if chain == nil {
				chain = []ExecutableIdentity{}
			}
			node.SymlinkChain = chain
			return node, nil
		}
		hop := node
		hop.CanonicalPath = current
		hop.SymlinkChain = []ExecutableIdentity{}
		chain = append(chain, hop)
		next, err := os.Readlink(current)
		if err != nil {
			return ExecutableIdentity{}, fmt.Errorf("read symlink %s: %w", current, err)
		}
		if !filepath.IsAbs(next) {
			next = filepath.Join(filepath.Dir(current), next)
		}
		current = filepath.Clean(next)
	}
}

func probeExecutableNode(path string) (ExecutableIdentity, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return ExecutableIdentity{}, err
	}
	node := ExecutableIdentity{
		CanonicalPath: path,
		Type:          executableType(info),
		Size:          info.Size(),
		MtimeNS:       info.ModTime().UnixNano(),
		SymlinkChain:  []ExecutableIdentity{},
	}
	node.Dev, node.Inode, node.VolumeID, node.FileID, err = executableFileIDs(path, info)
	if err != nil {
		return ExecutableIdentity{}, err
	}
	return node, nil
}

func executableType(info os.FileInfo) string {
	mode := info.Mode()
	switch {
	case mode&os.ModeSymlink != 0:
		return ExecutableTypeSymlink
	case info.IsDir():
		return ExecutableTypeDirectory
	case mode.IsRegular():
		return ExecutableTypeFile
	default:
		return ExecutableTypeOther
	}
}

// MarshalExecutableIdentity emits canonical JSON: struct field order, no HTML
// escape, empty symlink_chain as [], integers as decimal JSON numbers.
func MarshalExecutableIdentity(identity ExecutableIdentity) ([]byte, error) {
	if identity.SymlinkChain == nil {
		identity.SymlinkChain = []ExecutableIdentity{}
	}
	for i := range identity.SymlinkChain {
		if identity.SymlinkChain[i].SymlinkChain == nil {
			identity.SymlinkChain[i].SymlinkChain = []ExecutableIdentity{}
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(identity); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(buf.Bytes()), nil
}

func probeExecutableIdentityJSON(path string) (string, error) {
	identity, err := ProbeExecutableIdentity(path)
	if err != nil {
		return "", err
	}
	raw, err := MarshalExecutableIdentity(identity)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// ConsultedExecutable is the Prepare-subject binding for one provider
// executable. Requested is the caller string; Consulted is the PATH lookup or
// absolute path that was probed. Identity is MarshalExecutableIdentity JSON.
type ConsultedExecutable struct {
	Requested string          `json:"requested"`
	Consulted string          `json:"consulted"`
	Identity  json.RawMessage `json:"identity,omitempty"`
}

// ResolveConsultedExecutable looks up requested on PATH when it is not
// absolute, then probes the consulted path when it exists. Missing paths stay
// identity-free so schema v2 stays stable for plan-only names.
func ResolveConsultedExecutable(requested string) (ConsultedExecutable, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return ConsultedExecutable{}, fmt.Errorf("executable path is required")
	}
	consulted := requested
	if filepath.IsAbs(requested) {
		abs, err := filepath.Abs(requested)
		if err != nil {
			return ConsultedExecutable{}, fmt.Errorf("absolute executable path: %w", err)
		}
		consulted = abs
	} else if looked, err := exec.LookPath(requested); err == nil {
		consulted = looked
	}
	result := ConsultedExecutable{Requested: requested, Consulted: consulted}
	if _, err := os.Lstat(consulted); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return result, nil
		}
		return ConsultedExecutable{}, err
	}
	identity, err := ProbeExecutableIdentity(consulted)
	if err != nil {
		return ConsultedExecutable{}, err
	}
	raw, err := MarshalExecutableIdentity(identity)
	if err != nil {
		return ConsultedExecutable{}, err
	}
	result.Identity = raw
	return result, nil
}
