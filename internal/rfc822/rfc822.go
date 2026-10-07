// Package rfc822 converts a raw internet message (what SMTP and sendmail
// carry) into a Postmark API message.
package rfc822

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strings"
	"unicode/utf8"
)

// Envelope is the SMTP envelope. Recipients in the envelope are
// authoritative: header recipients not in it are dropped, and envelope
// recipients missing from the headers become Bcc. An empty To means
// "take recipients from the headers" (sendmail -t).
type Envelope struct {
	From string
	To   []string
}

// Header is a custom message header.
type Header struct {
	Name  string `json:"Name"`
	Value string `json:"Value"`
}

// Attachment is a Postmark attachment.
type Attachment struct {
	Name        string `json:"Name"`
	Content     string `json:"Content"`
	ContentType string `json:"ContentType"`
	ContentID   string `json:"ContentID,omitempty"`
}

// Message is a Postmark /email request.
type Message struct {
	From          string            `json:"From,omitempty"`
	To            string            `json:"To,omitempty"`
	Cc            string            `json:"Cc,omitempty"`
	Bcc           string            `json:"Bcc,omitempty"`
	Subject       string            `json:"Subject,omitempty"`
	Tag           string            `json:"Tag,omitempty"`
	HtmlBody      string            `json:"HtmlBody,omitempty"`
	TextBody      string            `json:"TextBody,omitempty"`
	ReplyTo       string            `json:"ReplyTo,omitempty"`
	Headers       []Header          `json:"Headers,omitempty"`
	Metadata      map[string]string `json:"Metadata,omitempty"`
	Attachments   []Attachment      `json:"Attachments,omitempty"`
	MessageStream string            `json:"MessageStream,omitempty"`
}

// maxNesting bounds multipart recursion.
const maxNesting = 10

var wordDecoder = &mime.WordDecoder{CharsetReader: charsetReader}

// Convert parses raw and builds the equivalent Postmark message.
//
// Postmark's SMTP conventions are honoured: X-PM-Message-Stream selects the
// stream, X-PM-Tag sets the tag and X-PM-Metadata-<key> adds metadata.
func Convert(raw []byte, env Envelope) (*Message, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("parsing message: %w", err)
	}
	h := msg.Header
	m := &Message{
		Subject:       decodeHeader(h.Get("Subject")),
		Tag:           decodeHeader(h.Get("X-PM-Tag")),
		MessageStream: strings.TrimSpace(h.Get("X-PM-Message-Stream")),
	}

	from, err := addressList(h, "From")
	if err != nil {
		return nil, err
	}
	if len(from) > 0 {
		m.From = format(from[:1])
	} else if env.From != "" {
		m.From = env.From
	}
	if replyTo, _ := addressList(h, "Reply-To"); len(replyTo) > 0 {
		m.ReplyTo = format(replyTo)
	}

	to, err := addressList(h, "To")
	if err != nil {
		return nil, err
	}
	cc, err := addressList(h, "Cc")
	if err != nil {
		return nil, err
	}
	bcc, err := addressList(h, "Bcc")
	if err != nil {
		return nil, err
	}
	if len(env.To) > 0 {
		want := map[string]bool{}
		for _, r := range env.To {
			want[strings.ToLower(r)] = true
		}
		seen := map[string]bool{}
		keep := func(list []*mail.Address) []*mail.Address {
			var out []*mail.Address
			for _, a := range list {
				k := strings.ToLower(a.Address)
				if want[k] && !seen[k] {
					seen[k] = true
					out = append(out, a)
				}
			}
			return out
		}
		to, cc = keep(to), keep(cc)
		bcc = nil
		for _, r := range env.To {
			if !seen[strings.ToLower(r)] {
				seen[strings.ToLower(r)] = true
				bcc = append(bcc, &mail.Address{Address: r})
			}
		}
	}
	// Typical for cron and sendmail: no To/Cc header, one envelope recipient.
	// Postmark expects a To, and with a single recipient nothing is revealed.
	if len(to) == 0 && len(cc) == 0 && len(bcc) == 1 {
		to, bcc = bcc, nil
	}
	m.To, m.Cc, m.Bcc = format(to), format(cc), format(bcc)

	for name, values := range h {
		canon := textproto.CanonicalMIMEHeaderKey(name)
		if k, ok := strings.CutPrefix(canon, "X-Pm-Metadata-"); ok {
			if m.Metadata == nil {
				m.Metadata = map[string]string{}
			}
			m.Metadata[strings.ToLower(k)] = decodeHeader(values[0])
			continue
		}
		if forwardHeader(canon) {
			for _, v := range values {
				m.Headers = append(m.Headers, Header{Name: canon, Value: decodeHeader(v)})
			}
		}
	}

	if err := walk(m, textproto.MIMEHeader(h), msg.Body, 0); err != nil {
		return nil, err
	}
	return m, nil
}

// forwardHeader reports whether a header is passed to Postmark as a custom
// header. Structural headers are rebuilt by Postmark and DKIM signatures
// would not survive re-encoding, so only headers with meaning to the
// recipient are kept.
func forwardHeader(name string) bool {
	switch name {
	case "In-Reply-To", "References", "Auto-Submitted", "Precedence",
		"List-Unsubscribe", "List-Unsubscribe-Post", "List-Id", "List-Help":
		return true
	}
	return strings.HasPrefix(name, "X-") && !strings.HasPrefix(name, "X-Pm-")
}

func walk(m *Message, h textproto.MIMEHeader, body io.Reader, depth int) error {
	if depth > maxNesting {
		return errors.New("message nests multipart parts too deeply")
	}
	ctype, params, err := mime.ParseMediaType(h.Get("Content-Type"))
	if err != nil {
		ctype, params = "text/plain", map[string]string{"charset": "us-ascii"}
	}

	if strings.HasPrefix(ctype, "multipart/") {
		if params["boundary"] == "" {
			return errors.New("multipart message without boundary")
		}
		mr := multipart.NewReader(body, params["boundary"])
		for {
			p, err := mr.NextRawPart()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return fmt.Errorf("reading multipart: %w", err)
			}
			if err := walk(m, p.Header, p, depth+1); err != nil {
				return err
			}
		}
	}

	data, err := io.ReadAll(decodeTransfer(h.Get("Content-Transfer-Encoding"), body))
	if err != nil {
		return fmt.Errorf("decoding %s part: %w", ctype, err)
	}

	disposition, dparams, _ := mime.ParseMediaType(h.Get("Content-Disposition"))
	filename := decodeHeader(dparams["filename"])
	if filename == "" {
		filename = decodeHeader(params["name"])
	}
	inline := disposition != "attachment" && filename == ""

	switch {
	case inline && ctype == "text/plain" && m.TextBody == "":
		m.TextBody = toUTF8(data, params["charset"])
	case inline && ctype == "text/html" && m.HtmlBody == "":
		m.HtmlBody = toUTF8(data, params["charset"])
	default:
		if filename == "" {
			filename = fmt.Sprintf("attachment-%d%s", len(m.Attachments)+1, extension(ctype))
		}
		a := Attachment{Name: filename, ContentType: ctype, Content: base64.StdEncoding.EncodeToString(data)}
		if cid := strings.Trim(h.Get("Content-Id"), "<> "); cid != "" {
			a.ContentID = "cid:" + cid
		}
		m.Attachments = append(m.Attachments, a)
	}
	return nil
}

func decodeTransfer(enc string, r io.Reader) io.Reader {
	switch strings.ToLower(strings.TrimSpace(enc)) {
	case "base64":
		return base64.NewDecoder(base64.StdEncoding, &base64Cleaner{r: r})
	case "quoted-printable":
		return quotedprintable.NewReader(r)
	}
	return r
}

// base64Cleaner drops characters a base64 decoder rejects but mailers emit
// (whitespace, stray padding lines).
type base64Cleaner struct{ r io.Reader }

func (c *base64Cleaner) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	j := 0
	for _, b := range p[:n] {
		if b != ' ' && b != '\t' && b != '\r' && b != '\n' {
			p[j] = b
			j++
		}
	}
	return j, err
}

func addressList(h mail.Header, key string) ([]*mail.Address, error) {
	v := h.Get(key)
	if strings.TrimSpace(v) == "" || strings.Contains(v, "undisclosed-recipients") {
		return nil, nil
	}
	p := mail.AddressParser{WordDecoder: wordDecoder}
	list, err := p.ParseList(v)
	if err != nil {
		return nil, fmt.Errorf("invalid %s header %q: %w", key, v, err)
	}
	return list, nil
}

// format renders addresses for Postmark: `"Name" <addr>` without
// RFC 2047 encoding, which Postmark applies itself.
func format(list []*mail.Address) string {
	parts := make([]string, len(list))
	for i, a := range list {
		if a.Name == "" {
			parts[i] = a.Address
		} else {
			name := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(a.Name)
			parts[i] = `"` + name + `" <` + a.Address + `>`
		}
	}
	return strings.Join(parts, ", ")
}

func decodeHeader(s string) string {
	d, err := wordDecoder.DecodeHeader(s)
	if err != nil {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(d)
}

// toUTF8 converts the charsets common in generated mail. Unknown charsets
// are passed through unchanged.
func toUTF8(b []byte, charset string) string {
	switch strings.ToLower(charset) {
	case "iso-8859-1", "latin1", "l1", "windows-1252", "cp1252":
		if !utf8.Valid(b) {
			return latin1(b)
		}
	}
	return string(b)
}

func latin1(b []byte) string {
	r := make([]rune, len(b))
	for i, c := range b {
		r[i] = rune(c)
	}
	return string(r)
}

func charsetReader(charset string, input io.Reader) (io.Reader, error) {
	switch strings.ToLower(charset) {
	case "windows-1252", "cp1252", "latin1":
		b, err := io.ReadAll(input)
		if err != nil {
			return nil, err
		}
		return strings.NewReader(latin1(b)), nil
	}
	return nil, fmt.Errorf("unsupported charset %q", charset)
}

func extension(ctype string) string {
	if exts, _ := mime.ExtensionsByType(ctype); len(exts) > 0 {
		return exts[0]
	}
	if ctype == "message/rfc822" {
		return ".eml"
	}
	return ".bin"
}
