package transfer

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/donaldsteele/rocket/internal/protocol"
	"github.com/donaldsteele/rocket/internal/protocol/securestream"
)

// The protocol v2 body (docs/protocol.md). Its plaintext is a 16-byte start
// header (entry uint32 BE, offset uint64 BE, 4 zero bytes), then the bytes
// of every file of the manifest in manifest order, beginning at byte offset
// of entry entry; folders contribute nothing. The signed manifest fixes the
// names, order and sizes, so the body needs no framing of its own. The
// plaintext travels as a securestream.

// StartHeaderSize is the length of the start header.
const StartHeaderSize = 16

// Start is where a body begins: byte Offset of manifest entry Entry. The
// zero Start is the beginning of the transfer.
type Start struct {
	Entry  uint32
	Offset uint64
}

// ErrStart is wrapped when a body begins at a point the receiver did not
// offer.
var ErrStart = errors.New("body start point was not offered")

// Header is the start header for s.
func (s Start) Header() [StartHeaderSize]byte {
	var h [StartHeaderSize]byte
	binary.BigEndian.PutUint32(h[0:4], s.Entry)
	binary.BigEndian.PutUint64(h[4:12], s.Offset)
	return h
}

// ReadStart reads a start header. Its last 4 bytes must be zero.
func ReadStart(r io.Reader) (Start, error) {
	var h [StartHeaderSize]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return Start{}, fmt.Errorf("%w: the body ended inside its start header", ErrSize)
		}
		return Start{}, err
	}
	if h[12]|h[13]|h[14]|h[15] != 0 {
		return Start{}, fmt.Errorf("%w: bad start header", ErrStart)
	}
	return Start{Entry: binary.BigEndian.Uint32(h[0:4]), Offset: binary.BigEndian.Uint64(h[4:12])}, nil
}

// CheckStart reports whether a body may begin at s: at the beginning, or
// exactly at the resume point the receiver offered (nil when it offered
// none).
func CheckStart(s Start, offered *protocol.ResumePoint) error {
	if s == (Start{}) {
		return nil
	}
	if offered != nil && s.Entry == offered.Entry && s.Offset == offered.Offset {
		return nil
	}
	return fmt.Errorf("%w: entry %d offset %d", ErrStart, s.Entry, s.Offset)
}

// BodySize is the plaintext length of the body of m that begins at s
// (header included). s must be a valid point of m.
func BodySize(m protocol.Manifest, s Start) int64 {
	n := int64(StartHeaderSize)
	for i, e := range m.Entries {
		switch {
		case uint32(i) < s.Entry || e.Mode&protocol.ModeDir != 0:
		case uint32(i) == s.Entry:
			n += e.Size - int64(s.Offset)
		default:
			n += e.Size
		}
	}
	return n
}

// SealedBodySize is the length on the wire of the body of m that begins at
// s: BodySize as a securestream.
func SealedBodySize(m protocol.Manifest, s Start) int64 {
	return securestream.SealedSize(BodySize(m, s))
}

// validStart reports whether s points into m: a file entry and an offset
// within its size (an offset equal to the size is allowed, the file then
// contributes nothing more).
func validStart(m protocol.Manifest, s Start) bool {
	if s == (Start{}) {
		return true
	}
	if int64(s.Entry) >= int64(len(m.Entries)) {
		return false
	}
	e := m.Entries[s.Entry]
	return e.Mode&protocol.ModeDir == 0 && s.Offset <= uint64(e.Size)
}

// writeBody writes the plaintext body of m beginning at s to w: the start
// header, then the content of every file from there on, read with open.
// Exactly e.Size bytes are taken from each file; a file that turns out
// shorter is an ErrSize.
func writeBody(w io.Writer, m protocol.Manifest, s Start, open OpenFunc, tr *tracker) error {
	if !validStart(m, s) {
		return fmt.Errorf("%w: entry %d offset %d", ErrStart, s.Entry, s.Offset)
	}
	h := s.Header()
	if _, err := w.Write(h[:]); err != nil {
		return err
	}
	for i, e := range m.Entries {
		if e.Mode&protocol.ModeDir != 0 || uint32(i) < s.Entry {
			continue
		}
		skip := int64(0)
		if uint32(i) == s.Entry {
			skip = int64(s.Offset)
		}
		if err := writeEntry(w, i, e, skip, open, tr); err != nil {
			return err
		}
	}
	return nil
}

// writeEntry writes bytes [skip, e.Size) of file entry i.
func writeEntry(w io.Writer, i int, e protocol.Entry, skip int64, open OpenFunc, tr *tracker) error {
	rc, err := open(i, e)
	if err != nil {
		return err
	}
	defer rc.Close()
	tr.file(e.Path)
	if skip > 0 {
		if sk, ok := rc.(io.Seeker); ok {
			_, err = sk.Seek(skip, io.SeekStart)
		} else {
			_, err = io.CopyN(io.Discard, rc, skip)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", e.Path, err)
		}
	}
	want := e.Size - skip
	n, err := io.CopyN(trackWriter{w: w, t: tr}, rc, want)
	if errors.Is(err, io.EOF) {
		err = fmt.Errorf("%s: %w: it shrank while sending (%d of %d bytes)", e.Path, ErrSize, skip+n, e.Size)
	}
	return err
}

// ReceiveBody stores a decrypted protocol v2 body through x, which was
// prepared for m: the start header must be the beginning of the transfer
// (resuming at another point is not supported by this receive path), then
// every file's bytes in manifest order, and nothing after them. src must
// return io.EOF only at an authenticated end (a securestream.Reader does).
// Any failure aborts x, which removes everything stored. m must already be
// validated.
func ReceiveBody(x EntryWriter, m protocol.Manifest, src io.Reader) (Result, error) {
	fail := func(err error) (Result, error) {
		x.Abort()
		return Result{}, err
	}
	s, err := ReadStart(src)
	if err != nil {
		return fail(err)
	}
	if err := CheckStart(s, nil); err != nil {
		return fail(err)
	}
	for i, e := range m.Entries {
		var r io.Reader = strings.NewReader("")
		if e.Mode&protocol.ModeDir == 0 {
			r = io.LimitReader(src, e.Size)
		}
		if err := x.Write(i, r); err != nil {
			return fail(err)
		}
	}
	var one [1]byte
	n, err := io.ReadFull(src, one[:])
	switch {
	case n > 0:
		return fail(fmt.Errorf("%w: more data than offered", ErrSize))
	case err != io.EOF:
		return fail(err)
	}
	return x.Finish()
}
