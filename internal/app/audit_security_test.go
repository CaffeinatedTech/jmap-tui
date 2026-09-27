package app

// Audit probes for SECURITY_AUDIT_FINDINGS.md appendix A.3/A.4/A.13:
// W-1/W-2 (attachment path traversal and the uniquePath hang — finding
// F-4), T-7 (error truncation must never split a UTF-8 rune — finding
// F-13) and I-6 (parsed addresses must carry no CR/LF/NUL — finding
// F-16), plus the fuzz targets. FAIL = the finding is confirmed.

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// uniquePath must keep server-controlled attachment names inside the
// chosen directory.
func TestAuditW1UniquePathStaysInDir(t *testing.T) {
	dir := t.TempDir()
	hostile := []string{
		"../../.ssh/authorized_keys",
		"../evil.txt",
		"a/../../evil.txt",
		"..",
		".",
		"",
		"/etc/cron.d/pwn",
		"sub/../../out.txt",
		"normal.pdf",
		"..\\..\\windows.txt",
		"file\x00name.txt",
		strings.Repeat("x", 300) + ".txt",
		"\x1b]0;pwn\x07.pdf",
	}
	for _, name := range hostile {
		got, err := uniquePathGuarded(t, dir, name)
		if err != nil {
			continue // rejecting outright is a valid secure outcome
		}
		rel, rerr := filepath.Rel(dir, got)
		if rerr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Errorf("FINDING W-1: uniquePath(%q) escaped dir → %q", name, got)
			continue
		}
		if filepath.IsAbs(got) && !strings.HasPrefix(got, dir+string(filepath.Separator)) && got != dir {
			t.Errorf("FINDING W-1: uniquePath(%q) returned absolute path outside dir: %q", name, got)
		}
	}
}

// uniquePathGuarded runs uniquePath with a timeout: the current
// implementation can spin forever on names os.Stat rejects with EINVAL
// (NUL byte), which would otherwise hang the whole test binary.
func uniquePathGuarded(t *testing.T, dir, name string) (string, error) {
	t.Helper()
	type res struct {
		p   string
		err error
	}
	ch := make(chan res, 1)
	go func() {
		p, err := uniquePath(dir, name)
		ch <- res{p, err}
	}()
	select {
	case r := <-ch:
		return r.p, r.err
	case <-time.After(2 * time.Second):
		t.Errorf("FINDING W-2: uniquePath(%q) never returned (infinite Stat loop on non-NotExist error)", name)
		return "", errTimeout
	}
}

var errTimeout = errors.New("uniquePath timed out")

// FuzzUniquePath: hostile attachment names must terminate, never escape
// the directory, and never corrupt UTF-8 handling (findings W-1/W-2).
// The guard turns the infinite-Stat-loop hang (W-2) into a fuzz failure
// instead of a wedged fuzz run.
func FuzzUniquePath(f *testing.F) {
	f.Add("../../etc/passwd")
	f.Add("normal.pdf")
	f.Add("a\x00b")
	f.Add("..")
	f.Add(strings.Repeat("x", 5000))
	f.Add("/abs/path")
	f.Add("sub/../../out")
	f.Fuzz(func(t *testing.T, name string) {
		dir := t.TempDir()
		type res struct {
			p   string
			err error
		}
		ch := make(chan res, 1)
		go func() {
			p, err := uniquePath(dir, name)
			ch <- res{p, err}
		}()
		var got res
		select {
		case got = <-ch:
		case <-time.After(3 * time.Second):
			t.Fatalf("uniquePath(%q) hung (infinite Stat loop)", clipInput(name))
		}
		if got.err != nil {
			return
		}
		rel, err := filepath.Rel(dir, got.p)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Fatalf("uniquePath(%q) escaped dir → %q", clipInput(name), got.p)
		}
	})
}

func clipInput(s string) string {
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}

// A.13: NUL-byte hang isolation (F-4).
func TestAuditW2UniquePathNulHangs(t *testing.T) {
	dir := t.TempDir()
	done := make(chan string, 1)
	go func() {
		p, err := uniquePath(dir, "file\x00name.txt")
		done <- p + "|" + err.Error()
	}()
	select {
	case r := <-done:
		t.Logf("uniquePath returned: %s", r)
	case <-time.After(2 * time.Second):
		t.Errorf("FINDING W-2: uniquePath hangs forever on a NUL byte in the attachment name (os.Stat EINVAL never satisfies IsNotExist)")
	}
	_ = filepath.Join
}

// T-7: truncateErr must never split a UTF-8 rune (appendix A.3).
func TestAuditT7TruncateErrUTF8Safe(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"multibyte-long", errors.New(strings.Repeat("é", 200))},
		{"multibyte-boundary", errors.New(strings.Repeat("あ", 60))},
		{"emoji", errors.New(strings.Repeat("🙂", 60))},
	}
	for _, tc := range cases {
		got := truncateErr("op", tc.err)
		if !utf8.ValidString(got) {
			t.Errorf("FINDING T-7: truncateErr(%s) split a rune → invalid UTF-8", tc.name)
		}
	}
}

// I-6: parsed address fields must never carry CR/LF (header injection raw
// material) into the JMAP payload (appendix A.3).
func TestAuditI6AddressListStripsCRLF(t *testing.T) {
	cases := []struct{ name, in string }{
		{"lf-injection", "evil@x.test\nBcc: victim@x.test"},
		{"crlf-injection", "evil@x.test\r\nBcc: victim@x.test"},
		{"cr-only", "evil@x.test\rBcc: victim@x.test"},
		{"newline-in-name", "\"Evil\r\nName\" <evil@x.test>"},
		{"nul", "evil@x.test\x00more"},
	}
	for _, tc := range cases {
		for _, a := range parseAddressList(tc.in) {
			if strings.ContainsAny(a.Name, "\r\n\x00") {
				t.Errorf("FINDING I-6: %s: address Name carries control %q", tc.name, a.Name)
			}
			if strings.ContainsAny(a.Email, "\r\n\x00") {
				t.Errorf("FINDING I-6: %s: address Email carries control %q", tc.name, a.Email)
			}
		}
	}
}

// FuzzTruncateErr: truncation must preserve valid UTF-8 for arbitrary
// input (appendix A.4, finding F-13).
func FuzzTruncateErr(f *testing.F) {
	f.Add("plain error")
	f.Add(strings.Repeat("é", 200))
	f.Add(strings.Repeat("🙂", 100))
	f.Add("short")
	f.Fuzz(func(t *testing.T, msg string) {
		got := truncateErr("op", errors.New(msg))
		if !utf8.ValidString(got) {
			t.Fatalf("truncateErr split a rune for input %q → %q", clipInput(msg), clipInput(got))
		}
	})
}
