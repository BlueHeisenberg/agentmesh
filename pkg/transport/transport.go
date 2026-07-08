// Package transport is the generic mutually-authenticated TLS HTTP layer for
// P2P nodes whose identity is an Ed25519 keypair (see pkg/identity).
//
// Every node presents a self-signed Ed25519 certificate whose CommonName is
// the hex-encoded public key (the peer_id). VerifySelfSignedCert checks
// exactly that on both sides; clients additionally pin the TLS handshake to
// the peer_id they expect (learned out of band, e.g. from mDNS), which
// defeats LAN spoofing.
//
// Application routes are injected: Server serves whatever http.Handler the
// caller provides, and Client hands back a pinned *http.Client (HTTPFor) or
// runs a JSON round-trip (DoJSON).
package transport

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"time"
)

const defaultClientTimeout = 30 * time.Second

// Server serves the injected Handler over mTLS.
type Server struct {
	Cert    tls.Certificate // self-signed, Ed25519-backed (identity.TLSCertificate)
	Handler http.Handler    // application routes

	mu       sync.Mutex
	listener net.Listener
	srv      *http.Server
}

// Rebind shuts down any existing listener and re-binds with the new scope.
// Returns the new port. Safe to call while the server is running.
func (s *Server) Rebind(bindAll bool) (int, error) {
	s.mu.Lock()
	if s.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = s.srv.Shutdown(ctx)
		cancel()
		s.srv = nil
		s.listener = nil
	}
	s.mu.Unlock()
	return s.Start(bindAll)
}

// Start binds to 127.0.0.1:0 OR 0.0.0.0:0 (LAN-wide) on IPv4. Returns the
// advertised port. We pin "tcp4" because macOS defaults IPV6_V6ONLY=1, which
// turned `net.Listen("tcp", ":0")` into an IPv6-only listener — and the mDNS
// advertisement only ships A (IPv4) records, so peers connecting to the
// advertised address would get "connection refused". IPv4-only also matches
// the peer-address picker, which filters to IPv4 candidates.
func (s *Server) Start(bindAll bool) (int, error) {
	addr := "127.0.0.1:0"
	if bindAll {
		addr = "0.0.0.0:0"
	}
	lis, err := net.Listen("tcp4", addr)
	if err != nil {
		return 0, err
	}

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{s.Cert},
		// We require a client cert and verify it ourselves; X.509 verification
		// is disabled because every node is its own self-signed CA.
		ClientAuth:            tls.RequireAnyClientCert,
		InsecureSkipVerify:    true,
		VerifyPeerCertificate: VerifySelfSignedCert,
		MinVersion:            tls.VersionTLS13,
	}

	srv := &http.Server{
		Handler:           s.Handler,
		ReadHeaderTimeout: 5 * time.Second,
		TLSConfig:         tlsCfg,
	}
	s.mu.Lock()
	s.listener = lis
	s.srv = srv
	s.mu.Unlock()

	go func() {
		// ServeTLS with empty cert/key paths uses TLSConfig.Certificates.
		if err := srv.ServeTLS(lis, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "transport: https server: %v\n", err)
		}
	}()
	return lis.Addr().(*net.TCPAddr).Port, nil
}

// Stop shuts down the listener. No-op if the server was never started.
func (s *Server) Stop(ctx context.Context) {
	s.mu.Lock()
	srv := s.srv
	s.mu.Unlock()
	if srv != nil {
		_ = srv.Shutdown(ctx)
	}
}

// VerifySelfSignedCert is reused by both server and client TLS configs. It
// confirms the presented cert is a self-signed Ed25519 cert whose CommonName
// matches its public key (hex). After this passes, the caller can trust that
// the connection peer controls the private key for the peer_id in the CN.
//
// Higher-level identity matching (does this match the peer_id we expected
// from mDNS?) happens in the pinned HTTP client built by HTTPFor.
func VerifySelfSignedCert(rawCerts [][]byte, _ [][]*x509.Certificate) error {
	if len(rawCerts) == 0 {
		return errors.New("no peer certificate")
	}
	cert, err := x509.ParseCertificate(rawCerts[0])
	if err != nil {
		return fmt.Errorf("parse cert: %w", err)
	}
	pub, ok := cert.PublicKey.(ed25519.PublicKey)
	if !ok {
		return errors.New("peer cert is not ed25519")
	}
	// Verify the cert is self-signed by `pub`.
	if err := cert.CheckSignatureFrom(cert); err != nil {
		return fmt.Errorf("self-signature invalid: %w", err)
	}
	expectedCN := hex.EncodeToString(pub)
	if cert.Subject.CommonName != expectedCN {
		return fmt.Errorf("cert CN %q does not match its pubkey %s", cert.Subject.CommonName, expectedCN)
	}
	return nil
}

// PeerIDFromConn extracts the validated peer_id (hex pubkey) from a request's
// TLS connection. Returns "" if the connection isn't TLS or has no peer certs.
func PeerIDFromConn(r *http.Request) string {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return ""
	}
	pub, ok := r.TLS.PeerCertificates[0].PublicKey.(ed25519.PublicKey)
	if !ok {
		return ""
	}
	return hex.EncodeToString(pub)
}

// WriteJSON writes v as a JSON response body with the given status code.
func WriteJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// --- Client ---

// Client makes outbound mTLS requests to peers. Each call must supply the
// expected peer_id so the TLS handshake can pin against it (defeats LAN
// spoofing).
type Client struct {
	// Timeout overrides the default 30s per-request timeout when non-zero.
	Timeout time.Duration

	cert tls.Certificate
}

func NewClient(cert tls.Certificate) *Client {
	return &Client{cert: cert}
}

// HTTPFor builds an http.Client with TLS pinned to expectedPeerID (hex pubkey).
func (c *Client) HTTPFor(expectedPeerID string) *http.Client {
	cfg := &tls.Config{
		Certificates:       []tls.Certificate{c.cert},
		InsecureSkipVerify: true, // we verify manually below
		MinVersion:         tls.VersionTLS13,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if err := VerifySelfSignedCert(rawCerts, nil); err != nil {
				return err
			}
			cert, _ := x509.ParseCertificate(rawCerts[0])
			pub := cert.PublicKey.(ed25519.PublicKey)
			got := hex.EncodeToString(pub)
			if got != expectedPeerID {
				return fmt.Errorf("peer id mismatch: expected %s got %s", expectedPeerID, got)
			}
			return nil
		},
	}
	timeout := c.Timeout
	if timeout == 0 {
		timeout = defaultClientTimeout
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{TLSClientConfig: cfg},
	}
}

// DoJSON runs a JSON round-trip against a pinned peer: marshals reqBody (when
// non-nil) as the request body, requires a 2xx response, and decodes the
// response into respBody (when non-nil).
func (c *Client) DoJSON(ctx context.Context, expectedPeerID, method, url string, reqBody, respBody any) error {
	var body io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTPFor(expectedPeerID).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s %s: %s: %s", method, url, resp.Status, b)
	}
	if respBody != nil {
		return json.NewDecoder(resp.Body).Decode(respBody)
	}
	return nil
}
