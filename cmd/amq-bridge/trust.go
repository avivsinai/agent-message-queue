package main

import (
	"bufio"
	"bytes"
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
	host := fs.String("host", "", "source host alias to trust (defaults to the record's host field)")
	from := fs.String("from", "", "file holding the peer's identity public output or key record (default: stdin)")
	replace := fs.Bool("replace", false, "rotate an existing trusted host: overwrite atomically, refusing a generation downgrade")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if strings.TrimSpace(*root) == "" {
		return fmt.Errorf("bridge root is required")
	}
	data, err := readTrustInput(*from, stdin)
	if err != nil {
		return err
	}
	recordHost, generation, pub, err := bridge.ParsePublicIdentity(data)
	if err != nil {
		return fmt.Errorf("trusted host %s: %w", strings.TrimSpace(*host), err)
	}
	// The record's host field is the sender's bridge/host-id — the exact
	// name apply-file looks the trust file up by. Default to it and refuse
	// an explicit --host that disagrees (review-845-r1 P2-1: anything else
	// wrote a dead trust record with rc=0). The two-line shape carries no
	// host, so there --host is still required.
	effectiveHost := strings.TrimSpace(*host)
	if recordHost != "" {
		if effectiveHost != "" && effectiveHost != recordHost {
			return fmt.Errorf("--host %q does not match the record's host %q; the trust file is looked up by the record's host", effectiveHost, recordHost)
		}
		effectiveHost = recordHost
	}
	if effectiveHost == "" {
		return fmt.Errorf("--host is required: the record carries no host field")
	}
	if *replace {
		err = bridge.ReplaceTrusted(*root, effectiveHost, pub, generation)
	} else {
		err = bridge.WriteTrusted(*root, effectiveHost, pub, generation)
	}
	if err != nil {
		return err
	}
	fmt.Printf("trusted host=%s generation=%s\n", effectiveHost, generation)
	return nil
}

// readTrustInput reads the provisioning record from --from, stdin, or the
// injected reader (tests). The terminal check only applies to the real
// stdin: an interactive run with no --from and nothing piped is refused
// instead of hanging, and empty input is the same no-input case
// (review-845-r1 P2-3) rather than a doubled parse error.
func readTrustInput(from string, stdin io.Reader) ([]byte, error) {
	var data []byte
	var err error
	if stdin != nil {
		return io.ReadAll(stdin)
	}
	if strings.TrimSpace(from) == "" {
		info, _ := os.Stdin.Stat()
		if info != nil && info.Mode()&os.ModeCharDevice != 0 {
			return nil, fmt.Errorf("no input: pass --from <file> or pipe the identity public output")
		}
		data, err = io.ReadAll(bufio.NewReader(os.Stdin))
	} else {
		data, err = os.ReadFile(from)
	}
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("no input: pass --from <file> or pipe the identity public output")
	}
	return data, nil
}
