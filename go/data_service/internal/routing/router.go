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

	"shared/placement"

	goredis "github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

// Router resolves player_id → home_zone_id → Redis client.
// It is the ONLY component that knows about cross-realm topology.
type Router struct {
	mappingRedis *redis.Redis                      // global mapping: player_id → home_zone_id
	zoneToClient map[uint32]*goredis.ClusterClient // zone_id → Redis Cluster
	devClient    *goredis.Client                   // non-nil in dev mode (single Redis for all)
	mu           sync.RWMutex
	lockTTLSec   int
}

func NewRouter(c config.Config) *Router {
	r := &Router{
		mappingRedis: redis.MustNewRedis(c.MappingRedis),
		zoneToClient: make(map[uint32]*goredis.ClusterClient),
		lockTTLSec:   c.PlayerLockTTLSec,
	}

	// Dev mode: single Redis for all zones
	if c.DevRedis.Host != "" {
		r.devClient = goredis.NewClient(&goredis.Options{
			Addr:     c.DevRedis.Host,
			Password: c.DevRedis.Password,
			DB:       c.DevRedis.DB,
		})
		logx.Infof("[Router] Dev mode: all zones → %s DB=%d", c.DevRedis.Host, c.DevRedis.DB)
		return r
	}

	// Production: build zone → cluster mapping
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
//	TTL    : 由工具设定,须长于整次合服;仅作进程被杀时的兜底,不是正常清除手段
//	谁清除 : tools/merge_zone 跑完(成功或失败退出)显式 DEL;TTL 到期是兜底
//	判据   : **键存在即封锁**,不看值、不看剩余 TTL
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
func (r *Router) RegisterPlayerZone(ctx context.Context, playerID uint64, homeZoneID, storageID uint32) error {
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
// ClientForPlayer 等内部路由调用把它和 Redis 错误一视同仁(没有映射就无法路由)。
func (r *Router) GetPlayerHomeZone(ctx context.Context, playerID uint64) (uint32, error) {
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
// 只给「决定能不能进场」的调用方用;内部路由(ClientForPlayer 等)只要 home,继续用
// GetPlayerHomeZone,不多付一次 EXISTS。
func (r *Router) GetPlayerHomeZoneAndMergeFence(ctx context.Context, playerID uint64) (zone uint32, merging bool, err error) {
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
// 事先不知道 home 就没法声明它。这只在单实例上成立:mapping Redis 是 go-zero 的 node 类型
// (恒 DB 0,见 router_integration_test.go),不是 Cluster;将来若换成 Cluster,未声明键会跨 slot,
// 那时 EXISTS 报错,会走 pcall 的失败分支(按合服处理 → 拒绝进场),不会静默放行。
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
func (r *Router) DeletePlayerZone(ctx context.Context, playerID uint64) error {
	_, err := r.mappingRedis.DelCtx(ctx, mappingKey(playerID))
	return err
}

// BatchGetPlayerHomeZone returns home zone for multiple players.
func (r *Router) BatchGetPlayerHomeZone(ctx context.Context, playerIDs []uint64) (map[uint64]uint32, error) {
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
		zoneID, err := strconv.ParseUint(val, 10, 32)
		if err != nil {
			continue
		}
		result[playerIDs[i]] = uint32(zoneID)
	}
	return result, nil
}

// ── Redis client resolution ────────────────────────────────────

// ClientForPlayer resolves player_id → the correct Redis Cmdable.
// This is the key routing function — all data access goes through here.
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
func (r *Router) GetAllPlayerIDsInZone(ctx context.Context, zoneID uint32) ([]uint64, error) {
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
			if err != nil || val != target {
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

// ErrMergeFenceMissing 表示源 zone 没有 merge:in_progress:{source} 标记,
// RemapHomeZoneForMerge 拒绝执行。用 errors.Is 判定。
var ErrMergeFenceMissing = errors.New("merge fence marker absent for source zone")

// RemapHomeZoneForMerge rewrites all player:zone:* keys whose value equals sourceZone
// to targetZone. O(N) over all player mapping keys; use only during a maintenance window.
// When dryRun is true, only counts matches (no SET); PlayersUpdated in response is 0 in that case.
//
// 前置条件:源 zone 必须已经立起 merge:in_progress:{source} 标记(见本文件顶部的
// 闸门契约),否则返回 ErrMergeFenceMissing、零变更。这不是形式主义:
// 那个标记同时在挡住 RegisterPlayerZone,没有它就意味着源 zone 仍在接受新映射,
// 本函数的 SCAN 游标扫过之后新写进来的玩家会永远留在源 zone —— 合服跑完看起来
// 成功,却有一批漏网玩家登进一个已经下线的区。dry-run 也要求标记:
// dry-run 的数字要能作为 apply 的依据,在没有封锁的库上数出来的数就不作数。
func (r *Router) RemapHomeZoneForMerge(ctx context.Context, sourceZone, targetZone uint32, dryRun bool) (matched int, updated int, err error) {
	if sourceZone == 0 || targetZone == 0 {
		return 0, 0, fmt.Errorf("source and target zone must be non-zero")
	}
	if sourceZone == targetZone {
		return 0, 0, fmt.Errorf("source and target must differ")
	}
	fenced, err := r.IsMergeInProgress(ctx, sourceZone)
	if err != nil {
		return 0, 0, fmt.Errorf("merge fence check for source zone %d failed: %w", sourceZone, err)
	}
	if !fenced {
		return 0, 0, fmt.Errorf("%w: set %s before remapping", ErrMergeFenceMissing, MergeFenceKey(sourceZone))
	}
	sourceVal := strconv.FormatUint(uint64(sourceZone), 10)
	targetVal := strconv.FormatUint(uint64(targetZone), 10)

	var cursor uint64
	for {
		keys, nextCursor, e := r.mappingRedis.ScanCtx(ctx, cursor, mappingKeyPrefix+"*", 500)
		if e != nil {
			return matched, updated, fmt.Errorf("scan mapping keys: %w", e)
		}
		for _, key := range keys {
			val, e := r.mappingRedis.GetCtx(ctx, key)
			if e != nil {
				return matched, updated, fmt.Errorf("mapping get %q: %w", key, e)
			}
			if val == "" || val != sourceVal {
				continue
			}
			matched++
			if dryRun {
				continue
			}
			if e := r.mappingRedis.SetCtx(ctx, key, targetVal); e != nil {
				return matched, updated, fmt.Errorf("set %s: %w", key, e)
			}
			updated++
		}
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}
	return matched, updated, nil
}

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
func (r *Router) AcquirePlayerLock(ctx context.Context, dataClient goredis.Cmdable, playerID uint64) (token string, ok bool, err error) {
	buf := make([]byte, 16)
	if _, err = rand.Read(buf); err != nil {
		return "", false, err
	}
	token = hex.EncodeToString(buf)

	ok, err = r.mappingRedis.SetnxExCtx(ctx, mappingPlayerLockKey(playerID), token, r.lockTTLSec)
	if err != nil || !ok {
		return "", ok, err
	}

	ttl := time.Duration(r.lockTTLSec) * time.Second
	ok, err = dataClient.SetNX(ctx, PlayerDataLockKey(playerID), token, ttl).Result()
	if err != nil || !ok {
		// 第二把锁失败时必须释放第一把；token 校验保证不会误删后来者。
		_, releaseErr := r.mappingRedis.EvalCtx(ctx, releasePlayerLockScript,
			[]string{mappingPlayerLockKey(playerID)}, token)
		if err == nil && releaseErr != nil {
			err = releaseErr
		}
		return "", ok, err
	}
	return token, true, nil
}

// releasePlayerLockScript 仅当锁仍属于该 token 时才删除(原子)。
const releasePlayerLockScript = `
if redis.call('GET', KEYS[1]) == ARGV[1] then
	return redis.call('DEL', KEYS[1])
end
return 0`

// ReleasePlayerLock releases both lock copies if they are still owned by token.
func (r *Router) ReleasePlayerLock(ctx context.Context, dataClient goredis.Cmdable, playerID uint64, token string) error {
	if token == "" {
		return nil
	}
	_, dataErr := dataClient.Eval(ctx, releasePlayerLockScript,
		[]string{PlayerDataLockKey(playerID)}, token).Result()
	_, mappingErr := r.mappingRedis.EvalCtx(ctx, releasePlayerLockScript,
		[]string{mappingPlayerLockKey(playerID)}, token)
	if dataErr != nil {
		return dataErr
	}
	return mappingErr
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
func (r *Router) Close() {
	if r.devClient != nil {
		r.devClient.Close()
	}
	closed := make(map[*goredis.ClusterClient]bool)
	for _, client := range r.zoneToClient {
		if !closed[client] {
			client.Close()
			closed[client] = true
		}
	}
}
