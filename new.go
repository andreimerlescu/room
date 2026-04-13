package room

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/andreimerlescu/sema"
	"github.com/gin-gonic/gin"
)

// NewWaitingRoom returns a gin.HandlerFunc that enforces a FIFO waiting room
// with the given capacity. It is the simplest way to add a waiting room to
// an existing gin application.
//
// cap is the maximum number of requests actively served at any moment.
// Any value between 1 and math.MaxInt32 is valid.
//
// Usage:
//
//	r := gin.Default()
//	r.Use(room.NewWaitingRoom(500))
//
// For access to SetCap, SetHTML, SetReaperInterval, or StatusHandler after
// initialisation, use NewWaitingRoomFromStruct instead.
//
// Related: NewWaitingRoomFromStruct, WaitingRoom.Middleware
func NewWaitingRoom(r *gin.Engine, cap int32) gin.HandlerFunc {
	wr := &WaitingRoom{}
	if err := wr.Init(cap); err != nil {
		panic(fmt.Sprintf("room.NewWaitingRoom: %v", err))
	}
	r.GET("/queue/status", wr.StatusHandler())
	return wr.Middleware()
}

// NewWaitingRoomFromStruct returns a gin.HandlerFunc from a fully configured
// WaitingRoom. Use this when you need to retain a handle to the WaitingRoom
// after initialisation.
//
// Usage:
//
//	wr := &room.WaitingRoom{}
//	if err := wr.Init(500); err != nil {
//	    log.Fatal(err)
//	}
//	defer wr.Stop()
//	wr.RegisterRoutes(r)
//
// Related: NewWaitingRoom, WaitingRoom.RegisterRoutes
func NewWaitingRoomFromStruct(wr *WaitingRoom) gin.HandlerFunc {
	if wr == nil {
		panic("room.NewWaitingRoomFromStruct: nil WaitingRoom")
	}
	return wr.Middleware()
}

// Init initialises the WaitingRoom with the given capacity and starts the
// background reaper. It must be called before Middleware or RegisterRoutes
// when constructing a WaitingRoom manually.
//
// Returns ErrInvalidCap if cap < 1.
//
// Related: WaitingRoom.Stop, WaitingRoom.SetCap
func (wr *WaitingRoom) Init(cap int32) error {
	if cap < 1 {
		return ErrInvalidCap{Given: cap}
	}

	if wr.stopReaper != nil {
		wr.stopReaper()
	}

	wr.cap.Store(cap)
	wr.sem = sema.Must(int(cap))
	wr.cond = sync.NewCond(&wr.mu)
	wr.tokens = newTokenStore()
	wr.reaperRestart = make(chan struct{}, 1)
	wr.nowServing.Store(0)
	wr.nextTicket.Store(0)
	wr.reaperInterval.Store(int64(reaperInterval))
	wr.initialised.Store(true)

	ctx, cancel := context.WithCancel(context.Background())
	wr.stopReaper = cancel
	wr.startReaper(ctx)

	return nil
}

// Stop shuts down the background reaper goroutine. Call it when your
// application is shutting down to ensure a clean exit with no leaked
// goroutines.
//
// Related: WaitingRoom.Init, WaitingRoom.startReaper
func (wr *WaitingRoom) Stop() {
	if wr.stopReaper != nil {
		wr.stopReaper()
	}
}

// checkInitialised aborts the request with 500 and returns false if the
// WaitingRoom has not been initialised. Prevents nil pointer dereferences
// on zero-value WaitingRoom structs.
func (wr *WaitingRoom) checkInitialised(c *gin.Context) bool {
	if !wr.initialised.Load() {
		c.AbortWithStatus(http.StatusInternalServerError)
		return false
	}
	return true
}
