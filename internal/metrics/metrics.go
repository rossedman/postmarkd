// Package metrics keeps postmarkd's counters and renders them in the
// Prometheus text exposition format without external dependencies.
package metrics

import (
	"cmp"
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

// counter returns the counter for k in m, creating it on first use.
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

// bucketLabels are the le="…" values, formatted once.
var bucketLabels = func() (l [len(latencyBuckets)]string) {
	for i, b := range latencyBuckets {
		l[i] = strconv.FormatFloat(b, 'g', -1, 64)
	}
	return l
}()

// Write renders all metrics in Prometheus text format. Label values are
// endpoint names and status codes, which never need escaping.
func (m *Metrics) Write(w io.Writer) {
	b := make([]byte, 0, 8<<10)
	b = appendLabeled(b, "postmarkd_http_requests_total", "API requests by endpoint and response status.", &m.requests)
	b = appendLabeled(b, "postmarkd_upstream_requests_total", "Requests to Postmark by endpoint and response status.", &m.upstream)

	const name = "postmarkd_upstream_request_duration_seconds"
	b = append(b, "# HELP "+name+" Latency of requests to Postmark, including retries.\n# TYPE "+name+" histogram\n"...)
	var eps []string
	m.latency.Range(func(k, _ any) bool { eps = append(eps, k.(string)); return true })
	slices.Sort(eps)
	for _, ep := range eps {
		v, _ := m.latency.Load(ep)
		h := v.(*histogram)
		var cum uint64
		for i := range latencyBuckets {
			cum += h.counts[i].Load()
			b = appendSample(b, name+"_bucket", ep, "le", bucketLabels[i], cum)
		}
		cum += h.counts[len(latencyBuckets)].Load()
		b = appendSample(b, name+"_bucket", ep, "le", "+Inf", cum)
		b = append(b, name+`_sum{endpoint="`...)
		b = append(b, ep...)
		b = append(b, `"} `...)
		b = strconv.AppendFloat(b, time.Duration(h.sumNs.Load()).Seconds(), 'g', -1, 64)
		b = append(b, '\n')
		b = appendSample(b, name+"_count", ep, "", "", cum)
	}

	b = appendCounter(b, "postmarkd_messages_sent_total", "Messages accepted by Postmark.", m.MessagesSent.Load())
	b = appendCounter(b, "postmarkd_messages_failed_total", "Messages rejected by Postmark or not delivered to it.", m.MessagesFailed.Load())
	b = appendCounter(b, "postmarkd_upstream_retries_total", "Retried requests to Postmark.", m.Retries.Load())
	b = appendCounter(b, "postmarkd_async_enqueued_total", "Requests accepted onto the async queue.", m.AsyncEnqueued.Load())
	b = appendCounter(b, "postmarkd_async_rejected_total", "Async requests rejected because the queue was full.", m.AsyncRejected.Load())
	b = appendCounter(b, "postmarkd_async_dropped_total", "Queued requests dropped at shutdown.", m.AsyncDropped.Load())

	for _, g := range m.gauges {
		b = appendHeader(b, g.name, g.help, "gauge")
		b = append(append(b, g.name...), ' ')
		b = strconv.AppendFloat(b, g.fn(), 'g', -1, 64)
		b = append(b, '\n')
	}
	b = appendHeader(b, "postmarkd_start_time_seconds", "Unix time the daemon started.", "gauge")
	b = append(b, "postmarkd_start_time_seconds "...)
	b = strconv.AppendInt(b, m.start.Unix(), 10)
	b = append(b, '\n')
	_, _ = w.Write(b) // the scraper went away; nothing to do
}

// appendHeader writes the # HELP and # TYPE lines for a metric.
func appendHeader(b []byte, name, help, typ string) []byte {
	b = append(append(append(b, "# HELP "...), name...), ' ')
	b = append(append(b, help...), '\n')
	b = append(append(append(b, "# TYPE "...), name...), ' ')
	return append(append(b, typ...), '\n')
}

// appendSample writes name{endpoint="ep"[,label="value"]} v.
func appendSample(b []byte, name, ep, label, value string, v uint64) []byte {
	b = append(append(append(b, name...), `{endpoint="`...), ep...)
	if label != "" {
		b = append(append(append(append(append(b, `",`...), label...), `="`...), value...), '"')
	} else {
		b = append(b, '"')
	}
	b = append(b, "} "...)
	b = strconv.AppendUint(b, v, 10)
	return append(b, '\n')
}

// appendLabeled writes a labeled counter family from m, sorted by labels
// so the output is stable.
func appendLabeled(b []byte, name, help string, m *sync.Map) []byte {
	b = appendHeader(b, name, help, "counter")
	type sample struct {
		labels
		v uint64
	}
	samples := make([]sample, 0, 32)
	m.Range(func(k, v any) bool {
		samples = append(samples, sample{k.(labels), v.(*atomic.Uint64).Load()})
		return true
	})
	slices.SortFunc(samples, func(a, b sample) int {
		return cmp.Or(cmp.Compare(a.endpoint, b.endpoint), cmp.Compare(a.code, b.code))
	})
	for _, s := range samples {
		b = appendSample(b, name, s.endpoint, "code", s.code, s.v)
	}
	return b
}

// appendCounter writes an unlabeled counter with its header.
func appendCounter(b []byte, name, help string, v uint64) []byte {
	b = appendHeader(b, name, help, "counter")
	b = append(append(b, name...), ' ')
	b = strconv.AppendUint(b, v, 10)
	return append(b, '\n')
}
