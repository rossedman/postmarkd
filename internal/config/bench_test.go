package config

import (
	"os"
	"path/filepath"
	"testing"
)

const benchFile = `# postmarkd configuration
export POSTMARKD_LISTEN=127.0.0.1:8025,unix:/run/postmarkd/http.sock
POSTMARKD_SMTP_LISTEN=":2525"
POSTMARKD_SMTP_HOSTNAME='mx.example.com'
POSTMARKD_SOCKET_MODE=0660
; tuning
POSTMARKD_MAX_CONCURRENCY=64
POSTMARKD_WORKERS=16
POSTMARKD_QUEUE_SIZE=5000
POSTMARKD_UPSTREAM_TIMEOUT=20s
POSTMARKD_SHUTDOWN_TIMEOUT=45s
POSTMARKD_LOG_LEVEL=warn
POSTMARKD_LOG_FORMAT=json
`

// BenchmarkParseFile measures reading and parsing a typical config file.
func BenchmarkParseFile(b *testing.B) {
	path := filepath.Join(b.TempDir(), "postmarkd.conf")
	writeBench(b, path, benchFile)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ParseFile(path); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkLoad measures full configuration resolution: defaults, file,
// environment, credentials and validation.
func BenchmarkLoad(b *testing.B) {
	dir := b.TempDir()
	path := filepath.Join(dir, "postmarkd.conf")
	writeBench(b, path, benchFile)
	writeBench(b, filepath.Join(dir, CredentialServerToken), "secret\n")
	writeBench(b, filepath.Join(dir, CredentialAuthTokens), "web-token # web\nworker-token\n")
	environ := []string{
		"HOME=/root",
		"PATH=/usr/bin:/bin",
		"CREDENTIALS_DIRECTORY=" + dir,
		"POSTMARKD_WORKERS=8",
		"POSTMARKD_MAX_RETRIES=2",
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Load(path, environ); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkParseAddrs measures splitting a listen address list.
func BenchmarkParseAddrs(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if len(ParseAddrs("127.0.0.1:8025, unix:/run/a.sock,/run/b.sock,[::1]:8025")) != 4 {
			b.Fatal("unexpected address count")
		}
	}
}

func writeBench(b *testing.B, path, content string) {
	b.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		b.Fatal(err)
	}
}
