package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMessageAccessors(t *testing.T) {
	t.Parallel()
	var m message
	mustUnmarshal(t, []byte(`{"from":" a@example.com ","To":5,"Subject":null,"tag":"x"}`), &m)
	cases := []struct {
		name, key, str string
		found          bool
	}{
		{"From", "from", "a@example.com", true}, // case-insensitive, trimmed
		{"FROM", "from", "a@example.com", true},
		{"To", "To", "", true}, // not a string
		{"subject", "Subject", "", true},
		{"Tag", "tag", "x", true},
		{"Cc", "", "", false},
	}
	for _, c := range cases {
		k, ok := m.key(c.name)
		if k != c.key || ok != c.found {
			t.Errorf("key(%q) = %q, %v; want %q, %v", c.name, k, ok, c.key, c.found)
		}
		if got := m.str(c.name); got != c.str {
			t.Errorf("str(%q) = %q, want %q", c.name, got, c.str)
		}
	}
	// An exact match wins over a case-insensitive one.
	var dup message
	mustUnmarshal(t, []byte(`{"from":"lower","From":"exact"}`), &dup)
	if k, _ := dup.key("From"); k != "From" || dup.str("From") != "exact" {
		t.Errorf("exact key not preferred: %q", k)
	}
}

func TestSetDefault(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, in, value, want string
	}{
		{"adds missing", `{}`, "d", `{"From":"d"}`},
		{"keeps existing", `{"From":"a"}`, "d", `{"From":"a"}`},
		{"replaces blank in place", `{"from":"  "}`, "d", `{"from":"d"}`},
		{"replaces null", `{"From":null}`, "d", `{"From":"d"}`},
		{"replaces non-string", `{"FROM":1}`, "d", `{"FROM":"d"}`},
		{"empty default is a no-op", `{"From":""}`, "", `{"From":""}`},
	}
	for _, c := range cases {
		var m message
		mustUnmarshal(t, []byte(c.in), &m)
		m.setDefault("From", c.value)
		out, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != c.want {
			t.Errorf("%s: got %s, want %s", c.name, out, c.want)
		}
	}
}

func TestAPIErrorMessage(t *testing.T) {
	t.Parallel()
	err := invalid("message %d: %s", 3, "bad")
	if err.Error() != "message 3: bad" || err.Status != 422 || err.ErrorCode != codeInvalidRequest {
		t.Fatalf("invalid() = %+v", err)
	}
	b, _ := json.Marshal(err)
	if string(b) != `{"ErrorCode":300,"Message":"message 3: bad"}` {
		t.Errorf("wire form %s", b)
	}
}

func TestBlankString(t *testing.T) {
	t.Parallel()
	cases := []struct {
		quoted string
		blank  bool
	}{
		{`""`, true},
		{`" "`, true},
		{`" \t\r\n "`, true},
		{"\"\\u0020\"", true},
		{"\"\\u000b\\f\"", true}, // vertical tab and form feed are space too
		{"\"\\u00a0\"", true},    // escaped NBSP
		{"\"\\u2028 \"", true},   // escaped line separator
		{"\"\u00a0\"", true},     // raw NBSP, same verdict as escaped
		{"\" \u2028\u3000 \"", true},
		{"\"\u0085\"", true}, // raw next line
		{"\"\u00a0a\"", false},
		{"\"caf\u00e9\"", false},
		{`"a"`, false},
		{`"  a  "`, false},
		{`"\n a"`, false},
		{"\"\\u0041\"", false},
		{"\"\\u0000\"", false}, // NUL is not space
		{`"\""`, false},
		{`"\\"`, false},
	}
	for _, c := range cases {
		if got := blankString([]byte(c.quoted)); got != c.blank {
			t.Errorf("blankString(%s) = %v, want %v", c.quoted, got, c.blank)
		}
	}
}

func TestFieldUnmarshal(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw  string
		want field
	}{
		{`"a"`, field{present: true, nonBlank: true}},
		{`""`, field{present: true}},
		{`"  "`, field{present: true}},
		{`null`, field{present: true, null: true}},
		{`0`, field{present: true}},
		{`["a"]`, field{present: true}},
		{`{"a":"b"}`, field{present: true}},
	}
	for _, c := range cases {
		var f field
		if err := f.UnmarshalJSON([]byte(c.raw)); err != nil {
			t.Fatal(err)
		}
		if f != c.want {
			t.Errorf("field(%s) = %+v, want %+v", c.raw, f, c.want)
		}
	}
}

func TestValidationRules(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		template bool
		in       string
		msg      string // "" means accepted
	}{
		{"cc only", false, `{"From":"f","Cc":"c","HtmlBody":"<p>"}`, ""},
		{"bcc only", false, `{"From":"f","Bcc":"b","TextBody":"t"}`, ""},
		{"blank recipients", false, `{"From":"f","To":" ","Cc":"","Bcc":null,"TextBody":"t"}`, "at least one recipient"},
		{"numeric To", false, `{"From":"f","To":1,"TextBody":"t"}`, "at least one recipient"},
		{"blank bodies", false, `{"From":"f","To":"a","HtmlBody":" ","TextBody":"\n"}`, "'HtmlBody' or 'TextBody' is required"},
		{"lowercase keys", false, `{"from":"f","to":"a","textbody":"t"}`, ""},
		{"template id zero", true, `{"From":"f","To":"a","TemplateId":0}`, ""},
		{"template id null", true, `{"From":"f","To":"a","TemplateId":null}`, "'TemplateId' or 'TemplateAlias' is required"},
		{"template alias", true, `{"From":"f","To":"a","TemplateAlias":"welcome"}`, ""},
		{"template alias blank", true, `{"From":"f","To":"a","TemplateAlias":"  "}`, "'TemplateId' or 'TemplateAlias' is required"},
		{"template ignores body rule", true, `{"From":"f","To":"a","TemplateId":1}`, ""},
		{"email ignores template", false, `{"From":"f","To":"a","TemplateId":1}`, "'HtmlBody' or 'TextBody' is required"},
		{"blank From", false, `{"From":" ","To":"a","TextBody":"t"}`, "'From' is required"},
		{"not an object", false, `"x"`, "Invalid JSON"},
		{"truncated", false, `{"From":"f"`, "Invalid JSON"},
		{"trailing garbage", false, `{"From":"f"} x`, "Invalid JSON"},
	}
	for _, c := range cases {
		out, err := normalize([]byte(c.in), c.template, defaults{})
		switch {
		case c.msg == "" && err != nil:
			t.Errorf("%s: rejected: %s", c.name, err.Message)
		case c.msg == "" && string(out) != c.in:
			t.Errorf("%s: rewritten to %s with no defaults", c.name, out)
		case c.msg != "" && (err == nil || !strings.Contains(err.Message, c.msg)):
			t.Errorf("%s: err = %v, want %q", c.name, err, c.msg)
		}
	}
}

func TestNormalizeSlowPreservesKeySpelling(t *testing.T) {
	t.Parallel()
	d := defaults{From: "def@example.com", MessageStream: "outbound"}
	out, err := normalize([]byte(`{"from":"","messagestream":" ","to":"a","textbody":"t","Extra":[1,2]}`), false, d)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	mustUnmarshal(t, out, &m)
	if m["from"] != "def@example.com" || m["messagestream"] != "outbound" || m["From"] != nil || m["MessageStream"] != nil {
		t.Errorf("got %s", out)
	}
	if extra, ok := m["Extra"].([]any); !ok || len(extra) != 2 {
		t.Errorf("unknown field lost: %s", out)
	}
}

func TestNormalizeInsertIntoMinimalObject(t *testing.T) {
	t.Parallel()
	// Defaults are inserted after the brace with exactly one separator,
	// whatever whitespace follows it.
	for _, in := range []string{
		`{"To":"a","TextBody":"t"}`,
		"{ \n\t\"To\":\"a\",\"TextBody\":\"t\"}",
		"\n {\"To\":\"a\",\"TextBody\":\"t\"} \n",
	} {
		out, err := normalize([]byte(in), false, defaults{From: "f", MessageStream: "s"})
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if !strings.HasPrefix(string(out), `{"From":"f","MessageStream":"s","To":"a"`) || !json.Valid(out) {
			t.Errorf("%q -> %s", in, out)
		}
	}
	out, err := normalize([]byte(`{"From":"me","To":"a","TextBody":"t"}`), false, defaults{From: "f", MessageStream: "s"})
	if err != nil || string(out) != `{"MessageStream":"s","From":"me","To":"a","TextBody":"t"}` {
		t.Errorf("stream only: %s %v", out, err)
	}
}

func TestPrepareTemplateBatch(t *testing.T) {
	t.Parallel()
	ep := endpoints[3]
	const msg = `{"From":"f","To":"a","TemplateId":1,"MessageStream":"s"}`
	cases := []struct {
		name, body string
		d          defaults
		n          int
		msg        string
		same       bool
	}{
		{"unchanged", ` {"Messages": [` + msg + `, ` + msg + `]} `, defaults{From: "x"}, 2, "", true},
		{"lowercase key", `{"messages":[` + msg + `]}`, defaults{}, 1, "", true},
		{"defaults applied", `{"Messages":[{"To":"a","TemplateAlias":"w"}],"Other":true}`, defaults{From: "d", MessageStream: "s"}, 1, "", false},
		{"not object", `[` + msg + `]`, defaults{}, 0, "Invalid JSON", false},
		{"truncated", `{"Messages":[`, defaults{}, 0, "Invalid JSON", false},
		{"missing Messages", `{"Msgs":[]}`, defaults{}, 0, "'Messages' is required", false},
		{"Messages not array", `{"Messages":{}}`, defaults{}, 0, "'Messages' must be an array", false},
		{"Messages null", `{"Messages":null}`, defaults{}, 0, "'Messages' must be an array", false},
		{"empty", `{"Messages":[]}`, defaults{}, 0, "batch contains no messages", false},
		{"invalid message", `{"Messages":[` + msg + `,{"From":"f","To":"a"}]}`, defaults{}, 0, "message 1: 'TemplateId' or 'TemplateAlias' is required", false},
		{"non-object message", `{"Messages":[` + msg + `,7]}`, defaults{}, 0, "message 1: expected a JSON object", false},
	}
	for _, c := range cases {
		out, n, err := prepare(ep, []byte(c.body), c.d)
		if c.msg != "" {
			if err == nil || !strings.Contains(err.Message, c.msg) || err.Status != 422 {
				t.Errorf("%s: err = %v, want %q", c.name, err, c.msg)
			}
			continue
		}
		if err != nil || n != c.n {
			t.Errorf("%s: n=%d err=%v", c.name, n, err)
			continue
		}
		if (string(out) == c.body) != c.same {
			t.Errorf("%s: forwarded unchanged = %v, want %v (%s)", c.name, string(out) == c.body, c.same, out)
		}
		if !c.same {
			var w struct {
				Messages []map[string]any
				Other    bool
			}
			mustUnmarshal(t, out, &w)
			if len(w.Messages) != 1 || w.Messages[0]["From"] != "d" || w.Messages[0]["MessageStream"] != "s" || !w.Other {
				t.Errorf("%s: got %s", c.name, out)
			}
		}
	}
}

func TestPrepareBatchErrors(t *testing.T) {
	t.Parallel()
	ok := `{"From":"f","To":"a","TextBody":"t"}`
	cases := []struct {
		body, msg string
		code      int
	}{
		{`{"From":"f"}`, "Invalid JSON", codeInvalidJSON},
		{`[` + ok + `,`, "Invalid JSON", codeInvalidJSON},
		{`[` + ok + `,"x"]`, "message 1: expected a JSON object", codeInvalidRequest},
		{`[` + ok + `,{"From":"f","To":"a"}]`, "message 1: 'HtmlBody' or 'TextBody' is required", codeInvalidRequest},
		{`[null]`, "message 0: expected a JSON object", codeInvalidRequest},
	}
	for _, c := range cases {
		_, _, err := prepare(endpoints[1], []byte(c.body), defaults{})
		if err == nil || err.Message != c.msg || err.ErrorCode != c.code {
			t.Errorf("%s: err = %+v, want %d %q", c.body, err, c.code, c.msg)
		}
	}
	// The limit is inclusive.
	msgs := make([]string, maxBatch)
	for i := range msgs {
		msgs[i] = ok
	}
	body := "[" + strings.Join(msgs, ",") + "]"
	if out, n, err := prepare(endpoints[1], []byte(body), defaults{}); err != nil || n != maxBatch || string(out) != body {
		t.Errorf("batch of exactly %d: n=%d err=%v", maxBatch, n, err)
	}
	_, _, err := prepare(endpoints[1], []byte("["+strings.Join(msgs, ",")+","+ok+"]"), defaults{})
	if err == nil || err.Message != "batch contains 501 messages; the maximum is 500" {
		t.Errorf("over limit: %v", err)
	}
}

func TestPrepareUnknownKindPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Error("unknown kind did not panic")
		}
	}()
	_, _, _ = prepare(endpoint{"x", "/x", kind(99)}, []byte(`{}`), defaults{})
}

func TestIsJSON(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		open byte
		want bool
	}{
		{`{}`, '{', true},
		{" \t\r\n[", '[', true},
		{`[`, '{', false},
		{``, '{', false},
		{"   ", '[', false},
		{"\v{", '{', false},
	}
	for _, c := range cases {
		if got := isJSON([]byte(c.in), c.open); got != c.want {
			t.Errorf("isJSON(%q, %c) = %v, want %v", c.in, c.open, got, c.want)
		}
	}
}

func TestOutcome(t *testing.T) {
	t.Parallel()
	single, batch := endpoints[0], endpoints[1]
	cases := []struct {
		name         string
		ep           endpoint
		status       int
		body         string
		n            int
		sent, failed int
		firstErr     string
	}{
		{"single ok", single, 200, `{"ErrorCode":0,"Message":"OK"}`, 1, 1, 0, ""},
		{"single rejected", single, 200, `{"ErrorCode":406,"Message":"inactive"}`, 1, 0, 1, "inactive"},
		{"single not json", single, 200, `<html>`, 1, 1, 0, ""},
		{"template rejected", endpoints[2], 200, `{"ErrorCode":1101,"Message":"no template"}`, 1, 0, 1, "no template"},
		{"http error with message", single, 422, `{"ErrorCode":300,"Message":"bad"}`, 1, 0, 1, "bad"},
		{"http error without body", batch, 503, ``, 4, 0, 4, "HTTP 503"},
		{"http error empty message", batch, 500, `{"ErrorCode":1}`, 2, 0, 2, "HTTP 500"},
		{"batch mixed", batch, 200, `[{"ErrorCode":0},{"ErrorCode":300,"Message":"first"},{"ErrorCode":406,"Message":"second"}]`, 3, 1, 2, "first"},
		{"batch all ok", batch, 200, `[{"ErrorCode":0},{"ErrorCode":0}]`, 2, 2, 0, ""},
		{"batch unparseable", batch, 200, `{"ErrorCode":0}`, 5, 5, 0, ""},
		{"batch shorter than request", batch, 200, `[{"ErrorCode":0}]`, 3, 1, 0, ""},
		{"template batch", endpoints[3], 200, `[{"ErrorCode":0},{"ErrorCode":1101,"Message":"x"}]`, 2, 1, 1, "x"},
	}
	for _, c := range cases {
		sent, failed, firstErr := outcome(c.ep, c.status, []byte(c.body), c.n)
		if sent != c.sent || failed != c.failed || firstErr != c.firstErr {
			t.Errorf("%s: outcome = %d, %d, %q; want %d, %d, %q", c.name, sent, failed, firstErr, c.sent, c.failed, c.firstErr)
		}
	}
}

// TestPrepareRawUnicodeWhitespace checks that a field holding only raw
// Unicode whitespace is blank, exactly as when it is written with escapes.
func TestPrepareRawUnicodeWhitespace(t *testing.T) {
	t.Parallel()
	email := endpoints[0]
	for _, from := range []string{"\u00a0", "\u2028", "\u3000", "\u0085", " \u00a0 "} {
		body := []byte(`{"From":"` + from + `","To":"a@example.com","TextBody":"t"}`)
		if _, _, err := prepare(email, body, defaults{}); err == nil || !strings.Contains(err.Message, "'From' is required") {
			t.Errorf("From %q with no default: err = %v, want 'From' is required", from, err)
		}
		out, _, err := prepare(email, body, defaults{From: "d@example.com"})
		if err != nil {
			t.Fatalf("From %q with a default: %v", from, err)
		}
		var m map[string]string
		mustUnmarshal(t, out, &m)
		if m["From"] != "d@example.com" {
			t.Errorf("From %q: default not applied: %s", from, out)
		}
	}
}

// TestPrepareDuplicateFields checks that a checked field given more than
// once, under the same or a different spelling, is rejected every time
// rather than resolved by chance.
func TestPrepareDuplicateFields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		ep    int
		body  string
		field string
	}{
		{0, `{"from":" ","FROM":" ","To":"b@example.com","TextBody":"x"}`, "From"},
		{0, `{"From":"a@example.com","from":"c@example.com","To":"b@example.com","TextBody":"x"}`, "From"},
		{0, `{"From":"a@example.com","To":"b@example.com","To":"c@example.com","TextBody":"x"}`, "To"},
		{0, `{"From":"a@example.com","To":"b@example.com","TextBody":"x","textbody":"y"}`, "TextBody"},
		{0, `{"From":"a@example.com","To":"b@example.com","TextBody":"x","MessageStream":"a","messagestream":"b"}`, "MessageStream"},
		{1, `[{"From":"a@example.com","To":"b@example.com","TextBody":"x"},{"From":"a@example.com","cc":"x","CC":"y","TextBody":"x"}]`, "Cc"},
		{2, `{"From":"a@example.com","To":"b@example.com","TemplateId":1,"templateid":2}`, "TemplateId"},
		{3, `{"messages":[],"MESSAGES":[{"From":"a@example.com","To":"b@example.com","TemplateId":1}]}`, "Messages"},
		{3, `{"Messages":[],"Messages":[{"From":"a@example.com","To":"b@example.com","TemplateId":1}]}`, "Messages"},
	}
	for _, c := range cases {
		for range 20 { // map iteration used to make this random
			_, _, err := prepare(endpoints[c.ep], []byte(c.body), defaults{From: "d@example.com"})
			if err == nil || err.Status != 422 || !strings.Contains(err.Message, "'"+c.field+"' appears more than once") {
				t.Fatalf("%s: err = %v, want '%s' appears more than once", c.body, err, c.field)
			}
		}
	}
}

// TestPrepareBlankDefaults checks that whitespace-only defaults are
// ignored rather than injected as blank fields.
func TestPrepareBlankDefaults(t *testing.T) {
	t.Parallel()
	email := endpoints[0]
	body := []byte(`{"From":"a@example.com","To":"b@example.com","TextBody":"x"}`)
	out, _, err := prepare(email, body, defaults{From: "  ", MessageStream: " \t"})
	if err != nil || string(out) != string(body) {
		t.Errorf("blank defaults changed the body: %s, %v", out, err)
	}
	noFrom := []byte(`{"To":"b@example.com","TextBody":"x"}`)
	if _, _, err := prepare(email, noFrom, defaults{From: " "}); err == nil || !strings.Contains(err.Message, "'From' is required") {
		t.Errorf("a blank default From satisfied the From check: %v", err)
	}
	out, _, err = prepare(email, noFrom, defaults{From: " d@example.com "})
	if err != nil || !strings.Contains(string(out), `"From":"d@example.com"`) {
		t.Errorf("default From not trimmed: %s, %v", out, err)
	}
}
