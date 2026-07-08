// Package transport is the agentmesh inter-node HTTP layer: the agentmesh
// routes and message payloads on top of pkg/transport's generic mTLS
// server/client.
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
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/BlueHeisenberg/agentmesh/internal/inbox"
	"github.com/BlueHeisenberg/agentmesh/internal/shares"
	ptransport "github.com/BlueHeisenberg/agentmesh/pkg/transport"
)

const (
	Version      = "0.6.0"
	PeerNmHeader = "X-Agentmesh-Peer-Name"
)

type Server struct {
	SelfPeerID string
	SelfName   string
	Cert       tls.Certificate // self-signed, Ed25519-backed
	Inbox      *inbox.Inbox
	Shares     *shares.Registry

	once sync.Once
	core *ptransport.Server
}

// coreServer lazily builds the generic mTLS server with the agentmesh routes
// mounted. Built once; Start/Rebind cycle the listener inside it.
func (s *Server) coreServer() *ptransport.Server {
	s.once.Do(func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/v1/hello", s.handleHello)
		mux.HandleFunc("/v1/msg", s.handleMsg)
		mux.HandleFunc("/v1/share/", s.handleShare)
		s.core = &ptransport.Server{Cert: s.Cert, Handler: mux}
	})
	return s.core
}

// Start binds to 127.0.0.1:0 OR 0.0.0.0:0 (LAN-wide) on IPv4. Returns the
// advertised port. See pkg/transport for the tcp4/TLS details.
func (s *Server) Start(bindAll bool) (int, error) { return s.coreServer().Start(bindAll) }

// Rebind shuts down any existing listener and re-binds with the new scope.
// Returns the new port. Safe to call while the server is running.
func (s *Server) Rebind(bindAll bool) (int, error) { return s.coreServer().Rebind(bindAll) }

func (s *Server) Stop(ctx context.Context) { s.coreServer().Stop(ctx) }

type helloResp struct {
	PeerID  string `json:"peer_id"`
	Name    string `json:"name"`
	Version string `json:"version"`
}

func (s *Server) handleHello(w http.ResponseWriter, _ *http.Request) {
	ptransport.WriteJSON(w, 200, helloResp{PeerID: s.SelfPeerID, Name: s.SelfName, Version: Version})
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
	caller := ptransport.PeerIDFromConn(r)
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
	ptransport.WriteJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleShare(w http.ResponseWriter, r *http.Request) {
	handle := strings.TrimPrefix(r.URL.Path, "/v1/share/")
	if handle == "" {
		http.Error(w, "missing handle", 400)
		return
	}
	caller := ptransport.PeerIDFromConn(r)
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

// Client makes outbound requests to peers, wrapping pkg/transport's pinned
// mTLS client with agentmesh's self-identity and message payloads. Each call
// must supply the expected peer_id so the TLS handshake can pin against it
// (defeats LAN spoofing).
type Client struct {
	SelfPeerID string
	SelfName   string
	core       *ptransport.Client
}

func NewClient(selfPeerID, selfName string, cert tls.Certificate) *Client {
	return &Client{SelfPeerID: selfPeerID, SelfName: selfName, core: ptransport.NewClient(cert)}
}

func (c *Client) Hello(ctx context.Context, expectedPeerID, baseURL string) (helloResp, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", baseURL+"/v1/hello", nil)
	resp, err := c.core.HTTPFor(expectedPeerID).Do(req)
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
	resp, err := c.core.HTTPFor(expectedPeerID).Do(req)
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
	resp, err := c.core.HTTPFor(expectedPeerID).Do(req)
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
