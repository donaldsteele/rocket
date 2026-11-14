package discovery

import (
	"context"
	"encoding/json"
	"net"
	"strconv"
	"time"
)

// The UDP beacon: each host periodically broadcasts a JSON Self on the beacon
// port, and every host listens and records the senders. Stdlib only; works on
// a flat IPv4 LAN.

const (
	announceInterval = 2 * time.Second
	maxPacket        = 2048
)

// Run announces this host over the UDP beacon and listens for other hosts'
// beacons until ctx is cancelled.
func (r *Registry) Run(ctx context.Context) error {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: r.beaconPort})
	if err != nil {
		return err
	}
	go r.listen(ctx, conn)
	go r.announce(ctx)
	<-ctx.Done()
	return conn.Close()
}

func (r *Registry) announce(ctx context.Context) {
	bcast := &net.UDPAddr{IP: net.IPv4bcast, Port: r.beaconPort}
	out, err := net.DialUDP("udp4", nil, bcast)
	if err != nil {
		return
	}
	defer out.Close()
	payload, _ := json.Marshal(r.self)
	t := time.NewTicker(announceInterval)
	defer t.Stop()
	_, _ = out.Write(payload) // announce immediately
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_, _ = out.Write(payload)
		}
	}
}

func (r *Registry) listen(ctx context.Context, conn *net.UDPConn) {
	buf := make([]byte, maxPacket)
	for {
		if ctx.Err() != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			continue
		}
		if p, ok := beaconPeer(buf[:n], src); ok {
			r.Observe(ViaBeacon, p)
		}
	}
}

// beaconPeer decodes one beacon packet from src. The HTTP address is the
// packet's source IP with the announced port.
func beaconPeer(pkt []byte, src *net.UDPAddr) (Peer, bool) {
	var s Self
	if json.Unmarshal(pkt, &s) != nil || s.Fingerprint == "" || src == nil {
		return Peer{}, false
	}
	if s.HTTPPort <= 0 || s.HTTPPort > 65535 {
		return Peer{}, false
	}
	return Peer{
		Name: s.Name, Fingerprint: s.Fingerprint,
		Addr:     net.JoinHostPort(src.IP.String(), strconv.Itoa(s.HTTPPort)),
		HTTPPort: s.HTTPPort, Version: s.Version, OS: s.OS,
	}, true
}
