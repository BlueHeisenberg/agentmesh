// Package inbox is an in-memory queue of received messages with a monotonic
// cursor and a blocking Wait. Messages from never-before-seen peers are tagged
// FirstContact=true so the agent can decide how to handle them.
package inbox

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

type Message struct {
	ID           int64           `json:"id"`
	FromPeerID   string          `json:"from_peer_id"`
	FromName     string          `json:"from_name"`
	Topic        string          `json:"topic,omitempty"`
	Body         json.RawMessage `json:"body"`
	ReceivedAt   time.Time       `json:"received_at"`
	FirstContact bool            `json:"first_contact,omitempty"`
}

type Inbox struct {
	mu      sync.Mutex
	notify  chan struct{} // closed-and-replaced on each push
	cap     int
	cursor  int64
	msgs    []Message
	known   map[string]bool // peer_ids we've received from before
}

func New(capacity int) *Inbox {
	if capacity <= 0 {
		capacity = 1000
	}
	return &Inbox{
		cap:    capacity,
		known:  map[string]bool{},
		notify: make(chan struct{}),
	}
}

// Push appends a message and wakes any waiter.
func (ib *Inbox) Push(fromPeerID, fromName, topic string, body json.RawMessage) Message {
	ib.mu.Lock()
	ib.cursor++
	first := !ib.known[fromPeerID]
	ib.known[fromPeerID] = true
	m := Message{
		ID:           ib.cursor,
		FromPeerID:   fromPeerID,
		FromName:     fromName,
		Topic:        topic,
		Body:         body,
		ReceivedAt:   time.Now(),
		FirstContact: first,
	}
	ib.msgs = append(ib.msgs, m)
	if len(ib.msgs) > ib.cap {
		ib.msgs = ib.msgs[len(ib.msgs)-ib.cap:]
	}
	old := ib.notify
	ib.notify = make(chan struct{})
	ib.mu.Unlock()
	close(old)
	return m
}

// Since returns all messages with id > since, plus the new cursor.
func (ib *Inbox) Since(since int64) ([]Message, int64) {
	ib.mu.Lock()
	defer ib.mu.Unlock()
	return ib.sinceLocked(since)
}

func (ib *Inbox) sinceLocked(since int64) ([]Message, int64) {
	out := make([]Message, 0)
	for _, m := range ib.msgs {
		if m.ID > since {
			out = append(out, m)
		}
	}
	return out, ib.cursor
}

// Wait blocks until at least one message with id > since arrives, ctx is done,
// or `timeout` elapses. Returns whatever is available at unblock time.
func (ib *Inbox) Wait(ctx context.Context, since int64, timeout time.Duration) ([]Message, int64) {
	for {
		ib.mu.Lock()
		if ib.cursor > since {
			out, cur := ib.sinceLocked(since)
			ib.mu.Unlock()
			return out, cur
		}
		ch := ib.notify
		ib.mu.Unlock()

		select {
		case <-ch:
			// loop and re-check
		case <-ctx.Done():
			return ib.Since(since)
		case <-time.After(timeout):
			return ib.Since(since)
		}
	}
}
