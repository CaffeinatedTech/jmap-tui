package keyring

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

func TestEnvVarName(t *testing.T) {
	cases := map[string]string{
		"personal":     "JMAP_TUI_PASSWORD_PERSONAL",
		"work-mail":    "JMAP_TUI_PASSWORD_WORK_MAIL",
		"agent.test/1": "JMAP_TUI_PASSWORD_AGENT_TEST_1",
	}
	for in, want := range cases {
		if got := EnvVar(in); got != want {
			t.Errorf("EnvVar(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPasswordEnvWins(t *testing.T) {
	t.Setenv("JMAP_TUI_PASSWORD_ACCT", "from-env")
	secret, warnings, err := Password("acct", "", func(string, string) (string, error) {
		t.Fatal("keyring must not be reached when env var is set")
		return "", nil
	})
	if err != nil || secret != "from-env" {
		t.Fatalf("Password = %q, %v; want from-env, nil", secret, err)
	}
	if len(warnings) == 0 {
		t.Error("expected a warning naming the env var")
	}
}

func TestPasswordFileFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(path, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	secret, warnings, err := Password("acct", path, func(string, string) (string, error) {
		t.Fatal("keyring must not be reached when password_file resolves")
		return "", nil
	})
	if err != nil || secret != "from-file" {
		t.Fatalf("Password = %q, %v; want from-file, nil", secret, err)
	}
	if len(warnings) == 0 || !strings.Contains(warnings[0], "plaintext") {
		t.Errorf("warnings = %v, want plaintext-on-disk warning", warnings)
	}
}

func TestPasswordFilePermRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := Password("acct", path, func(string, string) (string, error) {
		t.Fatal("keyring must not be reached when the file is rejected")
		return "", nil
	})
	if err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("err = %v, want chmod-600 error", err)
	}
}

func TestPasswordKeyringFallback(t *testing.T) {
	called := false
	secret, warnings, err := Password("acct", "", func(service, user string) (string, error) {
		called = true
		if service != Service || user != "acct" {
			t.Errorf("backend(service=%q, user=%q)", service, user)
		}
		return "from-keyring", nil
	})
	if err != nil || secret != "from-keyring" {
		t.Fatalf("Password = %q, %v", secret, err)
	}
	if !called || len(warnings) != 0 {
		t.Errorf("called=%v warnings=%v", called, warnings)
	}
}

func TestPasswordKeyringNotFoundIsActionable(t *testing.T) {
	_, _, err := Password("acct", "", func(string, string) (string, error) {
		return "", keyring.ErrNotFound
	})
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{"no secret for account", "JMAP_TUI_PASSWORD_ACCT", "password_file"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestPasswordKeyringErrorWrapped(t *testing.T) {
	sentinel := errors.New("dbus exploded")
	_, _, err := Password("acct", "", func(string, string) (string, error) {
		return "", sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want wrapped sentinel", err)
	}
}
