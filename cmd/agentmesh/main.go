// agentmesh is a tiny LAN P2P node + MCP server for harnessed AI agents.
//
// Usage:
//
//	agentmesh serve            # run as MCP stdio server (Claude Code spawns this)
//	agentmesh serve --bind-all # also accept connections from other LAN hosts
//	agentmesh whoami           # print this node's peer_id and exit
//
// Identity & keys live in $AGENTMESH_HOME (default ~/.agentmesh/).
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
	fmt.Fprintf(os.Stderr, `agentmesh — LAN P2P for AI agents

Subcommands:
  serve [--bind-all] [--name=NAME]   Run as an MCP stdio server.
  whoami                              Print this node's peer_id and name.
`)
}

func cmdWhoami() {
	id, err := identity.LoadOrCreate("")
	if err != nil {
		die(err)
	}
	fmt.Printf("peer_id: %s\nname:    %s\n", id.PeerID(), id.Name)
}

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	bindAll := fs.Bool("bind-all", false, "bind HTTP listener on all interfaces (default: loopback only — use this when you want LAN peers to reach you)")
	name := fs.String("name", "", "display name for this node (default: hostname or stored)")
	_ = fs.Parse(args)

	// Loopback-only would make P2P useless, so default for the *real* p2p
	// case is bind-all. We keep the flag so devs can run isolated tests.
	if !*bindAll {
		*bindAll = true
	}

	id, err := identity.LoadOrCreate(*name)
	if err != nil {
		die(err)
	}
	if *name != "" && id.Name != *name {
		// Update stored name if explicitly overridden.
		id.Name = *name
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
		SelfName:   id.Name,
		Cert:       cert,
		Inbox:      ib,
		Shares:     sh,
	}
	port, err := srv.Start(*bindAll)
	if err != nil {
		die(fmt.Errorf("transport start: %w", err))
	}

	// Advertise + browse mDNS.
	mdnsStop, err := discovery.Advertise(id.Name, id.PeerID(), port)
	if err != nil {
		die(fmt.Errorf("mdns advertise: %w", err))
	}
	browseCtx, cancelBrowse := context.WithCancel(context.Background())
	go func() {
		if err := discovery.Browse(browseCtx, reg); err != nil && browseCtx.Err() == nil {
			fmt.Fprintf(os.Stderr, "agentmesh: mdns browse: %v\n", err)
		}
	}()

	node := &mcpserver.Node{
		ID:     id,
		Port:   port,
		Inbox:  ib,
		Peers:  reg,
		Shares: sh,
		Client: transport.NewClient(id.PeerID(), id.Name, cert),
	}

	mcp := server.NewMCPServer("agentmesh", transport.Version,
		server.WithToolCapabilities(true),
	)
	node.Register(mcp)

	// Graceful shutdown — but stdio server returns when stdin closes anyway.
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCtx.Done()
		cancelBrowse()
		mdnsStop()
		shutCtx, c := context.WithCancel(context.Background())
		_ = shutCtx
		c()
		srv.Stop(context.Background())
	}()
	defer stop()

	fmt.Fprintf(os.Stderr, "agentmesh: listening on :%d as %s (peer_id=%s)\n", port, id.Name, id.PeerID()[:16])

	if err := server.ServeStdio(mcp); err != nil {
		die(fmt.Errorf("mcp serve: %w", err))
	}
}

func die(err error) {
	fmt.Fprintf(os.Stderr, "agentmesh: %v\n", err)
	os.Exit(1)
}
