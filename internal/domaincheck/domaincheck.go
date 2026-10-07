// Package domaincheck verifies the DNS records that make mail sent through
// Postmark authenticate: DKIM, a custom Return-Path (SPF alignment) and DMARC.
package domaincheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

// ReturnPathTarget is the CNAME target Postmark expects for a custom
// Return-Path domain.
const ReturnPathTarget = "pm.mtasv.net"

// Status is the result of one check.
type Status int

const (
	Pass Status = iota
	Info
	Warn
	Fail
)

func (s Status) String() string {
	return [...]string{"PASS", "INFO", "WARN", "FAIL"}[s]
}

// Check is one finding.
type Check struct {
	Name   string
	Status Status
	Detail string
	Fix    string // what to change, if anything
}

// Resolver is the subset of *net.Resolver used.
type Resolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
	LookupCNAME(ctx context.Context, host string) (string, error)
}

// Options configures Run.
type Options struct {
	Domain       string
	ReturnPath   string // full Return-Path host; default "pm-bounces.<domain>"
	DKIMSelector string // e.g. "20240101pm"; discovered via the API when an account token is set
	AccountToken string // Postmark account API token (optional)
	APIURL       string
	HTTP         *http.Client
	Resolver     Resolver
}

// Run performs every check.
func Run(ctx context.Context, o Options) []Check {
	o.Domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(o.Domain)), ".")
	if o.Resolver == nil {
		o.Resolver = net.DefaultResolver
	}
	if o.HTTP == nil {
		o.HTTP = http.DefaultClient
	}
	if o.APIURL == "" {
		o.APIURL = "https://api.postmarkapp.com"
	}

	var checks []Check
	var pm *postmarkDomain
	if o.AccountToken != "" {
		var c Check
		pm, c = lookupPostmark(ctx, o)
		checks = append(checks, c)
		if pm != nil {
			if o.DKIMSelector == "" {
				host := pm.DKIMHost
				if host == "" {
					host = pm.DKIMPendingHost
				}
				o.DKIMSelector, _, _ = strings.Cut(host, "._domainkey")
			}
			if o.ReturnPath == "" && pm.ReturnPathDomain != "" {
				o.ReturnPath = pm.ReturnPathDomain
			}
		}
	}
	if o.ReturnPath == "" {
		o.ReturnPath = "pm-bounces." + o.Domain
	}

	checks = append(checks, checkDKIM(ctx, o, pm))
	checks = append(checks, checkReturnPath(ctx, o, pm))
	checks = append(checks, checkDMARC(ctx, o))
	checks = append(checks, checkSPF(ctx, o))
	return checks
}

type postmarkDomain struct {
	ID                         int64
	Name                       string
	DKIMVerified               bool
	DKIMHost                   string
	DKIMTextValue              string
	DKIMPendingHost            string
	DKIMPendingTextValue       string
	ReturnPathDomain           string
	ReturnPathDomainVerified   bool
	ReturnPathDomainCNAMEValue string
}

func lookupPostmark(ctx context.Context, o Options) (*postmarkDomain, Check) {
	c := Check{Name: "Postmark domain"}
	var list struct{ Domains []postmarkDomain }
	if err := getJSON(ctx, o, "/domains?count=500&offset=0", &list); err != nil {
		c.Status, c.Detail = Fail, err.Error()
		return nil, c
	}
	for _, d := range list.Domains {
		if strings.EqualFold(d.Name, o.Domain) {
			var full postmarkDomain
			if err := getJSON(ctx, o, fmt.Sprintf("/domains/%d", d.ID), &full); err != nil {
				c.Status, c.Detail = Fail, err.Error()
				return nil, c
			}
			c.Status, c.Detail = Pass, fmt.Sprintf("%s is set up in Postmark (ID %d)", full.Name, full.ID)
			return &full, c
		}
	}
	c.Status = Fail
	c.Detail = o.Domain + " is not added to your Postmark account"
	c.Fix = "Add it in Postmark under Sender Signatures → Add Domain, then re-run this check for the DNS records to create."
	return nil, c
}

func getJSON(ctx context.Context, o Options, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.APIURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Postmark-Account-Token", o.AccountToken)
	res, err := o.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("Postmark API: %w", err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode != 200 {
		var e struct{ Message string }
		json.Unmarshal(b, &e)
		return fmt.Errorf("Postmark API returned %d: %s (an *account* token is required, not a server token)", res.StatusCode, e.Message)
	}
	return json.Unmarshal(b, v)
}

func checkDKIM(ctx context.Context, o Options, pm *postmarkDomain) Check {
	c := Check{Name: "DKIM"}
	if o.DKIMSelector == "" {
		c.Status = Info
		c.Detail = "selector unknown, so DKIM was not checked"
		c.Fix = "Pass -account-token (Postmark → Account → API Tokens) or -dkim-selector (the part before ._domainkey in Postmark's DKIM record)."
		return c
	}
	host := o.DKIMSelector + "._domainkey." + o.Domain
	txt, err := o.Resolver.LookupTXT(ctx, host)
	record := strings.Join(txt, "")
	switch {
	case pm != nil && pm.DKIMVerified:
		c.Status, c.Detail = Pass, "verified by Postmark ("+host+")"
	case err == nil && strings.Contains(record, "p="):
		c.Status, c.Detail = Pass, "TXT record found at "+host
		if pm != nil {
			c.Status = Warn
			c.Detail += ", but Postmark has not verified it yet"
			c.Fix = "Click Verify next to DKIM for this domain in Postmark."
		}
	default:
		c.Status, c.Detail = Fail, "no DKIM key at "+host
		c.Fix = "Create the DKIM TXT record shown in Postmark for this domain."
		if pm != nil {
			h, v := pm.DKIMPendingHost, pm.DKIMPendingTextValue
			if h == "" {
				h, v = pm.DKIMHost, pm.DKIMTextValue
			}
			if h != "" {
				c.Fix = fmt.Sprintf("Create TXT %s with value:\n%s", fqdn(h, o.Domain), v)
			}
		}
	}
	return c
}

func checkReturnPath(ctx context.Context, o Options, pm *postmarkDomain) Check {
	c := Check{Name: "Return-Path (SPF alignment)"}
	cname, err := o.Resolver.LookupCNAME(ctx, o.ReturnPath)
	ok := err == nil && strings.TrimSuffix(strings.ToLower(cname), ".") == ReturnPathTarget
	switch {
	case pm != nil && pm.ReturnPathDomainVerified:
		c.Status, c.Detail = Pass, o.ReturnPath+" verified by Postmark"
	case ok:
		c.Status, c.Detail = Pass, o.ReturnPath+" → "+ReturnPathTarget
		if pm != nil {
			c.Status = Warn
			c.Detail += ", but it is not set as this domain's Return-Path in Postmark"
			c.Fix = "Set " + o.ReturnPath + " as the custom Return-Path for the domain in Postmark."
		}
	default:
		c.Status = Warn
		c.Detail = "no CNAME " + o.ReturnPath + " → " + ReturnPathTarget
		c.Fix = fmt.Sprintf("Create CNAME %s pointing to %s and set it as the Return-Path in Postmark. Without it SPF passes only for Postmark's domain, so DMARC relies on DKIM alone.", o.ReturnPath, ReturnPathTarget)
	}
	return c
}

func checkDMARC(ctx context.Context, o Options) Check {
	c := Check{Name: "DMARC"}
	host := "_dmarc." + o.Domain
	txt, err := o.Resolver.LookupTXT(ctx, host)
	var records []string
	for _, t := range txt {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(t)), "v=dmarc1") {
			records = append(records, t)
		}
	}
	fix := fmt.Sprintf(`Create TXT %s with value:
v=DMARC1; p=none; rua=mailto:<reports address>
Postmark offers free weekly DMARC digests at https://dmarc.postmarkapp.com which gives you a rua address. Move to p=quarantine or p=reject once reports show all your mail passes.`, host)
	var dnsErr *net.DNSError
	switch {
	case err != nil && !(errors.As(err, &dnsErr) && dnsErr.IsNotFound):
		c.Status, c.Detail = Fail, "lookup failed: "+err.Error()
	case len(records) == 0:
		c.Status, c.Detail, c.Fix = Fail, "no DMARC record at "+host+". Gmail and Yahoo require one for bulk senders.", fix
	case len(records) > 1:
		c.Status, c.Detail, c.Fix = Fail, "multiple DMARC records; receivers will ignore all of them", "Keep exactly one TXT record at "+host+"."
	default:
		policy := tag(records[0], "p")
		c.Detail = fmt.Sprintf("p=%s (%s)", policy, records[0])
		switch policy {
		case "reject", "quarantine":
			c.Status = Pass
		case "none":
			c.Status = Info
			c.Fix = "p=none only monitors. Once DMARC reports show your mail passes, move to p=quarantine and then p=reject."
		default:
			c.Status, c.Fix = Fail, "The record has no valid p= tag."
		}
		if tag(records[0], "rua") == "" {
			c.Fix = strings.TrimSpace(c.Fix + " Add rua=mailto:… to receive aggregate reports (Postmark's free DMARC monitoring provides one).")
			if c.Status == Pass {
				c.Status = Info
			}
		}
	}
	return c
}

func checkSPF(ctx context.Context, o Options) Check {
	c := Check{Name: "SPF (" + o.Domain + ")"}
	txt, _ := o.Resolver.LookupTXT(ctx, o.Domain)
	var records []string
	for _, t := range txt {
		if strings.HasPrefix(strings.ToLower(t), "v=spf1") {
			records = append(records, t)
		}
	}
	switch len(records) {
	case 0:
		c.Status, c.Detail = Info, "no SPF record. Not needed for Postmark, which uses its own Return-Path domain."
	case 1:
		c.Status, c.Detail = Pass, records[0]
	default:
		c.Status, c.Detail = Fail, "multiple SPF records (SPF permerror)"
		c.Fix = "Merge them into a single v=spf1 record."
	}
	return c
}

func tag(record, name string) string {
	for _, part := range strings.Split(record, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && strings.EqualFold(strings.TrimSpace(k), name) {
			return strings.ToLower(strings.TrimSpace(v))
		}
	}
	return ""
}

func fqdn(host, domain string) string {
	host = strings.TrimSuffix(host, ".")
	if strings.HasSuffix(strings.ToLower(host), "."+domain) {
		return host
	}
	return host + "." + domain
}
