package httpapi

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/donaldsteele/rocket/internal/discovery"
)

func TestPeersIncludeVia(t *testing.T) {
	h := newTestHost(t, "host")
	fp := "abcdefabcdefabcd"
	h.srv.reg.Observe(discovery.ViaBeacon, discovery.Peer{Name: "desk", Fingerprint: fp, Addr: "192.168.1.30:29005", HTTPPort: 29005, OS: "windows", Version: "1.0.0"})
	h.srv.reg.Observe(discovery.ViaMDNS, discovery.Peer{Name: "desk", Fingerprint: fp, Addr: "192.168.1.30:29005", HTTPPort: 29005, OS: "windows", Version: "1.0.0"})

	code, body := h.call("GET", "/api/peers", nil, false)
	var list []struct {
		Name string   `json:"name"`
		FP   string   `json:"fp"`
		Addr string   `json:"addr"`
		OS   string   `json:"os"`
		Via  []string `json:"via"`
	}
	if err := json.Unmarshal(body, &list); code != 200 || err != nil || len(list) != 1 {
		t.Fatalf("peers: %d %s", code, body)
	}
	p := list[0]
	if p.FP != fp || p.Name != "desk" || p.Addr != "192.168.1.30:29005" || p.OS != "windows" {
		t.Fatalf("peer = %+v", p)
	}
	if !slices.Equal(p.Via, []string{"beacon", "mdns"}) {
		t.Fatalf("via = %v, want [beacon mdns]", p.Via)
	}
}
