//go:build darwin || linux

package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/launch"
)

var launchExecProcess = syscall.Exec

func runLaunchExec(args []string) error {
	amqArgs, targetArgv := splitDashDash(args)
	fs := flag.NewFlagSet("__launch-exec", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	rootFlag := fs.String("root", "", "")
	handleFlag := fs.String("handle", "", "")
	nonceFlag := fs.String("nonce", "", "")
	targetFlag := fs.String("target", "", "")
	executionOptionsFlag := fs.String(managedExecutionOptionsFlag, "", "")
	if err := fs.Parse(amqArgs); err != nil || len(fs.Args()) != 0 || *rootFlag == "" || *handleFlag == "" || *nonceFlag == "" || *targetFlag == "" || len(targetArgv) == 0 {
		return ActionRequiredError("refusing incomplete private launch wrapper invocation")
	}
	var executionOptions *launch.PrepareExecutionOptions
	if *executionOptionsFlag != "" {
		decoded, err := decodeManagedExecutionOptions(*executionOptionsFlag)
		if err != nil {
			return ActionRequiredError("refusing invalid managed execution options: %v", err)
		}
		executionOptions = &decoded
	}
	identity, err := fsq.SnapshotDeliveryRoot(*rootFlag)
	if err != nil {
		return ActionRequiredError("validate launch session root: %v", err)
	}
	root, err := fsq.OpenDeliveryRoot(*rootFlag, identity)
	if err != nil {
		return ActionRequiredError("open launch session root: %v", err)
	}
	defer func() { _ = root.Close() }()
	cwd, err := os.Getwd()
	if err != nil {
		return ActionRequiredError("resolve launch working directory: %v", err)
	}
	amqExecutable, err := os.Executable()
	if err != nil {
		return ActionRequiredError("resolve launch wrapper executable: %v", err)
	}
	prepared, err := launch.PrepareExecution(root, *handleFlag, *nonceFlag, launch.ExecutionEnvelope{
		Cwd: cwd, AMQExecutable: amqExecutable, ProviderExecutable: *targetFlag,
		TargetArgv: targetArgv, Environment: os.Environ(), Execution: executionOptions,
	})
	if err != nil {
		return ActionRequiredError("refusing managed launch execution: %v", err)
	}
	resolvedArgv, err := launch.ResolveExecutionArgv(prepared)
	if err != nil {
		revertErr := launch.RevertExecution(root, *handleFlag, *nonceFlag)
		return errors.Join(ActionRequiredError("refusing unresolved managed launch execution: %v", err), revertErr)
	}
	execErr := launchExecProcess(*targetFlag, resolvedArgv, os.Environ())
	if execErr == nil {
		execErr = fmt.Errorf("provider exec returned without replacing process")
	}
	revertErr := launch.RevertExecution(root, *handleFlag, *nonceFlag)
	return errors.Join(execErr, revertErr)
}
