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

var latencyBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

type labels struct{ endpoint, code string }

type histogram struct {
	counts []uint64 // per bucket, non-cumulative; last is +Inf
	sum    float64
	count  uint64
}

// Metrics is safe for concurrent use.
type Metrics struct {
	mu       sync.Mutex
	requests map[labels]uint64
	upstream map[labels]uint64
	latency  map[string]*histogram

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
	return &Metrics{
		requests: map[labels]uint64{},
		upstream: map[labels]uint64{},
		latency:  map[string]*histogram{},
		start:    time.Now(),
	}
}

// Gauge registers a gauge evaluated at scrape time. Not safe to call
// concurrently with Write; register gauges during setup.
func (m *Metrics) Gauge(name, help string, fn func() float64) {
	m.gauges = append(m.gauges, gauge{name, help, fn})
}

// Request counts an API request by endpoint and HTTP status.
func (m *Metrics) Request(endpoint string, status int) {
	m.mu.Lock()
	m.requests[labels{endpoint, strconv.Itoa(status)}]++
	m.mu.Unlock()
}

// Upstream records a call to Postmark. code is the HTTP status, or "error"
// for transport failures.
func (m *Metrics) Upstream(endpoint, code string, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.upstream[labels{endpoint, code}]++
	h := m.latency[endpoint]
	if h == nil {
		h = &histogram{counts: make([]uint64, len(latencyBuckets)+1)}
		m.latency[endpoint] = h
	}
	s := d.Seconds()
	i, _ := slices.BinarySearch(latencyBuckets, s)
	h.counts[i]++
	h.sum += s
	h.count++
}

// Write renders all metrics in Prometheus text format.
func (m *Metrics) Write(w io.Writer) {
	m.mu.Lock()
	defer m.mu.Unlock()

	writeLabeled(w, "postmarkd_http_requests_total", "API requests by endpoint and response status.", "code", m.requests)
	writeLabeled(w, "postmarkd_upstream_requests_total", "Requests to Postmark by endpoint and response status.", "code", m.upstream)

	name := "postmarkd_upstream_request_duration_seconds"
	fmt.Fprintf(w, "# HELP %s Latency of requests to Postmark, including retries.\n# TYPE %s histogram\n", name, name)
	for _, ep := range sortedKeys(m.latency) {
		h := m.latency[ep]
		var cum uint64
		for i, b := range latencyBuckets {
			cum += h.counts[i]
			fmt.Fprintf(w, "%s_bucket{endpoint=%q,le=%q} %d\n", name, ep, strconv.FormatFloat(b, 'g', -1, 64), cum)
		}
		fmt.Fprintf(w, "%s_bucket{endpoint=%q,le=\"+Inf\"} %d\n", name, ep, h.count)
		fmt.Fprintf(w, "%s_sum{endpoint=%q} %g\n%s_count{endpoint=%q} %d\n", name, ep, h.sum, name, ep, h.count)
	}

	counter(w, "postmarkd_messages_sent_total", "Messages accepted by Postmark.", m.MessagesSent.Load())
	counter(w, "postmarkd_messages_failed_total", "Messages rejected by Postmark or not delivered to it.", m.MessagesFailed.Load())
	counter(w, "postmarkd_upstream_retries_total", "Retried requests to Postmark.", m.Retries.Load())
	counter(w, "postmarkd_async_enqueued_total", "Requests accepted onto the async queue.", m.AsyncEnqueued.Load())
	counter(w, "postmarkd_async_rejected_total", "Async requests rejected because the queue was full.", m.AsyncRejected.Load())
	counter(w, "postmarkd_async_dropped_total", "Queued requests dropped at shutdown.", m.AsyncDropped.Load())

	for _, g := range m.gauges {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %g\n", g.name, g.help, g.name, g.name, g.fn())
	}
	fmt.Fprintf(w, "# HELP postmarkd_start_time_seconds Unix time the daemon started.\n# TYPE postmarkd_start_time_seconds gauge\npostmarkd_start_time_seconds %d\n", m.start.Unix())
}

func writeLabeled(w io.Writer, name, help, label string, vals map[labels]uint64) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
	keys := make([]labels, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b labels) int {
		return cmp.Or(cmp.Compare(a.endpoint, b.endpoint), cmp.Compare(a.code, b.code))
	})
	for _, k := range keys {
		fmt.Fprintf(w, "%s{endpoint=%q,%s=%q} %d\n", name, k.endpoint, label, k.code, vals[k])
	}
}

func counter(w io.Writer, name, help string, v uint64) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, v)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
