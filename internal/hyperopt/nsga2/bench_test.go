package nsga2

// bench_test.go is part of the permanent benchmark suite (`make bench`). It
// measures (1) the NSGA-II optimizer's pure trials/sec (the optimizer's own
// ask/tell + non-dominated-sort + crowding overhead, with a near-zero
// evaluator) and (2) parallel scaling under a fixed, CPU-bound per-trial cost
// that approximates a backtest evaluation. Deliverable for docs/benchmarks.md
// (b): trials/sec + parallel scaling.

import (
	"context"
	"math"
	"testing"
)

// benchSpace builds a representative 6-parameter search space (the
// dimensionality of a real strategy hyperopt) over [0,1].
func benchSpace(b *testing.B) *SearchSpace {
	b.Helper()
	sp, err := NewSearchSpace(
		FloatParam("x0", 0, 1), FloatParam("x1", 0, 1), FloatParam("x2", 0, 1),
		FloatParam("x3", 0, 1), FloatParam("x4", 0, 1), FloatParam("x5", 0, 1),
	)
	if err != nil {
		b.Fatalf("NewSearchSpace: %v", err)
	}
	return sp
}

var benchObjectives = []ObjectiveSpec{
	{Name: "f1", Maximize: false},
	{Name: "f2", Maximize: false},
}

// zdt1 is the classic two-objective ZDT1 test function over [0,1]^n: cheap, so
// BenchmarkOptimizerTrialsPerSec isolates the optimizer's own overhead.
func zdt1(p Params) []float64 {
	x0 := p["x0"].(float64)
	var sum float64
	n := 0
	for k, v := range p {
		if k == "x0" {
			continue
		}
		sum += v.(float64)
		n++
	}
	g := 1.0 + 9.0*sum/float64(n)
	f2 := g * (1.0 - math.Sqrt(x0/g))
	return []float64{x0, f2}
}

// busyEvaluate burns a fixed amount of CPU per trial to model a backtest's
// per-trial cost, so parallel scaling is observable. The iteration count is
// chosen so a single trial costs on the order of tens of microseconds — large
// enough to dominate scheduling overhead, small enough to keep the benchmark
// fast.
func busyEvaluate(iters int) []float64 {
	acc := 0.0
	for i := 0; i < iters; i++ {
		acc += math.Sqrt(float64(i)*1.0001 + 1.0)
	}
	// Fold the work into a valid in-range objective vector.
	r := math.Mod(math.Abs(acc), 1.0)
	return []float64{r, 1.0 - r}
}

// BenchmarkOptimizerTrialsPerSec measures the optimizer's own throughput with a
// near-free evaluator (ZDT1). Reports trials/sec = evaluations / wall-seconds.
// This is the ceiling on optimizer overhead independent of the backtest cost.
func BenchmarkOptimizerTrialsPerSec(b *testing.B) {
	sp := benchSpace(b)
	const gens = 20 // 20 generations * 50 pop = 1000 trials per op
	eval := EvaluatorFunc(func(_ context.Context, t Trial) ([]float64, error) {
		return zdt1(t.Params), nil
	})
	b.ReportAllocs()
	b.ResetTimer()
	var totalEvals int
	for i := 0; i < b.N; i++ {
		opt, err := New(Config{Space: sp, Objectives: benchObjectives, Seed: 42})
		if err != nil {
			b.Fatalf("New: %v", err)
		}
		res, err := opt.Optimize(context.Background(), eval, RunConfig{Generations: gens, Parallelism: 1})
		if err != nil {
			b.Fatalf("Optimize: %v", err)
		}
		totalEvals = res.Evaluations
	}
	b.StopTimer()
	reportTrialsPerSec(b, totalEvals)
}

// benchScaling runs a fixed-cost-per-trial study at the given parallelism and
// reports trials/sec, so docs/benchmarks.md can tabulate the scaling curve.
func benchScaling(b *testing.B, parallelism, busyIters int) {
	sp := benchSpace(b)
	const gens = 8 // 8 * 50 = 400 trials per op
	eval := EvaluatorFunc(func(_ context.Context, _ Trial) ([]float64, error) {
		return busyEvaluate(busyIters), nil
	})
	b.ResetTimer()
	var totalEvals int
	for i := 0; i < b.N; i++ {
		opt, err := New(Config{Space: sp, Objectives: benchObjectives, Seed: 42})
		if err != nil {
			b.Fatalf("New: %v", err)
		}
		res, err := opt.Optimize(context.Background(), eval, RunConfig{Generations: gens, Parallelism: parallelism})
		if err != nil {
			b.Fatalf("Optimize: %v", err)
		}
		totalEvals = res.Evaluations
	}
	b.StopTimer()
	reportTrialsPerSec(b, totalEvals)
}

const benchBusyIters = 200_000 // ~tens of microseconds per trial on M-class CPU

func BenchmarkParallelScaling_P1(b *testing.B)  { benchScaling(b, 1, benchBusyIters) }
func BenchmarkParallelScaling_P2(b *testing.B)  { benchScaling(b, 2, benchBusyIters) }
func BenchmarkParallelScaling_P4(b *testing.B)  { benchScaling(b, 4, benchBusyIters) }
func BenchmarkParallelScaling_P8(b *testing.B)  { benchScaling(b, 8, benchBusyIters) }
func BenchmarkParallelScaling_P16(b *testing.B) { benchScaling(b, 16, benchBusyIters) }

func reportTrialsPerSec(b *testing.B, evals int) {
	b.Helper()
	secPerOp := float64(b.Elapsed().Nanoseconds()) / float64(b.N) / 1e9
	if secPerOp > 0 {
		b.ReportMetric(float64(evals)/secPerOp, "trials/sec")
	}
}
