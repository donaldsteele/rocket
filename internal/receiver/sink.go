package receiver

import (
	"encoding/json"
	"fmt"
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

// FilesSink is a Sink that stores each transfer through the Files the
// function returns for it.
type FilesSink func(id string, m protocol.Manifest) (Files, error)

// Begin implements Sink.
func (fs FilesSink) Begin(id string, m protocol.Manifest, progress transfer.ProgressFn) (Output, error) {
	files, err := fs(id, m)
	if err != nil {
		return nil, err
	}
	return &filesOutput{files: files, m: m, tr: transfer.NewTracker(progress, m.Total, transfer.Files(m)), tops: map[string]string{}}, nil
}

// filesOutput writes a manifest through Files: names sanitized, top-level
// folders mapped to the fresh names Files made, each file exactly its size.
type filesOutput struct {
	files Files
	m     protocol.Manifest
	tr    *transfer.Tracker
	next  int
	tops  map[string]string // lower-cased sanitized top-level folder -> name Files made
	paths []string
	count int
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
	f, err := o.files.Create(rel, e.Size, e.Mode&0o777)
	if err != nil {
		return err
	}
	o.tr.File(e.Path)
	if err := copyExact(o.tr.Writer(f), src, e); err != nil {
		f.Abort()
		return err
	}
	loc, err := f.Commit()
	if err != nil {
		return err
	}
	if !nested {
		o.paths = append(o.paths, loc)
	}
	o.count++
	o.next++
	return nil
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

func (o *filesOutput) Abort() { o.files.Abort() }

func (o *filesOutput) Location() string { return o.files.Location() }

// copyExact copies exactly e.Size bytes from src to w and checks that src
// has nothing more.
func copyExact(w io.Writer, src io.Reader, e protocol.Entry) error {
	n, err := io.CopyN(w, src, e.Size)
	if err != nil {
		// Only a plain end of input means the body was short; a wrapped
		// error (a cut-off stream) keeps its identity.
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return fmt.Errorf("%w: %s: got %d of %d bytes", transfer.ErrSize, e.Path, n, e.Size)
		}
		return err
	}
	var one [1]byte
	if k, _ := io.ReadFull(src, one[:]); k > 0 {
		return fmt.Errorf("%w: %s: more than %d bytes sent", transfer.ErrSize, e.Path, e.Size)
	}
	return nil
}
