package logic

import (
	"context"
	"errors"
	"fmt"

	"data_service/internal/metrics"
	"data_service/internal/svc"
	componentpb "proto/common/component"
	dbpb "proto/common/database"

	goredis "github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// GetPlayerAssetOpLedger 的实现(docs/design/guild-phase2/07-rollback-fail-closed.md §7.8.2)。
//
// 一句话:guild 的重投循环对"长期离线、scene 可能已经记过账"的行,来这里读玩家**已落盘**的
// 资产账本,读到"已应用 / 已拒绝"就能提前终结,读不到就继续等(不变量 I7,判定在 guild 侧)。
//
// 【读哪个 key】scene 存盘写的是 `<PlayerAllData 全名>:{player_id}`(整份 blob,不带 TTL 的裸 SET),
// 账本与 currency / bag 同记录同一次 SET(不变量 I3)。data_service 自己那套 player:{id}:<field>
// 里**没有**账本,读它会永远 found=false —— L5 钉住这一点。
//
// 【为什么不回退读 MySQL】04 只承认"已在 Redis 即 durable";MySQL 是异步 DBTask 落库,可能更旧,
// 会把已记账的 seq 误判为"未见",让 guild 多等一轮甚至误判。
//
// 【为什么 gRPC code 在 logic 里定】这个 RPC 没有 in-band 的 error_code 字段,契约就是 gRPC code
// 本身(§7.8.2 语义表)。把表放在一处,handler 只做搬运,单测也能直接对着 code 断言(L3 / L4)。
//
// 语义表(调用方依赖,改动须同步 07 §7.8.2 与 go/shared/assetop/ledger_dataservice.go):
//
//	key 不存在(redis.Nil)                       → found=false,无 error
//	blob 反序列化失败 / player_id 与请求不符        → Internal(数据已矛盾,不能当"没有")
//	home_zone 查不到(无映射 / 映射库错 / zone 未配)→ Unavailable
//	zone Redis 读失败                              → Internal
//	player_id = 0                                  → InvalidArgument
//	调用方已取消 / 超时                            → Canceled / DeadlineExceeded(不是本服务故障)
//	正常                                           → found=true + 账本(从未有资产操作时为空消息)
//
// 任何一条错误路径都**不得**回 found=false:guild 会把 found=false 记成 absent 继续退避,本身无害,
// 但它会把"Redis 坏了 / 数据矛盾"藏进一个看起来正常的计数里。

// 指标 result 取值(metrics.ObserveAssetOpLedgerRead),集合封闭。
const (
	assetOpLedgerReadFound  = "found"
	assetOpLedgerReadAbsent = "absent"
	assetOpLedgerReadError  = "error"
)

// playerAllDataKeyPrefix 取 message 全名,与 scene(`MessageAsyncClient<Guid, PlayerAllData>`)、
// login(player_class_backfill.go)、match(team/presence.go)同源,不手写字面量:
// 给 player_cache.proto 加 package 时这里跟着变,而不是静默读错 key。
var playerAllDataKeyPrefix = string((&dbpb.PlayerAllData{}).ProtoReflect().Descriptor().FullName())

func playerAllDataKey(playerID uint64) string {
	return fmt.Sprintf("%s:%d", playerAllDataKeyPrefix, playerID)
}

// GetPlayerAssetOpLedger 读玩家已落盘的资产账本。返回的 error 已经是 gRPC status,可直接回给调用方。
//
// found=true 时账本非 nil(玩家从未有过资产操作时是空消息);found=false 时账本为 nil、error 为 nil。
// 只读:不加锁、不写任何 key、不碰 MySQL。
func GetPlayerAssetOpLedger(ctx context.Context, svcCtx *svc.ServiceContext, playerID uint64) (*componentpb.PlayerAssetOpLedgerComp, bool, error) {
	result := assetOpLedgerReadError
	defer func() { metrics.ObserveAssetOpLedgerRead(result) }()

	if playerID == 0 {
		return nil, false, status.Error(codes.InvalidArgument, "GetPlayerAssetOpLedger: player_id is required")
	}
	if svcCtx == nil || svcCtx.Router == nil {
		return nil, false, status.Error(codes.Unavailable, "GetPlayerAssetOpLedger: router not configured")
	}

	client, err := svcCtx.Router.ClientForPlayer(ctx, playerID)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, false, status.FromContextError(ctxErr).Err()
		}
		// 不打日志:这条路径的调用方(guild)自己会打 ERROR,映射缺席 / 映射库故障在
		// GetPlayerHomeZone 等入口也是同一口径(只回 status)。
		return nil, false, status.Errorf(codes.Unavailable, "GetPlayerAssetOpLedger: resolve home zone: %v", err)
	}

	raw, err := client.Get(ctx, playerAllDataKey(playerID)).Bytes()
	if errors.Is(err, goredis.Nil) {
		result = assetOpLedgerReadAbsent
		return nil, false, nil
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			// 调用方(guild 适配器自带 300ms 上限)已放弃:如实回它的取消原因,不记成本服务故障。
			return nil, false, status.FromContextError(ctxErr).Err()
		}
		logx.Errorf("[asset-op-ledger] zone redis GET failed: player_id=%d: %v", playerID, err)
		return nil, false, status.Errorf(codes.Internal, "GetPlayerAssetOpLedger: read player blob: %v", err)
	}

	data := &dbpb.PlayerAllData{}
	if err := proto.Unmarshal(raw, data); err != nil {
		logx.Errorf("[asset-op-ledger] corrupt PlayerAllData blob: player_id=%d bytes=%d: %v", playerID, len(raw), err)
		return nil, false, status.Errorf(codes.Internal, "GetPlayerAssetOpLedger: decode player blob: %v", err)
	}
	player := data.GetPlayerDatabaseData()
	// player_database_data 缺席时 GetPlayerId() 为 0,而请求的 id 已保证非 0,同样落进这一支。
	if got := player.GetPlayerId(); got != playerID {
		logx.Errorf("[asset-op-ledger] PlayerAllData blob belongs to another player: key player_id=%d, blob player_id=%d",
			playerID, got)
		return nil, false, status.Errorf(codes.Internal,
			"GetPlayerAssetOpLedger: blob player_id=%d does not match requested player_id=%d", got, playerID)
	}

	ledger := player.GetAssetOpLedger()
	if ledger == nil {
		// 从未有过资产操作:回空消息而不是 nil,让调用方区分"有 blob、账本为空"与"没有 blob"。
		ledger = &componentpb.PlayerAssetOpLedgerComp{}
	}
	result = assetOpLedgerReadFound
	return ledger, true, nil
}
