package nppa

import (
	"context"
	"errors"
	"testing"
	"time"
)

func newTestReporter(send func(context.Context, []Behavior) ([]int, error), now time.Time) *Reporter {
	return &Reporter{send: send, queue: make(chan Behavior, 4), interval: time.Second, maxAge: reportMaxAge,
		now: func() time.Time { return now }}
}

func TestFlushDropsExpiredAndSendsAtMostOneBatch(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	var sent [][]Behavior
	r := newTestReporter(func(_ context.Context, batch []Behavior) ([]int, error) {
		sent = append(sent, append([]Behavior(nil), batch...))
		return nil, nil
	}, now)

	pending := []Behavior{{SI: "old", OT: now.Add(-reportMaxAge - time.Second).Unix()}}
	for i := 0; i < MaxBatch+5; i++ {
		pending = append(pending, Behavior{SI: "s", OT: now.Unix()})
	}
	left := r.flush(context.Background(), pending)
	if len(sent) != 1 || len(sent[0]) != MaxBatch || sent[0][0].SI != "s" {
		t.Fatalf("first flush must send one batch of %d fresh behaviors, sent %d batches", MaxBatch, len(sent))
	}
	if len(left) != 5 {
		t.Fatalf("left = %d, want 5", len(left))
	}
}

func TestFlushKeepsTheBatchWhenSendFails(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	r := newTestReporter(func(context.Context, []Behavior) ([]int, error) {
		return nil, errors.New("network down")
	}, now)
	pending := []Behavior{{SI: "a", OT: now.Unix()}, {SI: "b", OT: now.Unix()}}
	if left := r.flush(context.Background(), pending); len(left) != 2 {
		t.Fatalf("a failed batch must stay pending for retry, left = %d", len(left))
	}
}

func TestFlushDropsRejectedBehaviors(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	r := newTestReporter(func(context.Context, []Behavior) ([]int, error) { return []int{1}, nil }, now)
	if left := r.flush(context.Background(), []Behavior{{SI: "a", OT: now.Unix()}}); len(left) != 0 {
		t.Fatalf("rejected behaviors are not retried, left = %d", len(left))
	}
}

func TestReportNeverBlocks(t *testing.T) {
	r := newTestReporter(nil, time.Now())
	for i := 0; i < cap(r.queue)+3; i++ {
		r.Report(Behavior{SI: "s"}) // 队列满了就丢,不阻塞
	}
	if len(r.queue) != cap(r.queue) {
		t.Fatalf("queue = %d, want full %d", len(r.queue), cap(r.queue))
	}
}
