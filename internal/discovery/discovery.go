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
	// Continuous in-memory consumer for the long-lived Browse below.
	merged := make(chan *zeroconf.ServiceEntry, 64)
	go func() {
		for entry := range merged {
			if os.Getenv("AGENTMESH_DEBUG") != "" {
				fmt.Fprintf(os.Stderr, "agentmesh: mdns entry instance=%q text=%v\n", entry.Instance, entry.Text)
			}
			p := entryToPeer(entry)
			if p.PeerID != "" {
				reg.upsert(p)
			}
		}
	}()

	// Re-Browse periodically. libp2p/zeroconf v2's Browse caches its own
	// emit-state inside one invocation: once it has surfaced a service entry
	// to us, it won't re-surface the same entry until cache expiry, even if
	// the peer keeps re-announcing on the network. By cycling Browse calls
	// every ~25s we force fresh mDNS queries and let new emissions land in
	// our registry — which is what kept peers visible to Apple's stack while
	// our long-lived single Browse silently went quiet.
	//
	// We also deliberately do NOT sweep entries on a TTL. Once we've seen a
	// peer, it stays in the registry until the agentmesh process restarts.
	// A peer that's truly gone surfaces as a TLS connect failure at send
	// time, which the agent can handle. The old 2-minute sweep was the
	// thing that made conversations die mid-flight after restarts.
	go func() {
		for {
			if ctx.Err() != nil {
				return
			}
			round, cancel := context.WithTimeout(ctx, 25*time.Second)
			entries := make(chan *zeroconf.ServiceEntry, 16)
			done := make(chan struct{})
			go func() {
				for e := range entries {
					select {
					case merged <- e:
					default:
					}
				}
				close(done)
			}()
			_ = zeroconf.Browse(round, ServiceType, Domain, entries)
			cancel()
			// libp2p/zeroconf closes `entries` itself in params.done() on
			// context cancel; do NOT close it here or we'll double-close
			// and panic the process.
			<-done
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}()

	<-ctx.Done()
	return ctx.Err()
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
	// Pick the best routable IPv4. Peers can advertise multiple A records
	// (one per interface), e.g. Windows with Hyper-V/WSL exposes a virtual
	// switch like 172.27.x.x alongside the real Wi-Fi 192.168.x.x. We score
	// candidates: a same-/24 match with one of our own interfaces wins,
	// then /16, then anything; loopback and link-local are skipped.
	var host string
	if len(e.AddrIPv4) > 0 {
		ip := pickBestIPv4(e.AddrIPv4)
		if ip == nil {
			return Peer{}
		}
		host = ip.String()
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

func pickBestIPv4(candidates []net.IP) net.IP {
	mine := localIPv4s()

	usable := candidates[:0:0]
	for _, c := range candidates {
		c4 := c.To4()
		if c4 == nil || c4.IsLoopback() || c4.IsLinkLocalUnicast() || c4.IsUnspecified() {
			continue
		}
		usable = append(usable, c4)
	}
	if len(usable) == 0 {
		return nil
	}

	// Best: same /24 as any of our local IPs.
	for _, c := range usable {
		for _, m := range mine {
			if sameSubnet(c, m, 24) {
				return c
			}
		}
	}
	// Second: same /16.
	for _, c := range usable {
		for _, m := range mine {
			if sameSubnet(c, m, 16) {
				return c
			}
		}
	}
	// Fallback: first usable.
	return usable[0]
}

func sameSubnet(a, b net.IP, prefix int) bool {
	a4, b4 := a.To4(), b.To4()
	if a4 == nil || b4 == nil {
		return false
	}
	mask := net.CIDRMask(prefix, 32)
	return a4.Mask(mask).Equal(b4.Mask(mask))
}

// localIPv4s returns this host's non-loopback, non-link-local IPv4 addresses
// across all up interfaces. Called per discovered peer — cheap enough.
func localIPv4s() []net.IP {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []net.IP
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifi.Addrs()
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() {
				continue
			}
			out = append(out, ip4)
		}
	}
	return out
}
