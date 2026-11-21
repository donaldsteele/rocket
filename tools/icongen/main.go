// Command icongen draws the rocket app icon (a warm orange rocket on the dark
// theme background) and writes every generated icon file:
//
//	web/favicon.png                  64 px, the page icon
//	cmd/rocket/winres/icon-<n>.png   16-256 px, the Windows exe icon (go-winres)
//	tray/assets/icon.ico             tray icon for Windows (16-256 px)
//	tray/assets/icon.png             tray icon for Linux (64 px)
//	tray/assets/icon-pending.ico     the same with a badge: offers are waiting
//	tray/assets/icon-pending.png
//	tray/assets/icon-pairing.ico     the same with an amber badge: a pairing
//	tray/assets/icon-pairing.png     window is open (new senders are trusted)
//	android/app/src/main/res/mipmap-<density>/
//	    ic_launcher_foreground.png   adaptive launcher icon: the rocket alone
//	                                 on the 108 dp canvas (the background
//	                                 layer is the --bg colour, in XML)
//	    ic_launcher_monochrome.png   themed (Android 13+) launcher icon: the
//	                                 rocket's silhouette in white
//	android/app/src/main/res/drawable-<density>/
//	    ic_stat_rocket.png           24 dp white silhouette for notifications
//
// Run it from the repository root with the release toolchain:
//
//	GOTOOLCHAIN=go1.26.9 go run ./tools/icongen
//
// A newer Go draws the same pixels but compresses the PNGs differently (Go
// 1.27 does), which would rewrite every committed icon.
//
// The drawing uses integer arithmetic only (supersampled point-in-shape
// tests), so the output is byte-identical on every platform and every run.
// The generated files are committed; re-running must leave git clean.
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
)

// Theme colours (web/styles.css dark tokens).
var (
	colBG     = color.NRGBA{0x1c, 0x19, 0x17, 0xff} // --bg
	colEdge   = color.NRGBA{0x44, 0x40, 0x3c, 0xff} // --line
	colBody   = color.NRGBA{0xfb, 0x92, 0x3c, 0xff} // --accent
	colShade  = color.NRGBA{0xc2, 0x41, 0x0c, 0xff} // light --accent, the nozzle and fins
	colFlame  = color.NRGBA{0xfa, 0xcc, 0x15, 0xff} // --warn
	colCore   = color.NRGBA{0xfd, 0xe6, 0x8a, 0xff} // --focus
	colGlass  = color.NRGBA{0xf5, 0xf0, 0xe8, 0xff} // --fg
	colBadge  = color.NRGBA{0xf8, 0x71, 0x71, 0xff} // --bad
	colPair   = color.NRGBA{0xfa, 0xcc, 0x15, 0xff} // --warn: the pairing badge
	transpare = color.NRGBA{}
)

// winresSizes are the PNGs go-winres embeds as the exe icon.
var winresSizes = []int{16, 24, 32, 48, 64, 128, 256}

// icoSizes are the images inside each tray .ico.
var icoSizes = []int{16, 20, 24, 32, 40, 48, 64, 256}

// androidDensities are Android's screen density buckets and their scale
// factor in quarters (mdpi = 4/4 = 1 px per dp).
var androidDensities = []struct {
	name     string
	quarters int
}{
	{"mdpi", 4}, {"hdpi", 6}, {"xhdpi", 8}, {"xxhdpi", 12}, {"xxxhdpi", 16},
}

// Android icon canvases, in dp.
const (
	adaptiveDP = 108 // adaptive icon layer canvas
	statusDP   = 24  // notification (status bar) icon
)

// adaptiveCanvas is the side of the adaptive icon canvas in design units: the
// 1024-unit tile drawing sits in its middle (about 61 of the 108 dp), so the
// whole rocket, flame tip included, stays inside the 66 dp safe zone that
// every launcher mask keeps.
const adaptiveCanvas = 1800

// layer picks what a render draws.
type layer int

const (
	layerTile       layer = iota // the full rounded tile (favicon, exe, tray)
	layerPending                 // the tile with the "offers waiting" badge
	layerPairing                 // the tile with the "pairing open" badge
	layerForeground              // the rocket alone, transparent around it
	layerMonochrome              // the rocket's silhouette in white
)

var colWhite = color.NRGBA{0xff, 0xff, 0xff, 0xff}

func main() {
	root := flag.String("root", ".", "repository root to write into")
	flag.Parse()
	if _, err := os.Stat(filepath.Join(*root, "go.mod")); err != nil {
		fail(fmt.Errorf("run from the repository root (or pass -root): %w", err))
	}
	out := func(rel string, data []byte) {
		p := filepath.Join(*root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			fail(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			fail(err)
		}
		fmt.Println("wrote", rel)
	}

	out("web/favicon.png", encodePNG(render(64, false)))
	for _, n := range winresSizes {
		out(fmt.Sprintf("cmd/rocket/winres/icon-%d.png", n), encodePNG(render(n, false)))
	}
	for _, v := range []struct {
		name string
		l    layer
	}{{"icon", layerTile}, {"icon-pending", layerPending}, {"icon-pairing", layerPairing}} {
		imgs := make([]*image.NRGBA, 0, len(icoSizes))
		for _, n := range icoSizes {
			imgs = append(imgs, renderLayer(n, design, v.l))
		}
		out("tray/assets/"+v.name+".ico", encodeICO(imgs))
		out("tray/assets/"+v.name+".png", encodePNG(renderLayer(64, design, v.l)))
	}
	const res = "android/app/src/main/res/"
	for _, d := range androidDensities {
		n := adaptiveDP * d.quarters / 4
		out(res+"mipmap-"+d.name+"/ic_launcher_foreground.png", encodePNG(renderLayer(n, adaptiveCanvas, layerForeground)))
		out(res+"mipmap-"+d.name+"/ic_launcher_monochrome.png", encodePNG(renderLayer(n, adaptiveCanvas, layerMonochrome)))
		out(res+"drawable-"+d.name+"/ic_stat_rocket.png", encodePNG(renderLayer(statusDP*d.quarters/4, design, layerMonochrome)))
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "icongen:", err)
	os.Exit(1)
}

// design is the side of the square design space every shape is laid out in.
const design = 1024

// sampler tests one supersample point. Coordinates are in design units
// multiplied by k (see render), so that every sample centre is an integer.
type sampler struct {
	k int64 // scale: one design unit is k sample units
}

// sc scales a design length to sample units.
func (s sampler) sc(v int64) int64 { return v * s.k }

// render draws the icon tile (with the badge when pending) at n x n pixels.
func render(n int, pending bool) *image.NRGBA {
	l := layerTile
	if pending {
		l = layerPending
	}
	return renderLayer(n, design, l)
}

// renderLayer draws layer l at n x n pixels on a square canvas of canvas
// design units, with the 1024-unit drawing centred on it. Each pixel is the
// average of an ss x ss grid of samples; a sample takes the colour of the
// topmost shape that contains it, or is transparent.
func renderLayer(n int, canvas int64, l layer) *image.NRGBA {
	ss := int((canvas + int64(n) - 1) / int64(n)) // samples per pixel side; n*ss >= canvas
	if ss < 4 {
		ss = 4
	}
	grid := int64(n * ss)
	// Sample i (0..grid-1) has its centre at canvas coordinate
	// (2i+1)*canvas/(2*grid). Multiply through by 2*grid: x = (2i+1)*canvas
	// and a design length L becomes L*2*grid. The drawing starts at
	// (canvas-design)/2 on the canvas, so subtract that offset (an integer:
	// every canvas differs from design by an even amount).
	s := sampler{k: 2 * grid}
	off := s.sc((canvas - design) / 2)
	img := image.NewNRGBA(image.Rect(0, 0, n, n))
	for py := range n {
		for px := range n {
			var r, g, b, hit int64
			for sy := range ss {
				y := (2*int64(py*ss+sy)+1)*canvas - off
				for sx := range ss {
					x := (2*int64(px*ss+sx)+1)*canvas - off
					c := s.layerAt(x, y, l)
					if c.A == 0 {
						continue
					}
					r += int64(c.R)
					g += int64(c.G)
					b += int64(c.B)
					hit++
				}
			}
			if hit == 0 {
				continue
			}
			total := int64(ss * ss)
			img.SetNRGBA(px, py, color.NRGBA{
				R: uint8((r + hit/2) / hit),
				G: uint8((g + hit/2) / hit),
				B: uint8((b + hit/2) / hit),
				A: uint8((hit*255 + total/2) / total),
			})
		}
	}
	return img
}

// layerAt returns the colour of layer l at sample point (x, y).
func (s sampler) layerAt(x, y int64, l layer) color.NRGBA {
	switch l {
	case layerForeground:
		c, _ := s.rocket(x, y)
		return c
	case layerMonochrome:
		// The dark ring around the window stays open, so the window still
		// reads in a one-colour icon.
		if c, ok := s.rocket(x, y); ok && c != colBG {
			return colWhite
		}
		return transpare
	case layerPending:
		return s.colourAt(x, y, colBadge)
	case layerPairing:
		return s.colourAt(x, y, colPair)
	default:
		return s.colourAt(x, y, transpare)
	}
}

// colourAt returns the colour at sample point (x, y), topmost shape first;
// badge is the colour of the top-left badge, or transparent for none.
func (s sampler) colourAt(x, y int64, badge color.NRGBA) color.NRGBA {
	if !s.roundedSquare(x, y, 0, 196) {
		return transpare
	}
	if badge.A != 0 {
		// Badge top left: a dark ring cut out of everything, then the dot.
		if s.disc(x, y, 236, 236, 176) {
			return badge
		}
		if s.disc(x, y, 236, 236, 214) {
			return colBG
		}
	}
	if c, ok := s.rocket(x, y); ok {
		return c
	}
	// A one-step lighter rim so the dark tile reads on a dark taskbar.
	if !s.roundedSquare(x, y, 28, 168) {
		return colEdge
	}
	return colBG
}

// roundedSquare reports whether (x, y) is inside the icon square inset by
// inset design units with corner radius rad.
func (s sampler) roundedSquare(x, y, inset, rad int64) bool {
	lo, hi := s.sc(inset), s.sc(design-inset)
	if x < lo || x > hi || y < lo || y > hi {
		return false
	}
	r := s.sc(rad)
	cx := clamp(x, lo+r, hi-r)
	cy := clamp(y, lo+r, hi-r)
	dx, dy := x-cx, y-cy
	return dx*dx+dy*dy <= r*r
}

func (s sampler) disc(x, y, cx, cy, r int64) bool {
	dx, dy := x-s.sc(cx), y-s.sc(cy)
	rr := s.sc(r)
	return dx*dx+dy*dy <= rr*rr
}

// rocket draws the rocket flying to the top right. Its shapes are laid out in
// axis coordinates relative to the icon centre: t runs along the rocket
// (nose positive), u across it. With dx, dy the offset from the centre,
// t = dx - dy and u = dx + dy (both sqrt(2) times the true distance, which
// keeps the rotation in integers).
func (s sampler) rocket(x, y int64) (color.NRGBA, bool) {
	dx, dy := x-s.sc(design/2), y-s.sc(design/2)
	t, u := dx-dy, dx+dy
	// Shift the whole rocket a little towards the tail, so nose and flame
	// sit evenly in the tile.
	t -= s.sc(-40)

	// Window: glass inside a dark ring.
	if s.adisc(t, u, 190, 0, 50) {
		return colGlass, true
	}
	if s.adisc(t, u, 190, 0, 84) {
		return colBG, true
	}
	if s.body(t, u) {
		return colBody, true
	}
	// Fins, either side of the tail.
	for _, side := range []int64{1, -1} {
		if s.tri(t, u*side, [3][2]int64{{-20, 120}, {-330, 120}, {-430, 330}}) {
			return colShade, true
		}
	}
	// Nozzle.
	if t >= s.sc(-380) && t < s.sc(-290) && abs(u) <= s.sc(96) {
		return colShade, true
	}
	// Flame: a teardrop behind the nozzle, with a pale core.
	if s.adisc(t, u, -450, 0, 48) || s.tri(t, u, [3][2]int64{{-430, 46}, {-430, -46}, {-570, 0}}) {
		return colCore, true
	}
	if s.adisc(t, u, -450, 0, 92) || s.tri(t, u, [3][2]int64{{-420, 90}, {-420, -90}, {-720, 0}}) {
		return colFlame, true
	}
	return transpare, false
}

// body is the fuselage: a straight tube from the tail to the shoulder, then
// an ogive nose (the overlap of two large discs).
func (s sampler) body(t, u int64) bool {
	const (
		tail     = -300
		shoulder = 210
		half     = 150 // half width of the tube
		R        = 620 // nose disc radius
	)
	if t < s.sc(tail) {
		return false
	}
	if t <= s.sc(shoulder) {
		return abs(u) <= s.sc(half)
	}
	c := int64(R - half) // disc centres sit c across the axis, at the shoulder
	return s.adisc(t, u, shoulder, c, R) && s.adisc(t, u, shoulder, -c, R)
}

// adisc is a disc in axis coordinates.
func (s sampler) adisc(t, u, ct, cu, r int64) bool {
	a, b := t-s.sc(ct), u-s.sc(cu)
	rr := s.sc(r)
	return a*a+b*b <= rr*rr
}

// tri reports whether (t, u) is inside the triangle with design-unit
// vertices v (either winding).
func (s sampler) tri(t, u int64, v [3][2]int64) bool {
	var neg, pos bool
	for i := range 3 {
		ax, ay := s.sc(v[i][0]), s.sc(v[i][1])
		// Edge vector in design units: the cross product then stays far
		// inside int64 (sample units are at most ~2^23).
		ex, ey := v[(i+1)%3][0]-v[i][0], v[(i+1)%3][1]-v[i][1]
		cr := ex*(u-ay) - ey*(t-ax)
		if cr < 0 {
			neg = true
		} else if cr > 0 {
			pos = true
		}
	}
	return !(neg && pos)
}

func clamp(v, lo, hi int64) int64 {
	return max(lo, min(v, hi))
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func encodePNG(img image.Image) []byte {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, img); err != nil {
		fail(err)
	}
	return buf.Bytes()
}

// encodeICO writes a Windows .ico. Images up to 64 px are stored as 32-bit
// BMP (DIB) entries, which every Windows icon loader accepts; larger ones as
// PNG, as Windows Vista and later expect for 256 px.
func encodeICO(imgs []*image.NRGBA) []byte {
	type entry struct {
		size int
		data []byte
	}
	entries := make([]entry, 0, len(imgs))
	for _, img := range imgs {
		n := img.Bounds().Dx()
		var data []byte
		if n > 64 {
			data = encodePNG(img)
		} else {
			data = dib(img)
		}
		entries = append(entries, entry{n, data})
	}
	var buf bytes.Buffer
	le := func(v any) { _ = binary.Write(&buf, binary.LittleEndian, v) }
	le(uint16(0))            // reserved
	le(uint16(1))            // type: icon
	le(uint16(len(entries))) // count
	offset := 6 + 16*len(entries)
	for _, e := range entries {
		dim := uint8(e.size)
		if e.size >= 256 {
			dim = 0 // 0 means 256
		}
		buf.WriteByte(dim) // width
		buf.WriteByte(dim) // height
		buf.WriteByte(0)   // palette colours
		buf.WriteByte(0)   // reserved
		le(uint16(1))      // colour planes
		le(uint16(32))     // bits per pixel
		le(uint32(len(e.data)))
		le(uint32(offset))
		offset += len(e.data)
	}
	for _, e := range entries {
		buf.Write(e.data)
	}
	return buf.Bytes()
}

// dib is one icon image as a BITMAPINFOHEADER, bottom-up BGRA pixels and the
// 1-bit AND mask (all zero: the alpha channel decides transparency, but old
// loaders still expect the mask to be there).
func dib(img *image.NRGBA) []byte {
	n := img.Bounds().Dx()
	var buf bytes.Buffer
	le := func(v any) { _ = binary.Write(&buf, binary.LittleEndian, v) }
	le(uint32(40))   // header size
	le(int32(n))     // width
	le(int32(2 * n)) // height: XOR image plus AND mask
	le(uint16(1))    // planes
	le(uint16(32))   // bits per pixel
	le(uint32(0))    // BI_RGB
	le(uint32(0))    // image size (may be 0 for BI_RGB)
	le(int32(0))     // x pixels per metre
	le(int32(0))     // y pixels per metre
	le(uint32(0))    // colours used
	le(uint32(0))    // important colours
	for y := n - 1; y >= 0; y-- {
		for x := range n {
			c := img.NRGBAAt(x, y)
			buf.Write([]byte{c.B, c.G, c.R, c.A})
		}
	}
	stride := ((n + 31) / 32) * 4
	buf.Write(make([]byte, stride*n))
	return buf.Bytes()
}
