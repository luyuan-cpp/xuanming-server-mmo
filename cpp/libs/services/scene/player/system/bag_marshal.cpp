#include "bag_marshal.h"

#include <muduo/base/Logging.h>

#include "modules/bag/comp/player_bags_comp.h"
#include "modules/bag/bag_system.h"
#include "thread_context/ecs_context.h"

// bag_marshal — bridge between BagAllData proto and PlayerBagsComp on a
// player entity. Read the header for the full design rationale.
//
// Real implementation as of 2026-05-17:
//   • Marshal walks all four Bag instances in PlayerBagsComp, emitting
//     one ItemEntry per (guid, configId, stackSize, pos, bagType) and
//     one capacity entry per bag. It then walks dynamicBags_, emitting
//     one DynamicBagData (bag_id + capacity + items) per transient bag.
//   • Unmarshal reverses: for each fixed bag clear the contents, set
//     capacity from the snapshot, then insert items via
//     Bag::InsertItemForRestore so the same guid+pos layout the player
//     saw in the source zone is preserved on the destination. It then
//     rebuilds dynamicBags_ from the DynamicBagData list.
//
// Schema is the conservative 5-field subset (item_uuid / config_id /
// stack_size / pos / bag_type). Game-design extensions (enchant level,
// affixes, gem inlay, bound state) are documented as TODO at known
// field numbers in proto/common/database/bag_quest_mail_data.proto and
// will land here as the schema grows; the migration path stays
// forward-compatible because proto field numbers are reserved.

namespace bag_marshal
{

void Marshal(entt::entity player, BagAllData& out)
{
    out.Clear();

    const auto* bags = tlsEcs.actorRegistry.try_get<PlayerBagsComp>(player);
    if (bags == nullptr)
    {
        // No bags attached — caller is e.g. a freshly-created player
        // entity that hasn't been through PlayerLifecycleSystem yet.
        // Empty BagAllData is the correct payload.
        return;
    }

    // Capacity slot per BagType, in order kInventory/kWarehouse/kEquipment/
    // kTemporary. We always emit kBagTypeCount entries so the consumer can
    // index bag_type → capacity directly without a sparse map.
    out.mutable_capacities()->Reserve(static_cast<int>(kBagTypeCount));
    for (uint32_t bagType = 0; bagType < static_cast<uint32_t>(kBagTypeCount); ++bagType)
    {
        out.add_capacities(static_cast<uint32_t>(bags->bags[bagType].Capacity()));
    }

    // Items across all four bags. ItemEntry.bag_type tags which bag
    // each entry belongs to, so the wire shape is one flat list and
    // the consumer can group on read.
    for (uint32_t bagType = 0; bagType < static_cast<uint32_t>(kBagTypeCount); ++bagType)
    {
        const auto& bag = bags->bags[bagType];
        bag.ForEachItem([&out, bagType, &bag](Guid guid, const ItemComp& item) {
            ItemEntry* entry = out.add_items();
            entry->set_item_uuid(static_cast<uint64_t>(guid));
            entry->set_config_id(item.config_id());
            entry->set_stack_size(item.size());
            entry->set_pos(bag.GetItemPosByGuid(guid));
            entry->set_bag_type(bagType);
        });
    }

    // Transient runtime bags (event bags / per-pet bags). Each gets its
    // own DynamicBagData carrying bag_id + capacity + items. Empty for
    // most players, so this loop usually does nothing.
    for (const auto& [bagId, bag] : bags->dynamicBags_)
    {
        BagAllData::DynamicBagData* dyn = out.add_dynamic_bags();
        dyn->set_bag_id(bagId);
        dyn->set_capacity(static_cast<uint32_t>(bag.Capacity()));
        bag.ForEachItem([dyn, &bag](Guid guid, const ItemComp& item) {
            ItemEntry* entry = dyn->add_items();
            entry->set_item_uuid(static_cast<uint64_t>(guid));
            entry->set_config_id(item.config_id());
            entry->set_stack_size(item.size());
            entry->set_pos(bag.GetItemPosByGuid(guid));
            // bag_type is meaningless for dynamic bags (identified by
            // bag_id); leave it at the proto default 0.
        });
    }
}

void Unmarshal(entt::entity player, const BagAllData& in)
{
    auto& bags = tlsEcs.actorRegistry.get_or_emplace<PlayerBagsComp>(player);

    // 把玩家 guid 记进每个 Bag —— **只用于日志**,不参与任何判定。
    // Bag::playerGuid 此前全仓没有任何写入点,于是每一条带 player= 的错误日志
    // 打的都是 kInvalidGuid 哨兵,排障时等于没有。这里取的是挂在实体上的 Guid
    // 组件,与 TransactionLogSystem::ResolvePlayerId 同源。
    const auto* ownerGuid = tlsEcs.actorRegistry.try_get<Guid>(player);
    const Guid owner = (ownerGuid != nullptr) ? *ownerGuid : kInvalidGuid;

    // Capacity replay first — InsertItemForRestore validates against
    // capacity_ via the bag's own bookkeeping, so getting capacities
    // straight before items go in matters.
    //
    // The capacities array on the wire SHOULD have exactly kBagTypeCount
    // entries (Marshal always emits that many). If we receive a
    // shorter array (older client / corrupt payload), the missing
    // bags fall through to default capacity_ from the Bag default ctor.
    const int caps = in.capacities_size();
    for (uint32_t bagType = 0; bagType < static_cast<uint32_t>(kBagTypeCount); ++bagType)
    {
        bags.bags[bagType].SetPlayerGuid(owner);
        bags.bags[bagType].ResetFromSnapshot();
        if (static_cast<int>(bagType) < caps)
        {
            bags.bags[bagType].SetCapacityForRestore(
                static_cast<std::size_t>(in.capacities(static_cast<int>(bagType))));
        }
    }

    // Items replay. Out-of-range bag_type values (corruption / future
    // bag types we don't support) are dropped with a WARN — better
    // than crashing, and the player just loses those items.
    for (const auto& entry : in.items())
    {
        const uint32_t bagType = entry.bag_type();
        if (bagType >= static_cast<uint32_t>(kBagTypeCount))
        {
            LOG_WARN << "[bag_marshal] dropping ItemEntry with bag_type="
                     << bagType << " (out of range, kBagTypeCount="
                     << static_cast<uint32_t>(kBagTypeCount) << ") "
                     << "item_uuid=" << entry.item_uuid()
                     << " config_id=" << entry.config_id()
                     << " player=" << entt::to_integral(player);
            continue;
        }
        bags.bags[bagType].InsertItemForRestore(
            static_cast<Guid>(entry.item_uuid()),
            entry.config_id(),
            entry.stack_size(),
            entry.pos());
    }

    // Transient runtime bags. Rebuild dynamicBags_ from scratch so a
    // re-Unmarshal (e.g. rollback into an already-populated entity)
    // doesn't leave stale bags behind.
    bags.dynamicBags_.clear();
    for (const auto& dyn : in.dynamic_bags())
    {
        Bag& bag = bags.dynamicBags_[dyn.bag_id()];
        bag.SetPlayerGuid(owner);
        bag.ResetFromSnapshot();
        bag.SetCapacityForRestore(static_cast<std::size_t>(dyn.capacity()));
        for (const auto& entry : dyn.items())
        {
            bag.InsertItemForRestore(
                static_cast<Guid>(entry.item_uuid()),
                entry.config_id(),
                entry.stack_size(),
                entry.pos());
        }
    }
}

}  // namespace bag_marshal
