package main_test

// End-to-end test: spawn two `agentmesh serve` processes with separate
// AGENTMESH_HOME dirs, drive each via JSON-RPC over stdio, and verify:
//   1. each side discovers the other via mDNS within a few seconds
//   2. mesh_send delivers a message to the peer's inbox
//   3. mesh_share + mesh_fetch round-trips a file

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// rpc is a tiny JSON-RPC 2.0 client over stdio.
type rpc struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	mu     sync.Mutex
	id     atomic.Int64
}

func startNode(t *testing.T, home, name string) *rpc {
	t.Helper()
	// --open-lan: the default is loopback-only since v0.3.0; the test needs
	// the nodes to advertise on mDNS straight away.
	cmd := exec.Command("./agentmesh-test", "serve", "--open-lan", "--name="+name)
	cmd.Env = append(os.Environ(), "AGENTMESH_HOME="+home)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	r := &rpc{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout)}
	t.Cleanup(func() {
		stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	// initialize
	if _, err := r.call("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "e2e-test", "version": "0"},
	}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if err := r.notify("notifications/initialized", map[string]any{}); err != nil {
		t.Fatalf("initialized notify: %v", err)
	}
	return r
}

func (r *rpc) call(method string, params any) (json.RawMessage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := r.id.Add(1)
	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
	buf, _ := json.Marshal(req)
	if _, err := r.stdin.Write(append(buf, '\n')); err != nil {
		return nil, err
	}
	line, err := r.stdout.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("decode resp: %w (line=%s)", err, line)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("rpc error: %s", resp.Error.Message)
	}
	return resp.Result, nil
}

func (r *rpc) notify(method string, params any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	req := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
	buf, _ := json.Marshal(req)
	_, err := r.stdin.Write(append(buf, '\n'))
	return err
}

// callTool invokes a tool and decodes the first text content as JSON into v.
func (r *rpc) callTool(name string, args map[string]any, v any) error {
	res, err := r.call("tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		return err
	}
	var wrap struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(res, &wrap); err != nil {
		return fmt.Errorf("decode tool result: %w (%s)", err, res)
	}
	if wrap.IsError {
		return fmt.Errorf("tool %s returned error: %s", name, wrap.Content)
	}
	if len(wrap.Content) == 0 {
		return fmt.Errorf("tool %s returned no content", name)
	}
	if v == nil {
		return nil
	}
	return json.Unmarshal([]byte(wrap.Content[0].Text), v)
}

func TestEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e requires mDNS on the loopback interface; -short skips")
	}
	// Build the binary into a known location.
	build := exec.Command("go", "build", "-o", "agentmesh-test", "./")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build: %v", err)
	}
	defer os.Remove("agentmesh-test")

	homeA := t.TempDir()
	homeB := t.TempDir()

	a := startNode(t, homeA, "node-a")
	b := startNode(t, homeB, "node-b")

	// Give mDNS a moment to settle.
	var aPeerOnB, bPeerOnA string
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var resA struct {
			Peers []struct {
				PeerID string `json:"peer_id"`
				Name   string `json:"name"`
			} `json:"peers"`
		}
		var resB struct {
			Peers []struct {
				PeerID string `json:"peer_id"`
				Name   string `json:"name"`
			} `json:"peers"`
		}
		if err := a.callTool("mesh_peers", map[string]any{}, &resA); err != nil {
			t.Fatalf("a.mesh_peers: %v", err)
		}
		if err := b.callTool("mesh_peers", map[string]any{}, &resB); err != nil {
			t.Fatalf("b.mesh_peers: %v", err)
		}
		for _, p := range resA.Peers {
			if p.Name == "node-b" {
				bPeerOnA = p.PeerID
			}
		}
		for _, p := range resB.Peers {
			if p.Name == "node-a" {
				aPeerOnB = p.PeerID
			}
		}
		if bPeerOnA != "" && aPeerOnB != "" {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if bPeerOnA == "" || aPeerOnB == "" {
		t.Fatalf("peers did not discover each other (a sees %q, b sees %q)", bPeerOnA, aPeerOnB)
	}

	// Send a message A -> B.
	if err := a.callTool("mesh_send", map[string]any{
		"to":    bPeerOnA,
		"topic": "greeting",
		"body":  map[string]any{"hello": "world"},
	}, nil); err != nil {
		t.Fatalf("mesh_send: %v", err)
	}

	// B reads inbox.
	var inboxRes struct {
		Messages []struct {
			Topic    string          `json:"topic"`
			FromName string          `json:"from_name"`
			Body     json.RawMessage `json:"body"`
		} `json:"messages"`
		Cursor int64 `json:"cursor"`
	}
	if err := b.callTool("mesh_inbox", map[string]any{"wait_seconds": 5.0}, &inboxRes); err != nil {
		t.Fatalf("b.mesh_inbox: %v", err)
	}
	if len(inboxRes.Messages) == 0 {
		t.Fatalf("B inbox is empty")
	}
	m := inboxRes.Messages[0]
	if m.FromName != "node-a" || m.Topic != "greeting" {
		t.Fatalf("unexpected msg: %+v", m)
	}
	if !bytes.Contains(m.Body, []byte(`"hello"`)) {
		t.Fatalf("unexpected body: %s", m.Body)
	}

	// File share: A shares, B fetches inline.
	tmpFile := filepath.Join(t.TempDir(), "hello.txt")
	if err := os.WriteFile(tmpFile, []byte("hi from A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var shareRes struct {
		Handle string `json:"handle"`
		Name   string `json:"name"`
	}
	if err := a.callTool("mesh_share", map[string]any{"path": tmpFile}, &shareRes); err != nil {
		t.Fatalf("mesh_share: %v", err)
	}
	if shareRes.Handle == "" {
		t.Fatal("empty handle")
	}

	var fetchRes struct {
		Name  string `json:"name"`
		Bytes int64  `json:"bytes"`
		Data  string `json:"data"`
	}
	if err := b.callTool("mesh_fetch", map[string]any{
		"peer_id": aPeerOnB,
		"handle":  shareRes.Handle,
	}, &fetchRes); err != nil {
		t.Fatalf("mesh_fetch: %v", err)
	}
	if !strings.Contains(fetchRes.Data, "hi from A") {
		t.Fatalf("fetched data wrong: %q", fetchRes.Data)
	}
}
