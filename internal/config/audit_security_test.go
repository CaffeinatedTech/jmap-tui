package config

// Audit regression tests for SECURITY_AUDIT_FINDINGS.md appendix A.7:
// W-5 (writeAtomic's temp file must not follow a planted symlink —
// finding F-11). The C-3/C-3b URL-policy probes land with F-5/F-6.

import (
	"os"
	"path/filepath"
	"testing"
)

// W-5: writeAtomic's temp file must not follow a planted symlink.
func TestAuditW5WriteAtomicSymlink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	victim := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(victim, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, path+".tmp"); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if err := writeAtomic(path, []byte("new config"), 0o600); err != nil {
		t.Fatalf("writeAtomic: %v", err)
	}
	data, _ := os.ReadFile(victim)
	if string(data) != "precious" {
		t.Errorf("FINDING W-5: writeAtomic followed the .tmp symlink; victim now holds %q", string(data))
	}
}
