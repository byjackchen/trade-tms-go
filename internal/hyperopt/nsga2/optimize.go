package nsga2

// optimize.go provides the high-level Optimize driver and the Result/Solution
// types. It wraps the ask/tell loop with a pluggable Evaluator and an optional
// parallel-evaluation fan-out over isolated evaluator instances sharing a
// read-only dataset (locked decision 5: aggregation deterministic regardless of
// trial completion order — the optimizer rebuilds the population in id order
// each generation, so completion order cannot affect the result).

import (
	"context"
	"errors"
	"sort"
	"sync"
)

// Evaluator maps a parameter set to an objective vector (in user direction,
// matching Config.Objectives order). It is the injection point for the
// backtest-based objective; tests inject synthetic objectives (ZDT, etc.).
//
// Evaluate must be safe for concurrent use when Optimize runs with Parallelism
// > 1 (the orchestrator satisfies this by giving each goroutine an isolated
// backtest engine over a shared read-only bar dataset). It must honor ctx
// cancellation and return promptly with ctx.Err() when cancelled.
type Evaluator interface {
	Evaluate(ctx context.Context, t Trial) (values []float64, err error)
}

// EvaluatorFunc adapts a function to the Evaluator interface.
type EvaluatorFunc func(ctx context.Context, t Trial) ([]float64, error)

// Evaluate calls the underlying function.
func (f EvaluatorFunc) Evaluate(ctx context.Context, t Trial) ([]float64, error) {
	return f(ctx, t)
}

// RunConfig parameterizes a full Optimize run.
type RunConfig struct {
	// Generations is the number of generations to run (>=1). Total evaluations
	// = Generations * PopulationSize.
	Generations int
	// Parallelism is the max concurrent Evaluate calls within a generation
	// (default 1 => sequential). The whole generation is asked up front, then
	// evaluated by a bounded worker pool; results are told back in id order so
	// the outcome is independent of completion order.
	Parallelism int
}

// Solution is one evaluated individual in the final result.
type Solution struct {
	ID     int
	Params Params
	Values []float64 // objective values in user direction
	Rank   int       // non-domination rank in the final population (0 = Pareto)
}

// Result is the outcome of a full Optimize run.
type Result struct {
	// Population is the final elite population, sorted by (rank asc, id asc).
	Population []Solution
	// ParetoFront is the rank-0 subset of Population (non-dominated solutions),
	// sorted by id asc.
	ParetoFront []Solution
	// Evaluations is the total number of successful evaluations performed.
	Evaluations int
	// Failures is the number of evaluations that returned an error (excluding
	// context cancellation, which aborts the run).
	Failures int
	// Generations actually completed before return.
	Generations int
}

// Optimize runs the NSGA-II loop for run.Generations generations using eval to
// score candidates, and returns the final population and Pareto front. It is the
// deterministic, self-contained entry point used by the correctness tests and a
// convenient default for the orchestrator.
//
// ctx cancellation aborts the run promptly between/within generations; Optimize
// returns ctx.Err() and no Result. A non-cancellation evaluation error does NOT
// abort the run — the trial is recorded as FAILed and excluded from the
// population (spec: FAILed trials do not join the population), with the count
// surfaced in Result.Failures.
func (o *Optimizer) Optimize(ctx context.Context, eval Evaluator, run RunConfig) (Result, error) {
	if run.Generations < 1 {
		return Result{}, errors.New("nsga2: RunConfig.Generations must be >=1")
	}
	if run.Parallelism < 1 {
		run.Parallelism = 1
	}
	var evals, failures int

	for g := 0; g < run.Generations; g++ {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		// Ask the whole generation.
		trials := make([]Trial, 0, o.cfg.PopulationSize)
		for {
			t, ok := o.Ask()
			if !ok {
				break
			}
			trials = append(trials, t)
		}

		// Evaluate (sequential or bounded-parallel). Results are buffered, then
		// told back in ascending id order for determinism.
		results := make([]evalResult, len(trials))
		if err := evaluateGeneration(ctx, eval, trials, results, run.Parallelism); err != nil {
			return Result{}, err // context cancellation
		}

		// Tell in ask order (trials are already in id-ascending ask order).
		for i, t := range trials {
			res := results[i]
			if err := o.Tell(t, res.values, res.err); err != nil {
				return Result{}, err
			}
			if res.err != nil {
				failures++
			} else {
				evals++
			}
		}
	}

	return o.buildResult(evals, failures, run.Generations), nil
}

// PopulationSolutions snapshots the current elite parent population as Solutions
// with non-domination ranks freshly computed (rank 0 = Pareto front). Sorted by
// (rank asc, id asc). Empty before generation 0 completes. The orchestrator uses
// this to pick best_params after a study completes without re-running the loop.
func (o *Optimizer) PopulationSolutions() []Solution {
	return o.buildResult(0, 0, o.gen).Population
}

type evalResult struct {
	values []float64
	err    error
}

// evaluateGeneration scores every trial, honoring ctx and a concurrency cap.
// A context cancellation surfaces as the returned error and aborts; evaluator
// errors are stored per-trial (the run continues, the trial FAILs).
func evaluateGeneration(ctx context.Context, eval Evaluator, trials []Trial, out []evalResult, parallelism int) error {
	if parallelism <= 1 {
		for i, t := range trials {
			if err := ctx.Err(); err != nil {
				return err
			}
			v, err := eval.Evaluate(ctx, t)
			if err != nil && ctx.Err() != nil {
				return ctx.Err()
			}
			out[i] = evalResult{values: v, err: err}
		}
		return nil
	}

	// Bounded worker pool. Each worker pulls indices; ctx cancellation drains.
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		canceled bool
		idx      = make(chan int)
	)
	worker := func() {
		defer wg.Done()
		for i := range idx {
			if ctx.Err() != nil {
				mu.Lock()
				canceled = true
				mu.Unlock()
				continue // drain remaining indices without evaluating
			}
			v, err := eval.Evaluate(ctx, trials[i])
			if err != nil && ctx.Err() != nil {
				mu.Lock()
				canceled = true
				mu.Unlock()
				out[i] = evalResult{err: ctx.Err()}
				continue
			}
			out[i] = evalResult{values: v, err: err}
		}
	}
	wg.Add(parallelism)
	for w := 0; w < parallelism; w++ {
		go worker()
	}
	for i := range trials {
		if ctx.Err() != nil {
			mu.Lock()
			canceled = true
			mu.Unlock()
			break
		}
		idx <- i
	}
	close(idx)
	wg.Wait()
	if canceled || ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

// buildResult snapshots the final population and Pareto front. The final
// population is re-ranked (fast-non-dominated-sort) so ranks reflect the latest
// state, then sorted (rank asc, id asc); the Pareto front is the rank-0 subset.
func (o *Optimizer) buildResult(evals, failures, gens int) Result {
	pop := make([]*individual, len(o.parents))
	copy(pop, o.parents)
	fastNonDominatedSort(pop)

	sort.SliceStable(pop, func(a, b int) bool {
		if pop[a].rank != pop[b].rank {
			return pop[a].rank < pop[b].rank
		}
		return pop[a].id < pop[b].id
	})

	sols := make([]Solution, len(pop))
	var front []Solution
	for i, ind := range pop {
		s := Solution{
			ID:     ind.id,
			Params: clonePar(ind.params),
			Values: append([]float64(nil), ind.values...),
			Rank:   ind.rank,
		}
		sols[i] = s
		if ind.rank == 0 {
			front = append(front, s)
		}
	}
	sort.SliceStable(front, func(a, b int) bool { return front[a].ID < front[b].ID })

	return Result{
		Population:  sols,
		ParetoFront: front,
		Evaluations: evals,
		Failures:    failures,
		Generations: gens,
	}
}
