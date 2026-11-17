package native

import (
	"context"
	"errors"

	"berth/internal/core/ports"
)

// errLifecycleUnbuilt stands in until the helper's rotate and remove commands are wired (E2b
// item 4, which replaces this file).
var errLifecycleUnbuilt = errors.New("native: rotate and remove are not built yet")

func (a *Approver) RotateSecret(context.Context, ports.LifecyclePrompt, ports.Ticket) (ports.StoreOutcome, error) {
	return ports.StoreFailed, errLifecycleUnbuilt
}

func (a *Approver) RemoveSecret(context.Context, ports.LifecyclePrompt, ports.Ticket) (ports.RemoveOutcome, error) {
	return ports.RemoveFailed, errLifecycleUnbuilt
}
