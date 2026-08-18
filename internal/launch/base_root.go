package launch

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

const (
	baseRootUnauthorized    = "base_root_unauthorized"
	baseRootRelationInvalid = "base_root_relation_invalid"
)

type projectAMQRC struct {
	Root    string            `json:"root"`
	Project string            `json:"project,omitempty"`
	Peers   map[string]string `json:"peers,omitempty"`
}

type explicitBaseAuthority struct {
	configBytes     []byte
	configIdentity  string
	configSHA256    string
	configuredRoot  string
	parentIdentity  string
	authorityDigest string
	baseName        string
	baseMissing     bool
	parentRoot      *fsq.DeliveryRoot
}

func openExplicitBaseAuthority(projectRoot *fsq.DeliveryRoot, target PrepareTarget) (*explicitBaseAuthority, *fsq.DeliveryRoot, error) {
	config, configBytes, configIdentity, configSHA256, err := readExactProjectAMQRC(projectRoot)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", baseRootUnauthorized, err)
	}
	configuredRoot, err := configuredBasePath(projectRoot.Base(), config.Root)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", baseRootUnauthorized, err)
	}
	if target.BaseRoot != configuredRoot && filepath.Dir(target.BaseRoot) != configuredRoot {
		return nil, nil, fmt.Errorf("%s: base_root must be the configured root or one direct child", baseRootUnauthorized)
	}
	if filepath.Dir(target.SessionRoot) != target.BaseRoot || filepath.Base(target.SessionRoot) != target.Session {
		return nil, nil, fmt.Errorf("%s: session_root must be the direct child named %q", baseRootRelationInvalid, target.Session)
	}

	configured, configuredExists, err := openCanonicalDirectoryIfPresent(configuredRoot)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: configured root: %w", baseRootRelationInvalid, err)
	}
	var parentRoot, baseRoot *fsq.DeliveryRoot
	baseName := filepath.Base(target.BaseRoot)
	baseMissing := false
	switch {
	case configuredExists && target.BaseRoot == configuredRoot:
		baseRoot = configured
		parentRoot, err = openCanonicalDirectory(filepath.Dir(configuredRoot))
	case configuredExists:
		parentRoot = configured
		baseRoot, err = parentRoot.OpenDirectChild(baseName)
		if errors.Is(err, os.ErrNotExist) {
			err = nil
			baseMissing = true
		} else if err == nil {
			exact, exactErr := hasExactDirectChildName(parentRoot, baseName)
			if exactErr != nil {
				err = exactErr
			} else if !exact {
				err = fmt.Errorf("base_root uses an alternate direct-child spelling")
			}
		}
	case target.BaseRoot != configuredRoot:
		return nil, nil, fmt.Errorf("%s: configured root must exist before creating a profile child", baseRootUnauthorized)
	default:
		parentRoot, err = openCanonicalDirectory(filepath.Dir(configuredRoot))
		baseMissing = true
	}
	if err != nil {
		if baseRoot != nil {
			_ = baseRoot.Close()
		}
		if configured != nil && configured != parentRoot && configured != baseRoot {
			_ = configured.Close()
		}
		if parentRoot != nil && parentRoot != configured {
			_ = parentRoot.Close()
		}
		return nil, nil, fmt.Errorf("%s: open base authority: %w", baseRootRelationInvalid, err)
	}
	if baseRoot != nil && baseRoot.Base() != target.BaseRoot {
		_ = baseRoot.Close()
		if parentRoot != nil && parentRoot != baseRoot {
			_ = parentRoot.Close()
		}
		return nil, nil, fmt.Errorf("%s: base_root changed after canonicalization", baseRootRelationInvalid)
	}
	parentIdentity, err := fsq.StableTreeIdentityInfo(parentRoot.FileInfo())
	if err != nil {
		if baseRoot != nil {
			_ = baseRoot.Close()
		}
		_ = parentRoot.Close()
		return nil, nil, err
	}
	authorityDigest, err := digestCanonical(struct {
		ConfigIdentity string `json:"config_identity"`
		ConfigSHA256   string `json:"config_sha256"`
		ConfiguredRoot string `json:"configured_root"`
		BaseRoot       string `json:"base_root"`
		ParentIdentity string `json:"parent_identity"`
	}{configIdentity, configSHA256, configuredRoot, target.BaseRoot, parentIdentity})
	if err != nil {
		if baseRoot != nil {
			_ = baseRoot.Close()
		}
		_ = parentRoot.Close()
		return nil, nil, err
	}
	return &explicitBaseAuthority{
		configBytes: configBytes, configIdentity: configIdentity, configSHA256: configSHA256,
		configuredRoot: configuredRoot, parentIdentity: parentIdentity, authorityDigest: authorityDigest,
		baseName: baseName, baseMissing: baseMissing, parentRoot: parentRoot,
	}, baseRoot, nil
}

func readExactProjectAMQRC(projectRoot *fsq.DeliveryRoot) (projectAMQRC, []byte, string, string, error) {
	file, info, err := projectRoot.OpenRegularNoFollow(".amqrc")
	if err != nil {
		return projectAMQRC{}, nil, "", "", err
	}
	defer func() { _ = file.Close() }()
	if err := validateExactAMQRCFileInfo(info); err != nil {
		return projectAMQRC{}, nil, "", "", err
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return projectAMQRC{}, nil, "", "", err
	}
	identity, err := fsq.StableFileIdentityInfo(info)
	if err != nil {
		return projectAMQRC{}, nil, "", "", err
	}
	var config projectAMQRC
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return projectAMQRC{}, nil, "", "", fmt.Errorf("strict decode .amqrc: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return projectAMQRC{}, nil, "", "", fmt.Errorf("strict decode .amqrc: trailing content")
	}
	if strings.TrimSpace(config.Root) == "" {
		return projectAMQRC{}, nil, "", "", fmt.Errorf(".amqrc root is required")
	}
	sum := sha256.Sum256(data)
	return config, data, identity, "sha256:" + hex.EncodeToString(sum[:]), nil
}

func configuredBasePath(projectRoot, configured string) (string, error) {
	if strings.ContainsRune(configured, 0) || filepath.Clean(configured) != configured || configured == "." {
		return "", fmt.Errorf(".amqrc root must be a clean path")
	}
	if !filepath.IsAbs(configured) {
		for _, element := range strings.Split(filepath.ToSlash(configured), "/") {
			if element == "" || element == ".." || element == "." {
				return "", fmt.Errorf(".amqrc root contains an unsafe path element")
			}
		}
		configured = filepath.Join(projectRoot, configured)
	}
	if !filepath.IsAbs(configured) || filepath.Clean(configured) != configured {
		return "", fmt.Errorf("resolved .amqrc root must be a clean absolute path")
	}
	return configured, nil
}

func openCanonicalDirectory(path string) (*fsq.DeliveryRoot, error) {
	root, exists, err := openCanonicalDirectoryIfPresent(path)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, os.ErrNotExist
	}
	return root, nil
}

func openCanonicalDirectoryIfPresent(path string) (*fsq.DeliveryRoot, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, false, fmt.Errorf("not a direct directory")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, false, err
	}
	if resolved != path {
		return nil, false, fmt.Errorf("path traverses a symlink or uses an alternate spelling")
	}
	snapshot, err := fsq.SnapshotDeliveryRoot(path)
	if err != nil {
		return nil, false, err
	}
	root, err := fsq.OpenDeliveryRoot(path, snapshot)
	return root, err == nil, err
}

func hasExactDirectChildName(root *fsq.DeliveryRoot, name string) (bool, error) {
	entries, err := root.ReadDir(".")
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if entry.Name() == name {
			return true, nil
		}
	}
	return false, nil
}

func (authority *explicitBaseAuthority) close() {
	if authority != nil && authority.parentRoot != nil {
		_ = authority.parentRoot.Close()
	}
}

func (authority *explicitBaseAuthority) verify(projectRoot *fsq.DeliveryRoot, baseRoot *fsq.DeliveryRoot) error {
	_, data, identity, digest, err := readExactProjectAMQRC(projectRoot)
	if err != nil {
		return err
	}
	if identity != authority.configIdentity || digest != authority.configSHA256 || !bytes.Equal(data, authority.configBytes) {
		return fmt.Errorf("project .amqrc changed")
	}
	if err := authority.parentRoot.VerifyBase(); err != nil {
		return fmt.Errorf("base parent changed: %w", err)
	}
	parentIdentity, err := fsq.StableTreeIdentityInfo(authority.parentRoot.FileInfo())
	if err != nil || parentIdentity != authority.parentIdentity {
		return fmt.Errorf("base parent identity changed")
	}
	if authority.baseMissing {
		child, childErr := authority.parentRoot.OpenDirectChild(authority.baseName)
		if childErr == nil {
			_ = child.Close()
			return fmt.Errorf("base root appeared")
		}
		if !errors.Is(childErr, os.ErrNotExist) {
			return childErr
		}
	} else if baseRoot == nil {
		return fmt.Errorf("base root capability is missing")
	} else if err := baseRoot.VerifyBase(); err != nil {
		return fmt.Errorf("base root changed: %w", err)
	}
	return nil
}

func baseRootRefusalReason(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	for _, reason := range []string{baseRootUnauthorized, baseRootRelationInvalid} {
		if message == reason || strings.HasPrefix(message, reason+":") {
			return reason
		}
	}
	return ""
}
