package transfer

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/donaldsteele/rocket/internal/protocol"
)

// ErrArchive is wrapped when a multi-file body does not match its manifest:
// wrong names, types, sizes, order or count, or an entry type rocket never
// writes (symlinks, hard links, devices).
var ErrArchive = errors.New("archive does not match the offer")

// Result describes a finished multi-file receive.
type Result struct {
	Paths []string // top-level files and folders created on disk, in order
	Files int      // files written (folders not counted)
	Bytes int64    // bytes written
}

// Extractor writes the entries of one accepted manifest under a download
// folder. Each top-level name gets a fresh, unique file or folder there
// ("photos", or "photos (1)" if taken), so nothing that existed before is
// ever written into or over; everything below a top-level folder is joined
// with SafeJoin. Files go to "<name>.rocket-part" and are renamed once
// complete. Entries must be written in manifest order. On any failure call
// Abort, which removes everything this extractor created.
type Extractor struct {
	dir     string
	m       protocol.Manifest
	next    int
	tops    map[string]string // lower-cased top-level folder name -> path on disk
	created []string          // top-level paths created, in order
	files   int
	tr      *tracker
}

// NewExtractor prepares to write m under dir. m must already be validated
// (protocol.Manifest.Validate).
func NewExtractor(dir string, m protocol.Manifest, progress ProgressFn) *Extractor {
	return &Extractor{dir: dir, m: m, tops: map[string]string{}, tr: newTracker(progress, m.Total, Files(m))}
}

// Write writes entry i, which must be the next entry of the manifest. For a
// file exactly the entry's size is read from src, which must then be at EOF;
// for a folder src is not read.
func (x *Extractor) Write(i int, src io.Reader) error {
	if i != x.next || i >= len(x.m.Entries) {
		return fmt.Errorf("%w: entry %d out of order", ErrArchive, i)
	}
	e := x.m.Entries[i]
	if err := protocol.ValidatePath(e.Path); err != nil {
		return fmt.Errorf("%w: %v", ErrArchive, err)
	}
	top, rest, nested := strings.Cut(e.Path, "/")
	isDir := e.Mode&protocol.ModeDir != 0

	switch {
	case isDir && !nested:
		if _, err := x.topDir(top); err != nil {
			return err
		}
	case isDir:
		root, err := x.topDir(top)
		if err != nil {
			return err
		}
		dest, err := SafeJoin(root, rest)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(dest, 0o755); err != nil {
			return err
		}
	case !nested:
		base, err := SafeJoin(x.dir, top)
		if err != nil {
			return err
		}
		final, err := x.writeFile(e, base, true, src)
		if err != nil {
			return err
		}
		x.created = append(x.created, final)
	default:
		root, err := x.topDir(top)
		if err != nil {
			return err
		}
		dest, err := SafeJoin(root, rest)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		if _, err := x.writeFile(e, dest, false, src); err != nil {
			return err
		}
	}
	x.next++
	return nil
}

// Finish checks that every entry was written and returns what was created.
func (x *Extractor) Finish() (Result, error) {
	if x.next != len(x.m.Entries) {
		return Result{}, fmt.Errorf("%w: got %d of %d entries", ErrArchive, x.next, len(x.m.Entries))
	}
	x.tr.finish()
	return Result{Paths: append([]string(nil), x.created...), Files: x.files, Bytes: x.tr.p.Sent}, nil
}

// Abort removes every file and folder this extractor created.
func (x *Extractor) Abort() {
	for i := len(x.created) - 1; i >= 0; i-- {
		os.RemoveAll(x.created[i])
	}
	x.created = nil
}

// topDir returns the on-disk folder for a top-level name, creating a fresh
// unique one the first time.
func (x *Extractor) topDir(top string) (string, error) {
	key := strings.ToLower(top)
	if d, ok := x.tops[key]; ok {
		return d, nil
	}
	base, err := SafeJoin(x.dir, top)
	if err != nil {
		return "", err
	}
	for n := 0; n < 1000; n++ {
		d := base
		if n > 0 {
			d = base + " (" + strconv.Itoa(n) + ")"
		}
		if exists(d + PartSuffix) {
			continue
		}
		err := os.Mkdir(d, 0o755)
		if err == nil {
			x.tops[key] = d
			x.created = append(x.created, d)
			return d, nil
		}
		if !os.IsExist(err) {
			return "", err
		}
	}
	return "", fmt.Errorf("cannot find a free name for %s", base)
}

// writeFile writes entry e to dest via a part file. With unique, dest is made
// unique (a top-level file); otherwise it must not exist yet (it lives in a
// folder this extractor created, so a clash means two entries sanitized to
// the same name). It returns the final path.
func (x *Extractor) writeFile(e protocol.Entry, dest string, unique bool, src io.Reader) (string, error) {
	perm := os.FileMode(0o644)
	if e.Mode&0o100 != 0 {
		perm = 0o755
	}
	var f *os.File
	final := dest
	for n := 0; f == nil; n++ {
		if unique {
			final = UniquePath(dest)
		} else if exists(final) {
			return "", fmt.Errorf("%w: %q clashes with another entry", ErrArchive, e.Path)
		}
		var err error
		f, err = os.OpenFile(final+PartSuffix, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
		if err != nil && (!os.IsExist(err) || !unique || n >= 100) {
			return "", err
		}
	}
	part := f.Name()
	fail := func(err error) (string, error) {
		f.Close()
		os.Remove(part)
		return "", err
	}
	x.tr.file(e.Path)
	n, err := io.CopyN(trackWriter{w: f, t: x.tr}, src, e.Size)
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return fail(fmt.Errorf("%w: %s: got %d of %d bytes", ErrSize, e.Path, n, e.Size))
		}
		return fail(err)
	}
	var one [1]byte
	if k, _ := io.ReadFull(src, one[:]); k > 0 {
		return fail(fmt.Errorf("%w: %s: more than %d bytes sent", ErrSize, e.Path, e.Size))
	}
	if err := f.Close(); err != nil {
		os.Remove(part)
		return "", err
	}
	if exists(final) {
		if !unique {
			os.Remove(part)
			return "", fmt.Errorf("%w: %q clashes with another entry", ErrArchive, e.Path)
		}
		final = UniquePath(dest)
	}
	if err := os.Rename(part, final); err != nil {
		os.Remove(part)
		return "", err
	}
	x.files++
	return final, nil
}

// EntryWriter stores the entries of one accepted manifest, in manifest
// order. *Extractor is the one that writes to a folder on disk; a receiver
// that stores files elsewhere (Android's MediaStore) supplies its own.
type EntryWriter interface {
	// Write stores entry i, which must be the next entry of the manifest.
	// For a file it reads exactly the entry's size from src, which must then
	// be at EOF; for a folder it does not read src.
	Write(i int, src io.Reader) error
	// Finish checks that every entry was written and returns what was
	// created.
	Finish() (Result, error)
	// Abort removes everything written so far.
	Abort()
}

// ReceiveTar extracts a kind:multi body: a tar archive whose entries must
// match m exactly, in order (same names, folder or regular-file type, same
// sizes, same count). Any other entry type, a mismatch, a short body or an
// extra entry aborts the receive and removes everything written. m must
// already be validated.
func ReceiveTar(dir string, m protocol.Manifest, src io.Reader, progress ProgressFn) (Result, error) {
	return ExtractTar(NewExtractor(dir, m, progress), m, src)
}

// ExtractTar is ReceiveTar writing through x, which was prepared for m.
func ExtractTar(x EntryWriter, m protocol.Manifest, src io.Reader) (Result, error) {
	fail := func(err error) (Result, error) {
		x.Abort()
		return Result{}, err
	}
	tr := tar.NewReader(src)
	for i, e := range m.Entries {
		hdr, err := tr.Next()
		if err == io.EOF {
			return fail(fmt.Errorf("%w: archive ended after %d of %d entries", ErrArchive, i, len(m.Entries)))
		}
		if err != nil {
			return fail(fmt.Errorf("%w: %v", ErrArchive, err))
		}
		if err := matchHeader(hdr, e); err != nil {
			return fail(fmt.Errorf("%w: entry %d: %v", ErrArchive, i+1, err))
		}
		if err := x.Write(i, tr); err != nil {
			return fail(err)
		}
	}
	if _, err := tr.Next(); err != io.EOF {
		return fail(fmt.Errorf("%w: more entries than offered", ErrArchive))
	}
	return x.Finish()
}

// matchHeader checks one tar header against its manifest entry.
func matchHeader(h *tar.Header, e protocol.Entry) error {
	if e.Mode&protocol.ModeDir != 0 {
		if h.Typeflag != tar.TypeDir || strings.TrimSuffix(h.Name, "/") != e.Path {
			return fmt.Errorf("got %q (type %q), offer says folder %q", h.Name, h.Typeflag, e.Path)
		}
		return nil
	}
	if h.Typeflag != tar.TypeReg || h.Name != e.Path {
		return fmt.Errorf("got %q (type %q), offer says file %q", h.Name, h.Typeflag, e.Path)
	}
	if h.Size != e.Size {
		return fmt.Errorf("%q is %d bytes, offer says %d", e.Path, h.Size, e.Size)
	}
	return nil
}
