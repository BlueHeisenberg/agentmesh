// Package mcp wires the agentmesh node to a stdio MCP server. Each MCP tool
// is a thin shim over the Node API.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/BlueHeisenberg/agentmesh/internal/discovery"
	"github.com/BlueHeisenberg/agentmesh/internal/identity"
	"github.com/BlueHeisenberg/agentmesh/internal/inbox"
	"github.com/BlueHeisenberg/agentmesh/internal/shares"
	"github.com/BlueHeisenberg/agentmesh/internal/transport"
)

// Node holds all the runtime state the MCP tools touch.
type Node struct {
	ID       *identity.Identity
	Port     int
	Inbox    *inbox.Inbox
	Peers    *discovery.Registry
	Shares   *shares.Registry
	Client   *transport.Client
}

func (n *Node) Register(s *server.MCPServer) {
	s.AddTool(mcplib.NewTool("mesh_whoami",
		mcplib.WithDescription("Return this node's peer id, display name, and local HTTP address."),
	), n.whoami)

	s.AddTool(mcplib.NewTool("mesh_peers",
		mcplib.WithDescription("List peers discovered on the local network."),
	), n.peers)

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
	return jsonResult(map[string]any{
		"peer_id": n.ID.PeerID(),
		"name":    n.ID.Name,
		"port":    n.Port,
		"version": transport.Version,
	})
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
