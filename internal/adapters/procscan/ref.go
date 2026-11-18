package procscan

import (
	"berth/internal/adapters/peer"
	"berth/internal/core/ports"
)

// Ref names the process instance pid names now, with the start time the kernel reports, for a
// process the caller just started.
func Ref(pid int) (ports.ProcessRef, error) {
	p, err := peer.Process(pid)
	if err != nil {
		return ports.ProcessRef{}, ports.ErrProcessGone
	}
	return p.ProcessRef, nil
}
