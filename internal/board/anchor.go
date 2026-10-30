package board

import (
	"regexp"
	"sort"
	"strings"
)

// AnchorLink is one automatic ask-to-task link. It exists only because the
// ask names exactly one catalogue task by an exact id; model output and
// candidate rank never produce one. Links are derived at projection time
// and never persisted, so an owner binding or dismissal always wins.
type AnchorLink struct {
	TaskID string `json:"task_id"`
	Name   string `json:"name"`
	Basis  string `json:"basis"`
}

// Anchor kinds, the first words of AnchorLink.Basis. A thread name basis
// adds a "thread name: " prefix.
const (
	anchorFullID      = "full id"
	anchorShortID     = "short id"
	anchorCuedID      = "bead cue"
	anchorPullRequest = "pull request"
)

// shortTokenRe finds the maximal runs that can be a bead id suffix. A hyphen
// separates runs, so a full id also yields its own suffix.
var shortTokenRe = regexp.MustCompile(`[0-9a-z.]+`)

// prNumberRe finds #N, PR N, PR #N and pull/N outside URLs and identifiers.
var prNumberRe = regexp.MustCompile(`(?:#|pr ?#?|pull/)([1-9][0-9]*)`)

type anchorName struct {
	kind, name string
	beads      []string
}

// anchorIndex resolves exact names against the whole catalogue, done and
// historical tasks included.
type anchorIndex struct {
	byBead    map[string][]string // bead id -> task ids
	byShort   map[string][]string // bead id suffix -> bead ids
	byForge   map[string][]string // forge ref id -> bead ids, external_ref only
	cancelled map[string]bool
}

func newAnchorIndex(s Snapshot) anchorIndex {
	idx := anchorIndex{byBead: map[string][]string{}, byShort: map[string][]string{}, byForge: map[string][]string{}, cancelled: map[string]bool{}}
	for _, task := range s.Catalogue() {
		if task.Cancelled {
			idx.cancelled[task.ID] = true
		}
		for _, fact := range task.Facts {
			ref := fact.Fact.Ref
			if ref.Kind != RefBead {
				continue
			}
			idx.byBead[ref.ID] = appendUnique(idx.byBead[ref.ID], task.ID)
			for _, rel := range fact.Fact.Relations {
				if rel.Kind == RelationSameWork && rel.Basis == "external_ref" && rel.To.Kind == RefForge {
					idx.byForge[rel.To.ID] = appendUnique(idx.byForge[rel.To.ID], ref.ID)
				}
			}
		}
	}
	for bead := range idx.byBead {
		if i := strings.LastIndexByte(bead, '-'); i > 0 {
			idx.byShort[bead[i+1:]] = appendUnique(idx.byShort[bead[i+1:]], bead)
		}
	}
	for _, index := range []map[string][]string{idx.byBead, idx.byShort, idx.byForge} {
		for _, values := range index {
			sort.Strings(values)
		}
	}
	return idx
}

// names returns the exact names in text: full catalogue bead ids, bead id
// suffixes, and pull request numbers that a bead's external_ref maps. A name
// may resolve to several beads; the caller treats that as ambiguity.
func (idx anchorIndex) names(text string) []anchorName {
	text = strings.ToLower(text)
	var out []anchorName
	full := []string{}
	for token := range exactBeadTokens(text) {
		if _, ok := idx.byBead[token]; ok {
			full = append(full, token)
		}
	}
	sort.Strings(full)
	for _, id := range full {
		out = append(out, anchorName{anchorFullID, id, []string{id}})
	}
	prev, prevEnd := "", 0
	for _, span := range shortTokenRe.FindAllStringIndex(text, -1) {
		start, end := span[0], span[1]
		raw := text[start:end]
		token := strings.Trim(raw, ".")
		start += strings.Index(raw, token)
		end = start + len(token)
		cued := prev == "bead" && strings.Trim(text[prevEnd:start], " :") == ""
		prev, prevEnd = token, end
		if suffix, ok := strings.CutPrefix(token, "bead"); ok && suffix != "" && len(idx.byShort[suffix]) > 0 {
			token, start, cued = suffix, start+len("bead"), true
		}
		if token == "" || len(idx.byShort[token]) == 0 || (start > 0 && text[start-1] == '_') || (end < len(text) && text[end] == '_') {
			continue
		}
		digit, other := strings.ContainsAny(token, "0123456789"), strings.Trim(token, "0123456789") != ""
		if !cued && (!digit || !other) {
			continue
		}
		beads := idx.byShort[token]
		if start > 0 && text[start-1] == '-' {
			// A hyphenated suffix names a bead only under that bead's own
			// prefix: other-611.15 is another project's id.
			var own []string
			for _, bead := range beads {
				if strings.HasSuffix(text[:start], bead[:len(bead)-len(token)]) {
					own = append(own, bead)
				}
			}
			beads = own
		}
		if len(beads) > 0 {
			kind := anchorShortID
			if cued {
				kind = anchorCuedID
			}
			out = append(out, anchorName{kind, token, beads})
		}
	}
	for _, ref := range ForgeURLRefs(text) {
		if beads := idx.byForge[ref.ID]; len(beads) > 0 {
			out = append(out, anchorName{anchorPullRequest, ref.ID, beads})
		}
	}
	for _, match := range prNumberRe.FindAllStringSubmatchIndex(text, -1) {
		if start, end := match[0], match[1]; (start > 0 && strings.ContainsAny(text[start-1:start], "0123456789abcdefghijklmnopqrstuvwxyz_/#")) || (end < len(text) && strings.ContainsAny(text[end:end+1], "0123456789abcdefghijklmnopqrstuvwxyz_")) {
			continue
		}
		number := text[match[2]:match[3]]
		// PRs and issues share one number space per repository; a bare
		// number names a bead only through exactly one forge ref across all
		// repositories. Several refs make the name ambiguous.
		var refs []string
		for ref := range idx.byForge {
			if strings.HasSuffix(ref, "#"+number) {
				refs = append(refs, ref)
			}
		}
		sort.Strings(refs)
		beads := refs
		if len(refs) == 1 {
			beads = idx.byForge[refs[0]]
		}
		if len(beads) > 0 {
			out = append(out, anchorName{anchorPullRequest, "#" + number, beads})
		}
	}
	return out
}

// resolve maps names to tasks. Any name with two or more beads or tasks, or
// one naming an owner-cancelled task, makes the whole ask abstain.
func (idx anchorIndex) resolve(names []anchorName, prefix string) ([]AnchorLink, string) {
	var links []AnchorLink
	seen := map[string]bool{}
	for _, n := range names {
		if len(n.beads) != 1 {
			return nil, prefix + n.kind + " " + n.name + " is ambiguous"
		}
		tasks := idx.byBead[n.beads[0]]
		if len(tasks) != 1 {
			return nil, prefix + n.kind + " " + n.name + " is ambiguous"
		}
		if idx.cancelled[tasks[0]] {
			return nil, prefix + n.kind + " " + n.name + " names an owner-cancelled task"
		}
		if !seen[tasks[0]] {
			seen[tasks[0]] = true
			links = append(links, AnchorLink{TaskID: tasks[0], Name: n.name, Basis: prefix + n.kind + " " + n.name})
		}
	}
	if len(links) == 0 {
		return nil, "no exact name"
	}
	return links, ""
}

// anchors resolves one message. Direct names come from its subject and
// body. Only without direct names does its thread name count, and never a
// p2p thread name, which holds handles. Other messages, including those it
// refs, are never read: a reply chain names neighbouring work too often.
func (idx anchorIndex) anchors(m Message) ([]AnchorLink, string) {
	if names := idx.names(m.Subject + "\n" + m.Body); len(names) > 0 {
		return idx.resolve(names, "")
	}
	if m.Thread == "" || strings.HasPrefix(m.Thread, "p2p/") {
		return nil, "no exact name"
	}
	links, abstain := idx.resolve(idx.names(m.Thread), "thread name: ")
	if len(links) > 1 {
		return nil, "thread name names more than one task"
	}
	return links, abstain
}

// exactAnchors links each open ask that no owner or adopted model binding
// holds to the tasks it names exactly, keyed by message key.
func exactAnchors(s Snapshot) map[string][]AnchorLink {
	states := messageActionStates(s)
	bound := map[string]bool{}
	for _, task := range s.Tasks {
		if task.RedirectTo != "" {
			continue
		}
		for _, key := range task.Attachments {
			bound[key] = true
		}
		for _, b := range task.ModelAttachments {
			bound[b.Unit.MessageKey] = true
		}
	}
	out := map[string][]AnchorLink{}
	var idx *anchorIndex
	for key, m := range s.Messages {
		if bound[key] || !openAsk(s, states, key) {
			continue
		}
		if idx == nil {
			built := newAnchorIndex(s)
			idx = &built
		}
		if links, _ := idx.anchors(m); len(links) > 0 {
			out[key] = links
		}
	}
	return out
}

// messageActionStates merges the AMQ action state of each message.
func messageActionStates(s Snapshot) map[string]string {
	states := map[string]string{}
	for _, nr := range s.Input.Results {
		for key, status := range nr.Result.ActionStates {
			states[key] = status
		}
	}
	return states
}

// openAsk is the unresolved-ask domain: an open question, review request or
// todo that the owner has not dismissed or cancelled.
func openAsk(s Snapshot, states map[string]string, key string) bool {
	m := s.Messages[key]
	return states[key] != "resolved" && !s.Dismissed[key] && !s.ActionCancelled[key] && (m.Kind == "question" || m.Kind == "review_request" || m.Kind == "todo")
}
