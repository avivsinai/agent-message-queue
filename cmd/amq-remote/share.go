package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/bodykey"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// shareNotAfterDefault is the attestation window when --days is not given:
// the design bounds NIP-OA lifetime between 1 hour and 90 days; 30 days is
// the working default, renewed by `share --renew` before expiry (doctor
// warns 7 days ahead).
const shareNotAfterDefault = 30 * 24 * time.Hour

// shareTagFile stores the owner-signed NIP-OA tag next to the body key so
// serve can present it on NIP-42 AUTH. Shape:
//
//	{"owner_pubkey":"...","conditions":"...","sig":"..."}
//
// The body secret never appears here or in argv; the owner signs the
// printed preimage with their own Buzz identity offline and hands the tag
// back through --tag-file.
type shareTagFile struct {
	OwnerPubKey string `json:"owner_pubkey"`
	Conditions  string `json:"conditions"`
	Sig         string `json:"sig"`
}

// shareKeyDir returns <root>/extensions/remote/keys/<session> — the
// remote-owned key location the design pins (§7.5: the bridge reads the
// path via config flag, never writes it).
func shareKeyDir(root, session string) string {
	return filepath.Join(root, stateDirName, "keys", session)
}

func share(args []string, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("share", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	root := fs.String("root", os.Getenv("AM_ROOT"), "AMQ root directory (default AM_ROOT)")
	session := fs.String("session", "", "shared session id (required)")
	renew := fs.Bool("renew", false, "re-print the preimage for a fresh attestation window")
	days := fs.Int("days", 0, "attestation window in days (default 30; 1..90)")
	tagFile := fs.String("tag-file", "", "JSON file with the owner-signed tag {owner_pubkey,conditions,sig}")
	dryRun := fs.Bool("dry-run", false, "print the preimage without writing or changing anything")
	if err := fs.Parse(args); err != nil {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "%v", err)
	}
	if *session == "" {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "--session is required")
	}
	if *root == "" {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "--root or AM_ROOT is required")
	}
	if !filepath.IsAbs(*root) {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "--root must be absolute")
	}
	if *days < 0 || *days > 90 {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "--days must be 1..90 (NIP-OA window bounds)")
	}
	if *days == 0 {
		*days = 30
	}
	if *days < 1 {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "--days must be at least 1")
	}

	keyDir := shareKeyDir(*root, *session)
	notAfter := time.Now().Add(time.Duration(*days) * 24 * time.Hour)
	conditions := bodykey.ShareConditions(notAfter.Unix())

	// Load-or-mint the body keypair. Mint is refused if a key already
	// exists; renewal and tag enrollment reuse the existing key.
	k, err := bodykey.LoadOrMint(keyDir)
	if err != nil {
		return protocol.ExitActionRequired, fmt.Errorf("body key at %s: %w", keyDir, err)
	}
	preimage := k.PreimageHex(conditions)

	if *dryRun {
		printShare(stdout, *session, k.PublicKeyHex(), conditions, preimage, notAfter)
		return 0, nil
	}

	if *tagFile != "" {
		return enrollTag(keyDir, *tagFile, k, conditions)
	}

	// Mint or renew: persist the pending window so doctor can warn before
	// expiry, and print the preimage for the owner to sign.
	if !*renew {
		if _, statErr := os.Stat(filepath.Join(keyDir, "share.json")); statErr == nil {
			// A tag is already enrolled; plain `share` does not disturb it.
			// The operator asked for the preimage — print it with the
			// EXISTING enrolled conditions instead of a new window.
			enrolled, rerr := readShareTag(keyDir)
			if rerr != nil {
				return protocol.ExitActionRequired, rerr
			}
			printShare(stdout, *session, k.PublicKeyHex(), enrolled.Conditions, k.PreimageHex(enrolled.Conditions), notAfter)
			return 0, nil
		}
	}
	pending := shareTagFile{Conditions: conditions}
	if err := writeSharePending(keyDir, &pending, notAfter); err != nil {
		return protocol.ExitActionRequired, err
	}
	printShare(stdout, *session, k.PublicKeyHex(), conditions, preimage, notAfter)
	say(stderr, "sign the preimage with the owner's Buzz identity, then run:\n  amq-remote share --root %s --session %s --tag-file <tag.json>", *root, *session)
	return 0, nil
}

// enrollTag validates an owner-signed tag against this session's body key
// and the pending conditions, then persists it for serve to present on
// NIP-42 AUTH.
func enrollTag(keyDir, tagPath string, k *bodykey.BodyKey, pendingConditions string) (int, error) {
	raw, err := os.ReadFile(tagPath)
	if err != nil {
		return protocol.ExitUsage, fmt.Errorf("--tag-file: %w", err)
	}
	var tf shareTagFile
	if err := json.Unmarshal(raw, &tf); err != nil {
		return protocol.ExitUsage, fmt.Errorf("--tag-file: %w", err)
	}
	// If a pending window exists, the tag must be for it (no preimage
	// desync between what was printed and what was signed).
	if pending, err := readSharePending(keyDir); err == nil && pending != nil && pending.Conditions != "" {
		if tf.Conditions != pending.Conditions {
			return protocol.ExitActionRequired, fmt.Errorf("tag conditions %q do not match the pending window %q; rerun `share --renew` to reprint", tf.Conditions, pending.Conditions)
		}
	}
	tag := bodykey.AuthTag{OwnerPubKey: tf.OwnerPubKey, Conditions: tf.Conditions}
	if err := tag.SetSigHex(tf.Sig); err != nil {
		return protocol.ExitUsage, err
	}
	if err := tag.Verify(k.PublicKeyHex()); err != nil {
		return protocol.ExitActionRequired, fmt.Errorf("owner tag rejected: %w", err)
	}
	if err := writeShareTag(keyDir, &tf); err != nil {
		return protocol.ExitActionRequired, err
	}
	_ = pendingConditions
	return 0, nil
}

func sharePaths(keyDir string) (pending, enrolled string) {
	return filepath.Join(keyDir, "share.pending.json"), filepath.Join(keyDir, "share.json")
}

func readSharePending(keyDir string) (*shareTagFile, error) {
	p, _ := sharePaths(keyDir)
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var tf shareTagFile
	if err := json.Unmarshal(raw, &tf); err != nil {
		return nil, err
	}
	return &tf, nil
}

func readShareTag(keyDir string) (*shareTagFile, error) {
	p, _ := sharePaths(keyDir)
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var tf shareTagFile
	if err := json.Unmarshal(raw, &tf); err != nil {
		return nil, err
	}
	return &tf, nil
}

func writeSharePending(keyDir string, tf *shareTagFile, notAfter time.Time) error {
	p, _ := sharePaths(keyDir)
	doc := struct {
		shareTagFile
		NotAfter int64 `json:"not_after"`
	}{shareTagFile: *tf, NotAfter: notAfter.Unix()}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, raw, 0o600)
}

func writeShareTag(keyDir string, tf *shareTagFile) error {
	_, enrolled := sharePaths(keyDir)
	raw, err := json.MarshalIndent(tf, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(enrolled, raw, 0o600); err != nil {
		return err
	}
	// Enrollment completes the flow: the pending window is consumed.
	p, _ := sharePaths(keyDir)
	_ = os.Remove(p)
	return nil
}

func printShare(w io.Writer, session, bodyPub, conditions, preimage string, notAfter time.Time) {
	say(w, "session:     %s", session)
	say(w, "body-pubkey: %s", bodyPub)
	say(w, "conditions:  %s", conditions)
	say(w, "preimage:    %s", preimage)
	say(w, "expires:     %s", notAfter.UTC().Format(time.RFC3339))
}

// doctorShareInspection is the doctor surface for body identity: it reports
// mint/enrolled/expiry state per session key dir and warns 7 days before
// the enrolled (or pending) window expires.
func doctorShareInspection(root string) map[string]any {
	keysRoot := filepath.Join(root, stateDirName, "keys")
	entries, err := os.ReadDir(keysRoot)
	if err != nil {
		return nil
	}
	out := map[string]any{}
	const warnHorizon = 7 * 24 * time.Hour
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		keyDir := filepath.Join(keysRoot, e.Name())
		info := map[string]any{}
		if k, err := bodykey.Load(filepath.Join(keyDir, "body.key")); err == nil {
			info["body_pubkey"] = k.PublicKeyHex()
		} else {
			info["key_error"] = err.Error()
			out[e.Name()] = info
			continue
		}
		pendingPath, enrolledPath := sharePaths(keyDir)
		raw, rerr := os.ReadFile(enrolledPath)
		source := "enrolled"
		if rerr != nil {
			raw, rerr = os.ReadFile(pendingPath)
			source = "pending"
		}
		if rerr != nil {
			info["attestation"] = "missing: run `amq-remote share --session " + e.Name() + "`"
			out[e.Name()] = info
			continue
		}
		var doc struct {
			shareTagFile
			NotAfter int64 `json:"not_after"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			info["attestation_error"] = err.Error()
			out[e.Name()] = info
			continue
		}
		notAfter := time.Unix(doc.NotAfter, 0)
		info["attestation"] = source
		info["owner_pubkey"] = doc.OwnerPubKey
		info["expires"] = notAfter.UTC().Format(time.RFC3339)
		remaining := time.Until(notAfter)
		switch {
		case remaining <= 0:
			info["warning"] = "expired: run `amq-remote share --session " + e.Name() + " --renew`"
		case remaining < warnHorizon:
			info["warning"] = fmt.Sprintf("expires in %s: run `amq-remote share --session %s --renew`", remaining.Round(time.Hour), e.Name())
		}
		out[e.Name()] = info
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
