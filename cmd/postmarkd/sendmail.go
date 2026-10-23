package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/mail"
	"os"
	"strings"

	"github.com/rossedman/postmarkd/internal/client"
	"github.com/rossedman/postmarkd/internal/rfc822"
)

// sysexits(3) codes, which sendmail callers understand.
const (
	exUsage       = 64
	exDataErr     = 65
	exUnavailable = 69
	exSoftware    = 70
	exTempFail    = 75
	exNoPerm      = 77
)

// sendmail implements the commonly used subset of sendmail(8): read a
// message on stdin and deliver it via postmarkd. Configured with
// $POSTMARKD_ADDR, $POSTMARKD_TOKEN and $POSTMARKD_STREAM.
func (c *cli) sendmail(args []string) int {
	var (
		rcpts      []string
		fromHeader bool // -t
		ignoreDots bool // -i / -oi
		envFrom    string
		fullName   string
	)
	// Flags that take a separate value argument and are otherwise ignored.
	withValue := map[string]bool{"-B": true, "-N": true, "-R": true, "-V": true, "-X": true, "-L": true, "-O": true, "-C": true, "-h": true, "-A": true}

	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch {
		case a == "--":
			rcpts = append(rcpts, args[i+1:]...)
			i = len(args)
		case !strings.HasPrefix(a, "-") || a == "-":
			rcpts = append(rcpts, a)
		case a == "-t":
			fromHeader = true
		case a == "-i" || a == "-oi":
			ignoreDots = true
		case strings.HasPrefix(a, "-f") || strings.HasPrefix(a, "-r"):
			if envFrom = a[2:]; envFrom == "" {
				envFrom = next()
			}
		case strings.HasPrefix(a, "-F"):
			if fullName = a[2:]; fullName == "" {
				fullName = next()
			}
		case a == "-bs" || a == "-bp" || a == "-bv" || a == "-bd" || a == "-bi" || a == "-q" || strings.HasPrefix(a, "-q"):
			c.errorf("sendmail (postmarkd): %s is not supported; postmarkd has no local queue\n", a)
			return exUsage
		case withValue[a]:
			next()
		default:
			// -bm, -v, -U, -G, -o<option>, -B<type>, -N<dsn> etc.: accepted and ignored.
		}
	}

	raw, err := readMessage(c.stdin, ignoreDots)
	if err != nil {
		c.errorf("sendmail (postmarkd): reading message: %v\n", err)
		return exDataErr
	}

	env := rfc822.Envelope{From: envFrom, To: rcpts}
	hasFrom := false
	if parsed, err := mail.ReadMessage(bytes.NewReader(raw)); err == nil {
		hasFrom = parsed.Header.Get("From") != ""
		if fromHeader && len(rcpts) > 0 {
			// -t with arguments: deliver to header recipients plus arguments.
			for _, h := range []string{"To", "Cc", "Bcc"} {
				list, _ := parsed.Header.AddressList(h)
				for _, a := range list {
					env.To = append(env.To, a.Address)
				}
			}
		}
	}
	if fromHeader && len(rcpts) == 0 {
		env.To = nil // take recipients from the headers
	} else if len(env.To) == 0 {
		c.errorf("sendmail (postmarkd): no recipients (pass addresses or use -t)\n")
		return exUsage
	}

	msg, err := rfc822.Convert(raw, env)
	if err != nil {
		c.errorf("sendmail (postmarkd): %v\n", err)
		return exDataErr
	}
	if !hasFrom && fullName != "" && msg.From != "" {
		msg.From = `"` + strings.ReplaceAll(fullName, `"`, `'`) + `" <` + msg.From + `>`
	}

	body, _ := json.Marshal(msg)
	h := http.Header{}
	if s := os.Getenv("POSTMARKD_STREAM"); s != "" {
		h.Set("X-Postmarkd-Stream", s)
	}
	res, err := client.New(client.ResolveAddr(""), os.Getenv("POSTMARKD_TOKEN")).Do(c.ctx, http.MethodPost, "/email", body, h)
	if err != nil {
		c.errorf("sendmail (postmarkd): %v\n", err)
		return exTempFail
	}
	var r struct {
		ErrorCode int
		Message   string
	}
	// A body that isn't JSON leaves r zero; the status code decides below.
	_ = json.Unmarshal(res.Body, &r)
	switch {
	case res.StatusCode == http.StatusOK && r.ErrorCode == 0:
		return 0
	case res.StatusCode == http.StatusUnauthorized:
		c.errorf("sendmail (postmarkd): not authorized; set POSTMARKD_TOKEN\n")
		return exNoPerm
	case res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500:
		c.errorf("sendmail (postmarkd): temporary failure: %s\n", r.Message)
		return exTempFail
	case res.StatusCode == http.StatusUnprocessableEntity || res.StatusCode == http.StatusRequestEntityTooLarge:
		c.errorf("sendmail (postmarkd): message rejected: %s\n", r.Message)
		return exDataErr
	case res.StatusCode == http.StatusNotFound:
		return exUnavailable
	default:
		c.errorf("sendmail (postmarkd): HTTP %d: %s\n", res.StatusCode, r.Message)
		return exSoftware
	}
}

// readMessage reads stdin. Unless ignoreDots is set, a line containing only
// "." ends the message, as in traditional sendmail.
func readMessage(r io.Reader, ignoreDots bool) ([]byte, error) {
	if ignoreDots {
		return io.ReadAll(r)
	}
	var buf bytes.Buffer
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if strings.TrimRight(line, "\r\n") == "." {
			break
		}
		buf.WriteString(line)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}
