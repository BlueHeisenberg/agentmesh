// Package sessionstore persists per-session inbox state to disk so that a
// short-lived `agentmesh hook prompt-inject` subcommand (run by a harness's
// UserPromptSubmit hook) can read messages that the long-running agentmesh
// MCP server has received from mesh peers since the last user prompt turn.
//
// The two processes — the long-running MCP server and the one-shot hook
// command — are both spawned as direct children of the same harness (Claude
// Code, Cursor, etc.). They identify "their" session by the harness's PID,
// which they each obtain via os.Getppid(). CLAUDE_SESSION_ID is honored as
// an explicit override when present.
package sessionstore

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/BlueHeisenberg/agentmesh/internal/inbox"
)

// Message is the persisted message shape. It is an alias for the in-memory
// inbox.Message so callers can pass values straight through without copying.
type Message = inbox.Message

const (
	inboxFile     = "inbox.jsonl"
	cursorFile    = "cursor"
	cursorTmpFile = "cursor.tmp"
	dirMode       = 0o700
	fileMode      = 0o600
	staleAfter    = 24 * time.Hour
)

// Store is a per-session on-disk inbox. It is safe for concurrent Append
// from multiple goroutines in the same process; cross-process coordination
// is intentionally not provided — the writer (MCP server) and reader (hook
// command) do not run concurrently in normal use.
type Store struct {
	dir string

	mu sync.Mutex
	f  *os.File // append handle for inbox.jsonl, lazily opened
}

// New opens (creating if needed) the directory for this process's session
// at ~/.agentmesh/sessions/<session-key>/. The session key is
// $CLAUDE_SESSION_ID when non-empty, otherwise "ppid-<os.Getppid()>". PPID
// works because both the MCP server and the hook command are direct
// children of the same harness process.
func New() (*Store, error) {
	root, err := defaultRoot()
	if err != nil {
		return nil, fmt.Errorf("sessionstore: %w", err)
	}
	dir := filepath.Join(root, sessionKey())
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return nil, fmt.Errorf("sessionstore: %w", err)
	}
	return &Store{dir: dir}, nil
}

// Dir returns the absolute path of this session's directory.
func (s *Store) Dir() string { return s.dir }

// Append writes one JSON-encoded line to inbox.jsonl. The append file is
// opened lazily and held open for the lifetime of the Store; O_APPEND
// guarantees each write is positioned at the current end of file, which
// is sufficient for single-process concurrent writers.
func (s *Store) Append(msg Message) error {
	line, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("sessionstore: %w", err)
	}
	line = append(line, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.f == nil {
		f, err := os.OpenFile(filepath.Join(s.dir, inboxFile),
			os.O_APPEND|os.O_CREATE|os.O_WRONLY, fileMode)
		if err != nil {
			return fmt.Errorf("sessionstore: %w", err)
		}
		s.f = f
	}
	if _, err := s.f.Write(line); err != nil {
		return fmt.Errorf("sessionstore: %w", err)
	}
	return nil
}

// ReadUnread parses lines from inbox.jsonl starting at the byte offset
// recorded in the sibling `cursor` file, returning the decoded messages.
// It deliberately does not advance the cursor — callers should only call
// MarkRead once they have successfully consumed the messages (e.g.
// emitted them into the agent's prompt), so a crash between read and
// consume does not lose data.
func (s *Store) ReadUnread() ([]Message, error) {
	off, err := s.readCursor()
	if err != nil {
		return nil, fmt.Errorf("sessionstore: %w", err)
	}
	f, err := os.Open(filepath.Join(s.dir, inboxFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("sessionstore: %w", err)
	}
	defer f.Close()

	if off > 0 {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return nil, fmt.Errorf("sessionstore: %w", err)
		}
	}
	var out []Message
	sc := bufio.NewScanner(f)
	// Allow generously large lines — message bodies may include sizable
	// JSON blobs from peers.
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var m Message
		if err := json.Unmarshal(line, &m); err != nil {
			// Skip malformed lines rather than failing the whole read;
			// a partial write should not strand subsequent messages.
			continue
		}
		out = append(out, m)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("sessionstore: %w", err)
	}
	return out, nil
}

// MarkRead atomically sets the cursor to the current end of inbox.jsonl.
// Writing to cursor.tmp and renaming over `cursor` ensures a partial write
// never leaves the cursor in an inconsistent state.
func (s *Store) MarkRead() error {
	path := filepath.Join(s.dir, inboxFile)
	var end int64
	if fi, err := os.Stat(path); err == nil {
		end = fi.Size()
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("sessionstore: %w", err)
	}
	tmp := filepath.Join(s.dir, cursorTmpFile)
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(end, 10)), fileMode); err != nil {
		return fmt.Errorf("sessionstore: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(s.dir, cursorFile)); err != nil {
		return fmt.Errorf("sessionstore: %w", err)
	}
	return nil
}

// Close releases the append file handle, if any. Safe to call from defer
// and safe to call multiple times.
func (s *Store) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f != nil {
		_ = s.f.Close()
		s.f = nil
	}
}

func (s *Store) readCursor() (int64, error) {
	b, err := os.ReadFile(filepath.Join(s.dir, cursorFile))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	n, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		// A corrupt cursor should not be fatal — treat as 0 and let the
		// next MarkRead repair it.
		return 0, nil
	}
	if n < 0 {
		return 0, nil
	}
	return n, nil
}

// SessionDirRoot returns ~/.agentmesh/sessions. It panics only if the
// user's home directory cannot be resolved, which would indicate a broken
// environment; in normal operation it is infallible.
func SessionDirRoot() string {
	root, err := defaultRoot()
	if err != nil {
		// Fall back to a temp-dir path so callers always get a usable
		// string; New() will surface the real error.
		return filepath.Join(os.TempDir(), "agentmesh", "sessions")
	}
	return root
}

// CleanupStaleDirs walks SessionDirRoot and removes any subdirectory whose
// mtime is older than 24 hours. We use mtime rather than process-existence
// checks because (a) PIDs are reused and unreliable as liveness signals,
// and (b) the strategy must work uniformly across macOS, Linux, and
// Windows. Each Append bumps the inbox file's mtime, which propagates to
// the directory on most filesystems, keeping live sessions fresh.
func CleanupStaleDirs() error {
	root := SessionDirRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("sessionstore: %w", err)
	}
	cutoff := time.Now().Add(-staleAfter)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(root, e.Name())
		fi, err := os.Stat(path)
		if err != nil {
			continue
		}
		// Also check the inbox file's mtime if present, since some
		// filesystems don't bump directory mtime on file writes.
		latest := fi.ModTime()
		if ifi, err := os.Stat(filepath.Join(path, inboxFile)); err == nil {
			if ifi.ModTime().After(latest) {
				latest = ifi.ModTime()
			}
		}
		if latest.Before(cutoff) {
			_ = os.RemoveAll(path)
		}
	}
	return nil
}

func sessionKey() string {
	if v := os.Getenv("CLAUDE_SESSION_ID"); v != "" {
		return v
	}
	return fmt.Sprintf("ppid-%d", os.Getppid())
}

func defaultRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".agentmesh", "sessions"), nil
}
