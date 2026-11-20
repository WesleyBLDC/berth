package testutil

import "time"

// Eventually polls cond until it holds or five seconds pass and reports whether it held, for a
// test waiting on what another process, a real socket or the kernel does in the background. A
// test whose waits are all on goroutines and timers runs in a synctest bubble and uses
// synctest.Wait instead: a real socket, kqueue or os/exec pipe would freeze the bubble's clock.
func Eventually(cond func() bool) bool {
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
	return true
}
