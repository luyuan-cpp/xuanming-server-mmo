#pragma once

#include <cstdint>
#include <unordered_set>
#include <utility>

// ─────────────────────────────────────────────────────────────────────────
// 准入层(admission policy)
//
// 这一层只回答一件事:**这个容器收不收这种东西**。
//
// 它与另外两层的分界:
//
//   实例层 ItemStore          有哪些实例、各是什么
//   布局层 IContainerLayout   摆在哪、还摆不摆得下          —— 永远不认识 config
//   准入层 IAdmissionPolicy   收不收这种东西                 —— 只认识 config
//
// **准入永远是纯谓词、零副作用。** 只有这样才能塞进 Bag 的
// `plan -> reserve -> commit` 三段里的 reserve 段,而不破坏"失败一定发生在写入
// 之前,绝无半写状态"这条纪律。任何一个会改状态的"规则"都不属于这里 ——
// 淘汰(满了挤掉谁)是另一根轴,它有副作用、必须带回执,
// 见 docs/design/bag-rule-policy-layering.md §3。
//
// ── 这里刻意**只有** Accepts(configId) 一个方法 ──────────────────────────
//
// 设计文档 §5 原本还画了一个 `AcceptsBatch(items, layout)`,用来回答"装备栏的
// 两个手镯位装不装得下这一批"。落地时发现那个方法不该在这里:
//
//   * "这批摆不摆得下"要同时读**布局的当前占用**和**配置表**,是桥层的活;
//   * 更关键的是,它必须与 `Bag::PlaceInstance` 在**同一个条件**上分叉
//     (`layout_->HasSlotSemantics()`)。那个 bug 的根就是两侧分叉条件不一致:
//     commit 侧按具名槽去查槽位表,reserve 侧却一律拿 FreeCells() 作答。
//     把它做成可插拔策略,等于让"两侧必须一致"重新变成一件靠人记住的事。
//
// 所以具名槽的批量判定留在桥层(`Bag::CanReserve`),这一层只管**与占用无关**
// 的准入:节日包只收节日道具、装备栏只收声明了部位的东西 —— 纯 config 问题。
// 等真出现"这个包最多收 3 件任务道具"这种跟占用有关的规则,再谈扩接口;
// 现在加就是没有使用者的臆测(判据同 GridLayout 的注释)。
// ─────────────────────────────────────────────────────────────────────────

class IAdmissionPolicy
{
public:
    virtual ~IAdmissionPolicy() = default;

    // 这个容器收不收这种东西。**只看 config,不看当前占用**。
    // 返回 false 时桥层在 reserve 段整批拒绝,一个字节都不会写。
    [[nodiscard]] virtual bool Accepts(uint32_t configId) const = 0;
};

// ─────────────────────────────────────────────────────────────────────────
// AcceptAll —— 什么都收。
//
// 人物背包 / 仓库 / 临时格,以及全部动态包的当前行为。
//
// 它看起来是个空壳,但意义与 `FlatLayout` 完全一样:拆分前"没有准入规则"是
// **唯一存在、且没有名字**的准入策略,散在"压根没人问过这个问题"里。给它一个
// 名字之后,"这个包没有准入规则"才成为一句**写下来的、可替换的**话,而不是一个
// 沉默的缺省。
// ─────────────────────────────────────────────────────────────────────────
class AcceptAll final : public IAdmissionPolicy
{
public:
    [[nodiscard]] bool Accepts(uint32_t /*configId*/) const override { return true; }
};

// ─────────────────────────────────────────────────────────────────────────
// AcceptByConfigSet —— 只收名单上的 config。
//
// **节日包 / 活动包用它。** 名单由创建这个包的玩法(活动系统)在注册 profile 时
// 给出 —— 那边本来就知道自己发哪些道具,不需要再去物品表上加一列。
//
// 为什么不是 `AcceptByTag`(在 CfgItem 上加一列 tag、按 tag 收):
// 那当然更像"数据驱动",但今天 `CfgItem` 只有 id / max_stack_size / equip_kind
// 三列,加列要改二进制 xlsx 源表 + 跑导表工具,不是代码能落地的事。而
// **没有那一列,AcceptByTag 会对所有东西返回 tag==0 → 什么都不收**,比没有更糟。
// 名单式准入今天就是完整可用的;等 tag 列真的落地,再加一个
// `class AcceptByTag final : public IAdmissionPolicy`(约二十行)与它并列即可,
// 这一层的接口一个字都不用改 —— 这正是把准入做成策略的意义。
// ─────────────────────────────────────────────────────────────────────────
class AcceptByConfigSet final : public IAdmissionPolicy
{
public:
    explicit AcceptByConfigSet(std::unordered_set<uint32_t> allowed)
        : allowed_(std::move(allowed))
    {
    }

    [[nodiscard]] bool Accepts(uint32_t configId) const override
    {
        return allowed_.contains(configId);
    }

    [[nodiscard]] const std::unordered_set<uint32_t> &Allowed() const { return allowed_; }

private:
    std::unordered_set<uint32_t> allowed_;
};
