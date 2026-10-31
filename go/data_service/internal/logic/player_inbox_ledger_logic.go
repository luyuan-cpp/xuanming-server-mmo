package logic

import (
	"context"

	"data_service/internal/metrics"
	"data_service/internal/svc"
	componentpb "proto/common/component"
)

// GetPlayerInboxLedger 的实现(docs/design/player-inbox-service.md §5.1、§9.3)。
//
// 一句话:go/player_inbox 的对账(领导者每小时一轮的定时作业,以及运维 ReconcilePlayer)来这里读玩家
// **已落盘**的收件箱账本(player_database.inbox_ledger),比对 decided_through_seq 与收件箱里已终结的行。
//
// 读法与语义表与 GetPlayerAssetOpLedger 完全相同(asset_op_ledger_logic.go 文件头;共用 readPlayerStoreAsset):
// 没有写权行 found=false,对账计 unknown,不告警。账本与 currency 在 scene 的同一次存盘里提交(IB-3)。
//
// 响应形状照 GetPlayerAssetOpLedgerResponse:只有 found 与 ledger,不带 error_code 也不带 tip(设计 §1.4 注),
// 契约就是 gRPC code 本身;本服务的进程级拦截器(ErrorCodeClassifier)读不到 in-band 码,对它无感。

// 指标 result 取值(metrics.ObservePlayerInboxLedgerRead),与资产账本读取是同一个封闭集合。
const (
	inboxLedgerReadFound  = assetOpLedgerReadFound
	inboxLedgerReadAbsent = assetOpLedgerReadAbsent
	inboxLedgerReadError  = assetOpLedgerReadError
)

// GetPlayerInboxLedger 读玩家已落盘的收件箱账本。返回的 error 已经是 gRPC status,可直接回给调用方。
//
// found=true 时账本非 nil(玩家从未消费过收件箱时是空消息);found=false 时账本为 nil、error 为 nil。
// 只读:不加锁、不写任何 key、不碰 MySQL。账本内容不做校验(各域唯一、recent 升序等由 scene 加载时校验,
// 设计 §8 #19);这里原样返回,让对账方看到的就是存档里的真实字节。
func GetPlayerInboxLedger(ctx context.Context, svcCtx *svc.ServiceContext, playerID uint64) (*componentpb.PlayerInboxLedgerComp, bool, error) {
	result := inboxLedgerReadError
	defer func() { metrics.ObservePlayerInboxLedgerRead(result) }()

	row, err := readPlayerStoreAsset(ctx, svcCtx, playerID, "GetPlayerInboxLedger", "player-inbox-ledger")
	if err != nil {
		return nil, false, err
	}
	if row == nil {
		result = inboxLedgerReadAbsent
		return nil, false, nil
	}

	ledger := row.Player.GetInboxLedger()
	if ledger == nil {
		// 从未消费过收件箱:回空消息而不是 nil,让调用方区分「有存档、账本为空」与「没有存档」。
		ledger = &componentpb.PlayerInboxLedgerComp{}
	}
	result = inboxLedgerReadFound
	return ledger, true, nil
}
