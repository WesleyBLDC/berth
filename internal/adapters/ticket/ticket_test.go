package ticket

import (
	"testing"

	"berth/internal/core/ports"
)

func TestTicketsAreRandomAndDistinct(t *testing.T) {
	s := New()
	seen := map[ports.Ticket]bool{}
	for range 100 {
		tk, err := s.NewTicket(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if tk == (ports.Ticket{}) || seen[tk] {
			t.Fatalf("ticket %x is zero or repeated", tk)
		}
		seen[tk] = true
	}
}
