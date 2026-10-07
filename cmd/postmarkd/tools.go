package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/rossedman/postmarkd/internal/client"
	"github.com/rossedman/postmarkd/internal/domaincheck"
)

func healthcheck(args []string) int {
	fl := flag.NewFlagSet("healthcheck", flag.ExitOnError)
	addr := fl.String("addr", "", "postmarkd address (default $POSTMARKD_ADDR, the first $POSTMARKD_LISTEN address, or "+client.DefaultSocket+")")
	fl.Parse(args)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := client.New(client.ResolveAddr(*addr), "").Do(ctx, http.MethodGet, "/readyz", nil, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "unhealthy:", err)
		return 1
	}
	if res.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "unhealthy: HTTP %d\n", res.StatusCode)
		return 1
	}
	return 0
}

func checkDomain(args []string) int {
	fl := flag.NewFlagSet("check-domain", flag.ExitOnError)
	token := fl.String("account-token", os.Getenv("POSTMARK_ACCOUNT_TOKEN"), "Postmark *account* API token, to look up the exact records Postmark expects (default $POSTMARK_ACCOUNT_TOKEN)")
	selector := fl.String("dkim-selector", "", "DKIM selector to check (found automatically with -account-token)")
	returnPath := fl.String("return-path", "", "custom Return-Path host (default pm-bounces.<domain>)")
	fl.Usage = func() {
		fmt.Fprint(fl.Output(), `Usage: postmarkd check-domain [flags] <domain>

Checks the DNS records that make mail from <domain> authenticate when sent
through Postmark: DKIM, a custom Return-Path (SPF alignment) and DMARC.

Flags:
`)
		fl.PrintDefaults()
	}
	// Allow flags after the domain too.
	var domain string
	for len(args) > 0 {
		fl.Parse(args)
		if fl.NArg() == 0 {
			break
		}
		domain, args = fl.Arg(0), fl.Args()[1:]
	}
	if domain == "" {
		fl.Usage()
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	checks := domaincheck.Run(ctx, domaincheck.Options{
		Domain: domain, AccountToken: *token, DKIMSelector: *selector, ReturnPath: *returnPath,
		HTTP: &http.Client{Timeout: 15 * time.Second},
	})

	fmt.Printf("Email authentication for %s\n\n", domain)
	failed := false
	for _, c := range checks {
		fmt.Printf("  [%s] %-28s %s\n", c.Status, c.Name, c.Detail)
		if c.Fix != "" {
			for _, l := range strings.Split(c.Fix, "\n") {
				fmt.Printf("         %-28s %s\n", "", l)
			}
		}
		failed = failed || c.Status == domaincheck.Fail
	}
	fmt.Println()
	if failed {
		fmt.Println("Some checks failed: mail from this domain may land in spam or be rejected.")
		return 1
	}
	fmt.Println("No blocking problems found.")
	return 0
}
