// Package identity holds the agentmesh-flavored identity layer: a thin
// re-export of pkg/identity (the generic Ed25519 keypair + mTLS cert) plus
// the display-name derivation that is specific to agentmesh's session model.
//
// agentmesh identity is ephemeral by design: a fresh keypair is generated at
// every `agentmesh serve` startup and exists only in memory. There is no
// on-disk persistence and no shared identity between sessions on the same
// machine. This is the property that makes multiple harness sessions on one
// machine mutually visible — each session has a distinct peer_id, so the
// registry's self-filter doesn't hide them from each other.
//
// The tradeoff: peer_id changes on every restart. `allow_peers` lists in
// mesh_share are session-scoped — if either side restarts, re-share. That's
// the right shape for chat-style use; persistent identities live in
// pkg/identity (FromPrivateKey) for consumers like lore that need them.
package identity

import (
	"os"
	"path/filepath"
	"strings"

	pkgidentity "github.com/BlueHeisenberg/agentmesh/pkg/identity"
)

// Identity is re-exported from pkg/identity so existing agentmesh code keeps
// working unchanged.
type Identity = pkgidentity.Identity

// Ephemeral generates a fresh Ed25519 keypair held only in memory.
func Ephemeral() (*Identity, error) { return pkgidentity.Ephemeral() }

// DefaultDisplayName composes the agent-facing display name for this node
// from the current working directory and git branch, optionally tagged with
// a 4-hex slice of the peer_id for same-project disambiguation. Examples:
//
//	~/projects/harnessP2P  on branch main, tag "a3f2"  -> "harnessP2P@main#a3f2"
//	~/projects/foo         no git,         tag "7c1e"  -> "foo#7c1e"
//	/                      weird,          tag "0000"  -> "<host>#0000"
func DefaultDisplayName(tag string) string {
	base := nameBase()
	if tag != "" {
		return base + "#" + tag
	}
	return base
}

func nameBase() string {
	wd, err := os.Getwd()
	if err != nil || wd == "" {
		return fallbackHost()
	}
	base := filepath.Base(wd)
	if base == "/" || base == "." || base == "" {
		return fallbackHost()
	}
	if branch := gitBranchAt(wd); branch != "" {
		return base + "@" + branch
	}
	return base
}

func fallbackHost() string {
	h, _ := os.Hostname()
	if h == "" {
		return "anonymous"
	}
	return strings.TrimSuffix(h, ".local")
}

// gitBranchAt walks up from dir looking for a .git directory and reads HEAD
// directly (no shelling out to git). Returns the branch name, a short detached
// HEAD hash, or "".
func gitBranchAt(dir string) string {
	for i := 0; i < 20; i++ {
		head := filepath.Join(dir, ".git", "HEAD")
		if data, err := os.ReadFile(head); err == nil {
			s := strings.TrimSpace(string(data))
			if strings.HasPrefix(s, "ref: refs/heads/") {
				return strings.TrimPrefix(s, "ref: refs/heads/")
			}
			if len(s) >= 7 {
				return s[:7] // detached HEAD
			}
			return ""
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
	return ""
}
