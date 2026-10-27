package routing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"data_service/internal/config"
	"data_service/internal/store"

	"shared/placement"

	goredis "github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

// Router resolves player_id → home_zone_id → Redis client.
// It is the ONLY component that knows about cross-realm topology.
type Router struct {
	closeOnce    sync.Once
	closeErr     error
	mappingRedis *redis.Redis                      // global mapping: player_id → home_zone_id
	zoneToClient map[uint32]*goredis.ClusterClient // zone_id → Redis Cluster
	devClient    *goredis.Client                   // non-nil in dev mode (single Redis for all)
	mu           sync.RWMutex
	lockTTLSec   int

	// routeMode 是 RegisterPlayerZone 的提交点(config.RouteStoreModeRedis | RouteStoreModeDBFirst),
	// routeStore 与 publish 只在 db_first 下使用(见 route_publish.go)。routeStore 为 nil 时 db_first 一律 fail-closed。
	routeMode  string
	routeStore RouteStore
	publish    publishConfig
	// publishRedis 是 db_first 下 W1 发布 + WAITAOF 专用的 go-redis 客户端(publish_redis.go):与 mappingRedis 同一个实例,
	// 但独立连接池、不经 go-zero 熔断器。只在 db_first 时由 NewRouter 构造(单测可用 withPublishRedis 注入);redis 模式恒为 nil。
	publishRedis *goredis.Client

	// 第 3 批读方门禁(route_read.go,规格 spec_D1_batch1b2_2_3.md §5.2):readRequiresReady 取 RouteStore.ReadRequiresReady,
	// 打开时 R2 / R3 / R6 先用就绪读脚本判副本可不可信,不可信回源 routeReader;R7 在 db_first 下一律读 routeReader。
	// routeReader 为 nil 时回源一律 fail-closed(ErrRouteReadUnavailable)。routeReadEval 是读脚本的执行入口
	// (缺省 mappingRedis.EvalCtx),只为单测能绕开 INFO(miniredis 不支持 INFO server / replication)。
	readRequiresReady bool
	routeReader       RouteReader
	routeReadEval     routeReadEvalFunc
}

// RouteReader 是第 3 批读方的回源读(规格 §5.2、§13.2 第 5 条):缓存不可信时按库读 home / 围栏 / 批量 home,
// db_first 下按 zone 枚举玩家。与写入点 RouteStore(route_publish.go)分成两个小接口(ISP):路由器的读路径只依赖它。
// *store.RouteStore 同时满足两者,svc 装配时同一个实例两边都注入。三个方法都是事务外一致读,契约见 store/route_reads.go
// 与 store.RouteStore.GetRoutes;单测注入假实现。
type RouteReader interface {
	GetHomeAndFence(ctx context.Context, playerID uint64) (store.RouteHomeFence, error)
	GetRoutes(ctx context.Context, ids []uint64) (map[uint64]store.RouteRow, error)
	ScanRoutesByHome(ctx context.Context, zone uint32, after store.RouteScanCursor, limit int) ([]store.RouteRow, error)
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
	if c.DevRedis.PoolSize < 0 {
		panic(fmt.Sprintf("data_service router: DevRedis.PoolSize=%d must be >= 0 (0 = go-redis default 10 x GOMAXPROCS)", c.DevRedis.PoolSize))
	}
	r := &Router{
		mappingRedis: redis.MustNewRedis(c.MappingRedis),
		zoneToClient: make(map[uint32]*goredis.ClusterClient),
		lockTTLSec:   c.PlayerLockTTLSec,
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

	// Dev mode: single Redis for all zones
	if c.DevRedis.Host != "" {
		r.devClient = goredis.NewClient(&goredis.Options{
			Addr:     c.DevRedis.Host,
			Password: c.DevRedis.Password,
			DB:       c.DevRedis.DB,
			// 共享 Redis 连接总账按它记账(0 = go-redis 缺省 10 × GOMAXPROCS),见 config.DevRedisConfig.PoolSize。
			PoolSize: c.DevRedis.PoolSize,
		})
		// pool_size 是生效值(缺省时是 go-redis 算出来的 10 × GOMAXPROCS):核对 ConfigMap 镜像的 PoolSize 是否真的被加载。
		logx.Infof("[Router] Dev mode: all zones → %s DB=%d pool_size=%d", c.DevRedis.Host, c.DevRedis.DB, r.devClient.Options().PoolSize)
		return r
	}

	// Production: build zone → cluster mapping
	if err := config.ValidateRegions(c.Regions); err != nil {
		panic(fmt.Sprintf("data_service router: invalid Regions config: %v", err))
	}
	for _, region := range c.Regions {
		client := goredis.NewClusterClient(&goredis.ClusterOptions{
			Addrs:    region.Redis.Addrs,
			Password: region.Redis.Password,
		})
		for _, zoneID := range region.Zones {
			r.zoneToClient[zoneID] = client
		}
		logx.Infof("[Router] Region %d: zones %v → cluster %v", region.Id, region.Zones, region.Redis.Addrs)
	}

	return r
}

// ── Player-zone mapping ────────────────────────────────────────

const mappingKeyPrefix = "player:zone:"

func mappingKey(playerID uint64) string {
	return mappingKeyPrefix + strconv.FormatUint(playerID, 10)
}

// ── 合服闸门(merge:in_progress:{zone})──────────────────────────
//
// 契约(与 tools/merge_zone 共享,改一处必须同步另一处):
//
//	Redis  : data_service 的 **mapping Redis**(config.MappingRedis,即
//	         player:zone:{id} 所在的那个实例/DB) —— 闸门必须和它守护的键同库,
//	         否则"检查"和"写入"可能落在两套连通性不同的实例上,闸门就成了摆设。
//	Key    : merge:in_progress:{zone}   zone = 十进制 zone id,无 hash tag
//	Value  : JSON,{"started_at": <RFC3339 UTC>, "started_at_unix_ms": <int>, ...}
//	         —— **本服务从不解析**,只看键在不在
//	TTL / 谁写 / 谁清除:按 merge_zone 的映射来源分两种(2026-10-20 更正,原文只写了 redis 源一种):
//	  - redis 源(-route-source=redis,data_service 全部是 Mode=redis):本键就是围栏真源。TTL 由工具设定,须长于整次合服,
//	    仅作进程被杀时的兜底;tools/merge_zone 跑完(成功或失败退出)显式 DEL,TTL 到期是兜底。
//	  - db 源(-route-source=db,第 2 批起,data_service 为 db_first):真源是全局库 zone_merge_fence,本键只是**镜像**:
//	    值 = 库里的 holder_json(带 fence_epoch),由 merge_zone 与对账 leader 按 fence_epoch set-if-newer 写入
//	    (placement.MergeFenceMirrorSetScript),PEXPIREAT = 库 expires_at_ms + placement.MergeFenceMirrorMarginMs;
//	    释放时先清库、再按完整值 compare-and-delete 本键,删不掉由 leader 按库双向对账删除。运维不手工 SET / DEL(runbook §15)。
//	    db_first 的 W1 另在库内锁围栏分片复核(route_publish.go),本键是发布脚本 KEYS[4] 的第二道复核。
//	判据   : 两种来源相同 —— **键存在即封锁**,不看值、不看剩余 TTL
//
// 为什么判据只有"存在":任何更复杂的判据(解析 started_at、比较时间窗)都会引入
// "值坏了/时钟歪了怎么办"的分支,而闸门唯一正确的失败方向是拒绝。存在性检查
// 没有可失败的解析步骤,Redis 报错时也只有一种处理:当成封锁(fail-closed)。

const mergeFenceKeyPrefix = "merge:in_progress:"

// MergeFenceKey 返回某个 zone 的合服标记键。导出是为了让合服工具 / 单测
// 引用同一份拼法,而不是各写一遍字符串。与 shared/placement.MergeFenceKey 必须逐字节相同
// (go/db、搬库工具用那一份),router_test.go 钉住两者相等;前缀常量留在本包是因为
// getHomeZoneAndMergeFenceScript 要把前缀传进 Lua,而 placement 只导出了拼好的键。
func MergeFenceKey(zoneID uint32) string {
	return mergeFenceKeyPrefix + strconv.FormatUint(uint64(zoneID), 10)
}

// IsMergeInProgress 报告某个 zone 是否正处在合服维护窗口。
// Redis 报错时返回 (false, err);调用方必须把 err 当成"封锁"处理(fail-closed),
// 绝不能因为查不到标记就放行。
func (r *Router) IsMergeInProgress(ctx context.Context, zoneID uint32) (bool, error) {
	if zoneID == 0 {
		return false, nil
	}
	return r.mappingRedis.ExistsCtx(ctx, MergeFenceKey(zoneID))
}

// ErrZoneMergeInProgress 表示目标 zone 有 merge:in_progress:{zone} 标记,
// 本次写入被合服闸门拒绝。用 errors.Is 判定。
// RegisterPlayerZone 与玩家数据写门禁 AcquirePlayerWriteLock 共用;写门禁里拿锁后发现 home 已被改写同样包它。
var ErrZoneMergeInProgress = errors.New("zone merge in progress")

// HomeZoneConflictError 表示 player:zone:{id} 已经存在且与请求的 zone 不同。
// 带上 Existing 是为了让调用方(和日志)一眼看出玩家现在归属哪里 —— 合服之后
// 这个值就是判断"谁在试图把玩家写回源区"的唯一线索。
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
// 另外,目标 zone 处在合服窗口(merge:in_progress:{zone})时整体拒绝,返回
// ErrZoneMergeInProgress。围栏查两次:写入前单独 EXISTS 预检(查询失败当封锁),
// 提交点再在写入脚本里原子复核(AGENTS.md §11.3「预检 + 提交点原子复核」)。
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
// 为什么不能覆盖:见 constants.ErrCodeZoneMappingConflict 的注释 —— 合服把映射
// 改写到目标 zone 之后,任何一次"按建角 zone 重新登记"都会静默把玩家送回已下线
// 的源区。原实现是无条件 SET,合服与一次 CreatePlayer 重试撞上就是这个后果。
//
// RouteStore.Mode=db_first 时整段换成 registerPlayerZoneDBFirst(route_publish.go):提交点改在全局库
// player_route,Redis 只是发布出去的副本(docs/design/player-route-mapping-durability-proposal.md §4)。
// 上面三种结果与合服闸门的语义不变,另有几种新的失败(库故障、发布未确认),见那个函数的注释。
// 下面的 redis 分支与 2026-10-12 之前逐字相同。
func (r *Router) RegisterPlayerZone(ctx context.Context, playerID uint64, homeZoneID, storageID uint32) error {
	if r.routeMode == config.RouteStoreModeDBFirst {
		return r.registerPlayerZoneDBFirst(ctx, playerID, homeZoneID, storageID)
	}
	// 先查闸门:合服窗口内连"新建"也不许,否则新写入的映射会跑在 remap 扫描
	// 的游标后面,合服跑完它仍指向源 zone。查询失败按封锁处理。
	merging, err := r.IsMergeInProgress(ctx, homeZoneID)
	if err != nil {
		return fmt.Errorf("merge fence check for zone %d failed (treated as fenced): %w", homeZoneID, err)
	}
	if merging {
		return fmt.Errorf("%w: zone %d (player %d)", ErrZoneMergeInProgress, homeZoneID, playerID)
	}
	return r.registerPlayerZoneFenced(ctx, playerID, homeZoneID, storageID)
}

// registerPlayerZoneFenced 是 RegisterPlayerZone 的提交点:一段 Lua 里先复核合服围栏,
// 再 SETNX player:zone(及可选的落点记录)。
//
// 为什么预检之外还要在脚本里复核:预检的 EXISTS 与写入之间,merge_zone 可能立起围栏并
// 按「先立围栏再收集」收完清单;此后才落下的 player:zone=src 不在清单里,按 CAS 改映射时
// 不会被改到,合服后仍指向已下线的源区,落点记录也不在任何清单里(§4.4 的按清单重放兜不住)。
// 围栏与写入在同一段脚本里,两者看到的是同一瞬间的状态。
//
// 单独成函数只为让单测能绕过预检、直接打到提交点的复核;业务调用一律走 RegisterPlayerZone。
func (r *Router) registerPlayerZoneFenced(ctx context.Context, playerID uint64, homeZoneID, storageID uint32) error {
	val := strconv.FormatUint(uint64(homeZoneID), 10)
	keys := []string{mappingKey(playerID), MergeFenceKey(homeZoneID)}
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
	// 写成功时脚本回整数(1 或 zoneRegisteredPlacementKept);围栏命中回 zoneRegisterFenced;
	// 冲突时回既有值(字符串)。
	// 分开 SETNX+GET 会在两条命令之间被删除/改写,拿到的"既有值"就不是拒绝写入的那一个。
	switch existing := res.(type) {
	case int64:
		if existing == zoneRegisterFenced {
			return fmt.Errorf("%w: zone %d (player %d)", ErrZoneMergeInProgress, homeZoneID, playerID)
		}
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

// setPlayerZoneIfAbsentScript:KEYS[1] = player:zone:{id}、KEYS[2] = merge:in_progress:{home}。
// 围栏存在则任何键都不写、回 zoneRegisterFenced;否则 player:zone 不存在则写入并回 1,
// 已存在则原子地回既有值。单条脚本保证"围栏状态""没写成"和"既有值是什么"看到的是同一瞬间的状态。
//
// 可选的第三个键 KEYS[3] = player:placement:{id}、ARGV[2] = 稳定态落点值:只在
// player:zone 这次写成之后 SETNX;落点已存在则不覆盖并回 zoneRegisteredPlacementKept。
// 只传两个键时 KEYS[3] 为 nil,不碰落点记录。
const setPlayerZoneIfAbsentScript = `
if redis.call('EXISTS', KEYS[2]) == 1 then
	return 3
end
if redis.call('SET', KEYS[1], ARGV[1], 'NX') then
	if KEYS[3] and not redis.call('SET', KEYS[3], ARGV[2], 'NX') then
		return 2
	end
	return 1
end
return redis.call('GET', KEYS[1])`

// zoneRegisteredPlacementKept 是 setPlayerZoneIfAbsentScript 的返回值:home 写成,
// 但落点记录已存在、未被覆盖。与脚本里的字面量 2 对应。
const zoneRegisteredPlacementKept int64 = 2

// zoneRegisterFenced 是 setPlayerZoneIfAbsentScript 的返回值:提交点复核时 home_zone
// 已处在合服围栏内,两键都没写。与脚本里的字面量 3 对应。
const zoneRegisterFenced int64 = 3

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

// GetPlayerHomeZone looks up a player's home zone.
// 映射缺席时返回包着 ErrHomeZoneNotMapped 的错误(errors.Is 可判);
// ClientForPlayer 等内部路由调用把它和 Redis 错误一视同仁(没有映射就无法路由):in-band 数据入口一律回
// ErrCodeRedis(故障),原因记进 logic 包的 [DataRoute] 日志。
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
// GetPlayerHomeZone 与 GetPlayerHomeZoneAndMergeFence 共用,两条读路径对「缺席」「畸形」的口径必须一致。
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

// GetPlayerHomeZoneAndMergeFence 同 GetPlayerHomeZone,另外报告 home_zone 此刻是否处在
// 合服围栏内(merge:in_progress:{home} 存在),供 GetPlayerHomeZone RPC 的 home_zone_merging
// (player-storage-placement.md §8.1 / §12 A16)。
//
// 为什么要在同一段 Lua 里读:围栏键由 home 值决定,分两次读时,先读到的 home 可能是合服中
// 的源区,而两次之间合服已经跑完(home 改到目标区、围栏撤掉)—— 调用方拿到「过期的源区 home +
// 没有围栏」的组合,放行进场后存盘写进已排空的源 topic,正是 A16 要堵的洞。一段脚本读出的
// home 与围栏状态属于同一瞬间。
//
// 错误口径:
//   - 映射缺席                 → 包着 ErrHomeZoneNotMapped(与 GetPlayerHomeZone 相同);
//   - 脚本整体失败(Redis 故障)→ 普通错误,调用方按 Unavailable 处理;
//   - home 读到了、围栏读失败   → merging=true、err=nil:与 IsMergeInProgress「查询失败当封锁」
//     同一口径(fail-closed)。home 本身是可信的,调用方照常拿到它,只是被告知不能进场。
//
// 只给决定能不能进场的调用方(GetPlayerHomeZone RPC)与玩家数据写门禁(AcquirePlayerWriteLock)用;
// 读路由(ClientForPlayer 等)只要 home,继续用 GetPlayerHomeZone,不多付一次 EXISTS。
//
// RouteStore.ReadRequiresReady=true 时整段换成 getPlayerHomeZoneAndMergeFenceReady(route_read.go,R2):就绪标记、
// 实例身份、home 与围栏同一段 Lua 读出,可信才用;否则回源 player_route(home 与 63 号片围栏同一条语句)。
// 库故障包 ErrRouteReadUnavailable(gRPC 回 Unavailable,绝不 NotFound)。开关为 false 时下面与今天逐字相同。
func (r *Router) GetPlayerHomeZoneAndMergeFence(ctx context.Context, playerID uint64) (zone uint32, merging bool, err error) {
	if r.readRequiresReady {
		return r.getPlayerHomeZoneAndMergeFenceReady(ctx, playerID)
	}
	res, err := r.mappingRedis.EvalCtx(ctx, getHomeZoneAndMergeFenceScript,
		[]string{mappingKey(playerID)}, mergeFenceKeyPrefix)
	if err != nil {
		return 0, false, fmt.Errorf("mapping lookup failed for player %d: %w", playerID, err)
	}
	return decodeHomeZoneAndMergeFenceReply(playerID, res)
}

// getHomeZoneAndMergeFenceScript 回 {home, fence}:home 缺席时为 {"", 0};
// fence 取 EXISTS 的结果(0/1),围栏读失败时为 mergeFenceUnreadable(-1)。
//
// 围栏键在脚本里由 ARGV[1](前缀,取自 mergeFenceKeyPrefix)拼上 home 值得到,不在 KEYS 里 ——
// 事先不知道 home 就没法声明它。今天 mapping Redis 是 go-zero 的 node 类型(恒 DB 0,见 router_integration_test.go),
// 不是 Cluster。将来若换成 Cluster,围栏键一般与映射键不同 slot;本脚本无 shebang,是 EVAL 兼容模式(带 SCRIPT_ALLOW_CROSS_SLOT):
// 围栏键的 slot 在本节点时**静默放行**,EXISTS 读到的就是本节点上的真围栏键;在别的节点时报 "non local key",
// 走 pcall 的失败分支(按合服处理 → 拒绝进场)。两种情形都不会把已立的围栏读成不存在,但「跨 slot 必报错」并不成立
// (data_service 侧的实测见 logic 包 TestRealRedisClusterUndeclaredKeys)。
const getHomeZoneAndMergeFenceScript = `
local home = redis.call('GET', KEYS[1])
if not home then
	return {'', 0}
end
local fenced = redis.pcall('EXISTS', ARGV[1] .. home)
if type(fenced) ~= 'number' then
	return {home, -1}
end
return {home, fenced}`

// mergeFenceUnreadable 与 getHomeZoneAndMergeFenceScript 里的字面量 -1 对应。
const mergeFenceUnreadable int64 = -1

// decodeHomeZoneAndMergeFenceReply 解读 getHomeZoneAndMergeFenceScript 的回复。
// 单独成函数是为了能直接测「围栏读失败」分支:miniredis 没法让脚本里的单条 EXISTS 失败。
func decodeHomeZoneAndMergeFenceReply(playerID uint64, res any) (uint32, bool, error) {
	reply, ok := res.([]any)
	if !ok || len(reply) != 2 {
		return 0, false, fmt.Errorf("unexpected home zone lookup reply for player %d: %T %v", playerID, res, res)
	}
	raw, rawOK := reply[0].(string)
	fence, fenceOK := reply[1].(int64)
	if !rawOK || !fenceOK {
		return 0, false, fmt.Errorf("unexpected home zone lookup reply for player %d: %v", playerID, reply)
	}
	zone, err := parseHomeZoneValue(playerID, raw)
	if err != nil {
		return 0, false, err
	}
	switch fence {
	case 0:
		return zone, false, nil
	case 1:
		return zone, true, nil
	default:
		// mergeFenceUnreadable,或任何意料之外的值:一律当封锁。放行的代价是往已排空的源 topic
		// 写存盘(数据丢失),拒绝的代价只是玩家稍后重试,失败方向只能是后者。
		logx.Errorf("[GetPlayerHomeZone] merge fence unreadable, treated as merging: player=%d zone=%d fence_reply=%d key=%s",
			playerID, zone, fence, MergeFenceKey(zone))
		return zone, true, nil
	}
}

// DeletePlayerZone removes the player → home_zone mapping.
//
// **生产路径已停用**(2026-10-12,docs/design/player-route-mapping-durability-proposal.md §4.3 的 W4):
// 只剩单测与排障用。db_first 下真源在全局库 player_route,只删 Redis 副本等于造一个「库有、缓存无」的分叉
// (下一次 W1 会把它重新发布回来);删号与落点的关系待删号设计定下来再处理(规格 Q6)。
func (r *Router) DeletePlayerZone(ctx context.Context, playerID uint64) error {
	_, err := r.mappingRedis.DelCtx(ctx, mappingKey(playerID))
	return err
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

// ── Redis client resolution ────────────────────────────────────

// ClientForPlayer resolves player_id → the correct Redis Cmdable.
// 读路径用;Save / SetField / Delete 走 AcquirePlayerWriteLock(带合服围栏预检与拿锁后复核)。
//
// 错误链原样保留,调用方用 errors.Is 区分:映射缺席 → ErrHomeZoneNotMapped;回源失败 → ErrRouteReadUnavailable;
// mapping Redis 故障 / 熔断 / 值畸形 / zone 未配集群 → 普通错误。in-band 入口(logic.clientForPlayer)一律折成
// ErrCodeRedis 并记 [DataRoute] 日志;账本读(persisted_player_database.go、asset_sql_commit.go)一律回 Unavailable,
// **绝不** NotFound。DevRedis 下第一行短路:不查映射、不会失败。
func (r *Router) ClientForPlayer(ctx context.Context, playerID uint64) (goredis.Cmdable, error) {
	if r.devClient != nil {
		return r.devClient, nil
	}

	zoneID, err := r.GetPlayerHomeZone(ctx, playerID)
	if err != nil {
		return nil, err
	}

	return r.ClientForZone(zoneID)
}

// ClientForZone returns the Redis client for a specific zone.
func (r *Router) ClientForZone(zoneID uint32) (goredis.Cmdable, error) {
	if r.devClient != nil {
		return r.devClient, nil
	}

	r.mu.RLock()
	client, ok := r.zoneToClient[zoneID]
	r.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("no Redis cluster configured for zone %d", zoneID)
	}
	return client, nil
}

// AllZoneIDs returns all configured zone IDs (for server-wide operations).
func (r *Router) AllZoneIDs() []uint32 {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ids := make([]uint32, 0, len(r.zoneToClient))
	for zoneID := range r.zoneToClient {
		ids = append(ids, zoneID)
	}
	return ids
}

// GetAllPlayerIDsInZone scans the global mapping Redis to find all players
// whose home zone matches the given zoneID. This is O(N) over all players
// and should only be used for admin operations (e.g., zone rollback).
//
// 单键 GET 失败时整体返回错误(2026-10-12,player-route-mapping-durability-proposal.md §9 第 0 批)。
// 此前错误被吞掉、那名玩家被当成「不属于本区」直接漏掉,调用方(回档的孤儿号报告、debug_fetch)
// 拿到的是一份静默缺人的名单。枚举结果用于运维决策,宁可失败也不能少人。
//
// RouteStore.Mode=db_first 时一律读库(allPlayerIDsInZoneFromDB,route_read.go,R7;与 ReadRequiresReady 无关):
// 那时真源是 player_route,Redis 副本缺键不能当成「不属于本区」。redis 模式保持下面的 SCAN(那时 Redis 才是真源)。
func (r *Router) GetAllPlayerIDsInZone(ctx context.Context, zoneID uint32) ([]uint64, error) {
	if r.routeMode == config.RouteStoreModeDBFirst {
		return r.allPlayerIDsInZoneFromDB(ctx, zoneID)
	}
	target := strconv.FormatUint(uint64(zoneID), 10)
	var playerIDs []uint64
	var cursor uint64

	for {
		keys, nextCursor, err := r.mappingRedis.ScanCtx(ctx, cursor, mappingKeyPrefix+"*", 500)
		if err != nil {
			return nil, fmt.Errorf("scan mapping keys: %w", err)
		}

		for _, key := range keys {
			val, err := r.mappingRedis.GetCtx(ctx, key)
			if err != nil {
				return nil, fmt.Errorf("get mapping key %s: %w", key, err)
			}
			if val != target {
				continue
			}
			// Extract player ID from key "player:zone:{ID}"
			idStr := key[len(mappingKeyPrefix):]
			pid, err := strconv.ParseUint(idStr, 10, 64)
			if err != nil {
				continue
			}
			playerIDs = append(playerIDs, pid)
		}

		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}

	return playerIDs, nil
}

// 【RemapHomeZoneForMerge 已删除】(2026-10-12,docs/design/player-route-mapping-durability-proposal.md §4.3 的 W2)
// 它对 SCAN 到的 player:zone:* 做无条件 SET(危害见 tools/merge_zone/merge_run.go 的 CAS 改写注释),
// 且只写 Redis、绕过 player_route 真源。仓库内没有调用方;gRPC 入口一律回 FailedPrecondition +
// ErrCodeNotImplemented(dataserviceserver.go),改映射只走 tools/merge_zone(第 2 批起库先写)。

// ── Player lock (distributed) ──────────────────────────────────

func mappingPlayerLockKey(playerID uint64) string {
	return "lock:player:" + strconv.FormatUint(playerID, 10)
}

// PlayerDataLockKey 返回和 player:{id}:* 数据位于同一 Redis Cluster slot 的锁键。
// data_service 的写/删 Lua 会在同一条脚本内校验该键的 token。
func PlayerDataLockKey(playerID uint64) string {
	return fmt.Sprintf("player:{%d}:__lock", playerID)
}

// AcquirePlayerLock attempts to acquire a per-player distributed lock in both
// the global mapping Redis and the player's data Redis.
// 成功时返回本次持锁身份 token,释放时必须原样回传。
//
// 双锁不是为了提高并发,而是为了跨两个 Redis 实例保持同一份所有权证明:
//   - mapping Redis 锁保护 player:zone:* 的删除;
//   - 玩家数据 Redis 锁与 player:{id}:* 同 slot,可被写/删 Lua 原子校验。
//
// 只做 token 校验释放还不够:旧持锁者在 TTL 后恢复执行时仍可能覆盖新持锁者。
// 数据侧 Lua 必须同时验证 PlayerDataLockKey 的 token,才能真正 fence 掉旧写者。
//
// 业务写路径不要直接调,走 AcquirePlayerWriteLock(合服围栏预检 + 拿锁后复核);本函数保留导出给锁语义单测。
func (r *Router) AcquirePlayerLock(ctx context.Context, dataClient goredis.Cmdable, playerID uint64) (token string, ok bool, err error) {
	buf := make([]byte, 16)
	if _, err = rand.Read(buf); err != nil {
		return "", false, err
	}
	token = hex.EncodeToString(buf)

	// 任一把锁没拿到,都把「可能已经落在服务端」的锁按 token 释放掉,再返回:
	//   - SETNX 报错不等于没写入:命令可能已在服务端执行、只是回包丢了;
	//   - 回 false 也不一定是别人持锁:客户端对断连自动重试时,第二次会撞上第一次已写入的自己的 token。
	// 这两种情况不释放,锁要挂到 PlayerLockTTLSec 过期,期间该玩家的写全部 LockConflict。
	// token 校验保证只删自己的,别人持有的锁不受影响;代价是锁冲突时多一次释放往返。
	// 请求 ctx 已超时 / 取消也照常释放(lockReleaseContext)。
	ok, err = r.mappingRedis.SetnxExCtx(ctx, mappingPlayerLockKey(playerID), token, r.lockTTLSec)
	if err != nil || !ok {
		return "", false, r.lockAcquireFailure(playerID, "mapping", err, r.releaseMappingLock(ctx, playerID, token))
	}

	ttl := time.Duration(r.lockTTLSec) * time.Second
	ok, err = dataClient.SetNX(ctx, PlayerDataLockKey(playerID), token, ttl).Result()
	if err != nil || !ok {
		return "", false, r.lockAcquireFailure(playerID, "data", err, r.ReleasePlayerLock(ctx, dataClient, playerID, token))
	}
	return token, true, nil
}

// lockAcquireFailure 决定拿锁失败时返回的错误,并记下回滚释放的失败:
//   - 拿锁本身的错误优先返回;
//   - 拿锁只是没抢到(acquireErr == nil)而回滚释放失败时返回释放错误:锁可能还挂在服务端,不能当成普通的锁冲突。
//
// 回滚释放失败一律记日志:之后最多 PlayerLockTTLSec 内该玩家的写都回 LockConflict,日志是唯一的解释
// (与 lockAndRecheckRoute 释放失败同一口径)。failedLock 只用于日志,标明哪把锁的 SETNX 没成功。
func (r *Router) lockAcquireFailure(playerID uint64, failedLock string, acquireErr, releaseErr error) error {
	if releaseErr != nil {
		logx.Errorf("[PlayerLock] rollback after a failed %s lock acquire could not release the locks, they expire in %ds: player=%d acquire_err=%v: %v",
			failedLock, r.lockTTLSec, playerID, acquireErr, releaseErr)
	}
	if acquireErr != nil {
		return acquireErr
	}
	if releaseErr != nil {
		return fmt.Errorf("player %d: releasing locks after a lock conflict failed: %w", playerID, releaseErr)
	}
	return nil
}

// writeRoute 是写门禁读到的路由快照,home 与合服围栏同一瞬间读出(GetPlayerHomeZoneAndMergeFence)。
//
// mapped=false 只在 dev 模式出现:那时数据位置不依赖映射,未登记的玩家照常可写,与引入写门禁之前一致。
// 它也不可能在任何合服清单里 —— 合服按 player:zone 收集清单,RegisterPlayerZone 受同一道围栏约束。
// Regions 模式下映射缺席照旧是错误(没有 home 就选不出集群)。
type writeRoute struct {
	home   uint32
	mapped bool
}

func (w writeRoute) String() string {
	if !w.mapped {
		return "unmapped"
	}
	return strconv.FormatUint(uint64(w.home), 10)
}

// readWriteRoute 读一次 writeRoute。home 处在合服围栏内时返回包 ErrZoneMergeInProgress 的错误;
// 其余错误原样包回(映射缺席包 ErrHomeZoneNotMapped,就绪读回源失败包 ErrRouteReadUnavailable)。
func (r *Router) readWriteRoute(ctx context.Context, playerID uint64) (writeRoute, error) {
	home, merging, err := r.GetPlayerHomeZoneAndMergeFence(ctx, playerID)
	if err != nil {
		if r.devClient != nil && errors.Is(err, ErrHomeZoneNotMapped) {
			return writeRoute{}, nil
		}
		return writeRoute{}, err
	}
	route := writeRoute{home: home, mapped: true}
	if merging {
		return route, fmt.Errorf("%w: player %d home zone %d is fenced", ErrZoneMergeInProgress, playerID, home)
	}
	return route, nil
}

// AcquirePlayerWriteLock 是 SavePlayerData / SetPlayerField / DeletePlayerData 唯一的拿锁入口(合服写门禁)。顺序固定:
//
//  1. 预检:readWriteRoute(home 与 merge:in_progress:{home} 同一段 Lua / 同一条 SQL 读出,A16);围栏在 → 不碰任何锁;
//  2. 按预检读到的 home 取数据客户端(dev 模式恒为 devClient);
//  3. AcquirePlayerLock 拿两把锁;
//  4. 复核:**数据锁 SETNX 成功之后**再读一次 writeRoute。围栏已立 / home 已变 / 读失败 → 释放两把锁再返回错误。
//
// 为什么复核必须晚于数据锁:提交脚本(saveFieldsScript / 删除脚本)校验的是数据锁 token,merge_zone 拷 blob 前检查的
// 也是数据锁(tools/merge_zone/player_blob_migrate.go copyPlayerStringKeys)。复核晚于数据锁,则任何在立围栏之后才
// 拿到数据锁的写者,复核必然看见围栏;立围栏之前拿到锁的写者,要么在锁有效期内提交(此时锁在,合服拷贝中止),要么锁
// 过期后提交被 token 校验拒绝。复核若早于数据锁,写者可以在 merge_zone 查锁之后才拿到数据锁、在拷贝之后提交,数据留在
// 已拷走的源集群里。mapping Redis 与 data Redis 是两个实例,做不到一段 Lua 原子,窗口靠「数据锁 + 复核 + 拷贝前查锁」合起来关。
//
// 返回:err != nil → 不持有任何锁(围栏 / home 变化包 ErrZoneMergeInProgress;Regions 下映射缺席包 ErrHomeZoneNotMapped;
// 就绪读回源失败包 ErrRouteReadUnavailable;其余是 Redis / 配置错误);err == nil && !locked → 锁被别人持有;
// locked → 两把锁都在,调用方必须 defer ReleasePlayerLock(ctx, client, playerID, token)。
// 合服窗口内 src / dst 两个 zone 的玩家写入一律拒绝,dev 与 Regions 同口径。读路径不走这里,继续用 ClientForPlayer。
// 代价:每次写多两次 mapping Redis 往返(ReadRequiresReady=true 时是两次就绪读,缓存不可信时两次库点查),
// 复核耗时计入 PlayerLockTTLSec;慢到锁过期时提交被 token 校验拒成 LockConflict,只损活性。
func (r *Router) AcquirePlayerWriteLock(ctx context.Context, playerID uint64) (client goredis.Cmdable, token string, locked bool, err error) {
	route, err := r.readWriteRoute(ctx, playerID)
	if err != nil {
		return nil, "", false, fmt.Errorf("write refused before lock: %w", err)
	}
	client, err = r.ClientForZone(route.home)
	if err != nil {
		return nil, "", false, fmt.Errorf("write lock for player %d: %w", playerID, err)
	}
	token, locked, err = r.lockAndRecheckRoute(ctx, client, playerID, route)
	if err != nil || !locked {
		return nil, "", false, err
	}
	return client, token, true, nil
}

// lockAndRecheckRoute 拿两把玩家锁,数据锁到手之后复核 writeRoute;复核不过就释放两把锁并返回错误。
// before 是拿锁所用客户端对应的路由(预检读到的值)。单独成函数只为让单测能传入在数据侧 SETNX 处注入动作的客户端,
// 钉住「复核晚于数据锁」这条顺序(同 registerPlayerZoneFenced 的接缝理由);业务一律走 AcquirePlayerWriteLock。
func (r *Router) lockAndRecheckRoute(ctx context.Context, client goredis.Cmdable, playerID uint64, before writeRoute) (string, bool, error) {
	token, locked, err := r.AcquirePlayerLock(ctx, client, playerID)
	if err != nil || !locked {
		return "", locked, err
	}
	after, err := r.readWriteRoute(ctx, playerID)
	var refused error
	switch {
	case err != nil:
		refused = fmt.Errorf("write refused after lock: %w", err)
	case after != before:
		// home 只会被合服 / 撤销合服改写(dev 下还有「刚登记」),一律按合服处理:零写入,调用方稍后重试。
		refused = fmt.Errorf("%w: write refused after lock: player %d home zone changed %s -> %s",
			ErrZoneMergeInProgress, playerID, before, after)
	default:
		return token, true, nil
	}
	// 释放走 lockReleaseContext,请求 ctx 已取消也照常发出;失败只记日志,两把锁按 TTL 过期。
	if releaseErr := r.ReleasePlayerLock(ctx, client, playerID, token); releaseErr != nil {
		logx.Errorf("[PlayerWriteLock] releasing locks after a refused recheck failed, they expire in %ds: player=%d: %v",
			r.lockTTLSec, playerID, releaseErr)
	}
	return "", false, refused
}

// releasePlayerLockScript 仅当锁仍属于该 token 时才删除(原子)。
const releasePlayerLockScript = `
if redis.call('GET', KEYS[1]) == ARGV[1] then
	return redis.call('DEL', KEYS[1])
end
return 0`

// playerLockReleaseTimeout 是释放**一份**锁副本的时间预算;两份各自计时,一份挂住不拖累另一份。
const playerLockReleaseTimeout = time.Second

// lockReleaseContext 给锁释放一个不随请求取消的 ctx。
//
// 释放发生在 defer 和获取失败的回滚里,这时请求 ctx 往往已经超时或被取消;go-redis 取连接前先看
// ctx.Done(),沿用它释放命令根本发不出去,两把锁只能等 PlayerLockTTLSec 过期,期间同一玩家的写
// 全部回 LockConflict。WithoutCancel 保留 ctx 里的值(trace 等),另套短超时,免得挂住的 Redis 卡住 defer。
//
// 1s 约束的是取连接、拨号与重试退避,不是单次读写:数据 / mapping 客户端都没开 ContextTimeoutEnabled,
// go-redis 的套接字读写截止只取 ReadTimeout / WriteTimeout(缺省 3s)。所以 Redis 挂住时释放一份副本最坏约 3s,
// 超过 1s 后不再重试。
func lockReleaseContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), playerLockReleaseTimeout)
}

// ReleasePlayerLock releases both lock copies if they are still owned by token.
// ctx 已取消 / 超时也照常释放(见 lockReleaseContext),调用方直接传请求 ctx 即可。
func (r *Router) ReleasePlayerLock(ctx context.Context, dataClient goredis.Cmdable, playerID uint64, token string) error {
	if token == "" {
		return nil
	}
	dataErr := releaseDataLock(ctx, dataClient, playerID, token)
	mappingErr := r.releaseMappingLock(ctx, playerID, token)
	if dataErr != nil {
		return dataErr
	}
	return mappingErr
}

func releaseDataLock(ctx context.Context, dataClient goredis.Cmdable, playerID uint64, token string) error {
	ctx, cancel := lockReleaseContext(ctx)
	defer cancel()
	return dataClient.Eval(ctx, releasePlayerLockScript, []string{PlayerDataLockKey(playerID)}, token).Err()
}

func (r *Router) releaseMappingLock(ctx context.Context, playerID uint64, token string) error {
	ctx, cancel := lockReleaseContext(ctx)
	defer cancel()
	_, err := r.mappingRedis.EvalCtx(ctx, releasePlayerLockScript, []string{mappingPlayerLockKey(playerID)}, token)
	return err
}

// deletePlayerZoneIfLockedScript 在 mapping Redis 内原子校验锁所有权并删除映射。
const deletePlayerZoneIfLockedScript = `
if redis.call('GET', KEYS[1]) ~= ARGV[1] then
	return -1
end
return redis.call('DEL', KEYS[2])`

// DeletePlayerZoneIfLocked 仅在 token 仍拥有 mapping 锁时删除玩家 zone 映射。
// owned=false 表示锁已过期或已被后来者接管；deleted=false 且 owned=true 表示
// 映射本来就不存在（幂等成功）。
//
// **生产路径已停用**(2026-10-12,docs/design/player-route-mapping-durability-proposal.md §4.3 的 W3):
// 唯一的调用方 DeletePlayerData 的 DeleteZoneMapping 分支已在 logic 入口零副作用拒绝
// (logic.ErrZoneMappingDeletionDisabled)—— 锁 token 在 Redis、真源删除在库里,两者无法原子。
// 函数留着给锁语义的单测用,删号设计定下来之前不要接回任何生产路径。
func (r *Router) DeletePlayerZoneIfLocked(ctx context.Context, playerID uint64, token string) (owned, deleted bool, err error) {
	if token == "" {
		return false, false, nil
	}
	res, err := r.mappingRedis.EvalCtx(ctx, deletePlayerZoneIfLockedScript,
		[]string{mappingPlayerLockKey(playerID), mappingKey(playerID)}, token)
	if err != nil {
		return false, false, err
	}
	value, ok := res.(int64)
	if !ok {
		return false, false, fmt.Errorf("unexpected deletePlayerZoneIfLocked reply: %T %v", res, res)
	}
	return value >= 0, value == 1, nil
}

// Close shuts down all Redis connections.
func (r *Router) Close() { _ = r.CloseAndReport() }

// CloseAndReport 只报告资源关闭；调用者必须先关闭接纳并 join 发布者，不能把 Close 当作发布终局证明。
// 同一 Router 只关闭一次，并永久保留第一次错误；mappingRedis 是共享客户端，不归这里关闭。
func (r *Router) CloseAndReport() error {
	r.closeOnce.Do(func() {
		r.mu.RLock()
		clients := []*goredis.Client{r.devClient, r.publishRedis}
		clusters := make([]*goredis.ClusterClient, 0, len(r.zoneToClient))
		for _, client := range r.zoneToClient {
			clusters = append(clusters, client)
		}
		r.mu.RUnlock()
		closed := map[*goredis.Client]bool{}
		for _, client := range clients {
			if client != nil && !closed[client] {
				closed[client] = true
				if err := client.Close(); err != nil {
					r.closeErr = errors.Join(r.closeErr, fmt.Errorf("关闭 Router Redis 客户端: %w", err))
				}
			}
		}
		closedClusters := map[*goredis.ClusterClient]bool{}
		for _, client := range clusters {
			if client != nil && !closedClusters[client] {
				closedClusters[client] = true
				if err := client.Close(); err != nil {
					r.closeErr = errors.Join(r.closeErr, fmt.Errorf("关闭 Router Redis Cluster 客户端: %w", err))
				}
			}
		}
	})
	return r.closeErr
}
