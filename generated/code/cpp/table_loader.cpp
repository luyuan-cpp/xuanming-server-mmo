
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
#include "appearancerule_table.h"
#include "appearanceseries_table.h"
#include "attributeallocratio_table.h"
#include "attributeautoplan_table.h"
#include "attributedimension_table.h"
#include "attributepool_table.h"
#include "attributerule_table.h"
#include "basescene_table.h"
#include "battleprogressionreward_table.h"
#include "battleprogressionrule_table.h"
#include "buff_table.h"
#include "characterappearance_table.h"
#include "class_table.h"
#include "condition_table.h"
#include "cooldown_table.h"
#include "dungeon_table.h"
#include "equipaffixpool_table.h"
#include "equipaffixquality_table.h"
#include "equipaffixrule_table.h"
#include "equipartifact_table.h"
#include "equipattribute_table.h"
#include "equipattributecap_table.h"
#include "equipenhance_table.h"
#include "equipgem_table.h"
#include "equipgemsocket_table.h"
#include "equipslot_table.h"
#include "equipv1affix_table.h"
#include "festivalbeastactivity_table.h"
#include "festivalbeasttier_table.h"
#include "globalvariable_table.h"
#include "guildactivity_table.h"
#include "guilddonate_table.h"
#include "guildlevel_table.h"
#include "guildrule_table.h"
#include "guildshop_table.h"
#include "guildwarmilestone_table.h"
#include "guildwarrule_table.h"
#include "item_table.h"
#include "jianghurule_table.h"
#include "messagelimiter_table.h"
#include "mirror_table.h"
#include "mission_table.h"
#include "monster_table.h"
#include "mysterymerchantgoods_table.h"
#include "mysterymerchantrule_table.h"
#include "mysterymerchantspot_table.h"
#include "pet_table.h"
#include "petcosmetic_table.h"
#include "petentry_table.h"
#include "petrule_table.h"
#include "pettier_table.h"
#include "playmode_table.h"
#include "playerlevelexp_table.h"
#include "rankedpreset_table.h"
#include "rankedrule_table.h"
#include "reward_table.h"
#include "rewardedadslot_table.h"
#include "rolenamerule_table.h"
#include "seasonrule_table.h"
#include "seasonseal_table.h"
#include "skill_table.h"
#include "skillpermission_table.h"
#include "skillscrollcraft_table.h"
#include "skillvfxcosmetic_table.h"
#include "spiritshopgoods_table.h"
#include "spiritshoplimitgroup_table.h"
#include "test_table.h"
#include "testmultikey_table.h"
#include "tournamentreward_table.h"
#include "tournamentrule_table.h"
#include "travelpassseason_table.h"
#include "travelpasstier_table.h"
#include "trialjournalentry_table.h"
#include "trialjournalmilestone_table.h"
#include "trialjournalseal_table.h"
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

constexpr std::size_t kTableCount = 83;

constexpr std::array<std::string_view, 83> kTableNames = {
    "ActivitySchedule",
    "ActivityVitalityReward",
    "ActivityVitalityRule",
    "ActorActionCombatState",
    "ActorActionState",
    "AfkPassRule",
    "AppearanceRule",
    "AppearanceSeries",
    "AttributeAllocRatio",
    "AttributeAutoPlan",
    "AttributeDimension",
    "AttributePool",
    "AttributeRule",
    "BaseScene",
    "BattleProgressionReward",
    "BattleProgressionRule",
    "Buff",
    "CharacterAppearance",
    "Class",
    "Condition",
    "Cooldown",
    "Dungeon",
    "EquipAffixPool",
    "EquipAffixQuality",
    "EquipAffixRule",
    "EquipArtifact",
    "EquipAttribute",
    "EquipAttributeCap",
    "EquipEnhance",
    "EquipGem",
    "EquipGemSocket",
    "EquipSlot",
    "EquipV1Affix",
    "FestivalBeastActivity",
    "FestivalBeastTier",
    "GlobalVariable",
    "GuildActivity",
    "GuildDonate",
    "GuildLevel",
    "GuildRule",
    "GuildShop",
    "GuildWarMilestone",
    "GuildWarRule",
    "Item",
    "JianghuRule",
    "MessageLimiter",
    "Mirror",
    "Mission",
    "Monster",
    "MysteryMerchantGoods",
    "MysteryMerchantRule",
    "MysteryMerchantSpot",
    "Pet",
    "PetCosmetic",
    "PetEntry",
    "PetRule",
    "PetTier",
    "PlayMode",
    "PlayerLevelExp",
    "RankedPreset",
    "RankedRule",
    "Reward",
    "RewardedAdSlot",
    "RoleNameRule",
    "SeasonRule",
    "SeasonSeal",
    "Skill",
    "SkillPermission",
    "SkillScrollCraft",
    "SkillVfxCosmetic",
    "SpiritShopGoods",
    "SpiritShopLimitGroup",
    "Test",
    "TestMultiKey",
    "TournamentReward",
    "TournamentRule",
    "TravelPassSeason",
    "TravelPassTier",
    "TrialJournalEntry",
    "TrialJournalMilestone",
    "TrialJournalSeal",
    "World",
    "ZoneGroup",
};


constexpr std::array<std::string_view, 33> kHotReloadableTables = {
    "ActivitySchedule",
    "ActivityVitalityReward",
    "ActivityVitalityRule",
    "BattleProgressionReward",
    "BattleProgressionRule",
    "Buff",
    "Cooldown",
    "EquipAffixQuality",
    "FestivalBeastActivity",
    "FestivalBeastTier",
    "GuildDonate",
    "GuildShop",
    "GuildWarMilestone",
    "GuildWarRule",
    "JianghuRule",
    "MessageLimiter",
    "Monster",
    "MysteryMerchantGoods",
    "MysteryMerchantRule",
    "PlayerLevelExp",
    "RewardedAdSlot",
    "SeasonRule",
    "SeasonSeal",
    "Skill",
    "SkillPermission",
    "SkillScrollCraft",
    "SpiritShopGoods",
    "SpiritShopLimitGroup",
    "TravelPassSeason",
    "TravelPassTier",
    "TrialJournalEntry",
    "TrialJournalMilestone",
    "TrialJournalSeal",
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

    set->appearanceRuleTable = AppearanceRuleTableManager::Instance().CurrentSnapshot();

    set->appearanceSeriesTable = AppearanceSeriesTableManager::Instance().CurrentSnapshot();

    set->attributeAllocRatioTable = AttributeAllocRatioTableManager::Instance().CurrentSnapshot();

    set->attributeAutoPlanTable = AttributeAutoPlanTableManager::Instance().CurrentSnapshot();

    set->attributeDimensionTable = AttributeDimensionTableManager::Instance().CurrentSnapshot();

    set->attributePoolTable = AttributePoolTableManager::Instance().CurrentSnapshot();

    set->attributeRuleTable = AttributeRuleTableManager::Instance().CurrentSnapshot();

    set->baseSceneTable = BaseSceneTableManager::Instance().CurrentSnapshot();

    set->battleProgressionRewardTable = BattleProgressionRewardTableManager::Instance().CurrentSnapshot();

    set->battleProgressionRuleTable = BattleProgressionRuleTableManager::Instance().CurrentSnapshot();

    set->buffTable = BuffTableManager::Instance().CurrentSnapshot();

    set->characterAppearanceTable = CharacterAppearanceTableManager::Instance().CurrentSnapshot();

    set->classTable = ClassTableManager::Instance().CurrentSnapshot();

    set->conditionTable = ConditionTableManager::Instance().CurrentSnapshot();

    set->cooldownTable = CooldownTableManager::Instance().CurrentSnapshot();

    set->dungeonTable = DungeonTableManager::Instance().CurrentSnapshot();

    set->equipAffixPoolTable = EquipAffixPoolTableManager::Instance().CurrentSnapshot();

    set->equipAffixQualityTable = EquipAffixQualityTableManager::Instance().CurrentSnapshot();

    set->equipAffixRuleTable = EquipAffixRuleTableManager::Instance().CurrentSnapshot();

    set->equipArtifactTable = EquipArtifactTableManager::Instance().CurrentSnapshot();

    set->equipAttributeTable = EquipAttributeTableManager::Instance().CurrentSnapshot();

    set->equipAttributeCapTable = EquipAttributeCapTableManager::Instance().CurrentSnapshot();

    set->equipEnhanceTable = EquipEnhanceTableManager::Instance().CurrentSnapshot();

    set->equipGemTable = EquipGemTableManager::Instance().CurrentSnapshot();

    set->equipGemSocketTable = EquipGemSocketTableManager::Instance().CurrentSnapshot();

    set->equipSlotTable = EquipSlotTableManager::Instance().CurrentSnapshot();

    set->equipV1AffixTable = EquipV1AffixTableManager::Instance().CurrentSnapshot();

    set->festivalBeastActivityTable = FestivalBeastActivityTableManager::Instance().CurrentSnapshot();

    set->festivalBeastTierTable = FestivalBeastTierTableManager::Instance().CurrentSnapshot();

    set->globalVariableTable = GlobalVariableTableManager::Instance().CurrentSnapshot();

    set->guildActivityTable = GuildActivityTableManager::Instance().CurrentSnapshot();

    set->guildDonateTable = GuildDonateTableManager::Instance().CurrentSnapshot();

    set->guildLevelTable = GuildLevelTableManager::Instance().CurrentSnapshot();

    set->guildRuleTable = GuildRuleTableManager::Instance().CurrentSnapshot();

    set->guildShopTable = GuildShopTableManager::Instance().CurrentSnapshot();

    set->guildWarMilestoneTable = GuildWarMilestoneTableManager::Instance().CurrentSnapshot();

    set->guildWarRuleTable = GuildWarRuleTableManager::Instance().CurrentSnapshot();

    set->itemTable = ItemTableManager::Instance().CurrentSnapshot();

    set->jianghuRuleTable = JianghuRuleTableManager::Instance().CurrentSnapshot();

    set->messageLimiterTable = MessageLimiterTableManager::Instance().CurrentSnapshot();

    set->mirrorTable = MirrorTableManager::Instance().CurrentSnapshot();

    set->missionTable = MissionTableManager::Instance().CurrentSnapshot();

    set->monsterTable = MonsterTableManager::Instance().CurrentSnapshot();

    set->mysteryMerchantGoodsTable = MysteryMerchantGoodsTableManager::Instance().CurrentSnapshot();

    set->mysteryMerchantRuleTable = MysteryMerchantRuleTableManager::Instance().CurrentSnapshot();

    set->mysteryMerchantSpotTable = MysteryMerchantSpotTableManager::Instance().CurrentSnapshot();

    set->petTable = PetTableManager::Instance().CurrentSnapshot();

    set->petCosmeticTable = PetCosmeticTableManager::Instance().CurrentSnapshot();

    set->petEntryTable = PetEntryTableManager::Instance().CurrentSnapshot();

    set->petRuleTable = PetRuleTableManager::Instance().CurrentSnapshot();

    set->petTierTable = PetTierTableManager::Instance().CurrentSnapshot();

    set->playModeTable = PlayModeTableManager::Instance().CurrentSnapshot();

    set->playerLevelExpTable = PlayerLevelExpTableManager::Instance().CurrentSnapshot();

    set->rankedPresetTable = RankedPresetTableManager::Instance().CurrentSnapshot();

    set->rankedRuleTable = RankedRuleTableManager::Instance().CurrentSnapshot();

    set->rewardTable = RewardTableManager::Instance().CurrentSnapshot();

    set->rewardedAdSlotTable = RewardedAdSlotTableManager::Instance().CurrentSnapshot();

    set->roleNameRuleTable = RoleNameRuleTableManager::Instance().CurrentSnapshot();

    set->seasonRuleTable = SeasonRuleTableManager::Instance().CurrentSnapshot();

    set->seasonSealTable = SeasonSealTableManager::Instance().CurrentSnapshot();

    set->skillTable = SkillTableManager::Instance().CurrentSnapshot();

    set->skillPermissionTable = SkillPermissionTableManager::Instance().CurrentSnapshot();

    set->skillScrollCraftTable = SkillScrollCraftTableManager::Instance().CurrentSnapshot();

    set->skillVfxCosmeticTable = SkillVfxCosmeticTableManager::Instance().CurrentSnapshot();

    set->spiritShopGoodsTable = SpiritShopGoodsTableManager::Instance().CurrentSnapshot();

    set->spiritShopLimitGroupTable = SpiritShopLimitGroupTableManager::Instance().CurrentSnapshot();

    set->testTable = TestTableManager::Instance().CurrentSnapshot();

    set->testMultiKeyTable = TestMultiKeyTableManager::Instance().CurrentSnapshot();

    set->tournamentRewardTable = TournamentRewardTableManager::Instance().CurrentSnapshot();

    set->tournamentRuleTable = TournamentRuleTableManager::Instance().CurrentSnapshot();

    set->travelPassSeasonTable = TravelPassSeasonTableManager::Instance().CurrentSnapshot();

    set->travelPassTierTable = TravelPassTierTableManager::Instance().CurrentSnapshot();

    set->trialJournalEntryTable = TrialJournalEntryTableManager::Instance().CurrentSnapshot();

    set->trialJournalMilestoneTable = TrialJournalMilestoneTableManager::Instance().CurrentSnapshot();

    set->trialJournalSealTable = TrialJournalSealTableManager::Instance().CurrentSnapshot();

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

    AppearanceRuleTableManager::Instance().InstallSnapshot(set.appearanceRuleTable);

    AppearanceSeriesTableManager::Instance().InstallSnapshot(set.appearanceSeriesTable);

    AttributeAllocRatioTableManager::Instance().InstallSnapshot(set.attributeAllocRatioTable);

    AttributeAutoPlanTableManager::Instance().InstallSnapshot(set.attributeAutoPlanTable);

    AttributeDimensionTableManager::Instance().InstallSnapshot(set.attributeDimensionTable);

    AttributePoolTableManager::Instance().InstallSnapshot(set.attributePoolTable);

    AttributeRuleTableManager::Instance().InstallSnapshot(set.attributeRuleTable);

    BaseSceneTableManager::Instance().InstallSnapshot(set.baseSceneTable);

    BattleProgressionRewardTableManager::Instance().InstallSnapshot(set.battleProgressionRewardTable);

    BattleProgressionRuleTableManager::Instance().InstallSnapshot(set.battleProgressionRuleTable);

    BuffTableManager::Instance().InstallSnapshot(set.buffTable);

    CharacterAppearanceTableManager::Instance().InstallSnapshot(set.characterAppearanceTable);

    ClassTableManager::Instance().InstallSnapshot(set.classTable);

    ConditionTableManager::Instance().InstallSnapshot(set.conditionTable);

    CooldownTableManager::Instance().InstallSnapshot(set.cooldownTable);

    DungeonTableManager::Instance().InstallSnapshot(set.dungeonTable);

    EquipAffixPoolTableManager::Instance().InstallSnapshot(set.equipAffixPoolTable);

    EquipAffixQualityTableManager::Instance().InstallSnapshot(set.equipAffixQualityTable);

    EquipAffixRuleTableManager::Instance().InstallSnapshot(set.equipAffixRuleTable);

    EquipArtifactTableManager::Instance().InstallSnapshot(set.equipArtifactTable);

    EquipAttributeTableManager::Instance().InstallSnapshot(set.equipAttributeTable);

    EquipAttributeCapTableManager::Instance().InstallSnapshot(set.equipAttributeCapTable);

    EquipEnhanceTableManager::Instance().InstallSnapshot(set.equipEnhanceTable);

    EquipGemTableManager::Instance().InstallSnapshot(set.equipGemTable);

    EquipGemSocketTableManager::Instance().InstallSnapshot(set.equipGemSocketTable);

    EquipSlotTableManager::Instance().InstallSnapshot(set.equipSlotTable);

    EquipV1AffixTableManager::Instance().InstallSnapshot(set.equipV1AffixTable);

    FestivalBeastActivityTableManager::Instance().InstallSnapshot(set.festivalBeastActivityTable);

    FestivalBeastTierTableManager::Instance().InstallSnapshot(set.festivalBeastTierTable);

    GlobalVariableTableManager::Instance().InstallSnapshot(set.globalVariableTable);

    GuildActivityTableManager::Instance().InstallSnapshot(set.guildActivityTable);

    GuildDonateTableManager::Instance().InstallSnapshot(set.guildDonateTable);

    GuildLevelTableManager::Instance().InstallSnapshot(set.guildLevelTable);

    GuildRuleTableManager::Instance().InstallSnapshot(set.guildRuleTable);

    GuildShopTableManager::Instance().InstallSnapshot(set.guildShopTable);

    GuildWarMilestoneTableManager::Instance().InstallSnapshot(set.guildWarMilestoneTable);

    GuildWarRuleTableManager::Instance().InstallSnapshot(set.guildWarRuleTable);

    ItemTableManager::Instance().InstallSnapshot(set.itemTable);

    JianghuRuleTableManager::Instance().InstallSnapshot(set.jianghuRuleTable);

    MessageLimiterTableManager::Instance().InstallSnapshot(set.messageLimiterTable);

    MirrorTableManager::Instance().InstallSnapshot(set.mirrorTable);

    MissionTableManager::Instance().InstallSnapshot(set.missionTable);

    MonsterTableManager::Instance().InstallSnapshot(set.monsterTable);

    MysteryMerchantGoodsTableManager::Instance().InstallSnapshot(set.mysteryMerchantGoodsTable);

    MysteryMerchantRuleTableManager::Instance().InstallSnapshot(set.mysteryMerchantRuleTable);

    MysteryMerchantSpotTableManager::Instance().InstallSnapshot(set.mysteryMerchantSpotTable);

    PetTableManager::Instance().InstallSnapshot(set.petTable);

    PetCosmeticTableManager::Instance().InstallSnapshot(set.petCosmeticTable);

    PetEntryTableManager::Instance().InstallSnapshot(set.petEntryTable);

    PetRuleTableManager::Instance().InstallSnapshot(set.petRuleTable);

    PetTierTableManager::Instance().InstallSnapshot(set.petTierTable);

    PlayModeTableManager::Instance().InstallSnapshot(set.playModeTable);

    PlayerLevelExpTableManager::Instance().InstallSnapshot(set.playerLevelExpTable);

    RankedPresetTableManager::Instance().InstallSnapshot(set.rankedPresetTable);

    RankedRuleTableManager::Instance().InstallSnapshot(set.rankedRuleTable);

    RewardTableManager::Instance().InstallSnapshot(set.rewardTable);

    RewardedAdSlotTableManager::Instance().InstallSnapshot(set.rewardedAdSlotTable);

    RoleNameRuleTableManager::Instance().InstallSnapshot(set.roleNameRuleTable);

    SeasonRuleTableManager::Instance().InstallSnapshot(set.seasonRuleTable);

    SeasonSealTableManager::Instance().InstallSnapshot(set.seasonSealTable);

    SkillTableManager::Instance().InstallSnapshot(set.skillTable);

    SkillPermissionTableManager::Instance().InstallSnapshot(set.skillPermissionTable);

    SkillScrollCraftTableManager::Instance().InstallSnapshot(set.skillScrollCraftTable);

    SkillVfxCosmeticTableManager::Instance().InstallSnapshot(set.skillVfxCosmeticTable);

    SpiritShopGoodsTableManager::Instance().InstallSnapshot(set.spiritShopGoodsTable);

    SpiritShopLimitGroupTableManager::Instance().InstallSnapshot(set.spiritShopLimitGroupTable);

    TestTableManager::Instance().InstallSnapshot(set.testTable);

    TestMultiKeyTableManager::Instance().InstallSnapshot(set.testMultiKeyTable);

    TournamentRewardTableManager::Instance().InstallSnapshot(set.tournamentRewardTable);

    TournamentRuleTableManager::Instance().InstallSnapshot(set.tournamentRuleTable);

    TravelPassSeasonTableManager::Instance().InstallSnapshot(set.travelPassSeasonTable);

    TravelPassTierTableManager::Instance().InstallSnapshot(set.travelPassTierTable);

    TrialJournalEntryTableManager::Instance().InstallSnapshot(set.trialJournalEntryTable);

    TrialJournalMilestoneTableManager::Instance().InstallSnapshot(set.trialJournalMilestoneTable);

    TrialJournalSealTableManager::Instance().InstallSnapshot(set.trialJournalSealTable);

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
    case 6: AppearanceRuleTableManager::Instance().Load(); break;
    case 7: AppearanceSeriesTableManager::Instance().Load(); break;
    case 8: AttributeAllocRatioTableManager::Instance().Load(); break;
    case 9: AttributeAutoPlanTableManager::Instance().Load(); break;
    case 10: AttributeDimensionTableManager::Instance().Load(); break;
    case 11: AttributePoolTableManager::Instance().Load(); break;
    case 12: AttributeRuleTableManager::Instance().Load(); break;
    case 13: BaseSceneTableManager::Instance().Load(); break;
    case 14: BattleProgressionRewardTableManager::Instance().Load(); break;
    case 15: BattleProgressionRuleTableManager::Instance().Load(); break;
    case 16: BuffTableManager::Instance().Load(); break;
    case 17: CharacterAppearanceTableManager::Instance().Load(); break;
    case 18: ClassTableManager::Instance().Load(); break;
    case 19: ConditionTableManager::Instance().Load(); break;
    case 20: CooldownTableManager::Instance().Load(); break;
    case 21: DungeonTableManager::Instance().Load(); break;
    case 22: EquipAffixPoolTableManager::Instance().Load(); break;
    case 23: EquipAffixQualityTableManager::Instance().Load(); break;
    case 24: EquipAffixRuleTableManager::Instance().Load(); break;
    case 25: EquipArtifactTableManager::Instance().Load(); break;
    case 26: EquipAttributeTableManager::Instance().Load(); break;
    case 27: EquipAttributeCapTableManager::Instance().Load(); break;
    case 28: EquipEnhanceTableManager::Instance().Load(); break;
    case 29: EquipGemTableManager::Instance().Load(); break;
    case 30: EquipGemSocketTableManager::Instance().Load(); break;
    case 31: EquipSlotTableManager::Instance().Load(); break;
    case 32: EquipV1AffixTableManager::Instance().Load(); break;
    case 33: FestivalBeastActivityTableManager::Instance().Load(); break;
    case 34: FestivalBeastTierTableManager::Instance().Load(); break;
    case 35: GlobalVariableTableManager::Instance().Load(); break;
    case 36: GuildActivityTableManager::Instance().Load(); break;
    case 37: GuildDonateTableManager::Instance().Load(); break;
    case 38: GuildLevelTableManager::Instance().Load(); break;
    case 39: GuildRuleTableManager::Instance().Load(); break;
    case 40: GuildShopTableManager::Instance().Load(); break;
    case 41: GuildWarMilestoneTableManager::Instance().Load(); break;
    case 42: GuildWarRuleTableManager::Instance().Load(); break;
    case 43: ItemTableManager::Instance().Load(); break;
    case 44: JianghuRuleTableManager::Instance().Load(); break;
    case 45: MessageLimiterTableManager::Instance().Load(); break;
    case 46: MirrorTableManager::Instance().Load(); break;
    case 47: MissionTableManager::Instance().Load(); break;
    case 48: MonsterTableManager::Instance().Load(); break;
    case 49: MysteryMerchantGoodsTableManager::Instance().Load(); break;
    case 50: MysteryMerchantRuleTableManager::Instance().Load(); break;
    case 51: MysteryMerchantSpotTableManager::Instance().Load(); break;
    case 52: PetTableManager::Instance().Load(); break;
    case 53: PetCosmeticTableManager::Instance().Load(); break;
    case 54: PetEntryTableManager::Instance().Load(); break;
    case 55: PetRuleTableManager::Instance().Load(); break;
    case 56: PetTierTableManager::Instance().Load(); break;
    case 57: PlayModeTableManager::Instance().Load(); break;
    case 58: PlayerLevelExpTableManager::Instance().Load(); break;
    case 59: RankedPresetTableManager::Instance().Load(); break;
    case 60: RankedRuleTableManager::Instance().Load(); break;
    case 61: RewardTableManager::Instance().Load(); break;
    case 62: RewardedAdSlotTableManager::Instance().Load(); break;
    case 63: RoleNameRuleTableManager::Instance().Load(); break;
    case 64: SeasonRuleTableManager::Instance().Load(); break;
    case 65: SeasonSealTableManager::Instance().Load(); break;
    case 66: SkillTableManager::Instance().Load(); break;
    case 67: SkillPermissionTableManager::Instance().Load(); break;
    case 68: SkillScrollCraftTableManager::Instance().Load(); break;
    case 69: SkillVfxCosmeticTableManager::Instance().Load(); break;
    case 70: SpiritShopGoodsTableManager::Instance().Load(); break;
    case 71: SpiritShopLimitGroupTableManager::Instance().Load(); break;
    case 72: TestTableManager::Instance().Load(); break;
    case 73: TestMultiKeyTableManager::Instance().Load(); break;
    case 74: TournamentRewardTableManager::Instance().Load(); break;
    case 75: TournamentRuleTableManager::Instance().Load(); break;
    case 76: TravelPassSeasonTableManager::Instance().Load(); break;
    case 77: TravelPassTierTableManager::Instance().Load(); break;
    case 78: TrialJournalEntryTableManager::Instance().Load(); break;
    case 79: TrialJournalMilestoneTableManager::Instance().Load(); break;
    case 80: TrialJournalSealTableManager::Instance().Load(); break;
    case 81: WorldTableManager::Instance().Load(); break;
    case 82: ZoneGroupTableManager::Instance().Load(); break;
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
        return AppearanceRuleTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.appearanceRuleTable, err);
    case 7:
        return AppearanceSeriesTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.appearanceSeriesTable, err);
    case 8:
        return AttributeAllocRatioTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.attributeAllocRatioTable, err);
    case 9:
        return AttributeAutoPlanTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.attributeAutoPlanTable, err);
    case 10:
        return AttributeDimensionTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.attributeDimensionTable, err);
    case 11:
        return AttributePoolTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.attributePoolTable, err);
    case 12:
        return AttributeRuleTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.attributeRuleTable, err);
    case 13:
        return BaseSceneTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.baseSceneTable, err);
    case 14:
        return BattleProgressionRewardTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.battleProgressionRewardTable, err);
    case 15:
        return BattleProgressionRuleTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.battleProgressionRuleTable, err);
    case 16:
        return BuffTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.buffTable, err);
    case 17:
        return CharacterAppearanceTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.characterAppearanceTable, err);
    case 18:
        return ClassTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.classTable, err);
    case 19:
        return ConditionTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.conditionTable, err);
    case 20:
        return CooldownTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.cooldownTable, err);
    case 21:
        return DungeonTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.dungeonTable, err);
    case 22:
        return EquipAffixPoolTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.equipAffixPoolTable, err);
    case 23:
        return EquipAffixQualityTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.equipAffixQualityTable, err);
    case 24:
        return EquipAffixRuleTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.equipAffixRuleTable, err);
    case 25:
        return EquipArtifactTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.equipArtifactTable, err);
    case 26:
        return EquipAttributeTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.equipAttributeTable, err);
    case 27:
        return EquipAttributeCapTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.equipAttributeCapTable, err);
    case 28:
        return EquipEnhanceTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.equipEnhanceTable, err);
    case 29:
        return EquipGemTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.equipGemTable, err);
    case 30:
        return EquipGemSocketTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.equipGemSocketTable, err);
    case 31:
        return EquipSlotTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.equipSlotTable, err);
    case 32:
        return EquipV1AffixTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.equipV1AffixTable, err);
    case 33:
        return FestivalBeastActivityTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.festivalBeastActivityTable, err);
    case 34:
        return FestivalBeastTierTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.festivalBeastTierTable, err);
    case 35:
        return GlobalVariableTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.globalVariableTable, err);
    case 36:
        return GuildActivityTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.guildActivityTable, err);
    case 37:
        return GuildDonateTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.guildDonateTable, err);
    case 38:
        return GuildLevelTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.guildLevelTable, err);
    case 39:
        return GuildRuleTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.guildRuleTable, err);
    case 40:
        return GuildShopTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.guildShopTable, err);
    case 41:
        return GuildWarMilestoneTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.guildWarMilestoneTable, err);
    case 42:
        return GuildWarRuleTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.guildWarRuleTable, err);
    case 43:
        return ItemTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.itemTable, err);
    case 44:
        return JianghuRuleTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.jianghuRuleTable, err);
    case 45:
        return MessageLimiterTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.messageLimiterTable, err);
    case 46:
        return MirrorTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.mirrorTable, err);
    case 47:
        return MissionTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.missionTable, err);
    case 48:
        return MonsterTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.monsterTable, err);
    case 49:
        return MysteryMerchantGoodsTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.mysteryMerchantGoodsTable, err);
    case 50:
        return MysteryMerchantRuleTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.mysteryMerchantRuleTable, err);
    case 51:
        return MysteryMerchantSpotTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.mysteryMerchantSpotTable, err);
    case 52:
        return PetTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.petTable, err);
    case 53:
        return PetCosmeticTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.petCosmeticTable, err);
    case 54:
        return PetEntryTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.petEntryTable, err);
    case 55:
        return PetRuleTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.petRuleTable, err);
    case 56:
        return PetTierTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.petTierTable, err);
    case 57:
        return PlayModeTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.playModeTable, err);
    case 58:
        return PlayerLevelExpTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.playerLevelExpTable, err);
    case 59:
        return RankedPresetTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.rankedPresetTable, err);
    case 60:
        return RankedRuleTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.rankedRuleTable, err);
    case 61:
        return RewardTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.rewardTable, err);
    case 62:
        return RewardedAdSlotTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.rewardedAdSlotTable, err);
    case 63:
        return RoleNameRuleTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.roleNameRuleTable, err);
    case 64:
        return SeasonRuleTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.seasonRuleTable, err);
    case 65:
        return SeasonSealTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.seasonSealTable, err);
    case 66:
        return SkillTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.skillTable, err);
    case 67:
        return SkillPermissionTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.skillPermissionTable, err);
    case 68:
        return SkillScrollCraftTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.skillScrollCraftTable, err);
    case 69:
        return SkillVfxCosmeticTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.skillVfxCosmeticTable, err);
    case 70:
        return SpiritShopGoodsTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.spiritShopGoodsTable, err);
    case 71:
        return SpiritShopLimitGroupTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.spiritShopLimitGroupTable, err);
    case 72:
        return TestTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.testTable, err);
    case 73:
        return TestMultiKeyTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.testMultiKeyTable, err);
    case 74:
        return TournamentRewardTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.tournamentRewardTable, err);
    case 75:
        return TournamentRuleTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.tournamentRuleTable, err);
    case 76:
        return TravelPassSeasonTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.travelPassSeasonTable, err);
    case 77:
        return TravelPassTierTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.travelPassTierTable, err);
    case 78:
        return TrialJournalEntryTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.trialJournalEntryTable, err);
    case 79:
        return TrialJournalMilestoneTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.trialJournalMilestoneTable, err);
    case 80:
        return TrialJournalSealTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.trialJournalSealTable, err);
    case 81:
        return WorldTableManager::ParseSnapshotFromDir(
            configDir, useBinary, target.worldTable, err);
    case 82:
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

    AppearanceRuleTableManager::Instance().LoadSuccess();

    AppearanceSeriesTableManager::Instance().LoadSuccess();

    AttributeAllocRatioTableManager::Instance().LoadSuccess();

    AttributeAutoPlanTableManager::Instance().LoadSuccess();

    AttributeDimensionTableManager::Instance().LoadSuccess();

    AttributePoolTableManager::Instance().LoadSuccess();

    AttributeRuleTableManager::Instance().LoadSuccess();

    BaseSceneTableManager::Instance().LoadSuccess();

    BattleProgressionRewardTableManager::Instance().LoadSuccess();

    BattleProgressionRuleTableManager::Instance().LoadSuccess();

    BuffTableManager::Instance().LoadSuccess();

    CharacterAppearanceTableManager::Instance().LoadSuccess();

    ClassTableManager::Instance().LoadSuccess();

    ConditionTableManager::Instance().LoadSuccess();

    CooldownTableManager::Instance().LoadSuccess();

    DungeonTableManager::Instance().LoadSuccess();

    EquipAffixPoolTableManager::Instance().LoadSuccess();

    EquipAffixQualityTableManager::Instance().LoadSuccess();

    EquipAffixRuleTableManager::Instance().LoadSuccess();

    EquipArtifactTableManager::Instance().LoadSuccess();

    EquipAttributeTableManager::Instance().LoadSuccess();

    EquipAttributeCapTableManager::Instance().LoadSuccess();

    EquipEnhanceTableManager::Instance().LoadSuccess();

    EquipGemTableManager::Instance().LoadSuccess();

    EquipGemSocketTableManager::Instance().LoadSuccess();

    EquipSlotTableManager::Instance().LoadSuccess();

    EquipV1AffixTableManager::Instance().LoadSuccess();

    FestivalBeastActivityTableManager::Instance().LoadSuccess();

    FestivalBeastTierTableManager::Instance().LoadSuccess();

    GlobalVariableTableManager::Instance().LoadSuccess();

    GuildActivityTableManager::Instance().LoadSuccess();

    GuildDonateTableManager::Instance().LoadSuccess();

    GuildLevelTableManager::Instance().LoadSuccess();

    GuildRuleTableManager::Instance().LoadSuccess();

    GuildShopTableManager::Instance().LoadSuccess();

    GuildWarMilestoneTableManager::Instance().LoadSuccess();

    GuildWarRuleTableManager::Instance().LoadSuccess();

    ItemTableManager::Instance().LoadSuccess();

    JianghuRuleTableManager::Instance().LoadSuccess();

    MessageLimiterTableManager::Instance().LoadSuccess();

    MirrorTableManager::Instance().LoadSuccess();

    MissionTableManager::Instance().LoadSuccess();

    MonsterTableManager::Instance().LoadSuccess();

    MysteryMerchantGoodsTableManager::Instance().LoadSuccess();

    MysteryMerchantRuleTableManager::Instance().LoadSuccess();

    MysteryMerchantSpotTableManager::Instance().LoadSuccess();

    PetTableManager::Instance().LoadSuccess();

    PetCosmeticTableManager::Instance().LoadSuccess();

    PetEntryTableManager::Instance().LoadSuccess();

    PetRuleTableManager::Instance().LoadSuccess();

    PetTierTableManager::Instance().LoadSuccess();

    PlayModeTableManager::Instance().LoadSuccess();

    PlayerLevelExpTableManager::Instance().LoadSuccess();

    RankedPresetTableManager::Instance().LoadSuccess();

    RankedRuleTableManager::Instance().LoadSuccess();

    RewardTableManager::Instance().LoadSuccess();

    RewardedAdSlotTableManager::Instance().LoadSuccess();

    RoleNameRuleTableManager::Instance().LoadSuccess();

    SeasonRuleTableManager::Instance().LoadSuccess();

    SeasonSealTableManager::Instance().LoadSuccess();

    SkillTableManager::Instance().LoadSuccess();

    SkillPermissionTableManager::Instance().LoadSuccess();

    SkillScrollCraftTableManager::Instance().LoadSuccess();

    SkillVfxCosmeticTableManager::Instance().LoadSuccess();

    SpiritShopGoodsTableManager::Instance().LoadSuccess();

    SpiritShopLimitGroupTableManager::Instance().LoadSuccess();

    TestTableManager::Instance().LoadSuccess();

    TestMultiKeyTableManager::Instance().LoadSuccess();

    TournamentRewardTableManager::Instance().LoadSuccess();

    TournamentRuleTableManager::Instance().LoadSuccess();

    TravelPassSeasonTableManager::Instance().LoadSuccess();

    TravelPassTierTableManager::Instance().LoadSuccess();

    TrialJournalEntryTableManager::Instance().LoadSuccess();

    TrialJournalMilestoneTableManager::Instance().LoadSuccess();

    TrialJournalSealTableManager::Instance().LoadSuccess();

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

    AppearanceRuleTableManager::Instance().Load();

    AppearanceSeriesTableManager::Instance().Load();

    AttributeAllocRatioTableManager::Instance().Load();

    AttributeAutoPlanTableManager::Instance().Load();

    AttributeDimensionTableManager::Instance().Load();

    AttributePoolTableManager::Instance().Load();

    AttributeRuleTableManager::Instance().Load();

    BaseSceneTableManager::Instance().Load();

    BattleProgressionRewardTableManager::Instance().Load();

    BattleProgressionRuleTableManager::Instance().Load();

    BuffTableManager::Instance().Load();

    CharacterAppearanceTableManager::Instance().Load();

    ClassTableManager::Instance().Load();

    ConditionTableManager::Instance().Load();

    CooldownTableManager::Instance().Load();

    DungeonTableManager::Instance().Load();

    EquipAffixPoolTableManager::Instance().Load();

    EquipAffixQualityTableManager::Instance().Load();

    EquipAffixRuleTableManager::Instance().Load();

    EquipArtifactTableManager::Instance().Load();

    EquipAttributeTableManager::Instance().Load();

    EquipAttributeCapTableManager::Instance().Load();

    EquipEnhanceTableManager::Instance().Load();

    EquipGemTableManager::Instance().Load();

    EquipGemSocketTableManager::Instance().Load();

    EquipSlotTableManager::Instance().Load();

    EquipV1AffixTableManager::Instance().Load();

    FestivalBeastActivityTableManager::Instance().Load();

    FestivalBeastTierTableManager::Instance().Load();

    GlobalVariableTableManager::Instance().Load();

    GuildActivityTableManager::Instance().Load();

    GuildDonateTableManager::Instance().Load();

    GuildLevelTableManager::Instance().Load();

    GuildRuleTableManager::Instance().Load();

    GuildShopTableManager::Instance().Load();

    GuildWarMilestoneTableManager::Instance().Load();

    GuildWarRuleTableManager::Instance().Load();

    ItemTableManager::Instance().Load();

    JianghuRuleTableManager::Instance().Load();

    MessageLimiterTableManager::Instance().Load();

    MirrorTableManager::Instance().Load();

    MissionTableManager::Instance().Load();

    MonsterTableManager::Instance().Load();

    MysteryMerchantGoodsTableManager::Instance().Load();

    MysteryMerchantRuleTableManager::Instance().Load();

    MysteryMerchantSpotTableManager::Instance().Load();

    PetTableManager::Instance().Load();

    PetCosmeticTableManager::Instance().Load();

    PetEntryTableManager::Instance().Load();

    PetRuleTableManager::Instance().Load();

    PetTierTableManager::Instance().Load();

    PlayModeTableManager::Instance().Load();

    PlayerLevelExpTableManager::Instance().Load();

    RankedPresetTableManager::Instance().Load();

    RankedRuleTableManager::Instance().Load();

    RewardTableManager::Instance().Load();

    RewardedAdSlotTableManager::Instance().Load();

    RoleNameRuleTableManager::Instance().Load();

    SeasonRuleTableManager::Instance().Load();

    SeasonSealTableManager::Instance().Load();

    SkillTableManager::Instance().Load();

    SkillPermissionTableManager::Instance().Load();

    SkillScrollCraftTableManager::Instance().Load();

    SkillVfxCosmeticTableManager::Instance().Load();

    SpiritShopGoodsTableManager::Instance().Load();

    SpiritShopLimitGroupTableManager::Instance().Load();

    TestTableManager::Instance().Load();

    TestMultiKeyTableManager::Instance().Load();

    TournamentRewardTableManager::Instance().Load();

    TournamentRuleTableManager::Instance().Load();

    TravelPassSeasonTableManager::Instance().Load();

    TravelPassTierTableManager::Instance().Load();

    TrialJournalEntryTableManager::Instance().Load();

    TrialJournalMilestoneTableManager::Instance().Load();

    TrialJournalSealTableManager::Instance().Load();

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