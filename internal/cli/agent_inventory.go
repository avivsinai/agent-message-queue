package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

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
	Err          error // agents/ exists but cannot be listed
}

type inventoryAgent struct {
	Handle       string
	Registration agentRegistration
	// MailboxIssues names each required mailbox leaf that is missing or not
	// a directory; empty means the layout is complete.
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
	entries, err := os.ReadDir(filepath.Join(root, "agents"))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			scope.Err = err
		}
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
	for _, leaf := range fsq.RequiredMailboxLeaves() {
		info, err := os.Lstat(fsq.AgentMailboxPath(root, handle, leaf))
		switch {
		case errors.Is(err, os.ErrNotExist):
			issues = append(issues, fmt.Sprintf("%s missing", leaf))
		case err != nil:
			issues = append(issues, fmt.Sprintf("%s unreadable: %v", leaf, err))
		case !info.IsDir():
			issues = append(issues, fmt.Sprintf("%s is not a directory", leaf))
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
	if dirExists(filepath.Join(base, "agents")) {
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
