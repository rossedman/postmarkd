package domaincheck

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// errDNS wraps fakeDNS and fails lookups for the names in errs.
type errDNS struct {
	fakeDNS
	errs map[string]error
}

func (f errDNS) LookupTXT(ctx context.Context, name string) ([]string, error) {
	if err, ok := f.errs[name]; ok {
		return nil, err
	}
	return f.fakeDNS.LookupTXT(ctx, name)
}

func (f errDNS) LookupCNAME(ctx context.Context, host string) (string, error) {
	if err, ok := f.errs[host]; ok {
		return "", err
	}
	return f.fakeDNS.LookupCNAME(ctx, host)
}

func TestStatusString(t *testing.T) {
	t.Parallel()
	for s, want := range map[Status]string{Pass: "PASS", Info: "INFO", Warn: "WARN", Fail: "FAIL"} {
		if got := s.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", s, got, want)
		}
	}
}

func TestRunOrderAndNormalization(t *testing.T) {
	t.Parallel()
	var asked []string
	dns := recordingDNS{asked: &asked}
	checks := Run(t.Context(), Options{Domain: "  Example.COM. ", DKIMSelector: "sel", Resolver: dns})
	var names []string
	for _, c := range checks {
		names = append(names, c.Name)
	}
	want := []string{"DKIM", "Return-Path (SPF alignment)", "DMARC", "SPF (example.com)"}
	if strings.Join(names, "|") != strings.Join(want, "|") {
		t.Fatalf("checks = %q, want %q", names, want)
	}
	wantAsked := []string{"TXT sel._domainkey.example.com", "CNAME pm-bounces.example.com", "TXT _dmarc.example.com", "TXT example.com"}
	if strings.Join(asked, "|") != strings.Join(wantAsked, "|") {
		t.Fatalf("lookups = %q, want %q", asked, wantAsked)
	}
}

type recordingDNS struct{ asked *[]string }

func (r recordingDNS) LookupTXT(_ context.Context, name string) ([]string, error) {
	*r.asked = append(*r.asked, "TXT "+name)
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func (r recordingDNS) LookupCNAME(_ context.Context, host string) (string, error) {
	*r.asked = append(*r.asked, "CNAME "+host)
	return "", &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

func TestExplicitReturnPath(t *testing.T) {
	t.Parallel()
	dns := fakeDNS{cname: map[string]string{"bounce.example.com": "PM.MTASV.NET"}}
	got := byName(Run(t.Context(), Options{Domain: "example.com", ReturnPath: "bounce.example.com", Resolver: dns}))
	rp := got["Return-Path"]
	if rp.Status != Pass || rp.Detail != "bounce.example.com → pm.mtasv.net" || rp.Fix != "" {
		t.Fatalf("%+v", rp)
	}
}

func TestReturnPathWrongTarget(t *testing.T) {
	t.Parallel()
	dns := fakeDNS{cname: map[string]string{"pm-bounces.example.com": "elsewhere.example.net."}}
	rp := byName(Run(t.Context(), Options{Domain: "example.com", Resolver: dns}))["Return-Path"]
	if rp.Status != Warn || !strings.Contains(rp.Fix, "Create CNAME pm-bounces.example.com pointing to pm.mtasv.net") {
		t.Fatalf("%+v", rp)
	}
}

func TestDMARC(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		records    []string
		err        error
		want       Status
		wantDetail string
		wantFix    string
	}{
		{"reject with rua", []string{"v=DMARC1; p=reject; rua=mailto:a@b"}, nil, Pass, "p=reject (v=DMARC1; p=reject; rua=mailto:a@b)", ""},
		{"quarantine with rua", []string{" V=DMARC1;P=Quarantine;RUA=mailto:a@b"}, nil, Pass, "p=quarantine", ""},
		{"reject without rua", []string{"v=DMARC1; p=reject"}, nil, Info, "p=reject", "Add rua=mailto:"},
		{"none", []string{"v=DMARC1; p=none; rua=mailto:a@b"}, nil, Info, "p=none", "p=none only monitors"},
		{"none without rua", []string{"v=DMARC1; p=none"}, nil, Info, "p=none", "move to p=quarantine and then p=reject. Add rua=mailto:"},
		{"invalid policy", []string{"v=DMARC1; p=sometimes; rua=mailto:a@b"}, nil, Fail, "p=sometimes", "no valid p= tag"},
		{"missing policy", []string{"v=DMARC1; rua=mailto:a@b"}, nil, Fail, "p= (", "no valid p= tag"},
		{"ignores other txt", []string{"some-verification=1", "v=DMARC1; p=reject; rua=mailto:a@b"}, nil, Pass, "p=reject", ""},
		{"only unrelated txt", []string{"v=spf1 -all"}, nil, Fail, "no DMARC record at _dmarc.example.com", "v=DMARC1; p=none"},
		{"multiple", []string{"v=DMARC1; p=none", "v=DMARC1; p=reject"}, nil, Fail, "multiple DMARC records", "Keep exactly one TXT record at _dmarc.example.com."},
		{"not found", nil, &net.DNSError{Err: "no such host", IsNotFound: true}, Fail, "no DMARC record", "v=DMARC1"},
		{"servfail", nil, &net.DNSError{Err: "server misbehaving", IsTemporary: true}, Fail, "lookup failed: lookup : server misbehaving", ""},
		{"other error", nil, errors.New("boom"), Fail, "lookup failed: boom", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dns := errDNS{fakeDNS: fakeDNS{txt: map[string][]string{}}, errs: map[string]error{}}
			if tt.err != nil {
				dns.errs["_dmarc.example.com"] = tt.err
			} else {
				dns.txt["_dmarc.example.com"] = tt.records
			}
			c := checkDMARC(t.Context(), Options{Domain: "example.com", Resolver: dns})
			if c.Name != "DMARC" || c.Status != tt.want {
				t.Fatalf("status = %v, want %v (%+v)", c.Status, tt.want, c)
			}
			if !strings.Contains(c.Detail, tt.wantDetail) {
				t.Errorf("detail = %q, want it to contain %q", c.Detail, tt.wantDetail)
			}
			if tt.wantFix == "" && c.Fix != "" && tt.want == Pass {
				t.Errorf("unexpected fix %q", c.Fix)
			}
			if !strings.Contains(c.Fix, tt.wantFix) {
				t.Errorf("fix = %q, want it to contain %q", c.Fix, tt.wantFix)
			}
		})
	}
}

func TestSPF(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		records []string
		err     error
		want    Status
		detail  string
	}{
		{"none", []string{"google-site-verification=x"}, nil, Info, "no SPF record"},
		{"lookup error treated as none", nil, errors.New("timeout"), Info, "no SPF record"},
		{"one", []string{"V=SPF1 -all", "x"}, nil, Pass, "V=SPF1 -all"},
		{"multiple", []string{"v=spf1 -all", "v=spf1 ~all"}, nil, Fail, "multiple SPF records"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dns := errDNS{fakeDNS: fakeDNS{txt: map[string][]string{"example.com": tt.records}}, errs: map[string]error{}}
			if tt.err != nil {
				dns.errs["example.com"] = tt.err
			}
			c := checkSPF(t.Context(), Options{Domain: "example.com", Resolver: dns})
			if c.Name != "SPF (example.com)" || c.Status != tt.want || !strings.Contains(c.Detail, tt.detail) {
				t.Fatalf("%+v", c)
			}
			if tt.want == Fail && c.Fix != "Merge them into a single v=spf1 record." {
				t.Errorf("fix = %q", c.Fix)
			}
		})
	}
}

func TestDKIM(t *testing.T) {
	t.Parallel()
	const host = "sel._domainkey.example.com"
	tests := []struct {
		name    string
		txt     []string
		pm      *postmarkDomain
		want    Status
		detail  string
		wantFix string
	}{
		{"record found", []string{"k=rsa; ", "p=MIG"}, nil, Pass, "TXT record found at " + host, ""},
		{"record without key", []string{"k=rsa"}, nil, Fail, "no DKIM key at " + host, "Create the DKIM TXT record shown in Postmark"},
		{"missing record", nil, nil, Fail, "no DKIM key", "Create the DKIM TXT record shown in Postmark"},
		{"verified by postmark", nil, &postmarkDomain{DKIMVerified: true}, Pass, "verified by Postmark (" + host + ")", ""},
		{"found but unverified", []string{"p=MIG"}, &postmarkDomain{}, Warn, "but Postmark has not verified it yet", "Click Verify"},
		{"pending host preferred", nil, &postmarkDomain{DKIMHost: "old._domainkey", DKIMTextValue: "p=OLD", DKIMPendingHost: "new._domainkey", DKIMPendingTextValue: "p=NEW"}, Fail, "no DKIM key", "Create TXT new._domainkey.example.com with value:\np=NEW"},
		{"active host fallback", nil, &postmarkDomain{DKIMHost: "old._domainkey.example.com.", DKIMTextValue: "p=OLD"}, Fail, "no DKIM key", "Create TXT old._domainkey.example.com with value:\np=OLD"},
		{"postmark without hosts", nil, &postmarkDomain{}, Fail, "no DKIM key", "Create the DKIM TXT record shown in Postmark"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dns := fakeDNS{txt: map[string][]string{}}
			if tt.txt != nil {
				dns.txt[host] = tt.txt
			}
			c := checkDKIM(t.Context(), Options{Domain: "example.com", DKIMSelector: "sel", Resolver: dns}, tt.pm)
			if c.Status != tt.want || !strings.Contains(c.Detail, tt.detail) || !strings.Contains(c.Fix, tt.wantFix) {
				t.Fatalf("got %+v", c)
			}
			if tt.wantFix == "" && c.Fix != "" {
				t.Errorf("unexpected fix %q", c.Fix)
			}
		})
	}
}

func TestReturnPathWithPostmark(t *testing.T) {
	t.Parallel()
	dns := fakeDNS{cname: map[string]string{"pm-bounces.example.com": "pm.mtasv.net."}}
	o := Options{Domain: "example.com", ReturnPath: "pm-bounces.example.com", Resolver: dns}
	c := checkReturnPath(t.Context(), o, &postmarkDomain{})
	if c.Status != Warn || !strings.Contains(c.Detail, "not set as this domain's Return-Path") || !strings.Contains(c.Fix, "Set pm-bounces.example.com as the custom Return-Path") {
		t.Fatalf("%+v", c)
	}
	c = checkReturnPath(t.Context(), o, &postmarkDomain{ReturnPathDomainVerified: true})
	if c.Status != Pass || c.Detail != "pm-bounces.example.com verified by Postmark" {
		t.Fatalf("%+v", c)
	}
}

func TestTag(t *testing.T) {
	t.Parallel()
	tests := []struct {
		record, name, want string
	}{
		{"v=DMARC1; p=reject", "p", "reject"},
		{"v=DMARC1;P = Quarantine ;", "p", "quarantine"},
		{"v=DMARC1; pct=50", "p", ""},
		{"v=DMARC1; p", "p", ""},
		{"v=DMARC1; p=none; p=reject", "p", "none"},
		{"rua=mailto:A@B", "RUA", "mailto:a@b"},
		{"", "p", ""},
	}
	for _, tt := range tests {
		if got := tag(tt.record, tt.name); got != tt.want {
			t.Errorf("tag(%q, %q) = %q, want %q", tt.record, tt.name, got, tt.want)
		}
	}
}

func TestFQDN(t *testing.T) {
	t.Parallel()
	tests := []struct{ host, domain, want string }{
		{"sel._domainkey", "example.com", "sel._domainkey.example.com"},
		{"sel._domainkey.example.com", "example.com", "sel._domainkey.example.com"},
		{"sel._domainkey.EXAMPLE.com.", "example.com", "sel._domainkey.EXAMPLE.com"},
		{"notexample.com", "example.com", "notexample.com.example.com"},
	}
	for _, tt := range tests {
		if got := fqdn(tt.host, tt.domain); got != tt.want {
			t.Errorf("fqdn(%q, %q) = %q, want %q", tt.host, tt.domain, got, tt.want)
		}
	}
}

func TestPostmarkLookup(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		handler    http.HandlerFunc
		want       Status
		wantDetail string
		wantFix    string
		check      func(t *testing.T, got map[string]Check)
	}{
		{
			name: "domain not added",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"Domains":[{"ID":1,"Name":"other.com"}]}`))
			},
			want: Fail, wantDetail: "example.com is not added to your Postmark account", wantFix: "Add Domain",
		},
		{
			name: "details request fails",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/domains" {
					_, _ = w.Write([]byte(`{"Domains":[{"ID":3,"Name":"EXAMPLE.com"}]}`))
					return
				}
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"Message":"oops"}`))
			},
			want: Fail, wantDetail: "the Postmark API returned 500: oops",
		},
		{
			name: "non-json error body",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte(`<html>`))
			},
			want: Fail, wantDetail: "the Postmark API returned 502:  (an *account* token is required",
		},
		{
			name: "invalid json",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"Domains":`))
			},
			want: Fail, wantDetail: "unexpected end of JSON input",
		},
		{
			name: "selector and return path from active DKIM host",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("count") != "" && (r.URL.Query().Get("count") != "500" || r.URL.Query().Get("offset") != "0") {
					t.Errorf("unexpected query %q", r.URL.RawQuery)
				}
				if r.Header.Get("Accept") != "application/json" || r.Method != http.MethodGet {
					t.Errorf("unexpected request %s %v", r.Method, r.Header)
				}
				switch r.URL.Path {
				case "/domains":
					_, _ = w.Write([]byte(`{"Domains":[{"ID":9,"Name":"example.com"}]}`))
				case "/domains/9":
					_, _ = w.Write([]byte(`{"ID":9,"Name":"example.com","DKIMVerified":true,"DKIMHost":"live._domainkey.example.com","ReturnPathDomain":"rp.example.com"}`))
				default:
					http.NotFound(w, r)
				}
			},
			want: Pass, wantDetail: "example.com is set up in Postmark (ID 9)",
			check: func(t *testing.T, got map[string]Check) {
				if d := got["DKIM"]; d.Status != Pass || d.Detail != "verified by Postmark (live._domainkey.example.com)" {
					t.Errorf("dkim: %+v", d)
				}
				if rp := got["Return-Path"]; rp.Status != Warn || !strings.Contains(rp.Detail, "no CNAME rp.example.com") {
					t.Errorf("return-path: %+v", rp)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()
			got := byName(Run(t.Context(), Options{Domain: "example.com", AccountToken: "acct", APIURL: srv.URL, HTTP: srv.Client(), Resolver: fakeDNS{}}))
			pm := got["Postmark"]
			if pm.Name != "Postmark domain" || pm.Status != tt.want || !strings.Contains(pm.Detail, tt.wantDetail) || !strings.Contains(pm.Fix, tt.wantFix) {
				t.Fatalf("postmark check: %+v", pm)
			}
			if tt.check != nil {
				tt.check(t, got)
			}
		})
	}
}

func TestExplicitOptionsBeatPostmark(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/domains":
			_, _ = w.Write([]byte(`{"Domains":[{"ID":1,"Name":"example.com"}]}`))
		default:
			_, _ = w.Write([]byte(`{"ID":1,"Name":"example.com","DKIMHost":"api._domainkey.example.com","ReturnPathDomain":"api-rp.example.com"}`))
		}
	}))
	defer srv.Close()
	got := byName(Run(t.Context(), Options{
		Domain: "example.com", AccountToken: "acct", APIURL: srv.URL, HTTP: srv.Client(),
		DKIMSelector: "mine", ReturnPath: "mine-rp.example.com", Resolver: fakeDNS{},
	}))
	if d := got["DKIM"]; !strings.Contains(d.Detail, "mine._domainkey.example.com") {
		t.Errorf("dkim: %+v", d)
	}
	if rp := got["Return-Path"]; !strings.Contains(rp.Detail, "mine-rp.example.com") {
		t.Errorf("return-path: %+v", rp)
	}
}

type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("network down")
}

func TestGetJSONErrors(t *testing.T) {
	t.Parallel()
	var v struct{}
	o := Options{APIURL: "http://api.invalid", AccountToken: "a", HTTP: &http.Client{Transport: failingTransport{}}}
	if err := getJSON(t.Context(), o, "/domains", &v); err == nil || !strings.Contains(err.Error(), "calling the Postmark API: ") || !strings.Contains(err.Error(), "network down") {
		t.Errorf("transport error: %v", err)
	}
	o.APIURL = "http://bad host"
	if err := getJSON(t.Context(), o, "/domains", &v); err == nil {
		t.Error("invalid URL should fail")
	}
}
