package rocketcore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/donaldsteele/rocket/internal/discovery"
	"github.com/donaldsteele/rocket/internal/history"
	"github.com/donaldsteele/rocket/internal/protocol"
	"github.com/donaldsteele/rocket/internal/receiver"
	"github.com/donaldsteele/rocket/internal/resume"
	"github.com/donaldsteele/rocket/internal/transfer"
)

// Source is what to send: files and folders picked through the Storage
// Access Framework, each named by a relative path ("photos/2024/a.jpg";
// a file picked alone is a bare name).
type Source interface {
	// Count is the number of items.
	Count() int
	// Path is item i's relative path, "/"-separated.
	Path(i int) string
	// Size is file i's size in bytes, or -1 when unknown (Go then reads
	// it from the descriptor). Ignored for a folder.
	Size(i int) int64
	// IsDir reports whether item i is a folder (it may be empty; a file's
	// folders need not be listed).
	IsDir(i int) bool
	// OpenFd opens file i for reading and returns a detached file
	// descriptor that Go now owns and closes, or -1 when the provider has
	// none (Go then calls ReadChunk).
	OpenFd(i int) (int, error)
	// ReadChunk returns up to max bytes of file i from offset; an empty
	// result means the end of the file.
	ReadChunk(i int, offset int64, max int) ([]byte, error)
}

// ProgressListener hears the events of one send, as JSON: progress, then
// sent, error or cancelled (see receiver.Event).
type ProgressListener interface {
	OnEvent(eventJSON string)
}

// chunkSize is what a ReadChunk asks for.
const chunkSize = 256 << 10

// Send offers src to the peer at addr (host:port) whose fingerprint must be
// fp ("" accepts whoever answers there), or, with addr "", to the
// discovered peer with fingerprint fp. It builds and checks the manifest,
// then runs the transfer in the background and returns its ID; l gets the
// transfer's events. Cancel(id) stops it.
func (n *Node) Send(addr, fp string, src Source, l ProgressListener) (string, error) {
	return n.SendNamed(addr, fp, "", src, l)
}

// SendNamed is Send with the name the app shows for the peer (shown), which
// the history records. The peer list can go stale while the person picks files
// (discovery pauses and sightings expire), so the app passes the name it
// showed; "" falls back to the discovered name, then to the address.
func (n *Node) SendNamed(addr, fp, shown string, src Source, l ProgressListener) (string, error) {
	if src == nil || l == nil {
		return "", errors.New("nothing to send or no listener")
	}
	if fp != "" && !discovery.IsFingerprint(fp) {
		return "", fmt.Errorf("%q is not a full fingerprint", fp)
	}
	target := transfer.Target{Addr: addr, FP: fp}
	peerName := addr
	if p, ok := n.registry().Find(fp); ok {
		peerName = p.Name
		if target.Addr == "" {
			target.Addr = p.Addr
		}
	}
	if s := receiver.CleanLabel(shown, ""); s != "" {
		peerName = s
	}
	if !transfer.IsHostPort(target.Addr) {
		if addr == "" {
			return "", fmt.Errorf("peer %s is not on the network", fp)
		}
		return "", fmt.Errorf("%q is not a host:port address", addr)
	}
	return n.startSend(target, peerName, src, l, nil)
}

// startSend builds the manifest of src and sends it to target in the
// background; l gets the transfer's events. After a network failure the
// send retries and resumes; when it gives up it is kept for Resume. rec,
// when not nil, is the outbox record it resumes, claimed by the caller.
func (n *Node) startSend(target transfer.Target, peerName string, src Source, l ProgressListener, rec *resume.Outgoing) (string, error) {
	items := make([]transfer.RelItem, src.Count())
	for i := range items {
		it := transfer.RelItem{Path: src.Path(i), Dir: src.IsDir(i)}
		if !it.Dir {
			size, err := sourceSize(src, i)
			if err != nil {
				return "", err
			}
			it.Size = size
		}
		items[i] = it
	}
	m, index, err := transfer.RelManifest(items)
	if err != nil {
		return "", err
	}
	open := func(i int, e protocol.Entry) (io.ReadCloser, error) {
		k := index[i]
		fd, err := src.OpenFd(k)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Path, err)
		}
		if fd >= 0 {
			return os.NewFile(uintptr(fd), e.Path), nil
		}
		return &chunkReader{src: src, i: k}, nil
	}

	tid := receiver.NewToken()
	name := transfer.Label(m)
	n.mu.Lock()
	n.sends[tid] = l
	delays := n.retryDelays
	n.mu.Unlock()
	ctx := n.ledger.Transfers.Start(context.Background(), transfer.Info{
		ID: tid, Dir: history.DirOut, Name: name, PeerName: peerName, PeerFP: target.FP,
		Files: transfer.Files(m), Total: m.Total, State: transfer.StateWaiting, Encrypted: true,
	}, nil)
	id := n.identity()
	o := receiver.Outgoing{
		ID: tid, PeerName: peerName, Target: target, Manifest: m, Open: open,
		Resumable: true, Delays: delays, Outbox: n.outbox,
	}
	if rec != nil {
		o.Resumes, o.Pin = rec.ID, rec.ReceiverFP
	}
	go func() {
		_ = n.ledger.Send(ctx, id, o)
		n.mu.Lock()
		delete(n.sends, tid)
		n.mu.Unlock()
	}()
	return tid, nil
}

// Resume sends interrupted send id again, to the same peer (looked up by
// fingerprint first, as its address may have changed), with src: the same
// files, which the app opens again (from the content URIs it kept, or
// picked again). When the files are unchanged, the receiver continues
// where it stopped; otherwise they go again from the start. A send to a
// bare address must reach the receiver that accepted it before. It returns
// the new transfer's ID; l gets its events. The interrupted send is left
// out of ResumableJSON while the new one runs, and is forgotten once the
// receiver accepts the new one: if the new send ends before that (the peer
// is away), it can still be resumed or discarded.
func (n *Node) Resume(id string, src Source, l ProgressListener) (string, error) {
	if src == nil || l == nil {
		return "", errors.New("nothing to send or no listener")
	}
	rec, ok := n.outbox.Claim(id)
	if !ok {
		return "", errors.New("nothing to resume with that id")
	}
	target := transfer.Target{Addr: rec.Addr, FP: rec.PeerFP}
	if p, found := n.registry().Find(rec.PeerFP); rec.PeerFP != "" && found {
		target.Addr = p.Addr
	}
	tid, err := n.startSend(target, rec.PeerName, src, l, &rec)
	if err != nil {
		n.outbox.Release(id)
		return "", err
	}
	return tid, nil
}

// sourceSize is file i's size: as the Source says, or, when it does not
// know, from a stat of its descriptor.
func sourceSize(src Source, i int) (int64, error) {
	if size := src.Size(i); size >= 0 {
		return size, nil
	}
	fd, err := src.OpenFd(i)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", src.Path(i), err)
	}
	if fd < 0 {
		return 0, fmt.Errorf("the size of %s is unknown, so it cannot be sent", src.Path(i))
	}
	f := os.NewFile(uintptr(fd), src.Path(i))
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return 0, fmt.Errorf("the size of %s is unknown, so it cannot be sent", src.Path(i))
	}
	return st.Size(), nil
}

// chunkReader reads file i of a Source with ReadChunk.
type chunkReader struct {
	src Source
	i   int
	off int64
	buf []byte
	eof bool
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if len(c.buf) == 0 {
		if c.eof {
			return 0, io.EOF
		}
		b, err := c.src.ReadChunk(c.i, c.off, chunkSize)
		if err != nil {
			return 0, err
		}
		if len(b) == 0 {
			c.eof = true
			return 0, io.EOF
		}
		c.buf = b
	}
	k := copy(p, c.buf)
	c.buf = c.buf[k:]
	c.off += int64(k)
	return k, nil
}

func (c *chunkReader) Close() error { return nil }
