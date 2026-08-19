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
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var launchdLabelPattern = regexp.MustCompile(`^[A-Za-z0-9]+(-[A-Za-z0-9]+)*(\.[A-Za-z0-9]+(-[A-Za-z0-9]+)*)+$`)

const DefaultLabel = "io.github.avivsinai.amq-keepalive"

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
	if err := validateLaunchdLabel(label); err != nil {
		return "", err
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
	if err := validateLaunchdLabel(label); err != nil {
		return "", "", err
	}
	dir := filepath.Join(home, "Library", "Logs", "amq-keepalive")
	return filepath.Join(dir, label+".out.log"), filepath.Join(dir, label+".err.log"), nil
}

func validateLaunchdLabel(label string) error {
	if !launchdLabelPattern.MatchString(label) || len(label) > 253 || label != filepath.Base(label) {
		return fmt.Errorf("launchd label %q must be reverse-DNS", label)
	}
	return nil
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
	if err := validateLaunchdLabel(opts.Label); err != nil {
		return Options{}, err
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
		opts.Interval = time.Minute
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
	if err := validateLaunchdLabel(label); err != nil {
		return err
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
		if err := runLaunchctl(ctx, "bootout", serviceTarget(label)); err != nil {
			return err
		}
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
	dict, err := parsePlistRootDict(data)
	if err != nil {
		return false
	}
	gotLabel, _ := dict["Label"].(string)
	if gotLabel != label {
		return false
	}
	rawArgs, ok := dict["ProgramArguments"].([]any)
	if !ok {
		return false
	}
	have := make(map[string]bool, len(rawArgs))
	for _, arg := range rawArgs {
		text, ok := arg.(string)
		if !ok {
			return false
		}
		have[text] = true
	}
	for _, required := range []string{"supervise", "--registry", "--amq", "--self"} {
		if !have[required] {
			return false
		}
	}
	return true
}

func parsePlistRootDict(data []byte) (map[string]any, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case "plist":
			continue
		case "dict":
			return parsePlistDict(dec)
		default:
			return nil, fmt.Errorf("plist root must be a dict")
		}
	}
}

func parsePlistDict(dec *xml.Decoder) (map[string]any, error) {
	out := make(map[string]any)
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.EndElement:
			if t.Name.Local == "dict" {
				return out, nil
			}
			return nil, fmt.Errorf("unexpected plist end element %s", t.Name.Local)
		case xml.StartElement:
			if t.Name.Local != "key" {
				return nil, fmt.Errorf("plist dict expected key, got %s", t.Name.Local)
			}
			var key string
			if err := dec.DecodeElement(&key, &t); err != nil {
				return nil, err
			}
			if key == "" {
				return nil, fmt.Errorf("plist dict key is empty")
			}
			if _, exists := out[key]; exists {
				return nil, fmt.Errorf("plist dict duplicate key %q", key)
			}
			start, err := nextPlistStart(dec)
			if err != nil {
				return nil, err
			}
			value, err := parsePlistValue(dec, start)
			if err != nil {
				return nil, err
			}
			out[key] = value
		}
	}
}

func parsePlistArray(dec *xml.Decoder) ([]any, error) {
	var out []any
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.EndElement:
			if t.Name.Local == "array" {
				return out, nil
			}
			return nil, fmt.Errorf("unexpected plist end element %s", t.Name.Local)
		case xml.StartElement:
			value, err := parsePlistValue(dec, t)
			if err != nil {
				return nil, err
			}
			out = append(out, value)
		}
	}
}

func parsePlistValue(dec *xml.Decoder, start xml.StartElement) (any, error) {
	switch start.Name.Local {
	case "dict":
		return parsePlistDict(dec)
	case "array":
		return parsePlistArray(dec)
	case "string", "integer", "real", "date", "data":
		var text string
		if err := dec.DecodeElement(&text, &start); err != nil {
			return nil, err
		}
		return text, nil
	case "true":
		if err := dec.Skip(); err != nil {
			return nil, err
		}
		return true, nil
	case "false":
		if err := dec.Skip(); err != nil {
			return nil, err
		}
		return false, nil
	default:
		return nil, fmt.Errorf("unsupported plist element %s", start.Name.Local)
	}
}

func nextPlistStart(dec *xml.Decoder) (xml.StartElement, error) {
	for {
		tok, err := dec.Token()
		if err != nil {
			return xml.StartElement{}, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			return t, nil
		case xml.EndElement:
			return xml.StartElement{}, fmt.Errorf("plist value missing")
		}
	}
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
