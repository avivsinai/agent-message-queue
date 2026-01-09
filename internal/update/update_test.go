package update

import "testing"

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		current string
		latest  string
		want    int
		ok      bool
	}{
		{"v1.2.3", "v1.2.3", 0, true},
		{"v1.2.3", "v1.2.4", -1, true},
		{"1.2.3", "v1.2.4", -1, true},
		{"v2.0.0", "v1.9.9", 1, true},
		{"dev", "v1.2.3", 0, false},
	}

	for _, tc := range cases {
		got, ok := CompareVersions(tc.current, tc.latest)
		if ok != tc.ok {
			t.Fatalf("CompareVersions(%q, %q) ok=%v want %v", tc.current, tc.latest, ok, tc.ok)
		}
		if ok && got != tc.want {
			t.Fatalf("CompareVersions(%q, %q)=%d want %d", tc.current, tc.latest, got, tc.want)
		}
	}
}

func TestNormalizeVersion(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"1.2.3", "v1.2.3"},
		{"v1.2.3", "v1.2.3"},
		{"dev", "dev"},
		{"", ""},
	}

	for _, tc := range cases {
		got := NormalizeVersion(tc.input)
		if got != tc.want {
			t.Fatalf("NormalizeVersion(%q)=%q want %q", tc.input, got, tc.want)
		}
	}
}

func TestParseChecksums(t *testing.T) {
	data := []byte(`abcd1234  amq_0.1.0_darwin_arm64.tar.gz
efgh5678 amq_0.1.0_linux_amd64.tar.gz

`) // trailing newline

	checksums, err := ParseChecksums(data)
	if err != nil {
		t.Fatalf("ParseChecksums: %v", err)
	}
	if checksums["amq_0.1.0_darwin_arm64.tar.gz"] != "abcd1234" {
		t.Fatalf("checksum mismatch for darwin asset")
	}
	if checksums["amq_0.1.0_linux_amd64.tar.gz"] != "efgh5678" {
		t.Fatalf("checksum mismatch for linux asset")
	}
}

func TestAssetName(t *testing.T) {
	name, err := AssetName("v0.1.0", "darwin", "arm64")
	if err != nil {
		t.Fatalf("AssetName error: %v", err)
	}
	if name != "amq_0.1.0_darwin_arm64.tar.gz" {
		t.Fatalf("AssetName=%q", name)
	}
	winName, err := AssetName("v0.1.0", "windows", "amd64")
	if err != nil {
		t.Fatalf("AssetName windows error: %v", err)
	}
	if winName != "amq_0.1.0_windows_amd64.zip" {
		t.Fatalf("AssetName windows=%q", winName)
	}
}
