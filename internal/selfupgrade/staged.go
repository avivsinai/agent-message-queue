package selfupgrade

// SameImageEvidenceExceptMethodPath compares image authority while ignoring
// how and where the image was observed.
func SameImageEvidenceExceptMethodPath(first, second ImageEvidence) bool {
	first.Method = second.Method
	first.ExecutionPath = second.ExecutionPath
	return first == second
}

// SameDarwinStagedImageEvidence ignores the ctime and path changes caused by
// Darwin hardlink staging. Device, inode, size, digest, and version remain
// the content and identity proof.
func SameDarwinStagedImageEvidence(first, second ImageEvidence) bool {
	if first.Platform != "darwin" || second.Platform != "darwin" {
		return first == second
	}
	if first.Schema != second.Schema ||
		first.Device != second.Device ||
		first.Inode != second.Inode ||
		first.Size != second.Size ||
		first.SHA256 != second.SHA256 ||
		first.EmbeddedVersion != second.EmbeddedVersion {
		return false
	}
	return true
}
