#pragma once

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
//
// **现状(2026-11-12):这一轴只有 `RejectWhenFull` 一个实现,任何包都不淘汰。**
// 先进先出 `EvictOldestFirst` 2026-09-01 ~ 2026-11-05 配在临时格上,满了会销毁最早进包的那件,
// 东西没有任何去处,对玩家就是真丢(审计 cpp-economy#2);临时格改为满即拒之后它没有生产使用者,
// 2026-11-12 连同它的用例一起删除(docs/design/bag-rule-policy-layering.md §6.6)。
// 接口与桥层的淘汰分支(`Bag::PlanEviction` 的不动点规划、先判后毁、回执落流水、具名槽不淘汰)保留为
// 这一轴的契约,但**没有实现、也没有用例覆盖**。以后真要给某个包配淘汰:
//   1. 先给被挤掉的东西一个去处(例如改投邮件),否则就是丢数据;
//   2. 把桥层机制的用例补回来 —— 2026-11-12 随先进先出一起删掉的那组用例,清单在设计文档 §6.6 (2),
//      源码在提交 4edb25640 的 cpp/tests/bag_test/ 里,可照着恢复。
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
// 四个固定包(人物背包 / 仓库 / 装备栏 / 临时格)与全部动态包。**这是拆分前唯一存在的行为**,
// 只是当时没有名字;2026-11-12 起也是这一轴唯一的实现(见文件头「现状」)。
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
