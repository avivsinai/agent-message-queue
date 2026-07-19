package launchd

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const DefaultLabel = "com.ohade.amq-keepalive"

type Options struct {
	Label        string
	PlistPath    string
	BinaryPath   string
	RegistryPath string
	AMQPath      string
	Interval     time.Duration
	StdoutPath   string
	StderrPath   string
	Load         bool
}

func DefaultPlistPath(label string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if label == "" {
		label = DefaultLabel
	}
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist"), nil
}

func DefaultLogPaths(label string) (stdoutPath, stderrPath string, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	if label == "" {
		label = DefaultLabel
	}
	dir := filepath.Join(home, "Library", "Logs", "amq-keepalive")
	return filepath.Join(dir, label+".out.log"), filepath.Join(dir, label+".err.log"), nil
}

func ResolveExecutable(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("executable path is required")
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	resolved, err := exec.LookPath(path)
	if err != nil {
		return "", err
	}
	if filepath.IsAbs(resolved) {
		return filepath.Clean(resolved), nil
	}
	abs, err := filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	return abs, nil
}

func NormalizeOptions(opts Options) (Options, error) {
	if opts.Label == "" {
		opts.Label = DefaultLabel
	}
	if opts.BinaryPath == "" {
		return Options{}, errors.New("binary path is required")
	}
	binary, err := ResolveExecutable(opts.BinaryPath)
	if err != nil {
		return Options{}, fmt.Errorf("resolve binary path: %w", err)
	}
	opts.BinaryPath = binary

	if opts.AMQPath == "" {
		opts.AMQPath = "amq"
	}
	amqPath, err := ResolveExecutable(opts.AMQPath)
	if err != nil {
		return Options{}, fmt.Errorf("resolve amq path: %w", err)
	}
	opts.AMQPath = amqPath

	if opts.RegistryPath == "" {
		return Options{}, errors.New("registry path is required")
	}
	if !filepath.IsAbs(opts.RegistryPath) {
		abs, err := filepath.Abs(opts.RegistryPath)
		if err != nil {
			return Options{}, err
		}
		opts.RegistryPath = abs
	}
	if opts.Interval <= 0 {
		opts.Interval = 10 * time.Second
	}
	if opts.PlistPath == "" {
		path, err := DefaultPlistPath(opts.Label)
		if err != nil {
			return Options{}, err
		}
		opts.PlistPath = path
	}
	if opts.StdoutPath == "" || opts.StderrPath == "" {
		stdoutPath, stderrPath, err := DefaultLogPaths(opts.Label)
		if err != nil {
			return Options{}, err
		}
		if opts.StdoutPath == "" {
			opts.StdoutPath = stdoutPath
		}
		if opts.StderrPath == "" {
			opts.StderrPath = stderrPath
		}
	}
	return opts, nil
}

func Install(ctx context.Context, opts Options) error {
	opts, err := NormalizeOptions(opts)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(opts.PlistPath), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(opts.StdoutPath), 0o755); err != nil {
		return err
	}
	if err := ensureExistingPlistOwned(opts.PlistPath, opts.Label); err != nil {
		return err
	}
	if err := writeFileAtomic(opts.PlistPath, BuildPlist(opts), 0o644); err != nil {
		return err
	}
	if !opts.Load {
		return nil
	}
	_ = runLaunchctl(ctx, "bootout", serviceTarget(opts.Label))
	if err := runLaunchctl(ctx, "bootstrap", userDomain(), opts.PlistPath); err != nil {
		return err
	}
	return runLaunchctl(ctx, "kickstart", "-k", userDomain()+"/"+opts.Label)
}

func Uninstall(ctx context.Context, label string, plistPath string, unload bool) error {
	if label == "" {
		label = DefaultLabel
	}
	if plistPath == "" {
		path, err := DefaultPlistPath(label)
		if err != nil {
			return err
		}
		plistPath = path
	}
	if err := ensureExistingPlistOwned(plistPath, label); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if unload {
		_ = runLaunchctl(ctx, "bootout", serviceTarget(label))
	}
	err := os.Remove(plistPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func BuildPlist(opts Options) []byte {
	args := []string{
		opts.BinaryPath,
		"supervise",
		"--registry", opts.RegistryPath,
		"--amq", opts.AMQPath,
		"--self", opts.BinaryPath,
		"--interval", opts.Interval.String(),
	}
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	buf.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	buf.WriteString(`<plist version="1.0">` + "\n")
	buf.WriteString("<dict>\n")
	writeKeyString(&buf, "Label", opts.Label)
	writeKeyArray(&buf, "ProgramArguments", args)
	writeKeyBool(&buf, "RunAtLoad", true)
	writeKeyBool(&buf, "KeepAlive", true)
	writeKeyString(&buf, "StandardOutPath", opts.StdoutPath)
	writeKeyString(&buf, "StandardErrorPath", opts.StderrPath)
	writeKeyDict(&buf, "EnvironmentVariables", map[string]string{
		"PATH": "/Applications/cmux.app/Contents/Resources/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
	})
	buf.WriteString("</dict>\n")
	buf.WriteString("</plist>\n")
	return buf.Bytes()
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".plist-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

func ensureExistingPlistOwned(path, label string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !isOwnedPlist(data, label) {
		return fmt.Errorf("refusing to modify non-amq-keepalive launchd plist %s", path)
	}
	return nil
}

func isOwnedPlist(data []byte, label string) bool {
	return bytes.Contains(data, []byte("<key>Label</key>")) &&
		bytes.Contains(data, []byte("<string>"+label+"</string>")) &&
		bytes.Contains(data, []byte("<key>ProgramArguments</key>")) &&
		bytes.Contains(data, []byte("<string>supervise</string>")) &&
		bytes.Contains(data, []byte("<string>--registry</string>")) &&
		bytes.Contains(data, []byte("<string>--amq</string>")) &&
		bytes.Contains(data, []byte("<string>--self</string>"))
}

func runLaunchctl(ctx context.Context, args ...string) error {
	out, err := exec.CommandContext(ctx, "launchctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func userDomain() string {
	return "gui/" + strconv.Itoa(os.Getuid())
}

func serviceTarget(label string) string {
	return userDomain() + "/" + label
}

func writeKeyString(buf *bytes.Buffer, key string, value string) {
	buf.WriteString("\t<key>")
	_ = xml.EscapeText(buf, []byte(key))
	buf.WriteString("</key>\n\t<string>")
	_ = xml.EscapeText(buf, []byte(value))
	buf.WriteString("</string>\n")
}

func writeKeyArray(buf *bytes.Buffer, key string, values []string) {
	buf.WriteString("\t<key>")
	_ = xml.EscapeText(buf, []byte(key))
	buf.WriteString("</key>\n\t<array>\n")
	for _, value := range values {
		buf.WriteString("\t\t<string>")
		_ = xml.EscapeText(buf, []byte(value))
		buf.WriteString("</string>\n")
	}
	buf.WriteString("\t</array>\n")
}

func writeKeyBool(buf *bytes.Buffer, key string, value bool) {
	buf.WriteString("\t<key>")
	_ = xml.EscapeText(buf, []byte(key))
	if value {
		buf.WriteString("</key>\n\t<true/>\n")
		return
	}
	buf.WriteString("</key>\n\t<false/>\n")
}

func writeKeyDict(buf *bytes.Buffer, key string, values map[string]string) {
	buf.WriteString("\t<key>")
	_ = xml.EscapeText(buf, []byte(key))
	buf.WriteString("</key>\n\t<dict>\n")
	keys := make([]string, 0, len(values))
	for dictKey := range values {
		keys = append(keys, dictKey)
	}
	sort.Strings(keys)
	for _, dictKey := range keys {
		value := values[dictKey]
		buf.WriteString("\t\t<key>")
		_ = xml.EscapeText(buf, []byte(dictKey))
		buf.WriteString("</key>\n\t\t<string>")
		_ = xml.EscapeText(buf, []byte(value))
		buf.WriteString("</string>\n")
	}
	buf.WriteString("\t</dict>\n")
}
