
#include "all_table.h"

#include <thread>
#include "muduo/base/CountDownLatch.h"

#include "actoractioncombatstate_table.h"
#include "actoractionstate_table.h"
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
#include "equipslot_table.h"
#include "globalvariable_table.h"
#include "item_table.h"
#include "messagelimiter_table.h"
#include "mirror_table.h"
#include "mission_table.h"
#include "monster_table.h"
#include "reward_table.h"
#include "skill_table.h"
#include "skillpermission_table.h"
#include "test_table.h"
#include "testmultikey_table.h"
#include "world_table.h"


LoadSuccessCallback loadSuccessCallback;

void LoadTables() {

    ActorActionCombatStateTableManager::Instance().Load();

    ActorActionStateTableManager::Instance().Load();

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

    EquipSlotTableManager::Instance().Load();

    GlobalVariableTableManager::Instance().Load();

    ItemTableManager::Instance().Load();

    MessageLimiterTableManager::Instance().Load();

    MirrorTableManager::Instance().Load();

    MissionTableManager::Instance().Load();

    MonsterTableManager::Instance().Load();

    RewardTableManager::Instance().Load();

    SkillTableManager::Instance().Load();

    SkillPermissionTableManager::Instance().Load();

    TestTableManager::Instance().Load();

    TestMultiKeyTableManager::Instance().Load();

    WorldTableManager::Instance().Load();



    ActorActionCombatStateTableManager::Instance().LoadSuccess();

    ActorActionStateTableManager::Instance().LoadSuccess();

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

    EquipSlotTableManager::Instance().LoadSuccess();

    GlobalVariableTableManager::Instance().LoadSuccess();

    ItemTableManager::Instance().LoadSuccess();

    MessageLimiterTableManager::Instance().LoadSuccess();

    MirrorTableManager::Instance().LoadSuccess();

    MissionTableManager::Instance().LoadSuccess();

    MonsterTableManager::Instance().LoadSuccess();

    RewardTableManager::Instance().LoadSuccess();

    SkillTableManager::Instance().LoadSuccess();

    SkillPermissionTableManager::Instance().LoadSuccess();

    TestTableManager::Instance().LoadSuccess();

    TestMultiKeyTableManager::Instance().LoadSuccess();

    WorldTableManager::Instance().LoadSuccess();


    if (loadSuccessCallback) {
        loadSuccessCallback();
    }
}

void LoadTablesAsync() {
    static muduo::CountDownLatch latch(25);

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

    std::thread RewardLoadThread([]() {
        void InitThreadLocalConfig();
        InitThreadLocalConfig();
        RewardTableManager::Instance().Load();
        latch.countDown();
    });
    RewardLoadThread.detach();

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

    latch.wait();


    ActorActionCombatStateTableManager::Instance().LoadSuccess();

    ActorActionStateTableManager::Instance().LoadSuccess();

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

    EquipSlotTableManager::Instance().LoadSuccess();

    GlobalVariableTableManager::Instance().LoadSuccess();

    ItemTableManager::Instance().LoadSuccess();

    MessageLimiterTableManager::Instance().LoadSuccess();

    MirrorTableManager::Instance().LoadSuccess();

    MissionTableManager::Instance().LoadSuccess();

    MonsterTableManager::Instance().LoadSuccess();

    RewardTableManager::Instance().LoadSuccess();

    SkillTableManager::Instance().LoadSuccess();

    SkillPermissionTableManager::Instance().LoadSuccess();

    TestTableManager::Instance().LoadSuccess();

    TestMultiKeyTableManager::Instance().LoadSuccess();

    WorldTableManager::Instance().LoadSuccess();

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