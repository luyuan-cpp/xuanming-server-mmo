package logic

import (
	"context"
	"errors"

	"data_service/internal/svc"
	dbpb "proto/common/database"

	goredis "github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// readPersistedPlayerDatabase 读 scene 存盘写的 `<PlayerAllData 全名>:{player_id}` blob,返回其中的 player_database。
//
// 两个「已落盘账本只读」RPC 共用这一条读路径:GetPlayerAssetOpLedger(资产通道,asset_op_ledger_logic.go)与
// GetPlayerInboxLedger(玩家收件箱对账,player_inbox_ledger_logic.go,docs/design/player-inbox-service.md §5.1、§9.3)。
// 两者读同一把 key、同一份 blob,错误语义也是同一张表(asset_op_ledger_logic.go 文件头的语义表),所以只写这一份:
// 改语义要同时改 proto/data_service/data_service.proto 里两个 RPC 的注释。
//
// 返回约定:
//   - found=true:player 非 nil,且 player_id 已核对等于请求值;
//   - found=false:key 不存在(redis.Nil),player 为 nil、error 为 nil;
//   - 其余一律是 gRPC status error,**绝不**退化成 found=false(调用方会把 absent 当成「再等等」,
//     把 Redis 故障 / 数据矛盾藏进一个看起来正常的计数里)。
//
// rpc 只进 status 文案,logTag 只进日志前缀,都由调用方传常量。只读:不加锁、不写任何 key、不碰 MySQL。
func readPersistedPlayerDatabase(ctx context.Context, svcCtx *svc.ServiceContext, playerID uint64, rpc, logTag string) (*dbpb.PlayerDatabase, bool, error) {
	return readPersistedPlayerDatabaseWith(ctx, svcCtx, playerID, rpc, logTag, getPlayerAllDataBlob)
}

// playerBlobFetcher 从玩家所在 zone 的 Redis 取 PlayerAllData blob 原文。key 不存在必须回 goredis.Nil
// (语义表的 found=false),其余错误原样返回,由 readPersistedPlayerDatabaseWith 统一映射成 gRPC status。
type playerBlobFetcher func(ctx context.Context, client goredis.Cmdable, playerID uint64) ([]byte, error)

// getPlayerAllDataBlob 是缺省取法:一次 GET。
func getPlayerAllDataBlob(ctx context.Context, client goredis.Cmdable, playerID uint64) ([]byte, error) {
	return client.Get(ctx, playerAllDataKey(playerID)).Bytes()
}

// readPersistedPlayerDatabaseWith 与 readPersistedPlayerDatabase 的返回约定、错误语义完全相同,只是 blob 原文由 fetch 取。
// 带副本核验的账本读(asset_op_ledger_verified.go)用它在取 blob 的同一段 Lua 里顺带取实例身份与身份戳,
// 路由、错误映射与解码仍只有这一份。
func readPersistedPlayerDatabaseWith(ctx context.Context, svcCtx *svc.ServiceContext, playerID uint64, rpc, logTag string,
	fetch playerBlobFetcher) (*dbpb.PlayerDatabase, bool, error) {
	if playerID == 0 {
		return nil, false, status.Errorf(codes.InvalidArgument, "%s: player_id is required", rpc)
	}
	if svcCtx == nil || svcCtx.Router == nil {
		return nil, false, status.Errorf(codes.Unavailable, "%s: router not configured", rpc)
	}

	client, err := svcCtx.Router.ClientForPlayer(ctx, playerID)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, false, status.FromContextError(ctxErr).Err()
		}
		// 不打日志:调用方(guild 重投循环 / player_inbox 对账)自己会打 ERROR,映射缺席 / 映射库故障在
		// GetPlayerHomeZone 等入口也是同一口径(只回 status)。
		return nil, false, status.Errorf(codes.Unavailable, "%s: resolve home zone: %v", rpc, err)
	}

	raw, err := fetch(ctx, client, playerID)
	if errors.Is(err, goredis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			// 调用方已放弃:如实回它的取消原因,不记成本服务故障。
			return nil, false, status.FromContextError(ctxErr).Err()
		}
		logx.Errorf("[%s] zone redis read failed: player_id=%d: %v", logTag, playerID, err)
		return nil, false, status.Errorf(codes.Internal, "%s: read player blob: %v", rpc, err)
	}

	data := &dbpb.PlayerAllData{}
	if err := proto.Unmarshal(raw, data); err != nil {
		logx.Errorf("[%s] corrupt PlayerAllData blob: player_id=%d bytes=%d: %v", logTag, playerID, len(raw), err)
		return nil, false, status.Errorf(codes.Internal, "%s: decode player blob: %v", rpc, err)
	}
	player := data.GetPlayerDatabaseData()
	// player_database_data 缺席时 GetPlayerId() 为 0,而请求的 id 已保证非 0,同样落进这一支。
	if got := player.GetPlayerId(); got != playerID {
		logx.Errorf("[%s] PlayerAllData blob belongs to another player: key player_id=%d, blob player_id=%d",
			logTag, playerID, got)
		return nil, false, status.Errorf(codes.Internal,
			"%s: blob player_id=%d does not match requested player_id=%d", rpc, got, playerID)
	}
	return player, true, nil
}
