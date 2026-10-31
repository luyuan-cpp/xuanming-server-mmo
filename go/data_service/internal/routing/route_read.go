package routing

// 第 3 批读方门禁:data_service 的读路径(docs/handoff/launch-readiness-20261013/spec_D1_batch1b2_2_3.md §5.1、§5.2、§13.2)。
//
// RouteStore.ReadRequiresReady=true 时(只许 db_first,config.RouteStoreConfig.Validate 钉住),R3 / R6 先在**同一段只读 Lua**
// 里取回就绪标记、本实例身份(INFO server / replication)与映射键,由 placement.VerifyRouteReady 判副本可不可信(M1):
//   - Trusted:用缓存;缺键 = 权威缺席(NotFound 契约不变);
//   - 其余 verdict、脚本执行出错(含 Redis 不可达、熔断拒绝)、回复形状不对:**值与缺键都不可信**,回源全局库(RouteReader):
//     无行 / home 为 NULL → ErrHomeZoneNotMapped(此时是库的权威缺席);SQL 故障或回源读者没装配 → ErrRouteReadUnavailable
//     (gRPC 回 Unavailable,绝不 NotFound —— 一次库故障不能伪装成「这名玩家没有归属」,login 会据此回退建角 zone)。
//   - **不回填缓存**(规格 §10 X6):缓存只由写者发布与对账 leader 推送;读方回填与预热并发虽安全,但标记挂上之前读方本来就不信缓存。
//   - 例外(M1-2,2026-10-21):读脚本或回源 SQL 失败时调用方已放弃(ctx 取消 / 超时)→ 不回源、不计 script_error / sql_error,
//     回带 ctx 错误的 error(callerAbandoned;gRPC 层回 Canceled / DeadlineExceeded,不回 Unavailable + 码 30)。
//
// 开关为 false 时 R3 / R6 与今天逐字节相同:本文件里带 Ready 的函数一个都不会被调到。
//
// 2026-10-30(A6b):R2(home + 合服围栏同一段 Lua)随合服围栏删除,单查只有 R6。
//
// 每次读多两次 INFO(在 Lua 里 string.match 取字段,不回传整段文本),开销未测,纳入第 3 批压测(规格 §2 M1)。

import (
	"context"
	"errors"
	"fmt"

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

// ── R6:只要 home ─────────────────────────────────────────────

// getPlayerHomeZoneReady 是开关打开时的 GetPlayerHomeZone。回源用 GetRoutes 的单 id 点查(提案 §11.2 G1a 模板「按 id 回源」);
// 缺席 → ErrHomeZoneNotMapped,回源失败 → ErrRouteReadUnavailable;调用方已放弃(ctx 取消 / 超时)时回带 ctx 错误的 error
// (callerAbandoned),不回源、不计数。
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
