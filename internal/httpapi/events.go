package httpapi

import (
	"encoding/json"
	"net/http"
	"sync"
)

// Event is a message pushed to connected browsers over SSE. Types: "offer"
// (an IncomingOffer), "offer_gone", "progress", "done", "sent", "error"; each
// carries a transfer_id.
type Event struct {
	Type string `json:"type"`
	Data any    `json:"data,omitempty"`
}

// eventHub fans out events to all connected SSE clients.
type eventHub struct {
	mu      sync.Mutex
	clients map[chan Event]struct{}
}

func newEventHub() *eventHub { return &eventHub{clients: map[chan Event]struct{}{}} }

func (h *eventHub) subscribe() chan Event {
	ch := make(chan Event, 64)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *eventHub) unsubscribe(ch chan Event) {
	h.mu.Lock()
	delete(h.clients, ch)
	h.mu.Unlock()
	close(ch)
}

// publish sends ev to every subscriber, dropping it for any slow client rather
// than blocking.
func (h *eventHub) publish(ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients {
		select {
		case ch <- ev:
		default: // slow client: drop rather than stall the sender
		}
	}
}

// handleEvents serves the SSE stream.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)
	flusher.Flush()

	enc := json.NewEncoder(w)
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			if _, err := w.Write([]byte("data: ")); err != nil {
				return
			}
			if err := enc.Encode(ev); err != nil {
				return
			}
			if _, err := w.Write([]byte("\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
