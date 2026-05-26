// Package mcp wires the agentmesh node to a stdio MCP server. Each MCP tool
// and resource is a thin shim over the Node API.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
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

// Visibility describes the current advertising scope.
//
//	loopback - bound to 127.0.0.1, advertising + browsing on lo0 only.
//	           Same-machine sessions discover each other; no LAN exposure.
//	lan      - bound to 0.0.0.0, advertising + browsing on lo0 + LAN ifaces.
//	           Other machines can reach this node.
const (
	VisibilityLoopback = "loopback"
	VisibilityLAN      = "lan"

	// InboxResourceURI is the MCP resource URI that mirrors the inbox state.
	// Harnesses can subscribe to notifications/resources/updated for this URI
	// to know when new mesh messages have arrived.
	InboxResourceURI = "agentmesh://inbox"
)

// ServerInstructions is published to the MCP client as part of the initialize
// response and lands in the agent's system prompt for harnesses that surface
// it. Designed to make the agent call mesh_inbox at every turn boundary
// without the user having to ask.
const ServerInstructions = `agentmesh connects this session to other AI agents in nearby harness sessions - same machine by default, LAN if you call mesh_open_lan. Tools start with mesh_*.

INCOMING PEER MESSAGES — handling rule (IMPORTANT):

At the start of EVERY user turn, before doing other work, call mesh_inbox to check for messages from peers.

When new messages are present, you MUST surface them to the user in your reply, even if the user's prompt is about something else entirely. The user has not seen these messages - they arrived between turns. Silently internalising peer input is the wrong default; the user needs to know peers are reaching out.

The expected shape of your reply when new messages exist:
  1. A short, prominent acknowledgement: who the message came from (peer name), what the topic was, and a one-line summary of the content. Place this at or near the start of your reply.
  2. Then engage with the message content - integrate it into your work where relevant, or set it aside with a clear note.
  3. Then handle the user's original prompt as usual.

A separate UserPromptSubmit hook (installed by default on Claude Code) may also inject mesh messages as additional context. The same rule applies: relay them to the user, don't swallow them.

NODE VISIBILITY:

By default this node is loopback-only - visible only to other sessions on this same machine. Call mesh_open_lan if you want this session reachable across the local network. Use mesh_set_name to rename the node when its default ("<folder>@<branch>#<tag>") isn't descriptive enough.`

// Node holds runtime state shared by all MCP tool handlers.
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
	visibility   string             // VisibilityLoopback | VisibilityLAN | ""
	mdnsStop     func()             // nil unless advertising
	browseCancel context.CancelFunc // nil unless browsing
	mcpServer    *server.MCPServer  // for SendNotificationToClient
	clientCtx    context.Context    // captured at session register
}

// ----------------------------------------------------------------------------
// Lifecycle
// ----------------------------------------------------------------------------

// transition atomically swaps the node's advertising scope. The single point
// where the listener gets rebound, mDNS is restarted, and the peer registry
// is cleared (since the scope of who's reachable just changed). Idempotent if
// called with the same target.
func (n *Node) transition(target string) (int, error) {
	n.mu.Lock()
	stop := n.mdnsStop
	cancel := n.browseCancel
	n.mdnsStop = nil
	n.browseCancel = nil
	currentName := n.displayName
	n.mu.Unlock()

	// Tear down current advertise/browse before rebinding so we don't briefly
	// have two registrations for the same instance.
	if stop != nil {
		stop()
	}
	if cancel != nil {
		cancel()
	}

	bindAll := target == VisibilityLAN
	newPort, err := n.Server.Rebind(bindAll)
	if err != nil {
		return 0, fmt.Errorf("rebind: %w", err)
	}

	// Scope changed; previously-known peers may or may not still be in scope.
	// Easier to clear and let discovery repopulate within a few seconds.
	n.Peers.Clear()

	var ifaces []net.Interface
	var ips []string
	switch target {
	case VisibilityLAN:
		ifaces = append(ifaces, discovery.LoopbackInterfaces()...)
		ifaces = append(ifaces, discovery.NonLoopbackMulticastInterfaces()...)
		ips = append([]string{"127.0.0.1"}, discovery.LANIPv4s()...)
	default:
		ifaces = discovery.LoopbackInterfaces()
		ips = []string{"127.0.0.1"}
	}

	newStop, err := discovery.Advertise(currentName, n.ID.PeerID(), newPort, ifaces, ips)
	if err != nil {
		return 0, fmt.Errorf("advertise: %w", err)
	}
	bctx, bcancel := context.WithCancel(context.Background())
	go func() {
		if err := discovery.Browse(bctx, n.Peers, ifaces); err != nil && bctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "agentmesh: browse: %v\n", err)
		}
	}()

	n.mu.Lock()
	n.port = newPort
	n.visibility = target
	n.mdnsStop = newStop
	n.browseCancel = bcancel
	n.mu.Unlock()
	return newPort, nil
}

// Start brings the node up at the given visibility for the first time. After
// Start, OpenLAN/CloseLAN are the way to change visibility.
func (n *Node) Start(target string) (int, error) {
	if target != VisibilityLoopback && target != VisibilityLAN {
		return 0, fmt.Errorf("Start: unknown visibility %q", target)
	}
	return n.transition(target)
}

func (n *Node) OpenLAN() (int, error)  { return n.transition(VisibilityLAN) }
func (n *Node) CloseLAN() (int, error) { return n.transition(VisibilityLoopback) }

// Shutdown stops mDNS advertise/browse and closes the listener. Best-effort,
// safe to call multiple times.
func (n *Node) Shutdown() {
	n.mu.Lock()
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
	if n.Server != nil {
		n.Server.Stop(context.Background())
	}
}

// SetName updates the display name and re-advertises on mDNS with the new
// name. Safe to call before Start.
func (n *Node) SetName(name string) error {
	if name == "" {
		return fmt.Errorf("name must not be empty")
	}
	n.mu.Lock()
	n.displayName = name
	if n.Server != nil {
		n.Server.SelfName = name
	}
	if n.Client != nil {
		n.Client.SelfName = name
	}
	target := n.visibility
	n.mu.Unlock()
	if target == "" {
		return nil // not started yet, name will be used at Start
	}
	_, err := n.transition(target)
	return err
}

// SetInitialName seeds the display name before Start. Doesn't trigger any
// network activity.
func (n *Node) SetInitialName(name string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.displayName = name
	if n.Server != nil {
		n.Server.SelfName = name
	}
	if n.Client != nil {
		n.Client.SelfName = name
	}
}

// Snapshot returns a copy of the externally-visible state.
func (n *Node) Snapshot() (name, visibility string, port int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.displayName, n.visibility, n.port
}

// ----------------------------------------------------------------------------
// MCP server construction
// ----------------------------------------------------------------------------

// NewMCPServer constructs the mark3labs/mcp-go server for this node, wires
// up tool handlers, resources, hooks, and the inbox push -> MCP notification
// path. Returns the server ready for ServeStdio.
func NewMCPServer(node *Node) *server.MCPServer {
	hooks := &server.Hooks{}
	// Capture the per-session context so we can send unsolicited notifications
	// (e.g. "inbox changed") even outside the response path of a tool call.
	hooks.AddOnRegisterSession(func(ctx context.Context, _ server.ClientSession) {
		node.mu.Lock()
		node.clientCtx = ctx
		node.mu.Unlock()
	})

	s := server.NewMCPServer("agentmesh", transport.Version,
		server.WithToolCapabilities(true),
		server.WithResourceCapabilities(true, true), // subscribe + listChanged
		server.WithInstructions(ServerInstructions),
		server.WithHooks(hooks),
	)
	node.mcpServer = s

	// Inbox MCP resource: lets harnesses subscribe to inbox activity.
	s.AddResource(
		mcplib.NewResource(InboxResourceURI, "agentmesh inbox",
			mcplib.WithResourceDescription(
				"Live inbox of messages received from mesh peers. Harnesses can subscribe "+
					"to notifications/resources/updated for this URI to know when new "+
					"messages arrive. Recommended access pattern: the agent calls mesh_inbox "+
					"at the start of each turn (per server instructions) rather than reading "+
					"this resource directly.",
			),
			mcplib.WithMIMEType("application/json"),
		),
		node.readInboxResource,
	)

	registerTools(s, node)

	// Push: on every inbox arrival, send notifications/resources/updated.
	// Best-effort - returns nil-no-op if no client session yet or context is
	// stale; never blocks the inbox push path.
	node.Inbox.OnPush(func(_ inbox.Message) {
		node.mu.Lock()
		ctx := node.clientCtx
		srv := node.mcpServer
		node.mu.Unlock()
		if ctx == nil || srv == nil {
			return
		}
		_ = srv.SendNotificationToClient(ctx, "notifications/resources/updated",
			map[string]any{"uri": InboxResourceURI})
	})

	return s
}

func (n *Node) readInboxResource(_ context.Context, _ mcplib.ReadResourceRequest) ([]mcplib.ResourceContents, error) {
	msgs, cursor := n.Inbox.Since(0)
	body, err := json.MarshalIndent(map[string]any{
		"messages": msgs,
		"cursor":   cursor,
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	return []mcplib.ResourceContents{
		mcplib.TextResourceContents{
			URI:      InboxResourceURI,
			MIMEType: "application/json",
			Text:     string(body),
		},
	}, nil
}

// ----------------------------------------------------------------------------
// Tool registration
// ----------------------------------------------------------------------------

func registerTools(s *server.MCPServer, n *Node) {
	s.AddTool(mcplib.NewTool("mesh_whoami",
		mcplib.WithDescription(
			"Returns this node's peer_id, display name, port, current visibility "+
				"(loopback or lan), and version. Loopback means same-machine peers "+
				"can see you but other machines cannot.",
		),
	), n.whoami)

	s.AddTool(mcplib.NewTool("mesh_peers",
		mcplib.WithDescription(
			"List currently-discovered peers. In loopback mode you'll see only "+
				"sessions on the same machine; after mesh_open_lan you'll also see "+
				"peers on the local network.",
		),
	), n.peers)

	s.AddTool(mcplib.NewTool("mesh_open_lan",
		mcplib.WithDescription(
			"Extend this session's visibility from same-machine-only to the local "+
				"network. Rebinds the listener to all interfaces, advertises on mDNS "+
				"across the LAN, and starts discovering LAN peers. Same-machine peers "+
				"remain visible. Use this when you want to coordinate with sessions on "+
				"other machines.",
		),
	), n.openLAN)

	s.AddTool(mcplib.NewTool("mesh_close_lan",
		mcplib.WithDescription(
			"Drop back to loopback-only. Stops LAN advertising and browsing; rebinds "+
				"the listener to 127.0.0.1. Same-machine peers remain visible; other "+
				"machines can no longer reach this session.",
		),
	), n.closeLAN)

	s.AddTool(mcplib.NewTool("mesh_set_name",
		mcplib.WithDescription(
			"Set this node's display name. Other peers see this name in their "+
				"mesh_peers output. Defaults to '<folder>@<branch>#<tag>' derived "+
				"from the working directory; override when you want a more "+
				"descriptive label.",
		),
		mcplib.WithString("name", mcplib.Required(),
			mcplib.Description("New display name, short and recognisable.")),
	), n.setName)

	s.AddTool(mcplib.NewTool("mesh_send",
		mcplib.WithDescription(
			"Send a JSON message to a specific peer or broadcast to all known peers. "+
				"Fire-and-forget over mTLS. Returns per-target success/failure.",
		),
		mcplib.WithString("to", mcplib.Required(),
			mcplib.Description("Target peer_id (full hex from mesh_peers) or \"*\" to broadcast.")),
		mcplib.WithString("topic",
			mcplib.Description("Optional topic string the receiver can route on.")),
		mcplib.WithObject("body",
			mcplib.Description("Arbitrary JSON object to send as the message body.")),
	), n.send)

	s.AddTool(mcplib.NewTool("mesh_inbox",
		mcplib.WithDescription(
			"Read incoming messages from peers. CALL THIS AT THE START OF EVERY TURN "+
				"before doing other work - peers may have sent you something since your "+
				"last reply that needs an answer. Returns messages and a cursor; pass "+
				"the cursor back as 'since' on the next call to read only newer messages. "+
				"Set wait_seconds>0 to long-poll if you want to block until something arrives.",
		),
		mcplib.WithNumber("since",
			mcplib.Description("Return only messages with id > since. Default 0 (= all).")),
		mcplib.WithNumber("wait_seconds",
			mcplib.Description("Block up to N seconds for new messages (0 = non-blocking).")),
	), n.inbox)

	s.AddTool(mcplib.NewTool("mesh_share",
		mcplib.WithDescription(
			"Register a local file as fetchable by mesh peers. Returns a handle peers "+
				"can pass to mesh_fetch. Use allow_peers to restrict access to specific "+
				"peer_ids.",
		),
		mcplib.WithString("path", mcplib.Required(),
			mcplib.Description("Absolute or working-dir-relative path to the file.")),
		mcplib.WithString("name",
			mcplib.Description("Optional display name (defaults to basename).")),
		mcplib.WithArray("allow_peers",
			mcplib.Description("Optional list of peer_ids permitted to fetch. Empty = any discovered peer.")),
		mcplib.WithNumber("ttl_seconds",
			mcplib.Description("Auto-expire the share after N seconds. 0 = no expiry.")),
	), n.share)

	s.AddTool(mcplib.NewTool("mesh_fetch",
		mcplib.WithDescription(
			"Fetch a file shared by another peer. Pass save_to for binary files or "+
				"anything over ~256 KB; without save_to the file content is returned "+
				"inline (text only).",
		),
		mcplib.WithString("peer_id", mcplib.Required(),
			mcplib.Description("Peer to fetch from.")),
		mcplib.WithString("handle", mcplib.Required(),
			mcplib.Description("Share handle received from the peer.")),
		mcplib.WithString("save_to",
			mcplib.Description("Where to write the file on disk. Required for binary/large blobs.")),
	), n.fetch)

	s.AddTool(mcplib.NewTool("mesh_shares",
		mcplib.WithDescription("List the files this session is currently offering via mesh_share."),
	), n.listShares)

	s.AddTool(mcplib.NewTool("mesh_unshare",
		mcplib.WithDescription("Revoke a local share by handle."),
		mcplib.WithString("handle", mcplib.Required()),
	), n.unshare)
}

// ----------------------------------------------------------------------------
// Arg helpers
// ----------------------------------------------------------------------------

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

// ----------------------------------------------------------------------------
// Tool handlers
// ----------------------------------------------------------------------------

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
		out["hint"] = "Loopback-only: other machines can't reach this session. Call mesh_open_lan to expose it on the LAN."
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
		"message":    "Now reachable on the LAN. Same-machine peers stay visible. LAN peers will appear in mesh_peers within a few seconds.",
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
		"message":    "Back to loopback - same-machine peers still visible, no LAN exposure.",
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
		"data":  string(buf.b),
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

// ----------------------------------------------------------------------------
// Helpers
// ----------------------------------------------------------------------------

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
