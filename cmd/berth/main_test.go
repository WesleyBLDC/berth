package main

import (
	"testing"

	"berth/internal/paths"
)

// TestReleaseIgnoresBERTHNATIVE: an agent must not be able to point berth run at a program
// of its own. The helper's receiver check would refuse it anyway; this keeps the override a
// test only knob.
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

func TestReleaseIgnoresBERTHPROXY(t *testing.T) {
	t.Setenv("BERTH_PROXY", "/tmp/not-berth-proxy")
	got, err := proxyBinary()
	if err != nil {
		t.Fatal(err)
	}
	want, err := paths.Beside("berth-proxy")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("proxy %q, want %q", got, want)
	}
}
