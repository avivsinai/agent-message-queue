package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/bridge"
)

func runIdentity(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("identity subcommand is required (init or public)")
	}
	switch args[0] {
	case "init":
		return runIdentityInit(args[1:])
	case "public":
		return runIdentityPublic(args[1:])
	default:
		return fmt.Errorf("unknown identity subcommand %q", args[0])
	}
}

func runIdentityInit(args []string) error {
	fs := flag.NewFlagSet("amq-bridge identity init", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	root := fs.String("root", os.Getenv("AM_ROOT"), "local AMQ root")
	generation := fs.String("generation", defaultKeyGeneration, "host key generation label")
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
	key, err := bridge.GenerateHostKey(*generation)
	if err != nil {
		return err
	}
	if err := bridge.WriteIdentity(*root, key); err != nil {
		return err
	}
	fmt.Printf("host=%s generation=%s public=%x\n", hostID, key.Generation, key.Public())
	return nil
}

func runIdentityPublic(args []string) error {
	fs := flag.NewFlagSet("amq-bridge identity public", flag.ContinueOnError)
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
	hostID, err := bridge.LoadHostID(*root)
	if err != nil {
		return err
	}
	key, err := bridge.LoadIdentity(*root)
	if err != nil {
		return err
	}
	fmt.Printf("host=%s generation=%s public=%x\n", hostID, key.Generation, key.Public())
	return nil
}
