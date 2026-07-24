//go:build darwin || linux

package cli

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

var wakeRepairAdmitTimeout = wakeReadyTimeout

const (
	wakeRepairHandoffSchema        = 1
	wakeRepairHandoffMaxFrameBytes = 64 * 1024

	wakeRepairHandoffKindSource   = "source"
	wakeRepairHandoffKindPrepared = "prepared"
	wakeRepairHandoffKindAdmit    = "admit"
	wakeRepairHandoffKindRelease  = "release"

	envWakeRepairHandoffReadFD  = "AMQ_WAKE_REPAIR_HANDOFF_READ_FD"
	envWakeRepairHandoffWriteFD = "AMQ_WAKE_REPAIR_HANDOFF_WRITE_FD"
	envWakeRepairAgentDirFD     = "AMQ_WAKE_REPAIR_AGENT_DIR_FD"
	envWakeRepairInboxDirFD     = "AMQ_WAKE_REPAIR_INBOX_DIR_FD"
)

// The handoff values deliberately contain no maps, slices, or pointers. Once
// constructed, copying one cannot alias mutable repair state held by either
// process.
type wakeRepairHandoffSource struct {
	schema             int
	root               string
	rootIdentity       string
	agent              string
	sourceGeneration   string
	sourceTargetDigest string
	sourceFloorDigest  string
	bootID             string
	agentDirDevice     uint64
	agentDirInode      uint64
	inboxDirDevice     uint64
	inboxDirInode      uint64
	hasOwner           bool
	ownerPID           int
	ownerProcessStart  string
	ownerBootID        string
	ownerSessionID     int
}

type wakeRepairHandoffPrepared struct {
	schema              int
	sourceDigest        string
	childPID            int
	childGeneration     string
	childTargetDigest   string
	childFloorDigest    string
	childFloorAuthority wakeRepairFloorAuthority
}

type wakeRepairHandoffAdmit struct {
	schema          int
	childGeneration string
	preparedDigest  string
}

type wakeRepairHandoffRelease struct {
	schema          int
	childGeneration string
	admitDigest     string
}

type wakeRepairHandoffSourceWire struct {
	Schema             int    `json:"schema"`
	Kind               string `json:"kind"`
	Root               string `json:"root"`
	RootIdentity       string `json:"root_identity"`
	Agent              string `json:"agent"`
	SourceGeneration   string `json:"source_generation"`
	SourceTargetDigest string `json:"source_target_digest"`
	SourceFloorDigest  string `json:"source_floor_digest"`
	BootID             string `json:"boot_id"`
	AgentDirDevice     uint64 `json:"agent_dir_device"`
	AgentDirInode      uint64 `json:"agent_dir_inode"`
	InboxDirDevice     uint64 `json:"inbox_dir_device"`
	InboxDirInode      uint64 `json:"inbox_dir_inode"`
	HasOwner           bool   `json:"has_owner,omitempty"`
	OwnerPID           int    `json:"owner_pid,omitempty"`
	OwnerProcessStart  string `json:"owner_process_start,omitempty"`
	OwnerBootID        string `json:"owner_boot_id,omitempty"`
	OwnerSessionID     int    `json:"owner_session_id,omitempty"`
}

type wakeRepairHandoffPreparedWire struct {
	Schema                 int              `json:"schema"`
	Kind                   string           `json:"kind"`
	SourceDigest           string           `json:"source_digest"`
	ChildPID               int              `json:"child_pid"`
	ChildGeneration        string           `json:"child_generation"`
	ChildTargetDigest      string           `json:"child_target_digest"`
	ChildFloorDigest       string           `json:"child_floor_digest"`
	ChildFloorSourceDigest string           `json:"child_floor_source_digest"`
	ChildFloorRawDigest    string           `json:"child_floor_raw_digest"`
	ChildFloorFileIdentity wakeFileIdentity `json:"child_floor_file_identity"`
}

type wakeRepairHandoffAdmitWire struct {
	Schema          int    `json:"schema"`
	Kind            string `json:"kind"`
	ChildGeneration string `json:"child_generation"`
	PreparedDigest  string `json:"prepared_digest"`
}

type wakeRepairHandoffReleaseWire struct {
	Schema          int    `json:"schema"`
	Kind            string `json:"kind"`
	ChildGeneration string `json:"child_generation"`
	AdmitDigest     string `json:"admit_digest"`
}

func newWakeRepairHandoffSource(
	floor wakeRepairFloor,
	target wakeTarget,
	agentDir *wakeAgentDir,
	inboxDir *wakeInboxDir,
) (wakeRepairHandoffSource, error) {
	targetDigest, err := wakeTargetDigest(target)
	if err != nil {
		return wakeRepairHandoffSource{}, err
	}
	floorDigest, err := wakeRepairFloorDigest(floor)
	if err != nil {
		return wakeRepairHandoffSource{}, err
	}
	source := wakeRepairHandoffSource{
		schema:             wakeRepairHandoffSchema,
		root:               floor.Root,
		rootIdentity:       floor.RootIdentity,
		agent:              floor.Agent,
		sourceGeneration:   floor.Generation,
		sourceTargetDigest: targetDigest,
		sourceFloorDigest:  floorDigest,
		bootID:             floor.BootID,
	}
	if floor.Owner != nil {
		source.hasOwner = true
		source.ownerPID = floor.Owner.PID
		source.ownerProcessStart = floor.Owner.ProcessStart
		source.ownerBootID = floor.Owner.BootID
		source.ownerSessionID = floor.Owner.SessionID
	}
	if source.sourceTargetDigest != floor.TargetDigest {
		return wakeRepairHandoffSource{}, fmt.Errorf("wake repair handoff source target digest mismatch")
	}
	if canonicalWakeRoot(target.Root) != canonicalWakeRoot(floor.Root) || target.Agent != floor.Agent {
		return wakeRepairHandoffSource{}, fmt.Errorf("wake repair handoff source target scope mismatch")
	}
	if !sameWakeOwner(floor.Owner, target.Owner) {
		return wakeRepairHandoffSource{}, fmt.Errorf("wake repair handoff source owner mismatch")
	}
	if err := source.bindRetainedDirectories(agentDir, inboxDir); err != nil {
		return wakeRepairHandoffSource{}, err
	}
	return source, nil
}

func (source *wakeRepairHandoffSource) bindRetainedDirectories(
	agentDir *wakeAgentDir,
	inboxDir *wakeInboxDir,
) error {
	if source == nil || agentDir == nil || inboxDir == nil {
		return fmt.Errorf("wake repair retained directory capability is missing")
	}
	agentIdentity, err := wakeRepairDirectoryIdentityForFile(agentDir.file)
	if err != nil {
		return err
	}
	inboxIdentity, err := wakeRepairDirectoryIdentityForFile(inboxDir.file)
	if err != nil {
		return err
	}
	source.agentDirDevice = agentIdentity.device
	source.agentDirInode = agentIdentity.inode
	source.inboxDirDevice = inboxIdentity.device
	source.inboxDirInode = inboxIdentity.inode
	return source.validate()
}

func (source wakeRepairHandoffSource) Owner() *wakeOwner {
	if !source.hasOwner {
		return nil
	}
	return &wakeOwner{
		PID:          source.ownerPID,
		ProcessStart: source.ownerProcessStart,
		BootID:       source.ownerBootID,
		SessionID:    source.ownerSessionID,
	}
}

func (source wakeRepairHandoffSource) SourceGeneration() string {
	return source.sourceGeneration
}

func (source wakeRepairHandoffSource) Root() string {
	return source.root
}

func (source wakeRepairHandoffSource) Agent() string {
	return source.agent
}

func (source wakeRepairHandoffSource) BootID() string {
	return source.bootID
}

func (source wakeRepairHandoffSource) SourceTargetDigest() string {
	return source.sourceTargetDigest
}

func (source wakeRepairHandoffSource) SourceFloorDigest() string {
	return source.sourceFloorDigest
}

func (source wakeRepairHandoffSource) RootIdentity() string {
	return source.rootIdentity
}

func (source wakeRepairHandoffSource) digest() (string, error) {
	if err := source.validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(source.wire())
	if err != nil {
		return "", fmt.Errorf("marshal wake repair handoff source digest: %w", err)
	}
	return wakeMetadataDigest(data), nil
}

func (source wakeRepairHandoffSource) validate() error {
	if source.schema != wakeRepairHandoffSchema {
		return fmt.Errorf("wake repair handoff source schema %d unsupported", source.schema)
	}
	for label, value := range map[string]string{
		"root":                 source.root,
		"root identity":        source.rootIdentity,
		"agent":                source.agent,
		"source generation":    source.sourceGeneration,
		"source target digest": source.sourceTargetDigest,
		"source floor digest":  source.sourceFloorDigest,
		"boot id":              source.bootID,
	} {
		if err := validateWakeRepairHandoffToken(label, value, 1024); err != nil {
			return err
		}
	}
	if err := validateWakeRepairHandoffDigest("source target", source.sourceTargetDigest); err != nil {
		return err
	}
	if err := validateWakeRepairHandoffDigest("source floor", source.sourceFloorDigest); err != nil {
		return err
	}
	if source.agentDirDevice == 0 || source.agentDirInode == 0 ||
		source.inboxDirDevice == 0 || source.inboxDirInode == 0 {
		return fmt.Errorf("wake repair handoff retained directory identity is invalid")
	}
	if source.hasOwner {
		if err := validateAuthoritativeWakeOwner(*source.Owner()); err != nil {
			return fmt.Errorf("wake repair handoff source owner is invalid: %w", err)
		}
	} else if source.ownerPID != 0 || source.ownerProcessStart != "" ||
		source.ownerBootID != "" || source.ownerSessionID != 0 {
		return fmt.Errorf("wake repair handoff source owner fields require has_owner")
	}
	return nil
}

func (source wakeRepairHandoffSource) wire() wakeRepairHandoffSourceWire {
	return wakeRepairHandoffSourceWire{
		Schema:             source.schema,
		Kind:               wakeRepairHandoffKindSource,
		Root:               source.root,
		RootIdentity:       source.rootIdentity,
		Agent:              source.agent,
		SourceGeneration:   source.sourceGeneration,
		SourceTargetDigest: source.sourceTargetDigest,
		SourceFloorDigest:  source.sourceFloorDigest,
		BootID:             source.bootID,
		AgentDirDevice:     source.agentDirDevice,
		AgentDirInode:      source.agentDirInode,
		InboxDirDevice:     source.inboxDirDevice,
		InboxDirInode:      source.inboxDirInode,
		HasOwner:           source.hasOwner,
		OwnerPID:           source.ownerPID,
		OwnerProcessStart:  source.ownerProcessStart,
		OwnerBootID:        source.ownerBootID,
		OwnerSessionID:     source.ownerSessionID,
	}
}

func sourceFromWire(wire wakeRepairHandoffSourceWire) (wakeRepairHandoffSource, error) {
	if wire.Kind != wakeRepairHandoffKindSource {
		return wakeRepairHandoffSource{}, fmt.Errorf("wake repair handoff message kind %q, want source", wire.Kind)
	}
	source := wakeRepairHandoffSource{
		schema:             wire.Schema,
		root:               wire.Root,
		rootIdentity:       wire.RootIdentity,
		agent:              wire.Agent,
		sourceGeneration:   wire.SourceGeneration,
		sourceTargetDigest: wire.SourceTargetDigest,
		sourceFloorDigest:  wire.SourceFloorDigest,
		bootID:             wire.BootID,
		agentDirDevice:     wire.AgentDirDevice,
		agentDirInode:      wire.AgentDirInode,
		inboxDirDevice:     wire.InboxDirDevice,
		inboxDirInode:      wire.InboxDirInode,
		hasOwner:           wire.HasOwner,
		ownerPID:           wire.OwnerPID,
		ownerProcessStart:  wire.OwnerProcessStart,
		ownerBootID:        wire.OwnerBootID,
		ownerSessionID:     wire.OwnerSessionID,
	}
	return source, source.validate()
}

func newWakeRepairHandoffPrepared(
	source wakeRepairHandoffSource,
	childPID int,
	childGeneration string,
	childTargetDigest string,
	childFloorDigest string,
	childFloorAuthority wakeRepairFloorAuthority,
) (wakeRepairHandoffPrepared, error) {
	sourceDigest, err := source.digest()
	if err != nil {
		return wakeRepairHandoffPrepared{}, err
	}
	prepared := wakeRepairHandoffPrepared{
		schema:              wakeRepairHandoffSchema,
		sourceDigest:        sourceDigest,
		childPID:            childPID,
		childGeneration:     childGeneration,
		childTargetDigest:   childTargetDigest,
		childFloorDigest:    childFloorDigest,
		childFloorAuthority: childFloorAuthority,
	}
	if prepared.childTargetDigest != source.sourceTargetDigest {
		return wakeRepairHandoffPrepared{}, fmt.Errorf("wake repair prepared target does not match exact source")
	}
	if prepared.childFloorAuthority.SourceFloorDigest != source.sourceFloorDigest {
		return wakeRepairHandoffPrepared{}, fmt.Errorf("wake repair prepared floor authority does not match exact source")
	}
	return prepared, prepared.validate()
}

func (prepared wakeRepairHandoffPrepared) ChildPID() int {
	return prepared.childPID
}

func (prepared wakeRepairHandoffPrepared) SourceDigest() string {
	return prepared.sourceDigest
}

func (prepared wakeRepairHandoffPrepared) ChildGeneration() string {
	return prepared.childGeneration
}

func (prepared wakeRepairHandoffPrepared) ChildTargetDigest() string {
	return prepared.childTargetDigest
}

func (prepared wakeRepairHandoffPrepared) ChildFloorDigest() string {
	return prepared.childFloorDigest
}

func (prepared wakeRepairHandoffPrepared) ChildFloorAuthority() wakeRepairFloorAuthority {
	return prepared.childFloorAuthority
}

func (prepared wakeRepairHandoffPrepared) digest() (string, error) {
	if err := prepared.validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(prepared.wire())
	if err != nil {
		return "", fmt.Errorf("marshal wake repair handoff prepared digest: %w", err)
	}
	return wakeMetadataDigest(data), nil
}

func (prepared wakeRepairHandoffPrepared) validate() error {
	if prepared.schema != wakeRepairHandoffSchema {
		return fmt.Errorf("wake repair handoff prepared schema %d unsupported", prepared.schema)
	}
	if err := validateWakeRepairHandoffDigest("source", prepared.sourceDigest); err != nil {
		return err
	}
	if prepared.childPID <= 0 {
		return fmt.Errorf("wake repair handoff prepared child pid is invalid")
	}
	if err := validateWakeRepairHandoffToken("child generation", prepared.childGeneration, 1024); err != nil {
		return err
	}
	if err := validateWakeRepairHandoffDigest("child target", prepared.childTargetDigest); err != nil {
		return err
	}
	if err := validateWakeRepairHandoffDigest("child floor", prepared.childFloorDigest); err != nil {
		return err
	}
	if err := prepared.childFloorAuthority.validate(); err != nil {
		return err
	}
	if prepared.childFloorAuthority.ChildGeneration != prepared.childGeneration {
		return fmt.Errorf("wake repair prepared floor authority generation mismatch")
	}
	return nil
}

func (prepared wakeRepairHandoffPrepared) validateSource(source wakeRepairHandoffSource) error {
	digest, err := source.digest()
	if err != nil {
		return err
	}
	if prepared.sourceDigest != digest {
		return fmt.Errorf("wake repair prepared message does not match exact source")
	}
	if prepared.childFloorAuthority.SourceFloorDigest != source.sourceFloorDigest {
		return fmt.Errorf("wake repair prepared floor authority does not match exact source")
	}
	return nil
}

func (prepared wakeRepairHandoffPrepared) wire() wakeRepairHandoffPreparedWire {
	return wakeRepairHandoffPreparedWire{
		Schema:                 prepared.schema,
		Kind:                   wakeRepairHandoffKindPrepared,
		SourceDigest:           prepared.sourceDigest,
		ChildPID:               prepared.childPID,
		ChildGeneration:        prepared.childGeneration,
		ChildTargetDigest:      prepared.childTargetDigest,
		ChildFloorDigest:       prepared.childFloorDigest,
		ChildFloorSourceDigest: prepared.childFloorAuthority.SourceFloorDigest,
		ChildFloorRawDigest:    prepared.childFloorAuthority.RawDigest,
		ChildFloorFileIdentity: prepared.childFloorAuthority.FileIdentity,
	}
}

func preparedFromWire(wire wakeRepairHandoffPreparedWire) (wakeRepairHandoffPrepared, error) {
	if wire.Kind != wakeRepairHandoffKindPrepared {
		return wakeRepairHandoffPrepared{}, fmt.Errorf("wake repair handoff message kind %q, want prepared", wire.Kind)
	}
	prepared := wakeRepairHandoffPrepared{
		schema:            wire.Schema,
		sourceDigest:      wire.SourceDigest,
		childPID:          wire.ChildPID,
		childGeneration:   wire.ChildGeneration,
		childTargetDigest: wire.ChildTargetDigest,
		childFloorDigest:  wire.ChildFloorDigest,
		childFloorAuthority: wakeRepairFloorAuthority{
			ChildGeneration:   wire.ChildGeneration,
			SourceFloorDigest: wire.ChildFloorSourceDigest,
			RawDigest:         wire.ChildFloorRawDigest,
			FileIdentity:      wire.ChildFloorFileIdentity,
		},
	}
	return prepared, prepared.validate()
}

func newWakeRepairHandoffAdmit(prepared wakeRepairHandoffPrepared) (wakeRepairHandoffAdmit, error) {
	digest, err := prepared.digest()
	if err != nil {
		return wakeRepairHandoffAdmit{}, err
	}
	admit := wakeRepairHandoffAdmit{
		schema:          wakeRepairHandoffSchema,
		childGeneration: prepared.childGeneration,
		preparedDigest:  digest,
	}
	return admit, admit.validate()
}

func (admit wakeRepairHandoffAdmit) validate() error {
	if admit.schema != wakeRepairHandoffSchema {
		return fmt.Errorf("wake repair handoff admit schema %d unsupported", admit.schema)
	}
	if err := validateWakeRepairHandoffToken("admit child generation", admit.childGeneration, 1024); err != nil {
		return err
	}
	return validateWakeRepairHandoffDigest("prepared", admit.preparedDigest)
}

func (admit wakeRepairHandoffAdmit) validatePrepared(prepared wakeRepairHandoffPrepared) error {
	expected, err := newWakeRepairHandoffAdmit(prepared)
	if err != nil {
		return err
	}
	if admit != expected {
		return fmt.Errorf("wake repair admit does not match exact prepared child")
	}
	return nil
}

func (admit wakeRepairHandoffAdmit) wire() wakeRepairHandoffAdmitWire {
	return wakeRepairHandoffAdmitWire{
		Schema:          admit.schema,
		Kind:            wakeRepairHandoffKindAdmit,
		ChildGeneration: admit.childGeneration,
		PreparedDigest:  admit.preparedDigest,
	}
}

func admitFromWire(wire wakeRepairHandoffAdmitWire) (wakeRepairHandoffAdmit, error) {
	if wire.Kind != wakeRepairHandoffKindAdmit {
		return wakeRepairHandoffAdmit{}, fmt.Errorf("wake repair handoff message kind %q, want admit", wire.Kind)
	}
	admit := wakeRepairHandoffAdmit{
		schema:          wire.Schema,
		childGeneration: wire.ChildGeneration,
		preparedDigest:  wire.PreparedDigest,
	}
	return admit, admit.validate()
}

func newWakeRepairHandoffRelease(admit wakeRepairHandoffAdmit) (wakeRepairHandoffRelease, error) {
	if err := admit.validate(); err != nil {
		return wakeRepairHandoffRelease{}, err
	}
	data, err := json.Marshal(admit.wire())
	if err != nil {
		return wakeRepairHandoffRelease{}, fmt.Errorf("marshal wake repair admit digest: %w", err)
	}
	release := wakeRepairHandoffRelease{
		schema:          wakeRepairHandoffSchema,
		childGeneration: admit.childGeneration,
		admitDigest:     wakeMetadataDigest(data),
	}
	return release, release.validate()
}

func (release wakeRepairHandoffRelease) validate() error {
	if release.schema != wakeRepairHandoffSchema {
		return fmt.Errorf("wake repair handoff release schema %d unsupported", release.schema)
	}
	if err := validateWakeRepairHandoffToken(
		"release child generation",
		release.childGeneration,
		1024,
	); err != nil {
		return err
	}
	return validateWakeRepairHandoffDigest("admit", release.admitDigest)
}

func (release wakeRepairHandoffRelease) validateAdmit(admit wakeRepairHandoffAdmit) error {
	expected, err := newWakeRepairHandoffRelease(admit)
	if err != nil {
		return err
	}
	if release != expected {
		return fmt.Errorf("wake repair release does not match exact admit")
	}
	return nil
}

func (release wakeRepairHandoffRelease) wire() wakeRepairHandoffReleaseWire {
	return wakeRepairHandoffReleaseWire{
		Schema:          release.schema,
		Kind:            wakeRepairHandoffKindRelease,
		ChildGeneration: release.childGeneration,
		AdmitDigest:     release.admitDigest,
	}
}

func releaseFromWire(wire wakeRepairHandoffReleaseWire) (wakeRepairHandoffRelease, error) {
	if wire.Kind != wakeRepairHandoffKindRelease {
		return wakeRepairHandoffRelease{}, fmt.Errorf(
			"wake repair handoff message kind %q, want release",
			wire.Kind,
		)
	}
	release := wakeRepairHandoffRelease{
		schema:          wire.Schema,
		childGeneration: wire.ChildGeneration,
		admitDigest:     wire.AdmitDigest,
	}
	return release, release.validate()
}

func validateWakeRepairHandoffToken(label, value string, maxBytes int) error {
	if value == "" || strings.TrimSpace(value) != value || strings.ContainsRune(value, 0) {
		return fmt.Errorf("wake repair handoff %s is invalid", label)
	}
	if len(value) > maxBytes {
		return fmt.Errorf("wake repair handoff %s is too long", label)
	}
	return nil
}

func validateWakeRepairHandoffDigest(label, value string) error {
	const prefix = "sha256:"
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+64 {
		return fmt.Errorf("wake repair handoff %s digest is invalid", label)
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(value, prefix)); err != nil {
		return fmt.Errorf("wake repair handoff %s digest is invalid", label)
	}
	return nil
}

type wakeRepairParentHandoff struct {
	reader *bufio.Reader
	writer io.Writer

	source     wakeRepairHandoffSource
	readFile   *os.File
	writeFile  *os.File
	childRead  *os.File
	childWrite *os.File
	childAgent *os.File
	childInbox *os.File
	stateMu    sync.Mutex
	admitted   wakeRepairHandoffAdmit
	hasAdmit   bool
	released   bool
	closeOnce  sync.Once
	closeErr   error
}

type wakeRepairChildHandoff struct {
	reader *bufio.Reader
	writer io.Writer

	readFile  *os.File
	writeFile *os.File
	agentFile *os.File
	inboxFile *os.File
	closeOnce sync.Once
	closeErr  error
}

func prepareWakeRepairHandoff(
	cmd *exec.Cmd,
	source wakeRepairHandoffSource,
	agentDir *wakeAgentDir,
	inboxDir *wakeInboxDir,
) (*wakeRepairParentHandoff, error) {
	if cmd == nil {
		return nil, fmt.Errorf("wake repair handoff command is missing")
	}
	if err := source.validate(); err != nil {
		return nil, err
	}
	if agentDir == nil || inboxDir == nil {
		return nil, fmt.Errorf("wake repair retained directory capability is missing")
	}
	agentIdentity, err := wakeRepairDirectoryIdentityForFile(agentDir.file)
	if err != nil {
		return nil, err
	}
	inboxIdentity, err := wakeRepairDirectoryIdentityForFile(inboxDir.file)
	if err != nil {
		return nil, err
	}
	if agentIdentity.device != source.agentDirDevice ||
		agentIdentity.inode != source.agentDirInode ||
		inboxIdentity.device != source.inboxDirDevice ||
		inboxIdentity.inode != source.inboxDirInode {
		return nil, fmt.Errorf("wake repair retained directory capability does not match exact source")
	}
	parentToChildRead, parentToChildWrite, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create wake repair source pipe: %w", err)
	}
	childToParentRead, childToParentWrite, err := os.Pipe()
	if err != nil {
		_ = parentToChildRead.Close()
		_ = parentToChildWrite.Close()
		return nil, fmt.Errorf("create wake repair response pipe: %w", err)
	}
	childAgent, err := duplicateWakeRepairDirectoryFile(agentDir.file, "wake-repair-agent-directory")
	if err != nil {
		_ = parentToChildRead.Close()
		_ = parentToChildWrite.Close()
		_ = childToParentRead.Close()
		_ = childToParentWrite.Close()
		return nil, err
	}
	childInbox, err := duplicateWakeRepairDirectoryFile(inboxDir.file, "wake-repair-inbox-directory")
	if err != nil {
		_ = parentToChildRead.Close()
		_ = parentToChildWrite.Close()
		_ = childToParentRead.Close()
		_ = childToParentWrite.Close()
		_ = childAgent.Close()
		return nil, err
	}
	baseFD := 3 + len(cmd.ExtraFiles)
	cmd.ExtraFiles = append(
		cmd.ExtraFiles,
		parentToChildRead,
		childToParentWrite,
		childAgent,
		childInbox,
	)
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = setEnvVar(unsetEnvVar(cmd.Env, envWakeRepairHandoffReadFD),
		envWakeRepairHandoffReadFD, strconv.Itoa(baseFD))
	cmd.Env = setEnvVar(unsetEnvVar(cmd.Env, envWakeRepairHandoffWriteFD),
		envWakeRepairHandoffWriteFD, strconv.Itoa(baseFD+1))
	cmd.Env = setEnvVar(unsetEnvVar(cmd.Env, envWakeRepairAgentDirFD),
		envWakeRepairAgentDirFD, strconv.Itoa(baseFD+2))
	cmd.Env = setEnvVar(unsetEnvVar(cmd.Env, envWakeRepairInboxDirFD),
		envWakeRepairInboxDirFD, strconv.Itoa(baseFD+3))
	return &wakeRepairParentHandoff{
		reader:     bufio.NewReader(childToParentRead),
		writer:     parentToChildWrite,
		source:     source,
		readFile:   childToParentRead,
		writeFile:  parentToChildWrite,
		childRead:  parentToChildRead,
		childWrite: childToParentWrite,
		childAgent: childAgent,
		childInbox: childInbox,
	}, nil
}

func newWakeRepairParentHandoffForFiles(
	writer *os.File,
	reader *os.File,
) *wakeRepairParentHandoff {
	return &wakeRepairParentHandoff{
		reader:    bufio.NewReader(reader),
		writer:    writer,
		readFile:  reader,
		writeFile: writer,
	}
}

func (handoff *wakeRepairParentHandoff) Bind(process *os.Process) error {
	if handoff == nil || process == nil || process.Pid <= 0 {
		return fmt.Errorf("wake repair handoff child process is missing")
	}
	if handoff.childRead != nil {
		if err := handoff.childRead.Close(); err != nil {
			return fmt.Errorf("close parent copy of wake repair child read fd: %w", err)
		}
		handoff.childRead = nil
	}
	if handoff.childWrite != nil {
		if err := handoff.childWrite.Close(); err != nil {
			return fmt.Errorf("close parent copy of wake repair child write fd: %w", err)
		}
		handoff.childWrite = nil
	}
	if handoff.childAgent != nil {
		if err := handoff.childAgent.Close(); err != nil {
			return fmt.Errorf("close parent copy of inherited wake agent directory: %w", err)
		}
		handoff.childAgent = nil
	}
	if handoff.childInbox != nil {
		if err := handoff.childInbox.Close(); err != nil {
			return fmt.Errorf("close parent copy of inherited wake inbox directory: %w", err)
		}
		handoff.childInbox = nil
	}
	return handoff.SendSource(handoff.source)
}

func (handoff *wakeRepairParentHandoff) SendSource(source wakeRepairHandoffSource) error {
	if handoff == nil || handoff.writer == nil {
		return fmt.Errorf("wake repair parent handoff is unavailable")
	}
	return writeWakeRepairHandoffSource(handoff.writer, source)
}

func (handoff *wakeRepairParentHandoff) ReceivePrepared(
	source wakeRepairHandoffSource,
) (wakeRepairHandoffPrepared, error) {
	if handoff == nil || handoff.reader == nil {
		return wakeRepairHandoffPrepared{}, fmt.Errorf("wake repair parent handoff is unavailable")
	}
	prepared, err := readWakeRepairHandoffPrepared(handoff.reader)
	if err != nil {
		return wakeRepairHandoffPrepared{}, err
	}
	if err := prepared.validateSource(source); err != nil {
		return wakeRepairHandoffPrepared{}, err
	}
	return prepared, nil
}

// Admit does not return until the child echoes the exact admit value. The
// caller may detach its stable child capability only after this succeeds.
func (handoff *wakeRepairParentHandoff) Admit(prepared wakeRepairHandoffPrepared) error {
	if handoff == nil || handoff.reader == nil || handoff.writer == nil ||
		handoff.readFile == nil || handoff.writeFile == nil {
		return fmt.Errorf("wake repair parent handoff is unavailable")
	}
	if wakeRepairAdmitTimeout <= 0 {
		return fmt.Errorf("wake repair admission timeout is invalid")
	}
	handoff.stateMu.Lock()
	defer handoff.stateMu.Unlock()
	if handoff.hasAdmit {
		return fmt.Errorf("wake repair admission was already acknowledged")
	}
	deadline := time.Now().Add(wakeRepairAdmitTimeout)
	if err := handoff.readFile.SetReadDeadline(deadline); err != nil {
		return fmt.Errorf("set wake repair admitted acknowledgement deadline: %w", err)
	}
	defer func() { _ = handoff.readFile.SetReadDeadline(time.Time{}) }()
	if err := handoff.writeFile.SetWriteDeadline(deadline); err != nil {
		return fmt.Errorf("set wake repair admit write deadline: %w", err)
	}
	defer func() { _ = handoff.writeFile.SetWriteDeadline(time.Time{}) }()
	admit, err := newWakeRepairHandoffAdmit(prepared)
	if err != nil {
		return err
	}
	if err := writeWakeRepairHandoffAdmit(handoff.writer, admit); err != nil {
		return err
	}
	ack, err := readWakeRepairHandoffAdmit(handoff.reader)
	if err != nil {
		return fmt.Errorf("read wake repair admitted acknowledgement: %w", err)
	}
	if ack != admit {
		return fmt.Errorf("wake repair admitted acknowledgement does not match exact admit")
	}
	handoff.admitted = admit
	handoff.hasAdmit = true
	return nil
}

// Release is the admission commit. The child remains blocked from scanning or
// injecting after its admit acknowledgement until this exact frame is written.
func (handoff *wakeRepairParentHandoff) Release(prepared wakeRepairHandoffPrepared) error {
	if handoff == nil || handoff.writer == nil || handoff.writeFile == nil {
		return fmt.Errorf("wake repair parent handoff is unavailable")
	}
	if wakeRepairAdmitTimeout <= 0 {
		return fmt.Errorf("wake repair admission timeout is invalid")
	}
	handoff.stateMu.Lock()
	defer handoff.stateMu.Unlock()
	if !handoff.hasAdmit {
		return fmt.Errorf("wake repair release requested before exact acknowledgement")
	}
	if handoff.released {
		return fmt.Errorf("wake repair child was already released")
	}
	admit, err := newWakeRepairHandoffAdmit(prepared)
	if err != nil {
		return err
	}
	if admit != handoff.admitted {
		return fmt.Errorf("wake repair release does not match exact acknowledged admit")
	}
	release, err := newWakeRepairHandoffRelease(admit)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(wakeRepairAdmitTimeout)
	if err := handoff.writeFile.SetWriteDeadline(deadline); err != nil {
		return fmt.Errorf("set wake repair release write deadline: %w", err)
	}
	defer func() { _ = handoff.writeFile.SetWriteDeadline(time.Time{}) }()
	if err := writeWakeRepairHandoffRelease(handoff.writer, release); err != nil {
		return fmt.Errorf("release admitted wake repair child: %w", err)
	}
	handoff.released = true
	return nil
}

func (handoff *wakeRepairParentHandoff) Close() error {
	if handoff == nil {
		return nil
	}
	handoff.closeOnce.Do(func() {
		handoff.closeErr = errors.Join(
			closeFile(handoff.writeFile),
			closeFile(handoff.readFile),
			closeFile(handoff.childRead),
			closeFile(handoff.childWrite),
			closeFile(handoff.childAgent),
			closeFile(handoff.childInbox),
		)
		handoff.writer = nil
		handoff.reader = nil
		handoff.writeFile = nil
		handoff.readFile = nil
		handoff.childRead = nil
		handoff.childWrite = nil
		handoff.childAgent = nil
		handoff.childInbox = nil
	})
	return handoff.closeErr
}

func wakeRepairChildHandoffFromEnv() (*wakeRepairChildHandoff, bool, error) {
	readRaw := strings.TrimSpace(os.Getenv(envWakeRepairHandoffReadFD))
	writeRaw := strings.TrimSpace(os.Getenv(envWakeRepairHandoffWriteFD))
	agentRaw := strings.TrimSpace(os.Getenv(envWakeRepairAgentDirFD))
	inboxRaw := strings.TrimSpace(os.Getenv(envWakeRepairInboxDirFD))
	if readRaw == "" && writeRaw == "" && agentRaw == "" && inboxRaw == "" {
		return nil, false, nil
	}
	if readRaw == "" || writeRaw == "" || agentRaw == "" || inboxRaw == "" {
		return nil, true, fmt.Errorf("wake repair handoff descriptors are incomplete")
	}
	readFD, err := parseWakeRepairHandoffFD(envWakeRepairHandoffReadFD, readRaw)
	if err != nil {
		return nil, true, err
	}
	writeFD, err := parseWakeRepairHandoffFD(envWakeRepairHandoffWriteFD, writeRaw)
	if err != nil {
		return nil, true, err
	}
	if readFD == writeFD {
		return nil, true, fmt.Errorf("wake repair handoff descriptors must be distinct")
	}
	agentFD, err := parseWakeRepairHandoffFD(envWakeRepairAgentDirFD, agentRaw)
	if err != nil {
		return nil, true, err
	}
	inboxFD, err := parseWakeRepairHandoffFD(envWakeRepairInboxDirFD, inboxRaw)
	if err != nil {
		return nil, true, err
	}
	if readFD == agentFD || readFD == inboxFD || writeFD == agentFD ||
		writeFD == inboxFD || agentFD == inboxFD {
		return nil, true, fmt.Errorf("wake repair handoff descriptors must be distinct")
	}
	inherited := []struct {
		label string
		fd    int
	}{
		{label: "read", fd: readFD},
		{label: "write", fd: writeFD},
		{label: "agent directory", fd: agentFD},
		{label: "inbox directory", fd: inboxFD},
	}
	for _, descriptor := range inherited {
		if err := setWakeRepairFDCloseOnExec(descriptor.fd, descriptor.label); err != nil {
			for _, candidate := range inherited {
				_ = unix.Close(candidate.fd)
			}
			return nil, true, err
		}
	}
	// os/exec obtains ExtraFiles through File.Fd, which restores blocking mode.
	// The child must remain cancellable while it waits for the final release
	// frame, so make its inherited read end pollable before os.NewFile adopts it.
	if err := unix.SetNonblock(readFD, true); err != nil {
		for _, candidate := range inherited {
			_ = unix.Close(candidate.fd)
		}
		return nil, true, fmt.Errorf("make wake repair handoff read fd nonblocking: %w", err)
	}
	readFile := os.NewFile(uintptr(readFD), "wake-repair-handoff-read")
	writeFile := os.NewFile(uintptr(writeFD), "wake-repair-handoff-write")
	agentFile := os.NewFile(uintptr(agentFD), "wake-repair-agent-directory")
	inboxFile := os.NewFile(uintptr(inboxFD), "wake-repair-inbox-directory")
	if readFile == nil || writeFile == nil || agentFile == nil || inboxFile == nil {
		_ = closeFile(readFile)
		_ = closeFile(writeFile)
		_ = closeFile(agentFile)
		_ = closeFile(inboxFile)
		return nil, true, fmt.Errorf("wake repair handoff descriptor is unavailable")
	}
	return &wakeRepairChildHandoff{
		reader:    bufio.NewReader(readFile),
		writer:    writeFile,
		readFile:  readFile,
		writeFile: writeFile,
		agentFile: agentFile,
		inboxFile: inboxFile,
	}, true, nil
}

func (handoff *wakeRepairChildHandoff) TakeRetainedDirectories(
	source wakeRepairHandoffSource,
) (*wakeAgentDir, *wakeInboxDir, error) {
	if handoff == nil || handoff.agentFile == nil || handoff.inboxFile == nil {
		return nil, nil, fmt.Errorf("wake repair inherited directory capability is unavailable")
	}
	agentFile := handoff.agentFile
	inboxFile := handoff.inboxFile
	handoff.agentFile = nil
	handoff.inboxFile = nil
	return openInheritedWakeRepairDirectories(agentFile, inboxFile, source)
}

func newWakeRepairChildHandoffForFiles(
	reader *os.File,
	writer *os.File,
) *wakeRepairChildHandoff {
	return &wakeRepairChildHandoff{
		reader:    bufio.NewReader(reader),
		writer:    writer,
		readFile:  reader,
		writeFile: writer,
	}
}

func (handoff *wakeRepairChildHandoff) ReceiveSource() (wakeRepairHandoffSource, error) {
	if handoff == nil || handoff.reader == nil {
		return wakeRepairHandoffSource{}, fmt.Errorf("wake repair child handoff is unavailable")
	}
	return readWakeRepairHandoffSource(handoff.reader)
}

func (handoff *wakeRepairChildHandoff) SendPrepared(prepared wakeRepairHandoffPrepared) error {
	if handoff == nil || handoff.writer == nil {
		return fmt.Errorf("wake repair child handoff is unavailable")
	}
	return writeWakeRepairHandoffPrepared(handoff.writer, prepared)
}

// AwaitAdmitAcknowledgeAndRelease is the child admission gate. A repaired wake
// must call it after publishing its prepared tuple and before scanning or
// injecting. Acknowledgement alone is never authorization: only the exact
// release frame commits admission.
func (handoff *wakeRepairChildHandoff) AwaitAdmitAcknowledgeAndRelease(
	prepared wakeRepairHandoffPrepared,
	beforeAcknowledge func() error,
) error {
	if handoff == nil || handoff.reader == nil || handoff.writer == nil ||
		handoff.readFile == nil {
		return fmt.Errorf("wake repair child handoff is unavailable")
	}
	admit, err := readWakeRepairHandoffAdmit(handoff.reader)
	if err != nil {
		return fmt.Errorf("wait for wake repair admission: %w", err)
	}
	if err := admit.validatePrepared(prepared); err != nil {
		return err
	}
	if beforeAcknowledge == nil {
		return fmt.Errorf("wake repair admission validation is missing")
	}
	if err := beforeAcknowledge(); err != nil {
		return fmt.Errorf("wake repair admission validation failed: %w", err)
	}
	if err := writeWakeRepairHandoffAdmit(handoff.writer, admit); err != nil {
		return fmt.Errorf("acknowledge wake repair admission: %w", err)
	}
	if wakeRepairAdmitTimeout <= 0 {
		return fmt.Errorf("wake repair release timeout is invalid")
	}
	deadline := time.Now().Add(wakeRepairAdmitTimeout)
	if err := handoff.readFile.SetReadDeadline(deadline); err != nil {
		return fmt.Errorf("set wake repair release deadline: %w", err)
	}
	defer func() { _ = handoff.readFile.SetReadDeadline(time.Time{}) }()
	release, err := readWakeRepairHandoffRelease(handoff.reader)
	if err != nil {
		return fmt.Errorf("wait for wake repair release: %w", err)
	}
	if err := release.validateAdmit(admit); err != nil {
		return err
	}
	if err := beforeAcknowledge(); err != nil {
		return fmt.Errorf("wake repair post-release admission validation failed: %w", err)
	}
	return nil
}

func (handoff *wakeRepairChildHandoff) Close() error {
	if handoff == nil {
		return nil
	}
	handoff.closeOnce.Do(func() {
		handoff.closeErr = errors.Join(
			closeFile(handoff.readFile),
			closeFile(handoff.writeFile),
			closeFile(handoff.agentFile),
			closeFile(handoff.inboxFile),
		)
		handoff.reader = nil
		handoff.writer = nil
		handoff.readFile = nil
		handoff.writeFile = nil
		handoff.agentFile = nil
		handoff.inboxFile = nil
	})
	return handoff.closeErr
}

func parseWakeRepairHandoffFD(label, raw string) (int, error) {
	fd, err := strconv.Atoi(raw)
	if err != nil || fd < 3 {
		return 0, fmt.Errorf("%s is invalid", label)
	}
	return fd, nil
}

func setWakeRepairFDCloseOnExec(fd int, label string) error {
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil {
		return fmt.Errorf("inspect wake repair %s fd: %w", label, err)
	}
	if flags&unix.FD_CLOEXEC != 0 {
		return nil
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, flags|unix.FD_CLOEXEC); err != nil {
		return fmt.Errorf("make wake repair %s fd close-on-exec: %w", label, err)
	}
	return nil
}

func writeWakeRepairHandoffSource(writer io.Writer, source wakeRepairHandoffSource) error {
	if err := source.validate(); err != nil {
		return err
	}
	return writeWakeRepairHandoffFrame(writer, source.wire())
}

func readWakeRepairHandoffSource(reader io.Reader) (wakeRepairHandoffSource, error) {
	var wire wakeRepairHandoffSourceWire
	if err := readWakeRepairHandoffFrame(reader, &wire); err != nil {
		return wakeRepairHandoffSource{}, err
	}
	return sourceFromWire(wire)
}

func writeWakeRepairHandoffPrepared(writer io.Writer, prepared wakeRepairHandoffPrepared) error {
	if err := prepared.validate(); err != nil {
		return err
	}
	return writeWakeRepairHandoffFrame(writer, prepared.wire())
}

func readWakeRepairHandoffPrepared(reader io.Reader) (wakeRepairHandoffPrepared, error) {
	var wire wakeRepairHandoffPreparedWire
	if err := readWakeRepairHandoffFrame(reader, &wire); err != nil {
		return wakeRepairHandoffPrepared{}, err
	}
	return preparedFromWire(wire)
}

func writeWakeRepairHandoffAdmit(writer io.Writer, admit wakeRepairHandoffAdmit) error {
	if err := admit.validate(); err != nil {
		return err
	}
	return writeWakeRepairHandoffFrame(writer, admit.wire())
}

func readWakeRepairHandoffAdmit(reader io.Reader) (wakeRepairHandoffAdmit, error) {
	var wire wakeRepairHandoffAdmitWire
	if err := readWakeRepairHandoffFrame(reader, &wire); err != nil {
		return wakeRepairHandoffAdmit{}, err
	}
	return admitFromWire(wire)
}

func writeWakeRepairHandoffRelease(writer io.Writer, release wakeRepairHandoffRelease) error {
	if err := release.validate(); err != nil {
		return err
	}
	return writeWakeRepairHandoffFrame(writer, release.wire())
}

func readWakeRepairHandoffRelease(reader io.Reader) (wakeRepairHandoffRelease, error) {
	var wire wakeRepairHandoffReleaseWire
	if err := readWakeRepairHandoffFrame(reader, &wire); err != nil {
		return wakeRepairHandoffRelease{}, err
	}
	return releaseFromWire(wire)
}

func writeWakeRepairHandoffFrame(writer io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal wake repair handoff message: %w", err)
	}
	data = append(data, '\n')
	if len(data) > wakeRepairHandoffMaxFrameBytes {
		return fmt.Errorf("wake repair handoff message is too large")
	}
	for len(data) > 0 {
		n, writeErr := writer.Write(data)
		if writeErr != nil {
			return fmt.Errorf("write wake repair handoff message: %w", writeErr)
		}
		if n <= 0 {
			return fmt.Errorf("write wake repair handoff message: %w", io.ErrShortWrite)
		}
		data = data[n:]
	}
	return nil
}

func readWakeRepairHandoffFrame(reader io.Reader, value any) error {
	buffered, ok := reader.(*bufio.Reader)
	if !ok {
		buffered = bufio.NewReader(reader)
	}
	var frame []byte
	for {
		part, err := buffered.ReadSlice('\n')
		frame = append(frame, part...)
		if len(frame) > wakeRepairHandoffMaxFrameBytes {
			return fmt.Errorf("wake repair handoff message is too large")
		}
		switch {
		case err == nil:
			goto decode
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			return fmt.Errorf("read wake repair handoff message: %w", io.ErrUnexpectedEOF)
		default:
			return fmt.Errorf("read wake repair handoff message: %w", err)
		}
	}

decode:
	decoder := json.NewDecoder(bytes.NewReader(frame))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("decode wake repair handoff message: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("decode wake repair handoff message: trailing value")
		}
		return fmt.Errorf("decode wake repair handoff message: %w", err)
	}
	return nil
}

func closeFile(file *os.File) error {
	if file == nil {
		return nil
	}
	return file.Close()
}

type wakeRepairChildCapability struct {
	bind   func(*os.Process) error
	stop   func() error
	detach func() error
	close  func() error
}

func (capability *wakeRepairChildCapability) Bind(process *os.Process) error {
	if capability == nil || capability.bind == nil {
		return fmt.Errorf("wake repair child capability cannot bind")
	}
	return capability.bind(process)
}

func (capability *wakeRepairChildCapability) Stop() error {
	if capability == nil || capability.stop == nil {
		return fmt.Errorf("wake repair child capability cannot stop")
	}
	return capability.stop()
}

// Detach relinquishes the stable child capability after the admitted
// acknowledgement. It must never be called merely because prepared was seen.
func (capability *wakeRepairChildCapability) Detach() error {
	if capability == nil || capability.detach == nil {
		return fmt.Errorf("wake repair child capability cannot detach")
	}
	return capability.detach()
}

func (capability *wakeRepairChildCapability) Close() error {
	if capability == nil || capability.close == nil {
		return nil
	}
	closeFn := capability.close
	capability.close = nil
	return closeFn()
}

var prepareWakeRepairChildCapability = prepareWakeRepairChildCapabilityPlatform
