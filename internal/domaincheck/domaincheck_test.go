package domaincheck

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeDNS struct {
	txt   map[string][]string
	cname map[string]string
}

func (f fakeDNS) LookupTXT(_ context.Context, name string) ([]string, error) {
	if v, ok := f.txt[name]; ok {
		return v, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func (f fakeDNS) LookupCNAME(_ context.Context, host string) (string, error) {
	if v, ok := f.cname[host]; ok {
		return v, nil
	}
	return "", &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

func byName(checks []Check) map[string]Check {
	m := map[string]Check{}
	for _, c := range checks {
		m[strings.SplitN(c.Name, " ", 2)[0]] = c
	}
	return m
}

func TestAllGood(t *testing.T) {
	dns := fakeDNS{
		txt: map[string][]string{
			"sel._domainkey.example.com": {"k=rsa;p=MIGf"},
			"_dmarc.example.com":         {"v=DMARC1; p=reject; rua=mailto:x@dmarc.postmarkapp.com"},
			"example.com":                {"v=spf1 include:_spf.google.com ~all", "google-site-verification=x"},
		},
		cname: map[string]string{"pm-bounces.example.com": "pm.mtasv.net."},
	}
	got := byName(Run(context.Background(), Options{Domain: "Example.com", DKIMSelector: "sel", Resolver: dns}))
	for name, c := range got {
		if c.Status != Pass {
			t.Errorf("%s: %v %s", name, c.Status, c.Detail)
		}
	}
}

func TestMissingRecords(t *testing.T) {
	got := byName(Run(context.Background(), Options{Domain: "example.com", Resolver: fakeDNS{}}))
	if got["DKIM"].Status != Info || got["Return-Path"].Status != Warn || got["DMARC"].Status != Fail || got["SPF"].Status != Info {
		t.Errorf("%+v", got)
	}
	if !strings.Contains(got["DMARC"].Fix, "v=DMARC1; p=none") {
		t.Errorf("DMARC fix = %q", got["DMARC"].Fix)
	}
}

func TestPostmarkAPI(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Postmark-Account-Token") != "acct" {
			w.WriteHeader(401)
			w.Write([]byte(`{"Message":"bad token"}`))
			return
		}
		switch r.URL.Path {
		case "/domains":
			w.Write([]byte(`{"TotalCount":1,"Domains":[{"ID":7,"Name":"example.com"}]}`))
		case "/domains/7":
			w.Write([]byte(`{"ID":7,"Name":"example.com","DKIMVerified":false,"DKIMPendingHost":"2026pm._domainkey.example.com","DKIMPendingTextValue":"k=rsa;p=ABC","ReturnPathDomain":"bounce.example.com","ReturnPathDomainVerified":true}`))
		}
	}))
	defer api.Close()

	got := byName(Run(context.Background(), Options{Domain: "example.com", AccountToken: "acct", APIURL: api.URL, Resolver: fakeDNS{}}))
	if got["Postmark"].Status != Pass {
		t.Errorf("postmark: %+v", got["Postmark"])
	}
	if got["DKIM"].Status != Fail || !strings.Contains(got["DKIM"].Fix, "TXT 2026pm._domainkey.example.com") || !strings.Contains(got["DKIM"].Fix, "p=ABC") {
		t.Errorf("dkim: %+v", got["DKIM"])
	}
	if got["Return-Path"].Status != Pass || !strings.Contains(got["Return-Path"].Detail, "bounce.example.com") {
		t.Errorf("return-path: %+v", got["Return-Path"])
	}

	got = byName(Run(context.Background(), Options{Domain: "example.com", AccountToken: "wrong", APIURL: api.URL, Resolver: fakeDNS{}}))
	if got["Postmark"].Status != Fail || !strings.Contains(got["Postmark"].Detail, "account") {
		t.Errorf("bad token: %+v", got["Postmark"])
	}
}
