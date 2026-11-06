#pragma once

#include <chrono>
#include <cstdint>
#include <memory>

#include "proto/common/database/player_cache.pb.h"

// 最近一次确认提交的整份 PlayerAllData,是「已提交」的证据。
//
// 谁读:
//   - 资产通道、战斗结算、收件箱:拿它判断某次改动是否已经落盘,再发确认;committed_owner_epoch / committed_save_seq
//     说明这份是哪个代次、哪一笔提交的;
//   - 存盘:快照上没有当前代次的提交时,哪怕一行都没变也强制写一笔(SubmitPlayerSave 的 options.force),
//     否则上面这些读者等不到证据。
// 判脏不读它:写哪些行由 PlayerStoreClient 逐行和确认提交的字节比较决定。退出收敛也不读它:比的是刚落地的那份
// 和当前内存(player_exit::IsPersistedPayloadCurrent)。
//
// 生命周期:
//   - 第一次确认提交之后才创建(HandlePlayerAsyncSaved,或还有排队快照时的 OnPlayerStoreSaveDone)。
//     加载之后、第一次提交之前没有这个组件。
//   - 之后每次确认提交整份替换;实体销毁时随实体一起清掉。
//
// 代价:
//   - 内存:每个在线玩家一份整份数据,和 ECS 实时数据、PlayerStoreClient 的 confirmed 字节合起来是三份
//     (docs/design/player-data-tidb-scaling-plan.md §4.6「常驻内存」)。读者把它当证据,不能简单去掉。
//   - CPU:Replace 是整份深拷贝,每次确认提交一次。
//
// 用 unique_ptr:第一次提交之前不付分配的代价;组件独占这份数据,别的系统改不到。
struct PlayerLastPersistedSnapshotComp
{
    // The persisted proto; nullptr until the first save completes.
    std::unique_ptr<PlayerAllData> snapshot;

    // Wall-clock millis when `snapshot` was set. Useful for forensics
    // ("how stale is our last-persisted state?") and for telemetry on
    // save-skip cadence. 0 when snapshot is nullptr.
    int64_t saved_at_ms{0};

    // 提交钩子提供的(E,q)。body中的fence仍保持0，last_save_seq仍从dirty快照剥离。
    uint64_t committed_owner_epoch{0};
    uint64_t committed_save_seq{0};

    // 钩子先于全局saved回调；只匹配同一个不可变源对象，旧/合并快照不借新回包补资格。
    // weak不延长异步客户端payload寿命，至多一个候选，下一次实际提交覆盖它。
    void RememberCommit(const std::shared_ptr<const PlayerAllData>& source, uint64_t epoch, uint64_t seq)
    {
        committed_source_ = source;
        source_owner_epoch_ = epoch;
        source_save_seq_ = seq;
    }

    // 整份替换快照(深拷贝)并记下时间。
    //
    // 拷贝之后清掉 last_save_seq(docs/design/db-write-fencing-mysql.md §7.3):它是每次写盘都变的存盘序号,
    // 不是业务数据;这份快照是哪一笔提交的由 committed_save_seq 记。快照的读者(资产 / 结算 / 收件箱账本的
    // 「已落盘」判定)都不读这个字段。
    void Replace(const PlayerAllData& fresh)
    {
        const auto source = committed_source_.lock();
        const bool exact = source && source.get() == &fresh && source_owner_epoch_ != 0
            && source_save_seq_ != 0 && fresh.last_save_seq() == source_save_seq_;
        committed_owner_epoch = exact ? source_owner_epoch_ : 0;
        committed_save_seq = exact ? source_save_seq_ : 0;
        committed_source_.reset(); source_owner_epoch_ = 0; source_save_seq_ = 0;
        if (!snapshot)
        {
            snapshot = std::make_unique<PlayerAllData>(fresh);
        }
        else
        {
            *snapshot = fresh;
        }
        snapshot->clear_last_save_seq();
        saved_at_ms = std::chrono::duration_cast<std::chrono::milliseconds>(
            std::chrono::system_clock::now().time_since_epoch()).count();
    }

    // True iff a snapshot has been recorded (first save completed).
    bool HasSnapshot() const { return snapshot != nullptr; }

private:
    std::weak_ptr<const PlayerAllData> committed_source_;
    uint64_t source_owner_epoch_{0};
    uint64_t source_save_seq_{0};
};
