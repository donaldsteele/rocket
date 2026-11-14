package peers

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DropTokenTTL is how long a browser guest stays remembered after its first
// accepted drop.
const DropTokenTTL = 30 * 24 * time.Hour

// maxDropTokens bounds the store; the oldest tokens are dropped first.
const maxDropTokens = 1000

// DropToken is one remembered browser. Only the SHA-256 of the token is
// stored, never the token itself.
type DropToken struct {
	TokenHash string    `json:"token_hash"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// DropTokens is the on-disk store of browser drop tokens (drop_tokens.json),
// safe for concurrent use.
type DropTokens struct {
	path string
	mu   sync.Mutex
	list []DropToken
	// Now returns the current time; tests override it.
	Now func() time.Time
}

// OpenDropTokens loads drop_tokens.json from dataDir (absent means empty) and
// discards expired tokens.
func OpenDropTokens(dataDir string) (*DropTokens, error) {
	d := &DropTokens{path: filepath.Join(dataDir, "drop_tokens.json"), Now: time.Now}
	b, err := os.ReadFile(d.path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err == nil {
		if err := json.Unmarshal(b, &d.list); err != nil {
			return nil, err
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.prune()
	return d, nil
}

// Issue creates, stores and returns a new random token.
func (d *DropTokens) Issue() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b[:])
	now := d.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.list = append(d.list, DropToken{TokenHash: hashToken(token), CreatedAt: now, ExpiresAt: now.Add(DropTokenTTL)})
	d.prune()
	return token, writeJSONAtomic(d.path, d.list)
}

// Valid reports whether token is a stored, unexpired token.
func (d *DropTokens) Valid(token string) bool {
	if token == "" {
		return false
	}
	h := hashToken(token)
	now := d.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, t := range d.list {
		if subtle.ConstantTimeCompare([]byte(t.TokenHash), []byte(h)) == 1 && now.Before(t.ExpiresAt) {
			return true
		}
	}
	return false
}

// prune drops expired tokens and caps the list. Caller holds d.mu.
func (d *DropTokens) prune() {
	now := d.Now()
	kept := d.list[:0]
	for _, t := range d.list {
		if now.Before(t.ExpiresAt) {
			kept = append(kept, t)
		}
	}
	if len(kept) > maxDropTokens {
		kept = kept[len(kept)-maxDropTokens:]
	}
	d.list = kept
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
