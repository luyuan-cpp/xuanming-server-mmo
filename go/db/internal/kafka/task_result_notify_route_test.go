package kafka

// 读回执通知定向发布的路由用例(B0-3 第 2 批)。
//
// 本文件刻意只用改造前就存在、签名没变的入口(publishDBReadResult / failReadTask / encodeRetryPayload /
// unwrapRetryPayload)和频道字面值,不引用 shared/taskresult、metrics 的新常量或本包的新函数:与已重生成的
// go/proto/db/db_task.pb.go 一起放到改造前的提交上也能编译,拿到断言层面的红证据(改造前只发旧频道)。
// 字面值同时钉住与已部署 login 之间的线上契约(login 侧同法见 task_result_dispatcher_subscribe_test.go)。
//
// 只用 miniredis 与挂在 go-redis 客户端上的 ProcessHook,不依赖墙钟、不等待。

import (
	"context"
	"sync"
	"testing"

	db_proto "proto/db"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

const (
	wireTaskResultLegacyChannel  = "task:result:notify"
	wireTaskResultInstancePrefix = "task:result:notify:"
	// testReplyInstance 取 login 实际生成的形状(uuid.NewString,36 字节)。
	testReplyInstance = "3f1c2a9e-7b4d-4e21-9c55-0a8b6d2e1f37"
)

// recordedRedisCmd 是 publishRecorder 记下的一条命令。
type recordedRedisCmd struct {
	name      string
	args      []interface{}
	err       error
	receivers int64 // 只对成功的 publish 有意义:PUBLISH 返回的订阅者数
}

// publishRecorder 是挂在 go-redis 客户端上的 ProcessHook:按执行顺序记下每条命令。failOn 里列出的命令名不发给
// miniredis,直接以给定错误返回(模拟这一步的 Redis 故障)。连接建立时的 HELLO 等命令也经过这里,断言只按命令名挑。
type publishRecorder struct {
	mu     sync.Mutex
	cmds   []recordedRedisCmd
	failOn map[string]error
}

func (r *publishRecorder) DialHook(next redis.DialHook) redis.DialHook { return next }

func (r *publishRecorder) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (r *publishRecorder) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := r.failOn[cmd.Name()]
		if err == nil {
			err = next(ctx, cmd)
		}
		rec := recordedRedisCmd{name: cmd.Name(), args: append([]interface{}(nil), cmd.Args()...), err: err}
		if intCmd, ok := cmd.(*redis.IntCmd); ok && cmd.Name() == "publish" && err == nil {
			rec.receivers = intCmd.Val()
		}
		r.mu.Lock()
		r.cmds = append(r.cmds, rec)
		r.mu.Unlock()
		return err
	}
}

// named 按执行顺序返回名为 name 的命令。
func (r *publishRecorder) named(name string) []recordedRedisCmd {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []recordedRedisCmd
	for _, c := range r.cmds {
		if c.name == name {
			out = append(out, c)
		}
	}
	return out
}

// indexOf 返回第一条名为 name 的命令在全部记录里的序号,没有返回 -1。
func (r *publishRecorder) indexOf(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, c := range r.cmds {
		if c.name == name {
			return i
		}
	}
	return -1
}

// newNotifyFixture 起一个 miniredis 与挂了 publishRecorder 的客户端。failOn 在客户端发出第一条命令前就定好,之后只读。
func newNotifyFixture(t *testing.T, failOn map[string]error) (*miniredis.Miniredis, *redis.Client, *publishRecorder) {
	t.Helper()
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	rec := &publishRecorder{failOn: failOn}
	rc.AddHook(rec)
	return mr, rc, rec
}

// assertNotifiedOnce 断言恰好一条 PUBLISH,发到 wantChannel、载荷是 taskID;结果列表的 LPUSH 与 EXPIRE 都在它之前。
func assertNotifiedOnce(t *testing.T, rec *publishRecorder, wantChannel, taskID string) {
	t.Helper()
	publishes := rec.named("publish")
	require.Len(t, publishes, 1, "a read result must be notified exactly once (no double publish)")
	require.Len(t, publishes[0].args, 3, "PUBLISH channel message")
	assert.Equal(t, wantChannel, publishes[0].args[1], "notify channel")
	assert.Equal(t, taskID, publishes[0].args[2], "notify payload must be the bare task_id")

	pub, lpush, expire := rec.indexOf("publish"), rec.indexOf("lpush"), rec.indexOf("expire")
	require.GreaterOrEqual(t, lpush, 0, "the result list must be written")
	require.GreaterOrEqual(t, expire, 0, "the result list must get a TTL")
	assert.Less(t, lpush, pub, "LPUSH must precede PUBLISH: login LPOPs as soon as it is notified")
	assert.Less(t, expire, pub, "EXPIRE must precede PUBLISH")
	assert.Equal(t, "task:result:"+taskID, rec.named("lpush")[0].args[1], "result list key")
}

// readNotifyResult 直接从 miniredis 读 task:result:{taskID}(不经客户端,不进记录),要求恰好一条回执。
func readNotifyResult(t *testing.T, mr *miniredis.Miniredis, taskID string) *db_proto.TaskResult {
	t.Helper()
	values, err := mr.List("task:result:" + taskID)
	require.NoError(t, err, "the result list must exist")
	require.Len(t, values, 1)
	var res db_proto.TaskResult
	require.NoError(t, proto.Unmarshal([]byte(values[0]), &res))
	return &res
}

// 成功回执:带回复目标的读任务只通知发起方 login 的实例频道。
// 改造前红:实际频道是旧频道 task:result:notify。
func TestPublishDBReadResult_NotifiesReplyInstanceChannel(t *testing.T) {
	mr, rc, rec := newNotifyFixture(t, nil)
	row := &db_proto.TaskResult{Timestamp: 42} // 读出的行:任何 proto 消息都行,这里借用 TaskResult
	task := &db_proto.DBTask{Key: 42, Op: db_proto.DBOp_DB_OP_READ, MsgType: "taskpb.TaskResult", TaskId: "t-1", ReplyInstance: testReplyInstance}

	require.NoError(t, publishDBReadResult(context.Background(), rc, task, row))

	assertNotifiedOnce(t, rec, wireTaskResultInstancePrefix+testReplyInstance, "t-1")
	res := readNotifyResult(t, mr, "t-1")
	assert.True(t, res.GetSuccess())
	var got db_proto.TaskResult
	require.NoError(t, proto.Unmarshal(res.GetData(), &got))
	assert.EqualValues(t, 42, got.GetTimestamp(), "result data must be the row that was read")
}

// 落点失败回执(Placement.Required 且无记录)走同一条发布路径,同样只通知实例频道。
// 改造前红:实际频道是旧频道。
func TestFailReadTask_NotifiesReplyInstanceChannel(t *testing.T) {
	mr, rc, rec := newNotifyFixture(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	w := &worker{ctx: ctx, redisClient: rc}
	const reason = "placement required but no record"
	dbTask := &db_proto.DBTask{Key: 42, Op: db_proto.DBOp_DB_OP_READ, MsgType: "taskpb.TaskResult", TaskId: "t-2", ReplyInstance: testReplyInstance}

	w.failReadTask(&workerTask{}, dbTask, reason)

	assertNotifiedOnce(t, rec, wireTaskResultInstancePrefix+testReplyInstance, "t-2")
	res := readNotifyResult(t, mr, "t-2")
	assert.False(t, res.GetSuccess())
	assert.Equal(t, reason, res.GetError())
}

// 兼容守卫:读任务可能在重试队列里停留几分钟,重试载荷整份重新序列化 DBTask,回复目标必须原样带回,
// 否则从重试队列出来的读回执会退回全服广播。三种载荷格式(v2 带分区、v1 不带分区、legacy 裸任务)都要保留。
func TestEncodeRetryPayload_PreservesReplyInstance(t *testing.T) {
	task := &db_proto.DBTask{Key: 42, Op: db_proto.DBOp_DB_OP_READ, MsgType: "taskpb.TaskResult", TaskId: "t-3", ReplyInstance: testReplyInstance}
	for _, tc := range []struct {
		name         string
		seq          uint64
		partition    int32
		hasPartition bool
	}{
		{name: "v2", seq: 7, partition: 3, hasPartition: true},
		{name: "v1", seq: 7},
		{name: "legacy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload, err := encodeRetryPayload(task, tc.seq, tc.partition, tc.hasPartition)
			require.NoError(t, err)
			seq, partition, hasPartition, body := unwrapRetryPayload(payload)
			assert.Equal(t, tc.seq, seq)
			assert.Equal(t, tc.partition, partition)
			assert.Equal(t, tc.hasPartition, hasPartition)
			var decoded db_proto.DBTask
			require.NoError(t, proto.Unmarshal(body, &decoded))
			assert.Equal(t, testReplyInstance, decoded.GetReplyInstance())
			assert.Equal(t, "t-3", decoded.GetTaskId())
		})
	}
}
