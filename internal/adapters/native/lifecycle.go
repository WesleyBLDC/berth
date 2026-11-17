package native

import (
	"context"
	"encoding/hex"

	"berth/internal/core/ports"
)

// lifecycleRequest is the stdin of rotate and remove. The ticket is an action ticket, never a
// value: berth-native redeems it with berthd before it shows any prompt.
type lifecycleRequest struct {
	Name      string    `json:"name"`
	Ticket    string    `json:"ticket"`
	Requester requester `json:"requester"`
}

func toLifecycle(p ports.LifecyclePrompt, ticket ports.Ticket) lifecycleRequest {
	return lifecycleRequest{Name: p.Name, Ticket: hex.EncodeToString(ticket[:]), Requester: toRequester(p.Requester)}
}

func (a *Approver) RotateSecret(ctx context.Context, p ports.LifecyclePrompt, ticket ports.Ticket) (ports.StoreOutcome, error) {
	out, err := a.ask(ctx, "rotate", toLifecycle(p, ticket))
	return result("rotate", out, err, ports.StoreFailed, ports.StoreStored, ports.StoreDenied)
}

func (a *Approver) RemoveSecret(ctx context.Context, p ports.LifecyclePrompt, ticket ports.Ticket) (ports.RemoveOutcome, error) {
	out, err := a.ask(ctx, "remove", toLifecycle(p, ticket))
	return result("remove", out, err, ports.RemoveFailed, ports.RemoveRemoved, ports.RemoveDenied)
}
