package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// maxBatch is Postmark's limit on messages per batch request.
const maxBatch = 500

// message is a Postmark message kept as raw JSON fields so that fields
// postmarkd does not know about are forwarded untouched.
type message map[string]json.RawMessage

// key returns the stored spelling of name, matched case-insensitively.
func (m message) key(name string) (string, bool) {
	if _, ok := m[name]; ok {
		return name, true
	}
	for k := range m {
		if strings.EqualFold(k, name) {
			return k, true
		}
	}
	return "", false
}

// str returns the string value of a field, or "" if missing or not a string.
func (m message) str(name string) string {
	k, ok := m.key(name)
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(m[k], &s) != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

func (m message) setDefault(name, value string) {
	if value == "" || m.str(name) != "" {
		return
	}
	k, ok := m.key(name)
	if !ok {
		k = name
	}
	m[k], _ = json.Marshal(value)
}

// kind is the shape of a send request.
type kind int

const (
	kindEmail kind = iota
	kindBatch
	kindTemplate
	kindTemplateBatch
)

// endpoint describes one send API route.
type endpoint struct {
	name string // metrics/log label
	path string // Postmark API path, also the route we serve
	kind kind
}

var endpoints = []endpoint{
	{"email", "/email", kindEmail},
	{"batch", "/email/batch", kindBatch},
	{"template", "/email/withTemplate", kindTemplate},
	{"template_batch", "/email/batchWithTemplates", kindTemplateBatch},
}

// apiError is an error returned to the client in Postmark's error format.
type apiError struct {
	Status    int    `json:"-"`
	ErrorCode int    `json:"ErrorCode"`
	Message   string `json:"Message"`
}

func (e *apiError) Error() string { return e.Message }

// Error codes. 10, 300 and 402 match Postmark's own codes; 9xxx are
// specific to postmarkd.
const (
	codeUnauthorized   = 10
	codeInvalidRequest = 300
	codeInvalidJSON    = 402
	codeUpstream       = 9000
	codeQueueFull      = 9001
	codeAsyncDisabled  = 9002
)

func invalid(format string, args ...any) *apiError {
	return &apiError{Status: 422, ErrorCode: codeInvalidRequest, Message: fmt.Sprintf(format, args...)}
}

var errInvalidJSON = &apiError{Status: 422, ErrorCode: codeInvalidJSON, Message: "Invalid JSON"}

// defaults are applied to every message that omits the field.
type defaults struct {
	From          string
	MessageStream string
}

// prepare parses and validates a request body for ep, applies defaults and
// returns the body to forward along with the number of messages it holds.
//
// This is on the path of every send, so it avoids decoding whole messages:
// each message is scanned once for the few fields postmarkd checks, and the
// original bytes are forwarded unchanged unless a default has to be added.
func prepare(ep endpoint, body []byte, d defaults) ([]byte, int, *apiError) {
	switch ep.kind {
	case kindEmail, kindTemplate:
		out, err := normalize(body, ep.kind == kindTemplate, d)
		if err != nil {
			return nil, 0, err
		}
		return out, 1, nil

	case kindBatch:
		if !isJSON(body, '[') {
			return nil, 0, errInvalidJSON
		}
		return normalizeBatch(body, false, d)

	case kindTemplateBatch:
		var wrapper message
		if !isJSON(body, '{') || json.Unmarshal(body, &wrapper) != nil {
			return nil, 0, errInvalidJSON
		}
		k, ok := wrapper.key("Messages")
		if !ok {
			return nil, 0, invalid("'Messages' is required")
		}
		if !isJSON(wrapper[k], '[') {
			return nil, 0, invalid("'Messages' must be an array of messages")
		}
		msgs, n, err := normalizeBatch(wrapper[k], true, d)
		if err != nil {
			return nil, 0, err
		}
		if &msgs[0] == &wrapper[k][0] { // unchanged
			return body, n, nil
		}
		wrapper[k] = msgs
		out, merr := json.Marshal(wrapper)
		if merr != nil {
			return nil, 0, errInvalidJSON
		}
		return out, n, nil
	}
	panic("unknown endpoint kind")
}

// rawJSON captures a JSON value without copying it. encoding/json hands
// UnmarshalJSON a subslice of the input, which outlives the call here.
type rawJSON []byte

func (r *rawJSON) UnmarshalJSON(b []byte) error { *r = b; return nil }

// normalizeBatch normalizes every message of a JSON array.
func normalizeBatch(body []byte, template bool, d defaults) ([]byte, int, *apiError) {
	var elems []rawJSON
	if json.Unmarshal(body, &elems) != nil {
		return nil, 0, errInvalidJSON
	}
	if len(elems) == 0 {
		return nil, 0, invalid("batch contains no messages")
	}
	if len(elems) > maxBatch {
		return nil, 0, invalid("batch contains %d messages; the maximum is %d", len(elems), maxBatch)
	}
	changed := false
	outs := make([][]byte, len(elems))
	for i, e := range elems {
		out, err := normalize(e, template, d)
		if err != nil {
			if err == errInvalidJSON {
				return nil, 0, invalid("message %d: expected a JSON object", i)
			}
			return nil, 0, invalid("message %d: %s", i, err.Message)
		}
		outs[i] = out
		changed = changed || &out[0] != &e[0]
	}
	if !changed {
		return body, len(elems), nil
	}
	size := 2 + len(outs)
	for _, o := range outs {
		size += len(o)
	}
	buf := make([]byte, 0, size)
	buf = append(buf, '[')
	for i, o := range outs {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, o...)
	}
	return append(buf, ']'), len(elems), nil
}

// field records whether a field is present and whether it holds a
// non-blank string, without copying its value.
type field struct{ present, null, nonBlank bool }

func (f *field) UnmarshalJSON(b []byte) error {
	f.present = true
	f.null = string(b) == "null"
	f.nonBlank = len(b) > 2 && b[0] == '"' && !blankString(b)
	return nil
}

func blankString(quoted []byte) bool {
	for _, c := range quoted[1 : len(quoted)-1] {
		switch c {
		case ' ', '\t', '\r', '\n':
		case '\\': // escapes are rare; decode to be exact
			var s string
			json.Unmarshal(quoted, &s)
			return strings.TrimSpace(s) == ""
		default:
			return false
		}
	}
	return true
}

// fields are the message fields postmarkd checks. Like Postmark,
// encoding/json matches keys case-insensitively.
type fields struct {
	From, To, Cc, Bcc, HtmlBody, TextBody    field
	TemplateId, TemplateAlias, MessageStream field
}

// normalize validates one message and applies defaults. It returns raw
// itself (same backing array) when nothing had to change, so callers can
// detect that cheaply. Validation is just enough to reject obviously broken
// messages before they reach Postmark (or the async queue, where the caller
// would never see the error).
func normalize(raw []byte, template bool, d defaults) ([]byte, *apiError) {
	var f fields
	if !isJSON(raw, '{') || json.Unmarshal(raw, &f) != nil {
		return nil, errInvalidJSON
	}

	type kv struct{ k, v string }
	var add []kv
	for _, x := range []struct {
		name string
		f    *field
		def  string
	}{{"From", &f.From, d.From}, {"MessageStream", &f.MessageStream, d.MessageStream}} {
		if x.def == "" || x.f.nonBlank {
			continue
		}
		if x.f.present {
			// Present but blank: replace in place (rare; full decode).
			return normalizeSlow(raw, template, d)
		}
		add = append(add, kv{x.name, x.def})
		x.f.nonBlank = true
	}

	if !f.From.nonBlank {
		return nil, invalid("'From' is required (or configure POSTMARKD_DEFAULT_FROM)")
	}
	if !f.To.nonBlank && !f.Cc.nonBlank && !f.Bcc.nonBlank {
		return nil, invalid("at least one recipient ('To', 'Cc' or 'Bcc') is required")
	}
	if template {
		if (!f.TemplateId.present || f.TemplateId.null) && !f.TemplateAlias.nonBlank {
			return nil, invalid("'TemplateId' or 'TemplateAlias' is required")
		}
	} else if !f.HtmlBody.nonBlank && !f.TextBody.nonBlank {
		return nil, invalid("'HtmlBody' or 'TextBody' is required")
	}
	if len(add) == 0 {
		return raw, nil
	}

	// Insert the defaults right after the opening brace.
	i := bytes.IndexByte(raw, '{') + 1
	rest := bytes.TrimLeft(raw[i:], " \t\r\n")
	out := make([]byte, 0, len(raw)+64)
	out = append(out, '{')
	for j, a := range add {
		if j > 0 {
			out = append(out, ',')
		}
		k, _ := json.Marshal(a.k)
		v, _ := json.Marshal(a.v)
		out = append(append(append(out, k...), ':'), v...)
	}
	if rest[0] != '}' {
		out = append(out, ',')
	}
	return append(out, rest...), nil
}

// normalizeSlow is the general path: decode to a map, edit, re-encode.
func normalizeSlow(raw []byte, template bool, d defaults) ([]byte, *apiError) {
	var m message
	if json.Unmarshal(raw, &m) != nil {
		return nil, errInvalidJSON
	}
	m.setDefault("From", d.From)
	m.setDefault("MessageStream", d.MessageStream)
	out, err := json.Marshal(m)
	if err != nil {
		return nil, errInvalidJSON
	}
	// Defaults now hold non-blank values, so this takes the fast path.
	return normalize(out, template, defaults{})
}

// isJSON reports whether b's first non-space byte is open.
func isJSON(b []byte, open byte) bool {
	b = bytes.TrimLeft(b, " \t\r\n")
	return len(b) > 0 && b[0] == open
}

// sendResult is the part of a Postmark send response postmarkd inspects.
type sendResult struct {
	ErrorCode int    `json:"ErrorCode"`
	Message   string `json:"Message"`
	To        string `json:"To"`
}

// outcome counts accepted and failed messages in an upstream response, and
// returns the first failure message, if any. Only ErrorCode is decoded on
// the success path.
func outcome(ep endpoint, status int, body []byte, n int) (sent, failed int, firstErr string) {
	if status != 200 {
		var r sendResult
		_ = json.Unmarshal(body, &r)
		if r.Message == "" {
			r.Message = fmt.Sprintf("HTTP %d", status)
		}
		return 0, n, r.Message
	}
	type code struct{ ErrorCode int }
	if ep.kind == kindEmail || ep.kind == kindTemplate {
		var c code
		if json.Unmarshal(body, &c) == nil && c.ErrorCode != 0 {
			var r sendResult
			json.Unmarshal(body, &r)
			return 0, 1, r.Message
		}
		return 1, 0, ""
	}
	var cs []code
	if json.Unmarshal(body, &cs) != nil {
		return n, 0, "" // 200 with an unparseable body; assume accepted
	}
	for i, c := range cs {
		if c.ErrorCode == 0 {
			sent++
			continue
		}
		failed++
		if firstErr == "" {
			var rs []sendResult
			if json.Unmarshal(body, &rs) == nil && i < len(rs) {
				firstErr = rs[i].Message
			}
		}
	}
	return sent, failed, firstErr
}
