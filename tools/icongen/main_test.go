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
