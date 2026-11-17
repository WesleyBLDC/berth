//go:build !berth_e2e

package main

import "berth/internal/paths"

// nativeHelper is always the berth-native beside the resolved berth executable; no environment
// variable points berth run elsewhere. The helper itself checks who receives the value.
func nativeHelper() (string, error) { return paths.Beside("berth-native") }

// proxyBinary is always the berth-proxy beside the resolved berth executable.
func proxyBinary() (string, error) { return paths.Beside("berth-proxy") }

// beforeStart is nil: nothing runs between berthd's check and the suspended start.
var beforeStart func(exe string) error
