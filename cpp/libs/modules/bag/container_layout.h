#pragma once

#include <cstddef>
#include <cstdint>
#include <unordered_map>
#include <vector>

#include "engine/core/type_define/type_define.h"

// ─────────────────────────────────────────────────────────────────────────
// 布局层(container layout)
//
// 这一层只回答一件事:**这些实例摆在哪、还摆不摆得下**。
//
// 与实例层(item_store.h)的分界是硬的 —— 布局层永远不认识 config_id、
// 不认识堆叠数量、不查 ItemTable、不 include entt。它看得见的只有
// (Guid, Footprint) 两样东西。哪天这里需要读一次 ItemComp,这条缝就漏了;
// 这是判断拆分有没有塌回去的唯一判据。
//
// 拆分之前,布局状态(posToGuid + capacity)和实例状态(items + itemRegistry)
// 都是 Bag 的私有成员,被同一批函数同时读写:IsFull() 拿实例数量去比容量、
// MergeAndCompact() 一个函数既改 size 又重排格子、DestroyItem() 销毁实体之后
// 还要线扫 posToGuid 找槽位。于是"扁平"根本不是一种可替换的策略,而是唯一
// 存在、且没有名字的布局 —— 想要格子背包只能另写一套。
//
// 拆开之后,扁平(FlatLayout)与格子(GridLayout)是同一个模型的两种布局策略:
// 换布局不需要动实例层一行代码。
// ─────────────────────────────────────────────────────────────────────────

// 槽位 -> 占用它的实例 guid。序列化出去就是 BagAllData.ItemEntry.pos,
// 所以这次拆分对线上协议完全不可见。
using PosMap = std::unordered_map<uint32_t, Guid>;

// 槽位号。各策略对它的解释不同,但对外一律是一个 uint32:
//   * FlatLayout      —— 下标 0..capacity-1
//   * FixedSlotLayout —— 槽位枚举值(头 / 胸 / …),见 docs/design/bag-instance-layout-split.md
//   * GridLayout      —— 左上角单元的 y * width + x
using SlotId = uint32_t;
inline constexpr SlotId kInvalidSlot{kInvalidU32Id};

// 一个实例在布局里占的形状。
// 扁平布局恒占一格并忽略宽高;格子布局(暗黑 / POE / 塔科夫式)才用得上。
// 形状由**桥层(Bag)**从配置表推导后传进来 —— 布局层自己不查表,
// 否则它就又认识 config 了。
struct Footprint
{
    uint32_t width{1};
    uint32_t height{1};

    [[nodiscard]] uint32_t Cells() const { return width * height; }
};

inline constexpr Footprint kSingleCell{1, 1};

// 布局策略接口。所有实现都必须做到:任何一次失败的放置都不留下半写状态。
class IContainerLayout
{
public:
    virtual ~IContainerLayout() = default;

    // ── 容量 ──────────────────────────────────────────────────────────
    // 这里的 capacity **只有"格子数"一个含义**。拆分前它同时兼任"最多能有
    // 几个实例"(IsFull() 写作 items.size() >= capacity),那正是两层被焊死
    // 的地方 —— 只有在"1 实例恒占 1 格"的前提下才成立,格子布局一上来就破。
    [[nodiscard]] virtual std::size_t Capacity() const = 0;
    virtual void Resize(std::size_t newCapacity) = 0;

    [[nodiscard]] virtual std::size_t OccupiedSlotCount() const = 0;
    [[nodiscard]] virtual std::size_t FreeCells() const = 0;

    // 还能不能再放下 count 个 footprint 形状的实例(整批判定,不改任何状态)。
    // 扁平布局等价于"空格数 >= count";格子布局必须真去试摆。
    [[nodiscard]] virtual bool CanFit(std::size_t count, Footprint footprint) const = 0;

    // ── 放置 / 取出 ───────────────────────────────────────────────────
    // Place:自动选位,放不下返回 kInvalidSlot 且不改任何状态。
    virtual SlotId Place(Guid guid, Footprint footprint) = 0;

    // PlaceAt:落到指定槽位,快照还原专用 —— 玩家在源服摆好的位置必须原样复现,
    // 不能重新分配。
    //
    // **fail-closed**:槽位越界、或已被别的实例占着,一律返回 false 且不改任何
    // 状态。放不下时怎么办是桥层的决策(Bag::InsertItemForRestore 会退化为自动
    // 选位),布局层只负责诚实回答"这个位置能不能用"。
    virtual bool PlaceAt(Guid guid, SlotId slot, Footprint footprint) = 0;

    virtual void Remove(Guid guid) = 0;
    virtual void Clear() = 0;

    // ── 查询 ──────────────────────────────────────────────────────────
    // 不在布局里的 guid 返回 kInvalidSlot;空槽位返回 kInvalidGuid。
    [[nodiscard]] virtual SlotId SlotOf(Guid guid) const = 0;
    [[nodiscard]] virtual Guid At(SlotId slot) const = 0;

    // 槽位 -> guid 的全量视图。对格子布局给的是"左上角锚点 -> guid"。
    [[nodiscard]] virtual const PosMap &Slots() const = 0;

    // ── 整理 ──────────────────────────────────────────────────────────
    // 该策略支不支持重排。装备栏那种具名固定槽、格子背包的手摆布局都不支持,
    // 桥层据此决定要不要进整理流程。
    [[nodiscard]] virtual bool SupportsCompaction() const = 0;

    // 槽位有没有**语义**(具名槽)。
    //
    // 为 true 时,桥层不会用 first-fit 找空位,而是去问配置数据"这件东西该去
    // 哪个槽",再 PlaceAt 到那儿 —— 头盔就得进头盔槽。为 false 时(人物背包、
    // 仓库、格子背包)位置本身没有含义,first-fit 即可。
    //
    // 注意它**不等于** !SupportsCompaction():格子背包同样不许自动重排,
    // 但它的槽位没有具名语义,物品放哪一格都行。
    [[nodiscard]] virtual bool HasSlotSemantics() const = 0;

    // 槽位是否已无空洞(扁平语义 = 键恰好是 0..n-1)。
    [[nodiscard]] virtual bool IsCompact() const = 0;

    // 按给定顺序把实例重新铺到槽位上。**顺序由桥层算出**(要读 config / size
    // 才排得出来),布局层只负责按序落位 —— 于是"按什么排"是可换的规则,
    // "排完怎么落位"才是布局自己的事。
    virtual void ApplyOrder(const std::vector<Guid> &order) = 0;
};

// ─────────────────────────────────────────────────────────────────────────
// FlatLayout —— 扁平布局:一个实例恒占一格,first-fit 铺 0..capacity-1。
//
// 这是拆分前 Bag 里那套无名逻辑的原样搬迁,行为逐字不变。唯一的改动是多了
// 一张反向索引 guidToSlot_:拆分前 GetItemPosByGuid / DestroyItem 都要线扫
// posToGuid(O(n)),现在是 O(1)。
//
// 使用者:kInventory / kWarehouse / kTemporary,以及全部 dynamicBags_。
// ─────────────────────────────────────────────────────────────────────────
class FlatLayout : public IContainerLayout
{
public:
    // 刻意不给默认容量 —— "背包默认几格"是背包域的常量(bag_system.h 的
    // kDefaultCapacity),布局层不该知道。
    explicit FlatLayout(std::size_t capacity) : capacity_(capacity) {}

    [[nodiscard]] std::size_t Capacity() const override { return capacity_; }
    void Resize(std::size_t newCapacity) override { capacity_ = newCapacity; }

    [[nodiscard]] std::size_t OccupiedSlotCount() const override { return slotToGuid_.size(); }
    [[nodiscard]] std::size_t FreeCells() const override;
    [[nodiscard]] bool CanFit(std::size_t count, Footprint footprint) const override;

    SlotId Place(Guid guid, Footprint footprint) override;
    bool PlaceAt(Guid guid, SlotId slot, Footprint footprint) override;
    void Remove(Guid guid) override;
    void Clear() override;

    [[nodiscard]] SlotId SlotOf(Guid guid) const override;
    [[nodiscard]] Guid At(SlotId slot) const override;
    [[nodiscard]] const PosMap &Slots() const override { return slotToGuid_; }

    [[nodiscard]] bool SupportsCompaction() const override { return true; }
    // 扁平背包的下标不代表任何东西,first-fit 即可。
    [[nodiscard]] bool HasSlotSemantics() const override { return false; }
    [[nodiscard]] bool IsCompact() const override;
    void ApplyOrder(const std::vector<Guid> &order) override;

protected:
    PosMap slotToGuid_{};
    std::unordered_map<Guid, SlotId> guidToSlot_{};
    std::size_t capacity_{0};
};

// ─────────────────────────────────────────────────────────────────────────
// FixedSlotLayout —— 具名固定槽:槽位有含义(头 / 胸 / 武器 / …),**不允许
// 被自动整理重排**。装备栏用它。
//
// 槽位的记账与扁平布局完全一样,所以直接继承 FlatLayout;唯一的区别就是
// SupportsCompaction() 返回 false —— "整理"绝不能把头盔挪到鞋子的位置上。
// 这一条正是它存在的全部理由,也正是 kEquipment 此前用 FlatLayout 的实际危害。
//
// "哪件装备进哪个槽"**不在这里**,也不在代码里任何地方 —— 它是两张表的事:
//
//   `CfgItem.equip_kind`      这件东西属于哪个**部位**(手镯 / 头 / 胸 …)
//   `CfgEquipSlot`(槽位表)   每个**槽位**接受哪个部位
//
// 于是"同一部位有多个槽"是天然支持的:槽位表里写两行 `equip_kind` 相同,
// 就是两个手镯位。(早先那版把部位和槽号混成一个字段,表达不了这个需求。)
//
// 布局层永远不认识 config(见本文件顶部的分界),所以这两次查表都长在桥层:
// `Bag::EquipKindFor()` + `Bag::FindFreeSlotForKind()`。`HasSlotSemantics()`
// 为 true 时桥层就按查到的槽位 `PlaceAt`,而不是 first-fit。
//
// 部位与槽位的含义完全由**表数据**定义:策划改表即可,这两层一行都不用动,
// C++ 里没有任何地方写死过某个部位号或槽位号。
//
// 刻意**没有**固定槽位数:看起来"具名槽的数量当然是契约、Resize 该拒绝",
// 但代价是实打实的 —— 一份 `capacities[kEquipment] = 20` 的存量快照撞上写死的
// 10,还原时会**静默丢掉装备**。忠实还原比假装有契约重要。
//
// 见 docs/design/bag-instance-layout-split.md §11.2。
// ─────────────────────────────────────────────────────────────────────────
class FixedSlotLayout final : public FlatLayout
{
public:
    explicit FixedSlotLayout(std::size_t slotCount) : FlatLayout(slotCount) {}

    // 具名槽绝不参与自动重排。桥层看到 false 就只合并堆叠、回收空实例,
    // 位置一格不动。
    [[nodiscard]] bool SupportsCompaction() const override { return false; }

    // 具名槽：东西该去哪个槽，由配置数据决定，不是第一个空位。
    [[nodiscard]] bool HasSlotSemantics() const override { return true; }

    // 防御性:SupportsCompaction() 已经是 false,桥层不该走到这里。
    void ApplyOrder(const std::vector<Guid> &order) override;
};

// ─────────────────────────────────────────────────────────────────────────
// GridLayout —— 格子布局(暗黑 / POE / 塔科夫式):W x H 的二维格,实例按
// Footprint 的宽高占据一块矩形。SlotId = 左上角单元的 y * width + x。
//
// **目前没有任何背包用它。** 它存在的意义是证明这条缝是真的:加一种布局
// 策略,实例层(ItemStore)与桥层(Bag)一行都不用改。覆盖在
// cpp/tests/bag_test/bag_test.cpp 的 GridLayoutTest 套件里。
//
// 不做旋转:塔科夫允许物品旋转 90°,那需要 Footprint 带朝向、SlotId 带一位
// 旋转标记,并让 ItemEntry.pos 承载它。等真要做格子背包时再加,现在加就是
// 没有使用者的臆测。
// ─────────────────────────────────────────────────────────────────────────
class GridLayout final : public IContainerLayout
{
public:
    GridLayout(uint32_t width, uint32_t height);

    [[nodiscard]] uint32_t Width() const { return width_; }
    [[nodiscard]] uint32_t Height() const { return height_; }

    [[nodiscard]] std::size_t Capacity() const override
    {
        return static_cast<std::size_t>(width_) * static_cast<std::size_t>(height_);
    }

    // 按整行增减。缩容只在被砍掉的行完全空着时才执行,否则拒绝并报错 ——
    // 布局层无权决定"哪件东西该被挤掉",那是桥层 / 玩法的事。
    void Resize(std::size_t newCapacity) override;

    [[nodiscard]] std::size_t OccupiedSlotCount() const override { return placed_.size(); }
    [[nodiscard]] std::size_t FreeCells() const override;
    [[nodiscard]] bool CanFit(std::size_t count, Footprint footprint) const override;

    SlotId Place(Guid guid, Footprint footprint) override;
    bool PlaceAt(Guid guid, SlotId slot, Footprint footprint) override;
    void Remove(Guid guid) override;
    void Clear() override;

    [[nodiscard]] SlotId SlotOf(Guid guid) const override;
    [[nodiscard]] Guid At(SlotId slot) const override;
    [[nodiscard]] const PosMap &Slots() const override { return anchorToGuid_; }

    // 格子背包不做自动整理:玩家的手摆布局就是布局本身,重排等于把人家
    // 摆好的东西全打乱。桥层看到 false 就只合并堆叠、不动位置。
    [[nodiscard]] bool SupportsCompaction() const override { return false; }
    // 注意：格子背包同样不许自动重排，但槽位没有具名语义。
    [[nodiscard]] bool HasSlotSemantics() const override { return false; }
    [[nodiscard]] bool IsCompact() const override { return true; }
    void ApplyOrder(const std::vector<Guid> &order) override;

private:
    struct Placement
    {
        SlotId anchor{kInvalidSlot};
        Footprint footprint{};
    };

    [[nodiscard]] bool RectFree(const std::vector<Guid> &cells, uint32_t x, uint32_t y,
                                Footprint footprint) const;
    [[nodiscard]] SlotId FindFreeRect(const std::vector<Guid> &cells, Footprint footprint) const;
    void Stamp(std::vector<Guid> &cells, SlotId anchor, Footprint footprint, Guid guid) const;

    uint32_t width_{0};
    uint32_t height_{0};
    std::vector<Guid> cells_{};  // 每个单元被谁占,kInvalidGuid = 空
    std::unordered_map<Guid, Placement> placed_{};
    PosMap anchorToGuid_{};
};
