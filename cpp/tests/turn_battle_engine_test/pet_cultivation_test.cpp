#include "gtest/gtest.h"
#include "services/battle/system/pet_cultivation_rules.h"
#include "services/scene/player/system/pet_entry_rules.h"
using namespace turnbattle;
TEST(PetCultivationRules, OnlyPetDirectPhysicalMagicDamageAndNormalComboReceiveContribution) {
 BattleActorState a;a.set_actor_type(BATTLE_ACTOR_TYPE_PET);a.mutable_pet_cultivation()->set_physical_damage_bps(500);a.mutable_pet_cultivation()->set_magic_damage_bps(700);
 EXPECT_DOUBLE_EQ(petcultivation::DamageBonus(a,combatdamage::kPhysicalDamage,BattleDamageOrigin::Direct,BATTLE_HIT_NORMAL),.05);
 EXPECT_DOUBLE_EQ(petcultivation::DamageBonus(a,combatdamage::kMagicDamage,BattleDamageOrigin::Direct,BATTLE_HIT_COMBO),.07);
 EXPECT_DOUBLE_EQ(petcultivation::DamageBonus(a,1,BattleDamageOrigin::Direct,BATTLE_HIT_COUNTER),0);
 for(auto origin:{BattleDamageOrigin::Periodic,BattleDamageOrigin::Pursuit,BattleDamageOrigin::Reflect,BattleDamageOrigin::Mirror,BattleDamageOrigin::Confusion,BattleDamageOrigin::Redirect})
  EXPECT_DOUBLE_EQ(petcultivation::DamageBonus(a,1,origin,BATTLE_HIT_NORMAL),0);
 a.set_actor_type(BATTLE_ACTOR_TYPE_PLAYER);EXPECT_DOUBLE_EQ(petcultivation::DamageBonus(a,1,BattleDamageOrigin::Direct,BATTLE_HIT_NORMAL),0);
}
TEST(PetCultivationRules, TypedControlAndSeparateHealingShieldDoNotLeak) {
 BattleActorState a;a.set_actor_type(BATTLE_ACTOR_TYPE_PET);auto* e=a.mutable_pet_cultivation();e->set_outgoing_heal_bps(100);e->set_shield_capacity_bps(300);(*e->mutable_ailment_hit_bps())[7]=75;(*e->mutable_ailment_resist_bps())[4]=25;
 EXPECT_DOUBLE_EQ(petcultivation::HealBonus(a),.01);EXPECT_DOUBLE_EQ(petcultivation::ShieldBonus(a),.03);
 EXPECT_DOUBLE_EQ(petcultivation::HitBps(a,7),75);EXPECT_DOUBLE_EQ(petcultivation::HitBps(a,4),0);EXPECT_DOUBLE_EQ(petcultivation::ResistBps(a,4),25);
 EXPECT_DOUBLE_EQ(petcultivation::ResistBps(a,7),0);EXPECT_DOUBLE_EQ(petcultivation::PoisonBonus(a),0);
}
TEST(PetEntriesRules, FullReplacementAllowsDecreaseAndAccountsUnknownKeys) {
 petentries::Rules r{{501,{20,50}},{503,{20,50}}};petentries::Points old{{501,12},{999,3}},target{{503,17}};
 EXPECT_EQ(petentries::Validate(old,target,r,20,0),petentries::Error::None);
 auto next=petentries::ReplaceKnown(old,target,r);EXPECT_FALSE(next.contains(501));EXPECT_EQ(next.at(999),3u);
 target[503]=18;EXPECT_EQ(petentries::Validate(old,target,r,20,0),petentries::Error::Budget);
 target={{501,1},{503,1}};EXPECT_EQ(petentries::Validate(old,target,r,20,1),petentries::Error::Directions);
 EXPECT_FALSE(petentries::ValidRequestId("short"));EXPECT_FALSE(petentries::ValidRequestId("entry-request-$bad"));EXPECT_TRUE(petentries::ValidRequestId("entry-request-0001"));
}

#include "memory_battle_data_provider.h"
#include "system/turn_battle_engine.h"
#include "table/proto/tip/common_error_tip.pb.h"
namespace {
struct PetBattleFixture {
 std::shared_ptr<MemoryBattleDataProvider> data=std::make_shared<MemoryBattleDataProvider>();
 CreateBattleRequest request;
 PetBattleFixture() {
  data->AddBattleReward(0);data->AddPet(1);request.set_battle_id(89001);request.set_match_mode(3);request.set_seed(20261118);
  for(uint32_t i=0;i<2;i++){auto* p=request.add_players();p->set_player_id(101+i*100);p->set_team_index(i);p->set_level(30);p->set_max_health(10000);p->set_max_mana(1000);p->mutable_base_attributes()->set_health(10000);p->mutable_base_attributes()->set_mana(1000);p->mutable_base_attributes()->set_speed(10);}
  auto* p=request.mutable_players(0)->add_pets();p->set_pet_id(901);p->set_owner_player_id(101);p->set_pet_table_id(1);p->set_level(30);p->set_max_health(10000);p->set_max_mana(1000);p->set_physical_attack(100);p->set_magic_attack(100);p->mutable_base_attributes()->set_health(1000);p->mutable_base_attributes()->set_mana(1000);p->mutable_base_attributes()->set_speed(100);
 }
 BattlePetSnapshot& Pet(){return *request.mutable_players(0)->mutable_pets(0);}
 SkillTable& Skill(uint32_t id,BattleEffectOp op,bool self=false) {
  auto& s=data->AddSkill(id);s.set_battle_model(1);s.set_caster_mask(2);s.set_ability_kind(1);s.set_target_team(self?3:1);s.set_max_targets(1);s.set_pet_max_targets(1);s.set_unlock_level(1);s.set_max_rank(10);s.add_skill_type(1);s.add_effect(id);
  auto& b=data->AddBuff(id);b.set_battle_op(static_cast<uint32_t>(op));b.set_max_layer(1);b.set_flat_value(100);b.set_duration_rounds(3);
  Pet().add_skill_table_ids(id);(*Pet().mutable_skill_ranks())[id]=1;data->SetSkillDamage(id,100);return s;
 }
 uint64_t PetId(const TurnBattleEngine& e){const auto state=e.BuildStateSnapshot();for(const auto& a:state.actors())if(a.actor_type()==BATTLE_ACTOR_TYPE_PET)return a.actor_id();return 0;}
 TurnResultS2C Round(TurnBattleEngine& e,uint32_t skill=0,bool self=false) {
  const auto id=PetId(e);BattleAction a;a.set_action_type(BATTLE_ACTION_DEFEND);e.SubmitAction(101,a);e.SubmitAction(201,a);
  a.set_action_type(skill?BATTLE_ACTION_SKILL:BATTLE_ACTION_ATTACK);a.set_skill_table_id(skill);a.set_target_id(self?id:201);
  EXPECT_EQ(e.ValidateAction(id,a),kSuccess);e.SubmitAction(id,a);return e.ResolveCurrentRound();
 }
};
uint64_t SumDamage(const TurnResultS2C& result,uint64_t source){uint64_t sum=0;for(const auto& event:result.events())if(event.event_type()==BATTLE_EVENT_DAMAGE && event.source_id()==source)sum+=event.value();return sum;}
}
TEST(PetCultivationBattle, BasicAndManualPhysicalMagicUseSavedSnapshotOnceWithoutChangingAttackPanel) {
 for(uint32_t mode=0;mode<3;mode++) {
  PetBattleFixture f;if(mode)f.Skill(901,BattleEffectOp::Damage).set_damage_type(mode==1?combatdamage::kPhysicalDamage:combatdamage::kMagicDamage);
  TurnBattleEngine baseline(f.data);ASSERT_TRUE(baseline.Initialize(f.request));auto base=f.Round(baseline,mode?901:0);auto damage=SumDamage(base,f.PetId(baseline));ASSERT_GT(damage,0u);
  f.Pet().mutable_pet_cultivation()->set_physical_damage_bps(1000);f.Pet().mutable_pet_cultivation()->set_magic_damage_bps(1000);
  TurnBattleEngine enhanced(f.data);ASSERT_TRUE(enhanced.Initialize(f.request));auto result=f.Round(enhanced,mode?901:0);auto actual=SumDamage(result,f.PetId(enhanced));
  EXPECT_GT(actual,damage);EXPECT_NEAR(static_cast<double>(actual),damage*1.10,2.0);
  const auto enhancedState=enhanced.BuildStateSnapshot();for(const auto& a:enhancedState.actors())if(a.actor_type()==BATTLE_ACTOR_TYPE_PET){EXPECT_EQ(a.physical_attack(),100u);EXPECT_EQ(a.magic_attack(),100u);}
  auto outgoing=enhanced.BuildStateSnapshot();TurnBattleEngine::StripEngineOnlyState(outgoing);for(const auto& a:outgoing.actors())EXPECT_FALSE(a.has_pet_cultivation());
 }
}
TEST(PetCultivationBattle, ImmediateHealAndFiniteShieldReceiveDistinctChannels) {
 for(const auto op:{BattleEffectOp::Heal,BattleEffectOp::Shield}){
  PetBattleFixture f;f.Skill(901,op,true);f.Pet().mutable_pet_cultivation()->set_outgoing_heal_bps(1000);f.Pet().mutable_pet_cultivation()->set_shield_capacity_bps(500);
  TurnBattleEngine engine(f.data);ASSERT_TRUE(engine.Initialize(f.request));auto result=f.Round(engine,901,true);bool found=false;
  for(const auto& event:result.events()){
   if(op==BattleEffectOp::Heal && event.event_type()==BATTLE_EVENT_HEAL && event.source_id()==f.PetId(engine)){EXPECT_EQ(event.value(),110u);found=true;}
   if(op==BattleEffectOp::Shield && event.event_type()==BATTLE_EVENT_BUFF_ADD && event.buff_table_id()==901){EXPECT_EQ(event.shield_after(),105u);found=true;}
  }
  EXPECT_TRUE(found);
 }
}

TEST(PetCultivationBattle, PoisonUsesItsOwnChannelOnceAtApplicationAndEachTick) {
 PetBattleFixture f;f.Skill(901,BattleEffectOp::PeriodicDamage);auto& row=f.data->AddBuff(901);
 row.set_state_kind(static_cast<uint32_t>(BattleStateKind::Poison));row.set_application_chance(1);row.set_tick_count(3);row.set_tick_on_apply(1);
 f.Pet().mutable_pet_cultivation()->set_poison_damage_bps(1000);f.Pet().mutable_pet_cultivation()->set_magic_damage_bps(1000);
 TurnBattleEngine engine(f.data);ASSERT_TRUE(engine.Initialize(f.request));const auto result=f.Round(engine,901);bool applied=false,ticked=false;
 const auto snapshot=engine.BuildStateSnapshot();
 for(const auto& actor:snapshot.actors())if(actor.actor_id()==201)for(const auto& buff:actor.buffs())if(buff.buff_table_id()==901){EXPECT_EQ(buff.snapshot_value(),110u);applied=true;}
 for(const auto& event:result.events())if(event.event_type()==BATTLE_EVENT_DAMAGE && event.source_id()==f.PetId(engine) && event.damage_origin()==static_cast<uint32_t>(BattleDamageOrigin::Periodic)){
  // The authoritative poison snapshot is 110; this fixture's target defends.
  EXPECT_EQ(event.value(),55u);ticked=true;
 }
 EXPECT_TRUE(applied);EXPECT_TRUE(ticked);
}
TEST(PetCultivationBattle, TypedAccuracyImprovesOnlyMatchingControlAcrossDeterministicSeeds) {
 uint32_t base=0,matching=0,unrelated=0;
 for(uint64_t seed=1;seed<=48;seed++)for(uint32_t variant=0;variant<3;variant++) {
  PetBattleFixture f;f.request.set_seed(seed);f.Skill(901,BattleEffectOp::Control);auto& row=f.data->AddBuff(901);
  row.set_state_kind(static_cast<uint32_t>(BattleStateKind::Confusion));row.set_application_chance(.20);
  if(variant>0)(*f.Pet().mutable_pet_cultivation()->mutable_ailment_hit_bps())[variant==1?4:7]=5000;
  TurnBattleEngine engine(f.data);ASSERT_TRUE(engine.Initialize(f.request));const auto result=f.Round(engine,901);bool success=false;
  for(const auto& event:result.events())if(event.event_type()==BATTLE_EVENT_BUFF_ADD && event.buff_table_id()==901 && event.source_id()==f.PetId(engine))success=true;
  if(success){if(variant==0)base++;else if(variant==1)matching++;else unrelated++;}
 }
 EXPECT_GT(matching,base);EXPECT_EQ(unrelated,base);EXPECT_GT(base,0u);EXPECT_LT(matching,48u);
}
TEST(PetCultivationBattle, EnemyPetResistanceAndRetreatChannelsModifyOnlyTheirOwnProbability) {
 for(const auto op:{BattleEffectOp::Control,BattleEffectOp::RetreatPet}) {
  uint32_t counts[3]{};
  for(uint64_t seed=1;seed<=48;seed++)for(uint32_t variant=0;variant<3;variant++) {
   PetBattleFixture f;f.request.set_seed(seed);f.Skill(901,op);auto& row=f.data->AddBuff(901);row.set_application_chance(.4);
   if(op==BattleEffectOp::Control)row.set_state_kind(static_cast<uint32_t>(BattleStateKind::Confusion));
   auto* enemy=f.request.mutable_players(1)->add_pets();*enemy=f.Pet();enemy->set_pet_id(902);enemy->set_owner_player_id(201);enemy->mutable_base_attributes()->set_speed(1);
   if(variant==1){if(op==BattleEffectOp::Control)(*f.Pet().mutable_pet_cultivation()->mutable_ailment_hit_bps())[4]=3000;else f.Pet().mutable_pet_cultivation()->set_retreat_hit_bps(3000);}
   if(variant==2){if(op==BattleEffectOp::Control)(*enemy->mutable_pet_cultivation()->mutable_ailment_resist_bps())[4]=3000;else enemy->mutable_pet_cultivation()->set_retreat_resist_bps(3000);}
   TurnBattleEngine engine(f.data);ASSERT_TRUE(engine.Initialize(f.request));auto state=engine.BuildStateSnapshot();uint64_t source=f.PetId(engine),target=0;
   for(const auto& a:state.actors()){BattleAction wait;wait.set_action_type(BATTLE_ACTION_DEFEND);engine.SubmitAction(a.actor_id(),wait);if(a.actor_type()==BATTLE_ACTOR_TYPE_PET && a.owner_player_id()==201)target=a.actor_id();}
   ASSERT_NE(target,0u);BattleAction cast;cast.set_action_type(BATTLE_ACTION_SKILL);cast.set_skill_table_id(901);cast.set_target_id(target);ASSERT_EQ(engine.ValidateAction(source,cast),kSuccess);engine.SubmitAction(source,cast);
   const auto result=engine.ResolveCurrentRound();bool success=false;
   for(const auto& event:result.events())if(event.source_id()==source && event.target_id()==target){
    if(op==BattleEffectOp::Control && event.event_type()==BATTLE_EVENT_BUFF_ADD && event.buff_table_id()==901)success=true;
    if(op==BattleEffectOp::RetreatPet && event.event_type()==BATTLE_EVENT_PET_RETREAT && event.success())success=true;
   }
   if(success)counts[variant]++;
  }
  EXPECT_GT(counts[1],counts[0]);EXPECT_LT(counts[2],counts[0]);
 }
}

TEST(PetCultivationBattle, MixedPhysicalControlKeepsOneActionGroupAndCasterSource) {
 bool sawMatched=false;
 for(uint64_t seed=1;seed<=32 && !sawMatched;seed++) {
  PetBattleFixture f;f.request.set_seed(seed);f.Skill(901,BattleEffectOp::Damage).set_damage_type(combatdamage::kPhysicalDamage);f.data->AddSkill(901).add_effect(902);
  auto& control=f.data->AddBuff(902);control.set_battle_op(static_cast<uint32_t>(BattleEffectOp::Control));control.set_state_kind(3);control.set_application_chance(.5);control.set_duration_rounds(2);control.set_max_layer(1);
  f.Pet().mutable_pet_cultivation()->set_physical_damage_bps(1000);(*f.Pet().mutable_pet_cultivation()->mutable_ailment_hit_bps())[3]=500;
  TurnBattleEngine engine(f.data);ASSERT_TRUE(engine.Initialize(f.request));auto result=f.Round(engine,901);const auto pet=f.PetId(engine);
  const BattleEventItem* damage=nullptr;const BattleEventItem* applied=nullptr;
  for(const auto& event:result.events()){
   if(event.event_type()==BATTLE_EVENT_DAMAGE && event.source_id()==pet)damage=&event;
   if(event.event_type()==BATTLE_EVENT_BUFF_ADD && event.buff_table_id()==902)applied=&event;
  }
  // 目标为玩家，按现行 PvP 系数结算：(100 底伤 + 100 攻击) × 0.3 × 1.1。
  ASSERT_NE(damage,nullptr);EXPECT_EQ(damage->target_id(),201u);EXPECT_EQ(damage->value(),66u);
  if(applied){EXPECT_EQ(applied->source_id(),pet);EXPECT_EQ(applied->target_id(),201u);EXPECT_EQ(applied->group_id(),damage->group_id());sawMatched=true;}
 }
 EXPECT_TRUE(sawMatched);
}
TEST(PetCultivationBattle, AnotherCasterConsumesOriginalPetPoisonSnapshotWithoutReapplyingDamageBonus) {
 PetBattleFixture f;f.Skill(901,BattleEffectOp::PeriodicDamage);auto& poison=f.data->AddBuff(901);poison.set_state_kind(7);poison.set_application_chance(1);poison.set_tick_count(5);poison.set_tick_on_apply(1);(*poison.mutable_tag())["Poison"]=true;
 f.Pet().mutable_pet_cultivation()->set_poison_damage_bps(1000);f.Pet().mutable_pet_cultivation()->set_magic_damage_bps(1000);
 auto& skill=f.data->AddSkill(903);skill.set_battle_model(1);skill.set_caster_mask(1);skill.set_ability_kind(1);skill.set_target_team(1);skill.set_max_targets(1);skill.set_unlock_level(1);skill.set_max_rank(10);skill.add_skill_type(1);skill.add_effect(903);
 auto& consume=f.data->AddBuff(903);consume.set_battle_op(static_cast<uint32_t>(BattleEffectOp::ConsumePeriodic));consume.set_required_tag("Poison");consume.set_effect_count(1);
 auto* owner=f.request.mutable_players(0);owner->add_skill_table_ids(903);(*owner->mutable_skill_ranks())[903]=1;
 TurnBattleEngine engine(f.data);ASSERT_TRUE(engine.Initialize(f.request));f.Round(engine,901);const auto pet=f.PetId(engine);
 auto state=engine.BuildStateSnapshot();for(const auto& actor:state.actors()){BattleAction wait;wait.set_action_type(BATTLE_ACTION_DEFEND);engine.SubmitAction(actor.actor_id(),wait);}
 BattleAction action;action.set_action_type(BATTLE_ACTION_SKILL);action.set_skill_table_id(903);action.set_target_id(201);ASSERT_EQ(engine.ValidateAction(101,action),kSuccess);engine.SubmitAction(101,action);
 auto result=engine.ResolveCurrentRound();uint32_t ticks=0;bool consumedByOwner=false;
 for(const auto& event:result.events())if(event.event_type()==BATTLE_EVENT_DAMAGE && event.damage_origin()==static_cast<uint32_t>(BattleDamageOrigin::Periodic)){
  // 主人先行动催动毒时目标尚未防御；回合末自然跳伤才受到本轮防御减伤。
  EXPECT_EQ(event.source_id(),pet);EXPECT_EQ(event.target_id(),201u);EXPECT_EQ(event.skill_table_id(),901u);EXPECT_EQ(event.value(),event.instigator_id()==101 ? 110u : 55u);ticks++;if(event.instigator_id()==101)consumedByOwner=true;
 }
 EXPECT_GE(ticks,1u);EXPECT_TRUE(consumedByOwner);
}

TEST(PetCultivationBattle, HitLimitedFiniteShieldReceivesCapacityBonusWithoutChangingHitsOrManaShield) {
 // B05/B06 型有限数值护盾(带 hit_limit=2)应吃 504 护盾容量加成，次数与持续回合不变。
 PetBattleFixture f;
 f.Skill(905, BattleEffectOp::Shield, true);
 auto& shieldRow = f.data->AddBuff(905);
 shieldRow.set_hit_limit(2);
 shieldRow.set_duration_rounds(3);
 shieldRow.set_tick_on_apply(1);
 f.Pet().mutable_pet_cultivation()->set_shield_capacity_bps(1000);
 TurnBattleEngine engine(f.data);
 ASSERT_TRUE(engine.Initialize(f.request));
 const auto result = f.Round(engine, 905, true);
 bool foundEvent = false;
 for (const auto& event : result.events()) {
  if (event.event_type() == BATTLE_EVENT_BUFF_ADD && event.buff_table_id() == 905) {
   EXPECT_EQ(event.shield_after(), 110u);
   foundEvent = true;
  }
 }
 EXPECT_TRUE(foundEvent);
 bool foundBuff = false;
 for (const auto& actor : result.state().actors()) {
  if (actor.actor_id() != f.PetId(engine)) continue;
  for (const auto& buff : actor.buffs()) {
   if (buff.buff_table_id() == 905) {
    EXPECT_EQ(buff.remaining_capacity(), 110u);
    EXPECT_EQ(buff.remaining_hits(), 2u);
    EXPECT_EQ(buff.remain_rounds(), 2u);
    foundBuff = true;
   }
  }
 }
 EXPECT_TRUE(foundBuff);
}

TEST(PetCultivationBattle, InvalidCultivationSnapshotIsRejectedAtBattleEntry) {
 PetBattleFixture f;
 f.Pet().mutable_pet_cultivation()->set_physical_damage_bps(10001);
 TurnBattleEngine badChannel(f.data);
 EXPECT_FALSE(badChannel.Initialize(f.request));

 f.Pet().mutable_pet_cultivation()->set_physical_damage_bps(500);
 (*f.Pet().mutable_pet_cultivation()->mutable_ailment_hit_bps())[8] = 100;
 TurnBattleEngine badStateKey(f.data);
 EXPECT_FALSE(badStateKey.Initialize(f.request));
}
