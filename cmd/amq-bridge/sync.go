package main

import (
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/bridge"
	"github.com/avivsinai/agent-message-queue/internal/bridge/xferv3"
)

func runSync(args []string) error {
	fs := flag.NewFlagSet("amq-bridge sync", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	root := fs.String("root", os.Getenv("AM_ROOT"), "local AMQ root")
	peerName := fs.String("peer", "", "peer host")
	all := fs.Bool("all", false, "sync every pinned peer")
	stdio := fs.Bool("stdio", false, "use stdin/stdout as the pipe")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if strings.TrimSpace(*root) == "" {
		return fmt.Errorf("bridge root is required")
	}
	if *all && *peerName != "" {
		return usageError("--peer and --all cannot be combined")
	}
	if !*all && strings.TrimSpace(*peerName) == "" {
		return usageError("--peer or --all is required")
	}
	if !*stdio {
		return fmt.Errorf("sync needs --stdio until a pipe command is configured")
	}
	if *all {
		peers, err := xferv3.ListPeers(*root)
		if err != nil {
			return err
		}
		if len(peers) != 1 {
			return fmt.Errorf("stdio sync --all needs exactly one peer")
		}
		*peerName = peers[0].Host().String()
	}
	return syncStdio(*root, *peerName, true, os.Stdin, os.Stdout)
}

func runListen(args []string) error {
	fs := flag.NewFlagSet("amq-bridge listen", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	root := fs.String("root", os.Getenv("AM_ROOT"), "local AMQ root")
	peerName := fs.String("peer", "", "expected peer host")
	stdio := fs.Bool("stdio", false, "use stdin/stdout as the pipe")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if strings.TrimSpace(*root) == "" {
		return fmt.Errorf("bridge root is required")
	}
	if strings.TrimSpace(*peerName) == "" {
		return usageError("--peer is required")
	}
	if !*stdio {
		return fmt.Errorf("listen needs --stdio until a pipe command is configured")
	}
	return syncStdio(*root, *peerName, false, os.Stdin, os.Stdout)
}

func syncStdio(root, peerName string, initiator bool, in io.Reader, out io.Writer) error {
	state, err := loadHostState(root, peerName)
	if err != nil {
		return err
	}
	rw := struct {
		io.Reader
		io.Writer
	}{in, out}
	var sess *xferv3.Session
	if initiator {
		sess, err = xferv3.Dial(rw, state.Static, state.Snap.X25519, rand.Reader)
	} else {
		sess, err = xferv3.Accept(rw, state.Static, state.Snap.X25519, rand.Reader)
	}
	if err != nil {
		return err
	}
	return xferv3.Round(sess, state, initiator)
}

func loadHostState(root, peerName string) (xferv3.HostState, error) {
	hostID, err := bridge.LoadHostID(root)
	if err != nil {
		return xferv3.HostState{}, err
	}
	local, err := xferv3.ParseHost(hostID)
	if err != nil {
		return xferv3.HostState{}, err
	}
	peerHost, err := xferv3.ParseHost(peerName)
	if err != nil {
		return xferv3.HostState{}, err
	}
	peer, err := xferv3.LoadPeer(root, peerHost)
	if err != nil {
		return xferv3.HostState{}, err
	}
	key, err := bridge.LoadIdentity(root)
	if err != nil {
		return xferv3.HostState{}, err
	}
	static, err := xferv3.StaticFromSeed(key.Private.Seed())
	if err != nil {
		return xferv3.HostState{}, err
	}
	pub, _, err := bridge.LoadTrusted(root, peerName)
	if err != nil {
		return xferv3.HostState{}, err
	}
	return xferv3.HostState{
		Root:     root,
		Local:    local,
		LocalKey: key.Private,
		Static:   static,
		Snap: xferv3.Snapshot{
			Local:   local,
			Peer:    peerHost,
			PeerKey: pub,
			AllowIn: peer.AllowIn(),
			X25519:  peer.X25519Public(),
		},
	}, nil
}
