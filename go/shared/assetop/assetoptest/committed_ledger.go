// Package assetoptest 给业务服务的测试提供资产通道替身。只给测试用,生产装配不引用。
package assetoptest

import (
	"context"
	"fmt"
	"sync"

	"google.golang.org/protobuf/proto"

	assetpb "proto/common/asset"
	componentpb "proto/common/component"

	"shared/assetop"
)

// CommittedLedger 是已提交账本(player_store 的 kAsset 行)的替身,实现 assetop.LedgerReader 与
// assetop.SQLCommitLedgerReader。假 scene 回已落盘结局时调 Commit,把结局连同请求身份记下来;Loop 终结前核对的
// 就是这一份,和生产里 scene 把资产与账本同一事务提交之后读到的一样。
//
// 零值可用,并发安全(重投循环的 worker 会并发调用)。
type CommittedLedger struct {
	mu      sync.Mutex
	players map[uint64]*componentpb.PlayerAssetOpLedgerComp
}

// Commit 把一次结局记进账本。只有 res.Terminal() 才算记账,其余(RETRY、NOT_HERE、未落盘)不留痕。
// 已记账的 seq 不再改变(不变量 I2)。身份不全或 seq 超出账本窗口时回错误:这是夹具写错了,不是 scene 的结局。
func (l *CommittedLedger) Commit(req *assetpb.AssetOpRequest, res assetop.Result) error {
	if !res.Terminal() {
		return nil
	}
	digest, ok := assetop.ReceiptDigest(assetop.Op{
		PlayerID:      req.GetPlayerId(),
		Stream:        req.GetStream(),
		StreamEpoch:   req.GetStreamEpoch(),
		Seq:           req.GetSeq(),
		CorrelationID: req.GetCorrelationId(),
		TxType:        req.GetTxType(),
		Bundle:        req.GetBundle(),
	})
	if !ok || req.GetSeq() > assetop.WindowBits {
		return fmt.Errorf("assetoptest: 请求身份不全或 seq 超出账本窗口 player_id=%d stream=%d seq=%d", req.GetPlayerId(), req.GetStream(), req.GetSeq())
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	stream := l.streamOf(req)
	switch assetop.ClassifySeq(stream, req.GetStreamEpoch(), req.GetSeq()) {
	case assetop.SeqApplied, assetop.SeqRejected:
		return nil
	}
	index := req.GetSeq() - 1
	stream.SeenBits[index/64] |= uint64(1) << (index % 64)
	if req.GetSeq() > stream.MaxSeq {
		stream.MaxSeq = req.GetSeq()
	}
	if res.Outcome == assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED {
		stream.AppliedBits[index/64] |= uint64(1) << (index % 64)
	} else if res.Reason != 0 {
		stream.Rejections = append(stream.Rejections, &componentpb.AssetOpRejection{Seq: req.GetSeq(), ReasonTipId: res.Reason})
	}
	if res.Partial {
		stream.PartialSeqs = append(stream.PartialSeqs, req.GetSeq())
	}
	stream.Receipts = append(stream.Receipts, &componentpb.AssetOpReceipt{
		Seq:           req.GetSeq(),
		CorrelationId: req.GetCorrelationId(),
		RequestDigest: digest[:],
		Snapshot:      res.Snapshot,
	})
	return nil
}

// streamOf 取这条流的账本,没有就建。新纪元整条流重置,和 scene 记账时一样。调用方持有 l.mu。
func (l *CommittedLedger) streamOf(req *assetpb.AssetOpRequest) *componentpb.AssetOpStreamLedger {
	if l.players == nil {
		l.players = make(map[uint64]*componentpb.PlayerAssetOpLedgerComp)
	}
	ledger := l.players[req.GetPlayerId()]
	if ledger == nil {
		ledger = &componentpb.PlayerAssetOpLedgerComp{}
		l.players[req.GetPlayerId()] = ledger
	}
	fresh := &componentpb.AssetOpStreamLedger{
		Stream:      req.GetStream(),
		StreamEpoch: req.GetStreamEpoch(),
		SeenBits:    make([]uint64, assetop.WindowWords),
		AppliedBits: make([]uint64, assetop.WindowWords),
	}
	for i, stream := range ledger.Streams {
		if stream.GetStream() != req.GetStream() {
			continue
		}
		if stream.GetStreamEpoch() == req.GetStreamEpoch() {
			return stream
		}
		ledger.Streams[i] = fresh
		return fresh
	}
	ledger.Streams = append(ledger.Streams, fresh)
	return fresh
}

// ReadPersistedLedger 实现 assetop.LedgerReader。玩家没有账本时回 (nil, nil),对应「没有写权行」。
func (l *CommittedLedger) ReadPersistedLedger(_ context.Context, playerID uint64) (*componentpb.PlayerAssetOpLedgerComp, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ledger := l.players[playerID]
	if ledger == nil {
		return nil, nil
	}
	return proto.Clone(ledger).(*componentpb.PlayerAssetOpLedgerComp), nil
}

// ReadSQLCommittedLedger 实现 assetop.SQLCommitLedgerReader,与 ReadPersistedLedger 读的是同一份。
func (l *CommittedLedger) ReadSQLCommittedLedger(ctx context.Context, playerID uint64) (*componentpb.PlayerAssetOpLedgerComp, error) {
	return l.ReadPersistedLedger(ctx, playerID)
}
