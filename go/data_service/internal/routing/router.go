package routing

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"data_service/internal/config"
	"data_service/internal/store"

	"shared/placement"

	goredis "github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

// Router 读写 player_id → home_zone 映射的 Redis 副本。玩家存档不经过这里(player_store 在 TiDB)。
// 这里的 home_zone 都是原始值;对外回答归属时由 handler 解析成最终区(MergedZoneCache)。
type Router struct {
	closeOnce    sync.Once
	closeErr     error
	mappingRedis *redis.Redis // global mapping: player_id → home_zone_id

	// routeMode 是 RegisterPlayerZone 的提交点(config.RouteStoreModeRedis | RouteStoreModeDBFirst),
	// routeStore 与 publish 只在 db_first 下使用(见 route_publish.go)。routeStore 为 nil 时 db_first 一律 fail-closed。
	routeMode  string
	routeStore RouteStore
	publish    publishConfig
	// publishRedis 是 db_first 下 W1 发布 + WAITAOF 专用的 go-redis 客户端(publish_redis.go):与 mappingRedis 同一个实例,
	// 但独立连接池、不经 go-zero 熔断器。只在 db_first 时由 NewRouter 构造(单测可用 withPublishRedis 注入);redis 模式恒为 nil。
	publishRedis *goredis.Client

	// 第 3 批读方门禁(route_read.go,规格 spec_D1_batch1b2_2_3.md §5.2):readRequiresReady 取 RouteStore.ReadRequiresReady,
	// 打开时 R3 / R6 先用就绪读脚本判副本可不可信,不可信回源 routeReader;R7 在 db_first 下一律读 routeReader。
	// routeReader 为 nil 时回源一律 fail-closed(ErrRouteReadUnavailable)。routeReadEval 是读脚本的执行入口
	// (缺省 mappingRedis.EvalCtx),只为单测能绕开 INFO(miniredis 不支持 INFO server / replication)。
	readRequiresReady bool
	routeReader       RouteReader
	routeReadEval     routeReadEvalFunc
}

// RouteReader 是第 3 批读方的回源读(规格 §5.2、§13.2 第 5 条):缓存不可信时按库读 home / 批量 home。
// 与写入点 RouteStore(route_publish.go)分成两个小接口(ISP):路由器的读路径只依赖它。
// *store.RouteStore 同时满足两者,svc 装配时同一个实例两边都注入。事务外一致读,契约见 store.RouteStore.GetRoutes;单测注入假实现。
type RouteReader interface {
	GetRoutes(ctx context.Context, ids []uint64) (map[uint64]store.RouteRow, error)
}

// WithRouteReader 注入回源读者。调用方只在 store 非 nil 时传(同 WithRouteStore 的「非 nil 接口包着 nil 指针」说明)。
func WithRouteReader(rr RouteReader) RouterOption {
	return func(r *Router) { r.routeReader = rr }
}

// NewRouter 建路由器。opts:WithRouteStore(db_first 的库内提交点)、WithRouteReader(第 3 批的回源读)。
//
// c.RouteStore 在这里再 Normalize + Validate 一次(main 启动时已校验过),非法就 panic,与 MustNewRedis 同口径:
// 单测与 debug 工具也走这里,不能拿一份非法配置静默跑成 redis 模式。校验放在建任何连接之前。
func NewRouter(c config.Config, opts ...RouterOption) *Router {
	routeCfg := c.RouteStore.Normalize()
	if err := routeCfg.Validate(); err != nil {
		panic(fmt.Sprintf("data_service router: invalid RouteStore config: %v", err))
	}
	r := &Router{
		mappingRedis: redis.MustNewRedis(c.MappingRedis),
		routeMode:    routeCfg.Mode,
		publish:      newPublishConfig(routeCfg),
	}
	r.readRequiresReady = routeCfg.ReadRequiresReady
	r.routeReadEval = r.mappingRedis.EvalCtx
	for _, opt := range opts {
		opt(r)
	}
	// db_first 的发布客户端(A2-1):在选项之后建,注入了就不自建。构造不拨号;MappingRedis 不合用(Type 不是 node)就 panic,
	// 与上面 MustNewRedis / Validate 同口径 —— 一份「发布必然失败」的配置不能装作健康跑下去。redis 模式不建,行为与改动前逐字相同。
	if routeCfg.DBFirst() && r.publishRedis == nil {
		pub, err := newPublishRedis(c.MappingRedis, routeCfg)
		if err != nil {
			panic(fmt.Sprintf("data_service router: RouteStore.Mode=db_first publish client: %v", err))
		}
		r.publishRedis = pub
		logx.Infof("[Router] db_first publish client → %s pool_size=%d read_timeout=%s (separate pool, no circuit breaker)",
			c.MappingRedis.Host, pub.Options().PoolSize, pub.Options().ReadTimeout)
	}

	return r
}

// ── Player-zone mapping ────────────────────────────────────────

const mappingKeyPrefix = "player:zone:"

func mappingKey(playerID uint64) string {
	return mappingKeyPrefix + strconv.FormatUint(playerID, 10)
}

// HomeZoneConflictError 表示 player:zone:{id} 已经存在且与请求的 zone 不同。
// 带上 Existing 是为了让调用方(和日志)一眼看出玩家原始归属是哪里。
type HomeZoneConflictError struct {
	PlayerID  uint64
	Existing  uint32
	Requested uint32
}

func (e *HomeZoneConflictError) Error() string {
	return fmt.Sprintf("player %d already mapped to home zone %d, refusing to overwrite with %d",
		e.PlayerID, e.Existing, e.Requested)
}

// RegisterPlayerZone 登记 player → home_zone 映射,**SETNX 语义:绝不覆盖**。
//
// 三种结果:
//   - 键不存在        → 写入,返回 nil
//   - 键存在且值相同  → 不写,返回 nil(幂等:login 的 CreatePlayer 允许重试)
//   - 键存在且值不同  → 不写,返回 *HomeZoneConflictError
//
// 合服不影响登记(docs/design/player-data-tidb-scaling-plan.md §7.1):合服过程中在源区建号是允许的,
// 他的 home_zone 就是源区,归属解析之后落到目标区,不需要任何闸门。
//
// storageID != 0 时顺带钉落点记录 player:placement:{id} = "{storageID}:1"
// (player-storage-placement.md §8.1),与 player:zone 在**同一段 Lua** 里完成:
// 只有 player:zone 这次真的写成了才写落点,home 已存在(不论同值幂等还是冲突)两键都不动。
// 为什么必须同一段脚本:分两次写,中间崩溃会留下「有 home、没落点」的玩家 ——
// 有效落点随之回落 home_zone,首次存盘写进 zone 库而不是登记时指定的库(如 Phase 2 全局库),
// 而且之后的重试因 home 已存在不会再补写落点,错位是永久的。
// storageID 只校验非 0;它指向的库能不能打开由 go/db 按需打开时判定(§6.1),这里不重复一份库名规则。
// storageID == 0 = 不钉落点,脚本只收一个键,行为与引入落点之前逐字节一致。
//
// 为什么不能覆盖:见 constants.ErrCodeZoneMappingConflict 的注释。
//
// RouteStore.Mode=db_first 时整段换成 registerPlayerZoneDBFirst(route_publish.go):提交点改在全局库
// player_route,Redis 只是发布出去的副本(docs/design/player-route-mapping-durability-proposal.md §4)。
// 上面三种结果的语义不变,另有几种新的失败(库故障、发布未确认),见那个函数的注释。
func (r *Router) RegisterPlayerZone(ctx context.Context, playerID uint64, homeZoneID, storageID uint32) error {
	if r.routeMode == config.RouteStoreModeDBFirst {
		return r.registerPlayerZoneDBFirst(ctx, playerID, homeZoneID, storageID)
	}
	val := strconv.FormatUint(uint64(homeZoneID), 10)
	keys := []string{mappingKey(playerID)}
	args := []any{val}
	if storageID != 0 {
		// 键名与值格式只取自 shared/placement(go/db 按同一份契约读),脚本里不拼任何格式。
		keys = append(keys, placement.Key(playerID))
		args = append(args, placement.StableValue(storageID, 1))
	}
	res, err := r.mappingRedis.EvalCtx(ctx, setPlayerZoneIfAbsentScript, keys, args...)
	if err != nil {
		return fmt.Errorf("register zone mapping for player %d: %w", playerID, err)
	}
	// 写成功时脚本回整数(1 或 zoneRegisteredPlacementKept);冲突时回既有值(字符串)。
	// 分开 SETNX+GET 会在两条命令之间被删除/改写,拿到的"既有值"就不是拒绝写入的那一个。
	switch existing := res.(type) {
	case int64:
		if existing == zoneRegisteredPlacementKept {
			// home 是这次新写的,落点记录却早已存在(例如映射曾被 DeletePlayerZone 删掉、
			// 落点留着)。不覆盖:落点记录指向的是这名玩家数据**真正所在**的库(P-1),
			// 用登记请求里的 storage_id 盖掉它会让之后的读写全部去错库。登记本身算成功,
			// 留 ERROR 日志让人核对这条旧记录是否符合预期。
			logx.Errorf("[RegisterPlayerZone] placement record already present, kept as is: player=%d zone=%d requested_storage=%d key=%s",
				playerID, homeZoneID, storageID, placement.Key(playerID))
		}
		return nil
	case string:
		if existing == val {
			return nil // 幂等重试:值一样就当登记成功
		}
		existingZone, parseErr := strconv.ParseUint(existing, 10, 32)
		if parseErr != nil {
			// 值坏了同样不许覆盖:这时更需要人来看一眼,而不是让一次写入把证据抹掉。
			return fmt.Errorf("player %d has an unparsable zone mapping %q, refusing to overwrite with %d",
				playerID, existing, homeZoneID)
		}
		return &HomeZoneConflictError{PlayerID: playerID, Existing: uint32(existingZone), Requested: homeZoneID}
	default:
		return fmt.Errorf("unexpected setPlayerZoneIfAbsent reply for player %d: %T %v", playerID, res, res)
	}
}

// setPlayerZoneIfAbsentScript:KEYS[1] = player:zone:{id}。player:zone 不存在则写入并回 1,
// 已存在则原子地回既有值。单条脚本保证"没写成"和"既有值是什么"看到的是同一瞬间的状态。
//
// 可选的第二个键 KEYS[2] = player:placement:{id}、ARGV[2] = 稳定态落点值:只在
// player:zone 这次写成之后 SETNX;落点已存在则不覆盖并回 zoneRegisteredPlacementKept。
// 只传一个键时 KEYS[2] 为 nil,不碰落点记录。
const setPlayerZoneIfAbsentScript = `
if redis.call('SET', KEYS[1], ARGV[1], 'NX') then
	if KEYS[2] and not redis.call('SET', KEYS[2], ARGV[2], 'NX') then
		return 2
	end
	return 1
end
return redis.call('GET', KEYS[1])`

// zoneRegisteredPlacementKept 是 setPlayerZoneIfAbsentScript 的返回值:home 写成,
// 但落点记录已存在、未被覆盖。与脚本里的字面量 2 对应。
const zoneRegisteredPlacementKept int64 = 2

// ErrHomeZoneNotMapped 表示 mapping Redis 里确实没有这名玩家的 player:zone:{id}
// (存量玩家在 CreatePlayer 注册映射上线之前建号、或注册当时失败),**不是** Redis
// 故障。调用方用 errors.Is 区分:gRPC 层把它翻成 codes.NotFound(消息保留
// "no home zone mapping" 文案),让 login 回退到账号 blob 里的建角 zone;
// mapping Redis 真出故障时是 codes.Unavailable,两者必须分得开。
//
// 文案是**契约的一部分**:login 的 homezone.IsUnmapped 除了认 NotFound,还认
// 老版本的 Unknown + 这段文案(滚动升级期间两种服务端并存)。改文案 = 让老客户端
// 把"没有映射"当成故障,登录整体失败。
var ErrHomeZoneNotMapped = errors.New("no home zone mapping")

// GetPlayerHomeZone looks up a player's home zone. 返回原始值,没有解析合服映射(解析在 RPC 出口做)。
// 映射缺席时返回包着 ErrHomeZoneNotMapped 的错误(errors.Is 可判);
//
// RouteStore.ReadRequiresReady=true 时整段换成 getPlayerHomeZoneReady(route_read.go,R6):缓存可信才用缓存,
// 否则回源 player_route;库故障包 ErrRouteReadUnavailable,绝不包 ErrHomeZoneNotMapped。开关为 false 时下面与今天逐字相同。
func (r *Router) GetPlayerHomeZone(ctx context.Context, playerID uint64) (uint32, error) {
	if r.readRequiresReady {
		return r.getPlayerHomeZoneReady(ctx, playerID)
	}
	val, err := r.mappingRedis.GetCtx(ctx, mappingKey(playerID))
	if err != nil {
		return 0, fmt.Errorf("mapping lookup failed for player %d: %w", playerID, err)
	}
	return parseHomeZoneValue(playerID, val)
}

// parseHomeZoneValue 把 player:zone:{id} 的原始值翻成 zone。空串 = 键不存在。
// 单查、批量与就绪读共用,几条读路径对「缺席」「畸形」的口径必须一致。
func parseHomeZoneValue(playerID uint64, val string) (uint32, error) {
	if val == "" {
		return 0, fmt.Errorf("%w for player %d", ErrHomeZoneNotMapped, playerID)
	}
	zoneID, err := strconv.ParseUint(val, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid zone mapping for player %d: %s", playerID, val)
	}
	return uint32(zoneID), nil
}

// BatchGetPlayerHomeZone returns home zone for multiple players.
//
// 缺席的 id 不出现在结果里(不是错误,见 dataserviceserver.go 的 BatchGetPlayerHomeZone 注释);
// 值畸形时整批返回错误,与单查 parseHomeZoneValue 同一口径(2026-10-12,
// player-route-mapping-durability-proposal.md §5.2 / §9 第 0 批)。此前畸形值被静默跳过,
// 调用方会把一条坏数据当成「没有映射」,login 角色列表据此退回建角 zone、friend 显示 zone 0,
// 坏数据本身无人发现。映射只由本仓库代码写入,畸形就是故障,fail-closed。
//
// RouteStore.ReadRequiresReady=true 时整段换成 batchGetPlayerHomeZoneReady(route_read.go,R3):按 store.RouteBatchLimit
// 分块,每块一段读脚本(就绪标记 + 身份 + MGET),可信的块用缓存,不可信的块按 GetRoutes 回源;任一块回源失败整批报错
// (包 ErrRouteReadUnavailable)。开关为 false 时下面与今天逐字相同。
func (r *Router) BatchGetPlayerHomeZone(ctx context.Context, playerIDs []uint64) (map[uint64]uint32, error) {
	if r.readRequiresReady {
		return r.batchGetPlayerHomeZoneReady(ctx, playerIDs)
	}
	result := make(map[uint64]uint32, len(playerIDs))
	// Use pipeline for efficiency
	keys := make([]string, len(playerIDs))
	for i, pid := range playerIDs {
		keys[i] = mappingKey(pid)
	}

	vals, err := r.mappingRedis.MgetCtx(ctx, keys...)
	if err != nil {
		return nil, fmt.Errorf("batch mapping lookup failed: %w", err)
	}

	for i, val := range vals {
		if val == "" {
			continue
		}
		zoneID, err := parseHomeZoneValue(playerIDs[i], val)
		if err != nil {
			return nil, fmt.Errorf("batch mapping lookup: %w", err)
		}
		result[playerIDs[i]] = zoneID
	}
	return result, nil
}

// Close shuts down all Redis connections.
func (r *Router) Close() { _ = r.CloseAndReport() }

// CloseAndReport 只报告资源关闭；调用者必须先关闭接纳并 join 发布者，不能把 Close 当作发布终局证明。
// 同一 Router 只关闭一次，并永久保留第一次错误；mappingRedis 是共享客户端，不归这里关闭。
func (r *Router) CloseAndReport() error {
	r.closeOnce.Do(func() {
		if r.publishRedis == nil {
			return
		}
		if err := r.publishRedis.Close(); err != nil {
			r.closeErr = fmt.Errorf("关闭 Router Redis 客户端: %w", err)
		}
	})
	return r.closeErr
}
