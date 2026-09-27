package sync

// Audit probes for SECURITY_AUDIT_FINDINGS.md finding F-13 on the sync
// side: truncateStatusErr must never split a UTF-8 rune and must strip
// controls before a server error string reaches the status line.
// (Appendix A.3's TestAuditT7StatusTruncationPattern was log-only — it
// replicated the old byte-slice locally instead of calling the function;
// this is the real assertion it stood in for.)

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestAuditT7StatusTruncationUTF8Safe(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"multibyte-long", errors.New(strings.Repeat("é", 200))},
		{"multibyte-boundary", errors.New(strings.Repeat("あ", 60))},
		{"emoji", errors.New(strings.Repeat("🙂", 60))},
		{"short", errors.New("plain")},
	}
	for _, tc := range cases {
		got := truncateStatusErr(tc.err)
		if !utf8.ValidString(got) {
			t.Errorf("FINDING T-7: truncateStatusErr(%s) split a rune → invalid UTF-8", tc.name)
		}
		if len(got) > 80 {
			t.Errorf("truncateStatusErr(%s) = %d bytes, want ≤ 80", tc.name, len(got))
		}
	}
}

func TestAuditT7StatusTruncationStripsControls(t *testing.T) {
	got := truncateStatusErr(errors.New("sync \x1b[2J failed \x07 ding"))
	if strings.ContainsAny(got, "\x1b\x07") {
		t.Errorf("control bytes survived truncateStatusErr: %q", got)
	}
}
