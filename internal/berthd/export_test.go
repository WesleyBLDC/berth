package berthd

// count is how many actors are running, for tests that check they stay bounded.
func (a *actors[S, E, F]) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.live)
}
