package machinetest

import (
	"math/rand/v2"
	"os"
	"strconv"
	"testing"
)

// SeedEnv names a seed to replay instead of a fresh one; StepsEnv scales a model walk up.
const (
	SeedEnv  = "BERTH_MODEL_SEED"
	StepsEnv = "BERTH_MODEL_STEPS"
)

// Seed returns the seed SeedEnv names, or a fresh one, and logs it so a failing walk can be
// replayed with the same commands.
func Seed(t testing.TB) uint64 {
	t.Helper()
	seed := rand.Uint64() //nolint:gosec // a replayable test seed, never a secret
	if v := os.Getenv(SeedEnv); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			t.Fatalf("%s=%q is not a seed: %v", SeedEnv, v, err)
		}
		seed = n
	}
	t.Logf("seed %d; replay with %s=%d", seed, SeedEnv, seed)
	return seed
}

// Steps returns how many steps a walk takes: StepsEnv when set, else def.
func Steps(t testing.TB, def int) int {
	t.Helper()
	v := os.Getenv(StepsEnv)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		t.Fatalf("%s=%q is not a positive count", StepsEnv, v)
	}
	return n
}

// Rand is a generator for one stream of a walk, so separate streams of one seed (a goroutine,
// an effect's outcomes) stay replayable whatever order they run in.
func Rand(seed uint64, stream string) *rand.Rand {
	var h uint64 = 14695981039346656037
	for i := range len(stream) {
		h = (h ^ uint64(stream[i])) * 1099511628211
	}
	return rand.New(rand.NewPCG(seed, h)) //nolint:gosec // a replayable test stream, never a secret
}

// Legal lists the events of events that transition accepts in state s.
func Legal[S ~string, E any, F any](s S, events []E, transition func(S, E) (S, []F, error)) []E {
	var out []E
	for _, e := range events {
		if _, _, err := transition(s, e); err == nil {
			out = append(out, e)
		}
	}
	return out
}
