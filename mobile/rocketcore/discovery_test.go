package rocketcore

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/donaldsteele/rocket/internal/discovery"
)

// The peer list keeps one order while beacons arrive, so a row does not
// move under a finger about to tap it.
func TestPeersSortedByName(t *testing.T) {
	phone := newPhone(t, "pixel")
	reg := phone.registry()
	see := func(name, fp string) {
		reg.Observe(discovery.ViaBeacon, discovery.Peer{Name: name, Fingerprint: fp, Addr: "10.0.0.9:29005", HTTPPort: 29005, OS: "windows"})
		time.Sleep(2 * time.Millisecond)
	}
	order := func() []string {
		var ps []struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal([]byte(phone.PeersJSON()), &ps); err != nil {
			t.Fatal(err)
		}
		names := make([]string, len(ps))
		for i, p := range ps {
			names[i] = p.Name
		}
		return names
	}
	see("zeta", "0000000000000001")
	see("Alpha", "0000000000000003")
	see("beta", "0000000000000002")
	want := []string{"Alpha", "beta", "zeta"}
	for round := 0; round < 3; round++ {
		if got := order(); len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
			t.Fatalf("round %d: peers %v, want %v", round, got, want)
		}
		// The next beacons arrive in another order.
		see("zeta", "0000000000000001")
		see("beta", "0000000000000002")
	}
}
