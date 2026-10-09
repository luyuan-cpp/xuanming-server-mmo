package data

// 终态资产指令的"清理水位"(docs/design/guild-phase2/07-rollback-fail-closed.md §7.4.2 落码修正)。
//
// 问题:回档前检查(GuildInternal.ListAppliedAssetOpsSince)按"now − TerminalRetention"判断一段历史还能不能证明。
// 这只在保留期配置从不调大时成立:把 TerminalRetentionDays 从 30 调到 60 之后,30 天前的终态行早被旧配置清掉了,
// 而新配置算出的可证明下界却是 60 天前 —— 那 30 天里的捐献既查不到、也不会报"不可证明",回档照常放行 = 复制资产(fail-open)。
//
// 做法:清理在删任何终态行**之前**,先把本轮截止时刻(next_attempt_ms < 截止的终态行会被删)以**只增**语义写进 guild
// 全局 Redis 的一个键;回档检查的可证明下界取 max(按当前配置算出的下界, 水位 + 安全余量)(internal/server/guild_internal_server.go)。
//   - 先写水位再删(write-ahead):写不进就不删,终态行多留一轮(方向安全);
//   - 读不到水位(Redis 故障、值损坏)→ 回档检查回 Unavailable,data_service 按"问不到"拒绝回档(fail-closed);
//   - 键不存在 = 从未清理过,水位为 0,只按配置算。
//
// 为什么放 Redis 而不是 MySQL:07 §7.10.4 承诺本批无库表变更;水位只是一个单调数字,没有与任何行的事务性关系。
// 残余风险(已写进 07 §7.9.3 运维手册):Redis 丢了这个键(FLUSHALL、换实例未迁移)**并且**同期调大过保留期时窗口重现,
// 所以调大 TerminalRetentionDays 之后的 (新值 − 旧值) 天内,不要清空 guild 的全局 Redis;压测前清库属于开发环境,不受影响。

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/redis/go-redis/v9"
)

// terminalCleanupWatermarkKey:值是十进制毫秒时间戳,无 TTL。全局唯一(guild 是全局服务,资产指令表只有一张)。
const terminalCleanupWatermarkKey = "guild:asset_op:terminal_cleanup_watermark_ms"

// advanceTerminalCleanupWatermarkScript 把水位推进到 ARGV[1],只增不减。
// 多副本各自跑清理、墙钟略有先后,所以必须是"比较后再写"的原子操作;毫秒时间戳远小于 2^53,Lua 的 double 比较是精确的。
// 不回传当前值:Lua 把大数转字符串会变成科学计数法。
var advanceTerminalCleanupWatermarkScript = redis.NewScript(`
local cur = tonumber(redis.call("GET", KEYS[1]) or "0") or 0
if tonumber(ARGV[1]) > cur then
  redis.call("SET", KEYS[1], ARGV[1])
  return 1
end
return 0
`)

var errNoWatermarkRedis = errors.New("guild asset store: no redis client for the terminal cleanup watermark")

// advanceTerminalCleanupWatermark 在删终态行之前调用(write-ahead)。返回 error 时调用方**不得**删终态行。
func (s *GuildAssetStore) advanceTerminalCleanupWatermark(ctx context.Context, cutoffMs uint64) error {
	if s.guilds == nil || s.guilds.rdb == nil {
		return errNoWatermarkRedis
	}
	wctx, cancel := context.WithTimeout(ctx, storeReadBudget)
	defer cancel()
	err := advanceTerminalCleanupWatermarkScript.Run(wctx, s.guilds.rdb,
		[]string{terminalCleanupWatermarkKey}, strconv.FormatUint(cutoffMs, 10)).Err()
	if err != nil {
		return fmt.Errorf("advance terminal cleanup watermark to %d: %w", cutoffMs, err)
	}
	return nil
}

// TerminalCleanupWatermarkMs 返回清理水位:next_attempt_ms 小于它的终态行**可能已被删除**。0 = 从未清理过。
// 任何读不准的情况(Redis 出错、值不是十进制整数)都返回 error —— 调用方(回档检查)据此 fail-closed,
// 不能把"读不到"当成 0。
func (s *GuildAssetStore) TerminalCleanupWatermarkMs(ctx context.Context) (uint64, error) {
	if s.guilds == nil || s.guilds.rdb == nil {
		return 0, errNoWatermarkRedis
	}
	rctx, cancel := context.WithTimeout(ctx, storeReadBudget)
	defer cancel()
	raw, err := s.guilds.rdb.Get(rctx, terminalCleanupWatermarkKey).Result()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read terminal cleanup watermark: %w", err)
	}
	ms, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("terminal cleanup watermark %q is not a decimal millisecond timestamp: %w", raw, err)
	}
	return ms, nil
}
