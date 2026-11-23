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

var ticketStatusTmpl = template.Must(template.New("ticket_status").Parse(ticketStatusPage))

type ticketStatusResponse struct {
	Waiting    bool  `json:"waiting"`
	Position   int64 `json:"position,omitempty"`
	QueueDepth int64 `json:"queue_depth,omitempty"`
}

// serveTicketStatus serves GET /queue/status/{id} as HTML (when Accept
// includes text/html) or JSON. Unknown IDs receive 404 {"waiting":false}.
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
	_, _ = w.Write(buf.Bytes())
}

// TicketStatusHandler returns the gin.HandlerFunc for GET /queue/status/:ticket.
// Register before [WaitingRoom.Middleware] if wiring routes manually.
func (wr *WaitingRoom) TicketStatusHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		wr.serveTicketStatus(c.Writer, c.Request, c.Param("ticket"))
	}
}

// Handler returns the status routes (OPTIONS /queue/status, GET /queue/status,
// GET /queue/status/{id}) as a standard net/http Handler. Mount under a prefix
// with http.StripPrefix and configure [WaitingRoom.SetStatusBaseURL] to match.
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
