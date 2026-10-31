package logic

import (
	"context"

	"data_service/internal/metrics"
	"data_service/internal/svc"
	componentpb "proto/common/component"
	dbpb "proto/common/database"
)

// GetPlayerAssetOpLedger 的实现(docs/design/guild-phase2/07-rollback-fail-closed.md §7.8.2)。
//
// 一句话:guild、trade、mail 的重投循环对「长期离线、scene 可能已经记过账」的行,来这里读玩家已提交的资产账本,
// 读到「已应用 / 已拒绝」就能提前终结,读不到就继续等(不变量 I7,判定在调用方)。账本在 player_store 的 kAsset 行,
// 写权行和账本出自同一条语句(docs/design/player-save-direct-commit.md §6)。
//
// 语义表(调用方依赖,改动须同步 07 §7.8.2 与 go/shared/assetop/ledger_dataservice.go):
//
//	没有写权行(从未入场)     → found=false,无 error
//	读失败 / 解析失败         → Unavailable
//	player_id = 0             → InvalidArgument
//	调用方已取消 / 超时       → Canceled / DeadlineExceeded(不是本服务故障)
//	正常                      → found=true + 账本(从未有资产操作时为空消息)

// 指标 result 取值(metrics.ObserveAssetOpLedgerRead),集合封闭。
const (
	assetOpLedgerReadFound  = "found"
	assetOpLedgerReadAbsent = "absent"
	assetOpLedgerReadError  = "error"
)

// PlayerStoreLedger 是一次账本读的结果。
type PlayerStoreLedger struct {
	Found    bool
	Released bool
	Ledger   *componentpb.PlayerAssetOpLedgerComp
}

// GetPlayerStoreLedger 读写权行和账本,两者出自同一条语句,自动放弃据此判「不在线」和「账本未见」。
// 返回的 error 已经是 gRPC status,可直接回给调用方。
func GetPlayerStoreLedger(ctx context.Context, svcCtx *svc.ServiceContext, playerID uint64) (PlayerStoreLedger, error) {
	row, err := readPlayerStoreAsset(ctx, svcCtx, playerID, "GetPlayerAssetOpLedger", "asset-op-ledger")
	if err != nil {
		metrics.ObserveAssetOpLedgerRead(assetOpLedgerReadError)
		return PlayerStoreLedger{}, err
	}
	if row == nil {
		metrics.ObserveAssetOpLedgerRead(assetOpLedgerReadAbsent)
		return PlayerStoreLedger{}, nil
	}
	metrics.ObserveAssetOpLedgerRead(assetOpLedgerReadFound)
	return PlayerStoreLedger{Found: true, Released: row.Released, Ledger: assetOpLedgerOf(row.Player)}, nil
}

// assetOpLedgerOf 取存档里的资产账本。从未有过资产操作时回空消息而不是 nil,
// 让调用方区分「有存档、账本为空」与「没有存档」(found=true ⇒ 账本非 nil 是语义表的一部分)。
func assetOpLedgerOf(player *dbpb.PlayerDatabase) *componentpb.PlayerAssetOpLedgerComp {
	if ledger := player.GetAssetOpLedger(); ledger != nil {
		return ledger
	}
	return &componentpb.PlayerAssetOpLedgerComp{}
}
