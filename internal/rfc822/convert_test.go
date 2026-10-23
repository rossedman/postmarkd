package rfc822

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestConvertErrors(t *testing.T) {
	t.Parallel()
	nested := "Content-Type: text/plain\n\nbottom\n"
	for i := range maxNesting + 1 {
		b := fmt.Sprintf("b%d", i)
		nested = "Content-Type: multipart/mixed; boundary=" + b + "\n\n--" + b + "\n" + nested + "--" + b + "--\n"
	}
	for _, tc := range []struct {
		name, raw, want string
	}{
		{"malformed header", "From app@example.com\n\nhi\n", "parsing message"},
		{"bad From", "From: <<<\n\nhi\n", "invalid From header"},
		{"bad To", "To: not an address\n\nhi\n", "invalid To header"},
		{"bad Cc", "Cc: a@example.com, ,,<\n\nhi\n", "invalid Cc header"},
		{"bad Bcc", "Bcc: @@\n\nhi\n", "invalid Bcc header"},
		{"multipart without boundary", "Content-Type: multipart/mixed\n\nhi\n", "without boundary"},
		{"nesting too deep", "MIME-Version: 1.0\n" + nested, "nests multipart parts too deeply"},
		{"malformed multipart", "Content-Type: multipart/mixed; boundary=x\n\n--x\nbroken header line\n\nbody\n--x--\n", "reading multipart"},
		{"truncated part", "Content-Type: multipart/mixed; boundary=x\n\n--x\nContent-Type: text/plain\n\nno closing boundary", "reading text/plain part"},
		{"bad base64 text", "Content-Transfer-Encoding: base64\n\n!!!notbase64!!!\n", "decoding text/plain part"},
		{"bad base64 html", "Content-Type: text/html\nContent-Transfer-Encoding: base64\n\n****\n", "decoding text/html part"},
		{"bad base64 attachment", "Content-Type: application/pdf; name=a.pdf\nContent-Transfer-Encoding: base64\n\n@@@@\n", `decoding attachment "a.pdf"`},
		{"unpadded base64 attachment", "Content-Type: application/pdf; name=a.pdf\nContent-Transfer-Encoding: base64\n\nAAECAw\n", `decoding attachment "a.pdf"`},
		{"bad quoted-printable", "Content-Transfer-Encoding: quoted-printable\n\nbad \x01 byte\n", "decoding text/plain part"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m, err := Convert(crlf(tc.raw), Envelope{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
			if m != nil {
				t.Errorf("message = %+v, want nil on error", m)
			}
		})
	}
}

func TestNestingLimit(t *testing.T) {
	t.Parallel()
	// maxNesting levels of multipart are accepted; one more is not (see
	// TestConvertErrors).
	raw := "Content-Type: text/plain\n\nbottom\n"
	for i := range maxNesting {
		b := fmt.Sprintf("b%d", i)
		raw = "Content-Type: multipart/mixed; boundary=" + b + "\n\n--" + b + "\n" + raw + "--" + b + "--\n"
	}
	m, err := Convert(crlf(raw), Envelope{})
	if err != nil {
		t.Fatal(err)
	}
	if m.TextBody != "bottom" {
		t.Errorf("TextBody = %q", m.TextBody)
	}
}

func TestBodies(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, raw  string
		text, html string
		atts       []Attachment
	}{
		{
			name: "no content type is us-ascii text",
			raw:  "Subject: x\n\nplain\n",
			text: "plain\r\n",
		},
		{
			name: "unparseable content type falls back to text",
			raw:  "Content-Type: ;;;\n\nfallback\n",
			text: "fallback\r\n",
		},
		{
			name: "base64 text with line breaks",
			raw:  "Content-Type: text/plain; charset=utf-8\nContent-Transfer-Encoding: base64\n\n" + "SGVs\n bG8g\n\tV29y bGQ=\n",
			text: "Hello World",
		},
		{
			name: "transfer encoding is case and space insensitive",
			raw:  "Content-Transfer-Encoding:  BASE64 \n\nSGk=\n",
			text: "Hi",
		},
		{
			name: "latin1 text converted to utf-8",
			raw:  "Content-Type: text/plain; charset=ISO-8859-1\nContent-Transfer-Encoding: quoted-printable\n\nna=EFve\n",
			text: "naïve\r\n",
		},
		{
			name: "windows-1252 already utf-8 is kept",
			raw:  "Content-Type: text/plain; charset=windows-1252\n\ncafé\n",
			text: "café\r\n",
		},
		{
			name: "unknown charset passes through",
			raw:  "Content-Type: text/plain; charset=koi8-r\nContent-Transfer-Encoding: quoted-printable\n\n=E9\n",
			text: "\xe9\r\n",
		},
		{
			name: "html only",
			raw:  "Content-Type: text/html; charset=utf-8\n\n<b>hi</b>",
			html: "<b>hi</b>",
		},
		{
			name: "second text part becomes attachment",
			raw: `Content-Type: multipart/mixed; boundary=x

--x
Content-Type: text/plain

first
--x
Content-Type: text/plain

second
--x
Content-Type: text/html

<p>one</p>
--x
Content-Type: text/html

<p>two</p>
--x--
`,
			text: "first",
			html: "<p>one</p>",
			atts: []Attachment{
				{Name: "attachment-1" + extension("text/plain"), ContentType: "text/plain", Content: b64("second")},
				{Name: "attachment-2" + extension("text/html"), ContentType: "text/html", Content: b64("<p>two</p>")},
			},
		},
		{
			name: "text with filename is an attachment",
			raw: `Content-Type: multipart/mixed; boundary=x

--x
Content-Type: text/plain; name="notes.txt"
Content-Transfer-Encoding: quoted-printable

caf=C3=A9
--x
Content-Type: text/csv
Content-Disposition: attachment

a,b
--x--
`,
			atts: []Attachment{
				{Name: "notes.txt", ContentType: "text/plain", Content: b64("café")},
				{Name: "attachment-2" + extension("text/csv"), ContentType: "text/csv", Content: b64("a,b")},
			},
		},
		{
			name: "encoded filenames and generated names",
			raw: `Content-Type: multipart/mixed; boundary=x

--x
Content-Type: application/octet-stream
Content-Disposition: attachment; filename="=?UTF-8?Q?r=C3=A9sum=C3=A9.pdf?="
Content-Transfer-Encoding: base64

AAEC
--x
Content-Type: application/x-postmarkd-unknown
Content-Transfer-Encoding: base64

AAEC AwQF
--x
Content-Type: message/rfc822

Subject: inner

body
--x
Content-Type: image/png
Content-Id: < logo@x >
Content-Transfer-Encoding: base64

iVBO
Rw0K
--x--
`,
			atts: []Attachment{
				{Name: "résumé.pdf", ContentType: "application/octet-stream", Content: "AAEC"},
				{Name: "attachment-2.bin", ContentType: "application/x-postmarkd-unknown", Content: "AAECAwQF"},
				{Name: "attachment-3.eml", ContentType: "message/rfc822", Content: b64("Subject: inner\r\n\r\nbody")},
				{Name: "attachment-4.png", ContentType: "image/png", Content: "iVBORw0K", ContentID: "cid:logo@x"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m, err := Convert(crlf(tc.raw), Envelope{})
			if err != nil {
				t.Fatal(err)
			}
			if m.TextBody != tc.text {
				t.Errorf("TextBody = %q, want %q", m.TextBody, tc.text)
			}
			if m.HtmlBody != tc.html {
				t.Errorf("HtmlBody = %q, want %q", m.HtmlBody, tc.html)
			}
			if len(m.Attachments) != len(tc.atts) {
				t.Fatalf("Attachments = %+v, want %+v", m.Attachments, tc.atts)
			}
			for i, a := range m.Attachments {
				if a != tc.atts[i] {
					t.Errorf("Attachments[%d] = %+v, want %+v", i, a, tc.atts[i])
				}
			}
		})
	}
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestLargeBase64Attachment(t *testing.T) {
	t.Parallel()
	// Larger than validBase64's 4 KiB chunk, so validation spans chunks.
	payload := bytes.Repeat([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}, 1000)
	enc := base64.StdEncoding.EncodeToString(payload)
	var wrapped strings.Builder
	for i := 0; i < len(enc); i += 76 {
		wrapped.WriteString(enc[i:min(i+76, len(enc))] + "\n")
	}
	raw := "Content-Type: application/octet-stream; name=blob\nContent-Transfer-Encoding: base64\n\n" + wrapped.String()
	m, err := Convert(crlf(raw), Envelope{})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Attachments) != 1 || m.Attachments[0].Content != enc {
		t.Fatalf("attachment content not passed through unchanged")
	}

	// Corruption past the first chunk must still be caught.
	bad := []byte(enc)
	bad[6000] = '*'
	raw = "Content-Type: application/octet-stream; name=blob\nContent-Transfer-Encoding: base64\n\n" + string(bad) + "\n"
	if _, err := Convert(crlf(raw), Envelope{}); err == nil {
		t.Fatal("corrupt base64 beyond first chunk accepted")
	}
}

func TestValidBase64(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"", true},
		{"AAEC", true},
		{"AA==", true},
		{"AAE", false},
		{"AA=A", false},
		{"****", false},
		{strings.Repeat("QUJD", 2048), true},
		{strings.Repeat("QUJD", 1024) + "QU*D", false},
	} {
		if got := validBase64([]byte(tc.in)); got != tc.want {
			t.Errorf("validBase64(%.20q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestHeaders(t *testing.T) {
	t.Parallel()
	m, err := Convert(crlf(`From: "Ops \"Team\" \\ Bot" <ops@example.com>, second@example.com
Reply-To: help@example.com, "Desk" <desk@example.com>
To: undisclosed-recipients:;
Subject: =?windows-1252?Q?Caf=E9?=
X-PM-Tag:   =?UTF-8?B?w6k=?=
X-PM-Message-Stream:   outbound
X-PM-Metadata-Customer-ID: =?UTF-8?Q?=C3=A9?=
x-pm-metadata-plan: pro
X-Mailer: =?unknown-charset?Q?abc?=
List-Unsubscribe: <mailto:u@example.com>
References: <a@x> <b@x>
Precedence: bulk
Received: from somewhere
Message-ID: <id@x>

hi
`), Envelope{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ field, got, want string }{
		{"From", m.From, `"Ops \"Team\" \\ Bot" <ops@example.com>`},
		{"ReplyTo", m.ReplyTo, `help@example.com, "Desk" <desk@example.com>`},
		{"To", m.To, ""},
		{"Subject", m.Subject, "Café"},
		{"Tag", m.Tag, "é"},
		{"MessageStream", m.MessageStream, "outbound"},
		{"Metadata[customer-id]", m.Metadata["customer-id"], "é"},
		{"Metadata[plan]", m.Metadata["plan"], "pro"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.field, c.got, c.want)
		}
	}
	if len(m.Metadata) != 2 {
		t.Errorf("Metadata = %v", m.Metadata)
	}
	got := map[string]string{}
	for _, h := range m.Headers {
		got[h.Name] = h.Value
	}
	want := map[string]string{
		"X-Mailer":         "=?unknown-charset?Q?abc?=", // undecodable: kept as is
		"List-Unsubscribe": "<mailto:u@example.com>",
		"References":       "<a@x> <b@x>",
		"Precedence":       "bulk",
	}
	if len(got) != len(want) {
		t.Errorf("Headers = %+v, want %v", m.Headers, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("header %s = %q, want %q", k, got[k], v)
		}
	}
}

func TestRepeatedForwardedHeader(t *testing.T) {
	t.Parallel()
	m, err := Convert(crlf("X-Trace: one\nX-Trace: two\n\nhi\n"), Envelope{})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Headers) != 2 || m.Headers[0] != (Header{"X-Trace", "one"}) || m.Headers[1] != (Header{"X-Trace", "two"}) {
		t.Errorf("Headers = %+v", m.Headers)
	}
}

func TestForwardHeader(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{
		"In-Reply-To": true, "References": true, "Auto-Submitted": true, "Precedence": true,
		"List-Unsubscribe": true, "List-Unsubscribe-Post": true, "List-Id": true, "List-Help": true,
		"X-Custom": true, "X-": true,
		"X-Pm-Tag": false, "X-Pm-Anything": false, "Dkim-Signature": false, "Received": false,
		"Subject": false, "Content-Type": false, "List-Archive": false,
	} {
		if got := forwardHeader(name); got != want {
			t.Errorf("forwardHeader(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestRecipients(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		headers     string
		env         Envelope
		to, cc, bcc string
		from        string
	}{
		{
			name:    "headers only",
			headers: "From: a@example.com\nTo: b@example.com\nCc: c@example.com\nBcc: d@example.com\n",
			from:    "a@example.com", to: "b@example.com", cc: "c@example.com", bcc: "d@example.com",
		},
		{
			name:    "header From wins over envelope",
			headers: "From: A <a@example.com>\nTo: b@example.com\n",
			env:     Envelope{From: "bounce@example.com"},
			from:    `"A" <a@example.com>`, to: "b@example.com",
		},
		{
			name:    "duplicates across headers are kept once",
			headers: "To: b@example.com, B@EXAMPLE.com\nCc: b@example.com, c@example.com\n",
			env:     Envelope{To: []string{"b@example.com", "c@example.com"}},
			to:      "b@example.com", cc: "c@example.com",
		},
		{
			name:    "duplicate envelope recipients become one Bcc",
			headers: "To: b@example.com\n",
			env:     Envelope{To: []string{"b@example.com", "x@example.com", "X@example.com", "y@example.com"}},
			to:      "b@example.com", bcc: "x@example.com, y@example.com",
		},
		{
			name:    "single envelope recipient promoted to To",
			headers: "To: undisclosed-recipients:;\n",
			env:     Envelope{To: []string{"x@example.com"}},
			to:      "x@example.com",
		},
		{
			name:    "multiple hidden recipients stay Bcc",
			headers: "Subject: x\n",
			env:     Envelope{To: []string{"x@example.com", "y@example.com"}},
			bcc:     "x@example.com, y@example.com",
		},
		{
			name:    "single Bcc header without To promoted",
			headers: "Bcc: only@example.com\n",
			to:      "only@example.com",
		},
		{
			name:    "names are kept",
			headers: "To: \"Doe, Jane\" <j@example.com>, =?UTF-8?Q?Z=C3=B6e?= <z@example.com>\n",
			env:     Envelope{To: []string{"z@example.com", "J@example.com"}},
			to:      `"Doe, Jane" <j@example.com>, "Zöe" <z@example.com>`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m, err := Convert(crlf(tc.headers+"\nbody\n"), tc.env)
			if err != nil {
				t.Fatal(err)
			}
			if m.From != tc.from || m.To != tc.to || m.Cc != tc.cc || m.Bcc != tc.bcc {
				t.Errorf("From=%q To=%q Cc=%q Bcc=%q\nwant From=%q To=%q Cc=%q Bcc=%q",
					m.From, m.To, m.Cc, m.Bcc, tc.from, tc.to, tc.cc, tc.bcc)
			}
		})
	}
}

func TestBadReplyToIgnored(t *testing.T) {
	t.Parallel()
	m, err := Convert(crlf("From: a@example.com\nReply-To: <<<\n\nhi\n"), Envelope{})
	if err != nil {
		t.Fatal(err)
	}
	if m.ReplyTo != "" {
		t.Errorf("ReplyTo = %q, want empty", m.ReplyTo)
	}
}

func TestConvertDoesNotModifyInput(t *testing.T) {
	t.Parallel()
	raw := crlf("Content-Type: application/pdf; name=a.pdf\nContent-Transfer-Encoding: base64\n\nAA\n EC\n")
	orig := bytes.Clone(raw)
	m, err := Convert(raw, Envelope{})
	if err != nil {
		t.Fatal(err)
	}
	if m.Attachments[0].Content != "AAEC" {
		t.Errorf("Content = %q", m.Attachments[0].Content)
	}
	if !bytes.Equal(raw, orig) {
		t.Errorf("input modified: %q", raw)
	}
}

func TestCharsetReader(t *testing.T) {
	t.Parallel()
	for _, cs := range []string{"windows-1252", "CP1252", "Latin1"} {
		r, err := charsetReader(cs, strings.NewReader("caf\xe9"))
		if err != nil {
			t.Fatalf("%s: %v", cs, err)
		}
		b, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != "café" {
			t.Errorf("%s: %q", cs, b)
		}
	}
	if _, err := charsetReader("ebcdic", strings.NewReader("x")); err == nil || !strings.Contains(err.Error(), `"ebcdic"`) {
		t.Errorf("unsupported charset: err = %v", err)
	}
	if _, err := charsetReader("latin1", errReader{}); err == nil {
		t.Error("read error not returned")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, fmt.Errorf("boom") }

func TestExtension(t *testing.T) {
	t.Parallel()
	for ctype, want := range map[string]string{
		"image/png":                    ".png",
		"message/rfc822":               ".eml",
		"application/x-postmarkd-none": ".bin",
		"":                             ".bin",
	} {
		if got := extension(ctype); got != want {
			t.Errorf("extension(%q) = %q, want %q", ctype, got, want)
		}
	}
}

func TestDecodeHeader(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"  plain  ":                   "plain",
		"=?UTF-8?B?SGk=?=":            "Hi",
		"=?ISO-8859-1?Q?caf=E9?=":     "café",
		"=?cp1252?Q?=80?=":            "\u0080",
		" =?x-unknown?Q?keep?= ":      "=?x-unknown?Q?keep?=",
		"=?UTF-8?Q?a?= =?UTF-8?Q?b?=": "ab",
	} {
		if got := decodeHeader(in); got != want {
			t.Errorf("decodeHeader(%q) = %q, want %q", in, got, want)
		}
	}
}
