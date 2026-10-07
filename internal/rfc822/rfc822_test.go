package rfc822

import (
	"encoding/base64"
	"strings"
	"testing"
)

func crlf(s string) []byte { return []byte(strings.ReplaceAll(s, "\n", "\r\n")) }

func TestSimpleText(t *testing.T) {
	m, err := Convert(crlf(`From: =?UTF-8?Q?J=C3=B6rg?= <jorg@example.com>
To: a@example.com, "B, Person" <b@example.com>
Subject: =?UTF-8?B?SGVsbG8g8J+Riw==?=
X-PM-Message-Stream: broadcast
X-PM-Tag: welcome
X-PM-Metadata-Order: 42
X-Custom: yes
DKIM-Signature: v=1; dropped
In-Reply-To: <abc@example.com>

Hello there.
`), Envelope{})
	if err != nil {
		t.Fatal(err)
	}
	if m.From != `"Jörg" <jorg@example.com>` {
		t.Errorf("From = %q", m.From)
	}
	if m.To != `a@example.com, "B, Person" <b@example.com>` {
		t.Errorf("To = %q", m.To)
	}
	if m.Subject != "Hello 👋" || m.MessageStream != "broadcast" || m.Tag != "welcome" || m.Metadata["order"] != "42" {
		t.Errorf("got %+v", m)
	}
	if m.TextBody != "Hello there.\r\n" {
		t.Errorf("TextBody = %q", m.TextBody)
	}
	names := map[string]bool{}
	for _, h := range m.Headers {
		names[h.Name] = true
	}
	if !names["X-Custom"] || !names["In-Reply-To"] || names["Dkim-Signature"] || names["X-Pm-Tag"] || len(names) != 2 {
		t.Errorf("Headers = %+v", m.Headers)
	}
}

func TestMultipartWithAttachment(t *testing.T) {
	pdf := base64.StdEncoding.EncodeToString([]byte("%PDF-1.4 fake"))
	m, err := Convert(crlf(`From: app@example.com
To: a@example.com
Subject: Receipt
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="outer"

--outer
Content-Type: multipart/alternative; boundary="inner"

--inner
Content-Type: text/plain; charset=utf-8
Content-Transfer-Encoding: quoted-printable

Caf=C3=A9 receipt
--inner
Content-Type: text/html; charset=iso-8859-1
Content-Transfer-Encoding: quoted-printable

<p>Caf=E9</p>
--inner--
--outer
Content-Type: image/png
Content-ID: <logo>
Content-Disposition: inline; filename="logo.png"
Content-Transfer-Encoding: base64

iVBORw0K
GgoAAAAN
--outer
Content-Type: application/pdf; name="receipt.pdf"
Content-Disposition: attachment
Content-Transfer-Encoding: base64

`+pdf+`
--outer--
`), Envelope{})
	if err != nil {
		t.Fatal(err)
	}
	if m.TextBody != "Café receipt" || m.HtmlBody != "<p>Café</p>" {
		t.Errorf("bodies: %q %q", m.TextBody, m.HtmlBody)
	}
	if len(m.Attachments) != 2 {
		t.Fatalf("attachments: %+v", m.Attachments)
	}
	logo, rec := m.Attachments[0], m.Attachments[1]
	if logo.Name != "logo.png" || logo.ContentID != "cid:logo" || logo.Content != "iVBORw0KGgoAAAAN" {
		t.Errorf("logo = %+v", logo)
	}
	if rec.Name != "receipt.pdf" || rec.ContentType != "application/pdf" || rec.Content != pdf {
		t.Errorf("receipt = %+v", rec)
	}
}

func TestEnvelopeRecipients(t *testing.T) {
	raw := crlf(`From: app@example.com
To: a@example.com, b@example.com
Cc: c@example.com
Bcc: leak@example.com

hi
`)
	m, err := Convert(raw, Envelope{From: "bounce@example.com", To: []string{"A@example.com", "c@example.com", "hidden@example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if m.To != "a@example.com" || m.Cc != "c@example.com" || m.Bcc != "hidden@example.com" {
		t.Errorf("To=%q Cc=%q Bcc=%q", m.To, m.Cc, m.Bcc)
	}

	// sendmail -t: no envelope recipients, Bcc header honoured.
	m, _ = Convert(raw, Envelope{})
	if m.Bcc != "leak@example.com" || m.To != "a@example.com, b@example.com" {
		t.Errorf("-t: To=%q Bcc=%q", m.To, m.Bcc)
	}
}

func TestEnvelopeFromFallback(t *testing.T) {
	m, err := Convert(crlf("Subject: cron\n\nhi\n"), Envelope{From: "cron@example.com", To: []string{"root@example.com"}})
	if err != nil || m.From != "cron@example.com" || m.To != "root@example.com" || m.Bcc != "" {
		t.Fatalf("From=%q To=%q Bcc=%q err=%v", m.From, m.To, m.Bcc, err)
	}
}

func TestBadAddress(t *testing.T) {
	if _, err := Convert(crlf("From: app@example.com\nTo: not an address\n\nhi\n"), Envelope{}); err == nil {
		t.Fatal("expected error")
	}
}
