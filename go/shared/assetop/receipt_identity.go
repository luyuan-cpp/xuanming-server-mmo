package assetop

import (
	"bytes"
	"crypto/sha256"

	assetpb "proto/common/asset"
	componentpb "proto/common/component"
)

// ReceiptDigest 与场景端的稳定业务身份信封一致。固定 debit 并清掉时间/签名，
// 使 credit、重查和超时 abort 始终核对同一原请求；caller 仍按流的唯一号源绑定。
func ReceiptDigest(op Op) ([sha256.Size]byte, bool) {
	var caller string
	switch op.Stream {
	case assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT, assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT:
		caller = "guild"
	case assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_DEBIT, assetpb.AssetOpStream_ASSET_OP_STREAM_TRADE_CREDIT:
		caller = "trade"
	case assetpb.AssetOpStream_ASSET_OP_STREAM_SYSTEM_CREDIT:
		caller = "mail"
	default:
		return [sha256.Size]byte{}, false
	}
	if op.PlayerID == 0 || op.StreamEpoch == 0 || op.Seq == 0 || op.CorrelationID == 0 {
		return [sha256.Size]byte{}, false
	}
	req := op.Request()
	req.Auth = &assetpb.AssetOpAuth{Caller: caller}
	return sha256.Sum256(Canonical(RPCDebit, req, 0)), true
}

// matchingReceipt 只给出唯一且与原请求逐字节身份相同的收据。旧账本缺摘要、
// 重复流/收据和同 seq 改载荷都不能靠位图冒充本业务的 SQL 提交证明。
func matchingReceipt(ledger *componentpb.PlayerAssetOpLedgerComp, op Op) *componentpb.AssetOpReceipt {
	digest, ok := ReceiptDigest(op)
	if !ok {
		return nil
	}
	var selected *componentpb.AssetOpStreamLedger
	for _, stream := range ledger.GetStreams() {
		if stream.GetStream() != op.Stream {
			continue
		}
		if selected != nil || stream.GetStreamEpoch() != op.StreamEpoch {
			return nil
		}
		selected = stream
	}
	var found *componentpb.AssetOpReceipt
	for _, receipt := range selected.GetReceipts() {
		if receipt.GetSeq() != op.Seq {
			continue
		}
		if found != nil || receipt.GetCorrelationId() != op.CorrelationID ||
			len(receipt.GetRequestDigest()) != sha256.Size || !bytes.Equal(receipt.GetRequestDigest(), digest[:]) {
			return nil
		}
		found = receipt
	}
	return found
}

func snapshotHasInstances(snapshot *assetpb.AssetSnapshot) bool {
	return len(snapshot.GetItems()) != 0 || snapshot.GetPet() != nil
}

func requestOp(req *assetpb.AssetOpRequest) Op {
	return Op{PlayerID: req.GetPlayerId(), Stream: req.GetStream(), StreamEpoch: req.GetStreamEpoch(),
		Seq: req.GetSeq(), CorrelationID: req.GetCorrelationId(), TxType: req.GetTxType(), Bundle: req.GetBundle()}
}
