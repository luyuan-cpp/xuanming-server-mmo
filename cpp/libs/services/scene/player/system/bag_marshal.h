#pragma once

// BagMarshal — bridge between BagAllData proto (cross-zone payload /
// persistence blob) and the player's runtime bag state.
//
// Status (2026-08-27):
//   FULLY IMPLEMENTED. Both directions walk PlayerBagsComp's four fixed
//   bags plus dynamicBags_ and map them to/from BagAllData — see the
//   comment block at the top of bag_marshal.cpp for the exact shape.
//
//   (This block used to read "CURRENT BEHAVIOR (2026-05-16): no-op",
//   which stopped being true when the real implementation landed on
//   2026-05-17. It stayed stale for three months and is exactly the kind
//   of comment that sends someone down the wrong path during an incident,
//   so: if you change these functions, change this paragraph too.)
//
//   Schema is still the conservative 5-field subset (item_uuid /
//   config_id / stack_size / pos / bag_type). Game-design extensions
//   (enchant level, affixes, gem inlay, bound state) are documented as
//   TODO at known field numbers in
//   proto/common/database/bag_quest_mail_data.proto and will land here as
//   the schema grows; proto field numbers are reserved so the migration
//   path stays forward-compatible.
//
//   ItemEntry.pos is a Bag SlotId. Its meaning is layout-strategy
//   dependent (flat = index, grid = y*width+x) but its wire type never
//   changes — see docs/design/bag-instance-layout-split.md §7.

#include "entt/src/entt/entity/registry.hpp"
#include "proto/common/database/bag_quest_mail_data.pb.h"

namespace bag_marshal
{
    // Read all bag/warehouse/equipment/temp items off the player's ECS
    // representation, write them to `out`. Idempotent — calling twice
    // produces equivalent BagAllData (modulo serialization order, which
    // proto3 doesn't guarantee anyway).
    //
    // A player entity with no PlayerBagsComp yields an empty BagAllData
    // (that is the correct payload for an entity that hasn't been through
    // the lifecycle yet), not an error.
    void Marshal(entt::entity player, BagAllData& out);

    // Reverse direction: rebuild bag/warehouse/equipment/temp ECS state
    // from the BagAllData payload. Called on cross-zone destination
    // when receiving a migrating player, on player login when loading
    // from Redis, and on rollback when restoring a snapshot.
    //
    // get_or_emplace's the component, so it works on a fresh entity.
    // Entries whose bag_type is out of range are dropped with a WARN.
    // An entry whose recorded pos is unusable (out of range, or already
    // taken by another restored item) is relocated to a free slot rather
    // than dropped — position can change, the item must not vanish.
    // See Bag::InsertItemForRestore.
    void Unmarshal(entt::entity player, const BagAllData& in);
}
