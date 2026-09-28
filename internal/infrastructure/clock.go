// Package infrastructure provides small production adapters shared by the app.
package infrastructure

import "time"

// SystemClock implements service.Clock using the process wall clock.
type SystemClock struct{}

// Now returns the current wall-clock time.
func (SystemClock) Now() time.Time { return time.Now() }

// After returns a channel that fires after d.
func (SystemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
