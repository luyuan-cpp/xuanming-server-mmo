package logic

import (
	"context"

	"data_service/internal/metrics"
	"data_service/internal/svc"
	componentpb "proto/common/component"
	dbpb "proto/common/database"
	"shared/savebarrier"
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

// playerAllDataKey 是 scene 存盘写的父快照键 PlayerAllData:{<pid>},与 scene(`MessageAsyncClient<Guid, PlayerAllData>`)、
// login、match、friend 同经 savebarrier.ParentKey 构造,不手写字面量。
func playerAllDataKey(playerID uint64) string {
	return savebarrier.ParentKey(playerID)
}

// GetPlayerAssetOpLedger 读玩家已落盘的资产账本。返回的 error 已经是 gRPC status,可直接回给调用方。
//
// found=true 时账本非 nil(玩家从未有过资产操作时是空消息);found=false 时账本为 nil、error 为 nil。
// 只读:不加锁、不写任何 key、不碰 MySQL。
func GetPlayerAssetOpLedger(ctx context.Context, svcCtx *svc.ServiceContext, playerID uint64) (*componentpb.PlayerAssetOpLedgerComp, bool, error) {
	result := assetOpLedgerReadError
	defer func() { metrics.ObserveAssetOpLedgerRead(result) }()

	// 读路径与语义表的实现在 readPersistedPlayerDatabase(persisted_player_database.go),与 GetPlayerInboxLedger 共用;
	// 上面的语义表仍是两者的契约说明。调用方(guild 适配器自带 300ms 上限)取消时如实回 Canceled / DeadlineExceeded。
	player, found, err := readPersistedPlayerDatabase(ctx, svcCtx, playerID, "GetPlayerAssetOpLedger", "asset-op-ledger")
	if err != nil {
		return nil, false, err
	}
	if !found {
		result = assetOpLedgerReadAbsent
		return nil, false, nil
	}

	result = assetOpLedgerReadFound
	return assetOpLedgerOf(player), true, nil
}

// assetOpLedgerOf 取 blob 里的资产账本。从未有过资产操作时回空消息而不是 nil,
// 让调用方区分"有 blob、账本为空"与"没有 blob"(found=true ⇒ 账本非 nil 是语义表的一部分)。
func assetOpLedgerOf(player *dbpb.PlayerDatabase) *componentpb.PlayerAssetOpLedgerComp {
	if ledger := player.GetAssetOpLedger(); ledger != nil {
		return ledger
	}
	return &componentpb.PlayerAssetOpLedgerComp{}
}
