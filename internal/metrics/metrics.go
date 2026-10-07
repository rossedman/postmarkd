// Package metrics keeps postmarkd's counters and renders them in the
// Prometheus text exposition format without external dependencies.
package metrics

import (
	"cmp"
	"fmt"
	"io"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

var latencyBuckets = [...]float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

type labels struct{ endpoint, code string }

// histogram is updated with atomics only.
type histogram struct {
	counts [len(latencyBuckets) + 1]atomic.Uint64 // non-cumulative; last is +Inf
	sumNs  atomic.Int64
}

// Metrics is safe for concurrent use. Recording is lock-free: counters
// are created once per label set and then updated atomically, so request
// handlers never contend on a mutex.
type Metrics struct {
	requests sync.Map // labels -> *atomic.Uint64
	upstream sync.Map // labels -> *atomic.Uint64
	latency  sync.Map // endpoint -> *histogram

	MessagesSent   atomic.Uint64
	MessagesFailed atomic.Uint64
	Retries        atomic.Uint64
	AsyncEnqueued  atomic.Uint64
	AsyncRejected  atomic.Uint64
	AsyncDropped   atomic.Uint64

	gauges []gauge
	start  time.Time
}

type gauge struct {
	name, help string
	fn         func() float64
}

// New returns an empty registry.
func New() *Metrics {
	return &Metrics{start: time.Now()}
}

// Gauge registers a gauge evaluated at scrape time. Not safe to call
// concurrently with Write; register gauges during setup.
func (m *Metrics) Gauge(name, help string, fn func() float64) {
	m.gauges = append(m.gauges, gauge{name, help, fn})
}

// statusText avoids an allocation for common HTTP statuses.
var statusText = func() map[int]string {
	m := map[int]string{}
	for _, c := range []int{200, 202, 204, 400, 401, 404, 405, 413, 422, 429, 499, 500, 502, 503, 504} {
		m[c] = strconv.Itoa(c)
	}
	return m
}()

func counter(m *sync.Map, k labels) *atomic.Uint64 {
	if c, ok := m.Load(k); ok {
		return c.(*atomic.Uint64)
	}
	c, _ := m.LoadOrStore(k, new(atomic.Uint64))
	return c.(*atomic.Uint64)
}

// Request counts an API request by endpoint and HTTP status.
func (m *Metrics) Request(endpoint string, status int) {
	code, ok := statusText[status]
	if !ok {
		code = strconv.Itoa(status)
	}
	counter(&m.requests, labels{endpoint, code}).Add(1)
}

// Upstream records a call to Postmark. code is the HTTP status, or "error"
// for transport failures.
func (m *Metrics) Upstream(endpoint, code string, d time.Duration) {
	counter(&m.upstream, labels{endpoint, code}).Add(1)
	v, ok := m.latency.Load(endpoint)
	if !ok {
		v, _ = m.latency.LoadOrStore(endpoint, new(histogram))
	}
	h := v.(*histogram)
	i, _ := slices.BinarySearch(latencyBuckets[:], d.Seconds())
	h.counts[i].Add(1)
	h.sumNs.Add(int64(d))
}

// Write renders all metrics in Prometheus text format.
func (m *Metrics) Write(w io.Writer) {
	writeLabeled(w, "postmarkd_http_requests_total", "API requests by endpoint and response status.", "code", &m.requests)
	writeLabeled(w, "postmarkd_upstream_requests_total", "Requests to Postmark by endpoint and response status.", "code", &m.upstream)

	name := "postmarkd_upstream_request_duration_seconds"
	fmt.Fprintf(w, "# HELP %s Latency of requests to Postmark, including retries.\n# TYPE %s histogram\n", name, name)
	var eps []string
	m.latency.Range(func(k, _ any) bool { eps = append(eps, k.(string)); return true })
	slices.Sort(eps)
	for _, ep := range eps {
		v, _ := m.latency.Load(ep)
		h := v.(*histogram)
		var cum uint64
		for i, b := range latencyBuckets {
			cum += h.counts[i].Load()
			fmt.Fprintf(w, "%s_bucket{endpoint=%q,le=%q} %d\n", name, ep, strconv.FormatFloat(b, 'g', -1, 64), cum)
		}
		cum += h.counts[len(latencyBuckets)].Load()
		fmt.Fprintf(w, "%s_bucket{endpoint=%q,le=\"+Inf\"} %d\n", name, ep, cum)
		fmt.Fprintf(w, "%s_sum{endpoint=%q} %g\n%s_count{endpoint=%q} %d\n", name, ep, time.Duration(h.sumNs.Load()).Seconds(), name, ep, cum)
	}

	writeCounter(w, "postmarkd_messages_sent_total", "Messages accepted by Postmark.", m.MessagesSent.Load())
	writeCounter(w, "postmarkd_messages_failed_total", "Messages rejected by Postmark or not delivered to it.", m.MessagesFailed.Load())
	writeCounter(w, "postmarkd_upstream_retries_total", "Retried requests to Postmark.", m.Retries.Load())
	writeCounter(w, "postmarkd_async_enqueued_total", "Requests accepted onto the async queue.", m.AsyncEnqueued.Load())
	writeCounter(w, "postmarkd_async_rejected_total", "Async requests rejected because the queue was full.", m.AsyncRejected.Load())
	writeCounter(w, "postmarkd_async_dropped_total", "Queued requests dropped at shutdown.", m.AsyncDropped.Load())

	for _, g := range m.gauges {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %g\n", g.name, g.help, g.name, g.name, g.fn())
	}
	fmt.Fprintf(w, "# HELP postmarkd_start_time_seconds Unix time the daemon started.\n# TYPE postmarkd_start_time_seconds gauge\npostmarkd_start_time_seconds %d\n", m.start.Unix())
}

func writeLabeled(w io.Writer, name, help, label string, m *sync.Map) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
	vals := map[labels]uint64{}
	var keys []labels
	m.Range(func(k, v any) bool {
		vals[k.(labels)] = v.(*atomic.Uint64).Load()
		keys = append(keys, k.(labels))
		return true
	})
	slices.SortFunc(keys, func(a, b labels) int {
		return cmp.Or(cmp.Compare(a.endpoint, b.endpoint), cmp.Compare(a.code, b.code))
	})
	for _, k := range keys {
		fmt.Fprintf(w, "%s{endpoint=%q,%s=%q} %d\n", name, k.endpoint, label, k.code, vals[k])
	}
}

func writeCounter(w io.Writer, name, help string, v uint64) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, v)
}
