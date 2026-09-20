// rate_quota.go —— 好友申请的每分钟发起配额(规格 §3.6)。
//
// # 它挡的是哪一种滥用
//
// MaxPendingRequests / MaxIncomingRequests 这类上限管的是"同时挂着多少条申请",
// 而刷子的玩法是"发一条、立刻撤一条":每一条都合法、任何时刻的挂起数都是 1,
// 总量闸一条都拦不住,但被骚扰者一分钟能收到几百次弹窗,friend_request 表也被反复写。
// 这一维只能用**频率**来管,所以配额和总量闸是两件事,不能互相替代。
//
// # 为什么 fail-open(AGENTS.md §11.3 要求记录风险依据)
//
// Redis 出错时**放行**。依据:
//   - 配额是**防刷**不是**防作弊** —— 它不守护玩家资产、不守护权限,漏掉一次的代价是
//     多几条好友申请,而所有真正的上限(好友数、收件箱、重复申请)都在 MySQL 的权威事务里
//     fail-closed,配额挂掉不会让任何一条硬上限被穿透。
//   - fail-closed 的代价是不可接受的:FriendRedis 抖一下,全服**所有人都加不了好友**,
//     而这是一个纯社交功能,可用性优先。
//   - 风险与配套:Redis 长时间不可用的窗口内,刷子不受频率限制。可观测性上,
//     friend_rate_quota_total{outcome="error"} 是这个窗口的唯一信号 —— 它必须配告警,
//     否则"配额静默失效"与"没人触发配额"在指标上长得一样。
//
// ⚠ 单 key 操作,Redis Cluster 安全(键自带 hash tag)。
package logic

import (
	"context"
	"fmt"
	"strconv"

	"github.com/zeromicro/go-zero/core/logx"

	"friend/internal/metrics"
)

// requestQuotaWindowSeconds 是配额窗口。配额项名叫 RequestQuotaPerMinute,所以窗口恒为 60s,
// 不做成配置:阈值可调、窗口不可调 —— 两个都可调时"每分钟 10 次"这句话就没有含义了。
//
// 这是**固定窗口**(不是滑动窗口):窗口交界处最坏能在两秒内放过 2×阈值 次。
// 防刷够用,换滑动窗口要多存一个 ZSET,不值得(AGENTS.md §11.2 KISS)。
const requestQuotaWindowSeconds = 60

// requestQuotaScript:INCR 后首次(=1)设过期,返回当前计数。照 go/chat 的 rateLimitScript。
//
// 必须**一条 Lua**:INCR 与 EXPIRE 分两次调用时,中间进程崩溃 / 超时会留下一个永不过期的
// 计数器,那名玩家从此永久发不出好友申请,而且没有任何报错。
// 额外判 `TTL == -1` 是对存量无 TTL 计数器的自愈(例如被人手工 SET 过、或旧版本留下的)。
const requestQuotaScript = `local n = redis.call("INCR", KEYS[1])
if n == 1 or redis.call("TTL", KEYS[1]) == -1 then
	redis.call("EXPIRE", KEYS[1], ARGV[1])
end
return n`

// requestQuotaKey 好友申请配额计数器。
//
// hash tag `{rq:<pid>}` 把同一玩家的键钉在同一个 slot 上;本脚本只碰一个键,
// 所以 Cluster 下不可能 CROSSSLOT。前缀 friend: 只为运维 SCAN 时一眼认出归属。
func requestQuotaKey(playerID uint64) string {
	return fmt.Sprintf("friend:{rq:%d}", playerID)
}

// allowFriendRequest 判定这名玩家本窗口内还能不能再发好友申请。
//
// 返回 true = 放行(含 Redis 故障时的 fail-open)。调用方必须在**任何副作用之前**调它:
// 放在事务后面就等于"先写库再限流",写放大照旧。
func (l *FriendLogic) allowFriendRequest(ctx context.Context, playerID uint64) bool {
	limit := l.deps.SvcCtx.Config.Friend.RequestQuotaPerMinute
	// limit==0 在生产不可能出现(config.Validate 拒收 0 阈值),这里只是防御:
	// 真出现时按"未配置"放行,而不是按"一次都不许"把功能锁死。
	if limit == 0 {
		return true
	}
	rds := l.deps.SvcCtx.FriendRedis
	if rds == nil {
		// FriendRedis 未配时 svc 层会回落到共享库,所以正常不为 nil;
		// 为 nil 只可能是单测手工构造 ServiceContext。按 fail-open 处理并计 error。
		logx.WithContext(ctx).Errorf("[friend] WARN FriendRedis 句柄为 nil,申请频控放行 player=%d", playerID)
		metrics.ObserveRateQuota(metrics.OutcomeError)
		return true
	}

	res, err := rds.EvalCtx(ctx, requestQuotaScript,
		[]string{requestQuotaKey(playerID)}, strconv.Itoa(requestQuotaWindowSeconds))
	if err != nil {
		logx.WithContext(ctx).Errorf("[friend] 申请频控脚本失败,按 fail-open 放行 player=%d: %v", playerID, err)
		metrics.ObserveRateQuota(metrics.OutcomeError)
		return true
	}
	count, isInt := res.(int64)
	if !isInt {
		// 返回形态异常同样 fail-open:它是我们自己的脚本写错了 / go-zero 改了返回类型,
		// 属于代码缺陷而不是玩家行为,不该由玩家承担"加不了好友"的后果。
		logx.WithContext(ctx).Errorf("[friend] 申请频控脚本返回形态异常,按 fail-open 放行 player=%d: %T %v",
			playerID, res, res)
		metrics.ObserveRateQuota(metrics.OutcomeError)
		return true
	}

	// 用 > 而不是 >=:本次 INCR 已经把自己算进去了,count == limit 时这是第 limit 次,
	// 恰好用完配额、应当放行。写成 >= 会让实际配额少一次。
	if count > int64(limit) {
		logx.WithContext(ctx).Infof("[friend] 申请频控命中 player=%d count=%d limit=%d", playerID, count, limit)
		metrics.ObserveRateQuota(metrics.OutcomeRejected)
		return false
	}
	metrics.ObserveRateQuota(metrics.OutcomeAllowed)
	return true
}
