// Package identity provides an Ed25519 keypair identity for P2P nodes.
//
// The hex-encoded public key is the node's peer_id — the stable identifier
// across mDNS announcements, mTLS handshakes, and message envelopes.
// TLSCertificate builds the self-signed X.509 cert (CommonName = peer_id)
// that pkg/transport uses for mutual TLS.
//
// Identities can be ephemeral (Ephemeral generates a fresh in-memory keypair;
// agentmesh's per-session model) or persistent (FromPrivateKey rebuilds an
// Identity from a stored Ed25519 private key; lore's per-user model).
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
	"time"
)

type Identity struct {
	PublicKey  ed25519.PublicKey
	PrivateKey ed25519.PrivateKey
}

// PeerID returns the hex-encoded public key — the stable identifier for this
// node across mDNS announcements, mTLS handshakes, and message envelopes.
func (i *Identity) PeerID() string { return hex.EncodeToString(i.PublicKey) }

// ShortID returns the first 16 hex chars of PeerID, suitable for display.
func (i *Identity) ShortID() string { return i.PeerID()[:16] }

// Tag returns the first 4 hex chars of PeerID, suitable for disambiguating
// same-project sessions in a default display name.
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

// FromPrivateKey builds an Identity from an existing Ed25519 private key.
// Used by callers with persistent identities (the key loaded from disk or a
// vault) rather than the ephemeral per-process model.
func FromPrivateKey(priv ed25519.PrivateKey) *Identity {
	return &Identity{
		PrivateKey: priv,
		PublicKey:  priv.Public().(ed25519.PublicKey),
	}
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
