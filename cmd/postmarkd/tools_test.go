package main

import (
	"context"
	"strings"
	"testing"

	"github.com/rossedman/postmarkd/internal/domaincheck"
)

func TestHealthcheck(t *testing.T) {
	d := newFakeDaemon(t, 200, "ready\n")
	c, _, stderr := testCLI("")
	if code := c.healthcheck(nil); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if r := d.last(t); r.method != "GET" || r.path != "/readyz" {
		t.Errorf("request %s %s", r.method, r.path)
	}

	newFakeDaemon(t, 503, "not ready\n")
	c, _, stderr = testCLI("")
	if code := c.healthcheck(nil); code != 1 || !strings.Contains(stderr.String(), "unhealthy: HTTP 503") {
		t.Errorf("not ready: exit %d, stderr %q", code, stderr)
	}

	// -addr wins over $POSTMARKD_ADDR.
	ok := newFakeDaemon(t, 200, "ready\n")
	unreachable(t)
	c, _, stderr = testCLI("")
	if code := c.healthcheck([]string{"-addr", ok.URL}); code != 0 {
		t.Errorf("-addr: exit %d: %s", code, stderr)
	}

	c, _, stderr = testCLI("")
	if code := c.healthcheck(nil); code != 1 || !strings.Contains(stderr.String(), "unhealthy: cannot reach postmarkd") {
		t.Errorf("unreachable: exit %d, stderr %q", code, stderr)
	}
}

func TestCheckDomain(t *testing.T) {
	t.Parallel()
	var got domaincheck.Options
	stub := func(checks ...domaincheck.Check) func(context.Context, domaincheck.Options) []domaincheck.Check {
		return func(_ context.Context, o domaincheck.Options) []domaincheck.Check {
			got = o
			return checks
		}
	}

	c, stdout, _ := testCLI("")
	c.checkDomain = stub(
		domaincheck.Check{Name: "DKIM", Status: domaincheck.Pass, Detail: "published"},
		domaincheck.Check{Name: "DMARC", Status: domaincheck.Info, Detail: "p=none", Fix: "line one\nline two"},
	)
	// Flags may come after the domain.
	if code := c.checkDomainCmd([]string{"example.com", "-dkim-selector", "pm", "-return-path", "bounce.example.com", "-account-token", "acct"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got.Domain != "example.com" || got.DKIMSelector != "pm" || got.ReturnPath != "bounce.example.com" || got.AccountToken != "acct" || got.HTTP == nil {
		t.Errorf("options = %+v", got)
	}
	out := stdout.String()
	for _, want := range []string{
		"Email authentication for example.com",
		"[PASS] DKIM",
		"[INFO] DMARC",
		"line one\n",
		"line two\n",
		"No blocking problems found.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	c, stdout, _ = testCLI("")
	c.checkDomain = stub(domaincheck.Check{Name: "DMARC", Status: domaincheck.Fail, Detail: "missing"})
	if code := c.checkDomainCmd([]string{"-dkim-selector", "pm", "example.org"}); code != 1 {
		t.Errorf("failing check: exit %d", code)
	}
	if !strings.Contains(stdout.String(), "Some checks failed") {
		t.Errorf("output:\n%s", stdout)
	}
}

func TestCheckDomainUsage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		args []string
		code int
		err  string
	}{
		{nil, 2, "Usage: postmarkd check-domain"},
		{[]string{"-h"}, 0, "Usage: postmarkd check-domain"},
		{[]string{"example.com", "-bogus"}, 2, "flag provided but not defined"},
	} {
		c, _, stderr := testCLI("")
		if code := c.checkDomainCmd(tc.args); code != tc.code || !strings.Contains(stderr.String(), tc.err) {
			t.Errorf("%q: exit %d, stderr %q", tc.args, code, stderr)
		}
	}
}
