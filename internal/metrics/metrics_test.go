package metrics

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWrite(t *testing.T) {
	m := New()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 1000 {
				m.Request("email", 200)
				m.Upstream("email", "200", 75*time.Millisecond)
			}
		}()
	}
	wg.Wait()
	m.Request("email", 299)
	m.Upstream("batch", "error", time.Minute)
	m.Gauge("x_gauge", "help", func() float64 { return 3 })

	var b strings.Builder
	m.Write(&b)
	out := b.String()
	for _, want := range []string{
		`postmarkd_http_requests_total{endpoint="email",code="200"} 8000`,
		`postmarkd_http_requests_total{endpoint="email",code="299"} 1`,
		`postmarkd_upstream_requests_total{endpoint="batch",code="error"} 1`,
		`postmarkd_upstream_request_duration_seconds_bucket{endpoint="email",le="0.05"} 0`,
		`postmarkd_upstream_request_duration_seconds_bucket{endpoint="email",le="0.1"} 8000`,
		`postmarkd_upstream_request_duration_seconds_bucket{endpoint="email",le="+Inf"} 8000`,
		`postmarkd_upstream_request_duration_seconds_count{endpoint="email"} 8000`,
		`postmarkd_upstream_request_duration_seconds_sum{endpoint="email"} 600`,
		`postmarkd_upstream_request_duration_seconds_bucket{endpoint="batch",le="30"} 0`,
		`postmarkd_upstream_request_duration_seconds_bucket{endpoint="batch",le="+Inf"} 1`,
		"x_gauge 3",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Index(out, `endpoint="batch",le="+Inf"`) > strings.Index(out, `endpoint="email",le="0.05"`) {
		t.Error("histograms not sorted by endpoint")
	}
}
