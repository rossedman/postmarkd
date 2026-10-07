# Benchmarks

postmarkd ships Go benchmarks for every component on the path of a message.
They run in CI on every push; on pull requests the same benchmarks are also
run against the base branch and compared with
[benchstat](https://pkg.go.dev/golang.org/x/perf/cmd/benchstat).

## Running them

```sh
make bench > new.txt                        # all benchmarks, 6 runs each
make bench BENCH=Prepare BENCHCOUNT=10      # a subset
go install golang.org/x/perf/cmd/benchstat@latest
benchstat old.txt new.txt                   # compare two runs
scripts/bench-compare.sh old.txt new.txt    # the CI regression gate
```

## The CI regression gate

`scripts/bench-compare.sh` fails a pull request when:

- **allocs/op** rises significantly for any benchmark. Allocation counts
  are deterministic, so any increase is real.
- **sec/op** is significantly worse by more than 25% (`MAX_SLOWDOWN`).
  Shared CI machines vary by 10–20% between runs, so smaller timing changes
  are reported but don't fail the build.

The comparison is written to the job summary of the `bench` job.

## What is measured

| Benchmark | Package | What it measures |
| --- | --- | --- |
| `UnixSocket` | server | End to end: concurrent HTTP clients → unix socket → daemon → instant upstream. Client and daemon share the CPUs, so the daemon alone is faster. |
| `Proxy` | server | The whole relay, including the pooled HTTP client to a local fake Postmark: three HTTP hops in one process. |
| `Handler/Email`, `Handler/Batch500` | server | One request through the API handler (auth, validation, defaults, metrics, response) without networking. |
| `Prepare/*` | server | Parsing, validating and defaulting a request body: the CPU work postmarkd adds to every send. |
| `HandleSMTP` | server | A MIME message received over SMTP converted and sent. |
| `Convert/*` | rfc822 | MIME → Postmark conversion: a cron-style plain text message, and multipart with a 100 KB base64 attachment. |
| `Transaction` | smtpd | A full SMTP transaction (MAIL, RCPT, DATA) on a reused connection. Dominated by network round trips. |
| `Post` | postmark | The upstream client: concurrency slot, pooled keep-alive connection, request, buffered response. |
| `Record`, `Write` | metrics | Recording a request from many goroutines; rendering a Prometheus scrape. |
| `Enqueue` | queue | Handing a job to the async workers from many goroutines. |

## Results

4 vCPU Intel Xeon @ 2.80GHz, Linux, Go 1.24.7. Medians of 6 runs. "Before"
is the first release, before the optimization pass described below.

| Benchmark | Before | After | Change |
| --- | --- | --- | --- |
| UnixSocket | 21,600 msg/s, 200 allocs | 33,700 msg/s, 126 allocs | +56% throughput |
| Proxy | 13,100 msg/s, 283 allocs | 15,400 msg/s, 209 allocs | +17% throughput |
| Handler/Email | 24.7 µs, 134 allocs | 15.9 µs, 60 allocs | −36% |
| Handler/Batch500 | 7.0 ms, 4.1 MB | 4.3 ms, 1.9 MB | −38% |
| Prepare/Email | 17.8 µs, 66 allocs | 4.4 µs, 6 allocs | −75% |
| Prepare/EmailWithDefaults | 18.1 µs, 68 allocs | 5.1 µs, 12 allocs | −72% |
| Prepare/Batch500 | 9.7 ms, 2.0 MB | 4.0 ms, 172 KB | −59% |
| HandleSMTP | 35.6 µs, 182 allocs | 22.0 µs, 131 allocs | −38% |
| Convert/PlainText | 14.9 µs, 12.7 KB | 12.0 µs, 7.3 KB | −20% |
| Convert/MultipartWith100KBAttachment | 1.25 ms, 812 KB | 0.71 ms, 309 KB | −43% |
| metrics Record | 293 ns, 1 alloc | 215 ns, 0 allocs | −27% |
| metrics Write | 67 µs, 386 allocs | 13 µs, 5 allocs | −80% |
| postmark Post | 31 µs | 32 µs | unchanged (net/http) |
| queue Enqueue | 293 ns | 309 ns | unchanged |
| smtpd Transaction | 47 µs | 41 µs | unchanged (network bound) |

## What was optimized

- **Request preparation** used to decode every message into a map and
  encode it again. It now scans each message once, recording only whether the
  fields it checks are present and non-blank, without copying their values,
  and forwards the original bytes untouched. Defaults are spliced in after
  the opening brace only when needed. Batch elements are referenced in place
  rather than copied.
- **Logging** no longer calls `slog.Logger.With` per request, which formats
  attributes even when nothing is logged. Request attributes are added only
  when a record is actually written.
- **Response inspection** decodes only `ErrorCode` from Postmark's response
  on the success path.
- **Request IDs** use `math/rand/v2` instead of `crypto/rand`; they only
  correlate logs.
- **Metrics** recording is lock-free (`sync.Map` of atomic counters, atomic
  histogram buckets), so request handlers never contend on a mutex. Scrapes
  append to one buffer instead of hundreds of `fmt.Fprintf` calls.
- **MIME conversion** reads all parts into one reused buffer, passes base64
  attachments through after validating them instead of decoding and
  re-encoding, and no longer builds a `strings.Replacer` per address.

## Reading the numbers

These benchmarks use an instant fake Postmark, so they show postmarkd's own
overhead. Real sends wait on Postmark (typically 100–500 ms), which is why
throughput in production is set by `POSTMARKD_MAX_CONCURRENCY`, batching and
your Postmark account's limits rather than by postmarkd. See
[Scaling and tuning](deployment.md#scaling-and-tuning).
