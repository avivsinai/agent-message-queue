//go:build linux

package fsq

// renameDirectChildNoReplace publishes a staged directory through
// renameatNoReplace. Where that falls back to check-then-rename, a plain
// rename could replace an empty directory created inside the window; the
// callers that create session names exclusively hold
// launch.WithSessionCreationLock, which closes that window between them.
func (r *DeliveryRoot) renameDirectChildNoReplace(oldName, newName string) error {
	dir, err := r.root.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return renameatNoReplace(int(dir.Fd()), oldName, int(dir.Fd()), newName)
}
