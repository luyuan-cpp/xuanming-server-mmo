package assetop

import (
	"math"

	assetpb "proto/common/asset"
	componentpb "proto/common/component"
)

// 已落盘账本的**只读**分类(规格 §4.37「离线已落盘结局读取」)。
//
// 这是 C++ ClassifyAssetOpSeq(cpp/.../player/system/asset_op_ledger.cpp)的 Go 镜像:
// 同一份位图、同一套顺序、同一批判定。两边的单测跑**同一张用例表**,这是防止两份实现
// 慢慢分叉的唯一手段 —— 分叉的表现是静默的:Go 把一个其实已 APPLIED 的行判成"未见",
// 于是永远卡着;或者反过来,把没记账的 seq 当成已终结,玩家的东西就丢了。
//
// 本文件**只读**账本,不写:写只发生在 scene 的 loop 线程里(不变量 I1 单写者)。

const (
	// WindowBits 是每条流的账本窗口位数,覆盖 (watermark, watermark+1024]。
	WindowBits = 1024
	// WindowWords 是位图的字数,16 × 64 == 1024。
	WindowWords = 16
	// MaxSeqJump 是同一纪元内允许的跳号上限:seq <= max_seq + 1024,超过即判 JumpTooFar。
	// 证明见规格 §4.29;它同时能检出 Redis 回退到旧 blob 导致 max_seq 变小的情况。
	MaxSeqJump = 1024
)

// SeqState 是一个 (stream, epoch, seq) 在账本里的状态,与 C++ AssetOpSeqState 逐项对应。
type SeqState uint8

const (
	// SeqInvalid seq==0 / epoch==0 / 位图长度不对 / watermark 溢出。一律 fail-closed。
	SeqInvalid SeqState = iota
	// SeqStaleEpoch 请求纪元比账本纪元小:这是**另一本流水簿**上的号,不能拿来当结论。
	SeqStaleEpoch
	// SeqBehindWindow seq 已滑出窗口下沿,结局不可知。
	SeqBehindWindow
	// SeqUnseen 窗口内未见(含"请求纪元更大、按空账本看"的情况)。
	SeqUnseen
	// SeqAheadOfWindow 超出窗口上沿,必为未见。
	SeqAheadOfWindow
	// SeqJumpTooFar 跳号过远,scene 会判 UNKNOWN。
	SeqJumpTooFar
	// SeqApplied 已应用。
	SeqApplied
	// SeqRejected 已拒绝(含中止占位)。
	SeqRejected

	seqStateCount
)

var seqStateNames = [...]string{
	"invalid", "stale_epoch", "behind_window", "unseen",
	"ahead_of_window", "jump_too_far", "applied", "rejected",
}

var _ = [1]struct{}{}[len(seqStateNames)-int(seqStateCount)]

// String 返回与 C++ 侧同名的状态名,方便两边日志对照。
func (s SeqState) String() string {
	if s >= seqStateCount {
		return seqStateNames[0]
	}
	return seqStateNames[s]
}

// FindStream 取出某条流的账本;没有这条流时返回 nil(等价于 epoch=0、watermark=0 的空账本)。
func FindStream(l *componentpb.PlayerAssetOpLedgerComp, s assetpb.AssetOpStream) *componentpb.AssetOpStreamLedger {
	for _, st := range l.GetStreams() {
		if st.GetStream() == s {
			return st
		}
	}
	return nil
}

// ClassifySeq 判定一个 seq 在这条流账本里的状态。判断顺序与 C++ 完全一致,不可重排。
func ClassifySeq(l *componentpb.AssetOpStreamLedger, epoch, seq uint64) SeqState {
	if seq == 0 || epoch == 0 {
		return SeqInvalid
	}

	ledgerEpoch := l.GetStreamEpoch()
	if epoch < ledgerEpoch {
		return SeqStaleEpoch
	}
	if epoch > ledgerEpoch {
		// 纪元更大:整条流按**空账本**看(scene 记账时会先重置)。
		if seq <= WindowBits {
			return SeqUnseen
		}
		return SeqJumpTooFar
	}
	// 走到这里 epoch == ledgerEpoch 且 epoch > 0,所以账本一定存在。

	seen := l.GetSeenBits()
	applied := l.GetAppliedBits()
	if len(seen) != WindowWords || len(applied) != WindowWords {
		// 位图长度不对 = 账本损坏。不猜、不补齐,直接判非法。
		return SeqInvalid
	}

	watermark := l.GetWatermark()
	if watermark > math.MaxUint64-WindowBits {
		return SeqInvalid
	}
	if seq <= watermark {
		return SeqBehindWindow
	}
	// 跳号上限,与 C++ ExceedsJumpCap(asset_op_ledger.cpp)逐字同义:max_seq 接近
	// uint64 上限时 max_seq+1024 不可表示,此时**任何** seq 都不算超,继续往下走
	// 窗口与位判定。这不是"放宽",而是两边必须站同一侧 —— 站反了,同一份账本
	// (max_seq > 2^64-1024 只可能来自数据损坏或人工改库)在 Go 眼里永远 UNKNOWN、
	// 行卡死终结不了,在 scene 眼里却照常给 APPLIED/REJECTED,而这种分叉线上是静默的。
	if maxSeq := l.GetMaxSeq(); maxSeq <= math.MaxUint64-MaxSeqJump && seq > maxSeq+MaxSeqJump {
		return SeqJumpTooFar
	}
	if seq > watermark+WindowBits {
		return SeqAheadOfWindow
	}

	i := seq - watermark - 1
	if !bitAt(seen, i) {
		return SeqUnseen
	}
	if bitAt(applied, i) {
		return SeqApplied
	}
	return SeqRejected
}

// bitAt 读第 i 位:第 i/64 个字的第 i%64 位(低位在前)。
// 这个布局是与 C++ 的**逐位契约**,改它等于让两边对同一份字节得出不同结论。
func bitAt(words []uint64, i uint64) bool {
	word := i / 64
	if word >= uint64(len(words)) {
		return false
	}
	return words[word]&(uint64(1)<<(i%64)) != 0
}

// ClassifyPersisted 从**已落盘**的玩家账本里读某个 seq 的结局。
//
// 只有 APPLIED / REJECTED 才是结论;其余一律回 UNKNOWN(含"未见"),调用方继续等。
// 返回的结论可以直接当 durable:它读的就是已经写进 Redis 的那份数据,而按不变量 I2,
// 已记账的结局永不改变 —— 所以玩家在不在线都不影响这次读取。
func ClassifyPersisted(l *componentpb.PlayerAssetOpLedgerComp, s assetpb.AssetOpStream, epoch, seq uint64) (assetpb.AssetOpOutcome, bool, uint32) {
	stream := FindStream(l, s)
	switch ClassifySeq(stream, epoch, seq) {
	case SeqApplied:
		if isPartialSeq(stream, seq) {
			return assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED, true, ReasonPartialApplied
		}
		return assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED, false, 0
	case SeqRejected:
		return assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_REJECTED, false, rejectionReason(stream, seq)
	default:
		return assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_UNKNOWN, false, 0
	}
}

// rejectionReason 取拒绝原因;查不到回 0(原因表被挤满时会丢,只影响展示,不影响正确性)。
func rejectionReason(l *componentpb.AssetOpStreamLedger, seq uint64) uint32 {
	for _, r := range l.GetRejections() {
		if r.GetSeq() == seq {
			return r.GetReasonTipId()
		}
	}
	return 0
}

// isPartialSeq 报告这个 seq 是不是"只发放了一部分"。
func isPartialSeq(l *componentpb.AssetOpStreamLedger, seq uint64) bool {
	for _, s := range l.GetPartialSeqs() {
		if s == seq {
			return true
		}
	}
	return false
}
