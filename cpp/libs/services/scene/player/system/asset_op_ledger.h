#pragma once

// 通用资产通道账本纯函数(零 ECS / 零表 / 零全局状态 / 零时间依赖,可单测),设计文档
// docs/design/guild-phase2/04-asset-channel.md §4.4(窗口算法)、§4.29(流纪元与跳号上限)。
//
// 这里只回答四个问题,不碰实体、不改资产、不触发存盘:
//   1) 某 (玩家, 流) 上的某个 seq 在这本账本里是什么状态(没见过 / 已扣 / 已拒 /
//      滑出窗口 / 纪元不对 / 跳号过远);
//   2) 一次结局怎么写进账本(含窗口滑动、拒绝原因环、部分发放名单);
//   3) 换了流纪元怎么把整条流重置;
//   4) 从存档读出来的账本是否自洽(损坏就 fail-closed,由调用方挂标签关闭该玩家的资产通道)。
//
// 窗口:每条流一个 1024 位窗口,覆盖 (watermark, watermark + 1024]。
// 位 i(i ∈ [0, 1024))对应 seq = watermark + 1 + i,存在第 i/64 个 fixed64 的第 i%64 位。
// 窗口只跟着"本纪元见过的最大 seq"上滑,不做"连续已见前缀压缩":压缩会把 Go 仍未决、
// scene 已记账的 seq 挤出窗口,Go 重查时就分不清当初是 APPLIED 还是 REJECTED。
//
// 线程模型:纯函数,状态全在入参里,由调用方(scene loop 线程)保证独占访问。
// 单测:cpp/tests/currency_test/asset_op_ledger_test.cpp。

#include <cstdint>
#include <string>

#include "proto/common/component/asset_op_ledger_comp.pb.h"

// 窗口宽度 1024 位 = 16 个 fixed64。
constexpr uint32_t kAssetOpWindowBits = 1024;
constexpr int kAssetOpWindowWords = 16;
static_assert(kAssetOpWindowWords * 64 == static_cast<int>(kAssetOpWindowBits),
              "窗口位数必须正好由 16 个 fixed64 覆盖");

// 拒绝原因环与部分发放名单的条数上限。两者只用于展示与审计:被挤掉的条目会让原因退化成
// 0(Go 按"通用拒绝"映射),不影响"某 seq 是 APPLIED 还是 REJECTED"这个正确性判定。
constexpr int kAssetOpMaxRejections = 64;
constexpr int kAssetOpMaxPartialSeqs = 64;

// 同纪元内允许的跳号上限:seq <= max_seq + 1024。超过一律 fail-closed(调用方回 UNKNOWN、
// 不记账、打 ERROR),证明见 §4.29。它同时兜住"Redis 回退到旧 blob 让 max_seq 变小"的情况。
constexpr uint64_t kAssetOpMaxSeqJump = 1024;

// 某个 (纪元, seq) 落在账本里的状态。最后一项固定为 kCount,同时用作名字表长度(AGENTS §11.2)。
enum class AssetOpSeqState : uint8_t {
    kInvalid,        // seq == 0、epoch == 0,或 watermark 已接近溢出
    kStaleEpoch,     // 请求纪元 < 账本纪元:旧流水簿上的号,结局不可采信
    kBehindWindow,   // seq <= watermark:已滑出窗口,结局不可知
    kUnseen,         // 窗口内未见(含"请求纪元更大、按空账本看"的情况)
    kAheadOfWindow,  // seq > watermark + 1024 但未超跳号上限:必为未见,记账时窗口会上滑
    kJumpTooFar,     // seq > max_seq + 1024(纪元更大时为 seq > 1024)
    kApplied,
    kRejected,
    kCount
};

// 一次结局要往账本里写什么。kAppliedPartial 在位图上与 kApplied 等价(都算"已应用、已终结"),
// 另外把 seq 记进 partial_seqs,供 Go 转人工补偿(§4.33)。
enum class AssetOpRecordKind : uint8_t { kApplied, kAppliedPartial, kRejected, kCount };

// 日志与测试失败信息用;未知值回 "?"。返回 const char*(不是 string_view):muduo 的
// LogStream 只认 const char* / std::string / StringPiece,string_view 进 LOG_ERROR 编不过。
const char* AssetOpSeqStateName(AssetOpSeqState state);

// 流不存在时返回 nullptr。nullptr 在 ClassifyAssetOpSeq 里等价于"纪元 0、watermark 0 的空账本"。
const AssetOpStreamLedger* FindAssetOpStream(const PlayerAssetOpLedgerComp& comp, AssetOpStream stream);

// 缺则按 stream 升序插入一条空账本(watermark 0、max_seq 0、两组位各 16 个 0、stream_epoch 0)。
// **调用约定**:只在确定接下来要 RecordAssetOpOutcome 时调用。新建出来的流 stream_epoch = 0,
// 要靠 Record 内部的纪元重置写上真实纪元;若建了流却没记账就存盘,下次加载时
// ValidateAssetOpLedger 会因 "stream_epoch = 0" 判损坏并对该玩家 fail-closed。
AssetOpStreamLedger& MutableAssetOpStream(PlayerAssetOpLedgerComp& comp, AssetOpStream stream);

// 分类顺序固定(§4.29 表格):
//   1) seq == 0 或 epoch == 0                     → kInvalid
//   2) epoch < 账本纪元                            → kStaleEpoch
//      epoch > 账本纪元                            → 按空账本看:seq <= 1024 ? kUnseen : kJumpTooFar
//   3) watermark > UINT64_MAX - 1024              → kInvalid(防溢出)
//   4) seq <= watermark                           → kBehindWindow
//      seq > max_seq + 1024                       → kJumpTooFar
//      seq > watermark + 1024                     → kAheadOfWindow
//   5) 看位:未置位 → kUnseen;applied 位 1 → kApplied;否则 kRejected
AssetOpSeqState ClassifyAssetOpSeq(const AssetOpStreamLedger* ledger, uint64_t epoch, uint64_t seq);

// 查窗口内某个被拒 seq 的原因 tip id;查不到(不在环里 / 被挤掉 / 不是拒绝)回 0。
uint32_t AssetOpRejectionReason(const AssetOpStreamLedger& ledger, uint64_t seq);

// seq 是否在"部分发放"名单里。前提是该 seq 已判为 kApplied,否则无意义。
bool IsAssetOpPartial(const AssetOpStreamLedger& ledger, uint64_t seq);

// 整条流重置到新纪元:watermark 0、两组位各 16 个 0、rejections 与 partial_seqs 清空、
// max_seq 0、stream_epoch = epoch。**只应由 RecordAssetOpOutcome 在确定要记账时调用**;
// 闸门回 RETRY 时不得重置,否则已见结局会凭空消失(违反 I2 结局固定)。
void ResetAssetOpStreamForEpoch(AssetOpStreamLedger& ledger, uint64_t epoch);

// 记一次结局。前置:ClassifyAssetOpSeq(&ledger, epoch, seq) ∈ {kUnseen, kAheadOfWindow},
// 且 kind != kCount。前置不满足时返回 false 且**不改动任何状态**(调用方 LOG_ERROR)。
// 通过前置后:epoch 更大则先整条流重置;必要时窗口上滑并清掉滑出窗口的原因/部分发放条目;
// 再置位、写名单、抬 max_seq。reasonTipId 只对 kRejected 有意义(0 = 中止占位)。
bool RecordAssetOpOutcome(AssetOpStreamLedger& ledger, uint64_t epoch, uint64_t seq,
                          AssetOpRecordKind kind, uint32_t reasonTipId);

// 加载校验。返回空串 = 通过;非空串是可直接进日志的损坏原因。
// 未上线无存量数据,损坏只可能来自 bug,处理是 fail-closed:调用方挂
// PlayerAssetOpLedgerInvalidComp,该玩家所有资产 RPC 回 RETRY + kAssetBlocked,**不改写**原数据。
std::string ValidateAssetOpLedger(const PlayerAssetOpLedgerComp& comp);
