package selfupgrade

const RefusalLimit = 8

// RefusedCandidate is a path-free, content-bound identity for one refused
// candidate. ctime, method, and execution path are excluded because Darwin
// staging changes them.
type RefusedCandidate struct {
	Platform        string `json:"platform"`
	Device          uint64 `json:"device"`
	Inode           uint64 `json:"inode"`
	Size            int64  `json:"size"`
	SHA256          string `json:"sha256"`
	EmbeddedVersion string `json:"embedded_version"`
}

func RefusedCandidateFromEvidence(evidence ImageEvidence) RefusedCandidate {
	return RefusedCandidate{
		Platform:        evidence.Platform,
		Device:          evidence.Device,
		Inode:           evidence.Inode,
		Size:            evidence.Size,
		SHA256:          evidence.SHA256,
		EmbeddedVersion: evidence.EmbeddedVersion,
	}
}

func SameRefusedCandidates(first, second []RefusedCandidate) bool {
	if len(first) != len(second) {
		return false
	}
	for index := range first {
		if first[index] != second[index] {
			return false
		}
	}
	return true
}

func RefusedCandidatesContain(candidates []RefusedCandidate, evidence ImageEvidence) bool {
	want := RefusedCandidateFromEvidence(evidence)
	for _, candidate := range candidates {
		if candidate == want {
			return true
		}
	}
	return false
}

// RememberRefusal appends a distinct refusal as the newest item and retains
// only the most recent bounded set.
func RememberRefusal(candidates []RefusedCandidate, evidence ImageEvidence) []RefusedCandidate {
	current := RefusedCandidateFromEvidence(evidence)
	remembered := make([]RefusedCandidate, 0, len(candidates)+1)
	for _, candidate := range candidates {
		if candidate != current {
			remembered = append(remembered, candidate)
		}
	}
	remembered = append(remembered, current)
	if len(remembered) > RefusalLimit {
		remembered = append([]RefusedCandidate(nil), remembered[len(remembered)-RefusalLimit:]...)
	}
	return remembered
}

func SameCandidateIdentity(first, second ImageEvidence) bool {
	return first.Platform == second.Platform &&
		first.Device == second.Device &&
		first.Inode == second.Inode &&
		first.Size == second.Size &&
		first.SHA256 == second.SHA256 &&
		first.EmbeddedVersion == second.EmbeddedVersion
}
