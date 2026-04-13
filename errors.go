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

// ErrInvalidMaxQueueDepth is returned by SetMaxQueueDepth when the
// value is negative.
type ErrInvalidMaxQueueDepth struct {
	Given int64
}

func (e ErrInvalidMaxQueueDepth) Error() string {
	return fmt.Sprintf("room: invalid max queue depth %d: must be >= 0", e.Given)
}

// ErrPromotionDisabled is returned when PromoteToken or QuoteCost is
// called without a RateFunc configured via SetRateFunc.
type ErrPromotionDisabled struct{}

func (e ErrPromotionDisabled) Error() string {
	return "room: promotions disabled — call SetRateFunc first"
}

// ErrTokenNotFound is returned when the token does not exist in the
// token store (expired, already admitted, or never issued).
type ErrTokenNotFound struct{}

func (e ErrTokenNotFound) Error() string {
	return "room: token not found"
}

// ErrAlreadyAdmitted is returned when a promotion is attempted on a
// token that is already within the serving window.
type ErrAlreadyAdmitted struct{}

func (e ErrAlreadyAdmitted) Error() string {
	return "room: token already within serving window"
}

// ErrInvalidTargetPosition is returned when targetPosition < 1.
type ErrInvalidTargetPosition struct {
	Given int64
}

func (e ErrInvalidTargetPosition) Error() string {
	return fmt.Sprintf("room: invalid target position %d: must be >= 1", e.Given)
}
