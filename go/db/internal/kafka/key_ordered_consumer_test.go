// Unit tests for the key-ordered Kafka consumer's batch-coalescing and
// retry-routing logic.
//
// SCOPE — what these tests do, and what they don't:
//
//   - They test `worker.processTaskBatch` and the `dbOpHandlers` dispatch path
//     IN ISOLATION: no real Kafka, no real MySQL, no real consumer-group setup.
//   - They use miniredis for the Redis side (write-back, retry queue).
//   - They REPLACE `dbOpHandlers` with recording handlers so tests can assert
//     "what would have been written to the DB, in what order".
//
// CONSISTENCY CONTRACT BEING TESTED
//
//	For any (player_id, msg_type), if Scene emits writes W1, W2, ..., Wn
//	through the key-ordered Kafka producer (key = player_id), the final
//	side effect on MySQL+Redis MUST equal Wn — regardless of how the
//	consumer batches, coalesces, or retries them.
//
// HISTORY — three bugs that these tests originally caught (now FIXED in
// key_ordered_consumer.go; kept here as regression tests):
//
//   - TC3 (read-as-barrier): a read between W1 and W2 used to coalesce W1
//     away, leaving the read to observe the pre-W1 row. Fixed by reverse-
//     pass coalescing that treats reads as segment barriers.
//   - TC5a (kafka-origin failure data loss): a failed Kafka-origin write
//     used to be silently dropped while its offset was committed. Fixed by
//     extracting dbTask from kafkaMsg up front in handleTask so the retry
//     queue path applies to both forms.
//   - TC5b (stale retry overwrite): a retry of W2 that arrived after W3 had
//     already persisted used to overwrite W3. Fixed by a per-key applied-seq
//     guard in Redis: every successful write records its seq, and any
//     subsequent task with a smaller seq is dropped.
//   - TC5c (same-batch retry inversion): a stale retry that entered the batch
//     after a newer fresh delivery used to win coalescing by queue position,
//     ACK the fresh offset, and regress MySQL. Coalescing now compares
//     (origin partition, offset+1) and ACKs skipped retry receipts.
//   - TC7 (owner_epoch 守卫,reentry-barrier §6.3):seq 只证明「同分区内更晚到」,
//     被废黜节点的迟到写可以带更大的 offset。落库前比对 DBTask.owner_epoch 与
//     player:{id}:owner_epoch,小于即丢;合并器把 epoch 变化当段边界,否则新主的
//     写会先被老写按 offset 合并掉、老写再被守卫丢弃,MySQL 少一版。
package kafka

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	db_locker "db/internal/locker"
	db_proto "proto/db"
	"shared/kafkautil"

	"github.com/IBM/sarama"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// ---------------------------------------------------------------------------
// Test harness
// ---------------------------------------------------------------------------

// recordedCall captures one effective handler invocation (i.e. NOT skipped by
// coalescing). The ordering of `calls` reflects the order in which the
// consumer actually drove side effects.
type recordedCall struct {
	op      string
	key     uint64
	msgType string
	taskID  string
	body    []byte
}

type recordingSession struct {
	ctx           context.Context
	claims        map[string][]int32
	mu            sync.Mutex
	markedOffsets []int64
}

func (s *recordingSession) Claims() map[string][]int32               { return s.claims }
func (s *recordingSession) MemberID() string                         { return "test-member" }
func (s *recordingSession) GenerationID() int32                      { return 1 }
func (s *recordingSession) MarkOffset(string, int32, int64, string)  {}
func (s *recordingSession) Commit()                                  {}
func (s *recordingSession) ResetOffset(string, int32, int64, string) {}
func (s *recordingSession) MarkMessage(msg *sarama.ConsumerMessage, _ string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markedOffsets = append(s.markedOffsets, msg.Offset)
}
func (s *recordingSession) Context() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}
func (s *recordingSession) marked() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int64(nil), s.markedOffsets...)
}

// recordingHarness installs replacement dbOpHandlers and lets the test inject
// per-call behavior (success / failure). All access is mutex-protected.
type recordingHarness struct {
	t          *testing.T
	mu         sync.Mutex
	calls      []recordedCall
	failNextOf map[string]int // taskID -> remaining failure count (0 = always succeed)
	prevHnd    map[string]dbOpHandler
}

func newHarness(t *testing.T) *recordingHarness {
	t.Helper()
	h := &recordingHarness{
		t:          t,
		failNextOf: map[string]int{},
		prevHnd:    map[string]dbOpHandler{},
	}

	// Save and override the package-level handler map.
	for k, v := range dbOpHandlers {
		h.prevHnd[k] = v
	}
	dbOpHandlers = map[string]dbOpHandler{
		"read":  h.makeHandler("read"),
		"write": h.makeHandler("write"),
	}

	t.Cleanup(func() {
		// Restore so other test files / packages aren't affected.
		dbOpHandlers = h.prevHnd
	})
	return h
}

func (h *recordingHarness) makeHandler(op string) dbOpHandler {
	return func(_ context.Context, _ redis.Cmdable, task *db_proto.DBTask, _ proto.Message) string {
		h.mu.Lock()
		defer h.mu.Unlock()

		// Honor injected failure plan first.
		if remaining, ok := h.failNextOf[task.TaskId]; ok && remaining > 0 {
			h.failNextOf[task.TaskId] = remaining - 1
			return fmt.Sprintf("injected failure for taskID=%s op=%s", task.TaskId, op)
		}

		body := append([]byte(nil), task.Body...) // deep copy, defensive
		h.calls = append(h.calls, recordedCall{
			op:      op,
			key:     task.Key,
			msgType: task.MsgType,
			taskID:  task.TaskId,
			body:    body,
		})
		return ""
	}
}

// programFailure makes the next `count` invocations of the handler for `taskID`
// fail before any subsequent one succeeds. Used by retry-ordering tests.
func (h *recordingHarness) programFailure(taskID string, count int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.failNextOf[taskID] = count
}

func (h *recordingHarness) snapshot() []recordedCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]recordedCall, len(h.calls))
	copy(out, h.calls)
	return out
}

// callsForKey returns the recorded calls for a given player key in order.
func (h *recordingHarness) callsForKey(key uint64) []recordedCall {
	all := h.snapshot()
	out := make([]recordedCall, 0, len(all))
	for _, c := range all {
		if c.key == key {
			out = append(out, c)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// worker construction
// ---------------------------------------------------------------------------

func newTestWorker(t *testing.T, partition int32) (*worker, *miniredis.Miniredis, *recordingHarness) {
	t.Helper()
	mr := miniredis.RunT(t)

	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	w := &worker{
		partition:          partition,
		taskCh:             make(chan *workerTask, 1024),
		ctx:                ctx,
		redisClient:        rc,
		locker:             db_locker.NewRedisLocker(rc),
		topic:              "test-db-task",
		retryQueueKey:      "kafka:retry:queue:test-db-task",
		retryProcessingKey: "kafka:retry:processing:test-db-task",
		retryDeadQueueKey:  "kafka:dead:queue:test-db-task",
		wg:                 &sync.WaitGroup{},

		// 与 buildWorker 同款生产实现,跑在 miniredis 上;需要隔离读失败分支的
		// 用例再把它换成 fakeAppliedEpochStore。
		appliedEpochs: redisAppliedEpochStore{rc: rc},
	}
	return w, mr, newHarness(t)
}

// makeWriteTask builds a DBTask wrapped as a workerTask whose payload is a
// well-formed (but trivial) proto message. We carry the logical write
// version in DBTask.TaskId so tests can assert ordering without parsing the
// payload.
//
// `seq` is the per-key monotonic write version; the highest seq actually
// applied for a given (key, msg_type) is the converged state.
func makeWriteTask(t *testing.T, key uint64, msgType string, seq uint64) *workerTask {
	t.Helper()

	// Body must deserialize as the requested msg_type. We use a TaskResult
	// because it's already imported and trivial to round-trip.
	dummy := &db_proto.TaskResult{Success: true, Timestamp: int64(seq)}
	bodyBytes, err := proto.Marshal(dummy)
	require.NoError(t, err)

	dbTask := &db_proto.DBTask{
		Key:     key,
		Op:      "write",
		MsgType: msgType,
		Body:    bodyBytes,
		TaskId:  fmt.Sprintf("k=%d:t=%s:seq=%d", key, msgType, seq),
	}
	// Populate workerTask.seq so the per-key applied-seq guard in
	// handleTask can reject stale retries (TC5b regression coverage).
	return &workerTask{
		dbTask:             dbTask,
		seq:                seq,
		originPartition:    0,
		hasOriginPartition: true,
	}
}

func makeReadTask(t *testing.T, key uint64, msgType string, tag string) *workerTask {
	t.Helper()
	dummy := &db_proto.TaskResult{}
	bodyBytes, err := proto.Marshal(dummy)
	require.NoError(t, err)
	return &workerTask{
		dbTask: &db_proto.DBTask{
			Key:       key,
			Op:        "read",
			MsgType:   msgType,
			Body:      bodyBytes,
			TaskId:    fmt.Sprintf("k=%d:t=%s:read=%s", key, msgType, tag),
			WhereCase: fmt.Sprintf("player_id='%d'", key),
		},
		originPartition:    0,
		hasOriginPartition: true,
	}
}

// makeKafkaWriteTask wraps the same payload but as a Kafka-origin task so we
// can exercise the kafkaMsg-vs-dbTask branching in handleTask.
func makeKafkaWriteTask(t *testing.T, key uint64, msgType string, seq uint64) *workerTask {
	t.Helper()
	wt := makeWriteTask(t, key, msgType, seq)

	rawTask := wt.dbTask
	taskBytes, err := proto.Marshal(rawTask)
	require.NoError(t, err)

	return &workerTask{
		kafkaMsg: &sarama.ConsumerMessage{
			Topic:     "test-db-task",
			Partition: 0,
			Offset:    int64(seq),
			Key:       []byte(strconv.FormatUint(key, 10)),
			Value:     taskBytes,
		},
		// Production code in ConsumeClaim sets seq = offset + 1 so that
		// seq=0 stays reserved as "no version info"; mirror that here.
		seq:                seq + 1,
		originPartition:    0,
		hasOriginPartition: true,
		// session intentionally nil: handleTask guards `if task.session != nil`.
	}
}

// extractSeqFromTaskID parses the per-key write version we encoded in TaskId.
// Returns 0 for read tasks or unparseable IDs.
func extractSeqFromTaskID(taskID string) uint64 {
	idx := strings.Index(taskID, ":seq=")
	if idx < 0 {
		return 0
	}
	v, err := strconv.ParseUint(taskID[idx+len(":seq="):], 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// ---------------------------------------------------------------------------
// TC1 — single batch coalesce
// ---------------------------------------------------------------------------

// A batch [W(seq=1), W(seq=2), ..., W(seq=N)] for the same (key, msg_type)
// must collapse to a single applied write whose seq == N.
func TestProcessTaskBatch_SingleBatchCoalesces(t *testing.T) {
	w, _, h := newTestWorker(t, 0)

	const key uint64 = 1001
	const msgType = "taskpb.TaskResult"
	const N = 8

	batch := make([]*workerTask, 0, N)
	for seq := uint64(1); seq <= N; seq++ {
		batch = append(batch, makeWriteTask(t, key, msgType, seq))
	}

	w.processTaskBatch(batch, true /* isOfflineExpand */)

	calls := h.callsForKey(key)
	require.Len(t, calls, 1, "all but the last write must be coalesced")
	assert.Equal(t, "write", calls[0].op)
	assert.Equal(t, msgType, calls[0].msgType)
	assert.Equal(t, uint64(N), extractSeqFromTaskID(calls[0].taskID),
		"only the latest write in the batch should survive")
}

// ---------------------------------------------------------------------------
// TC2 — cross-batch ordering (no coalescing across batches)
// ---------------------------------------------------------------------------

// processTaskBatch is invoked once per worker drain cycle. Sequential batches
// must each apply their own latest write; the final state for the key is the
// maximum seq across all batches.
func TestProcessTaskBatch_CrossBatchOrdering(t *testing.T) {
	w, _, h := newTestWorker(t, 0)

	const key uint64 = 2002
	const msgType = "taskpb.TaskResult"

	batches := [][]uint64{
		{1, 2, 3},
		{4, 5},
		{6},
		{7, 8, 9, 10},
	}

	for _, seqs := range batches {
		batch := make([]*workerTask, 0, len(seqs))
		for _, s := range seqs {
			batch = append(batch, makeWriteTask(t, key, msgType, s))
		}
		w.processTaskBatch(batch, true)
	}

	calls := h.callsForKey(key)
	require.Len(t, calls, len(batches),
		"each batch must contribute exactly one write (per-batch coalesce, no cross-batch coalesce)")

	// Each call's seq must equal the LAST seq of its batch, in batch order.
	for i, b := range batches {
		want := b[len(b)-1]
		got := extractSeqFromTaskID(calls[i].taskID)
		assert.Equal(t, want, got, "batch %d expected last seq=%d, got=%d", i, want, got)
	}

	// Final converged state = global max seq.
	last := calls[len(calls)-1]
	assert.Equal(t, uint64(10), extractSeqFromTaskID(last.taskID),
		"global last-write-wins must hold across batches")
}

// ---------------------------------------------------------------------------
// TC3 — Reads must act as a coalescing barrier (regression: was bug #1)
// ---------------------------------------------------------------------------

// SPEC: a read task R between writes W1 and W2 (same key+msg_type) acts as a
// barrier. W1 must still be applied, because R will subsequently be issued to
// MySQL and must observe the post-W1 state (read-after-write within the same
// per-key Kafka partition).
//
// FIX (key_ordered_consumer.go: processTaskBatch): coalesce in REVERSE,
// clearing the "live last write" tracker on every read. Writes before a
// read live in a different segment and execute independently from writes
// after the read.
func TestProcessTaskBatch_ReadActsAsBarrier(t *testing.T) {
	w, _, h := newTestWorker(t, 0)

	const key uint64 = 3003
	const msgType = "taskpb.TaskResult"

	batch := []*workerTask{
		makeWriteTask(t, key, msgType, 1),
		makeReadTask(t, key, msgType, "mid"),
		makeWriteTask(t, key, msgType, 2),
	}

	w.processTaskBatch(batch, true)

	calls := h.callsForKey(key)
	require.Len(t, calls, 3,
		"read between two writes must not collapse anything: W1 must execute so R observes its state")
	assert.Equal(t, "write", calls[0].op)
	assert.Equal(t, uint64(1), extractSeqFromTaskID(calls[0].taskID))
	assert.Equal(t, "read", calls[1].op)
	assert.Equal(t, "write", calls[2].op)
	assert.Equal(t, uint64(2), extractSeqFromTaskID(calls[2].taskID))
}

// ---------------------------------------------------------------------------
// TC4 — multi-player isolation within a single batch
// ---------------------------------------------------------------------------

// Within one batch we may interleave writes for many players. Coalescing must
// be per-(key, msg_type), so each player's last write survives.
func TestProcessTaskBatch_PerKeyCoalesce(t *testing.T) {
	w, _, h := newTestWorker(t, 0)

	const msgType = "taskpb.TaskResult"
	keys := []uint64{4001, 4002, 4003}

	// Interleave 5 writes per key: A1,B1,C1,A2,B2,C2,...
	const writesPerKey = 5
	batch := make([]*workerTask, 0, writesPerKey*len(keys))
	for seq := uint64(1); seq <= writesPerKey; seq++ {
		for _, k := range keys {
			batch = append(batch, makeWriteTask(t, k, msgType, seq))
		}
	}

	w.processTaskBatch(batch, true)

	for _, k := range keys {
		calls := h.callsForKey(k)
		require.Len(t, calls, 1, "key %d should have exactly one effective write", k)
		assert.Equal(t, uint64(writesPerKey), extractSeqFromTaskID(calls[0].taskID),
			"key %d must converge to last seq", k)
	}
}

// ---------------------------------------------------------------------------
// TC5a — Kafka-origin failures must enter retry queue (regression: was bug #2)
// ---------------------------------------------------------------------------

// SPEC: when a write task originating from Kafka fails (handler returns
// non-empty error), it must be enqueued to the retry queue so it can be
// retried. Otherwise the message is lost — and Kafka offset is already
// MarkMessage'd by handleTask immediately after, so it can never be re-read.
//
// FIX (key_ordered_consumer.go: handleTask): the dbTask is now extracted
// from kafkaMsg up front, and the failure branch saves it to the retry
// queue regardless of whether the task originated from Kafka or a prior
// retry.
func TestProcessTaskBatch_KafkaFailure_MustEnterRetryQueue(t *testing.T) {
	w, mr, h := newTestWorker(t, 0)

	const key uint64 = 5001
	const msgType = "taskpb.TaskResult"

	wt := makeKafkaWriteTask(t, key, msgType, 7)

	// Fail the very first attempt for this taskID. The handler we install
	// uses the parsed dbTask.TaskId, but kafkaMsg form has no parsed task
	// at injection time — the consumer parses it from Value during
	// processTaskBatch and uses `taskID = ...`. We program the failure on
	// the taskID we already know we encoded (deterministic).
	expectedTaskID := fmt.Sprintf("k=%d:t=%s:seq=%d", key, msgType, 7)
	h.programFailure(expectedTaskID, 1)

	w.processTaskBatch([]*workerTask{wt}, true)

	// Spec assertion: the failed task must be queued for retry.
	queueLen, err := w.redisClient.LLen(w.ctx, w.retryQueueKey).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(1), queueLen,
		"kafka-origin task that failed must be in retry queue (no silent drop)")

	// And the retry payload must be the wrapped (seq + dbTask) format so
	// the per-key applied-seq guard is preserved across retries.
	popped, err := w.redisClient.RPop(w.ctx, w.retryQueueKey).Bytes()
	require.NoError(t, err)
	gotSeq, gotPartition, hasPartition, taskBytes := unwrapRetryPayload(popped)
	require.NotZero(t, gotSeq, "retry payload must carry the original kafka seq, not 0")
	require.True(t, hasPartition, "retry payload must carry the origin partition (v2 format)")
	assert.Equal(t, w.partition, gotPartition,
		"retry payload must record the worker's own partition so the retry routes back to it")

	var retried db_proto.DBTask
	require.NoError(t, proto.Unmarshal(taskBytes, &retried))
	assert.Equal(t, expectedTaskID, retried.TaskId)
	assert.Equal(t, key, retried.Key)
	assert.Equal(t, msgType, retried.MsgType)

	_ = mr
}

func TestWriteBackDBCacheReturnsRedisFailure(t *testing.T) {
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	task := makeWriteTask(t, 5005, "taskpb.TaskResult", 1)
	msg := &db_proto.TaskResult{Success: true, Timestamp: 1}

	if err := writeBackDBCache(context.Background(), rc, task.dbTask, msg); err != nil {
		t.Fatalf("healthy cache publication: %v", err)
	}
	if _, err := rc.Get(context.Background(), buildCacheKey(task.dbTask)).Bytes(); err != nil {
		t.Fatalf("published cache value missing: %v", err)
	}

	mr.Close()
	if err := writeBackDBCache(context.Background(), rc, task.dbTask, msg); err == nil {
		t.Fatal("Redis publication failure must propagate so Kafka/retry is not ACKed")
	}
}

// ---------------------------------------------------------------------------
// TC5b — Out-of-order retry must not overwrite newer state (regression: bug #3)
// ---------------------------------------------------------------------------

// SPEC: per-key monotonic last-write-wins. If W2 fails and lands in the retry
// queue, then W3 succeeds, then W2 is retried — the final state must remain
// W3 (W2's retry is stale and must be dropped).
//
// FIX (key_ordered_consumer.go: shouldApplyBySeq + markAppliedSeq): every
// successful write records seq → Redis at appliedSeqKey(...). Before
// dispatching any write, handleTask compares the task's seq against the
// stored value; smaller seq → drop. The retry queue payload carries the
// original seq via the wrapRetryPayload format so retries inherit their
// place in the partition order.
func TestProcessTaskBatch_RetryAfterNewerWrite_MustNotRegress(t *testing.T) {
	w, _, h := newTestWorker(t, 0)

	const key uint64 = 5002
	const msgType = "taskpb.TaskResult"

	w1 := makeWriteTask(t, key, msgType, 1)
	w2 := makeWriteTask(t, key, msgType, 2)
	w3 := makeWriteTask(t, key, msgType, 3)

	// Make W2's first invocation fail; subsequent invocations succeed.
	// processTaskBatch will coalesce w1+w2+w3 into "the latest" — which
	// defeats the test if all three are in the SAME batch. Run them in
	// SEPARATE batches so coalescing doesn't hide the issue.
	h.programFailure(w2.dbTask.TaskId, 1)

	w.processTaskBatch([]*workerTask{w1}, true) // W1 OK -> applied=1
	w.processTaskBatch([]*workerTask{w2}, true) // W2 fails -> retry queue (seq=2)
	w.processTaskBatch([]*workerTask{w3}, true) // W3 OK -> applied=3

	// W2 is now in the retry queue. Drain it back into the worker. Mirror
	// what `consumeRetryQueue` does: pop, parse the wrapped payload to
	// recover the original seq, increment retry_count, deliver as a dbTask
	// workerTask carrying the original seq.
	popped, err := w.redisClient.RPopLPush(w.ctx, w.retryQueueKey, w.retryProcessingKey).Bytes()
	require.NoError(t, err, "W2 must be in retry queue after its initial failure")

	retrySeq, retryPartition, hasPartition, taskBytes := unwrapRetryPayload(popped)
	require.Equal(t, uint64(2), retrySeq,
		"retry payload must carry W2's original seq=2 so the applied-seq guard can compare")
	require.True(t, hasPartition)

	var retryTask db_proto.DBTask
	require.NoError(t, proto.Unmarshal(taskBytes, &retryTask))
	retryTask.RetryCount++
	w.processTaskBatch([]*workerTask{{
		dbTask:             &retryTask,
		seq:                retrySeq,
		originPartition:    retryPartition,
		hasOriginPartition: hasPartition,
		fromRetry:          true,
		retryReceipt:       popped,
	}}, true)

	// Final state: the LAST applied call for this key must still be W3,
	// and there must be NO call for W2 after W3 (the retry is dropped).
	calls := h.callsForKey(key)
	require.NotEmpty(t, calls)
	last := calls[len(calls)-1]
	assert.Equal(t, uint64(3), extractSeqFromTaskID(last.taskID),
		"final state must be W3; stale retry of W2 must NOT overwrite it")

	// Stronger: count writes by seq. We expect exactly one W1 and one W3,
	// and zero W2 (because W2's only successful invocation would be the
	// retry, which the guard drops).
	bySeq := map[uint64]int{}
	for _, c := range calls {
		bySeq[extractSeqFromTaskID(c.taskID)]++
	}
	assert.Equal(t, 1, bySeq[1], "W1 must apply exactly once")
	assert.Equal(t, 0, bySeq[2], "W2 must never be applied (initial failed, retry dropped as stale)")
	assert.Equal(t, 1, bySeq[3], "W3 must apply exactly once")
	processingLen, err := w.redisClient.LLen(w.ctx, w.retryProcessingKey).Result()
	require.NoError(t, err)
	assert.Zero(t, processingLen, "stale retry must ACK its reliable processing receipt")
}

// TC5c — a delayed retry can be queued after a newer fresh delivery in the
// same sub-shard drain. Queue position is not causal order: the greatest seq
// from the same proven origin partition must win, and the skipped retry's
// reliable ready->processing receipt must be removed.
func TestProcessTaskBatch_DelayedRetryAfterFreshInSameBatchDoesNotRegress(t *testing.T) {
	w, _, h := newTestWorker(t, 0)
	const key uint64 = 5003
	const msgType = "taskpb.TaskResult"

	newerFresh := makeWriteTask(t, key, msgType, 101)
	staleRetry := makeWriteTask(t, key, msgType, 100)
	staleRetry.fromRetry = true
	staleRetry.retryReceipt = []byte("retry-receipt-seq-100")
	require.NoError(t, w.redisClient.LPush(w.ctx, w.retryProcessingKey, staleRetry.retryReceipt).Err())

	// This is the bug-triggering order: the old retry is later in the queue.
	w.processTaskBatch([]*workerTask{newerFresh, staleRetry}, true)

	calls := h.callsForKey(key)
	require.Len(t, calls, 1, "same-partition coalescing must retain exactly one write")
	assert.Equal(t, uint64(101), extractSeqFromTaskID(calls[0].taskID),
		"delayed retry must not supersede a causally newer fresh write")
	rawCursor, err := w.redisClient.Get(w.ctx, appliedSeqKey(w.topic, key, msgType)).Result()
	require.NoError(t, err)
	assert.Equal(t, "v2:0:101", rawCursor)
	processingLen, err := w.redisClient.LLen(w.ctx, w.retryProcessingKey).Result()
	require.NoError(t, err)
	assert.Zero(t, processingLen, "superseded retry must ACK its processing receipt")
}

func TestProcessTaskBatch_DifferentOriginPartitionsAreNotCoalesced(t *testing.T) {
	w, _, h := newTestWorker(t, 5)
	const key uint64 = 5004
	const msgType = "taskpb.TaskResult"

	first := makeWriteTask(t, key, msgType, 1)
	first.originPartition = 5
	second := makeWriteTask(t, key, msgType, 999)
	second.originPartition = 0
	w.processTaskBatch([]*workerTask{first, second}, true)

	calls := h.callsForKey(key)
	require.Len(t, calls, 1, "first partition writes once; incomparable delivery is quarantined")
	assert.Equal(t, first.dbTask.TaskId, calls[0].taskID)
	deadLen, err := w.redisClient.LLen(w.ctx, w.retryDeadQueueKey).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(1), deadLen,
		"cross-partition writes must reach the ordering guard, not disappear in coalescing")
}

func TestClaimAcker_LowerHoleBlocksLaterSuccess(t *testing.T) {
	t.Run("out-of-order success waits for contiguous prefix", func(t *testing.T) {
		session := &recordingSession{}
		_, cancel := context.WithCancel(context.Background())
		defer cancel()
		acker := newClaimAcker(session, 10, cancel)
		m10 := &sarama.ConsumerMessage{Topic: "test", Partition: 0, Offset: 10}
		m11 := &sarama.ConsumerMessage{Topic: "test", Partition: 0, Offset: 11}
		acker.track(m10)
		acker.track(m11)

		acker.ack(m11)
		assert.Empty(t, session.marked(), "offset 11 must not jump over unfinished offset 10")
		acker.ack(m10)
		assert.Equal(t, []int64{10, 11}, session.marked())
	})

	t.Run("failed lower hole never marks later success", func(t *testing.T) {
		claimCtx, cancel := context.WithCancel(context.Background())
		session := &recordingSession{ctx: claimCtx}
		acker := newClaimAcker(session, 20, cancel)
		m20 := &sarama.ConsumerMessage{Topic: "test", Partition: 0, Offset: 20}
		m21 := &sarama.ConsumerMessage{Topic: "test", Partition: 0, Offset: 21}
		acker.track(m20)
		acker.track(m21)

		acker.fail(m20, errors.New("retry queue unavailable"))
		acker.ack(m21)
		assert.Empty(t, session.marked(), "offset 21 must remain unmarked behind failed offset 20")
		assert.Error(t, claimCtx.Err(), "a durability failure must cancel all old-claim work immediately")
	})
}

func TestConsumerSetupRejectsPartitionsOutsideImmutableContract(t *testing.T) {
	w, _, _ := newTestWorker(t, 0)
	c := &KeyOrderedKafkaConsumer{
		topic:          w.topic,
		groupID:        "test-group",
		partitionCount: 3,
	}
	h := &consumerGroupHandler{consumer: c}

	validSubset := &recordingSession{claims: map[string][]int32{w.topic: {0, 2}}}
	if err := h.Setup(validSubset); err != nil {
		t.Fatalf("a consumer-group member may own a valid subset of partitions: %v", err)
	}

	drifted := &recordingSession{claims: map[string][]int32{w.topic: {1, 3}}}
	if err := h.Setup(drifted); err == nil {
		t.Fatal("claiming a live-expanded partition must fail the rebalance")
	}
}

func TestEnsureWorkerRejectsPartitionOutsideImmutableContract(t *testing.T) {
	w, _, _ := newTestWorker(t, 0)
	c := &KeyOrderedKafkaConsumer{
		topic:          w.topic,
		partitionCount: 2,
		workers:        map[int32]*worker{0: w},
	}
	if _, err := c.ensureWorker(2); err == nil {
		t.Fatal("worker creation must not normalize a broker partition drift into supported routing")
	}
	if _, exists := c.workers[2]; exists {
		t.Fatal("out-of-contract worker must not be created")
	}
}

func TestHandleTask_CanceledClaimCannotReachDB(t *testing.T) {
	w, _, h := newTestWorker(t, 0)
	claimCtx, cancel := context.WithCancel(context.Background())
	session := &recordingSession{ctx: claimCtx}
	acker := newClaimAcker(session, 30, cancel)
	msg := &sarama.ConsumerMessage{Topic: w.topic, Partition: 0, Offset: 30}
	acker.track(msg)

	task := makeWriteTask(t, 5100, "taskpb.TaskResult", 31)
	task.kafkaMsg = msg
	task.acker = acker
	task.claimCtx = claimCtx
	cancel()
	w.handleTask(task, true)

	assert.Empty(t, h.callsForKey(5100), "revoked claim must be rejected before business DB I/O")
	assert.Empty(t, session.marked(), "revoked claim must not mark its old Sarama session")
	select {
	case <-acker.failureCh:
	default:
		t.Fatal("canceled queued task must close the acker hole")
	}
}

func TestHandleTask_ExpansionLockBusyDoesNotMarkApplied(t *testing.T) {
	w, _, h := newTestWorker(t, 0)
	const key uint64 = 5200
	require.NoError(t, kafkautil.SetExpandStatus(w.ctx, w.redisClient, w.topic,
		kafkautil.ExpandStatusExpanding, 1))
	expansionLockKey := "distributed:lock:" + fmt.Sprintf("kafka:consumer:lock:%d", key)
	require.NoError(t, w.redisClient.Set(w.ctx, expansionLockKey, "other-owner", time.Minute).Err())

	w.handleTask(makeWriteTask(t, key, "taskpb.TaskResult", 1), false)

	assert.Empty(t, h.callsForKey(key))
	_, err := w.redisClient.Get(w.ctx, appliedSeqKey(w.topic, key, "taskpb.TaskResult")).Result()
	assert.ErrorIs(t, err, redis.Nil, "deferred task must not publish an applied cursor")
	readyLen, err := w.redisClient.LLen(w.ctx, w.retryQueueKey).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(1), readyLen, "deferred Kafka work must have one durable retry copy")
}

func TestOrderingGuard_NewP5ThenDelayedOldP0IsQuarantined(t *testing.T) {
	w, _, h := newTestWorker(t, 5)
	const key uint64 = 5300
	const msgType = "taskpb.TaskResult"

	newP5 := makeWriteTask(t, key, msgType, 5)
	newP5.originPartition = 5
	w.handleTask(newP5, true)

	claimCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	session := &recordingSession{ctx: claimCtx}
	acker := newClaimAcker(session, 100, cancel)
	msg := &sarama.ConsumerMessage{Topic: w.topic, Partition: 0, Offset: 100}
	acker.track(msg)
	delayedOldP0 := makeWriteTask(t, key, msgType, 100)
	delayedOldP0.originPartition = 0
	delayedOldP0.kafkaMsg = msg
	delayedOldP0.acker = acker
	delayedOldP0.claimCtx = claimCtx
	w.handleTask(delayedOldP0, true)

	calls := h.callsForKey(key)
	require.Len(t, calls, 1, "a numerically larger offset from another partition is not causally newer")
	assert.Equal(t, newP5.dbTask.TaskId, calls[0].taskID)
	rawCursor, err := w.redisClient.Get(w.ctx, appliedSeqKey(w.topic, key, msgType)).Result()
	require.NoError(t, err)
	assert.Equal(t, "v2:5:5", rawCursor, "quarantined old-partition delivery must not replace the applied cursor")
	ttl, err := w.redisClient.TTL(w.ctx, appliedSeqKey(w.topic, key, msgType)).Result()
	require.NoError(t, err)
	assert.Equal(t, time.Duration(-1), ttl, "ordering cursor must survive business-cache TTL expiry")
	deadLen, err := w.redisClient.LLen(w.ctx, w.retryDeadQueueKey).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(1), deadLen, "cross-partition conflict must be durably quarantined")
	assert.Equal(t, []int64{100}, session.marked(), "Kafka may ACK only after the quarantine copy is durable")
}

func TestOrderingGuard_ExistingP0QuarantinesFirstP5Delivery(t *testing.T) {
	w, _, h := newTestWorker(t, 0)
	const key uint64 = 5301
	const msgType = "taskpb.TaskResult"

	p0 := makeWriteTask(t, key, msgType, 500)
	p0.originPartition = 0
	w.handleTask(p0, true)
	p5 := makeWriteTask(t, key, msgType, 1)
	p5.originPartition = 5
	w.handleTask(p5, true)

	calls := h.callsForKey(key)
	require.Len(t, calls, 1)
	assert.Equal(t, p0.dbTask.TaskId, calls[0].taskID)
	deadLen, err := w.redisClient.LLen(w.ctx, w.retryDeadQueueKey).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(1), deadLen, "first delivery on a new partition needs an explicit migration epoch")
}

func TestRetryConsumer_LegacyPayloadWithoutPartitionGoesToDeadQueue(t *testing.T) {
	w, _, h := newTestWorker(t, 0)
	task := makeWriteTask(t, 5400, "taskpb.TaskResult", 7).dbTask
	payload, err := encodeRetryPayload(task, 7, 0, false)
	require.NoError(t, err)
	require.NoError(t, w.redisClient.LPush(w.ctx, w.retryQueueKey, payload).Err())

	c := &KeyOrderedKafkaConsumer{
		redisClient:        w.redisClient,
		topic:              w.topic,
		partitionCount:     4,
		workers:            map[int32]*worker{0: w},
		ctx:                w.ctx,
		retryQueueKey:      w.retryQueueKey,
		retryProcessingKey: w.retryProcessingKey,
		retryDeadQueueKey:  w.retryDeadQueueKey,
		retryMaxTimes:      3,
	}
	c.consumeRetryQueue()

	assert.Empty(t, h.callsForKey(task.Key), "unknown-provenance legacy retry must never execute")
	readyLen, err := w.redisClient.LLen(w.ctx, w.retryQueueKey).Result()
	require.NoError(t, err)
	processingLen, err := w.redisClient.LLen(w.ctx, w.retryProcessingKey).Result()
	require.NoError(t, err)
	deadLen, err := w.redisClient.LLen(w.ctx, w.retryDeadQueueKey).Result()
	require.NoError(t, err)
	assert.Zero(t, readyLen)
	assert.Zero(t, processingLen)
	assert.Equal(t, int64(1), deadLen)
}

func TestRecoverRetryProcessingRestoresEveryReceipt(t *testing.T) {
	w, _, _ := newTestWorker(t, 0)
	for _, payload := range [][]byte{[]byte("one"), []byte("two")} {
		require.NoError(t, w.redisClient.LPush(w.ctx, w.retryProcessingKey, payload).Err())
	}
	c := &KeyOrderedKafkaConsumer{
		redisClient:        w.redisClient,
		ctx:                w.ctx,
		retryQueueKey:      w.retryQueueKey,
		retryProcessingKey: w.retryProcessingKey,
	}
	require.NoError(t, c.recoverRetryProcessing())
	processingLen, err := w.redisClient.LLen(w.ctx, w.retryProcessingKey).Result()
	require.NoError(t, err)
	assert.Zero(t, processingLen)
	ready, err := w.redisClient.LRange(w.ctx, w.retryQueueKey, 0, -1).Result()
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"one", "two"}, ready)
}

func TestPoisonKafkaPayloadIsPersistedBeforeAck(t *testing.T) {
	w, _, _ := newTestWorker(t, 0)
	claimCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	session := &recordingSession{ctx: claimCtx}
	acker := newClaimAcker(session, 60, cancel)
	msg := &sarama.ConsumerMessage{
		Topic: w.topic, Partition: 0, Offset: 60,
		Key: []byte("player-raw-key"), Value: []byte{0xff, 0x00, 0xff},
	}
	acker.track(msg)
	w.handleTask(&workerTask{kafkaMsg: msg, acker: acker, claimCtx: claimCtx}, true)

	assert.Equal(t, []int64{60}, session.marked(), "poison offset may ACK only after DLQ persistence")
	stored, err := w.redisClient.LIndex(w.ctx, w.retryDeadQueueKey, 0).Bytes()
	require.NoError(t, err)
	require.NotEmpty(t, stored)
	assert.Equal(t, poisonPayloadMagic, stored[0])
	var record poisonMessageRecord
	require.NoError(t, json.Unmarshal(stored[1:], &record))
	assert.Equal(t, msg.Topic, record.Topic)
	assert.Equal(t, msg.Partition, record.Partition)
	assert.Equal(t, msg.Offset, record.Offset)
	assert.Equal(t, msg.Key, record.Key)
	assert.Equal(t, msg.Value, record.Value)
	assert.NotEmpty(t, record.Error)
}

// ---------------------------------------------------------------------------
// TC7 — owner_epoch 守卫(reentry-barrier §6.3 / cross-zone-scene-travel CZ-4)
// ---------------------------------------------------------------------------

// fakeAppliedEpochStore 是 map 实现的测试替身。它存在的唯一理由是「读失败」分支:
// miniredis 一关,applied-seq 的 GET 会先失败,隔离不出 epoch 读失败这一支。
type fakeAppliedEpochStore struct {
	mu     sync.Mutex
	epochs map[uint64]uint64
	err    error
	reads  int
}

func (f *fakeAppliedEpochStore) ReadAppliedEpoch(_ context.Context, _ string, key uint64, _ string) (uint64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.err != nil {
		return 0, false, f.err
	}
	epoch, ok := f.epochs[key]
	return epoch, ok, nil
}

func (f *fakeAppliedEpochStore) MarkAppliedEpoch(_ context.Context, _ string, key uint64, _ string, epoch uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if f.epochs == nil {
		f.epochs = map[uint64]uint64{}
	}
	if epoch > f.epochs[key] {
		f.epochs[key] = epoch
	}
	return nil
}

func (f *fakeAppliedEpochStore) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

// makeEpochWriteTask 在 makeWriteTask 之上补生产者持有的归属 epoch。
func makeEpochWriteTask(t *testing.T, key uint64, msgType string, seq uint64, epoch uint64) *workerTask {
	t.Helper()
	wt := makeWriteTask(t, key, msgType, seq)
	wt.dbTask.OwnerEpoch = epoch
	return wt
}

// attachKafkaClaim 把一个 dbTask 源任务包成带 claimAcker 的 Kafka 源任务,让用例能
// 断言「offset 有没有被 ACK」。offset = seq-1,与 ConsumeClaim 的 seq=offset+1 对应。
func attachKafkaClaim(t *testing.T, w *worker, wt *workerTask) *recordingSession {
	t.Helper()
	claimCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	session := &recordingSession{ctx: claimCtx}
	offset := int64(wt.seq) - 1
	acker := newClaimAcker(session, offset, cancel)
	msg := &sarama.ConsumerMessage{Topic: w.topic, Partition: wt.originPartition, Offset: offset}
	acker.track(msg)
	wt.kafkaMsg = msg
	wt.acker = acker
	wt.claimCtx = claimCtx
	return session
}

// setAppliedEpoch 摆出「该 key 已经有 epoch 的写落过库」的前置状态。
func setAppliedEpoch(t *testing.T, w *worker, key uint64, msgType string, epoch uint64) {
	t.Helper()
	require.NoError(t, w.appliedEpochs.MarkAppliedEpoch(w.ctx, w.topic, key, msgType, epoch))
}

func appliedEpochOf(t *testing.T, w *worker, key uint64, msgType string) (uint64, bool) {
	t.Helper()
	epoch, found, err := w.appliedEpochs.ReadAppliedEpoch(w.ctx, w.topic, key, msgType)
	require.NoError(t, err)
	return epoch, found
}

// 键名是运维排障会直接去查的东西,钉死;与 applied cursor 同前缀、分开存(见 appliedEpochKey 注释)。
func TestAppliedEpochKey_Format(t *testing.T) {
	assert.Equal(t, "consumer:applied_epoch:db_task_zone_1:12345:taskpb.TaskResult",
		appliedEpochKey("db_task_zone_1", 12345, "taskpb.TaskResult"))
}

func TestCheckOwnerEpoch_Verdicts(t *testing.T) {
	const player uint64 = 7001
	const msgType = "taskpb.TaskResult"
	cases := []struct {
		name      string
		taskEpoch uint64
		store     appliedEpochStore
		wantOut   string
		wantRej   bool
		wantErr   bool
		wantReads int
	}{
		{name: "zero epoch is legacy and never reads redis", taskEpoch: 0,
			store: &fakeAppliedEpochStore{epochs: map[uint64]uint64{player: 5}}, wantOut: "legacy_zero"},
		{name: "zero epoch tolerates missing store", taskEpoch: 0, store: nil, wantOut: "legacy_zero"},
		{name: "no applied epoch yet is the first write", taskEpoch: 5,
			store: &fakeAppliedEpochStore{epochs: map[uint64]uint64{}}, wantOut: "first", wantReads: 1},
		{name: "older than applied is stale and rejected", taskEpoch: 3,
			store: &fakeAppliedEpochStore{epochs: map[uint64]uint64{player: 5}}, wantOut: "stale", wantRej: true, wantReads: 1},
		{name: "equal to applied matches", taskEpoch: 5,
			store: &fakeAppliedEpochStore{epochs: map[uint64]uint64{player: 5}}, wantOut: "match", wantReads: 1},
		{name: "newer than applied is the new owner's first write", taskEpoch: 6,
			store: &fakeAppliedEpochStore{epochs: map[uint64]uint64{player: 5}}, wantOut: "advance", wantReads: 1},
		{name: "read failure is an error, not a verdict", taskEpoch: 5,
			store: &fakeAppliedEpochStore{err: errors.New("redis down")}, wantErr: true, wantReads: 1},
		{name: "missing store with real epoch fails closed", taskEpoch: 5, store: nil, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task := &db_proto.DBTask{Key: player, MsgType: msgType, Op: "write", OwnerEpoch: tc.taskEpoch}
			verdict, err := checkOwnerEpoch(context.Background(), tc.store, "db_task_zone_1", task)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.wantOut, verdict.outcome)
				assert.Equal(t, tc.wantRej, verdict.reject)
			}
			if fake, ok := tc.store.(*fakeAppliedEpochStore); ok {
				assert.Equal(t, tc.wantReads, fake.readCount())
			}
		})
	}
}

func TestRedisAppliedEpochStore(t *testing.T) {
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	store := redisAppliedEpochStore{rc: rc}
	ctx := context.Background()
	const topic, msgType = "db_task_zone_1", "taskpb.TaskResult"

	_, found, err := store.ReadAppliedEpoch(ctx, topic, 8001, msgType)
	require.NoError(t, err)
	assert.False(t, found, "absent key is 'not found', not an error")

	require.NoError(t, store.MarkAppliedEpoch(ctx, topic, 8001, msgType, 4))
	require.NoError(t, store.MarkAppliedEpoch(ctx, topic, 8001, msgType, 2), "marking an older epoch is a no-op, not an error")
	epoch, found, err := store.ReadAppliedEpoch(ctx, topic, 8001, msgType)
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, uint64(4), epoch, "applied epoch only ever rises: a late retry of an older write must not lower it")

	require.NoError(t, rc.Set(ctx, appliedEpochKey(topic, 8002, msgType), "not-a-number", 0).Err())
	_, _, err = store.ReadAppliedEpoch(ctx, topic, 8002, msgType)
	require.Error(t, err, "a hand-corrupted key must surface as retryable error, never as missing")

	mr.Close()
	_, _, err = store.ReadAppliedEpoch(ctx, topic, 8001, msgType)
	require.Error(t, err)
}

// 小于已落库 epoch → 丢弃是终态:不落库、不进重试、不进死信、不推进 cursor,但 Kafka offset
// 要 ACK(否则这条被废黜的写会在每次 rebalance 后重放,永远堵着分区)。
func TestOwnerEpochGuard_StaleWriteIsDroppedTerminally(t *testing.T) {
	w, _, h := newTestWorker(t, 0)
	const key uint64 = 6001
	const msgType = "taskpb.TaskResult"
	setAppliedEpoch(t, w, key, msgType, 5)

	stale := makeEpochWriteTask(t, key, msgType, 10, 3)
	session := attachKafkaClaim(t, w, stale)
	w.handleTask(stale, true)

	assert.Empty(t, h.callsForKey(key), "deposed node's write must not reach MySQL")
	assert.Equal(t, []int64{9}, session.marked(), "dropped write is terminal: its offset must be ACKed")
	readyLen, err := w.redisClient.LLen(w.ctx, w.retryQueueKey).Result()
	require.NoError(t, err)
	assert.Zero(t, readyLen, "stale-owner write must not be retried")
	deadLen, err := w.redisClient.LLen(w.ctx, w.retryDeadQueueKey).Result()
	require.NoError(t, err)
	assert.Zero(t, deadLen, "stale-owner write is not an ordering conflict; no quarantine")
	_, err = w.redisClient.Get(w.ctx, appliedSeqKey(w.topic, key, msgType)).Result()
	assert.ErrorIs(t, err, redis.Nil, "a dropped write was never applied; cursor must not move")
	epoch, _ := appliedEpochOf(t, w, key, msgType)
	assert.Equal(t, uint64(5), epoch, "a dropped write must not touch the applied epoch")
}

func TestOwnerEpochGuard_StaleRetryAcksItsReceipt(t *testing.T) {
	w, _, h := newTestWorker(t, 0)
	const key uint64 = 6002
	const msgType = "taskpb.TaskResult"
	setAppliedEpoch(t, w, key, msgType, 5)

	stale := makeEpochWriteTask(t, key, msgType, 10, 3)
	stale.fromRetry = true
	stale.retryReceipt = []byte("receipt-stale-owner")
	require.NoError(t, w.redisClient.LPush(w.ctx, w.retryProcessingKey, stale.retryReceipt).Err())
	w.handleTask(stale, true)

	assert.Empty(t, h.callsForKey(key))
	for _, q := range []string{w.retryProcessingKey, w.retryQueueKey, w.retryDeadQueueKey} {
		n, err := w.redisClient.LLen(w.ctx, q).Result()
		require.NoError(t, err)
		assert.Zero(t, n, "queue %s must be empty after terminal drop", q)
	}
}

func TestOwnerEpochGuard_AllowedVerdictsReachMySQLAndAdvanceAppliedEpoch(t *testing.T) {
	const msgType = "taskpb.TaskResult"
	cases := []struct {
		name         string
		key          uint64
		appliedEpoch uint64 // 0 = 还没有记录
		taskEpoch    uint64
		wantApplied  uint64
	}{
		{name: "same owner keeps writing", key: 6101, appliedEpoch: 5, taskEpoch: 5, wantApplied: 5},
		{name: "first epoch-carrying write", key: 6102, appliedEpoch: 0, taskEpoch: 5, wantApplied: 5},
		{name: "new owner's first write", key: 6103, appliedEpoch: 5, taskEpoch: 6, wantApplied: 6},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, _, h := newTestWorker(t, 0)
			if tc.appliedEpoch != 0 {
				setAppliedEpoch(t, w, tc.key, msgType, tc.appliedEpoch)
			}
			wt := makeEpochWriteTask(t, tc.key, msgType, 10, tc.taskEpoch)
			session := attachKafkaClaim(t, w, wt)
			w.handleTask(wt, true)

			calls := h.callsForKey(tc.key)
			require.Len(t, calls, 1)
			assert.Equal(t, wt.dbTask.TaskId, calls[0].taskID)
			assert.Equal(t, []int64{9}, session.marked())
			cursor, err := w.redisClient.Get(w.ctx, appliedSeqKey(w.topic, tc.key, msgType)).Result()
			require.NoError(t, err)
			assert.Equal(t, "v2:0:10", cursor, "an applied write advances the cursor as before")
			epoch, found := appliedEpochOf(t, w, tc.key, msgType)
			require.True(t, found)
			assert.Equal(t, tc.wantApplied, epoch)
		})
	}
}

// 本设计的主链路:源节点以 epoch=E 发出最后一笔 DBTask,随即交接,scene_manager 把
// Redis 里的当前值推到 E+1;这笔写晚几百毫秒才被消费。它是**传送前的最终态**,必须落库。
// (旧实现拿 Redis 当前值比,每次合法传送都会把它当僵尸写丢掉。)
func TestOwnerEpochGuard_PreviousOwnersLastWriteLandsBeforeNewOwnerWrites(t *testing.T) {
	w, _, h := newTestWorker(t, 0)
	const key uint64 = 6150
	const msgType = "taskpb.TaskResult"
	setAppliedEpoch(t, w, key, msgType, 4)
	// scene_manager 早已把当前 epoch 推到 5;db 守卫不看这把键。
	require.NoError(t, w.redisClient.Set(w.ctx, fmt.Sprintf("player:%d:owner_epoch", key), "5", 0).Err())

	w.handleTask(makeEpochWriteTask(t, key, msgType, 10, 4), true) // 源节点的最后一笔
	w.handleTask(makeEpochWriteTask(t, key, msgType, 11, 5), true) // 新主的第一笔
	w.handleTask(makeEpochWriteTask(t, key, msgType, 12, 4), true) // 源节点僵尸写:新主已落库之后才到

	calls := h.callsForKey(key)
	require.Len(t, calls, 2, "previous owner's final state and the new owner's write both land; the zombie write does not")
	assert.Equal(t, uint64(10), extractSeqFromTaskID(calls[0].taskID))
	assert.Equal(t, uint64(11), extractSeqFromTaskID(calls[1].taskID))
	epoch, _ := appliedEpochOf(t, w, key, msgType)
	assert.Equal(t, uint64(5), epoch)
}

// 兼容窗口:旧版生产者不填 epoch(0),守卫必须完全旁路 —— 连 Redis 都不读,
// 否则升级期每条存盘多一次 GET,而且没有任何 epoch 可比。
func TestOwnerEpochGuard_ZeroEpochBypassesStore(t *testing.T) {
	w, _, h := newTestWorker(t, 0)
	const key uint64 = 6201
	fake := &fakeAppliedEpochStore{epochs: map[uint64]uint64{key: 5}}
	w.appliedEpochs = fake

	legacy := makeEpochWriteTask(t, key, "taskpb.TaskResult", 10, 0)
	w.handleTask(legacy, true)

	require.Len(t, h.callsForKey(key), 1, "legacy producer must be allowed during the compat window")
	assert.Zero(t, fake.readCount(), "epoch 0 must not cost a Redis round trip")
	assert.Equal(t, uint64(5), fake.epochs[key], "a legacy write carries no epoch and must not move the applied epoch")
}

// 读失败既不能放行(Redis 抖一下就串档)也不能原地丢(丢盘):走既有可重试路径。
func TestOwnerEpochGuard_ReadFailureIsRetriedNotDropped(t *testing.T) {
	const key uint64 = 6301
	const msgType = "taskpb.TaskResult"

	t.Run("kafka origin goes to retry queue then ACKs", func(t *testing.T) {
		w, _, h := newTestWorker(t, 0)
		w.appliedEpochs = &fakeAppliedEpochStore{err: errors.New("redis timeout")}
		wt := makeEpochWriteTask(t, key, msgType, 10, 5)
		session := attachKafkaClaim(t, w, wt)
		w.handleTask(wt, true)

		assert.Empty(t, h.callsForKey(key), "must not reach MySQL while ownership is unknown")
		readyLen, err := w.redisClient.LLen(w.ctx, w.retryQueueKey).Result()
		require.NoError(t, err)
		assert.Equal(t, int64(1), readyLen, "exactly one durable retry copy")
		assert.Equal(t, []int64{9}, session.marked(), "Kafka ACKs only after the retry copy is durable")
		_, err = w.redisClient.Get(w.ctx, appliedSeqKey(w.topic, key, msgType)).Result()
		assert.ErrorIs(t, err, redis.Nil, "deferred task must not publish an applied cursor")

		popped, err := w.redisClient.RPop(w.ctx, w.retryQueueKey).Bytes()
		require.NoError(t, err)
		gotSeq, gotPartition, hasPartition, taskBytes := unwrapRetryPayload(popped)
		assert.Equal(t, uint64(10), gotSeq)
		assert.True(t, hasPartition)
		assert.Equal(t, int32(0), gotPartition)
		var retried db_proto.DBTask
		require.NoError(t, proto.Unmarshal(taskBytes, &retried))
		assert.Equal(t, uint64(5), retried.OwnerEpoch, "retry payload must carry the epoch so the guard re-runs on replay")
	})

	t.Run("retry origin moves receipt back to ready", func(t *testing.T) {
		w, _, h := newTestWorker(t, 0)
		w.appliedEpochs = &fakeAppliedEpochStore{err: errors.New("redis timeout")}
		wt := makeEpochWriteTask(t, key, msgType, 10, 5)
		wt.fromRetry = true
		wt.retryReceipt = []byte("receipt-read-failure")
		require.NoError(t, w.redisClient.LPush(w.ctx, w.retryProcessingKey, wt.retryReceipt).Err())
		w.handleTask(wt, true)

		assert.Empty(t, h.callsForKey(key))
		processingLen, err := w.redisClient.LLen(w.ctx, w.retryProcessingKey).Result()
		require.NoError(t, err)
		assert.Zero(t, processingLen, "receipt must leave processing")
		readyLen, err := w.redisClient.LLen(w.ctx, w.retryQueueKey).Result()
		require.NoError(t, err)
		assert.Equal(t, int64(1), readyLen, "receipt must be back in ready for another attempt")
		deadLen, err := w.redisClient.LLen(w.ctx, w.retryDeadQueueKey).Result()
		require.NoError(t, err)
		assert.Zero(t, deadLen, "a transient read failure is not a dead-letter reason")
	})
}

// 丢弃不推进 cursor,但绝不能因此卡住同 key 之后的正常写。
func TestOwnerEpochGuard_DroppedWriteDoesNotBlockLaterWrites(t *testing.T) {
	w, _, h := newTestWorker(t, 0)
	const key uint64 = 6401
	const msgType = "taskpb.TaskResult"
	setAppliedEpoch(t, w, key, msgType, 5)

	w.handleTask(makeEpochWriteTask(t, key, msgType, 10, 3), true) // deposed node, dropped
	w.handleTask(makeEpochWriteTask(t, key, msgType, 11, 5), true) // current owner

	calls := h.callsForKey(key)
	require.Len(t, calls, 1)
	assert.Equal(t, uint64(11), extractSeqFromTaskID(calls[0].taskID))
	cursor, err := w.redisClient.Get(w.ctx, appliedSeqKey(w.topic, key, msgType)).Result()
	require.NoError(t, err)
	assert.Equal(t, "v2:0:11", cursor)

	// 同一条被丢的老写重复投递(rebalance 重放):cursor 已在前面,seq 守卫先挡;
	// 即使 cursor 没推进,epoch 守卫也会再挡一次。两道门任一都不能让它落库。
	w.handleTask(makeEpochWriteTask(t, key, msgType, 10, 3), true)
	assert.Len(t, h.callsForKey(key), 1)
}

// 合并器:epoch 变化必须当段边界。否则老节点带更大 offset 的迟到写会按 offset 赢得
// 合并、把新主的写 ACK 掉,再被守卫丢弃 —— 新主那一版就从 MySQL 消失了。
func TestProcessTaskBatch_EpochChangeIsNotCoalescedByOffset(t *testing.T) {
	const msgType = "taskpb.TaskResult"

	t.Run("zombie write with higher offset must not eat the new owner's write", func(t *testing.T) {
		w, _, h := newTestWorker(t, 0)
		const key uint64 = 6501
		newOwner := makeEpochWriteTask(t, key, msgType, 99, 2)
		zombie := makeEpochWriteTask(t, key, msgType, 100, 1)
		w.processTaskBatch([]*workerTask{newOwner, zombie}, true)

		calls := h.callsForKey(key)
		require.Len(t, calls, 1, "exactly the new owner's write must land")
		assert.Equal(t, newOwner.dbTask.TaskId, calls[0].taskID)
		cursor, err := w.redisClient.Get(w.ctx, appliedSeqKey(w.topic, key, msgType)).Result()
		require.NoError(t, err)
		assert.Equal(t, "v2:0:99", cursor, "the dropped zombie write must not become the cursor")
		for _, q := range []string{w.retryQueueKey, w.retryDeadQueueKey} {
			n, err := w.redisClient.LLen(w.ctx, q).Result()
			require.NoError(t, err)
			assert.Zero(t, n, "queue %s must stay empty", q)
		}
	})

	t.Run("normal handoff order lands both, previous owner first", func(t *testing.T) {
		w, _, h := newTestWorker(t, 0)
		const key uint64 = 6502
		oldOwner := makeEpochWriteTask(t, key, msgType, 100, 1)
		newOwner := makeEpochWriteTask(t, key, msgType, 101, 2)
		w.processTaskBatch([]*workerTask{oldOwner, newOwner}, true)

		calls := h.callsForKey(key)
		require.Len(t, calls, 2, "the previous owner's final state is a legitimate write, not a zombie")
		assert.Equal(t, oldOwner.dbTask.TaskId, calls[0].taskID)
		assert.Equal(t, newOwner.dbTask.TaskId, calls[1].taskID)
		epoch, _ := appliedEpochOf(t, w, key, msgType)
		assert.Equal(t, uint64(2), epoch)
	})

	t.Run("same epoch still coalesces to the latest offset", func(t *testing.T) {
		w, _, h := newTestWorker(t, 0)
		const key uint64 = 6503
		w.processTaskBatch([]*workerTask{
			makeEpochWriteTask(t, key, msgType, 1, 2),
			makeEpochWriteTask(t, key, msgType, 2, 2),
			makeEpochWriteTask(t, key, msgType, 3, 2),
		}, true)

		calls := h.callsForKey(key)
		require.Len(t, calls, 1, "epoch awareness must not disable coalescing within one ownership period")
		assert.Equal(t, uint64(3), extractSeqFromTaskID(calls[0].taskID))
	})

	t.Run("legacy zero epoch does not split a segment", func(t *testing.T) {
		w, _, h := newTestWorker(t, 0)
		const key uint64 = 6504
		w.processTaskBatch([]*workerTask{
			makeEpochWriteTask(t, key, msgType, 1, 0),
			makeEpochWriteTask(t, key, msgType, 2, 2),
		}, true)

		calls := h.callsForKey(key)
		require.Len(t, calls, 1, "0 is 'unknown', never a divergence")
		assert.Equal(t, uint64(2), extractSeqFromTaskID(calls[0].taskID))
	})
}

// ---------------------------------------------------------------------------
// TC6 — high-concurrency soak (smoke; runs in seconds)
// ---------------------------------------------------------------------------

// Drives many keys across many workers concurrently and asserts that each
// key's final applied write equals its global max seq. This is the
// goroutine-level analogue of the production invariant; it should pass even
// without the bug fixes above, because each worker drains its own taskCh
// serially and we don't inject failures here.
func TestConcurrentWorkers_FinalStateIsGlobalMax(t *testing.T) {
	if testing.Short() {
		t.Skip("soak test; skipping in -short mode")
	}

	const partitions int32 = 4
	const playersPerPartition = 50
	const writesPerPlayer = 100
	const msgType = "taskpb.TaskResult"

	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })

	// One harness shared across all workers (handlers are package-level).
	h := newHarness(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	workers := make([]*worker, partitions)
	for i := int32(0); i < partitions; i++ {
		workers[i] = &worker{
			partition:          i,
			taskCh:             make(chan *workerTask, 4096),
			ctx:                ctx,
			redisClient:        rc,
			locker:             db_locker.NewRedisLocker(rc),
			topic:              "soak",
			retryQueueKey:      "kafka:retry:queue:soak",
			retryProcessingKey: "kafka:retry:processing:soak",
			retryDeadQueueKey:  "kafka:dead:queue:soak",
			wg:                 &sync.WaitGroup{},
		}
	}

	// Launch worker goroutines (mirrors `worker.start` minus offset marking).
	stopWG := sync.WaitGroup{}
	for _, w := range workers {
		stopWG.Add(1)
		go func(w *worker) {
			defer stopWG.Done()
			for {
				select {
				case <-w.ctx.Done():
					return
				case task, ok := <-w.taskCh:
					if !ok {
						return
					}
					batch := []*workerTask{task}
				drain:
					for {
						select {
						case t2, ok := <-w.taskCh:
							if !ok {
								break drain
							}
							batch = append(batch, t2)
						default:
							break drain
						}
					}
					w.processTaskBatch(batch, true)
				}
			}
		}(w)
	}

	// Producers: each player gets writes 1..N, dispatched to the worker
	// chosen by `key % partitions` (matching production).
	totalKeys := uint64(int(partitions) * playersPerPartition)
	var produced atomic.Uint64

	produceWG := sync.WaitGroup{}
	for k := uint64(1); k <= totalKeys; k++ {
		produceWG.Add(1)
		go func(key uint64) {
			defer produceWG.Done()
			for seq := uint64(1); seq <= writesPerPlayer; seq++ {
				wt := makeWriteTask(t, key, msgType, seq)
				w := workers[key%uint64(partitions)]
				wt.originPartition = w.partition
				select {
				case w.taskCh <- wt:
					produced.Add(1)
				case <-time.After(2 * time.Second):
					t.Errorf("dispatch stalled for key=%d seq=%d", key, seq)
					return
				}
			}
		}(k)
	}
	produceWG.Wait()

	// Wait for the final write of every key, not merely len(taskCh)==0: a
	// sub-worker may already have drained the channel while its last DB/Redis
	// critical section is still in flight.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		lastByKey := make(map[uint64]uint64, int(totalKeys))
		for _, call := range h.snapshot() {
			lastByKey[call.key] = extractSeqFromTaskID(call.taskID)
		}
		converged := true
		for key := uint64(1); key <= totalKeys; key++ {
			if lastByKey[key] != writesPerPlayer {
				converged = false
				break
			}
		}
		if converged {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	stopWG.Wait()

	// Per-key invariant: for each key, the LAST recorded call's seq must
	// equal writesPerPlayer (global max). Earlier calls may exist due to
	// per-batch coalescing landing on different intermediate seqs.
	for k := uint64(1); k <= totalKeys; k++ {
		calls := h.callsForKey(k)
		require.NotEmpty(t, calls, "key %d had no effective writes", k)
		last := extractSeqFromTaskID(calls[len(calls)-1].taskID)
		assert.Equal(t, uint64(writesPerPlayer), last,
			"key %d: final applied seq must be the global max", k)
	}
	t.Logf("soak: produced=%d, totalKeys=%d, writesPerPlayer=%d",
		produced.Load(), totalKeys, writesPerPlayer)
}
