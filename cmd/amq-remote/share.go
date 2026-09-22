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

	"github.com/avivsinai/agent-message-queue/internal/fsq"
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
	days := fs.Int("days", 0, "attestation window in days (default 30; 1..90; on renew, an explicit --days that cannot exceed the current bounds is REFUSED, not silently bumped)")
	tagFile := fs.String("tag-file", "", "JSON file with one owner-signed tag {kind,owner_pubkey,conditions,sig}")
	dryRun := fs.Bool("dry-run", false, "print preimages without writing or changing anything")
	if err := fs.Parse(args); err != nil {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "%v", err)
	}
	daysSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "days" {
			daysSet = true
		}
	})
	if *root == "" {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "--root or AM_ROOT is required")
	}
	if !filepath.IsAbs(*root) {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "--root must be absolute")
	}
	if *days == 0 {
		*days = shareDefaultDays // unset flag: the documented default
	}
	if *days < 1 || *days > 90 {
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "--days must be 1..90 (NIP-OA window bounds), got %d", *days)
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
	// State-leaf confinement runs BEFORE anything else — dry-run, mint,
	// renew (codex r3 #4: the pre-mint refusal must not mint body.key/
	// body.pub first).
	if err := refuseSymlinkedState(keyDir); err != nil {
		return protocol.ExitActionRequired, err
	}
	if *dryRun {
		// Preview and apply run the SAME read-only validation (codex r3 #3):
		// a dry-run must never offer signing data that a real run would
		// refuse, and it must not mask key-load errors as "no body key".
		if loadErr != nil {
			if errors.Is(loadErr, os.ErrNotExist) {
				say(stdout, "dry-run: no body key at %s — a real run would mint one", keyPath)
				return 0, nil
			}
			return protocol.ExitActionRequired, fmt.Errorf("body key at %s: %w", keyPath, loadErr)
		}
		if _, gerr := readEnrolledState(keyDir); gerr != nil {
			return protocol.ExitActionRequired, gerr
		}
		// Ruling (claude 08:29Z): owners sign ONLY preimages printed from a
		// PERSISTED pending window. With a pending window, dry-run prints
		// exactly those (byte-identical, enrollable). A renewal preview
		// computes its bound at the preview instant and is explicitly NOT
		// enrollable — the bound is fixed when the real --renew persists
		// the window; sharing the calculation cannot share the instant
		// (codex r3 #1). With none, the illustration is likewise labeled.
		pendingTags, _, perr := readSharePending(keyDir)
		if perr != nil && !errors.Is(perr, os.ErrNotExist) {
			return protocol.ExitActionRequired, perr
		}
		if *renew {
			if len(pendingTags) > 0 {
				printSharePending(stdout, *session, k, pendingTags, time.Time{})
				say(stdout, "(dry-run: the CURRENT pending window, still enrollable; a real --renew would replace it with a later bound)")
				return 0, nil
			}
			preview, err := renewalWindow(keyDir, *days, daysSet)
			if err != nil {
				return protocol.ExitActionRequired, err
			}
			printSharePending(stdout, *session, k, preview.tags, preview.notAfter)
			say(stdout, "(dry-run preview only: no state written; bound is fixed when --renew runs — sign the preimages that plain share prints afterwards)")
			return 0, nil
		}
		if len(pendingTags) > 0 {
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
	// before expiry, and print the preimages for the owner to sign. The
	// window arithmetic lives in ONE place (renewalWindow) so --dry-run
	// previews exactly what a real run mints (codex re-review P2).
	window, err := renewalWindow(keyDir, *days, daysSet)
	if err != nil {
		return protocol.ExitActionRequired, err
	}
	notAfter, pending := window.notAfter, window.tags
	gen, gerr := readEnrolledState(keyDir)
	if gerr != nil {
		// Corrupt/unreadable enrolled state is NOT absence: refuse and
		// let the operator inspect it (verifier P0-1).
		return protocol.ExitActionRequired, gerr
	}
	pendingTags, _, pendingReadErr := readSharePending(keyDir)
	if pendingReadErr != nil && !errors.Is(pendingReadErr, os.ErrNotExist) {
		// Malformed pending state is NOT absence (codex r3 #2): never
		// silently replace an existing window the owner may be
		// signing against.
		return protocol.ExitActionRequired, pendingReadErr
	}
	if rerr := reconcileStagedState(keyDir, gen, pendingTags, pendingReadErr); rerr != nil {
		return protocol.ExitActionRequired, rerr
	}
	// Re-read after reconciliation: stale leftovers may be gone.
	pendingTags, _, pendingReadErr = readSharePending(keyDir)
	if pendingReadErr != nil && !errors.Is(pendingReadErr, os.ErrNotExist) {
		return protocol.ExitActionRequired, pendingReadErr
	}
	if !*renew {
		if pendingReadErr == nil && len(pendingTags) > 0 {
			// An enrollment is incomplete: reprint the OUTSTANDING pending
			// preimages (verifier P0-1: the owner signs one kind at a time
			// offline and must be able to recover the preimage list without
			// rotating the window). Enrolled credentials are untouched.
			staged, serr := readShareStaged(keyDir)
			if serr != nil {
				return protocol.ExitActionRequired, serr
			}
			outstanding := outstandingPending(pendingTags, staged, gen)
			printShareOutstanding(stdout, *session, k, outstanding, staged, pendingTags, gen)
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
	if err := writeSharePending(keyDir, pending, notAfter); err != nil {
		return protocol.ExitActionRequired, err
	}
	printSharePending(stdout, *session, k, pending, notAfter)
	say(stderr, "sign each preimage with the owner's Buzz identity, then enroll one per kind:\n  amq-remote share --root %s --session %s --tag-file <tag.json> (repeat per kind)", *root, *session)
	return 0, nil
}

// shareWindow is one computed attestation window: the per-kind condition
// strings plus the window's not-after bound. Computed by renewalWindow for
// BOTH real runs and --dry-run previews so the preview cannot drift from
// what a real run mints.
type shareWindow struct {
	notAfter time.Time
	tags     []shareTagFile
}

// renewalWindow computes the attestation window a real `share`/`share
// --renew` run would persist. For a renewal it bumps the bound past any
// existing pending/enrolled conditions so a renewal is a NEW generation
// even in the same second (generation identity comes from the signed
// conditions). For a plain mint it is now+days.
func renewalWindow(keyDir string, days int, daysExplicit bool) (shareWindow, error) {
	notAfter := time.Now().Add(time.Duration(days) * 24 * time.Hour)
	// State-leaf confinement (codex re-review P1): the pending and enrolled
	// files are read/written by these commands; a symlinked leaf must be
	// refused before anything follows it.
	if err := refuseSymlinkedState(keyDir); err != nil {
		return shareWindow{}, err
	}
	prev, _, pendingErr := readSharePending(keyDir)
	if pendingErr != nil && !errors.Is(pendingErr, os.ErrNotExist) {
		return shareWindow{}, pendingErr
	}
	var maxPending int64
	if pendingErr == nil && len(prev) > 0 {
		maxPending = maxPendingBound(prev)
	}
	var maxEnrolled int64
	if gen, gerr := readEnrolledState(keyDir); gerr != nil {
		// Corrupt/unreadable enrolled state is NOT absence (verifier r2
		// P1-2): a renewal over torn state would mint a window no tag can
		// ever be enrolled into. Fail closed like every other path.
		return shareWindow{}, gerr
	} else if gen != nil {
		maxEnrolled = maxEnrolledBound(gen.Tags)
	}
	// Ruling (claude 08:27Z #1): an EXPLICIT --days is never silently
	// overridden by the bump. If the requested window would not clear the
	// bounds the renewal must exceed, REFUSE with the minimum that would
	// work — shortening the horizon has no silent path. The default (no
	// --days) keeps the bump so plain renewals never fail.
	minDays := max(maxPending, maxEnrolled)
	if minDays > 0 && notAfter.Unix() <= minDays {
		if daysExplicit {
			need := int(time.Until(time.Unix(minDays, 0)).Hours()/24) + 1
			if need < 1 {
				need = 1
			}
			if need > 90 {
				need = 90 // never exceed the NIP-OA bound in the message
			}
			return shareWindow{}, protocol.Refuse(protocol.CodeInvalid,
				"--days %d would not exceed the current window bounds; a renewal must set a later bound. Minimum --days that works: %d", days, need)
		}
		notAfter = time.Unix(minDays+1, 0)
	}
	tags := make([]shareTagFile, 0, len(bodykey.ShareKinds))
	for _, kind := range bodykey.ShareKinds {
		tags = append(tags, shareTagFile{Kind: kind, Conditions: bodykey.ShareConditions(kind, notAfter.Unix())})
	}
	return shareWindow{notAfter: notAfter, tags: tags}, nil
}

// refuseSymlinkedState Lstats every state leaf; a symlinked or non-regular
// pending/staged/enrolled state file is refused (verifier r2 P1-4: --renew
// followed a share.pending.json symlink and rewrote an out-of-root file —
// also true of plain share and of share.json itself).
func refuseSymlinkedState(keyDir string) error {
	pendingPath, enrolledPath := sharePaths(keyDir)
	for _, p := range []string{pendingPath, filepath.Join(keyDir, stagedName), enrolledPath} {
		if err := lstatStateLeaf(p); err != nil {
			return protocol.Refuse(protocol.CodeInvalid, "state file %s: %v", p, err)
		}
	}
	return nil
}

// lstatStateLeaf is the confinement rule for one state-file leaf: absent is
// fine; a symlink is refused outright (rename-onto would replace the link,
// but reads would follow it); anything not a regular file (FIFO, directory)
// is refused.
func lstatStateLeaf(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return errors.New("symlinked; refusing")
	}
	if !fi.Mode().IsRegular() {
		return errors.New("not a regular file; refusing")
	}
	return nil
}

// enrollTag validates one owner-signed tag against this session's body key
// and the pending window for its kind, then persists it. An invalid tag
// leaves any existing good enrollment untouched (codex P1: validation must
// run on the real enrollment path, never silently skipped).
func enrollTag(keyDir, tagPath string, k *bodykey.BodyKey) (int, error) {
	// State-leaf confinement applies to enrollment too: the pending read
	// and the staged/published writes below must never follow a symlink.
	if err := refuseSymlinkedState(keyDir); err != nil {
		return protocol.ExitActionRequired, err
	}
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
		if errors.Is(perr, os.ErrNotExist) {
			return protocol.ExitActionRequired, fmt.Errorf("no pending window: run `amq-remote share --session ... --renew` first")
		}
		return protocol.ExitActionRequired, perr
	}
	// Reconcile crash leftovers before matching (verifier r3 P2-3): a
	// complete published generation with stale pending/staged heals here
	// so the owner is never asked to sign against a consumed window.
	gen0, _ := readEnrolledState(keyDir)
	if rerr := reconcileStagedState(keyDir, gen0, pending, nil); rerr != nil {
		return protocol.ExitActionRequired, rerr
	}
	pending, _, perr = readSharePending(keyDir)
	if perr != nil {
		if errors.Is(perr, os.ErrNotExist) {
			return protocol.ExitActionRequired, fmt.Errorf("no pending window: the current generation already covers it; run `amq-remote share --session ...` for the enrolled state")
		}
		return protocol.ExitActionRequired, perr
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
	if err := lstatStateLeaf(p); err != nil {
		return nil, time.Time{}, err
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, time.Time{}, err
	}
	var doc struct {
		Tags     []shareTagFile `json:"tags"`
		NotAfter int64          `json:"not_after"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, time.Time{}, fmt.Errorf("pending state at share.pending.json is invalid: %w", err)
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
	Tags []shareTagFile `json:"tags"`
}

// readEnrolledState distinguishes absence (nil, nil) from an unreadable or
// invalid enrolled doc (nil, err). A corrupt share.json is never treated
// as a fresh enrollment and never silently overwritten (codex P2).
func readEnrolledState(keyDir string) (*enrolledGeneration, error) {
	_, enrolled := sharePaths(keyDir)
	if err := lstatStateLeaf(enrolled); err != nil {
		return nil, err
	}
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
	return writeStateFile(p, raw)
}

func writeShareTag(keyDir string, pending []shareTagFile, tf *shareTagFile) error {
	// Staged publication (codex re-review P1): signed tags accumulate in
	// share.staged.json; the ACTIVE generation in share.json is untouched
	// until the pending generation is complete for every kind. Only then
	// is the new generation published atomically (tmp+rename) and the
	// pending window consumed. An interrupted enrollment therefore leaves
	// the previous generation fully intact, and no write ever truncates a
	// published file in place.
	if _, err := readEnrolledState(keyDir); err != nil {
		return err // corrupt/unreadable enrolled state is never overwritten
	}
	// Reconcile crash leftovers before reading staged state (verifier r3
	// P2-3): a complete published generation with stale pending/staged is
	// healed here so the enrollment targets the live window, not stale
	// leftovers.
	pendingTags, _, pendingReadErr := readSharePending(keyDir)
	if pendingReadErr != nil && !errors.Is(pendingReadErr, os.ErrNotExist) {
		return pendingReadErr
	}
	gen0, _ := readEnrolledState(keyDir)
	if rerr := reconcileStagedState(keyDir, gen0, pendingTags, pendingReadErr); rerr != nil {
		return rerr
	}
	staged, serr := readShareStaged(keyDir)
	if serr != nil {
		return serr
	}
	// The staged doc is keyed by kind; a re-enrollment of the same kind in
	// the same window replaces that staged entry.
	staged[tf.Kind] = *tf

	// Completion: one STAGED tag per kind in ShareKinds (verifier r3 P2-1:
	// enforced in code, not by construction — publication requires the
	// staged set to cover every kind the credential set defines, with
	// conditions matching the pending window for that kind). A short or
	// hand-edited pending window can never publish, so no still-valid tag
	// is ever dropped without a valid replacement for its kind.
	complete := true
	for _, kind := range bodykey.ShareKinds {
		t, ok := staged[kind]
		p := pendingTagForKind(pending, kind)
		if !ok || p == nil || p.Conditions != t.Conditions {
			complete = false
			break
		}
	}
	if !complete {
		return writeShareStaged(keyDir, staged)
	}

	// Publish: the new generation is exactly the staged tags, one per
	// ShareKinds kind (enforced above). share.json is single-generation
	// from here: the staged replacement supersedes every prior tag of its
	// kind, so nothing still-valid is dropped.
	doc := struct {
		Tags []shareTagFile `json:"tags"`
	}{make([]shareTagFile, 0, len(staged))}
	for _, kind := range bodykey.ShareKinds {
		doc.Tags = append(doc.Tags, staged[kind])
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	_, enrolledPath := sharePaths(keyDir)
	if err := writeStateFile(enrolledPath, raw); err != nil {
		return err
	}
	p, _ := sharePaths(keyDir)
	_ = os.Remove(p)
	_ = os.Remove(filepath.Join(keyDir, stagedName))
	return nil
}

// stagedName is the accumulation file for signed-but-unpublished tags.
const stagedName = "share.staged.json"

// reconcileStagedState heals or refuses leftovers, on every command that
// touches share state (verifier r3 P2-3/P2-4):
//   - a complete new generation with pending/staged still on disk (crash
//     between publication and cleanup) is reconciled by removing them —
//     the publication is already the committed state;
//   - staged entries whose conditions do not match the current pending
//     window (superseded by a later --renew) are dropped;
//   - a corrupt staged file is refused with the file named and the
//     remedy (remove it, re-sign) instead of an unrecoverable error.
func reconcileStagedState(keyDir string, gen *enrolledGeneration, pending []shareTagFile, pendingErr error) error {
	if pendingErr != nil && !errors.Is(pendingErr, os.ErrNotExist) {
		return pendingErr // malformed pending: refuse everywhere
	}
	genCoversPending := pendingErr == nil && gen != nil && len(gen.Tags) > 0 &&
		func() bool {
			byKind := map[uint16]string{}
			for _, t := range gen.Tags {
				byKind[t.Kind] = t.Conditions
			}
			if len(byKind) < len(bodykey.ShareKinds) {
				return false
			}
			for _, p := range pending {
				if byKind[p.Kind] != p.Conditions {
					return false
				}
			}
			return true
		}()
	stagedPath := filepath.Join(keyDir, stagedName)
	raw, readErr := os.ReadFile(stagedPath)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	if readErr == nil {
		var tags []shareTagFile
		if err := json.Unmarshal(raw, &tags); err != nil {
			return protocol.Refuse(protocol.CodeInvalid,
				"staged state at %s is invalid (%v); remedy: remove %s and re-sign the pending preimages (`amq-remote share --session ...` reprints them)",
				stagedPath, err, stagedPath)
		}
		if genCoversPending {
			// Crash leftovers after publication: pending and staged are
			// both stale; the published generation is the authority.
			p, _ := sharePaths(keyDir)
			_ = os.Remove(p)
			_ = os.Remove(stagedPath)
			return nil
		}
		// Drop staged entries superseded by the current pending window.
		conds := map[uint16]string{}
		for _, p := range pending {
			conds[p.Kind] = p.Conditions
		}
		kept := make([]shareTagFile, 0, len(tags))
		for _, t := range tags {
			if conds[t.Kind] == t.Conditions {
				kept = append(kept, t)
			}
		}
		if len(kept) != len(tags) {
			return writeShareStaged(keyDir, byKind(kept))
		}
	}
	if genCoversPending {
		// Staged absent but pending remains after publication (crash
		// between the two removes): pending is stale too.
		p, _ := sharePaths(keyDir)
		_ = os.Remove(p)
	}
	return nil
}

// doctorNamedLeafError names the state file in a leaf-confinement error so
// the operator knows which file to inspect (verifier r3 P2-7).
func doctorNamedLeafError(keyDir string, err error) string {
	pendingPath, enrolledPath := sharePaths(keyDir)
	for _, p := range []string{pendingPath, filepath.Join(keyDir, stagedName), enrolledPath} {
		if strings.Contains(err.Error(), p) || strings.Contains(err.Error(), filepath.Base(p)) {
			return p + ": " + err.Error()
		}
	}
	return err.Error()
}

// byKind converts a tag slice to the staged map shape.
func byKind(tags []shareTagFile) map[uint16]shareTagFile {
	m := make(map[uint16]shareTagFile, len(tags))
	for _, t := range tags {
		m[t.Kind] = t
	}
	return m
}

// readShareStaged returns the staged tags keyed by kind (empty map when no
// staged doc exists). A staged doc is advisory state; a corrupt one is
// refused, not silently discarded.
func readShareStaged(keyDir string) (map[uint16]shareTagFile, error) {
	stagedPath := filepath.Join(keyDir, stagedName)
	if err := lstatStateLeaf(stagedPath); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(stagedPath)
	if errors.Is(err, os.ErrNotExist) {
		return map[uint16]shareTagFile{}, nil
	}
	if err != nil {
		return nil, err
	}
	var tags []shareTagFile
	if err := json.Unmarshal(raw, &tags); err != nil {
		return nil, fmt.Errorf("staged state at %s is invalid: %w", stagedName, err)
	}
	m := make(map[uint16]shareTagFile, len(tags))
	for _, t := range tags {
		m[t.Kind] = t
	}
	return m, nil
}

func writeShareStaged(keyDir string, staged map[uint16]shareTagFile) error {
	tags := make([]shareTagFile, 0, len(staged))
	for _, kind := range bodykey.ShareKinds {
		if t, ok := staged[kind]; ok {
			tags = append(tags, t)
		}
	}
	raw, err := json.MarshalIndent(tags, "", "  ")
	if err != nil {
		return err
	}
	if err := writeStateFile(filepath.Join(keyDir, stagedName), raw); err != nil {
		return err
	}
	return nil
}

// writeStateFile publishes one state file through
// internal/fsq.WriteFileAtomic: tmp + fsync + directory sync around the
// rename, so a crash never truncates the target and the publication is
// durable, not merely atomically visible (codex r3: the local helper only
// synced the temp file; the parent directory was never synced, so cleanup
// could remove recovery state before the publication was durably named).
func writeStateFile(path string, raw []byte) error {
	dir, name := filepath.Dir(path), filepath.Base(path)
	if _, err := fsq.WriteFileAtomic(dir, name, raw, 0o600); err != nil {
		return fmt.Errorf("atomic write %s: %w", path, err)
	}
	return nil
}

func pendingKey(kind uint16, conditions string) string {
	return fmt.Sprintf("%d\x00%s", kind, conditions)
}

// outstandingPending returns the pending tags not yet enrolled for the
// current generation — the preimages the owner still has to sign.
func outstandingPending(pending []shareTagFile, staged map[uint16]shareTagFile, gen *enrolledGeneration) []shareTagFile {
	enrolledConds := map[string]bool{}
	// Staged tags count as signed progress: the owner does not re-sign a
	// tag the CLI already holds for this window.
	for _, t := range staged {
		enrolledConds[pendingKey(t.Kind, t.Conditions)] = true
	}
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
	} else if len(tags) > 0 {
		if exp, err := enrolledExpiry(tags[0]); err == nil {
			say(w, "expires:     %s (pending window)", exp.UTC().Format(time.RFC3339))
		}
	}
	for _, t := range tags {
		say(w, "kind %d preimage: %s", t.Kind, k.PreimageHex(t.Conditions))
		say(w, "kind %d conditions: %s", t.Kind, t.Conditions)
	}
}

// printShareOutstanding prints the still-outstanding preimages of the
// current pending generation plus the state of what is already enrolled.
func printShareOutstanding(w io.Writer, session string, k *bodykey.BodyKey, outstanding []shareTagFile, staged map[uint16]shareTagFile, pending []shareTagFile, gen *enrolledGeneration) {
	say(w, "session:     %s", session)
	say(w, "body-pubkey: %s", k.PublicKeyHex())
	if gen != nil {
		for _, t := range gen.Tags {
			if exp, err := enrolledExpiry(t); err == nil {
				say(w, "kind %d: enrolled, expires %s", t.Kind, exp.UTC().Format(time.RFC3339))
			}
		}
	}
	// Verifier r3 P2-2: only staged entries matching the CURRENT pending
	// window are signed progress; a stale entry (superseded by a later
	// --renew) is never presented as signed.
	conds := map[uint16]string{}
	for _, p := range pending {
		conds[p.Kind] = p.Conditions
	}
	for _, t := range staged {
		if conds[t.Kind] == t.Conditions {
			say(w, "kind %d: signed, staged until the generation completes", t.Kind)
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
	say(w, "(illustrative fresh %d-day window; nothing was written and these preimages are NOT enrollable — run a real share to mint the window)", days)
	for _, kind := range bodykey.ShareKinds {
		conds := bodykey.ShareConditions(kind, notAfter.Unix())
		say(w, "kind %d preimage: %s", kind, k.PreimageHex(conds))
		say(w, "kind %d conditions: %s", kind, conds)
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
			if errors.Is(err, os.ErrNotExist) {
				info["key_error"] = err.Error()
				info["remedy"] = "run `amq-remote share --session " + e.Name() + "` to mint one"
			} else {
				// Corrupt/unusable key: never point at --renew — a new
				// window signed under a replacement key would not match
				// the enrolled credentials. Operator inspection first.
				info["key_error"] = err.Error()
				info["remedy"] = "inspect the key directory by hand; do NOT renew over a broken key"
			}
			out[e.Name()] = info
			continue
		}
		gen, gerr := readEnrolledState(keyDir)
		if gerr != nil {
			// Name the file in leaf-confinement errors (verifier r3 P2-7).
			info["attestation_error"] = doctorNamedLeafError(keyDir, gerr)
			out[e.Name()] = info
			continue
		}
		// Outstanding pending preimages are visible in BOTH branches: a
		// half-finished renewal keeps every old tag, so the enrolled tag
		// count alone cannot distinguish it from a healthy enrollment
		// (verifier r2 P1-3). While unsigned preimages remain, the remedy
		// is always plain `share`, never `--renew` — renewing rotates the
		// window and strands preimages the owner may already be signing.
		pendingTags, _, pendingErr := readSharePending(keyDir)
		outstanding := 0
		if pendingErr != nil && !errors.Is(pendingErr, os.ErrNotExist) {
			// Malformed pending state is surfaced, never treated as absence
			// (codex r3 #2: doctor must not report a healthy state over a
			// corrupt window).
			info["attestation_error"] = doctorNamedLeafError(keyDir, pendingErr)
			out[e.Name()] = info
			continue
		}
		// Reconcile crash leftovers so doctor reports the true state
		// (verifier r3 P2-3); a corrupt staged file is surfaced with its
		// remedy, not a dead end (verifier r3 P2-4).
		if rerr := reconcileStagedState(keyDir, gen, pendingTags, pendingErr); rerr != nil {
			if refusal, ok := rerr.(*protocol.Refusal); ok {
				info["staged_error"] = refusal.Message
			} else {
				info["staged_error"] = rerr.Error()
			}
			out[e.Name()] = info
			continue
		}
		pendingTags, _, pendingErr = readSharePending(keyDir)
		if pendingErr == nil {
			staged, serr := readShareStaged(keyDir)
			if serr != nil {
				info["staged_error"] = serr.Error()
				out[e.Name()] = info
				continue
			}
			outstanding = len(outstandingPending(pendingTags, staged, gen))
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
			if pendingErr == nil && outstanding > 0 {
				info["attestation"] = fmt.Sprintf("enrolled, renewal in progress: %d of %d kinds signed (staged)", len(bodykey.ShareKinds)-outstanding, len(bodykey.ShareKinds))
				info["warning"] = fmt.Sprintf("renewal in progress: %d preimage(s) still unsigned; run `amq-remote share --session %s` to reprint them (do NOT renew: renewing rotates the window and the still-unsigned preimages)", outstanding, e.Name())
			} else {
				info["attestation"] = "enrolled"
			}
			renewalOutstanding := pendingErr == nil && outstanding > 0
			info["kinds"] = perKind
			if !earliest.IsZero() {
				// Verifier r3 P1-1: the expiry FACT is always emitted — an
				// active generation lapsing mid-renewal is exactly when the
				// alarm matters (the staged shape makes the whole renewal an
				// outage window). Only the REMEDY is conditional: while
				// preimages are outstanding the remedy is plain `share`,
				// never `--renew` (rotating the window would strand the
				// still-unsigned preimages).
				remaining := time.Until(earliest)
				shareRemedy := "run `amq-remote share --session " + e.Name() + " --renew`"
				if renewalOutstanding {
					shareRemedy = "run `amq-remote share --session " + e.Name() + "` to complete the in-progress renewal"
				}
				switch {
				case remaining <= 0:
					info["expiry_warning"] = "expired: " + shareRemedy
				case remaining < warnHorizon:
					info["expiry_warning"] = fmt.Sprintf("expires in %s: %s", remaining.Round(time.Hour), shareRemedy)
				}
			}
			out[e.Name()] = info
			continue
		}
		if pendingErr == nil {
			info["attestation"] = fmt.Sprintf("pending: %d of %d kinds signed (staged)", len(bodykey.ShareKinds)-outstanding, len(bodykey.ShareKinds))
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
