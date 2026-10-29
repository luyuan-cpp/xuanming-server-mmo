package routing

// 第 3 批读方门禁:data_service 的读路径(docs/handoff/launch-readiness-20261013/spec_D1_batch1b2_2_3.md §5.1、§5.2、§13.2)。
//
// RouteStore.ReadRequiresReady=true 时(只许 db_first,config.RouteStoreConfig.Validate 钉住),R2 / R3 / R6 先在**同一段只读 Lua**
// 里取回就绪标记、本实例身份(INFO server / replication)与映射键,由 placement.VerifyRouteReady 判副本可不可信(M1):
//   - Trusted:用缓存;缺键 = 权威缺席(NotFound 契约不变);
//   - 其余 verdict、脚本执行出错(含 Redis 不可达、熔断拒绝)、回复形状不对:**值与缺键都不可信**,回源全局库(RouteReader):
//     无行 / home 为 NULL → ErrHomeZoneNotMapped(此时是库的权威缺席);SQL 故障或回源读者没装配 → ErrRouteReadUnavailable
//     (gRPC 回 Unavailable,绝不 NotFound —— 一次库故障不能伪装成「这名玩家没有归属」,login 会据此回退建角 zone)。
//   - **不回填缓存**(规格 §10 X6):缓存只由写者发布与对账 leader 推送;读方回填与预热并发虽安全,但标记挂上之前读方本来就不信缓存。
//   - 例外(M1-2,2026-10-21):读脚本或回源 SQL 失败时调用方已放弃(ctx 取消 / 超时)→ 不回源、不计 script_error / sql_error,
//     回带 ctx 错误的 error(callerAbandoned;gRPC 层回 Canceled / DeadlineExceeded,不回 Unavailable + 码 30)。
//
// R7(GetAllPlayerIDsInZone)在 db_first 下一律读库(与开关无关);redis 模式保持 SCAN。
// 开关为 false 时 R2 / R3 / R6 与今天逐字节相同:本文件里带 Ready 的函数一个都不会被调到。
//
// 每次读多两次 INFO(在 Lua 里 string.match 取字段,不回传整段文本),开销未测,纳入第 3 批压测(规格 §2 M1)。

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"data_service/internal/metrics"
	"data_service/internal/store"

	"shared/placement"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

// ErrRouteReadUnavailable 表示读方门禁回源失败:缓存不可信,而 player_route 的回源读出了故障(SQL 错误、超时),
// 或回源读者没装配(RouteStore.Mode=db_first 但 route store 没起来)。R7 在 db_first 下的读库失败也包它。
// gRPC 层回 Unavailable + ErrCodeRouteStoreError,**绝不**回 NotFound;它与 ErrHomeZoneNotMapped 从不同时出现在一条错误链里。
var ErrRouteReadUnavailable = errors.New("route read unavailable: the mapping cache is not trusted and the player_route fallback failed")

// routeReadEvalFunc 是读脚本的执行入口(EVAL,签名同 go-zero redis.Redis.EvalCtx)。
type routeReadEvalFunc func(ctx context.Context, script string, keys []string, args ...any) (any, error)

// getHomeZoneFenceReadyScript 是 R2 在开关打开时的读脚本:在 getHomeZoneAndMergeFenceScript 的基础上,同一段 Lua 里
// 先取就绪标记与本实例身份(与 placement.RouteReadyReadScript 逐行相同的四个字段,TestHomeFenceReadyScriptSharesIdentityLines
// 钉住),再取 home 与按 home 拼出的围栏键。身份、home、围栏是同一进程、同一时刻的快照。
//
//	KEYS[1] = mapping:route:ready  KEYS[2] = player:zone:{id}  ARGV[1] = merge:in_progress: 前缀
//	返回 {marker 或 nil, run_id, role, master_replid, home 或 nil, fence 或 nil}
//
// 后两项按 placement.ParseRouteReadyRead 的约定都是 string 或 nil(nMappingKeys = 2),这样回复能直接交给共享的解析与判定:
//   - home 缺席时两项都是 nil;
//   - fence 是 EXISTS 结果的十进制串 "0" / "1";围栏读失败(pcall 拿到错误)为 "-1",与 getHomeZoneAndMergeFenceScript 的
//     mergeFenceUnreadable 同一口径(当封锁)。
//
// 围栏键不在 KEYS 里的理由与限制同 getHomeZoneAndMergeFenceScript(mapping Redis 是单实例;换 Cluster 后走 pcall 的失败分支)。
const getHomeZoneFenceReadyScript = `-- home + merge fence + route ready marker,spec_D1_batch1b2_2_3.md §5.2
local m = redis.pcall('GET', KEYS[1])
if type(m) ~= 'string' then m = false end
local srv = redis.call('INFO', 'server')
local repl = redis.call('INFO', 'replication')
local out = {m,
  string.match(srv, 'run_id:(%x+)') or '',
  string.match(repl, 'role:(%a+)') or '',
  string.match(repl, 'master_replid:(%x+)') or '',
  false, false}
local home = redis.call('GET', KEYS[2])
if not home then
  return out
end
out[5] = home
local fenced = redis.pcall('EXISTS', ARGV[1] .. home)
if type(fenced) ~= 'number' then
  out[6] = '-1'
else
  out[6] = tostring(fenced)
end
return out
`

// readyOutcome 是一次就绪读的结论:trusted 时 read.Values 可用;否则 reason 说明为什么不可信(verdict 名字或脚本错误),
// 只进日志与错误信息。abandoned 非 nil = 脚本执行失败时调用方已经放弃(见 callerAbandoned):调用方原样返回它,不回源。
type readyOutcome struct {
	read      placement.RouteReadyRead
	trusted   bool
	reason    string
	abandoned error
}

// callerAbandoned 是读方门禁对「调用方已放弃」的判据与包装(M1-2,2026-10-21):读失败(读脚本执行出错、回源 SQL 出错)时
// ctx 已取消 / 超时,失败就是调用方自己的截止时间造成的 —— 既不能当成「缓存不可信」去回源(拿着已结束的 ctx 回源必然立刻失败),
// 也不能记成 script_error / sql_error、回 Unavailable + 码 30(那会触发「缓存不可信、全局库也不可用」的 critical 告警,把值班引去查
// 一个正常的全局库)。判据同 logic/persisted_player_database.go:先看 ctx.Err()。
//
// 返回 nil = ctx 还活着,调用方照常处理 cause;非 nil 时调用方原样返回它,并且**不计** route_ready_verdict_total / route_read_total
// (调用方的取消 / 超时由 gRPC 层的状态码统计,见 server.GetPlayerHomeZone)。错误链带 ctx 的错误(errors.Is 认 context.Canceled /
// context.DeadlineExceeded),**不**包 ErrRouteReadUnavailable 与 ErrHomeZoneNotMapped;cause 只进文案。
// 代价:ctx 恰在一次真实故障之后、判定之前到期时,这一次故障不计数 —— 调用方已经放弃,这次读的结论本来就没人用。
func callerAbandoned(ctx context.Context, what string, cause error) error {
	ctxErr := ctx.Err()
	if ctxErr == nil {
		return nil
	}
	return fmt.Errorf("%s abandoned by the caller: %w (last error: %v)", what, ctxErr, cause)
}

// readyRead 执行一段就绪读脚本(回复形状同 placement.RouteReadyReadScript,nValues 个映射值)并判 verdict。
// 脚本执行出错与回复形状不对都按不可信处理(规格 §5.1「脚本执行出错 → 回源 SQL」),不向上返回错误;
// 例外是脚本执行出错时调用方已放弃(callerAbandoned):回 abandoned,不回源、不计 verdict。
// 其余每次调用恰好记一次 route_ready_verdict_total。
func (r *Router) readyRead(ctx context.Context, script string, keys []string, nValues int, args ...any) readyOutcome {
	raw, err := r.routeReadEval(ctx, script, keys, args...)
	if err != nil {
		if abandoned := callerAbandoned(ctx, "route ready read", err); abandoned != nil {
			return readyOutcome{abandoned: abandoned}
		}
	} else {
		var read placement.RouteReadyRead
		if read, err = placement.ParseRouteReadyRead(raw, nValues); err == nil {
			verdict, _ := placement.VerifyRouteReady(read)
			metrics.ObserveRouteReadyVerdict(verdict.String())
			return readyOutcome{read: read, trusted: verdict == placement.RouteReadyTrusted, reason: verdict.String()}
		}
	}
	metrics.ObserveRouteReadyVerdict(metrics.RouteReadyVerdictScriptError)
	return readyOutcome{reason: metrics.RouteReadyVerdictScriptError + ": " + err.Error()}
}

// observeCacheRead 记一次「用了可信缓存」的读:成功与权威缺席都算 cache;缓存值畸形是故障(调用方照今天的口径回错误),不计。
func observeCacheRead(path string, err error) {
	if err == nil || errors.Is(err, ErrHomeZoneNotMapped) {
		metrics.ObserveRouteRead(path, metrics.RouteReadResultCache)
	}
}

// routeReaderMissing 是回源读者没装配时的错误(db_first 但 route store 没起来;启动日志里已有一条带原因的 ERROR)。
func routeReaderMissing(path string, playerID uint64, reason string) error {
	return fmt.Errorf("%w: %s read for player %d: route reader not assembled (RouteStore.Mode=db_first, cache %s)",
		ErrRouteReadUnavailable, path, playerID, reason)
}

// ── R2:home + 合服围栏 ──────────────────────────────────────────────────────────

// getPlayerHomeZoneAndMergeFenceReady 是开关打开时的 GetPlayerHomeZoneAndMergeFence。返回口径与开关关闭时相同
// (缺席 → ErrHomeZoneNotMapped;home 读到了而围栏读失败 → merging=true),另加回源失败 → ErrRouteReadUnavailable;
// 调用方已放弃(ctx 取消 / 超时)时回带 ctx 错误的 error(callerAbandoned),不回源、不计数。
func (r *Router) getPlayerHomeZoneAndMergeFenceReady(ctx context.Context, playerID uint64) (uint32, bool, error) {
	const path = metrics.RouteReadPathHomeFence
	out := r.readyRead(ctx, getHomeZoneFenceReadyScript, placement.RouteReadyReadKeys(mappingKey(playerID)), 2, mergeFenceKeyPrefix)
	if out.abandoned != nil {
		return 0, false, fmt.Errorf("home and merge fence for player %d: %w", playerID, out.abandoned)
	}
	if out.trusted {
		zone, merging, err := decodeReadyHomeFence(playerID, out.read.Values)
		observeCacheRead(path, err)
		return zone, merging, err
	}
	logx.Debugf("[RouteRead] home_fence: cache not trusted (%s), reading player_route: player=%d", out.reason, playerID)
	if r.routeReader == nil {
		metrics.ObserveRouteRead(path, metrics.RouteReadResultSQLError)
		return 0, false, routeReaderMissing(path, playerID, out.reason)
	}
	hf, err := r.routeReader.GetHomeAndFence(ctx, playerID)
	if err != nil {
		if abandoned := callerAbandoned(ctx, fmt.Sprintf("player_route fallback for player %d (cache %s)", playerID, out.reason), err); abandoned != nil {
			return 0, false, abandoned
		}
		metrics.ObserveRouteRead(path, metrics.RouteReadResultSQLError)
		return 0, false, fmt.Errorf("%w: home and merge fence for player %d (cache %s): %w", ErrRouteReadUnavailable, playerID, out.reason, err)
	}
	if !hf.HasHome() {
		metrics.ObserveRouteRead(path, metrics.RouteReadResultNotFoundSQL)
		_, notMapped := parseHomeZoneValue(playerID, "")
		return 0, false, notMapped
	}
	metrics.ObserveRouteRead(path, metrics.RouteReadResultSQL)
	return hf.HomeZone, hf.Fenced, nil
}

// decodeReadyHomeFence 把就绪读脚本回的 [home, fence] 翻成 getHomeZoneAndMergeFenceScript 的回复形状,交给
// decodeHomeZoneAndMergeFenceReply:两条读路径对「缺席」「畸形」「围栏读失败」的口径只有一份。
// fence 不是可解析的十进制串时按 mergeFenceUnreadable(当封锁)。纯函数,单测直接测。
func decodeReadyHomeFence(playerID uint64, vals []any) (uint32, bool, error) {
	if len(vals) != 2 {
		return 0, false, fmt.Errorf("unexpected home zone ready lookup reply for player %d: %d values, want 2", playerID, len(vals))
	}
	home, _ := vals[0].(string) // nil = 键不存在,与空串同口径(parseHomeZoneValue)
	if home == "" {
		return decodeHomeZoneAndMergeFenceReply(playerID, []any{"", int64(0)})
	}
	fence := mergeFenceUnreadable
	if s, ok := vals[1].(string); ok {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			fence = n
		}
	}
	return decodeHomeZoneAndMergeFenceReply(playerID, []any{home, fence})
}

// ── R6:只要 home(含 ClientForPlayer)─────────────────────────────────────────────

// getPlayerHomeZoneReady 是开关打开时的 GetPlayerHomeZone:同 R2 去掉围栏。回源用 GetRoutes 的单 id 点查(提案 §11.2
// G1a 模板「按 id 回源」),不碰围栏表。调用方已放弃时同 R2(callerAbandoned)。
func (r *Router) getPlayerHomeZoneReady(ctx context.Context, playerID uint64) (uint32, error) {
	const path = metrics.RouteReadPathHome
	out := r.readyRead(ctx, placement.RouteReadyReadScript, placement.RouteReadyReadKeys(mappingKey(playerID)), 1)
	if out.abandoned != nil {
		return 0, fmt.Errorf("home for player %d: %w", playerID, out.abandoned)
	}
	if out.trusted {
		val, _ := out.read.Values[0].(string) // nil = 键不存在
		zone, err := parseHomeZoneValue(playerID, val)
		observeCacheRead(path, err)
		return zone, err
	}
	logx.Debugf("[RouteRead] home: cache not trusted (%s), reading player_route: player=%d", out.reason, playerID)
	if r.routeReader == nil {
		metrics.ObserveRouteRead(path, metrics.RouteReadResultSQLError)
		return 0, routeReaderMissing(path, playerID, out.reason)
	}
	rows, err := r.routeReader.GetRoutes(ctx, []uint64{playerID})
	if err != nil {
		if abandoned := callerAbandoned(ctx, fmt.Sprintf("player_route fallback for player %d (cache %s)", playerID, out.reason), err); abandoned != nil {
			return 0, abandoned
		}
		metrics.ObserveRouteRead(path, metrics.RouteReadResultSQLError)
		return 0, fmt.Errorf("%w: home for player %d (cache %s): %w", ErrRouteReadUnavailable, playerID, out.reason, err)
	}
	row, ok := rows[playerID]
	if !ok || !row.HasHome() {
		metrics.ObserveRouteRead(path, metrics.RouteReadResultNotFoundSQL)
		return parseHomeZoneValue(playerID, "")
	}
	metrics.ObserveRouteRead(path, metrics.RouteReadResultSQL)
	return row.HomeZone, nil
}

// ── R3:批量 home ─────────────────────────────────────────────────────────────────

// batchGetPlayerHomeZoneReady 是开关打开时的 BatchGetPlayerHomeZone。按 store.RouteBatchLimit 分块(一段脚本的 KEYS 与一次
// GetRoutes 的 IN 都受它约束),每块独立判可信:可信的块用缓存(缺键 = 缺席,不进结果;畸形值整批报错,同今天),不可信的块
// 整块 GetRoutes 回源;任一块回源失败整批报错、不回半份结果(半份会被调用方当成「这些人没有归属」)。
// 块与块之间不是同一快照 —— 今天的单次 MGET 之外,调用方本来就拿不到跨批的原子视图。
// 空列表直接回空 map,不发任何命令(开关关闭时空列表的 MGET 会被 Redis 以参数个数错误拒绝,这里不复刻那个错误)。
func (r *Router) batchGetPlayerHomeZoneReady(ctx context.Context, playerIDs []uint64) (map[uint64]uint32, error) {
	result := make(map[uint64]uint32, len(playerIDs))
	for start := 0; start < len(playerIDs); start += store.RouteBatchLimit {
		end := min(start+store.RouteBatchLimit, len(playerIDs))
		if err := r.batchChunkReady(ctx, playerIDs[start:end], result); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// batchChunkReady 处理一块(≤ store.RouteBatchLimit 个 id),结果写进 result。每块恰好记一次 route_read_total{path="batch"};
// 调用方已放弃(callerAbandoned)时不计、整批返回带 ctx 错误的 error。
func (r *Router) batchChunkReady(ctx context.Context, ids []uint64, result map[uint64]uint32) error {
	const path = metrics.RouteReadPathBatch
	keys := make([]string, len(ids))
	for i, pid := range ids {
		keys[i] = mappingKey(pid)
	}
	out := r.readyRead(ctx, placement.RouteReadyReadScript, placement.RouteReadyReadKeys(keys...), len(ids))
	if out.abandoned != nil {
		return fmt.Errorf("batch read of %d ids: %w", len(ids), out.abandoned)
	}
	if out.trusted {
		for i, v := range out.read.Values {
			val, _ := v.(string) // nil = 键不存在
			if val == "" {
				continue
			}
			zoneID, err := parseHomeZoneValue(ids[i], val)
			if err != nil {
				return fmt.Errorf("batch mapping lookup: %w", err)
			}
			result[ids[i]] = zoneID
		}
		metrics.ObserveRouteRead(path, metrics.RouteReadResultCache)
		return nil
	}
	logx.Debugf("[RouteRead] batch: cache not trusted (%s), reading player_route for %d ids", out.reason, len(ids))
	if r.routeReader == nil {
		metrics.ObserveRouteRead(path, metrics.RouteReadResultSQLError)
		return fmt.Errorf("%w: batch read of %d ids: route reader not assembled (RouteStore.Mode=db_first, cache %s)",
			ErrRouteReadUnavailable, len(ids), out.reason)
	}
	rows, err := r.routeReader.GetRoutes(ctx, ids)
	if err != nil {
		if abandoned := callerAbandoned(ctx, fmt.Sprintf("player_route fallback for a batch of %d ids (cache %s)", len(ids), out.reason), err); abandoned != nil {
			return abandoned
		}
		metrics.ObserveRouteRead(path, metrics.RouteReadResultSQLError)
		return fmt.Errorf("%w: batch read of %d ids (cache %s): %w", ErrRouteReadUnavailable, len(ids), out.reason, err)
	}
	for _, pid := range ids {
		if row, ok := rows[pid]; ok && row.HasHome() {
			result[pid] = row.HomeZone
		}
	}
	metrics.ObserveRouteRead(path, metrics.RouteReadResultSQL)
	return nil
}

// ── R7:按 zone 枚举(db_first 一律读库)──────────────────────────────────────────

// allPlayerIDsInZoneFromDB 是 db_first 下的 GetAllPlayerIDsInZone:沿 (route_shard, player_id) 键集翻页
// ScanRoutesByHome(走 idx_route_home),每页 store.RouteBatchLimit 行,一页不足即翻完。结果按聚簇键序,不是 player_id 序
// (调用方只当集合用)。任一页失败、或游标没有前进(store 的契约被破坏,防死循环)→ 整体报错(包 ErrRouteReadUnavailable),
// 不回半份名单 —— 与 redis 模式「宁可失败也不能少人」同口径。每次调用恰好记一次 route_read_total{path="enumerate"};
// 例外:某页失败时调用方已放弃(callerAbandoned,M1-2)—— 不计 sql_error,返回带 ctx 错误的 error(同样不回半份名单)。
func (r *Router) allPlayerIDsInZoneFromDB(ctx context.Context, zoneID uint32) ([]uint64, error) {
	const path = metrics.RouteReadPathEnumerate
	if r.routeReader == nil {
		metrics.ObserveRouteRead(path, metrics.RouteReadResultSQLError)
		return nil, fmt.Errorf("%w: enumerate zone %d: route reader not assembled (RouteStore.Mode=db_first)", ErrRouteReadUnavailable, zoneID)
	}
	var ids []uint64
	var cursor store.RouteScanCursor
	for {
		page, err := r.routeReader.ScanRoutesByHome(ctx, zoneID, cursor, store.RouteBatchLimit)
		if err != nil {
			if abandoned := callerAbandoned(ctx, fmt.Sprintf("enumerate zone %d after (shard, player_id)=(%d, %d)", zoneID, cursor.Shard, cursor.PlayerID), err); abandoned != nil {
				return nil, abandoned
			}
			metrics.ObserveRouteRead(path, metrics.RouteReadResultSQLError)
			return nil, fmt.Errorf("%w: enumerate zone %d after (shard, player_id)=(%d, %d): %w",
				ErrRouteReadUnavailable, zoneID, cursor.Shard, cursor.PlayerID, err)
		}
		if len(page) > store.RouteBatchLimit {
			metrics.ObserveRouteRead(path, metrics.RouteReadResultSQLError)
			return nil, fmt.Errorf("%w: enumerate zone %d: a page of %d rows exceeds the limit %d",
				ErrRouteReadUnavailable, zoneID, len(page), store.RouteBatchLimit)
		}
		for _, row := range page {
			ids = append(ids, row.PlayerID)
		}
		if len(page) < store.RouteBatchLimit {
			break
		}
		next := store.RouteScanCursorAt(page[len(page)-1])
		if !cursor.Less(next) {
			metrics.ObserveRouteRead(path, metrics.RouteReadResultSQLError)
			return nil, fmt.Errorf("%w: enumerate zone %d: cursor did not advance past (shard, player_id)=(%d, %d)",
				ErrRouteReadUnavailable, zoneID, cursor.Shard, cursor.PlayerID)
		}
		cursor = next
	}
	metrics.ObserveRouteRead(path, metrics.RouteReadResultSQL)
	return ids, nil
}

// ── 启动探针 ─────────────────────────────────────────────────────────────────────

// ProbeRouteReadyRead 是 ReadRequiresReady=true 时的启动探针(规格 §13.2 第 2 条):在 mapping Redis 上试跑一次
// placement.RouteReadyReadScript(只带就绪标记键,不带映射键)。脚本执行出错(INFO 在脚本里不可用、ACL 不许 EVAL / INFO、
// 连不上)或回复形状不对就返回 error,调用方拒启(svc.newPlayerRouteStore 不装配 route store → main 无论 Store.Required
// 取值都拒启,见 svc.ServiceContext.RouteReadGateUnassembled;M1-5,2026-10-22)。
//
// verdict 不要求是 Trusted:标记要等对账 leader 预热完才挂,启动时不可信是正常状态(读方届时回源库);返回它只为启动日志,
// 口径见 probeRouteReadyVerdict(本实例身份取不到 / 不是主时不论标记在不在都如实回报,A2-11)。
// 用自己新建的 *redis.Redis;go-zero 按地址复用底层客户端与熔断器(见 ProbeWaitAOF 的 2026-10-18 更正),探针失败同样计进
// Router 的熔断 —— 探针只在启动时跑一次,影响可以忽略。
func ProbeRouteReadyRead(ctx context.Context, mapping redis.RedisConf) (placement.RouteReadyVerdict, error) {
	rds, err := redis.NewRedis(mapping)
	if err != nil {
		return placement.RouteReadyAbsent, fmt.Errorf("route ready read probe: mapping redis %s: %w", mapping.Host, err)
	}
	raw, err := rds.EvalCtx(ctx, placement.RouteReadyReadScript, placement.RouteReadyReadKeys())
	if err != nil {
		return placement.RouteReadyAbsent, fmt.Errorf("route ready read script failed on mapping redis %s "+
			"(needs EVAL and INFO inside scripts, Redis >= 7.2): %w", mapping.Host, err)
	}
	read, err := placement.ParseRouteReadyRead(raw, 0)
	if err != nil {
		return placement.RouteReadyAbsent, fmt.Errorf("route ready read probe on mapping redis %s: %w", mapping.Host, err)
	}
	return probeRouteReadyVerdict(read), nil
}

// probeRouteReadyVerdict 是启动探针报给日志的结论(A2-11,2026-10-22,纯函数):本实例的身份取不到(脚本里 run_id /
// master_replid / role 为空)或不是主时,**不论标记在不在**都回 IdentityUnavailable / NotMaster;否则回
// placement.VerifyRouteReady 的结论(标记不在时是 Absent,启动时的正常状态)。
//
// 为什么身份先判:VerifyRouteReady 先看标记在不在,标记不在就回 Absent、不再看身份 —— 而启动时标记通常不在,MappingRedis 指到
// 副本(只读端点)这类确定性配置错误就被「anything but trusted is normal」盖掉。挂标记脚本同样要求本实例是主、身份取得到,
// 这两种情况下标记对这个实例永远挂不上,读方会永久回源库,调用方要把它当配置错误报出来(svc.probeRouteReadyReadAtStartup)。
// 只用于启动日志,读路径的判据仍只有 VerifyRouteReady。身份字段这里只判空(脚本用 string.match 取不到时回空串);
// 更细的格式校验(40 位小写十六进制)仍由 VerifyRouteReady 在标记在时做。判主的口径同 routereconcile 的 probe(role 恒为 "master")。
func probeRouteReadyVerdict(read placement.RouteReadyRead) placement.RouteReadyVerdict {
	id := read.Identity
	switch {
	case id.RunID == "" || id.MasterReplID == "" || id.Role == "":
		return placement.RouteReadyIdentityUnavailable
	case id.Role != "master":
		return placement.RouteReadyNotMaster
	}
	verdict, _ := placement.VerifyRouteReady(read)
	return verdict
}
