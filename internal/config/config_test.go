package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadValid(t *testing.T) {
	path := write(t, `
default_account = "personal"

[accounts.personal]
display_name = "Personal"
url = "https://mail.example.com"
username = "me@example.com"

[accounts.work]
url = "https://work.example.com"
username = "me@work.example.com"
password_file = "/run/secrets/work"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DefaultAccount != "personal" {
		t.Errorf("DefaultAccount = %q, want %q", cfg.DefaultAccount, "personal")
	}
	a, ok := cfg.Account("personal")
	if !ok {
		t.Fatal("account personal missing")
	}
	if a.URL != "https://mail.example.com" || a.Username != "me@example.com" {
		t.Errorf("personal = %+v", a)
	}
	// password_keyring defaults to true when unset.
	if a.PasswordKeyring != nil && !*a.PasswordKeyring {
		t.Errorf("PasswordKeyring should default to true (nil), got %v", *a.PasswordKeyring)
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.toml"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("err = %v, want os.ErrNotExist", err)
	}
}

func TestLoadRejectsPlaintextPassword(t *testing.T) {
	path := write(t, `
[accounts.personal]
url = "https://mail.example.com"
username = "me@example.com"
password = "hunter2"
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("want error for plaintext password")
	}
	for _, want := range []string{"plaintext", "keyring", "JMAP_TUI_PASSWORD_"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestLoadRejectsUnknownKey(t *testing.T) {
	path := write(t, `
[accounts.personal]
url = "https://mail.example.com"
username = "me@example.com"
usrname = "typo"
`)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "usrname") {
		t.Fatalf("err = %v, want unknown-key error mentioning usrname", err)
	}
}

func TestLoadRequiresURLAndUsername(t *testing.T) {
	path := write(t, `
[accounts.personal]
display_name = "no url"
`)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "url is required") {
		t.Fatalf("err = %v, want url-required error", err)
	}

	path = write(t, `
[accounts.personal]
url = "https://mail.example.com"
`)
	_, err = Load(path)
	if err == nil || !strings.Contains(err.Error(), "username is required") {
		t.Fatalf("err = %v, want username-required error", err)
	}
}

func TestLoadRequiresAccounts(t *testing.T) {
	path := write(t, "")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "no accounts") {
		t.Fatalf("err = %v, want no-accounts error", err)
	}
}

func TestLoadKeyringFalseNeedsFile(t *testing.T) {
	path := write(t, `
[accounts.personal]
url = "https://mail.example.com"
username = "me@example.com"
password_keyring = false
`)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "no password source") {
		t.Fatalf("err = %v, want no-password-source error", err)
	}
}

func TestPrimaryAccount(t *testing.T) {
	path := write(t, `
[accounts.a]
url = "https://a.example.com"
username = "a"
[accounts.b]
url = "https://b.example.com"
username = "b"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	// Two accounts and no default: error naming the choices.
	_, _, err = cfg.PrimaryAccount()
	if err == nil || !strings.Contains(err.Error(), "default_account") {
		t.Fatalf("err = %v, want default_account error", err)
	}

	// Explicit default that exists.
	cfg.DefaultAccount = "b"
	id, _, err := cfg.PrimaryAccount()
	if err != nil || id != "b" {
		t.Fatalf("PrimaryAccount = %q, %v; want b, nil", id, err)
	}

	// Explicit default that does not exist.
	cfg.DefaultAccount = "zzz"
	if _, _, err = cfg.PrimaryAccount(); err == nil {
		t.Fatal("want error for unknown default_account")
	}

	// A single account needs no default.
	path = write(t, `
[accounts.solo]
url = "https://s.example.com"
username = "s"
`)
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	id, _, err = cfg.PrimaryAccount()
	if err != nil || id != "solo" {
		t.Fatalf("PrimaryAccount = %q, %v; want solo, nil", id, err)
	}
}
