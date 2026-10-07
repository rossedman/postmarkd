package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadPrecedence(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "postmarkd.conf", `
# comment
POSTMARKD_SERVER_TOKEN="from-file"
export POSTMARKD_LISTEN=unix:/run/postmarkd/postmarkd.sock
POSTMARKD_SOCKET_MODE=0600
POSTMARKD_UPSTREAM_TIMEOUT=5s
POSTMARKD_AUTH_TOKENS=a, b,c
`)
	cfg, err := Load(path, []string{"POSTMARKD_SERVER_TOKEN=from-env", "POSTMARKD_WORKERS=7", "UNRELATED=1"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServerToken != "from-env" {
		t.Errorf("env should override file, got %q", cfg.ServerToken)
	}
	if p, ok := cfg.UnixSocket(); !ok || p != "/run/postmarkd/postmarkd.sock" {
		t.Errorf("UnixSocket() = %q, %v", p, ok)
	}
	if cfg.SocketMode != 0o600 || cfg.UpstreamTimeout != 5*time.Second || cfg.Workers != 7 {
		t.Errorf("unexpected config: %+v", cfg)
	}
	if len(cfg.AuthTokens) != 3 || cfg.AuthTokens[2] != "c" {
		t.Errorf("AuthTokens = %q", cfg.AuthTokens)
	}
}

func TestUnknownKeyInFile(t *testing.T) {
	path := write(t, t.TempDir(), "c", "POSTMARKD_SERVER_TOKEN=x\nPOSTMARKD_WROKERS=3\n")
	if _, err := Load(path, nil); err == nil {
		t.Fatal("expected error for misspelled key")
	}
}

func TestCredentials(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, CredentialServerToken, "secret\n")
	write(t, dir, CredentialAuthTokens, "# clients\nweb-token # the web app\nworker-token\n")
	cfg, err := Load("", []string{"CREDENTIALS_DIRECTORY=" + dir})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServerToken != "secret" {
		t.Errorf("ServerToken = %q", cfg.ServerToken)
	}
	if len(cfg.AuthTokens) != 2 || cfg.AuthTokens[0] != "web-token" || cfg.AuthTokens[1] != "worker-token" {
		t.Errorf("AuthTokens = %q", cfg.AuthTokens)
	}
}

func TestValidation(t *testing.T) {
	if _, err := Load("", nil); err == nil {
		t.Error("missing server token should fail")
	}
	for _, env := range [][]string{
		{"POSTMARKD_MAX_CONCURRENCY=0"},
		{"POSTMARKD_UPSTREAM_TIMEOUT=soon"},
		{"POSTMARKD_LOG_FORMAT=xml"},
		{"POSTMARKD_SOCKET_MODE=999"},
		{"POSTMARKD_LOG_LEVEL=loud"},
	} {
		if _, err := Load("", append(env, "POSTMARKD_SERVER_TOKEN=x")); err == nil {
			t.Errorf("%v should fail validation", env)
		}
	}
}

func TestTCPListen(t *testing.T) {
	if _, ok := (Config{Listen: "127.0.0.1:8025"}).UnixSocket(); ok {
		t.Error("TCP address treated as unix socket")
	}
	if p, ok := (Config{Listen: "/tmp/x.sock"}).UnixSocket(); !ok || p != "/tmp/x.sock" {
		t.Error("absolute path should be a unix socket")
	}
}
