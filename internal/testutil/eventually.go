package testutil

import "time"

// Eventually polls cond until it holds or five seconds pass and reports whether it held, for a
// test waiting on what another goroutine or process does in the background.
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
