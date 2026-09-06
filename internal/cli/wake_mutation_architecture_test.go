package cli

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/packages"
)

const (
	wakeMutationModulePath            = "github.com/avivsinai/agent-message-queue"
	wakeMutationCLIPackagePath        = wakeMutationModulePath + "/internal/cli"
	wakeMutationCapabilityPackagePath = wakeMutationCLIPackagePath + "/wakemutation"
)

type wakeMutationSourceFile struct {
	pkg  *packages.Package
	path string
	file *ast.File
}

type wakeMutationPackage struct {
	pkg           *packages.Package
	files         []wakeMutationSourceFile
	aliases       map[*types.Var]*types.Func
	ambiguous     map[*types.Var]bool
	typeAssertVar map[*types.Var]bool
	channelAlias  map[*types.Var]string
	channelAmbig  map[*types.Var]bool
	stringAliases map[*types.Var]string
	stringAmbig   map[*types.Var]bool
}

func isWakeMutationCLIPackage(pkg *packages.Package) bool {
	return pkg != nil && pkg.PkgPath == wakeMutationCLIPackagePath
}

type wakeMutationVisitor struct {
	context *wakeMutationPackage
	file    wakeMutationSourceFile
	add     func(ast.Node, string, *types.Func)
	owner   *types.Func
	frames  []wakeMutationVisitorFrame
}

type wakeMutationVisitorFrame struct {
	owner             *types.Func
	functionLiteral   *ast.FuncLit
	functionScopeVars map[*types.Var]struct{}
}

func (visitor *wakeMutationVisitor) Visit(node ast.Node) ast.Visitor {
	if node == nil {
		last := len(visitor.frames) - 1
		if last >= 0 {
			frame := visitor.frames[last]
			visitor.frames = visitor.frames[:last]
			visitor.owner = frame.owner
		}
		return nil
	}
	frame := wakeMutationVisitorFrame{owner: visitor.owner}
	visitor.frames = append(visitor.frames, frame)
	if declaration, ok := node.(*ast.FuncDecl); ok {
		if fn, ok := visitor.context.pkg.TypesInfo.Defs[declaration.Name].(*types.Func); ok {
			visitor.owner = fn
		}
	}
	if literal, ok := node.(*ast.FuncLit); ok {
		frame.functionLiteral = literal
		frame.functionScopeVars = wakeMutationScopeVars(visitor.context.pkg.TypesInfo, literal)
		visitor.owner = wakeMutationLiteralOwner(visitor.context, visitor.file, literal)
		visitor.frames[len(visitor.frames)-1] = frame
	}
	visitor.inspectNode(node)
	return visitor
}

func wakeMutationLiteralOwner(
	context *wakeMutationPackage,
	file wakeMutationSourceFile,
	literal *ast.FuncLit,
) *types.Func {
	signature, _ := context.pkg.TypesInfo.TypeOf(literal).(*types.Signature)
	if signature == nil {
		return nil
	}
	position := context.pkg.Fset.Position(literal.Pos())
	return types.NewFunc(
		literal.Pos(),
		context.pkg.Types,
		fmt.Sprintf("<func-literal %s:%d>", filepath.Base(file.path), position.Line),
		signature,
	)
}

func (visitor *wakeMutationVisitor) inspectNode(node ast.Node) {
	info := visitor.context.pkg.TypesInfo
	switch node := node.(type) {
	case *ast.CallExpr:
		visitor.inspectCall(node)
	case *ast.SendStmt:
		visitor.inspectLifecycleSend(node)
	case *ast.SelectorExpr:
		visitor.inspectScopeField(node)
	case *ast.CompositeLit:
		if wakeMutationIsScopeType(info.TypeOf(node.Type)) && !isWakeMutationScopeOwner(visitor.owner) {
			visitor.add(node, "mutation scope forged outside its owner", visitor.owner)
		}
	case *ast.ReturnStmt:
		for _, result := range node.Results {
			if wakeMutationIsScopePointer(info.TypeOf(result)) && !isWakeMutationScopeOwner(visitor.owner) {
				visitor.add(result, "mutation scope escaped through return", visitor.owner)
			}
		}
	case *ast.AssignStmt:
		for index, result := range node.Rhs {
			if wakeMutationIsScopePointer(info.TypeOf(result)) && !isWakeMutationScopeOwner(visitor.owner) {
				if isWakeMutationScopeLocalAssignment(visitor, node, index) {
					continue
				}
				visitor.add(result, "mutation scope escaped through assignment", visitor.owner)
			}
		}
	case *ast.FuncDecl:
		if node.Name != nil && (node.Name.Name == "withWakeMutationScopeModeInDir" ||
			node.Name.Name == "withExistingWakeMutationScopeModeInDir") {
			visitor.add(node, "mutation scope exposes arbitrary lock mode", visitor.owner)
		}
	case *ast.Ident:
		visitor.inspectScopeCapture(node)
	}
}

func (visitor *wakeMutationVisitor) inspectCall(call *ast.CallExpr) {
	fn := resolveWakeFunctionExpr(visitor.context, call.Fun)
	if fn == nil {
		if isWakeMutationUnresolvedEffectCall(visitor.context, call) {
			visitor.add(call, "unresolved wake effect call in cli", visitor.owner)
		}
		return
	}
	if wakeFunctionIsReflective(fn) {
		visitor.add(call, "reflective call can bypass mutation authorization", visitor.owner)
	}
	if wakeFunctionIsMutationScopeConstructor(fn) && !visitor.scopeConstructorIsOwned() {
		visitor.add(call, "mutation scope constructed outside its owner", visitor.owner)
	}
	if wakeFunctionIsLeaseConstructor(fn) && !visitor.capabilityOwnerIsOwned() {
		visitor.add(call, "wakemutation lease constructed outside its owner", visitor.owner)
	}
	if wakeFunctionIsLeaseEffect(fn) && !visitor.capabilityOwnerIsOwned() {
		visitor.add(call, "wakemutation lease effect called outside its owner", visitor.owner)
	}
	if wakeFunctionIsGuard(fn) {
		visitor.inspectGuardCallback(call)
	}
	if visitor.context.pkg.PkgPath != wakeMutationCapabilityPackagePath &&
		visitor.isWakeFilesystemEffect(call, fn) {
		visitor.add(call, "raw wake filesystem effect outside capability package", visitor.owner)
	}
	if isWakeMutationCLIPackage(visitor.context.pkg) &&
		wakeFunctionIsRawPidfdEffect(fn) {
		reason := "direct pidfd wake signal outside capability package"
		if wakeFunctionIsUnleasedPidfdEffect(fn) {
			reason = "direct unleased pidfd wake signal outside capability owner"
		}
		visitor.add(call, reason, visitor.owner)
	}
	if isWakeMutationCLIPackage(visitor.context.pkg) && wakeFunctionIsProcessSignal(fn) &&
		!isWakeZeroSignalCall(visitor.context, call) {
		visitor.add(call, "direct wake process signal outside capability package", visitor.owner)
	}
	if isWakeMutationCLIPackage(visitor.context.pkg) && wakeFunctionIsProcessKill(fn) {
		visitor.add(call, "direct wake process kill outside capability package", visitor.owner)
	}
}

func (visitor *wakeMutationVisitor) isWakeFilesystemEffect(call *ast.CallExpr, fn *types.Func) bool {
	if !wakeFunctionIsRawFilesystemEffect(fn) {
		return false
	}
	if wakeFunctionIsRawUnixFilesystemEffect(fn) {
		return isWakeMutationCLIPackage(visitor.context.pkg)
	}
	pathExpression := wakeRemovalPathExpression(call, fn)
	return pathExpression != nil && isWakeArtifactExpr(visitor.context, pathExpression)
}

func (visitor *wakeMutationVisitor) inspectLifecycleSend(send *ast.SendStmt) {
	origin, ok := resolveWakeChannelExpr(visitor.context, send.Chan)
	if !ok {
		return
	}
	if origin != "stopRequest" && origin != "restartSignals" {
		return
	}
	if isWakeMutationCLIPackage(visitor.context.pkg) {
		visitor.add(send, "direct lifecycle control send outside capability package", visitor.owner)
	}
}

func (visitor *wakeMutationVisitor) inspectGuardCallback(call *ast.CallExpr) {
	for _, argument := range call.Args {
		literal, ok := argument.(*ast.FuncLit)
		if !ok {
			continue
		}
		ast.Inspect(literal.Body, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.CallExpr:
				fn := resolveWakeFunctionExpr(visitor.context, node.Fun)
				if fn != nil && isWakeWaitFunction(fn) {
					visitor.add(node, "wait inside lifecycle guard scope", visitor.owner)
				}
			case *ast.UnaryExpr:
				if node.Op != token.ARROW {
					return true
				}
				origin, ok := resolveWakeChannelExpr(visitor.context, node.X)
				if ok && (origin == "loopStopped" || origin == "stopRequest" || origin == "restartSignals") {
					visitor.add(node, "control wait inside lifecycle guard scope", visitor.owner)
				}
			}
			return true
		})
	}
}

func (visitor *wakeMutationVisitor) inspectScopeField(selector *ast.SelectorExpr) {
	field, ok := visitor.context.pkg.TypesInfo.ObjectOf(selector.Sel).(*types.Var)
	if !ok || !field.IsField() || !wakeMutationIsScopeType(visitor.context.pkg.TypesInfo.TypeOf(selector.X)) {
		return
	}
	if !isWakeMutationScopeOwner(visitor.owner) {
		visitor.add(selector, "mutation scope field accessed outside its owner", visitor.owner)
	}
}

func (visitor *wakeMutationVisitor) inspectScopeCapture(identifier *ast.Ident) {
	object := visitor.context.pkg.TypesInfo.ObjectOf(identifier)
	variable, ok := object.(*types.Var)
	if !ok || variable.IsField() || !wakeMutationIsScopePointer(variable.Type()) {
		return
	}
	frame := visitor.currentFunctionFrame()
	if frame == nil {
		return
	}
	if _, local := frame.functionScopeVars[variable]; local || isWakeMutationScopeOwner(visitor.owner) {
		return
	}
	visitor.add(identifier, "mutation scope captured by nested function", visitor.owner)
}

func (visitor *wakeMutationVisitor) scopeConstructorIsOwned() bool {
	if isWakeMutationScopeOwner(visitor.owner) {
		return true
	}
	frame := visitor.currentFunctionFrame()
	if frame == nil {
		return false
	}
	return isWakeMutationScopeOwner(frame.owner)
}

func (visitor *wakeMutationVisitor) capabilityOwnerIsOwned() bool {
	if isWakeMutationCapabilityOwner(visitor.owner) {
		return true
	}
	frame := visitor.currentFunctionFrame()
	if frame == nil {
		return false
	}
	return isWakeMutationCapabilityOwner(frame.owner)
}

func (visitor *wakeMutationVisitor) currentFunctionFrame() *wakeMutationVisitorFrame {
	for index := len(visitor.frames) - 1; index >= 0; index-- {
		if visitor.frames[index].functionLiteral != nil {
			return &visitor.frames[index]
		}
	}
	return nil
}

func isWakeMutationScopeLocalAssignment(
	visitor *wakeMutationVisitor,
	assignment *ast.AssignStmt,
	index int,
) bool {
	if visitor == nil || assignment == nil || index >= len(assignment.Lhs) {
		return false
	}
	ident, ok := assignment.Lhs[index].(*ast.Ident)
	if !ok {
		return false
	}
	variable, ok := visitor.context.pkg.TypesInfo.ObjectOf(ident).(*types.Var)
	if !ok {
		return false
	}
	frame := visitor.currentFunctionFrame()
	if frame == nil {
		return false
	}
	_, local := frame.functionScopeVars[variable]
	return local
}

func wakeMutationScopeVars(info *types.Info, literal *ast.FuncLit) map[*types.Var]struct{} {
	vars := make(map[*types.Var]struct{})
	if literal.Type != nil && literal.Type.Params != nil {
		for _, field := range literal.Type.Params.List {
			for _, name := range field.Names {
				if variable, ok := info.Defs[name].(*types.Var); ok && wakeMutationIsScopePointer(variable.Type()) {
					vars[variable] = struct{}{}
				}
			}
		}
	}
	ast.Inspect(literal.Body, func(node ast.Node) bool {
		if _, nested := node.(*ast.FuncLit); nested {
			return false
		}
		if identifier, ok := node.(*ast.Ident); ok {
			if variable, ok := info.Defs[identifier].(*types.Var); ok && wakeMutationIsScopePointer(variable.Type()) {
				vars[variable] = struct{}{}
			}
		}
		return true
	})
	return vars
}

func resolveWakeFunctionExpr(context *wakeMutationPackage, expression ast.Expr) *types.Func {
	if expression == nil {
		return nil
	}
	info := context.pkg.TypesInfo
	switch expression := expression.(type) {
	case *ast.Ident:
		object := info.ObjectOf(expression)
		switch object := object.(type) {
		case *types.Func:
			return object
		case *types.Var:
			return context.aliases[object]
		}
	case *ast.SelectorExpr:
		if selection := info.Selections[expression]; selection != nil {
			if fn, ok := selection.Obj().(*types.Func); ok {
				return fn
			}
		}
		object := info.ObjectOf(expression.Sel)
		switch object := object.(type) {
		case *types.Func:
			return object
		case *types.Var:
			return context.aliases[object]
		}
	case *ast.ParenExpr:
		return resolveWakeFunctionExpr(context, expression.X)
	case *ast.IndexExpr:
		return resolveWakeFunctionExpr(context, expression.X)
	case *ast.IndexListExpr:
		return resolveWakeFunctionExpr(context, expression.X)
	}
	return nil
}

func resolveWakeStringExpr(context *wakeMutationPackage, expression ast.Expr) (string, bool) {
	if expression == nil {
		return "", false
	}
	info := context.pkg.TypesInfo
	if typeAndValue, ok := info.Types[expression]; ok && typeAndValue.Value != nil &&
		typeAndValue.Value.Kind() == constant.String {
		return constant.StringVal(typeAndValue.Value), true
	}
	switch expression := expression.(type) {
	case *ast.Ident:
		object := info.ObjectOf(expression)
		switch object := object.(type) {
		case *types.Const:
			if object.Val().Kind() == constant.String {
				return constant.StringVal(object.Val()), true
			}
		case *types.Var:
			value, ok := context.stringAliases[object]
			return value, ok
		}
	case *ast.BasicLit:
		if expression.Kind == token.STRING {
			return constant.StringVal(constant.MakeFromLiteral(expression.Value, token.STRING, 0)), true
		}
	case *ast.ParenExpr:
		return resolveWakeStringExpr(context, expression.X)
	case *ast.BinaryExpr:
		if expression.Op != token.ADD {
			return "", false
		}
		left, leftOK := resolveWakeStringExpr(context, expression.X)
		right, rightOK := resolveWakeStringExpr(context, expression.Y)
		return left + right, leftOK && rightOK
	}
	return "", false
}

func resolveWakeChannelExpr(context *wakeMutationPackage, expression ast.Expr) (string, bool) {
	if expression == nil {
		return "", false
	}
	info := context.pkg.TypesInfo
	switch expression := expression.(type) {
	case *ast.Ident:
		object, _ := info.ObjectOf(expression).(*types.Var)
		if object == nil {
			return "", false
		}
		if object.Name() == "stopRequest" || object.Name() == "restartSignals" || object.Name() == "loopStopped" {
			return object.Name(), true
		}
		origin, ok := context.channelAlias[object]
		return origin, ok
	case *ast.ParenExpr:
		return resolveWakeChannelExpr(context, expression.X)
	}
	return "", false
}

func isWakeArtifactExpr(context *wakeMutationPackage, expression ast.Expr) bool {
	if value, ok := resolveWakeStringExpr(context, expression); ok && strings.Contains(value, ".wake.") {
		return true
	}
	switch expression := expression.(type) {
	case *ast.ParenExpr:
		return isWakeArtifactExpr(context, expression.X)
	case *ast.BinaryExpr:
		return isWakeArtifactExpr(context, expression.X) || isWakeArtifactExpr(context, expression.Y)
	case *ast.CallExpr:
		for _, argument := range expression.Args {
			if isWakeArtifactExpr(context, argument) {
				return true
			}
		}
	}
	return false
}

func wakeRemovalPathExpression(call *ast.CallExpr, fn *types.Func) ast.Expr {
	switch wakeFunctionIdentity(fn) {
	case "golang.org/x/sys/unix::.Unlinkat", "syscall::.Unlinkat":
		if len(call.Args) >= 2 {
			return call.Args[1]
		}
	case "syscall::.Unlink", "os::.Remove", "os::.RemoveAll":
		if len(call.Args) >= 1 {
			return call.Args[0]
		}
	}
	return nil
}

func wakeFunctionIdentity(fn *types.Func) string {
	if fn == nil || fn.Pkg() == nil {
		return ""
	}
	receiver := ""
	if signature, ok := fn.Type().(*types.Signature); ok && signature.Recv() != nil {
		receiver = wakeNamedTypeName(signature.Recv().Type())
	}
	return wakeFunctionKey(fn.Pkg().Path(), receiver, fn.Name())
}

func wakeFunctionKey(pkgPath, receiver, name string) string {
	return pkgPath + "::" + receiver + "." + name
}

func wakeNamedTypeName(typ types.Type) string {
	typ = types.Unalias(typ)
	for {
		pointer, ok := typ.(*types.Pointer)
		if !ok {
			break
		}
		typ = types.Unalias(pointer.Elem())
	}
	if named, ok := typ.(*types.Named); ok && named.Obj() != nil {
		return named.Obj().Name()
	}
	return ""
}

func wakeMutationIsScopeType(typ types.Type) bool {
	if typ == nil {
		return false
	}
	typ = types.Unalias(typ)
	for {
		pointer, ok := typ.(*types.Pointer)
		if !ok {
			break
		}
		typ = types.Unalias(pointer.Elem())
	}
	named, ok := typ.(*types.Named)
	return ok && named.Obj() != nil && named.Obj().Name() == "wakeMutationScope" &&
		named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == wakeMutationCLIPackagePath
}

func wakeMutationIsScopePointer(typ types.Type) bool {
	if typ == nil {
		return false
	}
	if _, ok := types.Unalias(typ).(*types.Pointer); !ok {
		return false
	}
	return wakeMutationIsScopeType(typ)
}

func wakeFunctionSet(packagePath, receiver string, names ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(names))
	for _, name := range names {
		set[wakeFunctionKey(packagePath, receiver, name)] = struct{}{}
	}
	return set
}

func wakeMutationScopeOwnerSet() map[string]struct{} {
	set := wakeFunctionSet(
		wakeMutationCLIPackagePath,
		"",
		"newWakeMutationScope",
		"withWakeMutationScopeInDir",
		"withWakeMutationScopeNoWaitInDir",
		"withExistingWakeMutationScopeInDir",
		"withExistingWakeMutationScopeNoWaitInDir",
		"withWakeMutationScopeForRetainedCleanup",
		"withWakeMutationScopeOrRetainedDirNoWait",
	)
	for key := range wakeFunctionSet(
		wakeMutationCLIPackagePath,
		"wakeMutationScope",
		"release",
		"requireActive",
		"location",
		"unlinkWakeLock",
		"unlinkWakeLockForCleanup",
		"unlinkWakeLockForRetire",
		"requireCanonical",
		"requireCanonicalOrDetached",
		"sendPidfdSignal",
		"sendPidfdSignalForTermination",
		"queueStopRequest",
		"queueRestartSignal",
		"unlinkAt",
		"unlinkAtWith",
		"renameAt",
		"linkAtWith",
	) {
		set[key] = struct{}{}
	}
	return set
}

func wakeMutationCapabilityOwnerSet() map[string]struct{} {
	set := wakeMutationScopeOwnerSet()
	for key := range wakeFunctionSet(
		wakeMutationCLIPackagePath,
		"",
		"newDetachedWakeCleanupScope",
		"wakeUnlinkAt",
		"wakeRenameAt",
		"wakeRenameNoReplaceAt",
		"wakeLinkAt",
		"killWakeProcess",
		"sendWakePidfdSignal",
		"sendWakePidfdSignalWithLease",
	) {
		set[key] = struct{}{}
	}
	return set
}

func isWakeMutationScopeOwner(fn *types.Func) bool {
	_, ok := wakeMutationScopeOwnerSet()[wakeFunctionIdentity(fn)]
	return ok
}

func isWakeMutationCapabilityOwner(fn *types.Func) bool {
	_, ok := wakeMutationCapabilityOwnerSet()[wakeFunctionIdentity(fn)]
	return ok
}

func wakeFunctionIsMutationScopeConstructor(fn *types.Func) bool {
	return wakeFunctionIdentity(fn) == wakeFunctionKey(wakeMutationCLIPackagePath, "", "newWakeMutationScope")
}

func wakeFunctionIsLeaseConstructor(fn *types.Func) bool {
	return wakeFunctionIdentity(fn) == wakeFunctionKey(wakeMutationCapabilityPackagePath, "", "New")
}

func wakeFunctionIsLeaseEffect(fn *types.Func) bool {
	if fn == nil || fn.Pkg() == nil || fn.Pkg().Path() != wakeMutationCapabilityPackagePath {
		return false
	}
	switch fn.Name() {
	case "UnlinkAt", "UnlinkAtWith",
		"RenameAt", "RenameAtWith",
		"RenameNoReplaceAt", "RenameNoReplaceAtWith",
		"LinkAt", "LinkAtWith",
		"KillProcess", "QueueStop", "QueueRestart",
		"SendPidfdSignal", "SendPidfdSignalWith":
		signature, ok := fn.Type().(*types.Signature)
		return ok && signature.Recv() != nil && wakeNamedTypeName(signature.Recv().Type()) == "Lease"
	default:
		return false
	}
}

func wakeFunctionIsGuard(fn *types.Func) bool {
	if fn == nil || fn.Pkg() == nil || fn.Pkg().Path() != wakeMutationCLIPackagePath {
		return false
	}
	switch fn.Name() {
	case "withWakeLifecycleGuard",
		"withWakeLifecycleGuardInDir",
		"withWakeLifecycleGuardModeInDir",
		"withWakeLifecycleGuardNoWaitInDir",
		"withWakeLifecycleGuardModeAndTimeoutInDir",
		"withWakeLifecycleGuardLeaseModeAndTimeoutInDir",
		"withExistingWakeLifecycleGuardInDir",
		"withExistingWakeLifecycleGuardNoWaitInDir",
		"withExistingWakeLifecycleGuardModeInDir",
		"withWakeMutationScopeInDir",
		"withWakeMutationScopeNoWaitInDir",
		"withExistingWakeMutationScopeInDir",
		"withExistingWakeMutationScopeNoWaitInDir",
		"withWakeMutationScopeForRetainedCleanup",
		"withWakeMutationScopeOrRetainedDirNoWait":
		return true
	default:
		return false
	}
}

func wakeFunctionIsRawPidfdEffect(fn *types.Func) bool {
	return fn != nil && (wakeFunctionIdentity(fn) == wakeFunctionKey("golang.org/x/sys/unix", "", "PidfdSendSignal") ||
		wakeFunctionIdentity(fn) == wakeFunctionKey(wakeMutationCapabilityPackagePath, "", "PidfdSendSignal"))
}

func wakeFunctionIsUnleasedPidfdEffect(fn *types.Func) bool {
	return fn != nil && wakeFunctionIdentity(fn) == wakeFunctionKey(wakeMutationCapabilityPackagePath, "", "PidfdSendSignal")
}

func wakeFunctionIsRawFilesystemEffect(fn *types.Func) bool {
	if fn == nil {
		return false
	}
	switch wakeFunctionIdentity(fn) {
	case wakeFunctionKey("golang.org/x/sys/unix", "", "Unlinkat"),
		wakeFunctionKey("golang.org/x/sys/unix", "", "Renameat"),
		wakeFunctionKey("golang.org/x/sys/unix", "", "Renameat2"),
		wakeFunctionKey("golang.org/x/sys/unix", "", "RenameatxNp"),
		wakeFunctionKey("golang.org/x/sys/unix", "", "Linkat"),
		wakeFunctionKey("syscall", "", "Unlink"),
		wakeFunctionKey("syscall", "", "Unlinkat"),
		wakeFunctionKey("os", "", "Remove"),
		wakeFunctionKey("os", "", "RemoveAll"):
		return true
	default:
		return false
	}
}

func wakeFunctionIsRawUnixFilesystemEffect(fn *types.Func) bool {
	if fn == nil {
		return false
	}
	switch wakeFunctionIdentity(fn) {
	case wakeFunctionKey("golang.org/x/sys/unix", "", "Unlinkat"),
		wakeFunctionKey("golang.org/x/sys/unix", "", "Renameat"),
		wakeFunctionKey("golang.org/x/sys/unix", "", "Renameat2"),
		wakeFunctionKey("golang.org/x/sys/unix", "", "RenameatxNp"),
		wakeFunctionKey("golang.org/x/sys/unix", "", "Linkat"),
		wakeFunctionKey("syscall", "", "Unlink"),
		wakeFunctionKey("syscall", "", "Unlinkat"):
		return true
	default:
		return false
	}
}

func wakeFunctionIsProcessSignal(fn *types.Func) bool {
	return fn != nil && wakeFunctionIdentity(fn) == wakeFunctionKey("os", "Process", "Signal")
}

func wakeFunctionIsProcessKill(fn *types.Func) bool {
	return fn != nil && wakeFunctionIdentity(fn) == wakeFunctionKey("os", "Process", "Kill")
}

func isWakeMutationUnresolvedEffectCall(context *wakeMutationPackage, call *ast.CallExpr) bool {
	if context == nil || call == nil || context.pkg.PkgPath != wakeMutationCLIPackagePath {
		return false
	}
	if wakeMutationExprContainsTypeAssertion(call.Fun) {
		return true
	}
	if identifier, ok := call.Fun.(*ast.Ident); ok {
		if variable, ok := context.pkg.TypesInfo.ObjectOf(identifier).(*types.Var); ok && context.typeAssertVar[variable] {
			return true
		}
	}
	return wakeMutationEffectSignature(context.pkg.TypesInfo.TypeOf(call.Fun))
}

func wakeMutationExprContainsTypeAssertion(expression ast.Expr) bool {
	switch expression := expression.(type) {
	case *ast.TypeAssertExpr:
		return true
	case *ast.ParenExpr:
		return wakeMutationExprContainsTypeAssertion(expression.X)
	default:
		return false
	}
}

func wakeMutationEffectSignature(typ types.Type) bool {
	signature, ok := types.Unalias(typ).(*types.Signature)
	if !ok || signature.Results() == nil || signature.Results().Len() != 1 ||
		!types.AssignableTo(signature.Results().At(0).Type(), types.Universe.Lookup("error").Type()) {
		return false
	}
	params := signature.Params()
	switch params.Len() {
	case 1:
		return wakeMutationNamedType(params.At(0).Type()) == "os.Signal"
	case 3:
		return wakeMutationBasicKind(params.At(0).Type(), types.Int) &&
			wakeMutationBasicKind(params.At(1).Type(), types.String) &&
			wakeMutationBasicKind(params.At(2).Type(), types.Int)
	case 4:
		return (wakeMutationBasicKind(params.At(0).Type(), types.Int) &&
			wakeMutationIsSignalType(params.At(1).Type()) &&
			wakeMutationNamedType(params.At(2).Type()) == "golang.org/x/sys/unix.Siginfo" &&
			wakeMutationBasicKind(params.At(3).Type(), types.Int)) ||
			(wakeMutationBasicKind(params.At(0).Type(), types.Int) &&
				wakeMutationBasicKind(params.At(1).Type(), types.String) &&
				wakeMutationBasicKind(params.At(2).Type(), types.Int) &&
				wakeMutationBasicKind(params.At(3).Type(), types.String))
	case 5:
		return wakeMutationBasicKind(params.At(0).Type(), types.Int) &&
			wakeMutationBasicKind(params.At(1).Type(), types.String) &&
			wakeMutationBasicKind(params.At(2).Type(), types.Int) &&
			wakeMutationBasicKind(params.At(3).Type(), types.String) &&
			wakeMutationBasicKind(params.At(4).Type(), types.Int)
	default:
		return false
	}
}

func wakeMutationIsSignalType(typ types.Type) bool {
	switch wakeMutationNamedType(typ) {
	case "golang.org/x/sys/unix.Signal", "syscall.Signal":
		return true
	default:
		return false
	}
}

func wakeMutationBasicKind(typ types.Type, want types.BasicKind) bool {
	basic, ok := types.Unalias(typ).(*types.Basic)
	return ok && basic.Kind() == want
}

func wakeMutationNamedType(typ types.Type) string {
	pointer, isPointer := types.Unalias(typ).(*types.Pointer)
	if isPointer {
		typ = pointer.Elem()
	}
	named, ok := types.Unalias(typ).(*types.Named)
	if !ok || named.Obj() == nil || named.Obj().Pkg() == nil {
		return ""
	}
	return named.Obj().Pkg().Path() + "." + named.Obj().Name()
}

func wakeFunctionIsReflective(fn *types.Func) bool {
	if fn == nil {
		return false
	}
	key := wakeFunctionIdentity(fn)
	return key == wakeFunctionKey("reflect", "Value", "Call") ||
		key == wakeFunctionKey("reflect", "Value", "MethodByName")
}

func isWakeWaitFunction(fn *types.Func) bool {
	if fn == nil {
		return false
	}
	name := strings.ToLower(fn.Name())
	return strings.Contains(name, "wait") || strings.Contains(name, "poll")
}

func isWakeZeroSignalCall(context *wakeMutationPackage, call *ast.CallExpr) bool {
	if len(call.Args) != 1 {
		return false
	}
	typeAndValue, ok := context.pkg.TypesInfo.Types[call.Args[0]]
	if !ok || typeAndValue.Value == nil || typeAndValue.Value.Kind() != constant.Int {
		return false
	}
	value, ok := constant.Int64Val(typeAndValue.Value)
	return ok && value == 0
}
