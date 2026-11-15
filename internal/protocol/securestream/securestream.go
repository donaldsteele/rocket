// Package securestream is rocket's transfer encryption (protocol v2,
// docs/protocol.md): the body key agreed from two ephemeral X25519 keys
// (DeriveKey), and an authenticated, framed ChaCha20-Poly1305 stream that
// carries the body (NewWriter, NewReader).
//
// A stream is a sequence of frames. Each frame is a 4-byte big-endian
// ciphertext length followed by that many bytes of ChaCha20-Poly1305
// ciphertext (the sealed plaintext plus its 16-byte tag). A frame holds at
// most MaxPlain bytes of plaintext. The nonce of frame n (counting from 0) is
//
//	uint64 BE n ‖ 3 zero bytes ‖ last
//
// where last is 1 on the final frame and 0 on every other. The final frame
// may be empty; no other frame is. Because the frame counter and the final
// flag are inside the nonce, frames cannot be reordered, dropped, replayed
// or moved past the end without failing authentication, and a stream cut
// short at a frame boundary is caught because its final frame never comes.
//
// A key must protect one stream only: rocket derives a fresh key for every
// transfer from fresh ephemeral keys, the offer and the transfer ID.
package securestream

import (
	"bytes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"

	"golang.org/x/crypto/chacha20poly1305"
)

// Sizes of the stream format.
const (
	KeySize    = chacha20poly1305.KeySize // 32
	MaxPlain   = 64 << 10                 // plaintext bytes per frame
	Overhead   = chacha20poly1305.Overhead
	MaxCipher  = MaxPlain + Overhead // largest ciphertext length a frame may announce
	headerSize = 4                   // the ciphertext length prefix
)

// KeyInfoPrefix starts the HKDF info string of a body key.
const KeyInfoPrefix = "rocket-v2 body\n"

// Stream errors. A reader returns one of these (possibly wrapped) and then
// keeps returning it.
var (
	// ErrAuth: a frame failed authentication (wrong key, tampered, reordered,
	// replayed or misplaced frame).
	ErrAuth = errors.New("securestream: frame failed authentication")
	// ErrTruncated: the stream ended before its final frame. It also matches
	// io.ErrUnexpectedEOF.
	ErrTruncated = errors.New("securestream: stream ended before its final frame")
	// ErrTrailingData: bytes follow the final frame.
	ErrTrailingData = errors.New("securestream: data after the final frame")
	// ErrFrameSize: a frame announces more than MaxCipher bytes, fewer than
	// a tag, or is an empty frame that is not the final one.
	ErrFrameSize = errors.New("securestream: bad frame size")
	// ErrCounter: the frame counter would wrap.
	ErrCounter = errors.New("securestream: frame counter exhausted")
	// ErrClosed: Write after Close.
	ErrClosed = errors.New("securestream: write after close")
	// ErrSharedSecret: the X25519 result is all zero (a low-order peer key).
	ErrSharedSecret = errors.New("securestream: bad shared secret")
)

// DeriveKey computes the body key of one transfer:
//
//	shared = X25519(priv, peerPub)                     (all zero is refused)
//	key    = HKDF-SHA256(ikm = shared,
//	                     salt = sha256(offerSigningBytes),
//	                     info = "rocket-v2 body\n" + senderFP + "\n" + receiverFP + "\n" + transferID)
//
// 32 bytes. The sender calls it with its offer key and the receiver's reply
// key, the receiver with its reply key and the offer key; both get the same
// key.
func DeriveKey(priv *ecdh.PrivateKey, peerPub []byte, offerSigningBytes []byte, senderFP, receiverFP, transferID string) ([]byte, error) {
	if priv == nil || priv.Curve() != ecdh.X25519() {
		return nil, errors.New("securestream: need an X25519 private key")
	}
	pub, err := ecdh.X25519().NewPublicKey(peerPub)
	if err != nil {
		return nil, fmt.Errorf("securestream: bad peer key: %w", err)
	}
	shared, err := priv.ECDH(pub)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSharedSecret, err)
	}
	if allZero(shared) {
		return nil, ErrSharedSecret
	}
	salt := sha256.Sum256(offerSigningBytes)
	return hkdf.Key(sha256.New, shared, salt[:], KeyInfo(senderFP, receiverFP, transferID), KeySize)
}

// KeyInfo is the HKDF info string of a body key.
func KeyInfo(senderFP, receiverFP, transferID string) string {
	return KeyInfoPrefix + senderFP + "\n" + receiverFP + "\n" + transferID
}

func allZero(b []byte) bool {
	var acc byte
	for _, c := range b {
		acc |= c
	}
	return acc == 0
}

// nonce returns the nonce of frame n.
func nonce(n uint64, last bool) [chacha20poly1305.NonceSize]byte {
	var iv [chacha20poly1305.NonceSize]byte
	binary.BigEndian.PutUint64(iv[:8], n)
	if last {
		iv[11] = 1
	}
	return iv
}

// SealedSize is the exact length of the stream NewWriter produces for plain
// bytes of plaintext: every full frame, then a final frame with the rest
// (which is empty only when plain is 0).
func SealedSize(plain int64) int64 {
	if plain < 0 {
		return 0
	}
	frames := (plain + MaxPlain - 1) / MaxPlain
	if frames == 0 {
		frames = 1
	}
	return plain + frames*(headerSize+Overhead)
}

// Writer encrypts a stream. Data is cut into frames of MaxPlain bytes;
// Close seals the rest as the final frame. Close must be called, or the
// reader reports the stream as truncated. Not safe for concurrent use.
type Writer struct {
	w      io.Writer
	aead   cipher.AEAD
	buf    []byte // plaintext waiting for its frame, at most MaxPlain
	out    []byte // header + ciphertext scratch
	n      uint64 // counter of the next frame
	err    error
	closed bool
}

// NewWriter returns a Writer that writes the stream to w under key (KeySize
// bytes).
func NewWriter(w io.Writer, key []byte) (*Writer, error) {
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, fmt.Errorf("securestream: %w", err)
	}
	return &Writer{w: w, aead: aead, buf: make([]byte, 0, MaxPlain), out: make([]byte, headerSize, headerSize+MaxCipher)}, nil
}

// Write buffers p, writing a frame each time MaxPlain bytes are waiting and
// more follow (the last bytes always go out with Close).
func (sw *Writer) Write(p []byte) (int, error) {
	if sw.err != nil {
		return 0, sw.err
	}
	if sw.closed {
		return 0, ErrClosed
	}
	done := 0
	for len(p) > 0 {
		if len(sw.buf) == MaxPlain {
			if err := sw.seal(false); err != nil {
				return done, err
			}
		}
		k := copy(sw.buf[len(sw.buf):MaxPlain], p)
		sw.buf = sw.buf[:len(sw.buf)+k]
		p = p[k:]
		done += k
	}
	return done, nil
}

// Close writes the buffered plaintext as the final frame. It does not close
// the underlying writer.
func (sw *Writer) Close() error {
	if sw.err != nil {
		return sw.err
	}
	if sw.closed {
		return nil
	}
	sw.closed = true
	return sw.seal(true)
}

// seal writes the buffered plaintext as frame sw.n.
func (sw *Writer) seal(last bool) error {
	if sw.n == math.MaxUint64 {
		sw.err = ErrCounter
		return sw.err
	}
	iv := nonce(sw.n, last)
	sw.out = sw.aead.Seal(sw.out[:headerSize], iv[:], sw.buf, nil)
	binary.BigEndian.PutUint32(sw.out[:headerSize], uint32(len(sw.out)-headerSize))
	if _, err := sw.w.Write(sw.out); err != nil {
		sw.err = err
		return err
	}
	sw.n++
	sw.buf = sw.buf[:0]
	return nil
}

// Reader decrypts a stream. It hands out plaintext only from frames that
// passed authentication, and returns io.EOF only after the final frame has
// been authenticated and the underlying reader is at its end. Not safe for
// concurrent use.
type Reader struct {
	r     io.Reader
	aead  cipher.AEAD
	hdr   [headerSize]byte
	frame []byte // ciphertext scratch
	pbuf  []byte // plaintext scratch (never the ciphertext: a failed Open wipes its output)
	plain []byte // decrypted bytes not yet handed out
	n     uint64 // counter of the next frame
	final bool   // the final frame has been read
	err   error  // sticky
}

// NewReader returns a Reader that decrypts the stream read from r under key.
func NewReader(r io.Reader, key []byte) (*Reader, error) {
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, fmt.Errorf("securestream: %w", err)
	}
	return &Reader{r: r, aead: aead, frame: make([]byte, MaxCipher), pbuf: make([]byte, MaxPlain)}, nil
}

// Read implements io.Reader.
func (sr *Reader) Read(p []byte) (int, error) {
	for len(sr.plain) == 0 {
		if sr.err != nil {
			return 0, sr.err
		}
		if sr.final {
			return 0, io.EOF
		}
		if err := sr.next(); err != nil {
			sr.err = err
			return 0, err
		}
	}
	k := copy(p, sr.plain)
	sr.plain = sr.plain[k:]
	return k, nil
}

// next reads, authenticates and decrypts one frame into sr.plain.
func (sr *Reader) next() error {
	if _, err := io.ReadFull(sr.r, sr.hdr[:]); err != nil {
		return truncated(err)
	}
	size := binary.BigEndian.Uint32(sr.hdr[:])
	if size < Overhead || size > MaxCipher {
		return fmt.Errorf("%w: frame %d announces %d bytes", ErrFrameSize, sr.n, size)
	}
	if sr.n == math.MaxUint64 {
		return ErrCounter
	}
	ct := sr.frame[:size]
	if _, err := io.ReadFull(sr.r, ct); err != nil {
		return truncated(err)
	}
	// The final flag is not on the wire: a frame is the final one exactly
	// when it authenticates with the flag set.
	last := false
	iv := nonce(sr.n, false)
	plain, err := sr.aead.Open(sr.pbuf[:0], iv[:], ct, nil)
	if err != nil {
		iv = nonce(sr.n, true)
		if plain, err = sr.aead.Open(sr.pbuf[:0], iv[:], ct, nil); err != nil {
			return fmt.Errorf("%w (frame %d)", ErrAuth, sr.n)
		}
		last = true
	}
	if !last && len(plain) == 0 {
		return fmt.Errorf("%w: empty frame %d before the final one", ErrFrameSize, sr.n)
	}
	sr.n++
	if last {
		// Nothing may follow the final frame. Check before handing out its
		// plaintext, so a stream with trailing data yields no last bytes.
		var one [1]byte
		k, err := io.ReadFull(sr.r, one[:])
		if k > 0 {
			return ErrTrailingData
		}
		if err != io.EOF {
			return err
		}
		sr.final = true
	}
	sr.plain = plain
	return nil
}

// truncated maps an end of input in the middle of the stream to
// ErrTruncated; other read errors pass through.
func truncated(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("%w (%w)", ErrTruncated, io.ErrUnexpectedEOF)
	}
	return err
}

// Seal encrypts plain as one complete stream under key: a convenience for
// tests and small bodies.
func Seal(key, plain []byte) ([]byte, error) {
	var buf bytes.Buffer
	w, err := NewWriter(&buf, key)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(plain); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
