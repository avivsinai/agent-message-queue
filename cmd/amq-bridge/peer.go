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
		return fmt.Errorf("peer subcommand is required (add, list, remove, line)")
	}
	switch args[0] {
	case "add":
		return runPeerAdd(args[1:])
	case "list":
		return runPeerList(args[1:])
	case "remove":
		return runPeerRemove(args[1:])
	case "line":
		return runPeerLine(args[1:])
	default:
		return fmt.Errorf("unknown peer subcommand %q", args[0])
	}
}

func runPeerAdd(args []string) error {
	fs := flag.NewFlagSet("amq-bridge peer add", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	root := fs.String("root", os.Getenv("AM_ROOT"), "local AMQ root")
	allowIn := fs.String("allow-in", "", "comma-separated inbound handles")
	ticketFlag := fs.String("ticket", "", "reachability ticket")
	x25519 := fs.String("x25519", "", "peer's 64-character lowercase X25519 public hex")
	from := fs.String("from", "", "file holding the peer's identity public output (default: stdin)")
	fromLine := fs.String("from-line", "", "pairing line from peer line")
	confirm := fs.String("confirm-fingerprint", "", "26-character fingerprint that must match before the pin is written")
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
	var (
		data     []byte
		xPublic  [32]byte
		ticket   = *ticketFlag
		lineFP   string
		lineHost string
	)
	if strings.TrimSpace(*fromLine) != "" {
		parsed, err := xferv3.ParseLine(*fromLine)
		if err != nil {
			return err
		}
		data = []byte(parsed.Record)
		xPublic = parsed.X25519
		if ticket == "" {
			ticket = parsed.Ticket
		}
		lineFP = parsed.Fingerprint()
		lineHost = parsed.Host.String()
	} else {
		var err error
		data, err = readTrustInput(*from, nil)
		if err != nil {
			return err
		}
		raw, err := hex.DecodeString(*x25519)
		if err != nil || len(raw) != 32 || *x25519 != hex.EncodeToString(raw) {
			return fmt.Errorf("--x25519 must be 64 lowercase hex characters")
		}
		copy(xPublic[:], raw)
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
	if lineFP == "" {
		encoded, encErr := xferv3.FormatLine(host, strings.TrimSpace(string(data)), ticket, xPublic)
		if encErr != nil {
			return encErr
		}
		parsed, parseErr := xferv3.ParseLine(encoded)
		if parseErr != nil {
			return parseErr
		}
		lineFP = parsed.Fingerprint()
	}
	if lineHost != "" && lineHost != recordHost {
		return fmt.Errorf("line host %s does not match identity host %s", lineHost, recordHost)
	}
	if strings.TrimSpace(*confirm) == "" || *confirm != lineFP {
		return fmt.Errorf("pin is not written until --confirm-fingerprint matches the line")
	}
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
	peer, err := xferv3.NewPeer(host, allow, ticket, xPublic)
	if err != nil {
		return err
	}
	trustPath := bridge.TrustedPath(*root, recordHost)
	oldTrust, _ := os.ReadFile(trustPath)
	wroteTrust := false
	if *replace {
		err = bridge.ReplaceTrusted(*root, recordHost, pub, generation)
	} else {
		err = bridge.WriteTrusted(*root, recordHost, pub, generation)
		wroteTrust = err == nil
	}
	if err != nil {
		return err
	}
	if err := xferv3.StorePeer(*root, peer); err != nil {
		if *replace && len(oldTrust) > 0 {
			_ = os.WriteFile(trustPath, oldTrust, 0o600)
		} else if wroteTrust {
			_ = bridge.DeleteTrusted(*root, recordHost)
		}
		return err
	}
	fmt.Printf("peer host=%s allow_in=%s fingerprint=%s\n", host.String(), *allowIn, lineFP)
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
	confirmHost := fs.String("confirm-host", "", "must match --host before the pin is deleted")
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
	if strings.TrimSpace(*confirmHost) == "" || *confirmHost != host.String() {
		return fmt.Errorf("pin is not removed until --confirm-host matches --host")
	}
	if err := xferv3.RemovePeer(*root, host); err != nil && !errors.Is(err, xferv3.ErrNoPeer) {
		return err
	}
	if err := bridge.DeleteTrusted(*root, host.String()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fmt.Printf("removed peer host=%s (v2 trust pin deleted)\n", host.String())
	return nil
}

func runPeerLine(args []string) error {
	fs := flag.NewFlagSet("amq-bridge peer line", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	root := fs.String("root", os.Getenv("AM_ROOT"), "local AMQ root")
	ticket := fs.String("ticket", "", "reachability ticket")
	fingerprint := fs.Bool("fingerprint", false, "print only the 26-character fingerprint")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if strings.TrimSpace(*root) == "" {
		return fmt.Errorf("bridge root is required")
	}
	hostID, err := bridge.LoadHostID(*root)
	if err != nil {
		return err
	}
	host, err := xferv3.ParseHost(hostID)
	if err != nil {
		return err
	}
	key, err := bridge.LoadIdentity(*root)
	if err != nil {
		return err
	}
	static, err := xferv3.StaticFromSeed(key.Private.Seed())
	if err != nil {
		return err
	}
	record := fmt.Sprintf("host=%s generation=%s public=%x", hostID, key.Generation, key.Public())
	line, err := xferv3.FormatLine(host, record, *ticket, static.Public())
	if err != nil {
		return err
	}
	parsed, err := xferv3.ParseLine(line)
	if err != nil {
		return err
	}
	if *fingerprint {
		fmt.Println(parsed.Fingerprint())
		return nil
	}
	fmt.Println(line)
	return nil
}
