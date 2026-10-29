#pragma once

#include <functional>
#include <unordered_map>
#include <unordered_set>
#include <vector>

#include "engine/core/type_define/type_define.h"

#include "item_system.h"

// ─────────────────────────────────────────────────────────────────────────
// 实例层(item store)
//
// 物品实例是**领域实体**:有自己的身份(item_uuid,全局唯一、跨堆叠拆合稳定)、
// 自己的生命期(铸号 -> 存在 -> 销毁),以及自己的状态(config_id + size,
// 2026-10-07 起装备实例还带一段 equip —— 随机属性行,二期起强化等级与已镶宝石也在
// 这一段里(EquipInstanceData,docs/design/equipment-enhance-inlay.md §3.1);绑定态 /
// 限时等仍是预留)。本层把 ItemComp 当**整份**搬运与存放,不解释 equip 的内容,
// 只在堆叠判定上认它的 presence(见 CanStack)。
//
// 这一层只管"有哪些实例、它们各是什么、数量怎么在实例之间分配"。
// 它**不认识槽位、不认识容量、不知道背包满没满** —— 那些全在布局层
// (container_layout.h)。两层的唯一接头是一个 Guid。
//
// 堆叠算术之所以留在这一层而不是布局层,是因为它回答的是"这批数量要拆成
// 几个实例",不是"占几格"。拆分前这两个问题被 GridsNeededFor 这个名字混成
// 了一个,正是两层焊死的症状之一。
// ─────────────────────────────────────────────────────────────────────────

class ItemStore
{
public:
    // ── 身份与生命期 ──────────────────────────────────────────────────
    [[nodiscard]] std::size_t Size() const { return items_.size(); }
    [[nodiscard]] bool Contains(Guid guid) const { return items_.contains(guid); }

    // 返回仓库内的实例。调用方不得改 item_id / acquire_seq,也不得把指针留到下一次
    // Insert / Erase / Clear / 整理之后;接口不承诺跨写操作保留借用。
    ItemComp *Find(Guid guid);
    [[nodiscard]] const ItemComp *Find(Guid guid) const;

    // 当前水位(下一个会盖出的序号)。只读,给用例断言用。
    [[nodiscard]] uint64_t NextAcquireSeq() const { return nextAcquireSeq_; }
    // 加载边界先抬到全包最大既有序号之后，零章条目随后由 Insert 按出现顺序补章。
    void RaiseAcquireSeq(uint64_t sequence);

    // 存入整份实例并给 acquire_seq 盖章(0 则盖当前水位;非 0 则保留并抬高水位)。
    // guid 撞车时返回 nullptr,实例和入包水位都不改变。
    // proto 必须已经填好 item_id / config_id / size。
    //
    // items_ 是唯一权威,键始终等于 item_id;创建只走 Insert,销毁只走 Erase / Clear。
    ItemComp *Insert(ItemComp proto);

    // 销毁一个实例。未知 guid 是安全的空操作,返回 false。
    // 注意:**它不管槽位** —— 释放槽位是布局层的事,由桥层 Bag 成对调用。
    bool Erase(Guid guid);

    // 丢掉所有实例。容量 / 槽位不归它管,一点都不会动。
    void Clear();

    // 包内实例序:(acquire_seq 升序,item_id 升序),与 Java BagItem.ACQUIRE_ORDER 同口径。
    // guid 在包内唯一,所以这是一条全序;理论不可达的零序号排最前。
    [[nodiscard]] static bool AcquiredBefore(const ItemComp &lhs, const ItemComp &rhs);

    // 按实例序以 (guid, ItemComp) 遍历。回调不得修改本仓库,借用遵守 Find 的寿命约束。
    // guid 取唯一权威 map 的键,不从可写消息字段重新推导身份。
    template <typename Fn>
    void ForEach(Fn &&fn) const
    {
        for (const auto *entry : InAcquireOrder())
        {
            fn(entry->first, entry->second);
        }
    }

    // 按 config 计数、预检、扣除时,这个实例算不算可互换的普通单位:不带更新版本才有的字段即可
    // (已退役字段的残留不算,判据是 unknown_field_carry::CarriesNewerVersionFields)。
    // 新版本给物品加的属性(例如绑定、限时)可能决定它能不能被消耗、交易、镶嵌;旧代码看不懂这些属性,
    // 不能把它当普通单位计数或扣走 —— 否则资产通道会把一件绑定物品当普通物品交易出去,属性无声消失
    // (docs/design/mixed-version-persist-field-protection.md §4.4,第 6 批)。
    // 与 IsIndependentInstance 是两条规则:那条管"能不能并堆",这条管"能不能按 config 计数和扣除"。
    // has_equip 的实例照算:按 config 扣到装备,是调用方用配置表排除的事(BagService::RemoveItemsExact 注释)。
    [[nodiscard]] static bool IsCountableByConfig(const ItemComp &item);

    // 某个 config 在本仓库里可按数量计的总数:跨所有堆叠求和,只计 IsCountableByConfig 的实例。
    [[nodiscard]] std::size_t TotalCountOf(uint32_t configId) const;

    // 按数量预检的结论。三态而不是 bool:"只有算上带新版本字段的单位才够"是版本错位,
    // 调用方必须当成可重试的暂时状态,不能和"确实不够"一样记成终态拒绝。
    enum class QuantityVerdict : uint8_t
    {
        kEnough,       // 每一项的可计单位都够
        kVersionSkew,  // 没有哪项确定不够,但至少一项只有算上带更新版本字段的单位才够
        kInsufficient, // 至少一项连算上那些单位都不够:确定不足(任何版本都扣不出来)
    };

    // required(config_id -> 需求数量)整体的预检结论。只读,一次遍历。需求为 0 的项视为满足。
    // 结论优先级:确定不足 > 版本错位 > 够。
    [[nodiscard]] QuantityVerdict CheckQuantities(const ItemCountMap &required) const;

    // 是否持有 required 列出的每一项(只计可计单位)。等价于 CheckQuantities(required) == kEnough。
    [[nodiscard]] bool HasAll(const ItemCountMap &required) const;

    // ── 堆叠语义 ──────────────────────────────────────────────────────
    // 一次计划中的补堆:往既有 guid 里再倒 amount 个单位。
    struct StackFill
    {
        Guid guid{kInvalidGuid};
        uint32_t amount{0};
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

    // 一条"被抽走"的回执:哪个实例、抽走多少。实例本身仍在(可能 size==0)。
    // 与 StackFill 是一对:入包记灌进了哪些堆,扣除记抽了哪些堆。
    struct StackDrain
    {
        Guid guid{kInvalidGuid};
        uint32_t amount{0};
    };

    // 从某个 config 的若干堆叠里抽走 count 个单位，逐堆抽到够为止。
    // 抽光的堆 size 变 0，实例层不销毁；Bag 必须当场成对 DestroyItem（DrainOneConfig 已这样做）。
    //
    // 只从 IsCountableByConfig 的堆里抽:带更新版本字段的堆不当普通单位扣(与 TotalCountOf / CheckQuantities 同口径)。
    // 库存不足时按实际持有夹紧(本来就是逐堆 min(需求, 库存)),**返回值即实扣量**;
    // 调用方要"不够就整体失败"的语义,必须自己先 CheckQuantities(Bag::RemoveItems 就是这么做的)。
    // drainedOut(可为 nullptr)按抽取顺序收集 (guid, 抽走量),size==0 的僵尸堆不进回执。
    // rank 由调用方提供,实例层不解释它。桥层传槽位号,按 rank 再按实例序抽取。
    // 回调只读且不重入仓库;每个候选的 rank 在排序前只计算一次。
    using DrainRank = std::function<uint64_t(Guid)>;
    uint32_t DrainStacks(uint32_t configId, uint32_t count, const DrainRank &drainRank,
                         std::vector<StackDrain> *drainedOut = nullptr);

    // 存不存在"同一 config 有 >= 2 个非空未满堆"。空堆只回收,不当合并幸存者。
    [[nodiscard]] bool HasMergeablePartials() const;

    // 把同 config 的未满堆合并成尽量少的满堆:组内总量重新分配,前几个填满、
    // 最后一个放余数、多出来的清成 size=0。
    //
    // **它自己不销毁任何实例** —— 销毁必须两层成对进行(那是桥层的活)。
    // 被清空的那些不单独返回:它们已经是 size==0,由紧随其后的
    // CollectEmptyInstances 一并扫出来,不必维护两条重复的名单。
    void MergePartialStacks();

    // size==0 只会是 MergePartialStacks 刚清空的组员，紧接着由 CollectEmptyInstances 回收。
    // 扣到 0 已当场释放，旧档零堆在加载时跳过。
    [[nodiscard]] bool HasEmptyInstances() const;

    // 列出所有 size==0 的实例 guid,交给桥层连同槽位一起销毁。
    // 与 MergePartialStacks 一样,**它自己不销毁任何实例**。
    [[nodiscard]] GuidVector CollectEmptyInstances() const;

    // 两个实例能不能并成一堆:同 config,且**双方都不带实例数据**(equip 段)。
    // 后一条是兜底:装备本该 max_stack_size == 1、根本不走堆叠,但表一旦被误配成可堆叠,
    // 并堆只加数量,两件属性不同的装备会并成一堆、其中一份属性无声消失。
    // 判的是 has_equip() 这个 presence,不是属性条数。
    //
    // MeasureFreeRoomPerConfig 和合并两处的 IsMergeCandidate 同样排除 equip。
    // size>0 约束合并，空堆不参与幸存者选择。
    //
    // 同理,带着本版本不认识的字段(新版本加的物品属性,例如绑定、限时)的实例也不并堆:旧代码看不懂这些属性,
    // 并堆会把带属性的物品并进普通堆、或把属性抹成其中一份(docs/design/mixed-version-persist-field-protection.md §4.4)。
    // 包里既有的堆:MeasureFreeRoomPerConfig、IsMergeCandidate 与本函数共用 IsIndependentInstance 一个判定,
    // 预检与提交(PlanStackIntoExistingStacks)不会分叉。传入物品一侧同样只认这一个判定:
    // Bag::AddItems 的规划按它把传入物品分进「单独计实例」或「可并堆」,与提交时 CanStack(既有堆, 传入物品) 同口径。
    [[nodiscard]] static bool CanStack(const ItemComp &leftItem, const ItemComp &rightItem);

    // 不与任何实例并堆的独立实例:带实例数据(has_equip(),presence 即判据),或带着更新版本才有的字段(见 CanStack;
    // 已退役字段的残留字节不算,判据是 unknown_field_carry::CarriesNewerVersionFields)。
    // 包内既有的堆与传入物品都用它判,任何"能不能并"的预检、规划、提交不得另写一份判据。
    [[nodiscard]] static bool IsIndependentInstance(const ItemComp &item);

    // totalSize 个单位、每堆最多 maxStackSize 个,要拆成几个实例。
    // 拆分前它叫 GridsNeededFor("要几格"),名字把实例数和格子数混为一谈;
    // 在"1 实例恒占 1 格"的扁平布局下两者恰好相等,格子布局下就不等了。
    [[nodiscard]] static std::size_t StacksNeededFor(std::size_t totalSize,
                                                     std::size_t maxStackSize);

    // ── guid 发号(实例身份)────────────────────────────────────────
    // 唯一发号源 = item 种类的号段实例(tlsGuidSegmentRegistry.Get(GuidKind::kItem));
    // 拿不到号(未启用 / 两段耗尽且续段未到)返回 kInvalidGuid,**没有** snowflake 回退。
    [[nodiscard]] static Guid MintGuid();

    [[nodiscard]] static bool IsInvalidGuid(const ItemComp &item);

private:
    using Items = std::unordered_map<Guid, ItemComp>;
    // 节点容器在 rehash 时不移动实例,排序候选只借用节点地址,不复制物品数据。
    // 借用仅限本次调用;任何排序或 ForEach 回调都不得修改容器结构。
    [[nodiscard]] std::vector<const Items::value_type *> InAcquireOrder() const;
    [[nodiscard]] std::vector<Items::value_type *> InAcquireOrder();
    [[nodiscard]] static bool IsMergeCandidate(const ItemComp &item, uint32_t maxStack);

    Items items_{};

    // 下一个要盖的入包序号。1 起步,0 是"没盖章"的哨兵。
    // 只由 Insert 推进、只由 Clear 复位 —— 见 Insert 里的注释。
    uint64_t nextAcquireSeq_{1};
};
