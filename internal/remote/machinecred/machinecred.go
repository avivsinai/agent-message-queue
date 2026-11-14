// Package machinecred holds the credential of the AMQ Remote machine agent:
// a body key made on this machine and one NIP-OA grant the owner signs for
// it. The grant names no kind and ends at one `created_at<` bound, so the
// body acts as the owner's Buzz agent for every event kind until then.
//
// Layout, below the per-user AMQ home:
//
//	remote/machine/<agent_pubkey>/        0700
//	remote/machine/<agent_pubkey>/body.key 0600, written by bodykey.Mint
//	remote/machine/<agent_pubkey>/grant.json 0600, {"owner_pubkey","conditions","sig"}
//
// The owner key never reaches this package. The request text and the grant
// file the owner hands back hold public values only.
package machinecred

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/bodykey"
)

// ErrNoIdentity is returned when the agent has no key directory or key.
var ErrNoIdentity = errors.New("no machine agent key")

// ErrNoAuthority is returned when the agent key has no enrolled grant.
var ErrNoAuthority = errors.New("no owner grant enrolled for the machine agent")

const (
	keyFile   = "body.key"
	grantFile = "grant.json"
	// MaxNameLen bounds the agent name in runes.
	MaxNameLen = 64
	// MaxValidity is the longest window of a machine grant. The command that
	// asks for a grant and the command that signs one both hold it, so a
	// request file cannot ask the owner for more.
	MaxValidity = 365 * 24 * time.Hour
)

var pubkeyRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Credential is a loaded machine agent: its body key and the owner's grant.
// Load and Enroll check the grant's signature and shape, not the time; the
// caller checks Tag.Window at the time it signs.
type Credential struct {
	Key *bodykey.BodyKey
	Tag bodykey.AuthTag
}

// Grant is the file the owner hands back after signing a request.
type Grant struct {
	BodyPubKey  string `json:"body_pubkey"`
	OwnerPubKey string `json:"owner_pubkey"`
	Conditions  string `json:"conditions"`
	Sig         string `json:"sig"`
}

// EncodeGrant is the grant file content for a tag signed over body.
func EncodeGrant(body string, tag bodykey.AuthTag) ([]byte, error) {
	out, err := json.MarshalIndent(Grant{BodyPubKey: body, OwnerPubKey: tag.OwnerPubKey, Conditions: tag.Conditions, Sig: tag.SigHex()}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// storedGrant is grant.json: the grant without the body, which is the
// directory name.
type storedGrant struct {
	OwnerPubKey string `json:"owner_pubkey"`
	Conditions  string `json:"conditions"`
	Sig         string `json:"sig"`
}

// rel is the path of name below the AMQ home.
func rel(parts ...string) string {
	return filepath.Join(append([]string{"remote", "machine"}, parts...)...)
}

// machineRoot returns <home>/remote/machine after checking that each
// component below home is a plain directory. With create, a missing
// component is made 0700 and its parent synced; without, a missing
// component is os.ErrNotExist.
func machineRoot(home string, create bool) (string, error) {
	cur := home
	for _, part := range []string{"remote", "machine"} {
		parent := cur
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) && create {
			if err := os.Mkdir(cur, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return "", err
			}
			if err := fsq.SyncDir(parent); err != nil {
				return "", err
			}
			fi, err = os.Lstat(cur)
		}
		if err != nil {
			return "", err
		}
		if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
			return "", fmt.Errorf("%s is not a plain directory; refusing", cur)
		}
	}
	return cur, nil
}

// keyDir returns the agent's key directory, refusing a link or a file.
func keyDir(home, agent string) (string, error) {
	if !pubkeyRe.MatchString(agent) {
		return "", fmt.Errorf("agent pubkey %q is not 64 lowercase hex", agent)
	}
	root, err := machineRoot(home, false)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("%w: no key directory for agent %s", ErrNoIdentity, agent)
	}
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, agent)
	fi, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("%w: no key directory for agent %s", ErrNoIdentity, agent)
	}
	if err != nil {
		return "", err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return "", fmt.Errorf("%s is not a plain directory; refusing", dir)
	}
	return dir, nil
}

// Mint makes a new agent key and returns its public key. The key is made in
// a fresh private directory inside remote/machine and renamed to the public
// key, so a key directory always holds a whole key. An existing key
// directory is never replaced.
func Mint(home string) (string, error) {
	root, err := machineRoot(home, true)
	if err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(root, ".mint-")
	if err != nil {
		return "", err
	}
	renamed := false
	defer func() {
		if !renamed {
			_ = os.RemoveAll(tmp)
		}
	}()
	k, err := bodykey.Mint(tmp)
	if err != nil {
		return "", err
	}
	if err := fsq.SyncDir(tmp); err != nil {
		return "", err
	}
	pub := k.PublicKeyHex()
	final := filepath.Join(root, pub)
	if _, err := os.Lstat(final); err == nil {
		return "", fmt.Errorf("key directory %s already exists; refusing to replace it", final)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.Rename(tmp, final); err != nil {
		return "", err
	}
	renamed = true
	if err := fsq.SyncDir(root); err != nil {
		return "", err
	}
	return pub, nil
}

// Agents lists the agent public keys that have a key directory holding a
// key file, in name order. No machine directory is an empty list.
func Agents(home string) ([]string, error) {
	root, err := machineRoot(home, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if !pubkeyRe.MatchString(name) || e.Type()&os.ModeSymlink != 0 || !e.IsDir() {
			continue
		}
		fi, err := os.Lstat(filepath.Join(root, name, keyFile))
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		out = append(out, name)
	}
	return out, nil
}

// Request lines, in print order.
const (
	reqName     = "machine-agent"
	reqBody     = "body-pubkey"
	reqConds    = "machine conditions"
	reqPreimage = "machine preimage"
)

// Request is the text the owner signs with amq-owner-sign --machine.
func Request(pub, name string, notAfter int64) string {
	conds := bodykey.MachineConditions(notAfter)
	pre := hex.EncodeToString(bodykey.AuthTag{Conditions: conds}.Preimage(pub))
	return fmt.Sprintf("%s: %s\n%s: %s\n%s: %s\n%s: %s\n", reqName, name, reqBody, pub, reqConds, conds, reqPreimage, pre)
}

// CheckName refuses an agent name that is empty, longer than MaxNameLen
// runes, not valid UTF-8, has a character that is not printable, or has
// leading or trailing spaces.
func CheckName(name string) error {
	if name == "" {
		return errors.New("the agent name is empty")
	}
	if !utf8.ValidString(name) {
		return errors.New("the agent name is not valid UTF-8")
	}
	if n := utf8.RuneCountInString(name); n > MaxNameLen {
		return fmt.Errorf("the agent name has %d characters; at most %d", n, MaxNameLen)
	}
	for _, r := range name {
		if !unicode.IsPrint(r) {
			return fmt.Errorf("the agent name has a character that is not printable (%U)", r)
		}
	}
	if strings.TrimSpace(name) != name {
		return errors.New("the agent name starts or ends with a space")
	}
	return nil
}

// ParseRequest reads a request text. Every line must appear exactly once
// and no other line may appear; the preimage must equal the one the body
// and conditions give, and the conditions must have the machine shape.
func ParseRequest(text string) (name, body, conditions string, err error) {
	vals := map[string]string{}
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		key, val, ok := strings.Cut(line, ": ")
		switch key {
		case reqName, reqBody, reqConds, reqPreimage:
		default:
			ok = false
		}
		if !ok {
			return "", "", "", fmt.Errorf("unexpected line %q; pass the saved output of `amq-remote machine request`", line)
		}
		if _, dup := vals[key]; dup {
			if key == reqBody {
				return "", "", "", errors.New("more than one body pubkey")
			}
			return "", "", "", fmt.Errorf("the %q line appears more than once", key)
		}
		vals[key] = strings.TrimSpace(val)
	}
	for _, key := range []string{reqName, reqBody, reqConds, reqPreimage} {
		if _, ok := vals[key]; !ok {
			return "", "", "", fmt.Errorf("the request has no %q line", key)
		}
	}
	name, body, conditions = vals[reqName], vals[reqBody], vals[reqConds]
	if err := CheckName(name); err != nil {
		return "", "", "", err
	}
	if !pubkeyRe.MatchString(body) {
		return "", "", "", errors.New("the body pubkey is not 64 lowercase hex")
	}
	if _, err := machineEnd(conditions); err != nil {
		return "", "", "", err
	}
	want := hex.EncodeToString(bodykey.AuthTag{Conditions: conditions}.Preimage(body))
	if vals[reqPreimage] != want {
		return "", "", "", errors.New("the printed preimage does not match the body and conditions")
	}
	return name, body, conditions, nil
}

// machineEnd checks the machine shape, exactly `created_at<N` with no other
// clause, and returns N.
func machineEnd(conditions string) (int64, error) {
	conds, err := bodykey.ParseConditions(conditions)
	if err != nil {
		return 0, fmt.Errorf("conditions %q: %w", conditions, err)
	}
	if len(conds) != 1 || conds[0].CreatedLt == nil {
		return 0, fmt.Errorf("conditions %q are not a machine grant: want exactly created_at<N, with no kind", conditions)
	}
	end := int64(*conds[0].CreatedLt)
	if conditions != bodykey.MachineConditions(end) {
		return 0, fmt.Errorf("conditions %q are not a machine grant: want exactly created_at<N, with no kind", conditions)
	}
	return end, nil
}

// decodeStrict decodes exactly one JSON object with only known fields.
func decodeStrict(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("unexpected data after the grant object")
	}
	return nil
}

// checkTag builds the tag and checks its machine shape and its signature
// over body.
func checkTag(body, owner, conditions, sig string) (bodykey.AuthTag, error) {
	tag, err := bodykey.ParseAuthTag([]string{"auth", owner, conditions, sig})
	if err != nil {
		return bodykey.AuthTag{}, err
	}
	if _, err := machineEnd(conditions); err != nil {
		return bodykey.AuthTag{}, err
	}
	if err := tag.Verify(body); err != nil {
		return bodykey.AuthTag{}, fmt.Errorf("the owner signature does not verify for agent %s: %w", body, err)
	}
	return *tag, nil
}

// loadKey loads the agent's body key and checks it is the key its
// directory names.
func loadKey(home, agent string) (*bodykey.BodyKey, string, error) {
	dir, err := keyDir(home, agent)
	if err != nil {
		return nil, "", err
	}
	k, err := bodykey.Load(filepath.Join(dir, keyFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", fmt.Errorf("%w: %s has no %s", ErrNoIdentity, dir, keyFile)
	}
	if err != nil {
		return nil, "", err
	}
	if k.PublicKeyHex() != agent {
		return nil, "", fmt.Errorf("the key in %s is not the key its directory names; refusing", dir)
	}
	return k, dir, nil
}

// Enroll checks a signed grant file and stores it for its agent. The agent
// must have a key directory here, and the grant must have the machine shape
// and verify against the agent key. A later grant for the same agent
// replaces the stored one.
func Enroll(home string, grant []byte) (Credential, error) {
	var g Grant
	if err := decodeStrict(grant, &g); err != nil {
		return Credential{}, fmt.Errorf("grant file: %w", err)
	}
	if !pubkeyRe.MatchString(g.BodyPubKey) {
		return Credential{}, errors.New("grant file: body_pubkey is not 64 lowercase hex")
	}
	k, _, err := loadKey(home, g.BodyPubKey)
	if err != nil {
		return Credential{}, err
	}
	tag, err := checkTag(g.BodyPubKey, g.OwnerPubKey, g.Conditions, g.Sig)
	if err != nil {
		return Credential{}, err
	}
	raw, err := json.MarshalIndent(storedGrant{OwnerPubKey: tag.OwnerPubKey, Conditions: tag.Conditions, Sig: tag.SigHex()}, "", "  ")
	if err != nil {
		return Credential{}, err
	}
	if _, err := fsq.WriteStateFile(home, rel(g.BodyPubKey, grantFile), append(raw, '\n'), 0o600); err != nil {
		return Credential{}, err
	}
	return Credential{Key: k, Tag: tag}, nil
}

// Load reads the agent's key and grant and checks the grant's shape and
// signature. It does not check the time.
func Load(home, agent string) (Credential, error) {
	k, dir, err := loadKey(home, agent)
	if err != nil {
		return Credential{}, err
	}
	path := filepath.Join(dir, grantFile)
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return Credential{}, fmt.Errorf("%w: agent %s", ErrNoAuthority, agent)
	}
	if err != nil {
		return Credential{}, err
	}
	if !fi.Mode().IsRegular() {
		return Credential{}, fmt.Errorf("%s is not a regular file; refusing", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Credential{}, err
	}
	var g storedGrant
	if err := decodeStrict(raw, &g); err != nil {
		return Credential{}, fmt.Errorf("%s: %w", path, err)
	}
	tag, err := checkTag(agent, g.OwnerPubKey, g.Conditions, g.Sig)
	if err != nil {
		return Credential{}, fmt.Errorf("%s: %w", path, err)
	}
	return Credential{Key: k, Tag: tag}, nil
}
