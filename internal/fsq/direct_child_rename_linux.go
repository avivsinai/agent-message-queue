//go:build linux

package fsq

func (r *DeliveryRoot) renameDirectChildNoReplace(oldName, newName string) error {
	dir, err := r.root.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return renameatNoReplace(int(dir.Fd()), oldName, int(dir.Fd()), newName)
}
