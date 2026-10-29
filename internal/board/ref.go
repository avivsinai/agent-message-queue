package board

import (
	"regexp"
	"strconv"
	"strings"
)

// Ref kinds. Every ref in the board is built by one constructor below; no
// source formats a ref string.
const (
	RefBead       = "bead"
	RefForge      = "forge"
	RefObligation = "amq-obligation"
	RefAMQThread  = "amq-thread"
	RefAMQMessage = "amq-message"
	RefBranch     = "branch"
	RefExternal   = "ext"
)

// beadTokenRe finds candidate bead ids in free text (design 4.2). A token is
// a bead ref only when it is a verified member of the bead vocabulary.
var beadTokenRe = regexp.MustCompile(`[a-z][a-z0-9-]*-[0-9a-z]+(?:\.[0-9]+)*`)

var beadShapeRe = regexp.MustCompile(`^[a-z][a-z0-9-]*-[0-9a-z]+(?:\.[0-9]+)*$`)

var forgeNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*$`)

// forgeURLRe finds GitHub pull request and issue URLs in free text.
var forgeURLRe = regexp.MustCompile(`https://(github\.com)/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/(?:pull|issues)/([0-9]+)`)

// Forge item types, carried as evidence field "type": PRs and issues share
// one number space, so the type is display and action data, not identity.
const (
	ForgePullRequest = "pull_request"
	ForgeIssue       = "issue"
)

// ForgeType returns an item's forge type from its evidence.
func ForgeType(it Item) string {
	for _, e := range it.Evidence {
		if e.Field == "type" {
			return e.Value
		}
	}
	return ""
}

// Bead builds bead:<id>. The shape check is necessary, not sufficient: the
// link step also requires vocabulary membership.
func Bead(id string) (Ref, bool) {
	if !beadShapeRe.MatchString(id) {
		return Ref{}, false
	}
	return Ref{Kind: RefBead, ID: id}, true
}

// ForgeItem builds forge:<host>/<owner>/<repo>#<n>. Pull requests and issues
// share one number space per repository.
func ForgeItem(host, owner, repo string, n int) (Ref, bool) {
	host, owner, repo = strings.ToLower(host), strings.ToLower(owner), strings.ToLower(repo)
	if n <= 0 || !forgeNameRe.MatchString(host) || !forgeNameRe.MatchString(owner) || !forgeNameRe.MatchString(repo) {
		return Ref{}, false
	}
	return Ref{Kind: RefForge, ID: host + "/" + owner + "/" + repo + "#" + strconv.Itoa(n)}, true
}

// Obligation builds amq-obligation:<key> from a thread open key.
func Obligation(key string) (Ref, bool) {
	if key == "" || len(key) > 512 || hasControl(key) {
		return Ref{}, false
	}
	return Ref{Kind: RefObligation, ID: key}, true
}

// AMQMessage builds amq-message:<key> from a logical message key. It names
// one ask as related context; it is never a work identity.
func AMQMessage(key string) (Ref, bool) {
	if key == "" || len(key) > 512 || hasControl(key) {
		return Ref{}, false
	}
	return Ref{Kind: RefAMQMessage, ID: key}, true
}

// AMQThread builds amq-thread:<session>/<thread>, the key that groups
// obligations of one session and thread as related (never as the same work:
// a p2p thread holds unrelated requests). Session names hold no "/".
func AMQThread(session, thread string) (Ref, bool) {
	if session == "" || strings.Contains(session, "/") || thread == "" || len(thread) > 256 || hasControl(session+thread) {
		return Ref{}, false
	}
	return Ref{Kind: RefAMQThread, ID: session + "/" + thread}, true
}

// Branch builds branch:<host>/<owner>/<repo>:<name>. A branch is never a
// work identity; it is a related reference only.
func Branch(host, owner, repo, name string) (Ref, bool) {
	host, owner, repo = strings.ToLower(host), strings.ToLower(owner), strings.ToLower(repo)
	if !forgeNameRe.MatchString(host) || !forgeNameRe.MatchString(owner) || !forgeNameRe.MatchString(repo) ||
		name == "" || len(name) > 255 || hasControl(name) || strings.ContainsAny(name, " ~^:?*[\\") {
		return Ref{}, false
	}
	return Ref{Kind: RefBranch, ID: host + "/" + owner + "/" + repo + ":" + name}, true
}

// External builds ext:<provider>:<id> for an item of a configured external
// provider, so no provider can pose as another source's ref.
func External(provider, id string) (Ref, bool) {
	if !providerNameRe.MatchString(provider) || id == "" || len(id) > 256 || hasControl(id) {
		return Ref{}, false
	}
	return Ref{Kind: RefExternal, ID: provider + ":" + id}, true
}

// GitHubRepoRef builds the ForgeItem of number n in a configured
// "owner/repo" GitHub repository.
func GitHubRepoRef(ownerRepo string, n int) (Ref, bool) {
	owner, repo, ok := strings.Cut(ownerRepo, "/")
	if !ok {
		return Ref{}, false
	}
	return ForgeItem("github.com", owner, repo, n)
}

// BeadTokens returns the distinct candidate bead tokens in text.
func BeadTokens(text string) []string {
	return distinct(beadTokenRe.FindAllString(strings.ToLower(text), -1))
}

// BeadCandidates lists the ids a text token may name, longest first: the
// token and each prefix cut at a hyphen, so the branch feat/demo-a1b-parser
// can name bead demo-a1b. Only vocabulary members become refs.
func BeadCandidates(token string) []string {
	var out []string
	for t := token; beadShapeRe.MatchString(t); {
		out = append(out, t)
		i := strings.LastIndexByte(t, '-')
		if i <= 0 {
			break
		}
		t = t[:i]
	}
	return out
}

// ForgeURLRefs returns the forge refs of GitHub URLs in text.
func ForgeURLRefs(text string) []Ref {
	var refs []Ref
	seen := map[Ref]bool{}
	for _, m := range forgeURLRe.FindAllStringSubmatch(text, -1) {
		n, err := strconv.Atoi(m[4])
		if err != nil {
			continue
		}
		if ref, ok := ForgeItem(m[1], m[2], m[3], n); ok && !seen[ref] {
			seen[ref] = true
			refs = append(refs, ref)
		}
	}
	return refs
}

func distinct(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// HasControl reports whether s holds a control character (see isControl).
func HasControl(s string) bool { return hasControl(s) }

func hasControl(s string) bool {
	for _, r := range s {
		if isControl(r) {
			return true
		}
	}
	return false
}

// isControl reports C0, DEL, C1, and the Unicode line and paragraph
// separators.
func isControl(r rune) bool {
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == 0x2028 || r == 0x2029
}
