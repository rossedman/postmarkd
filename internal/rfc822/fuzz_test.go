package rfc822

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/mail"
	"strings"
	"testing"
)

// fuzzEnvelopes are the envelopes FuzzConvert chooses between: none
// (sendmail -t), a sender only, and an authoritative recipient list.
var fuzzEnvelopes = []Envelope{
	{},
	{From: "bounce@example.com"},
	{From: "bounce@example.com", To: []string{"a@example.com", "B@example.com", "c@example.com"}},
}

func FuzzConvert(f *testing.F) {
	pdf := base64.StdEncoding.EncodeToString([]byte("%PDF-1.4 fake"))
	for _, seed := range []string{
		// Plain cron-style message.
		"From: \"Cron Daemon\" <cron@example.com>\nTo: a@example.com\nSubject: Cron <root@web1> backup\n\nBackup completed.\n",
		// Encoded headers, Postmark conventions and forwarded headers.
		"From: =?UTF-8?Q?J=C3=B6rg?= <jorg@example.com>\nTo: a@example.com, \"B, Person\" <b@example.com>\nCc: c@example.com\nBcc: d@example.com\n" +
			"Reply-To: help@example.com\nSubject: =?UTF-8?B?SGVsbG8g8J+Riw==?=\nX-PM-Message-Stream: broadcast\nX-PM-Tag: welcome\n" +
			"X-PM-Metadata-Order: 42\nX-Custom: yes\nDKIM-Signature: v=1\nIn-Reply-To: <abc@example.com>\n\nHello there.\n",
		// Multipart with alternative bodies, inline image and attachment.
		"From: app@example.com\nTo: a@example.com\nMIME-Version: 1.0\nContent-Type: multipart/mixed; boundary=\"outer\"\n\n" +
			"--outer\nContent-Type: multipart/alternative; boundary=\"inner\"\n\n" +
			"--inner\nContent-Type: text/plain; charset=utf-8\nContent-Transfer-Encoding: quoted-printable\n\nCaf=C3=A9 receipt\n" +
			"--inner\nContent-Type: text/html; charset=iso-8859-1\nContent-Transfer-Encoding: quoted-printable\n\n<p>Caf=E9</p>\n--inner--\n" +
			"--outer\nContent-Type: image/png\nContent-ID: <logo>\nContent-Disposition: inline; filename=\"logo.png\"\nContent-Transfer-Encoding: base64\n\niVBORw0K\nGgoAAAAN\n" +
			"--outer\nContent-Type: application/pdf; name=\"receipt.pdf\"\nContent-Disposition: attachment\nContent-Transfer-Encoding: base64\n\n" + pdf + "\n--outer--\n",
		// Encoded attachment filename, message/rfc822 part, windows-1252 subject.
		"Subject: =?windows-1252?Q?Caf=E9?=\nContent-Type: multipart/mixed; boundary=x\n\n" +
			"--x\nContent-Type: application/octet-stream\nContent-Disposition: attachment; filename=\"=?UTF-8?Q?r=C3=A9sum=C3=A9.pdf?=\"\n\nraw bytes\n" +
			"--x\nContent-Type: message/rfc822\n\nSubject: inner\n\nbody\n--x--\n",
		// Malformed: no boundary, bad base64, broken headers, truncated part.
		"Content-Type: multipart/mixed\n\nhi\n",
		"Content-Transfer-Encoding: base64\n\n!!!notbase64!!!\n",
		"From app@example.com\n\nhi\n",
		"To: not an address\n\nhi\n",
		"Content-Type: multipart/mixed; boundary=x\n\n--x\nContent-Type: text/plain\n\nno closing boundary",
		"To: undisclosed-recipients:;\nContent-Type: ;;;\n\nfallback\n",
		"",
	} {
		for mode := range len(fuzzEnvelopes) {
			f.Add([]byte(seed), uint8(mode))
			f.Add(crlf(seed), uint8(mode))
		}
	}

	f.Fuzz(func(t *testing.T, raw []byte, mode uint8) {
		env := fuzzEnvelopes[int(mode)%len(fuzzEnvelopes)]
		orig := bytes.Clone(raw)
		m, err := Convert(raw, env)
		if !bytes.Equal(raw, orig) {
			t.Fatal("Convert modified its input")
		}
		if err != nil {
			if m != nil {
				t.Fatalf("Convert returned both a message and error %v", err)
			}
			return
		}
		if m == nil {
			t.Fatal("Convert returned nil message and nil error")
		}

		// The result must serialise to valid JSON, and re-encoding what
		// Postmark would decode must be stable.
		js, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !json.Valid(js) {
			t.Fatalf("invalid JSON: %s", js)
		}
		var back Message
		if err := json.Unmarshal(js, &back); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		js2, err := json.Marshal(&back)
		if err != nil {
			t.Fatalf("re-marshal: %v", err)
		}
		if !bytes.Equal(js, js2) {
			t.Fatalf("JSON round trip unstable:\n%s\n%s", js, js2)
		}

		for _, s := range []struct{ name, v string }{
			{"Subject", m.Subject}, {"Tag", m.Tag}, {"MessageStream", m.MessageStream},
		} {
			if s.v != strings.TrimSpace(s.v) {
				t.Errorf("%s not trimmed: %q", s.name, s.v)
			}
		}
		for k := range m.Metadata {
			if k != strings.ToLower(k) {
				t.Errorf("metadata key %q not lower case", k)
			}
		}
		for _, h := range m.Headers {
			if !forwardHeader(h.Name) {
				t.Errorf("forwarded header %q that should be dropped", h.Name)
			}
		}
		for i, a := range m.Attachments {
			if a.Name == "" || a.ContentType == "" {
				t.Errorf("attachment %d missing name or type: %+v", i, a)
			}
			if _, err := base64.StdEncoding.DecodeString(a.Content); err != nil {
				t.Errorf("attachment %d content is not base64: %v", i, err)
			}
			if a.ContentID != "" && !strings.HasPrefix(a.ContentID, "cid:") {
				t.Errorf("attachment %d ContentID = %q", i, a.ContentID)
			}
		}

		if env.From != "" && m.From == "" {
			t.Errorf("From empty despite envelope sender %q", env.From)
		}
		if len(env.To) > 0 {
			checkEnvelopeRecipients(t, m, env)
		}
	})
}

// checkEnvelopeRecipients asserts that with an authoritative envelope every
// envelope recipient is addressed exactly once and nobody else is.
func checkEnvelopeRecipients(t *testing.T, m *Message, env Envelope) {
	t.Helper()
	want := map[string]bool{}
	for _, r := range env.To {
		want[strings.ToLower(r)] = true
	}
	got := map[string]int{}
	for _, field := range []string{m.To, m.Cc, m.Bcc} {
		if field == "" {
			continue
		}
		list, err := mail.ParseAddressList(field)
		if err != nil {
			// Display names taken verbatim from the input are not always
			// re-parseable; the recipient set is still checked on every
			// input whose output parses.
			return
		}
		for _, a := range list {
			got[strings.ToLower(a.Address)]++
		}
	}
	for r := range want {
		if got[r] != 1 {
			t.Errorf("envelope recipient %s addressed %d times (To=%q Cc=%q Bcc=%q)", r, got[r], m.To, m.Cc, m.Bcc)
		}
	}
	for r := range got {
		if !want[r] {
			t.Errorf("recipient %s not in envelope (To=%q Cc=%q Bcc=%q)", r, m.To, m.Cc, m.Bcc)
		}
	}
}
