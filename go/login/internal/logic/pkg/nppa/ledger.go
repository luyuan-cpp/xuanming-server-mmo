package nppa

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"shared/redisslot"

	"github.com/redis/go-redis/v9"
)

// Ledger 是防沉迷会话账本(docs/design/real-name-anti-addiction.md §5.4、§5.5),存在登录域 Redis 里,所有 login 副本共用。
// 进账本的是两种会话:要向国家系统上报上下线的(自有渠道实名的),和有截止时刻的(未成年人);一条会话可以两样都是。
// 它保证两件事:报过上线的会话一定会报下线;事件在发给国家系统之前不因为 login 进程退出而丢。
//
//	会话表   login_nppa_sess:{nppa}    HASH  连接身份 → "si|pi|截止毫秒"(不上报的会话 si、pi 为空;截止 0 = 不限)
//	租约     login_nppa_live:{nppa}    ZSET  连接身份 → 租约到期时刻(毫秒)
//	待发事件 login_nppa_outbox:{nppa}  ZSET  "in|ot|si|pi" / "out|ot|si|pi" → 可以被领走的时刻(毫秒)
//	清扫记录 login_nppa_sweep:{nppa}   HASH  last = 上一次清扫的时刻,hold_until = 清扫暂停到的时刻
//
// 四个键同一个槽,每个动作一段脚本,原子。时刻一律取 Redis 服务器的时钟,各副本的时钟差不影响结论。
//   - Begin:登记会话、排一条上线;同一条连接上还留着上一次的会话,先给它排下线。
//   - Renew:连接还活着(gate 的续租到了),租约顺延,并回它的截止时刻。未成年人的租约不超过当天的截止时刻。
//   - End:会话收尾(断线、离开游戏),删掉登记、排一条下线。没有登记就什么都不做,所以重放无害。
//   - Sweep:租约到期还没收尾的(gate 进程没了、断线通知丢了、End 那次 Redis 出错),按租约到期时刻补一条下线。
//   - Claim / Ack / Release:发送方领走一批到期的事件(领走的 15 秒内别人看不见),发成功了删掉,没发的放回去;
//     发了却没来得及删的,15 秒后会被再发一次(至少一次)。
//
// shortcut: 只有一个槽,所有操作落在同一个 Redis 主节点上。能发出去的量由国家系统的限流封顶(每秒约 1000 条),
// 一个节点远远够用;国家系统放宽限流到一个节点吃不消时再按连接身份分片。
type Ledger struct {
	rdb   redis.UniversalClient
	lease time.Duration
}

const (
	// claimVisibility 是领走的事件对别的副本不可见的时长:比一次上报的超时(5 秒)长,发送中的批次不会被再领一遍。
	claimVisibility = 15 * time.Second
	// sweepBatch 是一次清扫至多收尾的会话数,一拍清不完的下一拍接着清。
	sweepBatch = 200
	// sweepBackfill 是补报的下线最多往回填多久。国家系统要求事件发生时刻距上报不到 180 秒,回填 120 秒,留一分钟给发送。
	sweepBackfill = 120 * time.Second
)

// luaPrelude 是各段脚本共用的开头:Redis 服务器的时钟,以及写数字、排事件、算租约到期时刻的三个函数。
// 数字一律用 %.0f 写:毫秒时刻超过 Lua 默认的有效位数时会被写成科学计数法。
const luaPrelude = `
local t = redis.call('TIME')
local now_s = tonumber(t[1])
local now_ms = now_s * 1000 + math.floor(tonumber(t[2]) / 1000)
local function num(n) return string.format('%.0f', n) end
local function emit(outbox, kind, ot, si, pi)
  redis.call('ZADD', outbox, num(now_ms), kind .. '|' .. num(ot) .. '|' .. si .. '|' .. pi)
end
local function lease_end(lease_ms, cap_ms)
  local e = now_ms + lease_ms
  if cap_ms > 0 and cap_ms < e then e = cap_ms end
  return e
end
`

// KEYS: 会话表、租约、待发事件。ARGV: 连接身份、si、pi、租约毫秒、截止毫秒。
var beginScript = redis.NewScript(luaPrelude + `
local old = redis.call('HGET', KEYS[1], ARGV[1])
if old then
  local si, pi = string.match(old, '^([^|]*)|([^|]*)|')
  if si ~= '' then emit(KEYS[3], 'out', now_s, si, pi) end
end
redis.call('HSET', KEYS[1], ARGV[1], ARGV[2] .. '|' .. ARGV[3] .. '|' .. ARGV[5])
redis.call('ZADD', KEYS[2], num(lease_end(tonumber(ARGV[4]), tonumber(ARGV[5]))), ARGV[1])
if ARGV[2] ~= '' then emit(KEYS[3], 'in', now_s, ARGV[2], ARGV[3]) end
return 1
`)

// KEYS: 会话表、租约。ARGV: 连接身份、租约毫秒。返回截止毫秒(0 = 不限),没有登记返回 -1。
var renewScript = redis.NewScript(luaPrelude + `
local v = redis.call('HGET', KEYS[1], ARGV[1])
if not v then return '-1' end
local cap = tonumber(string.match(v, '|([^|]*)$'))
redis.call('ZADD', KEYS[2], num(lease_end(tonumber(ARGV[2]), cap)), ARGV[1])
return num(cap)
`)

// KEYS: 会话表、租约、待发事件。ARGV: 连接身份。
var endScript = redis.NewScript(luaPrelude + `
local v = redis.call('HGET', KEYS[1], ARGV[1])
if not v then return 0 end
redis.call('HDEL', KEYS[1], ARGV[1])
redis.call('ZREM', KEYS[2], ARGV[1])
local si, pi = string.match(v, '^([^|]*)|([^|]*)|')
if si ~= '' then emit(KEYS[3], 'out', now_s, si, pi) end
return 1
`)

// KEYS: 会话表、租约、待发事件、清扫记录。ARGV: 租约毫秒、一次至多几个、最多回填几秒。
//
// 清扫停过(所有 login 副本都不在)超过租约的三分之一时,先暂停一个租约那么久:那段时间 gate 的续租也送不进来,
// 活着的连接的租约可能已经到期,不能当成断了。暂停期间活着的连接都会续上租约,之后还到期的才是真的没了。
var sweepScript = redis.NewScript(luaPrelude + `
local lease = tonumber(ARGV[1])
local last = tonumber(redis.call('HGET', KEYS[4], 'last') or '0')
redis.call('HSET', KEYS[4], 'last', num(now_ms))
if last > 0 and now_ms - last > lease / 3 then
  redis.call('HSET', KEYS[4], 'hold_until', num(now_ms + lease))
end
if now_ms < tonumber(redis.call('HGET', KEYS[4], 'hold_until') or '0') then
  return 0
end
local backfill = tonumber(ARGV[3])
local due = redis.call('ZRANGEBYSCORE', KEYS[2], '-inf', num(now_ms), 'WITHSCORES', 'LIMIT', 0, tonumber(ARGV[2]))
for i = 1, #due, 2 do
  local conn = due[i]
  redis.call('ZREM', KEYS[2], conn)
  local v = redis.call('HGET', KEYS[1], conn)
  if v then
    redis.call('HDEL', KEYS[1], conn)
    local si, pi = string.match(v, '^([^|]*)|([^|]*)|')
    if si ~= '' then
      local ot = math.floor(tonumber(due[i + 1]) / 1000)
      if ot > now_s then ot = now_s end
      if ot < now_s - backfill then ot = now_s - backfill end
      emit(KEYS[3], 'out', ot, si, pi)
    end
  end
end
return #due / 2
`)

// KEYS: 待发事件。ARGV: 一次至多几条、领走后不可见的毫秒数。
var claimScript = redis.NewScript(luaPrelude + `
local due = redis.call('ZRANGEBYSCORE', KEYS[1], '-inf', num(now_ms), 'LIMIT', 0, tonumber(ARGV[1]))
for _, member in ipairs(due) do
  redis.call('ZADD', KEYS[1], num(now_ms + tonumber(ARGV[2])), member)
end
return due
`)

// NewLedger 建账本。lease 是一条连接不续租多久算断开,取登录态的寿命:gate 每三分之一寿命续一次,漏两次还在。
func NewLedger(rdb redis.UniversalClient, lease time.Duration) *Ledger {
	return &Ledger{rdb: rdb, lease: lease}
}

func (l *Ledger) sessionKeys() []string {
	return []string{redisslot.NppaSessionsKey(), redisslot.NppaLeasesKey(), redisslot.NppaOutboxKey()}
}

// Begin 登记一条会话。conn 是连接身份(gate 实例加会话代次)。要上报的会话给 si(用 NewID 生成)与 pi,并排一条上线;
// 不上报、只有截止时刻的会话两者都给空串。playDeadline 是未成年人当天的截止时刻,零值 = 不限:租约不会超过它,
// 连接没了而断线通知又丢了时,下线按它补报。
func (l *Ledger) Begin(ctx context.Context, conn, si, pi string, playDeadline time.Time) error {
	var capMs int64
	if !playDeadline.IsZero() {
		capMs = playDeadline.UnixMilli()
	}
	return beginScript.Run(ctx, l.rdb, l.sessionKeys(), conn, si, pi, l.lease.Milliseconds(), capMs).Err()
}

// Renew 给还活着的连接的会话顺延租约,返回它的截止时刻(零值 = 不限,或账本里没有这条连接)。
func (l *Ledger) Renew(ctx context.Context, conn string) (time.Time, error) {
	capMs, err := renewScript.Run(ctx, l.rdb, l.sessionKeys()[:2], conn, l.lease.Milliseconds()).Int64()
	if err != nil || capMs <= 0 {
		return time.Time{}, err
	}
	return time.UnixMilli(capMs), nil
}

// End 收尾一条会话并排一条下线。没有登记时什么都不做。
func (l *Ledger) End(ctx context.Context, conn string) error {
	return endScript.Run(ctx, l.rdb, l.sessionKeys(), conn).Err()
}

// Sweep 收尾租约到期的会话,要上报的补一条下线。返回这次收尾了几条。每个副本每拍调一次,谁调都一样。
func (l *Ledger) Sweep(ctx context.Context) (int, error) {
	keys := append(l.sessionKeys(), redisslot.NppaSweepKey())
	return sweepScript.Run(ctx, l.rdb, keys, l.lease.Milliseconds(), sweepBatch, int64(sweepBackfill/time.Second)).Int()
}

// Claimed 是领到的一条待发事件。Member 是它在账本里的原样,Ack / Release 用;Valid 为假说明原样解不出来,只能删掉。
type Claimed struct {
	Member   string
	Behavior Behavior
	Valid    bool
}

// Claim 领走至多 limit 条到期的事件。领走的 15 秒内别的副本看不见;发成功了 Ack,没发的 Release。
func (l *Ledger) Claim(ctx context.Context, limit int) ([]Claimed, error) {
	members, err := claimScript.Run(ctx, l.rdb, []string{redisslot.NppaOutboxKey()}, limit, claimVisibility.Milliseconds()).StringSlice()
	if err != nil {
		return nil, err
	}
	claimed := make([]Claimed, len(members))
	for i, member := range members {
		behavior, ok := parseEvent(member)
		claimed[i] = Claimed{Member: member, Behavior: behavior, Valid: ok}
	}
	return claimed, nil
}

// Ack 删掉已经发给国家系统(或已经过期、解不出来)的事件。
func (l *Ledger) Ack(ctx context.Context, members []string) error {
	if len(members) == 0 {
		return nil
	}
	return l.rdb.ZRem(ctx, redisslot.NppaOutboxKey(), toInterfaces(members)...).Err()
}

// Release 把领走却没发的事件放回去,马上可以再被领走。已经被 Ack 的不会被放回来(只改还在的)。
func (l *Ledger) Release(ctx context.Context, members []string) error {
	if len(members) == 0 {
		return nil
	}
	entries := make([]redis.Z, len(members))
	for i, member := range members {
		entries[i] = redis.Z{Score: 0, Member: member}
	}
	return l.rdb.ZAddXX(ctx, redisslot.NppaOutboxKey(), entries...).Err()
}

// parseEvent 解出 "in|ot|si|pi" / "out|ot|si|pi"。
func parseEvent(member string) (Behavior, bool) {
	parts := strings.Split(member, "|")
	if len(parts) != 4 || parts[2] == "" || parts[3] == "" {
		return Behavior{}, false
	}
	ot, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return Behavior{}, false
	}
	b := Behavior{SI: parts[2], OT: ot, PI: parts[3]}
	switch parts[0] {
	case "in":
		b.BT = BehaviorLogin
	case "out":
		b.BT = BehaviorLogout
	default:
		return Behavior{}, false
	}
	return b, true
}

func toInterfaces(values []string) []interface{} {
	out := make([]interface{}, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

// NewID 生成一个 32 位随机十六进制标识。实名认证的 ai 与上报的会话标识 si 都用它:规范要求两者都不超过 32 位,
// 一次提交一个 ai,一次上线到下线一个 si。
func NewID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}
