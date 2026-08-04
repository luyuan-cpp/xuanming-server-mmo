package routing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

// RegisterPlayerZone writes the player → home_zone mapping.
func (r *Router) RegisterPlayerZone(ctx context.Context, playerID uint64, homeZoneID uint32) error {
	return r.mappingRedis.SetCtx(ctx, mappingKey(playerID), strconv.FormatUint(uint64(homeZoneID), 10))
}

// GetPlayerHomeZone looks up a player's home zone.
func (r *Router) GetPlayerHomeZone(ctx context.Context, playerID uint64) (uint32, error) {
	val, err := r.mappingRedis.GetCtx(ctx, mappingKey(playerID))
	if err != nil {
		return 0, fmt.Errorf("mapping lookup failed for player %d: %w", playerID, err)
	}
	if val == "" {
		return 0, fmt.Errorf("no home zone mapping for player %d", playerID)
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

// RemapHomeZoneForMerge rewrites all player:zone:* keys whose value equals sourceZone
// to targetZone. O(N) over all player mapping keys; use only during a maintenance window.
// When dryRun is true, only counts matches (no SET); PlayersUpdated in response is 0 in that case.
func (r *Router) RemapHomeZoneForMerge(ctx context.Context, sourceZone, targetZone uint32, dryRun bool) (matched int, updated int, err error) {
	if sourceZone == 0 || targetZone == 0 {
		return 0, 0, fmt.Errorf("source and target zone must be non-zero")
	}
	if sourceZone == targetZone {
		return 0, 0, fmt.Errorf("source and target must differ")
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
