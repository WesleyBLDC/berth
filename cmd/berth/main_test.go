package main

import (
	"testing"

	"berth/internal/paths"
)

// TestReleaseIgnoresBERTHNATIVE: an agent must not be able to point berth run at a program
// of its own that would receive the value on its stdout.
func TestReleaseIgnoresBERTHNATIVE(t *testing.T) {
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
}
