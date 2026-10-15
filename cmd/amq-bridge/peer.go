package main

import (
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/bridge"
	"github.com/avivsinai/agent-message-queue/internal/bridge/xferv3"
)

func runPeer(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("peer subcommand is required (add, list, remove)")
	}
	switch args[0] {
	case "add":
		return runPeerAdd(args[1:])
	case "list":
		return runPeerList(args[1:])
	case "remove":
		return runPeerRemove(args[1:])
	default:
		return fmt.Errorf("unknown peer subcommand %q", args[0])
	}
}

func runPeerAdd(args []string) error {
	fs := flag.NewFlagSet("amq-bridge peer add", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	root := fs.String("root", os.Getenv("AM_ROOT"), "local AMQ root")
	allowIn := fs.String("allow-in", "", "comma-separated inbound handles")
	ticket := fs.String("ticket", "", "reachability ticket")
	x25519 := fs.String("x25519", "", "peer's 64-character lowercase X25519 public hex")
	from := fs.String("from", "", "file holding the peer's identity public output (default: stdin)")
	replace := fs.Bool("replace", false, "rotate an existing trusted host")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if strings.TrimSpace(*root) == "" {
		return fmt.Errorf("bridge root is required")
	}
	data, err := readTrustInput(*from, nil)
	if err != nil {
		return err
	}
	recordHost, generation, pub, err := bridge.ParsePublicIdentity(data)
	if err != nil {
		return err
	}
	if recordHost == "" {
		return fmt.Errorf("identity record carries no host field")
	}
	host, err := xferv3.ParseHost(recordHost)
	if err != nil {
		return err
	}
	raw, err := hex.DecodeString(*x25519)
	if err != nil || len(raw) != 32 || *x25519 != hex.EncodeToString(raw) {
		return fmt.Errorf("--x25519 must be 64 lowercase hex characters")
	}
	var xPublic [32]byte
	copy(xPublic[:], raw)
	var allow []xferv3.Handle
	if strings.TrimSpace(*allowIn) != "" {
		for _, name := range strings.Split(*allowIn, ",") {
			handle, err := xferv3.ParseHandle(strings.TrimSpace(name))
			if err != nil {
				return err
			}
			allow = append(allow, handle)
		}
	}
	peer, err := xferv3.NewPeer(host, allow, *ticket, xPublic)
	if err != nil {
		return err
	}
	if err := xferv3.StorePeer(*root, peer); err != nil {
		return err
	}
	if *replace {
		err = bridge.ReplaceTrusted(*root, recordHost, pub, generation)
	} else {
		err = bridge.WriteTrusted(*root, recordHost, pub, generation)
	}
	if err != nil {
		_ = xferv3.RemovePeer(*root, host)
		return err
	}
	fmt.Printf("peer host=%s allow_in=%s\n", host.String(), *allowIn)
	return nil
}

func runPeerList(args []string) error {
	fs := flag.NewFlagSet("amq-bridge peer list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	root := fs.String("root", os.Getenv("AM_ROOT"), "local AMQ root")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if strings.TrimSpace(*root) == "" {
		return fmt.Errorf("bridge root is required")
	}
	peers, err := xferv3.ListPeers(*root)
	if err != nil {
		return err
	}
	for _, peer := range peers {
		allow := make([]string, 0, len(peer.AllowIn()))
		for _, handle := range peer.AllowIn() {
			allow = append(allow, handle.String())
		}
		fmt.Printf("host=%s allow_in=%s\n", peer.Host().String(), strings.Join(allow, ","))
	}
	return nil
}

func runPeerRemove(args []string) error {
	fs := flag.NewFlagSet("amq-bridge peer remove", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	root := fs.String("root", os.Getenv("AM_ROOT"), "local AMQ root")
	hostFlag := fs.String("host", "", "peer host to remove")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if strings.TrimSpace(*root) == "" {
		return fmt.Errorf("bridge root is required")
	}
	host, err := xferv3.ParseHost(*hostFlag)
	if err != nil {
		return err
	}
	if err := xferv3.RemovePeer(*root, host); err != nil && !errors.Is(err, xferv3.ErrNoPeer) {
		return err
	}
	if err := bridge.DeleteTrusted(*root, host.String()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fmt.Printf("removed peer host=%s\n", host.String())
	return nil
}
