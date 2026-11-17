//go:build berth_e2e

package main

import (
	"context"
	"os"

	"berth/internal/adapters/execid"
	"berth/internal/codetrust"
	"berth/internal/core/ports"
	"berth/internal/paths"
)

// nativeHelper honours BERTH_NATIVE only in binaries the end to end tests build with
// -tags berth_e2e; release builds lack this file.
func nativeHelper() (string, error) {
	if h := os.Getenv("BERTH_NATIVE"); h != "" {
		return h, nil
	}
	return paths.Beside("berth-native")
}

// daemonPinner accepts the ad hoc signed test berthd by cdhash, without Apple's chain; the
// live check still demands a valid signature and the hardened runtime.
func daemonPinner(x *execid.Identity) codetrust.Pinner {
	return func(ctx context.Context, path, _ string) (ports.CDHash, bool, error) {
		h, err := x.PinAny(ctx, path)
		return h, err == nil, err
	}
}
