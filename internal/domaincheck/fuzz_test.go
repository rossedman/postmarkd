package domaincheck

import (
	"strings"
	"testing"
)

// FuzzTag checks the DMARC tag=value parser: values are lowercase and
// trimmed, and a tag that is found really is present in the record.
func FuzzTag(f *testing.F) {
	for _, s := range [][2]string{
		{"v=DMARC1; p=reject; rua=mailto:a@b", "p"},
		{"v=DMARC1;P = Quarantine ;", "p"},
		{"p", "p"},
		{";;=;", ""},
		{"rua=mailto:A@B", "RUA"},
	} {
		f.Add(s[0], s[1])
	}
	f.Fuzz(func(t *testing.T, record, name string) {
		got := tag(record, name)
		if got != tag(record, name) {
			t.Fatal("not deterministic")
		}
		if got != strings.ToLower(got) || got != strings.TrimSpace(got) {
			t.Fatalf("tag(%q, %q) = %q is not lowercase and trimmed", record, name, got)
		}
		if strings.Contains(got, ";") {
			t.Fatalf("tag(%q, %q) = %q spans a separator", record, name, got)
		}
		if got != "" && !strings.Contains(strings.ToLower(record), got) {
			t.Fatalf("tag(%q, %q) = %q is not in the record", record, name, got)
		}
	})
}

// FuzzFQDN checks that a DNS host from the Postmark API is qualified with
// the (normalized) domain exactly once.
func FuzzFQDN(f *testing.F) {
	for _, s := range [][2]string{
		{"sel._domainkey", "example.com"},
		{"sel._domainkey.example.com.", "example.com"},
		{"x.EXAMPLE.COM", "example.com"},
		{"", ""},
	} {
		f.Add(s[0], s[1])
	}
	f.Fuzz(func(t *testing.T, host, domain string) {
		// Run normalizes the domain this way before any check.
		domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
		if domain == "" || domain != strings.ToLower(domain) || strings.HasSuffix(domain, ".") {
			return // Run never checks an empty domain
		}
		got := fqdn(host, domain)
		if !strings.HasSuffix(strings.ToLower(got), "."+domain) {
			t.Fatalf("fqdn(%q, %q) = %q does not end in .%s", host, domain, got, domain)
		}
		if again := fqdn(got, domain); again != got {
			t.Fatalf("fqdn not idempotent: %q -> %q", got, again)
		}
	})
}

// FuzzRecordChecks runs the DMARC and SPF evaluation over arbitrary TXT
// records and checks that verdicts are consistent with the records.
func FuzzRecordChecks(f *testing.F) {
	for _, s := range [][2]string{
		{"v=DMARC1; p=reject; rua=mailto:a@b", "v=spf1 -all"},
		{"v=DMARC1; p=none", "v=spf1 a\x00v=spf1 b"},
		{"v=DMARC1; p=quarantine; rua=x\x00 v=dmarc1; p=reject", ""},
		{"", ""},
		{"V=DMARC1;P=REJECT;RUA=M", "V=SPF1"},
	} {
		f.Add(s[0], s[1])
	}
	f.Fuzz(func(t *testing.T, dmarc, spf string) {
		dns := fakeDNS{txt: map[string][]string{
			"_dmarc.example.com": strings.Split(dmarc, "\x00"),
			"example.com":        strings.Split(spf, "\x00"),
		}}
		o := Options{Domain: "example.com", Resolver: dns}

		d := checkDMARC(t.Context(), o)
		_ = d.Status.String() // must be in range
		var dmarcRecords []string
		for _, r := range dns.txt["_dmarc.example.com"] {
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(r)), "v=dmarc1") {
				dmarcRecords = append(dmarcRecords, r)
			}
		}
		if d.Status == Pass {
			if len(dmarcRecords) != 1 {
				t.Fatalf("DMARC passed with %d records", len(dmarcRecords))
			}
			p := tag(dmarcRecords[0], "p")
			if p != "reject" && p != "quarantine" || tag(dmarcRecords[0], "rua") == "" {
				t.Fatalf("DMARC passed for %q", dmarcRecords[0])
			}
		}
		if len(dmarcRecords) != 1 && d.Status != Fail {
			t.Fatalf("DMARC with %d records is %v", len(dmarcRecords), d.Status)
		}

		s := checkSPF(t.Context(), o)
		_ = s.Status.String()
		n := 0
		for _, r := range dns.txt["example.com"] {
			if strings.HasPrefix(strings.ToLower(r), "v=spf1") {
				n++
			}
		}
		want := Info
		switch {
		case n == 1:
			want = Pass
		case n > 1:
			want = Fail
		}
		if s.Status != want {
			t.Fatalf("SPF with %d records is %v, want %v", n, s.Status, want)
		}
	})
}
