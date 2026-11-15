package rocketcore

import (
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/donaldsteele/rocket/internal/config"
	"github.com/donaldsteele/rocket/internal/discovery"
	"github.com/donaldsteele/rocket/internal/history"
	"github.com/donaldsteele/rocket/internal/node"
	"github.com/donaldsteele/rocket/internal/peers"
	"github.com/donaldsteele/rocket/internal/receiver"
	"github.com/donaldsteele/rocket/internal/transfer"
)

// OS is what an Android host reports in hello, beacons and mDNS TXT.
const OS = "android"

// DefaultName is the device name used when none is given.
const DefaultName = "Android"

// DefaultBeaconPort is the UDP discovery beacon port.
const DefaultBeaconPort = config.DefaultBeaconPort

// DefaultPort is the HTTP port the receiver tries first.
const DefaultPort = config.DefaultHTTPPort

// defaultHistoryLimit is what HistoryJSON returns for a limit below 1.
const defaultHistoryLimit = 100

// Node is this phone as a rocket host.
type Node struct {
	version  string
	id       atomic.Pointer[node.Identity]
	trust    *peers.Store
	hist     *history.Store
	settings *config.SettingsStore
	ledger   *receiver.Ledger

	mu         sync.Mutex
	beaconPort int
	reg        *discovery.Registry // made on first use (registryLocked)
	disc       *discovering
	recv       *receiving
	sends      map[string]ProgressListener // running sends, by transfer ID
	events     EventListener               // the receiver's, while it runs
}

// NewNode opens (creating on first run) the identity, trust store, history
// and settings in dataDir, the app's private files folder. name is the
// device name shown to others ("" means DefaultName); version is the app
// version reported to peers.
func NewNode(dataDir, name, version string) (*Node, error) {
	if dataDir == "" {
		return nil, errors.New("no data folder")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	id, err := node.Load(dataDir, cleanName(name))
	if err != nil {
		return nil, err
	}
	trust, err := peers.Open(dataDir)
	if err != nil {
		return nil, err
	}
	hist, err := history.Open(dataDir)
	if err != nil {
		return nil, err
	}
	// A damaged settings.json falls back to the defaults; the next save
	// replaces it.
	settings, _ := config.OpenSettings(dataDir)
	if version == "" {
		version = "dev"
	}
	n := &Node{
		version: version, trust: trust, hist: hist, settings: settings,
		beaconPort: DefaultBeaconPort,
		sends:      map[string]ProgressListener{},
	}
	n.id.Store(id)
	n.ledger = &receiver.Ledger{Transfers: transfer.NewManager(), History: hist, Events: n.dispatch}
	return n, nil
}

func cleanName(name string) string { return receiver.CleanLabel(name, DefaultName) }

func (n *Node) identity() *node.Identity { return n.id.Load() }

// Name is the device name shown to others.
func (n *Node) Name() string { return n.identity().Name }

// SetName changes the device name. Sends use it at once; a running receiver
// and its beacon announce it after a restart.
func (n *Node) SetName(name string) {
	next := *n.identity()
	next.Name = cleanName(name)
	n.id.Store(&next)
}

// Fingerprint is this host's 16-hex-character fingerprint.
func (n *Node) Fingerprint() string { return n.identity().Fingerprint() }

// PublicKey is this host's ed25519 public key, base64.
func (n *Node) PublicKey() string { return n.identity().PublicKeyB64() }

// Version is the app version reported to peers.
func (n *Node) Version() string { return n.version }

// AutoAccept reports whether trusted senders skip the prompt.
func (n *Node) AutoAccept() bool { return n.settings.Get().AutoAcceptTrusted }

// SetAutoAccept turns auto-accept of trusted senders on or off and saves it.
func (n *Node) SetAutoAccept(on bool) error {
	s := n.settings.Get()
	s.AutoAcceptTrusted = on
	_, err := n.settings.Put(s)
	return err
}

// SetBeaconPort sets the UDP beacon port used by the discovery and the
// receiver started next; 0 turns the beacon off.
func (n *Node) SetBeaconPort(port int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if port != n.beaconPort && n.disc == nil {
		n.reg = nil // the registry listens on its own port
	}
	n.beaconPort = port
}

// HistoryJSON returns up to limit finished transfers, newest first
// ([history entry]; limit below 1 means 100).
func (n *Node) HistoryJSON(limit int) (string, error) {
	if limit < 1 {
		limit = defaultHistoryLimit
	}
	list, err := n.hist.List(min(limit, history.Cap))
	if err != nil {
		return "", err
	}
	return toJSON(list), nil
}

// ClearHistory forgets every finished transfer.
func (n *Node) ClearHistory() error { return n.hist.Clear() }

// TrustedJSON lists the trusted senders, most recently approved first
// ([{fingerprint, name, pub, approved_at}]).
func (n *Node) TrustedJSON() (string, error) {
	list := n.trust.List()
	sort.Slice(list, func(i, j int) bool { return list[i].ApprovedAt.After(list[j].ApprovedAt) })
	return toJSON(list), nil
}

// Revoke forgets trusted sender fp: its next offer prompts again.
func (n *Node) Revoke(fp string) error {
	fp = strings.ToLower(strings.TrimSpace(fp))
	if _, ok := n.trust.Get(fp); !ok {
		return errors.New("not a trusted sender")
	}
	return n.trust.Revoke(fp)
}

// TransfersJSON lists the transfers in flight, oldest first.
func (n *Node) TransfersJSON() string { return toJSON(n.ledger.Transfers.List()) }

// Cancel stops transfer id in either direction, or declines the incoming
// offer id while it waits for a decision. It reports whether there was such
// a transfer or offer.
func (n *Node) Cancel(id string) bool {
	if n.ledger.Transfers.Cancel(id) {
		return true
	}
	n.mu.Lock()
	r := n.recv
	n.mu.Unlock()
	return r != nil && r.rx.Answer(id, false, false)
}

// dispatch delivers an event as JSON: to the ProgressListener of the send it
// belongs to, else to the receiver's EventListener.
func (n *Node) dispatch(ev receiver.Event) {
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	tid := ""
	switch d := ev.Data.(type) {
	case map[string]any:
		tid, _ = d["transfer_id"].(string)
	case receiver.IncomingOffer:
		tid = d.TransferID
	}
	n.mu.Lock()
	send := n.sends[tid]
	events := n.events
	n.mu.Unlock()
	switch {
	case send != nil:
		send.OnEvent(string(b))
	case events != nil:
		events.OnEvent(string(b))
	}
}

func toJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}
