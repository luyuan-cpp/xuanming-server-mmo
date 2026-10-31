// Package playersync 是「落地恢复版本摘要」在核心 Redis 上的写侧契约
// (docs/design/landing-recovery-sync-digest.md §3):键名、字段名、TTL 与 bump Lua 的 Go 侧唯一来源。
//
// 每个玩家一份 hash `player:{id}:sync_digest`:字段 g 是代次(hash 创建时刻的 Redis 服务器毫秒),
// 其余字段是「域号十进制 → 变更计数」。业务在**自己提交成功之后**给受影响的玩家 bump 对应域;
// 客户端落地后经 scene 读整份 hash,与本地缓存逐域比对,只拉变了的域。
//
// 摘要只是提示,不是数据:丢了、过期了、回档了都只会让客户端多拉一次(不变量 L-3),
// 所以 bump 失败不影响业务结果,只记一行 WARN 与低基数指标(§3.5 第 5 条)。
//
// 调用方必须遵守的时序(§3.5):
//  1. 只在提交成功之后 bump。先 bump 后提交,客户端会把旧数据记成新版本(违反 L-2);
//  2. cache-aside 的服务先失效缓存、再 bump;
//  3. bump 集合 = 拉取结果会变的所有玩家,包括操作者本人(推送排除操作者,bump 不能排除);
//  4. 给被动方(被删、被拉黑、被拒)bump 之后,客户端不得把摘要变化直接显示成红点或提示;
//  5. 跨库扇出放进与推送同一个异步上下文里做,带总预算(BumpAll 的 ctx)。
//
// C++ scene 是读侧(HGETALL),只镜像键格式:cpp/libs/services/scene/player/system/player_landing.h
// 的 kSyncDigestKeyFmt。两边逐字一致由本包的 TestKeyFormatMatchesCpp 机械钉住,不再维护手工清单。
//
// Redis 句柄必须是玩家域(player:{id}:location / player:session:{id} 所在的那个存储域,各服务的 PlayerRedis)。
// 私有实例(FriendRedis、match 集群等)C++ 读不到,写进去等于没写。适配器见 goredis.go。
package playersync

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/zeromicro/go-zero/core/logx"

	scenepb "proto/scene"
)

const (
	// KeyFormat 是摘要键的格式。C++ 侧同一个键写作 "player:{%llu}:sync_digest"(player_landing.h)。
	// 玩家域键,玩家 id 带 hash tag(docs/design/redis-player-store-cluster.md §5.2)。
	KeyFormat = "player:{%d}:sync_digest"

	// GenerationField 是代次字段名。BumpScript 只在 hash 里没有它时写入,所以 hash 被删、过期、
	// FLUSHALL 之后重建,g 必然与旧值不同(服务器时间前进),客户端据此全量拉(L-3)。
	GenerationField = "g"

	// DefaultTTL 是摘要键的存活时间,每次 bump 续期。过期即「摘要丢失」,按 L-3 全量拉一次。
	DefaultTTL = 30 * 24 * time.Hour
)

// BumpScript 是单键 bump 脚本(设计 §3.2 原文,改动须同步设计文档)。
//
//	KEYS[1] = 摘要键   ARGV[1] = TTL 秒   ARGV[2..] = 域号十进制
//
// 代次 g 只在 hash 不存在时以 Redis TIME 起种;各域计数 HINCRBY 加一;最后整键续期。
// 比较一律用「不相等」:AOF everysec 最多丢 1 秒,回档可能把旧计数连同旧 g 一起恢复,计数可能变小。
const BumpScript = `if redis.call('HEXISTS', KEYS[1], 'g') == 0 then
  local t = redis.call('TIME')
  redis.call('HSET', KEYS[1], 'g', tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000))
end
for i = 2, #ARGV do
  redis.call('HINCRBY', KEYS[1], ARGV[i], 1)
end
redis.call('EXPIRE', KEYS[1], ARGV[1])
return 1
`

// Key 返回 player:{id}:sync_digest。
func Key(playerID uint64) string {
	return fmt.Sprintf(KeyFormat, playerID)
}

// Evaler 是 bump 需要的最小 Redis 能力:执行一段 Lua,只关心是否出错(BumpScript 恒返回 1)。
// 生产实现是 NewGoRedisEvaler,单测用假实现记录调用。
type Evaler interface {
	Eval(ctx context.Context, script string, keys []string, args ...string) error
}

// 指标 result label 的全部取值。
const (
	resultOK    = "ok"
	resultError = "error"
)

// bumpTotal 是 player_sync_bump_total{service,result}:每个玩家的一次 bump 计一次(不按域拆)。
//
// label 全部低基数:service 是调用方写死的服务名(friend / team / guild),result 只有 ok / error。
// **player_id 一律不进 label**(AGENTS §9),只进 WARN 日志。
// 注册在默认 registry 上(与 guild / trade 的 assetop 指标同一个),由各服务的 promhttp.Handler() 暴露;
// 包级只建一次,单测里多次 NewBumper 不会重复注册。
var bumpTotal = promauto.With(prometheus.DefaultRegisterer).NewCounterVec(prometheus.CounterOpts{
	Name: "player_sync_bump_total",
	Help: "Landing sync digest bumps per player by service and result (ok|error). Failures only delay recovery until the next panel pull.",
}, []string{"service", "result"})

// Bumper 给玩家的版本摘要 bump 若干域。无可变状态,可被多个 goroutine 共享。
//
// nil *Bumper 的全部方法都是无操作:未装配摘要的服务、手工构造 ServiceContext 的单测不必判空。
type Bumper struct {
	eval    Evaler
	service string
	// ttlArg 是 ARGV[1](TTL 秒),构造时算好一次。
	ttlArg string
}

// NewBumper 用 eval 构造 Bumper。service 是指标 label,必须是调用方写死的服务名字面量。
// eval 为 nil 时返回 nil(bump 全部是无操作),对应「没有核心库句柄」的装配形态。
func NewBumper(eval Evaler, service string) *Bumper {
	if eval == nil {
		return nil
	}
	// 预建 0 值序列:Prometheus 里「从未发生」的序列根本不存在,
	// `increase(player_sync_bump_total{result="error"}[10m]) > 0` 在序列缺失时与「一切正常」长得一样。
	bumpTotal.WithLabelValues(service, resultOK)
	bumpTotal.WithLabelValues(service, resultError)
	return &Bumper{
		eval:    eval,
		service: service,
		ttlArg:  strconv.FormatInt(int64(DefaultTTL/time.Second), 10),
	}
}

// Bump 给一个玩家 bump 一组域(同一条 Lua,原子)。
//
// playerID 为 0 或没有合法域时不发任何命令,直接返回 nil。域按首次出现去重;非正的域号
// (UNSPECIFIED 或越界值)是调用方的编程错误,记一条错误日志后跳过 —— 写进 hash 会留下 C++ 不认的字段。
//
// 失败时已记一行 WARN 与 result="error",返回的错误只供调用方需要时使用;
// 调用方不得因它改变业务结果(§3.5 第 5 条)。
func (b *Bumper) Bump(ctx context.Context, playerID uint64, domains ...scenepb.SyncDomain) error {
	if b == nil || playerID == 0 {
		return nil
	}
	args := b.scriptArgs(ctx, domains)
	if args == nil {
		return nil
	}
	return b.bumpOne(ctx, playerID, args)
}

// BumpAll 给 playerIDs 里的每个玩家逐个 bump 同一组域(玩家去重,跳过 0)。
//
// 总超时由调用方的 ctx 给出(§3.5 第 6 条):每个玩家发命令之前先看 ctx,到期后剩余玩家不再发,
// 计入 result="error" 并合并成一行 WARN(不按人刷屏)。返回值汇总全部失败(errors.Join),
// 失败已记日志与指标,调用方通常忽略它。
func (b *Bumper) BumpAll(ctx context.Context, playerIDs []uint64, domains ...scenepb.SyncDomain) error {
	if b == nil {
		return nil
	}
	targets := uniquePlayers(playerIDs)
	if len(targets) == 0 {
		return nil
	}
	args := b.scriptArgs(ctx, domains)
	if args == nil {
		return nil
	}
	var errs []error
	for i, pid := range targets {
		if err := ctx.Err(); err != nil {
			skipped := len(targets) - i
			bumpTotal.WithLabelValues(b.service, resultError).Add(float64(skipped))
			logx.WithContext(ctx).Errorf("[playersync] WARN sync digest bump budget exhausted service=%s skipped=%d domains=%v: %v",
				b.service, skipped, args[1:], err)
			errs = append(errs, fmt.Errorf("playersync: %d players not bumped: %w", skipped, err))
			break
		}
		if err := b.bumpOne(ctx, pid, args); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// bumpOne 发一条 BumpScript 并记指标。args 是 scriptArgs 组好的 ARGV。
func (b *Bumper) bumpOne(ctx context.Context, playerID uint64, args []string) error {
	if err := b.eval.Eval(ctx, BumpScript, []string{Key(playerID)}, args...); err != nil {
		bumpTotal.WithLabelValues(b.service, resultError).Inc()
		// go-zero 的 logx 没有 Warn 级别:用 Error 级 + "WARN" 前缀,保证默认日志级别下可见(照 friend 的写法)。
		logx.WithContext(ctx).Errorf("[playersync] WARN sync digest bump failed service=%s player=%d domains=%v: %v",
			b.service, playerID, args[1:], err)
		return fmt.Errorf("playersync: bump player %d: %w", playerID, err)
	}
	bumpTotal.WithLabelValues(b.service, resultOK).Inc()
	return nil
}

// scriptArgs 组 ARGV:TTL 秒 + 去重后的域号十进制(保持首次出现的顺序)。没有合法域时返回 nil。
func (b *Bumper) scriptArgs(ctx context.Context, domains []scenepb.SyncDomain) []string {
	args := make([]string, 0, len(domains)+1)
	args = append(args, b.ttlArg)
	for i, d := range domains {
		if d <= scenepb.SyncDomain_SYNC_DOMAIN_UNSPECIFIED {
			logx.WithContext(ctx).Errorf("[playersync] invalid sync domain %d ignored service=%s (caller bug: domains must be positive SyncDomain values)",
				int32(d), b.service)
			continue
		}
		if slices.Contains(domains[:i], d) {
			continue
		}
		args = append(args, strconv.FormatInt(int64(d), 10))
	}
	if len(args) == 1 {
		return nil
	}
	return args
}

// uniquePlayers 去重并去掉 0,保持首次出现的顺序。
func uniquePlayers(ids []uint64) []uint64 {
	out := make([]uint64, 0, len(ids))
	seen := make(map[uint64]struct{}, len(ids))
	for _, id := range ids {
		if id == 0 {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// toAny 把字符串参数转成两套客户端 Eval 要的 []any。参数一律用字符串,避免数字格式在两套客户端间不一致。
func toAny(args []string) []any {
	out := make([]any, len(args))
	for i, a := range args {
		out[i] = a
	}
	return out
}
