package metrics

import (
	"io"
	"testing"
	"time"
)

// Every request records one request counter and one upstream sample; under
// load these are called from many goroutines at once.
func BenchmarkRecord(b *testing.B) {
	m := New()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			m.Request("email", 200)
			m.Upstream("email", "200", 120*time.Millisecond)
		}
	})
}

func BenchmarkWrite(b *testing.B) {
	m := New()
	for _, ep := range []string{"email", "batch", "template", "template_batch", "smtp"} {
		for _, code := range []int{200, 401, 422, 502} {
			m.Request(ep, code)
		}
		m.Upstream(ep, "200", time.Second)
	}
	b.ReportAllocs()
	for b.Loop() {
		m.Write(io.Discard)
	}
}
