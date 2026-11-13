#pragma once

#include <cstddef>
#include <cstdint>

#include <google/protobuf/repeated_ptr_field.h>

#include "proto/common/rollback/player_snapshot.pb.h"
#include "proto/common/rollback/transaction_log.pb.h"

// 审计发件箱的 scene 一侧(docs/design/player-audit-outbox.md §5,断线重连 R13)。
//
// 审计记录(流水、快照请求)描述的是玩家存档里的改动,所以它们跟着存盘提交走,而且和存档在同一个事务里落库:
//   - 记下时先进这名玩家的队列,带一个进程内单调的水位;
//   - 存盘状态机取快照时记下当时的水位 M,组请求时把这名玩家水位不大于 M 的记录拷进 SavePlayerRequest(AttachDue),
//     一笔最多带 kMaxBytesPerSave,带不完的由紧接着的下一笔带;
//   - go/db 在提交这笔存档的同一个 TiDB 事务里把它们写进本存档库的发件箱表,再由转发循环投 Kafka;
//   - 这一笔确定提交、go/db 也确认写了发件箱之后,AckThrough 删掉已经带走的记录;
//   - 改动最终没落库(实体销毁且存盘落定、本节点重新认领之前)整队丢掉(Discard)。
// 于是「发件箱里有这条」等于「这笔改动已提交」,Kafka 停多久都不丢。
// 被拒、结果未知时记录留在队列,随下一笔再交;之前的尝试其实已提交时会交两次,由 data_service 按 tx_id / snapshot_guid 去重。
//
// 线程:只在所属逻辑线程上调用,状态是线程局部的;scene 上就是 tlsEcs 所在的那条线程。
// 没开(battle、单测、工具)时流水和快照请求都被拒:这个进程没有存盘,审计记录没有提交的地方
// (transaction_log_system.h、snapshot_system.h)。
class AuditOutbox
{
public:
    // 一笔存盘最多带这么多审计字节(按序列化大小加每条的字段头计)。go/db 的行预算(MaxRequestBytes,缺省 3 MiB)加上它、
    // 再加余量,小于存盘请求的传输上限(PlayerStoreClient::Options::maxRequestBytes),所以审计永远不会让一笔存盘超限被拒。
    // go/db 的 playerstore.MaxAuditBytesPerSave 与它同值,Go 单测读本头文件核对,改一边要改两边。
    static constexpr std::size_t kMaxBytesPerSave = 512 * 1024;
    // 每名玩家最多排这么多条、这么多字节,只为封住内存。排着的记录超过一笔的预算时不丢:AttachDue 按预算分笔交出。
    // 正常情况下流水一入队就请求约 1 秒内存盘,队列里只有一两秒的量;排到这个数说明这名玩家的存盘中断了很久
    // (go/db 或 TiDB 长时间不可用)而他还在不停地产生流水,这时丢最旧的一条并打 ERROR。
    static constexpr std::size_t kMaxRecordsPerPlayer = 65536;
    static constexpr std::size_t kMaxBytesPerPlayer = 8 * 1024 * 1024;
    // 单条上限。超过它的记录是调用方的问题(extra 里塞了大段文本之类),拒收并打 ERROR。
    // go/db 的 playerstore.MaxAuditTransactionLogBytes 与它同值(同一个 Go 单测核对)。
    static constexpr std::size_t kMaxRecordBytes = 64 * 1024;
    // 单条装得进一笔的预算,所以每一笔至少带走一条,分笔一定能交完。
    static_assert(kMaxRecordBytes <= kMaxBytesPerSave);

    // scene 在装配存盘状态机时打开,和放行钩子一起接上(player_lifecycle.cpp 的 MakePlayerStoreOptions)。
    // 关掉时已排队的不动,仍由 AckThrough / Discard 处置。
    static void Enable(bool enabled);
    [[nodiscard]] static bool Enabled();

    // 此刻的水位:已经记下的记录,水位都不大于它。存盘取快照时调用并记在这份快照上。
    [[nodiscard]] static uint64_t Mark();

    // 入队。没开发件箱、playerId 为 0、记录号为 0、单条超限时拒收,返回 false(超限在这里打 ERROR 并计数
    // audit_outbox_stats,其余由调用方报)。
    // 流水只收「只涉及这一名玩家」的(TransactionLogSystem::RecordEntry 判过);zone 已由调用方盖章。
    // 资产变动的「请尽快存盘」通知不在这里发:流水被拒时资产照样变了,通知在 TransactionLogSystem::SetAssetChangedHook。
    static bool EnqueueTransactionLog(uint64_t playerId, TransactionLogEntry entry);
    // 快照请求只带 snapshot_id、trigger、schema_version、zone_id;整档由 go/db 按提交后的行在事务里拼(设计 §6.2)。
    static bool EnqueueSnapshot(uint64_t playerId, PlayerSnapshotEntry request);

    // 这名玩家水位不大于 mark 的记录还有没有。有就算没有脏行也要写一笔(设计 §5.2)。
    [[nodiscard]] static bool HasDue(uint64_t playerId, uint64_t mark);

    // AttachDue 的结果。
    struct Attached
    {
        // 这一笔带走的最后一条记录的水位;一条都没带时是 0。
        uint64_t through{0};
        // 水位不大于 mark 的记录还有没带上的(超过了一笔的预算):这一笔提交之后,由紧接着的下一笔带。
        bool more{false};
    };

    // 把这名玩家水位不大于 mark 的记录按记录顺序追加进两个输出(SavePlayerRequest 的 transaction_logs、audit_snapshots),
    // 合计不超过 kMaxBytesPerSave。队列不动:这一笔确定提交之前它们还要留着,被拒时随下一笔再交。
    static Attached AttachDue(uint64_t playerId, uint64_t mark,
                              google::protobuf::RepeatedPtrField<TransactionLogEntry> &transactionLogs,
                              google::protobuf::RepeatedPtrField<PlayerSnapshotEntry> &snapshots);

    // 水位不大于 mark 的记录已经随存盘写进发件箱:删掉。一笔带完时 mark 是那份快照的水位,没带完时是 Attached::through。
    static void AckThrough(uint64_t playerId, uint64_t mark);

    // 丢掉这名玩家还没交出去的记录,返回条数:它们描述的改动没有落库。reason 只进日志。
    static std::size_t Discard(uint64_t playerId, const char *reason);

    // 这名玩家排着的条数与字节数。日志与单测用。
    [[nodiscard]] static std::size_t PendingCount(uint64_t playerId);
    [[nodiscard]] static std::size_t PendingBytes(uint64_t playerId);
};
