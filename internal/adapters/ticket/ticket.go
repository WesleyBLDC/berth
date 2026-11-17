// Package ticket makes single use tickets from the operating system's random source.
package ticket

import (
	"context"
	"crypto/rand"

	"berth/internal/core/ports"
)

// Source implements ports.TicketSource with crypto/rand.
type Source struct{}

func New() *Source { return &Source{} }

var _ ports.TicketSource = (*Source)(nil)

func (*Source) NewTicket(_ context.Context) (ports.Ticket, error) {
	var t ports.Ticket
	if _, err := rand.Read(t[:]); err != nil {
		return ports.Ticket{}, err
	}
	return t, nil
}
