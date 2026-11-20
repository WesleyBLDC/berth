package machinetest

import (
	"math/rand/v2"
	"os"
	"strconv"
	"testing"
)

// SeedEnv names one seed to replay; RandomEnv, when set, walks a fresh random seed instead of the
// fixed ones (make model-long); StepsEnv scales a walk up.
const (
	SeedEnv   = "BERTH_MODEL_SEED"
	RandomEnv = "BERTH_MODEL_RANDOM"
	StepsEnv  = "BERTH_MODEL_STEPS"
)

// Seeds returns the seeds to walk: the one SeedEnv names, a fresh one when RandomEnv is set, or
// else fixed, the checked in seeds make check runs every time.
func Seeds(t testing.TB, fixed ...uint64) []uint64 {
	t.Helper()
	if v := os.Getenv(SeedEnv); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			t.Fatalf("%s=%q is not a seed: %v", SeedEnv, v, err)
		}
		return []uint64{n}
	}
	if os.Getenv(RandomEnv) != "" {
		return []uint64{rand.Uint64()} //nolint:gosec // a replayable test seed, never a secret
	}
	return fixed
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
