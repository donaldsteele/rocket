package transfer

import (
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/donaldsteele/rocket/internal/protocol"
)

// Plan is a manifest built from local paths, with the file on disk behind
// each manifest entry. Build one with PlanPaths and send it with SendPlan.
type Plan struct {
	Manifest protocol.Manifest
	// Sources holds the local path of each manifest entry ("" for a
	// directory), index for index with Manifest.Entries.
	Sources []string
	// Skipped lists local paths left out: symlinks, devices, sockets and
	// other non-regular files found while walking a folder.
	Skipped []string
}

// Files counts the non-directory entries of m.
func Files(m protocol.Manifest) int {
	n := 0
	for _, e := range m.Entries {
		if e.Mode&protocol.ModeDir == 0 {
			n++
		}
	}
	return n
}

// Label is a short display name for a whole manifest: the top-level name of
// its first entry. Events use it as "name" so one transfer keeps one name.
func Label(m protocol.Manifest) string {
	if len(m.Entries) == 0 {
		return ""
	}
	top, _, _ := strings.Cut(m.Entries[0].Path, "/")
	return top
}

// PlanPaths builds the manifest for sending paths: one regular file gives a
// kind:file manifest; several paths or any folder give kind:multi. Each
// argument is rooted at its own base name (a folder "C:\photos" becomes
// "photos/..."); folders are walked recursively, empty folders included.
// Symlinks and other non-regular entries inside a folder are skipped and
// logged (an argument that is itself a symlink is followed). Names are
// sanitized per segment and made unique case-insensitively ("a (1).txt").
// The result is validated against the protocol limits.
func PlanPaths(paths []string) (*Plan, error) {
	if len(paths) == 0 {
		return nil, fmt.Errorf("nothing to send")
	}
	b := newBuilder()
	p := &Plan{}
	for _, arg := range paths {
		abs, err := filepath.Abs(arg)
		if err != nil {
			return nil, err
		}
		st, err := os.Stat(abs)
		if err != nil {
			return nil, err
		}
		switch {
		case st.Mode().IsRegular():
			if _, err := b.addFile("", filepath.Base(abs), st.Size(), uint32(st.Mode().Perm())); err != nil {
				return nil, err
			}
			p.Sources = append(p.Sources, abs)
		case st.IsDir():
			if err := p.walk(b, abs); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("%s is not a regular file or folder", arg)
		}
	}
	p.Manifest = b.manifest()
	if err := p.Manifest.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}

// walk adds the folder root and everything under it.
func (p *Plan) walk(b *builder, root string) error {
	dirs := map[string]string{} // local folder -> manifest path
	return filepath.WalkDir(root, func(local string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		parent := ""
		if local != root {
			parent = dirs[filepath.Dir(local)]
		}
		name := d.Name()
		if local == root {
			name = filepath.Base(root)
		}
		switch t := d.Type(); {
		case t.IsDir():
			mp, err := b.addDir(parent, name)
			if err != nil {
				return err
			}
			dirs[local] = mp
			p.Sources = append(p.Sources, "")
		case t.IsRegular():
			info, err := d.Info()
			if err != nil {
				return err
			}
			if _, err := b.addFile(parent, name, info.Size(), uint32(info.Mode().Perm())); err != nil {
				return err
			}
			p.Sources = append(p.Sources, local)
		default:
			what := "special file"
			if t&fs.ModeSymlink != 0 {
				what = "symlink"
			}
			slog.Warn("skipping "+what, "path", local)
			p.Skipped = append(p.Skipped, local)
		}
		return nil
	})
}

// builder accumulates manifest entries with sanitized, unique paths.
type builder struct {
	entries []protocol.Entry
	taken   map[string]bool // lower-cased manifest paths in use
	dirs    map[string]bool // lower-cased manifest paths that are folders
	total   int64
}

func newBuilder() *builder {
	return &builder{taken: map[string]bool{}, dirs: map[string]bool{}}
}

func (b *builder) room() error {
	if len(b.entries) >= protocol.MaxEntries {
		return fmt.Errorf("too many files: at most %d entries per send", protocol.MaxEntries)
	}
	return nil
}

// unique returns parent/name with name sanitized and, if that path is taken,
// " (n)" added (before the extension for a file).
func (b *builder) unique(parent, name string, dir bool) string {
	name = SanitizeComponent(name)
	stem, ext := name, ""
	if !dir {
		ext = path.Ext(name)
		if ext == name { // ".bashrc": no stem to number
			ext = ""
		}
		stem = name[:len(name)-len(ext)]
	}
	cand := joinRel(parent, name)
	for n := 1; b.taken[strings.ToLower(cand)]; n++ {
		cand = joinRel(parent, stem+" ("+strconv.Itoa(n)+")"+ext)
	}
	return cand
}

// addDir adds a new folder entry under parent (never merging with an
// existing one) and returns its manifest path.
func (b *builder) addDir(parent, name string) (string, error) {
	if err := b.room(); err != nil {
		return "", err
	}
	p := b.unique(parent, name, true)
	b.taken[strings.ToLower(p)] = true
	b.dirs[strings.ToLower(p)] = true
	b.entries = append(b.entries, protocol.Entry{Path: p, Mode: protocol.ModeDir | 0o755})
	return p, nil
}

// ensureDir returns the folder parent/name, adding it unless a folder of that
// name already exists.
func (b *builder) ensureDir(parent, name string) (string, error) {
	p := joinRel(parent, SanitizeComponent(name))
	if b.dirs[strings.ToLower(p)] {
		return p, nil
	}
	return b.addDir(parent, name)
}

// addFile adds a file entry under parent and returns its manifest path.
func (b *builder) addFile(parent, name string, size int64, mode uint32) (string, error) {
	if err := b.room(); err != nil {
		return "", err
	}
	if size < 0 {
		return "", fmt.Errorf("negative size for %s", name)
	}
	b.total += size
	if b.total > protocol.MaxTotal {
		return "", fmt.Errorf("too much data: at most %d bytes per send", protocol.MaxTotal)
	}
	p := b.unique(parent, name, false)
	b.taken[strings.ToLower(p)] = true
	b.entries = append(b.entries, protocol.Entry{Path: p, Size: size, Mode: mode & 0o777})
	return p, nil
}

// addRelFile adds a file given as an untrusted relative path such as a
// browser's webkitRelativePath ("photos/2024/a.jpg"): folders on the way are
// created once and shared; empty, "." and ".." segments are dropped.
func (b *builder) addRelFile(rel string, size int64) (string, error) {
	segs := splitRel(rel)
	if len(segs) == 0 {
		segs = []string{"_"}
	}
	parent := ""
	for _, s := range segs[:len(segs)-1] {
		var err error
		if parent, err = b.ensureDir(parent, s); err != nil {
			return "", err
		}
	}
	return b.addFile(parent, segs[len(segs)-1], size, 0o644)
}

// manifest returns the built manifest: kind:file for one bare file entry,
// kind:multi otherwise.
func (b *builder) manifest() protocol.Manifest {
	m := protocol.Manifest{Kind: protocol.KindMulti, Entries: b.entries, Total: b.total}
	if len(b.entries) == 1 && b.entries[0].Mode&protocol.ModeDir == 0 && !strings.Contains(b.entries[0].Path, "/") {
		m.Kind = protocol.KindFile
	}
	return m
}

// splitRel splits an untrusted path on "/" and "\", dropping empty, "." and
// ".." segments.
func splitRel(rel string) []string {
	var out []string
	for _, s := range strings.FieldsFunc(rel, func(r rune) bool { return r == '/' || r == '\\' }) {
		if s == "." || s == ".." {
			continue
		}
		out = append(out, s)
	}
	return out
}

func joinRel(parent, name string) string {
	if parent == "" {
		return name
	}
	return parent + "/" + name
}

// BrowserFile is one file named by a browser: a bare name, or a relative path
// from a folder picker (webkitRelativePath).
type BrowserFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// BrowserManifest builds a validated manifest from a browser's file list.
// With folders, relative paths keep their folders; without, every name is
// reduced to its base name (BaseName). Names are sanitized and made unique.
// One bare file gives kind:file, anything else kind:multi. The k-th file of
// the list is the k-th non-directory entry of the manifest.
func BrowserManifest(files []BrowserFile, folders bool) (protocol.Manifest, error) {
	if len(files) == 0 {
		return protocol.Manifest{}, fmt.Errorf("no files")
	}
	if len(files) > protocol.MaxEntries {
		return protocol.Manifest{}, fmt.Errorf("too many files: at most %d per send", protocol.MaxEntries)
	}
	b := newBuilder()
	for _, f := range files {
		var err error
		if folders {
			_, err = b.addRelFile(f.Name, f.Size)
		} else {
			_, err = b.addFile("", BaseName(f.Name), f.Size, 0o644)
		}
		if err != nil {
			return protocol.Manifest{}, err
		}
	}
	m := b.manifest()
	if err := m.Validate(); err != nil {
		return protocol.Manifest{}, err
	}
	return m, nil
}
