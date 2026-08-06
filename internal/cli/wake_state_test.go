package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestWakeStateRefusesEverySchemaExceptExactlyOne(t *testing.T) {
	state, _ := validWakeStateFixture(t, true)

	tests := []struct {
		name   string
		mutate func(*wakeState)
	}{
		{name: "older document", mutate: func(state *wakeState) { state.Schema = 0 }},
		{name: "newer document", mutate: func(state *wakeState) { state.Schema = 2 }},
		{name: "older target", mutate: func(state *wakeState) { state.Target.Schema = 0 }},
		{name: "newer target", mutate: func(state *wakeState) { state.Target.Schema = 2 }},
		{name: "older prepared", mutate: func(state *wakeState) { state.Prepared.Schema = 0 }},
		{name: "newer prepared", mutate: func(state *wakeState) { state.Prepared.Schema = 2 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := state
			prepared := *state.Prepared
			candidate.Prepared = &prepared
			test.mutate(&candidate)
			data, err := json.Marshal(candidate)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := decodeWakeState(data); err == nil || !strings.Contains(err.Error(), "schema") {
				t.Fatalf("decode error = %v, want exact-schema refusal", err)
			}
		})
	}
}

func TestNewerWakeStateSchemaProbeSharesReaderClassification(t *testing.T) {
	tests := []struct {
		name          string
		data          string
		wantComponent string
		wantSchema    int
		wantNewer     bool
	}{
		{
			name:          "newer document before unknown fields",
			data:          `{"schema":2,"future":true,"target":"unknown"}`,
			wantComponent: "",
			wantSchema:    2,
			wantNewer:     true,
		},
		{
			name:          "newer target inside current document",
			data:          `{"schema":1,"target":{"schema":2,"future":true},"prepared":null}`,
			wantComponent: "target",
			wantSchema:    2,
			wantNewer:     true,
		},
		{
			name:          "newer prepared inside current document",
			data:          `{"schema":1,"target":{},"prepared":{"schema":2,"future":true}}`,
			wantComponent: "prepared",
			wantSchema:    2,
			wantNewer:     true,
		},
		{
			name: "section schema does not rescue older document",
			data: `{"schema":0,"target":{"schema":2},"prepared":null}`,
		},
		{name: "current document with unknown field", data: `{"schema":1,"future":true,"target":{},"prepared":null}`},
		{name: "current document with null prepared", data: `{"schema":1,"target":{},"prepared":null}`},
		{name: "malformed current document", data: `{"schema":1`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			component, schema, newer := newerWakeStateSchema([]byte(test.data))
			if component != test.wantComponent || schema != test.wantSchema || newer != test.wantNewer {
				t.Fatalf("probe = (%q, %d, %v), want (%q, %d, %v)", component, schema, newer, test.wantComponent, test.wantSchema, test.wantNewer)
			}
			if test.wantNewer {
				if _, err := decodeWakeState([]byte(test.data)); err == nil || !strings.Contains(err.Error(), "schema") {
					t.Fatalf("reader error = %v, want shared newer-schema refusal", err)
				}
			}
		})
	}
}

func TestWakeStateRefusesUnknownFieldsAndNonCanonicalBytes(t *testing.T) {
	state, _ := validWakeStateFixture(t, true)
	canonical, err := encodeWakeState(state)
	if err != nil {
		t.Fatal(err)
	}

	unknownDocument := bytes.Replace(canonical, []byte(`{"schema":1,`), []byte(`{"schema":1,"unknown":true,`), 1)
	unknownTarget := bytes.Replace(canonical, []byte(`"target":{"schema":1,`), []byte(`"target":{"schema":1,"unknown":true,`), 1)
	unknownPrepared := bytes.Replace(canonical, []byte(`"prepared":{"schema":1,`), []byte(`"prepared":{"schema":1,"unknown":true,`), 1)
	var indented bytes.Buffer
	if err := json.Indent(&indented, canonical, "", "  "); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		data []byte
	}{
		{name: "unknown document field", data: unknownDocument},
		{name: "unknown target field", data: unknownTarget},
		{name: "unknown prepared field", data: unknownPrepared},
		{name: "leading whitespace", data: append([]byte(" "), canonical...)},
		{name: "indented", data: indented.Bytes()},
		{name: "trailing newline", data: append(bytes.Clone(canonical), '\n')},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decodeWakeState(test.data); err == nil {
				t.Fatal("non-canonical state was accepted")
			}
		})
	}
}

func TestWakeStateCanonicalEncodingIsDeterministic(t *testing.T) {
	state, _ := validWakeStateFixture(t, true)
	first, err := encodeWakeState(state)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeWakeState(first)
	if err != nil {
		t.Fatal(err)
	}
	second, err := encodeWakeState(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("canonical encodings differ:\nfirst  %s\nsecond %s", first, second)
	}
	if bytes.HasSuffix(first, []byte("\n")) {
		t.Fatal("canonical state has a trailing newline")
	}
}

func TestWakeStateDigestDomainsRemainSeparated(t *testing.T) {
	state, legacy := validWakeStateFixture(t, true)
	wantTargetDigest, err := wakeTargetDigest(*legacy.Target)
	if err != nil {
		t.Fatal(err)
	}
	if state.Target.TargetDigest != wantTargetDigest {
		t.Fatalf("target digest = %q, want wakeTargetDigest %q", state.Target.TargetDigest, wantTargetDigest)
	}

	compactTarget, err := json.Marshal(*legacy.Target)
	if err != nil {
		t.Fatal(err)
	}
	if wakeLegacyDigest(compactTarget) == wakeLegacyDigest(legacy.TargetRaw) {
		t.Fatal("raw legacy digest ignored formatting differences")
	}
	if got := wakeLegacyDigest(legacy.TargetRaw); got != state.Target.LegacyDigest {
		t.Fatalf("target legacy digest = %q, want %q", state.Target.LegacyDigest, got)
	}

	canonical, err := encodeWakeState(state)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := wakeCanonicalStateDigest(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if digest == state.Target.TargetDigest || digest == state.Target.LegacyDigest {
		t.Fatal("canonical-state digest was conflated with a target digest domain")
	}
	if _, err := wakeCanonicalStateDigest(append(canonical, '\n')); err == nil {
		t.Fatal("canonical-state digest accepted non-canonical bytes")
	}
}

func TestWakeStatePreparedFourWayClassification(t *testing.T) {
	state, _ := validWakeStateFixture(t, true)
	currentGeneration := state.Prepared.Generation
	currentTarget := state.Prepared.TargetDigest

	tests := []struct {
		name     string
		prepared *wakeStatePrepared
		want     wakeStatePreparedObservation
	}{
		{name: "absent", prepared: nil, want: wakeStatePreparedAbsent},
		{name: "stale generation", prepared: func() *wakeStatePrepared {
			candidate := *state.Prepared
			candidate.Generation = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			return &candidate
		}(), want: wakeStatePreparedStale},
		{name: "current generation and digest", prepared: state.Prepared, want: wakeStatePreparedCurrent},
		{name: "current generation wrong digest", prepared: func() *wakeStatePrepared {
			candidate := *state.Prepared
			candidate.TargetDigest = "sha256:" + strings.Repeat("f", 64)
			return &candidate
		}(), want: wakeStatePreparedRefused},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyWakeStatePrepared(test.prepared, currentGeneration, currentTarget); got != test.want {
				t.Fatalf("classification = %q, want %q", got, test.want)
			}
		})
	}
}

func TestWakeStateMirrorRefusesBidirectionalExistenceMismatch(t *testing.T) {
	withPrepared, legacyWithPrepared := validWakeStateFixture(t, true)
	withoutPrepared, legacyWithoutPrepared := validWakeStateFixture(t, false)

	tests := []struct {
		name   string
		state  wakeState
		legacy wakeStateLegacy
	}{
		{name: "legacy present state absent", state: withoutPrepared, legacy: legacyWithPrepared},
		{name: "state present legacy absent", state: withPrepared, legacy: legacyWithoutPrepared},
		{name: "target state present legacy absent", state: withPrepared, legacy: wakeStateLegacy{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateWakeStateAgainstLegacy(test.state, test.legacy)
			var mismatch *wakeStateLegacyMismatchError
			if !errors.As(err, &mismatch) {
				t.Fatalf("error = %v, want typed legacy mismatch", err)
			}
		})
	}
}

func TestWakeStateMirrorClassifiesDigestMismatch(t *testing.T) {
	state, legacy := validWakeStateFixture(t, true)
	legacy.TargetRaw = append(bytes.Clone(legacy.TargetRaw), ' ')

	err := validateWakeStateAgainstLegacy(state, legacy)
	var mismatch *wakeStateLegacyMismatchError
	if !errors.As(err, &mismatch) || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("error = %v, want typed digest mismatch", err)
	}
}

func TestWakeStateRefusesMissingTargetAndPreparedWithoutTarget(t *testing.T) {
	tests := [][]byte{
		[]byte(`{"schema":1,"prepared":null}`),
		[]byte(`{"schema":1,"prepared":{"schema":1,"generation":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","legacy_present":true,"target_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","legacy_digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}`),
	}
	for _, data := range tests {
		if _, err := decodeWakeState(data); err == nil || !strings.Contains(err.Error(), "target") {
			t.Fatalf("decode error = %v, want missing-target refusal", err)
		}
	}
}

func TestValidateWakeStateTargetRejectsInvalidRetryUntil(t *testing.T) {
	state, _ := validWakeStateFixture(t, false)
	for _, ok := range []string{"", wakeRetryUntilDrained, wakeRetryUntilInjected} {
		candidate := state.Target
		candidate.RetryUntil = ok
		digest, err := wakeTargetDigest(candidate.wakeTarget())
		if err != nil {
			t.Fatal(err)
		}
		candidate.TargetDigest = digest
		if err := validateWakeStateTarget(candidate); err != nil {
			t.Fatalf("canonical RetryUntil %q refused: %v", ok, err)
		}
	}
	for _, invalid := range []string{"replied", " INJECTED ", "Injected", " drained "} {
		t.Run(invalid, func(t *testing.T) {
			candidate := state.Target
			candidate.RetryUntil = invalid
			digest, err := wakeTargetDigest(candidate.wakeTarget())
			if err != nil {
				t.Fatal(err)
			}
			candidate.TargetDigest = digest
			err = validateWakeStateTarget(candidate)
			if err == nil || !strings.Contains(err.Error(), "retry-until") {
				t.Fatalf("error = %v, want noncanonical retry-until refusal despite matching digest", err)
			}
			full := state
			full.Target = candidate
			if err := validateWakeState(full); err == nil || !strings.Contains(err.Error(), "retry-until") {
				t.Fatalf("validateWakeState error = %v, want noncanonical retry-until refusal", err)
			}
		})
	}
}

func validWakeStateFixture(t *testing.T, prepared bool) (wakeState, wakeStateLegacy) {
	t.Helper()
	root := canonicalWakeRoot(t.TempDir())
	target := wakeTarget{
		Schema:     wakeTargetSchema,
		Mode:       wakeTargetInjectVia,
		Root:       root,
		Agent:      "codex",
		Created:    "2026-08-02T00:00:00Z",
		InjectVia:  filepath.Join(root, "injector"),
		InjectArgs: []string{"--fixture"},
	}
	targetRaw, err := json.MarshalIndent(target, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	legacy := wakeStateLegacy{
		Target:    &target,
		TargetRaw: append(targetRaw, '\n'),
	}
	if prepared {
		legacy.Prepared = &wakeStateLegacyPrepared{
			Schema:       wakeStatePreparedSchema,
			Generation:   "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			TargetDigest: mustWakeTargetDigest(target),
		}
		preparedRaw, err := json.Marshal(legacy.Prepared)
		if err != nil {
			t.Fatal(err)
		}
		legacy.PreparedRaw = append(preparedRaw, '\n')
	}
	state, err := newWakeState(legacy)
	if err != nil {
		t.Fatal(err)
	}
	return state, legacy
}
