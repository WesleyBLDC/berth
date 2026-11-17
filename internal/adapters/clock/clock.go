// Package clock is the system clock adapter.
package clock

import "time"

type System struct{}

func (System) Now() time.Time { return time.Now() }
