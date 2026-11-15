package receiver

import (
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"

	"github.com/donaldsteele/rocket/internal/protocol"
	"github.com/donaldsteele/rocket/internal/transfer"
)

// Sink stores the content of accepted transfers.
type Sink interface {
	// Begin starts storing transfer id, whose manifest m has been verified.
	// progress receives the bytes stored as they arrive.
	Begin(id string, m protocol.Manifest, progress transfer.ProgressFn) (Output, error)
}

// Output stores the entries of one transfer, in manifest order. The receiver
// hands each entry its bytes in manifest order, after authenticating them;
// Write must still read exactly an entry's size and check that nothing
// follows. On any failure the receiver calls Abort, which must
// remove everything this transfer stored.
type Output interface {
	transfer.EntryWriter
	// Location says where the transfer went (a folder path or a URI). The
	// "done" event shows it when the transfer created several top-level
	// items.
	Location() string
}

// FSSink stores transfers in a folder on disk, as desktop rocket always has:
// each top-level item gets a fresh name ("photos", then "photos (1)"),
// names are sanitized, files are written as "<name>.rocket-part" and renamed
// when complete, and a failed transfer removes what it wrote (see
// transfer.Extractor). It is a ResumableSink: a transfer broken by a network
// failure can leave its folders, finished files and part file in place,
// described by a transfer.ExtractorState, to be continued later.
type FSSink struct {
	// Dir returns the download folder, read once per transfer.
	Dir func() string
}

// Begin implements Sink.
func (s FSSink) Begin(_ string, m protocol.Manifest, progress transfer.ProgressFn) (Output, error) {
	dir := s.Dir()
	return fsOut{Extractor: transfer.NewExtractor(dir, m, progress), dir: dir}, nil
}

// Resume implements ResumableSink.
func (FSSink) Resume(_ string, m protocol.Manifest, state json.RawMessage, progress transfer.ProgressFn) (Output, error) {
	var st transfer.ExtractorState
	if err := json.Unmarshal(state, &st); err != nil {
		return nil, err
	}
	x, err := transfer.ResumeExtractor(st, m, progress)
	if err != nil {
		return nil, err
	}
	return fsOut{Extractor: x, dir: st.Dir}, nil
}

// Check implements ResumableSink.
func (FSSink) Check(state json.RawMessage) error {
	var st transfer.ExtractorState
	if err := json.Unmarshal(state, &st); err != nil {
		return err
	}
	return transfer.CheckExtractorState(st)
}

// Discard implements ResumableSink.
func (FSSink) Discard(state json.RawMessage) {
	var st transfer.ExtractorState
	if json.Unmarshal(state, &st) == nil {
		transfer.DiscardExtractor(st)
	}
}

// fsOut writes a transfer with a transfer.Extractor.
type fsOut struct {
	*transfer.Extractor
	dir string
}

func (o fsOut) Location() string { return o.dir }

// Keep implements Keeper.
func (o fsOut) Keep() (KeptOutput, error) {
	st, pt, err := o.Extractor.Keep()
	if err != nil {
		return KeptOutput{}, err
	}
	b, err := json.Marshal(st)
	if err != nil {
		return KeptOutput{}, err
	}
	return KeptOutput{Next: st.Next, Point: pt, Done: st.Sent, State: b}, nil
}

// Files stores one transfer's folders and files one at a time, by relative
// path: the shape a platform without a plain download folder (Android's
// MediaStore) implements. FilesSink turns it into a Sink.
type Files interface {
	// Mkdir creates folder rel ("/"-separated, every segment sanitized) and
	// any missing parents, and returns the relative path it made. A
	// top-level folder (no "/") must be fresh: when the name is taken it
	// makes another ("photos (1)") and returns that name, and every later
	// entry under it is given with that name. A nested folder may already
	// exist (it was made for this transfer).
	Mkdir(rel string) (string, error)
	// Create starts file rel (sanitized; its folders made as needed) of
	// exactly size bytes. It must never replace an existing file: when rel is taken,
	// it picks a fresh name ("a (1).txt").
	Create(rel string, size int64, mode uint32) (File, error)
	// Abort removes every folder and file of this transfer, committed or not.
	Abort()
	// Location says where the transfer went (see Output).
	Location() string
}

// File is one file being stored.
type File interface {
	io.Writer
	// Commit makes the complete file visible and returns its location.
	Commit() (string, error)
	// Abort discards the incomplete file.
	Abort()
}

// KeepFiles is a Files that can leave a transfer broken by a network
// failure in place, to be continued later (NewFilesSink).
type KeepFiles interface {
	Files
	// Keep ends the transfer without removing anything it stored, finished
	// files and the incomplete one alike, and describes what it stored for
	// FilesStore.Resume, Check and Discard.
	Keep() (json.RawMessage, error)
	// Reopen opens incomplete file ref (a KeptFile's Ref, from this
	// transfer or the one it continues) to continue it at byte offset: the
	// file is cut to offset and writes go on from there.
	Reopen(ref string, offset int64) (KeptFile, error)
}

// KeptFile is a File that can be left incomplete for a resume.
type KeptFile interface {
	File
	// Ref names the file for KeepFiles.Reopen.
	Ref() string
	// Close stops writing and leaves the incomplete file as it is.
	Close() error
}

// FilesStore makes the KeepFiles of each transfer and continues kept ones.
// NewFilesSink turns it into a ResumableSink.
type FilesStore interface {
	// Begin starts storing transfer id of manifest m.
	Begin(id string, m protocol.Manifest) (KeepFiles, error)
	// Resume continues storing a transfer kept as state (what KeepFiles.Keep
	// returned). The KeepFiles it returns owns everything the kept transfer
	// stored: its Abort removes that too.
	Resume(id string, m protocol.Manifest, state json.RawMessage) (KeepFiles, error)
	// Check reports whether everything state describes is still there.
	Check(state json.RawMessage) error
	// Discard removes everything state describes.
	Discard(state json.RawMessage)
}

// FilesSink is a Sink that stores each transfer through the Files the
// function returns for it.
type FilesSink func(id string, m protocol.Manifest) (Files, error)

// Begin implements Sink.
func (fs FilesSink) Begin(id string, m protocol.Manifest, progress transfer.ProgressFn) (Output, error) {
	files, err := fs(id, m)
	if err != nil {
		return nil, err
	}
	return newFilesOutput(files, nil, m, progress), nil
}

// NewFilesSink is a ResumableSink that stores transfers through s, like
// FilesSink, and keeps a transfer broken by a network failure: its
// Outputs are Keepers. The resume point of a kept transfer is the
// incomplete file at the length written, with the SHA-256 of what was
// written to it (hashed on the way in, so nothing is read back).
func NewFilesSink(s FilesStore) ResumableSink { return filesStoreSink{s} }

type filesStoreSink struct{ s FilesStore }

func (fs filesStoreSink) Begin(id string, m protocol.Manifest, progress transfer.ProgressFn) (Output, error) {
	files, err := fs.s.Begin(id, m)
	if err != nil {
		return nil, err
	}
	if files == nil {
		return nil, errors.New("the sink returned no transfer")
	}
	return newFilesOutput(files, files, m, progress), nil
}

func (fs filesStoreSink) Resume(id string, m protocol.Manifest, state json.RawMessage, progress transfer.ProgressFn) (Output, error) {
	var st filesState
	if err := json.Unmarshal(state, &st); err != nil {
		return nil, err
	}
	if err := st.fits(m); err != nil {
		return nil, err
	}
	files, err := fs.s.Resume(id, m, st.Sink)
	if err != nil {
		return nil, err
	}
	if files == nil {
		return nil, errors.New("the sink returned no transfer")
	}
	o := newFilesOutput(files, files, m, progress)
	o.next, o.count, o.paths, o.lastSum = st.Next, st.Count, append([]string(nil), st.Paths...), st.Last
	for k, v := range st.Tops {
		o.tops[k] = v
	}
	o.part, o.partOff, o.partHash = st.Part, st.Offset, st.Hash
	o.tr.From(st.Sent, st.Count)
	return o, nil
}

func (fs filesStoreSink) Check(state json.RawMessage) error {
	var st filesState
	if err := json.Unmarshal(state, &st); err != nil {
		return err
	}
	return fs.s.Check(st.Sink)
}

func (fs filesStoreSink) Discard(state json.RawMessage) {
	var st filesState
	if json.Unmarshal(state, &st) == nil {
		fs.s.Discard(st.Sink)
	}
}

// filesState is what a filesOutput's Keep leaves: the sink's own
// description plus where the manifest stopped.
type filesState struct {
	Sink  json.RawMessage   `json:"sink"`
	Next  int               `json:"next"` // entries before Next are complete
	Tops  map[string]string `json:"tops"`
	Paths []string          `json:"paths"`
	Count int               `json:"count"` // files complete
	Sent  int64             `json:"sent"`  // bytes stored, the incomplete file included
	Last  string            `json:"last,omitempty"`
	// Part is the incomplete file of entry Next (KeptFile.Ref), Offset its
	// length and Hash the SHA-256 state of its bytes
	// (encoding.BinaryMarshaler of crypto/sha256).
	Part   string `json:"part,omitempty"`
	Offset int64  `json:"offset"`
	Hash   []byte `json:"hash,omitempty"`
}

// fits checks that st can continue manifest m.
func (st filesState) fits(m protocol.Manifest) error {
	bad := st.Next < 0 || st.Next > len(m.Entries) || st.Count < 0 || st.Sent < 0 || st.Sent > m.Total
	if !bad && st.Part != "" {
		bad = st.Next == len(m.Entries) || m.Entries[st.Next].Mode&protocol.ModeDir != 0 ||
			st.Offset < 0 || st.Offset > m.Entries[st.Next].Size || len(st.Hash) == 0
	}
	if bad {
		return fmt.Errorf("%w: kept state does not fit the offer", transfer.ErrArchive)
	}
	return nil
}

// filesOutput writes a manifest through Files: names sanitized, top-level
// folders mapped to the fresh names Files made, each file exactly its size.
// With keep set it can also keep a broken transfer (Keeper).
type filesOutput struct {
	files Files
	keep  KeepFiles // nil: cannot keep
	m     protocol.Manifest
	tr    *transfer.Tracker
	next  int
	tops  map[string]string // lower-cased sanitized top-level folder -> name Files made
	paths []string
	count int
	// lastSum is the hex SHA-256 of the last file completed.
	lastSum string
	// cur is the file of entry next after a network failure cut it short,
	// left for Keep or Abort.
	cur *inflight
	// part is the incomplete file of entry next that a resumed output
	// continues (until Write reopens it), partOff its length and partHash
	// the SHA-256 state of its bytes.
	part     string
	partOff  int64
	partHash []byte
}

func newFilesOutput(files Files, keep KeepFiles, m protocol.Manifest, progress transfer.ProgressFn) *filesOutput {
	return &filesOutput{
		files: files, keep: keep, m: m,
		tr:   transfer.NewTracker(progress, m.Total, transfer.Files(m)),
		tops: map[string]string{},
	}
}

// inflight is a file being written, hashing what reaches it.
type inflight struct {
	f File
	h hash.Hash
	n int64 // bytes in the file
}

func (c *inflight) Write(p []byte) (int, error) {
	k, err := c.f.Write(p)
	if k > 0 {
		c.h.Write(p[:k])
		c.n += int64(k)
	}
	return k, err
}

func (o *filesOutput) Write(i int, src io.Reader) error {
	if i != o.next || i >= len(o.m.Entries) {
		return fmt.Errorf("%w: entry %d out of order", transfer.ErrArchive, i)
	}
	e := o.m.Entries[i]
	if err := protocol.ValidatePath(e.Path); err != nil {
		return fmt.Errorf("%w: %v", transfer.ErrArchive, err)
	}
	rel := transfer.SanitizePath(e.Path)
	top, rest, nested := strings.Cut(rel, "/")
	isDir := e.Mode&protocol.ModeDir != 0
	if nested || isDir {
		made, err := o.topDir(top)
		if err != nil {
			return err
		}
		if !nested {
			o.next++
			return nil
		}
		rel = made + "/" + rest
	}
	if isDir {
		if _, err := o.files.Mkdir(rel); err != nil {
			return err
		}
		o.next++
		return nil
	}
	cur, err := o.open(rel, e)
	if err != nil {
		return err
	}
	o.tr.File(e.Path)
	if err := copyExact(o.tr.Writer(cur), src, e, cur.n); err != nil {
		if _, ok := cur.f.(KeptFile); ok && o.keep != nil && transfer.IsNetwork(err) {
			o.cur = cur // Keep keeps it, Abort removes it
			return err
		}
		cur.f.Abort()
		return err
	}
	loc, err := cur.f.Commit()
	if err != nil {
		return err
	}
	o.lastSum = hex.EncodeToString(cur.h.Sum(nil))
	if !nested {
		o.paths = append(o.paths, loc)
	}
	o.count++
	o.next++
	return nil
}

// open starts the file of entry e at rel, or reopens the incomplete one a
// resumed output continues.
func (o *filesOutput) open(rel string, e protocol.Entry) (*inflight, error) {
	if o.part == "" {
		f, err := o.files.Create(rel, e.Size, e.Mode&0o777)
		if err != nil {
			return nil, err
		}
		if f == nil {
			return nil, errors.New("the sink returned no file")
		}
		return &inflight{f: f, h: sha256.New()}, nil
	}
	h := sha256.New()
	if err := h.(encoding.BinaryUnmarshaler).UnmarshalBinary(o.partHash); err != nil {
		return nil, fmt.Errorf("kept hash of %s: %w", e.Path, err)
	}
	f, err := o.keep.Reopen(o.part, o.partOff)
	if err != nil {
		return nil, err
	}
	if f == nil {
		return nil, errors.New("the sink returned no file")
	}
	cur := &inflight{f: f, h: h, n: o.partOff}
	o.part, o.partOff, o.partHash = "", 0, nil
	return cur, nil
}

// topDir returns the name Files made for top-level folder top, making it
// the first time. Like transfer.Extractor it matches names case-blind.
func (o *filesOutput) topDir(top string) (string, error) {
	key := strings.ToLower(top)
	if made, ok := o.tops[key]; ok {
		return made, nil
	}
	made, err := o.files.Mkdir(top)
	if err != nil {
		return "", err
	}
	if made == "" || strings.Contains(made, "/") {
		return "", fmt.Errorf("the sink made folder %q for %q", made, top)
	}
	o.tops[key] = made
	o.paths = append(o.paths, made)
	return made, nil
}

func (o *filesOutput) Finish() (transfer.Result, error) {
	if o.next != len(o.m.Entries) {
		return transfer.Result{}, fmt.Errorf("%w: got %d of %d entries", transfer.ErrArchive, o.next, len(o.m.Entries))
	}
	o.tr.Finish()
	return transfer.Result{Paths: append([]string(nil), o.paths...), Files: o.count, Bytes: o.tr.Sent()}, nil
}

// Abort removes everything stored, including what a kept transfer this
// output continues stored.
func (o *filesOutput) Abort() {
	if o.cur != nil {
		o.cur.f.Abort()
		o.cur = nil
	}
	o.files.Abort()
}

func (o *filesOutput) Location() string { return o.files.Location() }

// Keep implements Keeper when the Files can keep (NewFilesSink). The resume
// point is, as transfer.Extractor's: the incomplete file at its length;
// else the next file at 0; else the last file at its end; else the
// beginning.
func (o *filesOutput) Keep() (KeptOutput, error) {
	if o.keep == nil {
		return KeptOutput{}, errors.New("this sink cannot keep a transfer")
	}
	st := filesState{Next: o.next, Tops: o.tops, Paths: o.paths, Count: o.count, Last: o.lastSum}
	for _, e := range o.m.Entries[:o.next] {
		if e.Mode&protocol.ModeDir == 0 {
			st.Sent += e.Size
		}
	}
	empty := sha256.Sum256(nil)
	var pt protocol.ResumePoint
	switch next := nextFileEntry(o.m, o.next); {
	case o.cur != nil:
		kf := o.cur.f.(KeptFile) // Write keeps only a KeptFile
		if o.cur.n > o.m.Entries[o.next].Size {
			return KeptOutput{}, fmt.Errorf("%w: %s is larger than offered", transfer.ErrSize, o.m.Entries[o.next].Path)
		}
		hb, err := o.cur.h.(encoding.BinaryMarshaler).MarshalBinary()
		if err != nil {
			return KeptOutput{}, err
		}
		if err := kf.Close(); err != nil {
			return KeptOutput{}, err
		}
		st.Part, st.Offset, st.Hash = kf.Ref(), o.cur.n, hb
		pt = protocol.ResumePoint{Entry: uint32(o.next), Offset: uint64(o.cur.n), SHA256: hex.EncodeToString(o.cur.h.Sum(nil))}
	case o.part != "":
		// Resumed, and broken again before the incomplete file was reached.
		h := sha256.New()
		if err := h.(encoding.BinaryUnmarshaler).UnmarshalBinary(o.partHash); err != nil {
			return KeptOutput{}, err
		}
		st.Part, st.Offset, st.Hash = o.part, o.partOff, o.partHash
		pt = protocol.ResumePoint{Entry: uint32(o.next), Offset: uint64(o.partOff), SHA256: hex.EncodeToString(h.Sum(nil))}
	case next >= 0:
		pt = protocol.ResumePoint{Entry: uint32(next), SHA256: hex.EncodeToString(empty[:])}
	case o.lastSum != "":
		last := prevFileEntry(o.m, o.next)
		pt = protocol.ResumePoint{Entry: uint32(last), Offset: uint64(o.m.Entries[last].Size), SHA256: o.lastSum}
	default:
		pt = protocol.ResumePoint{SHA256: hex.EncodeToString(empty[:])}
	}
	st.Sent += st.Offset
	sink, err := o.keep.Keep()
	if err != nil {
		return KeptOutput{}, err
	}
	st.Sink = sink
	b, err := json.Marshal(st)
	if err != nil {
		return KeptOutput{}, err
	}
	o.cur, o.part = nil, "" // kept: no longer this output's to remove
	return KeptOutput{Next: st.Next, Point: pt, Done: st.Sent, State: b}, nil
}

// nextFileEntry is the index of the first file entry at or after i, or -1.
func nextFileEntry(m protocol.Manifest, i int) int {
	for ; i < len(m.Entries); i++ {
		if m.Entries[i].Mode&protocol.ModeDir == 0 {
			return i
		}
	}
	return -1
}

// prevFileEntry is the index of the last file entry before i, or -1.
func prevFileEntry(m protocol.Manifest, i int) int {
	for i--; i >= 0; i-- {
		if m.Entries[i].Mode&protocol.ModeDir == 0 {
			return i
		}
	}
	return -1
}

// copyExact copies the rest of entry e (its size less have, the bytes an
// earlier attempt stored) from src to w and checks that src has nothing
// more.
func copyExact(w io.Writer, src io.Reader, e protocol.Entry, have int64) error {
	n, err := io.CopyN(w, src, e.Size-have)
	if err != nil {
		// Only a plain end of input means the body was short; a wrapped
		// error (a cut-off stream) keeps its identity.
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return fmt.Errorf("%w: %s: got %d of %d bytes", transfer.ErrSize, e.Path, have+n, e.Size)
		}
		return err
	}
	var one [1]byte
	if k, _ := io.ReadFull(src, one[:]); k > 0 {
		return fmt.Errorf("%w: %s: more than %d bytes sent", transfer.ErrSize, e.Path, e.Size)
	}
	return nil
}
