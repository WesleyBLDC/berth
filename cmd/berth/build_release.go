//go:build !berth_e2e

package main

import (
	"berth/internal/adapters/execid"
	"berth/internal/codetrust"
	"berth/internal/paths"
)

// nativeHelper is always the berth-native beside the resolved berth executable: no environment
// variable can point berth run elsewhere. berth runs a checked private copy of it, never the
// file itself.
func nativeHelper() (string, error) { return paths.Beside("berth-native") }

// filePinner trusts only a berthd or berth-native signed by Berth's team through Apple's chain.
func filePinner(x *execid.Identity) codetrust.Pinner { return codetrust.SignedPinner(x) }
