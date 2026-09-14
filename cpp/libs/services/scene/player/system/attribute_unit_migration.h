#pragma once

// 存档数值单位迁移(纯规则,零 ECS / 零表依赖,可单测),设计文档 docs/design/player-attribute-allocation.md §5 / §8。
//
// 为什么需要:2026-09-14 把法力单位整体 ×4(让每分配 1 点灵力面板至少 +1 法力)。法力上限是现算的,改表即生效;
// 但**当前法力**是绝对值,会出现在两处并被原样写回:
//   1) 存档(BaseAttributesComp.mana、PetInstance.mana):登录时只会被夹到新上限、不会跟着 ×4,全仓没有自然回蓝,
//      满蓝老号会长期停在 1/4。所以存档里记单位版本号(PlayerAttributeComp.attribute_unit_version),
//      加载时发现是旧版本就换算再盖戳。版本号与法力在同一个 player_database 行里一起落库,不会"乘了没盖戳"。
//   2) 战斗结算(BattleSettlementData.mana / pets[].mana):离线挂起结算在登录迁移**之后**才应用,
//      旧 battle 二进制产出的结算不换算就会把刚迁移好的法力写回旧值,且版本号已是新的、再也补不回来。
//      所以结算也带单位版本(BattleSettlementData.attribute_unit_version),应用前用 ManaToCurrentUnit 换算。
//
// 护甲(同日 ×12)不走这里:它没有运行时写入方,PlayerAttributeSystem::Recalculate 每次按职业表重写,老号登录即纠正;
// 结算里也不带护甲。速度(同日 ×12)同理,由 Recalculate 直写。
//
// 调用约定:MigrateLoadedAttributeUnits 只在 PlayerDatabaseMessageFieldsUnmarshal 里、
// PlayerAttributeSystem / PetSystem 的 InitializeOnLoad 之前调用,随后的 Recalculate 会把换算后的值夹进新上限;
// ManaToCurrentUnit 在 PlayerBattleSystem::ApplySettlementToEntity 应用结算法力(含宝宝)前调用。
// 以后再改单位:turnbattle::kAttributeUnitVersion 加 1,并在 ManaToCurrentUnit 里按版本号追加一段,不要改已有的段。
// 已知不覆盖:退回旧二进制期间登录的号法力会被旧二进制夹回旧上限,再升级时版本号已是新的、不再换算(§8 回滚须知)。
// 单测:cpp/tests/turn_battle_engine_test/attribute_unit_migration_test.cpp;
//       结算接线 cpp/tests/bag_test/player_battle_settlement_test.cpp。

#include <cstdint>
#include <limits>

#include "services/battle/constants/turn_battle_constants.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/player_attribute_comp.pb.h"
#include "proto/common/component/player_pet_comp.pb.h"

namespace attributeunit {

inline constexpr uint32_t kCurrentVersion = turnbattle::kAttributeUnitVersion;
// 版本 0 → 1:法力 ×4
inline constexpr uint64_t kManaScaleV1 = 4;

// 饱和乘法:坏存档 / 坏结算里的超大值不能乘溢出成一个小数
inline uint64_t SaturatingScale(uint64_t value, uint64_t factor) {
    if (factor != 0 && value > std::numeric_limits<uint64_t>::max() / factor) {
        return std::numeric_limits<uint64_t>::max();
    }
    return value * factor;
}

// 把 fromVersion 单位下的法力绝对值换算到当前单位;fromVersion 已是当前或更新版本时原样返回
inline uint64_t ManaToCurrentUnit(uint64_t mana, uint32_t fromVersion) {
    if (fromVersion < 1) {
        mana = SaturatingScale(mana, kManaScaleV1);
    }
    return mana;
}

// scalePlayerMana:角色本人的当前法力要不要换算。新号 / 阵亡复活的号加载后会被顶满到新上限,不必换算;
// 宝宝的当前法力与主人死活无关,旧存档一律换算。
// 返回是否做了迁移(调用方据此打日志);已是当前版本或更新版本时什么都不动。
inline bool MigrateLoadedAttributeUnits(BaseAttributesComp& base, PlayerAttributeComp& attributeComp,
                                        PlayerPetComp& petComp, bool scalePlayerMana) {
    const uint32_t version = attributeComp.attribute_unit_version();
    if (version >= kCurrentVersion) {
        return false;
    }
    if (scalePlayerMana) {
        base.set_mana(ManaToCurrentUnit(base.mana(), version));
    }
    for (auto& pet : *petComp.mutable_pets()) {
        pet.set_mana(ManaToCurrentUnit(pet.mana(), version));
    }
    attributeComp.set_attribute_unit_version(kCurrentVersion);
    return true;
}

}  // namespace attributeunit
