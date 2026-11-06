package logic

import (
	"context"

	"data_service/internal/store"
	"data_service/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// readPlayerStoreAsset 读 player_store 的写权行和 kWallet 行,两个「已落盘账本只读」RPC 共用:
// GetPlayerAssetOpLedger(资产通道)与 GetPlayerInboxLedger(玩家收件箱对账)。
//
// 返回约定:没有写权行回 (nil, nil),对应 found=false;读不成一律是 gRPC status error,绝不退化成 found=false
// (调用方会把 absent 当成「再等等」,把库故障藏进一个看起来正常的计数里)。写权行在、kWallet 行不在,是「存档在、账本为空」。
// 这份就是 SQL 已提交的内容,不需要副本核验,也不需要单独的提交证据。路由缺失是 found=false:路由是强读的权威,缺了就是没有这个角色。
//
// rpc 只进 status 文案,logTag 只进日志前缀,都由调用方传常量。只读:不加锁。
func readPlayerStoreAsset(ctx context.Context, svcCtx *svc.ServiceContext, playerID uint64, rpc, logTag string) (*store.PlayerAssetRow, error) {
	var read func(context.Context, uint64) (*store.PlayerAssetRow, error)
	if svcCtx != nil && svcCtx.PlayerStoreAssets != nil {
		read = svcCtx.PlayerStoreAssets.ReadPlayerStoreAsset
	}
	return readPersistedPlayerRow(ctx, playerID, rpc, logTag, read)
}

// readPlayerStoreProfile 读 player_store 的写权行和 kProfile 行,给 GetPlayerItemGainBlock(运维查单玩家物品获取封禁表)用。
// 返回约定与 readPlayerStoreAsset 相同;写权行在、kProfile 行不在,是「存档在、没封过」。
func readPlayerStoreProfile(ctx context.Context, svcCtx *svc.ServiceContext, playerID uint64, rpc, logTag string) (*store.PlayerProfileRow, error) {
	var read func(context.Context, uint64) (*store.PlayerProfileRow, error)
	if svcCtx != nil && svcCtx.PlayerStoreProfiles != nil {
		read = svcCtx.PlayerStoreProfiles.ReadPlayerStoreProfile
	}
	return readPersistedPlayerRow(ctx, playerID, rpc, logTag, read)
}

// readPersistedPlayerRow 是「已落盘存档只读」RPC 共用的错误口径,语义表一处定义:
//
//	player_id = 0             → InvalidArgument
//	读取器没装配(read 为 nil)→ Unavailable
//	调用方已取消 / 超时       → Canceled / DeadlineExceeded(不是本服务故障)
//	读失败 / 解析失败         → Unavailable,并记 ERROR
//	没有写权行或路由          → (nil, nil),由调用方回 found=false
func readPersistedPlayerRow[T any](ctx context.Context, playerID uint64, rpc, logTag string,
	read func(context.Context, uint64) (*T, error)) (*T, error) {
	if playerID == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "%s: player_id is required", rpc)
	}
	if read == nil {
		return nil, status.Errorf(codes.Unavailable, "%s: player store not configured", rpc)
	}
	row, err := read(ctx, playerID)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, status.FromContextError(ctxErr).Err()
		}
		logx.Errorf("[%s] player_store read failed: player_id=%d: %v", logTag, playerID, err)
		return nil, status.Errorf(codes.Unavailable, "%s: read player store: %v", rpc, err)
	}
	return row, nil
}
