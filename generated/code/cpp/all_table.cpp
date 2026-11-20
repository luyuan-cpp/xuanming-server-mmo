
#include "all_table.h"

#include <thread>

#include "activityschedule_table.h"
#include "activityvitalityreward_table.h"
#include "activityvitalityrule_table.h"
#include "actoractioncombatstate_table.h"
#include "actoractionstate_table.h"
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

void LoadTablesAsync() {
    // 每次调用各起一组线程,走出这对花括号时全部 join(std::jthread 析构即 join):可以重复调用,
    // LoadSuccess 不会和加载并发。别改回 detach 加函数内静态门闩,那样第二次调用不会等加载完。
    // shortcut: 每张表一条线程,表数量远多于 CPU 核数时改成按核数限流的线程池。
    {

        std::jthread ActivityScheduleLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            ActivityScheduleTableManager::Instance().Load();
        });

        std::jthread ActivityVitalityRewardLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            ActivityVitalityRewardTableManager::Instance().Load();
        });

        std::jthread ActivityVitalityRuleLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            ActivityVitalityRuleTableManager::Instance().Load();
        });

        std::jthread ActorActionCombatStateLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            ActorActionCombatStateTableManager::Instance().Load();
        });

        std::jthread ActorActionStateLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            ActorActionStateTableManager::Instance().Load();
        });

        std::jthread AttributeAllocRatioLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            AttributeAllocRatioTableManager::Instance().Load();
        });

        std::jthread AttributeAutoPlanLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            AttributeAutoPlanTableManager::Instance().Load();
        });

        std::jthread AttributeDimensionLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            AttributeDimensionTableManager::Instance().Load();
        });

        std::jthread AttributePoolLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            AttributePoolTableManager::Instance().Load();
        });

        std::jthread AttributeRuleLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            AttributeRuleTableManager::Instance().Load();
        });

        std::jthread BaseSceneLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            BaseSceneTableManager::Instance().Load();
        });

        std::jthread BuffLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            BuffTableManager::Instance().Load();
        });

        std::jthread ClassLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            ClassTableManager::Instance().Load();
        });

        std::jthread ConditionLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            ConditionTableManager::Instance().Load();
        });

        std::jthread CooldownLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            CooldownTableManager::Instance().Load();
        });

        std::jthread DungeonLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            DungeonTableManager::Instance().Load();
        });

        std::jthread EquipAffixPoolLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            EquipAffixPoolTableManager::Instance().Load();
        });

        std::jthread EquipAffixRuleLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            EquipAffixRuleTableManager::Instance().Load();
        });

        std::jthread EquipArtifactLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            EquipArtifactTableManager::Instance().Load();
        });

        std::jthread EquipAttributeLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            EquipAttributeTableManager::Instance().Load();
        });

        std::jthread EquipAttributeCapLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            EquipAttributeCapTableManager::Instance().Load();
        });

        std::jthread EquipEnhanceLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            EquipEnhanceTableManager::Instance().Load();
        });

        std::jthread EquipGemLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            EquipGemTableManager::Instance().Load();
        });

        std::jthread EquipGemSocketLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            EquipGemSocketTableManager::Instance().Load();
        });

        std::jthread EquipSlotLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            EquipSlotTableManager::Instance().Load();
        });

        std::jthread EquipV1AffixLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            EquipV1AffixTableManager::Instance().Load();
        });

        std::jthread GlobalVariableLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            GlobalVariableTableManager::Instance().Load();
        });

        std::jthread GuildActivityLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            GuildActivityTableManager::Instance().Load();
        });

        std::jthread GuildDonateLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            GuildDonateTableManager::Instance().Load();
        });

        std::jthread GuildLevelLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            GuildLevelTableManager::Instance().Load();
        });

        std::jthread GuildRuleLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            GuildRuleTableManager::Instance().Load();
        });

        std::jthread GuildShopLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            GuildShopTableManager::Instance().Load();
        });

        std::jthread ItemLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            ItemTableManager::Instance().Load();
        });

        std::jthread MessageLimiterLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            MessageLimiterTableManager::Instance().Load();
        });

        std::jthread MirrorLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            MirrorTableManager::Instance().Load();
        });

        std::jthread MissionLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            MissionTableManager::Instance().Load();
        });

        std::jthread MonsterLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            MonsterTableManager::Instance().Load();
        });

        std::jthread MysteryMerchantGoodsLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            MysteryMerchantGoodsTableManager::Instance().Load();
        });

        std::jthread MysteryMerchantRuleLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            MysteryMerchantRuleTableManager::Instance().Load();
        });

        std::jthread MysteryMerchantSpotLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            MysteryMerchantSpotTableManager::Instance().Load();
        });

        std::jthread PetLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            PetTableManager::Instance().Load();
        });

        std::jthread PetEntryLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            PetEntryTableManager::Instance().Load();
        });

        std::jthread PetRuleLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            PetRuleTableManager::Instance().Load();
        });

        std::jthread PetTierLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            PetTierTableManager::Instance().Load();
        });

        std::jthread PlayModeLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            PlayModeTableManager::Instance().Load();
        });

        std::jthread RewardLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            RewardTableManager::Instance().Load();
        });

        std::jthread RoleNameRuleLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            RoleNameRuleTableManager::Instance().Load();
        });

        std::jthread SkillLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            SkillTableManager::Instance().Load();
        });

        std::jthread SkillPermissionLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            SkillPermissionTableManager::Instance().Load();
        });

        std::jthread TestLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            TestTableManager::Instance().Load();
        });

        std::jthread TestMultiKeyLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            TestMultiKeyTableManager::Instance().Load();
        });

        std::jthread WorldLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            WorldTableManager::Instance().Load();
        });

        std::jthread ZoneGroupLoadThread([]() {
            void InitThreadLocalConfig();
            InitThreadLocalConfig();
            ZoneGroupTableManager::Instance().Load();
        });

    }


    ActivityScheduleTableManager::Instance().LoadSuccess();

    ActivityVitalityRewardTableManager::Instance().LoadSuccess();

    ActivityVitalityRuleTableManager::Instance().LoadSuccess();

    ActorActionCombatStateTableManager::Instance().LoadSuccess();

    ActorActionStateTableManager::Instance().LoadSuccess();

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