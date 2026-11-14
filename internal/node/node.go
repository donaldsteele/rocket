// Package node holds this host's stable identity: an ed25519 keypair created on
// first run and persisted. Its fingerprint is the trust-on-first-use identity,
// so it survives IP and hostname changes.
package node

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
)

// Identity is this node's persistent identity.
type Identity struct {
	Name string             // human label, defaults to the hostname
	Pub  ed25519.PublicKey  // public key
	priv ed25519.PrivateKey // private key, never leaves this host
}

// FingerprintOf returns the fingerprint of a public key: the first 16 hex
// chars of its SHA-256.
func FingerprintOf(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])[:16]
}

// Fingerprint is the short, stable ID shown to users and carried in beacons.
// It is the first 16 hex chars of the SHA-256 of the public key.
func (id *Identity) Fingerprint() string { return FingerprintOf(id.Pub) }

// PublicKeyB64 is the public key in standard base64, as carried on the wire
// and pinned in the trust store.
func (id *Identity) PublicKeyB64() string { return base64.StdEncoding.EncodeToString(id.Pub) }

// Sign signs msg with the node's private key.
func (id *Identity) Sign(msg []byte) []byte { return ed25519.Sign(id.priv, msg) }

// Verify reports whether sig is a valid ed25519 signature of msg by pub. A
// public key of the wrong length never verifies.
func Verify(pub ed25519.PublicKey, msg, sig []byte) bool {
	if len(pub) != ed25519.PublicKeySize || len(sig) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(pub, msg, sig)
}

// DecodePublicKey parses a base64 public key as produced by PublicKeyB64.
func DecodePublicKey(b64 string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, err
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, errors.New("public key has the wrong length")
	}
	return ed25519.PublicKey(b), nil
}

// Load reads the identity from dataDir, creating it on first run. name is the
// human label; an empty name falls back to the hostname.
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
