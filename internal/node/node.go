// Package node holds this host's stable identity: an ed25519 keypair created on
// first run and persisted. Its fingerprint is the trust-on-first-use identity,
// so it survives IP and hostname changes.
package node

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
)

// Identity is this node's persistent identity.
type Identity struct {
	Name string             // human label, defaults to the hostname
	Pub  ed25519.PublicKey  // public key
	priv ed25519.PrivateKey // private key, never leaves this host
}

// Fingerprint is the short, stable ID shown to users and carried in beacons.
// It is the first 16 hex chars of the SHA-256 of the public key.
func (id *Identity) Fingerprint() string {
	sum := sha256.Sum256(id.Pub)
	return hex.EncodeToString(sum[:])[:16]
}

// Sign signs msg with the node's private key.
func (id *Identity) Sign(msg []byte) []byte { return ed25519.Sign(id.priv, msg) }

// Load reads the identity from dataDir, creating it on first run. name is used
// only when a new identity is created; an empty name falls back to the hostname.
func Load(dataDir, name string) (*Identity, error) {
	keyPath := filepath.Join(dataDir, "node.key")
	if name == "" {
		name, _ = os.Hostname()
	}
	if b, err := os.ReadFile(keyPath); err == nil && len(b) == ed25519.PrivateKeySize {
		priv := ed25519.PrivateKey(b)
		return &Identity{Name: name, Pub: priv.Public().(ed25519.PublicKey), priv: priv}, nil
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, priv, 0o600); err != nil {
		return nil, err
	}
	return &Identity{Name: name, Pub: pub, priv: priv}, nil
}
