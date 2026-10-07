// Package queue is a bounded, in-memory work queue drained by a fixed pool
// of workers. It gives callers fast, non-blocking acceptance of mail and
// back-pressure (rejection) when the daemon is saturated.
package queue

import (
	"context"
	"sync"
	"time"
)

// Job is a unit of queued work.
type Job struct {
	ID       string
	Path     string // upstream API path, e.g. "/email/batch"
	Endpoint string // metrics label
	Body     []byte
	Messages int
	Enqueued time.Time
}

// HandlerFunc processes a job. ctx is cancelled if shutdown times out.
type HandlerFunc func(ctx context.Context, j Job)

// Queue is safe for concurrent use.
type Queue struct {
	mu     sync.RWMutex
	closed bool
	jobs   chan Job
	wg     sync.WaitGroup
	ctx    context.Context
	cancel context.CancelFunc
}

// New starts workers goroutines that call handle for every enqueued job.
func New(size, workers int, handle HandlerFunc) *Queue {
	ctx, cancel := context.WithCancel(context.Background())
	q := &Queue{jobs: make(chan Job, size), ctx: ctx, cancel: cancel}
	q.wg.Add(workers)
	for range workers {
		go func() {
			defer q.wg.Done()
			for j := range q.jobs {
				handle(q.ctx, j)
			}
		}()
	}
	return q
}

// Enqueue adds j without blocking. It returns false if the queue is full or
// shutting down.
func (q *Queue) Enqueue(j Job) bool {
	q.mu.RLock()
	defer q.mu.RUnlock()
	if q.closed {
		return false
	}
	select {
	case q.jobs <- j:
		return true
	default:
		return false
	}
}

// Len is the number of jobs waiting for a worker.
func (q *Queue) Len() int { return len(q.jobs) }

// Cap is the queue capacity.
func (q *Queue) Cap() int { return cap(q.jobs) }

// Shutdown stops accepting jobs and waits for queued jobs to be processed.
// If ctx expires first, in-flight work is cancelled, the remaining jobs are
// handed to the workers with a cancelled context (so they can be logged as
// dropped), and ctx.Err() is returned.
func (q *Queue) Shutdown(ctx context.Context) error {
	q.mu.Lock()
	if !q.closed {
		q.closed = true
		close(q.jobs)
	}
	q.mu.Unlock()

	done := make(chan struct{})
	go func() { q.wg.Wait(); close(done) }()
	select {
	case <-done:
		q.cancel()
		return nil
	case <-ctx.Done():
		q.cancel()
		<-done
		return ctx.Err()
	}
}
