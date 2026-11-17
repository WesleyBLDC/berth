//go:build !berth_e2e

package main

import (
	"testing"

	"berth/internal/paths"
)

// TestReleaseKeepsTestKnobsOut: a release berth-proxy ignores BERTH_NATIVE, accepts no extra
// variable, refuses local grants and trusts no root beyond the system's.
func TestReleaseKeepsTestKnobsOut(t *testing.T) {
	t.Setenv("BERTH_NATIVE", "/tmp/not-berth-native")
	got, err := nativeHelper()
	if err != nil {
		t.Fatal(err)
	}
	want, err := paths.Beside("berth-native")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("helper %q, want %q", got, want)
	}
	if len(extraEnv) != 0 || allowLocal || roots() != nil {
		t.Fatalf("extraEnv %v allowLocal %v roots set %v", extraEnv, allowLocal, roots() != nil)
	}
}
