package logic

import (
	"context"

	"data_service/internal/store"
	"data_service/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// readPlayerStoreAsset 读 player_store 的写权行和 kAsset 行,两个「已落盘账本只读」RPC 共用:
// GetPlayerAssetOpLedger(资产通道)与 GetPlayerInboxLedger(玩家收件箱对账)。
//
// 返回约定:没有写权行回 (nil, nil),对应 found=false;读不成一律是 gRPC status error,绝不退化成 found=false
// (调用方会把 absent 当成「再等等」,把库故障藏进一个看起来正常的计数里)。写权行在、kAsset 行不在,是「存档在、账本为空」。
// 这份就是 SQL 已提交的内容,不需要副本核验,也不需要单独的提交证据。路由缺失是 found=false:路由是强读的权威,缺了就是没有这个角色。
//
// rpc 只进 status 文案,logTag 只进日志前缀,都由调用方传常量。只读:不加锁。
func readPlayerStoreAsset(ctx context.Context, svcCtx *svc.ServiceContext, playerID uint64, rpc, logTag string) (*store.PlayerAssetRow, error) {
	if playerID == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "%s: player_id is required", rpc)
	}
	if svcCtx == nil || svcCtx.PlayerStoreAssets == nil {
		return nil, status.Errorf(codes.Unavailable, "%s: player store not configured", rpc)
	}
	row, err := svcCtx.PlayerStoreAssets.ReadPlayerStoreAsset(ctx, playerID)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, status.FromContextError(ctxErr).Err()
		}
		logx.Errorf("[%s] player_store read failed: player_id=%d: %v", logTag, playerID, err)
		return nil, status.Errorf(codes.Unavailable, "%s: read player store: %v", rpc, err)
	}
	return row, nil
}
