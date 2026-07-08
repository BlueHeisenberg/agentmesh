// agentmesh is a tiny LAN P2P node + MCP server for harnessed AI agents.
//
// Subcommands:
//
//	agentmesh serve [--open-lan]      MCP stdio server. Loopback-only by
//	                                  default; the agent calls mesh_open_lan
//	                                  at runtime to expose the session.
//	agentmesh hook prompt-inject      If this session has unread mesh
//	                                  messages, print a one-line unread-count
//	                                  ping for the harness to prepend to the
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
	"math/rand"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"github.com/BlueHeisenberg/agentmesh/internal/identity"
	"github.com/BlueHeisenberg/agentmesh/internal/inbox"
	mcpserver "github.com/BlueHeisenberg/agentmesh/internal/mcp"
	"github.com/BlueHeisenberg/agentmesh/internal/sessionstore"
	"github.com/BlueHeisenberg/agentmesh/internal/shares"
	"github.com/BlueHeisenberg/agentmesh/internal/transport"
	"github.com/BlueHeisenberg/agentmesh/internal/update"
	"github.com/BlueHeisenberg/agentmesh/pkg/discovery"
)

// autoUpdateInterval is how often each running `agentmesh serve` checks GitHub
// for a newer release. 6h keeps the request rate gentle (max 4 calls/day per
// running session) while making any new release land on the user's machine
// within a typical workday.
const autoUpdateInterval = 6 * time.Hour

// selfUpdateRepo is the GitHub owner/repo we self-update from. Kept as a
// package-level const so it lives next to the other release-y bits and
// is easy to grep for.
const selfUpdateRepo = "BlueHeisenberg/agentmesh"

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
	case "self-update":
		cmdSelfUpdate(os.Args[2:])
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
  hook prompt-inject            Emit a one-line unread-count ping when this
                                session has unread mesh messages. Run from a
                                harness's UserPromptSubmit hook; the agent
                                then reads the messages via mesh_inbox.
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

	// pkg/discovery no longer reads the env itself; wire the debug flag here.
	discovery.Debug = os.Getenv("AGENTMESH_DEBUG") != ""

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

	// Background auto-update poll. Off only when the user explicitly opted
	// out at install time (env injected into the harness mcp config). The
	// loop checks GitHub once at startup (after a small jitter) and then
	// every autoUpdateInterval. SelfReplace only swaps the on-disk binary;
	// the running process keeps serving until the harness restarts it, so
	// the update is invisible mid-session and safe to apply concurrently
	// across multiple sessions on the same machine.
	autoUpdateCtx, stopAutoUpdate := context.WithCancel(context.Background())
	defer stopAutoUpdate()
	if os.Getenv("AGENTMESH_NO_AUTOUPDATE") == "" {
		go runAutoUpdateLoop(autoUpdateCtx)
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

// cmdHookPromptInject checks for unread mesh messages for this session and,
// when there are any, emits the documented Claude Code UserPromptSubmit JSON
// envelope (hookSpecificOutput.additionalContext) containing a single-line
// unread-count ping. Message bodies are deliberately NOT injected - the agent
// reads them via the mesh_inbox tool - so an idle mesh costs zero context and
// a busy one costs one line. Each message is pinged once (MarkRead after a
// successful emit); it stays readable in mesh_inbox regardless.
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

	payload := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "UserPromptSubmit",
			"additionalContext": formatHookPing(msgs),
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

// formatHookPing builds the one-line unread notice. It names up to three
// distinct senders, flags first contacts (the trust-relevant signal), and
// tells the agent the one thing it must do: call mesh_inbox and surface the
// messages to the user, who hasn't seen them.
func formatHookPing(msgs []sessionstore.Message) string {
	var senders []string
	seen := map[string]bool{}
	firstContact := false
	for _, m := range msgs {
		if m.FirstContact {
			firstContact = true
		}
		if seen[m.FromName] {
			continue
		}
		seen[m.FromName] = true
		if len(senders) < 3 {
			senders = append(senders, m.FromName)
		}
	}
	from := strings.Join(senders, ", ")
	if len(seen) > 3 {
		from += fmt.Sprintf(" +%d more", len(seen)-3)
	}
	plural := "messages"
	if len(msgs) == 1 {
		plural = "message"
	}
	note := ""
	if firstContact {
		note = "; includes a first contact"
	}
	return fmt.Sprintf("[mesh] %d unread peer %s from %s%s - the user has not seen them; call mesh_inbox to read, then surface them to the user.",
		len(msgs), plural, from, note)
}

// ----------------------------------------------------------------------------
// self-update
// ----------------------------------------------------------------------------

// cmdSelfUpdate is a one-shot synchronous updater: ask GitHub for the
// latest release, compare with transport.Version, and (if newer, or
// --force) download + verify + replace the running binary in place.
//
// Exit codes:
//
//	0  updated, or already on the latest version
//	1  failure (network, checksum, rename, etc.)
//
// Human summary is printed to stdout; phase progress lines come from the
// update package on stderr.
func cmdSelfUpdate(args []string) {
	fs := flag.NewFlagSet("self-update", flag.ExitOnError)
	force := fs.Bool("force", false, "always re-download and replace, even when already on the latest version")
	_ = fs.Parse(args)

	ctx := context.Background()

	tag, err := update.LatestRelease(ctx, selfUpdateRepo)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentmesh self-update: %v\n", err)
		os.Exit(1)
	}

	if !*force && !update.IsNewer(transport.Version, tag) {
		fmt.Printf("already on the latest version (v%s)\n", transport.Version)
		return
	}

	if err := update.SelfReplace(ctx, selfUpdateRepo, tag); err != nil {
		fmt.Fprintf(os.Stderr, "agentmesh self-update: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("updated to %s (restart your harness to pick it up)\n", tag)
}

// runAutoUpdateLoop is the goroutine spawned by `serve` when auto-update is
// enabled. It does an immediate jittered check at startup so a fresh install
// can pick up a same-day release, then loops every autoUpdateInterval.
//
// Errors at any step are logged to stderr and otherwise ignored. The update
// path is best-effort: a broken network, an unwritable install dir, a flaky
// CDN should never block the user's session.
func runAutoUpdateLoop(ctx context.Context) {
	// Initial jitter so multiple sessions started in lockstep (e.g. a user
	// reopening the same Claude Code window twice in quick succession) don't
	// hammer GitHub or race each other on the same rename.
	jitter := time.Duration(5+rand.Intn(26)) * time.Second // 5..30s
	select {
	case <-ctx.Done():
		return
	case <-time.After(jitter):
	}

	runOnce := func() {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		tag, err := update.LatestRelease(ctx, selfUpdateRepo)
		if err != nil {
			// Quietly: failed checks shouldn't spam the MCP transcript.
			return
		}
		if !update.IsNewer(transport.Version, tag) {
			return
		}
		if err := update.SelfReplace(ctx, selfUpdateRepo, tag); err != nil {
			fmt.Fprintf(os.Stderr, "agentmesh auto-update: %v\n", err)
			return
		}
		fmt.Fprintf(os.Stderr,
			"agentmesh auto-update: replaced binary with %s; restart the harness to load it\n", tag)
	}

	runOnce()
	t := time.NewTicker(autoUpdateInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			runOnce()
		}
	}
}

// ----------------------------------------------------------------------------

func die(err error) {
	fmt.Fprintf(os.Stderr, "agentmesh: %v\n", err)
	os.Exit(1)
}
