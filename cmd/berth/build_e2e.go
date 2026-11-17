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

// filePinner accepts the ad hoc signed test berthd by its identifier,
// without Apple's chain; the live check of berthd still demands a valid signature and the
// hardened runtime.
func filePinner(x *execid.Identity) codetrust.Pinner {
	return func(ctx context.Context, path, identifier string) (ports.CDHash, bool, error) {
		return x.PinSigned(ctx, path, `identifier "`+identifier+`"`)
	}
}
