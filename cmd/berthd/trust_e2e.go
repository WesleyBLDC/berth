//go:build berth_e2e

package main

import (
	"context"
	"path/filepath"

	"berth/internal/berthd"
	"berth/internal/core/ports"
	"berth/internal/paths"
)

// testTrust exists only in binaries the end to end tests build with -tags berth_e2e: it trusts
// the unsigned fake helper they put beside berthd as berth-native. No environment variable or
// flag reaches it, and release builds do not compile this file.
func testTrust() berthd.TrustHelper {
	helper, err := paths.Beside("berth-native")
	if err != nil {
		return nil
	}
	return func(_ context.Context, exe ports.Executable) (bool, error) {
		resolved, err := filepath.EvalSymlinks(helper)
		return err == nil && exe.Path == resolved, nil
	}
}
