#pragma once

#include <array>
#include <cstdint>
#include <memory>
#include <unordered_map>
#include <vector>

#include "entt/src/entt/entity/registry.hpp"
#include "modules/bag/bag_system.h"
#include "proto/common/database/bag_quest_mail_data.pb.h"

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
    // 此前这里只装配了布局一根轴,再加一根(准入 / 过期)就要在这里
    // 多写一行、并且每处装配点都得记得同改。Profile 把"这个包是什么"收成
    // 一句话。见 docs/design/bag-rule-policy-layering.md §5.2。
    PlayerBagsComp()
    {
        bags[kInventory].SetProfile(BagProfile::Flat(kBagMaxCapacity));
        bags[kWarehouse].SetProfile(BagProfile::Flat(kWarehouseMaxCapacity));
        bags[kEquipment].SetProfile(BagProfile::Equipment(kEquipmentCapacity));
        // 临时格:主背包放不下的战斗掉落落在这里。满了**就拒**,不挤掉存量物品 ——
        // 2026-11-05 之前它是先进先出,被挤掉的东西没有去处,等于真丢(审计 cpp-economy#2)。
        // 现在新入账在任何副作用之前被拒、由来源保留(战斗结算整笔延后),
        // 玩家经 RetrieveTemporaryItems 取回人物背包腾出位置后自动补发。
        // 背包没有淘汰策略(2026-11-12 整条删除):任何包都不会自动销毁玩家的东西。
        bags[kTemporary].SetProfile(BagProfile::Flat(kTempBagMaxCapacity));
    }

    std::array<Bag, kBagTypeCount> bags{};

    // key = event id / pet guid (whatever owns the transient bag).
    // 动态背包沿用 Bag 的默认布局(FlatLayout / kDefaultCapacity),
    // 容量由创建方或快照还原决定 —— 它们没有固定的类型语义。
    std::unordered_map<uint64_t, Bag> dynamicBags_;

    // 本版本不认识的固定包类型的存档记录:BagAllData 里 bag_type ≥ kBagTypeCount 的条目,以及
    // capacities 下标 ≥ kBagTypeCount 的容量。新版本加了包类型、旧版本(或回滚后的版本)接手这名玩家时,
    // 这些记录原样携带、存盘原样写回,不丢(docs/design/mixed-version-persist-field-protection.md §12,第 5 批)。
    // 本版本的玩法看不到、也不碰它们:不计入任何包的数量与容量,不参与入包、整理。
    // 只由 bag_marshal::Unmarshal 整体重填(重新还原时先清空)、bag_marshal::Marshal 原样追加;全网同版本时两者皆空。
    // 存的是整份 ItemEntry(含其中实例的未知字段),拷贝不会失败,存盘不需要另一道失败分支。
    struct UnsupportedBagTypeRecords
    {
        std::vector<ItemEntry> items;     // 读入顺序
        std::vector<uint32_t> capacities; // BagAllData.capacities[kBagTypeCount..],下标顺序
    };
    UnsupportedBagTypeRecords unsupportedBagTypes;

    // 这名玩家名下是否已有这个实例身份:固定包、动态包,以及携带着的本版本不认识的包类型记录都算。
    // 跨包的 guid 唯一性检查(例如资产通道入账)必须用它,不能只扫运行时包 —— 携带记录虽然本版本的玩法不碰,
    // 存盘时仍原样写回;漏查会让同一实例在新包与旧包里各落一份,升回新版本后两份都还原(物品复制)。
    // 携带记录按 item_uuid 或整份实例的 item_id 任一命中算存在(Unmarshal 以 instance 为准,旧档只有 item_uuid)。
    [[nodiscard]] bool ContainsGuid(Guid guid) const
    {
        for (const auto &bag : bags)
        {
            if (bag.ContainsGuid(guid))
            {
                return true;
            }
        }
        for (const auto &dynamicBag : dynamicBags_)
        {
            if (dynamicBag.second.ContainsGuid(guid))
            {
                return true;
            }
        }
        for (const auto &entry : unsupportedBagTypes.items)
        {
            if (entry.item_uuid() == guid || (entry.has_instance() && entry.instance().item_id() == guid))
            {
                return true;
            }
        }
        return false;
    }
};
