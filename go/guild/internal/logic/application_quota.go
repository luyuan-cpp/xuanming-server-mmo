package logic

import (
	"context"
	"fmt"
	"sync"
	"time"

	"guild/internal/config"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/metric"
)

// ApplicationQuotaBudget 是整个配额查询的上限，父请求截止时间更早时服从父请求。
const ApplicationQuotaBudget = 250 * time.Millisecond

// ApplicationQuotaRedisPoolSize 是配额专用客户端的连接池上限（guild.go 装配时 PoolSize 与 MaxActiveConns 都取它）。
// 配额单独用一个小池：Redis 变慢时，卡住的配额查询只占这几条连接，不挤仓储池。
// 它连控制域核心 Redis（RedisClient.Host），每个 guild 副本固定多这么多条连接。
// K8s 的 Redis 连接总账 guild 行按它记 Fixed，k8s_deploy_contract 从本行读值比对，改值要同改总账。
const ApplicationQuotaRedisPoolSize = 4

const applicationQuotaWindowMs = 60_000

// 计数与 TTL 原子写入；修复无 TTL 的遗留键，重复尝试不延长窗口。
const applicationQuotaScript = `local n = redis.call('INCR', KEYS[1])
if n == 1 or redis.call('PTTL', KEYS[1]) < 0 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
end
return n`

var applicationQuotaTotal = metric.NewCounterVec(&metric.CounterVecOpts{
	Namespace: "guild",
	Name:      "application_quota_total",
	Help:      "入帮申请尝试配额，error 表示防刷门故障放行；SQL 硬上限仍生效。",
	Labels:    []string{"outcome"},
})

// ApplicationQuota 只做防刷背压，不裁决成员关系或数量。Redis 故障放行的风险是暂时失去
// 频率保护；保留源服务政策，SQL 的权限、单归属与数量硬上限仍 fail-closed。
// 每个对象的配置与依赖在装配后只读；日志节流状态由 warnMu 保护。
type ApplicationQuota struct {
	eval     func(context.Context, string) (any, error)
	limit    int64
	observe  func(string)
	warnMu   sync.Mutex
	lastWarn time.Time
	failures uint64
}

func NewApplicationQuota(rdb redis.UniversalClient, limit uint32) (*ApplicationQuota, error) {
	if rdb == nil || limit < 1 || limit > config.MaxApplicationQuotaPerMinute {
		return nil, fmt.Errorf("申请配额要求 Redis 客户端和 [1, 600] 内的阈值")
	}
	return &ApplicationQuota{
		limit: int64(limit),
		eval: func(ctx context.Context, key string) (any, error) {
			// 不重试已执行但丢失响应的 EVAL；调用方重试仍可能多计一次，属于保守防刷。
			return rdb.Eval(ctx, applicationQuotaScript, []string{key}, applicationQuotaWindowMs).Result()
		},
		observe: func(outcome string) { applicationQuotaTotal.Inc(outcome) },
	}, nil
}

// applicationQuotaKey 的玩家号必须保持完整 uint64；只涉及单键，Redis Cluster 无跨槽操作。
func applicationQuotaKey(playerID uint64) string { return fmt.Sprintf("guild:{apply:%d}", playerID) }

// Allow 消耗一次尝试。父请求取消永远返回取消；只有配额自身的错误或超时才放行。
func (q *ApplicationQuota) Allow(ctx context.Context, playerID uint64) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if q == nil {
		return true, nil
	}
	lookup, cancel := context.WithTimeout(ctx, ApplicationQuotaBudget)
	value, err := q.eval(lookup, applicationQuotaKey(playerID))
	cancel()
	if parentErr := ctx.Err(); parentErr != nil {
		return false, parentErr
	}
	count, valid := value.(int64)
	if err != nil || !valid || count < 1 {
		q.observe("error")
		q.warn(ctx)
		return true, nil
	}
	if count > q.limit {
		q.observe("rejected")
		return false, nil
	}
	q.observe("allowed")
	return true, nil
}

func (q *ApplicationQuota) warn(ctx context.Context) {
	q.warnMu.Lock()
	defer q.warnMu.Unlock()
	q.failures++
	if !q.lastWarn.IsZero() && time.Since(q.lastWarn) < time.Minute {
		return
	}
	q.lastWarn = time.Now()
	logx.WithContext(ctx).Errorf("[guild] Application quota unavailable; allowing under fail-open policy (failures=%d)", q.failures)
	q.failures = 0
}
