package discovery

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"
)

// TestListenRecordsBeacons: Listen records a beacon (sent here straight to
// the port rather than broadcast) and stops with its context.
func TestListenRecordsBeacons(t *testing.T) {
	const port = 29451
	r := New(Self{Name: "me", Fingerprint: "1111111111111111"}, port)
	ctx, cancel := context.WithCancel(context.Background())
	if err := r.Listen(ctx); err != nil {
		t.Fatal(err)
	}
	if err := New(Self{}, port).Listen(ctx); err == nil {
		t.Fatal("a second Listen on the same port succeeded")
	}
	c, err := net.Dial("udp4", "127.0.0.1:29451")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	pkt, _ := json.Marshal(Self{Name: "phone", Fingerprint: "abcdefabcdefabcd", HTTPPort: 29005, Version: "1.0.0", OS: "android"})
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, _ = c.Write(pkt)
		if ps := r.Peers(); len(ps) == 1 {
			if p := ps[0]; p.OS != "android" || p.Addr != "127.0.0.1:29005" {
				t.Fatalf("peer %+v", p)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("beacon not recorded")
		}
		time.Sleep(20 * time.Millisecond)
	}
	r.Forget(ViaBeacon, "abcdefabcdefabcd")
	if n := len(r.Peers()); n != 0 {
		t.Fatalf("%d peers after Forget", n)
	}
	cancel()
	// The port is free again once the context ends.
	deadline = time.Now().Add(5 * time.Second)
	for {
		ctx2, cancel2 := context.WithCancel(context.Background())
		err := New(Self{}, port).Listen(ctx2)
		cancel2()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("port still held: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
