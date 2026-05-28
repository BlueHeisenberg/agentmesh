// Package discovery wraps libp2p/zeroconf v2 with the agentmesh model:
// per-interface advertise + browse, with explicit IP lists so loopback works.
//
// libp2p/zeroconf's standard Register() refuses to publish loopback IPs as A
// records (its addrsForInterface hard-codes !IsLoopback). Same-machine
// discovery is the *primary* agentmesh use case, so we use RegisterProxy
// throughout and pass the IPs we want advertised explicitly.
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

// Peer is what we know about another node from mDNS.
type Peer struct {
	PeerID   string
	Name     string
	Addr     string // host:port
	LastSeen time.Time
}

func (p Peer) BaseURL() string { return "https://" + p.Addr }

// Registry is the live set of peers this node has discovered.
type Registry struct {
	mu     sync.RWMutex
	peers  map[string]*Peer
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

func (r *Registry) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.peers = map[string]*Peer{}
}

func (r *Registry) upsert(p Peer) {
	if p.PeerID == r.selfID || p.PeerID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.peers[p.PeerID] = &p
	if os.Getenv("AGENTMESH_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "agentmesh: discovered peer %s (%s) at %s\n",
			p.Name, p.PeerID[:16], p.Addr)
	}
}

// ----------------------------------------------------------------------------
// Scope helpers
// ----------------------------------------------------------------------------

// LoopbackInterfaces returns just lo0 (or equivalent). Used as the default
// advertise/browse scope: same-machine sessions auto-discover, zero LAN noise.
func LoopbackInterfaces() []net.Interface {
	all, _ := net.Interfaces()
	var out []net.Interface
	for _, ifi := range all {
		if ifi.Flags&net.FlagLoopback != 0 && ifi.Flags&net.FlagUp != 0 {
			out = append(out, ifi)
		}
	}
	return out
}

// NonLoopbackMulticastInterfaces returns up, multicast-capable, non-loopback
// interfaces — the LAN-facing set we add to the scope after mesh_open_lan.
func NonLoopbackMulticastInterfaces() []net.Interface {
	all, _ := net.Interfaces()
	var out []net.Interface
	for _, ifi := range all {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		if ifi.Flags&net.FlagMulticast == 0 {
			continue
		}
		out = append(out, ifi)
	}
	return out
}

// LANIPv4s returns the IPv4 addresses we have on non-loopback interfaces, for
// inclusion in our mDNS A records when we're in LAN mode.
func LANIPv4s() []string {
	var out []string
	for _, ifi := range NonLoopbackMulticastInterfaces() {
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
			out = append(out, ip4.String())
		}
	}
	return out
}

// ----------------------------------------------------------------------------
// Advertise
// ----------------------------------------------------------------------------

// Advertise registers this node on mDNS over the given interfaces, publishing
// the given IPs as A records. Use RegisterProxy because the library's plain
// Register() filters out loopback IPs (server.go: addrsForInterface drops
// anything that returns true from IsLoopback). Returns a shutdown func.
func Advertise(name, peerID string, port int, ifaces []net.Interface, ips []string) (func(), error) {
	if len(ips) == 0 {
		return nil, fmt.Errorf("Advertise: no IPs to publish")
	}
	if len(ifaces) == 0 {
		return nil, fmt.Errorf("Advertise: no interfaces to advertise on")
	}
	host, _ := os.Hostname()
	if host == "" {
		host = "agentmesh"
	}

	instance := peerID[:16]
	txt := []string{
		"v=1",
		"pk=" + peerID,
		"name=" + name,
	}
	server, err := zeroconf.RegisterProxy(instance, ServiceType, Domain, port, host, ips, txt, ifaces)
	if err != nil {
		return nil, fmt.Errorf("mdns register: %w", err)
	}
	return func() { server.Shutdown() }, nil
}

// ----------------------------------------------------------------------------
// Browse
// ----------------------------------------------------------------------------

// Browse runs until ctx is cancelled, feeding mDNS announcements into reg.
// Restricted to the given interfaces (use LoopbackInterfaces for same-machine
// only, or LoopbackInterfaces + NonLoopbackMulticastInterfaces for LAN mode).
func Browse(ctx context.Context, reg *Registry, ifaces []net.Interface) error {
	merged := make(chan *zeroconf.ServiceEntry, 64)
	go func() {
		for entry := range merged {
			if os.Getenv("AGENTMESH_DEBUG") != "" {
				fmt.Fprintf(os.Stderr, "agentmesh: mdns entry instance=%q text=%v\n",
					entry.Instance, entry.Text)
			}
			p := entryToPeer(entry)
			if p.PeerID != "" {
				reg.upsert(p)
			}
		}
	}()

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
			var opts []zeroconf.ClientOption
			if len(ifaces) > 0 {
				opts = append(opts, zeroconf.SelectIfaces(ifaces))
			}
			_ = zeroconf.Browse(round, ServiceType, Domain, entries, opts...)
			cancel()
			// libp2p/zeroconf closes `entries` itself in params.done(); do NOT
			// close it here or we double-close-panic.
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

// ----------------------------------------------------------------------------
// Address picking for incoming peer entries
// ----------------------------------------------------------------------------

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
	if pk == "" {
		return Peer{}
	}
	host := pickAddr(e.AddrIPv4, e.AddrIPv6)
	if host == "" {
		return Peer{}
	}
	return Peer{
		PeerID:   pk,
		Name:     name,
		Addr:     net.JoinHostPort(host, fmt.Sprintf("%d", e.Port)),
		LastSeen: time.Now(),
	}
}

// pickAddr scores candidate IPv4s and returns the best routable string form.
// Order of preference (best first):
//
//  1. 127.0.0.1 ONLY when the peer is verifiably on this machine - detected
//     by one of their advertised A records matching one of our own non-
//     loopback IPv4s. Without this guard, the v0.4+ habit of advertising
//     127.0.0.1 alongside the LAN IP would cause us to attribute every
//     remote peer to our own loopback (a real bug found in the field).
//  2. Same /24 as one of our LAN interfaces.
//  3. Same /16 as one of our LAN interfaces.
//  4. Any usable IPv4 (not link-local / unspecified / loopback).
//  5. IPv6 (link-local form) as last resort.
func pickAddr(v4 []net.IP, v6 []net.IP) string {
	mine := localLANIPv4s()

	// Same-machine detection: two signals, either is sufficient.
	//   (a) The peer's advertise includes one of OUR LAN IPs - they're us
	//       running on a different process and are advertising in LAN mode.
	//   (b) The peer advertises ONLY loopback addresses - they can only be
	//       same-machine; a truly-remote peer advertising loopback-only
	//       would be unreachable to anyone, so it must be the
	//       default-loopback v0.4+ "advertise 127.0.0.1 on lo0 only" case.
	sameMachine := false
	for _, c := range v4 {
		for _, m := range mine {
			if c4 := c.To4(); c4 != nil && c4.Equal(m) {
				sameMachine = true
				break
			}
		}
		if sameMachine {
			break
		}
	}
	if !sameMachine && len(v4) > 0 {
		allLoopback := true
		for _, c := range v4 {
			if !c.IsLoopback() {
				allLoopback = false
				break
			}
		}
		if allLoopback {
			sameMachine = true
		}
	}
	if sameMachine {
		for _, c := range v4 {
			if c.IsLoopback() {
				return c.String()
			}
		}
	}

	usable := make([]net.IP, 0, len(v4))
	for _, c := range v4 {
		c4 := c.To4()
		if c4 == nil || c4.IsLoopback() || c4.IsLinkLocalUnicast() || c4.IsUnspecified() {
			continue
		}
		usable = append(usable, c4)
	}

	for _, c := range usable {
		for _, m := range mine {
			if sameSubnet(c, m, 24) {
				return c.String()
			}
		}
	}
	for _, c := range usable {
		for _, m := range mine {
			if sameSubnet(c, m, 16) {
				return c.String()
			}
		}
	}
	if len(usable) > 0 {
		return usable[0].String()
	}
	if len(v6) > 0 {
		return "[" + v6[0].String() + "]"
	}
	return ""
}

func sameSubnet(a, b net.IP, prefix int) bool {
	a4, b4 := a.To4(), b.To4()
	if a4 == nil || b4 == nil {
		return false
	}
	mask := net.CIDRMask(prefix, 32)
	return a4.Mask(mask).Equal(b4.Mask(mask))
}

func localLANIPv4s() []net.IP {
	var out []net.IP
	for _, ifi := range NonLoopbackMulticastInterfaces() {
		addrs, _ := ifi.Addrs()
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil || ip4.IsLinkLocalUnicast() {
				continue
			}
			out = append(out, ip4)
		}
	}
	return out
}
