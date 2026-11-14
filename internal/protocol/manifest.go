package protocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"
)

// CanonicalJSON is the manifest encoding the offer signature covers: compact
// JSON, fields in the order kind, entries (path, size, mode), total, with no
// HTML escaping and no trailing newline. A nil entry list encodes as [].
func (m Manifest) CanonicalJSON() ([]byte, error) {
	if m.Entries == nil {
		m.Entries = []Entry{}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// Hash is hex(sha256(CanonicalJSON)).
func (m Manifest) Hash() (string, error) {
	b, err := m.CanonicalJSON()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// FileManifest builds a kind:file manifest for one file named name (a base
// name, no directories).
func FileManifest(name string, size int64, mode uint32) Manifest {
	return Manifest{Kind: KindFile, Entries: []Entry{{Path: name, Size: size, Mode: mode & 0o777}}, Total: size}
}

// Validate checks a manifest against the v1 rules: a known kind, 1 to
// MaxEntries entries (exactly one non-directory entry with a bare name for
// kind:file), every path valid (ValidatePath), no duplicate paths (compared
// case-insensitively, since receivers may be on Windows or macOS), no file
// used as a directory of another entry, non-negative sizes, directories of
// size 0, sizes summing to Total, and Total <= MaxTotal. The returned error
// wraps ErrManifest.
func (m Manifest) Validate() error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s", ErrManifest, fmt.Sprintf(format, a...))
	}
	switch m.Kind {
	case KindFile:
		if len(m.Entries) != 1 {
			return bad("kind file needs exactly one entry, got %d", len(m.Entries))
		}
		e := m.Entries[0]
		if e.Mode&ModeDir != 0 {
			return bad("kind file cannot be a directory")
		}
		if strings.Contains(e.Path, "/") {
			return bad("kind file needs a bare file name")
		}
	case KindMulti:
		if len(m.Entries) == 0 {
			return bad("no entries")
		}
	default:
		return bad("unknown kind %q", m.Kind)
	}
	if len(m.Entries) > MaxEntries {
		return bad("%d entries exceeds the limit of %d", len(m.Entries), MaxEntries)
	}
	if m.Total < 0 || m.Total > MaxTotal {
		return bad("total %d bytes is outside 0..%d", m.Total, MaxTotal)
	}

	seen := make(map[string]bool, len(m.Entries))
	files := make(map[string]bool, len(m.Entries))
	var sum int64
	for i, e := range m.Entries {
		if err := ValidatePath(e.Path); err != nil {
			return bad("entry %d: %v", i, err)
		}
		if e.Mode&^(ModeDir|0o777) != 0 {
			return bad("entry %d: unsupported mode bits %#o", i, e.Mode)
		}
		if e.Size < 0 {
			return bad("entry %d: negative size", i)
		}
		if e.Mode&ModeDir != 0 && e.Size != 0 {
			return bad("entry %d: directory with a size", i)
		}
		key := strings.ToLower(e.Path)
		if seen[key] {
			return bad("entry %d: duplicate path %q", i, e.Path)
		}
		seen[key] = true
		if e.Mode&ModeDir == 0 {
			files[key] = true
		}
		sum += e.Size
		if sum > MaxTotal {
			return bad("sizes exceed %d bytes", MaxTotal)
		}
	}
	if sum != m.Total {
		return bad("entry sizes sum to %d, total says %d", sum, m.Total)
	}
	// A file must not also be a parent directory of another entry.
	for key := range seen {
		for dir := path.Dir(key); dir != "."; dir = path.Dir(dir) {
			if files[dir] {
				return bad("%q is a file and also a directory", dir)
			}
		}
	}
	return nil
}

// ValidatePath checks one manifest path: non-empty, valid UTF-8, at most
// MaxPathBytes (segments at most MaxSegment), relative with "/" separators,
// already clean (no ".", ".." or empty segments, no trailing slash), and free
// of NUL and control characters, backslashes (a Windows separator) and colons
// (Windows volumes and alternate data streams). This rejects absolute paths,
// volume paths such as C:\x or C:x, and UNC paths such as //server/share.
func ValidatePath(p string) error {
	switch {
	case p == "":
		return fmt.Errorf("empty path")
	case len(p) > MaxPathBytes:
		return fmt.Errorf("path longer than %d bytes", MaxPathBytes)
	case !utf8.ValidString(p):
		return fmt.Errorf("path is not valid UTF-8")
	case strings.HasPrefix(p, "/"):
		return fmt.Errorf("absolute path %q", p)
	}
	for _, r := range p {
		switch {
		case r == 0:
			return fmt.Errorf("NUL in path")
		case r < 0x20 || r == 0x7f:
			return fmt.Errorf("control character in path %q", p)
		case r == '\\':
			return fmt.Errorf("backslash in path %q", p)
		case r == ':':
			return fmt.Errorf("colon in path %q", p)
		}
	}
	if path.Clean(p) != p {
		return fmt.Errorf("path %q is not clean", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == "." || seg == "" {
			return fmt.Errorf("path %q has a %q segment", p, seg)
		}
		if len(seg) > MaxSegment {
			return fmt.Errorf("path segment longer than %d bytes", MaxSegment)
		}
	}
	return nil
}
