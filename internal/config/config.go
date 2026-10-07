// Package config loads postmarkd configuration from defaults, an optional
// KEY=VALUE config file, and the process environment (in that order of
// precedence, lowest first).
package config

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Prefix is the prefix shared by every configuration key.
const Prefix = "POSTMARKD_"

// Names of files read from $CREDENTIALS_DIRECTORY (systemd LoadCredential=).
const (
	CredentialServerToken = "postmark-server-token"
	CredentialAuthTokens  = "auth-tokens"
)

// Config is the complete runtime configuration of the daemon.
type Config struct {
	// Listen is a comma separated list of HTTP API addresses: TCP
	// ("127.0.0.1:8025", ":8025") or unix sockets ("unix:/path" or any
	// absolute path). Ignored when systemd passes HTTP sockets.
	Listen string
	// SMTPListen is the same for the SMTP server. Empty disables SMTP.
	SMTPListen string
	// SMTPHostname is announced in the SMTP greeting.
	SMTPHostname string
	// SocketMode is the permission applied to unix sockets we create.
	SocketMode fs.FileMode

	// Blackhole accepts mail without sending it to Postmark, for testing.
	Blackhole bool
	// BlackholeKeep is how many blackholed messages to keep for inspection.
	BlackholeKeep int

	// ServerToken is the Postmark server API token.
	ServerToken string
	// APIURL is the Postmark API base URL.
	APIURL string
	// DefaultFrom is used when a message has no From address.
	DefaultFrom string
	// DefaultMessageStream is used when a message has no MessageStream.
	DefaultMessageStream string

	// AuthTokens, when non-empty, are the tokens clients must present.
	AuthTokens []string

	// MaxConcurrency caps simultaneous in-flight requests to Postmark.
	MaxConcurrency int
	// Workers is the number of goroutines delivering queued (async) mail.
	Workers int
	// QueueSize is the async queue capacity. Zero disables async sends.
	QueueSize int
	// MaxRetries is the number of retries for retryable upstream failures.
	MaxRetries int
	// UpstreamTimeout bounds a single request attempt to Postmark.
	UpstreamTimeout time.Duration
	// MaxBodyBytes caps the size of an incoming request body.
	MaxBodyBytes int64
	// ShutdownTimeout bounds graceful shutdown, including queue draining.
	ShutdownTimeout time.Duration

	LogLevel  slog.Level
	LogFormat string // "text" or "json"
}

// Default returns the built-in defaults.
func Default() Config {
	return Config{
		Listen:          "127.0.0.1:8025",
		SocketMode:      0o660,
		BlackholeKeep:   1000,
		APIURL:          "https://api.postmarkapp.com",
		MaxConcurrency:  128,
		Workers:         32,
		QueueSize:       10000,
		MaxRetries:      3,
		UpstreamTimeout: 30 * time.Second,
		MaxBodyBytes:    50 << 20, // Postmark's batch limit
		ShutdownTimeout: 30 * time.Second,
		LogLevel:        slog.LevelInfo,
		LogFormat:       "text",
	}
}

// Load builds a Config from defaults, then the file at path (if non-empty),
// then environ (KEY=VALUE pairs, typically os.Environ()).
func Load(path string, environ []string) (Config, error) {
	cfg := Default()
	vals := map[string]string{}

	if path != "" {
		fileVals, err := ParseFile(path)
		if err != nil {
			return cfg, err
		}
		for k, v := range fileVals {
			if !known[k] {
				return cfg, fmt.Errorf("%s: unknown key %q", path, k)
			}
			vals[k] = v
		}
	}
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if ok && strings.HasPrefix(k, Prefix) && known[k] {
			vals[k] = v
		}
	}

	if err := cfg.apply(vals); err != nil {
		return cfg, err
	}
	if err := cfg.loadCredentials(vals, lookup(environ, "CREDENTIALS_DIRECTORY")); err != nil {
		return cfg, err
	}
	return cfg, cfg.Validate()
}

var known = map[string]bool{}

func init() {
	for _, k := range []string{
		"LISTEN", "SMTP_LISTEN", "SMTP_HOSTNAME", "SOCKET_MODE",
		"BLACKHOLE", "BLACKHOLE_KEEP",
		"SERVER_TOKEN", "SERVER_TOKEN_FILE", "API_URL",
		"DEFAULT_FROM", "DEFAULT_MESSAGE_STREAM",
		"AUTH_TOKENS", "AUTH_TOKENS_FILE",
		"MAX_CONCURRENCY", "WORKERS", "QUEUE_SIZE", "MAX_RETRIES",
		"UPSTREAM_TIMEOUT", "MAX_BODY_BYTES", "SHUTDOWN_TIMEOUT",
		"LOG_LEVEL", "LOG_FORMAT",
	} {
		known[Prefix+k] = true
	}
}

func (c *Config) apply(vals map[string]string) error {
	var errs []error
	str := func(key string, dst *string) {
		if v, ok := vals[Prefix+key]; ok {
			*dst = v
		}
	}
	integer := func(key string, dst *int) {
		if v, ok := vals[Prefix+key]; ok {
			n, err := strconv.Atoi(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s%s: %q is not an integer", Prefix, key, v))
				return
			}
			*dst = n
		}
	}
	duration := func(key string, dst *time.Duration) {
		if v, ok := vals[Prefix+key]; ok {
			d, err := time.ParseDuration(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s%s: %q is not a duration (e.g. 30s, 1m)", Prefix, key, v))
				return
			}
			*dst = d
		}
	}

	str("LISTEN", &c.Listen)
	str("SMTP_LISTEN", &c.SMTPListen)
	str("SMTP_HOSTNAME", &c.SMTPHostname)
	integer("BLACKHOLE_KEEP", &c.BlackholeKeep)
	if v, ok := vals[Prefix+"BLACKHOLE"]; ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%sBLACKHOLE: %q must be true or false", Prefix, v))
		}
		c.Blackhole = b
	}
	if v, ok := vals[Prefix+"SOCKET_MODE"]; ok {
		m, err := strconv.ParseUint(v, 8, 32)
		if err != nil || m > 0o777 {
			errs = append(errs, fmt.Errorf("%sSOCKET_MODE: %q is not an octal file mode (e.g. 0660)", Prefix, v))
		} else {
			c.SocketMode = fs.FileMode(m)
		}
	}
	str("SERVER_TOKEN", &c.ServerToken)
	str("API_URL", &c.APIURL)
	str("DEFAULT_FROM", &c.DefaultFrom)
	str("DEFAULT_MESSAGE_STREAM", &c.DefaultMessageStream)
	if v, ok := vals[Prefix+"AUTH_TOKENS"]; ok {
		c.AuthTokens = splitTokens(v)
	}
	integer("MAX_CONCURRENCY", &c.MaxConcurrency)
	integer("WORKERS", &c.Workers)
	integer("QUEUE_SIZE", &c.QueueSize)
	integer("MAX_RETRIES", &c.MaxRetries)
	duration("UPSTREAM_TIMEOUT", &c.UpstreamTimeout)
	duration("SHUTDOWN_TIMEOUT", &c.ShutdownTimeout)
	if v, ok := vals[Prefix+"MAX_BODY_BYTES"]; ok {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			errs = append(errs, fmt.Errorf("%sMAX_BODY_BYTES: %q is not an integer", Prefix, v))
		} else {
			c.MaxBodyBytes = n
		}
	}
	if v, ok := vals[Prefix+"LOG_LEVEL"]; ok {
		if err := c.LogLevel.UnmarshalText([]byte(v)); err != nil {
			errs = append(errs, fmt.Errorf("%sLOG_LEVEL: %q must be debug, info, warn or error", Prefix, v))
		}
	}
	str("LOG_FORMAT", &c.LogFormat)
	return errors.Join(errs...)
}

// loadCredentials resolves secrets from *_FILE keys and from systemd
// credentials. Explicit values win over files, files over credentials.
func (c *Config) loadCredentials(vals map[string]string, credDir string) error {
	if c.ServerToken == "" {
		path := vals[Prefix+"SERVER_TOKEN_FILE"]
		if path == "" && credDir != "" {
			path = optional(filepath.Join(credDir, CredentialServerToken))
		}
		if path != "" {
			b, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("reading server token: %w", err)
			}
			c.ServerToken = strings.TrimSpace(string(b))
		}
	}
	if len(c.AuthTokens) == 0 {
		path := vals[Prefix+"AUTH_TOKENS_FILE"]
		if path == "" && credDir != "" {
			path = optional(filepath.Join(credDir, CredentialAuthTokens))
		}
		if path != "" {
			b, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("reading auth tokens: %w", err)
			}
			c.AuthTokens = splitTokens(string(b))
		}
	}
	return nil
}

// Validate reports configuration errors.
func (c Config) Validate() error {
	var errs []error
	if c.ServerToken == "" && !c.Blackhole {
		errs = append(errs, fmt.Errorf("a Postmark server token is required: set %sSERVER_TOKEN, %sSERVER_TOKEN_FILE or the %q systemd credential (or %sBLACKHOLE=true for testing)", Prefix, Prefix, CredentialServerToken, Prefix))
	}
	if len(ParseAddrs(c.Listen)) == 0 && len(ParseAddrs(c.SMTPListen)) == 0 {
		errs = append(errs, fmt.Errorf("%sLISTEN or %sSMTP_LISTEN must be set", Prefix, Prefix))
	}
	if c.BlackholeKeep < 0 {
		errs = append(errs, fmt.Errorf("%sBLACKHOLE_KEEP must be >= 0", Prefix))
	}
	if !strings.HasPrefix(c.APIURL, "https://") && !strings.HasPrefix(c.APIURL, "http://") {
		errs = append(errs, fmt.Errorf("%sAPI_URL must be an http(s) URL", Prefix))
	}
	if c.MaxConcurrency < 1 {
		errs = append(errs, fmt.Errorf("%sMAX_CONCURRENCY must be >= 1", Prefix))
	}
	if c.QueueSize < 0 {
		errs = append(errs, fmt.Errorf("%sQUEUE_SIZE must be >= 0", Prefix))
	}
	if c.QueueSize > 0 && c.Workers < 1 {
		errs = append(errs, fmt.Errorf("%sWORKERS must be >= 1 when the queue is enabled", Prefix))
	}
	if c.MaxRetries < 0 {
		errs = append(errs, fmt.Errorf("%sMAX_RETRIES must be >= 0", Prefix))
	}
	if c.UpstreamTimeout <= 0 || c.ShutdownTimeout <= 0 {
		errs = append(errs, fmt.Errorf("timeouts must be positive"))
	}
	if c.MaxBodyBytes < 1024 {
		errs = append(errs, fmt.Errorf("%sMAX_BODY_BYTES must be >= 1024", Prefix))
	}
	if c.LogFormat != "text" && c.LogFormat != "json" {
		errs = append(errs, fmt.Errorf("%sLOG_FORMAT must be text or json", Prefix))
	}
	return errors.Join(errs...)
}

// Addr is a listen address.
type Addr struct {
	Network string // "tcp" or "unix"
	Address string
}

// ParseAddrs parses a comma separated list of TCP addresses and unix
// sockets ("unix:/path" or an absolute path).
func ParseAddrs(list string) []Addr {
	var out []Addr
	for _, a := range strings.Split(list, ",") {
		a = strings.TrimSpace(a)
		switch {
		case a == "":
		case strings.HasPrefix(a, "unix:"):
			out = append(out, Addr{"unix", strings.TrimPrefix(a, "unix:")})
		case strings.HasPrefix(a, "/"):
			out = append(out, Addr{"unix", a})
		default:
			out = append(out, Addr{"tcp", a})
		}
	}
	return out
}

// ParseFile reads a KEY=VALUE file. The syntax is a subset of systemd's
// EnvironmentFile=: blank lines and lines starting with # or ; are ignored,
// an optional leading "export " is allowed, and values may be wrapped in
// single or double quotes.
func ParseFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	vals := map[string]string{}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected KEY=VALUE", path, n)
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		vals[k] = v
	}
	return vals, sc.Err()
}

// splitTokens splits a comma, whitespace or newline separated token list.
// Anything after a # on a line is a comment.
func splitTokens(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line, _, _ = strings.Cut(line, "#")
		out = append(out, strings.FieldsFunc(line, func(r rune) bool {
			return r == ',' || r == ' ' || r == '\t' || r == '\r'
		})...)
	}
	return out
}

func optional(path string) string {
	if _, err := os.Stat(path); err == nil {
		return path
	}
	return ""
}

func lookup(environ []string, key string) string {
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	return ""
}
