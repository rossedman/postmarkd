package queue

import (
	"context"
	"testing"
)

// BenchmarkEnqueue measures handing a job to the worker pool from many
// concurrent request handlers.
func BenchmarkEnqueue(b *testing.B) {
	q := New(1<<16, 32, func(context.Context, Job) {})
	defer q.Shutdown(context.Background())
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			for !q.Enqueue(Job{Path: "/email"}) {
			}
		}
	})
}
