package launch

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"
)

type Wrapper struct {
	Executable string   `json:"executable"`
	Args       []string `json:"args,omitempty"`
}

func (wrapper Wrapper) Validate() error {
	if !utf8.ValidString(wrapper.Executable) || strings.ContainsRune(wrapper.Executable, 0) {
		return fmt.Errorf("executable must be valid UTF-8 without NUL")
	}
	if !filepath.IsAbs(wrapper.Executable) || filepath.Clean(wrapper.Executable) != wrapper.Executable {
		return fmt.Errorf("executable must be a clean absolute path")
	}
	for i, arg := range wrapper.Args {
		if arg == "" {
			return fmt.Errorf("args[%d] must not be empty", i)
		}
		if !utf8.ValidString(arg) || strings.ContainsRune(arg, 0) {
			return fmt.Errorf("args[%d] must be valid UTF-8 without NUL", i)
		}
	}
	return nil
}

func validateWrapperFile(wrapper *Wrapper) error {
	if wrapper == nil {
		return nil
	}
	if err := wrapper.Validate(); err != nil {
		return err
	}
	info, err := os.Stat(wrapper.Executable)
	if err != nil {
		return fmt.Errorf("stat executable: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("executable must resolve to one regular file")
	}
	return nil
}

func cloneWrapper(wrapper *Wrapper) *Wrapper {
	if wrapper == nil {
		return nil
	}
	return &Wrapper{Executable: wrapper.Executable, Args: slices.Clone(wrapper.Args)}
}

func applyWrapper(plan AgentPlan, wrapper *Wrapper) (AgentPlan, error) {
	if wrapper == nil {
		return plan, nil
	}
	if err := validateWrapperFile(wrapper); err != nil {
		return AgentPlan{}, err
	}
	offset := len(wrapper.Args) + 1
	argv := make([]string, 0, offset+len(plan.Argv))
	argv = append(argv, wrapper.Executable)
	argv = append(argv, wrapper.Args...)
	argv = append(argv, plan.Argv...)
	plan.Argv = argv
	for i := range plan.DynamicArgv {
		plan.DynamicArgv[i].Index += offset
	}
	if plan.InitialInput != nil {
		plan.InitialInput.ArgvIndex += offset
	}
	plan.Wrapper = cloneWrapper(wrapper)
	return plan, plan.Validate()
}

func providerExecutable(plan AgentPlan) (string, error) {
	index := 0
	if plan.Wrapper != nil {
		index = len(plan.Wrapper.Args) + 1
	}
	if index >= len(plan.Argv) || strings.TrimSpace(plan.Argv[index]) == "" {
		return "", fmt.Errorf("provider executable is missing from wrapped argv")
	}
	return plan.Argv[index], nil
}
