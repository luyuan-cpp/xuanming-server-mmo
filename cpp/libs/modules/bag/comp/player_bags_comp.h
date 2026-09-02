#pragma once

#include <array>
#include <memory>
#include <unordered_map>

#include "entt/src/entt/entity/registry.hpp"
#include "modules/bag/bag_system.h"

// PlayerBagsComp — ECS component that holds all four Bag instances for
// one player on a scene node.
//
// Created by bag_marshal::Unmarshal via get_or_emplace, i.e. when a
// snapshot / cross-zone payload / login blob is applied to the entity.
// It goes away with the entity on DestroyPlayer.
//
// (This block used to say it was emplace'd in player_lifecycle.cpp at
// InitPlayerFromAllData time. That was never true — grep for
// PlayerBagsComp: player_lifecycle.cpp does not mention it. Corrected
// 2026-08-27. A player who never goes through Unmarshal therefore has no
// bags at all, which is why Marshal treats a missing component as an
// empty payload rather than an error.)
//
// Before this component existed (pre-2026-05-17), the Bag class in
// cpp/libs/modules/bag/bag_system.h was constructed only in
// cpp/tests/bag_test — production code had ZERO instantiations.
// That meant cross-zone migration / persistence / rollback couldn't
// carry bag data, because there was no in-memory bag to read from.
//
// Slot layout matches BagType in bag_system.h. The constructor below is
// what actually establishes this — before 2026-08-27 the table was comment
// only and every bag silently got kDefaultCapacity (10) with a flat layout:
//   bags[kInventory]  = main inventory   FlatLayout(kBagMaxCapacity)
//   bags[kWarehouse]  = warehouse        FlatLayout(kWarehouseMaxCapacity)
//   bags[kEquipment]  = equipment slots  FixedSlotLayout(kEquipmentCapacity)
//   bags[kTemporary]  = temp/loot bag    FlatLayout(kTempBagMaxCapacity)
//
// kEquipment uses FixedSlotLayout so "整理" can never shuffle worn gear
// between slots. The slot taxonomy (which gear fits which slot) lives in
// config — `CfgItem.equip_kind` + `CfgEquipSlot` — see the note on
// FixedSlotLayout in container_layout.h and
// docs/design/bag-instance-layout-split.md §11.2(d).
//
// (This block used to say the taxonomy "does not exist in config yet". That
// stopped being true on 2026-08-27 when the two tables landed. Corrected
// 2026-09-01.)
//
// Capacities themselves are persisted in BagAllData.capacities so that
// gameplay-unlocked slots (Bag::ExpandCapacity) survive across cross-zone hops.
//
// Two storage tiers:
//   * bags          — the fixed, always-present core bags. Dense array
//                     indexed by BagType, cache-friendly fast path.
//   * dynamicBags_  — runtime/temporary bags that come and go (event
//                     bags, per-pet bags). Keyed by a uint64 id (event
//                     id / pet guid). NOT every player has these, and
//                     the set changes at runtime, so they can't live in
//                     the fixed array. Empty for most players.
//
// Both tiers are persisted: cross-zone marshal (bag_marshal.cpp) carries
// the fixed `bags` array via BagAllData.items/capacities and the dynamic
// bags via BagAllData.dynamic_bags.
struct PlayerBagsComp
{
    // 按 BagType 装配各自的布局策略与起始容量。
    //
    // 拆分前四个背包全部是默认构造,也就是**清一色 kDefaultCapacity(10 格)**,
    // 跟上面那份容量表(100 / 200 / 10 / 200)对不上 —— 那份表一直只是注释,
    // 没有任何代码兑现过它。装备栏也因此跟人物背包一样会被"整理"重排。
    // 这里把注释兑现成代码,并让装备栏改用具名槽布局(不参与自动重排)。
    //
    // 这只是**起点**:容量随后仍可被 Bag::ExpandCapacity(玩法解锁)或
    // Bag::SetCapacityForRestore(快照还原)覆盖。
    //
    // 2026-09-01:改走 BagProfile —— 一个包的**一整套规则**在一处装配。
    // 此前这里只装配了布局一根轴,再加一根(准入 / 淘汰 / 过期)就要在这里
    // 多写一行、并且每处装配点都得记得同改。Profile 把"这个包是什么"收成
    // 一句话。见 docs/design/bag-rule-policy-layering.md §5.2。
    PlayerBagsComp()
    {
        bags[kInventory].SetProfile(BagProfile::Flat(kBagMaxCapacity));
        bags[kWarehouse].SetProfile(BagProfile::Flat(kWarehouseMaxCapacity));
        bags[kEquipment].SetProfile(BagProfile::Equipment(kEquipmentCapacity));
        // 临时格是**唯一**会挤掉存量物品的包(先进先出)。它是掉落溢出的缓冲,
        // 语义就是"新的进来、旧的顶出去",而不是"满了就捡不起来"。被挤掉的实例
        // 由 BagService 落 LogItemDestroy,绝不无声消失。
        bags[kTemporary].SetProfile(BagProfile::Temporary(kTempBagMaxCapacity));
    }

    std::array<Bag, kBagTypeCount> bags{};

    // key = event id / pet guid (whatever owns the transient bag).
    // 动态背包沿用 Bag 的默认布局(FlatLayout / kDefaultCapacity),
    // 容量由创建方或快照还原决定 —— 它们没有固定的类型语义。
    std::unordered_map<uint64_t, Bag> dynamicBags_;
};
