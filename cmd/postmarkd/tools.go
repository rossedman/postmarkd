package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/rossedman/postmarkd/internal/client"
	"github.com/rossedman/postmarkd/internal/domaincheck"
)

// healthcheck exits 0 if the daemon reports itself ready.
func (c *cli) healthcheck(args []string) int {
	fl := c.flags("healthcheck")
	addr := fl.String("addr", "", "postmarkd address (default $POSTMARKD_ADDR, the first $POSTMARKD_LISTEN address, or "+client.DefaultSocket+")")
	if code := parse(fl, args); code >= 0 {
		return code
	}
	ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
	defer cancel()
	res, err := client.New(client.ResolveAddr(*addr), "").Do(ctx, http.MethodGet, "/readyz", nil, nil)
	if err != nil {
		c.errorf("unhealthy: %v\n", err)
		return 1
	}
	if res.StatusCode != http.StatusOK {
		c.errorf("unhealthy: HTTP %d\n", res.StatusCode)
		return 1
	}
	return 0
}

// checkDomainCmd checks a domain's DNS records for sending through
// Postmark and prints a report.
func (c *cli) checkDomainCmd(args []string) int {
	fl := c.flags("check-domain")
	token := fl.String("account-token", os.Getenv("POSTMARK_ACCOUNT_TOKEN"), "Postmark *account* API token, to look up the exact records Postmark expects (default $POSTMARK_ACCOUNT_TOKEN)")
	selector := fl.String("dkim-selector", "", "DKIM selector to check (found automatically with -account-token)")
	returnPath := fl.String("return-path", "", "custom Return-Path host (default pm-bounces.<domain>)")
	fl.Usage = func() {
		_, _ = fmt.Fprint(fl.Output(), `Usage: postmarkd check-domain [flags] <domain>

Checks the DNS records that make mail from <domain> authenticate when sent
through Postmark: DKIM, a custom Return-Path (SPF alignment) and DMARC.

Flags:
`)
		fl.PrintDefaults()
	}
	// Allow flags after the domain too.
	var domain string
	for len(args) > 0 {
		if code := parse(fl, args); code >= 0 {
			return code
		}
		if fl.NArg() == 0 {
			break
		}
		domain, args = fl.Arg(0), fl.Args()[1:]
	}
	if domain == "" {
		fl.Usage()
		return 2
	}

	ctx, cancel := context.WithTimeout(c.ctx, 30*time.Second)
	defer cancel()
	checks := c.checkDomain(ctx, domaincheck.Options{
		Domain: domain, AccountToken: *token, DKIMSelector: *selector, ReturnPath: *returnPath,
		HTTP: &http.Client{Timeout: 15 * time.Second},
	})

	c.printf("Email authentication for %s\n\n", domain)
	failed := false
	for _, ch := range checks {
		c.printf("  [%s] %-28s %s\n", ch.Status, ch.Name, ch.Detail)
		if ch.Fix != "" {
			for _, l := range strings.Split(ch.Fix, "\n") {
				c.printf("         %-28s %s\n", "", l)
			}
		}
		failed = failed || ch.Status == domaincheck.Fail
	}
	c.printf("\n")
	if failed {
		c.printf("Some checks failed: mail from this domain may land in spam or be rejected.\n")
		return 1
	}
	c.printf("No blocking problems found.\n")
	return 0
}
