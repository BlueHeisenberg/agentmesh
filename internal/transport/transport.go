// Package transport is the inter-node HTTP layer.
//
// Routes (server side):
//
//	GET  /v1/hello                 -> {peer_id, name, version}
//	POST /v1/msg                   -> {ok}            body: {from_peer_id, from_name, topic, body}
//	GET  /v1/share/{handle}        -> blob bytes      (X-Agentmesh-Peer-ID header asserts caller)
//
// Caller identity in v1 is asserted by the client in the X-Agentmesh-Peer-ID
// header. No signatures yet — the threat model is a trusted LAN.
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
	"strings"
	"sync"
	"time"

	"github.com/BlueHeisenberg/agentmesh/internal/inbox"
	"github.com/BlueHeisenberg/agentmesh/internal/shares"
)

const (
	Version       = "0.4.2"
	PeerNmHeader  = "X-Agentmesh-Peer-Name"
	defaultClient = 30 * time.Second
)

type Server struct {
	SelfPeerID string
	SelfName   string
	Cert       tls.Certificate // self-signed, Ed25519-backed
	Inbox      *inbox.Inbox
	Shares     *shares.Registry

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
// turned `net.Listen("tcp", ":0")` into an IPv6-only listener — and our mDNS
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
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/hello", s.handleHello)
	mux.HandleFunc("/v1/msg", s.handleMsg)
	mux.HandleFunc("/v1/share/", s.handleShare)

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{s.Cert},
		// We require a client cert and verify it ourselves; X.509 verification
		// is disabled because every node is its own self-signed CA.
		ClientAuth:            tls.RequireAnyClientCert,
		InsecureSkipVerify:    true,
		VerifyPeerCertificate: verifySelfSignedCert,
		MinVersion:            tls.VersionTLS13,
	}

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		TLSConfig:         tlsCfg,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			return ctx // peer info is attached via TLS state on each request
		},
	}
	s.mu.Lock()
	s.listener = lis
	s.srv = srv
	s.mu.Unlock()

	go func() {
		// ServeTLS with empty cert/key paths uses TLSConfig.Certificates.
		if err := srv.ServeTLS(lis, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "agentmesh: https server: %v\n", err)
		}
	}()
	return lis.Addr().(*net.TCPAddr).Port, nil
}

// verifySelfSignedCert is reused by both server and client TLS configs. It
// confirms the presented cert is a self-signed Ed25519 cert whose CommonName
// matches its public key (hex). After this passes, the caller can trust that
// the connection peer controls the private key for the peer_id in the CN.
//
// Higher-level identity matching (does this match the peer_id we expected
// from mDNS?) happens in the HTTP-client tls.Config below.
func verifySelfSignedCert(rawCerts [][]byte, _ [][]*x509.Certificate) error {
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

func (s *Server) Stop(ctx context.Context) { _ = s.srv.Shutdown(ctx) }

type helloResp struct {
	PeerID  string `json:"peer_id"`
	Name    string `json:"name"`
	Version string `json:"version"`
}

func (s *Server) handleHello(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, helloResp{PeerID: s.SelfPeerID, Name: s.SelfName, Version: Version})
}

type msgPayload struct {
	FromPeerID string          `json:"from_peer_id"`
	FromName   string          `json:"from_name"`
	Topic      string          `json:"topic,omitempty"`
	Body       json.RawMessage `json:"body"`
}

func (s *Server) handleMsg(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	caller := PeerIDFromConn(r)
	if caller == "" {
		http.Error(w, "client cert required", 401)
		return
	}
	var p msgPayload
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	// Body-claimed from_peer_id must match the mTLS-authenticated peer id.
	if p.FromPeerID != "" && p.FromPeerID != caller {
		http.Error(w, "from_peer_id does not match client cert", 403)
		return
	}
	fromName := p.FromName
	if fromName == "" {
		fromName = r.Header.Get(PeerNmHeader)
	}
	s.Inbox.Push(caller, fromName, p.Topic, p.Body)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleShare(w http.ResponseWriter, r *http.Request) {
	handle := strings.TrimPrefix(r.URL.Path, "/v1/share/")
	if handle == "" {
		http.Error(w, "missing handle", 400)
		return
	}
	caller := PeerIDFromConn(r)
	if caller == "" {
		http.Error(w, "client cert required", 401)
		return
	}
	sh, err := s.Shares.Resolve(handle, caller)
	switch {
	case errors.Is(err, shares.ErrNotFound):
		http.Error(w, "not found", 404)
		return
	case errors.Is(err, shares.ErrForbidden):
		http.Error(w, "forbidden", 403)
		return
	case errors.Is(err, shares.ErrExpired):
		http.Error(w, "expired", 410)
		return
	case err != nil:
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Agentmesh-Share-Name", sh.Name)
	if sh.Path != "" {
		f, err := os.Open(sh.Path)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer f.Close()
		http.ServeContent(w, r, sh.Name, sh.CreatedAt, f)
		return
	}
	http.ServeContent(w, r, sh.Name, sh.CreatedAt, bytes.NewReader(sh.Bytes))
}

// --- Client ---

// Client makes outbound requests to peers. Each call must supply the expected
// peer_id so the TLS handshake can pin against it (defeats LAN spoofing).
type Client struct {
	SelfPeerID string
	SelfName   string
	cert       tls.Certificate
}

func NewClient(selfPeerID, selfName string, cert tls.Certificate) *Client {
	return &Client{SelfPeerID: selfPeerID, SelfName: selfName, cert: cert}
}

// httpFor builds an http.Client with TLS pinned to expectedPeerID (hex pubkey).
func (c *Client) httpFor(expectedPeerID string) *http.Client {
	cfg := &tls.Config{
		Certificates:       []tls.Certificate{c.cert},
		InsecureSkipVerify: true, // we verify manually below
		MinVersion:         tls.VersionTLS13,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if err := verifySelfSignedCert(rawCerts, nil); err != nil {
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
	return &http.Client{
		Timeout:   defaultClient,
		Transport: &http.Transport{TLSClientConfig: cfg},
	}
}

func (c *Client) Hello(ctx context.Context, expectedPeerID, baseURL string) (helloResp, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", baseURL+"/v1/hello", nil)
	resp, err := c.httpFor(expectedPeerID).Do(req)
	if err != nil {
		return helloResp{}, err
	}
	defer resp.Body.Close()
	var h helloResp
	return h, json.NewDecoder(resp.Body).Decode(&h)
}

func (c *Client) SendMsg(ctx context.Context, expectedPeerID, baseURL, topic string, body any) error {
	payload := msgPayload{
		FromPeerID: c.SelfPeerID,
		FromName:   c.SelfName,
		Topic:      topic,
	}
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload.Body = b
	}
	buf, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, "POST", baseURL+"/v1/msg", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(PeerNmHeader, c.SelfName)
	resp, err := c.httpFor(expectedPeerID).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("send msg: %s: %s", resp.Status, b)
	}
	return nil
}

// Fetch downloads a shared blob into w. Returns (name, bytesWritten, error).
func (c *Client) Fetch(ctx context.Context, expectedPeerID, baseURL, handle string, w io.Writer) (string, int64, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", baseURL+"/v1/share/"+handle, nil)
	req.Header.Set(PeerNmHeader, c.SelfName)
	resp, err := c.httpFor(expectedPeerID).Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 206 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", 0, fmt.Errorf("fetch: %s: %s", resp.Status, b)
	}
	name := resp.Header.Get("X-Agentmesh-Share-Name")
	n, err := io.Copy(w, resp.Body)
	return name, n, err
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
