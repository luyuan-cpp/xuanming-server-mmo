#pragma once

#include <cstddef>
#include <unordered_map>
#include <unordered_set>
#include <vector>

#include "entt/src/entt/entity/registry.hpp"
#include "engine/core/type_define/type_define.h"

#include "item_system.h"

// ─────────────────────────────────────────────────────────────────────────
// 实例层(item store)
//
// 物品实例是**领域实体**:有自己的身份(item_uuid,全局唯一、跨堆叠拆合稳定)、
// 自己的生命期(铸号 -> 存在 -> 销毁),以及自己的状态(今天只有 config_id +
// size,proto 里已经预留了强化等级 / 词条 / 镶嵌 / 绑定态 / 限时等字段)。
//
// 这一层只管"有哪些实例、它们各是什么、数量怎么在实例之间分配"。
// 它**不认识槽位、不认识容量、不知道背包满没满** —— 那些全在布局层
// (container_layout.h)。两层的唯一接头是一个 Guid。
//
// 堆叠算术之所以留在这一层而不是布局层,是因为它回答的是"这批数量要拆成
// 几个实例",不是"占几格"。拆分前这两个问题被 GridsNeededFor 这个名字混成
// 了一个,正是两层焊死的症状之一。
// ─────────────────────────────────────────────────────────────────────────

// guid -> 承载该实例的 ECS 实体。
using ItemsMap = std::unordered_map<Guid, entt::entity>;

class ItemStore
{
public:
    // ── 身份与生命期 ──────────────────────────────────────────────────
    [[nodiscard]] std::size_t Size() const { return guidToEntity_.size(); }
    [[nodiscard]] bool Contains(Guid guid) const { return guidToEntity_.contains(guid); }
    [[nodiscard]] entt::entity EntityOf(Guid guid) const;

    ItemComp *Find(Guid guid);
    [[nodiscard]] const ItemComp *Find(Guid guid) const;

    // 当前水位(下一个会盖出的序号)。只读,给用例断言用。
    [[nodiscard]] uint64_t NextAcquireSeq() const { return nextAcquireSeq_; }

    // 建实体、存组件、登记索引 —— 三件事一次做完。
    // 顺带给 acquire_seq 盖章(0 则盖当前水位;非 0 则保留并抬高水位)。
    // guid 撞车时回滚半建好的实体并返回 nullptr(绝不留下孤儿实体)。
    // proto 必须已经填好 item_id / config_id / size。
    //
    // guidToEntity_ 与 registry_ 必须始终同步。为了让"改了一边忘了另一边"
    // 在结构上不可能发生,**所有**创建走 Insert、**所有**销毁走 Erase /
    // Clear;任何地方都不要直接碰 registry_.create()/destroy() 或
    // guidToEntity_.emplace()/erase()。
    ItemComp *Insert(ItemComp proto);

    // 销毁一个实例(实体 + 索引)。未知 guid 是安全的空操作,返回 false。
    // 注意:**它不管槽位** —— 释放槽位是布局层的事,由桥层 Bag 成对调用。
    bool Erase(Guid guid);

    // 丢掉所有实例。容量 / 槽位不归它管,一点都不会动。
    void Clear();

    // 以 (guid, ItemComp) 遍历。ItemComp 是仓库里的那份,调用方不得在循环
    // 之外持有该指针 —— 下一次 Insert / Erase 就可能让它失效。
    template <typename Fn>
    void ForEach(Fn &&fn) const
    {
        // item.item_id() 就是 guid(它正是 guidToEntity_ 的键,见 Insert),
        // 所以不必反查 guidToEntity_,遍历保持 O(n) 而不是 O(n²)。
        for (const auto &[entity, item] : registry_.view<ItemComp>().each())
        {
            fn(static_cast<Guid>(item.item_id()), item);
        }
    }

    // 某个 config 在本仓库里的总数量(跨所有堆叠求和)。
    [[nodiscard]] std::size_t TotalCountOf(uint32_t configId) const;

    // 是否持有 required 列出的每一项(config_id -> 需求数量)。只读。
    [[nodiscard]] bool HasAll(const ItemCountMap &required) const;

    // ── 堆叠语义 ──────────────────────────────────────────────────────
    // 一次计划中的补堆:往既有堆 entity 里再倒 amount 个单位。
    struct StackFill
    {
        entt::entity entity;
        uint32_t amount;
    };

    // 对 wanted 里的每个 config,汇总既有堆叠还空着多少(config_id -> 空余单位)。
    // 不在 wanted 里的、以及不可叠加(满于 1)的 config 一律贡献 0。
    //
    // exclude(可为 nullptr):把这些 guid 的实例当作**已经不存在**来算。
    // 淘汰腾位前用它回答"如果这几件被挤掉了,这批还要新建几个实例" —— 被挤掉的
    // 可能正是本批打算并进去的未满堆,不排除就会把需求算小,于是"腾完仍放不下"。
    [[nodiscard]] ItemCountMap MeasureFreeRoomPerConfig(
        const ItemCountMap &wanted, const std::unordered_set<Guid> *exclude = nullptr) const;

    // 规划(不改任何状态)进来的数量如何填进同 config 的既有堆叠。
    // 填充计划写进 outFillPlan,返回还需要**新建实例**才装得下的剩余量。
    // exclude 语义同 MeasureFreeRoomPerConfig。
    uint32_t PlanStackIntoExistingStacks(const ItemComp &proto, uint32_t maxStackSize,
                                         std::vector<StackFill> &outFillPlan,
                                         const std::unordered_set<Guid> *exclude = nullptr) const;

    // 执行 PlanStackIntoExistingStacks 的计划(真正改 size)。
    // writtenGuidsOut(可为 nullptr)按写入顺序收集被灌入的既有堆 guid ——
    // 并堆不铸新号,但这一堆确实是本次数量的去处,流水按它才追溯得到。
    void ApplyStackFill(const std::vector<StackFill> &fillPlan,
                        std::vector<Guid> *writtenGuidsOut);

    // 从某个 config 的若干堆叠里抽走 count 个单位。数量可能分散在多堆,
    // 逐堆抽到够为止。被抽光的堆 size 变 0 但**实例保留**(槽位也就跟着留着),
    // 后续并堆可以再填回去 —— 这与 Erase(guid) 的"彻底销毁"是两回事。
    // 前置条件:调用方已用 HasAll 确认库存足够,这里不再校验。
    void DrainStacks(uint32_t configId, uint32_t count);

    // 存不存在"同一 config 有 >= 2 个未满堆"。有就说明还能合并出更少的实例。
    [[nodiscard]] bool HasMergeablePartials() const;

    // 把同 config 的未满堆合并成尽量少的满堆:组内总量重新分配,前几个填满、
    // 最后一个放余数、多出来的清成 size=0。
    //
    // **它自己不销毁任何实例** —— 销毁必须两层成对进行(那是桥层的活)。
    // 被清空的那些不单独返回:它们已经是 size==0,由紧随其后的
    // CollectEmptyInstances 一并扫出来,不必维护两条重复的名单。
    void MergePartialStacks();

    // 存不存在 size==0 的实例。这种实例什么都不装,却仍然占着一个槽位。
    //
    // 两条来路:①可叠加物品被抽光(DrainStacks 刻意保留空堆,好让后续并堆填回去);
    // ②**不可叠加物品被 RemoveItemByPos 扣到 0** —— 这一类 MergePartialStacks
    // 永远碰不到(它跳过 max_stack_size() <= 1),拆分前没有任何路径回收得了,
    // 就是一个永久占格的僵尸。现在由整理统一回收。
    [[nodiscard]] bool HasEmptyInstances() const;

    // 列出所有 size==0 的实例 guid,交给桥层连同槽位一起销毁。
    // 与 MergePartialStacks 一样,**它自己不销毁任何实例**。
    [[nodiscard]] GuidVector CollectEmptyInstances() const;

    [[nodiscard]] static bool CanStack(const ItemComp &leftItem, const ItemComp &rightItem);

    // totalSize 个单位、每堆最多 maxStackSize 个,要拆成几个实例。
    // 拆分前它叫 GridsNeededFor("要几格"),名字把实例数和格子数混为一谈;
    // 在"1 实例恒占 1 格"的扁平布局下两者恰好相等,格子布局下就不等了。
    [[nodiscard]] static std::size_t StacksNeededFor(std::size_t totalSize,
                                                     std::size_t maxStackSize);

    // ── guid 发号(实例身份)────────────────────────────────────────
    // 唯一发号源 = item 种类的号段实例(tlsGuidSegmentRegistry.Get(GuidKind::kItem));
    // 拿不到号(未启用 / 两段耗尽且续段未到)返回 kInvalidGuid,**没有** snowflake 回退。
    [[nodiscard]] static Guid MintGuid();

    // 发号源当前能否铸出合法 guid(号段:手里有号)。
    // 铸号型写入必须在**改动任何状态之前**用它 fail-closed,见 .cpp 注释。
    [[nodiscard]] static bool CanMintGuid() { return CanMintGuids(1); }

    // 同上,但要求一次能铸出 count 个。号段是有限库存:段尾只剩 k 个而下一段又没到时,
    // 一批要 n > k 个号的入包会在循环中途铸出哨兵、留下半批 —— 整批预检必须按需求量问。
    [[nodiscard]] static bool CanMintGuids(std::size_t count);

    [[nodiscard]] static bool IsInvalidGuid(const ItemComp &item);

private:
    ItemsMap guidToEntity_{};
    entt::registry registry_{};

    // 下一个要盖的入包序号。1 起步,0 是"没盖章"的哨兵。
    // 只由 Insert 推进、只由 Clear 复位 —— 见 Insert 里的注释。
    uint64_t nextAcquireSeq_{1};
};
