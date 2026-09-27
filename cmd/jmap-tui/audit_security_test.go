package main

// Audit probe for SECURITY_AUDIT_PLAN.md §4.4 T-5: smoke's stdout dump
// must be escape-free even when the server is hostile (username, account
// names, API URLs are all server-controlled). FAIL = finding F-3
// confirmed at that sink. Copied verbatim from SECURITY_AUDIT_FINDINGS.md
// appendix A.10 (the W-4/W-7 probes belong to F-10/F-14; T-6's stderr
// probe is inspection-only — main() now strips at the print site).

import (
	"os"
	"testing"

	"github.com/CaffeinatedTech/jmap-tui/internal/jmapclient"
	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
)

// T-5: smoke's session dump must be escape-free even when the server is
// hostile (username, account names, API URLs are server-controlled).
func TestAuditT5SmokeStdoutEscapeFree(t *testing.T) {
	// Capture stdout around printSession/printMailboxTree.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	func() {
		defer func() { os.Stdout = orig }()
		printSession(hostileSessionInfo())
		printMailboxTree(hostileMailboxes())
	}()
	_ = w.Close()
	var buf [1 << 20]byte
	n, _ := r.Read(buf[:])
	os.Stdout = orig
	out := string(buf[:n])

	if containsControl(out) {
		t.Errorf("FINDING T-5: smoke stdout carries control bytes from server data: %q", snippet(out))
	}
}

func containsControl(s string) bool {
	for _, r := range s {
		if r == '\n' || r == '\t' {
			continue
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return true
		}
		if r == 0x1b {
			return true
		}
	}
	return false
}

func snippet(s string) string {
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}

// hostileSessionInfo / hostileMailboxes build server-controlled inputs the
// way a compromised JMAP server would supply them.
func hostileSessionInfo() jmapclient.SessionInfo {
	return jmapclient.SessionInfo{
		Username:           "\x1b]0;owned\x07user",
		Accounts:           []jmapclient.AccountInfo{{ID: "acc1", Name: "\x1b[2Jadmin", IsPersonal: true}},
		PrimaryMailAccount: "acc1",
		Capabilities:       []string{"urn:ietf:params:jmap:mail"},
		APIURL:             "https://evil.test/api\x1b]52;c;cmVxdWVzdGVk\x07",
		EventSourceURL:     "https://evil.test/es",
		State:              "s\x1b[1;1H",
	}
}

func hostileMailboxes() []mail.Mailbox {
	return []mail.Mailbox{
		{ID: "mb1", Name: "\x1b]8;;https://evil.test\x07Inbox\x1b]8;;\x07", UnreadEmails: 1},
		{ID: "mb2", Name: "trusted\rEVIL", TotalEmails: 2},
	}
}
