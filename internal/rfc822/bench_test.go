package rfc822

import (
	"encoding/base64"
	"strings"
	"testing"
)

func BenchmarkConvert(b *testing.B) {
	text := crlf(`From: "Cron Daemon" <cron@example.com>
To: ops@example.com
Subject: Cron <root@web1> /usr/local/bin/backup

Backup completed in 42s. 1,234 files, 5.6 GB.
`)
	pdf := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("%PDF-1.4 receipt ", 6000))) // ~100 KB
	var wrapped strings.Builder
	for i := 0; i < len(pdf); i += 76 {
		wrapped.WriteString(pdf[i:min(i+76, len(pdf))] + "\n")
	}
	multipart := crlf(`From: Acme <noreply@example.com>
To: ada@example.com
Subject: =?UTF-8?Q?Your_receipt_=E2=9C=94?=
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="outer"

--outer
Content-Type: multipart/alternative; boundary="inner"

--inner
Content-Type: text/plain; charset=utf-8
Content-Transfer-Encoding: quoted-printable

Thanks for your order. Your receipt is attached.
--inner
Content-Type: text/html; charset=utf-8
Content-Transfer-Encoding: quoted-printable

<p>Thanks for your order. Your receipt is attached.</p>
--inner--
--outer
Content-Type: application/pdf; name="receipt.pdf"
Content-Disposition: attachment; filename="receipt.pdf"
Content-Transfer-Encoding: base64

` + wrapped.String() + `--outer--
`)
	for _, tc := range []struct {
		name string
		raw  []byte
	}{{"PlainText", text}, {"MultipartWith100KBAttachment", multipart}} {
		b.Run(tc.name, func(b *testing.B) {
			b.SetBytes(int64(len(tc.raw)))
			b.ReportAllocs()
			for range b.N {
				if _, err := Convert(tc.raw, Envelope{}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
