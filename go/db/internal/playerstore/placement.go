package playerstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"

	"shared/placement"
)

// ErrPlayerStorageMissing 表示玩家没有落点:没有路由行,或行上 storage_id 为 NULL。
//
// 建角恒写 storage_id,走到这里只能是角色没建完或行被写坏了。不回落 home_zone 或本进程 zone:
// 猜一个库认领,比拒绝更糟。
var ErrPlayerStorageMissing = errors.New("player has no placement record")

// routeSchemaPattern 校验要拼进 SQL 的库名:库名不是绑定参数,形状检查是唯一的防线。
var routeSchemaPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

// ResolverOptions 是 NewStorageResolver 的依赖。
type ResolverOptions struct {
	// DB 是能读全局库的连接池,读路由用它的一条连接。
	DB *sql.DB
	// RouteSchema 是 player_route 所在的库名。
	RouteSchema string
}

// NewStorageResolver 给认领和实体已没的空行释放选库。搬库中返回 ErrStorageFrozen,没有落点返回 ErrPlayerStorageMissing,
// 读路由失败返回包着 SQL 错误的 error。三者都不写库,调用方按暂不可用回给 scene。
//
// 只读一次、不在玩家库事务里跨库锁路由行:写权由玩家库提交事务里的写权行判定,路由行不是防旧写的手段;
// 跨库锁还会把每笔认领都串到全局库上,拆多集群时也用不了(docs/design/player-route-mapping-durability-proposal.md §15.9(d))。
func NewStorageResolver(opt ResolverOptions) (StorageResolver, error) {
	if opt.DB == nil {
		return nil, errors.New("storage resolver needs a pool that can read the global database")
	}
	if !routeSchemaPattern.MatchString(opt.RouteSchema) {
		return nil, fmt.Errorf("storage resolver: %q is not a plain schema name", opt.RouteSchema)
	}
	router := &storageRouter{routes: sqlRouteReader{db: opt.DB, query: routeReadSQL(opt.RouteSchema)}}
	return router.resolve, nil
}

// storageRoute 是路由行里选库要用的两列。
type storageRoute struct {
	storageID uint32 // 0 = NULL
	frozen    bool   // frozen_run_id 不是 NULL
}

// routeReader 读一名玩家的路由行。found=false 只表示确定没有这一行;读失败一律回 error,不得当成没有。单测注入假实现。
type routeReader interface {
	readRoute(ctx context.Context, playerID uint64) (route storageRoute, found bool, err error)
}

// storageRouter 按 player_route 选库。构造后只读。
type storageRouter struct {
	routes routeReader
}

func (r *storageRouter) resolve(ctx context.Context, playerID uint64) (uint32, error) {
	route, found, err := r.routes.readRoute(ctx, playerID)
	if err != nil {
		return 0, err
	}
	return decideStorage(route, found)
}

// decideStorage 是纯判定。冻结排在落点前面:搬库中的玩家不能按旧落点认领。
func decideStorage(route storageRoute, found bool) (uint32, error) {
	switch {
	case !found:
		return 0, ErrPlayerStorageMissing
	case route.frozen:
		return 0, ErrStorageFrozen
	case route.storageID == 0:
		return 0, ErrPlayerStorageMissing
	default:
		return route.storageID, nil
	}
}

// routeReadSQL 是一次主键点查。不加锁,读到的是语句开始时已提交的值:池的每条连接都固定成强读(TiDB leader 严格读,MySQL 主库)。
func routeReadSQL(schema string) string {
	return "SELECT storage_id, frozen_run_id FROM `" + schema + "`.`player_route` WHERE route_shard = ? AND player_id = ?"
}

type sqlRouteReader struct {
	db    *sql.DB
	query string
}

func (r sqlRouteReader) readRoute(ctx context.Context, playerID uint64) (storageRoute, bool, error) {
	var storage sql.Null[uint32]
	var frozen sql.NullString
	err := r.db.QueryRowContext(ctx, r.query, placement.RouteRowShard(playerID), playerID).Scan(&storage, &frozen)
	if errors.Is(err, sql.ErrNoRows) {
		return storageRoute{}, false, nil
	}
	if err != nil {
		return storageRoute{}, false, fmt.Errorf("read player_route for player %d: %w", playerID, err)
	}
	return storageRoute{storageID: storage.V, frozen: frozen.Valid}, true, nil
}
