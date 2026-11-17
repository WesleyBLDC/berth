//go:build !berth_e2e

package main

import "berth/internal/berthd"

// testPinner is nil in every real build, so berthd trusts only Berth binaries signed by
// Berth's team through Apple's chain.
func testPinner() berthd.Pinner { return nil }
