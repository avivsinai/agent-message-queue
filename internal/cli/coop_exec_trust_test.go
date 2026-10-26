//go:build darwin || linux

package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/format"
	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/remote/claude"
	"golang.org/x/sys/unix"
)

// Bead agent-message-queue-vd4.3 (surface audit, codex-cli 0.163.0): a
// doorbell at Codex's "Trust this folder?" dialog quit Codex. coop exec now
// writes a Startup marker before exec, and the wake holds until Codex
// records the decision in config.toml, with no hook installed.
func TestCodexTrustDialogHoldsTheDoorbellUntilTrustIsRecorded(t *testing.T) {
	h, config, real, marker := newCodexTrustDialogHarness(t)
	// #1180 review: a directory saved as untrusted still opens on Codex's
	// restricted trust dialog, so it gets the marker too.
	untrusted := "[projects.\"" + real + "\"]\ntrust_level = \"untrusted\"\n"
	if err := os.WriteFile(config, []byte(untrusted), 0o600); err != nil {
		t.Fatal(err)
	}
	syncStartupMarkerForTest(t, h.root, []string{"-c", "check_for_update_on_startup=false", "-s", "read-only"})
	if err := os.Chtimes(marker, h.now, h.now); err != nil {
		t.Fatalf("no Startup marker for an untrusted checkout: %v", err)
	}

	h.send("a", format.PriorityUrgent)
	h.scan(h.cfg)
	if len(h.injected) != 0 {
		t.Fatalf("typed %q into the trust dialog", h.injected)
	}

	// #1180 review: a half-written config.toml proves nothing, so the hold
	// stays.
	if err := os.WriteFile(config, []byte("[projects.\""), 0o600); err != nil {
		t.Fatal(err)
	}
	h.advance(wakeDialogRecheck)
	h.scan(h.cfg)
	if len(h.injected) != 0 {
		t.Fatalf("typed %q after the trust file became unreadable", h.injected)
	}
	// PR #1204 Codex review: another terminal drains during the dialog.
	// That drain follows no doorbell typed after the release, so it does
	// not prove that Codex accepts input.
	h.drain()
	h.advance(wakeDialogRecheck)
	h.scan(h.cfg)
	if len(h.injected) != 0 {
		t.Fatalf("typed %q into the trust dialog after another terminal drained", h.injected)
	}
	h.send("late", format.PriorityUrgent)

	trusted := "[projects.\"" + real + "\"]\ntrust_level = \"trusted\"\n"
	if err := os.WriteFile(config, []byte(trusted), 0o600); err != nil {
		t.Fatal(err)
	}
	h.advance(wakeDialogRecheck)
	h.scan(h.cfg)
	if got := h.rings(); got != 1 {
		t.Fatalf("full doorbells after trust was recorded = %d, want 1", got)
	}
	// Bead agent-message-queue-vd4.3, live on 0.107.0 without the Codex
	// hook: the doorbell typed on the trust release reached a Codex whose
	// composer was not ready yet and was lost; the retries were a bare
	// Enter into an empty composer. The retry rings the full doorbell.
	h.advance(wakeDoorbellRetryBase)
	h.scan(h.cfg)
	if got := h.rings(); got != 1 {
		t.Fatalf("full doorbells on the retry after a trust release = %d, want 1", got)
	}
	// The next launch with trusted input and a known-safe update decision
	// removes the leftover trust marker.
	syncStartupMarkerForTest(t, h.root, []string{"-c", "check_for_update_on_startup=false"})
	if _, err := os.Lstat(marker); !os.IsNotExist(err) {
		t.Fatalf("Startup marker left after a launch with no dialog: %v", err)
	}

	// #1180 ruling: a directory that stays untrusted has no readiness
	// signal without the Codex SessionStart hook; after 10 minutes the
	// existing aging applies: input still held, attention output instead.
	if err := os.WriteFile(config, []byte(untrusted), 0o600); err != nil {
		t.Fatal(err)
	}
	syncStartupMarkerForTest(t, h.root, []string{"-c", "check_for_update_on_startup=false"})
	if err := os.Chtimes(marker, h.now, h.now); err != nil {
		t.Fatal(err)
	}
	rang := len(h.injected)
	h.send("b", format.PriorityUrgent)
	h.advance(wakeDialogQuietHold)
	h.scan(h.cfg)
	if len(h.injected) != rang || h.cfg.dialogHold != wakeDialogUnknown {
		t.Fatalf("aged untrusted hold: injected %q, dialog state %v; want held with attention", h.injected[rang:], h.cfg.dialogHold)
	}
}

// PR #1204 Codex review: after a trust release, only a drain that follows
// a doorbell actually typed into Codex proves that Codex accepts input.
// Each row ends at a retry that must still type the full doorbell.
func TestCodexTrustReleaseRetypesTheDoorbellUntilInputIsProven(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, h *wakeHoldHarness)
	}{
		{
			// An inbox file replaced in place is a new message, not a drain.
			name: "replacement after the release",
			run: func(t *testing.T, h *wakeHoldHarness) {
				h.send("a", format.PriorityUrgent)
				h.scan(h.cfg)
				if got := h.rings(); got != 1 {
					t.Fatalf("full doorbells on the trust release = %d, want 1", got)
				}
				h.drain()
				h.send("a", format.PriorityUrgent)
				h.scan(h.cfg)
				if got := h.rings(); got != 1 {
					t.Fatalf("full doorbells for the replaced message = %d, want 1", got)
				}
			},
		},
		{
			// A deferred attempt types nothing, so a drain after it proves
			// nothing.
			name: "deferred attempt then a drain",
			run: func(t *testing.T, h *wakeHoldHarness) {
				h.cfg.injectVia = "test-hook"
				h.cfg.injectViaHook = func(string) (string, error) {
					return "AMQ_INJECT_PROGRESS=deferred\n", errors.New("exit status 1")
				}
				h.send("a", format.PriorityUrgent)
				if err := notifyNewMessages(h.cfg); !isWakeInjectorDeferred(err) {
					t.Fatalf("release attempt error = %v, want deferred", err)
				}
				h.cfg.injectVia, h.cfg.injectViaHook = "", nil
				h.drain()
				h.scan(h.cfg)
				h.send("b", format.PriorityUrgent)
				h.scan(h.cfg)
				if got := h.rings(); got != 1 {
					t.Fatalf("full doorbells for new mail after the drain = %d, want 1", got)
				}
			},
		},
		{
			// A busy-input deferral types nothing, also when its attention
			// output is throttled.
			name: "busy-input deferral with attention throttled then a drain",
			run: func(t *testing.T, h *wakeHoldHarness) {
				old := waitForWakeInputQuiet
				t.Cleanup(func() { waitForWakeInputQuiet = old })
				waitForWakeInputQuiet = func(*wakeConfig) bool { return false }
				h.cfg.settings.deferWhileInput = true
				h.cfg.doorbell.recordTransientAttentionAttempt(h.now)
				h.send("a", format.PriorityUrgent)
				h.scan(h.cfg)
				if len(h.injected) != 0 {
					t.Fatalf("typed %q while the input was busy", h.injected)
				}
				waitForWakeInputQuiet = old
				h.cfg.settings.deferWhileInput = false
				h.drain()
				h.scan(h.cfg)
				h.send("b", format.PriorityUrgent)
				h.scan(h.cfg)
				if got := h.rings(); got != 1 {
					t.Fatalf("full doorbells for new mail after the drain = %d, want 1", got)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, config, real, marker := newCodexTrustDialogHarness(t)
			syncStartupMarkerForTest(t, h.root, []string{"-c", "check_for_update_on_startup=false"})
			if _, err := os.Lstat(marker); err != nil {
				t.Fatalf("no Startup marker for an unrecorded checkout: %v", err)
			}
			// Codex records trust, but no SessionStart hook removes the
			// marker: a trust release.
			trusted := "[projects.\"" + real + "\"]\ntrust_level = \"trusted\"\n"
			if err := os.WriteFile(config, []byte(trusted), 0o600); err != nil {
				t.Fatal(err)
			}
			tc.run(t, h)
			h.advance(wakeDoorbellRetryBase)
			h.scan(h.cfg)
			if got := h.rings(); got != 1 {
				t.Fatalf("full doorbells on the retry = %d, want 1", got)
			}
		})
	}
}

// Live run on amq 0.110.0, 2026-10-25 (codex-cli 0.145.0 and 0.163.0): in a
// folder outside any git checkout Codex asks whether to trust it on every
// start and records nothing, but coop exec wrote no Startup marker. The
// doorbell answered the dialog, Codex quit, and the rest of the doorbell ran
// in the parent shell.
func TestCodexStartOutsideAGitCheckoutHoldsTheDoorbell(t *testing.T) {
	h, _, real, marker := newCodexTrustDialogHarness(t)
	if err := os.Remove(filepath.Join(real, ".git")); err != nil {
		t.Fatal(err)
	}
	syncStartupMarkerForTest(t, h.root, []string{"-c", "check_for_update_on_startup=false"})
	if err := os.Chtimes(marker, h.now, h.now); err != nil {
		t.Fatalf("no Startup marker outside a git checkout: %v", err)
	}
	h.send("a", format.PriorityUrgent)
	h.scan(h.cfg)
	if len(h.injected) != 0 {
		t.Fatalf("typed %q into the trust dialog of a folder outside git", h.injected)
	}
}

// Live run on amq 0.110.0, 2026-10-25 (codex-cli 0.163.0): a seat restarted
// in an already trusted checkout showed Codex's update prompt, whose default
// choice runs brew upgrade, and coop exec wrote no Startup marker. A seat
// whose installed version AMQ cannot verify holds the
// doorbell, Codex's trust record does not release it, and the Codex dialog
// hook's first-turn close does.
func TestCodexUnknownVersionOnATrustedSeatHoldsTheDoorbell(t *testing.T) {
	h, config, real, marker := newCodexTrustDialogHarness(t)
	trusted := "[projects.\"" + real + "\"]\ntrust_level = \"trusted\"\n"
	if err := os.WriteFile(config, []byte(trusted), 0o600); err != nil {
		t.Fatal(err)
	}
	// An explicit CLI setting is not proof that higher policy permits it.
	// Unlike the trust-only fixtures, this calls the full startup producer.
	if err := syncStartupDialogMarker(h.root, "codex", filepath.Join(t.TempDir(), "codex"), []string{"-c", "check_for_update_on_startup=false"}, false); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(marker, h.now, h.now); err != nil {
		t.Fatalf("no Startup marker for a possible update prompt: %v", err)
	}
	h.send("a", format.PriorityUrgent)
	h.scan(h.cfg)
	if len(h.injected) != 0 {
		t.Fatalf("typed %q into a possible update prompt", h.injected)
	}

	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	h.advance(wakeDialogRecheck)
	h.scan(h.cfg)
	if got := h.rings(); got != 1 {
		t.Fatalf("full doorbells after the dialog hook closed the marker = %d, want 1", got)
	}
}

// Bead agent-message-queue-uue (live check on codex-cli 0.164.0, findings 1
// and 3): after the person trusts the folder, Codex 0.164 shows "Hooks need
// review", so a trust record no longer means the seat accepts input. A
// 0.164 or unverified seat starts clear only when the update prompt is
// ruled out, the directory trust is recorded, and Codex's own hooks/list
// shows no hook to review; otherwise it gets the marker only the dialog
// hook releases.
func TestCodex164StartupNeedsTrustAndNativeHookReview(t *testing.T) {
	hooks := func(statuses ...string) string {
		var list []string
		for _, status := range statuses {
			list = append(list, codexHookForTest(status))
		}
		return `{"id":2,"result":{"data":[{"cwd":%q,"hooks":[` + strings.Join(list, ",") + `],"warnings":[],"errors":[]}]}}`
	}
	const noRequirements = `{"id":3,"result":{"supportsIndependentSpeedModes":true,"requirements":null}}`
	for _, tc := range []struct {
		name         string
		version      string
		hooks        string // the hooks/list reply line, %s the checkout
		requirements string // the configRequirements/read reply line
		config       string // the config/read reply line, or "" for the seat's -c false in effect
		kind         string // the marker kind, or "" for none
		// linkHooks, when set, is the hooks/list reply for a logical
		// spelling of the checkout (%s that spelling): a symlink that is the
		// working directory, or with cdTopLevel the -C directory below a
		// top-level symlink.
		linkHooks  string
		cdTopLevel bool
	}{
		{"every hook trusted", "0.164.0", hooks("trusted"), noRequirements, "", "", "", false},
		{"managed hooks only", "0.164.0", hooks("managed"), noRequirements, "", "", "", false},
		{"one hook untrusted", "0.164.0", hooks("trusted", "untrusted"), noRequirements, "", wakeStartupKindReview, "", false},
		{"hooks/list error", "0.164.0", `{"id":2,"error":{"code":-32603,"message":"failed to load hooks for %s"}}`, noRequirements, "", wakeStartupKindReview, "", false},
		// Review finding (bead agent-message-queue-uue): from a symlinked
		// working directory AMQ asked hooks/list for the logical path, while
		// the TUI asks for its physical cwd, where the project hooks are
		// untrusted.
		{"symlinked cwd, physical project hooks untrusted", "0.164.0", hooks("untrusted"), noRequirements, "", wakeStartupKindReview, hooks("trusted"), false},
		// Review finding (bead agent-message-queue-uue): the TUI
		// canonicalizes a -C directory and keeps the logical spelling only
		// below a symlink with a grandparent (utils/absolute-path/src/lib.rs:
		// 210-227 at rust-v0.164.0), so under macOS /var it asks for the
		// /private realpath, where the project hooks are untrusted.
		{"-C under a top-level symlink, realpath project hooks untrusted", "0.164.0", hooks("untrusted"), noRequirements, "", wakeStartupKindReview, hooks("trusted"), true},
		// Field: every 0.164 seat held until typed after v0.112.0 (bead
		// agent-message-queue-uue). The seat carries AMQ's
		// check_for_update_on_startup=false, which no requirement pins on,
		// so no update prompt can show on any version.
		{"unverified release, update check off, requirements clear", "0.165.0", hooks("trusted"),
			`{"id":3,"result":{"supportsIndependentSpeedModes":true,"requirements":{"checkForUpdateOnStartup":null,"allowManagedHooksOnly":null}}}`, "", "", "", false},
		{"requirements pin the update check on", "0.165.0", hooks("trusted"),
			`{"id":3,"result":{"supportsIndependentSpeedModes":true,"requirements":{"checkForUpdateOnStartup":true}}}`, "", wakeStartupKindUpdate, "", false},
		// Review finding (bead agent-message-queue-uue): legacy managed
		// layers rank above session flags (config/src/config_layer_source.rs:
		// 22-27 at rust-v0.164.0), so the seat's -c false can lose; only the
		// effective value counts.
		{"a legacy managed layer turns the update check on", "0.165.0", hooks("trusted"), noRequirements,
			`{"id":4,"result":{"config":{"check_for_update_on_startup":true},"origins":{"check_for_update_on_startup":{"name":{"type":"legacyManagedConfigTomlFromFile","file":"/etc/codex/managed_config.toml"},"version":"sha256:ab"}}}}`,
			wakeStartupKindUpdate, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, config, real, marker := newCodexTrustDialogHarness(t)
			trusted := "[projects.\"" + real + "\"]\ntrust_level = \"trusted\"\n"
			if err := os.WriteFile(config, []byte(trusted), 0o600); err != nil {
				t.Fatal(err)
			}
			byCwd := map[string]string{"": fmt.Sprintf(tc.hooks, real)}
			args := []string{"-c", "check_for_update_on_startup=false"}
			switch {
			case tc.cdTopLevel:
				// /var is a top-level symlink to /private/var on macOS.
				base, err := os.MkdirTemp("/var/tmp", "amq-cd-")
				if err != nil {
					t.Skip("no /var/tmp here")
				}
				t.Cleanup(func() { _ = os.RemoveAll(base) })
				logical := filepath.Join(base, "c")
				if err := os.MkdirAll(filepath.Join(logical, ".git"), 0o700); err != nil {
					t.Fatal(err)
				}
				canonical, err := filepath.EvalSymlinks(logical)
				if err != nil {
					t.Fatal(err)
				}
				if canonical == logical {
					t.Skip("/var is not a symlink here")
				}
				trusted = "[projects.\"" + logical + "\"]\ntrust_level = \"trusted\"\n[projects.\"" + canonical + "\"]\ntrust_level = \"trusted\"\n"
				if err := os.WriteFile(config, []byte(trusted), 0o600); err != nil {
					t.Fatal(err)
				}
				byCwd[""] = fmt.Sprintf(tc.hooks, canonical)
				byCwd[logical] = fmt.Sprintf(tc.linkHooks, logical)
				args = append(args, "-C", logical)
			case tc.linkHooks != "":
				link := filepath.Join(t.TempDir(), "link")
				if err := os.Symlink(real, link); err != nil {
					t.Fatal(err)
				}
				t.Chdir(link)
				trusted += "[projects.\"" + link + "\"]\ntrust_level = \"trusted\"\n"
				if err := os.WriteFile(config, []byte(trusted), 0o600); err != nil {
					t.Fatal(err)
				}
				byCwd[link] = fmt.Sprintf(tc.linkHooks, link)
			}
			bin := fakeCodexForTest(t, filepath.Dir(config), tc.version, byCwd, tc.requirements, tc.config)
			if err := syncStartupDialogMarker(h.root, "codex", bin, args, false); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(marker)
			if tc.kind == "" {
				if !os.IsNotExist(err) {
					t.Fatalf("Startup marker %q, %v; want none for a clear start", raw, err)
				}
				return
			}
			var got wakeDialogMarker
			if err != nil || json.Unmarshal(raw, &got) != nil || got.Kind != tc.kind {
				t.Fatalf("Startup marker %q, %v; want kind %s", raw, err, tc.kind)
			}
		})
	}
}

// Bead agent-message-queue-uue (live check on codex-cli 0.164.0, finding
// 1): Codex wrote the folder's trust record while its "Hooks need review"
// screen was still on screen. A 0.164 marker stays held after the trust
// record is written.
func TestCodex164MarkerIsNotReleasedByTheTrustRecord(t *testing.T) {
	h, config, real, marker := newCodexTrustDialogHarness(t)
	reply := fmt.Sprintf(`{"id":2,"result":{"data":[{"cwd":%q,"hooks":[%s],"warnings":[],"errors":[]}]}}`, real, codexHookForTest("trusted"))
	bin := fakeCodexForTest(t, filepath.Dir(config), "0.164.0", map[string]string{"": reply}, `{"id":3,"result":{"requirements":null}}`, "")
	if err := syncStartupDialogMarker(h.root, "codex", bin, []string{"-c", "check_for_update_on_startup=false"}, false); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(marker)
	var got wakeDialogMarker
	if err != nil || json.Unmarshal(raw, &got) != nil || got.Kind != wakeStartupKindReview {
		t.Fatalf("Startup marker %q, %v; want kind %s for an untrusted 0.164 checkout", raw, err, wakeStartupKindReview)
	}
	if err := os.Chtimes(marker, h.now, h.now); err != nil {
		t.Fatal(err)
	}
	trusted := "[projects.\"" + real + "\"]\ntrust_level = \"trusted\"\n"
	if err := os.WriteFile(config, []byte(trusted), 0o600); err != nil {
		t.Fatal(err)
	}
	h.send("a", format.PriorityUrgent)
	h.advance(wakeDialogRecheck)
	h.scan(h.cfg)
	if len(h.injected) != 0 {
		t.Fatalf("typed %q after the trust record, while Codex may review hooks", h.injected)
	}
}

// Bead agent-message-queue-uue (live check on codex-cli 0.164.0, finding
// 3): with the AMQ hook installed but not trusted, doctor's held-doorbell
// line named amq wake install-dialog-hook; the remedy is to trust the hook
// in Codex's /hooks.
func TestCodexHoldHintNamesHooksForAnUntrustedDialogHook(t *testing.T) {
	clearSendMailboxTestEnv(t)
	home := t.TempDir()
	codexHome := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", codexHome)
	codexRequirementsPath = filepath.Join(home, "no-requirements.toml")
	t.Cleanup(func() { codexRequirementsPath = "/etc/codex/requirements.toml" })
	bin := filepath.Join(home, "amq")
	if err := os.WriteFile(bin, nil, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := codexDialogStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := claude.InstallCommandHooksAt(filepath.Join(codexHome, "hooks.json"), codexDialogHooks(bin, store)); err != nil {
		t.Fatal(err)
	}
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	raw := `{"schema":1,"event":"Startup","harness":"codex","kind":"hook_review","cwd":"/work","opened_at":"x"}` + "\n"
	if err := os.WriteFile(filepath.Join(fsq.AgentBase(root, "codex"), wakeDialogFileName), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	hint, ok := codexStartupHoldHint(root, "codex")
	if !ok || !strings.Contains(hint.Message, "open /hooks in Codex") || strings.Contains(hint.Message, "install-dialog-hook") {
		t.Fatalf("held-doorbell hint = %+v, %v; want the /hooks remedy", hint, ok)
	}
}

// fakeCodexForTest writes a codex that reports version and answers
// hooks/list, configRequirements/read and config/read on a stdio
// app-server with the given reply lines (hooks by the requested cwd, ""
// for any other), plus a version cache in codexHome
// that names 0.164.0 as the latest release, checked now. An empty config
// answers as Codex 0.164 does for a seat whose -c
// check_for_update_on_startup=false is in effect.
func fakeCodexForTest(t *testing.T, codexHome, version string, hooks map[string]string, requirements, config string) string {
	t.Helper()
	cache := fmt.Sprintf(`{"latest_version":"0.164.0","last_checked_at":%q,"dismissed_version":null}`, time.Now().UTC().Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(codexHome, codexVersionFile), []byte(cache), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if config == "" {
		config = `{"id":4,"result":{"config":{"check_for_update_on_startup":false},"origins":{"check_for_update_on_startup":{"name":{"type":"sessionFlags"},"version":"sha256:ab"}}}}`
	}
	configFile := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configFile, []byte(config+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// One case per named cwd, before the case for any other hooks/list.
	var hooksCases []string
	for cwd, reply := range hooks {
		file := filepath.Join(dir, fmt.Sprintf("hooks%d.json", len(hooksCases)))
		if err := os.WriteFile(file, []byte(reply+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		pattern := `*'"method":"hooks/list"'*`
		if cwd != "" {
			pattern = `*'"cwds":["` + cwd + `"]'*`
		}
		hooksCases = append(hooksCases, "    "+pattern+") cat '"+file+"' ;;\n")
	}
	slices.SortFunc(hooksCases, func(a, b string) int { return strings.Compare(a, b) })
	requirementsFile := filepath.Join(dir, "requirements.json")
	if err := os.WriteFile(requirementsFile, []byte(requirements+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = --version ]; then printf 'codex-cli " + version + "\\n'; exit 0; fi\n" +
		"while IFS= read -r line; do\n" +
		"  case \"$line\" in\n" +
		"    *'\"method\":\"initialize\"'*) printf '{\"id\":1,\"result\":{\"userAgent\":\"fake\"}}\\n' ;;\n" +
		strings.Join(hooksCases, "") +
		"    *'\"method\":\"configRequirements/read\"'*) cat '" + requirementsFile + "' ;;\n" +
		"    *'\"method\":\"config/read\"'*) cat '" + configFile + "' ;;\n" +
		"  esac\n" +
		"done\n"
	bin := filepath.Join(dir, "codex")
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin
}

// codexHookForTest is one hooks/list entry in the shape Codex 0.164 sends
// (app-server-protocol v2 HookMetadata). A managed hook comes from a
// system requirements layer.
func codexHookForTest(trust string) string {
	source, managed := `"sourcePath":"/home/u/.codex/hooks.json","source":"user"`, "false"
	if trust == "managed" {
		source, managed = `"sourcePath":"/etc/codex/requirements.toml","source":"system"`, "true"
	}
	return `{"key":"/home/u/.codex/hooks.json:user_prompt_submit:0:0","eventName":"userPromptSubmit","matcher":null,"timeoutSec":5,` +
		`"statusMessage":null,"additionalContextLimit":null,` + source + `,"pluginId":null,` +
		`"displayOrder":0,"enabled":true,"isManaged":` + managed + `,"currentHash":"sha256:ab","trustStatus":"` + trust + `","handlerType":"command",` +
		`"command":"true","async":false}`
}

// syncStartupMarkerForTest exercises the trust decision and marker IO with
// the independent update decision already known safe. Update uncertainty
// is covered through the full producer by the unknown-version test.
func syncStartupMarkerForTest(t *testing.T, root string, args []string) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	harness, dir, trust := startupTrustDialog("/usr/local/bin/codex", args, wd)
	marker, hold := startupDialogMarkerDecision(harness, dir, trust, true)
	if err := syncStartupDialogMarkerState(root, "codex", marker, hold); err != nil {
		t.Fatal(err)
	}
}

// newCodexTrustDialogHarness returns a wake harness for a Codex agent in a
// fresh git checkout, with the dialog sources reading the agent's marker,
// plus the Codex config.toml path, the checkout's real path, and the
// marker path.
func newCodexTrustDialogHarness(t *testing.T) (h *wakeHoldHarness, config, real, marker string) {
	t.Helper()
	h = newWakeHoldHarness(t, wakeHoldPolicy{})
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	project := secureTempDirForTest(t)
	if err := os.Mkdir(filepath.Join(project, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)
	real, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	agentDir, err := openWakeAgentDir(h.root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agentDir.Close() })
	h.cfg.dialogSource = wakeDialogSourceInDir(agentDir)
	h.cfg.startupTrustRelease = wakeStartupTrustReleaseInDir(agentDir)
	return h, filepath.Join(codexHome, "config.toml"), real, filepath.Join(fsq.AgentBase(h.root, "codex"), wakeDialogFileName)
}

// #1180 review: coop exec wrote or removed the Startup marker before
// prepareCoopWakeLock refused a launch for another live owner, so a refused
// launch from a trusted directory cleared the running owner's trust hold.
func TestRefusedCoopLaunchKeepsTheLiveOwnersStartupMarker(t *testing.T) {
	clearSendMailboxTestEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	project := secureTempDirForTest(t)
	real, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	trusted := `{"projects":{"` + real + `":{"hasTrustDialogAccepted":true}}}`
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(trusted), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	root := secureTempDirForTest(t)
	// The test process is running and is no amq wake: a live owner coop
	// exec must not touch.
	writeWakeLockExactForTest(t, root, "claude", wakeLock{PID: os.Getpid(), Root: canonicalWakeRoot(root), Agent: "claude"})
	marker := filepath.Join(fsq.AgentBase(root, "claude"), wakeDialogFileName)
	owners := []byte(`{"schema":1,"event":"Startup","harness":"claude","cwd":"/elsewhere","opened_at":"x"}` + "\n")
	if err := os.WriteFile(marker, owners, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = stubCoopExecSentinel(t)

	if err := runCoopExec([]string{"--root", root, "--me", "claude", "-y", "claude"}); err == nil {
		t.Fatal("coop exec started beside a live wake owner")
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != string(owners) {
		t.Fatalf("live owner's Startup marker = %q, %v; want it unchanged", got, err)
	}
}

// #1180 review: coop exec wrote the Startup marker only after its wake was
// ready, so a message that arrived in between was typed into the terminal,
// waited there, and answered the trust dialog when the harness started.
// The wake now admits no input until coop exec releases its startup hold,
// which it does after the marker is in place.
func TestCoopWakeTypesNothingBeforeTheStartupHoldIsReleased(t *testing.T) {
	h := newWakeHoldHarness(t, wakeHoldPolicy{})
	childEnd, hold, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hold.Close() })
	fd, err := unix.Dup(int(childEnd.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	_ = childEnd.Close()
	released := adoptWakeStartupHoldForTest(t, hold, fd)
	agentDir, err := openWakeAgentDir(h.root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agentDir.Close() })
	h.cfg.dialogSource = withWakeStartupHold(wakeDialogSourceInDir(agentDir), released)

	h.send("a", format.PriorityUrgent)
	h.scan(h.cfg)
	if len(h.injected) != 0 {
		t.Fatalf("typed %q before coop exec released the startup hold", h.injected)
	}
	_ = hold.Close()
	<-released
	h.advance(wakeDialogRecheck)
	h.scan(h.cfg)
	if got := h.rings(); got != 1 {
		t.Fatalf("full doorbells after the startup hold was released = %d, want 1", got)
	}
}

// Lead ruling on #1180: a trust hold that never closes (for example a Codex
// directory kept untrusted, without the Codex hook) must be visible:
// amq wake check reports it, its age, and what releases it.
func TestWakeCheckReportsAnOpenTrustHold(t *testing.T) {
	clearSendMailboxTestEnv(t)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	root := secureTempDirForTest(t)
	if err := fsq.EnsureRootDirs(root); err != nil {
		t.Fatal(err)
	}
	if err := fsq.EnsureAgentDirs(root, "codex"); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(fsq.AgentBase(root, "codex"), wakeDialogFileName)
	raw := `{"schema":1,"event":"Startup","harness":"codex","cwd":"/work","opened_at":"x"}` + "\n"
	if err := os.WriteFile(marker, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	opened := time.Now().Add(-11 * time.Minute)
	if err := os.Chtimes(marker, opened, opened); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error {
		return runWake([]string{"check", "--me", "codex", "--root", root, "--json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		StartupHold *wakeStartupHoldStatus `json:"startup_hold"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("wake check JSON %q: %v", out, err)
	}
	hold := result.StartupHold
	if hold == nil || hold.Harness != "codex" || hold.Dir != "/work" || hold.AgeSeconds < 600 ||
		!strings.Contains(hold.Release, "install-dialog-hook --harness codex") {
		t.Fatalf("startup hold = %#v", hold)
	}

	// #1180 review: wake check reported a marker as held after Codex had
	// recorded trust and the wake had released input.
	project := secureTempDirForTest(t)
	if err := os.Mkdir(filepath.Join(project, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	trusted := "[projects.\"" + real + "\"]\ntrust_level = \"trusted\"\n"
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(trusted), 0o600); err != nil {
		t.Fatal(err)
	}
	accepted := `{"schema":1,"event":"Startup","harness":"codex","cwd":"` + real + `","opened_at":"x"}` + "\n"
	if err := os.WriteFile(marker, []byte(accepted), 0o600); err != nil {
		t.Fatal(err)
	}
	if hold := inspectWakeStartupHold(root, "codex", time.Now()); hold != nil {
		t.Fatalf("startup hold after trust was recorded = %#v, want none", hold)
	}
}

// #1180 review: a wake restart or self-upgrade during the trust dialog
// exec'd a successor without the startup hold, which then typed into the
// dialog. The restart passes the hold on; the successor holds until coop
// exec releases it.
func TestWakeRestartKeepsAPendingStartupHold(t *testing.T) {
	childEnd, hold, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hold.Close() })
	fd, err := unix.Dup(int(childEnd.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	_ = childEnd.Close()
	adoptWakeStartupHoldForTest(t, hold, fd)
	successorEnv := carryWakeStartupHold(nil)
	var value string
	for _, entry := range successorEnv {
		if v, found := strings.CutPrefix(entry, envWakeStartupHold+"="); found {
			value = v
		}
	}
	if value == "" {
		t.Fatalf("restart env %q carries no startup hold", successorEnv)
	}
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC != 0 {
		t.Fatalf("startup hold fd flags = %#x, %v; it would close at the restart exec", flags, err)
	}
	// After exec the successor alone owns the carried descriptor. In this one
	// process it adopts a duplicate, so each adopted file closes its own.
	carried, err := strconv.Atoi(value)
	if err != nil || carried != fd {
		t.Fatalf("restart env carries startup hold %q, want fd %d", value, fd)
	}
	successorFD, err := unix.Dup(carried)
	if err != nil {
		t.Fatal(err)
	}
	released := adoptWakeStartupHoldForTest(t, hold, successorFD)
	select {
	case <-released:
		t.Fatal("successor released the hold before coop exec did")
	case <-time.After(50 * time.Millisecond):
	}
	_ = hold.Close()
	<-released
}

// adoptWakeStartupHoldForTest adopts fd as a wake's startup hold and, once
// hold is closed and the wake saw the release, closes the adopted file.
// A CI run failed TestCoopWakeAllModesInstallExactOwnerSupervisor with
// "stat .wake.lifecycle.lock: bad file descriptor": the restart test had
// adopted one fd number into two files, and the second finalizer closed
// that number after a later test reused it for its lock. Each adoption here
// owns its own descriptor and closes it exactly once.
func adoptWakeStartupHoldForTest(t *testing.T, hold *os.File, fd int) <-chan struct{} {
	t.Helper()
	t.Setenv(envWakeStartupHold, strconv.Itoa(fd))
	released, err := wakeStartupHoldFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	adopted := wakeStartupHoldFile
	t.Cleanup(func() {
		_ = hold.Close()
		<-released
		_ = adopted.Close()
		wakeStartupHoldFile = nil
	})
	return released
}

// #1190 review: a remover read the marker, compared it, then unlinked
// whatever was there, so a writer that replaced the marker in between lost
// its hold. Replacement and conditional removal now share one lock.
func TestMarkerReplacedDuringARemovalSurvives(t *testing.T) {
	root := secureTempDirForTest(t)
	if err := fsq.EnsureAgentDirs(root, "agy"); err != nil {
		t.Fatal(err)
	}
	agentDir, err := openWakeAgentDir(root, "agy")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agentDir.Close() })
	marker := filepath.Join(fsq.AgentBase(root, "agy"), wakeDialogFileName)
	if err := os.WriteFile(marker, []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	newer := []byte(`{"schema":1,"event":"PermissionRequest","opened_at":"x"}` + "\n")
	replaced := make(chan struct{})
	err = agentDir.withFD(func(dirfd int) error {
		return removeWakeDialogMarkerIf(dirfd, agentDir, func(current []byte) bool {
			// Another writer replaces the marker between the compare and
			// the unlink.
			go func() {
				defer close(replaced)
				_ = agentDir.withFD(func(dirfd int) error {
					return replaceWakeDialogMarkerAt(dirfd, agentDir, newer)
				})
			}()
			select {
			case <-replaced:
			case <-time.After(200 * time.Millisecond):
			}
			return string(current) == "mine\n"
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	<-replaced
	if got, err := os.ReadFile(marker); err != nil || string(got) != string(newer) {
		t.Fatalf("marker after a replacement during removal = %q, %v; want the newer marker", got, err)
	}
}
