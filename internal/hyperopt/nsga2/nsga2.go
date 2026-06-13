package nsga2

// nsga2.go is the self-written, deterministic, seeded NSGA-II multi-objective
// optimizer (docs/spec/hyperopt-metrics.md §6.4, locked decision 1). It
// reproduces the ALGORITHMIC CONFIGURATION of Optuna 4.8.0's NSGAIISampler — not
// its NumPy MT19937 byte stream (Q1: semantic equivalence, validated against
// ZDT test problems, not Optuna's trial sequence):
//
//   - population_size = 50, generation-based.
//   - Generation 0: each parameter sampled independently, uniformly in range.
//   - Elite survival: fast-non-dominated-sort, fill fronts by rank; the boundary
//     front is truncated by crowding distance (standard NSGA-II, mu+lambda
//     elitist — parents and children compete together).
//   - Child generation: with crossover_prob = 0.9, UNIFORM crossover of two
//     parents (per-gene swap with swapping_prob = 0.5), retried until the child
//     lies in-bounds; otherwise clone one uniformly-chosen parent. Parent
//     selection = binary tournament on Pareto dominance only (two uniform
//     candidates, the dominator wins; no crowding tiebreak), the second parent
//     drawn from the population excluding the first.
//   - Mutation: per-gene with prob = 1/max(1, n_params), the gene is DROPPED and
//     re-sampled independently (uniform in range).
//   - FAILed evaluations do not join the population.
//
// Determinism: ONE seeded *rng threads the entire run. The ask/tell ordering is
// fixed (children asked in creation order; gen-0 individuals asked in index
// order). Aggregation over results is order-independent because the population
// is rebuilt deterministically each generation and individuals carry stable ids.
//
// The optimizer is pure with respect to its Evaluator: inject any
// params->objective-vector function (the orchestrator injects the backtest-based
// objective; tests inject synthetic ZDT objectives). Crossover/swapping/mutation
// probabilities and population size are configurable but default to the Optuna
// values above.

import (
	"errors"
	"fmt"
)

// Default hyperparameters (Optuna 4.8.0 NSGAIISampler defaults; spec §12).
const (
	DefaultPopulationSize = 50
	DefaultCrossoverProb  = 0.9
	DefaultSwappingProb   = 0.5
	DefaultSeed           = 42
)

// Config configures one optimizer run.
type Config struct {
	// Space is the parameter search space (required).
	Space *SearchSpace
	// Objectives declares each objective's name and direction (required, >=1).
	Objectives []ObjectiveSpec
	// PopulationSize is the NSGA-II generation size (default 50).
	PopulationSize int
	// CrossoverProb is the probability of crossover vs parent-clone (default 0.9).
	CrossoverProb float64
	// SwappingProb is the per-gene swap probability in uniform crossover
	// (default 0.5).
	SwappingProb float64
	// MutationProb is the per-gene drop-and-resample probability. When left at
	// its zero value (or set negative), it defaults to 1/max(1, n_params),
	// exactly as Optuna's NSGAIISampler does when mutation_prob is None. Set a
	// positive value to override.
	MutationProb float64
	// Seed seeds the single PRNG (default 42).
	Seed uint64
}

// withDefaults fills unset fields and validates required ones.
func (c Config) withDefaults() (Config, error) {
	if c.Space == nil || c.Space.Len() == 0 {
		return c, errors.New("nsga2: Config.Space is required and must be non-empty")
	}
	if len(c.Objectives) == 0 {
		return c, errors.New("nsga2: Config.Objectives must have >=1 entry")
	}
	if c.PopulationSize == 0 {
		c.PopulationSize = DefaultPopulationSize
	}
	if c.PopulationSize < 2 {
		return c, fmt.Errorf("nsga2: PopulationSize must be >=2 (got %d)", c.PopulationSize)
	}
	if c.CrossoverProb == 0 {
		c.CrossoverProb = DefaultCrossoverProb
	}
	if c.CrossoverProb < 0 || c.CrossoverProb > 1 {
		return c, fmt.Errorf("nsga2: CrossoverProb must be in [0,1] (got %g)", c.CrossoverProb)
	}
	if c.SwappingProb == 0 {
		c.SwappingProb = DefaultSwappingProb
	}
	if c.SwappingProb < 0 || c.SwappingProb > 1 {
		return c, fmt.Errorf("nsga2: SwappingProb must be in [0,1] (got %g)", c.SwappingProb)
	}
	if c.MutationProb <= 0 {
		c.MutationProb = 1.0 / float64(max(1, c.Space.Len()))
	}
	if c.MutationProb > 1 {
		return c, fmt.Errorf("nsga2: MutationProb must be <=1 (got %g)", c.MutationProb)
	}
	if c.Seed == 0 {
		c.Seed = DefaultSeed
	}
	return c, nil
}

// Trial is one asked candidate awaiting evaluation. The caller evaluates Params
// (possibly in parallel across isolated engine instances over a shared
// read-only dataset) and reports the objective vector back via Optimizer.Tell.
type Trial struct {
	// ID is the stable identifier of the underlying individual.
	ID int
	// Generation is the generation index this trial belongs to.
	Generation int
	// Params is the decoded parameter map to evaluate.
	Params Params

	ind *individual
}

// Optimizer drives the NSGA-II loop via an explicit ask/tell protocol. It is the
// low-level interface the study orchestrator builds on (it owns trial
// numbering, artifact writing, and parallel dispatch). For self-contained use,
// see Optimize.
//
// Concurrency: Ask/Tell are NOT safe for concurrent use among themselves; the
// orchestrator calls them from a single coordinator goroutine while worker
// goroutines/processes only run evaluations. This matches Optuna's
// single-coordinator ask/tell model and keeps the PRNG stream deterministic.
type Optimizer struct {
	cfg Config
	r   *rng

	nextID  int
	gen     int
	parents []*individual // current elite population (empty before gen 0 fills)

	// pending holds trials asked in the current generation but not yet told.
	pending map[int]*individual
	// asked counts how many trials have been asked in the current generation.
	asked int
	// told collects evaluated individuals for the current generation.
	told []*individual
}

// New constructs an Optimizer from cfg, applying Optuna defaults for unset
// fields and validating the rest.
func New(cfg Config) (*Optimizer, error) {
	c, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	return &Optimizer{
		cfg:     c,
		r:       newRNG(c.Seed),
		pending: map[int]*individual{},
	}, nil
}

// Config returns the effective (defaulted) configuration.
func (o *Optimizer) Config() Config { return o.cfg }

// newIndividual mints an individual with a fresh stable id.
func (o *Optimizer) newIndividual(genome Genome) *individual {
	id := o.nextID
	o.nextID++
	return &individual{
		id:       id,
		gen:      o.gen,
		genome:   genome,
		params:   o.cfg.Space.decode(genome),
		feasible: true,
	}
}

// Ask returns the next trial to evaluate, or ok=false when the current
// generation has been fully dispatched (call Tell for every outstanding trial,
// then the next Ask begins the following generation).
//
// Generation 0 yields PopulationSize independently-uniform individuals. Each
// later generation yields PopulationSize children produced from the elite
// parents via tournament + uniform crossover (or clone) + drop/resample
// mutation, identical to Optuna's child-generation strategy.
func (o *Optimizer) Ask() (Trial, bool) {
	if o.asked >= o.cfg.PopulationSize {
		return Trial{}, false
	}
	var ind *individual
	if o.gen == 0 {
		ind = o.newIndividual(o.cfg.Space.sample(o.r))
	} else {
		ind = o.newIndividual(o.generateChild())
	}
	o.asked++
	o.pending[ind.id] = ind
	return Trial{ID: ind.id, Generation: o.gen, Params: clonePar(ind.params), ind: ind}, true
}

// Tell reports the objective vector for a previously asked trial. A nil err
// records a successful evaluation; a non-nil err marks the trial FAILed, which
// excludes it from the population (it never influences survival or future
// children) but still counts toward generation completion so Ask/Tell stay
// balanced.
//
// values must have len == len(Objectives) on success.
func (o *Optimizer) Tell(t Trial, values []float64, err error) error {
	ind, ok := o.pending[t.ID]
	if !ok {
		return fmt.Errorf("nsga2: Tell for unknown or already-told trial id %d", t.ID)
	}
	delete(o.pending, t.ID)

	if err != nil {
		ind.evalOK = false
		ind.evalErr = err
	} else {
		if len(values) != len(o.cfg.Objectives) {
			return fmt.Errorf("nsga2: Tell expected %d objective values, got %d",
				len(o.cfg.Objectives), len(values))
		}
		ind.values = append([]float64(nil), values...)
		ind.loss = toLoss(ind.values, o.cfg.Objectives)
		ind.evalOK = true
	}
	o.told = append(o.told, ind)

	// When the whole generation is in, advance: select the next elite
	// population (mu+lambda over parents+children), reset per-generation state.
	if len(o.told) == o.cfg.PopulationSize {
		o.advanceGeneration()
	}
	return nil
}

// advanceGeneration runs elitist survival to form the next generation's parent
// population, then resets the per-generation counters. Survivors are drawn from
// the union of the previous parents and this generation's successful children
// (mu+lambda). FAILed children are dropped. Individuals are added to the
// combined pool in a deterministic id order so the sort is reproducible.
func (o *Optimizer) advanceGeneration() {
	combined := make([]*individual, 0, len(o.parents)+len(o.told))
	combined = append(combined, o.parents...)
	for _, ind := range o.told {
		if ind.evalOK {
			combined = append(combined, ind)
		}
	}
	o.parents = selectElite(combined, o.cfg.PopulationSize)

	o.gen++
	o.asked = 0
	o.told = nil
	// pending is already empty (every asked trial was told).
}

// selectElite performs NSGA-II elite selection: fast-non-dominated-sort, fill
// fronts by rank, truncate the boundary front by crowding distance (largest
// distance kept; deterministic id tiebreak). Matches
// NSGAIIElitePopulationSelectionStrategy.
func selectElite(pop []*individual, size int) []*individual {
	if len(pop) <= size {
		// Still assign ranks for consistency, but everyone survives.
		fastNonDominatedSort(pop)
		out := make([]*individual, len(pop))
		copy(out, pop)
		return out
	}
	fronts := fastNonDominatedSort(pop)
	elite := make([]*individual, 0, size)
	for _, front := range fronts {
		if len(elite)+len(front) <= size {
			elite = append(elite, front...)
			if len(elite) == size {
				break
			}
			continue
		}
		// Boundary front: keep the `need` most isolated individuals.
		need := size - len(elite)
		dist := crowdingDistances(front)
		sortByCrowdingDesc(front, dist)
		elite = append(elite, front[:need]...)
		break
	}
	return elite
}

// generateChild produces one child genome from the elite parents, mirroring
// NSGAIIChildGenerationStrategy.__call__ draw-for-draw in ordering:
//
//	if rand() < crossover_prob: crossover(two tournament-selected parents)
//	else:                       clone a uniformly chosen parent
//	then per-gene: if rand() >= mutation_prob keep, else drop+resample.
func (o *Optimizer) generateChild() Genome {
	space := o.cfg.Space
	n := space.Len()
	var child Genome

	if o.r.Float64() < o.cfg.CrossoverProb {
		child = o.crossover()
	} else {
		// Clone a uniformly chosen parent.
		p := o.parents[o.r.Intn(len(o.parents))]
		child = p.genome.Clone()
	}

	// Mutation: per-gene drop-and-resample. A "dropped" gene is one Optuna omits
	// from the returned params dict and Optuna's RandomSampler then fills by
	// independent uniform sampling; we resample it in place to the same effect.
	for i := 0; i < n; i++ {
		if o.r.Float64() >= o.cfg.MutationProb {
			continue // keep inherited gene
		}
		child[i] = space.sampleGene(i, o.r)
	}
	return child
}

// crossover performs binary-tournament parent selection followed by uniform
// crossover, retrying until the child is in-bounds (Optuna's perform_crossover
// while-loop). Because every gene comes from one of two in-bounds parents, the
// produced child is always in-bounds, but the retry loop and its draw pattern
// are preserved for fidelity.
func (o *Optimizer) crossover() Genome {
	space := o.cfg.Space
	n := space.Len()
	for {
		p0, p1 := o.selectParents()
		child := make(Genome, n)
		for i := 0; i < n; i++ {
			// swap mask: keep parent0's gene when rand() >= swapping_prob,
			// else take parent1's (matches the uniform-crossover mask semantics
			// where mask==1 selects parents[masks=1] i.e. the second row).
			if o.r.Float64() >= o.cfg.SwappingProb {
				child[i] = p0.genome[i]
			} else {
				child[i] = p1.genome[i]
			}
		}
		if space.contains(child) {
			return child
		}
	}
}

// selectParents draws two parents by binary tournament on Pareto dominance. The
// first parent is a tournament over the whole population; the second is a
// tournament over the population EXCLUDING the first parent (Optuna draws the
// second parent from `population minus already-chosen`).
func (o *Optimizer) selectParents() (*individual, *individual) {
	p0 := o.tournament(o.parents)
	rest := make([]*individual, 0, len(o.parents)-1)
	for _, ind := range o.parents {
		if ind.id != p0.id {
			rest = append(rest, ind)
		}
	}
	if len(rest) == 0 {
		// Degenerate: population of size 1 after exclusion. Reuse p0.
		return p0, p0
	}
	p1 := o.tournament(rest)
	return p0, p1
}

// tournament runs Optuna's binary tournament: pick two candidates uniformly at
// random (with replacement, exactly as rng.choice twice), return the dominator;
// on a non-dominating pair return the SECOND candidate (Optuna's `else` branch
// returns candidate1).
func (o *Optimizer) tournament(pop []*individual) *individual {
	c0 := pop[o.r.Intn(len(pop))]
	c1 := pop[o.r.Intn(len(pop))]
	if dominatesLoss(c0.loss, c1.loss) {
		return c0
	}
	return c1
}

// Generation returns the current generation index (0-based). Increments only
// after a full generation has been told.
func (o *Optimizer) Generation() int { return o.gen }

// Population returns a snapshot of the current elite parent population's decoded
// params and objective values. Empty before generation 0 completes.
func (o *Optimizer) Population() []Trial {
	out := make([]Trial, 0, len(o.parents))
	for _, ind := range o.parents {
		out = append(out, Trial{
			ID:         ind.id,
			Generation: ind.gen,
			Params:     clonePar(ind.params),
			ind:        ind,
		})
	}
	return out
}

func clonePar(p Params) Params {
	out := make(Params, len(p))
	for k, v := range p {
		out[k] = v
	}
	return out
}
