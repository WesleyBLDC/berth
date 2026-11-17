//go:build berth_e2e

package main

import (
	"os"

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
