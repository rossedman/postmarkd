package server

import (
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

// has reports whether a field is present and not null.
func (m message) has(name string) bool {
	k, ok := m.key(name)
	return ok && string(m[k]) != "null"
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
func prepare(ep endpoint, body []byte, d defaults) ([]byte, int, *apiError) {
	switch ep.kind {
	case kindEmail, kindTemplate:
		var m message
		if json.Unmarshal(body, &m) != nil || m == nil {
			return nil, 0, errInvalidJSON
		}
		if err := normalize(m, ep.kind == kindTemplate, d); err != nil {
			return nil, 0, invalid("%s", err)
		}
		out, err := json.Marshal(m)
		if err != nil {
			return nil, 0, errInvalidJSON
		}
		return out, 1, nil

	case kindBatch:
		var ms []message
		if json.Unmarshal(body, &ms) != nil || ms == nil {
			return nil, 0, errInvalidJSON
		}
		if err := normalizeAll(ms, false, d); err != nil {
			return nil, 0, err
		}
		out, err := json.Marshal(ms)
		if err != nil {
			return nil, 0, errInvalidJSON
		}
		return out, len(ms), nil

	case kindTemplateBatch:
		var wrapper message
		if json.Unmarshal(body, &wrapper) != nil || wrapper == nil {
			return nil, 0, errInvalidJSON
		}
		k, ok := wrapper.key("Messages")
		if !ok {
			return nil, 0, invalid("'Messages' is required")
		}
		var ms []message
		if json.Unmarshal(wrapper[k], &ms) != nil || ms == nil {
			return nil, 0, invalid("'Messages' must be an array of messages")
		}
		if err := normalizeAll(ms, true, d); err != nil {
			return nil, 0, err
		}
		raw, err := json.Marshal(ms)
		if err != nil {
			return nil, 0, errInvalidJSON
		}
		wrapper[k] = raw
		out, err := json.Marshal(wrapper)
		if err != nil {
			return nil, 0, errInvalidJSON
		}
		return out, len(ms), nil
	}
	panic("unknown endpoint kind")
}

func normalizeAll(ms []message, template bool, d defaults) *apiError {
	if len(ms) == 0 {
		return invalid("batch contains no messages")
	}
	if len(ms) > maxBatch {
		return invalid("batch contains %d messages; the maximum is %d", len(ms), maxBatch)
	}
	for i, m := range ms {
		if m == nil {
			return invalid("message %d: expected a JSON object", i)
		}
		if err := normalize(m, template, d); err != nil {
			return invalid("message %d: %s", i, err)
		}
	}
	return nil
}

// normalize applies defaults and performs the cheap validation needed to
// reject obviously broken messages before they reach Postmark (or the async
// queue, where the caller would never see the error).
func normalize(m message, template bool, d defaults) error {
	m.setDefault("From", d.From)
	m.setDefault("MessageStream", d.MessageStream)

	if m.str("From") == "" {
		return fmt.Errorf("'From' is required (or configure POSTMARKD_DEFAULT_FROM)")
	}
	if m.str("To") == "" && m.str("Cc") == "" && m.str("Bcc") == "" {
		return fmt.Errorf("at least one recipient ('To', 'Cc' or 'Bcc') is required")
	}
	if template {
		if !m.has("TemplateId") && m.str("TemplateAlias") == "" {
			return fmt.Errorf("'TemplateId' or 'TemplateAlias' is required")
		}
	} else if m.str("HtmlBody") == "" && m.str("TextBody") == "" {
		return fmt.Errorf("'HtmlBody' or 'TextBody' is required")
	}
	return nil
}

// sendResult is the part of a Postmark send response postmarkd inspects.
type sendResult struct {
	ErrorCode int    `json:"ErrorCode"`
	Message   string `json:"Message"`
	To        string `json:"To"`
}

// outcome counts accepted and failed messages in an upstream response, and
// returns the first failure message, if any.
func outcome(ep endpoint, status int, body []byte, n int) (sent, failed int, firstErr string) {
	if status != 200 {
		var r sendResult
		_ = json.Unmarshal(body, &r)
		if r.Message == "" {
			r.Message = fmt.Sprintf("HTTP %d", status)
		}
		return 0, n, r.Message
	}
	if ep.kind == kindEmail || ep.kind == kindTemplate {
		var r sendResult
		if json.Unmarshal(body, &r) == nil && r.ErrorCode != 0 {
			return 0, 1, r.Message
		}
		return 1, 0, ""
	}
	var rs []sendResult
	if json.Unmarshal(body, &rs) != nil {
		return n, 0, "" // 200 with an unparseable body; assume accepted
	}
	for _, r := range rs {
		if r.ErrorCode == 0 {
			sent++
			continue
		}
		failed++
		if firstErr == "" {
			firstErr = r.Message
		}
	}
	return sent, failed, firstErr
}
