package receiver

import (
	"fmt"
	"strings"
	"time"

	"github.com/donaldsteele/rocket/internal/history"
	"github.com/donaldsteele/rocket/internal/transfer"
)

// Event reports an offer or a transfer: desktop rocket pushes it to its
// browser UI over SSE, the Android app gets it as JSON. Types: "offer" (an
// IncomingOffer), "offer_gone" {transfer_id, reason: accepted, declined,
// timed out or withdrawn}, "progress", "done", "sent", "error" {result,
// error, dir} and "cancelled" {dir, by: local or peer, error}; each carries a
// transfer_id. "name" is the transfer label (the first top-level file or
// folder); "progress" adds dir, sent, total, file, file_index (1-based) and
// files, and "done"/"sent" add files and bytes.
type Event struct {
	Type string `json:"type"`
	Data any    `json:"data,omitempty"`
}

// Ledger keeps the books that sends and receives share: the transfers in
// flight, the history of finished ones, and the events that report both.
type Ledger struct {
	// Transfers tracks the transfers in flight in both directions.
	Transfers *transfer.Manager
	// History records finished transfers; nil records nothing.
	History *history.Store
	// Events receives every event; nil drops them. It must not block.
	Events func(Event)
	// Here names this host in history messages ("cancelled on this
	// computer"); empty means "this computer".
	Here string
}

// here is l.Here, or "this computer" when it is not set.
func (l *Ledger) here() string {
	if l.Here == "" {
		return "this computer"
	}
	return l.Here
}

// Publish sends ev to l.Events.
func (l *Ledger) Publish(ev Event) {
	if l.Events != nil {
		l.Events(ev)
	}
}

// AddHistory writes e to the history; a write failure is published as an
// "error" event.
func (l *Ledger) AddHistory(e history.Entry) {
	if l.History == nil {
		return
	}
	if err := l.History.Add(e); err != nil {
		l.Error(e.ID, e.Name, fmt.Errorf("could not write the history: %w", err))
	}
}

// Error publishes an "error" event for transfer transferID.
func (l *Ledger) Error(transferID, name string, err error) {
	l.Publish(Event{Type: "error", Data: map[string]any{"transfer_id": transferID, "name": name, "error": err.Error()}})
}

// Progress returns a transfer.ProgressFn that records progress with the
// transfer manager and publishes "progress" events. name is the transfer
// label (constant for the whole transfer); file, file_index (1-based) and
// files describe the file in flight.
func (l *Ledger) Progress(transferID, name, dir string) transfer.ProgressFn {
	return func(p transfer.Progress) {
		l.Transfers.Progress(transferID, p)
		l.Publish(Event{Type: "progress", Data: map[string]any{
			"transfer_id": transferID, "name": name, "sent": p.Sent, "total": p.Total, "dir": dir,
			"file": p.File, "file_index": p.FileIndex, "files": p.Files,
		}})
	}
}

// End finishes transfer id: it leaves the manager, its history entry is
// written, and for anything but success a "cancelled" or "error" event goes
// out (the caller publishes "done" or "sent" on success). A transfer
// cancelled here counts as cancelled whatever error the cancel caused.
func (l *Ledger) End(id string, err error) (history.Result, error) {
	info, here := l.Transfers.Finish(id)
	if err != nil && here {
		err = fmt.Errorf("%w on %s", transfer.ErrCancelled, l.here())
	}
	res := transfer.Outcome(err)
	e := history.Entry{
		ID: id, Dir: info.Dir, Name: info.Name, PeerName: info.PeerName, PeerFP: info.PeerFP,
		Files: info.Files, Bytes: info.Total, Started: info.Started, Result: res,
	}
	if err != nil {
		e.Error = err.Error()
	}
	l.AddHistory(e)
	switch res {
	case history.ResultOK:
	case history.ResultCancelled:
		by := "peer"
		if here {
			by = "local"
		}
		l.Publish(Event{Type: "cancelled", Data: map[string]any{
			"transfer_id": id, "name": info.Name, "dir": info.Dir, "by": by, "error": err.Error(),
		}})
	default:
		l.Publish(Event{Type: "error", Data: map[string]any{
			"transfer_id": id, "name": info.Name, "dir": info.Dir, "result": res, "error": err.Error(),
		}})
	}
	return res, err
}

// RecordOffer writes the history entry of an incoming offer that never
// became a transfer.
func (l *Ledger) RecordOffer(v IncomingOffer, res history.Result, msg string) {
	top, _, _ := strings.Cut(v.Name, "/")
	now := time.Now()
	l.AddHistory(history.Entry{
		ID: v.TransferID, Dir: history.DirIn, Name: top, PeerName: v.SenderName, PeerFP: v.SenderFP,
		Files: v.Files, Bytes: v.Size, Started: now, Ended: now, Result: res, Error: msg,
	})
}
