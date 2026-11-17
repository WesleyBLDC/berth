//go:build !berth_e2e

package main

import (
	"crypto/x509"

	"berth/internal/paths"
)

// Release builds accept only the four variables internal/proxy allows and never a local grant.
var (
	extraEnv   []string
	allowLocal = false
)

// nativeHelper is always the berth-native beside the resolved berth-proxy executable.
func nativeHelper() (string, error) { return paths.Beside("berth-native") }

// roots is nil: upstreams are verified by the system verifier alone.
func roots() *x509.CertPool { return nil }
