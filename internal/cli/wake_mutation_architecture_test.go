package cli

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

var wakeMutationScopeFiles = map[string]struct{}{
	"wake_mutation_scope_unix.go":   {},
	"wake_mutation_scope_linux.go":  {},
	"wake_mutation_scope_darwin.go": {},
}

var wakeMutationEffectExemptions = map[string]map[int]string{
	// The parent owns this process handle directly; there is no published wake
	// claim indirection between the spawned child and this cleanup operation.
	"coop_exec_unix.go": {25: "parent-owned startup helper process handle"},
	// The repair parent likewise kills only the child process it spawned and
	// retains by handle, before any claim-based wake mutation is authorized.
	"wake_repair_handoff_darwin.go": {28: "parent-owned repair child process handle"},
}

// These calls remove a validated, non-lifecycle artifact whose basename is
// intentionally supplied at runtime. Keep each exception tied to one call so
// a new dynamic unlink remains a failing architecture change.
var wakeMutationUnprovableUnlinkExemptions = map[string]map[int]string{
	"wake_control_darwin.go": {
		220: "validated Darwin control-socket basename cleanup; lock name rejected locally",
	},
	"wake_lock_at_unix.go": {
		695: "snapshot-validated generation artifact cleanup; lock name rejected locally",
		735: "owned temporary generation file cleanup",
	},
	"wake_owner_storage_unix.go": {
		97:  "owned temporary target file cleanup",
		177: "owned temporary lock file cleanup",
		300: "owned temporary metadata file cleanup",
		789: "validated authoritative control-socket basename cleanup; lock name rejected locally",
	},
	"wake_quarantine_unix.go": {
		210: "snapshot-validated quarantine artifact cleanup; lock name rejected locally",
	},
	"wake_reload_transport_linux.go": {
		435: "identity-validated private reload endpoint cleanup",
	},
	"wake_repair_at_unix.go": {
		273: "identity-validated repair-floor quarantine cleanup",
		422: "owned temporary repair metadata cleanup",
	},
	"wake_repair_dirs_unix.go": {
		731: "validated retained baseline barrier cleanup; lock name rejected locally",
	},
	"wake_restart_stage_darwin.go": {
		301: "identity-validated empty restart-stage directory cleanup; lock name rejected locally",
	},
	"wake_restart_unix.go": {
		1340: "owned temporary resume-lock cleanup",
	},
	"wake_state_unix.go": {
		214: "owned temporary state file cleanup",
	},
}

type wakeMutationSourceFile struct {
	path string
	file *ast.File
}

type wakeMutationArchitectureOffender struct {
	path   string
	line   int
	reason string
}

func TestWakeMutationEffectsStayBehindScope(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	cliDir := filepath.Dir(testFile)
	fset := token.NewFileSet()
	var offenders []wakeMutationArchitectureOffender
	add := func(path string, node ast.Node, reason string) {
		offenders = append(offenders, wakeMutationArchitectureOffender{
			path:   filepath.ToSlash(path),
			line:   fset.Position(node.Pos()).Line,
			reason: reason,
		})
	}
	var sourceFiles []wakeMutationSourceFile
	err := filepath.WalkDir(cliDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if _, ok := wakeMutationScopeFiles[filepath.Base(path)]; ok {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		sourceFiles = append(sourceFiles, wakeMutationSourceFile{path: path, file: file})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	stringConstants := resolveWakeStringConstants(sourceFiles)
	for _, source := range sourceFiles {
		path := source.path
		file := source.file
		ast.Inspect(file, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.CallExpr:
				if isWakeUnlinkCall(node) {
					line := fset.Position(node.Pos()).Line
					target, resolvable := resolveWakeUnlinkTarget(node, stringConstants)
					if !resolvable {
						if !isWakeMutationUnprovableUnlinkExempt(path, line) {
							add(path, node, "unprovable unlink target")
						}
					} else if target == wakeLockFileName {
						add(path, node, "direct .wake.lock unlink")
					}
				}
				if callName(node.Fun) == "linuxPidfdSendSignal" {
					add(path, node, "direct pidfd wake signal")
				}
				if isProcessSignalCall(node) && !isZeroSignalProbe(node) {
					add(path, node, "direct wake process signal")
				}
				if callName(node.Fun) == "Kill" && !isWakeMutationEffectExempt(path, fset.Position(node.Pos()).Line) {
					add(path, node, "direct wake child kill")
				}
				if isWakeLifecycleGuardCall(node) {
					findGuardWaits(node, func(waitNode ast.Node, reason string) {
						add(path, waitNode, reason)
					})
				}
			case *ast.SendStmt:
				if name := channelName(node.Chan); name == "stopRequest" || name == "restartSignals" {
					add(path, node, "direct lifecycle control send")
				}
			}
			return true
		})
	}
	sort.Slice(offenders, func(i, j int) bool {
		if offenders[i].path != offenders[j].path {
			return offenders[i].path < offenders[j].path
		}
		if offenders[i].line != offenders[j].line {
			return offenders[i].line < offenders[j].line
		}
		return offenders[i].reason < offenders[j].reason
	})
	if len(offenders) == 0 {
		return
	}
	var lines []string
	for _, offender := range offenders {
		lines = append(lines, fmt.Sprintf("%s:%d: %s", offender.path, offender.line, offender.reason))
	}
	t.Fatalf("wake lifecycle effects escaped the mutation scope:\n%s", strings.Join(lines, "\n"))
}

func isWakeUnlinkCall(call *ast.CallExpr) bool {
	name := callName(call.Fun)
	return name == "Unlinkat" || name == "wakeRetireUnlinkAt"
}

func resolveWakeUnlinkTarget(call *ast.CallExpr, constants map[string]string) (string, bool) {
	if len(call.Args) < 2 {
		return "", false
	}
	return resolveWakeStringExpr(call.Args[1], constants)
}

func resolveWakeStringExpr(expr ast.Expr, constants map[string]string) (string, bool) {
	switch expr := expr.(type) {
	case *ast.BasicLit:
		if expr.Kind != token.STRING {
			return "", false
		}
		value, err := strconv.Unquote(expr.Value)
		return value, err == nil
	case *ast.Ident:
		value, ok := constants[expr.Name]
		return value, ok
	case *ast.ParenExpr:
		return resolveWakeStringExpr(expr.X, constants)
	case *ast.BinaryExpr:
		if expr.Op != token.ADD {
			return "", false
		}
		left, leftOK := resolveWakeStringExpr(expr.X, constants)
		right, rightOK := resolveWakeStringExpr(expr.Y, constants)
		return left + right, leftOK && rightOK
	default:
		return "", false
	}
}

func resolveWakeStringConstants(files []wakeMutationSourceFile) map[string]string {
	expressions := make(map[string]ast.Expr)
	for _, source := range files {
		for _, decl := range source.file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			var inherited []ast.Expr
			for _, spec := range gen.Specs {
				values, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				current := values.Values
				if len(current) == 0 {
					current = inherited
				}
				if len(current) == len(values.Names) {
					for i, name := range values.Names {
						expressions[name.Name] = current[i]
					}
				}
				inherited = current
			}
		}
	}

	resolved := make(map[string]string)
	resolving := make(map[string]bool)
	var resolveName func(string) (string, bool)
	var resolveExpr func(ast.Expr) (string, bool)
	resolveName = func(name string) (string, bool) {
		if value, ok := resolved[name]; ok {
			return value, true
		}
		if resolving[name] {
			return "", false
		}
		expr, ok := expressions[name]
		if !ok {
			return "", false
		}
		resolving[name] = true
		defer delete(resolving, name)
		value, ok := resolveExpr(expr)
		if ok {
			resolved[name] = value
		}
		return value, ok
	}
	resolveExpr = func(expr ast.Expr) (string, bool) {
		switch expr := expr.(type) {
		case *ast.BasicLit:
			if expr.Kind != token.STRING {
				return "", false
			}
			value, err := strconv.Unquote(expr.Value)
			return value, err == nil
		case *ast.Ident:
			return resolveName(expr.Name)
		case *ast.ParenExpr:
			return resolveExpr(expr.X)
		case *ast.BinaryExpr:
			if expr.Op != token.ADD {
				return "", false
			}
			left, leftOK := resolveExpr(expr.X)
			right, rightOK := resolveExpr(expr.Y)
			return left + right, leftOK && rightOK
		default:
			return "", false
		}
	}
	for name := range expressions {
		_, _ = resolveName(name)
	}
	return resolved
}

func TestResolveWakeUnlinkTargetUsesPackageConstantsAndFailsClosed(t *testing.T) {
	const source = `package cli

const (
	wakeLockFileName = ".wake.lock"
	wakeLockAlias = wakeLockFileName
)

func removeLock(fd int) {
	unix.Unlinkat(fd, wakeLockAlias, 0)
}

func removeDynamic(fd int, name string) {
	unix.Unlinkat(fd, name, 0)
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "architecture_test.go", source, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	constants := resolveWakeStringConstants([]wakeMutationSourceFile{{file: file}})
	var targets []ast.Expr
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if ok && isWakeUnlinkCall(call) {
			targets = append(targets, call.Args[1])
		}
		return true
	})
	if len(targets) != 2 {
		t.Fatalf("found %d unlink targets, want 2", len(targets))
	}
	if got, ok := resolveWakeStringExpr(targets[0], constants); !ok || got != wakeLockFileName {
		t.Fatalf("resolved lock target = %q, %v; want %q, true", got, ok, wakeLockFileName)
	}
	if got, ok := resolveWakeStringExpr(targets[1], constants); ok || got != "" {
		t.Fatalf("resolved dynamic target = %q, %v; want empty, false", got, ok)
	}
}

func isZeroSignalProbe(call *ast.CallExpr) bool {
	if len(call.Args) != 1 {
		return false
	}
	inner, ok := call.Args[0].(*ast.CallExpr)
	if !ok || callName(inner.Fun) != "Signal" || len(inner.Args) != 1 {
		return false
	}
	lit, ok := inner.Args[0].(*ast.BasicLit)
	return ok && lit.Kind == token.INT && lit.Value == "0"
}

func isProcessSignalCall(call *ast.CallExpr) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Signal" {
		return false
	}
	identifier, ok := selector.X.(*ast.Ident)
	return !ok || identifier.Name != "syscall"
}

func isWakeMutationEffectExempt(path string, line int) bool {
	_, ok := wakeMutationEffectExemptions[filepath.Base(path)][line]
	return ok
}

func isWakeMutationUnprovableUnlinkExempt(path string, line int) bool {
	_, ok := wakeMutationUnprovableUnlinkExemptions[filepath.Base(path)][line]
	return ok
}

func isWakeLifecycleGuardCall(call *ast.CallExpr) bool {
	switch callName(call.Fun) {
	case "withWakeLifecycleGuardInDir",
		"withWakeLifecycleGuardModeInDir",
		"withWakeLifecycleGuardNoWaitInDir",
		"withExistingWakeLifecycleGuardInDir",
		"withExistingWakeLifecycleGuardModeInDir",
		"withWakeMutationScopeModeInDir",
		"withExistingWakeMutationScopeModeInDir",
		"withWakeMutationScopeOrRetainedDir":
		return true
	default:
		return false
	}
}

func findGuardWaits(call *ast.CallExpr, add func(ast.Node, string)) {
	for _, arg := range call.Args {
		callback, ok := arg.(*ast.FuncLit)
		if !ok {
			continue
		}
		ast.Inspect(callback.Body, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.CallExpr:
				if isGuardWaitCall(callName(node.Fun)) {
					add(node, "wait inside lifecycle guard scope")
				}
			case *ast.UnaryExpr:
				if node.Op == token.ARROW {
					if name := channelName(node.X); name == "loopStopped" || name == "stopRequest" {
						add(node, "control wait inside lifecycle guard scope")
					}
				}
			}
			return true
		})
	}
}

func isGuardWaitCall(name string) bool {
	if name == "" {
		return false
	}
	lower := strings.ToLower(name)
	return strings.Contains(lower, "wait") || strings.Contains(lower, "poll")
}

func callName(expr ast.Expr) string {
	switch expr := expr.(type) {
	case *ast.Ident:
		return expr.Name
	case *ast.SelectorExpr:
		return expr.Sel.Name
	default:
		return ""
	}
}

func channelName(expr ast.Expr) string {
	ident, ok := expr.(*ast.Ident)
	if !ok {
		return ""
	}
	return ident.Name
}
