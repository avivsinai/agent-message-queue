package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/bridge"
)

// runTrust provisions a trusted source host on the destination root.
//
// Bead agent-message-queue-ug3: following the README verbatim failed —
// `amq-bridge identity public` prints one line ("host=.. generation=..
// public=..") while the trusted-file parser requires two lines
// ("generation <g>" and "public <hex>"), the README pointed at a
// per-generation directory the code never reads, and bridge.WriteTrusted
// had no production caller. `trust add` closes the loop: it accepts the
// identity-public output (piped, pasted, or via --from file) or the
// two-line key-file form, parses it against the same rules the loader
// uses, and writes the trusted file through bridge.WriteTrusted.
func runTrust(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("trust subcommand is required (add)")
	}
	switch args[0] {
	case "add":
		return runTrustAdd(args[1:], nil)
	default:
		return fmt.Errorf("unknown trust subcommand %q", args[0])
	}
}

func runTrustAdd(args []string, stdin io.Reader) error {
	fs := flag.NewFlagSet("amq-bridge trust add", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	root := fs.String("root", os.Getenv("AM_ROOT"), "local AMQ root")
	host := fs.String("host", "", "source host alias to trust")
	from := fs.String("from", "", "file holding the peer's identity public output or key record (default: stdin)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if strings.TrimSpace(*root) == "" {
		return fmt.Errorf("bridge root is required")
	}
	if strings.TrimSpace(*host) == "" {
		return fmt.Errorf("--host is required")
	}
	data, err := readTrustInput(*from, stdin)
	if err != nil {
		return err
	}
	generation, pub, err := bridge.ParsePublicIdentity(data)
	if err != nil {
		return fmt.Errorf("trusted host %s: %w", *host, err)
	}
	if err := bridge.WriteTrusted(*root, *host, pub, generation); err != nil {
		return err
	}
	fmt.Printf("trusted host=%s generation=%s\n", *host, generation)
	return nil
}

// readTrustInput reads the provisioning record from --from, stdin, or the
// injected reader (tests). The terminal check only applies to the real
// stdin: an interactive run with no --from and nothing piped is refused
// instead of hanging.
func readTrustInput(from string, stdin io.Reader) ([]byte, error) {
	if stdin != nil {
		return io.ReadAll(stdin)
	}
	if strings.TrimSpace(from) == "" {
		info, _ := os.Stdin.Stat()
		if info != nil && info.Mode()&os.ModeCharDevice != 0 {
			return nil, fmt.Errorf("no input: pass --from <file> or pipe the identity public output")
		}
		return io.ReadAll(bufio.NewReader(os.Stdin))
	}
	return os.ReadFile(from)
}
