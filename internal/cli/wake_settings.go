package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
)

// Defaults for the live wake settings. A key absent from .wake.settings has
// its default; the amq wake flags use the same values.
const (
	defaultWakeDebounce          = 250 * time.Millisecond
	defaultWakePreviewLen        = 48
	defaultWakeDeferWhileInput   = true
	defaultWakeInputQuietFor     = 1200 * time.Millisecond
	defaultWakeInputPollInterval = 200 * time.Millisecond
	defaultWakeInputMaxHold      = 15 * time.Second
	defaultWakeInterrupt         = true
	defaultWakeInterruptLabel    = "interrupt"
	defaultWakeInterruptPriority = format.PriorityUrgent
	defaultWakeInterruptCooldown = 7 * time.Second
)

const (
	wakeSettingsFileName        = ".wake.settings"
	wakeSettingsAppliedFileName = ".wake.settings.applied"
	wakeSettingsSchemaV1        = 1
	wakeSettingsDigestAbsent    = "absent"
)

// wakeSettings is the policy a running wake consults per use. It is a plain
// comparable value, read and replaced only on the wake loop goroutine.
// Identity and transport inputs (handle, root, injector, retry policy) are
// not settings: they are bound into the lock and fixed for the process life.
type wakeSettings struct {
	debounce          time.Duration
	holdNormal        time.Duration
	holdLow           time.Duration
	previewLen        int
	bell              bool
	deferWhileInput   bool
	inputQuietFor     time.Duration
	inputPollInterval time.Duration
	inputMaxHold      time.Duration
	interrupt         bool
	interruptLabel    string
	interruptPriority string
	interruptNotice   string
	interruptCooldown time.Duration
	injectTimeout     time.Duration
}

func defaultWakeSettings() wakeSettings {
	return wakeSettings{
		debounce:          defaultWakeDebounce,
		previewLen:        defaultWakePreviewLen,
		deferWhileInput:   defaultWakeDeferWhileInput,
		inputQuietFor:     defaultWakeInputQuietFor,
		inputPollInterval: defaultWakeInputPollInterval,
		inputMaxHold:      defaultWakeInputMaxHold,
		interrupt:         defaultWakeInterrupt,
		interruptLabel:    defaultWakeInterruptLabel,
		interruptPriority: defaultWakeInterruptPriority,
		interruptCooldown: defaultWakeInterruptCooldown,
		injectTimeout:     defaultInjectTimeout,
	}
}

func (s wakeSettings) holdPolicy() wakeHoldPolicy {
	return wakeHoldPolicy{normal: s.holdNormal, low: s.holdLow}
}

// validate checks the whole value and returns it normalized (priority
// lowercased; label and notice trimmed). The interrupt label and priority
// must be valid only while interrupt is on, so enabling interrupt
// re-validates them in the same step. Errors name the amq wake flag.
func (s wakeSettings) validate() (wakeSettings, error) {
	s.interruptLabel = strings.TrimSpace(s.interruptLabel)
	s.interruptPriority = strings.ToLower(strings.TrimSpace(s.interruptPriority))
	s.interruptNotice = strings.TrimSpace(s.interruptNotice)
	switch {
	case s.previewLen < 0:
		return s, errors.New("--preview-len must be >= 0")
	case s.debounce < 0:
		return s, errors.New("--debounce must be >= 0")
	case s.holdNormal < 0:
		return s, errors.New("--hold-normal must be >= 0")
	case s.holdLow < 0:
		return s, errors.New("--hold-low must be >= 0")
	case s.interruptCooldown < 0:
		return s, errors.New("--interrupt-cooldown must be >= 0")
	case s.inputQuietFor < 0:
		return s, errors.New("--input-quiet-for must be >= 0")
	case s.inputPollInterval <= 0:
		return s, errors.New("--input-poll-interval must be > 0")
	case s.inputMaxHold < 0:
		return s, errors.New("--input-max-hold must be >= 0")
	case s.injectTimeout <= 0:
		return s, errors.New("--inject-timeout must be > 0")
	case s.interrupt && s.interruptLabel == "":
		return s, errors.New("interrupt-label is required when interrupt is enabled")
	case s.interrupt && s.interruptPriority == "":
		return s, errors.New("interrupt-priority is required when interrupt is enabled")
	case s.interrupt && !format.IsValidPriority(s.interruptPriority):
		return s, errors.New("--interrupt-priority must be one of: urgent, normal, low")
	}
	return s, nil
}

// wakeSettingDef is one row of the settings table. The table is the single
// definition of every live key: its JSON key in .wake.settings, its flag on
// amq wake and amq wake config, and its help text.
type wakeSettingDef struct {
	key   string
	flag  string
	usage string
	field func(*wakeSettings) any
}

var wakeSettingDefs = []wakeSettingDef{
	{"debounce", "debounce", "Debounce window for batching messages",
		func(s *wakeSettings) any { return &s.debounce }},
	{"hold_normal", "hold-normal", "Hold the first doorbell for normal-priority mail up to this long (0 = no priority hold)",
		func(s *wakeSettings) any { return &s.holdNormal }},
	{"hold_low", "hold-low", "Hold the first doorbell for low-priority mail up to this long (0 = no priority hold)",
		func(s *wakeSettings) any { return &s.holdLow }},
	{"preview_len", "preview-len", "Max subject preview length",
		func(s *wakeSettings) any { return &s.previewLen }},
	{"bell", "bell", "Ring terminal bell on new messages",
		func(s *wakeSettings) any { return &s.bell }},
	{"defer_while_input", "defer-while-input", "Best-effort: defer non-interrupt injection while terminal input appears active",
		func(s *wakeSettings) any { return &s.deferWhileInput }},
	{"input_quiet_for", "input-quiet-for", "Quiet window before deferred injection (advisory only on Linux; tty atime granularity is ~8s)",
		func(s *wakeSettings) any { return &s.inputQuietFor }},
	{"input_poll_interval", "input-poll-interval", "Polling interval while waiting for quiet terminal input",
		func(s *wakeSettings) any { return &s.inputPollInterval }},
	{"input_max_hold", "input-max-hold", "Maximum time to defer one wake injection (0 = no hold)",
		func(s *wakeSettings) any { return &s.inputMaxHold }},
	{"interrupt", "interrupt", "Enable interrupt injection for urgent interrupt messages",
		func(s *wakeSettings) any { return &s.interrupt }},
	{"interrupt_label", "interrupt-label", "Label required to trigger interrupt",
		func(s *wakeSettings) any { return &s.interruptLabel }},
	{"interrupt_priority", "interrupt-priority", "Priority required to trigger interrupt",
		func(s *wakeSettings) any { return &s.interruptPriority }},
	{"interrupt_notice", "interrupt-notice", "Custom interrupt notice (default: auto)",
		func(s *wakeSettings) any { return &s.interruptNotice }},
	{"interrupt_cooldown", "interrupt-cooldown", "Minimum time between interrupts",
		func(s *wakeSettings) any { return &s.interruptCooldown }},
	{"inject_timeout", "inject-timeout", "Timeout for one --inject-via command",
		func(s *wakeSettings) any { return &s.injectTimeout }},
}

// wakeRestartOnlyFlags are the amq wake flags bound into the running wake's
// identity or transport. amq wake config refuses them.
var wakeRestartOnlyFlags = []string{
	"inject-mode",
	"inject-via",
	"inject-arg",
	"inject-cmd",
	"interrupt-cmd",
	"retry-until",
}

func lookupWakeSettingDef(key string) (wakeSettingDef, bool) {
	for _, def := range wakeSettingDefs {
		if def.key == key {
			return def, true
		}
	}
	return wakeSettingDef{}, false
}

// wakeSettingsKeys lists the JSON keys in table order.
func wakeSettingsKeys() []string {
	keys := make([]string, 0, len(wakeSettingDefs))
	for _, def := range wakeSettingDefs {
		keys = append(keys, def.key)
	}
	return keys
}

// copyWakeSetting copies one key's value from src to dst.
func copyWakeSetting(def wakeSettingDef, dst *wakeSettings, src wakeSettings) {
	switch p := def.field(dst).(type) {
	case *time.Duration:
		*p = *def.field(&src).(*time.Duration)
	case *int:
		*p = *def.field(&src).(*int)
	case *bool:
		*p = *def.field(&src).(*bool)
	case *string:
		*p = *def.field(&src).(*string)
	}
}

// registerWakeSettingsFlags registers every settings flag on fs with its
// default and help text and returns the value the flags parse into.
func registerWakeSettingsFlags(fs *flag.FlagSet) *wakeSettings {
	values := defaultWakeSettings()
	defaults := defaultWakeSettings()
	for _, def := range wakeSettingDefs {
		switch p := def.field(&values).(type) {
		case *time.Duration:
			fs.DurationVar(p, def.flag, *def.field(&defaults).(*time.Duration), def.usage)
		case *int:
			fs.IntVar(p, def.flag, *def.field(&defaults).(*int), def.usage)
		case *bool:
			fs.BoolVar(p, def.flag, *def.field(&defaults).(*bool), def.usage)
		case *string:
			fs.StringVar(p, def.flag, *def.field(&defaults).(*string), def.usage)
		}
	}
	return &values
}

// visitedWakeSettingsKeys returns the JSON keys whose flags were set
// explicitly on fs, in table order.
func visitedWakeSettingsKeys(fs *flag.FlagSet) []string {
	var keys []string
	for _, def := range wakeSettingDefs {
		if flagWasVisited(fs, def.flag) {
			keys = append(keys, def.key)
		}
	}
	return keys
}

// changedWakeSettingsKeys lists the keys whose values differ, in table order.
func changedWakeSettingsKeys(previous, next wakeSettings) []string {
	var keys []string
	for _, def := range wakeSettingDefs {
		var a, b wakeSettings
		copyWakeSetting(def, &a, previous)
		copyWakeSetting(def, &b, next)
		if a != b {
			keys = append(keys, def.key)
		}
	}
	return keys
}

// wakeSettingsDoc is the stored form of .wake.settings: the keys an operator
// set and their values. An absent key means the default.
type wakeSettingsDoc struct {
	values  wakeSettings
	present map[string]bool
}

// set stores keys from values. A key given explicitly is always stored, even
// at its default value; only unset removes a key.
func (doc *wakeSettingsDoc) set(values wakeSettings, keys []string) {
	if doc.present == nil {
		doc.present = make(map[string]bool)
	}
	for _, key := range keys {
		def, ok := lookupWakeSettingDef(key)
		if !ok {
			continue
		}
		copyWakeSetting(def, &doc.values, values)
		doc.present[key] = true
	}
}

// unset removes key so it returns to its default.
func (doc *wakeSettingsDoc) unset(key string) error {
	if _, ok := lookupWakeSettingDef(key); !ok {
		return fmt.Errorf(
			"unknown wake setting %q (valid: %s)",
			key,
			strings.Join(wakeSettingsKeys(), ", "),
		)
	}
	delete(doc.present, key)
	return nil
}

// effective overlays the stored keys on the defaults and validates the
// result, which it returns normalized.
func (doc wakeSettingsDoc) effective() (wakeSettings, error) {
	settings := defaultWakeSettings()
	for _, def := range wakeSettingDefs {
		if doc.present[def.key] {
			copyWakeSetting(def, &settings, doc.values)
		}
	}
	return settings.validate()
}

// encode returns the canonical file bytes: schema first, stored keys in
// table order, durations as Go duration strings.
func (doc wakeSettingsDoc) encode() ([]byte, error) {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, `{"schema":%d,"settings":{`, wakeSettingsSchemaV1)
	first := true
	for _, def := range wakeSettingDefs {
		if !doc.present[def.key] {
			continue
		}
		var value any
		switch p := def.field(&doc.values).(type) {
		case *time.Duration:
			value = p.String()
		default:
			value = p
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("encode wake setting %s: %w", def.key, err)
		}
		if !first {
			buf.WriteByte(',')
		}
		first = false
		fmt.Fprintf(&buf, "%q:", def.key)
		buf.Write(encoded)
	}
	buf.WriteString("}}\n")
	return buf.Bytes(), nil
}

// decodeWakeSettingsDoc reads the file strictly: the schema envelope first
// (a newer schema is refused before its fields are looked at), then the
// whole document with unknown fields, unknown keys, wrong types and trailing
// data refused. The caller validates the effective value.
func decodeWakeSettingsDoc(raw []byte) (wakeSettingsDoc, error) {
	var envelope struct {
		Schema *int `json:"schema"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return wakeSettingsDoc{}, fmt.Errorf("decode wake settings: %w", err)
	}
	if envelope.Schema == nil {
		return wakeSettingsDoc{}, errors.New("decode wake settings: schema is missing")
	}
	if *envelope.Schema != wakeSettingsSchemaV1 {
		return wakeSettingsDoc{}, fmt.Errorf(
			"wake settings schema %d is not supported (want %d)",
			*envelope.Schema,
			wakeSettingsSchemaV1,
		)
	}
	var file struct {
		Schema   int                        `json:"schema"`
		Settings map[string]json.RawMessage `json:"settings"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil {
		return wakeSettingsDoc{}, fmt.Errorf("decode wake settings: %w", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return wakeSettingsDoc{}, errors.New("decode wake settings: trailing data after the document")
	}
	doc := wakeSettingsDoc{present: make(map[string]bool, len(file.Settings))}
	for key, value := range file.Settings {
		def, ok := lookupWakeSettingDef(key)
		if !ok {
			return wakeSettingsDoc{}, fmt.Errorf(
				"decode wake settings: unknown key %q (valid: %s)",
				key,
				strings.Join(wakeSettingsKeys(), ", "),
			)
		}
		if err := decodeWakeSetting(def, &doc.values, value); err != nil {
			return wakeSettingsDoc{}, err
		}
		doc.present[key] = true
	}
	return doc, nil
}

func decodeWakeSetting(def wakeSettingDef, dst *wakeSettings, value json.RawMessage) error {
	// Unmarshal treats null as "leave unchanged"; a stored key needs a value.
	if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return fmt.Errorf("decode wake setting %s: value is null", def.key)
	}
	var err error
	switch p := def.field(dst).(type) {
	case *time.Duration:
		var text string
		if err = json.Unmarshal(value, &text); err == nil {
			*p, err = time.ParseDuration(text)
		}
	case *int:
		err = json.Unmarshal(value, p)
	case *bool:
		err = json.Unmarshal(value, p)
	case *string:
		err = json.Unmarshal(value, p)
	}
	if err != nil {
		return fmt.Errorf("decode wake setting %s: %w", def.key, err)
	}
	return nil
}

// wakeSettingsDigest names one observation of .wake.settings: a sha256 over
// the exact file bytes, or wakeSettingsDigestAbsent when there is no file.
func wakeSettingsDigest(raw []byte, exists bool) string {
	if !exists {
		return wakeSettingsDigestAbsent
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// wakeSettingsObservation is the last .wake.settings read the wake acted on.
// Change detection compares bytes (and the read error), never stat fields:
// inode reuse and mtime granularity can hide a fast same-size rewrite.
type wakeSettingsObservation struct {
	seen    bool
	exists  bool
	raw     []byte
	readErr string
}

func observeWakeSettings(raw []byte, exists bool, readErr error) wakeSettingsObservation {
	if readErr != nil {
		return wakeSettingsObservation{seen: true, exists: exists, readErr: readErr.Error()}
	}
	return wakeSettingsObservation{seen: true, exists: exists, raw: raw}
}

func (o wakeSettingsObservation) same(other wakeSettingsObservation) bool {
	return o.seen == other.seen &&
		o.exists == other.exists &&
		o.readErr == other.readErr &&
		bytes.Equal(o.raw, other.raw)
}

func (o wakeSettingsObservation) digest() string {
	if o.readErr != "" {
		return ""
	}
	return wakeSettingsDigest(o.raw, o.exists)
}

// wakeSettingsSourceUnavailableError means the settings file could not be
// looked at from the canonical agent directory. The reload skips without
// recording an observation; the loop's own authority checks handle the cause.
type wakeSettingsSourceUnavailableError struct{ err error }

func (err *wakeSettingsSourceUnavailableError) Error() string { return err.err.Error() }
func (err *wakeSettingsSourceUnavailableError) Unwrap() error { return err.err }

// resolveWakeSettingsFile returns the effective settings for one read of the
// file: defaults when it is absent, else its keys over the defaults.
func resolveWakeSettingsFile(raw []byte, exists bool, readErr error) (wakeSettings, error) {
	if readErr != nil {
		return wakeSettings{}, readErr
	}
	if !exists {
		return defaultWakeSettings(), nil
	}
	doc, err := decodeWakeSettingsDoc(raw)
	if err != nil {
		return wakeSettings{}, err
	}
	return doc.effective()
}

const (
	wakeSettingsStatusApplied = "applied"
	wakeSettingsStatusRefused = "refused"
)

// wakeSettingsAppliedStatus is what the wake reports in
// .wake.settings.applied for one observed file digest.
type wakeSettingsAppliedStatus struct {
	status string
	digest string
	err    string
}

// wakeSettingsStartup is how a starting wake resolves its settings.
type wakeSettingsStartup struct {
	settings wakeSettings
	observed wakeSettingsObservation
	// write is the canonical file to store before the wake starts; nil means
	// no write.
	write []byte
	// refused is a file the wake could not use; it runs with defaults.
	refused error
	// unseeded is why a resume could not seed an absent file from its
	// argv settings; it runs with them in memory and the loop retries.
	unseeded error
	// seed is the canonical file an unseeded resume still has to store;
	// nil means nothing to seed.
	seed    []byte
	applied wakeSettingsAppliedStatus
}

// planWakeSettingsStartup merges a start's settings flags with one read of
// .wake.settings. The file owns settings: explicit flags (explicit lists their
// keys) on a fresh start are a set into the file, and every other start takes
// the file as it is. A resume ignores settings flags when the file exists,
// because self-upgrade re-executes the original argv and would undo later live
// changes. A resume with no file seeds it from the flags once: an image that
// predates the file kept its settings only in argv. flags must already be
// validated.
//
// The error return refuses the start: an operator gave explicit flags and the
// file cannot take them. Any other refused file is reported in refused and the
// wake runs with defaults; it never exits because of the settings file.
func planWakeSettingsStartup(
	raw []byte,
	exists bool,
	readErr error,
	flags wakeSettings,
	explicit []string,
	resume bool,
) (wakeSettingsStartup, error) {
	if len(explicit) == 0 || (resume && (exists || readErr != nil)) {
		plan := wakeSettingsStartup{observed: observeWakeSettings(raw, exists, readErr)}
		settings, err := resolveWakeSettingsFile(raw, exists, readErr)
		if err != nil {
			plan.settings = defaultWakeSettings()
			plan.refused = err
			plan.applied = wakeSettingsAppliedStatus{
				status: wakeSettingsStatusRefused,
				digest: plan.observed.digest(),
				err:    err.Error(),
			}
			return plan, nil
		}
		plan.settings = settings
		plan.applied = wakeSettingsAppliedStatus{
			status: wakeSettingsStatusApplied,
			digest: plan.observed.digest(),
		}
		return plan, nil
	}

	if readErr != nil {
		return wakeSettingsStartup{}, readErr
	}
	var doc wakeSettingsDoc
	if exists {
		var err error
		if doc, err = decodeWakeSettingsDoc(raw); err != nil {
			return wakeSettingsStartup{}, err
		}
	}
	doc.set(flags, explicit)
	settings, err := doc.effective()
	if err != nil {
		return wakeSettingsStartup{}, err
	}
	encoded, err := doc.encode()
	if err != nil {
		return wakeSettingsStartup{}, err
	}
	plan := wakeSettingsStartup{
		settings: settings,
		observed: observeWakeSettings(encoded, true, nil),
	}
	if !exists || !bytes.Equal(raw, encoded) {
		plan.write = encoded
	}
	plan.applied = wakeSettingsAppliedStatus{
		status: wakeSettingsStatusApplied,
		digest: plan.observed.digest(),
	}
	return plan, nil
}

// reloadSettings re-reads .wake.settings and, when its bytes changed and the
// new file is valid, replaces cfg.settings. It reports whether the settings
// changed. A refused file never changes the running settings; it is logged
// once per distinct observation. A hold-policy change sets
// cfg.holdPolicyChanged for the next inbox scan. It runs only on the loop
// goroutine and is a no-op without a settings source.
func (cfg *wakeConfig) reloadSettings() bool {
	if cfg.settingsSource == nil {
		return false
	}
	retryPendingWakeSettingsApplied(cfg)
	raw, exists, readErr := cfg.settingsSource()
	// A detached directory, or a write that renamed the file in during the
	// read, is no observation of the file; the next tick reads it settled.
	var unavailable *wakeSettingsSourceUnavailableError
	var snapshotChanged *wakeSnapshotReadChangedError
	if errors.As(readErr, &unavailable) || errors.As(readErr, &snapshotChanged) {
		return false
	}
	observed := observeWakeSettings(raw, exists, readErr)
	if observed.same(cfg.settingsObserved) {
		return false
	}
	cfg.settingsObserved = observed
	next, err := resolveWakeSettingsFile(raw, exists, readErr)
	if err != nil {
		_ = writeWakeDiagnostic(
			cfg,
			"amq wake: %s refused: %v; keeping current settings\n",
			wakeSettingsFileName,
			err,
		)
		noteWakeSettingsApplied(cfg, wakeSettingsAppliedStatus{
			status: wakeSettingsStatusRefused,
			digest: observed.digest(),
			err:    err.Error(),
		})
		return false
	}
	changed := changedWakeSettingsKeys(cfg.settings, next)
	if len(changed) > 0 {
		if cfg.settings.holdPolicy() != next.holdPolicy() {
			cfg.holdPolicyChanged = true
		}
		cfg.settings = next
		_ = writeWakeDiagnostic(
			cfg,
			"amq wake: applied %s: %s\n",
			wakeSettingsFileName,
			strings.Join(changed, ", "),
		)
	}
	noteWakeSettingsApplied(cfg, wakeSettingsAppliedStatus{
		status: wakeSettingsStatusApplied,
		digest: observed.digest(),
	})
	return len(changed) > 0
}

// noteWakeSettingsApplied records status in the applied sidecar, best effort:
// a failed write is logged once and retried on the next reload. It never
// rolls back or blocks the settings swap.
func noteWakeSettingsApplied(cfg *wakeConfig, status wakeSettingsAppliedStatus) {
	if cfg.recordSettingsApplied == nil {
		return
	}
	pending := status
	cfg.pendingSettingsApplied = &pending
	if err := cfg.recordSettingsApplied(status); err != nil {
		_ = writeWakeDiagnostic(
			cfg,
			"amq wake: record %s: %v; retrying\n",
			wakeSettingsAppliedFileName,
			err,
		)
		return
	}
	cfg.pendingSettingsApplied = nil
}

func retryPendingWakeSettingsApplied(cfg *wakeConfig) {
	if cfg.pendingSettingsApplied == nil || cfg.recordSettingsApplied == nil {
		return
	}
	if cfg.recordSettingsApplied(*cfg.pendingSettingsApplied) == nil {
		cfg.pendingSettingsApplied = nil
	}
}
