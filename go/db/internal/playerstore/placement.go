package playerstore

import (
	"context"
	"errors"
	"fmt"

	"db/internal/metrics"

	"shared/placement"

	"github.com/redis/go-redis/v9"
)

var (
	// ErrPlayerStorageMissing 表示 Placement.Required=true 而玩家没有落点记录。
	ErrPlayerStorageMissing = errors.New("player has no placement record")
	// ErrRouteNotReady 表示映射 Redis 的副本不可信(就绪标记不在、畸形,或身份不是本次读所在的主实例),
	// 读到的值与缺键都不能拿来选库。调用方按「暂不可用」处理,过一会儿再试。
	ErrRouteNotReady = errors.New("mapping Redis replica is not trusted (route ready marker)")
)

// ResolverOptions 是 NewStorageResolver 的依赖。
type ResolverOptions struct {
	// Redis 是落点记录所在的 Redis(data_service 的 mapping Redis)。
	Redis redis.Cmdable
	// Zone 是本进程的 zone。
	Zone uint32
	// Required=true:没有落点记录的玩家一律拒绝,不回落 home_zone 或本进程 zone。
	Required bool
	// RequireRouteReady=true:改用就绪读脚本,只有副本可信才按读到的值选库。
	RequireRouteReady bool
}

// routeReadyReader 在同一段只读 Lua 里读就绪标记、本实例身份与映射键。单测注入假实现:
// miniredis 不支持 INFO 的 server / replication 段。
type routeReadyReader interface {
	readRouteReady(ctx context.Context, mappingKeys ...string) (placement.RouteReadyRead, error)
}

// routeReadyReadScript 的 Run 先发 EVALSHA,脚本缓存里没有(例如 Redis 刚重启)时回退 EVAL。
var routeReadyReadScript = redis.NewScript(placement.RouteReadyReadScript)

type redisRouteReadyReader struct {
	rc redis.Scripter
}

func (r redisRouteReadyReader) readRouteReady(ctx context.Context, mappingKeys ...string) (placement.RouteReadyRead, error) {
	raw, err := routeReadyReadScript.Run(ctx, r.rc, placement.RouteReadyReadKeys(mappingKeys...)).Result()
	if err != nil {
		return placement.RouteReadyRead{}, fmt.Errorf("route ready read script: %w", err)
	}
	return placement.ParseRouteReadyRead(raw, len(mappingKeys))
}

// storageRouter 按 player:placement:{id} / player:zone:{id} 选库。构造后只读。
type storageRouter struct {
	rc       redis.Cmdable
	zone     uint32
	required bool
	// ready 非 nil 即 RequireRouteReady=true。
	ready routeReadyReader
}

// NewStorageResolver 给存盘路径选库。搬库中返回 ErrStorageFrozen。
func NewStorageResolver(opt ResolverOptions) (StorageResolver, error) {
	if opt.Redis == nil {
		return nil, errors.New("storage resolver needs the placement Redis client")
	}
	router := &storageRouter{rc: opt.Redis, zone: opt.Zone, required: opt.Required}
	if opt.RequireRouteReady {
		router.ready = redisRouteReadyReader{rc: opt.Redis}
		metrics.PrimeRouteReady(append(placement.RouteReadyVerdictNames(), metrics.RouteReadyVerdictScriptError))
	}
	return router.resolve, nil
}

func (r *storageRouter) resolve(ctx context.Context, playerID uint64) (uint32, error) {
	snap, err := r.lookup(ctx, playerID)
	if err != nil {
		return 0, err
	}
	return decideStorage(snap, r.zone, r.required)
}

// placementSnapshot 是一次读到的两把键。
type placementSnapshot struct {
	rec         placement.Record
	recPresent  bool
	home        uint32
	homePresent bool
}

// decideStorage 是纯判定:有记录用记录,无记录用 home_zone,都没有用本进程 zone。
func decideStorage(snap placementSnapshot, zone uint32, required bool) (uint32, error) {
	switch {
	case snap.recPresent && snap.rec.Frozen:
		return 0, ErrStorageFrozen
	case snap.recPresent:
		return snap.rec.StorageID, nil
	case required:
		return 0, ErrPlayerStorageMissing
	default:
		return placement.EffectiveStorage(snap.rec, snap.recPresent, snap.home, snap.homePresent, zone), nil
	}
}

// lookup 读 player:placement:{id} 与 player:zone:{id}。查询失败或任一值畸形都返回错误:
// 畸形不能当作缺席,缺席会让玩家回落 home_zone 或本进程 zone 选库,正是落点记录要防的错库写。
func (r *storageRouter) lookup(ctx context.Context, playerID uint64) (placementSnapshot, error) {
	if r.ready != nil {
		return r.lookupReady(ctx, playerID)
	}
	vals, err := r.rc.MGet(ctx, placement.Key(playerID), placement.HomeZoneKey(playerID)).Result()
	if err != nil {
		return placementSnapshot{}, fmt.Errorf("MGET placement/home_zone for player %d: %w", playerID, err)
	}
	snap, err := parsePlacementSnapshot(vals)
	if err != nil {
		return placementSnapshot{}, fmt.Errorf("player %d: %w", playerID, err)
	}
	return snap, nil
}

// lookupReady 用一次读脚本取回就绪标记、实例身份与两把键,判 Trusted 才用读到的值。
func (r *storageRouter) lookupReady(ctx context.Context, playerID uint64) (placementSnapshot, error) {
	read, err := r.ready.readRouteReady(ctx, placement.Key(playerID), placement.HomeZoneKey(playerID))
	if err != nil {
		metrics.CountRouteReadyVerdict(metrics.RouteReadyVerdictScriptError)
		return placementSnapshot{}, fmt.Errorf("route ready read placement/home_zone for player %d: %w", playerID, err)
	}
	verdict, _ := placement.VerifyRouteReady(read)
	metrics.CountRouteReadyVerdict(verdict.String())
	if verdict != placement.RouteReadyTrusted {
		return placementSnapshot{}, fmt.Errorf("%w: verdict=%s", ErrRouteNotReady, verdict)
	}
	snap, err := parsePlacementSnapshot(read.Values)
	if err != nil {
		return placementSnapshot{}, fmt.Errorf("player %d: %w", playerID, err)
	}
	return snap, nil
}

// parsePlacementSnapshot 解析两个值(nil = 键不存在)。
func parsePlacementSnapshot(vals []interface{}) (placementSnapshot, error) {
	if len(vals) != 2 {
		return placementSnapshot{}, fmt.Errorf("read returned %d values, want 2", len(vals))
	}
	var snap placementSnapshot
	if raw, present, err := optionalString(vals[0]); err != nil {
		return placementSnapshot{}, err
	} else if present {
		rec, err := placement.Parse(raw)
		if err != nil {
			return placementSnapshot{}, err
		}
		snap.rec, snap.recPresent = rec, true
	}
	if raw, present, err := optionalString(vals[1]); err != nil {
		return placementSnapshot{}, err
	} else if present {
		home, ok, err := placement.ParseHomeZone(raw)
		if err != nil {
			return placementSnapshot{}, err
		}
		// home_zone 必须能当 zone 库的落点编号用(无记录时就落它):>= 1000000 派生出的是
		// player_store_* 而不是这个 zone 的库,按畸形拒绝。
		if ok && !placement.IsZoneStorage(home) {
			return placementSnapshot{}, fmt.Errorf("%w: home zone %d is outside the zone storage range 1..%d",
				placement.ErrMalformed, home, placement.MaxZoneStorageID)
		}
		snap.home, snap.homePresent = home, ok
	}
	return snap, nil
}

// optionalString 把一个元素转成字符串;nil = 键不存在。
func optionalString(v interface{}) (string, bool, error) {
	switch s := v.(type) {
	case nil:
		return "", false, nil
	case string:
		return s, true, nil
	default:
		return "", false, fmt.Errorf("unexpected reply element type %T", v)
	}
}
