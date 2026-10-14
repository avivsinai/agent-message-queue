package xferv3

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	MaxTicketBytes = 2048
	peersDirName   = "peers"
	txDirName      = "tx"
)

var (
	ErrNoPeer = fmt.Errorf("xferv3: peer file is absent")
	ErrTxDiff = fmt.Errorf("xferv3: tx file has different bytes")
)

type Peer struct {
	host    Host
	allow   []Handle
	ticket  string
	xPublic [32]byte
}

type peerFile struct {
	AllowIn      []string `json:"allow_in"`
	Ticket       string   `json:"ticket"`
	X25519Public string   `json:"x25519_public"`
}

func NewPeer(host Host, allow []Handle, ticket string, xPublic [32]byte) (Peer, error) {
	if host.s == "" {
		return Peer{}, ErrName
	}
	if strings.ContainsRune(ticket, 0) || len(ticket) > MaxTicketBytes {
		return Peer{}, ErrLimit
	}
	seen := make(map[Handle]struct{}, len(allow))
	copied := make([]Handle, 0, len(allow))
	for _, handle := range allow {
		if handle.s == "" {
			return Peer{}, ErrName
		}
		if _, ok := seen[handle]; ok {
			return Peer{}, ErrName
		}
		seen[handle] = struct{}{}
		copied = append(copied, handle)
	}
	return Peer{host: host, allow: copied, ticket: ticket, xPublic: xPublic}, nil
}

func (p Peer) Host() Host             { return p.host }
func (p Peer) Ticket() string         { return p.ticket }
func (p Peer) X25519Public() [32]byte { return p.xPublic }
func (p Peer) AllowIn() []Handle      { return append([]Handle(nil), p.allow...) }

func LoadPeer(root string, host Host) (Peer, error) {
	if host.s == "" {
		return Peer{}, ErrName
	}
	path := peerPath(root, host)
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Peer{}, ErrNoPeer
		}
		return Peer{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return Peer{}, fmt.Errorf("%w: peer %s is a symlink", ErrName, host)
	}
	file, err := os.Open(path)
	if err != nil {
		return Peer{}, err
	}
	defer func() { _ = file.Close() }()
	dec := json.NewDecoder(file)
	dec.DisallowUnknownFields()
	var wire peerFile
	if err := dec.Decode(&wire); err != nil {
		return Peer{}, fmt.Errorf("%w: peer %s: %v", ErrName, host, err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return Peer{}, fmt.Errorf("%w: peer %s has trailing JSON", ErrName, host)
	}
	allow := make([]Handle, 0, len(wire.AllowIn))
	for _, name := range wire.AllowIn {
		handle, err := ParseHandle(name)
		if err != nil {
			return Peer{}, err
		}
		allow = append(allow, handle)
	}
	raw, err := hex.DecodeString(wire.X25519Public)
	if err != nil || len(raw) != 32 || wire.X25519Public != hex.EncodeToString(raw) {
		return Peer{}, fmt.Errorf("%w: peer %s x25519_public", ErrName, host)
	}
	var xPublic [32]byte
	copy(xPublic[:], raw)
	return NewPeer(host, allow, wire.Ticket, xPublic)
}

func StorePeer(root string, peer Peer) error {
	if peer.host.s == "" {
		return ErrName
	}
	dir := filepath.Join(root, "bridge", peersDirName)
	allow := make([]string, 0, len(peer.allow))
	for _, handle := range peer.allow {
		allow = append(allow, handle.s)
	}
	body, err := json.Marshal(peerFile{
		AllowIn:      allow,
		Ticket:       peer.ticket,
		X25519Public: hex.EncodeToString(peer.xPublic[:]),
	})
	if err != nil {
		return err
	}
	body = append(body, '\n')
	return commitBytes(dir, peer.host.s+".json", body, true)
}

func RemovePeer(root string, host Host) error {
	if host.s == "" {
		return ErrName
	}
	path := peerPath(root, host)
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ErrNoPeer
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: peer %s is a symlink", ErrName, host)
	}
	return os.Remove(path)
}

func ListPeers(root string) ([]Peer, error) {
	dir := filepath.Join(root, "bridge", peersDirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var peers []Peer
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}
		host, err := ParseHost(strings.TrimSuffix(name, ".json"))
		if err != nil {
			return nil, err
		}
		peer, err := LoadPeer(root, host)
		if err != nil {
			return nil, err
		}
		peers = append(peers, peer)
	}
	return peers, nil
}

func peerPath(root string, host Host) string {
	return filepath.Join(root, "bridge", peersDirName, host.s+".json")
}

func commitBytes(dir, name string, body []byte, replace bool) error {
	if name == "" || name != filepath.Base(name) || name == "." || name == ".." {
		return ErrName
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	dest := filepath.Join(dir, name)
	if !replace {
		if err := os.Link(tmpName, dest); err != nil {
			if !os.IsExist(err) {
				_ = os.Remove(tmpName)
				return err
			}
			existing, readErr := os.ReadFile(dest)
			_ = os.Remove(tmpName)
			if readErr != nil {
				return readErr
			}
			if bytes.Equal(existing, body) {
				return nil
			}
			return ErrTxDiff
		}
	} else if err := os.Rename(tmpName, dest); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	_ = os.Remove(tmpName)
	dirFile, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = dirFile.Close() }()
	return dirFile.Sync()
}
