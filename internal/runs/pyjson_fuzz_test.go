package runs

import (
	"encoding/json"
	"os"
	"testing"
)

// TestFormatPyFloatAgainstReference checks 400 randomized values against the
// pinned golden float surface form (testdata/pyfloat_fuzz.json:
// [[value, "<surface form>"], ...]). This is the byte-equality guarantee the
// api-ws-redis spec §7 / Q2 asks for.
func TestFormatPyFloatAgainstReference(t *testing.T) {
	raw, err := os.ReadFile("testdata/pyfloat_fuzz.json")
	if err != nil {
		t.Fatalf("read fixtures: %v", err)
	}
	var pairs [][2]json.RawMessage
	if err := json.Unmarshal(raw, &pairs); err != nil {
		t.Fatalf("decode fixtures: %v", err)
	}
	var mismatches int
	for _, p := range pairs {
		var v float64
		if err := json.Unmarshal(p[0], &v); err != nil {
			t.Fatalf("decode value: %v", err)
		}
		var want string
		if err := json.Unmarshal(p[1], &want); err != nil {
			t.Fatalf("decode want: %v", err)
		}
		if got := FormatPyFloat(v); got != want {
			t.Errorf("FormatPyFloat(%v) = %q, want %q", v, got, want)
			mismatches++
			if mismatches > 20 {
				t.Fatal("too many mismatches; aborting")
			}
		}
	}
}
