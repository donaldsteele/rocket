package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/donaldsteele/rocket/internal/receiver"
)

// handleGetPair reports the pairing window: GET /api/pair answers
// {open, until, seconds}.
func (s *Server) handleGetPair(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.pairing.State())
}

// handlePair opens or closes the pairing window: POST /api/pair
// {seconds}, where 0 closes it and anything above an hour is capped at an
// hour. While it is open, an offer from a rocket sender not yet trusted is
// accepted without asking and its key is trusted from then on (a "paired"
// event per sender); browser drops are not affected. The new state is
// answered and sent as a "pairing" event.
func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Seconds *int `json:"seconds"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&body); err != nil || body.Seconds == nil || *body.Seconds < 0 {
		http.Error(w, "bad request: want {\"seconds\": n}, 0 to stop", http.StatusBadRequest)
		return
	}
	s.pairing.Start(time.Duration(*body.Seconds) * time.Second)
	writeJSON(w, s.pairing.State())
}

// Subscribe returns the server's events (what GET /api/events streams)
// until ctx ends; the console of rocket recv prints pairing from it.
func (s *Server) Subscribe(ctx context.Context) <-chan Event {
	ch := s.hub.subscribe()
	go func() {
		<-ctx.Done()
		s.hub.unsubscribe(ch)
	}()
	return ch
}

// PairingState is what GET /api/pair answers.
type PairingState = receiver.PairingState
