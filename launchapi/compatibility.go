package launchapi

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const ContractSemverV1 = "0.61.1"

var compatibilityFeaturesV1 = []string{
	"launch_intent_v1",
	"prepare_apply_v1",
	"lifecycle_v1",
	"managed_tmux_v1",
	"plan_only_commands_v1",
	FeatureInitialInput,
	FeaturePlacement,
}

func Compatibility() CompatibilityV1 {
	return CompatibilityV1{
		ContractSemver: ContractSemverV1,
		IntentVersions: []int{IntentVersionV1},
		ResultVersions: []int{ResultVersionV1},
		Features:       slices.Clone(compatibilityFeaturesV1),
	}
}

func Negotiate(requirement RequirementV1) (NegotiatedV1, error) {
	if !semverRangeContains(requirement.ContractSemver, ContractSemverV1) {
		return NegotiatedV1{}, fmt.Errorf("contract semver %q does not include %s", requirement.ContractSemver, ContractSemverV1)
	}
	if requirement.IntentVersion != IntentVersionV1 {
		return NegotiatedV1{}, fmt.Errorf("unsupported intent version %d", requirement.IntentVersion)
	}
	if requirement.ResultVersion != ResultVersionV1 {
		return NegotiatedV1{}, fmt.Errorf("unsupported result version %d", requirement.ResultVersion)
	}
	seen := make(map[string]struct{}, len(requirement.Features))
	for _, feature := range requirement.Features {
		if _, ok := seen[feature]; ok {
			return NegotiatedV1{}, fmt.Errorf("duplicate required feature %q", feature)
		}
		seen[feature] = struct{}{}
		if !slices.Contains(compatibilityFeaturesV1, feature) {
			return NegotiatedV1{}, fmt.Errorf("unsupported required feature %q", feature)
		}
	}
	features := make([]string, 0, len(requirement.Features))
	for _, feature := range compatibilityFeaturesV1 {
		if _, ok := seen[feature]; ok {
			features = append(features, feature)
		}
	}
	return NegotiatedV1{
		ContractSemver: ContractSemverV1,
		IntentVersion:  IntentVersionV1,
		ResultVersion:  ResultVersionV1,
		Features:       features,
	}, nil
}

type semanticVersion struct {
	major int
	minor int
	patch int
}

var semanticVersionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

func parseSemanticVersion(value string) (semanticVersion, bool) {
	match := semanticVersionPattern.FindStringSubmatch(value)
	if match == nil {
		return semanticVersion{}, false
	}
	major, _ := strconv.Atoi(match[1])
	minor, _ := strconv.Atoi(match[2])
	patch, _ := strconv.Atoi(match[3])
	return semanticVersion{major: major, minor: minor, patch: patch}, true
}

func compareSemanticVersions(left, right semanticVersion) int {
	if left.major != right.major {
		return left.major - right.major
	}
	if left.minor != right.minor {
		return left.minor - right.minor
	}
	return left.patch - right.patch
}

func semverRangeContains(requirement, candidate string) bool {
	candidateVersion, ok := parseSemanticVersion(candidate)
	if !ok || strings.TrimSpace(requirement) != requirement || requirement == "" {
		return false
	}
	terms := strings.Fields(requirement)
	if len(terms) == 1 {
		if exact, exactOK := parseSemanticVersion(terms[0]); exactOK {
			return compareSemanticVersions(candidateVersion, exact) == 0
		}
	}
	for _, term := range terms {
		operator := ""
		versionText := term
		for _, prefix := range []string{">=", "<=", ">", "<", "="} {
			if strings.HasPrefix(term, prefix) {
				operator = prefix
				versionText = strings.TrimPrefix(term, prefix)
				break
			}
		}
		version, parsed := parseSemanticVersion(versionText)
		if !parsed || operator == "" {
			return false
		}
		comparison := compareSemanticVersions(candidateVersion, version)
		matches := map[string]bool{
			">=": comparison >= 0,
			"<=": comparison <= 0,
			">":  comparison > 0,
			"<":  comparison < 0,
			"=":  comparison == 0,
		}[operator]
		if !matches {
			return false
		}
	}
	return true
}
