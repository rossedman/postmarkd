package queue

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestDrainOnShutdown(t *testing.T) {
	var done atomic.Int32
	q := New(100, 4, func(ctx context.Context, j Job) {
		time.Sleep(time.Millisecond)
		done.Add(1)
	})
	for i := range 50 {
		if !q.Enqueue(Job{ID: string(rune('a' + i%26))}) {
			t.Fatal("enqueue failed")
		}
	}
	if err := q.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if done.Load() != 50 {
		t.Fatalf("processed %d of 50", done.Load())
	}
	if q.Enqueue(Job{}) {
		t.Fatal("enqueue after shutdown should fail")
	}
}

func TestFullQueueRejects(t *testing.T) {
	block := make(chan struct{})
	q := New(1, 1, func(ctx context.Context, j Job) { <-block })
	q.Enqueue(Job{}) // picked up by the worker
	time.Sleep(10 * time.Millisecond)
	if !q.Enqueue(Job{}) { // fills the buffer
		t.Fatal("second enqueue should fit in the buffer")
	}
	if q.Enqueue(Job{}) {
		t.Fatal("enqueue into a full queue should fail")
	}
	close(block)
	if err := q.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestShutdownTimeoutCancels(t *testing.T) {
	var cancelled atomic.Int32
	q := New(10, 1, func(ctx context.Context, j Job) {
		select {
		case <-ctx.Done():
			cancelled.Add(1)
		case <-time.After(5 * time.Second):
		}
	})
	q.Enqueue(Job{})
	q.Enqueue(Job{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := q.Shutdown(ctx); err == nil {
		t.Fatal("expected timeout error")
	}
	if cancelled.Load() != 2 {
		t.Fatalf("cancelled = %d, want 2", cancelled.Load())
	}
}

func TestLenAndCap(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	release := make(chan struct{})
	q := New(3, 1, func(ctx context.Context, j Job) {
		started <- struct{}{}
		<-release
	})
	if q.Cap() != 3 || q.Len() != 0 {
		t.Fatalf("new queue: Len=%d Cap=%d", q.Len(), q.Cap())
	}
	if !q.Enqueue(Job{ID: "running"}) {
		t.Fatal("enqueue failed")
	}
	<-started // the worker holds the first job, so the buffer is empty again
	for i := range 3 {
		if !q.Enqueue(Job{ID: "waiting"}) {
			t.Fatalf("enqueue %d failed", i)
		}
		if q.Len() != i+1 {
			t.Fatalf("Len = %d, want %d", q.Len(), i+1)
		}
	}
	if q.Enqueue(Job{}) {
		t.Fatal("enqueue into a full queue should fail")
	}
	if q.Cap() != 3 {
		t.Fatalf("Cap = %d", q.Cap())
	}
	go func() {
		for range 3 {
			<-started
		}
	}()
	close(release)
	if err := q.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if q.Len() != 0 {
		t.Fatalf("Len after drain = %d", q.Len())
	}
}

func TestShutdownIsIdempotent(t *testing.T) {
	t.Parallel()
	var handled atomic.Int32
	q := New(4, 2, func(ctx context.Context, j Job) {
		if j.ID != "x" || j.Path != "/email" || string(j.Body) != "{}" {
			t.Errorf("job not delivered intact: %+v", j)
		}
		handled.Add(1)
	})
	if !q.Enqueue(Job{ID: "x", Path: "/email", Body: []byte("{}")}) {
		t.Fatal("enqueue failed")
	}
	for range 2 {
		if err := q.Shutdown(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if handled.Load() != 1 {
		t.Fatalf("handled = %d", handled.Load())
	}
}

func TestHandlerContextCancelledAfterCleanShutdown(t *testing.T) {
	t.Parallel()
	ctxs := make(chan context.Context, 1)
	q := New(1, 1, func(ctx context.Context, j Job) { ctxs <- ctx })
	q.Enqueue(Job{})
	ctx := <-ctxs
	if ctx.Err() != nil {
		t.Fatal("handler context cancelled while running")
	}
	if err := q.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() == nil {
		t.Fatal("handler context should be cancelled after shutdown")
	}
}

func TestConcurrentEnqueueAndShutdown(t *testing.T) {
	t.Parallel()
	var accepted, handled atomic.Int32
	q := New(64, 4, func(context.Context, Job) { handled.Add(1) })
	start := make(chan struct{})
	done := make(chan struct{})
	for range 8 {
		go func() {
			defer func() { done <- struct{}{} }()
			<-start
			for range 100 {
				if q.Enqueue(Job{}) {
					accepted.Add(1)
				}
			}
		}()
	}
	close(start)
	if err := q.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	for range 8 {
		<-done
	}
	if handled.Load() != accepted.Load() {
		t.Fatalf("handled %d of %d accepted jobs", handled.Load(), accepted.Load())
	}
}
