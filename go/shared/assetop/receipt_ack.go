package assetop

import (
	"context"
	"errors"
	assetpb "proto/common/asset"
)

// AcknowledgeReceipt 供交易库已提交托管快照后的可靠 ACK 待办使用。
// 前置：业务SQL已保存原操作的APPLIED证明与完整托管原件，才可创建ACK待办。
// done=true须证明SQL收据身份仍匹配且托管原件已释放；RPC成功不等于完成。
// 已滑出窗口的操作不能再分类，但原件已在业务SQL；仅结束清理，不据此完成原资产操作。
// done=false 保留相同待办，离线、证据缓存丢失、SQL 在途均可重投，不另占资产 seq。
//
// 证据只认已提交账本（reader 必须实现 SQLCommitLedgerReader，读 ReadSQLCommittedLedger）。
// reader 只会读 Redis 父键 blob 时回错误，不退回 blob 判定。
func AcknowledgeReceipt(ctx context.Context, applier Applier, reader LedgerReader, req *assetpb.AssetOpRequest) (done bool, err error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if applier == nil || reader == nil || req == nil || req.GetSeq() == 0 || req.GetCorrelationId() == 0 {
		return false, errors.New("资产收据 ACK 依赖或身份不完整")
	}
	sqlReader, ok := reader.(SQLCommitLedgerReader)
	if !ok {
		return false, errors.New("资产收据 ACK 需要能读 SQL 提交证据的账本读取器")
	}
	res, err := applier.Do(ctx, RPCAckReceipt, req)
	if err != nil {
		return false, err
	}
	if res.Outcome != assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED {
		return false, nil
	}
	ledger, err := sqlReader.ReadSQLCommittedLedger(ctx, req.GetPlayerId())
	if err != nil || ledger == nil {
		return false, err
	}
	for _, stream := range ledger.GetStreams() {
		if stream.GetStream() != req.GetStream() || stream.GetStreamEpoch() != req.GetStreamEpoch() {
			continue
		}
		if req.GetSeq() <= stream.GetWatermark() {
			// 已保存原件的业务只做清理；窗口外不再认位图结局。
			for _, receipt := range stream.GetReceipts() {
				if receipt.GetSeq() == req.GetSeq() {
					return false, nil
				}
			}
			return true, nil
		}
		receipt := matchingReceipt(ledger, requestOp(req))
		if receipt == nil || snapshotHasInstances(receipt.GetSnapshot()) {
			return false, nil
		}
		outcome, partial, _ := ClassifyPersisted(ledger, req.GetStream(), req.GetStreamEpoch(), req.GetSeq())
		return outcome == assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED && !partial, nil
	}
	return false, nil
}
