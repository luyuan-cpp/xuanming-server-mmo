#pragma once

#include <unordered_map>
#include <vector>

#include "entt/src/entt/entity/registry.hpp"
#include "engine/core/type_define/type_define.h"

#include "item_system.h"

using ItemsMap = std::unordered_map<Guid, entt::entity>;
using PosMap = std::unordered_map<uint32_t, Guid>;

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

class Bag
{
public:
    // Capacity = how many grid slots this bag has unlocked. NOT the number
    // of items currently held (that's OccupiedGridCount()).
    std::size_t Capacity() const { return capacity; }
    [[nodiscard]] Guid PlayerGuid() const { return playerGuid; }
    std::size_t OccupiedGridCount() const { return items.size(); }
    // Number of grid slots recorded in the position map. Invariant: equals
    // OccupiedGridCount() — every held item owns exactly one grid slot.
    std::size_t GridSlotCount() const { return posToGuid.size(); }
    const PosMap &GridSlots() const { return posToGuid; }

    std::size_t GetTotalItemCount(uint32_t config_id) const;
    ItemComp *GetItemCompByGuid(Guid guid);
    ItemComp *GetItemCompByPos(uint32_t pos);
    entt::entity GetItemByGuid(Guid guid);
    entt::entity GetItemByPos(uint32_t pos);

    uint32_t CheckSpaceFor(const ItemCountMap &itemsToAdd);
    uint32_t CheckItemsAvailable(const ItemCountMap &requiredItems);
    uint32_t AddItems(const ItemCountMap &itemsToAdd);
    uint32_t AddItems(const std::vector<InitItemParam> &itemsToAdd);
    uint32_t RemoveItems(const ItemCountMap &itemsToRemove);
    uint32_t RemoveItemByPos(const RemoveItemByPosParam &param);

    bool IsFull() const { return items.size() >= Capacity(); }
    bool IsSpaceInsufficient(std::size_t gridCount) const
    {
        AssertCapacityInvariant();
        return Capacity() - items.size() < gridCount;
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

    bool MergeAndCompact();
    void ExpandCapacity(std::size_t additionalSize);

    // 注意:这里曾经有一个 static LastGeneratedItemGuid(),回读发号器里的"上一个号"当作
    // AddItem 的隐式返回值。它已被 AddItem 的 writtenGuidsOut 回执取代并删除 ——
    // 那条通道会被同一发号器铸出的 tx_id / snapshot_id 覆盖,且不铸号的路径读到的是上一件
    // 物品的残值。新增代码一律用回执,不要再引入任何"发完再回读"的隐式通道。

    static std::size_t GridsNeededFor(std::size_t totalSize, std::size_t maxStackSize);

    // ── Persistence/migration accessors ───────────────────────────────
    // Added 2026-05-17 to let bag_marshal serialize/restore the bag's
    // state across cross-zone migration / rollback / persistence
    // (see docs/design/cross-zone-readiness-audit.md §3.2 件 1 +
    // cpp/libs/services/scene/player/system/bag_marshal.{h,cpp}).
    //
    // These accessors are intentionally minimal — they expose just
    // enough surface to iterate items and rebuild from a snapshot,
    // without leaking ECS internals. Production gameplay still goes
    // through AddItem / RemoveItem / MergeAndCompact / ExpandCapacity above.

    // Iterate items as (guid, ItemComp) pairs. The ItemComp is the
    // proto stored in the bag's internal item registry — caller MUST
    // NOT mutate or persist the pointer beyond the loop, the next
    // AddItem/RemoveItem can invalidate it.
    template <typename Fn>
    void ForEachItem(Fn&& fn) const
    {
        // item.item_id() IS the guid (it's the key under which items
        // stored this entity — see InsertItemEntity). No reverse scan
        // of items needed; this keeps iteration O(n) instead of O(n²).
        for (const auto& [entity, item] : itemRegistry.view<ItemComp>().each())
        {
            fn(static_cast<Guid>(item.item_id()), item);
        }
    }

    // Get item position by guid (inverse of posToGuid). Returns kInvalidU32Id
    // if the guid isn't in this bag. Used by marshal to fill
    // ItemEntry.pos.
    uint32_t GetItemPosByGuid(Guid guid) const
    {
        for (const auto& [pos, mappedGuid] : posToGuid)
        {
            if (mappedGuid == guid)
            {
                return pos;
            }
        }
        return kInvalidU32Id;
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
    void SetCapacityForRestore(std::size_t newCapacity)
    {
        capacity = newCapacity;
    }

private:
    // ── Item-storage invariant helpers ────────────────────────────────
    // items (guid→entity) and itemRegistry (entity→ItemComp) MUST stay
    // in lockstep. To make it impossible to update one and forget the
    // other, ALL creation goes through InsertItemEntity and ALL removal
    // through DestroyItem / ClearAllItems. Do not call itemRegistry
    // .create()/.destroy() or items.emplace()/.erase() directly anywhere
    // else.

    // Create the ECS entity for `proto`, store the component, and register
    // it in items — atomically. Returns the stored component, or nullptr
    // if the guid already exists (the half-created entity is rolled back).
    // `proto` must already have its item_id / config_id / size set.
    ItemComp *InsertItemEntity(ItemComp proto);

    // Remove one item everywhere: destroys its ECS entity, drops it from
    // items, and frees its grid slot in posToGuid. Safe to call with an
    // unknown guid (no-op).
    void DestroyItem(Guid guid);

    // Drop every item the bag holds (entities + items + posToGuid). capacity
    // is left untouched. Used by ResetFromSnapshot.
    void ClearAllItems();

    Guid GenerateItemGuid();
    bool IsInvalidItemGuid(const ItemComp &item) const;
    uint32_t AllocateGridSlot(Guid guid);
    static bool CanStack(const ItemComp &leftItem, const ItemComp &rightItem);

    // Read-only early-out for MergeAndCompact, which runs on EVERY bag open
    // (hot path). Returns true when the bag is already optimal so the caller
    // can skip the destroy/rebuild entirely — and, just as important, avoid
    // reshuffling an already-tidy layout under the player. Optimal means all
    // three hold: (1) no stackable config has 2+ partial stacks to merge,
    // (2) grid slots are contiguous 0..n-1, (3) slots are laid out by
    // (config_id ascending, size descending) — same configs grouped together,
    // full stacks before partials within each group.
    bool IsAlreadyMergedAndCompact() const;

    // writtenGuidsOut(可为 nullptr)按写入顺序收集**本次调用实际落到背包里的实例 guid**:
    // 新铸的、沿用调用方预设的、以及被并入的既有堆,都算。调用方据此写流水,
    // 不必再回读任何"上一次发号"的残值。
    uint32_t AddNonStackableItem(ItemComp itemProto, std::vector<Guid> *writtenGuidsOut);
    uint32_t AddStackableItem(ItemComp itemProto, uint32_t maxStackSize,
                              std::vector<Guid> *writtenGuidsOut);

    // ── CheckSpaceFor helper ──────────────────────────────────────────
    // For every config in itemsToAdd, sum the spare room left in the bag's
    // existing stacks of that config (config_id -> free units). Configs not
    // being added, and non-stackable configs (full at size 1), contribute 0.
    ItemCountMap MeasureFreeRoomPerConfig(const ItemCountMap &itemsToAdd) const;

    // ── AddStackableItem helpers ──────────────────────────────────────
    // One planned top-up: pour `amount` more units into existing stack `entity`.
    struct StackFill
    {
        entt::entity entity;
        uint32_t amount;
    };

    // Plan (no mutation) how the incoming units fill existing stacks of the
    // same config. Fills outFillPlan and returns the leftover count that still
    // needs brand-new grids.
    uint32_t PlanStackIntoExistingStacks(const ItemComp &proto, uint32_t maxStackSize,
                                         std::vector<StackFill> &outFillPlan) const;

    // Execute a plan from PlanStackIntoExistingStacks (mutates stack sizes).
    void ApplyStackFill(const std::vector<StackFill> &fillPlan,
                        std::vector<Guid> *writtenGuidsOut);

    // Place `remaining` leftover units into `newGridCount` freshly created
    // grids, maxStackSize units per grid.
    uint32_t SpillIntoNewGrids(ItemComp proto, uint32_t maxStackSize,
                               uint32_t remaining, std::size_t newGridCount,
                               std::vector<Guid> *writtenGuidsOut);

    // Drain `count` units of one config from its stacks. The quantity may be
    // spread across several stacks; each drained stack keeps its grid slot
    // (size becomes 0). Precondition: the bag holds at least `count` of
    // configId (callers go through CheckItemsAvailable first).
    void DrainItemStacks(uint32_t configId, uint32_t count);

    std::size_t EmptyGridCount() const
    {
        AssertCapacityInvariant();
        return Capacity() - items.size();
    }
    void AssertCapacityInvariant() const { assert(Capacity() >= items.size()); }

    ItemsMap items{};
    PosMap posToGuid{};
    std::size_t capacity{kDefaultCapacity};
    entt::registry itemRegistry{};
    Guid playerGuid{kInvalidGuid};
};
