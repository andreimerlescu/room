package room

import (
	"encoding/json"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// WaitResponder writes the response for a queued request in place of the
// default (the waiting-room page, or the JSON wait response). t describes
// the request's ticket, with Place filled in. For a keyed ticket t.Token is
// empty: a keyed ticket's token is never handed out.
//
// The middleware has already set the room's cookies on a cookie request,
// and aborts the chain after fn returns; fn must write a complete
// response. It runs outside every room lock and may call back into the
// room.
//
// Related: WaitingRoom.SetWaitResponder
type WaitResponder func(c *gin.Context, t TicketInfo)

type waitResponderHolder struct {
	fn WaitResponder
}

// SetWaitResponder lets the application write the response for every
// queued request, cookie and keyed. Pass nil to restore the defaults:
// the waiting-room page for cookie requests, and the JSON wait response
// for keyed requests (and, with SetWaitJSON, for cookie requests that do
// not accept text/html). Like SetClientKeyFunc, it is not reset by Init.
//
// Safe to call at any time.
//
// Related: WaitResponder, WaitingRoom.SetWaitJSON, WaitingRoom.SetTicketKeyFunc
func (wr *WaitingRoom) SetWaitResponder(fn WaitResponder) {
	if fn == nil {
		wr.waitResponder.Store((*waitResponderHolder)(nil))
		return
	}
	wr.waitResponder.Store(&waitResponderHolder{fn: fn})
}

func (wr *WaitingRoom) waitResponderLoad() WaitResponder {
	h, _ := wr.waitResponder.Load().(*waitResponderHolder)
	if h == nil {
		return nil
	}
	return h.fn
}

// SetWaitJSON opts cookie requests into the JSON wait response when they
// do not accept text/html — API clients and fetch() calls rather than
// page loads. It is off by default, so every cookie request gets the
// waiting-room page exactly as before. Keyed requests (SetTicketKeyFunc)
// always get the JSON response.
//
// A request accepts text/html when its Accept header lists text/html or
// application/xhtml+xml with a non-zero quality. A missing Accept header
// or a bare */* does not count, so with this on, curl and most HTTP
// libraries get JSON; browsers navigating to a page always send text/html.
//
// A cookie request answered with JSON still receives the room_ticket and
// room_probe cookies, and polls GET /queue/status as before.
//
// Safe to call at any time. Reset to false by Init.
//
// Related: WaitingRoom.SetWaitResponder
func (wr *WaitingRoom) SetWaitJSON(on bool) {
	wr.waitJSON.Store(on)
}

// WaitJSON reports whether SetWaitJSON is on.
func (wr *WaitingRoom) WaitJSON() bool {
	return wr.waitJSON.Load()
}

// SetStatusBaseURL sets the prefix to which a keyed ticket's ID is appended
// to form the status_url in a JSON wait response. The default is
// "/queue/status/", where RegisterRoutes and Handler serve it. Set it to
// an absolute URL ("https://example.com/wait/queue/status/") when the
// status endpoint is mounted elsewhere or behind a proxy, since room cannot
// know the public scheme and host. An empty value restores the default.
//
// Safe to call at any time. Reset by Init.
func (wr *WaitingRoom) SetStatusBaseURL(base string) {
	if base == "" {
		base = defaultStatusBaseURL
	}
	wr.statusBaseURL.Store(base)
}

// StatusBaseURL returns the current status_url prefix.
func (wr *WaitingRoom) StatusBaseURL() string {
	s, _ := wr.statusBaseURL.Load().(string)
	if s == "" {
		return defaultStatusBaseURL
	}
	return s
}

// acceptsHTML reports whether the request's Accept header lists text/html
// or application/xhtml+xml with a non-zero quality.
func acceptsHTML(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept"), ",") {
		mt, params, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil || (mt != "text/html" && mt != "application/xhtml+xml") {
			continue
		}
		if q, ok := params["q"]; ok {
			if f, err := strconv.ParseFloat(q, 64); err == nil && f <= 0 {
				continue
			}
		}
		return true
	}
	return false
}

// waitResponse is the JSON wait response.
type waitResponse struct {
	Waiting    bool   `json:"waiting"`
	Position   int64  `json:"position"`
	QueueDepth int64  `json:"queue_depth"`
	StatusURL  string `json:"status_url"`
	RetryAfter int    `json:"retry_after"`
}

// writeJSON writes v as JSON with the given status, as gin's c.JSON does
// (encoding/json, no trailing newline).
func writeJSON(w http.ResponseWriter, code int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "room: encoding response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write(b) // a failed write means the client went away
}

// writeWaitJSON sends 503 with Retry-After and the JSON wait response.
func (wr *WaitingRoom) writeWaitJSON(w http.ResponseWriter, position int64, statusURL string) {
	secs := int(waitRetryAfter / time.Second)
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusServiceUnavailable, waitResponse{
		Waiting:    true,
		Position:   position,
		QueueDepth: wr.LiveQueueDepth(),
		StatusURL:  statusURL,
		RetryAfter: secs,
	})
}

// respondCookieWait answers a queued cookie request through the wait
// responder or the opt-in JSON response. It reports false when neither
// applies and the caller should render the waiting-room page. position is
// the window-relative position the page and /queue/status report.
func (wr *WaitingRoom) respondCookieWait(c *gin.Context, token string, position int64) bool {
	if fn := wr.waitResponderLoad(); fn != nil {
		e, ok := wr.tokens.get(token)
		ti := wr.ticketInfo(token, e)
		if ok {
			ti.Place, _ = wr.tokens.placeOfToken(token)
		}
		fn(c, ti)
		c.Abort()
		return true
	}
	if wr.waitJSON.Load() && !acceptsHTML(c.Request) {
		wr.writeWaitJSON(c.Writer, position, "/queue/status")
		c.Abort()
		return true
	}
	return false
}

// respondKeyedWait answers a queued keyed request: through the wait
// responder if one is set, otherwise with the JSON wait response, whose
// position is the key's place in line (as Enter reports it).
func (wr *WaitingRoom) respondKeyedWait(c *gin.Context, token string, e ticketEntry, place int64) {
	if fn := wr.waitResponderLoad(); fn != nil {
		ti := wr.ticketInfo(token, e)
		ti.Token = ""
		ti.Place = place
		fn(c, ti)
		c.Abort()
		return
	}
	wr.writeWaitJSON(c.Writer, place, wr.StatusBaseURL()+e.id)
	c.Abort()
}

// keyedAction is what serveKeyed decided under the lease lock.
type keyedAction uint8

const (
	keyedPass  keyedAction = iota // key holds a lease: run the handler
	keyedAdmit                    // admit ticket (claimed or fresh)
	keyedWait                     // queued: answer with the wait response
	keyedFull                     // breaker tripped
	keyedError                    // could not issue a ticket
)

// serveKeyed is the middleware for a request SetTicketKeyFunc recognised.
// It mirrors the cookie flow — resume, breaker, fast path, slow path —
// with the key as the ticket's identity and no cookies.
func (wr *WaitingRoom) serveKeyed(c *gin.Context, key string, hasPass bool) {
	if err := validKey(key); err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	act, ticket, k := wr.decideKeyed(key, wr.clientKey(c), time.Now())
	switch act {
	case keyedPass:
		c.Next()
		return
	case keyedAdmit:
		wr.admit(c, ticket)
		return
	case keyedFull:
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	case keyedError:
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	if hasPass && !k.entry.promoted {
		wr.autoPromote(k.token)
		if e, ok := wr.tokens.get(k.token); ok {
			k.entry = e
		}
		if p, ok := wr.tokens.placeOfToken(k.token); ok {
			k.place = p
		}
	}
	if k.created {
		wr.emit(EventQueue, wr.snapshotForKey(EventQueue, key))
	}
	wr.respondKeyedWait(c, k.token, k.entry, k.place)
}

// decideKeyed runs the part of serveKeyed that must see a consistent lease
// table: whether the key holds a lease, resumes its ticket, or gets a new
// one. It holds wr.leaseMu for reading and never calls the handler.
func (wr *WaitingRoom) decideKeyed(key, clientKey string, now time.Time) (keyedAction, int64, keyedEntry) {
	wr.leaseMu.RLock()
	defer wr.leaseMu.RUnlock()

	if _, held := wr.leases[key]; held {
		return keyedPass, 0, keyedEntry{}
	}

	if token, e, ok := wr.tokens.lookupKey(key); ok {
		if wr.ticketReady(e.ticket) {
			// Claim before acquiring, exactly as the cookie path does.
			if claimed, won := wr.tokens.take(token); won {
				return keyedAdmit, claimed.ticket, keyedEntry{}
			}
			// Reaped or removed in between: a new arrival.
		} else if k := wr.tokens.enterKey(key, "", ticketEntry{}, nil, now); k.token != "" {
			return keyedWait, 0, k
		}
		// Gone between the lookup and the refresh: a new arrival.
	}

	if _, full := wr.queueFull(); full {
		return keyedFull, 0, keyedEntry{}
	}

	ticket := wr.nextTicket.Add(1)
	if wr.ticketReady(ticket) {
		return keyedAdmit, ticket, keyedEntry{}
	}

	token, fresh, err := wr.newKeyedEntry(key, now)
	if err != nil {
		wr.retire(ticket)
		return keyedError, 0, keyedEntry{}
	}
	fresh.clientKey = clientKey
	k := wr.tokens.enterKey(key, token, fresh, func() int64 { return ticket }, now)
	if !k.created {
		// A concurrent request with this key queued first; this number
		// will never be admitted.
		wr.retire(ticket)
	}
	return keyedWait, 0, k
}
