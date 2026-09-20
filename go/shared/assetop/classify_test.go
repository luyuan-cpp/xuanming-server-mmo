package assetop

import (
	"math"
	"testing"

	assetpb "proto/common/asset"
	componentpb "proto/common/component"
)

// 本文件的用例表与 C++ `AssetOpLedgerTest`(cpp/tests/currency_test/asset_op_ledger_test.cpp)
// **一一对应**。两边任何一侧改了判定顺序,对同一份输入就会给出不同结论,而那种分叉在
// 线上是静默的 —— 所以这张表要一起改,不改就别动算法。

const testStream = assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT

func newStreamLedger(stream assetpb.AssetOpStream, epoch, watermark, maxSeq uint64) *componentpb.AssetOpStreamLedger {
	return &componentpb.AssetOpStreamLedger{
		Stream:      stream,
		Watermark:   watermark,
		SeenBits:    make([]uint64, WindowWords),
		AppliedBits: make([]uint64, WindowWords),
		MaxSeq:      maxSeq,
		StreamEpoch: epoch,
	}
}

// markSeen 按与 C++ 一致的位序写位:第 i/64 个字的第 i%64 位,i = seq - watermark - 1。
func markSeen(l *componentpb.AssetOpStreamLedger, seq uint64, isApplied bool) {
	i := seq - l.GetWatermark() - 1
	l.SeenBits[i/64] |= uint64(1) << (i % 64)
	if isApplied {
		l.AppliedBits[i/64] |= uint64(1) << (i % 64)
	}
	if seq > l.MaxSeq {
		l.MaxSeq = seq
	}
}

func ledgerWithApplied(stream assetpb.AssetOpStream, epoch, seq uint64, partial bool) *componentpb.PlayerAssetOpLedgerComp {
	sl := newStreamLedger(stream, epoch, 0, 0)
	markSeen(sl, seq, true)
	if partial {
		sl.PartialSeqs = append(sl.PartialSeqs, seq)
	}
	return &componentpb.PlayerAssetOpLedgerComp{Streams: []*componentpb.AssetOpStreamLedger{sl}}
}

func ledgerWithRejection(stream assetpb.AssetOpStream, epoch, seq uint64, reason uint32) *componentpb.PlayerAssetOpLedgerComp {
	sl := newStreamLedger(stream, epoch, 0, 0)
	markSeen(sl, seq, false)
	sl.Rejections = append(sl.Rejections, &componentpb.AssetOpRejection{Seq: seq, ReasonTipId: reason})
	return &componentpb.PlayerAssetOpLedgerComp{Streams: []*componentpb.AssetOpStreamLedger{sl}}
}

func TestClassifySeqTable(t *testing.T) {
	// 纪元 100、watermark 0、记过 seq 1(APPLIED)与 seq 2(REJECTED),max_seq=10。
	withHistory := newStreamLedger(testStream, 100, 0, 0)
	markSeen(withHistory, 1, true)
	markSeen(withHistory, 2, false)
	withHistory.MaxSeq = 10

	slid := newStreamLedger(testStream, 100, 100, 200)

	corrupt := newStreamLedger(testStream, 100, 0, 5)
	corrupt.SeenBits = corrupt.SeenBits[:8] // 位图长度不对 = 账本损坏

	// max_seq 接近 uint64 上限(只可能来自数据损坏或人工改库):max_seq+1024 不可表示,
	// C++ ExceedsJumpCap 此时返回 false,Go 必须站同一侧,否则同一份账本两边结论相反。
	nearOverflow := newStreamLedger(testStream, 100, 0, math.MaxUint64-1)

	cases := []struct {
		name   string
		ledger *componentpb.AssetOpStreamLedger
		epoch  uint64
		seq    uint64
		want   SeqState
	}{
		{"seq 为 0", withHistory, 100, 0, SeqInvalid},
		{"纪元为 0", withHistory, 0, 1, SeqInvalid},
		{"纪元过期", withHistory, 99, 1, SeqStaleEpoch},
		{"纪元更大按空账本看", withHistory, 200, 1, SeqUnseen},
		{"纪元更大但跳号过远", withHistory, 200, 1025, SeqJumpTooFar},
		{"纪元更大、恰好在上限", withHistory, 200, 1024, SeqUnseen},
		{"已应用", withHistory, 100, 1, SeqApplied},
		{"已拒绝", withHistory, 100, 2, SeqRejected},
		{"窗口内未见", withHistory, 100, 3, SeqUnseen},
		{"同纪元跳号上限内", withHistory, 100, 1034, SeqAheadOfWindow},
		{"同纪元跳号超上限", withHistory, 100, 1035, SeqJumpTooFar},
		{"max_seq 接近溢出时上限不可表示,不算跳号", nearOverflow, 100, 2000, SeqAheadOfWindow},
		{"落后窗口下沿", slid, 100, 100, SeqBehindWindow},
		{"窗口下沿之上", slid, 100, 101, SeqUnseen},
		{"位图长度不对", corrupt, 100, 1, SeqInvalid},
		{"空账本、窗口内", nil, 100, 1, SeqUnseen},
		{"空账本、跳号过远", nil, 100, 1025, SeqJumpTooFar},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ClassifySeq(c.ledger, c.epoch, c.seq); got != c.want {
				t.Fatalf("ClassifySeq(epoch=%d seq=%d) = %s,期望 %s", c.epoch, c.seq, got, c.want)
			}
		})
	}
}

func TestClassifyPersisted(t *testing.T) {
	const epoch = 1700000000000

	t.Run("已应用", func(t *testing.T) {
		outcome, partial, reason := ClassifyPersisted(ledgerWithApplied(testStream, epoch, 7, false), testStream, epoch, 7)
		if outcome != assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED || partial || reason != 0 {
			t.Fatalf("应为 APPLIED/非部分/无原因,实际 %v %v %d", outcome, partial, reason)
		}
	})

	t.Run("部分发放", func(t *testing.T) {
		outcome, partial, reason := ClassifyPersisted(ledgerWithApplied(testStream, epoch, 7, true), testStream, epoch, 7)
		if outcome != assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED || !partial || reason != ReasonPartialApplied {
			t.Fatalf("应为 APPLIED/部分/27007,实际 %v %v %d", outcome, partial, reason)
		}
	})

	t.Run("已拒绝带原因", func(t *testing.T) {
		ledger := ledgerWithRejection(testStream, epoch, 7, ReasonCurrencyInsufficient)
		outcome, partial, reason := ClassifyPersisted(ledger, testStream, epoch, 7)
		if outcome != assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_REJECTED || partial || reason != ReasonCurrencyInsufficient {
			t.Fatalf("应为 REJECTED/27000,实际 %v %v %d", outcome, partial, reason)
		}
	})

	t.Run("中止占位的 reason 为 0", func(t *testing.T) {
		ledger := ledgerWithRejection(testStream, epoch, 7, 0)
		outcome, _, reason := ClassifyPersisted(ledger, testStream, epoch, 7)
		if outcome != assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_REJECTED || reason != 0 {
			t.Fatalf("应为 REJECTED/0,实际 %v %d", outcome, reason)
		}
	})

	t.Run("没见过", func(t *testing.T) {
		outcome, _, _ := ClassifyPersisted(ledgerWithApplied(testStream, epoch, 7, false), testStream, epoch, 8)
		if outcome != assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_UNKNOWN {
			t.Fatalf("未见应回 UNKNOWN,实际 %v", outcome)
		}
	})

	t.Run("另一条流的账本不算数", func(t *testing.T) {
		ledger := ledgerWithApplied(assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT, epoch, 7, false)
		outcome, _, _ := ClassifyPersisted(ledger, testStream, epoch, 7)
		if outcome != assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_UNKNOWN {
			t.Fatalf("跨流不得串台,实际 %v", outcome)
		}
	})

	t.Run("玩家从未落盘", func(t *testing.T) {
		outcome, _, _ := ClassifyPersisted(nil, testStream, epoch, 1)
		if outcome != assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_UNKNOWN {
			t.Fatalf("空账本应回 UNKNOWN,实际 %v", outcome)
		}
	})
}
