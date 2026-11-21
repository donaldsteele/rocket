package rocketcore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/donaldsteele/rocket/internal/discovery"
	"github.com/donaldsteele/rocket/internal/history"
	"github.com/donaldsteele/rocket/internal/httpapi"
	"github.com/donaldsteele/rocket/internal/protocol"
	"github.com/donaldsteele/rocket/internal/receiver"
	"github.com/donaldsteele/rocket/internal/resume"
)

// Decider asks the person about an incoming offer.
type Decider interface {
	// Ask shows an offer ({id, transfer_id, kind, name, size, files, names,
	// sender_name, sender_fp, addr}; names as they will be saved). It must
	// return at once; answer later with Node.Answer(id, ...). If no answer
	// comes within two minutes, or the sender withdraws, an "offer_gone"
	// event {transfer_id, reason} tells the app to drop the prompt.
	Ask(offerJSON string)
}

// EventListener hears the receiver's events as JSON (offer_gone, progress,
// done, error, cancelled; see receiver.Event).
type EventListener interface {
	OnEvent(eventJSON string)
}

// Sink stores accepted transfers (on Android: Download/rocket through
// MediaStore). A transfer broken by a lost connection is kept
// (SinkTransfer.Keep) and continued by the sender's next attempt (Resume),
// or removed (Discard) when it is not continued within 24 hours.
type Sink interface {
	// Begin starts storing transfer transferID; manifestJSON is its checked
	// manifest ({kind, entries: [{path, size, mode}], total}).
	Begin(transferID, manifestJSON string) (SinkTransfer, error)
	// Resume continues storing a kept transfer as transferID; state is
	// what SinkTransfer.Keep returned. The SinkTransfer returned owns
	// everything the kept one stored: its Abort removes that too.
	Resume(transferID, manifestJSON, state string) (SinkTransfer, error)
	// Check fails when something state describes is gone (the person
	// deleted a file, say).
	Check(state string) error
	// Discard removes everything state describes.
	Discard(state string)
}

// SinkTransfer stores one transfer's folders and files, in order. Paths are
// relative, "/"-separated and already sanitized.
type SinkTransfer interface {
	// Mkdir creates folder rel and any missing parents, and returns the
	// relative path made. A top-level folder (no "/") must be fresh: when
	// the name is taken, make another ("photos (1)") and return it; later
	// paths under it use the returned name.
	Mkdir(rel string) (string, error)
	// Create starts file rel of exactly size bytes, making its folders as
	// needed. Never replace an existing file: pick a fresh name when taken.
	Create(rel string, size int64) (SinkFile, error)
	// Abort removes everything this transfer stored, finished files too.
	Abort()
	// Location says where the transfer went, for display.
	Location() string
	// Keep ends a transfer whose connection was lost, leaving everything
	// it stored in place (finished files, the incomplete file and its
	// folders), and returns a description of it for Sink.Resume, Check and
	// Discard. Go has closed its descriptors first.
	Keep() (string, error)
	// Reopen opens incomplete file ref (SinkFile.Ref, of this transfer or
	// the kept one it continues) to continue it. Its Fd must be readable
	// and writable ("rw"); Go cuts the file to where it continues.
	Reopen(ref string) (SinkFile, error)
}

// SinkFile is one file being stored.
type SinkFile interface {
	// Fd returns a detached, writable file descriptor that Go now owns and
	// closes before Commit or Abort, or -1 to receive the bytes through
	// Write instead. Called once.
	Fd() int
	// Write appends data (used only when Fd returned -1).
	Write(data []byte) error
	// Commit makes the complete file visible (MediaStore: IS_PENDING 0) and
	// returns its location (a content URI).
	Commit() (string, error)
	// Abort deletes the incomplete file.
	Abort()
	// Ref names the file while it is incomplete (a content URI), for
	// SinkTransfer.Reopen.
	Ref() string
}

type receiving struct {
	rx     *receiver.Receiver
	hs     *http.Server
	port   int
	cancel context.CancelFunc // stops the beacon announce
}

// StartReceiver serves the peer protocol (GET /api/hello, POST /api/offer,
// POST /api/recv, POST /api/recv/cancel; nothing else) on TCP port, or on a
// free port when port is
// taken or 0, and announces this host on the beacon. It returns the port it
// listens on, which NSD must advertise. Offers go to d, accepted transfers
// to s, and the receiver's events to l, until StopReceiver.
func (n *Node) StartReceiver(port int, d Decider, s Sink, l EventListener) (int, error) {
	if d == nil || s == nil || l == nil {
		return 0, errors.New("the receiver needs a decider, a sink and a listener")
	}
	if port < 0 || port > 65535 {
		return 0, fmt.Errorf("bad port %d", port)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.recv != nil {
		return 0, errors.New("the receiver is already running")
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil && port != 0 {
		ln, err = net.Listen("tcp", ":0")
	}
	if err != nil {
		return 0, err
	}
	bound := ln.Addr().(*net.TCPAddr).Port
	sink := sinkAdapter(s)
	kept, err := receiver.OpenKept(n.dataDir, sink)
	if err != nil {
		ln.Close()
		return 0, err
	}
	id := n.identity()
	rx := receiver.New(receiver.Config{
		Identity:   id,
		Trust:      n.trust,
		Ledger:     n.ledger,
		Decider:    receiver.DeciderFunc(func(o receiver.IncomingOffer) { d.Ask(toJSON(o)) }),
		Sink:       sink,
		Hello:      func() (string, string) { return n.version, OS },
		AutoAccept: n.AutoAccept,
		Kept:       kept,
		Pairing:    n.pairing,
	})
	hs := &http.Server{
		Handler:           rx.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}
	go func() { _ = hs.Serve(httpapi.LimitListener(ln, httpapi.MaxConnsPerIP)) }()
	ctx, cancel := context.WithCancel(context.Background())
	if n.beaconPort > 0 {
		go discovery.Announce(ctx, n.beaconPort, discovery.Self{
			Name: id.Name, Fingerprint: id.Fingerprint(), HTTPPort: bound, Version: n.version, OS: OS,
		})
	}
	// Kept transfers whose 24 hours are up go now and then every hour (in
	// the background: a sweep reports through dispatch, which takes n.mu).
	go sweepKept(ctx, rx)
	n.recv = &receiving{rx: rx, hs: hs, port: bound, cancel: cancel}
	n.kept = kept
	n.events = l
	return bound, nil
}

// sweepKept sweeps rx's expired kept transfers now and every
// resume.SweepEvery until ctx ends.
func sweepKept(ctx context.Context, rx *receiver.Receiver) {
	rx.Sweep()
	tick := time.NewTicker(resume.SweepEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			rx.Sweep()
		}
	}
}

// StopReceiver declines the offers waiting for a decision, cancels the
// receives in flight (each removes what it stored and tells the sender),
// stops announcing and closes the port.
func (n *Node) StopReceiver() {
	n.mu.Lock()
	r := n.recv
	n.recv = nil
	n.mu.Unlock()
	if r == nil {
		return
	}
	n.pairing.Stop() // nothing to pair while not receiving
	r.cancel()
	for _, o := range r.rx.PendingOffers() {
		r.rx.Answer(o.ID, false, false)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		in := 0
		for _, t := range n.ledger.Transfers.List() {
			if t.Dir == history.DirIn {
				n.ledger.Transfers.Cancel(t.ID)
				in++
			}
		}
		if in == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Give the declined offers a moment to answer their senders.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	_ = r.hs.Shutdown(ctx)
	cancel()
	_ = r.hs.Close()
	n.mu.Lock()
	if n.recv == nil {
		n.events = nil
	}
	n.mu.Unlock()
}

// DefaultPairingSeconds is the pairing window the app opens.
const DefaultPairingSeconds = int(receiver.DefaultPairing / time.Second)

// StartPairing opens the pairing window for seconds (at most an hour;
// 0 closes it) and returns its state as JSON {open, until, seconds}.
// While it is open, an offer from a rocket sender not yet trusted is
// accepted without asking and its key trusted from then on, reported to
// the EventListener as a "paired" event {fingerprint, name, addr}; the
// window opening and closing (also by running out) is a "pairing" event
// with the state. The receiver must be running; StopReceiver closes it.
func (n *Node) StartPairing(seconds int) (string, error) {
	if seconds < 0 {
		return "", errors.New("seconds cannot be negative")
	}
	if seconds > 0 && n.ReceiverPort() == 0 {
		return "", errors.New("turn on Receive files first")
	}
	n.pairing.Start(time.Duration(seconds) * time.Second)
	return n.PairingJSON(), nil
}

// StopPairing closes the pairing window.
func (n *Node) StopPairing() { n.pairing.Stop() }

// PairingJSON is the pairing window's state: {open, until, seconds}.
func (n *Node) PairingJSON() string { return toJSON(n.pairing.State()) }

// ReceiverPort is the port the receiver listens on, or 0 when it is off.
func (n *Node) ReceiverPort() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.recv == nil {
		return 0
	}
	return n.recv.port
}

// Answer decides incoming offer offerID: accept or decline, and with trust
// also pin the sender so its next offers are accepted at once. It reports
// false when the offer no longer waits.
func (n *Node) Answer(offerID string, accept, trust bool) bool {
	n.mu.Lock()
	r := n.recv
	n.mu.Unlock()
	return r != nil && r.rx.Answer(offerID, accept, trust)
}

// IncomingJSON lists the offers waiting for a decision.
func (n *Node) IncomingJSON() string {
	n.mu.Lock()
	r := n.recv
	n.mu.Unlock()
	if r == nil {
		return "[]"
	}
	return toJSON(r.rx.PendingOffers())
}

// sinkAdapter stores transfers through a Kotlin Sink, keeping a transfer
// broken by a lost connection (receiver.NewFilesSink).
func sinkAdapter(s Sink) receiver.ResumableSink { return receiver.NewFilesSink(sinkStore{s}) }

// sinkStore is a receiver.FilesStore over a Kotlin Sink. The Kotlin side's
// state string travels inside the receiver's kept record as a JSON string.
type sinkStore struct{ s Sink }

func (k sinkStore) Begin(id string, m protocol.Manifest) (receiver.KeepFiles, error) {
	t, err := k.s.Begin(id, toJSON(m))
	return sinkFilesOf(t, err)
}

func (k sinkStore) Resume(id string, m protocol.Manifest, state json.RawMessage) (receiver.KeepFiles, error) {
	var st string
	if err := json.Unmarshal(state, &st); err != nil {
		return nil, err
	}
	t, err := k.s.Resume(id, toJSON(m), st)
	return sinkFilesOf(t, err)
}

func (k sinkStore) Check(state json.RawMessage) error {
	var st string
	if err := json.Unmarshal(state, &st); err != nil {
		return err
	}
	return k.s.Check(st)
}

func (k sinkStore) Discard(state json.RawMessage) {
	var st string
	if json.Unmarshal(state, &st) == nil {
		k.s.Discard(st)
	}
}

func sinkFilesOf(t SinkTransfer, err error) (receiver.KeepFiles, error) {
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, errors.New("the sink returned no transfer")
	}
	return sinkFiles{t}, nil
}

type sinkFiles struct{ t SinkTransfer }

func (f sinkFiles) Mkdir(rel string) (string, error) { return f.t.Mkdir(rel) }

func (f sinkFiles) Create(rel string, size int64, _ uint32) (receiver.File, error) {
	sf, err := f.t.Create(rel, size)
	if err != nil {
		return nil, err
	}
	if sf == nil {
		return nil, errors.New("the sink returned no file")
	}
	out := &sinkFile{f: sf}
	if fd := sf.Fd(); fd >= 0 {
		out.w = os.NewFile(uintptr(fd), rel)
	}
	return out, nil
}

func (f sinkFiles) Abort() { f.t.Abort() }

func (f sinkFiles) Location() string { return f.t.Location() }

func (f sinkFiles) Keep() (json.RawMessage, error) {
	st, err := f.t.Keep()
	if err != nil {
		return nil, err
	}
	return json.Marshal(st)
}

// Reopen opens the kept incomplete file ref through its descriptor, cut
// to offset and positioned there. A file the sink cannot hand over as a
// descriptor cannot be continued.
func (f sinkFiles) Reopen(ref string, offset int64) (receiver.KeptFile, error) {
	sf, err := f.t.Reopen(ref)
	if err != nil {
		return nil, err
	}
	if sf == nil {
		return nil, errors.New("the sink returned no file")
	}
	fd := sf.Fd()
	if fd < 0 {
		return nil, fmt.Errorf("the kept part of %s cannot be continued", ref)
	}
	w := os.NewFile(uintptr(fd), ref)
	fail := func(err error) (receiver.KeptFile, error) {
		w.Close()
		return nil, err
	}
	st, err := w.Stat()
	if err != nil {
		return fail(err)
	}
	if st.Size() < offset {
		return fail(fmt.Errorf("the kept part of %s is shorter than recorded", ref))
	}
	if err := w.Truncate(offset); err != nil {
		return fail(err)
	}
	if _, err := w.Seek(offset, io.SeekStart); err != nil {
		return fail(err)
	}
	return &sinkFile{f: sf, w: w}, nil
}

// sinkFile writes to the descriptor the sink gave, or through its Write.
type sinkFile struct {
	f SinkFile
	w *os.File
}

func (s *sinkFile) Write(p []byte) (int, error) {
	if s.w != nil {
		return s.w.Write(p)
	}
	if err := s.f.Write(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (s *sinkFile) Commit() (string, error) {
	if s.w != nil {
		err := s.w.Close()
		s.w = nil
		if err != nil {
			s.f.Abort()
			return "", err
		}
	}
	return s.f.Commit()
}

func (s *sinkFile) Abort() {
	if s.w != nil {
		s.w.Close()
		s.w = nil
	}
	s.f.Abort()
}

func (s *sinkFile) Ref() string { return s.f.Ref() }

// Close closes Go's descriptor and leaves the incomplete file as it is
// (the SinkTransfer's Keep closes the Kotlin side).
func (s *sinkFile) Close() error {
	if s.w == nil {
		return nil
	}
	err := s.w.Close()
	s.w = nil
	return err
}
