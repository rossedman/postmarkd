package config

import (
	"bufio"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
	if a := ParseAddrs(cfg.Listen); len(a) != 1 || a[0] != (Addr{"unix", "/run/postmarkd/postmarkd.sock"}) {
		t.Errorf("ParseAddrs = %v", a)
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

func TestParseAddrs(t *testing.T) {
	got := ParseAddrs("127.0.0.1:8025, unix:/run/a.sock,/run/b.sock,")
	want := []Addr{{"tcp", "127.0.0.1:8025"}, {"unix", "/run/a.sock"}, {"unix", "/run/b.sock"}}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("addr %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestBlackholeNeedsNoToken(t *testing.T) {
	if _, err := Load("", []string{"POSTMARKD_BLACKHOLE=true"}); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultIsValidWithToken(t *testing.T) {
	t.Parallel()
	c := Default()
	c.ServerToken = "x"
	if err := c.Validate(); err != nil {
		t.Fatalf("defaults with a token should validate: %v", err)
	}
}

func TestValidateRules(t *testing.T) {
	t.Parallel()
	valid := func() Config {
		c := Default()
		c.ServerToken = "x"
		return c
	}
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string // empty means valid
	}{
		{"server token required", func(c *Config) { c.ServerToken = "" }, "server token is required"},
		{"blackhole needs no token", func(c *Config) { c.ServerToken, c.Blackhole = "", true }, ""},
		{"no listeners", func(c *Config) { c.Listen, c.SMTPListen = " , ", "" }, "LISTEN or POSTMARKD_SMTP_LISTEN must be set"},
		{"smtp only", func(c *Config) { c.Listen, c.SMTPListen = "", ":2525" }, ""},
		{"negative blackhole keep", func(c *Config) { c.BlackholeKeep = -1 }, "BLACKHOLE_KEEP must be >= 0"},
		{"zero blackhole keep", func(c *Config) { c.BlackholeKeep = 0 }, ""},
		{"api url scheme", func(c *Config) { c.APIURL = "ftp://x" }, "API_URL must be an http(s) URL"},
		{"plain http api url", func(c *Config) { c.APIURL = "http://localhost:1" }, ""},
		{"max concurrency zero", func(c *Config) { c.MaxConcurrency = 0 }, "MAX_CONCURRENCY must be >= 1"},
		{"max concurrency one", func(c *Config) { c.MaxConcurrency = 1 }, ""},
		{"negative queue", func(c *Config) { c.QueueSize = -1 }, "QUEUE_SIZE must be >= 0"},
		{"queue without workers", func(c *Config) { c.Workers = 0 }, "WORKERS must be >= 1"},
		{"no queue no workers", func(c *Config) { c.QueueSize, c.Workers = 0, 0 }, ""},
		{"negative retries", func(c *Config) { c.MaxRetries = -1 }, "MAX_RETRIES must be >= 0"},
		{"zero retries", func(c *Config) { c.MaxRetries = 0 }, ""},
		{"zero upstream timeout", func(c *Config) { c.UpstreamTimeout = 0 }, "timeouts must be positive"},
		{"negative shutdown timeout", func(c *Config) { c.ShutdownTimeout = -time.Second }, "timeouts must be positive"},
		{"body too small", func(c *Config) { c.MaxBodyBytes = 1023 }, "MAX_BODY_BYTES must be >= 1024"},
		{"body minimum", func(c *Config) { c.MaxBodyBytes = 1024 }, ""},
		{"log format", func(c *Config) { c.LogFormat = "xml" }, "LOG_FORMAT must be text or json"},
		{"json log format", func(c *Config) { c.LogFormat = "json" }, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := valid()
			tt.mutate(&c)
			err := c.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateJoinsAllErrors(t *testing.T) {
	t.Parallel()
	c := Config{}
	err := c.Validate()
	if err == nil {
		t.Fatal("zero Config should not validate")
	}
	for _, want := range []string{"server token", "LISTEN", "API_URL", "MAX_CONCURRENCY", "timeouts", "MAX_BODY_BYTES", "LOG_FORMAT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

func TestApplyValues(t *testing.T) {
	t.Parallel()
	cfg, err := Load("", []string{
		"POSTMARKD_SERVER_TOKEN=tok",
		"POSTMARKD_LISTEN=:1,unix:/a",
		"POSTMARKD_SMTP_LISTEN=:2525",
		"POSTMARKD_SMTP_HOSTNAME=mx.example.com",
		"POSTMARKD_SOCKET_MODE=0640",
		"POSTMARKD_BLACKHOLE=false",
		"POSTMARKD_BLACKHOLE_KEEP=5",
		"POSTMARKD_API_URL=http://127.0.0.1:9",
		"POSTMARKD_DEFAULT_FROM=a@example.com",
		"POSTMARKD_DEFAULT_MESSAGE_STREAM=broadcast",
		"POSTMARKD_AUTH_TOKENS=x y\tz",
		"POSTMARKD_MAX_CONCURRENCY=9",
		"POSTMARKD_WORKERS=3",
		"POSTMARKD_QUEUE_SIZE=0",
		"POSTMARKD_MAX_RETRIES=0",
		"POSTMARKD_UPSTREAM_TIMEOUT=2s",
		"POSTMARKD_MAX_BODY_BYTES=2048",
		"POSTMARKD_SHUTDOWN_TIMEOUT=1m",
		"POSTMARKD_LOG_LEVEL=debug",
		"POSTMARKD_LOG_FORMAT=json",
		"POSTMARKD_NOT_A_KEY=ignored",
		"NOEQUALS",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Listen:               ":1,unix:/a",
		SMTPListen:           ":2525",
		SMTPHostname:         "mx.example.com",
		SocketMode:           0o640,
		BlackholeKeep:        5,
		ServerToken:          "tok",
		APIURL:               "http://127.0.0.1:9",
		DefaultFrom:          "a@example.com",
		DefaultMessageStream: "broadcast",
		AuthTokens:           []string{"x", "y", "z"},
		MaxConcurrency:       9,
		Workers:              3,
		QueueSize:            0,
		MaxRetries:           0,
		UpstreamTimeout:      2 * time.Second,
		MaxBodyBytes:         2048,
		ShutdownTimeout:      time.Minute,
		LogLevel:             slog.LevelDebug,
		LogFormat:            "json",
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("got  %+v\nwant %+v", cfg, want)
	}
}

func TestApplyErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		env     string
		wantErr string
	}{
		{"POSTMARKD_WORKERS=many", `POSTMARKD_WORKERS: "many" is not an integer`},
		{"POSTMARKD_BLACKHOLE_KEEP=1.5", `POSTMARKD_BLACKHOLE_KEEP: "1.5" is not an integer`},
		{"POSTMARKD_BLACKHOLE=maybe", `POSTMARKD_BLACKHOLE: "maybe" must be true or false`},
		{"POSTMARKD_SOCKET_MODE=0x1ff", `POSTMARKD_SOCKET_MODE: "0x1ff" is not an octal file mode`},
		{"POSTMARKD_SOCKET_MODE=1000", `POSTMARKD_SOCKET_MODE: "1000" is not an octal file mode`},
		{"POSTMARKD_MAX_BODY_BYTES=big", `POSTMARKD_MAX_BODY_BYTES: "big" is not an integer`},
		{"POSTMARKD_SHUTDOWN_TIMEOUT=10", `POSTMARKD_SHUTDOWN_TIMEOUT: "10" is not a duration`},
		{"POSTMARKD_LOG_LEVEL=loud", `POSTMARKD_LOG_LEVEL: "loud" must be debug, info, warn or error`},
	}
	for _, tt := range tests {
		t.Run(tt.env, func(t *testing.T) {
			t.Parallel()
			_, err := Load("", []string{"POSTMARKD_SERVER_TOKEN=x", tt.env})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestApplyKeepsDefaultsOnError(t *testing.T) {
	t.Parallel()
	cfg, err := Load("", []string{"POSTMARKD_SERVER_TOKEN=x", "POSTMARKD_SOCKET_MODE=999", "POSTMARKD_MAX_BODY_BYTES=x", "POSTMARKD_WORKERS=x", "POSTMARKD_UPSTREAM_TIMEOUT=x"})
	if err == nil {
		t.Fatal("expected error")
	}
	d := Default()
	if cfg.SocketMode != d.SocketMode || cfg.MaxBodyBytes != d.MaxBodyBytes || cfg.Workers != d.Workers || cfg.UpstreamTimeout != d.UpstreamTimeout {
		t.Fatalf("invalid values should leave defaults untouched: %+v", cfg)
	}
}

func TestLoadFileErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := Load(filepath.Join(dir, "missing.conf"), nil); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing file: err = %v", err)
	}
	bad := write(t, dir, "bad.conf", "POSTMARKD_SERVER_TOKEN=x\njust words\n")
	if _, err := Load(bad, nil); err == nil || !strings.Contains(err.Error(), "bad.conf:2: expected KEY=VALUE") {
		t.Errorf("malformed file: err = %v", err)
	}
	unknown := write(t, dir, "unknown.conf", "POSTMARKD_WROKERS=3\n")
	if _, err := Load(unknown, nil); err == nil || !strings.Contains(err.Error(), `unknown key "POSTMARKD_WROKERS"`) {
		t.Errorf("unknown key: err = %v", err)
	}
}

func TestParseFile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
		want    map[string]string
		wantErr string
	}{
		{"empty", "", map[string]string{}, ""},
		{"comments and blanks", "# a\n; b\n\n   \n  # indented\n", map[string]string{}, ""},
		{"plain", "A=1\nB = two words \n", map[string]string{"A": "1", "B": "two words"}, ""},
		{"export", "export A=1\n  export B=2", map[string]string{"A": "1", "B": "2"}, ""},
		{"double quotes", `A="  spaced  "`, map[string]string{"A": "  spaced  "}, ""},
		{"single quotes", `A='x # y'`, map[string]string{"A": "x # y"}, ""},
		{"mismatched quotes kept", `A="x'`, map[string]string{"A": `"x'`}, ""},
		{"lone quote kept", `A="`, map[string]string{"A": `"`}, ""},
		{"empty quotes", `A=""`, map[string]string{"A": ""}, ""},
		{"value with equals", "A=b=c", map[string]string{"A": "b=c"}, ""},
		{"crlf", "A=1\r\nB=2\r\n", map[string]string{"A": "1", "B": "2"}, ""},
		{"last wins", "A=1\nA=2", map[string]string{"A": "2"}, ""},
		{"missing equals", "A=1\nB\n", nil, ":2: expected KEY=VALUE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := write(t, t.TempDir(), "env", tt.content)
			got, err := ParseFile(path)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				if got != nil {
					t.Errorf("got %v with error", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseFileLineTooLong(t *testing.T) {
	t.Parallel()
	path := write(t, t.TempDir(), "env", "A="+strings.Repeat("x", bufio.MaxScanTokenSize)+"\n")
	if _, err := ParseFile(path); !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("err = %v, want bufio.ErrTooLong", err)
	}
}

func TestParseAddrsTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want []Addr
	}{
		{"", nil},
		{" , ,", nil},
		{":8025", []Addr{{"tcp", ":8025"}}},
		{"[::1]:25", []Addr{{"tcp", "[::1]:25"}}},
		{"unix:", []Addr{{"unix", ""}}},
		{"unix:relative.sock", []Addr{{"unix", "relative.sock"}}},
		{" /run/x.sock , localhost:1 ", []Addr{{"unix", "/run/x.sock"}, {"tcp", "localhost:1"}}},
	}
	for _, tt := range tests {
		if got := ParseAddrs(tt.in); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("ParseAddrs(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestCredentialPrecedence(t *testing.T) {
	t.Parallel()
	creds := t.TempDir()
	write(t, creds, CredentialServerToken, "from-cred\n")
	write(t, creds, CredentialAuthTokens, "cred-token\n")
	files := t.TempDir()
	tokFile := write(t, files, "token", "  from-file \n")
	authFile := write(t, files, "auth", "f1,f2\n")

	tests := []struct {
		name      string
		env       []string
		wantToken string
		wantAuth  []string
	}{
		{"credentials", []string{"CREDENTIALS_DIRECTORY=" + creds}, "from-cred", []string{"cred-token"}},
		{"files beat credentials", []string{
			"CREDENTIALS_DIRECTORY=" + creds,
			"POSTMARKD_SERVER_TOKEN_FILE=" + tokFile,
			"POSTMARKD_AUTH_TOKENS_FILE=" + authFile,
		}, "from-file", []string{"f1", "f2"}},
		{"values beat files", []string{
			"CREDENTIALS_DIRECTORY=" + creds,
			"POSTMARKD_SERVER_TOKEN_FILE=" + tokFile,
			"POSTMARKD_AUTH_TOKENS_FILE=" + authFile,
			"POSTMARKD_SERVER_TOKEN=explicit",
			"POSTMARKD_AUTH_TOKENS=e1",
		}, "explicit", []string{"e1"}},
		{"empty credentials dir", []string{"CREDENTIALS_DIRECTORY=" + t.TempDir(), "POSTMARKD_BLACKHOLE=1"}, "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := Load("", tt.env)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.ServerToken != tt.wantToken || !reflect.DeepEqual(cfg.AuthTokens, tt.wantAuth) {
				t.Fatalf("token=%q auth=%q, want %q %q", cfg.ServerToken, cfg.AuthTokens, tt.wantToken, tt.wantAuth)
			}
		})
	}
}

func TestCredentialFileErrors(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "nope")
	tests := []struct {
		env     []string
		wantErr string
	}{
		{[]string{"POSTMARKD_SERVER_TOKEN_FILE=" + missing}, "reading server token"},
		{[]string{"POSTMARKD_SERVER_TOKEN=x", "POSTMARKD_AUTH_TOKENS_FILE=" + missing}, "reading auth tokens"},
	}
	for _, tt := range tests {
		_, err := Load("", tt.env)
		if err == nil || !strings.Contains(err.Error(), tt.wantErr) || !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%v: err = %v, want %q wrapping fs.ErrNotExist", tt.env, err, tt.wantErr)
		}
	}
}

func TestSplitTokens(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"# only a comment", nil},
		{"a,b c\td\r\ne", []string{"a", "b", "c", "d", "e"}},
		{"a#b\nc # d\n,,", []string{"a", "c"}},
	}
	for _, tt := range tests {
		if got := splitTokens(tt.in); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("splitTokens(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestLookup(t *testing.T) {
	t.Parallel()
	env := []string{"A", "B=1", "CREDENTIALS_DIRECTORY=/x=y", "CREDENTIALS_DIRECTORY=/second"}
	if got := lookup(env, "CREDENTIALS_DIRECTORY"); got != "/x=y" {
		t.Errorf("lookup = %q, want first match", got)
	}
	if got := lookup(env, "A"); got != "" {
		t.Errorf("lookup of key without = is %q", got)
	}
}
