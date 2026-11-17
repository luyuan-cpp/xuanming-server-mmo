#include <gtest/gtest.h>

#include <algorithm>
#include <cstdint>
#include <limits>
#include <memory>
#include <string>
#include <vector>

#include <google/protobuf/io/coded_stream.h>
#include <google/protobuf/io/zero_copy_stream_impl_lite.h>
#include <google/protobuf/message_lite.h>

#include "constants/turn_battle_constants.h"
#include "memory_battle_data_provider.h"
#include "system/class_skill_rules.h"
#include "system/turn_battle_engine.h"
#include "table/proto/tip/common_error_tip.pb.h"

// 只经公开行动与快照观察 Buff 事件，不注入引擎私有状态。
namespace {

using turnbattle::TurnBattleEngine;
constexpr uint64_t kHolder = 8601;
constexpr uint64_t kVictim = 8602;
constexpr uint32_t kArm = 8611;
constexpr uint32_t kStrike = 8612;
constexpr uint32_t kWait = 8613;
constexpr uint32_t kNextAttack = 8621;
constexpr uint32_t kSelfChild = 8622;
constexpr uint32_t kTargetChild = 8623;

SkillTable& AddSkill(MemoryBattleDataProvider& data, uint32_t id, classskill::TargetTeam team) {
    auto& row = data.AddSkill(id);
    row.set_class_id(1);
    row.add_skill_type(turnbattle::kSkillTypeBitGeneral);
    row.set_target_team(static_cast<uint32_t>(team));
    row.set_max_targets(1);
    row.set_unlock_level(1);
    row.set_max_rank(10);
    return row;
}

BuffTable& AddPersistentBuff(MemoryBattleDataProvider& data, uint32_t id, uint32_t type) {
    auto& row = data.AddBuff(id);
    row.set_buff_type(type);
    row.set_duration(60.0);
    row.set_max_layer(3);
    return row;
}

std::shared_ptr<MemoryBattleDataProvider> MakeData() {
    auto data = std::make_shared<MemoryBattleDataProvider>();
    data->AddBattleReward(0);
    AddSkill(*data, kArm, classskill::TargetTeam::kSelf).add_effect(kNextAttack);
    AddSkill(*data, kStrike, classskill::TargetTeam::kEnemy);
    data->SetSkillDamage(kStrike, 10.0);
    AddSkill(*data, kWait, classskill::TargetTeam::kSelf);
    AddPersistentBuff(*data, kNextAttack, turnbattle::kBuffTypeNextBasicAttack);
    data->SetBuffBonusDamage(kNextAttack, 17.0);
    AddPersistentBuff(*data, kSelfChild, turnbattle::kBuffTypeIncreaseDefense);
    AddPersistentBuff(*data, kTargetChild, turnbattle::kBuffTypePoison);
    return data;
}

CreateBattleRequest MakeRequest() {
    CreateBattleRequest request;
    request.set_battle_id(86001);
    request.set_match_mode(3);
    request.set_seed(20261117);
    for (uint32_t side = 0; side < 2; ++side) {
        auto* player = request.add_players();
        player->set_player_id(side == 0 ? kHolder : kVictim);
        player->set_team_index(side);
        player->set_level(30);
        player->set_class_id(1);
        player->set_max_health(10000);
        player->set_max_mana(100);
        player->set_physical_attack(100);
        player->set_magic_attack(100);
        player->mutable_base_attributes()->set_health(10000);
        player->mutable_base_attributes()->set_mana(100);
        player->mutable_base_attributes()->set_speed(side == 0 ? 120 : 60);
        for (const auto skill : {kArm, kStrike, kWait}) {
            player->add_skill_table_ids(skill);
            (*player->mutable_skill_ranks())[skill] = 1;
        }
    }
    return request;
}

void AddSnapshotBuff(CreateBattleRequest& request, int playerIndex, uint32_t tableId,
                     uint64_t instanceId = 100, uint64_t caster = kHolder) {
    auto* buff = request.mutable_players(playerIndex)->add_buffs();
    buff->set_buff_id(instanceId);
    buff->set_buff_table_id(tableId);
    buff->set_caster_id(caster);
    buff->set_layer(1);
    buff->set_remain_rounds(10);
}

BattleAction SkillAction(uint32_t skill, uint64_t target) {
    BattleAction action;
    action.set_action_type(BATTLE_ACTION_SKILL);
    action.set_skill_table_id(skill);
    action.set_target_id(target);
    return action;
}

BattleAction Attack(uint64_t target) {
    BattleAction action;
    action.set_action_type(BATTLE_ACTION_ATTACK);
    action.set_target_id(target);
    return action;
}

TurnResultS2C Round(TurnBattleEngine& engine, const BattleAction& holderAction,
                   const BattleAction& victimAction = SkillAction(kWait, kVictim)) {
    EXPECT_EQ(engine.ValidateAction(kHolder, holderAction), kSuccess);
    EXPECT_EQ(engine.ValidateAction(kVictim, victimAction), kSuccess);
    engine.SubmitAction(kHolder, holderAction);
    engine.SubmitAction(kVictim, victimAction);
    return engine.ResolveCurrentRound();
}

const BattleBuffEntry* BuffOn(const BattleStateS2C& state, uint64_t actorId, uint32_t tableId) {
    for (const auto& actor : state.actors()) {
        if (actor.actor_id() != actorId) continue;
        for (const auto& buff : actor.buffs()) {
            if (buff.buff_table_id() == tableId) return &buff;
        }
    }
    return nullptr;
}

std::vector<BattleEventItem> DamageFrom(const TurnResultS2C& result, uint64_t source) {
    std::vector<BattleEventItem> events;
    for (const auto& event : result.events()) {
        if (event.event_type() == BATTLE_EVENT_DAMAGE && event.source_id() == source) {
            events.push_back(event);
        }
    }
    return events;
}

int BuffEventCount(const TurnResultS2C& result, eBattleEventType type, uint32_t tableId) {
    return static_cast<int>(std::count_if(result.events().begin(), result.events().end(),
        [type, tableId](const auto& event) {
            return event.event_type() == type && event.buff_table_id() == tableId;
        }));
}

std::string DeterministicBytes(const google::protobuf::MessageLite& message) {
    std::string bytes;
    bool serialized = false;
    {
        google::protobuf::io::StringOutputStream sink(&bytes);
        google::protobuf::io::CodedOutputStream coded(&sink);
        // 与 battle_table_fingerprint 同口径，仅规范 map 编码；事件顺序与完整消息原样保留。
        coded.SetSerializationDeterministic(true);
        serialized = message.SerializeToCodedStream(&coded) && !coded.HadError();
    } // 必须等缓冲回写完毕，再读取字节。
    EXPECT_TRUE(serialized);
    return bytes;
}

TEST(BuffEventTest, SelfSkillArmsNextAttackWithoutRunningItsChildren) {
    auto data = MakeData();
    auto& parent = data->AddBuff(kNextAttack);
    parent.set_duration(5.0); // 真表 19/20：不足一轮的秒数仍须留下下一轮行动机会。
    parent.add_sub_buff(kSelfChild);
    parent.add_target_sub_buff(kTargetChild);
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(MakeRequest()));

    const auto result = Round(engine, SkillAction(kArm, kHolder));
    const auto state = engine.BuildStateSnapshot();
    EXPECT_NE(BuffOn(state, kHolder, kNextAttack), nullptr);
    EXPECT_EQ(BuffOn(state, kHolder, kSelfChild), nullptr);
    EXPECT_EQ(BuffOn(state, kHolder, kTargetChild), nullptr);
    EXPECT_EQ(BuffOn(state, kVictim, kTargetChild), nullptr);
    EXPECT_EQ(BuffEventCount(result, BATTLE_EVENT_BUFF_ADD, kNextAttack), 1);
    EXPECT_EQ(BuffEventCount(result, BATTLE_EVENT_BUFF_ADD, kSelfChild), 0);
    EXPECT_EQ(BuffEventCount(result, BATTLE_EVENT_BUFF_ADD, kTargetChild), 0);
    const auto* armed = BuffOn(state, kHolder, kNextAttack);
    ASSERT_NE(armed, nullptr);
    EXPECT_EQ(armed->remain_rounds(), 1u);
    const auto attackHits = DamageFrom(Round(engine, Attack(kVictim)), kHolder);
    ASSERT_EQ(attackHits.size(), 1u);
    EXPECT_EQ(attackHits.front().value(), 50u);
}

TEST(BuffEventTest, FirstNormalHitAddsBonusAndChildrenWithOriginalCasterExactlyOnce) {
    auto data = MakeData();
    auto& parent = data->AddBuff(kNextAttack);
    parent.add_sub_buff(kSelfChild);
    parent.add_target_sub_buff(kTargetChild);
    auto request = MakeRequest();
    AddSnapshotBuff(request, 0, kNextAttack, 100, kVictim);
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(request));

    const auto first = Round(engine, Attack(kVictim));
    const auto hits = DamageFrom(first, kHolder);
    ASSERT_EQ(hits.size(), 1u);
    EXPECT_EQ(hits.front().value(), 50u); // (10 + 100) * 0.3 + 17。
    const auto state = engine.BuildStateSnapshot();
    EXPECT_EQ(BuffOn(state, kHolder, kNextAttack), nullptr);
    const auto* self = BuffOn(state, kHolder, kSelfChild);
    const auto* target = BuffOn(state, kVictim, kTargetChild);
    ASSERT_NE(self, nullptr);
    ASSERT_NE(target, nullptr);
    EXPECT_EQ(self->caster_id(), kVictim);
    EXPECT_EQ(target->caster_id(), kVictim);
    EXPECT_EQ(BuffOn(state, kHolder, kTargetChild), nullptr);
    EXPECT_EQ(BuffEventCount(first, BATTLE_EVENT_BUFF_REMOVE, kNextAttack), 1);

    // 同一行动的完整因果顺序必须可供客户端直接播放，子效果不能在落血之后补发。
    ASSERT_EQ(first.events_size(), 6);
    EXPECT_EQ(first.events(0).event_type(), BATTLE_EVENT_ATTACK);
    EXPECT_EQ(first.events(1).event_type(), BATTLE_EVENT_BUFF_ADD);
    EXPECT_EQ(first.events(1).buff_table_id(), kSelfChild);
    EXPECT_EQ(first.events(2).event_type(), BATTLE_EVENT_BUFF_ADD);
    EXPECT_EQ(first.events(2).buff_table_id(), kTargetChild);
    EXPECT_EQ(first.events(3).event_type(), BATTLE_EVENT_BUFF_REMOVE);
    EXPECT_EQ(first.events(3).buff_table_id(), kNextAttack);
    EXPECT_EQ(first.events(4).event_type(), BATTLE_EVENT_DAMAGE);
    ASSERT_NE(first.events(0).group_id(), 0u);
    for (int index = 1; index <= 4; ++index) {
        EXPECT_EQ(first.events(index).group_id(), first.events(0).group_id());
        EXPECT_EQ(first.events(index).hit_index(), 0u);
    }
    EXPECT_EQ(first.events(5).event_type(), BATTLE_EVENT_SKILL);
    EXPECT_NE(first.events(5).group_id(), first.events(0).group_id());

    const auto second = Round(engine, Attack(kVictim));
    const auto nextHits = DamageFrom(second, kHolder);
    ASSERT_EQ(nextHits.size(), 1u);
    EXPECT_EQ(nextHits.front().value(), 33u);
    EXPECT_EQ(BuffEventCount(second, BATTLE_EVENT_BUFF_ADD, kSelfChild), 0);
    EXPECT_EQ(BuffEventCount(second, BATTLE_EVENT_BUFF_REMOVE, kNextAttack), 0);
}

TEST(BuffEventTest, ThirdPartyCasterOwnsChildResistanceAndZeroSourceStaysUnknown) {
    constexpr uint64_t originalCaster = 8603;
    // 第一档的原施加者能穿透抗毒；第二档是敏感性对照；第三档的 0 不猜成持有者。
    for (int variant = 0; variant < 3; ++variant) {
        SCOPED_TRACE(variant);
        auto data = MakeData();
        data->AddBuff(kNextAttack).add_target_sub_buff(kTargetChild);
        auto request = MakeRequest();
        request.mutable_players(0)->mutable_combat()->set_ignore_ailment_resist(0);
        request.mutable_players(1)->mutable_combat()->set_resist_poison(100);
        auto* original = request.add_players();
        original->CopyFrom(request.players(0));
        original->set_player_id(originalCaster);
        original->mutable_base_attributes()->set_speed(30);
        original->mutable_combat()->set_ignore_ailment_resist(variant == 1 ? 0 : 100);
        const uint64_t source = variant == 2 ? 0 : originalCaster;
        AddSnapshotBuff(request, 0, kNextAttack, 100, source);
        TurnBattleEngine engine(data);
        ASSERT_TRUE(engine.Initialize(request));
        const auto idle = SkillAction(kWait, originalCaster);
        ASSERT_EQ(engine.ValidateAction(originalCaster, idle), kSuccess);
        engine.SubmitAction(originalCaster, idle);

        const auto result = Round(engine, Attack(kVictim));
        const auto hits = DamageFrom(result, kHolder);
        ASSERT_EQ(hits.size(), 1u);
        EXPECT_EQ(hits.front().target_id(), kVictim);
        EXPECT_EQ(hits.front().value(), 50u);
        EXPECT_TRUE(DamageFrom(result, originalCaster).empty());
        const auto state = engine.BuildStateSnapshot();
        const auto* poison = BuffOn(state, kVictim, kTargetChild);
        if (variant == 1) {
            EXPECT_EQ(poison, nullptr);
            EXPECT_EQ(BuffEventCount(result, BATTLE_EVENT_RESIST, kTargetChild), 1);
            EXPECT_EQ(BuffEventCount(result, BATTLE_EVENT_BUFF_ADD, kTargetChild), 0);
        } else {
            ASSERT_NE(poison, nullptr);
            EXPECT_EQ(poison->caster_id(), source);
            EXPECT_EQ(BuffEventCount(result, BATTLE_EVENT_RESIST, kTargetChild), 0);
            EXPECT_EQ(BuffEventCount(result, BATTLE_EVENT_BUFF_ADD, kTargetChild), 1);
        }
        for (const auto& event : result.events()) {
            if (event.buff_table_id() == kTargetChild) {
                EXPECT_EQ(event.source_id(), source);
                EXPECT_EQ(event.target_id(), kVictim);
            }
        }
        EXPECT_EQ(BuffEventCount(result, BATTLE_EVENT_BUFF_REMOVE, kNextAttack), 1);
    }
}

TEST(BuffEventTest, OrdinarySkillDoesNotConsumeNextAttack) {
    auto data = MakeData();
    auto request = MakeRequest();
    AddSnapshotBuff(request, 0, kNextAttack);
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(request));

    const auto skill = Round(engine, SkillAction(kStrike, kVictim));
    const auto skillHits = DamageFrom(skill, kHolder);
    ASSERT_EQ(skillHits.size(), 1u);
    EXPECT_EQ(skillHits.front().value(), 33u);
    EXPECT_NE(BuffOn(engine.BuildStateSnapshot(), kHolder, kNextAttack), nullptr);
    EXPECT_EQ(BuffEventCount(skill, BATTLE_EVENT_BUFF_REMOVE, kNextAttack), 0);
    const auto attackHits = DamageFrom(Round(engine, Attack(kVictim)), kHolder);
    ASSERT_EQ(attackHits.size(), 1u);
    EXPECT_EQ(attackHits.front().value(), 50u);
}

TEST(BuffEventTest, CounterDoesNotConsumeButLaterOrdinaryActionDoes) {
    auto data = MakeData();
    auto request = MakeRequest();
    AddSnapshotBuff(request, 1, kNextAttack, 100, kVictim);
    request.mutable_players(1)->mutable_combat()->set_counter_rate(100);
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(request));

    const auto first = Round(engine, Attack(kVictim));
    const auto counterHits = DamageFrom(first, kVictim);
    ASSERT_EQ(counterHits.size(), 1u);
    EXPECT_EQ(counterHits.front().hit_kind(), BATTLE_HIT_COUNTER);
    EXPECT_EQ(counterHits.front().value(), 33u);
    EXPECT_NE(BuffOn(engine.BuildStateSnapshot(), kVictim, kNextAttack), nullptr);
    EXPECT_EQ(BuffEventCount(first, BATTLE_EVENT_BUFF_REMOVE, kNextAttack), 0);

    const auto normalHits = DamageFrom(
        Round(engine, SkillAction(kWait, kHolder), Attack(kHolder)), kVictim);
    ASSERT_EQ(normalHits.size(), 1u);
    EXPECT_EQ(normalHits.front().hit_kind(), BATTLE_HIT_NORMAL);
    EXPECT_EQ(normalHits.front().value(), 50u);
    EXPECT_EQ(BuffOn(engine.BuildStateSnapshot(), kVictim, kNextAttack), nullptr);
}

TEST(BuffEventTest, TriggerCreatedNextAttackWaitsThroughComboUntilNextOrdinaryAction) {
    constexpr uint32_t followup = 8631;
    auto data = MakeData();
    data->AddBuff(kNextAttack).add_sub_buff(followup);
    AddPersistentBuff(*data, followup, turnbattle::kBuffTypeNextBasicAttack);
    data->SetBuffBonusDamage(followup, 23.0);
    auto request = MakeRequest();
    AddSnapshotBuff(request, 0, kNextAttack);
    request.mutable_players(0)->mutable_combat()->set_combo_rate(100);
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(request));

    const auto first = Round(engine, Attack(kVictim));
    const auto hits = DamageFrom(first, kHolder);
    ASSERT_EQ(hits.size(), 1u + turnbattle::kMaxComboExtraHits);
    EXPECT_EQ(hits.front().value(), 50u);
    for (size_t index = 1; index < hits.size(); ++index) {
        EXPECT_EQ(hits[index].hit_kind(), BATTLE_HIT_COMBO);
        EXPECT_EQ(hits[index].value(), 17u); // ceil(33 * 50%)，不吃新产生的 23 点附伤。
    }
    EXPECT_EQ(BuffOn(engine.BuildStateSnapshot(), kHolder, kNextAttack), nullptr);
    EXPECT_NE(BuffOn(engine.BuildStateSnapshot(), kHolder, followup), nullptr);
    EXPECT_EQ(BuffEventCount(first, BATTLE_EVENT_BUFF_REMOVE, followup), 0);

    const auto second = Round(engine, Attack(kVictim));
    const auto nextHits = DamageFrom(second, kHolder);
    ASSERT_FALSE(nextHits.empty());
    EXPECT_EQ(nextHits.front().value(), 56u);
    EXPECT_EQ(BuffEventCount(second, BATTLE_EVENT_BUFF_REMOVE, followup), 1);
}

TEST(BuffEventTest, TriggerFinishesAfterSelfDispelAndSkipsOtherRemovedPendingBuff) {
    constexpr uint32_t other = 8632;
    constexpr uint32_t dispel = 8633;
    constexpr uint32_t unexpectedChild = 8634;
    auto data = MakeData();
    auto& parent = data->AddBuff(kNextAttack);
    (*parent.mutable_tag())["Armed"] = true;
    parent.add_sub_buff(dispel);
    parent.add_sub_buff(kSelfChild);
    parent.add_target_sub_buff(kTargetChild);
    auto& otherRow = AddPersistentBuff(*data, other, turnbattle::kBuffTypeNextBasicAttack);
    (*otherRow.mutable_tag())["Armed"] = true;
    otherRow.add_sub_buff(unexpectedChild);
    data->SetBuffBonusDamage(other, 71.0);
    AddPersistentBuff(*data, unexpectedChild, turnbattle::kBuffTypeIncreaseDefense);
    auto& dispelRow = data->AddBuff(dispel);
    dispelRow.set_buff_type(turnbattle::kBuffTypeDispel);
    (*dispelRow.mutable_dispel_tag())["Armed"] = true;
    auto request = MakeRequest();
    AddSnapshotBuff(request, 0, kNextAttack, 100);
    AddSnapshotBuff(request, 0, other, 101);
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(request));

    const auto result = Round(engine, Attack(kVictim));
    const auto hits = DamageFrom(result, kHolder);
    ASSERT_EQ(hits.size(), 1u);
    EXPECT_EQ(hits.front().value(), 50u);
    const auto state = engine.BuildStateSnapshot();
    EXPECT_NE(BuffOn(state, kHolder, kSelfChild), nullptr);
    EXPECT_NE(BuffOn(state, kVictim, kTargetChild), nullptr);
    EXPECT_EQ(BuffOn(state, kHolder, unexpectedChild), nullptr);
    EXPECT_EQ(BuffEventCount(result, BATTLE_EVENT_BUFF_REMOVE, kNextAttack), 1);
    EXPECT_EQ(BuffEventCount(result, BATTLE_EVENT_BUFF_REMOVE, other), 1);
}

TEST(BuffEventTest, OrdinaryStartStopsWhenAChildDispelsItsParent) {
    constexpr uint32_t dispel = 8635;
    auto data = MakeData();
    auto& arm = data->AddSkill(kArm);
    arm.clear_effect();
    arm.add_effect(kSelfChild);
    auto& parent = data->AddBuff(kSelfChild);
    (*parent.mutable_tag())["TransientParent"] = true;
    parent.add_sub_buff(dispel);
    parent.add_sub_buff(kNextAttack);
    parent.add_target_sub_buff(kTargetChild);
    auto& dispelRow = data->AddBuff(dispel);
    dispelRow.set_buff_type(turnbattle::kBuffTypeDispel);
    (*dispelRow.mutable_dispel_tag())["TransientParent"] = true;
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(MakeRequest()));

    const auto result = Round(engine, SkillAction(kArm, kHolder));
    const auto state = engine.BuildStateSnapshot();
    EXPECT_EQ(BuffOn(state, kHolder, kSelfChild), nullptr);
    EXPECT_EQ(BuffOn(state, kHolder, kNextAttack), nullptr);
    EXPECT_EQ(BuffOn(state, kHolder, kTargetChild), nullptr);
    EXPECT_EQ(BuffEventCount(result, BATTLE_EVENT_BUFF_ADD, kNextAttack), 0);
    EXPECT_EQ(BuffEventCount(result, BATTLE_EVENT_BUFF_REMOVE, kSelfChild), 1);
}

TEST(BuffEventTest, TriggerChildrenUseNormalImmunityAndDispelRules) {
    constexpr uint32_t immunity = 8636;
    constexpr uint32_t dispel = 8637;
    auto data = MakeData();
    data->AddBuff(kNextAttack).add_sub_buff(dispel);
    data->AddBuff(kNextAttack).add_target_sub_buff(kTargetChild);
    (*data->AddBuff(kTargetChild).mutable_tag())["Toxin"] = true;
    (*data->AddBuff(kSelfChild).mutable_tag())["OldBuff"] = true;
    auto& immunityRow = AddPersistentBuff(*data, immunity, turnbattle::kBuffTypeImmunity);
    (*immunityRow.mutable_immune_tag())["Toxin"] = true;
    auto& dispelRow = data->AddBuff(dispel);
    dispelRow.set_buff_type(turnbattle::kBuffTypeDispel);
    (*dispelRow.mutable_dispel_tag())["OldBuff"] = true;
    auto request = MakeRequest();
    AddSnapshotBuff(request, 0, kNextAttack, 100);
    AddSnapshotBuff(request, 0, kSelfChild, 101);
    AddSnapshotBuff(request, 1, immunity, 102, kVictim);
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(request));

    const auto result = Round(engine, Attack(kVictim));
    const auto state = engine.BuildStateSnapshot();
    EXPECT_EQ(BuffOn(state, kHolder, kSelfChild), nullptr);
    EXPECT_EQ(BuffOn(state, kVictim, kTargetChild), nullptr);
    EXPECT_NE(BuffOn(state, kVictim, immunity), nullptr);
    EXPECT_EQ(BuffEventCount(result, BATTLE_EVENT_BUFF_REMOVE, kSelfChild), 1);
    EXPECT_EQ(BuffEventCount(result, BATTLE_EVENT_BUFF_ADD, kTargetChild), 0);
    const auto hits = DamageFrom(result, kHolder);
    ASSERT_EQ(hits.size(), 1u);
    EXPECT_EQ(hits.front().value(), 50u);
}

TEST(BuffEventTest, SameCasterRefreshKeepsOneInstanceAndDoesNotMultiplyBonusByLayers) {
    auto data = MakeData();
    auto& trigger = data->AddBuff(kNextAttack);
    trigger.set_duration(5.0);
    trigger.add_sub_buff(kNextAttack); // 同表同来源自环只刷新当前实例，随后仍被消费。
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(MakeRequest()));
    Round(engine, SkillAction(kArm, kHolder));
    const auto firstState = engine.BuildStateSnapshot();
    const auto* first = BuffOn(firstState, kHolder, kNextAttack);
    ASSERT_NE(first, nullptr);
    EXPECT_EQ(first->remain_rounds(), 1u);
    const auto originalId = first->buff_id();

    const auto refreshed = Round(engine, SkillAction(kArm, kHolder));
    const auto secondState = engine.BuildStateSnapshot();
    const auto* second = BuffOn(secondState, kHolder, kNextAttack);
    ASSERT_NE(second, nullptr);
    EXPECT_EQ(second->buff_id(), originalId);
    EXPECT_EQ(second->layer(), 2u);
    EXPECT_EQ(second->remain_rounds(), 1u);
    EXPECT_EQ(BuffEventCount(refreshed, BATTLE_EVENT_BUFF_ADD, kNextAttack), 1);
    const auto result = Round(engine, Attack(kVictim));
    const auto hits = DamageFrom(result, kHolder);
    ASSERT_EQ(hits.size(), 1u);
    EXPECT_EQ(hits.front().value(), 50u);
    EXPECT_EQ(BuffEventCount(result, BATTLE_EVENT_BUFF_ADD, kNextAttack), 1);
    EXPECT_EQ(BuffEventCount(result, BATTLE_EVENT_BUFF_REMOVE, kNextAttack), 1);
    EXPECT_EQ(BuffOn(engine.BuildStateSnapshot(), kHolder, kNextAttack), nullptr);
}

TEST(BuffEventTest, ShortTriggerExpiresAfterOneCompleteUnspentRound) {
    auto data = MakeData();
    data->AddBuff(kNextAttack).set_duration(5.0);
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(MakeRequest()));
    const auto created = Round(engine, SkillAction(kArm, kHolder));
    EXPECT_EQ(BuffEventCount(created, BATTLE_EVENT_BUFF_REMOVE, kNextAttack), 0);
    EXPECT_NE(BuffOn(engine.BuildStateSnapshot(), kHolder, kNextAttack), nullptr);

    const auto waited = Round(engine, SkillAction(kWait, kHolder));
    EXPECT_EQ(BuffEventCount(waited, BATTLE_EVENT_BUFF_REMOVE, kNextAttack), 1);
    EXPECT_EQ(BuffOn(engine.BuildStateSnapshot(), kHolder, kNextAttack), nullptr);
    const auto hits = DamageFrom(Round(engine, Attack(kVictim)), kHolder);
    ASSERT_EQ(hits.size(), 1u);
    EXPECT_EQ(hits.front().value(), 33u);
}

TEST(BuffEventTest, ChildDispelAndRecreateCycleLeavesNewInstanceForNextAction) {
    constexpr uint32_t bridge = 8639;
    auto data = MakeData();
    auto& parent = data->AddBuff(kNextAttack);
    (*parent.mutable_tag())["Armed"] = true;
    parent.add_sub_buff(bridge);
    auto& child = AddPersistentBuff(*data, bridge, turnbattle::kBuffTypeIncreaseDefense);
    (*child.mutable_dispel_tag())["Armed"] = true;
    child.add_sub_buff(kNextAttack);
    auto request = MakeRequest();
    AddSnapshotBuff(request, 0, kNextAttack, 100);
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(request));

    const auto first = Round(engine, Attack(kVictim));
    const auto firstHits = DamageFrom(first, kHolder);
    ASSERT_EQ(firstHits.size(), 1u);
    EXPECT_EQ(firstHits.front().value(), 50u);
    EXPECT_EQ(BuffEventCount(first, BATTLE_EVENT_BUFF_REMOVE, kNextAttack), 1);
    EXPECT_EQ(BuffEventCount(first, BATTLE_EVENT_BUFF_ADD, kNextAttack), 1);
    const auto state = engine.BuildStateSnapshot();
    const auto* replacement = BuffOn(state, kHolder, kNextAttack);
    ASSERT_NE(replacement, nullptr);
    EXPECT_NE(replacement->buff_id(), 100u);
    EXPECT_NE(BuffOn(state, kHolder, bridge), nullptr);

    // 下一次先驱散本次 A，再刷新已存在的 B；不重新执行 B 的 Start，因此循环止步。
    const auto second = Round(engine, Attack(kVictim));
    const auto secondHits = DamageFrom(second, kHolder);
    ASSERT_EQ(secondHits.size(), 1u);
    EXPECT_EQ(secondHits.front().value(), 50u);
    EXPECT_EQ(BuffEventCount(second, BATTLE_EVENT_BUFF_REMOVE, kNextAttack), 1);
    EXPECT_EQ(BuffEventCount(second, BATTLE_EVENT_BUFF_ADD, kNextAttack), 0);
    EXPECT_EQ(BuffOn(engine.BuildStateSnapshot(), kHolder, kNextAttack), nullptr);
}

TEST(BuffEventTest, UnusedTriggerConfigurationDoesNotChangeOrdinaryEventStream) {
    auto baselineData = MakeData();
    auto changedData = MakeData();
    changedData->SetBuffBonusDamage(kNextAttack, std::numeric_limits<double>::max());
    changedData->AddBuff(kNextAttack).add_sub_buff(kTargetChild);
    TurnBattleEngine baseline(baselineData);
    TurnBattleEngine changed(changedData);
    const auto request = MakeRequest();
    ASSERT_TRUE(baseline.Initialize(request));
    ASSERT_TRUE(changed.Initialize(request));
    for (int round = 0; round < 3; ++round) {
        const auto first = Round(baseline, Attack(kVictim), Attack(kHolder));
        const auto second = Round(changed, Attack(kVictim), Attack(kHolder));
        EXPECT_EQ(DeterministicBytes(first), DeterministicBytes(second));
        EXPECT_EQ(DeterministicBytes(baseline.BuildStateSnapshot()),
                  DeterministicBytes(changed.BuildStateSnapshot()));
    }
}

TEST(BuffEventTest, InvalidBonusValuesPreserveBaseDamageAndStillConsumeTrigger) {
    for (const double bonus : {0.0, -17.0, std::numeric_limits<double>::quiet_NaN(),
                               std::numeric_limits<double>::infinity(),
                               -std::numeric_limits<double>::infinity()}) {
        SCOPED_TRACE(bonus);
        auto data = MakeData();
        data->SetBuffBonusDamage(kNextAttack, bonus);
        auto request = MakeRequest();
        AddSnapshotBuff(request, 0, kNextAttack);
        TurnBattleEngine engine(data);
        ASSERT_TRUE(engine.Initialize(request));
        const auto result = Round(engine, Attack(kVictim));
        const auto hits = DamageFrom(result, kHolder);
        ASSERT_EQ(hits.size(), 1u);
        EXPECT_EQ(hits.front().value(), 33u);
        EXPECT_EQ(BuffEventCount(result, BATTLE_EVENT_BUFF_REMOVE, kNextAttack), 1);
        EXPECT_EQ(BuffOn(engine.BuildStateSnapshot(), kHolder, kNextAttack), nullptr);
    }
}

TEST(BuffEventTest, MultipleLargeFiniteBonusesSaturateInsteadOfBecomingZeroDamage) {
    constexpr uint32_t other = 8638;
    auto data = MakeData();
    AddPersistentBuff(*data, other, turnbattle::kBuffTypeNextBasicAttack);
    data->SetBuffBonusDamage(kNextAttack, std::numeric_limits<double>::max());
    data->SetBuffBonusDamage(other, std::numeric_limits<double>::max());
    auto request = MakeRequest();
    AddSnapshotBuff(request, 0, kNextAttack, 100);
    AddSnapshotBuff(request, 0, other, 101);
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(request));
    const auto result = Round(engine, Attack(kVictim));
    const auto hits = DamageFrom(result, kHolder);
    ASSERT_EQ(hits.size(), 1u);
    EXPECT_EQ(hits.front().value(), 10000u);
    EXPECT_EQ(hits.front().target_health_after(), 0u);
    EXPECT_EQ(BuffEventCount(result, BATTLE_EVENT_BUFF_REMOVE, kNextAttack), 1);
    EXPECT_EQ(BuffEventCount(result, BATTLE_EVENT_BUFF_REMOVE, other), 1);
}

TEST(BuffEventTest, SeededTriggerComboAndCounterSequenceIsDeterministic) {
    auto data = MakeData();
    data->AddBuff(kNextAttack).add_sub_buff(kSelfChild);
    data->AddBuff(kNextAttack).add_target_sub_buff(kTargetChild);
    auto request = MakeRequest();
    AddSnapshotBuff(request, 0, kNextAttack, 100);
    request.mutable_players(0)->mutable_combat()->set_combo_rate(50);
    request.mutable_players(1)->mutable_combat()->set_counter_rate(50);
    TurnBattleEngine first(data);
    TurnBattleEngine second(data);
    ASSERT_TRUE(first.Initialize(request));
    ASSERT_TRUE(second.Initialize(request));
    for (int round = 0; round < 3; ++round) {
        const auto left = Round(first, Attack(kVictim));
        const auto right = Round(second, Attack(kVictim));
        EXPECT_EQ(DeterministicBytes(left), DeterministicBytes(right));
        EXPECT_EQ(DeterministicBytes(first.BuildStateSnapshot()),
                  DeterministicBytes(second.BuildStateSnapshot()));
    }
}

TEST(BuffEventTest, BonusIsAddedAfterCriticalAndBeforeDefendReduction) {
    auto data = MakeData();
    auto request = MakeRequest();
    AddSnapshotBuff(request, 0, kNextAttack);
    request.mutable_players(0)->mutable_combat()->set_physical_crit_rate(100);
    request.mutable_players(1)->mutable_base_attributes()->set_speed(240);
    TurnBattleEngine engine(data);
    ASSERT_TRUE(engine.Initialize(request));
    BattleAction defend;
    defend.set_action_type(BATTLE_ACTION_DEFEND);
    const auto result = Round(engine, Attack(kVictim), defend);
    const auto hits = DamageFrom(result, kHolder);
    ASSERT_EQ(hits.size(), 1u);
    EXPECT_TRUE(hits.front().is_critical());
    EXPECT_EQ(hits.front().value(), 42u); // ceil((33 * 2 + 17) / 2)。
}

} // namespace
