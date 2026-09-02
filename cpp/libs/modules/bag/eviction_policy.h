#pragma once

#include <cstddef>

#include "engine/core/type_define/type_define.h"

#include "container_layout.h"
#include "item_store.h"

// ─────────────────────────────────────────────────────────────────────────
// 淘汰层(eviction policy)
//
// 这一层只回答一件事:**摆不下的时候,该退役谁**。
//
// 它是六根轴里**唯一有副作用**的一根(见
// docs/design/bag-rule-policy-layering.md §3),因此有两条别的轴没有的纪律:
//
//   ① **策略自己不销毁任何东西。** 它只"选出"该退役哪些 guid,销毁由桥层
//      `Bag` 两层成对执行 —— 与 `ItemStore::MergePartialStacks` 同一条规矩:
//      只删实例不放槽位,槽位就被幽灵 guid 永久占着;只放槽位不删实例,实例
//      就成了查不到也删不掉的孤儿。
//
//   ② **销毁必须留痕。** 被挤掉的是玩家的资产,而 `item_uuid` 正是
//      transaction_log 做外挂回收关联的键。所以 `Bag` 把退役清单**回执**给
//      调用方,由 `BagService` 落 `LogItemDestroy` —— 形状与
//      `MergeAndCompact` 的 `DestroyedInstance` 回执逐字相同,那条路已经踩过
//      一次坑了(split 文档 §11.1(g)),不再重犯。
//
// 拆分前这一轴的实现是**隐式**的:`AddNonStackableItem` 里那句
// `// TODO: overflow to temp bag or mail` 底下直接返回"背包满了" —— 也就是
// 一个没有名字的 `RejectWhenFull`。给它名字之后,"这个包满了会怎样"才成为
// 一句写下来的、可替换的话。
// ─────────────────────────────────────────────────────────────────────────

class IEvictionPolicy
{
public:
    virtual ~IEvictionPolicy() = default;

    // 要腾出 needed 个实例位,应该按什么顺序退役哪些实例。
    //
    // 约定(桥层依赖它,实现必须遵守):
    //   * **零副作用**:只读 store / layout,不改任何状态;
    //   * 腾不够就返回**空**,不要返回"能腾多少算多少" —— 桥层看到不足会整批
    //     拒绝,而"销毁了一半仍然放不下"等于白白弄丢玩家的东西;
    //   * 返回顺序即退役顺序,便于调用方按序落流水。
    [[nodiscard]] virtual GuidVector SelectVictims(std::size_t needed,
                                                   const ItemStore &store,
                                                   const IContainerLayout &layout) const = 0;
};

// ─────────────────────────────────────────────────────────────────────────
// RejectWhenFull —— 满了就拒绝,不动任何已有物品。
//
// 人物背包 / 仓库 / 装备栏,以及全部动态包。**这是拆分前唯一存在的行为**,
// 只是当时没有名字。
// ─────────────────────────────────────────────────────────────────────────
class RejectWhenFull final : public IEvictionPolicy
{
public:
    [[nodiscard]] GuidVector SelectVictims(std::size_t /*needed*/, const ItemStore & /*store*/,
                                           const IContainerLayout & /*layout*/) const override
    {
        return {}; // 空 = 一件都不许挤掉
    }
};

// ─────────────────────────────────────────────────────────────────────────
// EvictOldestFirst —— 先进先出:满了就挤掉**最早进包的**那几件。
//
// 临时格(`kTemporary`)用它:那个包是掉落溢出的缓冲,语义本来就是"新的进来、
// 旧的顶出去",而不是"满了就捡不起来"。
//
// ⚠ **排序依据目前是 guid,这是一个已知的近似。**
//
// item guid 是 snowflake,高位是时间,所以升序**约等于**入包先后。两条路径会
// 打乱它:
//   ① 跨服迁移带回来的实例,guid 是**源服 node** 铸的;
//   ② `AddItems(std::vector<InitItemParam>)`(邮件附件)**沿用调用方预设的
//      guid**,那个号可能比包里已有的都小。
//
// 正解是给 `ItemEntry` / `ItemComp` 加一个显式的获得序号字段(`ItemEntry` 的
// 字段号 6/7/8 已被 TODO 预定,用 9),那要改 proto 并重生成,不在本批范围内。
// 届时**只需要改 AcquisitionOrderOf() 这一个函数**,策略与桥层都不用动 ——
// 把近似收在一个有名字的函数里,就是为了让那天的改动只有一行。
// ─────────────────────────────────────────────────────────────────────────
class EvictOldestFirst final : public IEvictionPolicy
{
public:
    [[nodiscard]] GuidVector SelectVictims(std::size_t needed, const ItemStore &store,
                                           const IContainerLayout &layout) const override;

    // 一个实例的"入包早晚"。越小越早。见上面那段关于近似的说明 ——
    // 换成真正的获得序号时,这里是唯一的改动点。
    [[nodiscard]] static uint64_t AcquisitionOrderOf(Guid guid, const ItemComp &item);
};
