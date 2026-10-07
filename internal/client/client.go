// Package client talks to a running postmarkd over its unix socket or TCP.
package client

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/rossedman/postmarkd/internal/config"
)

// DefaultSocket is where the packaged service listens.
const DefaultSocket = "/run/postmarkd/postmarkd.sock"

// Client is a postmarkd API client.
type Client struct {
	http  *http.Client
	base  string
	token string
}

// ResolveAddr picks the daemon address: the explicit value, then
// $POSTMARKD_ADDR, then the first $POSTMARKD_LISTEN address, then the
// default socket.
func ResolveAddr(explicit string) string {
	for _, a := range []string{explicit, os.Getenv("POSTMARKD_ADDR")} {
		if a != "" {
			return a
		}
	}
	if addrs := config.ParseAddrs(os.Getenv("POSTMARKD_LISTEN")); len(addrs) > 0 {
		a := addrs[0]
		if a.Network == "unix" {
			return a.Address
		}
		host, port, err := net.SplitHostPort(a.Address)
		if err == nil && (host == "" || host == "0.0.0.0" || host == "::") {
			host = "127.0.0.1"
		}
		return net.JoinHostPort(host, port)
	}
	return DefaultSocket
}

// New returns a client for addr: a socket path ("/path" or "unix:/path"),
// "host:port" or an http(s) URL. token is sent as a bearer token if set.
func New(addr, token string) *Client {
	c := &Client{token: token}
	tr := &http.Transport{MaxIdleConnsPerHost: 64}
	switch {
	case strings.HasPrefix(addr, "unix:") || strings.HasPrefix(addr, "/"):
		path := strings.TrimPrefix(addr, "unix:")
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		}
		c.base = "http://postmarkd"
	case strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://"):
		c.base = strings.TrimSuffix(addr, "/")
	default:
		c.base = "http://" + addr
	}
	c.http = &http.Client{Transport: tr, Timeout: 5 * time.Minute}
	return c
}

// Response is a raw API response.
type Response struct {
	StatusCode int
	Body       []byte
}

// Do sends a request and returns the response body.
func (c *Client) Do(ctx context.Context, method, path string, body []byte, header http.Header) (*Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach postmarkd: %w", err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	return &Response{StatusCode: res.StatusCode, Body: b}, nil
}
