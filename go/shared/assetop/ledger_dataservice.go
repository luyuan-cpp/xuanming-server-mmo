package assetop

import (
	"context"
	"errors"
	"fmt"
	"time"

	componentpb "proto/common/component"
	dspb "proto/data_service"
)

// DataServiceLedger 用 data_service.GetPlayerAssetOpLedger 实现 LedgerReader、SQLCommitLedgerReader 与 StoreLedgerReader
// (docs/design/guild-phase2/07-rollback-fail-closed.md §7.8.3,docs/design/player-save-direct-commit.md §6)。
// RPC 只有一种读法:强读 player_store 的写权行和 kAsset 行,读到的就是 SQL 已提交的内容,几个方法只差返回形状。
//
// 放在 shared 而不是 guild:trade 的重投循环留着同一个空位(go/trade/internal/reconcile/pipeline.go
// 只设了 Manual),两个服务各写一份迟早分叉。
//
// 【映射(契约,换实现时必须一并满足)】
//   - found=false(没有写权行)→ (nil, nil):Loop 计 absent,照常退避;
//   - found=true → 非 nil 账本;玩家从未有过资产操作时是**空消息**,不是 nil(nil 专属于"没有写权行");
//   - 任何 gRPC 错误(含本适配器自己的超时)→ (nil, err):
//     Loop 计 error、原样继续 Retry,**不终结**(不变量 I7:读不到结局只是继续等)。
//
// 【不重试】下一次读账本本身就是重试,节律由 Loop 的退避决定;在这里重试只会吃掉投递预算。
//
// 【超时】Timeout > 0 时在调用方 ctx 上再套一层上限(调用方 ctx 更早到期时以它为准)。
// 调用方传入的是投递预算的剩余部分,单次读账本不值得吃掉预算里的大头(读不到只是本轮不终结、下轮再读),
// guild 配 300ms。Timeout <= 0 表示只受调用方 ctx 约束。
//
// 并发安全:构造后只读,可被 Loop 的多个 worker 同时调用(dspb.DataServiceClient 本身并发安全)。
type DataServiceLedger struct {
	Client  dspb.DataServiceClient
	Timeout time.Duration
}

// errDataServiceLedgerNoClient 零值 / 装配漏了客户端。回错误而不是 panic:Loop 会把它计成 error
// 并继续退避,行为与"data_service 不可达"一致(fail-closed,不终结任何行)。
var errDataServiceLedgerNoClient = errors.New("assetop: DataServiceLedger 未装配 data_service 客户端")

// ReadPersistedLedger 实现 LedgerReader。
func (d *DataServiceLedger) ReadPersistedLedger(ctx context.Context, playerID uint64) (*componentpb.PlayerAssetOpLedgerComp, error) {
	snapshot, err := d.ReadStoreLedger(ctx, playerID)
	if err != nil || !snapshot.Found {
		return nil, err
	}
	return snapshot.Ledger, nil
}

// ReadSQLCommittedLedger 实现 SQLCommitLedgerReader。账本本身就是 SQL 已提交的内容,与 ReadPersistedLedger 同一次读。
func (d *DataServiceLedger) ReadSQLCommittedLedger(ctx context.Context, playerID uint64) (*componentpb.PlayerAssetOpLedgerComp, error) {
	return d.ReadPersistedLedger(ctx, playerID)
}

// ReadStoreLedger 实现 StoreLedgerReader:写权行和账本出自同一条语句。
func (d *DataServiceLedger) ReadStoreLedger(ctx context.Context, playerID uint64) (StoreLedger, error) {
	if d == nil || d.Client == nil {
		return StoreLedger{}, errDataServiceLedgerNoClient
	}
	if d.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d.Timeout)
		defer cancel()
	}
	resp, err := d.Client.GetPlayerAssetOpLedger(ctx, &dspb.GetPlayerAssetOpLedgerRequest{PlayerId: playerID})
	if err != nil {
		// %w 保留 gRPC status:调用方可用 status.Code(err) 取原 code。
		return StoreLedger{}, fmt.Errorf("data_service.GetPlayerAssetOpLedger player_id=%d: %w", playerID, err)
	}
	snapshot := StoreLedger{Found: resp.GetFound(), Released: resp.GetPlayerReleased(), Ledger: resp.GetLedger()}
	// found=true 但账本字段缺席(对端没显式带上空消息):按"存档在、账本为空"处理,不得回 nil 冒充"没有写权行"。
	if snapshot.Found && snapshot.Ledger == nil {
		snapshot.Ledger = &componentpb.PlayerAssetOpLedgerComp{}
	}
	return snapshot, nil
}
