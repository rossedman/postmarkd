package main

import (
	"errors"
	"io"
	"net/mail"
	"reflect"
	"sort"
	"strings"
	"testing"
)

const plainMessage = "From: Ada <ada@example.com>\nTo: Bob <bob@example.com>\nCc: carol@example.com\nSubject: hi\n\nHello.\n"

// recipients returns every address in a request's To, Cc and Bcc, sorted.
func recipients(t *testing.T, r request) []string {
	t.Helper()
	var out []string
	for _, k := range []string{"To", "Cc", "Bcc"} {
		s, _ := r.body[k].(string)
		if s == "" {
			continue
		}
		list, err := mail.ParseAddressList(s)
		if err != nil {
			t.Fatalf("%s: %q: %v", k, s, err)
		}
		for _, a := range list {
			out = append(out, a.Address)
		}
	}
	sort.Strings(out)
	return out
}

func TestSendmailDelivers(t *testing.T) {
	cases := []struct {
		desc    string
		args    []string
		stdin   string
		to      []string
		from    string
		textHas string
	}{
		{"arguments are recipients", []string{"x@example.com", "y@example.com"}, plainMessage, []string{"x@example.com", "y@example.com"}, "ada@example.com", "Hello."},
		{"envelope recipients in headers keep their place", []string{"bob@example.com", "carol@example.com"}, plainMessage, []string{"bob@example.com", "carol@example.com"}, "ada@example.com", ""},
		{"-t reads headers", []string{"-t"}, plainMessage, []string{"bob@example.com", "carol@example.com"}, "ada@example.com", "Hello."},
		{"-t with arguments adds header recipients", []string{"-t", "x@example.com"}, plainMessage, []string{"bob@example.com", "carol@example.com", "x@example.com"}, "ada@example.com", ""},
		{"-- ends options", []string{"-i", "--", "-odd@example.com"}, plainMessage, []string{"-odd@example.com"}, "ada@example.com", ""},
		{"-f and -F without From header", []string{"-f", "root@example.com", "-F", `The "Box"`, "x@example.com"}, "Subject: cron\n\nout\n", []string{"x@example.com"}, `"The 'Box'" <root@example.com>`, "out"},
		{"-fADDR and -FNAME joined", []string{"-froot@example.com", "-FBox", "x@example.com"}, "Subject: cron\n\nout\n", []string{"x@example.com"}, `"Box" <root@example.com>`, ""},
		{"-r is -f", []string{"-r", "root@example.com", "x@example.com"}, "Subject: s\n\nb\n", []string{"x@example.com"}, "root@example.com", ""},
		{"-F ignored with From header", []string{"-F", "Ignored", "x@example.com"}, plainMessage, []string{"x@example.com"}, "ada@example.com", ""},
		{"ignored options", []string{"-oi", "-v", "-bm", "-odq", "-B", "8BITMIME", "-N", "never", "-OUseMSP=1", "-L", "tag", "x@example.com"}, plainMessage, []string{"x@example.com"}, "ada@example.com", ""},
		{"a lone dot ends the message", []string{"x@example.com"}, "Subject: s\n\nbefore\n.\nafter\n", []string{"x@example.com"}, "", "before"},
		{"-i keeps lone dots", []string{"-i", "x@example.com"}, "Subject: s\n\nbefore\n.\nafter\n", []string{"x@example.com"}, "", "after"},
	}
	for _, tc := range cases {
		d := newFakeDaemon(t, 200, okResponse)
		c, _, stderr := testCLI(tc.stdin)
		if code := c.sendmail(tc.args); code != 0 {
			t.Errorf("%s: exit %d: %s", tc.desc, code, stderr)
			continue
		}
		r := d.last(t)
		if got := recipients(t, r); !reflect.DeepEqual(got, tc.to) {
			t.Errorf("%s: recipients %q, want %q", tc.desc, got, tc.to)
		}
		// A bare address must match the From address; anything else the
		// whole header, display name included.
		from, _ := r.body["From"].(string)
		if a, err := mail.ParseAddress(from); err == nil && !strings.Contains(tc.from, "<") {
			from = a.Address
		}
		if tc.from != "" && from != tc.from {
			t.Errorf("%s: From = %q, want %q", tc.desc, from, tc.from)
		}
		if text, _ := r.body["TextBody"].(string); !strings.Contains(text, tc.textHas) {
			t.Errorf("%s: TextBody = %q, want it to contain %q", tc.desc, text, tc.textHas)
		}
		if tc.desc == "a lone dot ends the message" && strings.Contains(r.body["TextBody"].(string), "after") {
			t.Errorf("%s: read past the dot", tc.desc)
		}
	}
}

func TestSendmailStream(t *testing.T) {
	d := newFakeDaemon(t, 200, okResponse)
	t.Setenv("POSTMARKD_STREAM", "broadcast")
	t.Setenv("POSTMARKD_TOKEN", "tok")
	c, _, _ := testCLI(plainMessage)
	if code := c.sendmail([]string{"x@example.com"}); code != 0 {
		t.Fatal(code)
	}
	r := d.last(t)
	if r.header.Get("X-Postmarkd-Stream") != "broadcast" || r.header.Get("Authorization") != "Bearer tok" {
		t.Errorf("headers = %v", r.header)
	}
}

func TestSendmailStatus(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		code   int
		stderr string
	}{
		{200, okResponse, 0, ""},
		{200, `{"ErrorCode":406,"Message":"inactive"}`, exSoftware, "HTTP 200: inactive"},
		{401, `{"ErrorCode":10,"Message":"no"}`, exNoPerm, "set POSTMARKD_TOKEN"},
		{429, `{"Message":"slow down"}`, exTempFail, "temporary failure: slow down"},
		{503, "not json", exTempFail, "temporary failure"},
		{422, `{"ErrorCode":300,"Message":"bad To"}`, exDataErr, "message rejected: bad To"},
		{413, `{"Message":"too big"}`, exDataErr, "message rejected: too big"},
		{404, "", exUnavailable, ""},
		{418, `{"Message":"teapot"}`, exSoftware, "HTTP 418: teapot"},
	} {
		newFakeDaemon(t, tc.status, tc.body)
		c, _, stderr := testCLI(plainMessage)
		if code := c.sendmail([]string{"x@example.com"}); code != tc.code {
			t.Errorf("HTTP %d: exit %d, want %d", tc.status, code, tc.code)
		}
		if !strings.Contains(stderr.String(), tc.stderr) {
			t.Errorf("HTTP %d: stderr %q, want %q", tc.status, stderr, tc.stderr)
		}
	}
}

func TestSendmailErrors(t *testing.T) {
	newFakeDaemon(t, 200, okResponse)
	for _, tc := range []struct {
		desc   string
		args   []string
		stdin  io.Reader
		code   int
		stderr string
	}{
		{"queue listing", []string{"-bp"}, strings.NewReader(""), exUsage, "-bp is not supported"},
		{"queue run", []string{"-q30m"}, strings.NewReader(""), exUsage, "-q30m is not supported"},
		{"no recipients", nil, strings.NewReader(plainMessage), exUsage, "no recipients"},
		{"unparseable message", []string{"x@example.com"}, strings.NewReader("From: <<bad\n\nbody\n"), exDataErr, "sendmail (postmarkd):"},
		{"unreadable stdin", []string{"x@example.com"}, iotest{}, exDataErr, "reading message: boom"},
	} {
		c, _, stderr := testCLI("")
		c.stdin = tc.stdin
		if code := c.sendmail(tc.args); code != tc.code {
			t.Errorf("%s: exit %d, want %d (%s)", tc.desc, code, tc.code, stderr)
		}
		if !strings.Contains(stderr.String(), tc.stderr) {
			t.Errorf("%s: stderr %q, want %q", tc.desc, stderr, tc.stderr)
		}
	}
}

func TestSendmailUnreachable(t *testing.T) {
	unreachable(t)
	c, _, stderr := testCLI(plainMessage)
	if code := c.sendmail([]string{"x@example.com"}); code != exTempFail {
		t.Errorf("exit %d, want %d", code, exTempFail)
	}
	if !strings.Contains(stderr.String(), "cannot reach postmarkd") {
		t.Errorf("stderr = %q", stderr)
	}
}

// iotest is a reader that always fails.
type iotest struct{}

// Read fails.
func (iotest) Read([]byte) (int, error) { return 0, errors.New("boom") }

func TestReadMessage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in         string
		ignoreDots bool
		want       string
	}{
		{"a\nb\n", false, "a\nb\n"},
		{"a\nb", false, "a\nb"},
		{"a\n.\nb\n", false, "a\n"},
		{"a\r\n.\r\nb\r\n", false, "a\r\n"},
		{"a\n..\nb\n", false, "a\n..\nb\n"},
		{".", false, ""},
		{"a\n.\nb\n", true, "a\n.\nb\n"},
		{"", false, ""},
	} {
		got, err := readMessage(strings.NewReader(tc.in), tc.ignoreDots)
		if err != nil || string(got) != tc.want {
			t.Errorf("readMessage(%q, %v) = %q, %v; want %q", tc.in, tc.ignoreDots, got, err, tc.want)
		}
	}
	if _, err := readMessage(iotest{}, false); err == nil {
		t.Error("read error: want error")
	}
}
