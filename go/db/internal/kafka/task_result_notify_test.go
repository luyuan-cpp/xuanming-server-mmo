package kafka

// 读回执通知的目标判定、回落与计数用例(B0-3 第 2 批)。用到 metrics 与本包的新符号,改造前是编译红;
// 只依赖签名不变入口的路由用例见 task_result_notify_route_test.go(那边的 fixture 与记录器这里复用)。
//
// db_task_result_notify_total 是进程级累计值,用例一律比较前后差值(metricCount 经 DefaultGatherer 读,见
// instrumentation_test.go)。计数器在计数入口里自注册,这里不调 metrics.Start:读得到差值本身就证明了自注册。
// 等待只用来等进程内订阅者转发消息,有上限,不拿时间判断对错。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"db/internal/metrics"
	db_proto "proto/db"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const notifyDeliveryWaitLimit = 5 * time.Second

var (
	allNotifyTargets = []string{metrics.TaskResultNotifyInstance, metrics.TaskResultNotifyLegacyUnset, metrics.TaskResultNotifyLegacyInvalid}
	allNotifyResults = []string{metrics.TaskResultNotifyOK, metrics.TaskResultNotifyNoReceiver, metrics.TaskResultNotifyError}
)

// notifyCounts 取 db_task_result_notify_total 全部 9 条序列,键为 "target|result"。
func notifyCounts(t *testing.T) map[string]float64 {
	t.Helper()
	out := make(map[string]float64, len(allNotifyTargets)*len(allNotifyResults))
	for _, target := range allNotifyTargets {
		for _, result := range allNotifyResults {
			out[target+"|"+result] = metricCount(t, "db_task_result_notify_total", map[string]string{"target": target, "result": result})
		}
	}
	return out
}

// assertNotifyCounted 断言 before → after 之间只有 target|result 这一条序列加了 1,其余 8 条不变。
// want 为空表示 9 条都不变。
func assertNotifyCounted(t *testing.T, before, after map[string]float64, want string) {
	t.Helper()
	for key, b := range before {
		delta := after[key] - b
		if key == want {
			assert.Equal(t, 1.0, delta, "db_task_result_notify_total{%s} must count exactly once", key)
			continue
		}
		assert.Equal(t, 0.0, delta, "db_task_result_notify_total{%s} must not change", key)
	}
}

// subscribeLikeLogin 在 miniredis 上起一个进程内订阅者订 channels(新版 login 同时订旧频道与本实例频道),
// 收到的消息转进带缓冲的通道。miniredis 的订阅者通道无缓冲,PUBLISH 会阻塞到有人读走,所以转发协程先起。
func subscribeLikeLogin(t *testing.T, mr *miniredis.Miniredis, channels ...string) <-chan miniredis.PubsubMessage {
	t.Helper()
	sub := mr.NewSubscriber()
	for _, ch := range channels {
		sub.Subscribe(ch)
	}
	got := make(chan miniredis.PubsubMessage, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for m := range sub.Messages() {
			got <- m
		}
	}()
	t.Cleanup(func() {
		// 先退订再关:关掉之后若还有 PUBLISH 命中这个订阅者,miniredis 会往已关闭的通道发送。
		for _, ch := range channels {
			sub.Unsubscribe(ch)
		}
		sub.Close()
		<-done
	})
	return got
}

// receiveNotify 等一条转发来的消息,超过 notifyDeliveryWaitLimit 即失败。
func receiveNotify(t *testing.T, got <-chan miniredis.PubsubMessage) miniredis.PubsubMessage {
	t.Helper()
	select {
	case m := <-got:
		return m
	case <-time.After(notifyDeliveryWaitLimit):
		t.Fatalf("no notification delivered within %v", notifyDeliveryWaitLimit)
		return miniredis.PubsubMessage{}
	}
}

func readTask(taskID, replyInstance string) *db_proto.DBTask {
	return &db_proto.DBTask{Key: 42, Op: db_proto.DBOp_DB_OP_READ, MsgType: "taskpb.TaskResult", TaskId: taskID, ReplyInstance: replyInstance}
}

func TestTaskResultNotifyTarget(t *testing.T) {
	for _, tc := range []struct {
		name          string
		replyInstance string
		wantChannel   string
		wantTarget    string
	}{
		{"unset", "", wireTaskResultLegacyChannel, metrics.TaskResultNotifyLegacyUnset},
		{"uuid", testReplyInstance, wireTaskResultInstancePrefix + testReplyInstance, metrics.TaskResultNotifyInstance},
		{"single char", "a", wireTaskResultInstancePrefix + "a", metrics.TaskResultNotifyInstance},
		{"max length", strings.Repeat("a", 64), wireTaskResultInstancePrefix + strings.Repeat("a", 64), metrics.TaskResultNotifyInstance},
		{"too long", strings.Repeat("a", 65), wireTaskResultLegacyChannel, metrics.TaskResultNotifyLegacyInvalid},
		{"colon", "a:b", wireTaskResultLegacyChannel, metrics.TaskResultNotifyLegacyInvalid},
		{"looks like the legacy channel", "task:result:notify", wireTaskResultLegacyChannel, metrics.TaskResultNotifyLegacyInvalid},
		{"glob star", "*", wireTaskResultLegacyChannel, metrics.TaskResultNotifyLegacyInvalid},
		{"glob class", "[a]", wireTaskResultLegacyChannel, metrics.TaskResultNotifyLegacyInvalid},
		{"space", "a b", wireTaskResultLegacyChannel, metrics.TaskResultNotifyLegacyInvalid},
		{"non ascii", "登录", wireTaskResultLegacyChannel, metrics.TaskResultNotifyLegacyInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			channel, target := taskResultNotifyTarget(tc.replyInstance)
			assert.Equal(t, tc.wantChannel, channel)
			assert.Equal(t, tc.wantTarget, target)
		})
	}
}

func TestTaskResultNotifyResult(t *testing.T) {
	boom := errors.New("redis down")
	assert.Equal(t, metrics.TaskResultNotifyNoReceiver, taskResultNotifyResult(0, nil))
	assert.Equal(t, metrics.TaskResultNotifyOK, taskResultNotifyResult(1, nil))
	assert.Equal(t, metrics.TaskResultNotifyOK, taskResultNotifyResult(2, nil))
	assert.Equal(t, metrics.TaskResultNotifyError, taskResultNotifyResult(0, boom))
	assert.Equal(t, metrics.TaskResultNotifyError, taskResultNotifyResult(3, boom), "an error wins over a stale receiver count")
}

// 新版 login 两条频道都订着:合法回复目标只在实例频道上送达一次,计 instance|ok。
func TestPublishTaskResult_InstanceDeliveredOnce(t *testing.T) {
	mr, rc, rec := newNotifyFixture(t, nil)
	instanceChannel := wireTaskResultInstancePrefix + testReplyInstance
	got := subscribeLikeLogin(t, mr, wireTaskResultLegacyChannel, instanceChannel)
	before := notifyCounts(t)

	require.NoError(t, publishTaskResult(context.Background(), rc, readTask("t-10", testReplyInstance), &db_proto.TaskResult{Success: true}))

	assertNotifiedOnce(t, rec, instanceChannel, "t-10")
	assert.EqualValues(t, 1, rec.named("publish")[0].receivers)
	m := receiveNotify(t, got)
	assert.Equal(t, instanceChannel, m.Channel)
	assert.Equal(t, "t-10", m.Message)
	assertNotifyCounted(t, before, notifyCounts(t), metrics.TaskResultNotifyInstance+"|"+metrics.TaskResultNotifyOK)
}

// 发起方 login 已经不在(重启、缩容):实例频道没人订,PUBLISH 返回 0,计 instance|no_receiver。
// 结果仍在列表里(TTL 5 分钟后过期),函数照常返回 nil。
func TestPublishTaskResult_NoReceiverCounted(t *testing.T) {
	mr, rc, rec := newNotifyFixture(t, nil)
	before := notifyCounts(t)

	require.NoError(t, publishTaskResult(context.Background(), rc, readTask("t-11", testReplyInstance), &db_proto.TaskResult{Success: true}))

	assertNotifiedOnce(t, rec, wireTaskResultInstancePrefix+testReplyInstance, "t-11")
	assert.EqualValues(t, 0, rec.named("publish")[0].receivers)
	assert.True(t, readNotifyResult(t, mr, "t-11").GetSuccess())
	assertNotifyCounted(t, before, notifyCounts(t), metrics.TaskResultNotifyInstance+"|"+metrics.TaskResultNotifyNoReceiver)
}

// 空或不合法的回复目标回落旧频道,只发一次;旧频道上订着的 login(新旧版都订)照样收到。
// 空串一例改造前就是这个行为,作兼容守卫(旧版 login 不填 10 号字段)。
func TestPublishTaskResult_FallsBackToLegacyChannelOnce(t *testing.T) {
	for _, tc := range []struct {
		name          string
		replyInstance string
		wantTarget    string
	}{
		{"unset", "", metrics.TaskResultNotifyLegacyUnset},
		{"colon", "a:b", metrics.TaskResultNotifyLegacyInvalid},
		{"glob", "*", metrics.TaskResultNotifyLegacyInvalid},
		{"too long", strings.Repeat("a", 65), metrics.TaskResultNotifyLegacyInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr, rc, rec := newNotifyFixture(t, nil)
			got := subscribeLikeLogin(t, mr, wireTaskResultLegacyChannel)
			before := notifyCounts(t)

			require.NoError(t, publishTaskResult(context.Background(), rc, readTask("t-12", tc.replyInstance), &db_proto.TaskResult{Success: true}))

			assertNotifiedOnce(t, rec, wireTaskResultLegacyChannel, "t-12")
			m := receiveNotify(t, got)
			assert.Equal(t, wireTaskResultLegacyChannel, m.Channel)
			assert.Equal(t, "t-12", m.Message)
			assertNotifyCounted(t, before, notifyCounts(t), tc.wantTarget+"|"+metrics.TaskResultNotifyOK)
		})
	}
}

// 通知失败只拉长时延:结果已经在列表里、由 login 的 TTL 兜底取回,所以返回 nil(调用方不会因此延后重试),
// 计 instance|error。
func TestPublishTaskResult_NotifyFailureKeepsResult(t *testing.T) {
	mr, rc, rec := newNotifyFixture(t, map[string]error{"publish": errors.New("injected publish failure")})
	before := notifyCounts(t)

	require.NoError(t, publishTaskResult(context.Background(), rc, readTask("t-13", testReplyInstance), &db_proto.TaskResult{Success: true}))

	require.Len(t, rec.named("publish"), 1, "a failed notification must not be retried or sent to a second channel")
	assert.Error(t, rec.named("publish")[0].err)
	assert.True(t, readNotifyResult(t, mr, "t-13").GetSuccess())
	assertNotifyCounted(t, before, notifyCounts(t), metrics.TaskResultNotifyInstance+"|"+metrics.TaskResultNotifyError)
}

// 结果没写进去就不通知、不计数,并返回错误交调用方延后重试:先通知后写结果会让 login LPOP 落空。
func TestPublishTaskResult_ResultWriteFailureSkipsNotify(t *testing.T) {
	_, rc, rec := newNotifyFixture(t, map[string]error{"lpush": errors.New("injected lpush failure")})
	before := notifyCounts(t)

	err := publishTaskResult(context.Background(), rc, readTask("t-14", testReplyInstance), &db_proto.TaskResult{Success: true})

	require.Error(t, err)
	assert.Empty(t, rec.named("publish"), "no notification may be sent before the result list exists")
	assertNotifyCounted(t, before, notifyCounts(t), "")
}

// 限速器的 key 只能落在 target × result 的常量组合里:回复目标与 task_id 每条都不同,用它们做 key 会让
// logLimiter 的 map 无界增长。
func TestTaskResultNotifyLogKeysStayClosed(t *testing.T) {
	_, invalidRC, _ := newNotifyFixture(t, nil)
	_, failingRC, _ := newNotifyFixture(t, map[string]error{"publish": errors.New("injected publish failure")})
	for i := 0; i < 50; i++ {
		taskID := fmt.Sprintf("t-15-%d", i)
		require.NoError(t, publishTaskResult(context.Background(), invalidRC, readTask(taskID, fmt.Sprintf("bad:%d", i)), &db_proto.TaskResult{}))
		require.NoError(t, publishTaskResult(context.Background(), failingRC, readTask(taskID, fmt.Sprintf("login-%d", i)), &db_proto.TaskResult{}))
		require.NoError(t, publishTaskResult(context.Background(), failingRC, readTask(taskID, ""), &db_proto.TaskResult{}))
	}

	allowed := map[string]bool{}
	for _, target := range allNotifyTargets {
		for _, result := range allNotifyResults {
			allowed[taskResultNotifyLogKey(target, result)] = true
		}
	}
	taskResultNotifyLogLimiter.mu.Lock()
	defer taskResultNotifyLogLimiter.mu.Unlock()
	assert.LessOrEqual(t, len(taskResultNotifyLogLimiter.last), len(allowed))
	for key := range taskResultNotifyLogLimiter.last {
		assert.True(t, allowed[key], "log limiter key %q is not a constant target/result combination", key)
	}
	for key := range taskResultNotifyLogLimiter.held {
		assert.True(t, allowed[key], "log limiter key %q is not a constant target/result combination", key)
	}
}
