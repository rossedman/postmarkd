package blackhole

import (
	"context"
	"encoding/json"
	"testing"
)

func TestBatchAndRing(t *testing.T) {
	s := New(2)
	res, err := s.Post(context.Background(), "/email/batch", []byte(`[{"To":"a@x.com"},{"To":"b@x.com"},{"To":"c@x.com"}]`))
	if err != nil || res.StatusCode != 200 {
		t.Fatal(err, res)
	}
	var rs []result
	json.Unmarshal(res.Body, &rs)
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
