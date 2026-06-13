package hyperopt

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func mustDate(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// TestExpandingAnchoredParity replays 300+ (start,end,folds,embargo) cases
// captured from the Python research.walkforward.expanding_anchored and asserts
// exact date equality on every segment boundary plus matching error behavior.
// Includes the spec §3.1 golden worked example and the four ValueError paths.
func TestExpandingAnchoredParity(t *testing.T) {
	raw, err := os.ReadFile("testdata/wf_parity.json")
	if err != nil {
		t.Skipf("parity fixture missing (%v)", err)
	}
	var cases []struct {
		Start    string `json:"start"`
		End      string `json:"end"`
		NFolds   int    `json:"n_folds"`
		Embargo  int    `json:"embargo"`
		Segments []struct {
			TS string `json:"ts"`
			TE string `json:"te"`
		} `json:"segments"`
		Error *string `json:"error"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for i, c := range cases {
		segs, gotErr := ExpandingAnchored(mustDate(c.Start), mustDate(c.End), c.NFolds, c.Embargo)
		if c.Error != nil {
			if gotErr == nil {
				t.Fatalf("case %d (%s..%s f=%d e=%d): want error %q, got nil", i, c.Start, c.End, c.NFolds, c.Embargo, *c.Error)
			}
			if gotErr.Error() != *c.Error {
				t.Fatalf("case %d: error %q want %q", i, gotErr.Error(), *c.Error)
			}
			continue
		}
		if gotErr != nil {
			t.Fatalf("case %d (%s..%s f=%d e=%d): unexpected error %v", i, c.Start, c.End, c.NFolds, c.Embargo, gotErr)
		}
		if len(segs) != len(c.Segments) {
			t.Fatalf("case %d: %d segments, want %d", i, len(segs), len(c.Segments))
		}
		for j, want := range c.Segments {
			if !segs[j].TestStart.Equal(mustDate(want.TS)) || !segs[j].TestEnd.Equal(mustDate(want.TE)) {
				t.Fatalf("case %d seg %d: got [%s..%s] want [%s..%s]", i, j,
					segs[j].TestStart.Format("2006-01-02"), segs[j].TestEnd.Format("2006-01-02"), want.TS, want.TE)
			}
		}
	}
	t.Logf("%d walk-forward cases parity-verified", len(cases))
}

// TestExpandingAnchoredGolden is the spec §3.1 worked example (start=2022-01-01,
// end=2024-12-31, n_folds=3, embargo=5), asserted directly against the ACTUAL
// reference output. NOTE: the spec doc's printed fold boundaries
// ([2022-06-06..], buffer=365) are stale — the live research.walkforward yields
// buffer = 1096//3 = 365 from 2022-01-01 => first test_start 2023-01-06. These
// values were captured from the venv and match the 305-case parity fixture.
func TestExpandingAnchoredGolden(t *testing.T) {
	segs, err := ExpandingAnchored(mustDate("2022-01-01"), mustDate("2024-12-31"), 3, 5)
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]string{
		{"2023-01-06", "2023-09-04"},
		{"2023-09-05", "2024-05-03"},
		{"2024-05-04", "2024-12-31"},
	}
	if len(segs) != 3 {
		t.Fatalf("got %d folds", len(segs))
	}
	for i, w := range want {
		if segs[i].TestStart.Format("2006-01-02") != w[0] || segs[i].TestEnd.Format("2006-01-02") != w[1] {
			t.Fatalf("fold %d: [%s..%s] want %v", i, segs[i].TestStart.Format("2006-01-02"), segs[i].TestEnd.Format("2006-01-02"), w)
		}
	}
	// Embargo quirk: consecutive segments are exactly adjacent.
	for i := 1; i < len(segs); i++ {
		if !segs[i].TestStart.Equal(addDays(segs[i-1].TestEnd, 1)) {
			t.Fatalf("segments %d/%d not adjacent (embargo must be vestigial)", i-1, i)
		}
	}
	// Final fold ends exactly at end.
	if !segs[len(segs)-1].TestEnd.Equal(mustDate("2024-12-31")) {
		t.Fatal("final fold must end at end")
	}
}
