package dataloader

import (
	"context"
	"fmt"
	"login/internal/config"
	"login/internal/dispatcher"
	db_proto "proto/db"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

// 通过同一调用面注入发送与回调登记；冷恢复测试不需要绕过 SQL 回包校验。
type readResultRegistrar interface {
	Register(string, time.Duration, dispatcher.ResultCallback)
	// ReplyInstance 是登记方的读回执回复目标;与 Register 放在同一接口,保证写进读任务的
	// 回复目标与接住回调的是同一个对象(见 dispatcher.TaskResultDispatcher.ReplyInstance)。
	ReplyInstance() string
	// Cancel 撤销已登记、但任务没能发出去的回调,不回调、不计数(见 dispatcher.TaskResultDispatcher.Cancel)。
	Cancel(taskIDs ...string)
}

// EnsurePlayerAllDataInRedisAsync guarantees that the
// `PlayerAllData:{playerId}` Redis parent key exists and invokes `onComplete`
// exactly once when the work is finished (or has failed).
//
// Crucially, this function NEVER blocks the caller waiting for DB results.
// Sub-table fetches are dispatched via the TaskResultDispatcher (Redis Pub/Sub
// driven). When the last sub-result arrives, this function assembles the
// parent blob, SETs it in Redis, and fires onComplete.
//
// Fast path: the parent key is already cached → onComplete(nil) is invoked
// inline.
//
// Slow path: Kafka task send happens in this goroutine (SyncProducer is fast
// once the connection is warm). All result waits are event-driven — no
// goroutine sits on BLPOP.
//
// `perTaskTTL` bounds how long an individual task registration may sit
// unfulfilled before the dispatcher fires its callback with a timeout error.
// Pass 0 to use the dispatcher default.
func EnsurePlayerAllDataInRedisAsync(
	ctx context.Context,
	rc redis.Cmdable,
	disp readResultRegistrar,
	producer dbTaskProducer,
	playerId uint64,
	perTaskTTL time.Duration,
	onComplete func(error),
) {
	ensurePlayerAllDataInRedisAsync(ctx, rc, defaultParentLoadScripts,
		config.AppConfig.ParentLoad.VerifyOnRedisIdentityChange, disp, producer, playerId, perTaskTTL, onComplete)
}

// 身份读取脚本显式注入；替身只替换不支持的 INFO，不替换判定、SQL 回包校验或原子提交。
func ensurePlayerAllDataInRedisAsync(ctx context.Context, rc redis.Cmdable, scripts parentLoadScripts,
	verifyOnIdentityChange bool, disp readResultRegistrar, producer dbTaskProducer, playerId uint64,
	perTaskTTL time.Duration, onComplete func(error)) {
	// 先建解码容器；任一 SQL Data 缺失必须拒绝，不能把此容器当真实初始行。
	parent := newPlayerAllDataSkeleton(playerId)
	parentKey := buildParentKey(parent, playerId)

	// C4 的模式:login.go 启动期已拒绝非法取值,这里解析失败只可能是运行期改写了配置,
	// 按入场失败处理,不猜一个模式继续。
	regressionMode, modeErr := config.ParsePersistSchemaRegressionMode(config.AppConfig.PersistSchemaRegressionMode)
	if modeErr != nil {
		onComplete(modeErr)
		return
	}

	// 预检与最终发布都复核归属和 SQL 提交屏障；Redis 故障不能按缓存未命中继续。
	cacheCheckStart := time.Now()
	snapshot, snapshotErr := beginParentLoad(ctx, rc, scripts, parentKey, playerId, regressionMode)
	hitLabel := "miss"
	if snapshotErr == nil && snapshot.raw != "" {
		hitLabel = "hit"
	}
	cacheCheckSeconds.ObserveFloat(time.Since(cacheCheckStart).Seconds(), hitLabel)
	if snapshotErr != nil {
		onComplete(snapshotErr)
		return
	}

	// 父键存在也不等于可用：身份变化、ACK 超前与历史缺口必须先读两张 SQL 权威行。
	route, _ := snapshot.route(verifyOnIdentityChange)
	switch route {
	case parentLoadFast:
		onComplete(nil)
		return
	case parentLoadWait:
		onComplete(errParentBarrierNotCommitted)
		return
	}

	playerIdStr := strconv.FormatUint(playerId, 10)

	type pendingRead struct {
		taskID string
		sub    proto.Message
	}
	var pending []pendingRead
	var tasks []*dbReadTask
	subs := collectSubMessages(parent)

	// 核验路径强制读两张 SQL 行。子表缓存可能落后，不能作为恢复权威。
	subCacheStart := time.Now()
	for _, sub := range subs {
		subKey := fmt.Sprintf("%s:%s", sub.ProtoReflect().Descriptor().FullName(), playerIdStr)
		taskID := uuid.NewString()
		tasks = append(tasks, &dbReadTask{taskID: taskID, message: sub, subKey: subKey})
		pending = append(pending, pendingRead{taskID: taskID, sub: sub})
	}
	// Label by "needs db fetch count" so a 0-fetch path (sub-cache full
	// hit) is visually separate from a 1-2-fetch path (typical) and a
	// many-fetch path (cold cache after restart).
	subCacheCheckSeconds.ObserveFloat(
		time.Since(subCacheStart).Seconds(),
		strconv.Itoa(len(tasks)),
	)

	if len(tasks) == 0 {
		if _, err := snapshot.settle(ctx, rc, scripts, parentKey, playerId, parent, regressionMode); err != nil {
			onComplete(fmt.Errorf("set parent key %s: %w", parentKey, err))
			return
		}
		onComplete(nil)
		return
	}

	// Fan-in coordinator: each registered callback decrements the counter.
	// When the counter hits zero we assemble + SET + fire onComplete. A
	// once+sync.Once wrapper guarantees onComplete fires exactly once even if
	// multiple callbacks happen to error.
	var (
		remaining atomic.Int32
		firstErr  atomic.Pointer[string]
		mu        sync.Mutex
		fireOnce  sync.Once
	)
	remaining.Store(int32(len(pending)))

	// callbackWaitStart captures t0 for stage 5 (Kafka-sent → finalize).
	// It is set inside the closure-returning helper so all timers use the
	// same clock origin; finalize then observes against this when it fires.
	// 回调可能在 SendTasks 返回前到达；时间原点在发布前固定，避免读写竞态。
	callbackWaitStart := time.Now()

	finalize := func() {
		fireOnce.Do(func() {
			// Stage 5 observation: wait from "kafka sent" to "finalize fires".
			// callbackWaitStart is zero only if we never got to the Kafka
			// send (registered+returned-early path); guard against that to
			// avoid logging bogus deltas.
			if !callbackWaitStart.IsZero() {
				if errPtr := firstErr.Load(); errPtr != nil {
					callbackWaitSeconds.ObserveFloat(time.Since(callbackWaitStart).Seconds(), resultFailed)
					callbackResultTotal.Inc(resultFailed)
				} else {
					callbackWaitSeconds.ObserveFloat(time.Since(callbackWaitStart).Seconds(), resultSuccess)
					callbackResultTotal.Inc(resultSuccess)
				}
			}

			if errPtr := firstErr.Load(); errPtr != nil {
				onComplete(fmt.Errorf("%s", *errPtr))
				return
			}
			if _, err := snapshot.settle(ctx, rc, scripts, parentKey, playerId, parent, regressionMode); err != nil {
				onComplete(fmt.Errorf("set parent key %s: %w", parentKey, err))
				return
			}
			logx.Infof("EnsurePlayerAllDataInRedisAsync: parent written for playerId=%d (%d sub reads)", playerId, len(tasks))
			onComplete(nil)
		})
	}

	// Stage 3: dispatcher Register loop.
	// Register dispatcher callbacks BEFORE sending the Kafka tasks to avoid
	// the (rare) race where a result arrives before the registration lands.
	registerStart := time.Now()
	for _, p := range pending {
		p := p
		disp.Register(p.taskID, perTaskTTL, func(res *db_proto.TaskResult, err error) {
			defer func() {
				if remaining.Add(-1) == 0 {
					finalize()
				}
			}()

			if err != nil {
				errStr := fmt.Sprintf("task %s wait: %v", p.taskID, err)
				firstErr.CompareAndSwap(nil, &errStr)
				return
			}
			if res == nil || !res.Success {
				if res == nil {
					res = &db_proto.TaskResult{Error: "empty SQL read result"}
				}
				errStr := fmt.Sprintf("db read failed for task %s: %s", p.taskID, res.Error)
				firstErr.CompareAndSwap(nil, &errStr)
				return
			}
			// 缺行不能等同已提交的初始空行，否则会把历史数据丢失伪装成新号。
			if len(res.Data) == 0 {
				errStr := fmt.Sprintf("玩家 SQL 权威历史行缺失，拒绝重建初始数据: %s", p.sub.ProtoReflect().Descriptor().FullName())
				firstErr.CompareAndSwap(nil, &errStr)
				return
			}
			if len(res.Data) > 0 {
				mu.Lock()
				if uerr := proto.Unmarshal(res.Data, p.sub); uerr != nil {
					mu.Unlock()
					errStr := fmt.Sprintf("unmarshal sub %s: %v", p.taskID, uerr)
					firstErr.CompareAndSwap(nil, &errStr)
					return
				}
				mu.Unlock()
			}
			if validationErr := validateSQLRowIdentity(p.sub, playerId); validationErr != nil {
				errStr := fmt.Sprintf("task %s: %v", p.taskID, validationErr)
				firstErr.CompareAndSwap(nil, &errStr)
			}
		})
	}
	dispatcherRegisterSeconds.ObserveFloat(time.Since(registerStart).Seconds())

	// Stage 4: Kafka SyncProducer.SendMessages call.
	kafkaSendStart := time.Now()
	// 回复目标必须取自上面登记回调的同一个 disp,通知才会投到持有这些回调的进程。
	if err := sendDBReadRequests(ctx, producer, tasks, playerId, playerIdStr, disp.ReplyInstance()); err != nil {
		kafkaSendSeconds.ObserveFloat(time.Since(kafkaSendStart).Seconds(), resultFailed)
		errStr := fmt.Sprintf("send db read tasks: %v", err)
		firstErr.CompareAndSwap(nil, &errStr)
		// 先撤掉本批登记再以错误收尾。留给 TTL sweep 清理的话,每条都要在到期时多做一次无用的
		// LPOP,并把这次 Kafka 发送失败计成 login_task_result_delivery_total{outcome="timed_out"},
		// 让「通知丢了且结果没取到」的口径混进发送故障。若有部分任务其实已发出,它们的结果只在
		// Redis 里按 TTL 过期;撤销前已被取走的回调照常执行,onComplete 仍由 fireOnce 保证恰好一次。
		cancelled := make([]string, 0, len(pending))
		for _, p := range pending {
			cancelled = append(cancelled, p.taskID)
		}
		disp.Cancel(cancelled...)
		fireOnce.Do(func() { onComplete(fmt.Errorf("%s", errStr)) })
		return
	}
	kafkaSendSeconds.ObserveFloat(time.Since(kafkaSendStart).Seconds(), resultSuccess)

}
