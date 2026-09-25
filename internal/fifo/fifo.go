// Package fifo provides an unbounded FIFO queue. Push never blocks, so store
// observers (which run on the coordinator goroutine) and operation pumps can
// hand work to an actor without ever stalling the harness loop.
package fifo

import (
	"context"
	"sync"
)

// Queue is an unbounded, goroutine-safe FIFO.
type Queue[T any] struct {
	mu     sync.Mutex
	items  []T
	signal chan struct{}
}

// New returns an empty queue.
func New[T any]() *Queue[T] {
	return &Queue[T]{signal: make(chan struct{}, 1)}
}

// Push appends item without blocking.
func (q *Queue[T]) Push(item T) {
	q.mu.Lock()
	q.items = append(q.items, item)
	q.mu.Unlock()
	select {
	case q.signal <- struct{}{}:
	default:
	}
}

// Pop blocks until an item is available or ctx ends; ok is false on ctx end.
func (q *Queue[T]) Pop(ctx context.Context) (item T, ok bool) {
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			item = q.items[0]
			var zero T
			q.items[0] = zero
			q.items = q.items[1:]
			q.mu.Unlock()
			return item, true
		}
		q.mu.Unlock()
		select {
		case <-q.signal:
		case <-ctx.Done():
			var zero T
			return zero, false
		}
	}
}
