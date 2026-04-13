package room

import (
	"fmt"
	"time"
)

// ErrReaperInterval is returned by SetReaperInterval when the provided
// duration falls outside [reaperMinInterval, reaperMaxInterval].
type ErrReaperInterval struct {
	Given time.Duration
	Min   time.Duration
	Max   time.Duration
}

func (e ErrReaperInterval) Error() string {
	return fmt.Sprintf(
		"room: reaper interval %s out of range [%s, %s]",
		e.Given, e.Min, e.Max,
	)
}

// ErrInvalidCap is returned by Init or SetCap when cap is less than 1.
type ErrInvalidCap struct {
	Given int32
}

func (e ErrInvalidCap) Error() string {
	return fmt.Sprintf("room: invalid capacity %d: must be >= 1", e.Given)
}

// ErrNotInitialised is returned by Middleware or RegisterRoutes when
// called on a WaitingRoom that has not been initialised via Init.
type ErrNotInitialised struct{}

func (e ErrNotInitialised) Error() string {
	return "room: WaitingRoom not initialised — call Init before use"
}
