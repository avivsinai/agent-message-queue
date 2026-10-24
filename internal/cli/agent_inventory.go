package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/presence"
)

// inventoryScopeBase marks the base or sessionless queue's own agents/, which
// has no session name. Listings must not invent one for it.
const inventoryScopeBase = "base"

// agentInventoryScope is the read-only mailbox inventory of one delivery
// root. Every mailbox directory with an inspectable handle name yields a row;
// a missing presence file, an invalid identity record or an incomplete
// mailbox is reported on the row and never hides it.
type agentInventoryScope struct {
	Root         string
	Session      string // "" for the base or sessionless queue
	Kind         string // inventoryScopeBase, sessionKindCanonical or sessionKindLegacy
	Declarations agentDeclarations
	Agents       []inventoryAgent
	Skipped      []inventorySkip
	Err          error // agents/ exists but is not a listable local directory
}

type inventoryAgent struct {
	Handle       string
	Registration agentRegistration
	// MailboxIssues names each mailbox component, parent first, that is
	// missing, unreadable, a symlink or not a directory; components below one
	// are not inspected. Empty means the local layout is complete.
	MailboxIssues []string
	Presence      *presence.Presence
	PresenceErr   error // presence.json exists but cannot be read
}

type inventorySkip struct {
	Name   string
	Reason string
}

// inventoryDeliveryRoot inventories one delivery root. base is the resolved
// base root that supplies the base roster ("" when root is its own base).
func inventoryDeliveryRoot(root, base string) agentInventoryScope {
	scope := agentInventoryScope{
		Root:         root,
		Kind:         inventoryScopeBase,
		Declarations: declarationsFor(root, base),
	}
	if base != "" && filepath.Clean(base) != filepath.Clean(root) {
		scope.Session = filepath.Base(root)
		scope.Kind = sessionKindCanonical
		if !isCanonicalSessionName(scope.Session) {
			scope.Kind = sessionKindLegacy
		}
	}
	agentsDir := filepath.Join(root, "agents")
	info, err := os.Lstat(agentsDir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return scope
	case err != nil:
		scope.Err = err
		return scope
	case info.Mode()&os.ModeSymlink != 0:
		scope.Err = fmt.Errorf("%s is a symlink", agentsDir)
		return scope
	case !info.IsDir():
		scope.Err = fmt.Errorf("%s is not a directory", agentsDir)
		return scope
	}
	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		scope.Err = err
		return scope
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		handle := entry.Name()
		if err := fsq.ValidateLegacyHandleForInspection(handle); err != nil {
			scope.Skipped = append(scope.Skipped, inventorySkip{Name: handle, Reason: err.Error()})
			continue
		}
		agent := inventoryAgent{
			Handle:        handle,
			Registration:  classifyAgentAt(scope.Declarations, root, handle),
			MailboxIssues: inventoryMailboxIssues(root, handle),
		}
		if p, err := presence.Read(root, handle); err == nil {
			agent.Presence = &p
		} else if !errors.Is(err, os.ErrNotExist) {
			agent.PresenceErr = err
		}
		scope.Agents = append(scope.Agents, agent)
	}
	return scope
}

func inventoryMailboxIssues(root, handle string) []string {
	var issues []string
	broken := map[string]bool{}
	for _, leaf := range fsq.RequiredMailboxLeaves() {
		parts := strings.Split(string(leaf), "/")
		for i := range parts {
			component := strings.Join(parts[:i+1], "/")
			if broken[component] {
				break
			}
			info, err := os.Lstat(filepath.Join(fsq.AgentBase(root, handle), filepath.FromSlash(component)))
			issue := ""
			switch {
			case errors.Is(err, os.ErrNotExist):
				issue = fmt.Sprintf("%s missing", component)
			case err != nil:
				issue = fmt.Sprintf("%s unreadable: %v", component, err)
			case info.Mode()&os.ModeSymlink != 0:
				issue = fmt.Sprintf("%s is a symlink", component)
			case !info.IsDir():
				issue = fmt.Sprintf("%s is not a directory", component)
			}
			if issue != "" {
				broken[component] = true
				issues = append(issues, issue)
				break
			}
		}
	}
	return issues
}

// inventoryProject inventories a whole project: the base queue's own agents/
// first, then every child that the session validator accepts, each with the
// base roster in its declarations.
func inventoryProject(base string) ([]agentInventoryScope, error) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil, err
	}
	var scopes []agentInventoryScope
	if _, err := os.Lstat(filepath.Join(base, "agents")); err == nil {
		scopes = append(scopes, inventoryDeliveryRoot(base, ""))
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if _, _, skip := classifySessionChild(base, entry.Name()); !skip {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		scopes = append(scopes, inventoryDeliveryRoot(filepath.Join(base, name), base))
	}
	return scopes, nil
}
