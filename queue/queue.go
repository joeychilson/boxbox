package queue

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// ErrQueueClosed is returned when trying to acquire from a closed queue.
	ErrQueueClosed = errors.New("queue is closed")
	// ErrQueueTimeout is returned when waiting in queue times out.
	ErrQueueTimeout = errors.New("queue wait timeout exceeded")
)

// Queue manages concurrent execution slots with queuing.
type Queue struct {
	sem          chan struct{}
	maxWait      time.Duration
	closed       atomic.Bool
	closedCh     chan struct{}
	mu           sync.Mutex
	waitingCount atomic.Int64
	activeCount  atomic.Int64
	totalCount   atomic.Int64
	timeoutCount atomic.Int64
}

// Metrics contains queue metrics.
type Metrics struct {
	Waiting  int64 // Currently waiting in queue
	Active   int64 // Currently executing
	Total    int64 // Total requests processed
	Timeouts int64 // Requests that timed out waiting
}

// New creates a new execution queue.
func New(maxConcurrent int, maxWait time.Duration) *Queue {
	return &Queue{
		sem:      make(chan struct{}, maxConcurrent),
		maxWait:  maxWait,
		closedCh: make(chan struct{}),
	}
}

// Acquire waits for an execution slot. Returns nil on success, or an error
// if the queue is closed, the context is cancelled, or the wait times out.
func (q *Queue) Acquire(ctx context.Context) error {
	if q.closed.Load() {
		return ErrQueueClosed
	}

	q.totalCount.Add(1)
	q.waitingCount.Add(1)
	defer q.waitingCount.Add(-1)

	timer := time.NewTimer(q.maxWait)
	defer timer.Stop()

	select {
	case q.sem <- struct{}{}:
		q.activeCount.Add(1)
		return nil
	case <-timer.C:
		q.timeoutCount.Add(1)
		return ErrQueueTimeout
	case <-ctx.Done():
		return ctx.Err()
	case <-q.closedCh:
		return ErrQueueClosed
	}
}

// Release releases an execution slot back to the queue.
func (q *Queue) Release() {
	<-q.sem
	q.activeCount.Add(-1)
}

// Close closes the queue, rejecting any new requests.
func (q *Queue) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.closed.Load() {
		return
	}

	q.closed.Store(true)
	close(q.closedCh)
}

// GetMetrics returns current queue metrics.
func (q *Queue) GetMetrics() Metrics {
	return Metrics{
		Waiting:  q.waitingCount.Load(),
		Active:   q.activeCount.Load(),
		Total:    q.totalCount.Load(),
		Timeouts: q.timeoutCount.Load(),
	}
}

// IsClosed returns whether the queue is closed.
func (q *Queue) IsClosed() bool {
	return q.closed.Load()
}
