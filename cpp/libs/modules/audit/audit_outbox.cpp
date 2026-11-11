#include "audit_outbox.h"

#include <deque>
#include <unordered_map>
#include <utility>
#include <variant>

#include <muduo/base/Logging.h>

namespace
{
    // 每条记录在 SavePlayerRequest 的 repeated 字段里多出的字段头:1 字节 tag + 至多 3 字节长度(单条不超过 64 KiB)。
    constexpr std::size_t kFieldOverheadBytes = 4;
    // 溢出一旦开始,这名玩家之后每记一条就要丢一条;第一条和之后每这么多条打一次 ERROR,不刷屏。
    constexpr uint64_t kOverflowLogEveryN = 1000;

    using Record = std::variant<TransactionLogEntry, PlayerSnapshotEntry>;

    struct QueuedRecord
    {
        // 进程内单调的水位:存盘快照记的是取快照那一刻的水位,水位不大于它的记录描述的改动都在那份快照里。
        uint64_t mark{0};
        std::size_t bytes{0};
        Record record;
    };

    struct PlayerQueue
    {
        std::deque<QueuedRecord> records;
        std::size_t bytes{0};
    };

    struct State
    {
        bool enabled{false};
        // 全进程(本线程)一条数轴,不按玩家分:实体销毁重建之后水位也不会回头,旧快照的水位盖不到新记录。
        uint64_t lastMark{0};
        std::unordered_map<uint64_t, PlayerQueue> players;
        uint64_t overflowDropped{0};
    };

    State &Outbox()
    {
        thread_local State state;
        return state;
    }

    const char *KindName(const Record &record)
    {
        return std::holds_alternative<TransactionLogEntry>(record) ? "transaction_log" : "player_snapshot";
    }

    uint64_t RecordId(const Record &record)
    {
        if (const auto *entry = std::get_if<TransactionLogEntry>(&record))
        {
            return entry->tx_id();
        }
        return std::get<PlayerSnapshotEntry>(record).snapshot_id();
    }

    // 记录已经核过非空、未超单条上限。单条上限小于每人字节上限,所以丢到队空一定放得下。
    void Push(uint64_t playerId, std::size_t bytes, Record record)
    {
        auto &state = Outbox();
        auto &queue = state.players[playerId];
        while (!queue.records.empty() &&
               (queue.records.size() >= AuditOutbox::kMaxRecordsPerPlayer || queue.bytes + bytes > AuditOutbox::kMaxBytesPerPlayer))
        {
            const auto &oldest = queue.records.front();
            if (state.overflowDropped++ % kOverflowLogEveryN == 0)
            {
                LOG_ERROR << "AuditOutbox: player " << playerId << " has " << queue.records.size() << " audit records ("
                          << queue.bytes << " bytes) waiting for a save commit; dropping the oldest " << KindName(oldest.record)
                          << " record_id=" << RecordId(oldest.record)
                          << " instead of letting one save carry more than the transport budget"
                          << " (dropped_total=" << state.overflowDropped << ", metric=audit_outbox_overflow)";
            }
            queue.bytes -= oldest.bytes;
            queue.records.pop_front();
        }
        queue.records.push_back(QueuedRecord{++state.lastMark, bytes, std::move(record)});
        queue.bytes += bytes;
    }

    bool Admit(uint64_t playerId, uint64_t recordId, std::size_t serializedBytes, const char *kind)
    {
        if (!Outbox().enabled || playerId == 0 || recordId == 0)
        {
            return false;
        }
        if (serializedBytes + kFieldOverheadBytes > AuditOutbox::kMaxRecordBytes)
        {
            LOG_ERROR << "AuditOutbox: refusing a " << kind << " record of player " << playerId << " record_id=" << recordId
                      << ": " << serializedBytes << " bytes exceeds the per-record limit " << AuditOutbox::kMaxRecordBytes
                      << " (metric=audit_outbox_record_too_large)";
            return false;
        }
        return true;
    }
} // namespace

void AuditOutbox::Enable(bool enabled)
{
    Outbox().enabled = enabled;
}

bool AuditOutbox::Enabled()
{
    return Outbox().enabled;
}

uint64_t AuditOutbox::Mark()
{
    return Outbox().lastMark;
}

bool AuditOutbox::EnqueueTransactionLog(uint64_t playerId, TransactionLogEntry entry)
{
    const std::size_t bytes = entry.ByteSizeLong();
    if (!Admit(playerId, entry.tx_id(), bytes, "transaction_log"))
    {
        return false;
    }
    Push(playerId, bytes + kFieldOverheadBytes, Record{std::move(entry)});
    return true;
}

bool AuditOutbox::EnqueueSnapshot(uint64_t playerId, PlayerSnapshotEntry request)
{
    const std::size_t bytes = request.ByteSizeLong();
    if (!Admit(playerId, request.snapshot_id(), bytes, "player_snapshot"))
    {
        return false;
    }
    Push(playerId, bytes + kFieldOverheadBytes, Record{std::move(request)});
    return true;
}

bool AuditOutbox::HasDue(uint64_t playerId, uint64_t mark)
{
    const auto &players = Outbox().players;
    const auto it = players.find(playerId);
    // 队列按水位递增(入队时水位单调),看队头就够。
    return it != players.end() && !it->second.records.empty() && it->second.records.front().mark <= mark;
}

void AuditOutbox::AttachDue(uint64_t playerId, uint64_t mark,
                            google::protobuf::RepeatedPtrField<TransactionLogEntry> &transactionLogs,
                            google::protobuf::RepeatedPtrField<PlayerSnapshotEntry> &snapshots)
{
    const auto &players = Outbox().players;
    const auto it = players.find(playerId);
    if (it == players.end())
    {
        return;
    }
    for (const auto &queued : it->second.records)
    {
        if (queued.mark > mark)
        {
            break;
        }
        if (const auto *entry = std::get_if<TransactionLogEntry>(&queued.record))
        {
            *transactionLogs.Add() = *entry;
        }
        else
        {
            *snapshots.Add() = std::get<PlayerSnapshotEntry>(queued.record);
        }
    }
}

void AuditOutbox::AckThrough(uint64_t playerId, uint64_t mark)
{
    auto &players = Outbox().players;
    const auto it = players.find(playerId);
    if (it == players.end())
    {
        return;
    }
    auto &queue = it->second;
    while (!queue.records.empty() && queue.records.front().mark <= mark)
    {
        queue.bytes -= queue.records.front().bytes;
        queue.records.pop_front();
    }
    if (queue.records.empty())
    {
        players.erase(it);
    }
}

std::size_t AuditOutbox::Discard(uint64_t playerId, const char *reason)
{
    auto &players = Outbox().players;
    const auto it = players.find(playerId);
    if (it == players.end())
    {
        return 0;
    }
    const std::size_t count = it->second.records.size();
    players.erase(it);
    LOG_WARN << "AuditOutbox: dropping " << count << " audit record" << (count == 1 ? "" : "s") << " of player " << playerId
             << " whose changes were never committed (" << (reason != nullptr ? reason : "")
             << ", metric=audit_outbox_discarded_uncommitted)";
    return count;
}

std::size_t AuditOutbox::PendingCount(uint64_t playerId)
{
    const auto &players = Outbox().players;
    const auto it = players.find(playerId);
    return it == players.end() ? 0 : it->second.records.size();
}

std::size_t AuditOutbox::PendingBytes(uint64_t playerId)
{
    const auto &players = Outbox().players;
    const auto it = players.find(playerId);
    return it == players.end() ? 0 : it->second.bytes;
}
