#include "gtest/gtest.h"
#include "gmock/gmock.h"
#include <limits>
#include <filesystem>
#include <fstream>
#include <functional>
#include <cstdint>
#include <string>
#include <google/protobuf/unknown_field_set.h>
#include "combat/skill/system/skill.h"
#include "actor/actor_net_id.h"
#include "core/system/id_generator.h"

#include "proto/scene/player_skill.pb.h"
#include "table/code/skill_table.h"
#include "table/code/actoractionstate_table.h"
#include "table/code/skillpermission_table.h"
#include "table/code/buff_table.h"
#include "table/code/class_table.h"
#include "table/code/actoractioncombatstate_table.h"
#include "combat/skill/comp/skill_comp.h"
#include "combat/skill/constants/skill.h"
#include "time/comp/timer_task_comp.h"
#include "time/system/time_cooldown.h"
#include "table/proto/tip/skill_error_tip.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/entity_error_tip.pb.h"
#include "player/system/player_skill.h"
#include "player/comp/player_frozen_comp.h"
#include "modules/scene/comp/scene_comp.h"
#include "proto/common/component/player_comp.pb.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/actor_attribute_state_comp.pb.h"
#include "actor/action_state/constants/actor_state.h"
#include "actor/action_state/system/actor_action_state.h"
#include "combat_state/constants/combat_state.h"
#include "combat_state/system/combat_state.h"
#include "combat/buff/constants/buff.h"
#include "combat/buff/system/buff.h"
#include "proto/common/component/actor_combat_state_comp.pb.h"
#include "muduo/base/CurrentThread.h"
#include "macros/return_define.h"
#include <thread_context/ecs_context.h>
#include "../test_config_helper.h"

using ::testing::_;
using ::testing::Return;

// Mocked classes and functions
class MockSkillTable {
public:
    MOCK_METHOD(const SkillTable*, GetSkillTable, (uint32_t), (const));
    MOCK_METHOD(bool, IsSkillOfType, (uint32_t, uint32_t), (const));
};

class MockCooldownTimeUtil {
public:
    MOCK_METHOD(bool, IsInCooldown, (const CooldownTimeComp&), (const));
};

namespace {
// 夹具模拟正式角色创建边界，先安装号源，再为每个参与技能的实体分配网络身份。
entt::entity CreateSkillActor()
{
    const auto actor = tlsEcs.actorRegistry.create();
    EXPECT_NE(0, actor_net_id::Assign(tlsEcs.actorRegistry, actor));
    return actor;
}
}
// Test Fixture
class SkillUtilTest : public ::testing::Test {
protected:
    void SetUp() override {
        skillUtil = std::make_unique<SkillSystem>();
        // Setup the mocks if necessary
    }

    void TearDown() override {
        tlsEcs.actorRegistry.clear(); // Clean up the thread-local storage after each test
        tlsEcs.sceneRegistry.clear();
    }

    // 放进指定场景的玩家：ValidateSkillTargetEntity 要求目标是 Player/Npc 且与施法者同场景。
    static entt::entity CreatePlayerInScene(SceneEntity scene) {
        const auto player = CreateSkillActor();
        tlsEcs.actorRegistry.emplace<Player>(player);
        tlsEcs.actorRegistry.emplace<SceneEntityComp>(player, SceneEntityComp{ scene });
        return player;
    }

    // 持久和派生数据由加载/初始化提供;不预挂冷却、技能上下文或行为状态。
    static entt::entity CreateLoadedPlayerInScene(SceneEntity scene) {
        const auto player = CreatePlayerInScene(scene);
        tlsEcs.actorRegistry.emplace<BaseAttributesComp>(player).set_health(100);
        tlsEcs.actorRegistry.emplace<LevelComp>(player).set_level(1);
        tlsEcs.actorRegistry.emplace<DerivedAttributesComp>(player);
        return player;
    }

    static ::ReleaseSkillRequest TargetedRequest(entt::entity target) {
        ::ReleaseSkillRequest request;
        request.set_skill_table_id(1); // 1 号技能是指定目标类(targeting_mode 首项为 1 → kTargetedSkill)
        request.set_target_id(actor_net_id::Of(tlsEcs.actorRegistry, target));
        return request;
    }

    static bool RunUntil(const std::function<bool()>& finished) {
        auto* loop = EventLoop::getEventLoopOfCurrentThread();
        if (loop == nullptr) return false;
        if (finished()) return true;
        const auto poll = loop->runEvery(0.01, [loop, finished] {
            if (finished()) loop->quit();
        });
        const auto deadline = loop->runAfter(5.0, [loop] { loop->quit(); });
        loop->loop();
        loop->cancel(poll);
        loop->cancel(deadline);
        return finished();
    }

    template <typename MissingComp>
    void ExpectDelayedCastWithMissingDataHasNoDamageOrBuffSideEffects() {
        const auto scene = tlsEcs.sceneRegistry.create();
        const auto caster = CreateLoadedPlayerInScene(scene);
        const auto target = CreateLoadedPlayerInScene(scene);
        auto request = TargetedRequest(target);
        ASSERT_EQ(kSuccess, SkillSystem::ReleaseSkill(caster, &request));
        const auto* contexts = tlsEcs.actorRegistry.try_get<SkillContextCompMap>(caster);
        ASSERT_NE(nullptr, contexts);
        ASSERT_EQ(1u, contexts->size());
        const auto skillId = contexts->begin()->first;
        const auto* castRow = SkillTableManager::Instance().FindById(1).first;
        ASSERT_NE(nullptr, castRow);
        ASSERT_GT(castRow->cast_point(), 0.0);
        const auto* bonusRow = BuffTableManager::Instance().FindById(19).first;
        ASSERT_NE(nullptr, bonusRow);
        ASSERT_EQ(kBuffTypeNextBasicAttack, bonusRow->buff_type());
        const auto [buffError, bonusBuffId] = BuffSystem::AddOrUpdateBuff(caster, bonusRow->id());
        ASSERT_EQ(kSuccess, buffError);
        ASSERT_NE(UINT64_MAX, bonusBuffId);
        tlsEcs.actorRegistry.remove<MissingComp>(caster);

        // 跑真实前摇回调,不直接复制落点步骤;按上下文结束退出,5秒为失败上限。
        ASSERT_TRUE(RunUntil([caster = caster, skillId = skillId] {
            const auto* current = tlsEcs.actorRegistry.try_get<SkillContextCompMap>(caster);
            return current == nullptr || !current->contains(skillId);
        }));

        EXPECT_EQ(100u, tlsEcs.actorRegistry.get<BaseAttributesComp>(target).health());
        const auto* buffs = tlsEcs.actorRegistry.try_get<BuffListComp>(caster);
        ASSERT_NE(nullptr, buffs);
        EXPECT_TRUE(buffs->contains(bonusBuffId)) << "数据缺失不得消费真实Buff19(+66伤害)";
        EXPECT_FALSE(tlsEcs.actorRegistry.all_of<BuffListComp>(target)) << "失败落点不得继续派发技能效果";
        EXPECT_FALSE(tlsEcs.actorRegistry.all_of<SkillContextCompMap>(target));
        EXPECT_FALSE((tlsEcs.actorRegistry.any_of<CastingTimerComp, RecoveryTimerComp,
            ChannelFinishTimerComp, ChannelIntervalTimerComp>(caster)));
        EXPECT_FALSE(tlsEcs.actorRegistry.all_of<MissingComp>(caster));
    }

    std::unique_ptr<SkillSystem> skillUtil;
    std::unique_ptr<MockSkillTable> mockSkillTable = std::make_unique<MockSkillTable>();
    std::unique_ptr<MockCooldownTimeUtil> mockCooldownTimeUtil = std::make_unique<MockCooldownTimeUtil>();
};

// 当前打包策略允许这些状态;临时表构造合法的拒绝策略,验证规则改变后也不会先破坏旧施法。
// 状态通过真实Buff事件/动作入口建立,不插入越界或不存在的状态编号。
class SkillPrecheckPolicyTest : public SkillUtilTest {
protected:
    bool WriteTable(const char* name, const std::string& contents) {
        std::ofstream output(tableDir / name, std::ios::binary | std::ios::trunc);
        output << contents;
        return output.good();
    }

    void SetSkillPermission(uint32_t generalSkillTip) {
        const auto success = std::to_string(kSuccess);
        ASSERT_TRUE(WriteTable("skillpermission.json", "{\"data\":[{\"id\":1,\"skill_type\":[" +
            success + "," + std::to_string(generalSkillTip) + "," + success + "," + success + "," + success + "," + success + "]}]}"));
        SkillPermissionTableManager::Instance().Load();
    }

    void SetActionPolicy(uint32_t followingTip) {
        const auto allowed = "{\"state_mode\":" + std::to_string(kActionStatePermitted) +
            ",\"state_tip\":" + std::to_string(kSuccess) + "}";
        const auto following = "{\"state_mode\":" + std::to_string(kActionStateMutualExclusion) +
            ",\"state_tip\":" + std::to_string(followingTip) + "}";
        ASSERT_TRUE(WriteTable("actoractionstate.json", "{\"data\":[{\"id\":0,\"state\":[" +
            allowed + "," + following + "," + allowed + "]},{\"id\":1,\"state\":[" +
            allowed + "," + allowed + "," + allowed + "]}]}"));
        ActorActionStateTableManager::Instance().Load();
    }

    void SetUp() override {
        SkillUtilTest::SetUp();
        auto& config = tlsNodeConfigManager.GetBaseDeployConfig();
        previousDirectory = config.table_data_directory();
        previousFormat = config.table_data_format();
        tableDir = std::filesystem::temp_directory_path() /
            ("mmorpg_skill_precheck_" + std::to_string(muduo::CurrentThread::tid()) + "_" +
             std::to_string(reinterpret_cast<std::uintptr_t>(this)));
        std::error_code error;
        std::filesystem::create_directories(tableDir, error);
        ASSERT_FALSE(error) << error.message();
        config.set_table_data_directory((tableDir / "").string());
        config.set_table_data_format("json");
        SetSkillPermission(kSuccess);
        SetActionPolicy(kSuccess);
        ASSERT_TRUE(WriteTable("buff.json", "{\"data\":[{\"id\":9001,\"buff_type\":" +
            std::to_string(kBuffTypeSilence) + ",\"duration\":-1,\"max_layer\":1}]}"));
        BuffTableManager::Instance().Load();

        const auto scene = tlsEcs.sceneRegistry.create();
        caster = CreateLoadedPlayerInScene(scene);
        target = CreateLoadedPlayerInScene(scene);
        auto oldRequest = TargetedRequest(target);
        oldRequest.set_skill_table_id(2); // 冷却0、前摇0;未推进loop前仍处于合法前摇阶段。
        ASSERT_EQ(kSuccess, SkillSystem::ReleaseSkill(caster, &oldRequest));
        const auto* contexts = tlsEcs.actorRegistry.try_get<SkillContextCompMap>(caster);
        ASSERT_NE(nullptr, contexts);
        ASSERT_EQ(1u, contexts->size());
        oldSkillId = contexts->begin()->first;
        oldContext = contexts->begin()->second;
        ASSERT_TRUE(tlsEcs.actorRegistry.get<CastingTimerComp>(caster).timer.IsActive());
    }

    void TearDown() override {
        SkillUtilTest::TearDown();
        auto& config = tlsNodeConfigManager.GetBaseDeployConfig();
        config.set_table_data_directory(previousDirectory);
        config.set_table_data_format(previousFormat);
        SkillPermissionTableManager::Instance().Load();
        ActorActionStateTableManager::Instance().Load();
        BuffTableManager::Instance().Load();
        std::error_code error;
        std::filesystem::remove_all(tableDir, error);
    }

    void ExpectRejectionPreservesOldCast() {
        EXPECT_FALSE(tlsEcs.actorRegistry.any_of<CooldownTimeListComp>(caster));
        const auto statesBefore = tlsEcs.actorRegistry.get<ActorStateComp>(caster).SerializeAsString();
        const auto* immediate = SkillTableManager::Instance().FindById(1).first;
        ASSERT_NE(nullptr, immediate);
        ASSERT_TRUE(immediate->immediate());
        auto request = TargetedRequest(target);
        EXPECT_EQ(kSkillCannotBeCastInCurrentState, SkillSystem::ReleaseSkill(caster, &request));

        const auto* phase = tlsEcs.actorRegistry.try_get<CastingTimerComp>(caster);
        ASSERT_NE(nullptr, phase);
        EXPECT_EQ(oldSkillId, phase->skillId);
        EXPECT_TRUE(phase->timer.IsActive());
        const auto* contexts = tlsEcs.actorRegistry.try_get<SkillContextCompMap>(caster);
        ASSERT_NE(nullptr, contexts);
        ASSERT_EQ(1u, contexts->size());
        ASSERT_TRUE(contexts->contains(oldSkillId));
        EXPECT_EQ(oldContext, contexts->at(oldSkillId));
        EXPECT_FALSE(tlsEcs.actorRegistry.all_of<SkillContextCompMap>(target));
        EXPECT_FALSE(tlsEcs.actorRegistry.any_of<CooldownTimeListComp>(caster));
        EXPECT_EQ(statesBefore, tlsEcs.actorRegistry.get<ActorStateComp>(caster).SerializeAsString());
    }

    std::filesystem::path tableDir;
    std::string previousDirectory, previousFormat;
    entt::entity caster{entt::null}, target{entt::null};
    uint64_t oldSkillId{};
    SkillContextPtrComp oldContext;
};

TEST_F(SkillPrecheckPolicyTest, BuffPermissionRefusalPreservesPriorPhaseAndContext) {
    SetSkillPermission(kSkillCannotBeCastInCurrentState);
    const auto [result, buffId] = BuffSystem::AddOrUpdateBuff(caster, 9001);
    ASSERT_EQ(kSuccess, result);
    ASSERT_NE(UINT64_MAX, buffId);
    const auto* states = tlsEcs.actorRegistry.try_get<CombatStateCollectionComp>(caster);
    ASSERT_NE(nullptr, states);
    ASSERT_TRUE(states->states().contains(kActorCombatStateSilence));
    ExpectRejectionPreservesOldCast();
}

TEST_F(SkillPrecheckPolicyTest, ActionConflictRefusalPreservesPriorPhaseAndContext) {
    SetActionPolicy(kSkillCannotBeCastInCurrentState);
    ASSERT_EQ(kSuccess, ActorActionStateSystem::TryPerformAction(caster, kActorActionJoinFollow, kActorStateTeamFollow));
    ExpectRejectionPreservesOldCast();
}

TEST_F(SkillUtilTest, ValidateTarget_InvalidTarget_ReturnsError) {
    ::ReleaseSkillRequest request;
    request.set_skill_table_id(1);
    request.set_target_id(-1); // Invalid target ID

    EXPECT_CALL(*mockSkillTable, GetSkillTable(request.skill_table_id()))
        .WillRepeatedly(Return(nullptr)); // Mock implementation

    const entt::entity caster = CreateSkillActor();
    uint32_t result = skillUtil->ValidateTarget(caster, &request, entt::null);
    EXPECT_EQ(result, kSkillInvalidTargetId);
}

TEST_F(SkillUtilTest, ValidateTarget_ValidTarget_ReturnsOk) {
    const auto scene = tlsEcs.sceneRegistry.create();
    const auto caster = CreatePlayerInScene(scene);
    const auto target = CreatePlayerInScene(scene);
    const auto request = TargetedRequest(target);
    EXPECT_EQ(kSuccess, skillUtil->ValidateTarget(caster, &request, target));
}

TEST_F(SkillUtilTest, ReleaseSkill_RejectsLocalEntityIntegerAndPreviousNetworkIdentity) {
    const auto scene = tlsEcs.sceneRegistry.create();
    const auto caster = CreateLoadedPlayerInScene(scene);
    const auto target = CreateLoadedPlayerInScene(scene);
    auto request = TargetedRequest(target);
    const auto previousNetId = request.target_id();
    ASSERT_NE(0u, previousNetId);
    request.set_target_id(entt::to_integral(target));
    EXPECT_EQ(kSkillInvalidTargetId, SkillSystem::ReleaseSkill(caster, &request));
    tlsEcs.actorRegistry.destroy(target);
    const auto replacement = CreateLoadedPlayerInScene(scene);
    request.set_target_id(previousNetId);
    EXPECT_EQ(kSkillInvalidTargetId, SkillSystem::ReleaseSkill(caster, &request));
    request = TargetedRequest(replacement);
    EXPECT_EQ(kSuccess, SkillSystem::ReleaseSkill(caster, &request));
}

// actorRegistry 由本节点所有场景共用：真实施法入口仍不能越过同场景约束。
TEST_F(SkillUtilTest, ReleaseSkill_TargetInAnotherScene_ReturnsError) {
    const auto casterScene = tlsEcs.sceneRegistry.create();
    const auto otherScene = tlsEcs.sceneRegistry.create();
    const auto caster = CreateLoadedPlayerInScene(casterScene);
    const auto sameSceneTarget = CreateLoadedPlayerInScene(casterScene);
    const auto otherSceneTarget = CreateLoadedPlayerInScene(otherScene);

    auto otherSceneRequest = TargetedRequest(otherSceneTarget);
    EXPECT_EQ(kSkillInvalidTargetId, SkillSystem::ReleaseSkill(caster, &otherSceneRequest));
    auto sameSceneRequest = TargetedRequest(sameSceneTarget);
    EXPECT_EQ(kSuccess, SkillSystem::ReleaseSkill(caster, &sameSceneRequest));
}

TEST_F(SkillUtilTest, ReleaseSkill_FrozenTarget_ReturnsError) {
    const auto scene = tlsEcs.sceneRegistry.create();
    const auto caster = CreateLoadedPlayerInScene(scene);
    const auto target = CreateLoadedPlayerInScene(scene);
    tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(target);

    auto request = TargetedRequest(target);
    EXPECT_EQ(kSkillInvalidTargetId, SkillSystem::ReleaseSkill(caster, &request));
}

TEST_F(SkillUtilTest, ReleaseSkillRejectsDestroyedSceneAfterItsSlotIsReused) {
    const auto scene = tlsEcs.sceneRegistry.create();
    const auto caster = CreateLoadedPlayerInScene(scene);
    const auto target = CreateLoadedPlayerInScene(scene);
    auto request = TargetedRequest(target);
    tlsEcs.sceneRegistry.destroy(scene);
    const auto replacement = tlsEcs.sceneRegistry.create();
    ASSERT_EQ(entt::to_entity(scene), entt::to_entity(replacement));
    ASSERT_NE(scene, replacement);
    ASSERT_FALSE(tlsEcs.sceneRegistry.valid(scene));
    ASSERT_TRUE(tlsEcs.sceneRegistry.valid(replacement));

    // 两个玩家仍保存同一个旧句柄，不能把「值相等」当作仍在同一存活场景。
    EXPECT_NE(kSuccess, SkillSystem::ReleaseSkill(caster, &request));

    EXPECT_FALSE((tlsEcs.actorRegistry.any_of<CooldownTimeListComp, SkillContextCompMap,
        ActorStateComp, CastingTimerComp, RecoveryTimerComp,
        ChannelFinishTimerComp, ChannelIntervalTimerComp>(caster)));
    EXPECT_FALSE((tlsEcs.actorRegistry.any_of<SkillContextCompMap, BuffListComp>(target)));
    EXPECT_EQ(100u, tlsEcs.actorRegistry.get<BaseAttributesComp>(target).health());
}

TEST_F(SkillUtilTest, DestroyedSceneBeforeCastPointHasNoDamageOrBuffSideEffects) {
    const auto scene = tlsEcs.sceneRegistry.create();
    const auto caster = CreateLoadedPlayerInScene(scene);
    const auto target = CreateLoadedPlayerInScene(scene);
    auto request = TargetedRequest(target);
    ASSERT_EQ(kSuccess, SkillSystem::ReleaseSkill(caster, &request));
    const auto* contexts = tlsEcs.actorRegistry.try_get<SkillContextCompMap>(caster);
    ASSERT_NE(nullptr, contexts);
    ASSERT_EQ(1u, contexts->size());
    const auto skillId = contexts->begin()->first;
    const auto* skillRow = SkillTableManager::Instance().FindById(1).first;
    ASSERT_NE(nullptr, skillRow);
    ASSERT_GT(skillRow->cast_point(), 0.0);
    const auto [buffError, bonusBuffId] = BuffSystem::AddOrUpdateBuff(caster, 19);
    ASSERT_EQ(kSuccess, buffError);
    ASSERT_NE(UINT64_MAX, bonusBuffId);
    tlsEcs.sceneRegistry.destroy(scene);

    // 驱动真实前摇回调，检查延迟落点会重新验证场景，而非只在施法入口检查。
    ASSERT_TRUE(RunUntil([caster = caster, skillId = skillId] {
        const auto* current = tlsEcs.actorRegistry.try_get<SkillContextCompMap>(caster);
        return current == nullptr || !current->contains(skillId);
    }));

    EXPECT_EQ(100u, tlsEcs.actorRegistry.get<BaseAttributesComp>(target).health());
    const auto* buffs = tlsEcs.actorRegistry.try_get<BuffListComp>(caster);
    ASSERT_NE(nullptr, buffs);
    EXPECT_TRUE(buffs->contains(bonusBuffId)) << "场景已销毁的落点不得消费下一次攻击加成";
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<BuffListComp>(target));
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<SkillContextCompMap>(target));
    EXPECT_FALSE((tlsEcs.actorRegistry.any_of<CastingTimerComp, RecoveryTimerComp,
        ChannelFinishTimerComp, ChannelIntervalTimerComp>(caster)));
}

TEST_F(SkillUtilTest, GeneralSkillCallbackImmediatelyAbortsDestroyedCasterScene) {
    const auto scene = tlsEcs.sceneRegistry.create();
    const auto caster = CreateLoadedPlayerInScene(scene);
    const auto target = CreateLoadedPlayerInScene(scene);
    auto request = TargetedRequest(target);
    ASSERT_EQ(kSuccess, SkillSystem::ReleaseSkill(caster, &request));
    const auto* contexts = tlsEcs.actorRegistry.try_get<SkillContextCompMap>(caster);
    ASSERT_NE(nullptr, contexts);
    ASSERT_EQ(1u, contexts->size());
    const auto skillId = contexts->begin()->first;
    ASSERT_TRUE(tlsEcs.actorRegistry.all_of<CastingTimerComp>(caster));
    tlsEcs.sceneRegistry.destroy(scene);

    // 直接进入真实前摇处理函数，不等后摇自然结束；失效场景须立即撤销本次施法。
    SkillSystem::HandleGeneralSkillSpell(caster, skillId);

    const auto* remaining = tlsEcs.actorRegistry.try_get<SkillContextCompMap>(caster);
    EXPECT_TRUE(remaining == nullptr || !remaining->contains(skillId));
    EXPECT_FALSE((tlsEcs.actorRegistry.any_of<CastingTimerComp, RecoveryTimerComp,
        ChannelFinishTimerComp, ChannelIntervalTimerComp>(caster)));
    EXPECT_EQ(100u, tlsEcs.actorRegistry.get<BaseAttributesComp>(target).health());
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<BuffListComp>(target));
}

TEST_F(SkillUtilTest, TriggerSkillEffectRejectsDestroyedSceneBinding) {
    constexpr uint32_t kSkillWithPersistentEffects = 13;
    const auto* skillRow = SkillTableManager::Instance().FindById(kSkillWithPersistentEffects).first;
    ASSERT_NE(nullptr, skillRow);
    ASSERT_GT(skillRow->effect_size(), 0);
    const auto scene = tlsEcs.sceneRegistry.create();
    const auto caster = CreateLoadedPlayerInScene(scene);
    const auto target = CreateLoadedPlayerInScene(scene);
    auto request = TargetedRequest(target);
    request.set_skill_table_id(kSkillWithPersistentEffects);
    ASSERT_EQ(kSuccess, SkillSystem::ReleaseSkill(caster, &request));
    const auto* contexts = tlsEcs.actorRegistry.try_get<SkillContextCompMap>(caster);
    ASSERT_NE(nullptr, contexts);
    ASSERT_EQ(1u, contexts->size());
    const auto skillId = contexts->begin()->first;
    ASSERT_FALSE(tlsEcs.actorRegistry.all_of<BuffListComp>(target));
    tlsEcs.sceneRegistry.destroy(scene);

    SkillSystem::TriggerSkillEffect(caster, skillId);

    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<BuffListComp>(target))
        << "技能效果入口也必须拒绝双方共同持有的已销毁场景句柄";
}

TEST_F(SkillUtilTest, ReleaseAreaSkillWithoutResolvedTargetRejectsDestroyedCasterScene) {
    constexpr uint32_t kAreaGeneralSkill = 2;
    const auto* skillRow = SkillTableManager::Instance().FindById(kAreaGeneralSkill).first;
    ASSERT_NE(nullptr, skillRow);
    ASSERT_TRUE(SkillSystem::IsSkillOfType(kAreaGeneralSkill, kGeneralSkill));
    ASSERT_GT(skillRow->targeting_mode_size(), 0);
    ASSERT_EQ(kAreaOfEffect, 1u << skillRow->targeting_mode(0));
    const auto scene = tlsEcs.sceneRegistry.create();
    const auto caster = CreateLoadedPlayerInScene(scene);
    ::ReleaseSkillRequest request;
    request.set_skill_table_id(kAreaGeneralSkill);
    // 当前目标协议仍要求非零编号；位置技能无需解析到某个 actor。
    request.set_target_id(std::numeric_limits<uint64_t>::max());
    request.mutable_position()->set_x(1.0);
    ASSERT_EQ(entt::null, actor_net_id::Resolve(tlsEcs.actorRegistry, request.target_id()));
    ASSERT_EQ(kSuccess, SkillSystem::ValidateTarget(caster, &request, entt::null));
    tlsEcs.sceneRegistry.destroy(scene);

    EXPECT_NE(kSuccess, SkillSystem::ReleaseSkill(caster, &request));

    EXPECT_FALSE((tlsEcs.actorRegistry.any_of<CooldownTimeListComp, SkillContextCompMap,
        ActorStateComp, CastingTimerComp, RecoveryTimerComp,
        ChannelFinishTimerComp, ChannelIntervalTimerComp>(caster)));
}

// 对自己施放的技能结束时,施法者表里的这一条只能删一次(以前先按 key 删、再用失效迭代器删)。
TEST_F(SkillUtilTest, HandleSkillFinish_SelfTargetedContextIsErasedOnce) {
    const auto caster = CreateSkillActor();
    auto& contexts = tlsEcs.actorRegistry.emplace<SkillContextCompMap>(caster);
    auto context = std::make_shared<SkillContextComp>();
    context->set_skillid(42);
    context->set_caster(entt::to_integral(caster));
    context->set_target(entt::to_integral(caster));
    contexts.emplace(context->skillid(), context);

    SkillSystem::HandleSkillFinish(caster, 42);

    EXPECT_TRUE(tlsEcs.actorRegistry.get<SkillContextCompMap>(caster).empty());
}

// 按需表缺失表示无冷却;只读检查不创建空表。
TEST_F(SkillUtilTest, CheckCooldown_MissingListIsReadyAndCreatesNothing) {
    const entt::entity caster = CreateSkillActor();
    SkillTable tableSkill;
    tableSkill.set_cooldown_id(1);

    EXPECT_EQ(kSuccess, skillUtil->CheckCooldown(caster, &tableSkill));
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<CooldownTimeListComp>(caster));
}

// 修复前 StartCooldown 只取不建,合法首写会被静默丢掉。
TEST_F(SkillUtilTest, StartCooldown_FirstWriteCreatesListAndSubsequentReadUsesIt) {
    const auto caster = CreateSkillActor();
    const auto* cooldownRow = CooldownTableManager::Instance().FindById(1).first;
    ASSERT_NE(nullptr, cooldownRow);
    ASSERT_GT(cooldownRow->duration(), 0);
    SkillTable tableSkill;
    tableSkill.set_cooldown_id(cooldownRow->id());

    SkillSystem::StartCooldown(caster, &tableSkill);

    auto* cooldowns = tlsEcs.actorRegistry.try_get<CooldownTimeListComp>(caster);
    ASSERT_NE(nullptr, cooldowns);
    ASSERT_EQ(1u, cooldowns->cooldown_list().size());
    auto entry = cooldowns->mutable_cooldown_list()->find(cooldownRow->id());
    ASSERT_TRUE(entry != cooldowns->mutable_cooldown_list()->end());
    EXPECT_EQ(cooldownRow->id(), entry->second.cooldown_table_id());
    EXPECT_GT(entry->second.start(), 0u);
    // 将截止判断固定在尚未开始,避免断点或慢机器让真实 500ms 冷却先过期。
    entry->second.set_start(std::numeric_limits<uint64_t>::max());
    EXPECT_EQ(kSkillCooldownNotReady, SkillSystem::CheckCooldown(caster, &tableSkill));
}

TEST_F(SkillUtilTest, DestroyedCasterDoesNotAcquireCooldownOrCastingPhase) {
    const auto caster = CreateSkillActor();
    tlsEcs.actorRegistry.destroy(caster);
    SkillTable tableSkill;
    tableSkill.set_id(1);
    tableSkill.set_cooldown_id(1);

    SkillSystem::StartCooldown(caster, &tableSkill);
    SkillSystem::SetupCastingTimer(caster, &tableSkill, 7);

    EXPECT_EQ(kThisEntityIsInvalid, SkillSystem::CheckCooldown(caster, &tableSkill));
    EXPECT_FALSE((tlsEcs.actorRegistry.any_of<CooldownTimeListComp, CastingTimerComp>(caster)));
}

namespace {
// 走与 ReleaseSkill handler 相同的前置宏,验证拒绝写入与成功字段存在性。
void CheckPlayerRequestForTest(entt::entity player, const ReleaseSkillRequest* request,
    ReleaseSkillResponse* response, uint32_t (*fn)(entt::entity, const ReleaseSkillRequest*)) {
    CHECK_PLAYER_REQUEST(request, fn);
}
} // namespace

TEST(PlayerRequestRejectTest, RejectionIsWrittenToThisResponse) {
    ReleaseSkillRequest request;
    ReleaseSkillResponse response;
    CheckPlayerRequestForTest(entt::null, &request, &response,
        [](entt::entity, const ReleaseSkillRequest*) -> uint32_t { return kSkillCooldownNotReady; });
    ASSERT_TRUE(response.has_error_message());
    EXPECT_EQ(kSkillCooldownNotReady, response.error_message().id());
}

TEST(PlayerRequestRejectTest, SuccessfulRequestCreatesNoErrorAndPreservesOtherResponse) {
    ReleaseSkillRequest request;
    ReleaseSkillResponse rejected;
    CheckPlayerRequestForTest(entt::null, &request, &rejected,
        [](entt::entity, const ReleaseSkillRequest*) -> uint32_t { return kSkillCooldownNotReady; });
    ReleaseSkillResponse accepted;
    CheckPlayerRequestForTest(entt::null, &request, &accepted,
        [](entt::entity, const ReleaseSkillRequest*) -> uint32_t { return kSuccess; });
    EXPECT_FALSE(accepted.has_error_message());
    EXPECT_EQ(kSkillCooldownNotReady, rejected.error_message().id());
}

// 全新 EcsContext 的 actorRegistry 已让 0 号句柄退役(内部 SkillContext/Buff 仍用 0 表示无目标),
// 第一个 actor 不能再是整数 0。用局部对象而不是 tlsEcs,结果不受其它用例执行顺序影响。
TEST(EcsContextTest, FirstActorEntityIntegralIsNotZero) {
    EcsContext fresh;
    const auto first = fresh.actorRegistry.create();
    EXPECT_NE(0u, entt::to_integral(first));
    EXPECT_FALSE(fresh.actorRegistry.valid(entt::entity{ 0 }));
}

TEST_F(SkillUtilTest, CheckCooldown_CooldownActive_ReturnsError) {
    entt::entity caster = CreateSkillActor();

    auto tableSkill = std::make_shared<SkillTable>();
    tableSkill->set_cooldown_id(1);

    CooldownTimeComp cooldownTimeComp;
    cooldownTimeComp.set_cooldown_table_id(1);

    EXPECT_CALL(*mockCooldownTimeUtil, IsInCooldown(_))
        .WillRepeatedly(Return(true));

    auto & cooldownList = tlsEcs.actorRegistry.get_or_emplace<CooldownTimeListComp>(caster);
    cooldownTimeComp.set_cooldown_table_id(1);
    CoolDownTimeMillisecondSystem::Reset(cooldownTimeComp);
    cooldownList.mutable_cooldown_list()->emplace(1, cooldownTimeComp);

    uint32_t result = skillUtil->CheckCooldown(caster, tableSkill.get());
    EXPECT_EQ(result, kSkillCooldownNotReady);
}

TEST_F(SkillUtilTest, CheckCooldown_CooldownInactive_ReturnsOk) {
    entt::entity caster = CreateSkillActor();

    auto tableSkill = std::make_shared<SkillTable>();
    tableSkill->set_cooldown_id(1);

    CooldownTimeComp cooldownTimeComp;
    cooldownTimeComp.set_cooldown_table_id(1);

    EXPECT_CALL(*mockCooldownTimeUtil, IsInCooldown(_))
        .WillRepeatedly(Return(false));

    auto& cooldownList = tlsEcs.actorRegistry.get_or_emplace<CooldownTimeListComp>(caster);
    cooldownList.mutable_cooldown_list()->emplace(1, cooldownTimeComp);

    uint32_t result = skillUtil->CheckCooldown(caster, tableSkill.get());
    EXPECT_EQ(result, kSuccess);
}

TEST_F(SkillUtilTest, HandleCastingTimer_ImmediateSkill_ReturnsOk) {
    entt::entity caster = CreateSkillActor();
    auto tableSkill = std::make_shared<SkillTable>();
    tableSkill->set_immediate(true);
    tableSkill->set_cast_point(1000); // Set cast point to 1000ms

    EXPECT_CALL(*mockSkillTable, GetSkillTable(_))
        .WillRepeatedly(Return(tableSkill.get()));

    uint32_t result = skillUtil->CheckCasting(caster, tableSkill.get());
    EXPECT_EQ(result, kSuccess);
}

TEST_F(SkillUtilTest, HandleRecoveryTimeTimer_ImmediateSkill_ReturnsOk) {
    entt::entity caster = CreateSkillActor();
    auto tableSkill = std::make_shared<SkillTable>();
    tableSkill->set_immediate(true);
    tableSkill->set_recovery_time(1000); // Set recovery time to 1000ms

    EXPECT_CALL(*mockSkillTable, GetSkillTable(_))
        .WillRepeatedly(Return(tableSkill.get()));

    uint32_t result = skillUtil->CheckRecovery(caster, tableSkill.get());
    EXPECT_EQ(result, kSuccess);
}

TEST_F(SkillUtilTest, HandleChannelTimeTimer_ImmediateSkill_ReturnsOk) {
    entt::entity caster = CreateSkillActor();
    auto tableSkill = std::make_shared<SkillTable>();
    tableSkill->set_immediate(true);
    tableSkill->set_channel_finish(1000); // Set channel finish time to 1000ms
    tableSkill->set_channel_think(500);   // Set channel interval to 500ms

    EXPECT_CALL(*mockSkillTable, GetSkillTable(_))
        .WillRepeatedly(Return(tableSkill.get()));

    uint32_t result = skillUtil->CheckChannel(caster, tableSkill.get());
    EXPECT_EQ(result, kSuccess);
}

TEST_F(SkillUtilTest, BroadcastSkillUsedMessage_CreatesMessage) {
    entt::entity caster = CreateSkillActor();
    ::ReleaseSkillRequest request;
    request.set_skill_table_id(1);
    request.set_target_id(2);
    request.mutable_position()->set_x(10); // Mock position

    EXPECT_CALL(*mockSkillTable, GetSkillTable(request.skill_table_id()))
        .WillRepeatedly(Return(nullptr)); // Mock implementation

    EXPECT_NO_THROW(skillUtil->BroadcastSkillUsedMessage(caster, &request, entt::null));
}

TEST_F(SkillUtilTest, SetupCastingTimer_SetsTimer) {
    entt::entity caster = CreateSkillActor();
    auto tableSkill = std::make_shared<SkillTable>();
    tableSkill->set_id(1);
    tableSkill->set_cast_point(1); // Set cast point to 1000ms

    EXPECT_CALL(*mockSkillTable, GetSkillTable(_))
        .WillRepeatedly(Return(tableSkill.get()));

    EXPECT_CALL(*mockSkillTable, IsSkillOfType(_, kGeneralSkill))
        .WillRepeatedly(Return(true));

    skillUtil->SetupCastingTimer(caster, tableSkill.get(), 2);

    auto* castingTimerComp = tlsEcs.actorRegistry.try_get<CastingTimerComp>(caster);
    ASSERT_NE(castingTimerComp, nullptr);  // Check if the component was created
    EXPECT_TRUE(castingTimerComp->timer.IsActive());  // Check if the timer is active
}

TEST_F(SkillUtilTest, HandleSkillSpell_TriggersEffect) {
    entt::entity caster = CreateSkillActor();


    auto tableSkill = std::make_shared<SkillTable>();
    tableSkill->set_id(1);

    EXPECT_CALL(*mockSkillTable, GetSkillTable(1))
        .WillRepeatedly(Return(tableSkill.get()));

    EXPECT_NO_THROW(skillUtil->HandleGeneralSkillSpell(caster, 1));
}

TEST_F(SkillUtilTest, HandleSkillRecovery_SetsRecoveryTimer) {
    entt::entity caster = CreateSkillActor();


    auto tableSkill = std::make_shared<SkillTable>();
    tableSkill->set_recovery_time(1000); // Set recovery time to 1000ms

    EXPECT_CALL(*mockSkillTable, GetSkillTable(_))
        .WillRepeatedly(Return(tableSkill.get()));

    EXPECT_NO_THROW(skillUtil->HandleSkillRecovery(caster, 1));
}

TEST_F(SkillUtilTest, SendSkillInterruptedMessage_SendsMessage) {
    entt::entity caster = CreateSkillActor();
    uint32_t skillId = 1;

    EXPECT_NO_THROW(skillUtil->SendSkillInterruptedMessage(caster, skillId));
}

// 实际释放一次技能,不能由夹具先挂表来掩盖生产首写入口缺失。
TEST_F(SkillUtilTest, ReleaseSkillDoesNotTouchTargetContexts) {
    const auto scene = tlsEcs.sceneRegistry.create();
    const auto caster = CreateLoadedPlayerInScene(scene);
    const auto target = CreateLoadedPlayerInScene(scene);
    ASSERT_FALSE((tlsEcs.actorRegistry.any_of<CooldownTimeListComp, SkillContextCompMap, ActorStateComp>(caster)));
    ASSERT_FALSE(tlsEcs.actorRegistry.all_of<SkillContextCompMap>(target));

    auto request = TargetedRequest(target);
    ASSERT_EQ(kSuccess, SkillSystem::ReleaseSkill(caster, &request));

    const auto* casterContexts = tlsEcs.actorRegistry.try_get<SkillContextCompMap>(caster);
    ASSERT_NE(nullptr, casterContexts);
    ASSERT_EQ(1u, casterContexts->size());
    const auto skillId = casterContexts->begin()->first;
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<SkillContextCompMap>(target));
    EXPECT_TRUE(tlsEcs.actorRegistry.all_of<CooldownTimeListComp>(caster));

    SkillSystem::HandleSkillFinish(caster, skillId);
    EXPECT_TRUE(tlsEcs.actorRegistry.get<SkillContextCompMap>(caster).empty());
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<SkillContextCompMap>(target));
}

TEST_F(SkillUtilTest, CasterDestroyedMidCastLeavesNoResidueOnTarget) {
    const auto scene = tlsEcs.sceneRegistry.create();
    const auto caster = CreateLoadedPlayerInScene(scene);
    const auto target = CreateLoadedPlayerInScene(scene);
    auto request = TargetedRequest(target);
    ASSERT_EQ(kSuccess, SkillSystem::ReleaseSkill(caster, &request));
    ASSERT_TRUE(tlsEcs.actorRegistry.all_of<CastingTimerComp>(caster));
    ASSERT_TRUE(tlsEcs.actorRegistry.get<CastingTimerComp>(caster).timer.IsActive());
    const auto skillId = tlsEcs.actorRegistry.get<SkillContextCompMap>(caster).begin()->first;
    tlsEcs.actorRegistry.destroy(caster);
    SkillSystem::HandleSkillFinish(caster, skillId);
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<SkillContextCompMap>(target));
}

TEST_F(SkillUtilTest, ReleasingAtAnotherCasterDoesNotAlterItsOwnContexts) {
    const auto scene = tlsEcs.sceneRegistry.create();
    const auto caster = CreateLoadedPlayerInScene(scene);
    const auto target = CreateLoadedPlayerInScene(scene);
    auto ownContext = std::make_shared<SkillContextComp>();
    ownContext->set_skillid(77);
    ownContext->set_caster(entt::to_integral(target));
    auto& targetContexts = tlsEcs.actorRegistry.emplace<SkillContextCompMap>(target);
    targetContexts.emplace(77, ownContext);
    auto request = TargetedRequest(target);
    ASSERT_EQ(kSuccess, SkillSystem::ReleaseSkill(caster, &request));
    const auto skillId = tlsEcs.actorRegistry.get<SkillContextCompMap>(caster).begin()->first;
    SkillSystem::HandleSkillFinish(caster, skillId);
    const auto* remaining = tlsEcs.actorRegistry.try_get<SkillContextCompMap>(target);
    ASSERT_NE(nullptr, remaining);
    ASSERT_EQ(1u, remaining->size());
    ASSERT_TRUE(remaining->contains(77));
    EXPECT_EQ(ownContext, remaining->at(77));
}

TEST_F(SkillUtilTest, RejectedTargetDoesNotCreateAnyOnDemandSkillState) {
    const auto scene = tlsEcs.sceneRegistry.create();
    const auto otherScene = tlsEcs.sceneRegistry.create();
    const auto caster = CreateLoadedPlayerInScene(scene);
    const auto target = CreateLoadedPlayerInScene(otherScene);
    auto request = TargetedRequest(target);

    EXPECT_EQ(kSkillInvalidTargetId, SkillSystem::ReleaseSkill(caster, &request));
    EXPECT_FALSE((tlsEcs.actorRegistry.any_of<CooldownTimeListComp, SkillContextCompMap, ActorStateComp,
        CastingTimerComp, RecoveryTimerComp, ChannelFinishTimerComp, ChannelIntervalTimerComp>(caster)));
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<SkillContextCompMap>(target));
}

TEST_F(SkillUtilTest, FrozenCasterDoesNotCreateAnyOnDemandSkillState) {
    const auto scene = tlsEcs.sceneRegistry.create();
    const auto caster = CreateLoadedPlayerInScene(scene);
    const auto target = CreateLoadedPlayerInScene(scene);
    tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(caster);
    auto request = TargetedRequest(target);

    EXPECT_EQ(kSkillCannotBeCastInCurrentState, SkillSystem::ReleaseSkill(caster, &request));
    EXPECT_FALSE((tlsEcs.actorRegistry.any_of<CooldownTimeListComp, SkillContextCompMap, ActorStateComp>(caster)));
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<SkillContextCompMap>(target));
}

TEST_F(SkillUtilTest, DestroyedCasterReleaseDoesNotCreateContextOnTarget) {
    const auto scene = tlsEcs.sceneRegistry.create();
    const auto caster = CreateLoadedPlayerInScene(scene);
    const auto target = CreateLoadedPlayerInScene(scene);
    tlsEcs.actorRegistry.destroy(caster);
    auto request = TargetedRequest(target);

    EXPECT_EQ(kThisEntityIsInvalid, SkillSystem::ReleaseSkill(caster, &request));
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<SkillContextCompMap>(target));
    EXPECT_FALSE((tlsEcs.actorRegistry.any_of<CooldownTimeListComp, SkillContextCompMap, ActorStateComp>(caster)));
}

TEST_F(SkillUtilTest, HandleSkillFinish_MissingContextMapCreatesNothing) {
    const auto caster = CreateSkillActor();
    SkillSystem::HandleSkillFinish(caster, 8);
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<SkillContextCompMap>(caster));
}

// 真正必需的持久/派生组件仍在打断与状态写入前拒绝,不能因允许按需表缺失而放宽。
TEST_F(SkillUtilTest, CheckSkillPrerequisites_MissingBaseAttributes_RejectsBeforeSideEffects) {
    const auto* immediateRow = SkillTableManager::Instance().FindById(1).first;
    ASSERT_NE(nullptr, immediateRow);
    ASSERT_TRUE(immediateRow->immediate()) << "夹具前提:1 号技能是立即技能(会打断在途相位)";

    const auto scene = tlsEcs.sceneRegistry.create();
    const auto caster = CreateLoadedPlayerInScene(scene);
    const auto target = CreatePlayerInScene(scene);
    tlsEcs.actorRegistry.remove<BaseAttributesComp>(caster);

    auto& recovery = tlsEcs.actorRegistry.emplace<RecoveryTimerComp>(caster);
    recovery.skillId = 7;
    recovery.timer.RunAfter(3600.0, [] {});
    ASSERT_TRUE(recovery.timer.IsActive());

    auto request = TargetedRequest(target);
    EXPECT_EQ(kServiceUnavailable, SkillSystem::CheckSkillPrerequisites(caster, &request, target));

    const auto* stillRecovering = tlsEcs.actorRegistry.try_get<RecoveryTimerComp>(caster);
    ASSERT_NE(nullptr, stillRecovering) << "缺持久组件时不应先打断在途后摇";
    EXPECT_TRUE(stillRecovering->timer.IsActive());
    EXPECT_FALSE((tlsEcs.actorRegistry.any_of<CooldownTimeListComp, SkillContextCompMap, ActorStateComp>(caster)));
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<BaseAttributesComp>(caster));
}

TEST_F(SkillUtilTest, CheckSkillPrerequisites_MissingPlayerDerivedAttributes_RejectsWithoutCreatingThem) {
    const auto scene = tlsEcs.sceneRegistry.create();
    const auto caster = CreateLoadedPlayerInScene(scene);
    const auto target = CreatePlayerInScene(scene);
    tlsEcs.actorRegistry.remove<DerivedAttributesComp>(caster);
    auto request = TargetedRequest(target);

    EXPECT_EQ(kServiceUnavailable, SkillSystem::CheckSkillPrerequisites(caster, &request, target));
    EXPECT_FALSE((tlsEcs.actorRegistry.any_of<DerivedAttributesComp, CooldownTimeListComp,
        SkillContextCompMap, ActorStateComp>(caster)));
}

TEST_F(SkillUtilTest, DelayedImpactMissingBaseAttributesDoesNotDealBonusDamageOrConsumeBuff) {
    ExpectDelayedCastWithMissingDataHasNoDamageOrBuffSideEffects<BaseAttributesComp>();
}

TEST_F(SkillUtilTest, DelayedImpactMissingLevelDoesNotDealBonusDamageOrConsumeBuff) {
    ExpectDelayedCastWithMissingDataHasNoDamageOrBuffSideEffects<LevelComp>();
}

TEST_F(SkillUtilTest, DelayedImpactMissingDerivedAttributesDoesNotDealBonusDamageOrConsumeBuff) {
    ExpectDelayedCastWithMissingDataHasNoDamageOrBuffSideEffects<DerivedAttributesComp>();
}

TEST_F(SkillUtilTest, ImpactOnTargetMissingDerivedAttributesDoesNotDamageOrConsumeBonusBuff) {
    const auto scene = tlsEcs.sceneRegistry.create();
    const auto caster = CreateLoadedPlayerInScene(scene);
    const auto target = CreateLoadedPlayerInScene(scene);
    auto request = TargetedRequest(target);
    ASSERT_EQ(kSuccess, SkillSystem::ReleaseSkill(caster, &request));
    const auto skillId = tlsEcs.actorRegistry.get<SkillContextCompMap>(caster).begin()->first;
    const auto [buffError, bonusBuffId] = BuffSystem::AddOrUpdateBuff(caster, 19);
    ASSERT_EQ(kSuccess, buffError);
    ASSERT_NE(UINT64_MAX, bonusBuffId);
    tlsEcs.actorRegistry.remove<DerivedAttributesComp>(target);

    SkillSystem::HandleSkillSpell(caster, skillId);

    EXPECT_EQ(100u, tlsEcs.actorRegistry.get<BaseAttributesComp>(target).health());
    EXPECT_TRUE(tlsEcs.actorRegistry.get<BuffListComp>(caster).contains(bonusBuffId));
    EXPECT_FALSE(tlsEcs.actorRegistry.get<SkillContextCompMap>(caster).contains(skillId));
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<SkillContextCompMap>(target));
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<CastingTimerComp>(caster));
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<DerivedAttributesComp>(target));
}

TEST_F(SkillUtilTest, ChannelTickMissingRequiredDataEndsChannelWithoutConsumingBonusBuff) {
    const auto scene = tlsEcs.sceneRegistry.create();
    const auto caster = CreateLoadedPlayerInScene(scene);
    const auto target = CreateLoadedPlayerInScene(scene);
    tlsEcs.actorRegistry.get<BaseAttributesComp>(target).set_health(10000);
    auto request = TargetedRequest(target);
    request.set_skill_table_id(3); // 真表引导技能,前摇0、引导结束与think间隔均1秒。
    ASSERT_EQ(kSuccess, SkillSystem::ReleaseSkill(caster, &request));
    const auto skillId = tlsEcs.actorRegistry.get<SkillContextCompMap>(caster).begin()->first;
    ASSERT_TRUE(RunUntil([caster = caster] {
        return tlsEcs.actorRegistry.all_of<ChannelFinishTimerComp>(caster);
    }));
    ASSERT_TRUE(tlsEcs.actorRegistry.all_of<ChannelIntervalTimerComp>(caster));
    const auto healthBefore = tlsEcs.actorRegistry.get<BaseAttributesComp>(target).health();
    const auto [buffError, bonusBuffId] = BuffSystem::AddOrUpdateBuff(caster, 19);
    ASSERT_EQ(kSuccess, buffError);
    ASSERT_NE(UINT64_MAX, bonusBuffId);
    tlsEcs.actorRegistry.remove<BaseAttributesComp>(caster);

    SkillSystem::HandleChannelThink(caster, skillId);

    EXPECT_EQ(healthBefore, tlsEcs.actorRegistry.get<BaseAttributesComp>(target).health());
    EXPECT_TRUE(tlsEcs.actorRegistry.get<BuffListComp>(caster).contains(bonusBuffId));
    EXPECT_FALSE(tlsEcs.actorRegistry.get<SkillContextCompMap>(caster).contains(skillId));
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<SkillContextCompMap>(target));
    EXPECT_FALSE((tlsEcs.actorRegistry.any_of<ChannelFinishTimerComp, ChannelIntervalTimerComp>(caster)));
}

// 立即技能打断引导:这次施法的整个引导相位都结束,周期 think 定时器也一起摘掉。
// 修复前只摘被检查的 ChannelFinishTimerComp,ChannelIntervalTimerComp 的 RunEvery 一直跑下去(F051)。
TEST_F(SkillUtilTest, CheckChannel_ImmediateInterrupt_EndsWholeChannelPhase) {
    const auto caster = CreateSkillActor();

    auto& channelFinish = tlsEcs.actorRegistry.emplace<ChannelFinishTimerComp>(caster);
    channelFinish.skillId = 9;
    channelFinish.timer.RunAfter(3600.0, [] {});
    tlsEcs.actorRegistry.emplace<ChannelIntervalTimerComp>(caster).timer.RunEvery(3600.0, [] {});

    SkillTable immediateSkill;
    immediateSkill.set_id(1);
    immediateSkill.set_immediate(true);

    EXPECT_EQ(kSuccess, SkillSystem::CheckChannel(caster, &immediateSkill));
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<ChannelFinishTimerComp>(caster));
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<ChannelIntervalTimerComp>(caster))
        << "引导被打断后周期 think 定时器还挂着";
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<SkillContextCompMap>(caster))
        << "没有上下文的相位清理不得补建空表";
}

// 相位组件还挂着、定时器却已不在跑(开始时没挂上定时器,或结束时漏摘):它的结束回调永远不会来,
// CheckTimerPhase 要按「这次施法结束」收尾 —— 收掉它的上下文、摘掉全部相位,然后放行。
// 修复前只摘被检查的那一个组件:残留的是引导结束定时器时,周期 think 定时器继续跑;
// 这次施法的上下文也没人删,一直留在上下文表里。
TEST_F(SkillUtilTest, CheckChannel_StalePhase_FinishesItsCastAndEndsAllPhases) {
    constexpr uint64_t kStaleSkillId = 11;
    const auto caster = CreateSkillActor();
    {
        auto context = std::make_shared<SkillContextComp>();
        context->set_skillid(kStaleSkillId);
        context->set_caster(entt::to_integral(caster));
        tlsEcs.actorRegistry.emplace<SkillContextCompMap>(caster).emplace(kStaleSkillId, context);
    }

    // 结束定时器从没武装:组件在,定时器不在跑。
    tlsEcs.actorRegistry.emplace<ChannelFinishTimerComp>(caster).skillId = kStaleSkillId;
    ASSERT_FALSE(tlsEcs.actorRegistry.get<ChannelFinishTimerComp>(caster).timer.IsActive());
    tlsEcs.actorRegistry.emplace<ChannelIntervalTimerComp>(caster).timer.RunEvery(3600.0, [] {});

    SkillTable nonImmediateSkill;
    nonImmediateSkill.set_id(1);
    nonImmediateSkill.set_immediate(false);

    EXPECT_EQ(kSuccess, SkillSystem::CheckChannel(caster, &nonImmediateSkill));
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<ChannelFinishTimerComp>(caster));
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<ChannelIntervalTimerComp>(caster))
        << "残留的引导被收尾后周期 think 定时器还挂着";
    EXPECT_FALSE(tlsEcs.actorRegistry.get<SkillContextCompMap>(caster).contains(kStaleSkillId))
        << "残留相位那次施法的上下文没人收";
}

// 引导正常结束:结束定时器与周期 think 定时器都随相位摘掉。
// 修复前 HandleChannelFinish 只摘 think 定时器,ChannelFinishTimerComp 留到下一次施法的 CheckChannel(F051)。
TEST_F(SkillUtilTest, HandleChannelFinish_EndsChannelPhase) {
    const auto caster = CreateSkillActor();

    auto& channelFinish = tlsEcs.actorRegistry.emplace<ChannelFinishTimerComp>(caster);
    channelFinish.skillId = 9;
    channelFinish.timer.RunAfter(3600.0, [] {});
    tlsEcs.actorRegistry.emplace<ChannelIntervalTimerComp>(caster).timer.RunEvery(3600.0, [] {});

    SkillSystem::HandleChannelFinish(caster, 9);

    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<ChannelFinishTimerComp>(caster));
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<ChannelIntervalTimerComp>(caster));
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<RecoveryTimerComp>(caster));
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<SkillContextCompMap>(caster));
}

// 没有前摇相位的技能(这里用配表里不存在的技能 id,既不是普通技能也不是引导技能)不挂相位组件。
// 修复前 SetupCastingTimer 先 get_or_emplace 一个空的 CastingTimerComp,留给下一次施法去摘(F051)。
TEST_F(SkillUtilTest, SetupCastingTimer_NoCastPhaseSkill_DoesNotStartPhase) {
    constexpr uint32_t kUnknownSkillTableId = 4000000000u;
    ASSERT_FALSE(SkillTableManager::Instance().Exists(kUnknownSkillTableId));

    const auto caster = CreateSkillActor();
    SkillTable noCastPhaseSkill;
    noCastPhaseSkill.set_id(kUnknownSkillTableId);
    noCastPhaseSkill.set_cast_point(1);

    SkillSystem::SetupCastingTimer(caster, &noCastPhaseSkill, 3);

    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<CastingTimerComp>(caster));
}

// 一次普通技能完整走完「前摇 → 施放 → 后摇」:每个相位结束时它的定时器组件随之摘掉,最后上下文出表。
// 修复前相位组件只在下一次施法的 CheckTimerPhase 里才摘(F051):技能早已结束,
// CastingTimerComp / RecoveryTimerComp 还挂在施法者身上。
//
// 由真实 EventLoop 驱动定时器:2 号技能前摇、后摇都是 0 秒。轮询定时器在「上下文出表」后退出,
// 另有 5 秒兜底;不按固定时长等待,结果与机器快慢无关。
TEST_F(SkillUtilTest, GeneralSkill_PhaseComponentsEndWithTheirPhase) {
    constexpr uint32_t kInstantGeneralSkill = 2;
    constexpr uint64_t kSkillInstanceId = 77;
    const auto* skillRow = SkillTableManager::Instance().FindById(kInstantGeneralSkill).first;
    ASSERT_NE(nullptr, skillRow);
    ASSERT_TRUE(SkillSystem::IsSkillOfType(kInstantGeneralSkill, kGeneralSkill)) << "夹具前提:2 号是普通技能";
    // 配表一旦改成非 0(单位秒),用例会拖到 5 秒兜底才红,报错还不指向配表 —— 先把前提钉死。
    ASSERT_EQ(0.0, skillRow->cast_point()) << "夹具前提:2 号技能前摇为 0(generated/tables/skill.json)";
    ASSERT_EQ(0.0, skillRow->recovery_time()) << "夹具前提:2 号技能后摇为 0(generated/tables/skill.json)";

    const auto scene = tlsEcs.sceneRegistry.create();
    const auto caster = CreateLoadedPlayerInScene(scene);
    {
        auto context = std::make_shared<SkillContextComp>();
        context->set_skillid(kSkillInstanceId);
        context->set_skilltableid(kInstantGeneralSkill);
        context->set_caster(entt::to_integral(caster));
        tlsEcs.actorRegistry.emplace<SkillContextCompMap>(caster).emplace(kSkillInstanceId, context);
    }

    SkillSystem::SetupCastingTimer(caster, skillRow, kSkillInstanceId);
    ASSERT_TRUE(tlsEcs.actorRegistry.all_of<CastingTimerComp>(caster));

    const auto skillFinished = [caster = caster, skillId = kSkillInstanceId] {
        const auto* contexts = tlsEcs.actorRegistry.try_get<SkillContextCompMap>(caster);
        return contexts != nullptr && !contexts->contains(skillId);
    };

    auto* loop = EventLoop::getEventLoopOfCurrentThread();
    ASSERT_NE(nullptr, loop);
    const TimerId poll = loop->runEvery(0.01, [loop, skillFinished] {
        if (skillFinished()) {
            loop->quit();
        }
    });
    const TimerId safetyNet = loop->runAfter(5.0, [loop] { loop->quit(); });
    loop->loop();
    loop->cancel(poll);
    loop->cancel(safetyNet);

    ASSERT_TRUE(skillFinished()) << "5 秒内技能没走完:前摇 / 后摇定时器没有按预期触发";
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<CastingTimerComp>(caster)) << "前摇结束后相位组件仍挂着";
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<RecoveryTimerComp>(caster)) << "后摇结束后相位组件仍挂着";
}

TEST_F(SkillUtilTest, GeneralSkillOnDeadTarget_StillEntersRecovery) {
    constexpr uint64_t kSkillInstanceId = 91;
    const auto* skillRow = SkillTableManager::Instance().FindById(1).first;
    ASSERT_NE(nullptr, skillRow);
    ASSERT_TRUE(SkillSystem::IsSkillOfType(1, kGeneralSkill));
    ASSERT_GT(skillRow->recovery_time(), 0.0);

    const auto scene = tlsEcs.sceneRegistry.create();
    const auto caster = CreateLoadedPlayerInScene(scene);
    const auto target = CreateLoadedPlayerInScene(scene);
    tlsEcs.actorRegistry.get<BaseAttributesComp>(target).set_health(0);
    {
        auto context = std::make_shared<SkillContextComp>();
        context->set_skillid(kSkillInstanceId);
        context->set_skilltableid(1);
        context->set_caster(entt::to_integral(caster));
        context->set_target(entt::to_integral(target));
        tlsEcs.actorRegistry.emplace<SkillContextCompMap>(caster).emplace(kSkillInstanceId, context);
    }

    // 尸体命中只应落成零伤害，不能连同施法效果与后摇一起中止。
    SkillSystem::HandleGeneralSkillSpell(caster, kSkillInstanceId);

    const auto* recovery = tlsEcs.actorRegistry.try_get<RecoveryTimerComp>(caster);
    EXPECT_NE(nullptr, recovery);
    if (recovery != nullptr) {
        EXPECT_EQ(kSkillInstanceId, recovery->skillId);
    }
    const auto* contexts = tlsEcs.actorRegistry.try_get<SkillContextCompMap>(caster);
    ASSERT_NE(nullptr, contexts);
    EXPECT_TRUE(contexts->contains(kSkillInstanceId));
    EXPECT_EQ(0u, tlsEcs.actorRegistry.get<BaseAttributesComp>(target).health());
}

TEST_F(SkillUtilTest, ReleaseNoCastPhaseSkillLeavesNoContext) {
    const auto* skillRow = SkillTableManager::Instance().FindById(4).first;
    ASSERT_NE(nullptr, skillRow);
    ASSERT_FALSE(SkillSystem::IsSkillOfType(4, kGeneralSkill) ||
        SkillSystem::IsSkillOfType(4, kChannelSkill));

    const auto scene = tlsEcs.sceneRegistry.create();
    const auto caster = CreateLoadedPlayerInScene(scene);
    const auto target = CreateLoadedPlayerInScene(scene);
    auto request = TargetedRequest(target);
    request.set_skill_table_id(4);

    ASSERT_EQ(kSuccess, SkillSystem::ReleaseSkill(caster, &request));

    EXPECT_FALSE((tlsEcs.actorRegistry.any_of<SkillContextCompMap, CastingTimerComp>(caster)));
}

TEST_F(SkillUtilTest, StartCooldown_NoCooldownSkillCreatesNothing) {
    const auto* noCooldownRow = SkillTableManager::Instance().FindById(2).first;
    const auto* cooldownRow = SkillTableManager::Instance().FindById(1).first;
    ASSERT_NE(nullptr, noCooldownRow);
    ASSERT_NE(nullptr, cooldownRow);
    ASSERT_EQ(0u, noCooldownRow->cooldown_id());
    ASSERT_NE(0u, cooldownRow->cooldown_id());

    const auto caster = CreateSkillActor();
    SkillSystem::StartCooldown(caster, noCooldownRow);
    EXPECT_FALSE(tlsEcs.actorRegistry.all_of<CooldownTimeListComp>(caster));

    const auto casterWithCooldown = CreateSkillActor();
    SkillSystem::StartCooldown(casterWithCooldown, cooldownRow);
    SkillSystem::StartCooldown(casterWithCooldown, noCooldownRow);
    const auto* cooldowns = tlsEcs.actorRegistry.try_get<CooldownTimeListComp>(casterWithCooldown);
    ASSERT_NE(nullptr, cooldowns);
    EXPECT_FALSE(cooldowns->cooldown_list().contains(0));
}

TEST_F(SkillUtilTest, FinishedCastIsNotRetainedByTargetBuffs) {
    const auto* skillRow = SkillTableManager::Instance().FindById(13).first;
    ASSERT_NE(nullptr, skillRow);
    ASSERT_TRUE(SkillSystem::IsSkillOfType(13, kGeneralSkill));
    ASSERT_GT(skillRow->cast_point(), 0.0);
    for (const uint32_t buffTableId : {19u, 20u}) {
        const auto* buffRow = BuffTableManager::Instance().FindById(buffTableId).first;
        ASSERT_NE(nullptr, buffRow);
        ASSERT_GT(buffRow->duration(), 0.0);
    }

    const auto scene = tlsEcs.sceneRegistry.create();
    const auto caster = CreateLoadedPlayerInScene(scene);
    const auto target = CreateLoadedPlayerInScene(scene);
    auto request = TargetedRequest(target);
    request.set_skill_table_id(13);
    ASSERT_EQ(kSuccess, SkillSystem::ReleaseSkill(caster, &request));

    uint64_t skillId = 0;
    std::weak_ptr<SkillContextComp> observer;
    {
        const auto* contexts = tlsEcs.actorRegistry.try_get<SkillContextCompMap>(caster);
        ASSERT_NE(nullptr, contexts);
        ASSERT_EQ(1u, contexts->size());
        skillId = contexts->begin()->first;
        observer = contexts->at(skillId);
    }
    // 技能13的真实持续 Buff 跨过施法结束，能检出目标偷持施法者上下文的强引用。
    ASSERT_TRUE(RunUntil([caster = caster, skillId = skillId] {
        const auto* contexts = tlsEcs.actorRegistry.try_get<SkillContextCompMap>(caster);
        return contexts == nullptr || !contexts->contains(skillId);
    }));

    const auto* buffs = tlsEcs.actorRegistry.try_get<BuffListComp>(target);
    ASSERT_NE(nullptr, buffs);
    ASSERT_FALSE(buffs->empty());
    bool hasLongLivedEffect = false;
    for (const auto& [buffId, buff] : *buffs) {
        if (buff.buffPb.buff_table_id() == 19 || buff.buffPb.buff_table_id() == 20) {
            hasLongLivedEffect = true;
        }
    }
    ASSERT_TRUE(hasLongLivedEffect);
    EXPECT_TRUE(observer.expired());
}

TEST(PlayerSkillSystemTest, RegisterPlayerDoesNotCreateMissingSkillList)
{
    const auto player = tlsEcs.actorRegistry.create();

    PlayerSkillSystem::RegisterPlayer(player);

    EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerSkillListComp>(player));
    tlsEcs.actorRegistry.destroy(player);
}

// 本版本技能表查不到的技能(1001,例如新版本才加的技能,回滚后旧表里没有)保留在列表里、存盘原样写回,
// 但不生效:HasSkill 答"没有"。0 与重复项照旧去掉(docs/design/mixed-version-persist-field-protection.md §12,第 5 批)。
TEST(PlayerSkillSystemTest, SanitizeSkillListKeepsUnknownSkillsInactiveAndRemovesDuplicates)
{
    entt::entity player = CreateSkillActor();
    auto &skillList = tlsEcs.actorRegistry.get_or_emplace<PlayerSkillListComp>(player);
    ASSERT_TRUE(SkillTableManager::Instance().Exists(1));
    ASSERT_FALSE(SkillTableManager::Instance().Exists(1001)) << "fixture: 1001 must be missing from SkillTable";

    skillList.add_skill_list()->set_skill_table_id(1);
    auto* unknownSkill = skillList.add_skill_list();
    unknownSkill->set_skill_table_id(1001);
    unknownSkill->set_rank(37); // 旧表未知的职业技能等级也不能归零
    unknownSkill->mutable_unknown_fields()->AddVarint(412346, 9);
    const std::string unknownBefore = unknownSkill->SerializeAsString();
    skillList.add_skill_list()->set_skill_table_id(0);
    skillList.add_skill_list()->set_skill_table_id(1);
    skillList.add_skill_list()->set_skill_table_id(1001);
    skillList.mutable_unknown_fields()->AddVarint(412345, 5); // 新版本给组件本层加的字段

    PlayerSkillSystem::SanitizeSkillList(player);

    ASSERT_EQ(1, skillList.unknown_fields().field_count()) << "整体替换时组件本层未知字段并回(设计 §4.2 L5)";
    EXPECT_EQ(412345, skillList.unknown_fields().field(0).number());
    ASSERT_EQ(skillList.skill_list_size(), 2) << "only 0 and duplicates are removed";
    EXPECT_EQ(skillList.skill_list(0).skill_table_id(), 1);
    EXPECT_EQ(skillList.skill_list(1).skill_table_id(), 1001) << "unknown skill is kept, not deleted";
    EXPECT_EQ(unknownBefore, skillList.skill_list(1).SerializeAsString());
    EXPECT_TRUE(PlayerSkillSystem::HasSkill(player, 1));
    EXPECT_FALSE(PlayerSkillSystem::HasSkill(player, 1001)) << "kept but inactive";
    EXPECT_TRUE(PlayerSkillSystem::IsActiveSkill(1));
    EXPECT_FALSE(PlayerSkillSystem::IsActiveSkill(1001));
    EXPECT_FALSE(PlayerSkillSystem::IsActiveSkill(0));
}

// 列表里没有 0 与重复项时不整体替换(查不到表的技能也不算要改):组件本层未知字段与逐条技能都原样不动。
TEST(PlayerSkillSystemTest, SanitizeSkillListLeavesACleanListUntouched)
{
    entt::entity player = CreateSkillActor();
    auto &skillList = tlsEcs.actorRegistry.get_or_emplace<PlayerSkillListComp>(player);
    skillList.add_skill_list()->set_skill_table_id(1);
    skillList.add_skill_list()->set_skill_table_id(1001); // 查不到表:保留,不算"要改"
    skillList.mutable_unknown_fields()->AddVarint(412345, 3);
    const std::string before = skillList.SerializeAsString();

    PlayerSkillSystem::SanitizeSkillList(player);

    EXPECT_EQ(before, skillList.SerializeAsString());
}

// 本版本解释不了的取值一律原样携带(docs/design/mixed-version-persist-field-protection.md §12.1 D5 / D6):
// 超过本版本 max_rank 的职业技能重数、rank 0 的职业技能、写着 rank 的旧通用技能、查不到表的技能,
// 连同条目与组件本层的未知字段,Sanitize 前后同一实例的序列化逐字节相同 —— 加载期不改写,存盘写回的就是原值。
// 修复前:重数压回 max_rank、rank 0 的职业技能整条删掉、旧通用技能 rank 清 0,并整体替换组件。
TEST(PlayerSkillSystemTest, SanitizeSkillListCarriesOutOfRangeRanksVerbatim)
{
    constexpr uint32_t kClassActive = 1102;  // 破军 主动(非初始种子)
    constexpr uint32_t kClassSeed = 1101;    // 破军 初始主动
    constexpr uint32_t kLegacy = 1;          // 旧通用占位技能
    const auto* classRow = SkillTableManager::Instance().FindByIdSilent(kClassActive).first;
    ASSERT_NE(nullptr, classRow);
    ASSERT_NE(0u, classRow->class_id()) << "fixture: 1102 must be a class skill";
    const auto* seedRow = SkillTableManager::Instance().FindByIdSilent(kClassSeed).first;
    ASSERT_NE(nullptr, seedRow);
    ASSERT_NE(0u, seedRow->class_id()) << "fixture: 1101 must be a class skill";
    const auto* legacyRow = SkillTableManager::Instance().FindByIdSilent(kLegacy).first;
    ASSERT_NE(nullptr, legacyRow);
    ASSERT_EQ(0u, legacyRow->class_id()) << "fixture: 1 must be a legacy skill";
    ASSERT_FALSE(SkillTableManager::Instance().Exists(1001)) << "fixture: 1001 must be missing from SkillTable";
    const uint32_t aboveMax = classRow->max_rank() + 5;

    entt::entity player = CreateSkillActor();
    auto &skillList = tlsEcs.actorRegistry.get_or_emplace<PlayerSkillListComp>(player);
    auto* rankAboveMax = skillList.add_skill_list();
    rankAboveMax->set_skill_table_id(kClassActive);
    rankAboveMax->set_rank(aboveMax);
    rankAboveMax->mutable_unknown_fields()->AddVarint(412346, 1);
    auto* rankZero = skillList.add_skill_list();
    rankZero->set_skill_table_id(kClassSeed);
    rankZero->set_rank(0);
    rankZero->mutable_unknown_fields()->AddVarint(412346, 2);
    auto* legacyWithRank = skillList.add_skill_list();
    legacyWithRank->set_skill_table_id(kLegacy);
    legacyWithRank->set_rank(7);
    legacyWithRank->set_id(42);
    legacyWithRank->mutable_unknown_fields()->AddVarint(412346, 3);
    auto* unknownSkill = skillList.add_skill_list();
    unknownSkill->set_skill_table_id(1001);
    unknownSkill->set_rank(37);
    skillList.mutable_unknown_fields()->AddVarint(412345, 5); // 新版本给组件本层加的字段
    const std::string before = skillList.SerializeAsString();

    PlayerSkillSystem::SanitizeSkillList(player);

    EXPECT_EQ(before, skillList.SerializeAsString());
    ASSERT_EQ(4, skillList.skill_list_size());
    EXPECT_EQ(aboveMax, skillList.skill_list(0).rank()) << "超过本版本 max_rank 不压回";
    EXPECT_EQ(kClassSeed, skillList.skill_list(1).skill_table_id()) << "rank 0 的职业技能不删";
    EXPECT_EQ(7u, skillList.skill_list(2).rank()) << "旧通用技能的 rank 不清 0";
    EXPECT_EQ(1, skillList.unknown_fields().field_count());
}

// Main function
int main(int argc, char** argv) {
    if (!test_config::FindAndLoadTestConfig(argc, argv))
        return 1;

    tlsIdGeneratorManager.SetNodeId(91);
    actor_net_id::Install(tlsEcs.actorRegistry);
    EventLoop loop;

    ::testing::InitGoogleTest(&argc, argv);
    CooldownTableManager::Instance().Load();
    SkillTableManager::Instance().Load();
    ActorActionStateTableManager::Instance().Load();
    ActorActionCombatStateTableManager::Instance().Load();
    SkillPermissionTableManager::Instance().Load();
    BuffTableManager::Instance().Load();
    ClassTableManager::Instance().Load();  // player_skill_book_test:职业初始技能种子

    int ret = RUN_ALL_TESTS();
    tlsEcs.actorRegistry.clear(); // Clean up thread-local storage after all tests
    return ret;
}
