package kafka

import (
	"context"
	"db/internal/logic/pkg/proto_sql"
	"db/internal/metrics"
	"db/internal/storeerr"
	"errors"
	"net"
	"sync"
	"time"

	db_proto "proto/db"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
)

// ───────────── 存储不可用时的延后(L-04,docs/design/db-task-store-unavailable-deferral.md)─────────────
//
// 原则:重试预算只花在任务自身的问题上(数据超限、表结构不符、消息解不出来)。存储此刻连不上、连接数满、
// 只读、忙,是环境问题,会随外部状态自己消失;为它耗预算就是把一笔完好的存盘推进没有消费者的死信。
//
// 分工:
//   - SQL 调用点(handleDBWriteOp / handleDBReadOp)用 storeerr.Tag 给驱动错误打来源标记;
//   - runDBOp 把 handler 这一步的结果报给落点库注册表(连接断开 → 探活 → 可能冷却;成功 → 解除冷却);
//     SQL 的 ctx 被本进程取消(写:失租;读:claim 收回或停机)时不报;
//   - retryBudgetExempt、deferReason 只认标记,不对任意错误跑类型判断(Redis 侧的超时不能算成 MySQL 不可用)。

// errOrderingLeaseLost 表示排序锁失租:续租放弃(次数用尽)、本地截止时间已过、或核锁发现锁已易主(ordering_lease.go)。
// 可能发生在 SQL 之前(一条 SQL 都不发)、SQL 途中(SQL 被取消,锁留给 TTL)或 SQL 之后(不发布缓存、不推进游标)。
// 用于延后原因的计数(lease_lost);是否豁免重试预算仍不在本批范围(条件写另议),照旧耗预算。
var errOrderingLeaseLost = errors.New("ordering lock ownership lost")

// storeHealth 是 StoreResolver 的可选能力:SQL 调用点把连接类故障与成功报给注册表,注册表据此进出库级冷却;
// 重试循环据此判断主库是否在冷却。生产由 *proto_sql.StoreRegistry 实现;测试替身可以不实现,不实现即没有冷却。
type storeHealth interface {
	ReportConnLost(storageID uint32, cause error)
	ReportHealthy(storageID uint32)
	CoolingDown(storageID uint32) bool
}

var _ storeHealth = (*proto_sql.StoreRegistry)(nil)

// reportStoreResult 把 handler(SQL 这一步)的结果报给注册表。只有「连接断开 / 连不上」才触发探活;
// 连接数满、只读、忙都说明服务端在应答,不动冷却。必须只用 handler 的返回值调用:复核、缓存回写的错误来自 Redis。
func (r *placementRoute) reportStoreResult(err error) {
	if r == nil || r.router == nil {
		return
	}
	health, ok := r.router.stores.(storeHealth)
	if !ok {
		return
	}
	if err == nil {
		health.ReportHealthy(r.storageID)
		return
	}
	if storeerr.KindOf(err) == storeerr.KindConnLost {
		health.ReportConnLost(r.storageID, err)
	}
}

// homeStoreCoolingDown 报告本 zone 的主库此刻是否已被确认不可达。
func (c *KeyOrderedKafkaConsumer) homeStoreCoolingDown() bool {
	if c.placement == nil || c.placement.stores == nil {
		return false
	}
	health, ok := c.placement.stores.(storeHealth)
	return ok && health.CoolingDown(c.placement.zone)
}

// logsAsStoreOutage 判定这次失败是否按「存储不可用」限速记日志:SQL 调用点打过标记的错误,或库在冷却期内。
// 落点库打开失败不在此列:它沿用原来的逐条 ERROR(player-storage-placement.md §6.1 v2.2 的日志口径)。
func logsAsStoreOutage(cause error) bool {
	return storeerr.KindOf(cause) != storeerr.KindNone || errors.Is(cause, proto_sql.ErrStoreCoolingDown)
}

// deferReason 把延后原因归到 db_task_deferred_total 的 reason 取值。
func deferReason(cause error) string {
	if kind := storeerr.KindOf(cause); kind != storeerr.KindNone {
		return kind.Reason()
	}
	switch {
	case errors.Is(cause, errPlayerRowVanished):
		return metrics.DeferFenceRace
	case errors.Is(cause, errFenceEvidenceConflict):
		return metrics.DeferFenceEvidenceConflict
	case errors.Is(cause, errFenceRedis):
		return metrics.DeferRedisUnavailable
	case errors.Is(cause, proto_sql.ErrStoreCoolingDown):
		return metrics.DeferStoreCooldown
	case errors.Is(cause, errPlacementStoreUnavailable):
		return metrics.DeferStoreOpenFailed
	case errors.Is(cause, errOrderingLockBusy):
		return metrics.DeferOrderingLockBusy
	case errors.Is(cause, errOrderingLeaseLost):
		return metrics.DeferLeaseLost
	case errors.Is(cause, errRouteNotReady):
		// 先于 errPlacementMoved:复核时的不可信两个哨兵都包着(按「路由变了」重试),原因记在副本不可信上。
		return metrics.DeferRouteNotReady
	case errors.Is(cause, errPlacementFrozen):
		return metrics.DeferPlacementFrozen
	case errors.Is(cause, errPlacementMoved):
		return metrics.DeferPlacementMoved
	case errors.Is(cause, errTaskDeferred):
		return metrics.DeferExpandLockBusy
	case errors.Is(cause, errSchemaTooNew):
		return metrics.DeferSchemaTooNew
	}
	// SQL 调用点的网络错误都已打过标记,剩下的网络错误 / 超时只可能来自 Redis(守卫、选库、复核、缓存)。
	var ne net.Error
	if errors.As(cause, &ne) || errors.Is(cause, context.DeadlineExceeded) {
		return metrics.DeferRedisUnavailable
	}
	return metrics.DeferOther
}

// taskOpLabel 把 DBTask.Op 归到指标的 op 取值。
func taskOpLabel(dbTask *db_proto.DBTask) string {
	switch dbTask.GetOp() {
	case db_proto.DBOp_DB_OP_READ:
		return "read"
	case db_proto.DBOp_DB_OP_WRITE:
		return "write"
	}
	return "unknown"
}

// storeLogEvery 是存储不可用类日志的限速间隔:一次 MySQL 重启会让每条任务每轮都失败一次,逐条打会刷出上万行,
// 把真故障淹掉。每个原因每个间隔只打一行,并带上间隔内被压掉的条数。
const storeLogEvery = 10 * time.Second

type logLimiter struct {
	mu   sync.Mutex
	last map[string]time.Time
	held map[string]int
}

var storeLogLimiter = &logLimiter{last: map[string]time.Time{}, held: map[string]int{}}

// allow 返回本次是否该打日志,以及上次打印之后被压掉的条数。
func (l *logLimiter) allow(key string, now time.Time) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if last, ok := l.last[key]; ok && now.Sub(last) < storeLogEvery {
		l.held[key]++
		return false, 0
	}
	suppressed := l.held[key]
	l.last[key] = now
	l.held[key] = 0
	return true, suppressed
}

// logStoreDeferral 限速记录一次存储不可用引起的延后。go-zero 的 logx 没有 WARN 级别;这是真实的基础设施故障,
// 用 ERROR,但每个原因每个间隔只打一行。持续多久看 db_task_deferred_total 与 db_store_cooldown_stores。
func logStoreDeferral(dbTask *db_proto.DBTask, reason string, cause error) {
	ok, suppressed := storeLogLimiter.allow(reason, time.Now())
	if !ok {
		return
	}
	logx.Errorf("task deferred, store unavailable (retry budget kept for writes): reason=%s taskID=%s op=%s suppressed_since_last=%d cause=%v",
		reason, dbTask.GetTaskId(), taskOpLabel(dbTask), suppressed, cause)
}

// retryQueueSample 是一拍开始时的重试列表长度(sampleRetryQueues),给本拍的认领额度用。
type retryQueueSample struct {
	// shared 是共享收件箱 kafka:retry:queue:{topic} 的长度;读失败为 -1(调用方回落到固定上限)。
	shared int64
	// partitioned 与 c.retryPartitionKeys 下标对齐(下标 = 原始分区号),是各 p{N} 的长度;单个键读失败为 -1。
	// 没有 p 键(直接拼结构体的单测)时为空。
	partitioned []int64
}

// sampleRetryQueues 每拍采样一次 ready / partitioned / processing / dead 的长度并更新 db_retry_queue_length:
// 一条 Pipelined 发出全部 LLEN(共享收件箱、本实例 processing、死信、每个 p 键),逐条判结果 —— 单个键报错(例如
// WRONGTYPE)只让那一项记 -1、不设对应指标,不拖垮整次采样。partitioned 指标是 p* 之和,任一 p 键读失败则本拍不设。
func (c *KeyOrderedKafkaConsumer) sampleRetryQueues() retryQueueSample {
	var sharedCmd, processingCmd, deadCmd *redis.IntCmd
	partitionCmds := make([]*redis.IntCmd, len(c.retryPartitionKeys))
	// 不看 Pipelined 的返回值:它只是第一条失败命令的错误,逐条命令的结果才是判据。
	_, _ = c.redisClient.Pipelined(c.ctx, func(p redis.Pipeliner) error {
		sharedCmd = p.LLen(c.ctx, c.retryQueueKey)
		processingCmd = p.LLen(c.ctx, c.retryProcessingKey)
		deadCmd = p.LLen(c.ctx, c.retryDeadQueueKey)
		for i, key := range c.retryPartitionKeys {
			partitionCmds[i] = p.LLen(c.ctx, key)
		}
		return nil
	})

	s := retryQueueSample{shared: sampledLength(sharedCmd)}
	if s.shared >= 0 {
		metrics.SetRetryQueueLength(metrics.RetryQueueReady, s.shared)
	}
	if n := sampledLength(processingCmd); n >= 0 {
		metrics.SetRetryQueueLength(metrics.RetryQueueProcessing, n)
	}
	if n := sampledLength(deadCmd); n >= 0 {
		metrics.SetRetryQueueLength(metrics.RetryQueueDead, n)
	}
	if len(partitionCmds) > 0 {
		s.partitioned = make([]int64, len(partitionCmds))
	}
	var partitionedTotal int64
	partitionedOK := true
	for i, cmd := range partitionCmds {
		s.partitioned[i] = sampledLength(cmd)
		if s.partitioned[i] < 0 {
			partitionedOK = false
			continue
		}
		partitionedTotal += s.partitioned[i]
	}
	if partitionedOK {
		metrics.SetRetryQueueLength(metrics.RetryQueuePartitioned, partitionedTotal)
	}
	return s
}

// sampledLength 是一条 LLEN 的结果;命令没发出去(nil)或出错时为 -1。
func sampledLength(cmd *redis.IntCmd) int64 {
	if cmd == nil {
		return -1
	}
	n, err := cmd.Result()
	if err != nil {
		return -1
	}
	return n
}

// abandonClaimedReadRetry 丢弃一条用完重试次数的读任务:读不改数据,等待它的 login 早已超时,
// 进死信只会让死信告警失去判别力。收据从本实例的 processing 列表摘掉;摘不掉就留着,之后照常回收。
// 返回是否摘掉(调用方据此计 db_retry_route_total{action=dead})。
func (c *KeyOrderedKafkaConsumer) abandonClaimedReadRetry(receipt []byte, task *db_proto.DBTask) bool {
	if err := ackRetryReceipt(c.ctx, c.redisClient, c.retryProcessingKey, receipt); err != nil {
		logx.Errorf("drop exhausted read retry failed; receipt remains recoverable: taskID=%s err=%v", task.TaskId, err)
		return false
	}
	metrics.CountReadAbandoned(metrics.ReadAbandonedBudgetExhausted)
	logx.Infof("read retry dropped after exhausting its budget (reads are never dead-lettered): taskID=%s msgType=%s retryCount=%d",
		task.TaskId, task.MsgType, task.RetryCount)
	return true
}
