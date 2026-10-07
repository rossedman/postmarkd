package postmark

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// BenchmarkPost measures the upstream client (concurrency slot, pooled
// keep-alive connection, request, buffered response) against a local server.
func BenchmarkPost(b *testing.B) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		io.WriteString(w, `{"ErrorCode":0,"Message":"OK","MessageID":"b7bc2f4a-e38e-4336-af7d-e6c392c2f817"}`)
	}))
	defer srv.Close()
	c := New(Options{BaseURL: srv.URL, ServerToken: "t", MaxConcurrency: 128})
	body := []byte(`{"From":"a@example.com","To":"b@example.com","Subject":"hi","TextBody":"hello"}`)
	b.SetParallelism(8)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			res, err := c.Post(context.Background(), "/email", body)
			if err != nil || res.StatusCode != 200 {
				b.Fatal(err)
			}
		}
	})
}
