package cli

import "testing"

func TestIsCanonicalSessionName(t *testing.T) {
	valid := []string{"collab", "feature-x", "auth", "my_session", "abc123"}
	for _, name := range valid {
		if !isCanonicalSessionName(name) {
			t.Errorf("isCanonicalSessionName(%q) = false, want true", name)
		}
	}
	invalid := []string{"", "Collab", "foo.bar", "my/session", ".", "..", "has space"}
	for _, name := range invalid {
		if isCanonicalSessionName(name) {
			t.Errorf("isCanonicalSessionName(%q) = true, want false", name)
		}
	}
}

func TestIsSafeLegacySessionName(t *testing.T) {
	legacy := []string{"Collab", "foo.bar", "My_Session", "Auth.v2"}
	for _, name := range legacy {
		if !isSafeLegacySessionName(name) {
			t.Errorf("isSafeLegacySessionName(%q) = false, want true", name)
		}
	}
	notLegacy := []string{
		"collab", "feature-x", "", ".", "..", "my/session", `my\session`, "foo+bar", "has space",
	}
	for _, name := range notLegacy {
		if isSafeLegacySessionName(name) {
			t.Errorf("isSafeLegacySessionName(%q) = true, want false", name)
		}
	}
}
