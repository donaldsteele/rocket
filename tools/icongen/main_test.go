package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/png"
	"testing"
)

func TestRenderDeterministic(t *testing.T) {
	a, b := encodePNG(render(32, false)), encodePNG(render(32, false))
	if !bytes.Equal(a, b) {
		t.Fatal("two renders differ")
	}
	if bytes.Equal(a, encodePNG(render(32, true))) {
		t.Fatal("pending icon looks the same as the plain one")
	}
}

func TestRenderShape(t *testing.T) {
	img := render(64, false)
	if c := img.NRGBAAt(0, 0); c.A != 0 {
		t.Errorf("corner pixel is not transparent: %v", c)
	}
	if c := img.NRGBAAt(32, 32); c.A != 255 {
		t.Errorf("centre pixel is not opaque: %v", c)
	}
	// Somewhere on the rocket is the accent orange.
	found := false
	for y := range 64 {
		for x := range 64 {
			if img.NRGBAAt(x, y) == colBody {
				found = true
			}
		}
	}
	if !found {
		t.Error("no accent-orange pixel: the rocket is missing")
	}
}

func TestAndroidLayers(t *testing.T) {
	fg := renderLayer(108, adaptiveCanvas, layerForeground)
	if c := fg.NRGBAAt(54, 54); c.A != 255 {
		t.Errorf("foreground centre is not opaque: %v", c)
	}
	// Outside the 66 dp safe zone (radius 33 around the centre) the
	// foreground is empty, so no launcher mask clips the rocket.
	for y := range 108 {
		for x := range 108 {
			dx, dy := x-54, y-54
			if dx*dx+dy*dy > 33*33 && fg.NRGBAAt(x, y).A != 0 {
				t.Fatalf("foreground pixel (%d,%d) outside the safe zone", x, y)
			}
		}
	}
	mono := renderLayer(108, adaptiveCanvas, layerMonochrome)
	opaque := 0
	for y := range 108 {
		for x := range 108 {
			c := mono.NRGBAAt(x, y)
			if c.A > 0 && (c.R != 0xff || c.G != 0xff || c.B != 0xff) {
				t.Fatalf("monochrome pixel (%d,%d) is %v, not white", x, y, c)
			}
			if c.A == 255 {
				opaque++
			}
		}
	}
	if opaque == 0 {
		t.Error("monochrome icon is empty")
	}
	// The tile still renders exactly as before the canvas was generalised.
	if !bytes.Equal(encodePNG(render(48, false)), encodePNG(renderLayer(48, design, layerTile))) {
		t.Error("render and renderLayer disagree for the tile")
	}
}

func TestICO(t *testing.T) {
	data := encodeICO([]*image.NRGBA{render(16, false), render(256, false)})
	var hdr struct{ Reserved, Type, Count uint16 }
	if err := binary.Read(bytes.NewReader(data), binary.LittleEndian, &hdr); err != nil {
		t.Fatal(err)
	}
	if hdr.Reserved != 0 || hdr.Type != 1 || hdr.Count != 2 {
		t.Fatalf("header %+v", hdr)
	}
	type dirent struct {
		W, H, Colors, Res uint8
		Planes, BPP       uint16
		Size, Offset      uint32
	}
	ents := make([]dirent, 2)
	if err := binary.Read(bytes.NewReader(data[6:]), binary.LittleEndian, &ents); err != nil {
		t.Fatal(err)
	}
	if ents[0].W != 16 || ents[1].W != 0 {
		t.Fatalf("sizes %d, %d", ents[0].W, ents[1].W)
	}
	// 16 px: a DIB of header + pixels + AND mask, height doubled.
	dibData := data[ents[0].Offset : ents[0].Offset+ents[0].Size]
	if want := 40 + 16*16*4 + 4*16; len(dibData) != want {
		t.Errorf("16 px DIB is %d bytes, want %d", len(dibData), want)
	}
	if h := int32(binary.LittleEndian.Uint32(dibData[8:])); h != 32 {
		t.Errorf("DIB height %d, want 32", h)
	}
	// 256 px: an embedded PNG.
	pngData := data[ents[1].Offset : ents[1].Offset+ents[1].Size]
	img, err := png.Decode(bytes.NewReader(pngData))
	if err != nil {
		t.Fatalf("256 px entry: %v", err)
	}
	if img.Bounds().Dx() != 256 {
		t.Errorf("256 px entry is %d wide", img.Bounds().Dx())
	}
	if int(ents[1].Offset+ents[1].Size) != len(data) {
		t.Error("trailing bytes after the last image")
	}
}
