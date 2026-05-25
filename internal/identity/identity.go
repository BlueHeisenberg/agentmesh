// Package identity holds the per-process Ed25519 keypair this agentmesh
// instance uses for mTLS and for its mesh peer_id.
//
// Identity is ephemeral by design: a fresh keypair is generated at every
// `agentmesh serve` startup and exists only in memory. There is no on-disk
// persistence and no shared identity between sessions on the same machine.
// This is the property that makes multiple harness sessions on one machine
// mutually visible — each session has a distinct peer_id, so the registry's
// self-filter doesn't hide them from each other.
//
// The tradeoff: peer_id changes on every restart. `allow_peers` lists in
// mesh_share are session-scoped — if either side restarts, re-share. That's
// the right shape for chat-style use; if anyone needs persistent identities
// (long-lived allow lists, "remember this peer across days"), that's a v0.5+
// design discussion.
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Identity struct {
	PublicKey  ed25519.PublicKey
	PrivateKey ed25519.PrivateKey
}

// PeerID returns the hex-encoded public key — the stable identifier for this
// process across mDNS announcements, mTLS handshakes, and message envelopes.
func (i *Identity) PeerID() string { return hex.EncodeToString(i.PublicKey) }

// ShortID returns the first 16 hex chars of PeerID, suitable for display.
func (i *Identity) ShortID() string { return i.PeerID()[:16] }

// Tag returns the first 4 hex chars of PeerID, suitable for disambiguating
// same-project sessions in the default display name.
func (i *Identity) Tag() string { return i.PeerID()[:4] }

// Ephemeral generates a fresh Ed25519 keypair held only in memory.
func Ephemeral() (*Identity, error) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, fmt.Errorf("generate seed: %w", err)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return &Identity{
		PrivateKey: priv,
		PublicKey:  priv.Public().(ed25519.PublicKey),
	}, nil
}

// TLSCertificate builds a self-signed X.509 cert backed by the Ed25519 keypair.
// The cert's CommonName is the hex peer_id; its Subject Public Key is the
// Ed25519 public key. Peers can verify it by knowing the peer_id we advertised
// over mDNS. Same cert is used for both server and client auth (mTLS).
func (i *Identity) TLSCertificate() (tls.Certificate, error) {
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: i.PeerID()},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
			x509.ExtKeyUsageClientAuth,
		},
		BasicConstraintsValid: true,
		IsCA:                  true, // self-signed cert is its own CA
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, i.PublicKey, i.PrivateKey)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create cert: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  i.PrivateKey,
		Leaf:        leaf,
	}, nil
}

// DefaultDisplayName composes the agent-facing display name for this node
// from the current working directory and git branch, optionally tagged with
// a 4-hex slice of the peer_id for same-project disambiguation. Examples:
//
//	~/projects/harnessP2P  on branch main, tag "a3f2"  -> "harnessP2P@main#a3f2"
//	~/projects/foo         no git,         tag "7c1e"  -> "foo#7c1e"
//	/                      weird,          tag "0000"  -> "<host>#0000"
func DefaultDisplayName(tag string) string {
	base := nameBase()
	if tag != "" {
		return base + "#" + tag
	}
	return base
}

func nameBase() string {
	wd, err := os.Getwd()
	if err != nil || wd == "" {
		return fallbackHost()
	}
	base := filepath.Base(wd)
	if base == "/" || base == "." || base == "" {
		return fallbackHost()
	}
	if branch := gitBranchAt(wd); branch != "" {
		return base + "@" + branch
	}
	return base
}

func fallbackHost() string {
	h, _ := os.Hostname()
	if h == "" {
		return "anonymous"
	}
	return strings.TrimSuffix(h, ".local")
}

// gitBranchAt walks up from dir looking for a .git directory and reads HEAD
// directly (no shelling out to git). Returns the branch name, a short detached
// HEAD hash, or "".
func gitBranchAt(dir string) string {
	for i := 0; i < 20; i++ {
		head := filepath.Join(dir, ".git", "HEAD")
		if data, err := os.ReadFile(head); err == nil {
			s := strings.TrimSpace(string(data))
			if strings.HasPrefix(s, "ref: refs/heads/") {
				return strings.TrimPrefix(s, "ref: refs/heads/")
			}
			if len(s) >= 7 {
				return s[:7] // detached HEAD
			}
			return ""
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
	return ""
}
