package semver

import "testing"

func TestParseAndCompareCore(t *testing.T) {
	parse := []struct {
		raw string
		ok  bool
	}{
		{"1.2.3", true},
		{"v1.2.3", true},
		{"1.2.3-rc.1+build.5", true},
		{"0.69.0-5-g7a7fbc6", true},
		{"01.2.3", false},
		{"1.02.3", false},
		{"1.2.03", false},
		{"1.2", false},
		{"1.2.3-", false},
		{"1.2.3-01", false},
		{"1.2.3+", false},
		{"", false},
		{"dev", false},
	}
	for _, test := range parse {
		if _, ok := Parse(test.raw); ok != test.ok {
			t.Errorf("Parse(%q) ok = %t, want %t", test.raw, ok, test.ok)
		}
	}
	compare := []struct {
		a, b string
		want int
	}{
		{"1.2.3", "1.2.4", -1},
		{"1.2.3", "1.2.3-rc.1", 0},
		{"10.0.0", "9.9.9", 1},
		{"v1.10.0", "1.9.0", 1},
	}
	for _, test := range compare {
		a, _ := Parse(test.a)
		b, _ := Parse(test.b)
		if got := CompareCore(a, b); got != test.want {
			t.Errorf("CompareCore(%q, %q) = %d, want %d", test.a, test.b, got, test.want)
		}
	}
}
