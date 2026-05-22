// Package identity persists an Ed25519 keypair + display name for this node.
// Stored at $AGENTMESH_HOME or ~/.agentmesh/identity.json.
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"
)

type Identity struct {
	Name       string             `json:"name"`
	Seed       string             `json:"seed_hex"`
	PublicKey  ed25519.PublicKey  `json:"-"`
	PrivateKey ed25519.PrivateKey `json:"-"`
}

func (i *Identity) PeerID() string  { return hex.EncodeToString(i.PublicKey) }
func (i *Identity) ShortID() string { return i.PeerID()[:16] }

// TLSCertificate builds a self-signed X.509 cert backed by the Ed25519 keypair.
// The cert's CommonName is the hex peer_id, and its Subject Public Key is the
// Ed25519 public key — so peers can verify it just by knowing the peer_id we
// advertised over mDNS. Same cert is used for both server and client auth (mTLS).
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
		IsCA:                  true, // self-signed, so the cert is its own CA
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

// DefaultDisplayName picks a meaningful name for this agentmesh instance
// based on the current working directory and git branch — the place the
// harness was launched from. Examples:
//
//	~/projects/harnessP2P   on branch main         -> "harnessP2P@main"
//	~/projects/harnessP2P   no git                 -> "harnessP2P"
//	/                       (root, weird)          -> hostname or "anonymous"
//
// Falls back to the machine hostname if CWD can't be read or yields nothing
// useful. Used when the binary is invoked without an explicit --name flag.
func DefaultDisplayName() string {
	host, _ := os.Hostname()

	wd, err := os.Getwd()
	if err != nil || wd == "" {
		return fallbackName(host)
	}
	base := filepath.Base(wd)
	if base == "/" || base == "." || base == "" {
		return fallbackName(host)
	}

	if branch := gitBranchAt(wd); branch != "" {
		return base + "@" + branch
	}
	return base
}

func fallbackName(host string) string {
	if host != "" {
		// Strip the trailing ".local" mDNS hosts often carry on macOS.
		return strings.TrimSuffix(host, ".local")
	}
	return "anonymous"
}

// gitBranchAt walks up from dir looking for a .git directory and reads HEAD
// without shelling out to git (so we don't depend on git being installed).
// Returns the branch name, a short detached HEAD hash, or "".
func gitBranchAt(dir string) string {
	for i := 0; i < 20; i++ { // bounded walk; abandon if we get nowhere
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

func Home() (string, error) {
	if h := os.Getenv("AGENTMESH_HOME"); h != "" {
		return h, nil
	}
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	return filepath.Join(u.HomeDir, ".agentmesh"), nil
}

// LoadOrCreate returns the persisted identity, creating one with `defaultName`
// if no file exists yet. defaultName may be empty (hostname is used).
func LoadOrCreate(defaultName string) (*Identity, error) {
	home, err := Home()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(home, "identity.json")

	if data, err := os.ReadFile(path); err == nil {
		var id Identity
		if err := json.Unmarshal(data, &id); err != nil {
			return nil, fmt.Errorf("parse identity: %w", err)
		}
		seed, err := hex.DecodeString(id.Seed)
		if err != nil || len(seed) != ed25519.SeedSize {
			return nil, fmt.Errorf("bad seed in identity.json")
		}
		id.PrivateKey = ed25519.NewKeyFromSeed(seed)
		id.PublicKey = id.PrivateKey.Public().(ed25519.PublicKey)
		return &id, nil
	}

	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	priv := ed25519.NewKeyFromSeed(seed)
	name := defaultName
	if name == "" {
		if h, err := os.Hostname(); err == nil {
			name = h
		} else {
			name = "anonymous"
		}
	}
	id := &Identity{
		Name:       name,
		Seed:       hex.EncodeToString(seed),
		PrivateKey: priv,
		PublicKey:  priv.Public().(ed25519.PublicKey),
	}
	data, _ := json.MarshalIndent(id, "", "  ")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return nil, err
	}
	return id, nil
}
