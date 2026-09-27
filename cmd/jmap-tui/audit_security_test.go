package main

// Audit probes for SECURITY_AUDIT_FINDINGS.md appendix A.10: T-5 (smoke's
// stdout dump must be escape-free even when the server is hostile —
// finding F-3), W-4 (the crash report must not follow a planted symlink
// in a shared tmp dir — finding F-10) and W-7 (a pre-existing
// world-writable log file must be tightened to 0600 — finding F-14).
// FAIL = finding confirmed. T-6's stderr probe is inspection-only —
// main() strips at the print site.

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

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

// W-4: crash report must not follow a planted symlink in a shared tmp
// dir (appendix A.10). writeCrashReport now uses a random O_EXCL name,
// so the planted predictable names can never be opened.
func TestAuditW4CrashReportDoesNotFollowSymlink(t *testing.T) {
	tmp := t.TempDir() // stands in for $TMPDIR
	victim := filepath.Join(t.TempDir(), "victim.txt")
	if err := os.WriteFile(victim, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The old implementation named the file by unix seconds; plant
	// symlinks for this second and the next so the race cannot dodge a
	// name check.
	now := time.Now().Unix()
	var planted string
	for _, sec := range []int64{now, now + 1} {
		p := filepath.Join(tmp, "jmap-tui-crash-"+strconv.FormatInt(sec, 10)+".log")
		if err := os.Symlink(victim, p); err == nil {
			planted = p
		}
	}
	if planted == "" {
		t.Skip("could not plant symlink")
	}

	orig := os.Getenv("TMPDIR")
	_ = os.Setenv("TMPDIR", tmp)
	defer func() { _ = os.Setenv("TMPDIR", orig) }()

	if _, err := writeCrashReport("boom", []byte("stack")); err != nil {
		t.Logf("writeCrashReport error: %v", err)
	}
	data, _ := os.ReadFile(victim)
	if string(data) != "precious" {
		t.Errorf("FINDING W-4: crash report followed the symlink; victim now holds %q", snippet(string(data)))
	}
}

// W-7: setupLogger must not leave a pre-existing world-writable log file
// (appendix A.10).
func TestAuditW7LogFileTightenedTo0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "debug.log")
	if err := os.WriteFile(path, []byte("old\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	_, stop, err := setupLogger(path, "debug")
	if err != nil {
		t.Fatalf("setupLogger: %v", err)
	}
	if stop != nil {
		stop()
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("FINDING W-7: debug log keeps pre-existing mode %o; want 0600", fi.Mode().Perm())
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
