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
	q.Shutdown(context.Background())
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
