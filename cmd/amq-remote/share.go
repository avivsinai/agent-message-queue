package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/bodykey"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// shareDefaultDays is the attestation window when --days is not given. The
// design bounds NIP-OA lifetime between 1 hour and 90 days; 30 days is the
// working default, renewed by `share --renew` before expiry (doctor warns
// 7 days ahead).
const shareDefaultDays = 30

// shareTagFile stores ONE owner-signed NIP-OA tag (one kind) next to the
// body key so serve can present it on NIP-42 AUTH. Shape:
//
//	{"kind":20003,"owner_pubkey":"...","conditions":"...","sig":"..."}
//
// The body secret never appears here or in argv; the owner signs the
// printed preimage with their own Buzz identity offline and hands the tag
// back through --tag-file.
type shareTagFile struct {
	Kind        uint16 `json:"kind"`
	OwnerPubKey string `json:"owner_pubkey"`
	Conditions  string `json:"conditions"`
	Sig         string `json:"sig"`
}

// shareKeyDir returns <root>/extensions/remote/keys/<session> — the
// remote-owned key location the design pins (§7.5: the bridge reads the
// path via config flag, never writes it). The session id must be a single
// safe path component; traversal or escaping is refused (codex P1: an
// unchecked session wrote body.key outside --root).
func shareKeyDir(root, session string) (string, error) {
	if session == "" || session == "." || session == ".." ||
		strings.ContainsAny(session, "/\\") || strings.ContainsRune(session, 0) {
		return "", protocol.Refuse(protocol.CodeInvalid, "--session must be one path component, got %q", session)
	}
	dir := filepath.Join(root, stateDirName, "keys", filepath.Clean("/" + session)[1:])
	// Defense in depth: the resolved dir must stay inside the keys root.
	keysRoot := filepath.Join(root, stateDirName, "keys")
	resolved, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	rootResolved, err := filepath.Abs(keysRoot)
	if err != nil {
		return "", err
	}
	if resolved != rootResolved && !strings.HasPrefix(resolved, rootResolved+string(filepath.Separator)) {
		return "", protocol.Refuse(protocol.CodeInvalid, "--session %q escapes the key directory", session)
	}
	return dir, nil
}

// validateKeyDirSymlinks walks each component of dir below the keys root
// and refuses any symlink (codex P1: a symlinked key path could place
// body.key outside the root). Absent components are fine — Mint creates
// them as real directories.
func validateKeyDirSymlinks(keysRoot, dir string) error {
	rel, err := filepath.Rel(keysRoot, dir)
	if err != nil {
		return err
	}
	cur := keysRoot
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return protocol.Refuse(protocol.CodeInvalid, "key path component %s is a symlink; refusing", cur)
		}
		if !fi.IsDir() {
			return protocol.Refuse(protocol.CodeInvalid, "key path component %s is not a directory", cur)
		}
	}
	return nil
}

func share(args []string, stdout, stderr io.Writer) (int, error) {
	fs := flag.NewFlagSet("share", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	root := fs.String("root", os.Getenv("AM_ROOT"), "AMQ root directory (default AM_ROOT)")
	session := fs.String("session", "", "shared session id (required)")
	renew := fs.Bool("renew", false, "re-print preimages for a fresh attestation window (all kinds)")
	days := fs.Int("days", 0, "attestation window in days (default 30; 1..90)")
	tagFile := fs.String("tag-file", "", "JSON file with one owner-signed tag {kind,owner_pubkey,conditions,sig}")
	dryRun := fs.Bool("dry-run", false, "print preimages without writing or changing anything")
	if err := fs.Parse(args); err != nil {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "%v", err)
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
		*days = shareDefaultDays
	}

	keyDir, err := shareKeyDir(*root, *session)
	if err != nil {
		return protocol.ExitUsage, err
	}
	keysRoot := filepath.Join(*root, stateDirName, "keys")
	if err := validateKeyDirSymlinks(keysRoot, keyDir); err != nil {
		return protocol.ExitActionRequired, err
	}

	// --dry-run NEVER mutates state: if a key exists report it, otherwise
	// report what mint WOULD do, without writing (codex P2: dry-run minted
	// body.key before returning).
	keyPath := filepath.Join(keyDir, "body.key")
	k, loadErr := bodykey.Load(keyPath)
	if *dryRun {
		if loadErr != nil {
			say(stdout, "dry-run: no body key at %s — a real run would mint one", keyPath)
			return 0, nil
		}
		printShareDryRun(stdout, *session, k, *days)
		return 0, nil
	}

	// Load-or-mint the body keypair. Mint is refused if a key already
	// exists; renewal and tag enrollment reuse the existing key.
	if loadErr != nil {
		if !errors.Is(loadErr, os.ErrNotExist) && !errors.Is(loadErr, bodykey.ErrWrongFormat) {
			return protocol.ExitActionRequired, fmt.Errorf("body key at %s: %w", keyPath, loadErr)
		}
		if k, err = bodykey.Mint(keyDir); err != nil {
			return protocol.ExitActionRequired, fmt.Errorf("body key at %s: %w", keyPath, err)
		}
	}

	if *tagFile != "" {
		return enrollTag(keyDir, *tagFile, k)
	}

	// Mint or renew: persist one pending window per kind so doctor can warn
	// before expiry, and print the preimages for the owner to sign.
	notAfter := time.Now().Add(time.Duration(*days) * 24 * time.Hour)
	if !*renew {
		if enrolled := readEnrolledTags(keyDir); len(enrolled) > 0 {
			// Tags are already enrolled; plain `share` does not disturb
			// them. Print the ENROLLED state (expiry derived from the
			// signed conditions — codex P1: pending/enrolled mixing).
			printShareEnrolled(stdout, *session, k, enrolled)
			return 0, nil
		}
	}
	pending := make([]shareTagFile, 0, len(bodykey.ShareKinds))
	for _, kind := range bodykey.ShareKinds {
		pending = append(pending, shareTagFile{Kind: kind, Conditions: bodykey.ShareConditions(kind, notAfter.Unix())})
	}
	if err := writeSharePending(keyDir, pending, notAfter); err != nil {
		return protocol.ExitActionRequired, err
	}
	printSharePending(stdout, *session, k, pending, notAfter)
	say(stderr, "sign each preimage with the owner's Buzz identity, then enroll one per kind:\n  amq-remote share --root %s --session %s --tag-file <tag.json> (repeat per kind)", *root, *session)
	return 0, nil
}

// enrollTag validates one owner-signed tag against this session's body key
// and the pending window for its kind, then persists it. An invalid tag
// leaves any existing good enrollment untouched (codex P1: validation must
// run on the real enrollment path, never silently skipped).
func enrollTag(keyDir, tagPath string, k *bodykey.BodyKey) (int, error) {
	raw, err := os.ReadFile(tagPath)
	if err != nil {
		return protocol.ExitUsage, fmt.Errorf("--tag-file: %w", err)
	}
	var tf shareTagFile
	if err := json.Unmarshal(raw, &tf); err != nil {
		return protocol.ExitUsage, fmt.Errorf("--tag-file: %w", err)
	}
	// The kind must be one the share credential set defines, and the
	// conditions string must be exactly the canonical per-kind string for
	// the pending (or current) window — no free-form conditions.
	pending, _, perr := readSharePending(keyDir)
	if perr != nil {
		return protocol.ExitActionRequired, fmt.Errorf("no pending window: run `amq-remote share --session ... --renew` first: %v", perr)
	}
	var match *shareTagFile
	for i := range pending {
		if pending[i].Kind == tf.Kind {
			match = &pending[i]
			break
		}
	}
	if match == nil {
		return protocol.ExitActionRequired, fmt.Errorf("tag kind %d is not in the pending window set; rerun `share --renew`", tf.Kind)
	}
	if tf.Conditions != match.Conditions {
		return protocol.ExitActionRequired, fmt.Errorf("tag conditions %q do not match the pending window %q for kind %d; rerun `share --renew` to reprint", tf.Conditions, match.Conditions, tf.Kind)
	}
	tag := bodykey.AuthTag{OwnerPubKey: tf.OwnerPubKey, Conditions: tf.Conditions}
	if err := tag.SetSigHex(tf.Sig); err != nil {
		return protocol.ExitUsage, err
	}
	if err := tag.Verify(k.PublicKeyHex()); err != nil {
		return protocol.ExitActionRequired, fmt.Errorf("owner tag rejected: %w", err)
	}
	// Verify the conditions actually satisfy their own kind (defense in
	// depth: the canonical string always does; anything else is refused).
	if err := tag.Satisfies(tf.Kind, time.Now().Add(time.Minute).Unix()); err != nil {
		return protocol.ExitActionRequired, fmt.Errorf("owner tag conditions rejected for kind %d: %v", tf.Kind, err)
	}
	if err := writeShareTag(keyDir, &tf); err != nil {
		return protocol.ExitActionRequired, err
	}
	return 0, nil
}

func sharePaths(keyDir string) (pending, enrolled string) {
	return filepath.Join(keyDir, "share.pending.json"), filepath.Join(keyDir, "share.json")
}

// readSharePending returns the pending per-kind windows and the recorded
// not_after. Missing or corrupt pending is an error — callers must not
// silently bypass the pending constraint (codex P1 finding 7).
func readSharePending(keyDir string) ([]shareTagFile, time.Time, error) {
	p, _ := sharePaths(keyDir)
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, time.Time{}, err
	}
	var doc struct {
		Tags     []shareTagFile `json:"tags"`
		NotAfter int64          `json:"not_after"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, time.Time{}, err
	}
	if len(doc.Tags) == 0 {
		return nil, time.Time{}, errors.New("pending window carries no tags")
	}
	return doc.Tags, time.Unix(doc.NotAfter, 0), nil
}

// readEnrolledTags returns the enrolled per-kind tags from share.json.
// Empty when the file is absent or unreadable — callers distinguish that
// from a validation failure.
func readEnrolledTags(keyDir string) []shareTagFile {
	_, enrolled := sharePaths(keyDir)
	raw, err := os.ReadFile(enrolled)
	if err != nil {
		return nil
	}
	var doc struct {
		Tags []shareTagFile `json:"tags"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	return doc.Tags
}

func writeSharePending(keyDir string, tags []shareTagFile, notAfter time.Time) error {
	p, _ := sharePaths(keyDir)
	doc := struct {
		Tags     []shareTagFile `json:"tags"`
		NotAfter int64          `json:"not_after"`
	}{tags, notAfter.Unix()}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, raw, 0o600)
}

func writeShareTag(keyDir string, tf *shareTagFile) error {
	// Enrolled doc keeps every tag of the current generation: the freshly
	// enrolled one replaces its kind's entry; a generation is complete when
	// all ShareKinds are present.
	tags := enrolledOfKind(readEnrolledTags(keyDir), tf.Kind, tf)
	complete := len(tags) == len(bodykey.ShareKinds)
	doc := struct {
		Tags     []shareTagFile `json:"tags"`
		Complete bool           `json:"complete"`
	}{tags, complete}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	_, enrolled := sharePaths(keyDir)
	if err := os.WriteFile(enrolled, raw, 0o600); err != nil {
		return err
	}
	if complete {
		// Enrollment of all kinds completes the flow: the pending windows
		// are consumed.
		p, _ := sharePaths(keyDir)
		_ = os.Remove(p)
	}
	return nil
}

func enrolledOfKind(tags []shareTagFile, kind uint16, replacement *shareTagFile) []shareTagFile {
	out := make([]shareTagFile, 0, len(bodykey.ShareKinds))
	for _, t := range tags {
		if t.Kind != kind {
			out = append(out, t)
		}
	}
	out = append(out, *replacement)
	return out
}

// enrolledExpiry derives the expiry of one enrolled tag from its OWN signed
// conditions (the created_at< bound is part of the signature) — one
// authority, no second writable not_after field to drift (codex P1
// finding 5).
func enrolledExpiry(tf shareTagFile) (time.Time, error) {
	conds, err := bodykey.ParseConditions(tf.Conditions)
	if err != nil {
		return time.Time{}, err
	}
	for _, c := range conds {
		if c.CreatedLt != nil {
			return time.Unix(int64(*c.CreatedLt), 0), nil
		}
	}
	return time.Time{}, errors.New("conditions carry no created_at< bound")
}

func printSharePending(w io.Writer, session string, k *bodykey.BodyKey, tags []shareTagFile, notAfter time.Time) {
	say(w, "session:     %s", session)
	say(w, "body-pubkey: %s", k.PublicKeyHex())
	say(w, "expires:     %s", notAfter.UTC().Format(time.RFC3339))
	for _, t := range tags {
		say(w, "kind %d preimage: %s", t.Kind, k.PreimageHex(t.Conditions))
		say(w, "kind %d conditions: %s", t.Kind, t.Conditions)
	}
}

func printShareEnrolled(w io.Writer, session string, k *bodykey.BodyKey, tags []shareTagFile) {
	say(w, "session:     %s", session)
	say(w, "body-pubkey: %s", k.PublicKeyHex())
	for _, t := range tags {
		exp, err := enrolledExpiry(t)
		if err != nil {
			say(w, "kind %d: enrolled, conditions unreadable: %v", t.Kind, err)
			continue
		}
		say(w, "kind %d: enrolled, expires %s", t.Kind, exp.UTC().Format(time.RFC3339))
	}
}

func printShareDryRun(w io.Writer, session string, k *bodykey.BodyKey, days int) {
	notAfter := time.Now().Add(time.Duration(days) * 24 * time.Hour)
	say(w, "session:     %s (dry-run)", session)
	say(w, "body-pubkey: %s", k.PublicKeyHex())
	for _, kind := range bodykey.ShareKinds {
		conds := bodykey.ShareConditions(kind, notAfter.Unix())
		say(w, "kind %d preimage: %s", kind, k.PreimageHex(conds))
	}
}

// doctorShareInspection is the doctor surface for body identity: it reports
// mint/enrolled/expiry state per session key dir and warns 7 days before
// the signed window expires. Expiry comes from the signed conditions, so a
// zero/unset field cannot make a fresh enrollment read as expired (codex
// P1 finding 5). An enrolled-but-incomplete generation is reported as such.
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
		if tags := readEnrolledTags(keyDir); len(tags) > 0 {
			perKind := map[string]any{}
			earliest := time.Time{}
			for _, t := range tags {
				exp, err := enrolledExpiry(t)
				if err != nil {
					perKind[fmt.Sprint(t.Kind)] = map[string]string{"error": err.Error()}
					continue
				}
				perKind[fmt.Sprint(t.Kind)] = map[string]string{"expires": exp.UTC().Format(time.RFC3339)}
				if earliest.IsZero() || exp.Before(earliest) {
					earliest = exp
				}
			}
			info["attestation"] = "enrolled"
			info["kinds"] = perKind
			if len(perKind) < len(bodykey.ShareKinds) {
				info["warning"] = fmt.Sprintf("incomplete enrollment: %d of %d kinds enrolled; rerun `amq-remote share --session %s --renew`", len(perKind), len(bodykey.ShareKinds), e.Name())
			}
			if !earliest.IsZero() {
				remaining := time.Until(earliest)
				switch {
				case remaining <= 0:
					info["expiry_warning"] = "expired: run `amq-remote share --session " + e.Name() + " --renew`"
				case remaining < warnHorizon:
					info["expiry_warning"] = fmt.Sprintf("expires in %s: run `amq-remote share --session %s --renew`", remaining.Round(time.Hour), e.Name())
				}
			}
			out[e.Name()] = info
			continue
		}
		if _, _, err := readSharePending(keyDir); err == nil {
			info["attestation"] = "pending: owner has not returned signed tags yet"
		} else {
			info["attestation"] = "missing: run `amq-remote share --session " + e.Name() + "`"
		}
		out[e.Name()] = info
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
