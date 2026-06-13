package nsga2

// rng.go wraps a single seeded math/rand/v2 PCG generator. The whole optimizer
// threads ONE *rng instance; there is NO use of the global math/rand or
// math/rand/v2 top-level functions anywhere in the package. Same seed + same
// evaluation results => identical PRNG draw sequence => identical population
// trajectory (locked decision: deterministic, seeded).
//
// PCG is chosen (over ChaCha8) because it is the fast, fixed, well-defined
// generator in math/rand/v2 with a small explicit 128-bit state we can derive
// reproducibly from a single user seed, and it marshals/unmarshals for resume.

import (
	"math/rand/v2"
)

// rng is a deterministic source of randomness for one optimizer run.
type rng struct {
	src *rand.PCG
	r   *rand.Rand
}

// newRNG builds a generator seeded from a single uint64. The two PCG seed words
// are derived from the seed via the SplitMix64 finalizer so that nearby seeds
// (e.g. 41, 42, 43) produce well-separated streams. This derivation is fixed:
// a given seed always yields the same stream.
func newRNG(seed uint64) *rng {
	s1 := splitmix64(seed)
	s2 := splitmix64(seed ^ 0x9E3779B97F4A7C15)
	src := rand.NewPCG(s1, s2)
	return &rng{src: src, r: rand.New(src)}
}

// splitmix64 is the standard SplitMix64 mixing function, used purely to expand a
// single seed into two decorrelated PCG seed words.
func splitmix64(x uint64) uint64 {
	x += 0x9E3779B97F4A7C15
	x = (x ^ (x >> 30)) * 0xBF58476D1CE4E5B9
	x = (x ^ (x >> 27)) * 0x94D049BB133111EB
	return x ^ (x >> 31)
}

// Float64 returns a uniform float64 in [0,1).
func (g *rng) Float64() float64 { return g.r.Float64() }

// Int64n returns a uniform int64 in [0,n). Requires n>0.
func (g *rng) Int64n(n int64) int64 { return g.r.Int64N(n) }

// Intn returns a uniform int in [0,n). Requires n>0.
func (g *rng) Intn(n int) int { return g.r.IntN(n) }

// snapshot serializes the generator state so a study can resume with an
// identical continuation of the PRNG stream.
func (g *rng) snapshot() ([]byte, error) { return g.src.MarshalBinary() }

// restore reloads generator state produced by snapshot.
func (g *rng) restore(b []byte) error { return g.src.UnmarshalBinary(b) }
