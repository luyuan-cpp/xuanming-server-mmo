#pragma once

// 服务发现键索引(docs/design/grpc-muduo-transport.md §13.5、§13.9):etcd 发现键 → 最近一次已知的值、mod_revision
// 与持有者(节点 uuid),外加删除墓碑。用途:
//   * DELETE 事件不依赖 prev_kv(开 MVCC 压缩后可能拿不到):按 key 取回删除前的 NodeInfo JSON;
//   * 对账:前缀的完整 Range 快照里没有、且本地记录不比快照新的 key,视为流断期间被删;
//   * 丢掉过时的条目:Range 与 watch 走不同的流,快照可能比已经处理过的 watch 事件旧,反之亦然;
//   * 按持有者计数:同一个 uuid 可能同时挂在两个键上(Go 侧 ReallocateNewID 换号时保留 uuid、换 node_id),
//     发现层按 uuid 摘节点,所以摘之前要确认这个 uuid 已不再持有任何键(HoldsAnyKey)。
//
// 只收发现键(`<Service>.rpc/zone/...`),不收 `/allocated/` 占位键(值是裸 uuid,不是发现数据)。
// 纯数据结构:不碰全局状态、不打日志、不解析值(持有者由调用方解析后传入,解析不了传空串);只在节点 loop 线程使用。
// 前提:etcd 的 revision 单调递增(etcd API 不变量;恢复快照须带 --bump-revision,见设计 §13.9)。

#include <algorithm>
#include <cstdint>
#include <functional>
#include <map>
#include <optional>
#include <string>
#include <string_view>
#include <unordered_map>
#include <unordered_set>
#include <utility>
#include <vector>

class DiscoveryKeyIndex
{
public:
    struct Removed
    {
        std::string key;
        std::string value;
        std::string holder;
    };

    enum class DeleteResult : uint8_t
    {
        kRemoved,     // 有记录,已移除;value / holder 是删除前的
        kUnknownKey,  // 本地没有记录(只记墓碑)
        kStale,       // 本地记录比这次删除还新,或这次删除已经处理过(重放):索引不动,调用方忽略
    };

    struct DeleteOutcome
    {
        DeleteResult result{DeleteResult::kUnknownKey};
        std::string value;
        std::string holder;
    };

    struct PutOutcome
    {
        // false = 比本地已知状态旧(本地有更新的 PUT,或不早于它的删除墓碑),调用方应忽略这一条。
        // 同一版本重复到达(每 2s 的全量 Range)为 true。
        bool accepted{false};
        // 被这次 PUT 覆盖掉的旧值与旧持有者(旧记录存在且值不同才有)。同一个键换了持有者(对端重启拿回同一 node_id)时,
        // 旧持有者再也不会有指向它的事件,调用方要按旧值把它摘掉(先确认它没挂在别的键上),否则留成幽灵节点。
        std::optional<std::string> replacedValue;
        std::string replacedHolder;
    };

    // key 在 modRevision 处的值;holder 是值里的节点 uuid(解析不了传空串)。
    PutOutcome ApplyPut(const std::string &key, const std::string &value, int64_t modRevision,
                        const std::string &holder = {})
    {
        PutOutcome outcome;
        if (const auto tomb = tombstones.find(key); tomb != tombstones.end())
        {
            if (modRevision <= tomb->second)
            {
                return outcome;
            }
            tombstones.erase(tomb);
        }
        const auto [it, inserted] = entries.try_emplace(key);
        if (!inserted && modRevision < it->second.modRevision)
        {
            return outcome;
        }
        if (!inserted)
        {
            if (it->second.value != value)
            {
                outcome.replacedValue = std::move(it->second.value);
                outcome.replacedHolder = it->second.holder;
            }
            DropHolder(it->second.holder);
        }
        it->second.value = value;
        it->second.modRevision = modRevision;
        it->second.holder = holder;
        AddHolder(holder);
        outcome.accepted = true;
        return outcome;
    }

    // key 在 revision(删除事件的 kv.mod_revision)处被删除。
    DeleteOutcome ApplyDelete(const std::string &key, int64_t revision)
    {
        DeleteOutcome outcome;
        const auto it = entries.find(key);
        if (it != entries.end() && it->second.modRevision > revision)
        {
            outcome.result = DeleteResult::kStale;
            return outcome;
        }
        const auto tomb = tombstones.find(key);
        if (it == entries.end() && tomb != tombstones.end() && tomb->second >= revision)
        {
            // 已经处理过不早于它的删除(重建后的 watch 从较早的 revision 回放):再交给发现层只会把宽限期重置一遍。
            outcome.result = DeleteResult::kStale;
            return outcome;
        }
        auto &tombRevision = tombstones[key];
        tombRevision = std::max(tombRevision, revision);
        if (it == entries.end())
        {
            outcome.result = DeleteResult::kUnknownKey;
            return outcome;
        }
        outcome.result = DeleteResult::kRemoved;
        outcome.value = std::move(it->second.value);
        outcome.holder = it->second.holder;
        DropHolder(it->second.holder);
        entries.erase(it);
        return outcome;
    }

    // 前缀的 Range 快照:revision = snapshotRevision(Range 应答的 header.revision),snapshotKeys = 快照里的全部发现键。
    //   * 总是清掉该前缀下 ≤ snapshotRevision 的墓碑(快照已经反映了那些删除);
    //   * complete 时再移除本地有、快照里没有、且 modRevision ≤ snapshotRevision 的记录并返回(按 key 有序),
    //     同时在 snapshotRevision 处给它们记墓碑,挡住之后才到的、更旧的 PUT。
    //     modRevision > snapshotRevision 的是快照之后经 watch 才到的新键,不动。
    // complete = false(more=true / 值解析失败 / 有 key 不属于前缀)时一律不删 —— 宁可多留,不错删。
    std::vector<Removed> ApplySnapshot(std::string_view prefix, const std::unordered_set<std::string> &snapshotKeys,
                                       int64_t snapshotRevision, bool complete)
    {
        for (auto it = tombstones.lower_bound(prefix); it != tombstones.end() && it->first.starts_with(prefix);)
        {
            it = it->second <= snapshotRevision ? tombstones.erase(it) : std::next(it);
        }
        std::vector<Removed> removed;
        if (!complete)
        {
            return removed;
        }
        for (auto it = entries.lower_bound(prefix); it != entries.end() && it->first.starts_with(prefix);)
        {
            if (it->second.modRevision <= snapshotRevision && !snapshotKeys.contains(it->first))
            {
                DropHolder(it->second.holder);
                removed.push_back({it->first, std::move(it->second.value), std::move(it->second.holder)});
                tombstones[it->first] = snapshotRevision;
                it = entries.erase(it);
                continue;
            }
            ++it;
        }
        return removed;
    }

    // 这个持有者(节点 uuid)当前是否还挂在任何发现键上。空串恒为 false(无法判断)。
    bool HoldsAnyKey(const std::string &holder) const { return !holder.empty() && holderKeys.contains(holder); }

    // 没有记录返回 nullptr;指针在下一次修改索引前有效。
    const std::string *FindValue(const std::string &key) const
    {
        const auto it = entries.find(key);
        return it == entries.end() ? nullptr : &it->second.value;
    }

    std::size_t size() const { return entries.size(); }
    std::size_t tombstoneCount() const { return tombstones.size(); }

private:
    struct Entry
    {
        std::string value;
        int64_t modRevision{0};
        std::string holder;
    };

    void AddHolder(const std::string &holder)
    {
        if (!holder.empty())
        {
            ++holderKeys[holder];
        }
    }

    void DropHolder(const std::string &holder)
    {
        if (holder.empty())
        {
            return;
        }
        if (const auto it = holderKeys.find(holder); it != holderKeys.end() && --it->second == 0)
        {
            holderKeys.erase(it);
        }
    }

    // 有序容器:前缀对账用 lower_bound 扫一段。
    std::map<std::string, Entry, std::less<>> entries;
    std::map<std::string, int64_t, std::less<>> tombstones;
    std::unordered_map<std::string, std::size_t> holderKeys; // 持有者 → 它挂着的键数
};
