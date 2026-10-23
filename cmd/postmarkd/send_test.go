package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const okResponse = `{"ErrorCode":0,"Message":"OK","MessageID":"b7bc2f4a-e38e-4336-af7d-e6c392c2f817"}`

// writeFile writes contents to name in dir and returns its path.
func writeFile(t *testing.T, dir, name, contents string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSendMessage(t *testing.T) {
	d := newFakeDaemon(t, 200, okResponse)
	dir := t.TempDir()
	html := writeFile(t, dir, "body.html", "<p>Hi</p>")
	pdf := writeFile(t, dir, "report.pdf", "%PDF")
	blob := writeFile(t, dir, "data.unknownext", "\x00\x01")

	c, stdout, stderr := testCLI("")
	code := c.send([]string{
		"-to", "a@example.com", "-to", "b@example.com", "-cc", "c@example.com", "-bcc", "d@example.com",
		"-from", "me@example.com", "-subject", "Report", "-text", "plain", "-html", "@" + html,
		"-reply-to", "r@example.com", "-tag", "weekly", "-stream", "broadcast",
		"-metadata", "k=v", "-metadata", "x=y=z", "-header", "X-One: 1", "-header", "X-Two:2",
		"-attach", pdf, "-attach", blob, "-token", "secret",
	})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if got := stdout.String(); got != "b7bc2f4a-e38e-4336-af7d-e6c392c2f817\n" {
		t.Errorf("stdout = %q", got)
	}
	r := d.last(t)
	if r.method != "POST" || r.path != "/email" {
		t.Errorf("request %s %s", r.method, r.path)
	}
	if got := r.header.Get("Authorization"); got != "Bearer secret" {
		t.Errorf("Authorization = %q", got)
	}
	if r.header.Get("X-Postmarkd-Async") != "" {
		t.Error("sync send sent the async header")
	}
	want := map[string]any{
		"From": "me@example.com", "To": "a@example.com, b@example.com", "Cc": "c@example.com", "Bcc": "d@example.com",
		"Subject": "Report", "TextBody": "plain", "HtmlBody": "<p>Hi</p>", "ReplyTo": "r@example.com",
		"Tag": "weekly", "MessageStream": "broadcast",
		"Metadata": map[string]any{"k": "v", "x": "y=z"},
		"Headers": []any{
			map[string]any{"Name": "X-One", "Value": "1"},
			map[string]any{"Name": "X-Two", "Value": "2"},
		},
		"Attachments": []any{
			map[string]any{"Name": "report.pdf", "ContentType": "application/pdf", "Content": base64.StdEncoding.EncodeToString([]byte("%PDF"))},
			map[string]any{"Name": "data.unknownext", "ContentType": "application/octet-stream", "Content": base64.StdEncoding.EncodeToString([]byte("\x00\x01"))},
		},
	}
	if !reflect.DeepEqual(r.body, want) {
		t.Errorf("body\n got %#v\nwant %#v", r.body, want)
	}
}

func TestSendBodyFromStdin(t *testing.T) {
	d := newFakeDaemon(t, 200, okResponse)

	// No body flag and piped stdin: stdin is the text body.
	c, _, stderr := testCLI("Backup finished\n")
	if code := c.send([]string{"-to", "ops@example.com"}); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if got := d.last(t).body["TextBody"]; got != "Backup finished\n" {
		t.Errorf("TextBody = %q", got)
	}

	// -html - reads stdin explicitly.
	c, _, _ = testCLI("<b>hi</b>")
	if code := c.send([]string{"-to", "ops@example.com", "-html", "-"}); code != 0 {
		t.Fatal(code)
	}
	if b := d.last(t).body; b["HtmlBody"] != "<b>hi</b>" || b["TextBody"] != nil {
		t.Errorf("body = %v", b)
	}

	// Nothing piped: no body at all.
	c, _, _ = testCLI("")
	if code := c.send([]string{"-to", "ops@example.com", "-subject", "empty"}); code != 0 {
		t.Fatal(code)
	}
	if b := d.last(t).body; b["TextBody"] != nil || b["HtmlBody"] != nil {
		t.Errorf("body = %v", b)
	}
}

func TestSendTemplate(t *testing.T) {
	d := newFakeDaemon(t, 200, okResponse)
	model := writeFile(t, t.TempDir(), "model.json", `{"name":"Ada"}`)

	for _, tc := range []struct {
		args []string
		want map[string]any
	}{
		{[]string{"-template", "welcome", "-model", `{"n":1}`}, map[string]any{"TemplateAlias": "welcome", "TemplateModel": map[string]any{"n": float64(1)}}},
		{[]string{"-template", "12345", "-model", "@" + model}, map[string]any{"TemplateId": float64(12345), "TemplateModel": map[string]any{"name": "Ada"}}},
		{[]string{"-template", "welcome"}, map[string]any{"TemplateAlias": "welcome", "TemplateModel": map[string]any{}}},
	} {
		// A template send never reads stdin as a body, even when piped.
		c, _, stderr := testCLI("ignored")
		if code := c.send(append([]string{"-to", "a@example.com"}, tc.args...)); code != 0 {
			t.Fatalf("%q: exit %d: %s", tc.args, code, stderr)
		}
		r := d.last(t)
		if r.path != "/email/withTemplate" {
			t.Errorf("%q: path %s", tc.args, r.path)
		}
		delete(r.body, "To")
		if !reflect.DeepEqual(r.body, tc.want) {
			t.Errorf("%q: body %v, want %v", tc.args, r.body, tc.want)
		}
	}
}

func TestSendAsync(t *testing.T) {
	d := newFakeDaemon(t, 202, `{"RequestID":"req-1","Message":"Queued"}`)
	c, stdout, _ := testCLI("")
	if code := c.send([]string{"-to", "a@example.com", "-text", "x", "-async"}); code != 0 {
		t.Fatal(code)
	}
	if stdout.String() != "queued req-1\n" {
		t.Errorf("stdout = %q", stdout)
	}
	if d.last(t).header.Get("X-Postmarkd-Async") != "true" {
		t.Error("async header not sent")
	}
}

func TestSendFailures(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	for _, tc := range []struct {
		desc   string
		status int
		resp   string
		args   []string
		code   int
		stderr string
	}{
		{"no recipients", 200, okResponse, []string{"-subject", "x"}, 2, "at least one -to, -cc or -bcc"},
		{"bad flag", 200, okResponse, []string{"-nope"}, 2, "flag provided but not defined"},
		{"help", 200, okResponse, []string{"-h"}, 0, "Usage: postmarkd send"},
		{"bad model", 200, okResponse, []string{"-to", "a@x", "-template", "t", "-model", "{"}, 2, "-model is not valid JSON"},
		{"missing model file", 200, okResponse, []string{"-to", "a@x", "-template", "t", "-model", "@" + missing}, 1, "no such file"},
		{"missing body file", 200, okResponse, []string{"-to", "a@x", "-text", "@" + missing}, 1, "no such file"},
		{"bad metadata", 200, okResponse, []string{"-to", "a@x", "-metadata", "novalue"}, 2, `-metadata "novalue" must be key=value`},
		{"bad header", 200, okResponse, []string{"-to", "a@x", "-header", "NoColon"}, 2, `-header "NoColon" must be Name:Value`},
		{"missing attachment", 200, okResponse, []string{"-to", "a@x", "-attach", missing}, 1, "no such file"},
		{"API error", 422, `{"ErrorCode":300,"Message":"Invalid 'To' address"}`, []string{"-to", "nope"}, 1, "Invalid 'To' address (HTTP 422, error 300)"},
		{"Postmark error with 200", 200, `{"ErrorCode":406,"Message":"inactive"}`, []string{"-to", "a@x"}, 1, "inactive (HTTP 200, error 406)"},
		{"non-JSON error", 502, "Bad Gateway\n", []string{"-to", "a@x"}, 1, "Bad Gateway (HTTP 502, error 0)"},
	} {
		newFakeDaemon(t, tc.status, tc.resp)
		c, _, stderr := testCLI("")
		if code := c.send(tc.args); code != tc.code {
			t.Errorf("%s: exit %d, want %d (%s)", tc.desc, code, tc.code, stderr)
		}
		if !strings.Contains(stderr.String(), tc.stderr) {
			t.Errorf("%s: stderr %q, want %q", tc.desc, stderr, tc.stderr)
		}
	}
}

func TestSendUnreachable(t *testing.T) {
	unreachable(t)
	c, _, stderr := testCLI("")
	if code := c.send([]string{"-to", "a@example.com", "-text", "x"}); code != 1 {
		t.Errorf("exit %d", code)
	}
	if !strings.Contains(stderr.String(), "cannot reach postmarkd") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestMulti(t *testing.T) {
	t.Parallel()
	var m multi
	for _, v := range []string{"a", "b"} {
		if err := m.Set(v); err != nil {
			t.Fatal(err)
		}
	}
	if m.String() != "a,b" {
		t.Errorf("String() = %q", m.String())
	}
}
