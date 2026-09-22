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

// validateKeyDirSymlinks walks the FULL owned path from root down to dir
// (including extensions/remote/keys itself and its parents — codex P1:
// checking only below the keys root misses a symlink AT keys that redirects
// mint outside the root) and refuses any symlink or non-directory
// component. Absent components are fine — Mint creates real directories.
func validateKeyDirSymlinks(root, dir string) error {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return err
	}
	cur := root
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
	if err := validateKeyDirSymlinks(*root, keyDir); err != nil {
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
		// Dry-run must print exactly what a real run would enroll
		// (verifier P1-3): when a pending window exists, print ITS
		// preimages, not a recomputed fresh bound.
		if pendingTags, _, perr := readSharePending(keyDir); perr == nil && len(pendingTags) > 0 {
			if _, gerr := readEnrolledState(keyDir); gerr != nil {
				return protocol.ExitActionRequired, gerr
			}
			printSharePending(stdout, *session, k, pendingTags, time.Time{})
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
	if *renew {
		// A renewal is a NEW generation even in the same second as the old
		// window: bump the bound past any existing pending/enrolled
		// conditions so the signed preimages (and generation identity)
		// actually change.
		if prev, _, perr := readSharePending(keyDir); perr == nil && len(prev) > 0 {
			if maxOld := maxPendingBound(prev); notAfter.Unix() <= maxOld {
				notAfter = time.Unix(maxOld+1, 0)
			}
		}
		if gen, gerr := readEnrolledState(keyDir); gerr == nil && gen != nil {
			if maxEnrolled := maxEnrolledBound(gen.Tags); notAfter.Unix() <= maxEnrolled {
				notAfter = time.Unix(maxEnrolled+1, 0)
			}
		}
	}
	if !*renew {
		gen, gerr := readEnrolledState(keyDir)
		if gerr != nil {
			// Corrupt/unreadable enrolled state is NOT absence: refuse and
			// let the operator inspect it (verifier P0-1).
			return protocol.ExitActionRequired, gerr
		}
		if pendingTags, _, perr := readSharePending(keyDir); perr == nil && len(pendingTags) > 0 {
			// An enrollment is incomplete: reprint the OUTSTANDING pending
			// preimages (verifier P0-1: the owner signs one kind at a time
			// offline and must be able to recover the preimage list without
			// rotating the window). Enrolled credentials are untouched.
			outstanding := outstandingPending(keyDir, pendingTags, gen)
			printShareOutstanding(stdout, *session, k, outstanding, gen)
			return 0, nil
		}
		if gen != nil && len(gen.Tags) > 0 {
			// Fully enrolled; plain `share` does not disturb the state.
			// Print the ENROLLED state (expiry derived from the signed
			// conditions).
			printShareEnrolled(stdout, *session, k, gen.Tags)
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
	if err := writeShareTag(keyDir, pending, &tf); err != nil {
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

// enrolledGeneration records the signed tags enrolled for the pending
// generation. Generation identity comes from the tags' own signed
// conditions: renewing mints a new created_at< bound, so a tag belongs to
// the current generation iff its conditions match the pending window for
// its kind (codex P1: counting five tags old-plus-new wrongly completed a
// renewal after its first enrollment).
type enrolledGeneration struct {
	Tags     []shareTagFile `json:"tags"`
	Complete bool           `json:"complete"`
}

// readEnrolledState distinguishes absence (nil, nil) from an unreadable or
// invalid enrolled doc (nil, err). A corrupt share.json is never treated
// as a fresh enrollment and never silently overwritten (codex P2).
func readEnrolledState(keyDir string) (*enrolledGeneration, error) {
	_, enrolled := sharePaths(keyDir)
	raw, err := os.ReadFile(enrolled)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var gen enrolledGeneration
	if err := json.Unmarshal(raw, &gen); err != nil {
		return nil, fmt.Errorf("enrolled state at share.json is invalid: %w", err)
	}
	return &gen, nil
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

func writeShareTag(keyDir string, pending []shareTagFile, tf *shareTagFile) error {
	// Retention is make-before-break (verifier P0-1): the enrolled doc keeps
	// every still-valid credential — previous-generation tags stay until
	// their own signed created_at< bound passes or a newer tag for the same
	// kind supersedes them. COMPLETION, by contrast, is keyed on the pending
	// generation only: one enrolled tag per pending kind, matched by signed
	// conditions. The two obligations are separate; the earlier fix
	// conflated them by deleting the old generation.
	gen, err := readEnrolledState(keyDir)
	if err != nil {
		return err // corrupt/unreadable enrolled state is never overwritten
	}
	now := time.Now()
	retained := map[uint16]shareTagFile{}
	if gen != nil {
		for _, t := range gen.Tags {
			// A tag for the kind being enrolled right now is superseded by
			// the fresh one; every OTHER still-valid tag is retained, even
			// from a previous generation (its own signed bound governs
			// its lifetime, not the pending window).
			if t.Kind == tf.Kind {
				continue
			}
			exp, expErr := enrolledExpiry(t)
			if expErr != nil || !exp.After(now) {
				continue // expired or unreadable: drop
			}
			retained[t.Kind] = t
		}
	}
	// The freshly enrolled tag supersedes any retained tag for its kind.
	retained[tf.Kind] = *tf

	// Completion: one enrolled tag whose conditions match each pending kind.
	newGen := map[uint16]shareTagFile{}
	for kind, t := range retained {
		if p := pendingTagForKind(pending, kind); p != nil && p.Conditions == t.Conditions {
			newGen[kind] = t
		}
	}
	complete := len(newGen) == len(pending)

	doc := struct {
		Tags     []shareTagFile `json:"tags"`
		Complete bool           `json:"complete"`
	}{make([]shareTagFile, 0, len(retained)), complete}
	for _, t := range retained {
		doc.Tags = append(doc.Tags, t)
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	_, enrolledPath := sharePaths(keyDir)
	if err := os.WriteFile(enrolledPath, raw, 0o600); err != nil {
		return err
	}
	if complete {
		// The pending generation is fully enrolled: the previous generation
		// is superseded for every kind and its windows are consumed. The
		// swap is atomic in effect: one write carried both.
		p, _ := sharePaths(keyDir)
		_ = os.Remove(p)
	}
	return nil
}

func pendingKey(kind uint16, conditions string) string {
	return fmt.Sprintf("%d\x00%s", kind, conditions)
}

// outstandingPending returns the pending tags not yet enrolled for the
// current generation — the preimages the owner still has to sign.
func outstandingPending(keyDir string, pending []shareTagFile, gen *enrolledGeneration) []shareTagFile {
	enrolledConds := map[string]bool{}
	if gen != nil {
		for _, t := range gen.Tags {
			enrolledConds[pendingKey(t.Kind, t.Conditions)] = true
		}
	}
	out := make([]shareTagFile, 0, len(pending))
	for _, p := range pending {
		if !enrolledConds[pendingKey(p.Kind, p.Conditions)] {
			out = append(out, p)
		}
	}
	return out
}

// maxPendingBound returns the largest created_at< bound across the pending
// windows.
func maxPendingBound(tags []shareTagFile) int64 {
	var max int64
	for _, t := range tags {
		if exp, err := enrolledExpiry(t); err == nil && exp.Unix() > max {
			max = exp.Unix()
		}
	}
	return max
}

// maxEnrolledBound returns the largest created_at< bound across enrolled
// tags.
func maxEnrolledBound(tags []shareTagFile) int64 {
	return maxPendingBound(tags)
}

func pendingTagForKind(pending []shareTagFile, kind uint16) *shareTagFile {
	for i := range pending {
		if pending[i].Kind == kind {
			return &pending[i]
		}
	}
	return nil
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
	if !notAfter.IsZero() {
		say(w, "expires:     %s", notAfter.UTC().Format(time.RFC3339))
	} else if exp, err := enrolledExpiry(tags[0]); err == nil {
		say(w, "expires:     %s (pending window)", exp.UTC().Format(time.RFC3339))
	}
	for _, t := range tags {
		say(w, "kind %d preimage: %s", t.Kind, k.PreimageHex(t.Conditions))
		say(w, "kind %d conditions: %s", t.Kind, t.Conditions)
	}
}

// printShareOutstanding prints the still-outstanding preimages of the
// current pending generation plus the state of what is already enrolled.
func printShareOutstanding(w io.Writer, session string, k *bodykey.BodyKey, outstanding []shareTagFile, gen *enrolledGeneration) {
	say(w, "session:     %s", session)
	say(w, "body-pubkey: %s", k.PublicKeyHex())
	if gen != nil {
		for _, t := range gen.Tags {
			if exp, err := enrolledExpiry(t); err == nil {
				say(w, "kind %d: enrolled, expires %s", t.Kind, exp.UTC().Format(time.RFC3339))
			}
		}
	}
	if len(outstanding) == 0 {
		return
	}
	say(w, "outstanding preimages (%d kind(s) still unsigned):", len(outstanding))
	for _, t := range outstanding {
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
		gen, gerr := readEnrolledState(keyDir)
		if gerr != nil {
			info["attestation_error"] = gerr.Error()
			out[e.Name()] = info
			continue
		}
		if gen != nil && len(gen.Tags) > 0 {
			perKind := map[string]any{}
			earliest := time.Time{}
			for _, t := range gen.Tags {
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
				info["warning"] = fmt.Sprintf("incomplete enrollment: %d of %d kinds enrolled; run `amq-remote share --session %s` to reprint the outstanding preimages (do not renew: renewing rotates the window and the still-unsigned preimages)", len(perKind), len(bodykey.ShareKinds), e.Name())
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
