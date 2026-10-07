package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/rossedman/postmarkd/internal/client"
)

// multi is a repeatable string flag.
type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func send(args []string) int {
	fl := flag.NewFlagSet("send", flag.ExitOnError)
	addr := fl.String("addr", "", "postmarkd address: socket path, host:port or URL (default $POSTMARKD_ADDR or "+client.DefaultSocket+")")
	token := fl.String("token", os.Getenv("POSTMARKD_TOKEN"), "client token, if the daemon requires one (default $POSTMARKD_TOKEN)")
	from := fl.String("from", "", "sender (default: the daemon's POSTMARKD_DEFAULT_FROM)")
	var to, cc, bcc, attach, meta, headers multi
	fl.Var(&to, "to", "recipient (repeatable, or comma separated)")
	fl.Var(&cc, "cc", "Cc recipient (repeatable)")
	fl.Var(&bcc, "bcc", "Bcc recipient (repeatable)")
	subject := fl.String("subject", "", "subject")
	text := fl.String("text", "", "plain text body; @file reads a file, - reads stdin")
	html := fl.String("html", "", "HTML body; @file reads a file, - reads stdin")
	fl.Var(&attach, "attach", "file to attach (repeatable)")
	replyTo := fl.String("reply-to", "", "Reply-To address")
	tag := fl.String("tag", "", "Postmark tag")
	stream := fl.String("stream", "", "Postmark message stream (e.g. outbound, broadcast)")
	fl.Var(&meta, "metadata", "metadata key=value (repeatable)")
	fl.Var(&headers, "header", "custom header Name:Value (repeatable)")
	template := fl.String("template", "", "Postmark template alias or numeric ID")
	model := fl.String("model", "", "template model as JSON; @file reads a file")
	async := fl.Bool("async", false, "queue the message and return immediately")
	fl.Usage = func() {
		fmt.Fprint(fl.Output(), `Usage: postmarkd send -to ADDRESS [flags]

Send a message through a running postmarkd. If no body is given and stdin
is not a terminal, stdin is used as the plain text body:

  echo "Backup finished" | postmarkd send -to ops@example.com -subject "Backup OK"
  postmarkd send -to ada@example.com -subject Report -html @report.html -attach report.pdf
  postmarkd send -to ada@example.com -template welcome -model '{"name":"Ada"}'

Flags:
`)
		fl.PrintDefaults()
	}
	fl.Parse(args)
	if len(to)+len(cc)+len(bcc) == 0 {
		fmt.Fprintln(os.Stderr, "postmarkd send: at least one -to, -cc or -bcc is required")
		fl.Usage()
		return 2
	}

	msg := map[string]any{}
	set := func(k, v string) {
		if v != "" {
			msg[k] = v
		}
	}
	set("From", *from)
	set("To", strings.Join(to, ", "))
	set("Cc", strings.Join(cc, ", "))
	set("Bcc", strings.Join(bcc, ", "))
	set("Subject", *subject)
	set("ReplyTo", *replyTo)
	set("Tag", *tag)
	set("MessageStream", *stream)

	var err error
	readArg := func(v string) string {
		if err != nil || v == "" {
			return v
		}
		var b []byte
		switch {
		case v == "-":
			b, err = io.ReadAll(os.Stdin)
		case strings.HasPrefix(v, "@"):
			b, err = os.ReadFile(v[1:])
		default:
			return v
		}
		return string(b)
	}
	if *text == "" && *html == "" && *template == "" && stdinPiped() {
		*text = "-"
	}
	set("TextBody", readArg(*text))
	set("HtmlBody", readArg(*html))

	path := "/email"
	if *template != "" {
		path = "/email/withTemplate"
		if id, perr := strconv.ParseInt(*template, 10, 64); perr == nil {
			msg["TemplateId"] = id
		} else {
			msg["TemplateAlias"] = *template
		}
		m := readArg(*model)
		if m == "" {
			m = "{}"
		}
		if !json.Valid([]byte(m)) {
			fmt.Fprintln(os.Stderr, "postmarkd send: -model is not valid JSON")
			return 2
		}
		msg["TemplateModel"] = json.RawMessage(m)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "postmarkd send:", err)
		return 1
	}

	if len(meta) > 0 {
		md := map[string]string{}
		for _, kv := range meta {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				fmt.Fprintf(os.Stderr, "postmarkd send: -metadata %q must be key=value\n", kv)
				return 2
			}
			md[k] = v
		}
		msg["Metadata"] = md
	}
	if len(headers) > 0 {
		var hs []map[string]string
		for _, h := range headers {
			k, v, ok := strings.Cut(h, ":")
			if !ok {
				fmt.Fprintf(os.Stderr, "postmarkd send: -header %q must be Name:Value\n", h)
				return 2
			}
			hs = append(hs, map[string]string{"Name": strings.TrimSpace(k), "Value": strings.TrimSpace(v)})
		}
		msg["Headers"] = hs
	}
	var atts []map[string]string
	for _, f := range attach {
		b, err := os.ReadFile(f)
		if err != nil {
			fmt.Fprintln(os.Stderr, "postmarkd send:", err)
			return 1
		}
		ct := mime.TypeByExtension(filepath.Ext(f))
		if ct == "" {
			ct = "application/octet-stream"
		}
		atts = append(atts, map[string]string{"Name": filepath.Base(f), "ContentType": ct, "Content": base64.StdEncoding.EncodeToString(b)})
	}
	if atts != nil {
		msg["Attachments"] = atts
	}

	body, _ := json.Marshal(msg)
	h := http.Header{}
	if *async {
		h.Set("X-Postmarkd-Async", "true")
	}
	res, err := client.New(client.ResolveAddr(*addr), *token).Do(context.Background(), http.MethodPost, path, body, h)
	if err != nil {
		fmt.Fprintln(os.Stderr, "postmarkd send:", err)
		return 1
	}
	var r struct {
		ErrorCode int
		Message   string
		MessageID string
		RequestID string
	}
	json.Unmarshal(res.Body, &r)
	switch {
	case res.StatusCode == http.StatusAccepted:
		fmt.Println("queued", r.RequestID)
	case res.StatusCode == http.StatusOK && r.ErrorCode == 0:
		fmt.Println(r.MessageID)
	default:
		if r.Message == "" {
			r.Message = strings.TrimSpace(string(res.Body))
		}
		fmt.Fprintf(os.Stderr, "postmarkd send: %s (HTTP %d, error %d)\n", r.Message, res.StatusCode, r.ErrorCode)
		return 1
	}
	return 0
}

func stdinPiped() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice == 0
}
