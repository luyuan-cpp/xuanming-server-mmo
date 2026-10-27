package main

// 合服工具把库里提交后的路由行发布到 mapping Redis(D1 第 2 批底座;规格 docs/handoff/launch-readiness-20261013/spec_D1_batch1b2_2_3.md
// §6.2「发布」、§13.1 第 3 条、§13.3 第 2、3 条)。
//
// 本文件镜像 go/shared/placement 的 Redis 契约(merge_zone 不 import shared,理由见 placement_codec.go 文件头):
//   - 发布脚本 PublishRouteScript(set-if-newer)、rev 的规范形式与比较、回复分类 ClassifyPublishReply;
//   - 按完整值 compare-and-delete 的 RouteReadyCompareAndDeleteScript(镜像删除复用它);
//   - 围栏镜像脚本 MergeFenceMirrorSetScript 与 MergeFenceMirrorMarginMs;
//   - 水位键 mapping:route:watermark 的值格式(只用于「库与 Redis 是否成对」的核对)。
//
// 脚本与常量由 route_mirror_test.go 用 go/parser 读 shared 源码逐字节比对;改 shared 那边必须同步改这里,反之亦然。
//
// 同一连接:go-redis 的 Pipelined 在一条连接上按序发出一批命令,所以每批的 EVAL 与末尾的 WAITAOF 在同一条连接上 ——
// WAITAOF 只等本连接此前的写(提案 §4.1-4)。不用 go-redis 的 WaitAOF 方法:它返回 *IntCmd,而 WAITAOF 回两元素数组。
// 份数不足 / 网络错误整批重发(脚本是 set-if-newer + touch,重发幂等);脚本拒绝、冲突、回复形状不对一律不重试、不伪装成功。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// ── 镜像的常量与脚本(route_mirror_test.go 与 shared 逐字节比对)─────────────

// routeMapRevKeyPrefix 是 player:maprev:{id} 的前缀(shared/placement.MapRevKeyPrefix)。
const routeMapRevKeyPrefix = "player:maprev:"

// routeWatermarkKey 是库回档检测用的水位键(shared/placement.RouteWatermarkKey)。本工具只读不写。
const routeWatermarkKey = "mapping:route:watermark"

// routeWatermarkFormatVersion = shared/placement.RouteWatermarkFormatVersion。
const routeWatermarkFormatVersion = 1

// routeGenerationMaxLen = shared/placement 的 routeReadyGenerationMaxLen(route_meta.generation 是 varchar(32))。
const routeGenerationMaxLen = 32

// routeMaxRevDigits = shared/placement 的 maxRevDigits(uint64 十进制最大位数)。
const routeMaxRevDigits = 20

// routeFenceMirrorMarginMs = shared/placement.MergeFenceMirrorMarginMs:镜像的过期时刻 = 库 expires_at_ms + 本值。
const routeFenceMirrorMarginMs = 10 * 60 * 1000

// routePublishScript = shared/placement.PublishRouteScript(逐字节)。
// KEYS: 1=player:zone:{id} 2=player:placement:{id} 3=player:maprev:{id};本工具只用三键形态(fenceZone 一律 0:
// 持围栏的工具自己写,传 home 会撞自己的围栏回 {3})。
const routePublishScript = `-- route publish (set-if-newer),docs/design/player-route-mapping-durability-proposal.md §4.2(第 1a 批定稿)
-- KEYS: 1=player:zone:{id} 2=player:placement:{id} 3=player:maprev:{id} [4=merge:in_progress:{home},可选]
-- ARGV: 1=rev(十进制,无前导零,1..20 位) 2=home(''=DEL) 3=placement(''=DEL)
-- 返回: {1, rev}                          已写入三键
--       {0, cur, curHome, curPlacement}   缓存 rev >= 本次:映射未改(maprev 原值写回一次,见下)
--       {2, '', curHome, curPlacement}    缓存无 maprev 且遗留值与本次不同:零写入
--       {3}                               KEYS[4] 存在(合服围栏):零写入
local function valid_rev(s)
  return type(s) == 'string' and #s >= 1 and #s <= 20 and string.match(s, '^[1-9][0-9]*$') ~= nil
end
-- 两边都已过 valid_rev:先比长度再逐字节比。不经 Lua number(double),也不依赖 strcoll / locale。
local function rev_ge(a, b)
  if #a ~= #b then return #a > #b end
  for i = 1, #a do
    local x, y = string.byte(a, i), string.byte(b, i)
    if x ~= y then return x > y end
  end
  return true
end
local rev = ARGV[1]
if not valid_rev(rev) then
  return redis.error_reply('ERR route publish: bad rev argument')
end
-- 提交点复核合服围栏(与 router.go setPlayerZoneIfAbsentScript 的 EXISTS KEYS[2] 同一判据:键存在即封锁)
if KEYS[4] and redis.call('EXISTS', KEYS[4]) == 1 then
  return {3}
end
local cur = redis.call('GET', KEYS[3])
local curHome = redis.call('GET', KEYS[1])
local curPlc = redis.call('GET', KEYS[2])
if cur then
  if not valid_rev(cur) then
    return redis.error_reply('ERR route publish: malformed maprev')
  end
  if rev_ge(cur, rev) then
    -- 同值写回:让本连接随后的 WAITAOF 覆盖「此刻已在缓存里的状态」,
    -- 否则幂等重发(上次的 WAITAOF 超时)会在没确认落盘的情况下回成功。
    redis.call('SET', KEYS[3], cur)
    return {0, cur, curHome or '', curPlc or ''}
  end
else
  -- 无 maprev:这两把键要么从没写过,要么是 P1 导入之前、或绕过本脚本的写者(第 1a 批的 merge_zone)
  -- 留下的遗留值。遗留值存在且与本次不同时一律不覆盖 —— 静默改回库里的旧值就是写错库。
  if (curHome and curHome ~= ARGV[2]) or (curPlc and curPlc ~= ARGV[3]) then
    return {2, '', curHome or '', curPlc or ''}
  end
end
if ARGV[2] == '' then redis.call('DEL', KEYS[1]) else redis.call('SET', KEYS[1], ARGV[2]) end
if ARGV[3] == '' then redis.call('DEL', KEYS[2]) else redis.call('SET', KEYS[2], ARGV[3]) end
-- maprev 最后写,表示「三键已到这一版」。Redis 7 对没有 shebang 的脚本只在第一条写命令前做一次 OOM 检查,
-- 之后不会中途拒写,所以不存在 OOM 半写;顺序只为可读。
redis.call('SET', KEYS[3], rev)
return {1, rev}
`

// routeCompareAndDeleteScript = shared/placement.RouteReadyCompareAndDeleteScript(逐字节):KEYS[1] 当前值等于 ARGV[1] 才 DEL。
// 本工具拿它按完整值删围栏镜像与合走标记镜像(脚本与键名无关)。
const routeCompareAndDeleteScript = `-- route ready marker compare-and-delete,docs/design/player-route-mapping-durability-proposal.md §11.2(第 1b 批)
if redis.call('GET', KEYS[1]) == ARGV[1] then
  redis.call('DEL', KEYS[1])
  return 1
end
return 0
`

// routeFenceMirrorSetScript = shared/placement.MergeFenceMirrorSetScript(逐字节):按 fence_epoch set-if-newer 写围栏镜像。
const routeFenceMirrorSetScript = `-- merge fence mirror set-if-newer,spec_D1_batch1b2_2_3.md §13.1
local cur = redis.call('GET', KEYS[1])
if cur then
  local ok, v = pcall(cjson.decode, cur)
  if not ok or type(v) ~= 'table' or type(v.fence_epoch) ~= 'number' then return 2 end
  if v.fence_epoch > tonumber(ARGV[2]) then return 0 end
end
redis.call('SET', KEYS[1], ARGV[1])
redis.call('PEXPIREAT', KEYS[1], ARGV[3])
return 1
`

// ── 发布参数 ──────────────────────────────────────────────────────────────────

const (
	// routePublishBatch 是一条 pipeline 里最多的发布行数(与 W5 每批 500 人对齐)。
	routePublishBatch = 500
	// routePublishAttempts 是一批的总尝试次数:首发 + 份数不足 / 网络错误时整批重发最多 3 次(规格 §6.2)。
	routePublishAttempts = 4
	// routePublishRetryBackoffMin / Max 是整批重发之前的抖动等待(可被 ctx 取消)。
	routePublishRetryBackoffMin = 10 * time.Millisecond
	routePublishRetryBackoffMax = 50 * time.Millisecond
	// routeWaitAOFMargin:WAITAOF 的等待上限 = min(配置值, ctx 剩余 − 本值),给回包留的余量。
	routeWaitAOFMargin = 100 * time.Millisecond
	// routeWaitAOFTimeoutMaxMs 是 -waitaof-timeout-ms 的上限:本工具的 go-redis 客户端用缺省 ReadTimeout(3s),
	// 一条 pipeline 的读超时覆盖整批回复;WAITAOF 等得比它久,客户端先超时,结论就成了「网络错误」。
	routeWaitAOFTimeoutMaxMs = 2000
	// routeCatchUpReadBatch 是 catch-up 每次 MGET 覆盖的玩家数(每人 3 把键)。
	routeCatchUpReadBatch = 300
	// routePublishFailureSample 是错误文案里最多列出的失败行数。
	routePublishFailureSample = 10
)

// 发布层的错误。
var (
	// errRoutePublishRejected 发布被确定性地拒绝(脚本错误回复、冲突、遗留值、围栏、回复形状不对):不重试,
	// 调用方按「这一批未完成」失败,退出非 0;库里已提交的行由重跑的幂等分支或 leader 增量推送补齐。
	errRoutePublishRejected = errors.New("route publish rejected")
	// errRoutePublishNotDurable WAITAOF 在全部尝试里都没确认到要求的落盘份数(或预算已不够发一次 WAITAOF)。
	errRoutePublishNotDurable = errors.New("route publish not confirmed durable by WAITAOF")
	// errUnexpectedRoutePublishReply 发布脚本的回复不是它能产生的四种形状之一(shared ErrUnexpectedPublishReply 的镜像)。
	errUnexpectedRoutePublishReply = errors.New("unexpected route publish reply")
	// errUnexpectedRouteWaitAOFReply WAITAOF 的回复不是两个非负整数:确定性错误,不重试。
	errUnexpectedRouteWaitAOFReply = errors.New("unexpected WAITAOF reply")
	// errUnexpectedRouteScriptReply 围栏镜像 / compare-and-delete 脚本的回复形状不对。
	errUnexpectedRouteScriptReply = errors.New("unexpected route script reply")
	// errRouteFenceMirrorStale 围栏镜像里已是更大的 fence_epoch(另一个持有者接手了):零写入,本次运行必须当作失权。
	errRouteFenceMirrorStale = errors.New("merge fence mirror holds a newer fence_epoch")
	// errRouteFenceMirrorForeign 围栏镜像的值解不出数字 fence_epoch(旧版 merge_zone 写的或手工写的):零写入,不覆盖。
	errRouteFenceMirrorForeign = errors.New("merge fence mirror holds a value without a numeric fence_epoch")
	// errRouteWatermarkMalformed 水位键的值不合规(镜像 shared 的 ErrMalformed 口径)。
	errRouteWatermarkMalformed = errors.New("malformed route watermark")
	// errRouteWatermarkNewerVersion 水位键是更新格式版本写的(shared ErrRouteWatermarkNewerVersion 的镜像):不信任。
	errRouteWatermarkNewerVersion = errors.New("route watermark has a newer format version")
	// errRoutePairing 全局库与 mapping Redis 不成对(规格 §13.3 第 3 条),拒绝开写。
	errRoutePairing = errors.New("route DB and mapping Redis are not paired")
)

// routeDurability 是 WAITAOF 的份数与等待(-waitaof-local / -waitaof-replicas / -waitaof-timeout-ms)。
type routeDurability struct {
	NumLocal    int
	NumReplicas int
	Timeout     time.Duration
}

// newRouteDurability 校验 WAITAOF 参数:numlocal 只能是 0 或 1;numreplicas ≥ 0;两者不能都是 0(那等于不确认落盘,
// 本工具不允许);timeout 在 [1, routeWaitAOFTimeoutMaxMs] 毫秒(0 = WAITAOF 无限阻塞,绝不发)。
func newRouteDurability(numLocal, numReplicas, timeoutMs int) (routeDurability, error) {
	switch {
	case numLocal < 0 || numLocal > 1:
		return routeDurability{}, fmt.Errorf("-waitaof-local %d must be 0 or 1", numLocal)
	case numReplicas < 0:
		return routeDurability{}, fmt.Errorf("-waitaof-replicas %d must be >= 0", numReplicas)
	case numLocal == 0 && numReplicas == 0:
		return routeDurability{}, errors.New("-waitaof-local and -waitaof-replicas are both 0: every route publish must be confirmed by WAITAOF")
	case timeoutMs < 1 || timeoutMs > routeWaitAOFTimeoutMaxMs:
		return routeDurability{}, fmt.Errorf("-waitaof-timeout-ms %d must be in [1, %d] (0 would block forever; above the limit the "+
			"client read timeout fires first)", timeoutMs, routeWaitAOFTimeoutMaxMs)
	}
	return routeDurability{NumLocal: numLocal, NumReplicas: numReplicas, Timeout: time.Duration(timeoutMs) * time.Millisecond}, nil
}

// ── rev ───────────────────────────────────────────────────────────────────────

// mapRevKey 返回 player:maprev:{id}(shared/placement.MapRevKey)。
func mapRevKey(playerID uint64) string {
	return routeMapRevKeyPrefix + strconv.FormatUint(playerID, 10)
}

// formatRouteRev 把库里的 rev 格式化成发布脚本要的十进制串;0 不是合法版本,回空串(shared FormatRev)。
func formatRouteRev(rev uint64) string {
	if rev == 0 {
		return ""
	}
	return strconv.FormatUint(rev, 10)
}

// validRouteRev 判断 s 是否是规范 rev:^[1-9][0-9]{0,19}$ 且不超过 MaxUint64(shared ValidRev)。
func validRouteRev(s string) bool {
	if len(s) < 1 || len(s) > routeMaxRevDigits || s[0] < '1' || s[0] > '9' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	_, err := strconv.ParseUint(s, 10, 64)
	return err == nil
}

// compareRouteRev 先比长度、再逐字节比(与发布脚本的 rev_ge 同一判据;shared CompareRev)。任一方不规范回错误。
func compareRouteRev(a, b string) (int, error) {
	if !validRouteRev(a) || !validRouteRev(b) {
		return 0, fmt.Errorf("%w: rev %q vs %q", errPlacementMalformed, a, b)
	}
	switch {
	case len(a) < len(b):
		return -1, nil
	case len(a) > len(b):
		return 1, nil
	case a < b:
		return -1, nil
	case a > b:
		return 1, nil
	}
	return 0, nil
}

// ── 发布脚本的回复 ────────────────────────────────────────────────────────────

// routePublishOutcome 是一次发布脚本调用的结局(shared PublishOutcome 的镜像)。
type routePublishOutcome uint8

const (
	// routePublishApplied {1, rev}:三键已写成本次的值。
	routePublishApplied routePublishOutcome = iota + 1
	// routePublishSuperseded {0, cur, h, p} 且 cur > rev:缓存已是更新的版本。值与本次相同才算通过。
	routePublishSuperseded
	// routePublishConfirmed {0, cur, h, p} 且 cur == rev、值相同:幂等重发,已确认。
	routePublishConfirmed
	// routePublishConflict {0, cur, h, p} 且 cur == rev、值不同:有绕过脚本的写者,或库回档后 rev 被复用。
	routePublishConflict
	// routePublishLegacyConflict {2, '', h, p}:缓存没有 maprev,遗留值与本次不同。
	routePublishLegacyConflict
	// routePublishFenced {3}:KEYS[4] 围栏存在(本工具不传第 4 把键,出现即契约坏了)。
	routePublishFenced
)

// String 只供日志(低基数,与 data_service_route_publish_total{result} 的取值同名)。
func (o routePublishOutcome) String() string {
	switch o {
	case routePublishApplied:
		return "applied"
	case routePublishSuperseded:
		return "superseded"
	case routePublishConfirmed:
		return "confirmed"
	case routePublishConflict:
		return "conflict"
	case routePublishLegacyConflict:
		return "legacy_conflict"
	case routePublishFenced:
		return "fenced"
	}
	return "unknown"
}

// routePublishReply 是分类后的脚本回复(CurRev / CurHome / CurPlacement 是脚本执行那一刻缓存里的值)。
type routePublishReply struct {
	Outcome      routePublishOutcome
	CurRev       string
	CurHome      string
	CurPlacement string
}

// classifyRoutePublishReply 是 shared ClassifyPublishReply 的镜像(同一组测试向量钉住,route_publish_test.go)。
// 只接受脚本能产生的四种形状;其余一律 errUnexpectedRoutePublishReply(fail-closed)。
func classifyRoutePublishReply(raw any, rev, home, plc string) (routePublishReply, error) {
	arr, ok := raw.([]any)
	if !ok || len(arr) == 0 {
		return routePublishReply{}, fmt.Errorf("%w: %T %v", errUnexpectedRoutePublishReply, raw, raw)
	}
	code, ok := arr[0].(int64)
	if !ok {
		return routePublishReply{}, fmt.Errorf("%w: first element %T %v is not an integer", errUnexpectedRoutePublishReply, arr[0], arr[0])
	}
	switch code {
	case 1:
		if len(arr) != 2 {
			return routePublishReply{}, fmt.Errorf("%w: applied reply has %d elements, want 2", errUnexpectedRoutePublishReply, len(arr))
		}
		got, ok := arr[1].(string)
		if !ok || got != rev {
			return routePublishReply{}, fmt.Errorf("%w: applied reply carries rev %v, sent %q", errUnexpectedRoutePublishReply, arr[1], rev)
		}
		return routePublishReply{Outcome: routePublishApplied, CurRev: rev, CurHome: home, CurPlacement: plc}, nil
	case 0:
		cur, h, p, err := routePublishReplyTail(arr, "unchanged")
		if err != nil {
			return routePublishReply{}, err
		}
		cmp, err := compareRouteRev(cur, rev)
		if err != nil {
			return routePublishReply{}, fmt.Errorf("%w: unchanged reply: %w", errUnexpectedRoutePublishReply, err)
		}
		reply := routePublishReply{CurRev: cur, CurHome: h, CurPlacement: p}
		switch {
		case cmp < 0:
			return routePublishReply{}, fmt.Errorf("%w: unchanged reply with cached rev %s < sent rev %s", errUnexpectedRoutePublishReply, cur, rev)
		case cmp > 0:
			reply.Outcome = routePublishSuperseded
		case h == home && p == plc:
			reply.Outcome = routePublishConfirmed
		default:
			reply.Outcome = routePublishConflict
		}
		return reply, nil
	case 2:
		first, h, p, err := routePublishReplyTail(arr, "legacy")
		if err != nil {
			return routePublishReply{}, err
		}
		if first != "" {
			return routePublishReply{}, fmt.Errorf("%w: legacy reply carries rev %q, want empty", errUnexpectedRoutePublishReply, first)
		}
		if h == home && p == plc {
			return routePublishReply{}, fmt.Errorf("%w: legacy reply with values identical to the sent ones", errUnexpectedRoutePublishReply)
		}
		return routePublishReply{Outcome: routePublishLegacyConflict, CurHome: h, CurPlacement: p}, nil
	case 3:
		if len(arr) != 1 {
			return routePublishReply{}, fmt.Errorf("%w: fenced reply has %d elements, want 1", errUnexpectedRoutePublishReply, len(arr))
		}
		return routePublishReply{Outcome: routePublishFenced}, nil
	}
	return routePublishReply{}, fmt.Errorf("%w: unknown reply code %d", errUnexpectedRoutePublishReply, code)
}

func routePublishReplyTail(arr []any, kind string) (string, string, string, error) {
	if len(arr) != 4 {
		return "", "", "", fmt.Errorf("%w: %s reply has %d elements, want 4", errUnexpectedRoutePublishReply, kind, len(arr))
	}
	var out [3]string
	for i := range out {
		s, ok := arr[i+1].(string)
		if !ok {
			return "", "", "", fmt.Errorf("%w: %s reply element %d is %T, want string", errUnexpectedRoutePublishReply, kind, i+1, arr[i+1])
		}
		out[i] = s
	}
	return out[0], out[1], out[2], nil
}

// routePublishAccepted 是本工具对一行发布结果的判据(规格 §6.2):Applied / Confirmed 通过;Superseded 且缓存值等于本次值通过
// (缓存已是更新的一版、内容相同);其余(Superseded 而值不同、Conflict、LegacyConflict、Fenced)都是失败。
func routePublishAccepted(reply routePublishReply, home, plc string) bool {
	switch reply.Outcome {
	case routePublishApplied, routePublishConfirmed:
		return true
	case routePublishSuperseded:
		return reply.CurHome == home && reply.CurPlacement == plc
	}
	return false
}

// ── WAITAOF ───────────────────────────────────────────────────────────────────

// checkRouteWaitAOFReply 判断 WAITAOF 的回复 [numlocal, numreplicas] 是否满足要求的份数(data_service checkWaitAOFReply 的镜像)。
// 份数不足回普通错误(调用方整批重发);形状不对回 errUnexpectedRouteWaitAOFReply(不重试)。
func checkRouteWaitAOFReply(vals []any, numLocal, numReplicas int) error {
	if len(vals) != 2 {
		return fmt.Errorf("%w: %d elements %v, want [numlocal, numreplicas]", errUnexpectedRouteWaitAOFReply, len(vals), vals)
	}
	local, localOK := vals[0].(int64)
	replicas, replicasOK := vals[1].(int64)
	if !localOK || !replicasOK || local < 0 || replicas < 0 {
		return fmt.Errorf("%w: %v", errUnexpectedRouteWaitAOFReply, vals)
	}
	if local < int64(numLocal) || replicas < int64(numReplicas) {
		return fmt.Errorf("%w: WAITAOF acked numlocal=%d numreplicas=%d, want at least %d and %d",
			errRoutePublishNotDurable, local, replicas, numLocal, numReplicas)
	}
	return nil
}

// routeWaitAOFBudgetMs 算本次 WAITAOF 的 timeout(毫秒):min(configured, 截止前剩余 − margin),先截断成整毫秒再判断;
// 小于 1 时 ok=false,调用方不得发送(WAITAOF 的 timeout 为 0 表示无限阻塞)。纯函数,时间由参数注入。
func routeWaitAOFBudgetMs(now, deadline time.Time, hasDeadline bool, configured time.Duration) (int64, bool) {
	budget := configured
	if hasDeadline {
		if remaining := deadline.Sub(now) - routeWaitAOFMargin; remaining < budget {
			budget = remaining
		}
	}
	ms := budget.Milliseconds()
	if ms < 1 {
		return 0, false
	}
	return ms, true
}

// isRouteRedisErrorReply 判断 err 是不是 Redis 回的错误回复(确定性错误)。网络错误、ctx 错误都不是。
func isRouteRedisErrorReply(err error) bool {
	var re redis.Error
	return errors.As(err, &re) && !errors.Is(err, redis.Nil)
}

// ── 发布器 ────────────────────────────────────────────────────────────────────

// routePublisher 在 mapping Redis(DB 0)上发布路由行、写 / 删镜像、读水位键。时钟、睡眠、抖动是单测接缝。
type routePublisher struct {
	rdb *redis.Client
	dur routeDurability
	// now / sleep / backoff:生产 time.Now / sleepCtx / 均匀抖动;单测注入,不依赖墙钟、不真睡。
	now     func() time.Time
	sleep   func(ctx context.Context, d time.Duration) error
	backoff func() time.Duration
}

// newRoutePublisher 装配生产用的发布器。
func newRoutePublisher(rdb *redis.Client, dur routeDurability) *routePublisher {
	return &routePublisher{rdb: rdb, dur: dur, now: time.Now, sleep: sleepCtx, backoff: routePublishRetryBackoff}
}

// routePublishRetryBackoff 在 [Min, Max] 内均匀取一个等待时长。
func routePublishRetryBackoff() time.Duration {
	return routePublishRetryBackoffMin + rand.N(routePublishRetryBackoffMax-routePublishRetryBackoffMin+1)
}

// durableWrite 是「一批写 + 同一连接末尾的 WAITAOF」的公共循环:
//   - write 往 pipeline 里排命令,返回 check;Exec 之后 check 判这批写的结果:(false, nil) 通过;(true, err) 可重试
//     (网络错误);(false, err) 确定性失败,立刻返回;
//   - 写都通过后判 WAITAOF:Redis 错误回复(appendonly 关着、版本不支持)与形状不对是确定性的;网络错误与份数不足整批重发;
//   - 总尝试次数 routePublishAttempts,每次之前按 ctx 剩余时间重算 WAITAOF 的 timeout,预算不够就不发。
//
// 每次 Pipelined 只取一条连接,EVAL / SET 与 WAITAOF 在同一条连接上(go-redis 自身的 MaxRetries 也是整条 pipeline 换连接重发)。
func (p *routePublisher) durableWrite(ctx context.Context, what string, write func(pipe redis.Pipeliner) func() (bool, error)) error {
	if err := checkMaintenanceTool(ctx); err != nil {
		return err
	}
	deadline, hasDeadline := ctx.Deadline()
	var lastErr error
	for attempt := 1; attempt <= routePublishAttempts; attempt++ {
		if attempt > 1 {
			if werr := p.sleep(ctx, p.backoff()); werr != nil {
				return fmt.Errorf("%s: %w", what, errors.Join(lastErr, werr))
			}
		}
		waitMs, ok := routeWaitAOFBudgetMs(p.now(), deadline, hasDeadline, p.dur.Timeout)
		if !ok {
			// errors.Join 跳过 nil:第一次尝试之前就没预算时 lastErr 与 ctx.Err() 可能都是 nil。
			return errors.Join(fmt.Errorf("%s: %w: deadline too close to send WAITAOF (attempt %d/%d)",
				what, errRoutePublishNotDurable, attempt, routePublishAttempts), lastErr, ctx.Err())
		}
		var check func() (bool, error)
		var wait *redis.Cmd
		// pipeline 的返回值只是第一条失败命令的错误;逐条命令的结果才是判据。
		_, pipelineErr := p.rdb.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			check = write(pipe)
			wait = pipe.Do(ctx, "WAITAOF", p.dur.NumLocal, p.dur.NumReplicas, waitMs)
			return nil
		})
		if pipelineErr != nil {
			holdMaintenanceTool(ctx)
		}
		if check == nil || wait == nil {
			holdMaintenanceTool(ctx)
			return errors.Join(fmt.Errorf("%s: pipeline did not construct its write/WAITAOF result", what), pipelineErr)
		}
		retry, err := check()
		if err != nil {
			holdMaintenanceTool(ctx)
			if !retry {
				return fmt.Errorf("%s: %w", what, err)
			}
			lastErr = err
			continue
		}
		acks, werr := wait.Slice()
		if werr != nil {
			holdMaintenanceTool(ctx)
			if isRouteRedisErrorReply(werr) {
				return fmt.Errorf("%s: WAITAOF %d %d %d rejected (needs appendonly yes and Redis >= 7.2; not retried): %w",
					what, p.dur.NumLocal, p.dur.NumReplicas, waitMs, werr)
			}
			lastErr = fmt.Errorf("WAITAOF: %w", werr)
			continue
		}
		if aerr := checkRouteWaitAOFReply(acks, p.dur.NumLocal, p.dur.NumReplicas); aerr != nil {
			holdMaintenanceTool(ctx)
			if errors.Is(aerr, errUnexpectedRouteWaitAOFReply) {
				return fmt.Errorf("%s: %w", what, aerr)
			}
			lastErr = aerr
			continue
		}
		if pipelineErr != nil {
			return fmt.Errorf("%s: %w", what, pipelineErr)
		}
		return nil
	}
	return fmt.Errorf("%s: gave up after %d attempts: %w", what, routePublishAttempts, lastErr)
}

// ── 发布路由行 ────────────────────────────────────────────────────────────────

// routePublishArgs 是一行的发布参数(KEYS 顺序是脚本契约:player:zone、player:placement、player:maprev)。
type routePublishArgs struct {
	playerID uint64
	keys     []string
	rev      string
	home     string
	plc      string
}

// routePublishArgsOf 校验一行并组装发布参数。不发布垃圾:player_id 为 0、rev 不规范、落点三列不自洽、home 与落点都没有
// (库里的「真没有记录」;本工具不定义删号,绝不按它 DEL 缓存)一律报错。
func routePublishArgsOf(row mergeRouteRow) (routePublishArgs, error) {
	if row.PlayerID == 0 {
		return routePublishArgs{}, fmt.Errorf("%w: player_id 0", errRouteInvalidArgument)
	}
	rev := formatRouteRev(row.Rev)
	if !validRouteRev(rev) {
		return routePublishArgs{}, fmt.Errorf("%w: player %d rev %d", errRouteInvalidArgument, row.PlayerID, row.Rev)
	}
	plc, err := row.placementValue()
	if err != nil {
		return routePublishArgs{}, err
	}
	home := row.homeValue()
	if home == "" && plc == "" {
		return routePublishArgs{}, fmt.Errorf("%w: player %d has neither home_zone nor placement; refusing to publish a tombstone",
			errRouteInvalidArgument, row.PlayerID)
	}
	keys := []string{playerZoneKey(row.PlayerID), placementKey(row.PlayerID), mapRevKey(row.PlayerID)}
	return routePublishArgs{playerID: row.PlayerID, keys: keys, rev: rev, home: home, plc: plc}, nil
}

// routePublishReport 是一次发布的计数(全部通过时才有意义)。
type routePublishReport struct {
	Rows       int
	Applied    int
	Confirmed  int
	Superseded int
}

func (r *routePublishReport) add(o routePublishReport) {
	r.Rows += o.Rows
	r.Applied += o.Applied
	r.Confirmed += o.Confirmed
	r.Superseded += o.Superseded
}

func (r routePublishReport) String() string {
	return fmt.Sprintf("rows=%d applied=%d confirmed=%d superseded_same_value=%d", r.Rows, r.Applied, r.Confirmed, r.Superseded)
}

// publishRoutes 把一批已提交的行发布到 mapping Redis(规格 §6.2):每 routePublishBatch 行一条 pipeline(单连接),
// 每行一条 EVAL 三键形态,末尾一条 WAITAOF;逐行按 routePublishAccepted 判。任一行失败 = 这一批「未完成」:返回错误,
// 调用方不标记完成、退出非 0(不伪装成功)。重复的 player_id 拒绝。
func (p *routePublisher) publishRoutes(ctx context.Context, rows []mergeRouteRow) (routePublishReport, error) {
	var rep routePublishReport
	args := make([]routePublishArgs, 0, len(rows))
	seen := make(map[uint64]bool, len(rows))
	for _, row := range rows {
		a, err := routePublishArgsOf(row)
		if err != nil {
			return rep, err
		}
		if seen[row.PlayerID] {
			return rep, fmt.Errorf("%w: player %d published twice in one call", errRouteInvalidArgument, row.PlayerID)
		}
		seen[row.PlayerID] = true
		args = append(args, a)
	}
	for start := 0; start < len(args); start += routePublishBatch {
		chunk := args[start:min(start+routePublishBatch, len(args))]
		crep, err := p.publishChunk(ctx, chunk)
		if err != nil {
			return rep, err
		}
		rep.add(crep)
	}
	return rep, nil
}

func (p *routePublisher) publishChunk(ctx context.Context, chunk []routePublishArgs) (routePublishReport, error) {
	var rep routePublishReport
	err := p.durableWrite(ctx, fmt.Sprintf("route publish of %d rows", len(chunk)), func(pipe redis.Pipeliner) func() (bool, error) {
		cmds := make([]*redis.Cmd, len(chunk))
		for i, a := range chunk {
			cmds[i] = pipe.Eval(ctx, routePublishScript, a.keys, a.rev, a.home, a.plc)
		}
		return func() (bool, error) {
			rep = routePublishReport{Rows: len(chunk)}
			var rejected []string
			var transient error
			for i, cmd := range cmds {
				a := chunk[i]
				if err := cmd.Err(); err != nil {
					if isRouteRedisErrorReply(err) {
						rejected = append(rejected, fmt.Sprintf("player %d rev=%s: script error: %v", a.playerID, a.rev, err))
					} else if transient == nil {
						transient = fmt.Errorf("player %d: %w", a.playerID, err)
					}
					continue
				}
				reply, err := classifyRoutePublishReply(cmd.Val(), a.rev, a.home, a.plc)
				if err != nil {
					rejected = append(rejected, fmt.Sprintf("player %d rev=%s: %v", a.playerID, a.rev, err))
					continue
				}
				if !routePublishAccepted(reply, a.home, a.plc) {
					rejected = append(rejected, fmt.Sprintf("player %d %s: cached rev=%q home=%q placement=%q, publishing rev=%s home=%q placement=%q",
						a.playerID, reply.Outcome, reply.CurRev, reply.CurHome, reply.CurPlacement, a.rev, a.home, a.plc))
					continue
				}
				switch reply.Outcome {
				case routePublishApplied:
					rep.Applied++
				case routePublishConfirmed:
					rep.Confirmed++
				default:
					rep.Superseded++
				}
			}
			if len(rejected) > 0 {
				return false, fmt.Errorf("%w: %d of %d rows (zero writes for those rows): %s", errRoutePublishRejected,
					len(rejected), len(chunk), strings.Join(rejected[:min(len(rejected), routePublishFailureSample)], "; "))
			}
			if transient != nil {
				return true, transient
			}
			return false, nil
		}
	})
	if err != nil {
		return routePublishReport{}, err
	}
	return rep, nil
}

// ── catch-up:库 rev 领先缓存的行先发布确认(规格 §13.3 第 2 条)──────────────

// routeCatchUpReport 是一次 catch-up 的计数。
type routeCatchUpReport struct {
	// Current 是缓存已是这一版(或更新)且值相同、不必再发的行数。
	Current   int
	Published routePublishReport
}

// decideRouteCatchUp 判一行要不要先发布(纯函数):缓存没有 maprev、缓存 rev 落后于库、或值与库不同 → 要发(值不同时由脚本给出
// 冲突结论,发布失败);缓存 rev ≥ 库 rev 且值相同 → 不必发。缓存 maprev 不规范 → 错误(fail-closed)。
// cacheHome / cachePlc / cacheRev 是 MGET 的元素(nil = 键不存在)。
func decideRouteCatchUp(row mergeRouteRow, cacheHome, cachePlc, cacheRev any) (bool, error) {
	rev := formatRouteRev(row.Rev)
	if !validRouteRev(rev) {
		return false, fmt.Errorf("%w: player %d rev %d", errRouteInvalidArgument, row.PlayerID, row.Rev)
	}
	plc, err := row.placementValue()
	if err != nil {
		return false, err
	}
	vals := [3]string{}
	present := [3]bool{}
	for i, v := range []any{cacheHome, cachePlc, cacheRev} {
		switch s := v.(type) {
		case nil:
		case string:
			vals[i], present[i] = s, true
		default:
			return false, fmt.Errorf("%w: MGET element %d is %T", errUnexpectedRouteScriptReply, i, v)
		}
	}
	if !present[2] {
		return true, nil
	}
	cmp, err := compareRouteRev(vals[2], rev)
	if err != nil {
		return false, fmt.Errorf("player %d: malformed %s: %w", row.PlayerID, mapRevKey(row.PlayerID), err)
	}
	if cmp < 0 || vals[0] != row.homeValue() || vals[1] != plc {
		return true, nil
	}
	return false, nil
}

// catchUpRoutes 先核对缓存:库 rev 领先缓存 maprev(或缓存没有 maprev、或值不同)的行按库值发布并 WAITAOF 确认;
// 缓存已是这一版且值相同的行不发。任何一行确认不了就返回错误,依赖步骤不推进(规格 §13.3 第 2 条)。
// 「不必发」的行是别人(本工具上一次运行、leader)发布过的,它们的落盘确认由当时的发布者负责。
func (p *routePublisher) catchUpRoutes(ctx context.Context, rows []mergeRouteRow) (routeCatchUpReport, error) {
	var rep routeCatchUpReport
	var lagging []mergeRouteRow
	for start := 0; start < len(rows); start += routeCatchUpReadBatch {
		chunk := rows[start:min(start+routeCatchUpReadBatch, len(rows))]
		keys := make([]string, 0, 3*len(chunk))
		for _, r := range chunk {
			keys = append(keys, playerZoneKey(r.PlayerID), placementKey(r.PlayerID), mapRevKey(r.PlayerID))
		}
		vals, err := p.rdb.MGet(ctx, keys...).Result()
		if err != nil {
			return rep, fmt.Errorf("route catch-up mget (%d keys): %w", len(keys), err)
		}
		if len(vals) != len(keys) {
			return rep, fmt.Errorf("%w: route catch-up mget returned %d values for %d keys", errUnexpectedRouteScriptReply, len(vals), len(keys))
		}
		for i, r := range chunk {
			need, err := decideRouteCatchUp(r, vals[3*i], vals[3*i+1], vals[3*i+2])
			if err != nil {
				return rep, err
			}
			if need {
				lagging = append(lagging, r)
			} else {
				rep.Current++
			}
		}
	}
	pub, err := p.publishRoutes(ctx, lagging)
	if err != nil {
		return rep, err
	}
	rep.Published = pub
	return rep, nil
}

// ── 围栏镜像 merge:in_progress:{zone}(规格 §13.1 第 3 条、§6.3 W12)────────────

// routeFenceHolderEpoch 从持有者 JSON 里取 fence_epoch(shared MergeFenceHolderEpoch 的镜像);取不到(不是 JSON 对象、
// 没有该字段、不是非负整数的 JSON **数字**)回 false。
//
// 与 shared 的实现有一处刻意不同:shared 把字段解进 *json.Number,而 encoding/json 允许把**带引号**的合法数字串
// (`{"fence_epoch":"7"}`)解进 json.Number,于是它回 (7, true);镜像脚本里 type(v.fence_epoch) 却是 string、按「解不出」回 2。
// 这里只认数字字面量(取原始 token 再 ParseUint),与 Lua 的判定一致(差异已登记为 shared 的缺陷)。
func routeFenceHolderEpoch(raw string) (uint64, bool) {
	var v struct {
		FenceEpoch json.RawMessage `json:"fence_epoch"`
	}
	if err := json.Unmarshal([]byte(raw), &v); err != nil || len(v.FenceEpoch) == 0 {
		return 0, false
	}
	n, err := strconv.ParseUint(string(v.FenceEpoch), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// routeFenceMirrorArgs 组装围栏镜像脚本的 KEYS / ARGV(shared MergeFenceMirrorArgs 的镜像):holder 里的 fence_epoch 必须等于 epoch,
// 过期时刻 = 库 expires_at_ms + routeFenceMirrorMarginMs(只会比库晚过期:多封锁、不少封锁)。
func routeFenceMirrorArgs(zone uint32, holderJSON string, epoch, expiresAtMs uint64) ([]string, []any, error) {
	if zone == 0 {
		return nil, nil, fmt.Errorf("%w: merge fence zone must be >= 1", errRouteInvalidArgument)
	}
	got, ok := routeFenceHolderEpoch(holderJSON)
	if !ok || got != epoch || epoch == 0 {
		return nil, nil, fmt.Errorf("%w: merge fence holder carries fence_epoch %d (ok=%v), want %d (> 0)", errRouteInvalidArgument, got, ok, epoch)
	}
	if expiresAtMs == 0 {
		return nil, nil, fmt.Errorf("%w: merge fence expires_at_ms must be > 0", errRouteInvalidArgument)
	}
	return []string{mergeFenceKey(zone)},
		[]any{holderJSON, strconv.FormatUint(epoch, 10), strconv.FormatUint(expiresAtMs+routeFenceMirrorMarginMs, 10)}, nil
}

// setFenceMirror 写围栏镜像(获取之后、续期之后):同一 pipeline 里 EVAL 镜像脚本 + WAITAOF。回 0(镜像里已是更大的 epoch)
// → errRouteFenceMirrorStale;回 2(解不出 fence_epoch 的旧值)→ errRouteFenceMirrorForeign;都是零写入、不重试。
// 镜像 + WAITAOF 确认之后,调用方才能收集清单(scene_manager / guild 读的是镜像)。
func (p *routePublisher) setFenceMirror(ctx context.Context, zone uint32, holderJSON string, epoch, expiresAtMs uint64) error {
	keys, args, err := routeFenceMirrorArgs(zone, holderJSON, epoch, expiresAtMs)
	if err != nil {
		return err
	}
	return p.durableWrite(ctx, "merge fence mirror "+keys[0], func(pipe redis.Pipeliner) func() (bool, error) {
		cmd := pipe.Eval(ctx, routeFenceMirrorSetScript, keys, args...)
		return func() (bool, error) {
			if err := cmd.Err(); err != nil {
				return !isRouteRedisErrorReply(err), err
			}
			n, ok := cmd.Val().(int64)
			switch {
			case !ok || n < 0 || n > 2:
				return false, fmt.Errorf("%w: merge fence mirror reply %T %v", errUnexpectedRouteScriptReply, cmd.Val(), cmd.Val())
			case n == 0:
				return false, fmt.Errorf("%w: %s, this run writes fence_epoch %d (another holder took the fence)", errRouteFenceMirrorStale, keys[0], epoch)
			case n == 2:
				return false, fmt.Errorf("%w: %s was written by an older merge_zone or by hand; check it and remove it after confirming no merge runs",
					errRouteFenceMirrorForeign, keys[0])
			}
			return false, nil
		}
	})
}

// deleteFenceMirror 释放之后按完整值 compare-and-delete 镜像(不再用子串匹配)。返回是否删了;值已不是 holderJSON(被换过 /
// 已过期消失)时不删、回 false。删失败由 leader 的围栏镜像对账按库删。不带 WAITAOF:删没落盘的后果只是多封锁一会儿。
func (p *routePublisher) deleteFenceMirror(ctx context.Context, zone uint32, holderJSON string) (bool, error) {
	return p.compareAndDelete(ctx, mergeFenceKey(zone), holderJSON)
}

// ── 合走标记镜像 merge:merged_into:{src}(W6 / 5'a)─────────────────────────────

// setMergedIntoMirror 在库里写好合走标记之后写镜像(无 TTL)+ WAITAOF(同一 pipeline)。value 必须是 encodeRouteMergedIntoValue 的结果。
func (p *routePublisher) setMergedIntoMirror(ctx context.Context, src uint32, value string) error {
	var v mergedIntoValue
	if err := json.Unmarshal([]byte(value), &v); err != nil || v.Dst == 0 || v.Dst == src || !validRunID(v.RunID) {
		return fmt.Errorf("%w: merged_into value %q for zone %d", errRouteInvalidArgument, value, src)
	}
	key := mergedIntoKey(src)
	return p.durableWrite(ctx, "merged_into mirror "+key, func(pipe redis.Pipeliner) func() (bool, error) {
		cmd := pipe.Set(ctx, key, value, 0)
		return func() (bool, error) {
			if err := cmd.Err(); err != nil {
				return !isRouteRedisErrorReply(err), err
			}
			return false, nil
		}
	})
}

// deleteMergedIntoMirror 撤销删掉库里的标记之后,按完整值 compare-and-delete 镜像。
func (p *routePublisher) deleteMergedIntoMirror(ctx context.Context, src uint32, value string) (bool, error) {
	return p.compareAndDelete(ctx, mergedIntoKey(src), value)
}

func (p *routePublisher) compareAndDelete(ctx context.Context, key, value string) (bool, error) {
	if err := checkMaintenanceTool(ctx); err != nil {
		return false, err
	}
	v, err := p.rdb.Eval(ctx, routeCompareAndDeleteScript, []string{key}, value).Result()
	if err != nil {
		holdMaintenanceTool(ctx)
		return false, fmt.Errorf("compare-and-delete %s: %w", key, err)
	}
	n, ok := v.(int64)
	if !ok || (n != 0 && n != 1) {
		holdMaintenanceTool(ctx)
		return false, fmt.Errorf("%w: compare-and-delete %s replied %T %v", errUnexpectedRouteScriptReply, key, v, v)
	}
	return n == 1, nil
}

// ── 水位键与「库与 Redis 成对」核对(规格 §13.3 第 3 条)───────────────────────

// routeWatermark 是 mapping:route:watermark 的值(shared RouteWatermark 的镜像)。
type routeWatermark struct {
	V           int    `json:"v"`
	Generation  string `json:"generation"`
	WatermarkMs uint64 `json:"watermark_ms"`
}

// decodeRouteWatermark 解析水位键(shared DecodeRouteWatermark 的镜像,同样分两步解:先只解 v,
// v 大于已知版本回 errRouteWatermarkNewerVersion,再整体解与校验)。
func decodeRouteWatermark(raw string) (routeWatermark, error) {
	var head struct {
		V int `json:"v"`
	}
	if err := json.Unmarshal([]byte(raw), &head); err != nil {
		return routeWatermark{}, fmt.Errorf("%w: not a JSON object with an integer v: %v", errRouteWatermarkMalformed, err)
	}
	if head.V > routeWatermarkFormatVersion {
		return routeWatermark{}, fmt.Errorf("%w: v=%d, this binary knows v<=%d", errRouteWatermarkNewerVersion, head.V, routeWatermarkFormatVersion)
	}
	var w routeWatermark
	if err := json.Unmarshal([]byte(raw), &w); err != nil {
		return routeWatermark{}, fmt.Errorf("%w: not a JSON object of the expected shape: %v", errRouteWatermarkMalformed, err)
	}
	switch {
	case w.V != routeWatermarkFormatVersion:
		return routeWatermark{}, fmt.Errorf("%w: v=%d, want %d", errRouteWatermarkMalformed, w.V, routeWatermarkFormatVersion)
	case !routeLowerHexOfLen(w.Generation, 1, routeGenerationMaxLen):
		return routeWatermark{}, fmt.Errorf("%w: generation %q must be 1..%d chars of [0-9a-f]", errRouteWatermarkMalformed, w.Generation, routeGenerationMaxLen)
	case w.WatermarkMs == 0:
		return routeWatermark{}, fmt.Errorf("%w: watermark_ms must be > 0", errRouteWatermarkMalformed)
	}
	return w, nil
}

// routeLowerHexOfLen 报告 s 是否由 [minLen, maxLen] 个 [0-9a-f] 组成(shared lowerHexOfLen)。
func routeLowerHexOfLen(s string, minLen, maxLen int) bool {
	if len(s) < minLen || len(s) > maxLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// readWatermark 读水位键:不存在回 present=false;不是 string(WRONGTYPE)等错误原样返回(fail-closed)。
func (p *routePublisher) readWatermark(ctx context.Context) (string, bool, error) {
	raw, err := p.rdb.Get(ctx, routeWatermarkKey).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get %s: %w", routeWatermarkKey, err)
	}
	return raw, true, nil
}

// decideRoutePairing 是「库与 Redis 是否成对」的纯判定(规格 §13.3 第 3 条):
//   - redis:见到水位键就拒绝 —— 有 db_first 的对账 leader 在维护这个 Redis,映射必须经库写;
//   - db:水位键必须存在、可解码、世代等于该库 route_meta.generation —— 证明有 db_first 的 leader 正在用这份库维护这个 Redis。
//     不存在 = 还没到 P2,或 -mapping-redis-addr 指错了实例;世代不等 = 这个 Redis 配的是另一份全局库。
func decideRoutePairing(source, wmRaw string, wmPresent bool, dbGeneration string) error {
	switch source {
	case routeSourceRedis:
		if wmPresent {
			return fmt.Errorf("%w: %s exists on this mapping Redis (%q): a db_first data_service maintains it, "+
				"so the mapping must be written through the global DB (-route-source=db)", errRoutePairing, routeWatermarkKey, wmRaw)
		}
		return nil
	case routeSourceDB:
		if !wmPresent {
			return fmt.Errorf("%w: %s is missing: no db_first reconcile leader maintains this mapping Redis with the global DB "+
				"(data_service not on RouteStore.Mode=db_first yet, or -mapping-redis-addr points at another instance)", errRoutePairing, routeWatermarkKey)
		}
		wm, err := decodeRouteWatermark(wmRaw)
		if err != nil {
			return fmt.Errorf("%w: %s = %q: %w", errRoutePairing, routeWatermarkKey, wmRaw, err)
		}
		if wm.Generation != dbGeneration {
			return fmt.Errorf("%w: %s generation %q != route_meta.generation %q of -route-db-dsn: this Redis is paired with another global DB",
				errRoutePairing, routeWatermarkKey, wm.Generation, dbGeneration)
		}
		return nil
	}
	return fmt.Errorf("%w: -route-source %q", errRouteInvalidArgument, source)
}
