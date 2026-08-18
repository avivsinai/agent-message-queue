//go:build darwin || linux

package cli

func wakeRestartRecordBinaryDirGone(record wakeRestartRecord) bool {
	paths := []string{record.StagePath}
	if record.BoundImage != nil {
		paths = append(paths, record.BoundImage.ExecutionPath)
	}
	if record.PreviousBoundImage != nil {
		paths = append(paths, record.PreviousBoundImage.ExecutionPath)
	}
	return wakeAnyRecordedPathParentGone(paths)
}
