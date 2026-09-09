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
// 引用同一份拼法,而不是各写一遍字符串。
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
// ErrZoneMergeInProgress。
//
// 为什么不能覆盖:见 constants.ErrCodeZoneMappingConflict 的注释 —— 合服把映射
// 改写到目标 zone 之后,任何一次"按建角 zone 重新登记"都会静默把玩家送回已下线
// 的源区。原实现是无条件 SET,合服与一次 CreatePlayer 重试撞上就是这个后果。
func (r *Router) RegisterPlayerZone(ctx context.Context, playerID uint64, homeZoneID uint32) error {
	// 先查闸门:合服窗口内连"新建"也不许,否则新写入的映射会跑在 remap 扫描
	// 的游标后面,合服跑完它仍指向源 zone。查询失败按封锁处理。
	merging, err := r.IsMergeInProgress(ctx, homeZoneID)
	if err != nil {
		return fmt.Errorf("merge fence check for zone %d failed (treated as fenced): %w", homeZoneID, err)
	}
	if merging {
		return fmt.Errorf("%w: zone %d (player %d)", ErrZoneMergeInProgress, homeZoneID, playerID)
	}

	val := strconv.FormatUint(uint64(homeZoneID), 10)
	res, err := r.mappingRedis.EvalCtx(ctx, setPlayerZoneIfAbsentScript, []string{mappingKey(playerID)}, val)
	if err != nil {
		return fmt.Errorf("register zone mapping for player %d: %w", playerID, err)
	}
	// 写成功时脚本回 1(整数);冲突时回既有值(字符串)。分开 SETNX+GET 会在两条
	// 命令之间被删除/改写,拿到的"既有值"就不是拒绝写入的那一个。
	switch existing := res.(type) {
	case int64:
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

// setPlayerZoneIfAbsentScript:不存在则写入并回 1,已存在则原子地回既有值。
// 单条脚本保证"没写成"和"既有值是什么"看到的是同一瞬间的状态。
const setPlayerZoneIfAbsentScript = `
if redis.call('SET', KEYS[1], ARGV[1], 'NX') then
	return 1
end
return redis.call('GET', KEYS[1])`

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
	if val == "" {
		return 0, fmt.Errorf("%w for player %d", ErrHomeZoneNotMapped, playerID)
	}
	zoneID, err := strconv.ParseUint(val, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid zone mapping for player %d: %s", playerID, val)
	}
	return uint32(zoneID), nil
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
