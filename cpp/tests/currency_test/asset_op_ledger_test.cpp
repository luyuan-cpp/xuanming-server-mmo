// 通用资产通道账本纯函数单测,对应 docs/design/guild-phase2/04-asset-channel.md
// §4.4(窗口算法)、§4.14.1(用例表)、§4.29/§4.30(流纪元与跳号上限)。
//
// 这里只测纯函数:不建实体、不碰 ECS、不读表、不用墙钟,随机用例固定种子(AGENTS §11.4)。
//
// 与 §4.14.1 原表的偏差(第二轮评审的跳号上限 §4.29 优先级更高,前文冲突处以它为准):
//   - 原 `LedgerEmptyClassify` 期望空流 seq 1025 → kAheadOfWindow;加了跳号上限之后,
//     空流 max_seq = 0,1025 > 0 + 1024,正确结果是 kJumpTooFar。kAheadOfWindow 只有在
//     "max_seq 已经被推高、但 watermark 还没跟上"时才出现,由 `JumpCapSameEpoch` 覆盖。
//   - 原 `LedgerSlideExactly1024`(记 1 后记 2048)、`LedgerSlideBeyondWindowClears`
//     (记 1 后记 5000)、`LedgerRejectionsPrunedOnSlide`(记 2 后记 1030)写的 seq 都超了
//     跳号上限,会被判 kJumpTooFar 而记不进去。改为先把 max_seq 推到允许的位置再跳,
//     断言的 watermark / 位图结果与原表一致。
//   - 顺带记录一条结论:因为 max_seq <= watermark + 1024 恒成立,跳号上限使得
//     "shift > 1024" 在真实流里不可达,shift == 1024 是能达到的最大滑动。
//     `LedgerSlideBeyondWindowClears` 手工抬高 max_seq 来覆盖 shift > 1024 这条防御分支。

#include <gtest/gtest.h>

#include <cstdint>
#include <limits>
#include <map>
#include <random>
#include <string>
#include <vector>

#include "services/scene/player/system/asset_op_ledger.h"

namespace {

constexpr AssetOpStream kStream = ASSET_OP_STREAM_GUILD_DEBIT;
constexpr uint64_t kEpoch = 100;
constexpr uint32_t kRejectReason = 27000;  // kAssetCurrencyInsufficient
constexpr uint64_t kUint64Max = (std::numeric_limits<uint64_t>::max)();

// 绕开 MutableAssetOpStream 直接拼一条流,用来构造"损坏账本"和越界 watermark。
AssetOpStreamLedger* AddRawStream(PlayerAssetOpLedgerComp& comp, AssetOpStream stream, uint64_t epoch) {
    auto* raw = comp.add_streams();
    raw->set_stream(stream);
    raw->set_stream_epoch(epoch);
    raw->set_watermark(0);
    raw->set_max_seq(0);
    for (int i = 0; i < kAssetOpWindowWords; ++i) {
        raw->add_seen_bits(0);
        raw->add_applied_bits(0);
    }
    return raw;
}

AssetOpSeqState Classify(const AssetOpStreamLedger& ledger, uint64_t seq, uint64_t epoch = kEpoch) {
    return ClassifyAssetOpSeq(&ledger, epoch, seq);
}

void RecordOrFail(AssetOpStreamLedger& ledger, uint64_t seq, AssetOpRecordKind kind,
                  uint32_t reason = 0, uint64_t epoch = kEpoch) {
    const AssetOpSeqState before = ClassifyAssetOpSeq(&ledger, epoch, seq);
    ASSERT_TRUE(RecordAssetOpOutcome(ledger, epoch, seq, kind, reason))
        << "seq=" << seq << " 记账前状态=" << AssetOpSeqStateName(before);
}

uint64_t SeenWord(const AssetOpStreamLedger& ledger, int word) { return ledger.seen_bits(word); }

// 一本合法账本:一条流,记过 seq 3 = APPLIED、seq 5 = REJECTED、seq 7 = 部分发放。
PlayerAssetOpLedgerComp BuildValidComp() {
    PlayerAssetOpLedgerComp comp;
    auto& ledger = MutableAssetOpStream(comp, kStream);
    RecordAssetOpOutcome(ledger, kEpoch, 3, AssetOpRecordKind::kApplied, 0);
    RecordAssetOpOutcome(ledger, kEpoch, 5, AssetOpRecordKind::kRejected, kRejectReason);
    RecordAssetOpOutcome(ledger, kEpoch, 7, AssetOpRecordKind::kAppliedPartial, 0);
    return comp;
}

}  // namespace

// ---------------------------------------------------------------------------
// 分类
// ---------------------------------------------------------------------------

TEST(AssetOpLedgerTest, LedgerEmptyClassify) {
    // 流不存在 = 纪元 0、watermark 0 的空账本。
    EXPECT_EQ(ClassifyAssetOpSeq(nullptr, kEpoch, 0), AssetOpSeqState::kInvalid);
    EXPECT_EQ(ClassifyAssetOpSeq(nullptr, 0, 1), AssetOpSeqState::kInvalid);
    EXPECT_EQ(ClassifyAssetOpSeq(nullptr, kEpoch, 1), AssetOpSeqState::kUnseen);
    EXPECT_EQ(ClassifyAssetOpSeq(nullptr, kEpoch, kAssetOpWindowBits), AssetOpSeqState::kUnseen);
    // §4.29 跳号上限:空账本 max_seq = 0,1025 超过 0 + 1024(原 §4.14.1 写的 kAheadOfWindow 已作废)。
    EXPECT_EQ(ClassifyAssetOpSeq(nullptr, kEpoch, kAssetOpWindowBits + 1), AssetOpSeqState::kJumpTooFar);

    // 刚建出来、还没记过账的流(stream_epoch = 0)与"流不存在"等价。
    PlayerAssetOpLedgerComp comp;
    auto& ledger = MutableAssetOpStream(comp, kStream);
    EXPECT_EQ(Classify(ledger, 0), AssetOpSeqState::kInvalid);
    EXPECT_EQ(Classify(ledger, 1), AssetOpSeqState::kUnseen);
    EXPECT_EQ(Classify(ledger, kAssetOpWindowBits), AssetOpSeqState::kUnseen);
    EXPECT_EQ(Classify(ledger, kAssetOpWindowBits + 1), AssetOpSeqState::kJumpTooFar);
}

TEST(AssetOpLedgerTest, LedgerFindMissingStreamNull) {
    PlayerAssetOpLedgerComp comp;
    EXPECT_EQ(FindAssetOpStream(comp, kStream), nullptr);
    MutableAssetOpStream(comp, kStream);
    ASSERT_NE(FindAssetOpStream(comp, kStream), nullptr);
    EXPECT_EQ(FindAssetOpStream(comp, ASSET_OP_STREAM_TRADE_CREDIT), nullptr);
}

TEST(AssetOpLedgerTest, LedgerRecordApplyReject) {
    PlayerAssetOpLedgerComp comp;
    auto& ledger = MutableAssetOpStream(comp, kStream);
    ASSERT_NO_FATAL_FAILURE(RecordOrFail(ledger, 3, AssetOpRecordKind::kApplied));
    ASSERT_NO_FATAL_FAILURE(RecordOrFail(ledger, 5, AssetOpRecordKind::kRejected, kRejectReason));

    EXPECT_EQ(Classify(ledger, 3), AssetOpSeqState::kApplied);
    EXPECT_EQ(Classify(ledger, 5), AssetOpSeqState::kRejected);
    EXPECT_EQ(Classify(ledger, 4), AssetOpSeqState::kUnseen);
    EXPECT_EQ(AssetOpRejectionReason(ledger, 5), kRejectReason);
    EXPECT_EQ(AssetOpRejectionReason(ledger, 3), 0u);
    EXPECT_EQ(ledger.max_seq(), 5u);
    EXPECT_EQ(ledger.watermark(), 0u);
    EXPECT_EQ(ledger.stream_epoch(), kEpoch);
}

TEST(AssetOpLedgerTest, LedgerAppliedSubsetOfSeen) {
    PlayerAssetOpLedgerComp comp;
    auto& ledger = MutableAssetOpStream(comp, kStream);
    for (uint64_t seq = 1; seq <= 200; ++seq) {
        const AssetOpRecordKind kind = (seq % 3 == 0)   ? AssetOpRecordKind::kRejected
                                       : (seq % 7 == 0) ? AssetOpRecordKind::kAppliedPartial
                                                        : AssetOpRecordKind::kApplied;
        ASSERT_NO_FATAL_FAILURE(RecordOrFail(ledger, seq, kind, kind == AssetOpRecordKind::kRejected ? kRejectReason : 0));
        for (int word = 0; word < kAssetOpWindowWords; ++word) {
            ASSERT_EQ(ledger.applied_bits(word) & ~ledger.seen_bits(word), 0u)
                << "seq=" << seq << " word=" << word;
        }
    }
    EXPECT_TRUE(ValidateAssetOpLedger(comp).empty()) << ValidateAssetOpLedger(comp);
}

TEST(AssetOpLedgerTest, LedgerBitIndexBoundaries) {
    PlayerAssetOpLedgerComp comp;
    auto& ledger = MutableAssetOpStream(comp, kStream);
    // 位 0 = watermark + 1 = seq 1;位 1023 = watermark + 1024 = seq 1024。
    ASSERT_NO_FATAL_FAILURE(RecordOrFail(ledger, 1, AssetOpRecordKind::kApplied));
    EXPECT_EQ(SeenWord(ledger, 0), 1ULL);
    ASSERT_NO_FATAL_FAILURE(RecordOrFail(ledger, kAssetOpWindowBits, AssetOpRecordKind::kRejected, kRejectReason));
    EXPECT_EQ(SeenWord(ledger, 15), 1ULL << 63);
    EXPECT_EQ(ledger.applied_bits(15), 0ULL);

    EXPECT_EQ(Classify(ledger, 1), AssetOpSeqState::kApplied);
    EXPECT_EQ(Classify(ledger, kAssetOpWindowBits), AssetOpSeqState::kRejected);
    EXPECT_EQ(Classify(ledger, 2), AssetOpSeqState::kUnseen);
    EXPECT_EQ(Classify(ledger, kAssetOpWindowBits - 1), AssetOpSeqState::kUnseen);
    for (int word = 1; word < 15; ++word) EXPECT_EQ(SeenWord(ledger, word), 0ULL) << "word=" << word;
}

// ---------------------------------------------------------------------------
// 窗口滑动
// ---------------------------------------------------------------------------

TEST(AssetOpLedgerTest, LedgerSlideByOne) {
    PlayerAssetOpLedgerComp comp;
    auto& ledger = MutableAssetOpStream(comp, kStream);
    for (uint64_t seq = 1; seq <= kAssetOpWindowBits; ++seq) {
        ASSERT_NO_FATAL_FAILURE(RecordOrFail(ledger, seq, AssetOpRecordKind::kApplied));
    }
    EXPECT_EQ(Classify(ledger, kAssetOpWindowBits + 1), AssetOpSeqState::kAheadOfWindow);
    ASSERT_NO_FATAL_FAILURE(RecordOrFail(ledger, kAssetOpWindowBits + 1, AssetOpRecordKind::kApplied));

    EXPECT_EQ(ledger.watermark(), 1u);
    EXPECT_EQ(ledger.max_seq(), kAssetOpWindowBits + 1);
    EXPECT_EQ(Classify(ledger, 1), AssetOpSeqState::kBehindWindow);
    EXPECT_EQ(Classify(ledger, 2), AssetOpSeqState::kApplied);
    EXPECT_EQ(Classify(ledger, kAssetOpWindowBits), AssetOpSeqState::kApplied);
    EXPECT_EQ(Classify(ledger, kAssetOpWindowBits + 1), AssetOpSeqState::kApplied);
    EXPECT_TRUE(ValidateAssetOpLedger(comp).empty()) << ValidateAssetOpLedger(comp);
}

TEST(AssetOpLedgerTest, LedgerSlideExactly1024) {
    PlayerAssetOpLedgerComp comp;
    auto& ledger = MutableAssetOpStream(comp, kStream);
    // 先记 1024 把 max_seq 抬到窗口上沿,跳号上限才允许下一步直接跳到 2048。
    ASSERT_NO_FATAL_FAILURE(RecordOrFail(ledger, kAssetOpWindowBits, AssetOpRecordKind::kApplied));
    EXPECT_EQ(Classify(ledger, 2 * kAssetOpWindowBits), AssetOpSeqState::kAheadOfWindow);
    ASSERT_NO_FATAL_FAILURE(RecordOrFail(ledger, 2 * kAssetOpWindowBits, AssetOpRecordKind::kApplied));

    EXPECT_EQ(ledger.watermark(), kAssetOpWindowBits);
    EXPECT_EQ(Classify(ledger, 1), AssetOpSeqState::kBehindWindow);
    EXPECT_EQ(Classify(ledger, kAssetOpWindowBits), AssetOpSeqState::kBehindWindow);
    EXPECT_EQ(Classify(ledger, 2 * kAssetOpWindowBits), AssetOpSeqState::kApplied);
    // 滑动正好 1024 位:旧位全部丢弃,只剩新记的最高位。
    EXPECT_EQ(SeenWord(ledger, 15), 1ULL << 63);
    for (int word = 0; word < 15; ++word) EXPECT_EQ(SeenWord(ledger, word), 0ULL) << "word=" << word;
}

TEST(AssetOpLedgerTest, LedgerSlideBeyondWindowClears) {
    PlayerAssetOpLedgerComp comp;
    auto& ledger = MutableAssetOpStream(comp, kStream);
    ASSERT_NO_FATAL_FAILURE(RecordOrFail(ledger, 1, AssetOpRecordKind::kApplied));
    // 真实流里 max_seq <= watermark + 1024 恒成立,跳号上限让 shift > 1024 不可达;
    // 这里手工抬高 max_seq,专门覆盖"整窗清空"这条防御分支。
    ledger.set_max_seq(4000);
    EXPECT_EQ(Classify(ledger, 5000), AssetOpSeqState::kAheadOfWindow);
    ASSERT_NO_FATAL_FAILURE(RecordOrFail(ledger, 5000, AssetOpRecordKind::kApplied));

    EXPECT_EQ(ledger.watermark(), 3976u);  // 5000 - 1024
    EXPECT_EQ(Classify(ledger, 1), AssetOpSeqState::kBehindWindow);
    EXPECT_EQ(Classify(ledger, 5000), AssetOpSeqState::kApplied);
    EXPECT_EQ(SeenWord(ledger, 15), 1ULL << 63);
    for (int word = 0; word < 15; ++word) EXPECT_EQ(SeenWord(ledger, word), 0ULL) << "word=" << word;
}

TEST(AssetOpLedgerTest, LedgerRejectionsPrunedOnSlide) {
    PlayerAssetOpLedgerComp comp;
    auto& ledger = MutableAssetOpStream(comp, kStream);
    ASSERT_NO_FATAL_FAILURE(RecordOrFail(ledger, 2, AssetOpRecordKind::kRejected, kRejectReason));
    ASSERT_NO_FATAL_FAILURE(RecordOrFail(ledger, kAssetOpWindowBits, AssetOpRecordKind::kApplied));
    ASSERT_EQ(ledger.rejections_size(), 1);

    // 滑 6 位:seq 2 的拒绝原因滑出窗口被删,seq 1024 的位跟着搬到新下标 1017。
    ASSERT_NO_FATAL_FAILURE(RecordOrFail(ledger, kAssetOpWindowBits + 6, AssetOpRecordKind::kApplied));
    EXPECT_EQ(ledger.watermark(), 6u);
    EXPECT_EQ(ledger.rejections_size(), 0);
    EXPECT_EQ(AssetOpRejectionReason(ledger, 2), 0u);
    EXPECT_EQ(Classify(ledger, 2), AssetOpSeqState::kBehindWindow);
    EXPECT_EQ(Classify(ledger, kAssetOpWindowBits), AssetOpSeqState::kApplied);
    EXPECT_EQ(Classify(ledger, kAssetOpWindowBits + 6), AssetOpSeqState::kApplied);
    EXPECT_TRUE(ValidateAssetOpLedger(comp).empty()) << ValidateAssetOpLedger(comp);
}

TEST(AssetOpLedgerTest, LedgerRejectionRingCap) {
    PlayerAssetOpLedgerComp comp;
    auto& ledger = MutableAssetOpStream(comp, kStream);
    for (uint64_t seq = 1; seq <= 70; ++seq) {
        ASSERT_NO_FATAL_FAILURE(
            RecordOrFail(ledger, seq, AssetOpRecordKind::kRejected, kRejectReason + static_cast<uint32_t>(seq)));
    }
    ASSERT_EQ(ledger.rejections_size(), kAssetOpMaxRejections);
    EXPECT_EQ(ledger.rejections(0).seq(), 7u);
    EXPECT_EQ(ledger.rejections(kAssetOpMaxRejections - 1).seq(), 70u);
    for (int i = 1; i < ledger.rejections_size(); ++i) {
        EXPECT_LT(ledger.rejections(i - 1).seq(), ledger.rejections(i).seq()) << "i=" << i;
    }
    // 被挤掉的只是展示用的原因,结局本身仍然固定。
    // 注意"只是展示"不等于"没有后果":跨语言那一侧的后果由下面
    // LedgerEvictedRejectionReasonDegradesByDesign 单独钉。
    EXPECT_EQ(AssetOpRejectionReason(ledger, 1), 0u);
    EXPECT_EQ(Classify(ledger, 1), AssetOpSeqState::kRejected);
    EXPECT_EQ(AssetOpRejectionReason(ledger, 7), kRejectReason + 7);
}

// 中止占位(reason 0)不占原因环的名额。
// 占位没有业务原因可展示,进不进环查出来都是 0;占一格却会把真·业务拒绝更早挤出去,
// 而挤出去是**有**代价的(见下一条用例)。所以这一格不给它。
TEST(AssetOpLedgerTest, LedgerAbortPlaceholderNotInRejectionRing) {
    PlayerAssetOpLedgerComp comp;
    auto& ledger = MutableAssetOpStream(comp, kStream);
    ASSERT_NO_FATAL_FAILURE(RecordOrFail(ledger, 1, AssetOpRecordKind::kRejected, 0));  // 中止占位
    ASSERT_NO_FATAL_FAILURE(RecordOrFail(ledger, 2, AssetOpRecordKind::kRejected, kRejectReason));

    ASSERT_EQ(ledger.rejections_size(), 1);
    EXPECT_EQ(ledger.rejections(0).seq(), 2u);
    // 不进环**不影响**占位的效力:这条 seq 此后永远是拒绝,晚到的扣款再也应用不了(I2)。
    EXPECT_EQ(Classify(ledger, 1), AssetOpSeqState::kRejected);
    EXPECT_EQ(AssetOpRejectionReason(ledger, 1), 0u);
    EXPECT_EQ(AssetOpRejectionReason(ledger, 2), kRejectReason);
    EXPECT_TRUE(ValidateAssetOpLedger(comp).empty()) << ValidateAssetOpLedger(comp);

    // 63 个中止占位夹在中间,也挤不掉任何一条业务拒绝:环里正好攒满 64 条真原因,
    // 最早的 seq 2 仍在。若占位也进环,这里会变成 127 条抢 64 格,seq 2 早被挤掉。
    for (uint64_t seq = 3; seq <= 128; ++seq) {
        const bool abortPlaceholder = (seq % 2 == 1);
        ASSERT_NO_FATAL_FAILURE(RecordOrFail(ledger, seq, AssetOpRecordKind::kRejected,
                                             abortPlaceholder ? 0 : kRejectReason + static_cast<uint32_t>(seq)));
    }
    ASSERT_EQ(ledger.rejections_size(), kAssetOpMaxRejections);
    EXPECT_EQ(ledger.rejections(0).seq(), 2u);
    EXPECT_EQ(ledger.rejections(kAssetOpMaxRejections - 1).seq(), 128u);
    EXPECT_EQ(AssetOpRejectionReason(ledger, 2), kRejectReason);
    EXPECT_EQ(AssetOpRejectionReason(ledger, 128), kRejectReason + 128);
    EXPECT_EQ(AssetOpRejectionReason(ledger, 127), 0u);  // 占位
    EXPECT_EQ(Classify(ledger, 127), AssetOpSeqState::kRejected);
    EXPECT_TRUE(ValidateAssetOpLedger(comp).empty()) << ValidateAssetOpLedger(comp);
}

// **这条用例钉的是一个刻意接受的退化,不是 bug,看到它失败不要"修"原因环。**
//
// 原因环挤满后重查被挤掉的那条业务拒绝,AssetOpRejectionReason 回 0,scene 答
// REJECTED + reason 0 —— 与中止占位逐字不可区分。Go 的 FinalStatus
// (go/shared/assetop/decide.go)对「RPCAbort + reason==0」判 StatusAborted,
// 于是一条余额不足会以 ABORTED 终结。
// 之所以接受:账务两者完全相同(B5 对 REJECTED / ABORTED 都是退次数、退帮贡、退限购,
// docs/design/guild-phase2/05-economy.md 的处置表),唯一损失是订单文案。
// 为这一行文案加 proto 字段要改协议 + 存档 + 两侧代码,不值 —— 完整代价换算写在
// asset_op_ledger.cpp 的 InsertRejection 与 decide.go 的 FinalStatus 上,两边成对。
TEST(AssetOpLedgerTest, LedgerEvictedRejectionReasonDegradesByDesign) {
    PlayerAssetOpLedgerComp comp;
    auto& ledger = MutableAssetOpStream(comp, kStream);
    // 65 条真·业务拒绝:正好把最早的 seq 1 挤出环。
    for (uint64_t seq = 1; seq <= static_cast<uint64_t>(kAssetOpMaxRejections) + 1; ++seq) {
        ASSERT_NO_FATAL_FAILURE(
            RecordOrFail(ledger, seq, AssetOpRecordKind::kRejected, kRejectReason + static_cast<uint32_t>(seq)));
    }
    ASSERT_EQ(ledger.rejections_size(), kAssetOpMaxRejections);
    ASSERT_EQ(ledger.rejections(0).seq(), 2u);

    // 结局这一半**不退化**:仍然是拒绝,永远不会翻成应用。退的只有原因。
    EXPECT_EQ(Classify(ledger, 1), AssetOpSeqState::kRejected);
    EXPECT_EQ(AssetOpRejectionReason(ledger, 1), 0u);

    // 与中止占位不可区分,正是这一点让 Go 把它判成 ABORTED:两个 seq 答复完全一致。
    ASSERT_NO_FATAL_FAILURE(RecordOrFail(ledger, 200, AssetOpRecordKind::kRejected, 0));  // 中止占位
    EXPECT_EQ(Classify(ledger, 200), Classify(ledger, 1));
    EXPECT_EQ(AssetOpRejectionReason(ledger, 200), AssetOpRejectionReason(ledger, 1));
    EXPECT_TRUE(ValidateAssetOpLedger(comp).empty()) << ValidateAssetOpLedger(comp);
}

TEST(AssetOpLedgerTest, LedgerRejectionInsertedOutOfOrderStaysSorted) {
    PlayerAssetOpLedgerComp comp;
    auto& ledger = MutableAssetOpStream(comp, kStream);
    ASSERT_NO_FATAL_FAILURE(RecordOrFail(ledger, 10, AssetOpRecordKind::kRejected, kRejectReason + 10));
    ASSERT_NO_FATAL_FAILURE(RecordOrFail(ledger, 3, AssetOpRecordKind::kRejected, kRejectReason + 3));
    ASSERT_NO_FATAL_FAILURE(RecordOrFail(ledger, 7, AssetOpRecordKind::kRejected, kRejectReason + 7));
    ASSERT_EQ(ledger.rejections_size(), 3);
    EXPECT_EQ(ledger.rejections(0).seq(), 3u);
    EXPECT_EQ(ledger.rejections(1).seq(), 7u);
    EXPECT_EQ(ledger.rejections(2).seq(), 10u);
    EXPECT_EQ(AssetOpRejectionReason(ledger, 7), kRejectReason + 7);
    EXPECT_TRUE(ValidateAssetOpLedger(comp).empty()) << ValidateAssetOpLedger(comp);
}

// ---------------------------------------------------------------------------
// 部分发放
// ---------------------------------------------------------------------------

TEST(AssetOpLedgerTest, LedgerPartialRecorded) {
    PlayerAssetOpLedgerComp comp;
    auto& ledger = MutableAssetOpStream(comp, kStream);
    ASSERT_NO_FATAL_FAILURE(RecordOrFail(ledger, 1, AssetOpRecordKind::kAppliedPartial));
    ASSERT_NO_FATAL_FAILURE(RecordOrFail(ledger, 2, AssetOpRecordKind::kApplied));

    // 部分发放在位图上等价于已应用(已终结),只是另外进名单。
    EXPECT_EQ(Classify(ledger, 1), AssetOpSeqState::kApplied);
    EXPECT_EQ(Classify(ledger, 2), AssetOpSeqState::kApplied);
    EXPECT_TRUE(IsAssetOpPartial(ledger, 1));
    EXPECT_FALSE(IsAssetOpPartial(ledger, 2));
    ASSERT_EQ(ledger.partial_seqs_size(), 1);
    EXPECT_EQ(ledger.partial_seqs(0), 1u);
}

// §4.33 点名的 `LedgerPartialSeqsCapAndValidate` 按关注点拆成两个用例:环上限 + 滑窗剪枝在
// 这里,Validate 判据在下面的 `LedgerValidatePartialSeqs`,断言合起来与规格等价。
TEST(AssetOpLedgerTest, LedgerPartialRingCapAndPrune) {
    PlayerAssetOpLedgerComp comp;
    auto& ledger = MutableAssetOpStream(comp, kStream);
    for (uint64_t seq = 1; seq <= 70; ++seq) {
        ASSERT_NO_FATAL_FAILURE(RecordOrFail(ledger, seq, AssetOpRecordKind::kAppliedPartial));
    }
    ASSERT_EQ(ledger.partial_seqs_size(), kAssetOpMaxPartialSeqs);
    EXPECT_EQ(ledger.partial_seqs(0), 7u);
    EXPECT_EQ(ledger.partial_seqs(kAssetOpMaxPartialSeqs - 1), 70u);
    // 被挤掉的 seq 1 重查会答 partial = false。这是 §4.33 规定的行为,不是本层的漏判:
    // 兜底在首次答复 + Go 行上粘着的 last_reason(见 asset_op_ledger.h kAssetOpMaxPartialSeqs)。
    EXPECT_FALSE(IsAssetOpPartial(ledger, 1));
    EXPECT_TRUE(IsAssetOpPartial(ledger, 7));

    // 滑窗把 <= 新 watermark(70)的名单条目全部清掉。
    ASSERT_NO_FATAL_FAILURE(RecordOrFail(ledger, 70 + kAssetOpWindowBits, AssetOpRecordKind::kApplied));
    EXPECT_EQ(ledger.watermark(), 70u);
    EXPECT_EQ(ledger.partial_seqs_size(), 0);
    EXPECT_TRUE(ValidateAssetOpLedger(comp).empty()) << ValidateAssetOpLedger(comp);
}

// ---------------------------------------------------------------------------
// 前置与边界
// ---------------------------------------------------------------------------

TEST(AssetOpLedgerTest, LedgerRecordPreconditionFails) {
    PlayerAssetOpLedgerComp comp;
    auto& ledger = MutableAssetOpStream(comp, kStream);
    ASSERT_NO_FATAL_FAILURE(RecordOrFail(ledger, 3, AssetOpRecordKind::kApplied));
    ASSERT_NO_FATAL_FAILURE(RecordOrFail(ledger, 5, AssetOpRecordKind::kRejected, kRejectReason));
    const std::string before = ledger.SerializeAsString();

    // 已见 seq 不得改写结局(I2);非法入参、纪元回退、跳号过远同样一律拒绝且不改状态。
    EXPECT_FALSE(RecordAssetOpOutcome(ledger, kEpoch, 3, AssetOpRecordKind::kRejected, kRejectReason));
    EXPECT_FALSE(RecordAssetOpOutcome(ledger, kEpoch, 5, AssetOpRecordKind::kApplied, 0));
    EXPECT_FALSE(RecordAssetOpOutcome(ledger, kEpoch, 0, AssetOpRecordKind::kApplied, 0));
    EXPECT_FALSE(RecordAssetOpOutcome(ledger, 0, 9, AssetOpRecordKind::kApplied, 0));
    EXPECT_FALSE(RecordAssetOpOutcome(ledger, kEpoch - 1, 9, AssetOpRecordKind::kApplied, 0));
    EXPECT_FALSE(RecordAssetOpOutcome(ledger, kEpoch, 5 + kAssetOpMaxSeqJump + 1, AssetOpRecordKind::kApplied, 0));
    EXPECT_FALSE(RecordAssetOpOutcome(ledger, kEpoch, 9, AssetOpRecordKind::kCount, 0));
    EXPECT_EQ(ledger.SerializeAsString(), before);

    // 滑出窗口的 seq 也不得再记(kBehindWindow)。
    ASSERT_NO_FATAL_FAILURE(RecordOrFail(ledger, kAssetOpWindowBits + 5, AssetOpRecordKind::kApplied));
    ASSERT_EQ(ledger.watermark(), 5u);
    EXPECT_EQ(Classify(ledger, 3), AssetOpSeqState::kBehindWindow);
    EXPECT_FALSE(RecordAssetOpOutcome(ledger, kEpoch, 3, AssetOpRecordKind::kApplied, 0));
}

TEST(AssetOpLedgerTest, LedgerWatermarkOverflowInvalid) {
    PlayerAssetOpLedgerComp comp;
    auto* raw = AddRawStream(comp, kStream, kEpoch);
    raw->set_watermark(kUint64Max - 1000);
    raw->set_max_seq(kUint64Max - 1000);

    // 注意:这里 watermark 和 max_seq 一起抬高,先被 watermark 守卫判 kInvalid,
    // 走不到 ExceedsJumpCap 的溢出分支;那条分支由 LedgerJumpCapNearUint64Max 单独压。
    EXPECT_EQ(Classify(*raw, kUint64Max - 999), AssetOpSeqState::kInvalid);
    EXPECT_EQ(Classify(*raw, 1), AssetOpSeqState::kInvalid);
    EXPECT_FALSE(RecordAssetOpOutcome(*raw, kEpoch, kUint64Max - 999, AssetOpRecordKind::kApplied, 0));
    EXPECT_FALSE(ValidateAssetOpLedger(comp).empty());
}

// 与 go/shared/assetop/classify_test.go 的用例 "max_seq 接近溢出时上限不可表示,不算跳号"
// (classify_test.go:70 的 nearOverflow)成对:max_seq + 1024 不可表示时,C++ ExceedsJumpCap
// 与 Go ClassifySeq 必须站同一侧(都不判跳号)。两边结论相反的话,同一份账本会出现
// "Go 判 JumpTooFar 回 UNKNOWN、scene 判 AheadOfWindow 照常记账"的撕裂,资产操作永久卡住。
TEST(AssetOpLedgerTest, LedgerJumpCapNearUint64Max) {
    PlayerAssetOpLedgerComp comp;
    auto* raw = AddRawStream(comp, kStream, kEpoch);
    raw->set_watermark(0);
    raw->set_max_seq(kUint64Max - 1);

    // 这本账本按 ValidateAssetOpLedger 的判据是**合法**的(watermark 没接近溢出、
    // max_seq >= watermark),不会在加载时被 fail-closed,所以溢出分支确实可达。
    // 真实流里到不了这个状态(max_seq <= watermark + 1024),只可能来自改库或存档损坏。
    ASSERT_TRUE(ValidateAssetOpLedger(comp).empty()) << ValidateAssetOpLedger(comp);

    // 上限 max_seq + 1024 不可表示 → 一律不判跳号,退回按窗口分类。
    EXPECT_EQ(Classify(*raw, 2000), AssetOpSeqState::kAheadOfWindow);
    EXPECT_EQ(Classify(*raw, kUint64Max), AssetOpSeqState::kAheadOfWindow);
    // 窗口内的 seq 仍按位图答,不受 max_seq 影响(这里顺带压到第 1023 位)。
    EXPECT_EQ(Classify(*raw, kAssetOpWindowBits), AssetOpSeqState::kUnseen);

    // 守卫边界:max_seq 正好是 kUint64Max - 1024 时上限可表示(恰为 kUint64Max),
    // 走的是非溢出分支,任何 seq 都不超上限 —— 若加法绕回会错判成 kJumpTooFar。
    raw->set_max_seq(kUint64Max - kAssetOpMaxSeqJump);
    EXPECT_EQ(Classify(*raw, kUint64Max), AssetOpSeqState::kAheadOfWindow);
    EXPECT_EQ(Classify(*raw, 2000), AssetOpSeqState::kAheadOfWindow);
}

// 上一条用例从 Go↔C++ 对齐的角度压 ExceedsJumpCap;这一条只压守卫那一行自己的边界,
// 把 max_seq 三个相邻取值排成一张表。为什么值得单列:那行守卫写错了也不会有人发现 ——
// 少了它,max_seq + 1024 会静默绕回成一个很小的数,于是一个**巨大的** seq 反而被判成
// "没超跳号上限"而被受理;账本随后按它滑窗,结局张冠李戴且全程零报错。
//
// 读表时注意一个反直觉点:max_seq == kUint64Max - 1024 时上限恰为 kUint64Max,
// **没有任何 uint64 能超过它**,所以想在"上限仍可表示"这一侧断言 kJumpTooFar,
// 必须再往下挪一格取 kUint64Max - 1025(上限 = kUint64Max - 1)。
TEST(AssetOpLedgerTest, LedgerJumpCapOverflowGuardBoundary) {
    PlayerAssetOpLedgerComp comp;
    auto* raw = AddRawStream(comp, kStream, kEpoch);
    raw->set_watermark(0);

    // ① 刚好踏进不可表示区间的第一个 max_seq:守卫生效,任何 seq 都不判跳号。
    raw->set_max_seq(kUint64Max - kAssetOpMaxSeqJump + 1);
    ASSERT_TRUE(ValidateAssetOpLedger(comp).empty()) << ValidateAssetOpLedger(comp);  // 这本账本加载得进来
    EXPECT_EQ(Classify(*raw, kUint64Max), AssetOpSeqState::kAheadOfWindow);
    EXPECT_EQ(Classify(*raw, kUint64Max - kAssetOpMaxSeqJump), AssetOpSeqState::kAheadOfWindow);
    EXPECT_EQ(Classify(*raw, kAssetOpWindowBits), AssetOpSeqState::kUnseen);  // 窗口内仍按位图答

    // ② 区间另一侧的最后一个 max_seq:上限恰为 kUint64Max,同样无 seq 可超(见上文说明)。
    raw->set_max_seq(kUint64Max - kAssetOpMaxSeqJump);
    EXPECT_EQ(Classify(*raw, kUint64Max), AssetOpSeqState::kAheadOfWindow);

    // ③ 再往下一格:上限 = kUint64Max - 1,这才是能真正观测到 kJumpTooFar 的位置。
    //    等于上限不算超,超一个才算 —— 加法若绕回,这两行会一起翻。
    raw->set_max_seq(kUint64Max - kAssetOpMaxSeqJump - 1);
    EXPECT_EQ(Classify(*raw, kUint64Max), AssetOpSeqState::kJumpTooFar);
    EXPECT_EQ(Classify(*raw, kUint64Max - 1), AssetOpSeqState::kAheadOfWindow);
}

// 钉 asset_op_ledger.cpp 里"Classify 已排除 watermark 接近溢出的账本,下面的
// watermark + 1024 不会绕回"这句被断言成立、却没人守着的前提。它有**两条腿**:
//   - 同纪元:Classify 的 watermark 守卫判 kInvalid,记账被前置挡住(LedgerWatermarkOverflowInvalid);
//   - 纪元更大:Classify **不看** watermark(直接按空账本答 kUnseen),前提改由记账内部的
//     整条流重置兜住 —— 重置把 watermark 归 0,之后那句加法才安全。这条腿就是本用例。
// 任一条腿断了,那句加法都会静默绕回:巨大的 seq 被算成"在窗口内",位下标成垃圾值,
// 结局落到别的 seq 的位上,且没有任何报错。
TEST(AssetOpLedgerTest, LedgerHigherEpochResetsWatermarkNearOverflow) {
    PlayerAssetOpLedgerComp comp;
    auto* raw = AddRawStream(comp, kStream, kEpoch);
    raw->set_watermark(kUint64Max - 1000);  // 已接近溢出:同纪元下 Classify 判 kInvalid
    raw->set_max_seq(kUint64Max - 1000);
    ASSERT_EQ(Classify(*raw, 1), AssetOpSeqState::kInvalid);

    // 纪元更大:绕过 watermark 守卫,按空账本判 kUnseen,于是前置通过、真的会去记账。
    ASSERT_EQ(Classify(*raw, kAssetOpWindowBits, 200), AssetOpSeqState::kUnseen);
    ASSERT_TRUE(RecordAssetOpOutcome(*raw, 200, kAssetOpWindowBits, AssetOpRecordKind::kApplied, 0));

    // 记账前先整条流重置,所以那句加法看到的是 watermark = 0,不是 kUint64Max - 1000。
    EXPECT_EQ(raw->watermark(), 0u);
    EXPECT_EQ(raw->max_seq(), kAssetOpWindowBits);
    EXPECT_EQ(raw->stream_epoch(), 200u);
    EXPECT_EQ(Classify(*raw, kAssetOpWindowBits, 200), AssetOpSeqState::kApplied);
    EXPECT_TRUE(ValidateAssetOpLedger(comp).empty()) << ValidateAssetOpLedger(comp);
}

TEST(AssetOpLedgerTest, LedgerStreamsSortedUnique) {
    PlayerAssetOpLedgerComp comp;
    MutableAssetOpStream(comp, ASSET_OP_STREAM_SYSTEM_CREDIT);  // 5
    MutableAssetOpStream(comp, ASSET_OP_STREAM_GUILD_DEBIT);    // 1
    MutableAssetOpStream(comp, ASSET_OP_STREAM_TRADE_DEBIT);    // 3
    ASSERT_EQ(comp.streams_size(), 3);
    EXPECT_EQ(comp.streams(0).stream(), ASSET_OP_STREAM_GUILD_DEBIT);
    EXPECT_EQ(comp.streams(1).stream(), ASSET_OP_STREAM_TRADE_DEBIT);
    EXPECT_EQ(comp.streams(2).stream(), ASSET_OP_STREAM_SYSTEM_CREDIT);

    // 重复 Mutable 不新增,且拿到的是同一条(改了 watermark 能读回来)。
    MutableAssetOpStream(comp, ASSET_OP_STREAM_SYSTEM_CREDIT).set_max_seq(42);
    EXPECT_EQ(comp.streams_size(), 3);
    EXPECT_EQ(MutableAssetOpStream(comp, ASSET_OP_STREAM_SYSTEM_CREDIT).max_seq(), 42u);
    ASSERT_NE(FindAssetOpStream(comp, ASSET_OP_STREAM_SYSTEM_CREDIT), nullptr);
    EXPECT_EQ(FindAssetOpStream(comp, ASSET_OP_STREAM_SYSTEM_CREDIT)->max_seq(), 42u);

    // 新建的流位图是 16 + 16 个 0、纪元 0(纪元由首次 Record 写上)。
    const auto& fresh = comp.streams(1);
    EXPECT_EQ(fresh.seen_bits_size(), kAssetOpWindowWords);
    EXPECT_EQ(fresh.applied_bits_size(), kAssetOpWindowWords);
    EXPECT_EQ(fresh.stream_epoch(), 0u);
}

// ---------------------------------------------------------------------------
// 流纪元与跳号上限(§4.30)
// ---------------------------------------------------------------------------

TEST(AssetOpLedgerTest, EpochZeroInvalid) {
    PlayerAssetOpLedgerComp comp;
    auto& ledger = MutableAssetOpStream(comp, kStream);
    ASSERT_NO_FATAL_FAILURE(RecordOrFail(ledger, 1, AssetOpRecordKind::kApplied));
    EXPECT_EQ(Classify(ledger, 1, 0), AssetOpSeqState::kInvalid);
    EXPECT_EQ(ClassifyAssetOpSeq(nullptr, 0, 1), AssetOpSeqState::kInvalid);
}

TEST(AssetOpLedgerTest, EpochHigherResets) {
    PlayerAssetOpLedgerComp comp;
    auto& ledger = MutableAssetOpStream(comp, kStream);
    for (uint64_t seq = 1; seq <= 3; ++seq) {
        ASSERT_NO_FATAL_FAILURE(RecordOrFail(ledger, seq, AssetOpRecordKind::kRejected, kRejectReason));
    }
    ASSERT_EQ(ledger.rejections_size(), 3);

    // 新流水簿:同一个 seq 1 是另一笔业务,整条流重置后按未见处理。
    EXPECT_EQ(Classify(ledger, 1, 200), AssetOpSeqState::kUnseen);
    ASSERT_NO_FATAL_FAILURE(RecordOrFail(ledger, 1, AssetOpRecordKind::kApplied, 0, 200));
    EXPECT_EQ(ledger.watermark(), 0u);
    EXPECT_EQ(ledger.max_seq(), 1u);
    EXPECT_EQ(ledger.stream_epoch(), 200u);
    EXPECT_EQ(ledger.rejections_size(), 0);
    EXPECT_EQ(Classify(ledger, 1, 200), AssetOpSeqState::kApplied);
    EXPECT_EQ(Classify(ledger, 2, 200), AssetOpSeqState::kUnseen);
    EXPECT_TRUE(ValidateAssetOpLedger(comp).empty()) << ValidateAssetOpLedger(comp);
}

TEST(AssetOpLedgerTest, EpochHigherFarSeqJump) {
    PlayerAssetOpLedgerComp comp;
    auto& ledger = MutableAssetOpStream(comp, kStream);
    ASSERT_NO_FATAL_FAILURE(RecordOrFail(ledger, 1, AssetOpRecordKind::kApplied));
    const std::string before = ledger.SerializeAsString();

    // 新纪元按空账本看,1025 超过 0 + 1024:拒绝且**不得**先把账本重置掉。
    EXPECT_EQ(Classify(ledger, kAssetOpWindowBits + 1, 200), AssetOpSeqState::kJumpTooFar);
    EXPECT_FALSE(RecordAssetOpOutcome(ledger, 200, kAssetOpWindowBits + 1, AssetOpRecordKind::kApplied, 0));
    EXPECT_EQ(ledger.SerializeAsString(), before);
    EXPECT_EQ(ledger.stream_epoch(), kEpoch);
    EXPECT_EQ(Classify(ledger, 1), AssetOpSeqState::kApplied);
}

TEST(AssetOpLedgerTest, EpochStale) {
    PlayerAssetOpLedgerComp comp;
    auto& ledger = MutableAssetOpStream(comp, kStream);
    ASSERT_NO_FATAL_FAILURE(RecordOrFail(ledger, 1, AssetOpRecordKind::kApplied, 0, 200));
    EXPECT_EQ(Classify(ledger, 1, kEpoch), AssetOpSeqState::kStaleEpoch);
    EXPECT_EQ(Classify(ledger, 9, kEpoch), AssetOpSeqState::kStaleEpoch);
    EXPECT_FALSE(RecordAssetOpOutcome(ledger, kEpoch, 9, AssetOpRecordKind::kApplied, 0));
    EXPECT_EQ(ledger.stream_epoch(), 200u);
}

TEST(AssetOpLedgerTest, JumpCapSameEpoch) {
    PlayerAssetOpLedgerComp comp;
    auto& ledger = MutableAssetOpStream(comp, kStream);
    ASSERT_NO_FATAL_FAILURE(RecordOrFail(ledger, 10, AssetOpRecordKind::kApplied));
    ASSERT_EQ(ledger.watermark(), 0u);
    ASSERT_EQ(ledger.max_seq(), 10u);

    EXPECT_EQ(Classify(ledger, 1024), AssetOpSeqState::kUnseen);          // 仍在窗口内
    EXPECT_EQ(Classify(ledger, 1034), AssetOpSeqState::kAheadOfWindow);   // 10 + 1024
    EXPECT_EQ(Classify(ledger, 1035), AssetOpSeqState::kJumpTooFar);
    EXPECT_FALSE(RecordAssetOpOutcome(ledger, kEpoch, 1035, AssetOpRecordKind::kApplied, 0));
    EXPECT_EQ(ledger.max_seq(), 10u);
}

// ---------------------------------------------------------------------------
// 加载校验(fail-closed)
// ---------------------------------------------------------------------------

TEST(AssetOpLedgerTest, LedgerValidateOk) {
    PlayerAssetOpLedgerComp comp = BuildValidComp();
    EXPECT_TRUE(ValidateAssetOpLedger(comp).empty()) << ValidateAssetOpLedger(comp);
    EXPECT_TRUE(ValidateAssetOpLedger(PlayerAssetOpLedgerComp()).empty());  // 没有流也合法
}

TEST(AssetOpLedgerTest, LedgerValidateBitWordCount) {
    PlayerAssetOpLedgerComp comp = BuildValidComp();
    comp.mutable_streams(0)->mutable_seen_bits()->RemoveLast();  // 只剩 15 个
    EXPECT_FALSE(ValidateAssetOpLedger(comp).empty());

    PlayerAssetOpLedgerComp extra = BuildValidComp();
    extra.mutable_streams(0)->add_applied_bits(0);  // 17 个
    EXPECT_FALSE(ValidateAssetOpLedger(extra).empty());
}

TEST(AssetOpLedgerTest, LedgerValidateAppliedNotSubsetOfSeen) {
    PlayerAssetOpLedgerComp comp = BuildValidComp();
    comp.mutable_streams(0)->set_applied_bits(0, kUint64Max);
    EXPECT_FALSE(ValidateAssetOpLedger(comp).empty());
}

TEST(AssetOpLedgerTest, LedgerValidateDuplicateStream) {
    PlayerAssetOpLedgerComp comp = BuildValidComp();
    AddRawStream(comp, kStream, kEpoch);
    EXPECT_FALSE(ValidateAssetOpLedger(comp).empty());
}

TEST(AssetOpLedgerTest, LedgerValidateStreamOrderAndRange) {
    PlayerAssetOpLedgerComp unordered;
    AddRawStream(unordered, ASSET_OP_STREAM_SYSTEM_CREDIT, kEpoch);
    AddRawStream(unordered, ASSET_OP_STREAM_GUILD_DEBIT, kEpoch);
    EXPECT_FALSE(ValidateAssetOpLedger(unordered).empty());

    PlayerAssetOpLedgerComp zero;
    AddRawStream(zero, ASSET_OP_STREAM_UNSPECIFIED, kEpoch);
    EXPECT_FALSE(ValidateAssetOpLedger(zero).empty());

    // proto3 开放枚举:不认识的值会原样留在字段里,必须判损坏。判据走 AssetOpStream_IsValid,
    // 所以将来 proto 追加合法流时这条仍只拦"真的不在枚举里"的值,不会误伤新流。
    PlayerAssetOpLedgerComp unknown;
    AddRawStream(unknown, static_cast<AssetOpStream>(99), kEpoch);
    EXPECT_FALSE(ValidateAssetOpLedger(unknown).empty());

    // 枚举当前的最大合法流本身必须通过(写死上界的写法会在这里退化成"新流一律损坏")。
    PlayerAssetOpLedgerComp maxStream;
    {
        auto& ledger = MutableAssetOpStream(maxStream, static_cast<AssetOpStream>(AssetOpStream_MAX));
        ASSERT_NO_FATAL_FAILURE(RecordOrFail(ledger, 1, AssetOpRecordKind::kApplied));
    }
    EXPECT_TRUE(ValidateAssetOpLedger(maxStream).empty()) << ValidateAssetOpLedger(maxStream);
}

TEST(AssetOpLedgerTest, LedgerValidateRejectionOutOfWindow) {
    PlayerAssetOpLedgerComp behind = BuildValidComp();
    auto* rejection = behind.mutable_streams(0)->mutable_rejections(0);
    rejection->set_seq(0);  // <= watermark(0)
    EXPECT_FALSE(ValidateAssetOpLedger(behind).empty());

    PlayerAssetOpLedgerComp ahead = BuildValidComp();
    ahead.mutable_streams(0)->mutable_rejections(0)->set_seq(kAssetOpWindowBits + 1);
    EXPECT_FALSE(ValidateAssetOpLedger(ahead).empty());
}

TEST(AssetOpLedgerTest, LedgerValidateRejectionOnAppliedBit) {
    PlayerAssetOpLedgerComp comp = BuildValidComp();
    // seq 3 是 APPLIED,把拒绝条目指过去 → 位不是"已见且未应用"。
    comp.mutable_streams(0)->mutable_rejections(0)->set_seq(3);
    EXPECT_FALSE(ValidateAssetOpLedger(comp).empty());

    // 指到一个从没见过的 seq 也要判损坏。
    PlayerAssetOpLedgerComp unseen = BuildValidComp();
    unseen.mutable_streams(0)->mutable_rejections(0)->set_seq(4);
    EXPECT_FALSE(ValidateAssetOpLedger(unseen).empty());
}

TEST(AssetOpLedgerTest, LedgerValidateRejectionsNotSorted) {
    PlayerAssetOpLedgerComp comp;
    auto& ledger = MutableAssetOpStream(comp, kStream);
    RecordAssetOpOutcome(ledger, kEpoch, 3, AssetOpRecordKind::kRejected, kRejectReason);
    RecordAssetOpOutcome(ledger, kEpoch, 5, AssetOpRecordKind::kRejected, kRejectReason);
    ASSERT_EQ(ledger.rejections_size(), 2);
    ledger.mutable_rejections()->SwapElements(0, 1);
    EXPECT_FALSE(ValidateAssetOpLedger(comp).empty());
}

// §4.33 `LedgerPartialSeqsCapAndValidate` 的后半(见 `LedgerPartialRingCapAndPrune` 的说明)。
TEST(AssetOpLedgerTest, LedgerValidatePartialSeqs) {
    PlayerAssetOpLedgerComp notApplied = BuildValidComp();
    notApplied.mutable_streams(0)->set_partial_seqs(0, 5);  // seq 5 是 REJECTED
    EXPECT_FALSE(ValidateAssetOpLedger(notApplied).empty());

    PlayerAssetOpLedgerComp outOfWindow = BuildValidComp();
    outOfWindow.mutable_streams(0)->set_partial_seqs(0, kAssetOpWindowBits + 9);
    EXPECT_FALSE(ValidateAssetOpLedger(outOfWindow).empty());
}

TEST(AssetOpLedgerTest, ValidateEpochZero) {
    PlayerAssetOpLedgerComp comp = BuildValidComp();
    comp.mutable_streams(0)->set_stream_epoch(0);
    EXPECT_FALSE(ValidateAssetOpLedger(comp).empty());

    // 建了流却没记账就存盘,同样判损坏 —— MutableAssetOpStream 只能在确定要记账时调用。
    PlayerAssetOpLedgerComp fresh;
    MutableAssetOpStream(fresh, kStream);
    EXPECT_FALSE(ValidateAssetOpLedger(fresh).empty());
}

TEST(AssetOpLedgerTest, LedgerValidateMaxSeqBelowWatermark) {
    PlayerAssetOpLedgerComp comp = BuildValidComp();
    auto* ledger = comp.mutable_streams(0);
    ledger->set_watermark(100);
    ledger->set_max_seq(99);
    EXPECT_FALSE(ValidateAssetOpLedger(comp).empty());
}

TEST(AssetOpLedgerTest, LedgerResetForEpochClearsEverything) {
    PlayerAssetOpLedgerComp comp = BuildValidComp();
    auto& ledger = *comp.mutable_streams(0);
    ResetAssetOpStreamForEpoch(ledger, 777);

    EXPECT_EQ(ledger.stream(), kStream);  // 流号不变
    EXPECT_EQ(ledger.stream_epoch(), 777u);
    EXPECT_EQ(ledger.watermark(), 0u);
    EXPECT_EQ(ledger.max_seq(), 0u);
    EXPECT_EQ(ledger.rejections_size(), 0);
    EXPECT_EQ(ledger.partial_seqs_size(), 0);
    ASSERT_EQ(ledger.seen_bits_size(), kAssetOpWindowWords);
    ASSERT_EQ(ledger.applied_bits_size(), kAssetOpWindowWords);
    for (int word = 0; word < kAssetOpWindowWords; ++word) {
        EXPECT_EQ(ledger.seen_bits(word), 0ULL) << "word=" << word;
        EXPECT_EQ(ledger.applied_bits(word), 0ULL) << "word=" << word;
    }
    EXPECT_TRUE(ValidateAssetOpLedger(comp).empty()) << ValidateAssetOpLedger(comp);
}

// ---------------------------------------------------------------------------
// I5 守卫的机器证明:未决 seq 永远在窗口内,新分配的 seq 永远不超跳号上限
// ---------------------------------------------------------------------------

TEST(AssetOpLedgerTest, LedgerGapGuardProof) {
    constexpr int kSteps = 10000;
    constexpr size_t kMaxPending = 16;   // I5:每流未决 <= 16 行
    constexpr uint64_t kMaxSpan = 512;   // I5:next_seq - 最小未决 seq < 512

    PlayerAssetOpLedgerComp comp;
    auto& ledger = MutableAssetOpStream(comp, kStream);

    std::mt19937 rng(20260917);  // 固定种子:单测必须可重复
    uint64_t nextSeq = 1;
    std::map<uint64_t, bool> pending;                    // 已分配未终结的行 → scene 是否已记账
    std::map<uint64_t, AssetOpSeqState> recordedOutcome;  // 记过账的 seq → 当初的结局
    uint64_t allocated = 0;
    uint64_t recorded = 0;

    for (int step = 0; step < kSteps; ++step) {
        switch (rng() % 3) {
            case 0: {  // Go 分配一个新 seq(先过 I5 守卫)
                const bool spanOk = pending.empty() || (nextSeq - pending.begin()->first) < kMaxSpan;
                if (pending.size() >= kMaxPending || !spanOk) break;
                const AssetOpSeqState state = ClassifyAssetOpSeq(&ledger, kEpoch, nextSeq);
                ASSERT_NE(state, AssetOpSeqState::kJumpTooFar)
                    << "step=" << step << " 新分配 seq=" << nextSeq << " max_seq=" << ledger.max_seq();
                ASSERT_NE(state, AssetOpSeqState::kBehindWindow)
                    << "step=" << step << " 新分配 seq=" << nextSeq << " watermark=" << ledger.watermark();
                pending.emplace(nextSeq, false);
                ++nextSeq;
                ++allocated;
                break;
            }
            case 1: {  // scene 处理一条还没记过账的未决行
                std::vector<uint64_t> candidates;
                for (const auto& [seq, done] : pending) {
                    if (!done) candidates.push_back(seq);
                }
                if (candidates.empty()) break;
                const uint64_t seq = candidates[rng() % candidates.size()];
                const AssetOpSeqState before = ClassifyAssetOpSeq(&ledger, kEpoch, seq);
                ASSERT_TRUE(before == AssetOpSeqState::kUnseen || before == AssetOpSeqState::kAheadOfWindow)
                    << "step=" << step << " seq=" << seq << " 状态=" << AssetOpSeqStateName(before);
                const bool applied = (rng() % 2) == 0;
                ASSERT_TRUE(RecordAssetOpOutcome(ledger, kEpoch, seq,
                                                 applied ? AssetOpRecordKind::kApplied : AssetOpRecordKind::kRejected,
                                                 applied ? 0 : kRejectReason))
                    << "step=" << step << " seq=" << seq;
                recordedOutcome[seq] = applied ? AssetOpSeqState::kApplied : AssetOpSeqState::kRejected;
                pending[seq] = true;
                ++recorded;
                break;
            }
            default: {  // Go 终结一条已拿到结局的未决行
                std::vector<uint64_t> candidates;
                for (const auto& [seq, done] : pending) {
                    if (done) candidates.push_back(seq);
                }
                if (candidates.empty()) break;
                pending.erase(candidates[rng() % candidates.size()]);
                break;
            }
        }

        // 不变量:未决行永远查得到(不滑出窗口、不撞跳号上限),已记账的未决行永远回同一结局(I2)。
        for (const auto& [seq, done] : pending) {
            const AssetOpSeqState state = ClassifyAssetOpSeq(&ledger, kEpoch, seq);
            ASSERT_NE(state, AssetOpSeqState::kBehindWindow)
                << "step=" << step << " 未决 seq=" << seq << " watermark=" << ledger.watermark();
            ASSERT_NE(state, AssetOpSeqState::kJumpTooFar) << "step=" << step << " 未决 seq=" << seq;
            if (done) {
                ASSERT_EQ(state, recordedOutcome[seq])
                    << "step=" << step << " 未决 seq=" << seq << " 结局变了:" << AssetOpSeqStateName(state);
            }
        }
    }

    // 确认这轮确实跑出了有意义的规模,不是空转。
    EXPECT_GT(allocated, 500u);
    EXPECT_GT(recorded, 500u);
    EXPECT_TRUE(ValidateAssetOpLedger(comp).empty()) << ValidateAssetOpLedger(comp);
}
