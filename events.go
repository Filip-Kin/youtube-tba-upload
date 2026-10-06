package main

// Event stream for FIM-AV Assistant: GET /api/events.
//
// A Server-Sent Events stream on the same listener as the rest of the API. On
// connect the client gets one "hello" message with the full current state, then
// one JSON message per change. Messages carry their kind in a "type" field
// (there is no SSE "event:" line), and a ": ping" comment goes out every 15 s
// so a dead connection shows up on either end.
//
// Nothing here polls. Changes come from the places that already make them:
// stateStore.update (queue counts, the re-auth flag), setSignIn (sign-in), the
// upload worker (finished/failed uploads, quota errors) and switchWatch (what is
// being watched). With no client connected every hook returns at its first
// check, so the stream costs nothing when nobody listens.
//
// The protocol is shared with the other FIM-AV Assistant add-ons; keep the
// shapes in INTEGRATION.md in step with this file.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	eventsAddon    = "youtube-tba-upload"
	eventsProtocol = 1
	// queueDebounce collects a burst of state changes (a scan, an upload's
	// several writes) into one queue message.
	queueDebounce = time.Second
	// eventsBuffer is how many messages a client may fall behind before it is
	// dropped. A dropped client reconnects and gets a fresh hello.
	eventsBuffer = 64
)

// eventsPing is the keep-alive interval. A var so a test could shorten it.
var eventsPing = 15 * time.Second

type signinMsg struct {
	SignedIn bool    `json:"signedIn"`
	Channel  *string `json:"channel"`
}

type queueMsg struct {
	EventKey *string        `json:"eventKey"`
	Counts   map[string]int `json:"counts"`
}

type watchingMsg struct {
	VideoDir string  `json:"videoDir"`
	EventKey *string `json:"eventKey"`
	Program  string  `json:"program"`
}

type uploadMsg struct {
	File     string  `json:"file"`
	EventKey string  `json:"eventKey"`
	Match    *string `json:"match"`
	Status   string  `json:"status"` // done | failed
	URL      *string `json:"url"`
	Error    *string `json:"error"`
}

type quotaMsg struct {
	Error string `json:"error"`
}

func strOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// typed marshals v (a JSON object) with "type" as its first field.
func typed(kind string, v any) []byte {
	b, _ := json.Marshal(v)
	head := `{"type":` + mustJSON(kind)
	if len(b) <= 2 {
		return []byte(head + "}")
	}
	return []byte(head + "," + string(b[1:]))
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// eventHub fans messages out to the connected stream clients and remembers the
// last state it sent, so it only speaks on a real change.
//
// Lock order: hub.mu is a leaf with respect to managersMu and stateStore.mu.
// Code holding hub.mu may call getSignIn, currentProgram and read
// settings.VideoDir, and nothing that takes a store or manager lock.
type eventHub struct {
	mu      sync.Mutex
	clients map[chan []byte]struct{}

	// cur is the store of the current event: the one queue counts and the
	// re-auth flag come from. Set when a manager is created and on a switch.
	cur    *stateStore
	reauth bool // cur's needs-sign-in flag, as last seen

	signin     signinMsg
	watching   watchingMsg
	lastQueue  string
	queueTimer *time.Timer
}

var hub = &eventHub{clients: map[chan []byte]struct{}{}}

func (h *eventHub) subscribe() chan []byte {
	ch := make(chan []byte, eventsBuffer)
	h.mu.Lock()
	if len(h.clients) == 0 {
		// Counts were not tracked while nobody listened; make the next flush
		// send whatever it finds.
		h.lastQueue = ""
	}
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *eventHub) unsubscribe(ch chan []byte) {
	h.mu.Lock()
	if _, ok := h.clients[ch]; ok {
		delete(h.clients, ch)
		close(ch)
	}
	h.mu.Unlock()
}

// broadcastLocked sends one message to every client. A client whose buffer is
// full is dropped rather than allowed to stall the sender.
func (h *eventHub) broadcastLocked(msg []byte) {
	for ch := range h.clients {
		select {
		case ch <- msg:
		default:
			delete(h.clients, ch)
			close(ch)
		}
	}
}

func (h *eventHub) emit(kind string, v any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.clients) == 0 {
		return
	}
	h.broadcastLocked(typed(kind, v))
}

func (h *eventHub) currentKey() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cur == nil {
		return ""
	}
	return h.cur.eventKey
}

// setCurrent makes s the current event (nil for none) and reports what changed.
func (h *eventHub) setCurrent(s *stateStore) {
	reauth := false
	if s != nil {
		_, reauth = s.queueSummary()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cur = s
	h.reauth = reauth
	h.signinChangedLocked()
	h.noteWatchingLocked()
	h.scheduleQueueLocked()
}

// noteWatching reports a change in the folder, event or program.
func (h *eventHub) noteWatching() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.noteWatchingLocked()
}

func (h *eventHub) noteWatchingLocked() {
	w := watchingMsg{VideoDir: settings.VideoDir, Program: currentProgram()}
	if h.cur != nil {
		w.EventKey = strOrNil(h.cur.eventKey)
	}
	if mustJSON(w) == mustJSON(h.watching) {
		return
	}
	h.watching = w
	if len(h.clients) > 0 {
		h.broadcastLocked(typed("watching", w))
	}
}

// signinChanged is called by setSignIn.
func (h *eventHub) signinChanged() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.signinChangedLocked()
}

// signinChangedLocked recomputes the sign-in the stream reports: signed in to
// the profile AND the current event not stopped for a session that expired
// mid-upload. The second half is what turns an auth failure into a message.
func (h *eventHub) signinChangedLocked() {
	si := getSignIn()
	m := signinMsg{SignedIn: si.SignedIn && !h.reauth}
	if m.SignedIn {
		m.Channel = strOrNil(si.ChannelName)
	}
	if mustJSON(m) == mustJSON(h.signin) {
		return
	}
	h.signin = m
	if len(h.clients) > 0 {
		h.broadcastLocked(typed("signin", m))
	}
}

// storeChanged is every stateStore's onChange hook. Only the current event's
// store matters, and only while someone is listening.
func (h *eventHub) storeChanged(s *stateStore) {
	h.mu.Lock()
	idle := len(h.clients) == 0 || s != h.cur
	h.mu.Unlock()
	if idle {
		return
	}
	_, reauth := s.queueSummary()
	h.mu.Lock()
	defer h.mu.Unlock()
	if s != h.cur {
		return
	}
	h.reauth = reauth
	h.signinChangedLocked()
	h.scheduleQueueLocked()
}

func (h *eventHub) scheduleQueueLocked() {
	if len(h.clients) == 0 || h.queueTimer != nil {
		return
	}
	h.queueTimer = time.AfterFunc(queueDebounce, h.flushQueue)
}

func (h *eventHub) flushQueue() {
	h.mu.Lock()
	h.queueTimer = nil
	s := h.cur
	h.mu.Unlock()
	q := queueOf(s)
	b := mustJSON(q)
	h.mu.Lock()
	defer h.mu.Unlock()
	if s != h.cur || b == h.lastQueue || len(h.clients) == 0 {
		return
	}
	h.lastQueue = b
	h.broadcastLocked(typed("queue", q))
}

// queueOf counts the current event's match videos by status.
func queueOf(s *stateStore) queueMsg {
	if s == nil {
		return queueMsg{Counts: map[string]int{}}
	}
	counts, _ := s.queueSummary()
	return queueMsg{EventKey: strOrNil(s.eventKey), Counts: counts}
}

// hello builds the first message for a new client: everything the change
// messages would otherwise have told it.
func (h *eventHub) hello() []byte {
	h.mu.Lock()
	s := h.cur
	h.mu.Unlock()
	q := queueOf(s)
	reauth := false
	if s != nil {
		_, reauth = s.queueSummary()
	}
	h.mu.Lock()
	if s == h.cur {
		h.reauth = reauth
	}
	h.signinChangedLocked()
	h.noteWatchingLocked()
	msg := map[string]any{
		"type":     "hello",
		"addon":    eventsAddon,
		"protocol": eventsProtocol,
		"version":  Version,
		"signin":   h.signin,
		"queue":    q,
		"watching": h.watching,
	}
	h.mu.Unlock()
	b, _ := json.Marshal(msg)
	return b
}

// emitUpload reports one video's final outcome.
func emitUpload(eventKey, file string, e *videoEntry) {
	if e == nil {
		return
	}
	m := uploadMsg{File: file, EventKey: eventKey}
	if e.Meta != nil {
		key := e.Meta.TBAMatchKey
		if isFTC() {
			key = e.Meta.TOAMatchKey
		}
		m.Match = strOrNil(key)
	}
	if e.Status == statusUploaded {
		m.Status = "done"
		m.URL = strOrNil(youtubeURL(e.YTVideoID))
	} else {
		m.Status = "failed"
		m.Error = strOrNil(e.LastError)
	}
	hub.emit("upload", m)
}

func youtubeURL(id string) string {
	if id == "" {
		return ""
	}
	return "https://www.youtube.com/watch?v=" + id
}

// quotaWords are what YouTube says when it stops taking uploads for a while:
// the daily upload cap, a quota, or a rate limit.
var quotaWords = []string{"quota", "upload limit", "daily limit", "rate limit", "ratelimit", "too many requests"}

// isQuotaError reports whether an upload error is YouTube refusing for volume
// rather than something wrong with this video.
func isQuotaError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, w := range quotaWords {
		if strings.Contains(msg, w) {
			return true
		}
	}
	return false
}

// apiEvents serves GET /api/events.
func apiEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("access-control-allow-origin", "*")
	if r.Method == http.MethodOptions {
		w.Header().Set("access-control-allow-methods", "GET, OPTIONS")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.Header().Set("connection", "keep-alive")
	w.Header().Set("x-accel-buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Subscribe before building hello: a change in between is then delivered
	// after hello (at worst a repeat) instead of lost.
	ch := hub.subscribe()
	defer hub.unsubscribe(ch)
	if _, err := fmt.Fprintf(w, "data: %s\n\n", hub.hello()); err != nil {
		return
	}
	fl.Flush()

	ping := time.NewTicker(eventsPing)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return // fell too far behind; the client reconnects
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", msg); err != nil {
				return
			}
			fl.Flush()
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			fl.Flush()
		}
	}
}
