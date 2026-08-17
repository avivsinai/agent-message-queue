package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const cmuxLiveInspectTimeout = 15 * time.Second

func TestCmuxLiveDiscoverProbe(t *testing.T) {
	if os.Getenv("AMQ_CMUX_LIVE") != "1" {
		t.Skip("AMQ_CMUX_LIVE=1 required; run from a shell inside a cmux surface")
	}
	skipCmuxNonDarwin(t)

	path, err := (Cmux{}).executable()
	if err != nil {
		t.Skipf("cmux CLI unavailable: %v", err)
	}
	precheckArgs := []string{"--id-format", "uuids", "--json", "list-workspaces"}
	t.Logf("cmux precheck binary=%s args=%s", path, strings.Join(precheckArgs, " "))
	start := time.Now()
	beforeRaw, err := cmuxLiveRun(path, cmuxLiveInspectTimeout, precheckArgs...)
	elapsed := time.Since(start)
	t.Logf("cmux precheck duration=%s output=%q err=%v", elapsed, beforeRaw, err)
	if err != nil {
		detail := strings.TrimSpace(beforeRaw + ": " + err.Error())
		if cmuxLiveSocketDenied(beforeRaw, err) {
			t.Skipf("cmux socket denied: %s", detail)
		}
		t.Fatalf("cmux list-workspaces precheck failed: binary=%s args=%s duration=%s output=%q err=%v", path, strings.Join(precheckArgs, " "), elapsed, beforeRaw, err)
	}
	before, err := parseCmuxLiveWorkspaces(beforeRaw)
	if err != nil {
		t.Fatalf("parse list-workspaces: %v raw=%s", err, beforeRaw)
	}
	beforeIDs := cmuxLiveIDs(before)
	beforeSelected := cmuxLiveSelected(before)
	beforeCount := len(before)
	var workspaceID string
	t.Cleanup(func() {
		listedRaw, listErr := cmuxLiveRun(path, 30*time.Second, "--id-format", "uuids", "--json", "list-workspaces")
		if listErr != nil {
			t.Errorf("cleanup list-workspaces: %s", strings.TrimSpace(listedRaw+": "+listErr.Error()))
			return
		}
		listed, parseErr := parseCmuxLiveWorkspaces(listedRaw)
		if parseErr != nil {
			t.Errorf("cleanup parse list-workspaces: %v", parseErr)
			return
		}
		for _, id := range cmuxLiveIDs(listed) {
			if containsString(beforeIDs, id) {
				continue
			}
			if _, closeErr := cmuxLiveRun(path, 30*time.Second, "close-workspace", "--workspace", id); closeErr != nil {
				t.Errorf("cleanup close %s: %v", id, closeErr)
			}
		}
		afterRaw, afterErr := cmuxLiveRun(path, cmuxLiveInspectTimeout, "--id-format", "uuids", "--json", "list-workspaces")
		if afterErr != nil {
			t.Errorf("cleanup recount: %s", strings.TrimSpace(afterRaw+": "+afterErr.Error()))
			return
		}
		after, afterParse := parseCmuxLiveWorkspaces(afterRaw)
		if afterParse != nil {
			t.Errorf("cleanup parse recount: %v", afterParse)
			return
		}
		if len(after) != beforeCount {
			t.Errorf("live workspace count not restored: before=%d after=%d", beforeCount, len(after))
		}
		if beforeSelected == "" || !containsString(cmuxLiveIDs(after), beforeSelected) {
			return
		}
		if cmuxLiveSelected(after) == beforeSelected {
			return
		}
		if _, selErr := cmuxLiveRun(path, cmuxLiveInspectTimeout, "select-workspace", "--workspace", beforeSelected); selErr != nil {
			t.Errorf("cleanup restore selected %s: %v", beforeSelected, selErr)
		}
	})

	name := "amq-aop1-" + strings.ReplaceAll(t.Name(), "/", "-")
	cwd := t.TempDir()
	ack, err := cmuxLiveRun(path, 30*time.Second, "--id-format", "uuids", "--json", "new-workspace", "--name", name, "--cwd", cwd, "--focus", "false")
	if err != nil {
		t.Fatalf("new-workspace: %s", strings.TrimSpace(ack+": "+err.Error()))
	}
	if !strings.HasPrefix(strings.TrimSpace(ack), "OK workspace:") {
		t.Fatalf("new-workspace ack = %q, want OK workspace:", ack)
	}
	listedRaw, err := cmuxLiveRun(path, cmuxLiveInspectTimeout, "--id-format", "uuids", "--json", "list-workspaces")
	if err != nil {
		t.Fatalf("list-workspaces after create: %s", strings.TrimSpace(listedRaw+": "+err.Error()))
	}
	listed, err := parseCmuxLiveWorkspaces(listedRaw)
	if err != nil {
		t.Fatal(err)
	}
	matches := make([]cmuxLiveWorkspace, 0, 1)
	for _, workspace := range listed {
		if workspace.Title == name {
			matches = append(matches, workspace)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("throwaway title %q matched %d workspaces, want 1", name, len(matches))
	}
	workspaceID = matches[0].ID
	surfaceID, err := cmuxLiveTerminalSurface(path, workspaceID)
	if err != nil {
		t.Fatal(err)
	}

	adapter := Cmux{
		Path: path,
		Getenv: func(key string) string {
			if key == "CMUX_SURFACE_ID" {
				return surfaceID
			}
			return os.Getenv(key)
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	target, err := adapter.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	gotID, err := parseCmuxSurfaceTarget(target)
	if err != nil {
		t.Fatalf("Discover target %q: %v", target, err)
	}
	wantID, err := normalizeCmuxSurfaceID(surfaceID)
	if err != nil {
		t.Fatal(err)
	}
	if gotID != strings.ToUpper(wantID) {
		t.Fatalf("Discover = %q, want throwaway surface %q (refusing to touch another surface)", target, surfaceID)
	}
	t.Logf("PASS Discover throwaway %s", target)
	entry, tty, err := cmuxLiveWaitForTerminalSurface(t, path, surfaceID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("throwaway system.tree entry=%s tty=%q", entry, tty)
	if err := adapter.Probe(ctx, target); err != nil {
		t.Fatalf("Probe() error = %v (null tty must still pass)", err)
	}
	t.Logf("PASS Probe throwaway %s", target)

	missing := "cmux:surface:00000000-0000-4000-8000-000000000000"
	if err := adapter.Probe(ctx, missing); !errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("Probe missing UUID error = %v, want ErrTargetNotFound", err)
	}

	if _, err := cmuxLiveRun(path, 30*time.Second, "close-workspace", "--workspace", workspaceID); err != nil {
		t.Fatalf("close throwaway: %v", err)
	}
	workspaceID = ""
	if err := adapter.Probe(ctx, target); !errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("Probe after close error = %v, want ErrTargetNotFound", err)
	}
	t.Logf("PASS Probe after close is not-found")
}

type cmuxLiveWorkspace struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Selected bool   `json:"selected"`
}

func parseCmuxLiveWorkspaces(raw string) ([]cmuxLiveWorkspace, error) {
	var parsed struct {
		Workspaces []cmuxLiveWorkspace `json:"workspaces"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, err
	}
	if parsed.Workspaces == nil {
		return []cmuxLiveWorkspace{}, nil
	}
	return parsed.Workspaces, nil
}

func cmuxLiveIDs(records []cmuxLiveWorkspace) []string {
	ids := make([]string, 0, len(records))
	for _, workspace := range records {
		ids = append(ids, strings.ToLower(workspace.ID))
	}
	return ids
}

func cmuxLiveSelected(records []cmuxLiveWorkspace) string {
	for _, workspace := range records {
		if workspace.Selected {
			return strings.ToLower(workspace.ID)
		}
	}
	return ""
}

func cmuxLiveTerminalSurface(path, workspaceID string) (string, error) {
	raw, err := cmuxLiveRun(path, cmuxLiveInspectTimeout, "--id-format", "uuids", "--json", "list-panes", "--workspace", workspaceID)
	if err != nil {
		return "", err
	}
	var panes struct {
		Panes []struct {
			ID string `json:"id"`
		} `json:"panes"`
	}
	if err := json.Unmarshal([]byte(raw), &panes); err != nil {
		return "", err
	}
	for _, pane := range panes.Panes {
		surfRaw, surfErr := cmuxLiveRun(path, cmuxLiveInspectTimeout, "--id-format", "uuids", "--json", "list-pane-surfaces", "--workspace", workspaceID, "--pane", pane.ID)
		if surfErr != nil {
			return "", surfErr
		}
		var surfaces struct {
			Surfaces []struct {
				ID   string `json:"id"`
				Type string `json:"type"`
			} `json:"surfaces"`
		}
		if err := json.Unmarshal([]byte(surfRaw), &surfaces); err != nil {
			return "", err
		}
		for _, surface := range surfaces.Surfaces {
			if surface.Type == "terminal" && strings.TrimSpace(surface.ID) != "" {
				return surface.ID, nil
			}
		}
	}
	return "", errors.New("throwaway cmux workspace has no terminal surface")
}

func cmuxLiveWaitForTerminalSurface(t *testing.T, path, surfaceID string) (json.RawMessage, string, error) {
	t.Helper()
	deadline := time.Now().Add(cmuxLiveInspectTimeout)
	var lastEntry string
	for {
		raw, err := cmuxLiveRun(path, cmuxLiveInspectTimeout, "rpc", "system.tree", cmuxSystemTreeParams)
		if err != nil {
			lastEntry = strings.TrimSpace(raw + ": " + err.Error())
			t.Logf("system.tree poll error=%s", lastEntry)
		} else {
			entry, tty, found := cmuxLiveFindSurfaceEntry([]byte(raw), surfaceID)
			if !found {
				lastEntry = "<absent from system.tree>"
				t.Logf("system.tree surface %s absent", surfaceID)
			} else {
				lastEntry = string(entry)
				t.Logf("system.tree surface entry=%s tty=%q", entry, tty)
				return entry, tty, nil
			}
		}
		if !time.Now().Before(deadline) {
			return nil, "", fmt.Errorf("throwaway surface %s never appeared in system.tree within %s: last entry=%s", surfaceID, cmuxLiveInspectTimeout, lastEntry)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func cmuxLiveFindSurfaceEntry(tree []byte, surfaceID string) (json.RawMessage, string, bool) {
	want, err := normalizeCmuxSurfaceID(surfaceID)
	if err != nil {
		return nil, "", false
	}
	want = strings.ToUpper(want)
	var parsed struct {
		Windows []struct {
			Workspaces []struct {
				Panes []struct {
					Surfaces []json.RawMessage `json:"surfaces"`
				} `json:"panes"`
			} `json:"workspaces"`
		} `json:"windows"`
	}
	if err := json.Unmarshal(tree, &parsed); err != nil {
		return nil, "", false
	}
	for _, window := range parsed.Windows {
		for _, workspace := range window.Workspaces {
			for _, pane := range workspace.Panes {
				for _, entry := range pane.Surfaces {
					var surface struct {
						ID  string `json:"id"`
						TTY string `json:"tty"`
					}
					if json.Unmarshal(entry, &surface) != nil {
						continue
					}
					id, idErr := normalizeCmuxSurfaceID(surface.ID)
					if idErr != nil || strings.ToUpper(id) != want {
						continue
					}
					return entry, surface.TTY, true
				}
			}
		}
	}
	return nil, "", false
}

func cmuxLiveTTYReady(tty string) bool {
	canon, err := canonicalCmuxTTY(tty)
	if err != nil {
		return false
	}
	return isCmuxPTY(canon)
}

func TestCmuxLiveFindSurfaceEntryAndTTYReady(t *testing.T) {
	blank := cmuxTreeWithSurfaceRecords(map[string]string{"id": testCmuxSurfaceID, "tty": ""})
	entry, tty, ok := cmuxLiveFindSurfaceEntry(blank, testCmuxSurfaceID)
	if !ok || len(entry) == 0 {
		t.Fatalf("blank tree missing surface: ok=%v entry=%s", ok, entry)
	}
	if cmuxLiveTTYReady(tty) {
		t.Fatal("blank tty must not be ready")
	}
	ready := cmuxTreeWithSurfaces(testCmuxSurfaceID)
	_, tty, ok = cmuxLiveFindSurfaceEntry(ready, testCmuxSurfaceID)
	if !ok || !cmuxLiveTTYReady(tty) {
		t.Fatalf("populated tty ready=%v tty=%q", ok, tty)
	}
}

func cmuxLiveRun(path string, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func cmuxLiveSocketDenied(output string, err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(strings.TrimSpace(output + "\n" + err.Error()))
	for _, needle := range []string{"operation not permitted", "permission denied", "access denied"} {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

func TestCmuxLiveSocketDeniedClassification(t *testing.T) {
	denied := errors.New("cmux: Operation not permitted")
	if !cmuxLiveSocketDenied("connect: operation not permitted", denied) {
		t.Fatal("access-denied text must skip")
	}
	if cmuxLiveSocketDenied("", errors.New("signal: killed")) {
		t.Fatal("precheck timeout/kill must not skip when LIVE=1")
	}
	if cmuxLiveSocketDenied("", context.DeadlineExceeded) {
		t.Fatal("deadline exceeded must not skip when LIVE=1")
	}
}

func containsString(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
