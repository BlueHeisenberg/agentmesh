// Package discovery advertises this node and tracks LAN peers via mDNS.
package discovery

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	zeroconf "github.com/libp2p/zeroconf/v2"
)

const (
	ServiceType = "_agentmesh._tcp"
	Domain      = "local."
)

// Peer is what we know about another node from mDNS + /v1/hello.
type Peer struct {
	PeerID   string    // full hex pubkey
	Name     string    // human-readable
	Addr     string    // host:port (HTTP base)
	LastSeen time.Time
}

func (p Peer) BaseURL() string { return "https://" + p.Addr }

type Registry struct {
	mu    sync.RWMutex
	peers map[string]*Peer // keyed by PeerID
	selfID string
}

func NewRegistry(selfID string) *Registry {
	return &Registry{peers: map[string]*Peer{}, selfID: selfID}
}

func (r *Registry) List() []Peer {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Peer, 0, len(r.peers))
	for _, p := range r.peers {
		out = append(out, *p)
	}
	return out
}

func (r *Registry) Get(peerID string) (Peer, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.peers[peerID]
	if !ok {
		return Peer{}, false
	}
	return *p, true
}

func (r *Registry) upsert(p Peer) {
	if p.PeerID == r.selfID || p.PeerID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.peers[p.PeerID] = &p
	if os.Getenv("AGENTMESH_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "agentmesh: discovered peer %s (%s) at %s\n", p.Name, p.PeerID[:16], p.Addr)
	}
}

func (r *Registry) sweep(maxAge time.Duration) {
	cutoff := time.Now().Add(-maxAge)
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, p := range r.peers {
		if p.LastSeen.Before(cutoff) {
			delete(r.peers, id)
		}
	}
}

// Advertise registers this node on mDNS. Returns a shutdown func.
func Advertise(name, peerID string, port int) (func(), error) {
	// The service "instance name" must be unique on the network — use short id.
	instance := peerID[:16]
	txt := []string{
		"v=1",
		"pk=" + peerID,
		"name=" + name,
	}
	server, err := zeroconf.Register(instance, ServiceType, Domain, port, txt, nil)
	if err != nil {
		return nil, fmt.Errorf("mdns register: %w", err)
	}
	return func() { server.Shutdown() }, nil
}

// Browse runs until ctx is cancelled, feeding the registry from mDNS announcements.
func Browse(ctx context.Context, reg *Registry) error {
	entries := make(chan *zeroconf.ServiceEntry, 16)
	go func() {
		for entry := range entries {
			if os.Getenv("AGENTMESH_DEBUG") != "" {
				fmt.Fprintf(os.Stderr, "agentmesh: mdns entry instance=%q text=%v\n", entry.Instance, entry.Text)
			}
			p := entryToPeer(entry)
			if p.PeerID != "" {
				reg.upsert(p)
			}
		}
	}()

	// Sweeper goroutine.
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				reg.sweep(2 * time.Minute)
			}
		}
	}()

	return zeroconf.Browse(ctx, ServiceType, Domain, entries)
}

func entryToPeer(e *zeroconf.ServiceEntry) Peer {
	var pk, name string
	for _, txt := range e.Text {
		switch {
		case strings.HasPrefix(txt, "pk="):
			pk = strings.TrimPrefix(txt, "pk=")
		case strings.HasPrefix(txt, "name="):
			name = strings.TrimPrefix(txt, "name=")
		}
	}
	// Pick the first usable IPv4, fall back to IPv6.
	var host string
	if len(e.AddrIPv4) > 0 {
		host = e.AddrIPv4[0].String()
	} else if len(e.AddrIPv6) > 0 {
		host = "[" + e.AddrIPv6[0].String() + "]"
	} else {
		return Peer{}
	}
	return Peer{
		PeerID:   pk,
		Name:     name,
		Addr:     net.JoinHostPort(host, fmt.Sprintf("%d", e.Port)),
		LastSeen: time.Now(),
	}
}
