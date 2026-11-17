//go:build !berth_e2e

package main

import (
	"berth/internal/adapters/execid"
	"berth/internal/codetrust"
	"berth/internal/paths"
)

// nativeHelper is always the berth-native beside the resolved berth executable: no environment
// variable can point berth run at another program that would get the value on its stdout.
func nativeHelper() (string, error) { return paths.Beside("berth-native") }

// daemonPinner trusts only a berthd signed by Berth's team through Apple's chain.
func daemonPinner(x *execid.Identity) codetrust.Pinner { return codetrust.SignedPinner(x) }
