package blackhole

import (
	"context"
	"encoding/json"
	"regexp"
	"sync"
	"testing"
	"time"
)

func TestBatchAndRing(t *testing.T) {
	s := New(2)
	res, err := s.Post(context.Background(), "/email/batch", []byte(`[{"To":"a@x.com"},{"To":"b@x.com"},{"To":"c@x.com"}]`))
	if err != nil || res.StatusCode != 200 {
		t.Fatal(err, res)
	}
	var rs []result
	if err := json.Unmarshal(res.Body, &rs); err != nil {
		t.Fatal(err)
	}
	if len(rs) != 3 || rs[2].To != "c@x.com" || rs[0].MessageID == "" {
		t.Fatalf("results %+v", rs)
	}
	got := s.Messages()
	if len(got) != 2 || string(got[0].Message) != `{"To":"b@x.com"}` || string(got[1].Message) != `{"To":"c@x.com"}` {
		t.Fatalf("ring = %+v", got)
	}
	s.Reset()
	if len(s.Messages()) != 0 {
		t.Fatal("reset failed")
	}
}

func TestSingle(t *testing.T) {
	s := New(10)
	res, _ := s.Post(context.Background(), "/email", []byte(`{"To":"a@x.com"}`))
	var r result
	if json.Unmarshal(res.Body, &r) != nil || r.To != "a@x.com" || r.ErrorCode != 0 {
		t.Fatalf("%s", res.Body)
	}
}

func TestPostEndpoints(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, path, body string
		wantErr          bool
		status           int
		batch            bool
		to               []string
	}{
		{"email", "/email", `{"To":"a@x.com"}`, false, 200, false, []string{"a@x.com"}},
		{"template", "/email/withTemplate", `{"To":"t@x.com","TemplateId":1}`, false, 200, false, []string{"t@x.com"}},
		{"single without To", "/email", `{"Cc":"c@x.com"}`, false, 200, false, []string{""}},
		{"batch", "/email/batch", `[{"To":"a@x.com"},{"To":"b@x.com"}]`, false, 200, true, []string{"a@x.com", "b@x.com"}},
		{"empty batch", "/email/batch", `[]`, false, 200, true, []string{}},
		{"template batch", "/email/batchWithTemplates", `{"Messages":[{"To":"a@x.com"},{"To":"b@x.com"}]}`, false, 200, true, []string{"a@x.com", "b@x.com"}},
		{"bad batch", "/email/batch", `{"To":"a@x.com"}`, true, 0, false, nil},
		{"bad template batch", "/email/batchWithTemplates", `[{"To":"a@x.com"}]`, true, 0, false, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := New(10)
			res, err := s.Post(t.Context(), c.path, []byte(c.body))
			if c.wantErr {
				if err == nil {
					t.Fatalf("accepted %s: %s", c.body, res.Body)
				}
				if n := len(s.Messages()); n != 0 {
					t.Errorf("stored %d messages from a rejected request", n)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if res.StatusCode != c.status || res.ContentType != "application/json" || res.Attempts != 1 {
				t.Fatalf("response = %+v", res)
			}
			var rs []result
			if c.batch {
				if err := json.Unmarshal(res.Body, &rs); err != nil {
					t.Fatalf("batch body %s: %v", res.Body, err)
				}
			} else {
				var r result
				if err := json.Unmarshal(res.Body, &r); err != nil {
					t.Fatalf("single body %s: %v", res.Body, err)
				}
				rs = []result{r}
			}
			got := s.Messages()
			if len(rs) != len(c.to) || len(got) != len(c.to) {
				t.Fatalf("results %d, stored %d, want %d", len(rs), len(got), len(c.to))
			}
			for i, r := range rs {
				if r.To != c.to[i] || r.ErrorCode != 0 || r.Message != "OK" {
					t.Errorf("result %d = %+v", i, r)
				}
				if _, err := time.Parse(time.RFC3339Nano, r.SubmittedAt); err != nil {
					t.Errorf("result %d SubmittedAt %q: %v", i, r.SubmittedAt, err)
				}
				if got[i].MessageID != r.MessageID || got[i].Path != c.path {
					t.Errorf("stored %d = %+v, result %+v", i, got[i], r)
				}
			}
		})
	}
}

func TestPostUnknownPath(t *testing.T) {
	t.Parallel()
	s := New(10)
	res, err := s.Post(t.Context(), "/bounces", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var e result
	if res.StatusCode != 404 || json.Unmarshal(res.Body, &e) != nil || e.ErrorCode != 404 || e.Message != "not found" {
		t.Fatalf("response = %d %s", res.StatusCode, res.Body)
	}
	if len(s.Messages()) != 0 {
		t.Error("unknown path stored a message")
	}
}

func TestKeepZero(t *testing.T) {
	t.Parallel()
	s := New(0)
	res, err := s.Post(t.Context(), "/email/batch", []byte(`[{"To":"a@x.com"},{"To":"b@x.com"}]`))
	if err != nil || res.StatusCode != 200 {
		t.Fatal(err, res)
	}
	if got := s.Messages(); len(got) != 0 {
		t.Fatalf("keep=0 stored %d messages", len(got))
	}
	s.Reset()
}

func TestRingOrder(t *testing.T) {
	t.Parallel()
	for n := range 8 {
		s := New(3)
		for i := range n {
			body, _ := json.Marshal(map[string]int{"N": i})
			if _, err := s.Post(t.Context(), "/email", body); err != nil {
				t.Fatal(err)
			}
		}
		got := s.Messages()
		want := min(n, 3)
		if len(got) != want {
			t.Fatalf("after %d posts: %d messages, want %d", n, len(got), want)
		}
		for i, m := range got {
			var v struct{ N int }
			if err := json.Unmarshal(m.Message, &v); err != nil {
				t.Fatal(err)
			}
			if wantN := n - want + i; v.N != wantN {
				t.Errorf("after %d posts: message %d is #%d, want #%d", n, i, v.N, wantN)
			}
		}
		// Messages returns a copy.
		if len(got) > 0 {
			got[0].Path = "mutated"
			if s.Messages()[0].Path == "mutated" {
				t.Error("Messages exposes the internal buffer")
			}
		}
	}
}

func TestResetThenReuse(t *testing.T) {
	t.Parallel()
	s := New(2)
	for range 3 {
		_, _ = s.Post(t.Context(), "/email", []byte(`{"To":"old@x.com"}`))
	}
	s.Reset()
	_, _ = s.Post(t.Context(), "/email", []byte(`{"To":"new@x.com"}`))
	got := s.Messages()
	if len(got) != 1 || string(got[0].Message) != `{"To":"new@x.com"}` {
		t.Fatalf("after reset: %+v", got)
	}
}

func TestConcurrentPost(t *testing.T) {
	t.Parallel()
	s := New(50)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 20 {
				if _, err := s.Post(context.Background(), "/email", []byte(`{"To":"a@x.com"}`)); err != nil {
					t.Error(err)
				}
				_ = s.Messages()
			}
		})
	}
	wg.Wait()
	got := s.Messages()
	if len(got) != 50 {
		t.Fatalf("stored %d, want 50", len(got))
	}
	ids := map[string]bool{}
	for _, m := range got {
		if ids[m.MessageID] {
			t.Errorf("duplicate MessageID %s", m.MessageID)
		}
		ids[m.MessageID] = true
	}
}

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestUUID(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for range 1000 {
		id := uuid()
		if !uuidRE.MatchString(id) {
			t.Fatalf("uuid %q is not an RFC 4122 version 4 UUID", id)
		}
		if seen[id] {
			t.Fatalf("duplicate uuid %q", id)
		}
		seen[id] = true
	}
}
