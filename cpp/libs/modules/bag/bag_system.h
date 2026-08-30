#pragma once

#include <cassert>
#include <cstddef>
#include <cstdint>
#include <memory>
#include <utility>
#include <vector>

#include "engine/core/type_define/type_define.h"

#include "container_layout.h"
#include "item_store.h"
#include "item_system.h"

// ─────────────────────────────────────────────────────────────────────────
// Bag —— 桥层。
//
// 背包由两层组成,Bag 自己不再持有任何一层的状态:
//
//   实例层 ItemStore(item_store.h)         —— 有哪些实例、各是什么、
//                                              数量怎么在实例之间分配
//   布局层 IContainerLayout(container_layout.h)—— 这些实例摆在哪、
//                                              还摆不摆得下
//
// Bag 只做一件事:实现**同时需要两层才能回答**的玩法规则(堆叠 + 摆放 +
// 整理),并且严格按「规划 -> 预留 -> 提交」三段走:
//
//   plan    : 问实例层「这批数量要填哪几个既有堆、还要新建几个实例」  不碰布局
//   reserve : 问布局层「这几个新实例摆得下吗」                        不碰实例
//   commit  : 两层一起写;失败一定发生在 reserve 之前,绝无半写状态
//
// 拆分之前这三段是揉在一起的:IsFull() 拿实例数量去比容量、MergeAndCompact()
// 一个函数既改 size 又重排格子、DestroyItem() 销毁实体后还要线扫 posToGuid。
// 详见 docs/design/bag-instance-layout-split.md。
//
// **公开 API 与拆分前逐字一致** —— BagService / bag_marshal / PlayerBagsComp /
// bag_test 一行都不用改。这也是这次重构的验收判据。
// ─────────────────────────────────────────────────────────────────────────

constexpr std::size_t kDefaultCapacity{10};
constexpr std::size_t kEquipmentCapacity{10};
constexpr std::size_t kBagMaxCapacity{100};
constexpr std::size_t kTempBagMaxCapacity{200};
constexpr std::size_t kWarehouseMaxCapacity{200};

// Fixed, always-present bag slots that every player owns. Stored as a
// dense std::array<Bag, kBagTypeCount> in PlayerBagsComp, so each value
// doubles as BOTH the array index AND the persisted bag_type on the wire
// (BagAllData.ItemEntry.bag_type).
//
// Extending this enum:
//   * New PERMANENT per-player bag (pet bag / material bag / ...): append
//     a new entry right before kBagTypeCount and bump nothing else. Never
//     renumber an existing value — the numbers are a persistence contract.
//   * Runtime/temporary bags (event bags, per-pet bags that come and go):
//     these have NO fixed slot, do not add them here. Put them in an
//     id-keyed map instead — see the note in player_bags_comp.h.
enum BagType : uint32_t
{
    kInventory = 0,  // main character inventory (人物背包)
    kWarehouse = 1,  // bank / storage           (仓库)
    kEquipment = 2,  // worn equipment slots     (装备栏)
    kTemporary = 3,  // overflow / loot pickup   (溢出临时格)
    kBagTypeCount = 4,
};

struct RemoveItemByPosParam
{
    Guid item_guid{kInvalidGuid};
    uint32_t item_config_id{kInvalidU32Id};
    uint32_t pos{kInvalidU32Id};
    uint32_t size{1};
};

// 一条"整理时被销毁的实例"的销毁前抓拍。
//
// 整理会把被合并空的堆、以及 size==0 的僵尸实例连 guid 一起销毁,而 item_uuid
// 正是 transaction_log 做外挂回收关联的键 —— 实例消失必须留痕,否则追溯链在
// 整理这一步无声断掉。Bag 是纯容器不写流水,所以它只负责**报告**销毁了哪些,
// 由 BagService::MergeAndCompact 落流水。
struct DestroyedInstance
{
    Guid guid{kInvalidGuid};
    uint32_t configId{0};
    uint32_t size{0};
};

// 整理到什么程度。
//
// 这个枚举存在的理由是**两个意图会打架**:
//   "东西还在你离开时摆的地方"(跨服还原忠实复现源服的 pos)
//   "开背包自动整理"(重排会把上面那句话抹掉)
// 拆分前只有后者,于是前者只在布局恰好已是 (config 升序, size 降序) 时才成立 ——
// 不是 bug,是没人做过取舍。现在把取舍变成调用点必须填的参数:
// BagService 的编排入口**不给默认值**,你没法在不表态的情况下调用它。
enum class CompactPolicy : uint8_t
{
    // 只合并堆叠 + 回收 size==0 的空实例,**一格都不挪**。
    // 自动触发的路径(开背包、入包后顺手整理)应该用这个:它是幂等的、
    // 不会把玩家或源服摆好的位置洗掉。
    kMergeOnly,

    // 在上面的基础上,再按 (config 升序, size 降序) 重新铺满 0..n-1。
    // 玩家显式点"整理"才用这个。
    kMergeAndReorder,
};

class Bag
{
public:
    // 特殊成员函数全部走隐式生成:layout_ 用类内初始化器给出默认布局,
    // 于是 Bag 保持"默认可构造 + 可移动 + 不可复制"—— 与拆分前(entt::registry
    // 本身就不可复制)完全一致。PlayerBagsComp 的 std::array<Bag,4> 和 entt
    // 的组件存储都依赖这一点,别去手写它们。

    // ── 布局策略 ──────────────────────────────────────────────────────
    // 默认 FlatLayout(kDefaultCapacity),即拆分前的唯一行为。
    // 换成 GridLayout / 未来的 FixedSlotLayout 只需在这里换一个对象,
    // 实例层与本类的其余部分一行都不用改。
    void SetLayout(std::unique_ptr<IContainerLayout> layout);
    [[nodiscard]] const IContainerLayout &Layout() const { return *layout_; }

    // Capacity = how many grid slots this bag has unlocked. NOT the number
    // of items currently held (that's OccupiedGridCount()).
    std::size_t Capacity() const { return layout_->Capacity(); }
    [[nodiscard]] Guid PlayerGuid() const { return playerGuid; }

    // 记下这个背包属于谁。**只用于日志**,不参与任何判定。
    //
    // 拆分前这个成员全仓没有任何写入点,于是每一条带 player= 的错误日志打的都是
    // kInvalidGuid 哨兵 —— 排障时等于没有。现在由 bag_marshal 在装配
    // PlayerBagsComp 时调用(玩家 guid 就挂在实体上,与 TransactionLogSystem::
    // ResolvePlayerId 取的是同一个 Guid 组件)。
    void SetPlayerGuid(Guid guid) { playerGuid = guid; }
    std::size_t OccupiedGridCount() const { return store_.Size(); }
    // Number of grid slots recorded in the position map. Invariant: equals
    // OccupiedGridCount() — every held item owns exactly one grid slot.
    std::size_t GridSlotCount() const { return layout_->OccupiedSlotCount(); }
    const PosMap &GridSlots() const { return layout_->Slots(); }

    std::size_t GetTotalItemCount(uint32_t config_id) const;
    ItemComp *GetItemCompByGuid(Guid guid);
    ItemComp *GetItemCompByPos(uint32_t pos);
    entt::entity GetItemByGuid(Guid guid);
    entt::entity GetItemByPos(uint32_t pos);

    // instancesNeededOut(可为 nullptr)回执:这一批总共要**新建多少个实例**。
    // 批量入包拿它做铸号预检 —— 见 AddItems 的注释。
    uint32_t CheckSpaceFor(const ItemCountMap &itemsToAdd,
                           std::size_t *instancesNeededOut = nullptr);
    uint32_t CheckItemsAvailable(const ItemCountMap &requiredItems);
    uint32_t AddItems(const ItemCountMap &itemsToAdd);
    uint32_t AddItems(const std::vector<InitItemParam> &itemsToAdd);
    uint32_t RemoveItems(const ItemCountMap &itemsToRemove);
    uint32_t RemoveItemByPos(const RemoveItemByPosParam &param);

    // 「满没满」是**布局层**的问题。拆分前它写作 items.size() >= capacity,
    // 拿实例数量去比格子数 —— 只有在"1 实例恒占 1 格"的前提下才成立。
    bool IsFull() const { return layout_->FreeCells() == 0; }
    bool IsSpaceInsufficient(std::size_t gridCount) const
    {
        AssertCapacityInvariant();
        return !layout_->CanFit(gridCount, kSingleCell);
    }

    // writtenGuidsOut(可为 nullptr)按写入顺序收集**本次调用实际落到背包里的实例 guid**。
    //
    // 这是取代 LastGeneratedItemGuid() 那条隐式返回通道的显式回执。旧做法是发完号再回读
    // 发号器里的"上一个号"残值,有两个硬伤:①同一个 tls 发号器还在铸 tx_id / snapshot_id,
    // 它们会把残值覆盖掉,于是流水里的 item_uuid 可能根本不是任何一件物品的 guid;
    // ②纯并堆(remaining==0)与"单件沿用预设 guid"这两条路径**根本不铸号**,残值是上一件
    // 物品留下的,直接张冠李戴。显式回执把"这次写了哪些实例"变成返回值,两个问题一起消失。
    uint32_t AddItem(const InitItemParam &initItemParam,
                     std::vector<Guid> *writtenGuidsOut = nullptr);
    uint32_t RemoveItem(Guid guid);

    // destroyedOut(可为 nullptr)按销毁顺序收集**本次整理销毁掉的实例**(销毁前
    // 抓拍 guid/config/size)。整理会退役掉一批 item_uuid,而那是 transaction_log
    // 的关联键;Bag 是纯容器不写流水,所以把"退役了哪些"做成返回值,由
    // BagService::MergeAndCompact 落流水。生产代码请走 BagService 那个入口。
    //
    // policy 决定要不要重排位置(见 CompactPolicy)。这里保留
    // kMergeAndReorder 作为默认值**只是为了让既有调用方和用例不用改**;
    // 生产代码请走 BagService::MergeAndCompact,那个入口强制你表态。
    bool MergeAndCompact(std::vector<DestroyedInstance> *destroyedOut = nullptr,
                         CompactPolicy policy = CompactPolicy::kMergeAndReorder);
    void ExpandCapacity(std::size_t additionalSize);

    // 注意:这里曾经有一个 static LastGeneratedItemGuid(),回读发号器里的"上一个号"当作
    // AddItem 的隐式返回值。它已被 AddItem 的 writtenGuidsOut 回执取代并删除 ——
    // 那条通道会被同一发号器铸出的 tx_id / snapshot_id 覆盖,且不铸号的路径读到的是上一件
    // 物品的残值。新增代码一律用回执,不要再引入任何"发完再回读"的隐式通道。

    // 名字保留给既有调用方;实现已下沉到实例层的 ItemStore::StacksNeededFor。
    // 它算的是"这批数量要拆成几个实例",不是"占几格" —— 在扁平布局下两者恰好
    // 相等,格子布局下就不等了。
    static std::size_t GridsNeededFor(std::size_t totalSize, std::size_t maxStackSize);

    // ── Persistence/migration accessors ───────────────────────────────
    // Added 2026-05-17 to let bag_marshal serialize/restore the bag's
    // state across cross-zone migration / rollback / persistence
    // (see docs/design/cross-zone-readiness-audit.md §3.2 件 1 +
    // cpp/libs/services/scene/player/system/bag_marshal.{h,cpp}).
    //
    // These accessors are intentionally minimal — they expose just
    // enough surface to iterate items and rebuild from a snapshot,
    // without leaking either layer's internals. Production gameplay still
    // goes through AddItem / RemoveItem / MergeAndCompact / ExpandCapacity.

    // Iterate items as (guid, ItemComp) pairs. The ItemComp is the
    // proto stored in the item store — caller MUST NOT mutate or persist
    // the pointer beyond the loop, the next AddItem/RemoveItem can
    // invalidate it.
    template <typename Fn>
    void ForEachItem(Fn &&fn) const
    {
        store_.ForEach(std::forward<Fn>(fn));
    }

    // Get item position by guid. Returns kInvalidU32Id if the guid isn't
    // in this bag. Used by marshal to fill ItemEntry.pos.
    //
    // 拆分前这里是遍历 posToGuid 比对 value 的 O(n) 线扫;布局层有了自己的
    // 反向索引之后是 O(1)。语义完全不变。
    uint32_t GetItemPosByGuid(Guid guid) const { return layout_->SlotOf(guid); }

    // 跨层一致性:每个实例恰好占一个槽位,且槽位数不超过容量。
    //
    // 做成**公开谓词**而不是只有一个私有 assert,是为了让用例能直接验它 ——
    // `assert` 在 Release 下会被整个编译掉,只靠断言等于在发行版里没有任何保护,
    // 也没法写出"这个操作之后两层仍然一致"这种用例。AssertLayerConsistency()
    // 只是它在 Debug 下的断言包装。
    [[nodiscard]] bool IsLayerConsistent() const
    {
        return store_.Size() == layout_->OccupiedSlotCount() &&
               layout_->Capacity() >= layout_->OccupiedSlotCount();
    }

    // Replace the bag's contents wholesale. Used by bag_marshal::Unmarshal
    // when receiving a migrating player or restoring a snapshot —
    // destroys whatever was here and rebuilds from the snapshot.
    //
    // NOT for gameplay use. Skips AddItem's anomaly detection /
    // transaction_log emission / gain-block checks because the items
    // were already validated when they were originally added in the
    // source zone / pre-snapshot state.
    void ResetFromSnapshot();

    // Insert one item entry at a known position with a known guid.
    // Companion to ResetFromSnapshot. Used by bag_marshal::Unmarshal
    // to replay items one by one without re-running stack/anomaly logic.
    void InsertItemForRestore(Guid guid, uint32_t configId, uint32_t stackSize, uint32_t pos);

    // Set capacity (replays Bag::ExpandCapacity's effect without the audit log).
    // Used by Unmarshal to restore gameplay-unlocked slots.
    void SetCapacityForRestore(std::size_t newCapacity) { layout_->Resize(newCapacity); }

private:
    // ── 两层成对操作(桥层的核心职责)────────────────────────────────
    // 销毁一个实例:实例层抹掉实体与索引,布局层释放槽位。两边必须成对,
    // 所以它只能长在桥层。拆分前这是 Bag::DestroyItem,一个函数里既 destroy
    // 实体、又线扫 posToGuid 找槽位 —— 那正是两层焊死的样子。
    void DestroyItem(Guid guid);

    // 一件该 config 的实例在布局里占的形状。
    // **今天恒为 1x1**:配置表还没有宽高列,扁平布局也用不上。真要做格子背包时
    // 唯一要改的就是这一个函数(去读 CfgItem 的 grid_w / grid_h),实例层与布局层
    // 都不用动。这是"形状从哪来"的唯一入口,别在别处推导。
    [[nodiscard]] static Footprint FootprintFor(uint32_t /*configId*/) { return kSingleCell; }

    // 这件东西**该去哪个槽**,答案来自配置数据(`CfgItem.equip_slot`):
    //   0            -> 不是装备 / 不受槽位约束,返回 kInvalidSlot
    //   N > 0        -> 只能进具名槽布局的第 N 号槽位
    //
    // 槽位号的含义(1=头 2=胸 …)**完全由表数据定义**,C++ 里没有任何地方
    // 写死过某个槽位号 —— 策划改表即可,两层都不用动。
    //
    // 它需要同时知道 config 和槽位,而布局层永远不认识 config,所以这道查询
    // 天然属于桥层。
    [[nodiscard]] static uint32_t EquipKindFor(uint32_t configId);

    // 在槽位表里找第一个「接受这个部位、且当前空着」的槽。
    //
    // 同一部位可以有**多个**槽（两个手镯位就是表里两行 equip_kind 相同），
    // 所以这里是「找第一个空的」，不是「找那个唯一的」—— 上一版的
    // 模型（equip_slot = 只能进第 N 号槽）根本表达不了这个需求。
    [[nodiscard]] SlotId FindFreeSlotForKind(uint32_t kind) const;

    // 把一个实例放进布局:
    //   * 具名槽布局(装备栏)-> 按「物品的部位 + 槽位表」找第一个空的接受槽,
    //     没有声明部位的东西压根不该进这个包;
    //   * 其余布局           -> first-fit,与拆分前一致。
    // 放不下返回 kInvalidSlot,不留下任何半写状态。
    [[nodiscard]] SlotId PlaceInstance(Guid guid, uint32_t configId);

    // 整理用:实例层按 (config 升序, size 降序) 给出期望顺序,布局层负责落位。
    [[nodiscard]] GuidVector DesiredOrder() const;

    // 当前槽位顺序是否已满足 (config 升序, size 降序)。需要同时读两层,
    // 所以留在桥层。
    [[nodiscard]] bool IsOrderedByConfigThenSize() const;

    // writtenGuidsOut(可为 nullptr)按写入顺序收集**本次调用实际落到背包里的实例 guid**:
    // 新铸的、沿用调用方预设的、以及被并入的既有堆,都算。调用方据此写流水,
    // 不必再回读任何"上一次发号"的残值。
    uint32_t AddNonStackableItem(ItemComp itemProto, std::vector<Guid> *writtenGuidsOut);
    uint32_t AddStackableItem(ItemComp itemProto, uint32_t maxStackSize,
                              std::vector<Guid> *writtenGuidsOut);

    // 把 remaining 个剩余单位铺进 newInstanceCount 个新建实例,每个最多
    // maxStackSize 个,并为每个实例向布局层申请一个位置。
    uint32_t SpillIntoNewInstances(ItemComp proto, uint32_t maxStackSize,
                                   uint32_t remaining, std::size_t newInstanceCount,
                                   std::vector<Guid> *writtenGuidsOut);

    std::size_t EmptyGridCount() const
    {
        AssertCapacityInvariant();
        return layout_->FreeCells();
    }
    void AssertCapacityInvariant() const { assert(layout_->Capacity() >= store_.Size()); }

    // 跨层一致性:每个实例恰好占一个槽位,槽位数不超过容量。
    //
    // 拆分前这条不变量只写在 GridSlotCount() 的注释里(“Invariant: equals
    // OccupiedGridCount()”),**代码里从没断言过**,两张表可以静默漂移。拆成两个
    // 对象之后漂移在结构上更容易发生而不是更难,所以必须真断言 —— 每个公开写入
    // 方法的出口都调它一次。Release 下 assert 被编译掉,零开销。
    void AssertLayerConsistency() const { assert(IsLayerConsistent()); }

    ItemStore store_{};
    std::unique_ptr<IContainerLayout> layout_{std::make_unique<FlatLayout>(kDefaultCapacity)};
    Guid playerGuid{kInvalidGuid};
};
