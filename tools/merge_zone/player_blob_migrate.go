package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"
)

// Key pattern must match data_service/internal/logic/data_logic.go (player:{%d}:field).
func playerDataKeyPattern(playerID uint64) string {
	return fmt.Sprintf("player:{%d}:*", playerID)
}

// collectPlayerIDsWithHomeZone scans mapping Redis for keys player:zone:{id} where value == zoneStr.
//
// 每批 SCAN 用一次 MGET 取值,而不是逐键 GET:mapping 里是**全服所有玩家**的
// 映射(不只源区),百万级 key 逐键 GET 就是百万次 RTT。SCAN COUNT=500 配
// 一次 MGET 把往返压到 1/500。mapping Redis 是单实例(data_service.yaml 的
// MappingRedis Type: node),MGET 跨 slot 的问题不存在。
//
// 返回值有序去重:清单要能 diff,分批边界要稳定。
func collectPlayerIDsWithHomeZone(ctx context.Context, rdb *redis.Client, zone uint32) ([]uint64, error) {
	if rdb == nil {
		return nil, errors.New("nil mapping redis handle")
	}
	want := strconv.FormatUint(uint64(zone), 10)
	var out []uint64
	var cur uint64
	for {
		keys, next, err := rdb.Scan(ctx, cur, playerZoneKeyPrefix+"*", mappingScanCount).Result()
		if err != nil {
			return nil, fmt.Errorf("scan %s*: %w", playerZoneKeyPrefix, err)
		}
		if len(keys) > 0 {
			vals, err := rdb.MGet(ctx, keys...).Result()
			if err != nil {
				return nil, fmt.Errorf("mapping mget (%d keys): %w", len(keys), err)
			}
			for i, key := range keys {
				s, ok := vals[i].(string)
				if !ok || s != want { // nil(扫描后被删)或值不匹配
					continue
				}
				pid, perr := strconv.ParseUint(strings.TrimPrefix(key, playerZoneKeyPrefix), 10, 64)
				if perr != nil {
					log.Printf("WARN: skip bad mapping key %q", key)
					continue
				}
				out = append(out, pid)
			}
		}
		cur = next
		if cur == 0 {
			break
		}
	}
	return sortedUint64(out), nil
}

// mappingScanCount 是 mapping Redis 上 SCAN 的批量。500 与 remapPlayerMapping
// 保持一致,便于两次扫描的耗时可比。
const mappingScanCount = 500

// newDataRedisClient returns a standalone client (one addr) or cluster client (multiple addrs).
// For Redis Cluster, DB is ignored (always 0).
func newDataRedisClient(addrs []string, password string, db int) redis.UniversalClient {
	if len(addrs) == 0 {
		return nil
	}
	if len(addrs) == 1 {
		return redis.NewClient(&redis.Options{
			Addr:     addrs[0],
			Password: password,
			DB:       db,
		})
	}
	return redis.NewClusterClient(&redis.ClusterOptions{
		Addrs:    addrs,
		Password: password,
	})
}

// playerDataLockKey 是 data_service 的 per-player 数据锁
// (go/data_service/internal/routing/router.go::PlayerDataLockKey)。
// 它与 player:{id}:* 同 slot,所以会被 SCAN 命中 —— 但**绝不能拷**:
// 目标端拷进去的是一把 **TTL=0 的锁**(SET 不带 EX),data_service 之后
// 对这名玩家的每一次写都会认为锁被别人持着,而它永远不会过期。
// 这名玩家从此在目标区彻底写不进数据,且没有任何日志说明原因。
func playerDataLockKey(playerID uint64) string {
	return fmt.Sprintf("player:{%d}:__lock", playerID)
}

// scanPlayerKeys 列出一名玩家在 src 上的全部 player:{id}:* 键。
//
// Cluster 修正:go-redis 的 ClusterClient.Scan(不带 key)只会打到**一个**
// 随机节点,漏掉其它分片上的键。player:{id}:* 用了 hash tag,同一名玩家的
// 键确实都在同一个 slot / 节点上 —— 但那是**哪个**节点,客户端在无 key 的
// SCAN 里并不知道。必须 ForEachMaster 扫全部主节点再合并。
func scanPlayerKeys(ctx context.Context, src redis.UniversalClient, playerID uint64) ([]string, error) {
	pat := playerDataKeyPattern(playerID)
	seen := map[string]struct{}{}
	var all []string
	collect := func(ctx context.Context, c *redis.Client) error {
		var cur uint64
		for {
			keys, next, err := c.Scan(ctx, cur, pat, 200).Result()
			if err != nil {
				return err
			}
			for _, k := range keys {
				if _, dup := seen[k]; dup {
					continue
				}
				seen[k] = struct{}{}
				all = append(all, k)
			}
			cur = next
			if cur == 0 {
				return nil
			}
		}
	}
	switch c := src.(type) {
	case *redis.ClusterClient:
		if err := c.ForEachMaster(ctx, collect); err != nil {
			return nil, fmt.Errorf("cluster scan player %d: %w", playerID, err)
		}
	case *redis.Client:
		if err := collect(ctx, c); err != nil {
			return nil, fmt.Errorf("scan player %d: %w", playerID, err)
		}
	default:
		return nil, fmt.Errorf("unsupported redis client type %T", src)
	}
	sort.Strings(all) // 让 dry-run 与 apply 的日志顺序稳定
	return all, nil
}

// copyPlayerStringKeys copies all string keys matching player:{id}:* from src to dst (SET with TTL 0).
// 锁键被显式排除,见 playerDataLockKey。
func copyPlayerStringKeys(ctx context.Context, src, dst redis.UniversalClient, playerID uint64, dryRun bool) (int, error) {
	scanned, err := scanPlayerKeys(ctx, src, playerID)
	if err != nil {
		return 0, err
	}
	lockKey := playerDataLockKey(playerID)
	all := make([]string, 0, len(scanned))
	for _, k := range scanned {
		if k == lockKey {
			log.Printf("player %d: skipping %s (copying a lock with no TTL would deadlock the player in the target zone)", playerID, k)
			continue
		}
		all = append(all, k)
	}
	if len(all) == 0 {
		return 0, nil
	}
	if dryRun {
		return len(all), nil
	}

	// Read values from source
	vals, err := src.MGet(ctx, all...).Result()
	if err != nil {
		return 0, fmt.Errorf("mget player %d: %w", playerID, err)
	}
	pipe := dst.Pipeline()
	written := 0
	for i, k := range all {
		v := vals[i]
		if v == nil {
			continue
		}
		s, ok := v.(string)
		if !ok {
			return 0, fmt.Errorf("player %d key %s: non-string type", playerID, k)
		}
		pipe.Set(ctx, k, s, 0)
		written++
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, fmt.Errorf("pipeline set player %d: %w", playerID, err)
	}
	return written, nil
}

func addrsFromCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func sameDataBackend(a, b []string, da, db int) bool {
	if len(a) != len(b) || len(a) == 0 {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return da == db
}
