//go:build !berth_e2e

package peertrust

import (
	"context"
	"testing"

	"berth/internal/codetrust"
	"berth/internal/core/ports"
)

type requirements struct{ got string }

func (r *requirements) PinSigned(_ context.Context, _, requirement string) (ports.CDHash, bool, error) {
	r.got = requirement
	return ports.CDHash{}, false, nil
}

// TestReleaseTrustsOnlyAppleChain: a release build pins by Apple's chain and Berth's team, and
// accepts no local proxy targets.
func TestReleaseTrustsOnlyAppleChain(t *testing.T) {
	r := &requirements{}
	if _, _, err := Pinner(r)(t.Context(), "/bin/berth", codetrust.CLISigningID); err != nil {
		t.Fatal(err)
	}
	if want := codetrust.Requirement(codetrust.CLISigningID); r.got != want {
		t.Fatalf("requirement %q, want %q", r.got, want)
	}
	if E2E {
		t.Fatal("a release build says it is an end to end build")
	}
}
