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
	if daysSet && *days == 0 {
		// Verifier r4 P2-5: an EXPLICIT --days 0 is out of range, never
		// silently promoted to the default; only an unset flag gets the
		// documented default.
		return protocol.ExitUsage, protocol.Refuse(protocol.CodeInvalid, "--days must be 1..90 (NIP-OA window bounds), got 0")
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
	// Round-9: ONE loader reads all five state leaves once. Confinement,
	// dry-run validation, mint/renew/enroll and doctor all branch on the
	// typed outcomes; nothing below re-reads the state files.
	st, err := loadShareState(keyDir)
	if err != nil {
		return protocol.ExitActionRequired, err
	}
	// State-leaf confinement runs BEFORE anything else — dry-run, mint,
	// renew (codex r3 #4) — and refuses on every leaf, every command.
	if err := st.refuseIfConfined(); err != nil {
		return protocol.ExitActionRequired, err
	}
	if *dryRun {
		// Preview and apply run the SAME read-only validation (codex r3 #3,
		// re-ruled r10-P1): a dry-run must never offer signing data that a
		// real run would refuse, and the validation runs BEFORE the
		// missing-key early return — with no body.key and a corrupt staged
		// leaf the preview refuses, it does not say "would mint".
		leaves := st.unreadableLeaves()
		if len(leaves) > 0 {
			l := leaves[0]
			if l.Path == filepath.Join(keyDir, stagedName) {
				return protocol.ExitActionRequired, stagedCorruptRefusal(*session, l)
			}
			return protocol.ExitActionRequired, l.Err
		}
		if loadErr != nil {
			if errors.Is(loadErr, os.ErrNotExist) {
				say(stdout, "dry-run: no body key at %s — a real run would mint one", keyPath)
				return 0, nil
			}
			return protocol.ExitActionRequired, fmt.Errorf("body key at %s: %w", keyPath, loadErr)
		}
		// Ruling (claude 08:29Z, re-ruled r10-P3): owners sign ONLY
		// preimages printed from a PERSISTED pending window. A renewal
		// preview ALWAYS validates and calculates the REQUESTED renewal —
		// including the explicit --days bounds — whether or not a window
		// exists; it is labelled non-enrollable and the current window is
		// never substituted for it (with --renew --days 1 over a --days 90
		// window the preview refuses exactly where the real run refuses).
		pendingTags := st.Pending.Tags
		if *renew {
			preview, err := renewalWindow(st, *days, daysSet)
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

	// r10-P1: the real run validates the loaded snapshot BEFORE minting
	// or reconciling anything — mint must not create body.key/body.pub
	// next to state it is about to refuse (with no body.key and a corrupt
	// staged leaf the run exits 6 WITHOUT minting).
	leaves := st.unreadableLeaves()
	if len(leaves) > 0 {
		l := leaves[0]
		if l.Path == filepath.Join(keyDir, stagedName) {
			return protocol.ExitActionRequired, stagedCorruptRefusal(*session, l)
		}
		return protocol.ExitActionRequired, l.Err
	}

	// Load-or-mint the body keypair. Mint is refused if a key already
	// exists; renewal and tag enrollment reuse the existing key.
	if loadErr != nil {
		if !errors.Is(loadErr, os.ErrNotExist) && !errors.Is(loadErr, bodykey.ErrWrongFormat) {
			return protocol.ExitActionRequired, fmt.Errorf("body key at %s: %w", keyPath, loadErr)
		}
		if k, err = bodykey.Mint(keyDir); err != nil {
			// Verifier r6 P3: name the offending leaf, not body.key — Mint
			// can refuse over body.pub too (body.pub is checked first now).
			return protocol.ExitActionRequired, fmt.Errorf("mint body keypair in %s: %w", keyDir, err)
		}
	}

	if *tagFile != "" {
		return enrollTag(*session, keyDir, *tagFile, k, st)
	}

	// Mint or renew: persist one pending window per kind so doctor can warn
	// before expiry, and print the preimages for the owner to sign. The
	// window arithmetic lives in ONE place (renewalWindow) so --dry-run
	// previews exactly what a real run mints (codex re-review P2).
	window, err := renewalWindow(st, *days, daysSet)
	if err != nil {
		return protocol.ExitActionRequired, err
	}
	notAfter, pending := window.notAfter, window.tags
	if rerr := reconcileStagedState(*session, keyDir, st); rerr != nil {
		return protocol.ExitActionRequired, rerr
	}
	// Re-load after reconciliation: stale leftovers may be gone. The loader
	// is the only reader, so a fresh snapshot is a fresh call.
	st2, err := loadShareState(keyDir)
	if err != nil {
		return protocol.ExitActionRequired, err
	}
	if err := st2.refuseIfConfined(); err != nil {
		return protocol.ExitActionRequired, err
	}
	pendingTags := st2.Pending.Tags
	pendingGone := st2.Pending.State == leafAbsent
	if !*renew {
		if !pendingGone && len(pendingTags) > 0 {
			// An enrollment is incomplete: reprint the OUTSTANDING pending
			// preimages (verifier P0-1: the owner signs one kind at a time
			// offline and must be able to recover the preimage list without
			// rotating the window). Enrolled credentials are untouched.
			if st2.Staged.Err != nil {
				return protocol.ExitActionRequired, stagedCorruptRefusal(*session, st2.Staged.leafRead)
			}
			outstanding := outstandingPending(pendingTags, st2.Staged.Tags, st2.Enrolled.Gen)
			printShareOutstanding(stdout, *session, k, outstanding, st2.Staged.Tags, pendingTags, st2.Enrolled.Gen)
			return 0, nil
		}
		if st2.Enrolled.Gen != nil && len(st2.Enrolled.Gen.Tags) > 0 {
			// Fully enrolled; plain `share` does not disturb the state.
			// Print the ENROLLED state (expiry derived from the signed
			// conditions).
			printShareEnrolled(stdout, *session, k, st2.Enrolled.Gen.Tags)
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
// --renew` run would persist. It consumes the loader snapshot (r9): no
// re-reads, and the confinement check already ran in the caller.
func renewalWindow(st *shareState, days int, daysExplicit bool) (shareWindow, error) {
	notAfter := time.Now().Add(time.Duration(days) * 24 * time.Hour)
	// Unreadable state is NOT absence (verifier r2 P1-2): a renewal over
	// torn state would mint a window no tag can ever be enrolled into.
	// Fail closed like every other path.
	if st.Pending.Err != nil {
		return shareWindow{}, st.Pending.Err
	}
	if st.Enrolled.Err != nil {
		return shareWindow{}, st.Enrolled.Err
	}
	var maxPending int64
	if len(st.Pending.Tags) > 0 {
		maxPending = maxPendingBound(st.Pending.Tags)
	}
	var maxEnrolled int64
	if st.Enrolled.Gen != nil {
		maxEnrolled = maxEnrolledBound(st.Enrolled.Gen.Tags)
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

// refuseSymlinkedState was folded into loadShareState (r9): the loader
// lstats all five leaves once and refuseIfConfined renders the same
// refusal from the typed outcomes.

// leafConfinementReason classifies a non-regular lstat result (r10-P2).
func leafConfinementReason(fi os.FileInfo) string {
	if fi.Mode()&os.ModeSymlink != 0 {
		return "symlinked"
	}
	return "not a regular file"
}

// stateLeafError is the typed leaf-confinement refusal: it names the state
// file and why it was refused (verifier r4 P1-2).
type stateLeafError struct {
	Path   string
	Reason string
}

func (e *stateLeafError) Error() string {
	return "state file " + e.Path + ": " + e.Reason + "; refusing"
}

// enrollTag validates one owner-signed tag against this session's body key
// and the pending window for its kind, then persists it. An invalid tag
// leaves any existing good enrollment untouched (codex P1: validation must
// run on the real enrollment path, never silently skipped).
func enrollTag(session, keyDir, tagPath string, k *bodykey.BodyKey, st *shareState) (int, error) {
	// State-leaf confinement already ran in share() via refuseIfConfined.
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
	if st.Pending.State == leafAbsent {
		return protocol.ExitActionRequired, fmt.Errorf("no pending window: run `amq-remote share --session %s --renew` first", session)
	}
	if st.Pending.Err != nil {
		return protocol.ExitActionRequired, st.Pending.Err
	}
	// Reconcile crash leftovers before matching (verifier r3 P2-3): a
	// complete published generation with stale pending/staged heals here
	// so the owner is never asked to sign against a consumed window.
	if rerr := reconcileStagedState(session, keyDir, st); rerr != nil {
		return protocol.ExitActionRequired, rerr
	}
	// Re-load after reconciliation; the loader is the only reader.
	st2, err := loadShareState(keyDir)
	if err != nil {
		return protocol.ExitActionRequired, err
	}
	if err := st2.refuseIfConfined(); err != nil {
		return protocol.ExitActionRequired, err
	}
	if st2.Pending.State == leafAbsent {
		return protocol.ExitActionRequired, fmt.Errorf("no pending window: the current generation already covers it; run `amq-remote share --session %s` for the enrolled state", session)
	}
	if st2.Pending.Err != nil {
		return protocol.ExitActionRequired, st2.Pending.Err
	}
	pending := st2.Pending.Tags
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
	if err := writeShareTag(session, keyDir, &tf); err != nil {
		return protocol.ExitActionRequired, err
	}
	return 0, nil
}

func sharePaths(keyDir string) (pending, enrolled string) {
	return filepath.Join(keyDir, "share.pending.json"), filepath.Join(keyDir, "share.json")
}

// ---------- Round-9 state loader: the ONLY reader of the state leaves ----------
//
// The r8 ruling: there was no read-validated state struct — the three read
// helpers were called directly from fourteen sites, each guarded by a
// different condition, so every output branch read a different subset of
// state (dry-run printing signing data the real run refuses; doctor rows
// derived from different subsets than the mutating path refuses on). One
// loader, one snapshot, typed per-leaf outcomes; consumers branch on the
// outcome, never re-read.

// leafState is the typed outcome class for one state leaf.
type leafState int

const (
	leafAbsent     leafState = iota // leaf does not exist
	leafOK                          // present and successfully read/parsed
	leafUnreadable                  // present but read or parse failed (Err)
	leafConfined                    // symlinked or non-regular (see Path, leaf.Confinement below)
)

// leafRead is one leaf's typed outcome: absent / ok(value) / unreadable(reason) /
// confined(path). Confinement carries the typed *stateLeafError so every
// remedy string can derive from the outcome instead of prose matching.
type leafRead struct {
	State       leafState
	Path        string
	Err         error           // leafUnreadable: the read/parse failure
	Confinement *stateLeafError // leafConfined: the typed leaf refusal
}

// readable reports whether the leaf's VALUE may be consumed.
func (l *leafRead) readable() bool { return l.State == leafOK }

// confinedErr renders the leaf's confinement as the refusal the mutating
// path prints (the same refusal refuseSymlinkedState produced).
func (l *leafRead) confinedErr() error {
	return protocol.RefuseWrap(protocol.CodeInvalid, l.Confinement, "%s", l.Confinement)
}

// shareState is the read-validated snapshot of all five state leaves.
type shareState struct {
	KeyDir  string
	BodyKey leafRead
	BodyPub leafRead
	Pending struct {
		leafRead
		Tags     []shareTagFile
		NotAfter time.Time
	}
	Enrolled struct {
		leafRead
		Gen *enrolledGeneration
	}
	Staged struct {
		leafRead
		Tags map[uint16]shareTagFile
	}
}

// confinedLeaves returns every leaf the loader found confined, in a fixed
// order, so one refusal can name them all.
func (st *shareState) confinedLeaves() []leafRead {
	var out []leafRead
	for _, l := range []leafRead{st.Pending.leafRead, st.Enrolled.leafRead, st.Staged.leafRead, st.BodyPub, st.BodyKey} {
		if l.State == leafConfined {
			out = append(out, l)
		}
	}
	return out
}

// refuseIfConfined is the single confinement gate every command path runs
// immediately after loadShareState: the categorical rule (every state
// leaf, on every command) in one place.
func (st *shareState) refuseIfConfined() error {
	for _, l := range st.confinedLeaves() {
		return l.confinedErr()
	}
	return nil
}

// unreadableLeaves returns every state leaf that is present but unreadable
// (read or parse failure) — the dry-run and doctor surfaces walk this to
// refuse or report exactly where the real mutating path would refuse.
func (st *shareState) unreadableLeaves() []leafRead {
	var out []leafRead
	for _, l := range []leafRead{st.Pending.leafRead, st.Enrolled.leafRead, st.Staged.leafRead} {
		if l.State == leafUnreadable {
			out = append(out, l)
		}
	}
	return out
}

// loadShareState lstat-checks all five state leaves (pending, enrolled,
// staged, body.pub, body.key), reads enrolled, pending and staged ONCE
// each, and returns the typed snapshot. It never writes and never follows
// a confined leaf. This function is the ONLY caller of the three read
// helpers below; every consumer — share, dry-run, renew preview,
// enrollment, publication, doctor — takes the snapshot.
func loadShareState(keyDir string) (*shareState, error) {
	pendingPath, enrolledPath := sharePaths(keyDir)
	stagedPath := filepath.Join(keyDir, stagedName)
	st := &shareState{KeyDir: keyDir}
	leaves := []struct {
		path string
		out  *leafRead
	}{
		{pendingPath, &st.Pending.leafRead},
		{enrolledPath, &st.Enrolled.leafRead},
		{stagedPath, &st.Staged.leafRead},
		{filepath.Join(keyDir, "body.pub"), &st.BodyPub},
		{filepath.Join(keyDir, "body.key"), &st.BodyKey},
	}
	for _, leaf := range leaves {
		leaf.out.Path = leaf.path
		// r10-P2: lstatStateLeaf returns nil for a MISSING leaf (absence is
		// not a confinement), so the loader must classify absence itself —
		// otherwise an empty directory's body.key/body.pub read as leafOK
		// and no consumer ever corrects them (the key leaves are never
		// re-read). All five leaves get a true state here.
		fi, err := os.Lstat(leaf.path)
		switch {
		case errors.Is(err, os.ErrNotExist):
			leaf.out.State = leafAbsent
		case err != nil:
			return nil, err // unexpected lstat failure: propagate
		case fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular():
			leaf.out.State = leafConfined
			leaf.out.Confinement = &stateLeafError{Path: leaf.path, Reason: leafConfinementReason(fi)}
		default:
			leaf.out.State = leafOK
		}
	}
	// The three reads, once each. Absence is a state, not an error; a
	// read/parse failure downgrades the leaf to leafUnreadable with the
	// SAME message text the direct helpers produced.
	if st.Pending.readable() {
		raw, err := os.ReadFile(pendingPath)
		switch {
		case err == nil:
			var doc struct {
				Tags     []shareTagFile `json:"tags"`
				NotAfter int64          `json:"not_after"`
			}
			if jerr := json.Unmarshal(raw, &doc); jerr != nil {
				st.Pending.State = leafUnreadable
				st.Pending.Err = fmt.Errorf("pending state at share.pending.json is invalid: %w", jerr)
			} else if len(doc.Tags) == 0 {
				st.Pending.State = leafUnreadable
				st.Pending.Err = errors.New("pending window carries no tags")
			} else {
				st.Pending.Tags = doc.Tags
				st.Pending.NotAfter = time.Unix(doc.NotAfter, 0)
			}
		case errors.Is(err, os.ErrNotExist):
			st.Pending.State = leafAbsent // vanished between lstat and read
		default:
			st.Pending.State, st.Pending.Err = leafUnreadable, err
		}
	}
	if st.Enrolled.readable() {
		raw, err := os.ReadFile(enrolledPath)
		switch {
		case errors.Is(err, os.ErrNotExist):
			st.Enrolled.State = leafAbsent
		case err != nil:
			st.Enrolled.State, st.Enrolled.Err = leafUnreadable, err
		default:
			var gen enrolledGeneration
			if jerr := json.Unmarshal(raw, &gen); jerr != nil {
				st.Enrolled.State = leafUnreadable
				st.Enrolled.Err = fmt.Errorf("enrolled state at share.json is invalid: %w", jerr)
			} else {
				st.Enrolled.Gen = &gen
			}
		}
	}
	if st.Staged.readable() {
		raw, err := os.ReadFile(stagedPath)
		switch {
		case errors.Is(err, os.ErrNotExist):
			st.Staged.State = leafAbsent
		case err != nil:
			st.Staged.State, st.Staged.Err = leafUnreadable, err
		default:
			var tags []shareTagFile
			if jerr := json.Unmarshal(raw, &tags); jerr != nil {
				st.Staged.State = leafUnreadable
				st.Staged.Err = fmt.Errorf("staged state at %s is invalid: %w", stagedName, jerr)
			} else {
				m := make(map[uint16]shareTagFile, len(tags))
				for _, t := range tags {
					m[t.Kind] = t
				}
				st.Staged.Tags = m
			}
		}
	}
	return st, nil
}

// stagedCorruptRefusal is the ONE refusal text for a corrupt staged leaf,
// shared by the real reconcile path and the dry-run preview so the preview
// refuses with exactly the refusal its comment claims (r8 P2).
func stagedCorruptRefusal(session string, l leafRead) error {
	return protocol.Refuse(protocol.CodeInvalid,
		"staged state at %s is invalid (%v); remedy: remove %s and re-sign the pending preimages (`amq-remote share --session %s` reprints them)",
		l.Path, l.Err, l.Path, session)
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

// The enrolled read was folded into loadShareState (r9): enrolled is read
// once by the loader; a corrupt share.json is never treated as a fresh
// enrollment and never silently overwritten (codex P2).

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

func writeShareTag(session, keyDir string, tf *shareTagFile) error {
	// Staged publication (codex re-review P1): signed tags accumulate in
	// share.staged.json; the ACTIVE generation in share.json is untouched
	// until the pending generation is complete for every kind. Only then
	// is the new generation published atomically (tmp+rename) and the
	// pending window consumed. An interrupted enrollment therefore leaves
	// the previous generation fully intact, and no write ever truncates a
	// published file in place.
	//
	// Round-9: the loader is the only reader. The state may have changed
	// since the caller loaded (enrollment is offline-paced), so this path
	// loads its own snapshot and refuses on confinement or unreadable
	// leaves — corrupt/unreadable enrolled state is never overwritten.
	st, err := loadShareState(keyDir)
	if err != nil {
		return err
	}
	if err := st.refuseIfConfined(); err != nil {
		return err
	}
	if st.Enrolled.Err != nil {
		return st.Enrolled.Err
	}
	var pending []shareTagFile
	if st.Pending.readable() {
		pending = st.Pending.Tags
	} else if st.Pending.Err != nil {
		return st.Pending.Err // malformed pending: never publish over it
	}
	if rerr := reconcileStagedState(session, keyDir, st); rerr != nil {
		return rerr
	}
	if st.Staged.Err != nil {
		return stagedCorruptRefusal(session, st.Staged.leafRead)
	}
	staged := st.Staged.Tags
	if staged == nil {
		staged = map[uint16]shareTagFile{} // absent staged leaf: start the doc
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
		// Verifier r4 P2-2: a window that cannot cover ShareKinds never
		// publishes — the operator gets a REFUSAL naming the missing kinds,
		// not silent staging forever. (A complete-but-in-progress window
		// for a healthy renewal just stages; the refusal is only for
		// windows that can never be completed.)
		coversShareKinds := true
		for _, kind := range bodykey.ShareKinds {
			if pendingTagForKind(pending, kind) == nil {
				coversShareKinds = false
				break
			}
		}
		if !coversShareKinds {
			missing := make([]uint16, 0, len(bodykey.ShareKinds))
			for _, kind := range bodykey.ShareKinds {
				if pendingTagForKind(pending, kind) == nil {
					missing = append(missing, kind)
				}
			}
			return protocol.Refuse(protocol.CodeInvalid,
				"pending window at share.pending.json does not cover every kind in ShareKinds (missing %v); publication is refused because it would drop still-valid tags — remedy: remove share.pending.json and run `amq-remote share --session %s --renew` for a fresh full window",
				missing, session)
		}
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
	// The generation is committed (renamed into place); the pending and
	// staged files are cleanup. Failures are propagated with the
	// committed-state distinction (verifier r4 P2-4) — the caller must
	// know the publication succeeded even if cleanup did not.
	p, _ := sharePaths(keyDir)
	if err := removeStateLeaf(p); err != nil {
		return err
	}
	stagedPath := filepath.Join(keyDir, stagedName)
	if err := removeStateLeaf(stagedPath); err != nil {
		return err
	}
	return nil
}

// removeStateLeaf removes one state leaf, propagating failures with the
// committed-state distinction (verifier r4 P2-4): the publication has
// already been renamed into place, so a failed cleanup must reach the
// operator without suggesting the generation is lost.
func removeStateLeaf(path string) error {
	err := os.Remove(path)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	// No fallback: a failed removal propagates with the committed-state
	// text for every file shape (r5 review P1-1 — the removeEmptyDir
	// fallback returned nil for a regular file, silently discarding the
	// failure; os.Remove already removes an empty directory itself).
	return fmt.Errorf("cleanup after publication: removing %s: %w (published generation is committed)", path, err)
}

// stagedName is the accumulation file for signed-but-unpublished tags.
const stagedName = "share.staged.json"

// reconcileStagedState heals or refuses leftovers, inside share, enroll
// and renew — explicit operator actions on exactly that state (architect
// ruling 10:09Z on r4 P2-3: doctor is REPORT-ONLY; it never deletes or
// rewrites anything, per CLAUDE.md's "cleanup is explicit" constraint):
//   - a complete new generation with pending/staged still on disk (crash
//     between publication and cleanup) is reconciled by removing them —
//     the publication is already the committed state;
//   - staged entries whose conditions do not match the current pending
//     window (superseded by a later --renew) are dropped;
//   - a corrupt staged file is refused with the file named and the
//     remedy (remove it, re-sign) instead of an unrecoverable error.
//
// Doctor uses reportStagedState below: same detection, zero mutation.
func reconcileStagedState(session, keyDir string, st *shareState) error {
	return reconcileStagedStateMutating(session, keyDir, st)
}

// reportStagedState is doctor's read-only view of the same leftovers:
// it detects what reconcileStagedState would heal or refuse and returns
// the row text ("" when nothing is stale), but NEVER writes, deletes or
// renames anything (architect ruling 10:09Z: doctor reports the leftover
// with the remedy "run amq-remote share to reconcile"). Round-9: it
// consumes the loader snapshot; the staged outcome is typed, never
// substring-matched from prose (r8 P2).
func reportStagedState(session, keyDir string, st *shareState) string {
	stagedPath := filepath.Join(keyDir, stagedName)
	switch st.Staged.State {
	case leafConfined:
		// Verifier r6 P2-1: the remedy must be EXECUTABLE and derives from
		// the typed outcome, not a hardcoded shape (r8 P2).
		return st.Staged.Confinement.Error() + "; " + confinedLeafRemedy(st.Staged.leafRead) + ", then run `amq-remote share --session " + session + "` to reconcile"
	case leafUnreadable:
		return "staged state at " + stagedPath + " is invalid (" + st.Staged.Err.Error() + "); remedy: remove " + stagedPath + " and re-sign the pending preimages (`amq-remote share --session " + session + "` reprints them)"
	}
	raw := stagedTags(st)
	if st.Enrolled.Gen != nil && st.Pending.readable() && genCoversPendingWindow(st.Enrolled.Gen, st.Pending.Tags, nil) {
		leftover := st.Staged.State == leafOK
		pendingPath, _ := sharePaths(keyDir)
		if pendingExists(pendingPath) {
			leftover = true
		}
		if leftover {
			return "published generation already covers the pending window; stale pending/staged leftovers remain; remedy: run `amq-remote share --session " + session + "` to reconcile"
		}
		return ""
	}
	// Window in progress: report superseded staged entries if any.
	if st.Staged.State == leafOK && st.Pending.readable() {
		conds := map[uint16]string{}
		for _, p := range st.Pending.Tags {
			conds[p.Kind] = p.Conditions
		}
		stale := 0
		for _, t := range raw {
			if conds[t.Kind] != t.Conditions {
				stale++
			}
		}
		if stale > 0 {
			return fmt.Sprintf("%d staged entr%s superseded by the current window; remedy: run `amq-remote share --session %s` to reconcile", stale, pluralYIes(stale), session)
		}
	}
	return ""
}

// confinedLeafRemedy derives the removal remedy from the leaf's SHAPE as
// the typed error recorded it (r8 P2: a symlink and a FIFO need different
// words; a directory a third).
func confinedLeafRemedy(l leafRead) string {
	switch {
	case l.Confinement != nil && strings.Contains(l.Confinement.Reason, "symlink"):
		return "remove the symlink at " + l.Path
	case l.Confinement != nil && strings.Contains(l.Confinement.Reason, "not a regular file"):
		return "remove " + l.Path + " (it is not a regular file)"
	default:
		return "remove " + l.Path
	}
}

// stagedTags returns the staged tags as a slice (loader snapshot; empty
// when the leaf is absent or unreadable — callers gate on the outcome).
func stagedTags(st *shareState) []shareTagFile {
	if st.Staged.State != leafOK {
		return nil
	}
	out := make([]shareTagFile, 0, len(st.Staged.Tags))
	for _, kind := range bodykey.ShareKinds {
		if t, ok := st.Staged.Tags[kind]; ok {
			out = append(out, t)
		}
	}
	return out
}

func pluralYIes(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

func pendingExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func genCoversPendingWindow(gen *enrolledGeneration, pending []shareTagFile, pendingErr error) bool {
	return pendingErr == nil && gen != nil && len(gen.Tags) > 0 &&
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
}

func reconcileStagedStateMutating(session, keyDir string, st *shareState) error {
	if st.Pending.Err != nil {
		return st.Pending.Err // malformed pending: refuse everywhere
	}
	var pending []shareTagFile
	if st.Pending.readable() {
		pending = st.Pending.Tags
	}
	gen := st.Enrolled.Gen
	genCoversPending := st.Pending.readable() && genCoversPendingWindow(gen, pending, nil)
	stagedPath := filepath.Join(keyDir, stagedName)
	if st.Staged.Err != nil {
		return stagedCorruptRefusal(session, st.Staged.leafRead)
	}
	if st.Staged.State == leafOK {
		tags := stagedTags(st)
		if genCoversPending {
			// Crash leftovers after publication: pending and staged are
			// both stale; the published generation is the authority. The
			// publication already happened, so this is the committed
			// state: cleanup failures are propagated (verifier r4 P2-4)
			// but never rolled back.
			p, _ := sharePaths(keyDir)
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("cleanup after publication: removing %s: %w (published generation is committed)", p, err)
			}
			if err := os.Remove(stagedPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("cleanup after publication: removing %s: %w (published generation is committed)", stagedPath, err)
			}
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
		// between the two removes): pending is stale too. Committed state;
		// cleanup failures propagate (verifier r4 P2-4).
		p, _ := sharePaths(keyDir)
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("cleanup after publication: removing %s: %w (published generation is committed)", p, err)
		}
	}
	return nil
}

// byKind converts a tag slice to the staged map shape.
func byKind(tags []shareTagFile) map[uint16]shareTagFile {
	m := make(map[uint16]shareTagFile, len(tags))
	for _, t := range tags {
		m[t.Kind] = t
	}
	return m
}

// The staged read was folded into loadShareState (r9): staged is read
// once by the loader; a corrupt staged doc is refused, not discarded
// (the typed refusal is stagedCorruptRefusal).

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
		// Round-9: ONE loader snapshot per session; every row derives from
		// its typed outcomes.
		st, lerr := loadShareState(keyDir)
		if lerr != nil {
			info["attestation_error"] = lerr.Error()
			out[e.Name()] = info
			continue
		}
		// Confined state leaves (enrolled/pending/staged) surface as the
		// attestation_error row: the typed refusal names the leaf (the
		// r4 P1-2 pin: doctor reports a symlinked state leaf by name).
		// body.pub stays its own body_pub_error row below.
		confined, progressKnownLater := false, true
		for _, l := range st.confinedLeaves() {
			if strings.HasSuffix(l.Path, "body.pub") {
				continue // its own body_pub_error row below
			}
			if strings.HasSuffix(l.Path, stagedName) {
				// Staged confinement keeps its own row + executable remedy
				// (the r6 P2-1 pin) and still completes the diagnostic.
				info["staged_error"] = l.Confinement.Error() + "; " + confinedLeafRemedy(l) + ", then run `amq-remote share --session " + e.Name() + "` to reconcile"
				progressKnownLater = false
				continue
			}
			info["attestation_error"] = l.Confinement.Error()
			out[e.Name()] = info
			confined = true
			break
		}
		if confined {
			out[e.Name()] = info
			continue
		}
		// r8 P1-2: the body.pub check runs BEFORE the key-presence branch —
		// it was unreachable whenever body.key was absent, so the mint
		// remedy (follow the leaf, exit 6) survived. The remedy derives
		// from the leaf SHAPE (r8 P2); when the key is absent the mint step
		// is named only AFTER the leaf is repaired.
		if st.BodyPub.State == leafConfined {
			info["body_pub_error"] = st.BodyPub.Confinement.Error()
			info["remedy"] = confinedLeafRemedy(st.BodyPub) + ", then re-run share/renew"
		}
		k, kerr := bodykey.Load(filepath.Join(keyDir, "body.key"))
		if kerr != nil {
			if errors.Is(kerr, os.ErrNotExist) {
				info["key_error"] = kerr.Error()
				if _, confined := info["body_pub_error"]; confined {
					// Mint would refuse (exit 6) until the leaf is repaired:
					// keep the remedy executable.
					info["remedy"] = confinedLeafRemedy(st.BodyPub) + ", then run `amq-remote share --session " + e.Name() + "` to mint the key"
				} else {
					info["remedy"] = "run `amq-remote share --session " + e.Name() + "` to mint one"
				}
			} else {
				// Corrupt/unusable key: never point at --renew — a new
				// window signed under a replacement key would not match
				// the enrolled credentials. Operator inspection first.
				info["key_error"] = kerr.Error()
				info["remedy"] = "inspect the key directory by hand; do NOT renew over a broken key"
			}
			out[e.Name()] = info
			continue
		}
		info["body_pubkey"] = k.PublicKeyHex()
		if st.Enrolled.Err != nil {
			// Name the file in leaf errors (verifier r3 P2-7) — typed, from
			// the loader outcome.
			info["attestation_error"] = st.Enrolled.Err.Error()
			out[e.Name()] = info
			continue
		}
		gen := st.Enrolled.Gen
		// Outstanding pending preimages are visible in BOTH branches: a
		// half-finished renewal keeps every old tag, so the enrolled tag
		// count alone cannot distinguish it from a healthy enrollment
		// (verifier r2 P1-3). While unsigned preimages remain, the remedy
		// is always plain `share`, never `--renew` — renewing rotates the
		// window and strands preimages the owner may already be signing.
		if st.Pending.Err != nil {
			// Malformed pending state is surfaced, never treated as absence
			// (codex r3 #2: doctor must not report a healthy state over a
			// corrupt window).
			info["attestation_error"] = st.Pending.Err.Error()
			out[e.Name()] = info
			continue
		}
		pendingTags := st.Pending.Tags
		outstanding := 0
		// progressKnown is the r7 P1 tri-state: progress over the staged
		// leaf is only reported when the leaf was actually read. An
		// unreadable staged leaf (corrupt or confined) sets it false —
		// doctor then reports "progress unknown" and never a signed count
		// it did not read, and the renewal-in-progress warning/attestation
		// paths that a zero default would fabricate are suppressed.
		progressKnown := true
		// Architect ruling 10:09Z (r4 P2-3): doctor is REPORT-ONLY — a
		// diagnostic never deletes or rewrites state (CLAUDE.md: cleanup is
		// explicit). Leftovers are reported as a row with the reconcile
		// remedy; the deletion runs only inside share/enroll/renew.
		// Verifier r6 P1-1: the leftover row is ADDITIVE — it joins the
		// report and the attestation/expiry/per-kind rows are still
		// emitted (an expired generation with leftovers must still say
		// expired). Only a genuine corrupt-staged refusal continues early,
		// and even that keeps the rows computed so far.
		leftoverRow := reportStagedState(e.Name(), keyDir, st)
		stagedUnusable := st.Staged.State == leafUnreadable || st.Staged.State == leafConfined || !progressKnownLater
		if stagedUnusable {
			// Corrupt or confined staged: surface the refusal and still
			// complete the diagnostic (attestation/expiry below). Progress
			// over unreadable state is UNKNOWN (r7 P1): no signed count is
			// printed and the renew remedy is suppressed — an unreadable
			// staged leaf must never read as a completed renewal.
			info["staged_error"] = leftoverRow
			progressKnown = false
		} else if leftoverRow != "" {
			info["leftover"] = leftoverRow
		}
		if progressKnown {
			outstanding = len(outstandingPending(pendingTags, st.Staged.Tags, gen))
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
			renewalInProgress := len(pendingTags) > 0
			if renewalInProgress && progressKnown && outstanding > 0 {
				info["attestation"] = fmt.Sprintf("enrolled, renewal in progress: %d of %d kinds signed (staged)", len(bodykey.ShareKinds)-outstanding, len(bodykey.ShareKinds))
				info["warning"] = fmt.Sprintf("renewal in progress: %d preimage(s) still unsigned; run `amq-remote share --session %s` to reprint them (do NOT renew: renewing rotates the window and the still-unsigned preimages)", outstanding, e.Name())
			} else if renewalInProgress && !progressKnown {
				// r8 P1-3: whether a renewal is in progress is knowable from
				// the pending window ALONE; only the COUNT needs the staged
				// leaf. The known fact stays; the unknown count is never
				// fabricated, and the do-NOT-renew warning survives.
				info["attestation"] = "enrolled"
				info["progress"] = "unknown: staged state unreadable (see staged_error)"
				info["warning"] = "renewal in progress: preimage count unknown (staged state unreadable); run `amq-remote share --session " + e.Name() + "` to reprint the preimages (do NOT renew: renewing rotates the window and the still-unsigned preimages)"
			} else {
				info["attestation"] = "enrolled"
			}
			renewalOutstanding := renewalInProgress && (progressKnown && outstanding > 0 || !progressKnown)
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
				} else if !progressKnown {
					// r7 P1: while progress is unknown the renew remedy is
					// suppressed — an unreadable staged leaf may be an
					// in-progress renewal; renewing would rotate the window
					// and strand preimages the owner may already be signing.
					shareRemedy = "run `amq-remote share --session " + e.Name() + "` (renewal state unreadable; do NOT renew until the staged leaf is repaired)"
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
		if st.Pending.readable() && progressKnown {
			info["attestation"] = fmt.Sprintf("pending: %d of %d kinds signed (staged)", len(bodykey.ShareKinds)-outstanding, len(bodykey.ShareKinds))
		} else if st.Pending.readable() && !progressKnown {
			// r7 P1: progress over unreadable staged state is UNKNOWN —
			// "pending: 5 of 5" over a corrupt leaf fabricated completion.
			info["attestation"] = "pending: progress unknown (staged state unreadable: see staged_error)"
			// r8 P1-3: a pending window alone proves a renewal is in
			// progress — the warning survives even when the count is
			// unknown; only a fabricated count is forbidden.
			if len(pendingTags) > 0 {
				info["warning"] = "renewal in progress: preimage count unknown (staged state unreadable); run `amq-remote share --session " + e.Name() + "` to reprint the preimages (do NOT renew: renewing rotates the window and the still-unsigned preimages)"
			}
		} else {
			info["attestation"] = "missing: run `amq-remote share --session " + e.Name() + "`"
		}
		// A refusal at the staged leaf (corrupt or symlinked) is an ERROR
		// row, not an attestation state — it must never overwrite the
		// attestation line computed above (verifier r6 P1-1: rows are
		// additive; only the attestation FACT itself may occupy "attestation").
		out[e.Name()] = info
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
