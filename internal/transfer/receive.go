package transfer

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

// PartSuffix marks a file still being received. It is renamed to its final
// name only after every announced byte has arrived.
const PartSuffix = ".rocket-part"

// ErrSize is wrapped when a body is shorter or longer than announced.
var ErrSize = errors.New("size does not match the offer")

// reserved holds Windows device names, which cannot be used as a file name
// (with or without an extension) on Windows.
var reserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true, "CONIN$": true, "CONOUT$": true,
	"COM0": true, "COM1": true, "COM2": true, "COM3": true, "COM4": true,
	"COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT0": true, "LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true,
	"LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// SanitizeComponent turns one untrusted name segment into a safe, portable
// file name on every OS: characters Windows forbids (<>:"/\|?*) and control
// characters become "_", trailing dots and spaces are trimmed, Windows device
// names (CON, NUL, COM1, ...) get a "_" prefix, an empty or dot-only result
// becomes "_", and the result is cut to 255 bytes on a rune boundary.
func SanitizeComponent(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(`<>:"/\|?*`, r) {
			b.WriteByte('_')
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimRight(b.String(), " .")
	if out == "" {
		return "_"
	}
	stem := out
	if i := strings.IndexByte(stem, '.'); i >= 0 {
		stem = stem[:i]
	}
	if reserved[strings.ToUpper(strings.TrimRight(stem, " "))] {
		out = "_" + out
	}
	for len(out) > 255 {
		_, size := utf8.DecodeLastRuneInString(out)
		out = out[:len(out)-size]
	}
	return out
}

// BaseName reduces an untrusted file name (for example a browser upload name,
// which may carry a client path) to a safe bare name.
func BaseName(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	return SanitizeComponent(name)
}

// SafeJoin joins a "/"-separated relative path (already checked with
// protocol.ValidatePath) under root, sanitizing every segment, and re-checks
// with filepath.Rel that the result stays inside root.
func SafeJoin(root, rel string) (string, error) {
	segs := strings.Split(rel, "/")
	for i, s := range segs {
		if s == "" || s == "." || s == ".." {
			return "", fmt.Errorf("unsafe path %q", rel)
		}
		segs[i] = SanitizeComponent(s)
	}
	root = filepath.Clean(root)
	full := filepath.Join(append([]string{root}, segs...)...)
	r, err := filepath.Rel(root, full)
	if err != nil || r == "." || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) || filepath.IsAbs(r) {
		return "", fmt.Errorf("path %q escapes the download folder", rel)
	}
	return full, nil
}

// UniquePath returns path, or path with " (n)" before the extension, such
// that neither it nor its .rocket-part twin exists.
func UniquePath(path string) string {
	ext := filepath.Ext(path)
	stem := path[:len(path)-len(ext)]
	for n := 0; ; n++ {
		cand := path
		if n > 0 {
			cand = stem + " (" + strconv.Itoa(n) + ")" + ext
		}
		if !exists(cand) && !exists(cand+PartSuffix) {
			return cand
		}
	}
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// ReceiveFile writes exactly size bytes from src into dir under the bare name
// name (sanitized, made unique). Bytes go to "<final>.rocket-part" first; the
// part file is renamed to its final name only when exactly size bytes arrived
// and src has nothing more. On any failure the part file is removed. It
// returns the final path.
func ReceiveFile(dir, name string, size int64, src io.Reader, progress ProgressFn) (string, error) {
	if size < 0 {
		return "", fmt.Errorf("%w: negative size", ErrSize)
	}
	base := filepath.Join(dir, BaseName(name))
	var f *os.File
	var final string
	for range 100 {
		final = UniquePath(base)
		var err error
		f, err = os.OpenFile(final+PartSuffix, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			break
		}
		if !os.IsExist(err) {
			return "", err
		}
	}
	if f == nil {
		return "", fmt.Errorf("cannot find a free name for %s", base)
	}
	part := f.Name()
	fail := func(err error) (string, error) {
		f.Close()
		os.Remove(part)
		return "", err
	}

	tr := newTracker(progress, size, 1)
	tr.file(BaseName(name))
	n, err := io.CopyN(trackWriter{w: f, t: tr}, src, size)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return fail(fmt.Errorf("%w: got %d of %d bytes", ErrSize, n, size))
		}
		return fail(err)
	}
	var one [1]byte
	if k, _ := io.ReadFull(src, one[:]); k > 0 {
		return fail(fmt.Errorf("%w: more than %d bytes sent", ErrSize, size))
	}
	if err := f.Close(); err != nil {
		os.Remove(part)
		return "", err
	}
	if exists(final) { // someone took the name meanwhile
		final = UniquePath(base)
	}
	if err := os.Rename(part, final); err != nil {
		os.Remove(part)
		return "", err
	}
	tr.finish()
	return final, nil
}
