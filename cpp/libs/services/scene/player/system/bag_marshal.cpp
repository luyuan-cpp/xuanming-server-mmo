#include "bag_marshal.h"
#include "engine/core/type_define/player_guid_comp.h"
#include "engine/core/utils/registry/actor_log.h"

#include <muduo/base/Logging.h>

#include <algorithm>
#include <array>
#include <vector>

#include "modules/bag/comp/player_bags_comp.h"
#include "modules/bag/bag_profile_registry.h"
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
// Schema: item_uuid / config_id / stack_size / pos / bag_type, plus
// acquire_seq (13) and, since 2026-10-07, equip (14) — the equipment
// instance data: random attribute rows (see
// docs/design/equipment-attributes.md §3.1) and, since the enhance /
// inlay phase, the enhance level and the inlaid gems
// (EquipInstanceData.enhance_level / .gems, see
// docs/design/equipment-enhance-inlay.md §3.1). Those two ride inside
// `equip`, so the conversion pair below carries them without naming
// them; ItemEntry's old TODO slots 6 (enchant_level) and 8 (gem_uuids)
// are retired and must not be reused. Remaining game-design extensions
// (bound state, trade count, expiry) are still TODO at known field
// numbers in proto/common/database/bag_quest_mail_data.proto; the
// migration path stays forward-compatible because proto field numbers
// are reserved.
//
// ItemComp(运行时实例)与 ItemEntry(存盘 / 跨服记录)之间的转换**只有下面这一对
// 函数**,固定包与动态包、Marshal 与 Unmarshal 共用。此前四处各抄一遍字段列表,
// 实例层每加一个字段就有四处要记得同改,漏一处就是"某一种包、某一个方向"静默丢数据。

namespace
{

// ItemComp -> ItemEntry。
//
// pos / bagType 不是实例的一部分(AGENTS §11.6:实例只回答「是什么」),所以由调用方
// 从布局层 / 所在的包另行给出;ItemEntry 是「实例 + 摆在哪」拼在一起的存盘记录。
// 动态包传 bagType = 0(那里由 bag_id 指认包,该字段无意义,等于 proto 默认值、不上线)。
void ItemToEntry(const ItemComp& item, uint32_t pos, uint32_t bagType, ItemEntry& entry)
{
    // ItemEntry 的字段号与 ItemComp 不同，不能合并顶层 UnknownFieldSet。
    // 完整实例是权威，旧字段仅由它计算兼容投影。
    entry.mutable_instance()->CopyFrom(item);
    entry.set_item_uuid(item.item_id());
    entry.set_config_id(item.config_id());
    entry.set_stack_size(item.size());
    entry.set_pos(pos);
    entry.set_bag_type(bagType);
    // 入包序号:淘汰策略按它排先后。不带走的话跨服一跳顺序就丢了,
    // 而 item_uuid 顶替不了它(跨服的号是源服铸的)。
    entry.set_acquire_seq(item.acquire_seq());
    // 装备实例数据:**presence 有语义**。has_equip() == true 表示这件装备已经掷过属性
    // (哪怕一条都没掷出);无条件 mutable_equip() 会让每件普通物品读回来都"带一段空的
    // equip",反过来漏拷则让装备读回来像没初始化过、下次入包被重掷。
    // 强化等级与已镶宝石也在这一段里,随这一次整份 CopyFrom 走:不要改成逐字段拷,
    // 那样 EquipInstanceData 每加一个字段,这里就多一处「忘了拷 = 存档往返丢数据」。
    if (item.has_equip())
    {
        entry.mutable_equip()->CopyFrom(item.equip());
    }
}

// ItemEntry -> ItemComp。pos / bag_type 不进实例,由调用方单独读 entry.pos()。
ItemComp ItemFromEntry(const ItemEntry& entry)
{
    if (entry.has_instance()) return entry.instance();
    // 无完整实例的旧档，继续按既有字段还原；正常保存时补齐 instance。
    ItemComp item;
    item.set_item_id(entry.item_uuid());
    item.set_config_id(entry.config_id());
    item.set_size(entry.stack_size());
    // 零章旧档在全包最大序号之后按出现顺序补盖；非零章原样保留。
    item.set_acquire_seq(entry.acquire_seq());
    if (entry.has_equip())  // presence 原样带回,理由同 ItemToEntry
    {
        item.mutable_equip()->CopyFrom(entry.equip());
    }
    return item;
}

// 写盘顺序属于布局：固定包按 bag_type，包内按槽位，动态包另按 bag_id 排序。
void MarshalItems(const Bag &bag, uint32_t bagType, google::protobuf::RepeatedPtrField<ItemEntry> &out)
{
    std::vector<std::pair<uint32_t, const ItemComp *>> ordered;
    bag.ForEachItem([&](Guid guid, const ItemComp &item)
    {
        if (item.size() > 0)
        {
            ordered.emplace_back(bag.GetItemPosByGuid(guid), &item);
        }
    });
    std::sort(ordered.begin(), ordered.end(), [](const auto &left, const auto &right)
    {
        return left.first != right.first ? left.first < right.first : left.second->item_id() < right.second->item_id();
    });
    for (const auto &[slot, item] : ordered)
    {
        ItemToEntry(*item, slot, bagType, *out.Add());
    }
}

}  // namespace

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
    // kTemporary: the first kBagTypeCount entries are always emitted so the
    // consumer can index bag_type → capacity directly without a sparse map.
    // 之后是本版本不认识的包类型(新版本加的包)的容量,原样携带,下标仍与 bag_type 一一对应
    // (混版本设计 §12,第 5 批)。所以数组长度可以大于 kBagTypeCount,读写两侧都不得截断或拒绝。
    const auto& unsupported = bags->unsupportedBagTypes;
    out.mutable_capacities()->Reserve(static_cast<int>(kBagTypeCount + unsupported.capacities.size()));
    for (uint32_t bagType = 0; bagType < static_cast<uint32_t>(kBagTypeCount); ++bagType)
    {
        out.add_capacities(static_cast<uint32_t>(bags->bags[bagType].Capacity()));
    }
    // 本版本不认识的包类型的容量(新版本加的包)原样接在后面:下标仍与 bag_type 一一对应
    // (混版本设计 §12,第 5 批;见 PlayerBagsComp::unsupportedBagTypes)。
    for (const auto capacity : unsupported.capacities)
    {
        out.add_capacities(capacity);
    }

    // Items across all four bags. ItemEntry.bag_type tags which bag
    // each entry belongs to, so the wire shape is one flat list and
    // the consumer can group on read.
    for (uint32_t bagType = 0; bagType < static_cast<uint32_t>(kBagTypeCount); ++bagType)
    {
        const auto& bag = bags->bags[bagType];
        MarshalItems(bag, bagType, *out.mutable_items());
    }
    // 本版本不认识的包类型的条目整份原样追加(含其中实例的未知字段)。它们的 bag_type 都 ≥ kBagTypeCount,
    // 接在已知包之后仍保持「按 bag_type 升序」的写盘顺序;同一份输入每次写出的字节相同,脏比较不会因此抖动。
    for (const auto& entry : unsupported.items)
    {
        *out.add_items() = entry;
    }

    // Transient runtime bags (event bags / per-pet bags). Each gets its
    // own DynamicBagData carrying bag_id + capacity + items. Empty for
    // most players, so this loop usually does nothing.
    std::vector<uint64_t> dynamicIds;
    dynamicIds.reserve(bags->dynamicBags_.size());
    for (const auto &entry : bags->dynamicBags_)
    {
        dynamicIds.push_back(entry.first);
    }
    std::sort(dynamicIds.begin(), dynamicIds.end());
    for (const auto bagId : dynamicIds)
    {
        const auto &bag = bags->dynamicBags_.at(bagId);
        BagAllData::DynamicBagData* dyn = out.add_dynamic_bags();
        dyn->set_bag_id(bagId);
        dyn->set_capacity(static_cast<uint32_t>(bag.Capacity()));
        // **规则本身不进快照,进快照的只是"这个包是哪套规则"这一个 id。**
        // 不带它,节日包跨服回来会退化成默认的自由格 + 什么都收 —— 规则静默消失。
        dyn->set_profile_id(bag.ProfileId());
        MarshalItems(bag, 0, *dyn->mutable_items());
    }
}

void Unmarshal(entt::entity player, const BagAllData& in)
{
    auto& bags = tlsEcs.actorRegistry.get_or_emplace<PlayerBagsComp>(player);

    // 把玩家 guid 记进每个 Bag —— **只用于日志**,不参与任何判定。
    // Bag::playerGuid 此前全仓没有任何写入点,于是每一条带 player= 的错误日志
    // 打的都是 kInvalidGuid 哨兵,排障时等于没有。这里取的是挂在实体上的 Guid
    // 组件,与 TransactionLogSystem::ResolvePlayerId 同源。
    const auto* ownerGuid = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(player);
    const Guid owner = (ownerGuid != nullptr) ? ownerGuid->value : kInvalidGuid;

    // Capacity replay first — InsertItemForRestore validates against
    // capacity_ via the bag's own bookkeeping, so getting capacities
    // straight before items go in matters.
    //
    // Marshal 至少写出 kBagTypeCount 个容量;超出的部分是本版本不认识的包类型的容量,
    // 不得截断或拒绝,下面进 unsupportedBagTypes 原样携带(混版本设计 §12,第 5 批)。
    // If we receive a shorter array (older client / corrupt payload), the
    // missing bags fall through to default capacity_ from the Bag default ctor.
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

    // 本版本不认识的包类型(新版本加的包,或回滚前的版本留下的):整体重填,重新还原(回档到已有实体)时不留旧的。
    // 以前这里直接丢弃越界条目 —— 新版本加了包类型后回滚 scene,旧版本一存盘,新包里的物品就全没了
    // (混版本设计 §12、§8.3「scene 有条件回滚」的前提,第 5 批)。现在原样携带,存盘时由 Marshal 原样写回。
    auto& unsupported = bags.unsupportedBagTypes;
    unsupported = {};
    for (int index = static_cast<int>(kBagTypeCount); index < caps; ++index)
    {
        unsupported.capacities.push_back(in.capacities(index));
    }

    // Items replay. bag_type 越界的条目(新版本的包类型,或数据损坏)不进任何运行时包,整份原样携带。
    std::array<std::vector<std::pair<ItemComp, uint32_t>>, kBagTypeCount> perBag;
    uint32_t maxUnsupportedBagType = 0;
    for (const auto& entry : in.items())
    {
        const uint32_t bagType = entry.bag_type();
        if (bagType >= static_cast<uint32_t>(kBagTypeCount))
        {
            unsupported.items.push_back(entry);
            maxUnsupportedBagType = std::max(maxUnsupportedBagType, bagType);
            continue;
        }
        perBag[bagType].emplace_back(ItemFromEntry(entry), entry.pos());
    }
    if (!unsupported.items.empty() || !unsupported.capacities.empty())
    {
        // 每次还原只打一行汇总:混版本窗口里这是正常现象,逐条打会刷屏;数据损坏时同一行也足够定位。
        LOG_WARN << "[bag_marshal] carrying records of bag types unknown to this version verbatim: entries="
                 << unsupported.items.size() << " extra_capacities=" << unsupported.capacities.size()
                 << " max_bag_type=" << maxUnsupportedBagType
                 << " (kBagTypeCount=" << static_cast<uint32_t>(kBagTypeCount) << ") " << ActorLog(player);
    }
    for (uint32_t bagType = 0; bagType < static_cast<uint32_t>(kBagTypeCount); ++bagType)
    {
        bags.bags[bagType].RestoreItems(std::move(perBag[bagType]));
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
        // 先按 profile_id 装回规则,**再**放物品 —— SetProfile 只接受空包
        // (槽位号在不同布局下含义不同),顺序反了规则就装不上。
        // 注册表查不到时 fail-open 成自由格但保留 id:活动下线也绝不丢东西,
        // 活动重新上线后这个包会自己变回去。见 bag_profile_registry.h。
        bag.SetProfile(BagProfileRegistry::Instance().Make(
            dyn.profile_id(), static_cast<std::size_t>(dyn.capacity())));
        bag.SetCapacityForRestore(static_cast<std::size_t>(dyn.capacity()));
        std::vector<std::pair<ItemComp, uint32_t>> items;
        items.reserve(dyn.items_size());
        for (const auto &entry : dyn.items())
        {
            items.emplace_back(ItemFromEntry(entry), entry.pos());
        }
        bag.RestoreItems(std::move(items));
    }
}

}  // namespace bag_marshal
