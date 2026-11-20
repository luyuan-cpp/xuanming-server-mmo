
#include "table_loader.h"

#include <algorithm>
#include <atomic>
#include <cstddef>
#include <thread>
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


LoadSuccessCallback loadSuccessCallback;

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

namespace {

constexpr std::size_t kTableCount = 54;

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

} // namespace

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

void OnTablesLoadSuccess(const LoadSuccessCallback& callback) {
    loadSuccessCallback = callback;
}

static uint64_t tableLoadVersion = 0;

void ReloadTables() {
    LoadTables();
    ++tableLoadVersion;
}

uint64_t GetTableLoadVersion() {
    return tableLoadVersion;
}