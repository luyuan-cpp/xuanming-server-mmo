package playerstore

import (
	"context"
	"errors"
	"fmt"
)

// ErrPlayerStorageMissing 表示玩家没有落点:目录没有路由行,或行上 storage_id 为 NULL。
//
// 建角恒写 storage_id,走到这里只能是角色没建完或行被写坏了。不回落 home_zone 或本进程 zone:
// 猜一个库认领,比拒绝更糟。
var ErrPlayerStorageMissing = errors.New("player has no placement record")

// RouteReader 读一名玩家的落点。go/db 不连目录,生产实现是 accountdirectory.RouteClient(调 data_service 的 GetPlayerRoute)。
// found=false 只表示目录确定没有落点;读失败一律回 error,不得当成没有。
type RouteReader interface {
	StorageOf(ctx context.Context, playerID uint64) (storageID uint32, found bool, err error)
}

// NewStorageResolver 给认领和实体已没的空行释放选库。没有落点返回 ErrPlayerStorageMissing,读失败返回包着原因的 error。
// 两者都不写库,调用方按暂不可用回给 scene。
//
// 只读一次,不在玩家库事务里核路由:storage_id 建角后不变,写权由玩家库提交事务里的写权行判定;
// 物理位置由映射决定,闸门在同一事务里拦住旧位置的写(docs/design/horizontal-scale-logical-db.md §7)。
func NewStorageResolver(routes RouteReader) (StorageResolver, error) {
	if routes == nil {
		return nil, errors.New("storage resolver needs a route reader")
	}
	return func(ctx context.Context, playerID uint64) (uint32, error) {
		storageID, found, err := routes.StorageOf(ctx, playerID)
		if err != nil {
			return 0, fmt.Errorf("read the route of player %d: %w", playerID, err)
		}
		return decideStorage(storageID, found)
	}, nil
}

// decideStorage 是纯判定。
func decideStorage(storageID uint32, found bool) (uint32, error) {
	if !found || storageID == 0 {
		return 0, ErrPlayerStorageMissing
	}
	return storageID, nil
}
