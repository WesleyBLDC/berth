//go:build berth_e2e

package main

import (
	"context"

	"berth/internal/adapters/execid"
	"berth/internal/codetrust"
	"berth/internal/core/ports"
)

// testPinner exists only in binaries the end to end tests build with -tags berth_e2e. It pins
// the berth and berth-native beside berthd by identifier and cdhash without Apple's chain, so
// the tests can sign them ad hoc (with the hardened runtime, which the live check still
// demands). No environment variable or flag reaches it, and release builds lack this file.
func testPinner() codetrust.Pinner {
	x := execid.New(execid.DefaultCodesign)
	return func(ctx context.Context, path, identifier string) (ports.CDHash, bool, error) {
		return x.PinSigned(ctx, path, `identifier "`+identifier+`"`)
	}
}
