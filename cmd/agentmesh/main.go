// agentmesh is a tiny LAN P2P node + MCP server for harnessed AI agents.
//
// Usage:
//
//	agentmesh serve            # run as MCP stdio server (Claude Code spawns this)
//	agentmesh serve --open-lan # also start in LAN mode at boot (skip the
//	                           # default loopback-only state — useful for tests)
//	agentmesh whoami           # print this node's peer_id and exit
//
// Default behaviour: the node starts loopback-only — listener bound to
// 127.0.0.1, no mDNS advertise, no browse. It's invisible to other machines
// until the agent calls the mesh_open_lan MCP tool. Identity & keys live in
// $AGENTMESH_HOME (default ~/.agentmesh/).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/mark3labs/mcp-go/server"

	"github.com/BlueHeisenberg/agentmesh/internal/discovery"
	"github.com/BlueHeisenberg/agentmesh/internal/identity"
	"github.com/BlueHeisenberg/agentmesh/internal/inbox"
	mcpserver "github.com/BlueHeisenberg/agentmesh/internal/mcp"
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

func usage() {
	fmt.Fprintf(os.Stderr, `agentmesh - LAN P2P for AI agents

Subcommands:
  serve [--open-lan] [--name=NAME]    Run as an MCP stdio server. Starts in
                                      loopback-only mode unless --open-lan is
                                      passed; the agent can call mesh_open_lan
                                      at any time to expose the node to the LAN.
  whoami                              Print this node's peer_id and default
                                      display name, then exit.
`)
}

func cmdWhoami() {
	id, err := identity.LoadOrCreate("")
	if err != nil {
		die(err)
	}
	fmt.Printf("peer_id: %s\nname:    %s\n", id.PeerID(), identity.DefaultDisplayName())
}

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	openLAN := fs.Bool("open-lan", false, "start in LAN mode immediately (default: loopback; agent can call mesh_open_lan at runtime)")
	nameFlag := fs.String("name", "", "explicit display name (default: derived from CWD + git branch)")
	_ = fs.Parse(args)

	// Identity uses a stable hostname-derived default for the *stored* name
	// field (legacy); the runtime display name is what the agent and mDNS
	// actually see, and we recompute that from the CWD context here.
	id, err := identity.LoadOrCreate("")
	if err != nil {
		die(err)
	}
	displayName := *nameFlag
	if displayName == "" {
		displayName = identity.DefaultDisplayName()
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
	// Start in loopback mode by default. The agent can call mesh_open_lan to
	// rebind to 0.0.0.0 and start advertising.
	port, err := srv.Start(false)
	if err != nil {
		die(fmt.Errorf("transport start: %w", err))
	}

	node := &mcpserver.Node{
		ID:     id,
		Inbox:  ib,
		Peers:  reg,
		Shares: sh,
		Server: srv,
		Client: transport.NewClient(id.PeerID(), displayName, cert),
	}
	node.MarkInitialLoopback(port)
	if err := node.SetName(displayName); err != nil {
		die(fmt.Errorf("set name: %w", err))
	}

	if *openLAN {
		if _, err := node.OpenLAN(); err != nil {
			die(fmt.Errorf("open-lan at boot: %w", err))
		}
	}

	mcp := server.NewMCPServer("agentmesh", transport.Version,
		server.WithToolCapabilities(true),
	)
	node.Register(mcp)

	// Graceful shutdown — stdio server returns when stdin closes anyway, but
	// catch signals so a Ctrl-C from a manual `agentmesh serve` invocation
	// tears down cleanly.
	sigCtx, stopSig := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCtx.Done()
		_, _ = node.CloseLAN() // best-effort: stop mDNS + rebind to loopback
		srv.Stop(context.Background())
	}()
	defer stopSig()

	_, vis, runtimePort := node.Snapshot()
	fmt.Fprintf(os.Stderr, "agentmesh: %s as %s on :%d (peer_id=%s)\n",
		vis, displayName, runtimePort, id.PeerID()[:16])

	if err := server.ServeStdio(mcp); err != nil {
		die(fmt.Errorf("mcp serve: %w", err))
	}
}

func die(err error) {
	fmt.Fprintf(os.Stderr, "agentmesh: %v\n", err)
	os.Exit(1)
}
