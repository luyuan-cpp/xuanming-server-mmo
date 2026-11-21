
#include "table_loader.h"

#include <algorithm>
#include <array>
#include <atomic>
#include <cstddef>
#include <thread>
#include <utility>
#include <vector>

#include "activityschedule_table.h"
#include "activityvitalityreward_table.h"
#include "activityvitalityrule_table.h"
#include "actoractioncombatstate_table.h"
#include "actoractionstate_table.h"
#include "afkpassrule_table.h"
#include "attributeallocratio_table.h"
#include "attributeautoplan_table.h"
#include "attributedimension_table.h"
#include "attributepool_table.h"
#include "attributerule_table.h"
#include "basescene_table.h"
#include "buff_table.h"
#include "class_table.h"
#include "condition_table.h"
#include "cooldown_table.h"
#include "dungeon_table.h"
#include "equipaffixpool_table.h"
#include "equipaffixrule_table.h"
#include "equipartifact_table.h"
#include "equipattribute_table.h"
#include "equipattributecap_table.h"
#include "equipenhance_table.h"
#include "equipgem_table.h"
#include "equipgemsocket_table.h"
#include "equipslot_table.h"
#include "equipv1affix_table.h"
#include "globalvariable_table.h"
#include "guildactivity_table.h"
#include "guilddonate_table.h"
#include "guildlevel_table.h"
#include "guildrule_table.h"
#include "guildshop_table.h"
#include "item_table.h"
#include "messagelimiter_table.h"
#include "mirror_table.h"
#include "mission_table.h"
#include "monster_table.h"
#include "mysterymerchantgoods_table.h"
#include "mysterymerchantrule_table.h"
#include "mysterymerchantspot_table.h"
#include "pet_table.h"
#include "petentry_table.h"
#include "petrule_table.h"
#include "pettier_table.h"
#include "playmode_table.h"
#include "reward_table.h"
#include "rolenamerule_table.h"
#include "skill_table.h"
#include "skillpermission_table.h"
#include "test_table.h"
#include "testmultikey_table.h"
#include "world_table.h"
#include "zonegroup_table.h"


TableSet::TableSet() = default;
TableSet::~TableSet() = default;
TableSet::TableSet(const TableSet&) = default;
TableSet& TableSet::operator=(const TableSet&) = default;
TableSet::TableSet(TableSet&&) noexcept = default;
TableSet& TableSet::operator=(TableSet&&) noexcept = default;

LoadSuccessCallback loadSuccessCallback;

namespace {

constexpr std::size_t kTableCount = 54;

constexpr std::array<std::string_view, 54> kTableNames = {
    "ActivitySchedule",
    "ActivityVitalityReward",
    "ActivityVitalityRule",
    "ActorActionCombatState",
    "ActorActionState",
    "AfkPassRule",
    "AttributeAllocRatio",
    "AttributeAutoPlan",
    "AttributeDimension",
    "AttributePool",
    "AttributeRule",
    "BaseScene",
    "Buff",
    "Class",
    "Condition",
    "Cooldown",
    "Dungeon",
    "EquipAffixPool",
    "EquipAffixRule",
    "EquipArtifact",
    "EquipAttribute",
    "EquipAttributeCap",
    "EquipEnhance",
    "EquipGem",
    "EquipGemSocket",
    "EquipSlot",
    "EquipV1Affix",
    "GlobalVariable",
    "GuildActivity",
    "GuildDonate",
    "GuildLevel",
    "GuildRule",
    "GuildShop",
    "Item",
    "MessageLimiter",
    "Mirror",
    "Mission",
    "Monster",
    "MysteryMerchantGoods",
    "MysteryMerchantRule",
    "MysteryMerchantSpot",
    "Pet",
    "PetEntry",
    "PetRule",
    "PetTier",
    "PlayMode",
    "Reward",
    "RoleNameRule",
    "Skill",
    "SkillPermission",
    "Test",
    "TestMultiKey",
    "World",
    "ZoneGroup",
};


constexpr std::array<std::string_view, 13> kHotReloadableTables = {
    "ActivitySchedule",
    "ActivityVitalityReward",
    "ActivityVitalityRule",
    "Buff",
    "Cooldown",
    "GuildDonate",
    "GuildShop",
    "MessageLimiter",
    "Monster",
    "MysteryMerchantGoods",
    "MysteryMerchantRule",
    "Skill",
    "SkillPermission",
};


uint64_t tableLoadVersion = 0;
std::shared_ptr<const TableSet> gCurrentTableSet;

std::shared_ptr<TableSet> CaptureManagersAsTableSet(const TableBundleIdentity& identity) {
    auto set = std::make_shared<TableSet>();
    set->identity = identity;

    set->activityScheduleTable = ActivityScheduleTableManager::Instance().CurrentSnapshot();

    set->activityVitalityRewardTable = ActivityVitalityRewardTableManager::Instance().CurrentSnapshot();

    set->activityVitalityRuleTable = ActivityVitalityRuleTableManager::Instance().CurrentSnapshot();

    set->actorActionCombatStateTable = ActorActionCombatStateTableManager::Instance().CurrentSnapshot();

    set->actorActionStateTable = ActorActionStateTableManager::Instance().CurrentSnapshot();

    set->afkPassRuleTable = AfkPassRuleTableManager::Instance().CurrentSnapshot();

    set->attributeAllocRatioTable = AttributeAllocRatioTableManager::Instance().CurrentSnapshot();

    set->attributeAutoPlanTable = AttributeAutoPlanTableManager::Instance().CurrentSnapshot();

    set->attributeDimensionTable = AttributeDimensionTableManager::Instance().CurrentSnapshot();

    set->attributePoolTable = AttributePoolTableManager::Instance().CurrentSnapshot();

    set->attributeRuleTable = AttributeRuleTableManager::Instance().CurrentSnapshot();

    set->baseSceneTable = BaseSceneTableManager::Instance().CurrentSnapshot();

    set->buffTable = BuffTableManager::Instance().CurrentSnapshot();

    set->classTable = ClassTableManager::Instance().CurrentSnapshot();

    set->conditionTable = ConditionTableManager::Instance().CurrentSnapshot();

    set->cooldownTable = CooldownTableManager::Instance().CurrentSnapshot();

    set->dungeonTable = DungeonTableManager::Instance().CurrentSnapshot();

    set->equipAffixPoolTable = EquipAffixPoolTableManager::Instance().CurrentSnapshot();

    set->equipAffixRuleTable = EquipAffixRuleTableManager::Instance().CurrentSnapshot();

    set->equipArtifactTable = EquipArtifactTableManager::Instance().CurrentSnapshot();

    set->equipAttributeTable = EquipAttributeTableManager::Instance().CurrentSnapshot();

    set->equipAttributeCapTable = EquipAttributeCapTableManager::Instance().CurrentSnapshot();

    set->equipEnhanceTable = EquipEnhanceTableManager::Instance().CurrentSnapshot();

    set->equipGemTable = EquipGemTableManager::Instance().CurrentSnapshot();

    set->equipGemSocketTable = EquipGemSocketTableManager::Instance().CurrentSnapshot();

    set->equipSlotTable = EquipSlotTableManager::Instance().CurrentSnapshot();

    set->equipV1AffixTable = EquipV1AffixTableManager::Instance().CurrentSnapshot();

    set->globalVariableTable = GlobalVariableTableManager::Instance().CurrentSnapshot();

    set->guildActivityTable = GuildActivityTableManager::Instance().CurrentSnapshot();

    set->guildDonateTable = GuildDonateTableManager::Instance().CurrentSnapshot();

    set->guildLevelTable = GuildLevelTableManager::Instance().CurrentSnapshot();

    set->guildRuleTable = GuildRuleTableManager::Instance().CurrentSnapshot();

    set->guildShopTable = GuildShopTableManager::Instance().CurrentSnapshot();

    set->itemTable = ItemTableManager::Instance().CurrentSnapshot();

    set->messageLimiterTable = MessageLimiterTableManager::Instance().CurrentSnapshot();

    set->mirrorTable = MirrorTableManager::Instance().CurrentSnapshot();

    set->missionTable = MissionTableManager::Instance().CurrentSnapshot();

    set->monsterTable = MonsterTableManager::Instance().CurrentSnapshot();

    set->mysteryMerchantGoodsTable = MysteryMerchantGoodsTableManager::Instance().CurrentSnapshot();

    set->mysteryMerchantRuleTable = MysteryMerchantRuleTableManager::Instance().CurrentSnapshot();

    set->mysteryMerchantSpotTable = MysteryMerchantSpotTableManager::Instance().CurrentSnapshot();

    set->petTable = PetTableManager::Instance().CurrentSnapshot();

    set->petEntryTable = PetEntryTableManager::Instance().CurrentSnapshot();

    set->petRuleTable = PetRuleTableManager::Instance().CurrentSnapshot();

    set->petTierTable = PetTierTableManager::Instance().CurrentSnapshot();

    set->playModeTable = PlayModeTableManager::Instance().CurrentSnapshot();

    set->rewardTable = RewardTableManager::Instance().CurrentSnapshot();

    set->roleNameRuleTable = RoleNameRuleTableManager::Instance().CurrentSnapshot();

    set->skillTable = SkillTableManager::Instance().CurrentSnapshot();

    set->skillPermissionTable = SkillPermissionTableManager::Instance().CurrentSnapshot();

    set->testTable = TestTableManager::Instance().CurrentSnapshot();

    set->testMultiKeyTable = TestMultiKeyTableManager::Instance().CurrentSnapshot();

    set->worldTable = WorldTableManager::Instance().CurrentSnapshot();

    set->zoneGroupTable = ZoneGroupTableManager::Instance().CurrentSnapshot();

    return set;
}

void InstallTableSetIntoManagers(const TableSet& set) {

    ActivityScheduleTableManager::Instance().InstallSnapshot(set.activityScheduleTable);

    ActivityVitalityRewardTableManager::Instance().InstallSnapshot(set.activityVitalityRewardTable);

    ActivityVitalityRuleTableManager::Instance().InstallSnapshot(set.activityVitalityRuleTable);

    ActorActionCombatStateTableManager::Instance().InstallSnapshot(set.actorActionCombatStateTable);

    ActorActionStateTableManager::Instance().InstallSnapshot(set.actorActionStateTable);

    AfkPassRuleTableManager::Instance().InstallSnapshot(set.afkPassRuleTable);

    AttributeAllocRatioTableManager::Instance().InstallSnapshot(set.attributeAllocRatioTable);

    AttributeAutoPlanTableManager::Instance().InstallSnapshot(set.attributeAutoPlanTable);

    AttributeDimensionTableManager::Instance().InstallSnapshot(set.attributeDimensionTable);

    AttributePoolTableManager::Instance().InstallSnapshot(set.attributePoolTable);

    AttributeRuleTableManager::Instance().InstallSnapshot(set.attributeRuleTable);

    BaseSceneTableManager::Instance().InstallSnapshot(set.baseSceneTable);

    BuffTableManager::Instance().InstallSnapshot(set.buffTable);

    ClassTableManager::Instance().InstallSnapshot(set.classTable);

    ConditionTableManager::Instance().InstallSnapshot(set.conditionTable);

    CooldownTableManager::Instance().InstallSnapshot(set.cooldownTable);

    DungeonTableManager::Instance().InstallSnapshot(set.dungeonTable);

    EquipAffixPoolTableManager::Instance().InstallSnapshot(set.equipAffixPoolTable);

    EquipAffixRuleTableManager::Instance().InstallSnapshot(set.equipAffixRuleTable);

    EquipArtifactTableManager::Instance().InstallSnapshot(set.equipArtifactTable);

    EquipAttributeTableManager::Instance().InstallSnapshot(set.equipAttributeTable);

    EquipAttributeCapTableManager::Instance().InstallSnapshot(set.equipAttributeCapTable);

    EquipEnhanceTableManager::Instance().InstallSnapshot(set.equipEnhanceTable);

    EquipGemTableManager::Instance().InstallSnapshot(set.equipGemTable);

    EquipGemSocketTableManager::Instance().InstallSnapshot(set.equipGemSocketTable);

    EquipSlotTableManager::Instance().InstallSnapshot(set.equipSlotTable);

    EquipV1AffixTableManager::Instance().InstallSnapshot(set.equipV1AffixTable);

    GlobalVariableTableManager::Instance().InstallSnapshot(set.globalVariableTable);

    GuildActivityTableManager::Instance().InstallSnapshot(set.guildActivityTable);

    GuildDonateTableManager::Instance().InstallSnapshot(set.guildDonateTable);

    GuildLevelTableManager::Instance().InstallSnapshot(set.guildLevelTable);

    GuildRuleTableManager::Instance().InstallSnapshot(set.guildRuleTable);

    GuildShopTableManager::Instance().InstallSnapshot(set.guildShopTable);

    ItemTableManager::Instance().InstallSnapshot(set.itemTable);

    MessageLimiterTableManager::Instance().InstallSnapshot(set.messageLimiterTable);

    MirrorTableManager::Instance().InstallSnapshot(set.mirrorTable);

    MissionTableManager::Instance().InstallSnapshot(set.missionTable);

    MonsterTableManager::Instance().InstallSnapshot(set.monsterTable);

    MysteryMerchantGoodsTableManager::Instance().InstallSnapshot(set.mysteryMerchantGoodsTable);

    MysteryMerchantRuleTableManager::Instance().InstallSnapshot(set.mysteryMerchantRuleTable);

    MysteryMerchantSpotTableManager::Instance().InstallSnapshot(set.mysteryMerchantSpotTable);

    PetTableManager::Instance().InstallSnapshot(set.petTable);

    PetEntryTableManager::Instance().InstallSnapshot(set.petEntryTable);

    PetRuleTableManager::Instance().InstallSnapshot(set.petRuleTable);

    PetTierTableManager::Instance().InstallSnapshot(set.petTierTable);

    PlayModeTableManager::Instance().InstallSnapshot(set.playModeTable);

    RewardTableManager::Instance().InstallSnapshot(set.rewardTable);

    RoleNameRuleTableManager::Instance().InstallSnapshot(set.roleNameRuleTable);

    SkillTableManager::Instance().InstallSnapshot(set.skillTable);

    SkillPermissionTableManager::Instance().InstallSnapshot(set.skillPermissionTable);

    TestTableManager::Instance().InstallSnapshot(set.testTable);

    TestMultiKeyTableManager::Instance().InstallSnapshot(set.testMultiKeyTable);

    WorldTableManager::Instance().InstallSnapshot(set.worldTable);

    ZoneGroupTableManager::Instance().InstallSnapshot(set.zoneGroupTable);

}

// 按序号加载一张表,序号与上面 LoadTables() 的顺序一致。
void LoadTableAt(std::size_t index) {
    switch (index) {
    case 0: ActivityScheduleTableManager::Instance().Load(); break;
    case 1: ActivityVitalityRewardTableManager::Instance().Load(); break;
    case 2: ActivityVitalityRuleTableManager::Instance().Load(); break;
    case 3: ActorActionCombatStateTableManager::Instance().Load(); break;
    case 4: ActorActionStateTableManager::Instance().Load(); break;
    case 5: AfkPassRuleTableManager::Instance().Load(); break;
    case 6: AttributeAllocRatioTableManager::Instance().Load(); break;
    case 7: AttributeAutoPlanTableManager::Instance().Load(); break;
    case 8: AttributeDimensionTableManager::Instance().Load(); break;
    case 9: AttributePoolTableManager::Instance().Load(); break;
    case 10: AttributeRuleTableManager::Instance().Load(); break;
    case 11: BaseSceneTableManager::Instance().Load(); break;
    case 12: BuffTableManager::Instance().Load(); break;
    case 13: ClassTableManager::Instance().Load(); break;
    case 14: ConditionTableManager::Instance().Load(); break;
    case 15: CooldownTableManager::Instance().Load(); break;
    case 16: DungeonTableManager::Instance().Load(); break;
    case 17: EquipAffixPoolTableManager::Instance().Load(); break;
    case 18: EquipAffixRuleTableManager::Instance().Load(); break;
    case 19: EquipArtifactTableManager::Instance().Load(); break;
    case 20: EquipAttributeTableManager::Instance().Load(); break;
    case 21: EquipAttributeCapTableManager::Instance().Load(); break;
    case 22: EquipEnhanceTableManager::Instance().Load(); break;
    case 23: EquipGemTableManager::Instance().Load(); break;
    case 24: EquipGemSocketTableManager::Instance().Load(); break;
    case 25: EquipSlotTableManager::Instance().Load(); break;
    case 26: EquipV1AffixTableManager::Instance().Load(); break;
    case 27: GlobalVariableTableManager::Instance().Load(); break;
    case 28: GuildActivityTableManager::Instance().Load(); break;
    case 29: GuildDonateTableManager::Instance().Load(); break;
    case 30: GuildLevelTableManager::Instance().Load(); break;
    case 31: GuildRuleTableManager::Instance().Load(); break;
    case 32: GuildShopTableManager::Instance().Load(); break;
    case 33: ItemTableManager::Instance().Load(); break;
    case 34: MessageLimiterTableManager::Instance().Load(); break;
    case 35: MirrorTableManager::Instance().Load(); break;
    case 36: MissionTableManager::Instance().Load(); break;
    case 37: MonsterTableManager::Instance().Load(); break;
    case 38: MysteryMerchantGoodsTableManager::Instance().Load(); break;
    case 39: MysteryMerchantRuleTableManager::Instance().Load(); break;
    case 40: MysteryMerchantSpotTableManager::Instance().Load(); break;
    case 41: PetTableManager::Instance().Load(); break;
    case 42: PetEntryTableManager::Instance().Load(); break;
    case 43: PetRuleTableManager::Instance().Load(); break;
    case 44: PetTierTableManager::Instance().Load(); break;
    case 45: PlayModeTableManager::Instance().Load(); break;
    case 46: RewardTableManager::Instance().Load(); break;
    case 47: RoleNameRuleTableManager::Instance().Load(); break;
    case 48: SkillTableManager::Instance().Load(); break;
    case 49: SkillPermissionTableManager::Instance().Load(); break;
    case 50: TestTableManager::Instance().Load(); break;
    case 51: TestMultiKeyTableManager::Instance().Load(); break;
    case 52: WorldTableManager::Instance().Load(); break;
    case 53: ZoneGroupTableManager::Instance().Load(); break;
    }
}

void PublishCapturedCurrentSet() {
    TableBundleIdentity identity;
    if (gCurrentTableSet) {
        identity = gCurrentTableSet->identity;
    } else {
        identity.schemaDigest = std::string(kTableSchemaDigest);
        identity.source = "image";
    }
    identity.loadSeq = tableLoadVersion;
    gCurrentTableSet = CaptureManagersAsTableSet(identity);
}

} // namespace

ScopedTableSetOverride::ScopedTableSetOverride(const std::shared_ptr<const TableSet>& staged) {
    TableBundleIdentity prevIdentity;
    if (gCurrentTableSet) {
        prevIdentity = gCurrentTableSet->identity;
    }
    previous = CaptureManagersAsTableSet(prevIdentity);
    if (staged) {
        InstallTableSetIntoManagers(*staged);
    }
}

ScopedTableSetOverride::~ScopedTableSetOverride() {
    if (previous) {
        InstallTableSetIntoManagers(*previous);
    }
}

bool IsTableHotReloadable(const std::string_view tableName) {

    return std::binary_search(kHotReloadableTables.begin(), kHotReloadableTables.end(), tableName);

}

std::size_t GetTableCount() {
    return kTableCount;
}

std::string_view GetTableNameAt(const std::size_t index) {
    return index < kTableNames.size() ? kTableNames[index] : std::string_view{};
}

bool LoadTableIntoSetAt(const std::size_t index,
                        const std::string& configDir,
                        const bool useBinary,
                        TableSet& target,
                        std::string& err) {
    switch (index) {
    case 0:
        return ActivityScheduleTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.activityScheduleTable, err);
    case 1:
        return ActivityVitalityRewardTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.activityVitalityRewardTable, err);
    case 2:
        return ActivityVitalityRuleTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.activityVitalityRuleTable, err);
    case 3:
        return ActorActionCombatStateTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.actorActionCombatStateTable, err);
    case 4:
        return ActorActionStateTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.actorActionStateTable, err);
    case 5:
        return AfkPassRuleTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.afkPassRuleTable, err);
    case 6:
        return AttributeAllocRatioTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.attributeAllocRatioTable, err);
    case 7:
        return AttributeAutoPlanTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.attributeAutoPlanTable, err);
    case 8:
        return AttributeDimensionTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.attributeDimensionTable, err);
    case 9:
        return AttributePoolTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.attributePoolTable, err);
    case 10:
        return AttributeRuleTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.attributeRuleTable, err);
    case 11:
        return BaseSceneTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.baseSceneTable, err);
    case 12:
        return BuffTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.buffTable, err);
    case 13:
        return ClassTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.classTable, err);
    case 14:
        return ConditionTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.conditionTable, err);
    case 15:
        return CooldownTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.cooldownTable, err);
    case 16:
        return DungeonTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.dungeonTable, err);
    case 17:
        return EquipAffixPoolTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.equipAffixPoolTable, err);
    case 18:
        return EquipAffixRuleTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.equipAffixRuleTable, err);
    case 19:
        return EquipArtifactTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.equipArtifactTable, err);
    case 20:
        return EquipAttributeTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.equipAttributeTable, err);
    case 21:
        return EquipAttributeCapTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.equipAttributeCapTable, err);
    case 22:
        return EquipEnhanceTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.equipEnhanceTable, err);
    case 23:
        return EquipGemTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.equipGemTable, err);
    case 24:
        return EquipGemSocketTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.equipGemSocketTable, err);
    case 25:
        return EquipSlotTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.equipSlotTable, err);
    case 26:
        return EquipV1AffixTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.equipV1AffixTable, err);
    case 27:
        return GlobalVariableTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.globalVariableTable, err);
    case 28:
        return GuildActivityTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.guildActivityTable, err);
    case 29:
        return GuildDonateTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.guildDonateTable, err);
    case 30:
        return GuildLevelTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.guildLevelTable, err);
    case 31:
        return GuildRuleTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.guildRuleTable, err);
    case 32:
        return GuildShopTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.guildShopTable, err);
    case 33:
        return ItemTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.itemTable, err);
    case 34:
        return MessageLimiterTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.messageLimiterTable, err);
    case 35:
        return MirrorTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.mirrorTable, err);
    case 36:
        return MissionTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.missionTable, err);
    case 37:
        return MonsterTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.monsterTable, err);
    case 38:
        return MysteryMerchantGoodsTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.mysteryMerchantGoodsTable, err);
    case 39:
        return MysteryMerchantRuleTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.mysteryMerchantRuleTable, err);
    case 40:
        return MysteryMerchantSpotTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.mysteryMerchantSpotTable, err);
    case 41:
        return PetTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.petTable, err);
    case 42:
        return PetEntryTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.petEntryTable, err);
    case 43:
        return PetRuleTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.petRuleTable, err);
    case 44:
        return PetTierTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.petTierTable, err);
    case 45:
        return PlayModeTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.playModeTable, err);
    case 46:
        return RewardTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.rewardTable, err);
    case 47:
        return RoleNameRuleTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.roleNameRuleTable, err);
    case 48:
        return SkillTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.skillTable, err);
    case 49:
        return SkillPermissionTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.skillPermissionTable, err);
    case 50:
        return TestTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.testTable, err);
    case 51:
        return TestMultiKeyTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.testMultiKeyTable, err);
    case 52:
        return WorldTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.worldTable, err);
    case 53:
        return ZoneGroupTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.zoneGroupTable, err);
    default:
        err = "table index out of range";
        return false;
    }
}

bool PrepareTableSetSync(const std::string& configDir,
                         const bool useBinary,
                         std::shared_ptr<TableSet>& out,
                         std::string& err) {
    auto staged = std::make_shared<TableSet>();
    for (std::size_t i = 0; i < kTableCount; ++i) {
        if (!LoadTableIntoSetAt(i, configDir, useBinary, *staged, err)) {
            return false;
        }
    }
    out = std::move(staged);
    return true;
}

std::shared_ptr<const TableSet> CurrentTableSet() {
    if (!gCurrentTableSet) {
        PublishCapturedCurrentSet();
    }
    return gCurrentTableSet;
}

std::shared_ptr<const TableSet> CommitTableSet(std::shared_ptr<TableSet> next) {
    if (!next) {
        return CurrentTableSet();
    }
    auto previous = CurrentTableSet();
    next->identity.loadSeq = ++tableLoadVersion;
    InstallTableSetIntoManagers(*next);
    gCurrentTableSet = std::move(next);
    return previous;
}

void SetCurrentTableBundleIdentity(const TableBundleIdentity& identity) {
    TableBundleIdentity nextIdentity = identity;
    nextIdentity.loadSeq = tableLoadVersion;
    gCurrentTableSet = CaptureManagersAsTableSet(nextIdentity);
}

void InvokeTablesLoadSuccess() {

    ActivityScheduleTableManager::Instance().LoadSuccess();

    ActivityVitalityRewardTableManager::Instance().LoadSuccess();

    ActivityVitalityRuleTableManager::Instance().LoadSuccess();

    ActorActionCombatStateTableManager::Instance().LoadSuccess();

    ActorActionStateTableManager::Instance().LoadSuccess();

    AfkPassRuleTableManager::Instance().LoadSuccess();

    AttributeAllocRatioTableManager::Instance().LoadSuccess();

    AttributeAutoPlanTableManager::Instance().LoadSuccess();

    AttributeDimensionTableManager::Instance().LoadSuccess();

    AttributePoolTableManager::Instance().LoadSuccess();

    AttributeRuleTableManager::Instance().LoadSuccess();

    BaseSceneTableManager::Instance().LoadSuccess();

    BuffTableManager::Instance().LoadSuccess();

    ClassTableManager::Instance().LoadSuccess();

    ConditionTableManager::Instance().LoadSuccess();

    CooldownTableManager::Instance().LoadSuccess();

    DungeonTableManager::Instance().LoadSuccess();

    EquipAffixPoolTableManager::Instance().LoadSuccess();

    EquipAffixRuleTableManager::Instance().LoadSuccess();

    EquipArtifactTableManager::Instance().LoadSuccess();

    EquipAttributeTableManager::Instance().LoadSuccess();

    EquipAttributeCapTableManager::Instance().LoadSuccess();

    EquipEnhanceTableManager::Instance().LoadSuccess();

    EquipGemTableManager::Instance().LoadSuccess();

    EquipGemSocketTableManager::Instance().LoadSuccess();

    EquipSlotTableManager::Instance().LoadSuccess();

    EquipV1AffixTableManager::Instance().LoadSuccess();

    GlobalVariableTableManager::Instance().LoadSuccess();

    GuildActivityTableManager::Instance().LoadSuccess();

    GuildDonateTableManager::Instance().LoadSuccess();

    GuildLevelTableManager::Instance().LoadSuccess();

    GuildRuleTableManager::Instance().LoadSuccess();

    GuildShopTableManager::Instance().LoadSuccess();

    ItemTableManager::Instance().LoadSuccess();

    MessageLimiterTableManager::Instance().LoadSuccess();

    MirrorTableManager::Instance().LoadSuccess();

    MissionTableManager::Instance().LoadSuccess();

    MonsterTableManager::Instance().LoadSuccess();

    MysteryMerchantGoodsTableManager::Instance().LoadSuccess();

    MysteryMerchantRuleTableManager::Instance().LoadSuccess();

    MysteryMerchantSpotTableManager::Instance().LoadSuccess();

    PetTableManager::Instance().LoadSuccess();

    PetEntryTableManager::Instance().LoadSuccess();

    PetRuleTableManager::Instance().LoadSuccess();

    PetTierTableManager::Instance().LoadSuccess();

    PlayModeTableManager::Instance().LoadSuccess();

    RewardTableManager::Instance().LoadSuccess();

    RoleNameRuleTableManager::Instance().LoadSuccess();

    SkillTableManager::Instance().LoadSuccess();

    SkillPermissionTableManager::Instance().LoadSuccess();

    TestTableManager::Instance().LoadSuccess();

    TestMultiKeyTableManager::Instance().LoadSuccess();

    WorldTableManager::Instance().LoadSuccess();

    ZoneGroupTableManager::Instance().LoadSuccess();


    if (loadSuccessCallback) {
        loadSuccessCallback();
    }
}

void LoadTables() {

    ActivityScheduleTableManager::Instance().Load();

    ActivityVitalityRewardTableManager::Instance().Load();

    ActivityVitalityRuleTableManager::Instance().Load();

    ActorActionCombatStateTableManager::Instance().Load();

    ActorActionStateTableManager::Instance().Load();

    AfkPassRuleTableManager::Instance().Load();

    AttributeAllocRatioTableManager::Instance().Load();

    AttributeAutoPlanTableManager::Instance().Load();

    AttributeDimensionTableManager::Instance().Load();

    AttributePoolTableManager::Instance().Load();

    AttributeRuleTableManager::Instance().Load();

    BaseSceneTableManager::Instance().Load();

    BuffTableManager::Instance().Load();

    ClassTableManager::Instance().Load();

    ConditionTableManager::Instance().Load();

    CooldownTableManager::Instance().Load();

    DungeonTableManager::Instance().Load();

    EquipAffixPoolTableManager::Instance().Load();

    EquipAffixRuleTableManager::Instance().Load();

    EquipArtifactTableManager::Instance().Load();

    EquipAttributeTableManager::Instance().Load();

    EquipAttributeCapTableManager::Instance().Load();

    EquipEnhanceTableManager::Instance().Load();

    EquipGemTableManager::Instance().Load();

    EquipGemSocketTableManager::Instance().Load();

    EquipSlotTableManager::Instance().Load();

    EquipV1AffixTableManager::Instance().Load();

    GlobalVariableTableManager::Instance().Load();

    GuildActivityTableManager::Instance().Load();

    GuildDonateTableManager::Instance().Load();

    GuildLevelTableManager::Instance().Load();

    GuildRuleTableManager::Instance().Load();

    GuildShopTableManager::Instance().Load();

    ItemTableManager::Instance().Load();

    MessageLimiterTableManager::Instance().Load();

    MirrorTableManager::Instance().Load();

    MissionTableManager::Instance().Load();

    MonsterTableManager::Instance().Load();

    MysteryMerchantGoodsTableManager::Instance().Load();

    MysteryMerchantRuleTableManager::Instance().Load();

    MysteryMerchantSpotTableManager::Instance().Load();

    PetTableManager::Instance().Load();

    PetEntryTableManager::Instance().Load();

    PetRuleTableManager::Instance().Load();

    PetTierTableManager::Instance().Load();

    PlayModeTableManager::Instance().Load();

    RewardTableManager::Instance().Load();

    RoleNameRuleTableManager::Instance().Load();

    SkillTableManager::Instance().Load();

    SkillPermissionTableManager::Instance().Load();

    TestTableManager::Instance().Load();

    TestMultiKeyTableManager::Instance().Load();

    WorldTableManager::Instance().Load();

    ZoneGroupTableManager::Instance().Load();


    PublishCapturedCurrentSet();
    InvokeTablesLoadSuccess();
}

void LoadTablesAsync() {
    // 工作线程数取 CPU 核数与表数的较小者:表再多,线程数和同时在解析的表数都不超过核数。
    const std::size_t workerCount = std::min<std::size_t>(
        std::max<std::size_t>(1, std::thread::hardware_concurrency()), kTableCount);
    std::atomic<std::size_t> nextTable{0};
    {
        // 每次调用各起一组线程,走出这对花括号时全部 join(std::jthread 析构即 join):可以重复调用,
        // LoadSuccess 不会和加载并发。别改回 detach 加函数内静态门闩,那样第二次调用不会等加载完。
        std::vector<std::jthread> workers;
        workers.reserve(workerCount);
        for (std::size_t worker = 0; worker < workerCount; ++worker) {
            workers.emplace_back([&nextTable] {
                void InitThreadLocalConfig();
                InitThreadLocalConfig();
                // 各工作线程循环取号,同一个序号只会被一个线程取到。
                for (;;) {
                    const std::size_t index = nextTable.fetch_add(1);
                    if (index >= kTableCount) {
                        break;
                    }
                    LoadTableAt(index);
                }
            });
        }
    }

    PublishCapturedCurrentSet();
    InvokeTablesLoadSuccess();
}

void OnTablesLoadSuccess(const LoadSuccessCallback& callback) {
    loadSuccessCallback = callback;
}

void ReloadTables() {
    ++tableLoadVersion;
    LoadTables();
}

uint64_t GetTableLoadVersion() {
    return tableLoadVersion;
}