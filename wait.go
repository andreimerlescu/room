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
// default waiting-room page or JSON wait response. For keyed tickets t.Token
// is empty. Runs outside all room locks; the middleware aborts after fn returns.
type WaitResponder func(c *gin.Context, t TicketInfo)

// SetWaitResponder overrides the queued response for both cookie and keyed
// requests. Pass nil to restore defaults. Not reset by [WaitingRoom.Init].
func (wr *WaitingRoom) SetWaitResponder(fn WaitResponder) {
	if fn == nil {
		wr.waitResponder.Store(nil)
		return
	}
	wr.waitResponder.Store(&fn)
}

func (wr *WaitingRoom) waitResponderLoad() WaitResponder {
	if p := wr.waitResponder.Load(); p != nil {
		return *p
	}
	return nil
}

// SetWaitJSON opts cookie requests that do not accept text/html into the JSON
// wait response (503, Retry-After). Off by default; reset to false by Init.
func (wr *WaitingRoom) SetWaitJSON(on bool) {
	wr.waitJSON.Store(on)
}

// WaitJSON reports whether SetWaitJSON is enabled.
func (wr *WaitingRoom) WaitJSON() bool {
	return wr.waitJSON.Load()
}

// SetStatusBaseURL sets the prefix prepended to a keyed ticket's public ID in
// status_url (empty restores "/queue/status/"). Reset by Init.
func (wr *WaitingRoom) SetStatusBaseURL(base string) {
	if base == "" {
		base = defaultStatusBaseURL
	}
	wr.statusBaseURL.Store(&base)
}

// StatusBaseURL returns the current status_url prefix.
func (wr *WaitingRoom) StatusBaseURL() string {
	if p := wr.statusBaseURL.Load(); p != nil && *p != "" {
		return *p
	}
	return defaultStatusBaseURL
}

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

type waitResponse struct {
	Waiting    bool   `json:"waiting"`
	Position   int64  `json:"position"`
	QueueDepth int64  `json:"queue_depth"`
	StatusURL  string `json:"status_url"`
	RetryAfter int    `json:"retry_after"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "room: encoding response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write(b)
}

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

type keyedAction uint8

const (
	keyedPass keyedAction = iota
	keyedAdmit
	keyedWait
	keyedFull
	keyedError
)

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

func (wr *WaitingRoom) decideKeyed(key, clientKey string, now time.Time) (keyedAction, int64, keyedEntry) {
	wr.leaseMu.RLock()
	defer wr.leaseMu.RUnlock()

	if _, held := wr.leases[key]; held {
		return keyedPass, 0, keyedEntry{}
	}

	if token, e, ok := wr.tokens.lookupKey(key); ok {
		if wr.ticketReady(e.ticket) {
			if claimed, won := wr.tokens.take(token); won {
				return keyedAdmit, claimed.ticket, keyedEntry{}
			}
		} else if k := wr.tokens.enterKey(key, "", ticketEntry{}, nil, now); k.token != "" {
			return keyedWait, 0, k
		}
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
		wr.retire(ticket)
	}
	return keyedWait, 0, k
}
