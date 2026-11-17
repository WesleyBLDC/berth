//go:build !berth_e2e

package main

import (
	"berth/internal/adapters/execid"
	"berth/internal/codetrust"
	"berth/internal/paths"
)

// nativeHelper is always the berth-native beside the resolved berth executable; no environment
// variable points berth run elsewhere. The helper itself checks who receives the value.
func nativeHelper() (string, error) { return paths.Beside("berth-native") }

// proxyBinary is always the berth-proxy beside the resolved berth executable.
func proxyBinary() (string, error) { return paths.Beside("berth-proxy") }

// filePinner trusts only a berthd signed by Berth's team through Apple's chain.
func filePinner(x *execid.Identity) codetrust.Pinner { return codetrust.SignedPinner(x) }
