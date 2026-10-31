
#include "all_table.h"

#include <thread>
#include "muduo/base/CountDownLatch.h"

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
#include "equipattribute_table.h"
#include "equipattributecap_table.h"
#include "equipenhance_table.h"
#include "equipgem_table.h"
#include "equipgemsocket_table.h"
#include "equipslot_table.h"
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
#include "petrule_table.h"
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

    EquipAttributeTableManager::Instance().Load();

    EquipAttributeCapTableManager::Instance().Load();

    EquipEnhanceTableManager::Instance().Load();

    EquipGemTableManager::Instance().Load();

    EquipGemSocketTableManager::Instance().Load();

    EquipSlotTableManager::Instance().Load();

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

    PetRuleTableManager::Instance().Load();

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

    EquipAttributeTableManager::Instance().LoadSuccess();

    EquipAttributeCapTableManager::Instance().LoadSuccess();

    EquipEnhanceTableManager::Instance().LoadSuccess();

    EquipGemTableManager::Instance().LoadSuccess();

    EquipGemSocketTableManager::Instance().LoadSuccess();

    EquipSlotTableManager::Instance().LoadSuccess();

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

    PetRuleTableManager::Instance().LoadSuccess();

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
    static muduo::CountDownLatch latch(49);

    std::thread ActivityScheduleLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        ActivityScheduleTableManager::Instance().Load();
        latch.countDown();
    });
    ActivityScheduleLoadThread.detach();

    std::thread ActivityVitalityRewardLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        ActivityVitalityRewardTableManager::Instance().Load();
        latch.countDown();
    });
    ActivityVitalityRewardLoadThread.detach();

    std::thread ActivityVitalityRuleLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        ActivityVitalityRuleTableManager::Instance().Load();
        latch.countDown();
    });
    ActivityVitalityRuleLoadThread.detach();

    std::thread ActorActionCombatStateLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        ActorActionCombatStateTableManager::Instance().Load();
        latch.countDown();
    });
    ActorActionCombatStateLoadThread.detach();

    std::thread ActorActionStateLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        ActorActionStateTableManager::Instance().Load();
        latch.countDown();
    });
    ActorActionStateLoadThread.detach();

    std::thread AttributeAllocRatioLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        AttributeAllocRatioTableManager::Instance().Load();
        latch.countDown();
    });
    AttributeAllocRatioLoadThread.detach();

    std::thread AttributeAutoPlanLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        AttributeAutoPlanTableManager::Instance().Load();
        latch.countDown();
    });
    AttributeAutoPlanLoadThread.detach();

    std::thread AttributeDimensionLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        AttributeDimensionTableManager::Instance().Load();
        latch.countDown();
    });
    AttributeDimensionLoadThread.detach();

    std::thread AttributePoolLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        AttributePoolTableManager::Instance().Load();
        latch.countDown();
    });
    AttributePoolLoadThread.detach();

    std::thread AttributeRuleLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        AttributeRuleTableManager::Instance().Load();
        latch.countDown();
    });
    AttributeRuleLoadThread.detach();

    std::thread BaseSceneLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        BaseSceneTableManager::Instance().Load();
        latch.countDown();
    });
    BaseSceneLoadThread.detach();

    std::thread BuffLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        BuffTableManager::Instance().Load();
        latch.countDown();
    });
    BuffLoadThread.detach();

    std::thread ClassLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        ClassTableManager::Instance().Load();
        latch.countDown();
    });
    ClassLoadThread.detach();

    std::thread ConditionLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        ConditionTableManager::Instance().Load();
        latch.countDown();
    });
    ConditionLoadThread.detach();

    std::thread CooldownLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        CooldownTableManager::Instance().Load();
        latch.countDown();
    });
    CooldownLoadThread.detach();

    std::thread DungeonLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        DungeonTableManager::Instance().Load();
        latch.countDown();
    });
    DungeonLoadThread.detach();

    std::thread EquipAffixPoolLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        EquipAffixPoolTableManager::Instance().Load();
        latch.countDown();
    });
    EquipAffixPoolLoadThread.detach();

    std::thread EquipAffixRuleLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        EquipAffixRuleTableManager::Instance().Load();
        latch.countDown();
    });
    EquipAffixRuleLoadThread.detach();

    std::thread EquipAttributeLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        EquipAttributeTableManager::Instance().Load();
        latch.countDown();
    });
    EquipAttributeLoadThread.detach();

    std::thread EquipAttributeCapLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        EquipAttributeCapTableManager::Instance().Load();
        latch.countDown();
    });
    EquipAttributeCapLoadThread.detach();

    std::thread EquipEnhanceLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        EquipEnhanceTableManager::Instance().Load();
        latch.countDown();
    });
    EquipEnhanceLoadThread.detach();

    std::thread EquipGemLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        EquipGemTableManager::Instance().Load();
        latch.countDown();
    });
    EquipGemLoadThread.detach();

    std::thread EquipGemSocketLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        EquipGemSocketTableManager::Instance().Load();
        latch.countDown();
    });
    EquipGemSocketLoadThread.detach();

    std::thread EquipSlotLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        EquipSlotTableManager::Instance().Load();
        latch.countDown();
    });
    EquipSlotLoadThread.detach();

    std::thread GlobalVariableLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        GlobalVariableTableManager::Instance().Load();
        latch.countDown();
    });
    GlobalVariableLoadThread.detach();

    std::thread GuildActivityLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        GuildActivityTableManager::Instance().Load();
        latch.countDown();
    });
    GuildActivityLoadThread.detach();

    std::thread GuildDonateLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        GuildDonateTableManager::Instance().Load();
        latch.countDown();
    });
    GuildDonateLoadThread.detach();

    std::thread GuildLevelLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        GuildLevelTableManager::Instance().Load();
        latch.countDown();
    });
    GuildLevelLoadThread.detach();

    std::thread GuildRuleLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        GuildRuleTableManager::Instance().Load();
        latch.countDown();
    });
    GuildRuleLoadThread.detach();

    std::thread GuildShopLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        GuildShopTableManager::Instance().Load();
        latch.countDown();
    });
    GuildShopLoadThread.detach();

    std::thread ItemLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        ItemTableManager::Instance().Load();
        latch.countDown();
    });
    ItemLoadThread.detach();

    std::thread MessageLimiterLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        MessageLimiterTableManager::Instance().Load();
        latch.countDown();
    });
    MessageLimiterLoadThread.detach();

    std::thread MirrorLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        MirrorTableManager::Instance().Load();
        latch.countDown();
    });
    MirrorLoadThread.detach();

    std::thread MissionLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        MissionTableManager::Instance().Load();
        latch.countDown();
    });
    MissionLoadThread.detach();

    std::thread MonsterLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        MonsterTableManager::Instance().Load();
        latch.countDown();
    });
    MonsterLoadThread.detach();

    std::thread MysteryMerchantGoodsLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        MysteryMerchantGoodsTableManager::Instance().Load();
        latch.countDown();
    });
    MysteryMerchantGoodsLoadThread.detach();

    std::thread MysteryMerchantRuleLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        MysteryMerchantRuleTableManager::Instance().Load();
        latch.countDown();
    });
    MysteryMerchantRuleLoadThread.detach();

    std::thread MysteryMerchantSpotLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        MysteryMerchantSpotTableManager::Instance().Load();
        latch.countDown();
    });
    MysteryMerchantSpotLoadThread.detach();

    std::thread PetLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        PetTableManager::Instance().Load();
        latch.countDown();
    });
    PetLoadThread.detach();

    std::thread PetRuleLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        PetRuleTableManager::Instance().Load();
        latch.countDown();
    });
    PetRuleLoadThread.detach();

    std::thread PlayModeLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        PlayModeTableManager::Instance().Load();
        latch.countDown();
    });
    PlayModeLoadThread.detach();

    std::thread RewardLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        RewardTableManager::Instance().Load();
        latch.countDown();
    });
    RewardLoadThread.detach();

    std::thread RoleNameRuleLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        RoleNameRuleTableManager::Instance().Load();
        latch.countDown();
    });
    RoleNameRuleLoadThread.detach();

    std::thread SkillLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        SkillTableManager::Instance().Load();
        latch.countDown();
    });
    SkillLoadThread.detach();

    std::thread SkillPermissionLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        SkillPermissionTableManager::Instance().Load();
        latch.countDown();
    });
    SkillPermissionLoadThread.detach();

    std::thread TestLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        TestTableManager::Instance().Load();
        latch.countDown();
    });
    TestLoadThread.detach();

    std::thread TestMultiKeyLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        TestMultiKeyTableManager::Instance().Load();
        latch.countDown();
    });
    TestMultiKeyLoadThread.detach();

    std::thread WorldLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        WorldTableManager::Instance().Load();
        latch.countDown();
    });
    WorldLoadThread.detach();

    std::thread ZoneGroupLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        ZoneGroupTableManager::Instance().Load();
        latch.countDown();
    });
    ZoneGroupLoadThread.detach();

    latch.wait();


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

    EquipAttributeTableManager::Instance().LoadSuccess();

    EquipAttributeCapTableManager::Instance().LoadSuccess();

    EquipEnhanceTableManager::Instance().LoadSuccess();

    EquipGemTableManager::Instance().LoadSuccess();

    EquipGemSocketTableManager::Instance().LoadSuccess();

    EquipSlotTableManager::Instance().LoadSuccess();

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

    PetRuleTableManager::Instance().LoadSuccess();

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