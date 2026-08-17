package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"

	"github.com/avivsinai/agent-message-queue/internal/launch"
)

const managedExecutionOptionsFlag = "execution-options"

func validateManagedExecutionOptions(options launch.PrepareExecutionOptions) error {
	return launch.ValidatePrepareExecutionOptionsGrammar(options, launch.PrepareExecutionOptionsPresence{
		Injector: options.InjectorMode != "" || options.InjectorVia != "" || len(options.InjectorArgs) != 0,
		Symphony: len(options.SymphonyEvents) != 0 || options.SymphonyWorkspaceKey != "",
	})
}

func encodeManagedExecutionOptions(options launch.PrepareExecutionOptions) (string, error) {
	if err := validateManagedExecutionOptions(options); err != nil {
		return "", err
	}
	data, err := json.Marshal(options)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func decodeManagedExecutionOptions(encoded string) (launch.PrepareExecutionOptions, error) {
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return launch.PrepareExecutionOptions{}, fmt.Errorf("decode execution options: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var options launch.PrepareExecutionOptions
	if err := decoder.Decode(&options); err != nil {
		return launch.PrepareExecutionOptions{}, fmt.Errorf("decode execution options: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return launch.PrepareExecutionOptions{}, fmt.Errorf("decode execution options: trailing data")
	}
	if err := validateManagedExecutionOptions(options); err != nil {
		return launch.PrepareExecutionOptions{}, err
	}
	return options, nil
}
