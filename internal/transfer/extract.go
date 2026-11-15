package transfer

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/donaldsteele/rocket/internal/protocol"
)

// ErrArchive is wrapped when the entries written do not match the manifest:
// out of order, missing, or a name that clashes with another entry.
var ErrArchive = errors.New("body does not match the offer")

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
// Abort, which removes everything this extractor created, or Keep, which
// leaves it in place so that ResumeExtractor can continue the transfer.
type Extractor struct {
	dir     string
	m       protocol.Manifest
	next    int
	tops    map[string]string // lower-cased top-level folder name -> path on disk
	created []string          // top-level paths created, in order
	files   int
	tr      *tracker
	// part is the part file of entry next while it is written, and after a
	// failed write (Keep keeps it, Abort removes it); final is the name it
	// gets. partOff is how much of it ResumeExtractor took over.
	part, final string
	partOff     int64
	lastFile    string // final path of the last file completed
}

// NewExtractor prepares to write m under dir. m must already be validated
// (protocol.Manifest.Validate).
func NewExtractor(dir string, m protocol.Manifest, progress ProgressFn) *Extractor {
	return &Extractor{dir: dir, m: m, tops: map[string]string{}, tr: newTracker(progress, m.Total, Files(m))}
}

// Write writes entry i, which must be the next entry of the manifest. For a
// file exactly the entry's size is read from src, which must then be at EOF;
// for a folder src is not read. For the entry a resumed extractor holds a
// part file of, src gives only the rest: the entry's size less what the
// part file holds.
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

// Abort removes every file and folder this extractor created, including
// what an earlier, kept attempt created when it was resumed.
func (x *Extractor) Abort() {
	if x.part != "" {
		os.Remove(x.part)
		x.part = ""
	}
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
// the same name). It returns the final path. When x holds a part file for
// this entry (a resumed transfer), it is continued instead. On failure the
// part file stays in x.part for Abort or Keep.
func (x *Extractor) writeFile(e protocol.Entry, dest string, unique bool, src io.Reader) (string, error) {
	perm := os.FileMode(0o644)
	if e.Mode&0o100 != 0 {
		perm = 0o755
	}
	var f *os.File
	final := dest
	skip := int64(0)
	if x.part != "" {
		var err error
		if f, err = reopenPart(x.part, x.partOff); err != nil {
			return "", err
		}
		final, skip = x.final, x.partOff
	}
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
	x.part, x.final, x.partOff = f.Name(), final, 0
	fail := func(err error) (string, error) {
		f.Close()
		return "", err
	}
	x.tr.file(e.Path)
	want := e.Size - skip
	n, err := io.CopyN(trackWriter{w: f, t: x.tr}, src, want)
	if err != nil {
		// Only a plain end of input means the body was short; a wrapped
		// error (a cut-off stream) keeps its identity.
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return fail(fmt.Errorf("%w: %s: got %d of %d bytes", ErrSize, e.Path, skip+n, e.Size))
		}
		return fail(err)
	}
	var one [1]byte
	if k, _ := io.ReadFull(src, one[:]); k > 0 {
		return fail(fmt.Errorf("%w: %s: more than %d bytes sent", ErrSize, e.Path, e.Size))
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if exists(final) {
		if !unique {
			return "", fmt.Errorf("%w: %q clashes with another entry", ErrArchive, e.Path)
		}
		final = UniquePath(dest)
	}
	if err := os.Rename(x.part, final); err != nil {
		return "", err
	}
	x.part, x.final = "", ""
	x.lastFile = final
	x.files++
	return final, nil
}

// reopenPart opens a kept part file to continue it at byte off.
func reopenPart(part string, off int64) (*os.File, error) {
	f, err := os.OpenFile(part, os.O_WRONLY, 0)
	if err != nil {
		return nil, err
	}
	if err := f.Truncate(off); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// ExtractorState is what Keep left of a transfer an Extractor did not
// finish: enough for ResumeExtractor to continue it, or for
// DiscardExtractor to remove it. Every path is inside Dir.
type ExtractorState struct {
	Dir      string            `json:"dir"`
	Next     int               `json:"next"` // entries before Next are complete
	Tops     map[string]string `json:"tops"` // lower-cased top-level folder name -> path
	Created  []string          `json:"created"`
	Files    int               `json:"files"` // files complete
	Sent     int64             `json:"sent"`  // bytes stored, the part file included
	Part     string            `json:"part,omitempty"`
	Final    string            `json:"final,omitempty"`
	Offset   int64             `json:"offset"` // bytes in Part
	LastFile string            `json:"last_file,omitempty"`
}

// Keep ends a failed extraction without removing anything and returns its
// state and the point a sender may continue from: the part file of the
// entry in flight with the SHA-256 of its bytes; else the next file entry
// at 0; else the last file at its end (everything is stored, only the end
// of the body is missing); else the beginning.
func (x *Extractor) Keep() (ExtractorState, protocol.ResumePoint, error) {
	st := ExtractorState{
		Dir: x.dir, Next: x.next, Tops: x.tops, Created: x.created, Files: x.files,
		Part: x.part, Final: x.final, LastFile: x.lastFile,
	}
	for _, e := range x.m.Entries[:x.next] {
		if e.Mode&protocol.ModeDir == 0 {
			st.Sent += e.Size
		}
	}
	empty := sha256.Sum256(nil)
	var pt protocol.ResumePoint
	switch next := nextFile(x.m, x.next); {
	case x.part != "":
		sum, n, err := hashFile(x.part, -1)
		if err != nil {
			return st, pt, err
		}
		if n > x.m.Entries[x.next].Size {
			return st, pt, fmt.Errorf("%w: %s is larger than offered", ErrSize, x.part)
		}
		st.Offset, st.Sent = n, st.Sent+n
		pt = protocol.ResumePoint{Entry: uint32(x.next), Offset: uint64(n), SHA256: sum}
	case next >= 0:
		pt = protocol.ResumePoint{Entry: uint32(next), SHA256: hex.EncodeToString(empty[:])}
	case x.lastFile != "":
		last := prevFile(x.m, x.next)
		size := x.m.Entries[last].Size
		sum, n, err := hashFile(x.lastFile, size)
		if err != nil {
			return st, pt, err
		}
		if n != size {
			return st, pt, fmt.Errorf("%s changed", x.lastFile)
		}
		pt = protocol.ResumePoint{Entry: uint32(last), Offset: uint64(size), SHA256: sum}
	default:
		pt = protocol.ResumePoint{SHA256: hex.EncodeToString(empty[:])}
	}
	x.part, x.created = "", nil // kept: no longer this extractor's to remove
	return st, pt, nil
}

// nextFile is the index of the first file entry at or after i, or -1.
func nextFile(m protocol.Manifest, i int) int {
	for ; i < len(m.Entries); i++ {
		if m.Entries[i].Mode&protocol.ModeDir == 0 {
			return i
		}
	}
	return -1
}

// prevFile is the index of the last file entry before i, or -1.
func prevFile(m protocol.Manifest, i int) int {
	for i--; i >= 0; i-- {
		if m.Entries[i].Mode&protocol.ModeDir == 0 {
			return i
		}
	}
	return -1
}

// hashFile returns the hex SHA-256 of the first max bytes of file p (all of
// it for max < 0) and how many bytes that was.
func hashFile(p string, max int64) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	var r io.Reader = f
	if max >= 0 {
		r = io.LimitReader(f, max)
	}
	n, err := io.Copy(h, r)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// inside reports whether p is dir or below it.
func inside(dir, p string) bool {
	r, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(p))
	return err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) && !filepath.IsAbs(r)
}

// CheckExtractorState reports whether st still describes what is on disk:
// every path inside st.Dir, every created item and the part file present,
// the part file holding at least st.Offset bytes.
func CheckExtractorState(st ExtractorState) error {
	if st.Dir == "" {
		return errors.New("kept transfer without a folder")
	}
	paths := append([]string{}, st.Created...)
	for _, p := range st.Tops {
		paths = append(paths, p)
	}
	for _, p := range append(paths, st.Part, st.Final, st.LastFile) {
		if p != "" && (!inside(st.Dir, p) || filepath.Clean(p) == filepath.Clean(st.Dir)) {
			return fmt.Errorf("kept path %s is outside %s", p, st.Dir)
		}
	}
	for _, p := range paths {
		if _, err := os.Lstat(p); err != nil {
			return fmt.Errorf("kept item %s is gone", p)
		}
	}
	if st.Part != "" {
		fi, err := os.Lstat(st.Part)
		if err != nil || !fi.Mode().IsRegular() || fi.Size() < st.Offset {
			return fmt.Errorf("kept part file %s is gone or shorter", st.Part)
		}
	}
	return nil
}

// ResumeExtractor continues the extraction st describes (see Keep) of the
// same manifest m: entries before st.Next are taken as written, and the
// part file of entry st.Next, if any, is continued from st.Offset.
func ResumeExtractor(st ExtractorState, m protocol.Manifest, progress ProgressFn) (*Extractor, error) {
	if st.Next < 0 || st.Next > len(m.Entries) || (st.Part != "" && st.Next == len(m.Entries)) {
		return nil, fmt.Errorf("%w: kept state does not fit the offer", ErrArchive)
	}
	if err := CheckExtractorState(st); err != nil {
		return nil, err
	}
	x := NewExtractor(st.Dir, m, progress)
	x.next, x.created, x.files = st.Next, append([]string(nil), st.Created...), st.Files
	for k, v := range st.Tops {
		x.tops[k] = v
	}
	x.part, x.final, x.partOff, x.lastFile = st.Part, st.Final, st.Offset, st.LastFile
	x.tr.from(st.Sent, st.Files)
	return x, nil
}

// DiscardExtractor removes everything st describes: the part file and the
// top-level items created. Paths outside st.Dir are left alone.
func DiscardExtractor(st ExtractorState) {
	if st.Part != "" && inside(st.Dir, st.Part) {
		os.Remove(st.Part)
	}
	for i := len(st.Created) - 1; i >= 0; i-- {
		if p := st.Created[i]; inside(st.Dir, p) && filepath.Clean(p) != filepath.Clean(st.Dir) {
			os.RemoveAll(p)
		}
	}
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
