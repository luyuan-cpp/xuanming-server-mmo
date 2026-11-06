package logic

import (
	"context"

	"data_service/internal/metrics"
	"data_service/internal/svc"
	componentpb "proto/common/component"
)

// GetPlayerItemGainBlock 的实现(docs/design/player-inbox-service.md §2.2)。
//
// 一句话:go/player_inbox 的运维查询 PlayerInboxAdmin.GetItemGainBlocks 来这里读玩家**已落盘**的单玩家物品获取封禁表
// (player_database.item_gain_block,资料行 kProfile),和 scene 下一次加载这名玩家时拿到的是同一份。
//
// 读法与语义表同 GetPlayerInboxLedger,只是读的是资料行(共用 readPersistedPlayerRow 的错误口径):
// 没有写权行 found=false;读不成一律是 gRPC 错误,绝不退化成 found=false —— 运维会把「没有」读成「没封」。
// 封禁表与收件箱账本不在同一行,但由同一次存盘、同一事务提交(player-save-direct-commit.md §1.1)。

// 指标 result 取值(metrics.ObservePlayerItemGainBlockRead),与两本账本读取是同一个封闭集合。
const (
	itemGainBlockReadFound  = assetOpLedgerReadFound
	itemGainBlockReadAbsent = assetOpLedgerReadAbsent
	itemGainBlockReadError  = assetOpLedgerReadError
)

// GetPlayerItemGainBlock 读玩家已落盘的单玩家物品获取封禁表。返回的 error 已经是 gRPC status,可直接回给调用方。
//
// found=true 时封禁表非 nil(从未被封过时是空消息);found=false 时封禁表为 nil、error 为 nil。
// 只读:不加锁、不写任何 key。内容不做校验(升序、无重复、不含 0 由 scene 写入侧保证),原样返回,
// 让运维看到的就是存档里的真实内容。
func GetPlayerItemGainBlock(ctx context.Context, svcCtx *svc.ServiceContext, playerID uint64) (*componentpb.PlayerItemGainBlockComp, bool, error) {
	result := itemGainBlockReadError
	defer func() { metrics.ObservePlayerItemGainBlockRead(result) }()

	row, err := readPlayerStoreProfile(ctx, svcCtx, playerID, "GetPlayerItemGainBlock", "player-item-gain-block")
	if err != nil {
		return nil, false, err
	}
	if row == nil {
		result = itemGainBlockReadAbsent
		return nil, false, nil
	}

	block := row.Player.GetItemGainBlock()
	if block == nil {
		// 从未被封过:回空消息而不是 nil,让调用方区分「有存档、没封过」与「没有存档」。
		block = &componentpb.PlayerItemGainBlockComp{}
	}
	result = itemGainBlockReadFound
	return block, true, nil
}
