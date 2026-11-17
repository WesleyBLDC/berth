//go:build !berth_e2e

package peertrust

import "berth/internal/codetrust"

// E2E is false in every real build.
const E2E = false

// Pinner trusts only Berth binaries signed by Berth's team through Apple's chain.
func Pinner(x codetrust.SignedFiles) codetrust.Pinner { return codetrust.SignedPinner(x) }
