package main

import (
	"strings"
	"testing"

	"github.com/BlueHeisenberg/agentmesh/internal/sessionstore"
)

func TestFormatHookPingSingle(t *testing.T) {
	got := formatHookPing([]sessionstore.Message{
		{FromName: "alice#53d6"},
	})
	want := "[mesh] 1 unread peer message from alice#53d6 - the user has not seen them; call mesh_inbox to read, then surface them to the user."
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestFormatHookPingDedupAndFirstContact(t *testing.T) {
	got := formatHookPing([]sessionstore.Message{
		{FromName: "alice#53d6"},
		{FromName: "alice#53d6"},
		{FromName: "tothemoon#4ca8", FirstContact: true},
	})
	if !strings.HasPrefix(got, "[mesh] 3 unread peer messages from alice#53d6, tothemoon#4ca8; includes a first contact") {
		t.Errorf("unexpected ping: %q", got)
	}
	if strings.Count(got, "alice#53d6") != 1 {
		t.Errorf("sender not deduped: %q", got)
	}
}

func TestFormatHookPingCapsSenderList(t *testing.T) {
	got := formatHookPing([]sessionstore.Message{
		{FromName: "a#1"}, {FromName: "b#2"}, {FromName: "c#3"}, {FromName: "d#4"}, {FromName: "e#5"},
	})
	if !strings.Contains(got, "a#1, b#2, c#3 +2 more") {
		t.Errorf("sender cap missing: %q", got)
	}
	if strings.Contains(got, "d#4") {
		t.Errorf("more than 3 senders listed: %q", got)
	}
}
