// agentmesh is a tiny LAN P2P node + MCP server for harnessed AI agents.
//
// Subcommands:
//
//	agentmesh serve [--open-lan]      MCP stdio server. Loopback-only by
//	                                  default; the agent calls mesh_open_lan
//	                                  at runtime to expose the session.
//	agentmesh hook prompt-inject      Read unread mesh messages for this
//	                                  session and print them to stdout in a
//	                                  format the harness will prepend to the
//	                                  user's next prompt. Designed for
//	                                  Claude Code's UserPromptSubmit hook.
//	agentmesh version                 Print version + exit.
//
// Identity is ephemeral - a fresh Ed25519 keypair per process. Session inbox
// state lives at ~/.agentmesh/sessions/<key>/ and is cleaned up on exit (+
// stale entries swept on startup).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/mark3labs/mcp-go/server"

	"github.com/BlueHeisenberg/agentmesh/internal/discovery"
	"github.com/BlueHeisenberg/agentmesh/internal/identity"
	"github.com/BlueHeisenberg/agentmesh/internal/inbox"
	mcpserver "github.com/BlueHeisenberg/agentmesh/internal/mcp"
	"github.com/BlueHeisenberg/agentmesh/internal/sessionstore"
	"github.com/BlueHeisenberg/agentmesh/internal/shares"
	"github.com/BlueHeisenberg/agentmesh/internal/transport"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		cmdServe(os.Args[2:])
	case "hook":
		cmdHook(os.Args[2:])
	case "version":
		fmt.Println("agentmesh " + transport.Version)
	case "whoami":
		cmdWhoami()
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

// cmdWhoami prints a short post-install self-check: version, identity model
// (ephemeral in v0.4+), and the default display name we'd derive in the
// current working directory. Used by the install scripts to confirm a
// fresh binary works.
func cmdWhoami() {
	// Reuse a one-off ephemeral identity just to show the tag form.
	id, err := identity.Ephemeral()
	tag := ""
	if err == nil {
		tag = id.Tag()
	}
	defaultName := identity.DefaultDisplayName(tag)
	fmt.Println("agentmesh " + transport.Version)
	fmt.Println("identity: ephemeral (fresh Ed25519 keypair per `agentmesh serve`)")
	fmt.Println("default name (this dir): " + defaultName)
	fmt.Println("default visibility: loopback - agent calls mesh_open_lan to advertise on the LAN")
}

func usage() {
	fmt.Fprintf(os.Stderr, `agentmesh - the agents in your editor sessions, talking to each other.

Subcommands:
  serve [--open-lan]            Run as an MCP stdio server. Loopback-only
                                by default (same-machine peers visible); the
                                agent can call mesh_open_lan to also accept
                                LAN traffic.
  hook prompt-inject            Emit any unread mesh messages for this
                                session to stdout. Run from a harness's
                                UserPromptSubmit hook so peers' messages
                                appear in the user's next prompt.
  whoami                        Print a short self-check (version, identity
                                model, default name in current dir).
  version                       Print version and exit.

Identity is ephemeral - a fresh Ed25519 keypair per process. No on-disk
identity to manage.
`)
}

// ----------------------------------------------------------------------------
// serve
// ----------------------------------------------------------------------------

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	openLAN := fs.Bool("open-lan", false, "start in LAN mode immediately (default: loopback; the agent can call mesh_open_lan later)")
	nameFlag := fs.String("name", "", "explicit display name (default: derived from CWD + git branch + peer_id tag)")
	_ = fs.Parse(args)

	// Best-effort cleanup of stale session dirs from previously-killed
	// harness sessions. Runs before we create our own so we don't sweep
	// ourselves.
	_ = sessionstore.CleanupStaleDirs()

	id, err := identity.Ephemeral()
	if err != nil {
		die(err)
	}

	displayName := *nameFlag
	if displayName == "" {
		displayName = identity.DefaultDisplayName(id.Tag())
	}

	ib := inbox.New(2000)
	sh := shares.New()
	reg := discovery.NewRegistry(id.PeerID())

	cert, err := id.TLSCertificate()
	if err != nil {
		die(fmt.Errorf("build tls cert: %w", err))
	}

	srv := &transport.Server{
		SelfPeerID: id.PeerID(),
		SelfName:   displayName,
		Cert:       cert,
		Inbox:      ib,
		Shares:     sh,
	}

	// Open the per-session sessionstore. Failure is non-fatal - the hook
	// integration just won't work, but the mesh itself still does.
	store, storeErr := sessionstore.New()
	if storeErr != nil {
		fmt.Fprintf(os.Stderr, "agentmesh: sessionstore: %v (hook delivery disabled)\n", storeErr)
	}

	node := &mcpserver.Node{
		ID:     id,
		Inbox:  ib,
		Peers:  reg,
		Shares: sh,
		Server: srv,
		Client: transport.NewClient(id.PeerID(), displayName, cert),
	}
	node.SetInitialName(displayName)

	// Persist every incoming message into the per-session jsonl file so the
	// UserPromptSubmit hook can read them on the user's next prompt.
	if store != nil {
		ib.OnPush(func(m inbox.Message) {
			if err := store.Append(m); err != nil {
				fmt.Fprintf(os.Stderr, "agentmesh: sessionstore append: %v\n", err)
			}
		})
	}

	// Build the MCP server (registers tools, resources, hooks, and wires the
	// inbox -> MCP notification path).
	mcp := mcpserver.NewMCPServer(node)

	// Bring the node up.
	target := mcpserver.VisibilityLoopback
	if *openLAN {
		target = mcpserver.VisibilityLAN
	}
	if _, err := node.Start(target); err != nil {
		die(fmt.Errorf("start node: %w", err))
	}

	// Graceful shutdown - stdio MCP will also exit when stdin closes, but
	// catch signals so Ctrl-C from a manual invocation tears down mDNS, the
	// listener, and the session dir cleanly.
	sigCtx, stopSig := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCtx.Done()
		node.Shutdown()
		if store != nil {
			_ = os.RemoveAll(store.Dir())
			store.Close()
		}
	}()
	defer stopSig()

	_, vis, runtimePort := node.Snapshot()
	fmt.Fprintf(os.Stderr, "agentmesh: %s as %s on :%d (peer_id=%s)\n",
		vis, displayName, runtimePort, id.PeerID()[:16])

	if err := server.ServeStdio(mcp); err != nil {
		die(fmt.Errorf("mcp serve: %w", err))
	}

	// stdin closed; tear down explicitly. The signal goroutine may also
	// run this path - both are idempotent.
	node.Shutdown()
	if store != nil {
		_ = os.RemoveAll(store.Dir())
		store.Close()
	}
}

// ----------------------------------------------------------------------------
// hook
// ----------------------------------------------------------------------------

func cmdHook(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: agentmesh hook prompt-inject")
		os.Exit(2)
	}
	switch args[0] {
	case "prompt-inject":
		cmdHookPromptInject()
	default:
		fmt.Fprintf(os.Stderr, "unknown hook %q (expected: prompt-inject)\n", args[0])
		os.Exit(2)
	}
}

// cmdHookPromptInject reads any unread mesh messages for this session and
// emits the documented Claude Code UserPromptSubmit JSON envelope
// (hookSpecificOutput.additionalContext) so they become part of the agent's
// context for the upcoming turn. The injected text is framed imperatively:
// the agent is told these arrived between turns, that the user hasn't seen
// them, and that it should surface sender + topic + summary to the user
// before engaging with the content - so an agent answering an unrelated
// question still relays peer activity instead of silently swallowing it.
//
// Outputs nothing (and exits 0) when there are no unread messages, no
// session store, or any read/marshal error occurs - never fail the user's
// prompt because of a delivery hiccup.
func cmdHookPromptInject() {
	store, err := sessionstore.New()
	if err != nil {
		return
	}
	defer store.Close()

	msgs, err := store.ReadUnread()
	if err != nil || len(msgs) == 0 {
		return
	}

	context := formatHookContext(msgs)
	payload := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "UserPromptSubmit",
			"additionalContext": context,
		},
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return
	}
	if _, err := os.Stdout.Write(out); err != nil {
		return
	}
	_ = store.MarkRead()
}

// formatHookContext builds the additionalContext string for the agent. The
// header is directive on purpose: the agent must mention these messages to
// the user in its reply, even when the user's prompt is on a different
// topic. Without that prompt, agents tend to internalise peer input and
// move on, which defeats the "messages just appear" UX.
func formatHookContext(msgs []sessionstore.Message) string {
	var b strings.Builder

	plural := "messages"
	if len(msgs) == 1 {
		plural = "message"
	}
	fmt.Fprintf(&b, "[mesh-inbox] %d new peer %s arrived since your last turn.\n", len(msgs), plural)
	b.WriteString("These came from other AI agents on the same machine or LAN. ")
	b.WriteString("THE USER HAS NOT SEEN THEM. Before answering the user's prompt, briefly surface each one: sender name, topic, and a one-line summary of the content. ")
	b.WriteString("Then engage with the message content as part of your reply - treat it as a legitimate input alongside the user's prompt, even if the user asked about something unrelated.\n\n")

	for i, m := range msgs {
		short := m.FromPeerID
		if len(short) > 8 {
			short = short[:8]
		}
		topic := m.Topic
		if topic == "" {
			topic = "(no topic)"
		}
		ts := m.ReceivedAt.Format("15:04:05")
		first := ""
		if m.FirstContact {
			first = "  [first contact from this peer]"
		}
		fmt.Fprintf(&b, "message %d: from %s (peer_id=%s) topic=%q at %s%s\n",
			i+1, m.FromName, short, topic, ts, first)

		var pretty []byte
		if len(m.Body) > 0 {
			if tmp, perr := indentJSON(m.Body); perr == nil {
				pretty = tmp
			} else {
				pretty = m.Body
			}
		}
		if len(pretty) > 0 {
			b.WriteString("body:\n")
			for _, line := range strings.Split(string(pretty), "\n") {
				fmt.Fprintf(&b, "  %s\n", line)
			}
		}
		b.WriteString("\n")
	}
	b.WriteString("[end mesh-inbox]")
	return b.String()
}

func indentJSON(raw []byte) ([]byte, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return json.MarshalIndent(v, "", "  ")
}

// ----------------------------------------------------------------------------

func die(err error) {
	fmt.Fprintf(os.Stderr, "agentmesh: %v\n", err)
	os.Exit(1)
}
