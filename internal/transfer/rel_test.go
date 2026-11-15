package transfer

import (
	"slices"
	"testing"

	"github.com/donaldsteele/rocket/internal/protocol"
)

func TestRelManifest(t *testing.T) {
	m, index, err := RelManifest([]RelItem{
		{Path: "album", Dir: true},
		{Path: "album/a.jpg", Size: 3},
		{Path: "album/sub/b.txt", Size: 1},
		{Path: "album/empty", Dir: true},
		{Path: "album/a.jpg", Size: 2},      // same name again
		{Path: `..\..\evil\c.txt`, Size: 4}, // client path junk
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"album/", "album/a.jpg", "album/sub/", "album/sub/b.txt", "album/empty/", "album/a (1).jpg", "evil/", "evil/c.txt"}
	if got := paths(m); !slices.Equal(got, want) {
		t.Fatalf("paths %q, want %q", got, want)
	}
	if want := []int{-1, 1, -1, 2, -1, 4, -1, 5}; !slices.Equal(index, want) {
		t.Fatalf("index %v, want %v", index, want)
	}
	if m.Kind != protocol.KindMulti || m.Total != 10 || m.Entries[4].Mode&protocol.ModeDir == 0 {
		t.Fatalf("manifest %+v", m)
	}

	m, index, err = RelManifest([]RelItem{{Path: "photo.jpg", Size: 7}})
	if err != nil || m.Kind != protocol.KindFile || !slices.Equal(index, []int{0}) {
		t.Fatalf("one file: %+v %v %v", m, index, err)
	}
	if _, _, err := RelManifest(nil); err == nil {
		t.Fatal("empty list accepted")
	}
	if _, _, err := RelManifest([]RelItem{{Path: "x", Size: -1}}); err == nil {
		t.Fatal("negative size accepted")
	}
}
