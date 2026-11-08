package route

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Peer walk bounds. A walk that would pass MaxPeerProjects stops and reports
// itself truncated.
const (
	DefaultPeerDepth = 1
	MaxPeerDepth     = 4
	MaxPeerProjects  = 64
)

// PeerStatus is what a peer walk could learn about one peer base root.
type PeerStatus string

const (
	// PeerOK means the root exists and an .amqrc owns it.
	PeerOK PeerStatus = "ok"
	// PeerUnreadable means the root or its .amqrc could not be read or was
	// refused.
	PeerUnreadable PeerStatus = "unreadable"
	// PeerMissingRoot means the peer base root does not exist.
	PeerMissingRoot PeerStatus = "missing_root"
	// PeerNoAmqrc means the root exists but no .amqrc with a project
	// identity owns it, so the peer cannot route a reply to anyone.
	PeerNoAmqrc PeerStatus = "no_amqrc"
)

// ReplyBack says whether a peer's .amqrc routes replies back to the project
// that lists it.
type ReplyBack string

const (
	ReplyBackYes     ReplyBack = "yes"
	ReplyBackNo      ReplyBack = "no"
	ReplyBackUnknown ReplyBack = "unknown"
)

// PeerEdge is one entry of a project's peers map, as seen from the project
// that lists it.
type PeerEdge struct {
	// Via is the project whose .amqrc lists this peer.
	Via string `json:"via"`
	// Name is the key in Via's peers map.
	Name     string     `json:"name"`
	BaseRoot string     `json:"base_root"`
	Depth    int        `json:"depth"`
	Status   PeerStatus `json:"status"`
	// Project is the peer's own project identity, when its .amqrc is read.
	Project string `json:"project,omitempty"`
	// Config is the peer's .amqrc, when one owns its root.
	Config string `json:"config,omitempty"`
	// ReplyBack is yes only when the peer's peers map has Via's exact
	// project name and that entry is Via's base root.
	ReplyBack ReplyBack `json:"reply_back"`
	Detail    string    `json:"detail,omitempty"`
}

// PeerGraph is every peer reachable from one project's .amqrc.
type PeerGraph struct {
	Project   string     `json:"project"`
	Config    string     `json:"config"`
	BaseRoot  string     `json:"base_root"`
	Depth     int        `json:"depth"`
	Peers     []PeerEdge `json:"peers"`
	Truncated bool       `json:"truncated,omitempty"`
}

// peerNode is a project whose .amqrc a walk has read.
type peerNode struct {
	project  string
	baseRoot string
	config   AmqrcFile
	depth    int
}

// Peers lists the peers reachable from sourceRoot's .amqrc, following each
// readable peer's own .amqrc up to depth hops. It reads only .amqrc files and
// stats base roots; it reads no message files. One unreadable peer never
// fails the walk; only a source without an owning .amqrc and project does.
func (w Walker) Peers(sourceRoot string, depth int) (PeerGraph, error) {
	if depth < 1 || depth > MaxPeerDepth {
		return PeerGraph{}, fmt.Errorf("depth must be between 1 and %d (got %d)", MaxPeerDepth, depth)
	}
	config, err := w.FindAmqrcForRoot(sourceRoot)
	if err != nil {
		return PeerGraph{}, federationSourceProjectError(sourceRoot)
	}
	project, err := w.requireFederationSourceProject(config, sourceRoot)
	if err != nil {
		return PeerGraph{}, err
	}
	baseRoot, err := config.PeerPath(strings.TrimSpace(config.Config.Root))
	if err != nil {
		return PeerGraph{}, fmt.Errorf("resolve configured source root: %w", err)
	}

	graph := PeerGraph{Project: project, Config: config.Path, BaseRoot: baseRoot, Depth: depth, Peers: []PeerEdge{}}
	visited := map[string]bool{canonicalPath(baseRoot): true}
	queue := []peerNode{{project: project, baseRoot: baseRoot, config: config}}
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		for _, name := range sortedPeerNames(node.config.Config.Peers) {
			peerRoot, err := node.config.Peer(name)
			if err != nil {
				graph.Peers = append(graph.Peers, PeerEdge{
					Via: node.project, Name: name, Depth: node.depth + 1,
					Status: PeerUnreadable, ReplyBack: ReplyBackUnknown, Detail: err.Error(),
				})
				continue
			}
			// Every edge is reported, an edge back to a visited project
			// included, since its reply_back is a fact of its own; visited
			// only stops a project from being expanded twice.
			key := canonicalPath(peerRoot)
			seen := visited[key]
			if !seen {
				if len(visited) > MaxPeerProjects {
					graph.Truncated = true
					return graph, nil
				}
				visited[key] = true
			}
			edge, peerConfig := w.inspectPeer(node, name, peerRoot)
			graph.Peers = append(graph.Peers, edge)
			if !seen && edge.Status == PeerOK && edge.Depth < depth {
				queue = append(queue, peerNode{project: edge.Project, baseRoot: peerRoot, config: peerConfig, depth: edge.Depth})
			}
		}
	}
	return graph, nil
}

// inspectPeer classifies one peer of node. The root is stat-ed before any
// .amqrc walk: a walk from a missing root would climb into an unrelated
// ancestor's .amqrc.
func (w Walker) inspectPeer(node peerNode, name, peerRoot string) (PeerEdge, AmqrcFile) {
	edge := PeerEdge{
		Via: node.project, Name: name, BaseRoot: peerRoot, Depth: node.depth + 1,
		ReplyBack: ReplyBackUnknown,
	}
	info, err := os.Stat(peerRoot)
	switch {
	case errors.Is(err, os.ErrNotExist):
		edge.Status, edge.Detail = PeerMissingRoot, err.Error()
		return edge, AmqrcFile{}
	case err != nil:
		edge.Status, edge.Detail = PeerUnreadable, err.Error()
		return edge, AmqrcFile{}
	case !info.IsDir():
		edge.Status, edge.Detail = PeerMissingRoot, fmt.Sprintf("%s is not a directory", peerRoot)
		return edge, AmqrcFile{}
	}
	config, err := w.FindAmqrcForRoot(peerRoot)
	switch {
	case errors.Is(err, ErrAmqrcNotFound):
		edge.Status, edge.Detail = PeerNoAmqrc, fmt.Sprintf("no .amqrc owns %s", peerRoot)
		return edge, AmqrcFile{}
	case err != nil:
		edge.Status, edge.Detail = PeerUnreadable, err.Error()
		return edge, AmqrcFile{}
	}
	project, err := w.requireFederationSourceProject(config, peerRoot)
	if err != nil {
		edge.Status, edge.Detail = PeerNoAmqrc, err.Error()
		return edge, AmqrcFile{}
	}
	edge.Status = PeerOK
	edge.Config = config.Path
	edge.Project = project
	edge.ReplyBack, edge.Detail = w.replyBack(config, node.project, node.baseRoot)
	return edge, config
}

// replyBack applies the reply planner's lookup: a reply from the peer to
// project resolves project in the peer's peers map, so that entry must exist
// under the exact name and name project's base root.
func (w Walker) replyBack(peer AmqrcFile, project, baseRoot string) (ReplyBack, string) {
	entry, ok := peer.Config.Peers[project]
	if !ok {
		return ReplyBackNo, fmt.Sprintf("%s has no peer named %q; replies to %s will fail", peer.Path, project, project)
	}
	resolved, err := peer.PeerPath(entry)
	if err != nil {
		return ReplyBackNo, fmt.Sprintf("%s: resolve peer %q: %v", peer.Path, project, err)
	}
	if w.RelateTrees(resolved, baseRoot) != TreeRelationSame {
		return ReplyBackNo, fmt.Sprintf("%s maps peer %q to %s, not %s; replies to %s will go there", peer.Path, project, resolved, baseRoot, project)
	}
	return ReplyBackYes, ""
}

func sortedPeerNames(peers map[string]string) []string {
	names := make([]string, 0, len(peers))
	for name := range peers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func canonicalPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return absPath(path)
}
