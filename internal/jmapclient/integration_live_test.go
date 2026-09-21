package jmapclient

import (
	"context"
	"os"
	"testing"
)

// TestLiveSessionAndMailboxes is the live-server integration test (M0 gate).
// It reads creds from the environment per AGENTS.md and SKIPS when unset —
// never hardcode, never commit, never echo credentials.
func TestLiveSessionAndMailboxes(t *testing.T) {
	url := os.Getenv("JMAP_TUI_TEST_URL")
	user := os.Getenv("JMAP_TUI_TEST_USER")
	pass := os.Getenv("JMAP_TUI_TEST_PASSWORD")
	if url == "" || user == "" || pass == "" {
		t.Skip("live Stalwart creds not set (JMAP_TUI_TEST_URL / _USER / _PASSWORD)")
	}

	c := New(Options{
		ServerURL: url,
		Username:  user,
		Password:  pass,
	})
	ctx := context.Background()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	info, err := c.SessionInfo()
	if err != nil {
		t.Fatalf("SessionInfo: %v", err)
	}
	t.Logf("connected: %d account(s), primary mail account %q, %d capabilities",
		len(info.Accounts), info.PrimaryMailAccount, len(info.Capabilities))

	mbs, err := c.Mailboxes(ctx)
	if err != nil {
		t.Fatalf("Mailboxes: %v", err)
	}
	roles := 0
	for _, mb := range mbs {
		if mb.Role != "" {
			roles++
		}
	}
	t.Logf("mailboxes: %d total, %d with roles", len(mbs), roles)
}
