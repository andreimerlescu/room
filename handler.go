package room

import (
	"bytes"
	_ "embed"
	"html/template"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

//go:embed ticket_status.html
var ticketStatusPage string

// ticketStatusTmpl renders ticket_status.html. The page polls its own URL,
// so the only value injected is the position.
var ticketStatusTmpl = template.Must(template.New("ticket_status").Parse(ticketStatusPage))

// ticketStatusResponse is the JSON body of GET /queue/status/{id}.
type ticketStatusResponse struct {
	Waiting    bool  `json:"waiting"`
	Position   int64 `json:"position,omitempty"`
	QueueDepth int64 `json:"queue_depth,omitempty"`
}

// viewOutcome classifies a view of /queue/status/{id}.
type viewOutcome uint8

const (
	viewUnknown viewOutcome = iota
	viewRateLimited
	viewAccepted
)

// view applies one view of /queue/status/{id} under ts.mu.RLock + node.mu:
// the rate limit (one accepted view per minInterval per ID) and the O(log N)
// place in line. It never touches issuedAt or seen, so watching a place never
// keeps it alive: only its own client can.
func (ts *tokenStore) view(id string, now time.Time, minInterval time.Duration) (viewOutcome, int64) {
	ts.mu.RLock()
	t, ok := ts.byID[id]
	if !ok {
		ts.mu.RUnlock()
		return viewUnknown, 0
	}
	node := ts.entries[t]
	node.mu.Lock()
	if !node.viewedAt.IsZero() && now.Sub(node.viewedAt) < minInterval {
		node.mu.Unlock()
		ts.mu.RUnlock()
		return viewRateLimited, 0
	}
	node.viewedAt = now
	node.mu.Unlock()
	place := ts.tree.place(node)
	ts.mu.RUnlock()
	return viewAccepted, place
}

// serveTicketStatus is GET /queue/status/{id}: the place of a keyed
// ticket, by its public ID. A request that accepts text/html gets a small
// page that updates itself; anything else gets JSON. Unknown IDs — never
// issued, admitted, left, removed or reaped — get 404 {"waiting":false}.
func (wr *WaitingRoom) serveTicketStatus(w http.ResponseWriter, r *http.Request, id string) {
	if !wr.initialised.Load() {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	html := acceptsHTML(r)

	outcome := viewUnknown
	var place int64
	if id != "" && len(id) <= maxPersistedTokenLen {
		outcome, place = wr.tokens.view(id, time.Now(), statusPollMinInterval)
	}

	switch outcome {
	case viewUnknown:
		writeJSON(w, http.StatusNotFound, ticketStatusResponse{Waiting: false})
		return
	case viewRateLimited:
		w.Header().Set("Retry-After", "1")
		if html {
			http.Error(w, "Too many requests: try again in a second.", http.StatusTooManyRequests)
			return
		}
		writeJSON(w, http.StatusTooManyRequests, ticketStatusResponse{Waiting: true})
		return
	}

	if !html {
		writeJSON(w, http.StatusOK, ticketStatusResponse{
			Waiting:    true,
			Position:   place,
			QueueDepth: wr.LiveQueueDepth(),
		})
		return
	}

	var buf bytes.Buffer
	if err := ticketStatusTmpl.Execute(&buf, struct{ Position string }{
		Position: strconv.FormatInt(place, 10),
	}); err != nil {
		http.Error(w, "room: rendering status page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes()) // a failed write means the client went away
}

// TicketStatusHandler returns the gin.HandlerFunc for
// GET /queue/status/:ticket, which RegisterRoutes registers. Register it
// yourself, before the middleware, if you register routes manually. The
// route parameter must be named ticket.
//
// Related: WaitingRoom.Handler, WaitingRoom.Position
func (wr *WaitingRoom) TicketStatusHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		wr.serveTicketStatus(c.Writer, c.Request, c.Param("ticket"))
	}
}

// Handler returns the room's status routes as an http.Handler, for servers
// that are not built on gin:
//
//	OPTIONS /queue/status       CORS preflight (204)
//	GET     /queue/status       a cookie client's status (see StatusHandler)
//	GET     /queue/status/{id}  a keyed ticket's place, by public ID
//
// Mount it at the root, or under a prefix with http.StripPrefix and point
// SetStatusBaseURL at the prefixed path:
//
//	mux.Handle("/wait/", http.StripPrefix("/wait", wr.Handler()))
//	wr.SetStatusBaseURL("https://example.com/wait/queue/status/")
//
// A net/http server drives the line with Enter, Position and Leave (or
// Acquire and Release); the middleware itself is gin-only.
//
// Each call returns a new handler over the same room.
//
// Related: WaitingRoom.RegisterRoutes, WaitingRoom.Enter
func (wr *WaitingRoom) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("OPTIONS /queue/status", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /queue/status", wr.serveStatus)
	mux.HandleFunc("GET /queue/status/{id}", func(w http.ResponseWriter, r *http.Request) {
		wr.serveTicketStatus(w, r, r.PathValue("id"))
	})
	return mux
}
