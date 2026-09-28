package config

// Audit regression tests: C-3 (cleartext non-loopback URLs must be
// rejected), C-3b (URL userinfo must be rejected) and W-5 (writeAtomic's
// temp file must not follow a planted symlink). The URL table also pins
// the policy the --url/smoke flag paths share through ValidateServerURL.
// FAIL = regression confirmed.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Non-loopback cleartext must be rejected; loopback stays allowed
// (mockjmap and local Stalwart experiments).
func TestAuditC3RejectsNonLoopbackHTTP(t *testing.T) {
	cases := []struct {
		name, url string
		wantErr   bool
	}{
		{"https-ok", "https://mail.example.com", false},
		{"http-nonloopback", "http://mail.example.com", true},
		{"http-loopback-127", "http://127.0.0.1:8080", false},
		{"http-localhost", "http://localhost:8080", false},
		{"http-loopback-private", "http://192.168.1.10:8080", true},
		{"http-public-ip", "http://203.0.113.7", true},
	}
	for _, tc := range cases {
		body := "[accounts.main]\nurl = \"" + tc.url + "\"\nusername = \"u\"\n"
		_, err := Load(write(t, body))
		if tc.wantErr && err == nil {
			t.Errorf("[%s] cleartext non-loopback URL accepted; want rejection", tc.name)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("[%s] unexpected error: %v", tc.name, err)
		}
	}
}

// C-3b: config URLs with userinfo must be rejected outright (they leak to
// logs per C-4 and duplicate Basic auth).
func TestAuditC3bRejectsURLUserinfo(t *testing.T) {
	body := "[accounts.main]\nurl = \"https://user:pass@mail.example.com\"\nusername = \"u\"\n"
	if _, err := Load(write(t, body)); err == nil {
		t.Errorf("FINDING C-3b: URL userinfo (user:pass@) accepted in config; must be rejected (leaks to logs, C-4)")
	}
}

// The same policy, called directly: this is what the --url and smoke
// flag paths and the wizard run, so every branch must hold there too.
func TestValidateServerURL(t *testing.T) {
	cases := []struct {
		name, url string
		wantErr   bool
	}{
		{"https", "https://mail.example.com", false},
		{"https-port", "https://mail.example.com:8443", false},
		{"http-loopback-v4", "http://127.0.0.1:8080", false},
		{"http-localhost", "http://LOCALHOST:8080", false},
		{"http-loopback-v6", "http://[::1]:8080", false},
		{"http-remote-host", "http://mail.example.com", true},
		{"http-remote-ip", "http://192.168.1.10:8080", true},
		{"userinfo-password", "https://user:pass@mail.example.com", true},
		{"userinfo-user", "https://user@mail.example.com", true},
		{"ftp-scheme", "ftp://mail.example.com", true},
		{"schemeless", "mail.example.com", true},
		{"empty", "", true},
	}
	for _, tc := range cases {
		err := ValidateServerURL(tc.url)
		if tc.wantErr && err == nil {
			t.Errorf("[%s] accepted; want rejection", tc.name)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("[%s] unexpected error: %v", tc.name, err)
		}
		// URLs carrying userinfo must never be echoed into an error that
		// reaches the terminal (the message may show placeholder guidance
		// like user:pass@host, but never the raw value).
		if err != nil && strings.Contains(tc.url, "@") && strings.Contains(err.Error(), tc.url) {
			t.Errorf("[%s] error echoes the raw URL (userinfo would leak to the terminal): %v", tc.name, err)
		}
	}
}

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
