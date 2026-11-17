//go:build !berth_e2e

package main

import "berth/internal/berthd"

// testTrust is nil in every real build, so berthd trusts only the signed berth-native.
func testTrust() berthd.TrustHelper { return nil }
