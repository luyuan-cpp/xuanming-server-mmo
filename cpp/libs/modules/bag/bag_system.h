#pragma once

#include <cassert>
#include <cstddef>
#include <cstdint>
#include <memory>
#include <unordered_set>
#include <utility>
#include <vector>
#include <vector>

#include "engine/core/type_define/type_define.h"

#include "admission_policy.h"
#include "container_layout.h"
#include "eviction_policy.h"
#include "item_store.h"
#include "item_system.h"

// ─────────────────────────────────────────────────────────────────────────
// Bag —— 桥层。
//
// 背包由三层组成,Bag 自己不再持有任何一层的状态:
//
//   实例层 ItemStore(item_store.h)         —— 有哪些实例、各是什么、
//                                              数量怎么在实例之间分配
//   布局层 IContainerLayout(container_layout.h)—— 这些实例摆在哪、
//                                              还摆不摆得下
//   准入层 IAdmissionPolicy(admission_policy.h)—— 这个包收不收这种东西
//                                              (纯谓词,零副作用)
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

// ─────────────────────────────────────────────────────────────────────────
// BagProfile —— 一个背包的**一整套规则**,装配用。
//
// 「装备栏 = 具名槽 + 只收装备」这句话,在这之前是散在两处的:布局在
// PlayerBagsComp 的构造函数里,准入压根不存在。Profile 把它收成**一句话、
// 一个地方** —— 于是"这个包是什么"有了唯一的定义点,而不是每加一根轴就
// 多一处必须记得同改的调用点。
//
// 今天有三根轴(布局 + 准入 + 淘汰)。过期是后面的字段,加进来时调用点一行
// 都不用改 —— 这正是做成 struct 而不是三个 setter 的理由。
// 见 docs/design/bag-rule-policy-layering.md §3 的轴清单。
//
// 刻意**不带容量**:容量是 profile 给的**起点**,随后会被玩法解锁
// (ExpandCapacity)或快照还原(SetCapacityForRestore)覆盖,它是状态不是规则。
// 起点写在下面各个工厂里。
// 动态包(节日 / 活动 / 宠物包)的 profile id。0 = 未指定,按默认自由格装配。
// 固定包不用它 —— 它们由 BagType 指认,构造函数直接装。
// id 与工厂的对应关系在 bag_profile_registry.h,**必须持久化**(见那里的说明)。
inline constexpr uint32_t kBagProfileUnspecified{0};

struct BagProfile
{
    std::unique_ptr<IContainerLayout> layout;
    std::unique_ptr<IAdmissionPolicy> admission;
    std::unique_ptr<IEvictionPolicy> eviction;

    // 这套规则的注册 id。跟着包走进快照,还原时据此重新装配。
    // 固定包留 0:它们不经注册表,BagType 就是身份。
    uint32_t id{kBagProfileUnspecified};

    // 人物背包 / 仓库:自由格 + 什么都收 + 满了就拒。
    // 两者只有容量不同,所以共用一个工厂 —— 免得多一个只差一个数字的函数。
    static BagProfile Flat(std::size_t capacity)
    {
        return BagProfile{std::make_unique<FlatLayout>(capacity),
                          std::make_unique<AcceptAll>(),
                          std::make_unique<RejectWhenFull>()};
    }

    // 装备栏:具名槽(绝不被整理重排)+ 满了就拒。
    //
    // 「只收声明了部位的东西」**不在 admission 里** —— 它与"该进哪个槽、那个
    // 部位还有没有空槽"是同一个判定,必须与 Bag::PlaceInstance 同源,所以留在
    // 桥层(Bag::CanReserve)。这里给 AcceptAll 不是"没规则",是"准入这一根轴
    // 上没有额外规则"。详见 admission_policy.h 顶部那段。
    //
    // 淘汰给 RejectWhenFull 是**硬要求**,不是默认值:"为了给新手镯腾位而销毁
    // 你正戴着的手镯"不是任何游戏想要的行为。桥层对具名槽布局额外上了一道锁
    // (Bag::ReserveOrEvict 直接拒绝淘汰),两道一起才够。
    static BagProfile Equipment(std::size_t slotCount)
    {
        return BagProfile{std::make_unique<FixedSlotLayout>(slotCount),
                          std::make_unique<AcceptAll>(),
                          std::make_unique<RejectWhenFull>()};
    }

    // 临时格:自由格 + 什么都收 + **先进先出**。
    //
    // 这个包是掉落溢出的缓冲,语义本来就是"新的进来、旧的顶出去",而不是
    // "满了就捡不起来"。它是目前唯一一个会挤掉存量物品的包 —— 被挤掉的实例
    // 由 BagService::AddItem 落 LogItemDestroy,绝不无声消失。
    static BagProfile Temporary(std::size_t capacity)
    {
        return BagProfile{std::make_unique<FlatLayout>(capacity),
                          std::make_unique<AcceptAll>(),
                          std::make_unique<EvictOldestFirst>()};
    }

    // 节日 / 活动包:自由格 + **只收名单上的道具** + 满了就拒。
    //
    // 满了拒而不是先进先出,是刻意的:活动道具通常是限量发放的凭证 / 材料,
    // "为了塞新的而挤掉旧的"在这里是丢失而不是缓冲。要 FIFO 的活动自己注册
    // 一个换掉 eviction 的 profile 即可 —— 这正是三根轴正交的好处。
    //
    // 名单由创建这个包的玩法给出(活动系统本来就知道自己发哪些道具),
    // 不需要在物品表上加 tag 列。见 admission_policy.h 里 AcceptByConfigSet 的说明。
    static BagProfile Festival(std::size_t capacity, std::unordered_set<uint32_t> allowedConfigs)
    {
        return BagProfile{std::make_unique<FlatLayout>(capacity),
                          std::make_unique<AcceptByConfigSet>(std::move(allowedConfigs)),
                          std::make_unique<RejectWhenFull>()};
    }
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

    // ── 准入策略 ──────────────────────────────────────────────────────
    // 默认 AcceptAll,即拆分前的唯一行为(压根没人问过"这个包收不收")。
    // 换成 AcceptByTag(节日包)只需在这里换一个对象,其余三层都不用动。
    void SetAdmission(std::unique_ptr<IAdmissionPolicy> admission);
    [[nodiscard]] const IAdmissionPolicy &Admission() const { return *admission_; }

    // ── 淘汰策略 ──────────────────────────────────────────────────────
    // 默认 RejectWhenFull,即拆分前的唯一行为(那句 `// TODO: overflow to
    // temp bag or mail` 底下直接返回"背包满了")。
    // 换成 EvictOldestFirst(临时格 FIFO)只需在这里换一个对象。
    void SetEviction(std::unique_ptr<IEvictionPolicy> eviction);
    [[nodiscard]] const IEvictionPolicy &Eviction() const { return *eviction_; }

    // 一次装配一整套规则。生产代码请走这个,不要逐轴 Set —— 每加一根轴,
    // 逐轴调用的地方都得记得同改一遍,而 Profile 只改一处。
    void SetProfile(BagProfile profile);

    // 这个包按哪套注册规则装配的。0 = 没经过注册表(四个固定包、以及默认构造的
    // 动态包)。bag_marshal 持久化它,还原时据此重新装配 —— **规则本身不进快照,
    // 进快照的只是"这个包是哪套规则"这一个 id**。
    [[nodiscard]] uint32_t ProfileId() const { return profileId_; }

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

    // 批量入包前的 reserve:摆得下就什么都不做,摆不下就按淘汰策略腾位。
    //
    // **编排层(BagService)的批量入包用它,不要用 CheckSpaceFor。**
    // CheckSpaceFor 是纯预测("照现在这样放得下吗"),对会淘汰的包偏保守 ——
    // 它会抢在淘汰之前把整批拒掉,于是临时格的先进先出在批量路径上永远不生效。
    //
    // evictedOut(可为 nullptr)收集为腾位而销毁的实例,调用方必须落流水。
    //
    // **两个重载各自带齐了全部零副作用预检**(零数量、配置表、准入、预设 guid 撞车、
    // 铸号),并保证它们全跑在腾位之前。编排层不需要、也不应该在外面再补预检 ——
    // 第一版把预检留给 Bag::AddItems 自己,而 BagService 有另一套批量循环并不经过
    // 它,于是发号器被 fence 时临时格先挤掉旧物、再整批失败。把预检并进 reserve
    // 入口,这个错就没法再犯。
    uint32_t ReserveForBatchAdd(const ItemCountMap &itemsToAdd,
                                std::vector<DestroyedInstance> *evictedOut = nullptr);
    uint32_t ReserveForBatchAdd(const std::vector<InitItemParam> &itemsToAdd,
                                std::vector<DestroyedInstance> *evictedOut = nullptr);
    uint32_t CheckItemsAvailable(const ItemCountMap &requiredItems);
    // evictedOut(可为 nullptr)按退役顺序收集**本次入包为了腾位而销毁的实例**
    // (销毁前抓拍 guid/config/size)。今天只有临时格(EvictOldestFirst)会非空。
    // 与 MergeAndCompact 的 destroyedOut 是同一个形状、同一个理由:被挤掉的
    // item_uuid 是 transaction_log 做外挂回收的关联键,不留痕追溯链就断了。
    // 生产代码请走 BagService::AddItem / AddItems,那里会把回执落成流水。
    uint32_t AddItems(const ItemCountMap &itemsToAdd,
                      std::vector<DestroyedInstance> *evictedOut = nullptr);
    uint32_t AddItems(const std::vector<InitItemParam> &itemsToAdd,
                      std::vector<DestroyedInstance> *evictedOut = nullptr);
    uint32_t RemoveItems(const ItemCountMap &itemsToRemove);
    uint32_t RemoveItemByPos(const RemoveItemByPosParam &param);

    // 「满没满」是**布局层**的问题。拆分前它写作 items.size() >= capacity,
    // 拿实例数量去比格子数 —— 只有在"1 实例恒占 1 格"的前提下才成立。
    bool IsFull() const { return layout_->FreeCells() == 0; }

    // 只回答**格子数**够不够。
    //
    // ⚠ 对具名槽布局(装备栏)这个答案是不完整的:那里的容量是**按部位分桶**
    // 的,"还有 8 个空格"并不代表"还能再穿一只手镯"。入包路径一律走
    // CanReserve(),它才会按部位问。这个谓词保留是因为它是公开 API,
    // 且对自由格布局仍然正确。
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
                     std::vector<Guid> *writtenGuidsOut = nullptr,
                     std::vector<DestroyedInstance> *evictedOut = nullptr);
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
    // 只读展示占位；形状仍由桥层的唯一配置入口决定，调用者不自行猜测。
    [[nodiscard]] Footprint GetItemFootprintByGuid(Guid guid) const
    {
        const auto* item = store_.Find(guid);
        return item != nullptr ? FootprintFor(item->config_id()) : Footprint{0, 0};
    }

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
    // acquireSeq:快照里的入包序号。0 = 旧存档没盖过章,由 ItemStore 按重放顺序
    // 重新盖 —— 那至少还原出一个自洽的先后,好过拿 guid 猜。
    void InsertItemForRestore(Guid guid, uint32_t configId, uint32_t stackSize, uint32_t pos,
                              uint64_t acquireSeq = 0);

    // Set capacity (replays Bag::ExpandCapacity's effect without the audit log).
    // Used by Unmarshal to restore gameplay-unlocked slots.
    //
    // 拒绝把容量压到**已占槽位之下**:那会当场打破"每个实例恰占一格、槽位数
    // 不超过容量"的不变量 —— Debug 下下一次入包就断言,Release 下这个包"永远满"。
    // 正常还原序列是 Reset -> SetCapacity -> Insert,到不了这里;这是给日后任何
    // 误用者(GM 工具、新的还原路径)的闸。「位置可以变,物品不能丢」。
    void SetCapacityForRestore(std::size_t newCapacity);

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

    // 这件东西属于哪个**部位**,答案来自配置数据(`CfgItem.equip_kind`):
    //   0            -> 不是装备 / 不受槽位约束
    //   N > 0        -> 部位 N;它能进哪些槽由槽位表(`CfgEquipSlot`)说了算
    //   kInvalidSlot -> 物品表里压根没这个 config(哨兵)。它匹配不到任何
    //                   槽位行,于是空槽数恒为 0、一律被拒 —— 调用方不必单独判它。
    //
    // (这段注释一度写着 `CfgItem.equip_slot` = "只能进第 N 号槽",那是被推翻的
    //  上一版模型 —— 它表达不了"同一部位有两个槽"。已随实现更正。)
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

    // 这个部位当前还空着几个槽。`FindFreeSlotForKind` 的计数版,两者**必须**
    // 走同一套筛选(同 kind + 槽位在容量内 + 当前空着),否则 reserve 与 commit
    // 会给出不一致的答案 —— 那正是下面 CanReserve 要消灭的那类 bug。
    [[nodiscard]] std::size_t CountFreeSlotsForKind(uint32_t kind) const;

    // reserve 段:这一批**新实例**摆不摆得下。零副作用。
    //
    // instancesByConfig 是 config -> 本次要为它新建的实例数(不是单位数 ——
    // 可叠加物品并进既有堆的那部分不新建实例,也就不占槽位)。
    // totalInstances 是它们的和,单独传是为了省一次求和。
    //
    // **它必须与 PlaceInstance 在同一个条件上分叉**(`HasSlotSemantics()`)。
    // 这是本函数存在的全部理由:在它之前,reserve 一律用 `layout_->CanFit()`
    // 作答,而那对具名槽是**说谎** —— FlatLayout::CanFit 是 `FreeCells() >= n`,
    // FixedSlotLayout 继承了它却没有覆盖,于是"装备栏还有 10 个空格"会让
    // 第 3 只手镯通过预检,然后在 commit 中途放不下,留下**前两只已入包却返回
    // 失败**的半批 —— 正是 AddItems 注释里发誓不会发生的那件事。
    // 详见 docs/design/bag-rule-policy-layering.md §6.1。
    // plan 段:校验配置表与准入,算出这一批**要为每个 config 新建几个实例**。
    // 零副作用,不问摆不摆得下 —— 那是 reserve 的事。
    //
    // 抽出来是因为它有两个用途且**不能共用同一个结尾**:
    //   * CheckSpaceFor(公开预测,必须纯)= PlanInstances + CanReserve
    //   * AddItems(真入包,可淘汰)      = PlanInstances + ReserveOrEvict
    // 从前两者是一个函数,于是会淘汰的包在批量路径上根本没机会淘汰:
    // CheckSpaceFor 先一步把整批拒了。
    //
    // exclude(可为 nullptr):把这些 guid 的实例当作已不存在来算 —— 淘汰腾位前
    // 用它回答"如果这几件被挤掉,这批还要新建几个实例"。见 ReserveOrEvict。
    uint32_t PlanInstances(const ItemCountMap &itemsToAdd, ItemCountMap *instancesByConfigOut,
                           std::size_t *totalInstancesOut,
                           const std::unordered_set<Guid> *exclude = nullptr);

    // 批量入包的 reserve 段:先用 PlanInstances 拿精确错误码(配置表 / 准入),
    // 再交给 ReserveOrEvict(摆得下直接过;摆不下就先算到不动点再腾位)。
    // totalInstancesOut(可为 nullptr)给的是**腾位之后**的实例数。
    uint32_t ReserveBatch(const ItemCountMap &itemsToAdd, std::size_t *totalInstancesOut,
                          std::vector<DestroyedInstance> *evictedOut);

    [[nodiscard]] bool CanReserve(const ItemCountMap &instancesByConfig,
                                  std::size_t totalInstances) const;

    // reserve 段的完整版:摆不下时先按淘汰策略腾位,腾够了再回答"能放"。
    // 入参是 config -> **单位数**(不是实例数):它要自己反复规划。
    //
    // **这是唯一一处会在 reserve 段改状态的地方**,所以它的纪律最严:
    //   * **先算到不动点,再销毁。** 被挤掉的可能正是本批打算并进去的未满堆,
    //     那样需求会变大 —— 所以把牺牲者当作已不存在重新规划,需求变大就多选,
    //     直到"腾出的格子 >= 排除牺牲者后的需求"稳定下来,全程零副作用;稳定了
    //     才真的 DestroyItem。于是"腾不够就一件都不动"是**结构保证**,不是祈愿。
    //     (2026-09-01 第一版按淘汰前的规划算缺口,临时格里拾取同种材料就会
    //      "先销毁旧堆、再放不下、返回失败" —— 玩家净损失。见设计文档 §6.3。)
    //   * 腾位是一次**独立且完整**的提交,发生在这一批写入任何东西之前 ——
    //     于是三段仍然成立:要么腾完再放,要么一件不动;
    //   * 具名槽布局**一律不淘汰**;容量不是标量的布局(格子碎片)也不淘汰;
    //   * **没有回执就不淘汰**(evictedOut == nullptr 直接拒):被销毁的实例必须
    //     经 evictedOut 回执给调用方、由 BagService 落流水 —— item_uuid 是外挂
    //     回收的关联键,绝不能无声消失。这条此前只是注释,现在是闸。
    [[nodiscard]] bool ReserveOrEvict(const ItemCountMap &unitsToAdd,
                                      std::vector<DestroyedInstance> *evictedOut);

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
    // evictedOut 透传给 ReserveOrEvict —— 腾位发生在这两个函数的 reserve 段。
    uint32_t AddNonStackableItem(ItemComp itemProto, std::vector<Guid> *writtenGuidsOut,
                                 std::vector<DestroyedInstance> *evictedOut);
    uint32_t AddStackableItem(ItemComp itemProto, uint32_t maxStackSize,
                              std::vector<Guid> *writtenGuidsOut,
                              std::vector<DestroyedInstance> *evictedOut);

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
    // 默认什么都收 —— 与 layout_ 默认扁平同理:默认值必须等于拆分前的行为,
    // 否则 bag_marshal 那些默认构造出来的动态包会悄悄换语义。
    std::unique_ptr<IAdmissionPolicy> admission_{std::make_unique<AcceptAll>()};
    // 默认满了就拒 —— 同上,默认值必须等于拆分前的行为。一个默认构造出来的包
    // 绝不能自作主张挤掉玩家的东西。
    std::unique_ptr<IEvictionPolicy> eviction_{std::make_unique<RejectWhenFull>()};
    uint32_t profileId_{kBagProfileUnspecified};
    Guid playerGuid{kInvalidGuid};
};
