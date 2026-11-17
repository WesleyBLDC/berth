//go:build berth_e2e

package main

import (
	"os"
	"path/filepath"

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

// proxyBinary honours BERTH_PROXY only in berth_e2e builds, like BERTH_NATIVE.
func proxyBinary() (string, error) {
	if p := os.Getenv("BERTH_PROXY"); p != "" {
		return p, nil
	}
	return paths.Beside("berth-proxy")
}

// beforeStart swaps the granted file for BERTH_E2E_SWAP_EXE after berthd checked it and before
// berth run starts it, so the end to end tests can prove the suspended cdhash check catches the
// race; release builds have no such hook.
func beforeStart(exe string) error {
	src := os.Getenv("BERTH_E2E_SWAP_EXE")
	if src == "" {
		return nil
	}
	b, err := os.ReadFile(src) //nolint:gosec // a test fixture the end to end test names
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(exe), ".swap")
	if err := os.WriteFile(tmp, b, 0o700); err != nil { //nolint:gosec // an executable fixture
		return err
	}
	return os.Rename(tmp, exe)
}
