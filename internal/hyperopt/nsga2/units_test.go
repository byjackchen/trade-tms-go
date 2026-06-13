package nsga2

import (
	"context"
	"errors"
	"math"
	"sync/atomic"
	"testing"
)

func TestDominatesLoss(t *testing.T) {
	cases := []struct {
		a, b []float64
		want bool
	}{
		{[]float64{1, 1}, []float64{2, 2}, true},           // a strictly better in both
		{[]float64{1, 2}, []float64{1, 3}, true},           // equal in one, better in other
		{[]float64{1, 3}, []float64{1, 2}, false},          // worse in one
		{[]float64{1, 1}, []float64{1, 1}, false},          // identical -> no domination
		{[]float64{1, 3}, []float64{2, 2}, false},          // trade-off, incomparable
		{[]float64{math.NaN(), 1}, []float64{2, 2}, false}, // NaN never dominates
	}
	for i, c := range cases {
		if got := dominatesLoss(c.a, c.b); got != c.want {
			t.Errorf("case %d: dominatesLoss(%v,%v)=%v want %v", i, c.a, c.b, got, c.want)
		}
	}
}

func mkInd(id int, loss ...float64) *individual {
	return &individual{id: id, loss: loss, values: loss}
}

func TestFastNonDominatedSort(t *testing.T) {
	// Front 0: (1,4),(2,2),(4,1) mutually non-dominating.
	// Front 1: (3,5),(5,3) dominated by front 0 but not each other.
	// Front 2: (6,6) dominated by everything.
	pop := []*individual{
		mkInd(0, 1, 4), mkInd(1, 2, 2), mkInd(2, 4, 1),
		mkInd(3, 3, 5), mkInd(4, 5, 3), mkInd(5, 6, 6),
	}
	fronts := fastNonDominatedSort(pop)
	if len(fronts) != 3 {
		t.Fatalf("expected 3 fronts, got %d", len(fronts))
	}
	wantRank := map[int]int{0: 0, 1: 0, 2: 0, 3: 1, 4: 1, 5: 2}
	for _, f := range fronts {
		for _, ind := range f {
			if ind.rank != wantRank[ind.id] {
				t.Errorf("ind %d rank=%d want %d", ind.id, ind.rank, wantRank[ind.id])
			}
		}
	}
	if len(fronts[0]) != 3 || len(fronts[1]) != 2 || len(fronts[2]) != 1 {
		t.Errorf("front sizes = %d,%d,%d want 3,2,1", len(fronts[0]), len(fronts[1]), len(fronts[2]))
	}
}

func TestCrowdingDistanceBoundariesInfinite(t *testing.T) {
	// Three points on a line: boundary points should get the (collapsed) inf
	// neighbour treatment per Optuna; with the -inf/+inf sentinels the boundary
	// gap is finite (one neighbour) -> boundaries are most isolated.
	front := []*individual{
		mkInd(0, 0, 2), // boundary low f1
		mkInd(1, 1, 1), // middle
		mkInd(2, 2, 0), // boundary high f1
	}
	dist := crowdingDistances(front)
	// Middle point's distance should be finite and equal in both dims:
	// f1 dim: (2-0)/(2-0)=1 ; f2 dim: (2-0)/(2-0)=1 ; total 2.
	if math.Abs(dist[1]-2.0) > 1e-12 {
		t.Errorf("middle crowding distance = %v want 2.0", dist[1])
	}
	// Boundaries get gap = above-below where one side is the +/-inf sentinel,
	// which collapses with the other boundary's value; here boundaries end up
	// with the larger isolation. Just assert boundaries >= middle.
	if dist[0] < dist[1] || dist[2] < dist[1] {
		t.Errorf("boundary distances (%v,%v) should be >= middle (%v)", dist[0], dist[2], dist[1])
	}
}

func TestCrowdingDistanceAllEqualDimensionIgnored(t *testing.T) {
	// All equal in f1 -> that dimension contributes 0; only f2 varies.
	front := []*individual{
		mkInd(0, 5, 0),
		mkInd(1, 5, 1),
		mkInd(2, 5, 2),
	}
	dist := crowdingDistances(front)
	// f1 all-equal => skipped; f2: middle gets (2-0)/(2-0)=1.
	if math.Abs(dist[1]-1.0) > 1e-12 {
		t.Errorf("middle distance = %v want 1.0 (f1 dim ignored)", dist[1])
	}
}

func TestSelectEliteTruncatesByCrowding(t *testing.T) {
	// One front of 4 collinear points, keep 2: the two boundary (most isolated)
	// points must survive.
	pop := []*individual{
		mkInd(0, 0, 3),
		mkInd(1, 1, 2),
		mkInd(2, 2, 1),
		mkInd(3, 3, 0),
	}
	elite := selectElite(pop, 2)
	if len(elite) != 2 {
		t.Fatalf("elite size = %d want 2", len(elite))
	}
	got := map[int]bool{elite[0].id: true, elite[1].id: true}
	if !got[0] || !got[3] {
		t.Errorf("expected boundary points 0 and 3 to survive, got ids %v", got)
	}
}

func TestIntParamInclusiveBounds(t *testing.T) {
	s, err := NewSearchSpace(IntParam("k", 3, 10))
	if err != nil {
		t.Fatal(err)
	}
	r := newRNG(42)
	seenLo, seenHi := false, false
	for i := 0; i < 5000; i++ {
		g := s.sample(r)
		v := s.decode(g)["k"].(int64)
		if v < 3 || v > 10 {
			t.Fatalf("int sample %d outside [3,10]", v)
		}
		if v == 3 {
			seenLo = true
		}
		if v == 10 {
			seenHi = true
		}
	}
	if !seenLo || !seenHi {
		t.Errorf("inclusive ends not both sampled (lo=%v hi=%v)", seenLo, seenHi)
	}
}

func TestLogFloatParamWithinBounds(t *testing.T) {
	s, err := NewSearchSpace(LogFloatParam("lr", 1e-5, 1e-1))
	if err != nil {
		t.Fatal(err)
	}
	r := newRNG(1)
	for i := 0; i < 2000; i++ {
		g := s.sample(r)
		v := s.decode(g)["lr"].(float64)
		if v < 1e-5*(1-1e-9) || v > 1e-1*(1+1e-9) {
			t.Fatalf("log sample %g outside [1e-5,1e-1]", v)
		}
	}
	// log space should NOT be contained if we hand an out-of-range internal gene.
	if s.contains(Genome{math.Log10(1e-1) + 1}) {
		t.Errorf("contains accepted out-of-range log gene")
	}
}

func TestCategoricalParam(t *testing.T) {
	s, err := NewSearchSpace(CategoricalParam("mode", "a", "b", "c"))
	if err != nil {
		t.Fatal(err)
	}
	r := newRNG(5)
	counts := map[any]int{}
	for i := 0; i < 3000; i++ {
		g := s.sample(r)
		counts[s.decode(g)["mode"]]++
	}
	for _, c := range []any{"a", "b", "c"} {
		if counts[c] == 0 {
			t.Errorf("category %v never sampled", c)
		}
	}
	if s.contains(Genome{3}) || s.contains(Genome{-1}) {
		t.Errorf("contains accepted out-of-range categorical index")
	}
}

func TestChildrenStayInBounds(t *testing.T) {
	// Mixed space; run several generations and assert every asked child decodes
	// in-bounds (crossover+mutation must respect the search space).
	s, err := NewSearchSpace(
		FloatParam("a", -2, 2),
		IntParam("b", 0, 9),
		LogFloatParam("c", 1e-3, 1e3),
		CategoricalParam("d", "x", "y"),
	)
	if err != nil {
		t.Fatal(err)
	}
	opt, err := New(Config{
		Space:          s,
		Objectives:     []ObjectiveSpec{{Name: "o1"}, {Name: "o2"}},
		PopulationSize: 20,
		Seed:           3,
	})
	if err != nil {
		t.Fatal(err)
	}
	eval := EvaluatorFunc(func(_ context.Context, tr Trial) ([]float64, error) {
		a := tr.Params["a"].(float64)
		b := float64(tr.Params["b"].(int64))
		if a < -2 || a > 2 {
			t.Fatalf("param a out of bounds: %g", a)
		}
		if b < 0 || b > 9 {
			t.Fatalf("param b out of bounds: %g", b)
		}
		c := tr.Params["c"].(float64)
		if c < 1e-3*(1-1e-9) || c > 1e3*(1+1e-9) {
			t.Fatalf("param c out of bounds: %g", c)
		}
		if d := tr.Params["d"]; d != "x" && d != "y" {
			t.Fatalf("param d invalid: %v", d)
		}
		return []float64{a, b}, nil
	})
	if _, err := opt.Optimize(context.Background(), eval, RunConfig{Generations: 25}); err != nil {
		t.Fatal(err)
	}
}

func TestFailedEvalsExcludedFromPopulation(t *testing.T) {
	s, err := NewSearchSpace(FloatParam("x", 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	opt, err := New(Config{
		Space:          s,
		Objectives:     []ObjectiveSpec{{Name: "f1"}, {Name: "f2"}},
		PopulationSize: 10,
		Seed:           8,
	})
	if err != nil {
		t.Fatal(err)
	}
	failErr := errors.New("synthetic failure")
	var failed int
	eval := EvaluatorFunc(func(_ context.Context, tr Trial) ([]float64, error) {
		x := tr.Params["x"].(float64)
		if x > 0.5 { // fail roughly half
			failed++
			return nil, failErr
		}
		return []float64{x, 1 - x}, nil
	})
	res, err := opt.Optimize(context.Background(), eval, RunConfig{Generations: 15})
	if err != nil {
		t.Fatal(err)
	}
	if res.Failures == 0 {
		t.Fatal("expected some failures")
	}
	// Every surviving individual must have valid (non-NaN) objective values.
	for _, sol := range res.Population {
		if len(sol.Values) != 2 || math.IsNaN(sol.Values[0]) || math.IsNaN(sol.Values[1]) {
			t.Errorf("failed individual leaked into population: %+v", sol)
		}
		if sol.Values[0] > 0.5+1e-12 {
			t.Errorf("an x>0.5 (failing) individual survived: x=%g", sol.Values[0])
		}
	}
}

func TestContextCancellationAborts(t *testing.T) {
	s, _ := NewSearchSpace(FloatParam("x", 0, 1))
	opt, _ := New(Config{
		Space:          s,
		Objectives:     []ObjectiveSpec{{Name: "f1"}, {Name: "f2"}},
		PopulationSize: 16,
		Seed:           2,
	})
	ctx, cancel := context.WithCancel(context.Background())
	var n atomic.Int64
	eval := EvaluatorFunc(func(c context.Context, tr Trial) ([]float64, error) {
		if n.Add(1) == 5 {
			cancel()
		}
		if c.Err() != nil {
			return nil, c.Err()
		}
		x := tr.Params["x"].(float64)
		return []float64{x, 1 - x}, nil
	})
	_, err := opt.Optimize(ctx, eval, RunConfig{Generations: 100, Parallelism: 4})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestAskTellBalanceAndGenerationAdvance(t *testing.T) {
	s, _ := NewSearchSpace(FloatParam("x", 0, 1))
	opt, _ := New(Config{
		Space:          s,
		Objectives:     []ObjectiveSpec{{Name: "f1"}, {Name: "f2"}},
		PopulationSize: 6,
		Seed:           11,
	})
	if opt.Generation() != 0 {
		t.Fatalf("initial generation = %d want 0", opt.Generation())
	}
	// Ask all 6, then Ask again should return ok=false until told.
	var trials []Trial
	for {
		tr, ok := opt.Ask()
		if !ok {
			break
		}
		trials = append(trials, tr)
	}
	if len(trials) != 6 {
		t.Fatalf("asked %d want 6", len(trials))
	}
	if _, ok := opt.Ask(); ok {
		t.Fatal("Ask returned a trial before generation was told out")
	}
	for _, tr := range trials {
		x := tr.Params["x"].(float64)
		if err := opt.Tell(tr, []float64{x, 1 - x}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if opt.Generation() != 1 {
		t.Fatalf("generation after full tell = %d want 1", opt.Generation())
	}
	if len(opt.Population()) != 6 {
		t.Fatalf("population size = %d want 6", len(opt.Population()))
	}
	// Telling an unknown trial errors.
	if err := opt.Tell(Trial{ID: 9999}, []float64{0, 0}, nil); err == nil {
		t.Fatal("expected error telling unknown trial")
	}
}

func TestConfigValidation(t *testing.T) {
	good, _ := NewSearchSpace(FloatParam("x", 0, 1))
	cases := []Config{
		{Space: nil, Objectives: []ObjectiveSpec{{Name: "a"}}},
		{Space: good, Objectives: nil},
		{Space: good, Objectives: []ObjectiveSpec{{Name: "a"}}, PopulationSize: 1},
		{Space: good, Objectives: []ObjectiveSpec{{Name: "a"}}, CrossoverProb: 1.5},
		{Space: good, Objectives: []ObjectiveSpec{{Name: "a"}}, SwappingProb: -0.1},
		{Space: good, Objectives: []ObjectiveSpec{{Name: "a"}}, MutationProb: 2},
	}
	for i, c := range cases {
		if _, err := New(c); err == nil {
			t.Errorf("case %d: expected validation error", i)
		}
	}
	// MutationProb default = 1/n_params when left at the zero sentinel via -1.
	opt, err := New(Config{Space: good, Objectives: []ObjectiveSpec{{Name: "a"}}, MutationProb: -1})
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(opt.Config().MutationProb-1.0) > 1e-12 {
		t.Errorf("default mutation prob = %g want 1.0 (1/1 param)", opt.Config().MutationProb)
	}
}
