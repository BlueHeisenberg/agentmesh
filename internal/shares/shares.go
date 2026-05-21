// Package shares is the registry of blobs the agent has explicitly made
// shareable. Nothing on disk is reachable unless registered here.
package shares

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var (
	ErrNotFound  = errors.New("share not found")
	ErrForbidden = errors.New("share not permitted for this peer")
	ErrExpired   = errors.New("share expired")
)

type Share struct {
	Handle     string    `json:"handle"`
	Name       string    `json:"name"`
	Path       string    `json:"path,omitempty"` // absolute path on disk
	Bytes      []byte    `json:"-"`              // inline bytes (path empty)
	Size       int64     `json:"size"`
	AllowPeers []string  `json:"allow_peers,omitempty"` // nil/empty => anyone
	ExpiresAt  time.Time `json:"expires_at,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

type Registry struct {
	mu     sync.RWMutex
	shares map[string]*Share
}

func New() *Registry { return &Registry{shares: map[string]*Share{}} }

func randHandle() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// AddFile registers a file path. The file must be readable now; we re-open on
// fetch so subsequent edits are visible.
func (r *Registry) AddFile(path string, name string, allow []string, ttl time.Duration) (*Share, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, errors.New("path is a directory; only files are supported")
	}
	if name == "" {
		name = filepath.Base(abs)
	}
	s := &Share{
		Handle:     randHandle(),
		Name:       name,
		Path:       abs,
		Size:       info.Size(),
		AllowPeers: allow,
		CreatedAt:  time.Now(),
	}
	if ttl > 0 {
		s.ExpiresAt = s.CreatedAt.Add(ttl)
	}
	r.mu.Lock()
	r.shares[s.Handle] = s
	r.mu.Unlock()
	return s, nil
}

// AddBytes registers inline bytes (handy for small payloads from the agent).
func (r *Registry) AddBytes(name string, data []byte, allow []string, ttl time.Duration) *Share {
	s := &Share{
		Handle:     randHandle(),
		Name:       name,
		Bytes:      data,
		Size:       int64(len(data)),
		AllowPeers: allow,
		CreatedAt:  time.Now(),
	}
	if ttl > 0 {
		s.ExpiresAt = s.CreatedAt.Add(ttl)
	}
	r.mu.Lock()
	r.shares[s.Handle] = s
	r.mu.Unlock()
	return s
}

func (r *Registry) Remove(handle string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.shares[handle]; !ok {
		return false
	}
	delete(r.shares, handle)
	return true
}

func (r *Registry) List() []Share {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Share, 0, len(r.shares))
	for _, s := range r.shares {
		out = append(out, *s)
	}
	return out
}

// Resolve returns the share if allowed for callerPeerID. callerPeerID may be ""
// for unauthenticated (we treat that as "anyone allowlist passes through").
func (r *Registry) Resolve(handle, callerPeerID string) (*Share, error) {
	r.mu.RLock()
	s, ok := r.shares[handle]
	r.mu.RUnlock()
	if !ok {
		return nil, ErrNotFound
	}
	if !s.ExpiresAt.IsZero() && time.Now().After(s.ExpiresAt) {
		r.Remove(handle)
		return nil, ErrExpired
	}
	if len(s.AllowPeers) > 0 {
		allowed := false
		for _, p := range s.AllowPeers {
			if p == callerPeerID {
				allowed = true
				break
			}
		}
		if !allowed {
			return nil, ErrForbidden
		}
	}
	return s, nil
}
