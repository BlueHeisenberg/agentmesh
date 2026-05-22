// Package mcp wires the agentmesh node to a stdio MCP server. Each MCP tool
// is a thin shim over the Node API.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/BlueHeisenberg/agentmesh/internal/discovery"
	"github.com/BlueHeisenberg/agentmesh/internal/identity"
	"github.com/BlueHeisenberg/agentmesh/internal/inbox"
	"github.com/BlueHeisenberg/agentmesh/internal/shares"
	"github.com/BlueHeisenberg/agentmesh/internal/transport"
)

// Visibility describes whether this node is reachable on the LAN.
//
//	"loopback"  — listener bound to 127.0.0.1, no mDNS advertise, no browse.
//	              the default at startup. invisible to other machines.
//	"lan"       — listener on 0.0.0.0, advertised on mDNS, actively browsing.
//	              enabled by the agent calling mesh_open_lan.
const (
	VisibilityLoopback = "loopback"
	VisibilityLAN      = "lan"
)

// Node holds all the runtime state the MCP tools touch.
type Node struct {
	ID     *identity.Identity
	Inbox  *inbox.Inbox
	Peers  *discovery.Registry
	Shares *shares.Registry
	Server *transport.Server
	Client *transport.Client

	mu           sync.Mutex
	displayName  string
	port         int
	visibility   string             // VisibilityLoopback | VisibilityLAN
	mdnsStop     func()             // nil unless advertising
	browseCancel context.CancelFunc // nil unless browsing
}

// MarkInitialLoopback records the bind port for the listener main() created
// at startup and sets visibility=loopback. Called once before MCP serving.
func (n *Node) MarkInitialLoopback(port int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.port = port
	n.visibility = VisibilityLoopback
}

// SetName overrides the display name and re-advertises on mDNS if currently
// in LAN mode. Safe to call before or after OpenLAN.
func (n *Node) SetName(name string) error {
	if name == "" {
		return fmt.Errorf("name must not be empty")
	}
	n.mu.Lock()
	n.displayName = name
	n.Server.SelfName = name
	n.Client.SelfName = name
	needReannounce := n.visibility == VisibilityLAN
	n.mu.Unlock()
	if !needReannounce {
		return nil
	}
	// Re-advertise on mDNS so peers see the new name.
	return n.reannounceLocked()
}

// reannounceLocked stops the existing mDNS Register and starts a new one with
// current name+port. Must be called outside the node mutex.
func (n *Node) reannounceLocked() error {
	n.mu.Lock()
	stop := n.mdnsStop
	name := n.displayName
	port := n.port
	n.mu.Unlock()
	if stop != nil {
		stop()
	}
	newStop, err := discovery.Advertise(name, n.ID.PeerID(), port)
	if err != nil {
		return err
	}
	n.mu.Lock()
	n.mdnsStop = newStop
	n.mu.Unlock()
	return nil
}

// OpenLAN rebinds the listener to 0.0.0.0, starts mDNS advertising, and starts
// browsing for peers. Idempotent. Returns the new port.
func (n *Node) OpenLAN() (int, error) {
	n.mu.Lock()
	if n.visibility == VisibilityLAN {
		port := n.port
		n.mu.Unlock()
		return port, nil
	}
	n.mu.Unlock()

	newPort, err := n.Server.Rebind(true)
	if err != nil {
		return 0, fmt.Errorf("rebind: %w", err)
	}
	n.mu.Lock()
	n.port = newPort
	name := n.displayName
	n.mu.Unlock()

	stop, err := discovery.Advertise(name, n.ID.PeerID(), newPort)
	if err != nil {
		return 0, fmt.Errorf("mdns advertise: %w", err)
	}

	bctx, cancel := context.WithCancel(context.Background())
	go func() {
		if err := discovery.Browse(bctx, n.Peers); err != nil && bctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "agentmesh: browse: %v\n", err)
		}
	}()

	n.mu.Lock()
	n.mdnsStop = stop
	n.browseCancel = cancel
	n.visibility = VisibilityLAN
	n.mu.Unlock()
	return newPort, nil
}

// CloseLAN goes back to loopback: stops mDNS advertise/browse, rebinds the
// listener to 127.0.0.1. The peer table is left intact (so messages can still
// be sent to peers we already discovered), but no new peers will be found and
// no other machine can reach us.
func (n *Node) CloseLAN() (int, error) {
	n.mu.Lock()
	if n.visibility == VisibilityLoopback {
		port := n.port
		n.mu.Unlock()
		return port, nil
	}
	stop := n.mdnsStop
	cancel := n.browseCancel
	n.mdnsStop = nil
	n.browseCancel = nil
	n.mu.Unlock()

	if stop != nil {
		stop()
	}
	if cancel != nil {
		cancel()
	}

	newPort, err := n.Server.Rebind(false)
	if err != nil {
		return 0, fmt.Errorf("rebind: %w", err)
	}
	n.mu.Lock()
	n.port = newPort
	n.visibility = VisibilityLoopback
	n.mu.Unlock()
	return newPort, nil
}

// Snapshot returns a copy of the externally-visible state for whoami.
func (n *Node) Snapshot() (name, visibility string, port int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.displayName, n.visibility, n.port
}

func (n *Node) Register(s *server.MCPServer) {
	s.AddTool(mcplib.NewTool("mesh_whoami",
		mcplib.WithDescription("Return this node's peer id, display name, port, and visibility (loopback or lan). Loopback means this node is invisible to other machines — call mesh_open_lan to advertise on the LAN."),
	), n.whoami)

	s.AddTool(mcplib.NewTool("mesh_peers",
		mcplib.WithDescription("List peers discovered on the LAN. Returns [] when this node is in loopback mode (call mesh_open_lan to start discovering peers)."),
	), n.peers)

	s.AddTool(mcplib.NewTool("mesh_open_lan",
		mcplib.WithDescription("Open this node to the LAN: rebind the listener to all interfaces, advertise via mDNS, and start browsing for peers. Call this when you want this session to be reachable by other machines and to discover them. Default startup is loopback-only."),
	), n.openLAN)

	s.AddTool(mcplib.NewTool("mesh_close_lan",
		mcplib.WithDescription("Go back to loopback: stop mDNS advertising and browsing, rebind the listener to 127.0.0.1. Known peers in the registry are kept, but no new peers will be found and other machines can no longer reach this node."),
	), n.closeLAN)

	s.AddTool(mcplib.NewTool("mesh_set_name",
		mcplib.WithDescription("Set this node's display name. Other peers will see this name in their mesh_peers output. Defaults to the working directory + git branch (e.g. \"harnessP2P@main\"); override when you want a more descriptive label for what this session is doing."),
		mcplib.WithString("name", mcplib.Required(), mcplib.Description("New display name. Should be short and recognisable.")),
	), n.setName)

	s.AddTool(mcplib.NewTool("mesh_send",
		mcplib.WithDescription("Send a JSON message to a peer (or broadcast). Fire-and-forget."),
		mcplib.WithString("to", mcplib.Description("Target peer_id, or \"*\" to broadcast to all known peers.")),
		mcplib.WithString("topic", mcplib.Description("Optional topic string the receiver can route on.")),
		mcplib.WithObject("body", mcplib.Description("Arbitrary JSON object to send as the message body.")),
	), n.send)

	s.AddTool(mcplib.NewTool("mesh_inbox",
		mcplib.WithDescription("Read incoming messages. Set wait_seconds>0 to long-poll until a new message arrives."),
		mcplib.WithNumber("since", mcplib.Description("Return only messages with id > since. Default 0.")),
		mcplib.WithNumber("wait_seconds", mcplib.Description("Block up to N seconds for a new message (0 = non-blocking).")),
	), n.inbox)

	s.AddTool(mcplib.NewTool("mesh_share",
		mcplib.WithDescription("Register a file as shareable. Returns a handle peers can fetch."),
		mcplib.WithString("path", mcplib.Required(), mcplib.Description("Absolute or working-dir-relative path to the file.")),
		mcplib.WithString("name", mcplib.Description("Optional display name (defaults to basename).")),
		mcplib.WithArray("allow_peers", mcplib.Description("Optional list of peer_ids permitted to fetch. Empty = anyone discovered.")),
		mcplib.WithNumber("ttl_seconds", mcplib.Description("Auto-expire the share after N seconds. 0 = no expiry.")),
	), n.share)

	s.AddTool(mcplib.NewTool("mesh_fetch",
		mcplib.WithDescription("Fetch a shared blob from a peer. Saves to save_to or returns inline (small blobs only)."),
		mcplib.WithString("peer_id", mcplib.Required(), mcplib.Description("Peer to fetch from.")),
		mcplib.WithString("handle", mcplib.Required(), mcplib.Description("Share handle from the peer.")),
		mcplib.WithString("save_to", mcplib.Description("If set, write to this path and return metadata.")),
	), n.fetch)

	s.AddTool(mcplib.NewTool("mesh_shares",
		mcplib.WithDescription("List local shares we are currently offering."),
	), n.listShares)

	s.AddTool(mcplib.NewTool("mesh_unshare",
		mcplib.WithDescription("Revoke a local share by handle."),
		mcplib.WithString("handle", mcplib.Required()),
	), n.unshare)
}

// --- arg helpers (v0.20.1 has no req.GetString etc.) ---

func argString(req mcplib.CallToolRequest, key, def string) string {
	if v, ok := req.Params.Arguments[key].(string); ok {
		return v
	}
	return def
}

func argFloat(req mcplib.CallToolRequest, key string, def float64) float64 {
	if v, ok := req.Params.Arguments[key].(float64); ok {
		return v
	}
	return def
}

func argAny(req mcplib.CallToolRequest, key string) any {
	return req.Params.Arguments[key]
}

func argStringSlice(req mcplib.CallToolRequest, key string) []string {
	raw, ok := req.Params.Arguments[key]
	if !ok {
		return nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, v := range arr {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// --- tool handlers ---

func (n *Node) whoami(_ context.Context, _ mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	name, vis, port := n.Snapshot()
	out := map[string]any{
		"peer_id":    n.ID.PeerID(),
		"name":       name,
		"port":       port,
		"visibility": vis,
		"version":    transport.Version,
	}
	if vis == VisibilityLoopback {
		out["hint"] = "This node is loopback-only — no other machine can reach it and mesh_peers will be empty. Call mesh_open_lan to advertise on the LAN."
	}
	return jsonResult(out)
}

func (n *Node) openLAN(_ context.Context, _ mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	port, err := n.OpenLAN()
	if err != nil {
		return errResult(err.Error())
	}
	name, vis, _ := n.Snapshot()
	return jsonResult(map[string]any{
		"visibility": vis,
		"port":       port,
		"name":       name,
		"peer_id":    n.ID.PeerID(),
		"message":    "Now reachable on the LAN. Other agentmesh nodes will discover this peer within a few seconds.",
	})
}

func (n *Node) closeLAN(_ context.Context, _ mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	port, err := n.CloseLAN()
	if err != nil {
		return errResult(err.Error())
	}
	name, vis, _ := n.Snapshot()
	return jsonResult(map[string]any{
		"visibility": vis,
		"port":       port,
		"name":       name,
		"peer_id":    n.ID.PeerID(),
		"message":    "Back on loopback — invisible to other machines. Known peers are preserved.",
	})
}

func (n *Node) setName(_ context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	name := argString(req, "name", "")
	if name == "" {
		return errResult("`name` is required")
	}
	if err := n.SetName(name); err != nil {
		return errResult(err.Error())
	}
	return jsonResult(map[string]any{"name": name, "ok": true})
}

func (n *Node) peers(_ context.Context, _ mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	list := n.Peers.List()
	out := make([]map[string]any, 0, len(list))
	for _, p := range list {
		out = append(out, map[string]any{
			"peer_id":   p.PeerID,
			"name":      p.Name,
			"addr":      p.Addr,
			"last_seen": p.LastSeen.Format(time.RFC3339),
		})
	}
	return jsonResult(map[string]any{"peers": out})
}

func (n *Node) send(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	to := argString(req, "to", "")
	topic := argString(req, "topic", "")
	body := argAny(req, "body")
	if to == "" {
		return errResult("`to` is required (peer_id or \"*\")")
	}

	type result struct {
		PeerID string `json:"peer_id"`
		Name   string `json:"name"`
		OK     bool   `json:"ok"`
		Err    string `json:"error,omitempty"`
	}

	targets := []discovery.Peer{}
	if to == "*" {
		targets = n.Peers.List()
	} else {
		p, ok := n.Peers.Get(to)
		if !ok {
			return errResult(fmt.Sprintf("unknown peer_id %q (try mesh_peers)", to))
		}
		targets = append(targets, p)
	}

	results := make([]result, 0, len(targets))
	for _, p := range targets {
		err := n.Client.SendMsg(ctx, p.PeerID, p.BaseURL(), topic, body)
		r := result{PeerID: p.PeerID, Name: p.Name, OK: err == nil}
		if err != nil {
			r.Err = err.Error()
		}
		results = append(results, r)
	}
	return jsonResult(map[string]any{"results": results})
}

func (n *Node) inbox(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	since := int64(argFloat(req, "since", 0))
	waitS := argFloat(req, "wait_seconds", 0)
	var msgs []inbox.Message
	var cur int64
	if waitS > 0 {
		msgs, cur = n.Inbox.Wait(ctx, since, time.Duration(waitS*float64(time.Second)))
	} else {
		msgs, cur = n.Inbox.Since(since)
	}
	return jsonResult(map[string]any{
		"messages": msgs,
		"cursor":   cur,
	})
}

func (n *Node) share(_ context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	path := argString(req, "path", "")
	if path == "" {
		return errResult("`path` is required")
	}
	if !filepath.IsAbs(path) {
		wd, _ := os.Getwd()
		path = filepath.Join(wd, path)
	}
	name := argString(req, "name", "")
	ttl := time.Duration(argFloat(req, "ttl_seconds", 0) * float64(time.Second))
	allow := argStringSlice(req, "allow_peers")

	sh, err := n.Shares.AddFile(path, name, allow, ttl)
	if err != nil {
		return errResult(err.Error())
	}
	return jsonResult(map[string]any{
		"handle":      sh.Handle,
		"name":        sh.Name,
		"size":        sh.Size,
		"path":        sh.Path,
		"allow_peers": sh.AllowPeers,
		"expires_at":  optionalTime(sh.ExpiresAt),
	})
}

func (n *Node) fetch(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	peerID := argString(req, "peer_id", "")
	handle := argString(req, "handle", "")
	saveTo := argString(req, "save_to", "")
	if peerID == "" || handle == "" {
		return errResult("`peer_id` and `handle` are required")
	}
	peer, ok := n.Peers.Get(peerID)
	if !ok {
		return errResult(fmt.Sprintf("unknown peer_id %q", peerID))
	}

	if saveTo != "" {
		if !filepath.IsAbs(saveTo) {
			wd, _ := os.Getwd()
			saveTo = filepath.Join(wd, saveTo)
		}
		f, err := os.Create(saveTo)
		if err != nil {
			return errResult(err.Error())
		}
		defer f.Close()
		name, n2, err := n.Client.Fetch(ctx, peer.PeerID, peer.BaseURL(), handle, f)
		if err != nil {
			return errResult(err.Error())
		}
		return jsonResult(map[string]any{
			"name":     name,
			"bytes":    n2,
			"saved_to": saveTo,
		})
	}

	// Inline: cap at 256 KiB to avoid blowing up the MCP message.
	const inlineMax = 256 * 1024
	buf := &capBuf{max: inlineMax}
	name, n2, err := n.Client.Fetch(ctx, peer.PeerID, peer.BaseURL(), handle, buf)
	if err != nil {
		return errResult(err.Error())
	}
	if buf.over {
		return errResult(fmt.Sprintf("blob is larger than %d bytes; pass save_to to write it to disk", inlineMax))
	}
	return jsonResult(map[string]any{
		"name":  name,
		"bytes": n2,
		"data":  string(buf.b), // assumed UTF-8 text; if binary, caller should use save_to
	})
}

func (n *Node) listShares(_ context.Context, _ mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	return jsonResult(map[string]any{"shares": n.Shares.List()})
}

func (n *Node) unshare(_ context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	h := argString(req, "handle", "")
	if h == "" {
		return errResult("`handle` is required")
	}
	return jsonResult(map[string]any{"removed": n.Shares.Remove(h)})
}

// --- helpers ---

type capBuf struct {
	b    []byte
	max  int
	over bool
}

func (c *capBuf) Write(p []byte) (int, error) {
	if c.over {
		return len(p), nil
	}
	if len(c.b)+len(p) > c.max {
		c.over = true
		return len(p), nil
	}
	c.b = append(c.b, p...)
	return len(p), nil
}

func optionalTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.Format(time.RFC3339)
}

func jsonResult(v any) (*mcplib.CallToolResult, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return mcplib.NewToolResultText(string(b)), nil
}

func errResult(msg string) (*mcplib.CallToolResult, error) {
	return mcplib.NewToolResultError(msg), nil
}
