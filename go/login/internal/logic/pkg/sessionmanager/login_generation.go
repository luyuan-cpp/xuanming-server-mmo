package sessionmanager

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"shared/redisslot"
)

// 登录代次(login_generation)的铸造。规格:docs/design/scene-enter-session-fence.md §3。
//
// 它是 scene 入场栅栏区分"同一角色的新旧两次登录"的依据:owner_epoch 只在持有节点换了时才变,
// session_version 断线也 +1、删会话归零,两者都分不出"同节点顶号"。
//
// 约束(调用方负责):
//   - 只在持玩家锁、读到会话记录之后、踢线与写会话之前调用;sessionFloor 就是锁内刚读到的
//     PlayerSession.LoginGeneration(没有记录为 0)。
//   - 每条走到 persistEnterGameSession 的 EnterGame 链铸一次;铸了没用上的号直接作废(G3),
//     不需要回收。
//   - 铸造失败 = 登录链失败,不得带 0 继续(0 在 scene 侧意味着"旧版无代次",会被当成另一套规则处理)。
//
// 不变量:返回值严格大于 sessionFloor,且在同一把 Redis 上严格递增;键丢失后重建也不回退
// (时间下限 + 记录下限)。所以这把键不必进清库、回档、合服清单。

// LoginGenerationTTL 是铸造键的过期时间:7 天无登录自动清理。远大于在途消息寿命
// (命令 topic 保留 1h、gate 补发预算 20s)与合理的时钟偏差;过期后重建不回退,见包注释。
const LoginGenerationTTL = 7 * 24 * time.Hour

// LoginGenerationKey 返回 player_id 的铸造键。与 player_locker 同在玩家域(login 的 PlayerRedis)。
func LoginGenerationKey(playerID uint64) string {
	return redisslot.LoginGenerationKey(playerID)
}

// mintLoginGenerationScript 与设计文档 §3.2 逐字一致,改动须同步文档。
//
// ARGV[1] = login 本机 now 的 Unix 毫秒;ARGV[2] = 锁内读到的会话记录代次(无记录为 0);ARGV[3] = TTL 秒。
// 结果 = max(键值 + 1, 当前毫秒, 记录代次 + 1),以纯十进制字符串写回并返回。值在 2^53 以内
// (毫秒时间戳约 1.8e12),Lua 的 double 表示无损;越界导致的丢位由 parseMintedLoginGeneration 兜底拒绝。
var mintLoginGenerationScript = redis.NewScript(`
local nxt = (tonumber(redis.call('GET', KEYS[1])) or 0) + 1
local nowMs = tonumber(ARGV[1])
if nowMs > nxt then nxt = nowMs end
local floor = tonumber(ARGV[2]) + 1
if floor > nxt then nxt = floor end
local s = string.format('%.0f', nxt)
redis.call('SET', KEYS[1], s, 'EX', ARGV[3])
return s
`)

// MintLoginGeneration 为 playerID 铸造一个新的登录代次。
//
// sessionFloor 是锁内读到的会话记录代次(没有记录传 0);now 由调用方给出,生产传 time.Now(),
// 测试注入固定值。now 为零值时拒绝:那会让时间下限悄悄失效,键丢失后铸出的值可能回退到很小,
// 被 scene 上残留的身份判成陈旧。
//
// 返回的错误一律视为登录链失败;成功时返回值恒非 0 且严格大于 sessionFloor。
func MintLoginGeneration(ctx context.Context, rdb redis.UniversalClient, playerID uint64, sessionFloor uint64, now time.Time) (uint64, error) {
	if rdb == nil {
		return 0, fmt.Errorf("mint login generation for player %d: redis client not wired", playerID)
	}
	if playerID == 0 {
		return 0, errors.New("mint login generation: player_id is 0")
	}
	if now.IsZero() {
		return 0, fmt.Errorf("mint login generation for player %d: now is zero, the time floor would be lost", playerID)
	}

	raw, err := mintLoginGenerationScript.Run(ctx, rdb,
		[]string{LoginGenerationKey(playerID)},
		now.UnixMilli(), sessionFloor, int64(LoginGenerationTTL/time.Second),
	).Text()
	if err != nil {
		return 0, fmt.Errorf("mint login generation for player %d: %w", playerID, err)
	}
	gen, err := parseMintedLoginGeneration(raw, sessionFloor)
	if err != nil {
		return 0, fmt.Errorf("mint login generation for player %d: %w", playerID, err)
	}
	return gen, nil
}

// parseMintedLoginGeneration 把脚本应答解析成 uint64,并复核"非 0 且严格大于记录代次"。
// 复核失败说明脚本被改坏或数值越过了 double 的精确范围;此时提交到 player_locator 也会因
// "代次不增"被拒,在这里先失败,日志更直接。
func parseMintedLoginGeneration(raw string, sessionFloor uint64) (uint64, error) {
	gen, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("unparsable script result %q: %w", raw, err)
	}
	if gen == 0 {
		return 0, errors.New("script returned zero")
	}
	if gen <= sessionFloor {
		return 0, fmt.Errorf("minted generation %d is not above session floor %d", gen, sessionFloor)
	}
	return gen, nil
}
