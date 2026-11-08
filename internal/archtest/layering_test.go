// Package archtest checks the module's package layering on the transitive
// import graph. It reads import clauses only, so it needs no build and no
// child process. Build constraints are ignored: every file counts, which is
// the safe superset for a rule that forbids an edge.
package archtest

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const module = "github.com/avivsinai/agent-message-queue"

const (
	cliPkg = module + "/internal/cli"
	acpPkg = module + "/internal/acp"
)

// importGraph holds the module-local imports of each package. Test-file
// imports are kept apart: they count only for the package under check, never
// for the packages it reaches.
type importGraph struct {
	deps  map[string][]string
	tests map[string][]string
}

// TestOnlyAMQCommandLinksCLI keeps internal/cli a leaf of the amq binary:
// no other package may reach it directly or transitively.
func TestOnlyAMQCommandLinksCLI(t *testing.T) {
	graph := loadImportGraph(t)
	allowed := map[string]bool{
		module + "/cmd/amq": true,
		// amq-remote still uses reply routing (ResolveReplyRoute) and
		// AgentRegistration from internal/cli. Remove this entry when those
		// move to their own packages.
		module + "/cmd/amq-remote": true,
	}
	for _, pkg := range graph.packages() {
		if pkg == cliPkg || allowed[pkg] {
			continue
		}
		if chain := graph.chain(pkg, cliPkg); chain != nil {
			t.Errorf("%s reaches internal/cli: %s", pkg, strings.Join(chain, " -> "))
		}
	}
}

// TestRemotePackagesDoNotLinkACP keeps the remote-control packages below
// the ACP companion: cmd/amq-remote is the wiring point that may join them.
func TestRemotePackagesDoNotLinkACP(t *testing.T) {
	graph := loadImportGraph(t)
	allowed := map[string]bool{
		// The mailbox prompt, reply scan, inbox probe, and session-pin check
		// still live in internal/acp. Remove when they move out.
		module + "/internal/remote/mailbox": true,
		// MarshalTextSessionUpdate still lives in internal/acp. Remove when
		// it moves to a leaf package.
		module + "/internal/remote/activity": true,
	}
	remote := module + "/internal/remote"
	for _, pkg := range graph.packages() {
		if pkg != remote && !strings.HasPrefix(pkg, remote+"/") {
			continue
		}
		if allowed[pkg] {
			continue
		}
		if chain := graph.chain(pkg, acpPkg); chain != nil {
			t.Errorf("%s reaches internal/acp: %s", pkg, strings.Join(chain, " -> "))
		}
	}
}

func loadImportGraph(t *testing.T) importGraph {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the archtest source file")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	graph := importGraph{deps: map[string][]string{}, tests: map[string][]string{}}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && (name == "testdata" || name == "vendor" ||
				strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") {
			return nil
		}
		parsed, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		pkg := module
		if rel != "." {
			pkg = module + "/" + filepath.ToSlash(rel)
		}
		target := graph.deps
		if strings.HasSuffix(name, "_test.go") {
			target = graph.tests
		}
		if _, seen := target[pkg]; !seen {
			target[pkg] = nil
		}
		for _, spec := range parsed.Imports {
			imp, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			if imp == module || strings.HasPrefix(imp, module+"/") {
				target[pkg] = append(target[pkg], imp)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read module imports: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root %s has no go.mod: %v", root, err)
	}
	return graph
}

func (g importGraph) packages() []string {
	set := map[string]bool{}
	for pkg := range g.deps {
		set[pkg] = true
	}
	for pkg := range g.tests {
		set[pkg] = true
	}
	out := make([]string, 0, len(set))
	for pkg := range set {
		out = append(out, pkg)
	}
	sort.Strings(out)
	return out
}

// chain returns one import path from pkg to target, or nil when target is
// unreachable. pkg's own test files count; the test files of the packages it
// reaches do not.
func (g importGraph) chain(pkg, target string) []string {
	parent := map[string]string{pkg: ""}
	queue := []string{pkg}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		next := g.deps[cur]
		if cur == pkg {
			next = append(append([]string(nil), next...), g.tests[pkg]...)
		}
		for _, dep := range next {
			if _, seen := parent[dep]; seen {
				continue
			}
			parent[dep] = cur
			if dep == target {
				var chain []string
				for at := dep; at != ""; at = parent[at] {
					chain = append([]string{at}, chain...)
				}
				return chain
			}
			queue = append(queue, dep)
		}
	}
	return nil
}
