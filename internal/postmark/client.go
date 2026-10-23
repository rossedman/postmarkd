// Package postmark is a minimal, concurrency-limited, retrying HTTP client
// for the Postmark email API. Request and response bodies are passed through
// as raw JSON so postmarkd stays forward compatible with new API fields.
package postmark

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"time"
)

// DefaultBaseURL is the production Postmark API.
const DefaultBaseURL = "https://api.postmarkapp.com"

// maxResponseBytes bounds how much of an upstream response we buffer.
const maxResponseBytes = 16 << 20

// Options configures a Client.
type Options struct {
	BaseURL        string
	ServerToken    string
	MaxConcurrency int           // simultaneous in-flight requests
	MaxRetries     int           // retries for retryable failures
	Timeout        time.Duration // per attempt
	UserAgent      string
}

// Client sends requests to Postmark.
type Client struct {
	base       string
	token      string
	ua         string
	http       *http.Client
	sem        chan struct{}
	maxRetries int
	timeout    time.Duration
	backoff    func(attempt int) time.Duration
}

// Response is a buffered upstream response.
type Response struct {
	StatusCode  int
	ContentType string
	Body        []byte
	Attempts    int
}

// New returns a Client tuned for high request volume against a single host.
func New(o Options) *Client {
	if o.BaseURL == "" {
		o.BaseURL = DefaultBaseURL
	}
	if o.MaxConcurrency < 1 {
		o.MaxConcurrency = 1
	}
	if o.Timeout <= 0 {
		o.Timeout = 30 * time.Second
	}
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          o.MaxConcurrency,
		MaxIdleConnsPerHost:   o.MaxConcurrency,
		MaxConnsPerHost:       o.MaxConcurrency,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &Client{
		base:       o.BaseURL,
		token:      o.ServerToken,
		ua:         o.UserAgent,
		http:       &http.Client{Transport: transport},
		sem:        make(chan struct{}, o.MaxConcurrency),
		maxRetries: o.MaxRetries,
		timeout:    o.Timeout,
		backoff:    defaultBackoff,
	}
}

// InFlight reports the number of requests currently holding a concurrency slot.
func (c *Client) InFlight() int { return len(c.sem) }

// Post sends body as JSON to path (e.g. "/email"). Retryable failures are
// retried with exponential backoff. A non-2xx response is not an error; the
// caller inspects StatusCode and Body.
//
// Only failures where Postmark cannot have accepted the message are retried
// (connection failures, 429, 502 and 503), so retries never duplicate mail.
func (c *Client) Post(ctx context.Context, path string, body []byte) (*Response, error) {
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	for attempt := 0; ; attempt++ {
		resp, retryAfter, err := c.do(ctx, path, body)
		if resp != nil {
			resp.Attempts = attempt + 1
		}
		retry := attempt < c.maxRetries && (retryableStatus(resp) || retryableErr(err))
		if !retry {
			if err != nil {
				return nil, fmt.Errorf("postmark %s: %w (after %d attempt(s))", path, err, attempt+1)
			}
			return resp, nil
		}
		wait := c.backoff(attempt)
		if retryAfter > 0 && retryAfter < time.Minute {
			wait = max(wait, retryAfter)
		}
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			if err == nil {
				return resp, nil
			}
			return nil, ctx.Err()
		}
	}
}

// do makes one POST attempt with the per-attempt timeout. It returns the
// response and the delay the server asked for in Retry-After, if any.
func (c *Client) do(ctx context.Context, path string, body []byte) (*Response, time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Postmark-Server-Token", c.token)
	if c.ua != "" {
		req.Header.Set("User-Agent", c.ua)
	}

	res, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes))
	if err != nil {
		return nil, 0, fmt.Errorf("reading response: %w", err)
	}
	var retryAfter time.Duration
	if s, err := strconv.Atoi(res.Header.Get("Retry-After")); err == nil && s > 0 {
		retryAfter = time.Duration(s) * time.Second
	}
	return &Response{
		StatusCode:  res.StatusCode,
		ContentType: res.Header.Get("Content-Type"),
		Body:        b,
	}, retryAfter, nil
}

// retryableStatus reports whether r is a rate limit or a transient
// upstream failure worth retrying.
func retryableStatus(r *Response) bool {
	if r == nil {
		return false
	}
	switch r.StatusCode {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable:
		return true
	}
	return false
}

// retryableErr reports whether err happened before the request could have
// reached Postmark (DNS failure, connection refused, dial timeout).
func retryableErr(err error) bool {
	var op *net.OpError
	return err != nil && errors.As(err, &op) && op.Op == "dial"
}

// defaultBackoff is exponential backoff (250ms, 500ms, 1s, … capped at 10s)
// with jitter over the upper half of each interval.
func defaultBackoff(attempt int) time.Duration {
	d := min(250*time.Millisecond<<min(attempt, 6), 10*time.Second)
	return d/2 + rand.N(d/2+1)
}
