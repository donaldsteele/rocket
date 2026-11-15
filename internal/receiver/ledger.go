package receiver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/donaldsteele/rocket/internal/history"
	"github.com/donaldsteele/rocket/internal/node"
	"github.com/donaldsteele/rocket/internal/protocol"
	"github.com/donaldsteele/rocket/internal/resume"
	"github.com/donaldsteele/rocket/internal/transfer"
)

// Event reports an offer or a transfer: desktop rocket pushes it to its
// browser UI over SSE, the Android app gets it as JSON. Types: "offer" (an
// IncomingOffer), "offer_gone" {transfer_id, reason: accepted, declined,
// timed out or withdrawn}, "progress", "done", "sent", "error" {result,
// error, dir}, "cancelled" {dir, by: local or peer, error} and "retrying"
// {dir, attempt, attempts, delay_ms, error}; each carries a transfer_id.
// "name" is the transfer label (the first top-level file or folder);
// "progress" adds dir, sent, total, file, file_index (1-based) and files,
// and "done"/"sent" add files and bytes. An "error" whose result is
// "interrupted" ended resumable. Kept transfers on the receiving side have
// "kept" (a KeptView) and "kept_gone" {id, reason}.
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

// Retrying records that outgoing transfer id broke (err) and will be
// offered again after delay, retry n of max: the transfer shows as
// reconnecting, and a "retrying" event {transfer_id, name, dir, attempt,
// attempts, delay_ms, error} goes out.
func (l *Ledger) Retrying(id, name string, n, max int, delay time.Duration, err error) {
	l.Transfers.Retrying(id, n, max)
	l.Publish(Event{Type: "retrying", Data: map[string]any{
		"transfer_id": id, "name": name, "dir": history.DirOut, "attempt": n, "attempts": max,
		"delay_ms": delay.Milliseconds(), "error": err.Error(),
	}})
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
		Files: info.Files, Bytes: info.Total, Started: info.Started, Result: res, Encrypted: info.Encrypted,
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

// Outgoing is a send run by Ledger.Send.
type Outgoing struct {
	ID       string // local transfer ID, already registered with Transfers.Start
	PeerName string
	Target   transfer.Target
	Manifest protocol.Manifest
	Open     transfer.OpenFunc
	// Resumable makes the send retry and resume (transfer.SendOptions.Retry;
	// Open must then be callable again), and, when it gives up after its
	// retries, recorded in Outbox with Paths.
	Resumable bool
	Delays    []time.Duration // nil: transfer.RetryDelays
	Outbox    *resume.Outbox
	Paths     []string
}

// Send runs outgoing transfer o to its end: retries show as reconnecting
// ("retrying" events), a send that gives up after its retries is put in
// o.Outbox (so it can be resumed or discarded), a send that completes
// removes earlier interrupted sends of the same files to the same peer
// from o.Outbox (resume.Outbox.Supersede), and then the transfer ends
// as End does, with a "sent" event on success. It returns the error as End
// reports it.
func (l *Ledger) Send(ctx context.Context, id *node.Identity, o Outgoing) error {
	name := transfer.Label(o.Manifest)
	var receiverFP, remoteID string
	opts := transfer.SendOptions{
		Retry:  o.Resumable,
		Delays: o.Delays,
		OnRetry: func(n, max int, delay time.Duration, err error) {
			l.Retrying(o.ID, name, n, max, delay, err)
		},
		OnAccepted: func(fp, rid string) { receiverFP, remoteID = fp, rid },
	}
	err := transfer.SendManifestOpts(ctx, id, o.Target, o.Manifest, o.Open, l.Progress(o.ID, name, "out"), opts)
	mh, _ := o.Manifest.Hash()
	if err == nil && o.Outbox != nil {
		// The same files reached the same peer: an earlier interrupted send
		// of them has nothing left to resume.
		o.Outbox.Supersede(mh, o.Target.FP, o.Target.Addr)
	}
	if errors.Is(err, transfer.ErrInterrupted) && o.Outbox != nil && !l.Transfers.Cancelled(o.ID) {
		rec := resume.Outgoing{
			ID: o.ID, Name: name, PeerName: o.PeerName, PeerFP: o.Target.FP, Addr: o.Target.Addr,
			Files: transfer.Files(o.Manifest), Bytes: o.Manifest.Total, ManifestHash: mh, Paths: o.Paths,
			ReceiverFP: receiverFP, RemoteID: remoteID, Error: err.Error(),
		}
		if perr := o.Outbox.Put(rec); perr != nil {
			l.Error(o.ID, name, fmt.Errorf("could not keep the send for a resume: %w", perr))
		}
	}
	if _, err = l.End(o.ID, err); err != nil {
		return err
	}
	l.Publish(Event{Type: "sent", Data: map[string]any{
		"transfer_id": o.ID, "name": name, "to": o.Target.Addr, "files": transfer.Files(o.Manifest), "bytes": o.Manifest.Total,
	}})
	return nil
}

// DiscardOutgoing forgets interrupted send rec (the person's Discard) and
// tells its receiver, in the background and on a best-effort basis, to
// drop what it kept.
func DiscardOutgoing(b *resume.Outbox, id *node.Identity, rec resume.Outgoing) error {
	if err := b.Remove(rec.ID); err != nil {
		return err
	}
	if rec.RemoteID != "" && rec.ReceiverFP != "" {
		go func() { _ = transfer.NotifyCancel(context.Background(), id, rec.Addr, rec.ReceiverFP, rec.RemoteID) }()
	}
	return nil
}

// RecordOffer writes the history entry of an incoming offer that never
// became a transfer.
func (l *Ledger) RecordOffer(v IncomingOffer, res history.Result, msg string) {
	top, _, _ := strings.Cut(v.Name, "/")
	now := time.Now()
	l.AddHistory(history.Entry{
		ID: v.TransferID, Dir: history.DirIn, Name: top, PeerName: v.SenderName, PeerFP: v.SenderFP,
		Files: v.Files, Bytes: v.Size, Started: now, Ended: now, Result: res, Error: msg,
		Encrypted: !v.FromBrowser,
	})
}
