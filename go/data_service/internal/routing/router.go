package routing

// 玩家归属映射的读写入口。player_route 只在全局库一份,这里直接走 SQL,提交即完成
// (docs/design/player-route-mapping-durability-proposal.md §15)。
//
// 这里的 home_zone 都是原始值;对外回答归属时由 handler 解析成最终区(placement.MergedZones)。
// 玩家存档不经过这里(player_store 在 TiDB)。

import (
	"context"
	"errors"
	"fmt"

	"data_service/internal/metrics"
	"data_service/internal/store"

	"github.com/zeromicro/go-zero/core/logx"
)

// RouteStore 是路由器对全局库的全部依赖。由消费方(本包)定义:路由器只需要这两个方法,单测注入假实现;
// *store.RouteStore 逐字满足。svc 那边再加一个 Close 组成 svc.PlayerRouteStore。
type RouteStore interface {
	// RegisterHome 是建角登记的库内提交点(W1)。
	RegisterHome(ctx context.Context, req store.RouteRegisterRequest) (store.RouteRegisterResult, error)
	// GetRoutes 按 id 批量点查,事务外一致读,缺席的 id 不出现在结果里。
	GetRoutes(ctx context.Context, ids []uint64) (map[uint64]store.RouteRow, error)
}

var (
	// ErrInvalidRegistration:player_id 为 0,或 home_zone 不在 [1, store.RouteHomeZoneMax]。
	// gRPC 层回 Unavailable + ErrCodeInvalidRequest。
	ErrInvalidRegistration = errors.New("route register: invalid player_id or home_zone")

	// ErrRouteStore:库这一段的故障 —— route store 没装配、SQL 错(含 1205)、route_meta 缺失或 writable≠1、
	// 1213/9007 重试用尽、库里的行过不了校验。gRPC 层回 Unavailable + ErrCodeRouteStoreError,**绝不**回 NotFound:
	// 一次库故障不能伪装成「这名玩家没有归属」,login 会据此退回建角 zone。它与 ErrHomeZoneNotMapped 从不同时出现在一条错误链里。
	ErrRouteStore = errors.New("player_route store failure")

	// ErrHomeZoneNotMapped 表示 player_route 里确实没有这名玩家的归属(没有行,或 home_zone 为 NULL),**不是**库故障。
	// 调用方用 errors.Is 区分:gRPC 层把它翻成 codes.NotFound(消息保留 "no home zone mapping" 文案),
	// 让 login 回退到账号 blob 里的建角 zone;库出故障时是 codes.Unavailable,两者必须分得开。
	//
	// 文案是**契约的一部分**:login 的 homezone.IsUnmapped 除了认 NotFound,还认这段文案。
	// 改文案 = 让调用方把「没有映射」当成故障,登录整体失败。
	ErrHomeZoneNotMapped = errors.New("no home zone mapping")
)

// HomeZoneConflictError 表示 player_route 里已有另一个 home_zone。
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

// Router 读写 player_route。并发安全(状态都在 RouteStore 的连接池里)。
type Router struct {
	routeStore RouteStore
}

// NewRouter 建路由器。routeStore 没装配起来时传字面量 nil:所有读写 fail-closed(ErrRouteStore)。
// 不能传一个 nil 的 *store.RouteStore:那是「非 nil 接口包着 nil 指针」,这里的判空拦不住,调用即 panic。
func NewRouter(routeStore RouteStore) *Router {
	return &Router{routeStore: routeStore}
}

// RegisterPlayerZone 登记 player → home_zone 映射,**绝不覆盖**。提交点是全局库 player_route 的事务,提交即完成。
//
// 结果:
//   - 没有行,或行里 home 为 NULL → 写入,返回 nil;
//   - 行里 home 与请求相同         → 不写,返回 nil(幂等:login 的 CreatePlayer 允许重试);
//   - 行里 home 与请求不同         → 不写,返回 *HomeZoneConflictError;
//   - player_id 或 storage 为 0、home 越界 → ErrInvalidRegistration,不碰库;
//   - 库故障                       → ErrRouteStore。
//
// storageID 与 home 在同一行里原子写入(player-storage-placement.md §8.1):go/db 对没有落点的角色一律拒绝认领,所以必填。行里已有落点时不覆盖,
// 它指向玩家数据**真正所在**的库。storageID 只校验非 0,它指向的库能不能打开由 go/db 按需打开时判定(§6.1)。
//
// 为什么不能覆盖:见 constants.ErrCodeZoneMappingConflict 的注释。
// 合服不影响登记(docs/design/player-data-tidb-scaling-plan.md §7.1):合服过程中在源区建号是允许的,
// 他的 home_zone 就是源区,归属解析之后落到目标区,不需要任何闸门。
//
// 每次调用恰好计一次 route_register_total{outcome}。
func (r *Router) RegisterPlayerZone(ctx context.Context, playerID uint64, homeZoneID, storageID uint32) error {
	if playerID == 0 || homeZoneID == 0 || homeZoneID > store.RouteHomeZoneMax || storageID == 0 {
		metrics.ObserveRouteRegister(metrics.RouteRegisterInvalid)
		return fmt.Errorf("%w: player_id=%d home_zone=%d storage_id=%d (want player_id > 0, home_zone in [1, %d], storage_id > 0)",
			ErrInvalidRegistration, playerID, homeZoneID, storageID, store.RouteHomeZoneMax)
	}
	if r.routeStore == nil {
		metrics.ObserveRouteRegister(metrics.RouteRegisterStoreError)
		return fmt.Errorf("%w: route store not assembled", ErrRouteStore)
	}

	res, err := r.routeStore.RegisterHome(ctx, store.RouteRegisterRequest{
		PlayerID:  playerID,
		HomeZone:  homeZoneID,
		StorageID: storageID,
	})
	if err != nil {
		metrics.ObserveRouteRegister(metrics.RouteRegisterStoreError)
		return fmt.Errorf("%w: %w", ErrRouteStore, err)
	}
	switch res.Outcome {
	case store.RouteRegisterInserted:
		metrics.ObserveRouteRegister(metrics.RouteRegisterInserted)
	case store.RouteRegisterFilledHome:
		metrics.ObserveRouteRegister(metrics.RouteRegisterFilledHome)
	case store.RouteRegisterFilledHomePlacementKept:
		metrics.ObserveRouteRegister(metrics.RouteRegisterFilledHomePlacementKept)
		// 登记本身算成功,留 ERROR 日志让人核对这条旧落点是否符合预期。
		logx.Errorf("[RegisterPlayerZone] placement already present in player_route, kept as is: player=%d zone=%d requested_storage=%d",
			playerID, homeZoneID, storageID)
	case store.RouteRegisterAlreadySame:
		metrics.ObserveRouteRegister(metrics.RouteRegisterAlreadySame)
	case store.RouteRegisterConflict:
		metrics.ObserveRouteRegister(metrics.RouteRegisterConflict)
		return &HomeZoneConflictError{PlayerID: playerID, Existing: res.ExistingHome, Requested: homeZoneID}
	default:
		metrics.ObserveRouteRegister(metrics.RouteRegisterStoreError)
		return fmt.Errorf("%w: unexpected register outcome %s for player %d", ErrRouteStore, res.Outcome, playerID)
	}
	return nil
}

// GetPlayerHomeZone 读一名玩家的 home_zone。返回原始值,没有解析合服映射(解析在 RPC 出口做)。
//
// 没有行或 home_zone 为 NULL → 包 ErrHomeZoneNotMapped;库故障 → 包 ErrRouteStore;
// 调用方已放弃(ctx 取消 / 超时)→ 带 ctx 错误,两者都不包,也不计 route_read_total。
func (r *Router) GetPlayerHomeZone(ctx context.Context, playerID uint64) (uint32, error) {
	const path = metrics.RouteReadPathHome
	rows, err := r.readRoutes(ctx, path, []uint64{playerID})
	if err != nil {
		return 0, err
	}
	row, ok := rows[playerID]
	if !ok || !row.HasHome() {
		metrics.ObserveRouteRead(path, metrics.RouteReadResultNotFound)
		return 0, fmt.Errorf("%w for player %d", ErrHomeZoneNotMapped, playerID)
	}
	metrics.ObserveRouteRead(path, metrics.RouteReadResultOK)
	return row.HomeZone, nil
}

// BatchGetPlayerHomeZone 批量读 home_zone,返回原始值。缺席的 id(没有行或 home_zone 为 NULL)不出现在结果里,不是错误,
// 见 dataserviceserver.go 的 BatchGetPlayerHomeZone 注释。
//
// 按 store.RouteBatchLimit 分块,每块一条 SQL,每块恰好记一次 route_read_total{path="batch"}。任一块失败整批报错、不回半份结果:
// 半份会被调用方当成「这些人没有归属」。块与块之间不是同一快照,调用方本来也拿不到跨批的原子视图。
// 空列表直接回空 map,不发任何 SQL。错误语义同 GetPlayerHomeZone。
func (r *Router) BatchGetPlayerHomeZone(ctx context.Context, playerIDs []uint64) (map[uint64]uint32, error) {
	const path = metrics.RouteReadPathBatch
	result := make(map[uint64]uint32, len(playerIDs))
	for start := 0; start < len(playerIDs); start += store.RouteBatchLimit {
		ids := playerIDs[start:min(start+store.RouteBatchLimit, len(playerIDs))]
		rows, err := r.readRoutes(ctx, path, ids)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			if row, ok := rows[id]; ok && row.HasHome() {
				result[id] = row.HomeZone
			}
		}
		metrics.ObserveRouteRead(path, metrics.RouteReadResultOK)
	}
	return result, nil
}

// readRoutes 读一块路由行。读失败时才在这里计数,成功的结局由调用方计。
//
// 读失败时 ctx 已取消 / 超时的话,失败就是调用方自己的截止时间造成的:不计 error、不包 ErrRouteStore,错误链带 ctx 的错误
// (gRPC 层据此回 Canceled / DeadlineExceeded)。记成 error 会触发全局库故障告警,把值班引去查一个正常的全局库。
// 代价:ctx 恰在一次真实故障之后到期时这一次故障不计数,调用方已经放弃,这次读的结论本来就没人用。
func (r *Router) readRoutes(ctx context.Context, path string, ids []uint64) (map[uint64]store.RouteRow, error) {
	if r.routeStore == nil {
		metrics.ObserveRouteRead(path, metrics.RouteReadResultError)
		return nil, fmt.Errorf("%w: %s read of %d ids: route store not assembled", ErrRouteStore, path, len(ids))
	}
	rows, err := r.routeStore.GetRoutes(ctx, ids)
	if err == nil {
		return rows, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("%s read of %d ids abandoned by the caller: %w (last error: %v)", path, len(ids), ctxErr, err)
	}
	metrics.ObserveRouteRead(path, metrics.RouteReadResultError)
	return nil, fmt.Errorf("%w: %s read of %d ids: %w", ErrRouteStore, path, len(ids), err)
}
