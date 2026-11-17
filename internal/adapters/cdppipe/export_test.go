package cdppipe

// WatchKills records every process group Kill signals until the returned function is called.
func WatchKills() (kills func() []int, restore func()) {
	orig := killGroup
	var got []int
	killGroup = func(pgid int) error {
		got = append(got, pgid)
		return orig(pgid)
	}
	return func() []int { return got }, func() { killGroup = orig }
}
