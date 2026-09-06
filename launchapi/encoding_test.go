package launchapi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestV061ResultGoldensRemainDecodeCompatible(t *testing.T) {
	for _, test := range []struct {
		file   string
		result any
	}{
		{file: "prepare_result_v0610.golden.json", result: &PrepareResultV1{}},
		{file: "apply_result_v0610.golden.json", result: &ApplyResultV1{}},
	} {
		data, err := os.ReadFile(filepath.Join("testdata", test.file))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, test.result); err != nil {
			t.Fatalf("decode v0.61.0 fixture %s: %v", test.file, err)
		}
		switch result := test.result.(type) {
		case *PrepareResultV1:
			if result.SubjectSchema != 0 || result.Preview.Capabilities != nil {
				t.Fatalf("v0.61.0 Prepare defaults changed: %#v", result)
			}
		case *ApplyResultV1:
			if result.SubjectSchema != 0 || result.Hints != nil {
				t.Fatalf("v0.61.0 Apply defaults changed: %#v", result)
			}
		}
	}
}

func TestMarshalResultV1RejectsNonContractType(t *testing.T) {
	if _, err := MarshalResultV1(struct{}{}); err == nil {
		t.Fatal("canonical result encoder accepted a non-contract type")
	}
}
