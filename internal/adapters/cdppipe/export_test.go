package cdppipe

import (
	"context"
	"time"
)

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

// FailExitWatch makes the exit watch fail with err until restore is called.
func FailExitWatch(err error) (restore func()) {
	orig := watchExit
	watchExit = func(context.Context, int) (<-chan struct{}, error) { return nil, err }
	return func() { watchExit = orig }
}

// ShortenWrites sets the write timeout until restore is called.
func ShortenWrites(d time.Duration) (restore func()) {
	orig := writeTimeout
	writeTimeout = d
	return func() { writeTimeout = orig }
}
