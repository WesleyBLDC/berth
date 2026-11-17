//go:build berth_e2e

package peertrust

import (
	"context"

	"berth/internal/codetrust"
	"berth/internal/core/ports"
)

// E2E is true only in binaries the end to end tests build with -tags berth_e2e.
const E2E = true

// Pinner accepts the ad hoc signed test binaries by identifier, without Apple's chain; the live
// check still demands a valid signature and the hardened runtime.
func Pinner(x codetrust.SignedFiles) codetrust.Pinner {
	return func(ctx context.Context, path, identifier string) (ports.CDHash, bool, error) {
		return x.PinSigned(ctx, path, `identifier "`+identifier+`"`)
	}
}
