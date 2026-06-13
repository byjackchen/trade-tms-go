package nsga2

import (
	"context"
	"math"
	"testing"
)

// ---- Standard multi-objective test problems -------------------------------
//
// ZDT1 and ZDT2 are canonical bi-objective benchmarks (Zitzler-Deb-Thiele).
// Both use n real variables in [0,1]; both minimize (f1, f2).
//
//	f1(x) = x_0
//	g(x)  = 1 + 9 * sum(x_1..x_{n-1}) / (n-1)
//	ZDT1: f2 = g * (1 - sqrt(f1/g))   -> true front f2 = 1 - sqrt(f1), f1 in [0,1]
//	ZDT2: f2 = g * (1 - (f1/g)^2)     -> true front f2 = 1 - f1^2,     f1 in [0,1]
//
// The Pareto-optimal set is x_0 in [0,1], x_1..x_{n-1} = 0 (so g = 1).

const zdtN = 30

func zdtSpace(t *testing.T) *SearchSpace {
	t.Helper()
	ps := make([]Param, zdtN)
	for i := range ps {
		ps[i] = FloatParam("x"+itoa(i), 0, 1)
	}
	s, err := NewSearchSpace(ps...)
	if err != nil {
		t.Fatalf("NewSearchSpace: %v", err)
	}
	return s
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func zdtVars(p Params) []float64 {
	x := make([]float64, zdtN)
	for i := 0; i < zdtN; i++ {
		x[i] = p["x"+itoa(i)].(float64)
	}
	return x
}

var zdt1Eval EvaluatorFunc = func(_ context.Context, tr Trial) ([]float64, error) {
	x := zdtVars(tr.Params)
	f1 := x[0]
	var s float64
	for i := 1; i < zdtN; i++ {
		s += x[i]
	}
	g := 1 + 9*s/float64(zdtN-1)
	f2 := g * (1 - math.Sqrt(f1/g))
	return []float64{f1, f2}, nil
}

var zdt2Eval EvaluatorFunc = func(_ context.Context, tr Trial) ([]float64, error) {
	x := zdtVars(tr.Params)
	f1 := x[0]
	var s float64
	for i := 1; i < zdtN; i++ {
		s += x[i]
	}
	g := 1 + 9*s/float64(zdtN-1)
	f2 := g * (1 - (f1/g)*(f1/g))
	return []float64{f1, f2}, nil
}

// trueFrontDist returns the Euclidean distance from a found (f1,f2) point to the
// analytic ZDT front (parameterized by f1 in [0,1]).
func zdt1FrontDist(f1, f2 float64) float64 {
	// Front: F2 = 1 - sqrt(F1). Minimize distance over F1 in [0,1] by sampling.
	return minFrontDist(f1, f2, func(a float64) float64 { return 1 - math.Sqrt(a) })
}

func zdt2FrontDist(f1, f2 float64) float64 {
	return minFrontDist(f1, f2, func(a float64) float64 { return 1 - a*a })
}

func minFrontDist(f1, f2 float64, front func(float64) float64) float64 {
	best := math.Inf(1)
	const steps = 2000
	for k := 0; k <= steps; k++ {
		a := float64(k) / steps
		b := front(a)
		d := math.Hypot(f1-a, f2-b)
		if d < best {
			best = d
		}
	}
	return best
}

// generationalDistance is the mean distance from the found Pareto front to the
// true front — the standard GD convergence metric (lower is better).
func generationalDistance(front []Solution, distFn func(f1, f2 float64) float64) float64 {
	if len(front) == 0 {
		return math.Inf(1)
	}
	var sum float64
	for _, s := range front {
		sum += distFn(s.Values[0], s.Values[1])
	}
	return sum / float64(len(front))
}

// spread reports the f1-range covered by the front; a converged, well-spread
// NSGA-II front should span most of [0,1].
func spread(front []Solution) float64 {
	if len(front) == 0 {
		return 0
	}
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, s := range front {
		lo = math.Min(lo, s.Values[0])
		hi = math.Max(hi, s.Values[0])
	}
	return hi - lo
}

func runZDT(t *testing.T, eval EvaluatorFunc, distFn func(f1, f2 float64) float64) Result {
	t.Helper()
	opt, err := New(Config{
		Space:          zdtSpace(t),
		Objectives:     []ObjectiveSpec{{Name: "f1"}, {Name: "f2"}}, // both minimize
		PopulationSize: 50,
		Seed:           42,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Optuna's NSGA-II config (uniform crossover + uniform-resample mutation)
	// converges on ZDT but more slowly than SBX-based NSGA-II; GD roughly halves
	// per doubling of generations. 1500 generations (pop 50 => 75k evals, ~0.4s)
	// drives both ZDT1 and ZDT2 below the 0.01 GD threshold with margin.
	res, err := opt.Optimize(context.Background(), eval, RunConfig{Generations: 1500})
	if err != nil {
		t.Fatalf("Optimize: %v", err)
	}
	return res
}

func TestZDT1Converges(t *testing.T) {
	res := runZDT(t, zdt1Eval, zdt1FrontDist)
	gd := generationalDistance(res.ParetoFront, zdt1FrontDist)
	sp := spread(res.ParetoFront)
	t.Logf("ZDT1: |front|=%d GD=%.5f spread=%.3f evals=%d",
		len(res.ParetoFront), gd, sp, res.Evaluations)
	if gd > 0.01 {
		t.Errorf("ZDT1 generational distance %.5f exceeds 0.01 (front did not converge)", gd)
	}
	if sp < 0.85 {
		t.Errorf("ZDT1 front spread %.3f too small (<0.85); poor diversity", sp)
	}
	if len(res.ParetoFront) < 20 {
		t.Errorf("ZDT1 front too small: %d", len(res.ParetoFront))
	}
}

func TestZDT2Converges(t *testing.T) {
	res := runZDT(t, zdt2Eval, zdt2FrontDist)
	gd := generationalDistance(res.ParetoFront, zdt2FrontDist)
	sp := spread(res.ParetoFront)
	t.Logf("ZDT2: |front|=%d GD=%.5f spread=%.3f evals=%d",
		len(res.ParetoFront), gd, sp, res.Evaluations)
	if gd > 0.01 {
		t.Errorf("ZDT2 generational distance %.5f exceeds 0.01 (front did not converge)", gd)
	}
	if sp < 0.85 {
		t.Errorf("ZDT2 front spread %.3f too small (<0.85); poor diversity", sp)
	}
}

// ---- Simple 2-variable problem (Schaffer N.1 style) -----------------------
//
// Minimize f1 = x^2, f2 = (x-2)^2 over x in [-5,5]. Pareto-optimal x in [0,2];
// the front is the convex trade-off curve. We assert all front solutions have x
// in [0,2] (within tolerance) and the front spans most of that interval.
func TestSchafferSimple(t *testing.T) {
	s, err := NewSearchSpace(FloatParam("x", -5, 5))
	if err != nil {
		t.Fatal(err)
	}
	opt, err := New(Config{
		Space:          s,
		Objectives:     []ObjectiveSpec{{Name: "f1"}, {Name: "f2"}},
		PopulationSize: 40,
		Seed:           7,
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := opt.Optimize(context.Background(), EvaluatorFunc(func(_ context.Context, tr Trial) ([]float64, error) {
		x := tr.Params["x"].(float64)
		return []float64{x * x, (x - 2) * (x - 2)}, nil
	}), RunConfig{Generations: 120})
	if err != nil {
		t.Fatal(err)
	}
	loX, hiX := math.Inf(1), math.Inf(-1)
	for _, sol := range res.ParetoFront {
		// Recover x from f1 = x^2 (front has x>=0).
		x := math.Sqrt(sol.Values[0])
		if x < -1e-6 || x > 2+1e-6 {
			t.Errorf("Pareto solution x=%.4f outside optimal [0,2]", x)
		}
		loX = math.Min(loX, x)
		hiX = math.Max(hiX, x)
	}
	t.Logf("Schaffer: |front|=%d x-range=[%.3f,%.3f]", len(res.ParetoFront), loX, hiX)
	if hiX-loX < 1.5 {
		t.Errorf("Schaffer front x-span %.3f too small (<1.5)", hiX-loX)
	}
}

// ---- Determinism ----------------------------------------------------------

func TestDeterministicSameSeed(t *testing.T) {
	run := func() Result {
		opt, err := New(Config{
			Space:          zdtSpace(t),
			Objectives:     []ObjectiveSpec{{Name: "f1"}, {Name: "f2"}},
			PopulationSize: 30,
			Seed:           1234,
		})
		if err != nil {
			t.Fatal(err)
		}
		res, err := opt.Optimize(context.Background(), zdt1Eval, RunConfig{Generations: 40})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	a, b := run(), run()
	if len(a.Population) != len(b.Population) {
		t.Fatalf("population size differs: %d vs %d", len(a.Population), len(b.Population))
	}
	for i := range a.Population {
		pa, pb := a.Population[i], b.Population[i]
		if pa.ID != pb.ID || pa.Rank != pb.Rank {
			t.Fatalf("individual %d differs: id %d/%d rank %d/%d", i, pa.ID, pb.ID, pa.Rank, pb.Rank)
		}
		for k := range pa.Values {
			if pa.Values[k] != pb.Values[k] {
				t.Fatalf("individual %d objective %d differs: %.17g vs %.17g", i, k, pa.Values[k], pb.Values[k])
			}
		}
		// Param maps must match bit-for-bit.
		for name, va := range pa.Params {
			if pb.Params[name] != va {
				t.Fatalf("individual %d param %q differs: %v vs %v", i, name, va, pb.Params[name])
			}
		}
	}
}

// Determinism must hold under parallel evaluation too: completion order cannot
// affect the result because the population is rebuilt in id order.
func TestDeterministicParallelMatchesSequential(t *testing.T) {
	build := func() *Optimizer {
		opt, err := New(Config{
			Space:          zdtSpace(t),
			Objectives:     []ObjectiveSpec{{Name: "f1"}, {Name: "f2"}},
			PopulationSize: 32,
			Seed:           99,
		})
		if err != nil {
			t.Fatal(err)
		}
		return opt
	}
	seqRes, err := build().Optimize(context.Background(), zdt1Eval, RunConfig{Generations: 30, Parallelism: 1})
	if err != nil {
		t.Fatal(err)
	}
	parRes, err := build().Optimize(context.Background(), zdt1Eval, RunConfig{Generations: 30, Parallelism: 8})
	if err != nil {
		t.Fatal(err)
	}
	if len(seqRes.Population) != len(parRes.Population) {
		t.Fatalf("size mismatch seq=%d par=%d", len(seqRes.Population), len(parRes.Population))
	}
	for i := range seqRes.Population {
		s, p := seqRes.Population[i], parRes.Population[i]
		if s.ID != p.ID {
			t.Fatalf("id mismatch at %d: %d vs %d", i, s.ID, p.ID)
		}
		for k := range s.Values {
			if s.Values[k] != p.Values[k] {
				t.Fatalf("value mismatch ind %d obj %d: %.17g vs %.17g", i, k, s.Values[k], p.Values[k])
			}
		}
	}
}

func TestDifferentSeedDiffers(t *testing.T) {
	mk := func(seed uint64) Result {
		opt, _ := New(Config{
			Space:          zdtSpace(t),
			Objectives:     []ObjectiveSpec{{Name: "f1"}, {Name: "f2"}},
			PopulationSize: 30,
			Seed:           seed,
		})
		r, err := opt.Optimize(context.Background(), zdt1Eval, RunConfig{Generations: 5})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	a, b := mk(1), mk(2)
	// With only 5 generations the populations should not be identical.
	same := len(a.Population) == len(b.Population)
	if same {
		for i := range a.Population {
			if a.Population[i].Values[0] != b.Population[i].Values[0] {
				same = false
				break
			}
		}
	}
	if same {
		t.Errorf("different seeds produced identical early populations (RNG not seed-dependent)")
	}
}
