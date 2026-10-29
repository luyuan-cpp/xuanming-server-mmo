package dataloader

// 读任务带回复目标(DBTask.reply_instance)的单测:go/db 按它把读回执的到达通知只发到发起方
// login 进程的实例频道(go/shared/taskresult)。全部跑在进程内,不碰 Kafka / Pub/Sub。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"login/internal/dispatcher"
	login_proto_database "proto/common/database"
	login_proto "proto/db"
)

const testReplyInstance = "0b6c2f5e-6a3d-4c1b-9f2e-8d7a6b5c4d3e"

// replyStampRecorder 同时充当回调登记方与读任务生产者:只记下登记的 taskID 与发出的任务,
// 不回调,收尾由用例显式驱动。sendErr 非空时发送记下任务后返回该错误,模拟 Kafka 发送失败。
type replyStampRecorder struct {
	instance   string
	registered map[string]dispatcher.ResultCallback
	cancelled  []string
	single     int
	batch      int
	sent       []*login_proto.DBTask
	sendErr    error
}

func (r *replyStampRecorder) Register(id string, _ time.Duration, cb dispatcher.ResultCallback) {
	if r.registered == nil {
		r.registered = make(map[string]dispatcher.ResultCallback)
	}
	r.registered[id] = cb
}

func (r *replyStampRecorder) ReplyInstance() string { return r.instance }

func (r *replyStampRecorder) Cancel(ids ...string) {
	r.cancelled = append(r.cancelled, ids...)
	for _, id := range ids {
		delete(r.registered, id)
	}
}

func (r *replyStampRecorder) SendTask(_ context.Context, task *login_proto.DBTask, _ string) error {
	r.single++
	r.sent = append(r.sent, task)
	return r.sendErr
}

func (r *replyStampRecorder) SendTasks(_ context.Context, tasks []*login_proto.DBTask, _ string) error {
	r.batch++
	r.sent = append(r.sent, tasks...)
	return r.sendErr
}

func TestNewDBReadTask_StampsReplyInstance(t *testing.T) {
	sub := &login_proto_database.PlayerDatabase{PlayerId: 42}
	task, err := newDBReadTask(&dbReadTask{taskID: "t-1", message: sub}, 42, "42", testReplyInstance)
	if err != nil {
		t.Fatal(err)
	}
	if task.GetReplyInstance() != testReplyInstance {
		t.Fatalf("ReplyInstance = %q, want %q", task.GetReplyInstance(), testReplyInstance)
	}
	// 其余字段是 go/db 读路径的既有契约,加字段不得改动它们。
	wantType := string(sub.ProtoReflect().Descriptor().FullName())
	if task.GetKey() != 42 || task.GetWhereCase() != "player_id='42'" || task.GetOp() != login_proto.DBOp_DB_OP_READ ||
		task.GetMsgType() != wantType || task.GetTaskId() != "t-1" ||
		task.GetOwnerEpoch() != 0 || task.GetSaveSeq() != 0 || task.GetRetryCount() != 0 {
		t.Fatalf("read task fields changed: %+v", task)
	}
	body := &login_proto_database.PlayerDatabase{}
	if err := proto.Unmarshal(task.GetBody(), body); err != nil || !proto.Equal(body, sub) {
		t.Fatalf("body round-trip = %v err=%v, want %v", body, err, sub)
	}
}

func TestSendDBReadRequests_StampsEveryTask(t *testing.T) {
	newTasks := func(n int) []*dbReadTask {
		tasks := make([]*dbReadTask, n)
		for i := range tasks {
			tasks[i] = &dbReadTask{taskID: fmt.Sprintf("t-%d", i), message: &login_proto_database.PlayerDatabase_1{PlayerId: 7}}
		}
		return tasks
	}
	for _, c := range []struct {
		name       string
		n          int
		wantSingle int
		wantBatch  int
	}{
		{"单条走 SendTask", 1, 1, 0},
		{"多条走 SendTasks", 3, 0, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := &replyStampRecorder{}
			if err := sendDBReadRequests(context.Background(), rec, newTasks(c.n), 7, "7", testReplyInstance); err != nil {
				t.Fatal(err)
			}
			if rec.single != c.wantSingle || rec.batch != c.wantBatch || len(rec.sent) != c.n {
				t.Fatalf("SendTask=%d SendTasks=%d sent=%d, want %d / %d / %d",
					rec.single, rec.batch, len(rec.sent), c.wantSingle, c.wantBatch, c.n)
			}
			for _, task := range rec.sent {
				if task.GetReplyInstance() != testReplyInstance {
					t.Fatalf("task %s ReplyInstance = %q, want %q", task.GetTaskId(), task.GetReplyInstance(), testReplyInstance)
				}
			}
		})
	}
}

// 冷读路径上写进读任务的回复目标必须取自登记回调的同一个对象:取错了,通知会投到不持有这些
// 回调的进程,进场要等满 TTL 才靠兜底取回。
func TestEnsurePlayerAllDataInRedisAsync_StampsRegistrarReplyInstance(t *testing.T) {
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })

	rec := &replyStampRecorder{instance: testReplyInstance}
	calls := 0
	var gotErr error
	ensureTestPlayerAllDataInRedisAsync(context.Background(), rc, rec, rec, 42, 0, func(err error) {
		calls++
		gotErr = err
	})

	if len(rec.sent) == 0 || len(rec.sent) != len(rec.registered) {
		t.Fatalf("cold read sent %d tasks, registered %d; want equal and non-zero", len(rec.sent), len(rec.registered))
	}
	for _, task := range rec.sent {
		if task.GetReplyInstance() != testReplyInstance {
			t.Fatalf("task %s ReplyInstance = %q, want registrar's %q", task.GetTaskId(), task.GetReplyInstance(), testReplyInstance)
		}
		if _, ok := rec.registered[task.GetTaskId()]; !ok {
			t.Fatalf("task %s was sent but not registered on the same registrar", task.GetTaskId())
		}
	}

	// 收尾:以错误回调全部登记,组装以失败结束,onComplete 恰好一次、不写父键。
	for _, cb := range rec.registered {
		cb(nil, errors.New("test teardown"))
	}
	if calls != 1 || gotErr == nil {
		t.Fatalf("onComplete calls = %d err = %v, want 1 / non-nil", calls, gotErr)
	}
}

// Kafka 发送失败时必须撤掉本批全部登记:留着它们,每条都会在 TTL 到期时被兜底扫描计成
// timed_out,把发送故障伪装成 Pub/Sub 或 go/db 的送达问题。
func TestEnsurePlayerAllDataInRedisAsync_SendFailureCancelsRegistrations(t *testing.T) {
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })

	sendErr := errors.New("injected kafka send failure")
	rec := &replyStampRecorder{instance: testReplyInstance, sendErr: sendErr}
	calls := 0
	var gotErr error
	ensureTestPlayerAllDataInRedisAsync(context.Background(), rc, rec, rec, 42, 0, func(err error) {
		calls++
		gotErr = err
	})

	if calls != 1 || gotErr == nil || !strings.Contains(gotErr.Error(), sendErr.Error()) {
		t.Fatalf("onComplete calls = %d err = %v, want exactly 1 carrying %q", calls, gotErr, sendErr)
	}
	if len(rec.sent) == 0 {
		t.Fatal("cold read did not try to send any task")
	}
	if len(rec.registered) != 0 {
		t.Fatalf("%d registrations left behind after send failure, want 0", len(rec.registered))
	}
	cancelled := make(map[string]bool, len(rec.cancelled))
	for _, id := range rec.cancelled {
		cancelled[id] = true
	}
	for _, task := range rec.sent {
		if !cancelled[task.GetTaskId()] {
			t.Fatalf("task %s was registered but not cancelled after send failure", task.GetTaskId())
		}
	}
}
