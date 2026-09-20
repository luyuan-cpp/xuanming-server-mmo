#include "asset_op_ledger.h"

#include <array>
#include <iterator>
#include <limits>

namespace {

constexpr uint64_t kUint64Max = (std::numeric_limits<uint64_t>::max)();

constexpr const char* kAssetOpSeqStateNames[] = {
    "Invalid", "StaleEpoch", "BehindWindow", "Unseen", "AheadOfWindow", "JumpTooFar", "Applied", "Rejected",
};
static_assert(std::size(kAssetOpSeqStateNames) == static_cast<size_t>(AssetOpSeqState::kCount),
              "名字表必须和 AssetOpSeqState 逐项对齐");

// 账本里两组位图的选择子。用枚举而不是 bool 参数,避免调用点出现看不懂的 true/false。
enum class BitGroup : uint8_t { kSeen, kApplied };

// 合法流 = proto 当前枚举里除 UNSPECIFIED 之外的任意一个;0 不得落进存档。
// 判据跟着生成的 AssetOpStream_IsValid 走,不写死 [1, 5]:枚举是会长的(§4.1 的 5 条已是
// 第二轮追加的结果)。写死上界的话,追加第 6 条流既不会编译报错也没有任何提示,持有新流账本
// 的玩家一加载就被判损坏 → 挂 PlayerAssetOpLedgerInvalidComp → 资产通道对他永久 fail-closed。
bool IsAssetOpStreamValid(AssetOpStream stream) {
    return stream != ASSET_OP_STREAM_UNSPECIFIED && AssetOpStream_IsValid(static_cast<int>(stream));
}

// 越界一律当 0 读;真正的长度不合法由 ValidateAssetOpLedger 在加载时 fail-closed。
uint64_t ReadWord(const AssetOpStreamLedger& ledger, BitGroup group, int word) {
    if (word < 0) return 0;
    if (group == BitGroup::kSeen) {
        return word < ledger.seen_bits_size() ? ledger.seen_bits(word) : 0;
    }
    return word < ledger.applied_bits_size() ? ledger.applied_bits(word) : 0;
}

void WriteWord(AssetOpStreamLedger& ledger, BitGroup group, int word, uint64_t value) {
    if (word < 0) return;
    if (group == BitGroup::kSeen) {
        if (word < ledger.seen_bits_size()) ledger.set_seen_bits(word, value);
        return;
    }
    if (word < ledger.applied_bits_size()) ledger.set_applied_bits(word, value);
}

bool TestBit(const AssetOpStreamLedger& ledger, BitGroup group, uint32_t index) {
    return ((ReadWord(ledger, group, static_cast<int>(index / 64)) >> (index % 64)) & 1ULL) != 0ULL;
}

void SetBit(AssetOpStreamLedger& ledger, BitGroup group, uint32_t index) {
    const int word = static_cast<int>(index / 64);
    WriteWord(ledger, group, word, ReadWord(ledger, group, word) | (1ULL << (index % 64)));
}

// 只补不截:短了补 0 让后续置位有地方落;长了是损坏,留给 ValidateAssetOpLedger 报出来,
// 这里不静默截断(AGENTS §11.3 不吞错)。
void EnsureWindowWords(AssetOpStreamLedger& ledger) {
    while (ledger.seen_bits_size() < kAssetOpWindowWords) ledger.add_seen_bits(0);
    while (ledger.applied_bits_size() < kAssetOpWindowWords) ledger.add_applied_bits(0);
}

void ClearWindowBits(AssetOpStreamLedger& ledger) {
    for (int word = 0; word < kAssetOpWindowWords; ++word) {
        WriteWord(ledger, BitGroup::kSeen, word, 0);
        WriteWord(ledger, BitGroup::kApplied, word, 0);
    }
}

// 两组位各自作为 1024 位大整数整体右移 shift 位(低位丢弃):新的位 i = 旧的位 i + shift。
// 位 i 对应 seq = watermark + 1 + i,watermark 上滑 shift 之后正好是这个映射。
// 前置:0 < shift < 1024(shift >= 1024 走 ClearWindowBits,否则 64 - bitShift 会退化)。
void ShiftGroupRight(AssetOpStreamLedger& ledger, BitGroup group, uint64_t shift) {
    const int wordShift = static_cast<int>(shift / 64);
    const uint64_t bitShift = shift % 64;
    std::array<uint64_t, kAssetOpWindowWords> shifted{};
    for (int word = 0; word < kAssetOpWindowWords; ++word) {
        const int src = word + wordShift;
        uint64_t value = src < kAssetOpWindowWords ? ReadWord(ledger, group, src) : 0;
        value >>= bitShift;  // bitShift ∈ [0, 64),不会触发移位 UB
        if (bitShift != 0 && src + 1 < kAssetOpWindowWords) {
            value |= ReadWord(ledger, group, src + 1) << (64 - bitShift);
        }
        shifted[word] = value;
    }
    for (int word = 0; word < kAssetOpWindowWords; ++word) WriteWord(ledger, group, word, shifted[word]);
}

// 删掉列表最前面的 count 条。两个列表(rejections 是 RepeatedPtrField,partial_seqs 是
// RepeatedField)只共用 size / SwapElements / RemoveLast 这三个接口,用模板避免把
// protobuf 的容器拼写钉进本文件。条数 <= 64,代价可忽略。
template <class List>
void RemoveFirst(List& list, int count) {
    for (int removed = 0; removed < count; ++removed) {
        if (list.size() <= 0) return;
        for (int i = 0; i + 1 < list.size(); ++i) list.SwapElements(i, i + 1);
        list.RemoveLast();
    }
}

// 只收**真·业务拒绝**(reasonTipId != 0);中止占位不进来,理由见调用点。
//
// **明文契约:环溢出后原因退化成 0,这是已知且被接受的损失,不是待修缺陷。**
// 环只有 64 格,挤满后被挤掉的 seq 再被重查时 AssetOpRejectionReason 回 0,scene 答
// REJECTED + reason 0;Go 的 FinalStatus(go/shared/assetop/decide.go)对
// 「RPCAbort + reason == 0」判 StatusAborted,于是一条余额不足会以 ABORTED 终结。
// 可以接受的依据是**账务两者完全相同**:B5 对 REJECTED 与 ABORTED 都是退次数、退帮贡、
// 退限购(docs/design/guild-phase2/05-economy.md 的「REJECTED / ABORTED」处置表),
// 唯一损失是订单文案从「余额不足 / 背包已满」退成「已中止」。
// 代价换算:要消掉这一行文案损失,得在 AssetOpResponse 上加一个「是否中止占位」的字段
// —— 改协议、改存档、改两侧代码,换一行展示文字,不值,所以**不加**。
// 结局判定(APPLIED / REJECTED)只看位图,任何时候都不受本环影响。
// 用例:asset_op_ledger_test.cpp 的 LedgerEvictedRejectionReasonDegradesByDesign。
void InsertRejection(AssetOpStreamLedger& ledger, uint64_t seq, uint32_t reasonTipId) {
    auto* list = ledger.mutable_rejections();
    int pos = list->size();
    for (int i = 0; i < list->size(); ++i) {
        if (list->Get(i).seq() > seq) {
            pos = i;
            break;
        }
    }
    auto* added = list->Add();
    added->set_seq(seq);
    added->set_reason_tip_id(reasonTipId);
    for (int i = list->size() - 1; i > pos; --i) list->SwapElements(i, i - 1);
    // 超过上限丢最小的一条:原因只用于展示,丢掉最早的不影响 APPLIED/REJECTED 的判定。
    if (list->size() > kAssetOpMaxRejections) RemoveFirst(*list, list->size() - kAssetOpMaxRejections);
}

void InsertPartialSeq(AssetOpStreamLedger& ledger, uint64_t seq) {
    auto* list = ledger.mutable_partial_seqs();
    int pos = list->size();
    for (int i = 0; i < list->size(); ++i) {
        if (list->Get(i) > seq) {
            pos = i;
            break;
        }
    }
    list->Add(seq);
    for (int i = list->size() - 1; i > pos; --i) list->SwapElements(i, i - 1);
    // 超过上限丢最小的一条(§4.33 规定的行为)。与 rejections 不同,丢掉之后重查会答
    // partial = false;为什么仍可接受见 asset_op_ledger.h 里 kAssetOpMaxPartialSeqs 的注释。
    if (list->size() > kAssetOpMaxPartialSeqs) RemoveFirst(*list, list->size() - kAssetOpMaxPartialSeqs);
}

// 窗口上滑之后,两个列表里 seq <= 新 watermark 的条目已经不可查询,删掉。列表升序,删的是前缀。
void PruneBelowWatermark(AssetOpStreamLedger& ledger, uint64_t watermark) {
    int drop = 0;
    while (drop < ledger.rejections_size() && ledger.rejections(drop).seq() <= watermark) ++drop;
    RemoveFirst(*ledger.mutable_rejections(), drop);

    drop = 0;
    while (drop < ledger.partial_seqs_size() && ledger.partial_seqs(drop) <= watermark) ++drop;
    RemoveFirst(*ledger.mutable_partial_seqs(), drop);
}

// seq 是否超出同纪元跳号上限。max_seq 接近 uint64 上限时上限不可表示,此时任何 seq 都不算超。
// 这条"不可表示就不算跳号"是 Go↔C++ 的契约,Go 侧同判据在 go/shared/assetop/classify.go:116;
// 两边必须站同一侧,否则同一份账本会出现"一边回 UNKNOWN、一边照常记账"的撕裂。
// 单测:LedgerJumpCapNearUint64Max(C++)/ TestClassifySeqTable 的 nearOverflow 用例(Go)。
bool ExceedsJumpCap(uint64_t maxSeq, uint64_t seq) {
    if (maxSeq > kUint64Max - kAssetOpMaxSeqJump) return false;
    return seq > maxSeq + kAssetOpMaxSeqJump;
}

std::string StreamPrefix(const AssetOpStreamLedger& ledger, int index) {
    return "streams[" + std::to_string(index) + "] stream=" + std::to_string(static_cast<int>(ledger.stream())) + " ";
}

// 单条流的自洽校验;返回空串 = 通过。
std::string ValidateStreamLedger(const AssetOpStreamLedger& ledger, int index) {
    const std::string prefix = StreamPrefix(ledger, index);
    if (!IsAssetOpStreamValid(ledger.stream())) return prefix + "不是合法 AssetOpStream";
    if (ledger.stream_epoch() == 0) return prefix + "stream_epoch = 0";
    if (ledger.seen_bits_size() != kAssetOpWindowWords) {
        return prefix + "seen_bits 数量=" + std::to_string(ledger.seen_bits_size()) + " 应为 16";
    }
    if (ledger.applied_bits_size() != kAssetOpWindowWords) {
        return prefix + "applied_bits 数量=" + std::to_string(ledger.applied_bits_size()) + " 应为 16";
    }
    for (int word = 0; word < kAssetOpWindowWords; ++word) {
        if ((ledger.applied_bits(word) & ~ledger.seen_bits(word)) != 0ULL) {
            return prefix + "applied 不是 seen 的子集,word=" + std::to_string(word);
        }
    }

    const uint64_t watermark = ledger.watermark();
    if (watermark > kUint64Max - kAssetOpWindowBits) return prefix + "watermark 接近溢出";
    if (ledger.max_seq() < watermark) {
        return prefix + "max_seq=" + std::to_string(ledger.max_seq()) + " < watermark=" + std::to_string(watermark);
    }

    if (ledger.rejections_size() > kAssetOpMaxRejections) {
        return prefix + "rejections 数量=" + std::to_string(ledger.rejections_size()) + " 超过 64";
    }
    uint64_t previous = 0;
    for (int i = 0; i < ledger.rejections_size(); ++i) {
        const uint64_t seq = ledger.rejections(i).seq();
        if (i > 0 && seq <= previous) return prefix + "rejections 未严格升序,seq=" + std::to_string(seq);
        previous = seq;
        if (seq <= watermark || seq > watermark + kAssetOpWindowBits) {
            return prefix + "rejections 越窗,seq=" + std::to_string(seq);
        }
        const uint32_t bit = static_cast<uint32_t>(seq - watermark - 1);
        if (!TestBit(ledger, BitGroup::kSeen, bit) || TestBit(ledger, BitGroup::kApplied, bit)) {
            return prefix + "rejections 对应位不是'已见且未应用',seq=" + std::to_string(seq);
        }
    }

    if (ledger.partial_seqs_size() > kAssetOpMaxPartialSeqs) {
        return prefix + "partial_seqs 数量=" + std::to_string(ledger.partial_seqs_size()) + " 超过 64";
    }
    previous = 0;
    for (int i = 0; i < ledger.partial_seqs_size(); ++i) {
        const uint64_t seq = ledger.partial_seqs(i);
        if (i > 0 && seq <= previous) return prefix + "partial_seqs 未严格升序,seq=" + std::to_string(seq);
        previous = seq;
        if (seq <= watermark || seq > watermark + kAssetOpWindowBits) {
            return prefix + "partial_seqs 越窗,seq=" + std::to_string(seq);
        }
        if (!TestBit(ledger, BitGroup::kApplied, static_cast<uint32_t>(seq - watermark - 1))) {
            return prefix + "partial_seqs 对应位不是 applied,seq=" + std::to_string(seq);
        }
    }
    return std::string();
}

}  // namespace

const char* AssetOpSeqStateName(AssetOpSeqState state) {
    const auto index = static_cast<size_t>(state);
    if (index >= std::size(kAssetOpSeqStateNames)) return "?";
    return kAssetOpSeqStateNames[index];
}

const AssetOpStreamLedger* FindAssetOpStream(const PlayerAssetOpLedgerComp& comp, AssetOpStream stream) {
    for (int i = 0; i < comp.streams_size(); ++i) {
        if (comp.streams(i).stream() == stream) return &comp.streams(i);
    }
    return nullptr;
}

AssetOpStreamLedger& MutableAssetOpStream(PlayerAssetOpLedgerComp& comp, AssetOpStream stream) {
    auto* list = comp.mutable_streams();
    for (int i = 0; i < list->size(); ++i) {
        if (list->Get(i).stream() == stream) return *list->Mutable(i);
    }
    auto* added = list->Add();
    added->set_stream(stream);
    added->set_watermark(0);
    added->set_max_seq(0);
    added->set_stream_epoch(0);
    EnsureWindowWords(*added);
    // 按 stream 升序落位。每玩家至多 5 条流,冒泡代价可忽略;好处是存档里顺序稳定,
    // SavePlayerToRedis 那套"与上次落盘快照逐字段相等就不写"的判定不会因顺序抖动误判为变更。
    int pos = list->size() - 1;
    while (pos > 0 && list->Get(pos - 1).stream() > list->Get(pos).stream()) {
        list->SwapElements(pos - 1, pos);
        --pos;
    }
    return *list->Mutable(pos);
}

AssetOpSeqState ClassifyAssetOpSeq(const AssetOpStreamLedger* ledger, uint64_t epoch, uint64_t seq) {
    if (seq == 0 || epoch == 0) return AssetOpSeqState::kInvalid;

    const uint64_t ledgerEpoch = ledger != nullptr ? ledger->stream_epoch() : 0;
    if (epoch < ledgerEpoch) return AssetOpSeqState::kStaleEpoch;
    if (epoch > ledgerEpoch) {
        // 更大的纪元 = 换了一本新流水簿:按空账本(watermark 0、max_seq 0)分类,
        // 真正的重置留到 RecordAssetOpOutcome 里做,免得只是查询就把已见结局抹掉。
        return seq <= kAssetOpMaxSeqJump ? AssetOpSeqState::kUnseen : AssetOpSeqState::kJumpTooFar;
    }
    // 走到这里 epoch == ledgerEpoch 且 epoch != 0,ledger 必然非空(空账本的纪元是 0)。

    const uint64_t watermark = ledger->watermark();
    if (watermark > kUint64Max - kAssetOpWindowBits) return AssetOpSeqState::kInvalid;
    if (seq <= watermark) return AssetOpSeqState::kBehindWindow;
    if (ExceedsJumpCap(ledger->max_seq(), seq)) return AssetOpSeqState::kJumpTooFar;
    if (seq > watermark + kAssetOpWindowBits) return AssetOpSeqState::kAheadOfWindow;

    const uint32_t index = static_cast<uint32_t>(seq - watermark - 1);
    if (!TestBit(*ledger, BitGroup::kSeen, index)) return AssetOpSeqState::kUnseen;
    return TestBit(*ledger, BitGroup::kApplied, index) ? AssetOpSeqState::kApplied : AssetOpSeqState::kRejected;
}

uint32_t AssetOpRejectionReason(const AssetOpStreamLedger& ledger, uint64_t seq) {
    for (int i = 0; i < ledger.rejections_size(); ++i) {
        if (ledger.rejections(i).seq() == seq) return ledger.rejections(i).reason_tip_id();
    }
    return 0;
}

bool IsAssetOpPartial(const AssetOpStreamLedger& ledger, uint64_t seq) {
    for (int i = 0; i < ledger.partial_seqs_size(); ++i) {
        if (ledger.partial_seqs(i) == seq) return true;
    }
    return false;
}

void ResetAssetOpStreamForEpoch(AssetOpStreamLedger& ledger, uint64_t epoch) {
    ledger.set_watermark(0);
    ledger.set_max_seq(0);
    ledger.set_stream_epoch(epoch);
    ledger.clear_seen_bits();
    ledger.clear_applied_bits();
    ledger.clear_rejections();
    ledger.clear_partial_seqs();
    EnsureWindowWords(ledger);
}

bool RecordAssetOpOutcome(AssetOpStreamLedger& ledger, uint64_t epoch, uint64_t seq,
                          AssetOpRecordKind kind, uint32_t reasonTipId) {
    if (kind >= AssetOpRecordKind::kCount) return false;

    // 先对**原账本**做前置检查,再决定要不要重置。顺序反过来的话,前置失败时账本已经被清空,
    // 已见结局会凭空消失(违反 I2 结局固定)。
    const AssetOpSeqState state = ClassifyAssetOpSeq(&ledger, epoch, seq);
    if (state != AssetOpSeqState::kUnseen && state != AssetOpSeqState::kAheadOfWindow) return false;

    if (epoch > ledger.stream_epoch()) ResetAssetOpStreamForEpoch(ledger, epoch);
    EnsureWindowWords(ledger);

    // Classify 已排除 watermark 接近溢出的账本,下面的 watermark + 1024 不会绕回。
    uint64_t watermark = ledger.watermark();
    if (seq > watermark + kAssetOpWindowBits) {
        const uint64_t shift = seq - (watermark + kAssetOpWindowBits);
        if (shift >= kAssetOpWindowBits) {
            ClearWindowBits(ledger);
        } else {
            ShiftGroupRight(ledger, BitGroup::kSeen, shift);
            ShiftGroupRight(ledger, BitGroup::kApplied, shift);
        }
        watermark += shift;
        ledger.set_watermark(watermark);
        PruneBelowWatermark(ledger, watermark);
    }

    const uint32_t index = static_cast<uint32_t>(seq - watermark - 1);
    SetBit(ledger, BitGroup::kSeen, index);
    if (kind == AssetOpRecordKind::kApplied || kind == AssetOpRecordKind::kAppliedPartial) {
        SetBit(ledger, BitGroup::kApplied, index);
    }
    if (kind == AssetOpRecordKind::kAppliedPartial) InsertPartialSeq(ledger, seq);
    // 中止占位(reasonTipId == 0)**不占**原因环的名额:它压根没有业务原因可展示,查它
    // 无论进不进环都回 0;占一格只会把真·业务拒绝更早挤出去,而被挤掉是有代价的
    // (原因退化 → Go 判 ABORTED,见 InsertRejection 上的契约)。中止占位在位图上照常
    // 置 seen、不置 applied,所以 Classify 仍答 kRejected,「此后这条 seq 永远拒绝」不变。
    if (kind == AssetOpRecordKind::kRejected && reasonTipId != 0) InsertRejection(ledger, seq, reasonTipId);
    if (seq > ledger.max_seq()) ledger.set_max_seq(seq);
    return true;
}

std::string ValidateAssetOpLedger(const PlayerAssetOpLedgerComp& comp) {
    uint64_t previousStream = 0;
    for (int i = 0; i < comp.streams_size(); ++i) {
        const auto& ledger = comp.streams(i);
        const uint64_t stream = static_cast<uint64_t>(ledger.stream());
        if (i > 0 && stream <= previousStream) {
            return StreamPrefix(ledger, i) + "流未严格升序(重复或乱序)";
        }
        previousStream = stream;
        if (auto reason = ValidateStreamLedger(ledger, i); !reason.empty()) return reason;
    }
    return std::string();
}
