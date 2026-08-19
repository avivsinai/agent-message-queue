package launch

import (
	"fmt"
	"regexp"
	"slices"
)

type valueRule func(string) bool

var safeEnvironmentValue = regexp.MustCompile(`^[A-Za-z0-9_.@+:-]{1,128}$`).MatchString
var validPOSIXEnvironmentKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString

func commonCommittedEnvRules() map[string]valueRule {
	return map[string]valueRule{
		"COLORTERM": safeEnvironmentValue,
		"LANG":      safeEnvironmentValue,
		"LC_ALL":    safeEnvironmentValue,
		"NO_COLOR": func(value string) bool {
			return value == "1" || value == "true"
		},
		"TERM": safeEnvironmentValue,
	}
}

func validateCommittedEnv(overlay map[string]string, rules map[string]valueRule) error {
	for key, value := range overlay {
		rule, ok := rules[key]
		if !ok {
			return fmt.Errorf("committed environment key %q is not allowed by adapter", key)
		}
		if !rule(value) {
			return fmt.Errorf("committed environment key %q has invalid literal value", key)
		}
	}
	return nil
}

func committedEnvKeys(rules map[string]valueRule) []string {
	keys := make([]string, 0, len(rules))
	for key := range rules {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
