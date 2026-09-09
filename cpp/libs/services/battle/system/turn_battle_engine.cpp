#include "system/turn_battle_engine.h"

#include <algorithm>
#include <cmath>

#include "data/table_battle_data_provider.h"
#include "muduo/base/Logging.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/skill_error_tip.pb.h"

// 回合制战斗确定性引擎实现(设计文档 §5.1)。
//
// 技能流程镜像实时战斗 SkillSystem/BuffSystem(cpp/libs/services/scene/combat/),
// 差异点:
//   1. 一切时间维度换算为回合(RoundsFrom*,kRoundDurationMs=6000),无任何 timer;
//   2. 去掉 casting/recovery/channel 相位与对应校验(CheckCasting/CheckRecovery/CheckChannel);
//   3. 所有随机只走成员 RNG(mt19937_64(seed)),禁 tlsRandom/rand();
//   4. 引擎内不打日志(纯逻辑库,校验失败以错误码/静默降级表达,由 battle 节点记日志);
//      唯一例外:Initialize 建房参数违规打 ERROR(编排层错误须运维可见,设计文档 §11 D14)。

namespace turnbattle {

namespace {

// 快照未携带 max_health/max_mana 时的保守回退:以当前值为上限
uint64_t FallbackMax(uint64_t declaredMax, uint64_t currentValue) {
    return declaredMax > 0 ? declaredMax : currentValue;
}

}  // namespace

TurnBattleEngine::TurnBattleEngine() : TurnBattleEngine(MakeTableBattleDataProvider()) {}

TurnBattleEngine::TurnBattleEngine(std::shared_ptr<BattleDataProvider> provider)
    : dataProvider(std::move(provider)) {}

// ---------------------------------------------------------------------------
// 初始化
// ---------------------------------------------------------------------------

bool TurnBattleEngine::Initialize(const CreateBattleRequest& request) {
    // 引擎实例一次性使用:一场战斗一个引擎,禁止复用后重置
    if (initialized || dataProvider == nullptr) {
        return false;
    }
    if (request.battle_id() == 0 || request.players_size() == 0) {
        return false;
    }

    // 每队玩家数上限双侧强制(D14):match 收口 + 引擎兜底,超编直接拒绝建房;
    // team_index 越界交给 InitPlayers 统一兜住,这里只数合法队伍
    uint32_t teamPlayerCounts[2] = {0, 0};
    for (const auto& snapshot : request.players()) {
        if (snapshot.team_index() <= 1) {
            ++teamPlayerCounts[snapshot.team_index()];
        }
    }
    for (uint32_t teamIndex = 0; teamIndex < 2; ++teamIndex) {
        if (teamPlayerCounts[teamIndex] > kMaxBattleTeamSize) {
            LOG_ERROR << "CreateBattle 队伍人数超限: battle_id=" << request.battle_id()
                      << " team_index=" << teamIndex
                      << " players=" << teamPlayerCounts[teamIndex]
                      << " limit=" << kMaxBattleTeamSize;
            return false;
        }
    }

    createRequest = request;
    rng.seed(request.seed());
    roundIndex = 1;
    outcome = BATTLE_OUTCOME_ONGOING;

    // 回合上限:DungeonTable.time_limit(秒)换算,缺省 30 回合
    maxRounds = kDefaultMaxRounds;
    if (const auto* dungeonRow = dataProvider->FindDungeon(request.battle_config_id());
        dungeonRow != nullptr && dungeonRow->time_limit() > 0) {
        maxRounds = RoundsFromMilliseconds(static_cast<uint64_t>(dungeonRow->time_limit()) * 1000);
    }

    if (!InitPlayers(request)) {
        return false;
    }

    // 宝宝必须在玩家之后追加:阵位按插入序排,主人先站前排
    if (!InitPets(request)) {
        return false;
    }

    const bool isPve =
        request.match_mode() == kMatchModePveSolo || request.match_mode() == kMatchModePveTeam;
    if (isPve && !InitMonsters(request)) {
        return false;
    }

    // 两侧都必须有单位,否则开局即无意义(PVP 快照分边错误也在此兜住)
    bool hasSideA = false;
    bool hasSideB = false;
    for (const auto& actor : actors) {
        (actor.team_index() == 0 ? hasSideA : hasSideB) = true;
    }
    if (!hasSideA || !hasSideB) {
        return false;
    }

    initialized = true;
    return true;
}

bool TurnBattleEngine::InitPlayers(const CreateBattleRequest& request) {
    for (const auto& snapshot : request.players()) {
        if (snapshot.player_id() == 0 || snapshot.team_index() > 1) {
            return false;
        }
        if (FindActor(snapshot.player_id()) != nullptr) {
            return false;  // 重复参战
        }

        BattleActorState actor;
        actor.set_actor_id(snapshot.player_id());
        actor.set_actor_type(BATTLE_ACTOR_TYPE_PLAYER);
        actor.set_team_index(snapshot.team_index());
        actor.set_name(snapshot.player_name());
        actor.set_level(snapshot.level());
        *actor.mutable_attributes() = snapshot.base_attributes();
        actor.set_max_health(FallbackMax(snapshot.max_health(), snapshot.base_attributes().health()));
        actor.set_max_mana(FallbackMax(snapshot.max_mana(), snapshot.base_attributes().mana()));
        // 二级属性(属性加点系统):怪物不带 = 0,公式退化为老口径
        actor.set_physical_attack(snapshot.physical_attack());
        actor.set_magic_attack(snapshot.magic_attack());
        actor.set_defense(snapshot.defense());
        // 阵位(表现规格 D4):按快照顺序在本队内 0.. 递增,前 5 个前排、之后后排
        actor.set_formation_slot(NextFormationSlot(snapshot.team_index()));
        // 参战 buff 副本:remain_rounds 已是回合口径(快照契约,scene 侧负责换算)
        for (const auto& buff : snapshot.buffs()) {
            *actor.add_buffs() = buff;
            // 快照携带的 buff 实例 id 也纳入局内自增域,避免与新加 buff 撞 id
            if (buff.buff_id() >= nextBuffInstanceId) {
                nextBuffInstanceId = buff.buff_id() + 1;
            }
        }
        for (const auto skillTableId : snapshot.skill_table_ids()) {
            actor.add_skill_table_ids(skillTableId);
        }
        actors.emplace_back(std::move(actor));

        auto& settlement = settlements[snapshot.player_id()];
        settlement.set_battle_id(request.battle_id());
        settlement.set_player_id(snapshot.player_id());
    }
    return true;
}

bool TurnBattleEngine::InitPets(const CreateBattleRequest& request) {
    for (const auto& snapshot : request.players()) {
        for (const auto& pet : snapshot.pets()) {
            if (pet.pet_id() == 0) {
                return false;  // 快照非法:scene 不应产出无 id 的宝宝
            }
            // pet_id 与 player_id 是**两套互不兼容的 SnowFlake 布局**(pet_id 走 scene 的
            // item guid 发号器:17-bit worker / 秒级 epoch;player_id 走 login 的
            // bwmarrin 布局:13-bit node / 毫秒 epoch,见 AGENTS §7 不变量 1)。
            // 两个数值域理论上可以相交,所以这里不是"断言不会撞",而是**查重后 fail-closed**:
            // 撞了就拒绝开局,绝不让一只宝宝顶替掉某个玩家的 actor。
            if (FindActor(pet.pet_id()) != nullptr) {
                LOG_ERROR << "CreateBattle 宝宝 actor_id 与既有单位相撞: battle_id="
                          << request.battle_id() << " pet_id=" << pet.pet_id();
                return false;
            }

            BattleActorState actor;
            actor.set_actor_id(pet.pet_id());
            actor.set_actor_type(BATTLE_ACTOR_TYPE_PET);
            // 与主人同队:快照的 team_index 由 match 改写,宝宝只跟随,不自带阵营
            actor.set_team_index(snapshot.team_index());
            // 归属**只认快照的主人**:宝宝挂在谁的快照里就是谁的。快照自带的 owner_player_id
            // 只用来对账,不当权威 —— 让它改写归属的话,一份被改过的快照就能把宝宝的战后结算
            // 记到别人名下。对不上直接拒绝开局。
            if (pet.owner_player_id() != 0 && pet.owner_player_id() != snapshot.player_id()) {
                LOG_ERROR << "CreateBattle 宝宝归属与所在快照不一致: battle_id=" << request.battle_id()
                          << " pet_id=" << pet.pet_id() << " owner=" << pet.owner_player_id()
                          << " snapshot_player=" << snapshot.player_id();
                return false;
            }
            actor.set_owner_player_id(snapshot.player_id());
            actor.set_pet_table_id(pet.pet_table_id());
            actor.set_name(pet.pet_name());
            actor.set_level(pet.level());
            *actor.mutable_attributes() = pet.base_attributes();
            actor.set_max_health(FallbackMax(pet.max_health(), pet.base_attributes().health()));
            actor.set_max_mana(FallbackMax(pet.max_mana(), pet.base_attributes().mana()));
            actor.set_physical_attack(pet.physical_attack());
            actor.set_magic_attack(pet.magic_attack());
            actor.set_defense(pet.defense());
            actor.set_formation_slot(NextFormationSlot(snapshot.team_index()));
            // 宝宝没有客户端行动权:标 auto,让默认行动路径(零随机分支之外的普攻)代打。
            // 这同时保证 AllPlayersReady 不会因为宝宝没提交行动而卡住整个回合。
            actor.set_is_auto(true);
            for (const auto skillTableId : pet.skill_table_ids()) {
                actor.add_skill_table_ids(skillTableId);
            }
            actors.emplace_back(std::move(actor));
        }
    }
    return true;
}

bool TurnBattleEngine::InitMonsters(const CreateBattleRequest& request) {
    // 参考等级取玩家侧最高等级(怪物属性表列缺失时的保守基准)
    uint32_t referenceLevel = 1;
    for (const auto& snapshot : request.players()) {
        referenceLevel = std::max(referenceLevel, snapshot.level());
    }

    auto monsterIds = dataProvider->GetDungeonMonsterIds(request.battle_config_id());
    if (monsterIds.empty()) {
        // DungeonTable 缺怪物组列:按玩家人数生成默认怪,保证 PVE 一定有对手
        monsterIds.assign(static_cast<size_t>(request.players_size()), 0);
    }

    uint32_t monsterIndex = 0;
    for (const auto monsterTableId : monsterIds) {
        AppendMonsterActor(monsterTableId, monsterIndex, referenceLevel);
        ++monsterIndex;
    }
    return true;
}

void TurnBattleEngine::AppendMonsterActor(uint32_t monsterTableId, uint32_t monsterIndex,
                                          uint32_t referenceLevel) {
    BattleActorState actor;
    actor.set_actor_id(kMonsterActorIdBase + monsterIndex);  // 局内序号 id,确定性
    actor.set_actor_type(BATTLE_ACTOR_TYPE_MONSTER);
    actor.set_team_index(1);
    actor.set_name("野怪");
    actor.set_level(referenceLevel);
    actor.set_monster_table_id(monsterTableId);
    // 阵位(D4):按副本怪物组顺序在 B 方内 0.. 递增(PVE 的 B 方只有怪物,
    // 故等于 monsterIndex;若 B 方已有玩家则接在其后,保证队内唯一)
    actor.set_formation_slot(NextFormationSlot(1));

    // 优先读 MonsterTable 属性;行缺失(如 monster_table_id=0 的兜底怪)才回退
    // kMonsterDefault* 常量,保证任何配置下 PVE 都有能打的对手。
    const MonsterTable* row = dataProvider->FindMonster(monsterTableId);
    auto* attributes = actor.mutable_attributes();
    if (row != nullptr && row->health() > 0) {
        attributes->set_health(row->health());
        attributes->set_strength(row->strength());
        attributes->set_armor(row->armor());
        attributes->set_resistance(row->resistance());
        attributes->set_critchance(row->critchance());
        attributes->set_speed(row->speed() > 0 ? row->speed() : kMonsterDefaultSpeed);
        actor.set_max_health(row->health());
    } else {
        attributes->set_health(kMonsterDefaultHealth);
        attributes->set_strength(kMonsterDefaultStrength);
        attributes->set_armor(kMonsterDefaultArmor);
        attributes->set_resistance(kMonsterDefaultResistance);
        attributes->set_critchance(kMonsterDefaultCritChance);
        attributes->set_speed(kMonsterDefaultSpeed);
        actor.set_max_health(kMonsterDefaultHealth);
    }

    actors.emplace_back(std::move(actor));
}

uint32_t TurnBattleEngine::NextFormationSlot(uint32_t teamIndex) const {
    // 阵位 = 该队已有单位数:插入序即阵位序,天然队内唯一;
    // 0..kFormationFrontRowSize-1 前排,其后后排(表现规格 D4,不参与任何判定)
    uint32_t count = 0;
    for (const auto& actor : actors) {
        if (actor.team_index() == teamIndex) {
            ++count;
        }
    }
    return count;
}

// ---------------------------------------------------------------------------
// 行动收集
// ---------------------------------------------------------------------------

bool TurnBattleEngine::SubmitAction(uint64_t actorId, const BattleAction& action) {
    if (!initialized || outcome != BATTLE_OUTCOME_ONGOING) {
        return false;
    }

    // 只收玩家行动;死亡/已逃单位不再有行动权
    const auto* actor = FindActor(actorId);
    if (actor != nullptr && actor->actor_type() == BATTLE_ACTOR_TYPE_PLAYER && IsActorActive(*actor)) {
        // 校验链不过的行动不落账,按未提交处理(回合超时默认普攻兜底);
        // 截止前允许重复提交改主意,最后一次为准
        if (CheckActionPrerequisites(*actor, action) == kSuccess) {
            pendingActions[actorId] = action;
        }
    }

    return AllPlayersReady();
}

uint32_t TurnBattleEngine::SetActorAuto(uint64_t actorId, bool enabled) {
    // 只翻转状态位,不消耗 RNG、不触发结算:确定性由默认行动路径保证,
    // "开 auto 后立即结算"由节点在查 AllPlayersReady 后自行调 ResolveCurrentRound
    if (!initialized || outcome != BATTLE_OUTCOME_ONGOING) {
        return kInvalidParameter;
    }
    auto* actor = FindActor(actorId);
    if (actor == nullptr || actor->actor_type() != BATTLE_ACTOR_TYPE_PLAYER) {
        return kInvalidParameter;
    }
    // 与 CheckState 同口径:死亡/已逃单位无行动权,自然也无挂机语义
    if (actor->is_dead() || actor->fled()) {
        return kThisEntityIsInvalid;
    }
    actor->set_is_auto(enabled);
    return 0;
}

bool TurnBattleEngine::AllPlayersReady() const {
    for (const auto& actor : actors) {
        if (actor.actor_type() != BATTLE_ACTOR_TYPE_PLAYER || !IsActorActive(actor)) {
            continue;
        }
        // 挂机单位由默认行动路径代打,视为已就绪(D12)
        if (actor.is_auto()) {
            continue;
        }
        if (pendingActions.find(actor.actor_id()) == pendingActions.end()) {
            return false;
        }
    }
    return true;
}

// ---------------------------------------------------------------------------
// 行动校验链(镜像 SkillSystem::CheckSkillPrerequisites,去掉相位校验)
// ---------------------------------------------------------------------------

uint32_t TurnBattleEngine::CheckActionPrerequisites(const BattleActorState& actor,
                                                    const BattleAction& action) const {
    if (const auto stateResult = CheckState(actor); stateResult != kSuccess) {
        return stateResult;
    }

    switch (action.action_type()) {
    case BATTLE_ACTION_ATTACK:
    case BATTLE_ACTION_DEFEND:
        // 普攻目标失效时结算期会重选,防御无目标,状态检查通过即可
        return kSuccess;
    case BATTLE_ACTION_FLEE: {
        // PVE 可逃,PVP 一期不可逃(设计文档 §5.1)
        const bool isPve = createRequest.match_mode() == kMatchModePveSolo ||
                           createRequest.match_mode() == kMatchModePveTeam;
        return isPve ? kSuccess : kInvalidParameter;
    }
    case BATTLE_ACTION_ITEM: {
        // 道具从快照副本扣:提交时校验持有数
        for (const auto& snapshot : createRequest.players()) {
            if (snapshot.player_id() != actor.actor_id()) {
                continue;
            }
            for (const auto& item : snapshot.items()) {
                if (item.item_table_id() == action.item_table_id() && item.count() > 0) {
                    return kSuccess;
                }
            }
        }
        return kInvalidParameter;
    }
    case BATTLE_ACTION_SKILL: {
        const auto* skillRow = dataProvider->FindSkill(action.skill_table_id());
        if (skillRow == nullptr) {
            return kInvalidTableId;
        }
        // 快照携带的技能列表即战斗内可用技能全集
        const auto& ownedSkills = actor.skill_table_ids();
        if (std::find(ownedSkills.begin(), ownedSkills.end(), action.skill_table_id()) ==
            ownedSkills.end()) {
            return kInvalidParameter;
        }
        if (const auto result = ValidateSkillTarget(actor, *skillRow, action.target_id());
            result != kSuccess) {
            return result;
        }
        if (const auto result = CheckCooldown(actor, *skillRow); result != kSuccess) {
            return result;
        }
        if (const auto result = CheckPlayerLevel(actor, *skillRow); result != kSuccess) {
            return result;
        }
        if (const auto result = CheckBuff(actor, *skillRow); result != kSuccess) {
            return result;
        }
        if (const auto result = CheckSkillCost(actor, *skillRow); result != kSuccess) {
            return result;
        }
        return kSuccess;
    }
    default:
        return kInvalidParameter;
    }
}

uint32_t TurnBattleEngine::CheckSkillCost(const BattleActorState& actor,
                                          const SkillTable& skillRow) const {
    // 耗蓝不足:提交期不落账(回合超时默认普攻兜底),出手期降级为普攻(ExecuteSkill)。
    // tip 表暂无"法力不足"专用码,借用"当前状态不可施放"(见任务 open_issues)
    if (SkillManaCost(skillRow) > actor.attributes().mana()) {
        return kSkillCannotBeCastInCurrentState;
    }
    return kSuccess;
}

uint32_t TurnBattleEngine::ValidateSkillTarget(const BattleActorState& actor,
                                               const SkillTable& skillRow,
                                               uint64_t targetId) const {
    (void)actor;
    // 镜像 SkillSystem::ValidateTarget:targeting_mode 存位号
    if (!skillRow.targeting_mode().empty() && targetId == 0) {
        return kSkillInvalidTargetId;
    }

    for (const auto modeBit : skillRow.targeting_mode()) {
        const uint32_t targetingMode = 1u << modeBit;

        // 无目标 / AOE 技能不需要具体目标单位
        if (targetingMode == kTargetingNoTargetRequired || targetingMode == kTargetingAreaOfEffect) {
            return kSuccess;
        }
        if (targetingMode != kTargetingTargetedSkill) {
            continue;
        }

        const auto* target = FindActor(targetId);
        if (target == nullptr || !IsActorActive(*target)) {
            return kSkillInvalidTargetId;
        }
        return kSuccess;
    }

    return kSuccess;
}

uint32_t TurnBattleEngine::CheckCooldown(const BattleActorState& actor,
                                         const SkillTable& skillRow) const {
    // 冷却按 cooldown_id 分组共享(镜像实时的 CooldownTimeListComp 键语义);
    // 状态 map 的键按 proto 契约是 skill_table_id,分组语义靠反查各在冷技能的 cooldown_id。
    // map 遍历序不定,但结果是"是否存在"布尔,不影响确定性
    for (const auto& coolingEntry : actor.skill_cooldown_rounds()) {
        if (coolingEntry.second == 0) {
            continue;
        }
        const auto* coolingRow = dataProvider->FindSkill(coolingEntry.first);
        if (coolingRow != nullptr && coolingRow->cooldown_id() == skillRow.cooldown_id()) {
            return kSkillCooldownNotReady;
        }
    }
    return kSuccess;
}

uint32_t TurnBattleEngine::CheckPlayerLevel(const BattleActorState& actor,
                                            const SkillTable& skillRow) const {
    // 实时侧同名校验尚未实现(TODO 挂点),此处保持同口径:恒通过
    (void)actor;
    (void)skillRow;
    return kSuccess;
}

uint32_t TurnBattleEngine::CheckBuff(const BattleActorState& actor,
                                     const SkillTable& skillRow) const {
    // 镜像 SkillSystem::CheckBuff + CanUseSkillInCurrentState:
    // 战斗状态由 buff 派生(目前唯一入 SkillPermission 的状态是沉默),
    // SkillPermission.skill_type 列按技能类型位号平铺,格值即许可结果(kSuccess=放行)
    if (!ActorHasBuffOfType(actor, kBuffTypeSilence)) {
        return kSuccess;
    }

    const auto* permissionRow = dataProvider->FindSkillPermission(kCombatStateSilence);
    if (permissionRow == nullptr) {
        return kInvalidTableData;
    }

    for (const auto skillTypeBit : skillRow.skill_type()) {
        const auto columnIndex = static_cast<int32_t>(skillTypeBit);
        if (columnIndex >= permissionRow->skill_type_size()) {
            return kInvalidTableData;
        }
        if (const auto cell = permissionRow->skill_type(columnIndex); cell != kSuccess) {
            return cell;
        }
    }
    return kSuccess;
}

uint32_t TurnBattleEngine::CheckState(const BattleActorState& actor) const {
    if (actor.is_dead() || actor.fled()) {
        return kThisEntityIsInvalid;
    }
    // 眩晕/冰冻:本回合完全无法行动(相位概念已删,控制类语义收敛到这里)
    if (ActorHasBuffOfType(actor, kBuffTypeStun) || ActorHasBuffOfType(actor, kBuffTypeFreeze)) {
        return kSkillCannotBeCastStunRestriction;
    }
    return kSuccess;
}

// ---------------------------------------------------------------------------
// 回合结算
// ---------------------------------------------------------------------------

TurnResultS2C TurnBattleEngine::ResolveCurrentRound() {
    TurnResultS2C result;
    if (!initialized) {
        return result;
    }

    result.set_battle_id(createRequest.battle_id());
    result.set_round_index(roundIndex);

    if (outcome != BATTLE_OUTCOME_ONGOING) {
        *result.mutable_state() = BuildStateSnapshot();
        return result;
    }

    // 1. 未提交者(含掉线/超时玩家与全部怪物)填默认普攻
    FillDefaultActions();

    // 事件组编号每回合从 1 起(表现规格 D2);出手序在回合开始时一次排定并保留下来
    // (表现规格 D3:含回合中途死亡/逃离而被跳过者,节点透传到 TurnResultS2C.action_order)
    currentGroupId = 0;
    currentHitIndex = 0;
    lastActionOrder = BuildTurnOrder();

    // 2. 速度降序、同速按 actor_id 稳定序,逐个执行;死亡/已逃单位跳过
    for (const auto actorId : lastActionOrder) {
        auto* actor = FindActor(actorId);
        if (actor == nullptr || !IsActorActive(*actor)) {
            continue;
        }
        const auto actionIt = pendingActions.find(actorId);
        if (actionIt == pendingActions.end()) {
            continue;
        }
        // 每个行动一组:该行动产生的全部事件(含降级普攻/死亡/buff)共用 group_id
        BeginEventGroup();
        ExecuteAction(*actor, actionIt->second, result);
    }

    // 3. 回合末 buff tick:周期效果 → 持续减一 → 到期移除
    TickBuffsAtRoundEnd(result);

    // 4. 冷却回合递减
    DecayCooldowns();

    // 5. 防御只覆盖本回合(含刚结算的回合末周期伤害),此刻统一摘除
    for (auto& actor : actors) {
        actor.set_is_defending(false);
    }

    // 6. 胜负判定(含 max_rounds 打满进攻方判负)
    UpdateOutcome();

    pendingActions.clear();
    if (outcome == BATTLE_OUTCOME_ONGOING) {
        ++roundIndex;
    }

    *result.mutable_state() = BuildStateSnapshot();
    return result;
}

void TurnBattleEngine::FillDefaultActions() {
    for (const auto& actor : actors) {
        if (!IsActorActive(actor)) {
            continue;
        }
        if (pendingActions.find(actor.actor_id()) != pendingActions.end()) {
            continue;
        }
        // 超时/掉线/怪物:默认普攻,目标置 0,结算时用引擎 RNG 随机存活敌方
        BattleAction defaultAction;
        defaultAction.set_action_type(BATTLE_ACTION_ATTACK);
        defaultAction.set_target_id(0);
        pendingActions[actor.actor_id()] = defaultAction;
    }
}

std::vector<uint64_t> TurnBattleEngine::BuildTurnOrder() const {
    std::vector<const BattleActorState*> ordered;
    ordered.reserve(actors.size());
    for (const auto& actor : actors) {
        if (IsActorActive(actor)) {
            ordered.push_back(&actor);
        }
    }
    std::sort(ordered.begin(), ordered.end(),
              [](const BattleActorState* lhs, const BattleActorState* rhs) {
                  if (lhs->attributes().speed() != rhs->attributes().speed()) {
                      return lhs->attributes().speed() > rhs->attributes().speed();
                  }
                  return lhs->actor_id() < rhs->actor_id();
              });

    std::vector<uint64_t> order;
    order.reserve(ordered.size());
    for (const auto* actor : ordered) {
        order.push_back(actor->actor_id());
    }
    return order;
}

void TurnBattleEngine::ExecuteAction(BattleActorState& actor, const BattleAction& action,
                                     TurnResultS2C& result) {
    // 回合中途被眩晕/冰冻:行动作废(不发事件,客户端以 buff 状态解释)
    if (ActorHasBuffOfType(actor, kBuffTypeStun) || ActorHasBuffOfType(actor, kBuffTypeFreeze)) {
        return;
    }

    switch (action.action_type()) {
    case BATTLE_ACTION_ATTACK:
        ExecuteAttack(actor, action.target_id(), result);
        break;
    case BATTLE_ACTION_SKILL:
        ExecuteSkill(actor, action, result);
        break;
    case BATTLE_ACTION_DEFEND:
        ExecuteDefend(actor, result);
        break;
    case BATTLE_ACTION_ITEM:
        ExecuteItem(actor, action, result);
        break;
    case BATTLE_ACTION_FLEE:
        ExecuteFlee(actor, result);
        break;
    default:
        break;
    }
}

void TurnBattleEngine::ExecuteAttack(BattleActorState& actor, uint64_t targetId,
                                     TurnResultS2C& result) {
    // 目标失效(未指定/已死/已逃/是己方)→ 引擎 RNG 随机存活敌方(默认行动同路径)
    auto* target = FindActor(targetId);
    if (target == nullptr || !IsActorActive(*target) || target->team_index() == actor.team_index()) {
        const auto enemyIds = CollectAliveEnemyIds(actor);
        if (enemyIds.empty()) {
            return;  // 敌方已清场,本次行动落空
        }
        targetId = enemyIds[static_cast<size_t>(RandIndex(enemyIds.size()))];
        target = FindActor(targetId);
        if (target == nullptr) {
            return;
        }
    }

    AppendEvent(result, BATTLE_EVENT_ATTACK, actor.actor_id(), targetId);

    // 命中判定(D1):未命中只出 MISS,不出 DAMAGE(一期命中率 100%,永不进入此分支)
    if (!RollHit(actor, *target)) {
        auto* missEvent = AppendEvent(result, BATTLE_EVENT_MISS, actor.actor_id(), targetId);
        missEvent->set_value(0);
        missEvent->set_target_health_after(target->attributes().health());
        missEvent->set_target_mana_after(target->attributes().mana());
        return;
    }

    bool isCritical = false;
    // 普攻吃"物伤"(属性加点二级属性),技能吃"法伤"(见 ExecuteSkill)
    const double finalDamage = CalculateFinalDamage(actor, *target, kBasicAttackBaseDamage,
                                                    actor.physical_attack(), isCritical);
    const uint64_t dealt = ApplyDamage(*target, finalDamage);

    auto* damageEvent = AppendEvent(result, BATTLE_EVENT_DAMAGE, actor.actor_id(), targetId);
    damageEvent->set_value(dealt);
    damageEvent->set_is_critical(isCritical);
    damageEvent->set_target_health_after(target->attributes().health());
    damageEvent->set_target_mana_after(target->attributes().mana());

    if (target->attributes().health() == 0) {
        HandleDeath(*target, result);
    }
}

void TurnBattleEngine::ExecuteSkill(BattleActorState& actor, const BattleAction& action,
                                    TurnResultS2C& result) {
    // 结算前重跑校验链(提交后到出手前,冷却/沉默/目标状态都可能已变化);
    // 校验不过降级为默认普攻,保证回合不空转
    if (CheckActionPrerequisites(actor, action) != kSuccess) {
        ExecuteAttack(actor, action.target_id(), result);
        return;
    }

    const auto* skillRow = dataProvider->FindSkill(action.skill_table_id());
    if (skillRow == nullptr) {
        ExecuteAttack(actor, action.target_id(), result);
        return;
    }

    // 目标集:AOE 技能(targeting_mode 含 AOE 位)对全部存活敌方生效,按 actors 插入序,
    // 不消耗随机数;单体技能沿用"目标失效 → 随机存活敌方"(与普攻同口径,随机数消费不变)
    std::vector<uint64_t> targetIds;
    if (IsAreaSkill(*skillRow)) {
        targetIds = CollectAliveEnemyIds(actor);
        if (targetIds.empty()) {
            return;
        }
    } else {
        uint64_t targetId = action.target_id();
        const auto* target = FindActor(targetId);
        if (target == nullptr || !IsActorActive(*target)) {
            const auto enemyIds = CollectAliveEnemyIds(actor);
            if (enemyIds.empty()) {
                return;
            }
            targetId = enemyIds[static_cast<size_t>(RandIndex(enemyIds.size()))];
            if (FindActor(targetId) == nullptr) {
                return;
            }
        }
        targetIds.push_back(targetId);
    }
    // SKILL 事件的 target 取首目标(单体即唯一目标;AOE 为敌方首位,演出以 DAMAGE 逐目标为准)
    const uint64_t primaryTargetId = targetIds.front();

    // 出手即挂冷却(镜像 SkillSystem::StartCooldown),时长换算为回合;
    // 状态 map 键为 skill_table_id(proto 契约),分组共享语义见 CheckCooldown
    const auto cooldownMs = dataProvider->GetCooldownDurationMs(skillRow->cooldown_id());
    if (cooldownMs > 0) {
        (*actor.mutable_skill_cooldown_rounds())[action.skill_table_id()] =
            RoundsFromMilliseconds(cooldownMs);
    }

    auto* skillEvent = AppendEvent(result, BATTLE_EVENT_SKILL, actor.actor_id(), primaryTargetId);
    skillEvent->set_skill_table_id(action.skill_table_id());

    // 耗蓝(D5):SKILL 事件之后、伤害之前产出 MANA 事件(校验链已保证蓝够)
    ConsumeSkillMana(actor, *skillRow, action.skill_table_id(), result);

    // 伤害:damage 表达式两步调用(SetDamageParam({casterLevel}) + GetDamage),
    // 之后镜像 CalculateFinalDamage 公式;表达式为空/求值为 0 视为纯 buff 技能。
    // 表达式只求值一次,多目标共用同一 base
    const double baseDamage =
        dataProvider->GetSkillDamage(action.skill_table_id(), static_cast<double>(actor.level()));

    // 逐目标落地,hit_index = 目标序(D2:群攻多目标同一 group_id、hit_index 0..n-1)
    for (size_t hitIndex = 0; hitIndex < targetIds.size(); ++hitIndex) {
        auto* target = FindActor(targetIds[hitIndex]);
        if (target == nullptr) {
            continue;
        }
        currentHitIndex = static_cast<uint32_t>(hitIndex);
        ApplySkillToTarget(actor, action, *skillRow, baseDamage, *target, result);
    }
    currentHitIndex = 0;
}

void TurnBattleEngine::ApplySkillToTarget(BattleActorState& actor, const BattleAction& action,
                                          const SkillTable& skillRow, double baseDamage,
                                          BattleActorState& target, TurnResultS2C& result) {
    // 命中判定(D1):未命中只出 MISS,伤害与 effect[] buff 都不落地
    // (一期命中率 100%,永不进入此分支)
    if (!RollHit(actor, target)) {
        auto* missEvent =
            AppendEvent(result, BATTLE_EVENT_MISS, actor.actor_id(), target.actor_id());
        missEvent->set_skill_table_id(action.skill_table_id());
        missEvent->set_value(0);
        missEvent->set_target_health_after(target.attributes().health());
        missEvent->set_target_mana_after(target.attributes().mana());
        return;
    }

    if (baseDamage > 0) {
        bool isCritical = false;
        const double finalDamage =
            CalculateFinalDamage(actor, target, baseDamage, actor.magic_attack(), isCritical);
        const uint64_t dealt = ApplyDamage(target, finalDamage);

        auto* damageEvent =
            AppendEvent(result, BATTLE_EVENT_DAMAGE, actor.actor_id(), target.actor_id());
        damageEvent->set_skill_table_id(action.skill_table_id());
        damageEvent->set_value(dealt);
        damageEvent->set_is_critical(isCritical);
        damageEvent->set_target_health_after(target.attributes().health());
        damageEvent->set_target_mana_after(target.attributes().mana());

        if (target.attributes().health() == 0) {
            HandleDeath(target, result);
        }
    }

    // effect[] → buff(镜像 SkillSystem::TriggerSkillEffect,打在技能目标上);
    // 目标已死则不再挂 buff
    if (!target.is_dead()) {
        for (const auto effectBuffId : skillRow.effect()) {
            AddBuffToActor(target, effectBuffId, actor.actor_id(), 0, result);
        }
    }
}

bool TurnBattleEngine::IsAreaSkill(const SkillTable& skillRow) const {
    // targeting_mode 存位号(与 ValidateSkillTarget 同口径)
    for (const auto modeBit : skillRow.targeting_mode()) {
        if ((1u << modeBit) == kTargetingAreaOfEffect) {
            return true;
        }
    }
    return false;
}

uint64_t TurnBattleEngine::SkillManaCost(const SkillTable& skillRow) const {
    // cost_resource[] 平铺多列,Excel 空格补 {0,0};只认法力资源 id
    uint64_t cost = 0;
    for (const auto& entry : skillRow.cost_resource()) {
        if (entry.cost_resource_id() == kSkillCostResourceMana) {
            cost += entry.cost_resource_cost();
        }
    }
    return cost;
}

void TurnBattleEngine::ConsumeSkillMana(BattleActorState& actor, const SkillTable& skillRow,
                                        uint32_t skillTableId, TurnResultS2C& result) {
    const uint64_t manaCost = SkillManaCost(skillRow);
    if (manaCost == 0) {
        return;  // 无耗蓝技能不产出 MANA 事件
    }
    // 校验链已保证蓝够,这里仍饱和到 0 兜底(纯逻辑库不 assert)
    const uint64_t manaBefore = actor.attributes().mana();
    const uint64_t manaAfter = manaBefore > manaCost ? manaBefore - manaCost : 0;
    actor.mutable_attributes()->set_mana(manaAfter);

    auto* manaEvent = AppendEvent(result, BATTLE_EVENT_MANA, actor.actor_id(), actor.actor_id());
    manaEvent->set_skill_table_id(skillTableId);
    manaEvent->set_value(manaBefore - manaAfter);  // 实际消耗量(客户端按 MANA 语义显示为负)
    manaEvent->set_target_health_after(actor.attributes().health());
    manaEvent->set_target_mana_after(manaAfter);
}

void TurnBattleEngine::ExecuteDefend(BattleActorState& actor, TurnResultS2C& result) {
    actor.set_is_defending(true);
    AppendEvent(result, BATTLE_EVENT_DEFEND, actor.actor_id(), actor.actor_id());
}

void TurnBattleEngine::ExecuteItem(BattleActorState& actor, const BattleAction& action,
                                   TurnResultS2C& result) {
    // 从快照道具副本扣数,结算时经 items_consumed 回写 scene(scene 按实际持有校验,防刷)
    BattleItemEntry* itemEntry = nullptr;
    for (auto& snapshot : *createRequest.mutable_players()) {
        if (snapshot.player_id() != actor.actor_id()) {
            continue;
        }
        for (auto& item : *snapshot.mutable_items()) {
            if (item.item_table_id() == action.item_table_id() && item.count() > 0) {
                itemEntry = &item;
                break;
            }
        }
        break;
    }
    if (itemEntry == nullptr) {
        return;  // 副本内无此道具或已用尽,行动落空
    }

    itemEntry->set_count(itemEntry->count() - 1);

    // 结算账本累加消耗
    auto& settlement = settlements[actor.actor_id()];
    BattleItemEntry* consumedEntry = nullptr;
    for (auto& consumed : *settlement.mutable_items_consumed()) {
        if (consumed.item_table_id() == action.item_table_id()) {
            consumedEntry = &consumed;
            break;
        }
    }
    if (consumedEntry == nullptr) {
        consumedEntry = settlement.add_items_consumed();
        consumedEntry->set_item_table_id(action.item_table_id());
    }
    consumedEntry->set_count(consumedEntry->count() + 1);

    // v1 保守效果:固定回血(ItemTable 未接入回合引擎,见 open_issues)
    const uint64_t healed = ApplyHeal(actor, static_cast<double>(kDefaultItemHealHp));

    auto* itemEvent = AppendEvent(result, BATTLE_EVENT_ITEM, actor.actor_id(), actor.actor_id());
    itemEvent->set_item_table_id(action.item_table_id());
    itemEvent->set_value(healed);
    itemEvent->set_target_health_after(actor.attributes().health());
    itemEvent->set_target_mana_after(actor.attributes().mana());
}

void TurnBattleEngine::ExecuteFlee(BattleActorState& actor, TurnResultS2C& result) {
    const bool isPve = createRequest.match_mode() == kMatchModePveSolo ||
                       createRequest.match_mode() == kMatchModePveTeam;

    bool success = false;
    if (isPve) {
        // 成功率基于速度差:base + 系数 * (自身速度 - 存活敌方最高速度),夹在上下限内
        const double speedDiff = static_cast<double>(actor.attributes().speed()) -
                                 static_cast<double>(MaxAliveEnemySpeed(actor));
        const double chance = std::clamp(kFleeBaseChance + kFleeSpeedFactor * speedDiff,
                                         kFleeMinChance, kFleeMaxChance);
        success = Rand01() < chance;
    }

    auto* fleeEvent = AppendEvent(result, BATTLE_EVENT_FLEE, actor.actor_id(), actor.actor_id());
    fleeEvent->set_success(success);

    if (success) {
        actor.set_fled(true);
    }
}

// ---------------------------------------------------------------------------
// 回合末 buff tick
// ---------------------------------------------------------------------------

void TurnBattleEngine::TickBuffsAtRoundEnd(TurnResultS2C& result) {
    // 按 actors 插入序(玩家在前、怪物在后)逐单位 tick,顺序稳定即确定性
    for (auto& actor : actors) {
        if (!IsActorActive(actor)) {
            continue;
        }
        TickActorBuffs(actor, result);
    }
}

void TurnBattleEngine::TickActorBuffs(BattleActorState& actor, TurnResultS2C& result) {
    // 先快照实例 id 再逐个按 id 重查:tick 过程会增删条目
    // (镜像实时 ProcessBuffs 的防迭代器失效手法)
    std::vector<uint64_t> buffIds;
    buffIds.reserve(static_cast<size_t>(actor.buffs_size()));
    for (const auto& buff : actor.buffs()) {
        buffIds.push_back(buff.buff_id());
    }
    if (buffIds.empty()) {
        return;
    }
    // 回合末结算:每个单位的 buff tick(周期效果/到期移除/致死)独立成组,
    // 与任何行动的 group_id 都不同(表现规格 D2)
    BeginEventGroup();

    for (const auto buffId : buffIds) {
        int index = FindBuffIndex(actor, buffId);
        if (index < 0) {
            continue;  // 本轮 tick 中已被移除
        }
        const auto entrySnapshot = actor.buffs(index);
        const auto* buffRow = dataProvider->FindBuff(entrySnapshot.buff_table_id());
        if (buffRow == nullptr) {
            continue;  // 行缺失只跳过这一条,不影响其余 buff
        }

        // 周期效果:interval 换算为回合;有限时长以自身经过回合数对齐周期,
        // 无限时长(remain_rounds=0 哨兵)以全局回合序号对齐
        if (buffRow->interval() > 0) {
            const uint32_t intervalRounds = RoundsFromSeconds(buffRow->interval());
            uint32_t elapsedRounds;
            if (entrySnapshot.remain_rounds() > 0) {
                const uint32_t totalRounds = RoundsFromSeconds(buffRow->duration());
                elapsedRounds = totalRounds >= entrySnapshot.remain_rounds()
                                    ? totalRounds - entrySnapshot.remain_rounds() + 1
                                    : 1;
            } else {
                elapsedRounds = roundIndex;
            }
            const bool onIntervalBoundary = elapsedRounds % intervalRounds == 0;
            const uint32_t ticksDone = elapsedRounds / intervalRounds;
            // interval_count = 0 表示不限次数(镜像 CanApplyMoreTicks)
            const bool underTickLimit =
                buffRow->interval_count() == 0 || ticksDone <= buffRow->interval_count();
            if (onIntervalBoundary && underTickLimit) {
                ApplyBuffIntervalEffect(actor, entrySnapshot, *buffRow, result);
            }
        }

        // 周期伤害可能致死:死亡时 buff 已整体清空,本单位 tick 就此打住
        if (actor.is_dead()) {
            return;
        }

        // 持续回合递减与到期移除(remain_rounds=0 为无限持续,不递减)
        index = FindBuffIndex(actor, buffId);
        if (index < 0) {
            continue;
        }
        auto* entry = actor.mutable_buffs(index);
        if (entry->remain_rounds() == 0) {
            continue;
        }
        if (entry->remain_rounds() == 1) {
            RemoveBuffAt(actor, index, result);
        } else {
            entry->set_remain_rounds(entry->remain_rounds() - 1);
        }
    }
}

void TurnBattleEngine::ApplyBuffIntervalEffect(BattleActorState& actor,
                                               const BattleBuffEntry& entry,
                                               const BuffTable& buffRow, TurnResultS2C& result) {
    switch (buffRow.buff_type()) {
    case kBuffTypeHealthRegeneration:
    case kBuffTypeHealthRegenerationBasedOnLostHealth: {
        // 镜像 ModifierBuffImplSystem::OnHealthRegenerationBasedOnLostHealth 的参数口径
        const double lostHealth =
            static_cast<double>(actor.max_health() - actor.attributes().health());
        const double healAmount = dataProvider->GetBuffHealthRegeneration(
            buffRow.id(), static_cast<double>(actor.level()), lostHealth);
        const uint64_t healed = ApplyHeal(actor, healAmount);
        if (healed > 0) {
            auto* tickEvent =
                AppendEvent(result, BATTLE_EVENT_BUFF_TICK, entry.caster_id(), actor.actor_id());
            tickEvent->set_buff_table_id(buffRow.id());
            tickEvent->set_value(healed);
            tickEvent->set_target_health_after(actor.attributes().health());
            tickEvent->set_target_mana_after(actor.attributes().mana());
        }
        break;
    }
    case kBuffTypePoison:
    case kBuffTypeBurn: {
        // 周期伤害取 interval_effect[0] * 叠层数(该列在实时侧尚无消费者,
        // 回合侧按此口径启用;列缺省时不产生伤害,见 open_issues)
        if (buffRow.interval_effect_size() == 0) {
            break;
        }
        const double rawDamage =
            buffRow.interval_effect(0) * static_cast<double>(std::max(1u, entry.layer()));
        const uint64_t dealt = ApplyDamage(actor, rawDamage);
        if (dealt > 0) {
            auto* tickEvent =
                AppendEvent(result, BATTLE_EVENT_BUFF_TICK, entry.caster_id(), actor.actor_id());
            tickEvent->set_buff_table_id(buffRow.id());
            tickEvent->set_value(dealt);
            tickEvent->set_target_health_after(actor.attributes().health());
            tickEvent->set_target_mana_after(actor.attributes().mana());
        }
        if (actor.attributes().health() == 0) {
            HandleDeath(actor, result);
        }
        break;
    }
    default:
        // 其余 buff 类型的周期语义一期不启用
        break;
    }
}

void TurnBattleEngine::DecayCooldowns() {
    for (auto& actor : actors) {
        auto* cooldownMap = actor.mutable_skill_cooldown_rounds();
        for (auto it = cooldownMap->begin(); it != cooldownMap->end(); ++it) {
            if (it->second > 0) {
                it->second -= 1;
            }
        }
    }
}

void TurnBattleEngine::UpdateOutcome() {
    const bool sideAWiped = SideWiped(0);
    const bool sideBWiped = SideWiped(1);

    if (sideAWiped && sideBWiped) {
        outcome = BATTLE_OUTCOME_DRAW;
    } else if (sideAWiped) {
        outcome = BATTLE_OUTCOME_SIDE_B_WIN;
    } else if (sideBWiped) {
        outcome = BATTLE_OUTCOME_SIDE_A_WIN;
    } else if (roundIndex >= maxRounds) {
        // max_rounds 打满:进攻方(A 方)判负(设计文档 §5.1)
        outcome = BATTLE_OUTCOME_SIDE_B_WIN;
    }
}

// ---------------------------------------------------------------------------
// 伤害 / 治疗 / 死亡
// ---------------------------------------------------------------------------

double TurnBattleEngine::CalculateFinalDamage(const BattleActorState& caster,
                                              const BattleActorState& target, double baseDamage,
                                              uint64_t attackBonus, bool& isCritical) {
    isCritical = false;

    // critchance 为整数百分比口径,换算后夹到 [0,1](镜像实时 CalculateFinalDamage)
    const double critChance = std::clamp(
        static_cast<double>(caster.attributes().critchance()) / 100.0, 0.0, 1.0);
    const double strength = static_cast<double>(caster.attributes().strength());
    const double armor = static_cast<double>(target.attributes().armor());
    const double resistance = static_cast<double>(target.attributes().resistance());
    // 属性加点二级属性:攻方物伤/法伤加法进 base,守方防御加法进减伤
    // (怪物/老存档两项皆 0,公式与实时 CalculateFinalDamage 老口径逐字节一致)
    const double defense = static_cast<double>(target.defense());

    double finalDamage = baseDamage * (1 + strength * 0.1) + static_cast<double>(attackBonus);
    finalDamage = finalDamage - armor - defense;
    finalDamage *= (1 - resistance * 0.01);

    // 暴击只走引擎 RNG;critChance 为 0 时不消耗随机数(与实时短路口径一致)
    if (critChance > 0.0 && Rand01() < critChance) {
        finalDamage *= 2;
        isCritical = true;
    }

    return std::max(finalDamage, 0.0);
}

uint64_t TurnBattleEngine::ApplyDamage(BattleActorState& target, double rawDamage) {
    if (rawDamage <= 0) {
        return 0;
    }
    // DEFEND:本回合受伤减半(含回合末周期伤害,防御摘除在 tick 之后)
    if (target.is_defending()) {
        rawDamage *= 0.5;
    }

    // 镜像实时 ApplyDamage:ceil 取整,饱和到 0
    const auto damage = static_cast<uint64_t>(std::ceil(rawDamage));
    const uint64_t healthBefore = target.attributes().health();
    const uint64_t healthAfter = healthBefore > damage ? healthBefore - damage : 0;
    target.mutable_attributes()->set_health(healthAfter);
    return healthBefore - healthAfter;
}

uint64_t TurnBattleEngine::ApplyHeal(BattleActorState& target, double rawHeal) {
    if (rawHeal <= 0 || target.is_dead()) {
        return 0;
    }
    const uint64_t healthBefore = target.attributes().health();
    const auto healthAfter = std::min<uint64_t>(
        target.max_health(),
        static_cast<uint64_t>(static_cast<double>(healthBefore) + rawHeal));
    target.mutable_attributes()->set_health(healthAfter);
    return healthAfter - healthBefore;
}

void TurnBattleEngine::HandleDeath(BattleActorState& target, TurnResultS2C& result) {
    target.set_is_dead(true);
    target.set_is_defending(false);
    // 死亡即清空 buff(回合制无复活流,不保留到期簿记)
    target.clear_buffs();
    AppendEvent(result, BATTLE_EVENT_DEATH, target.actor_id(), target.actor_id());
}

// ---------------------------------------------------------------------------
// effect[] → buff(镜像 BuffSystem::AddOrUpdateBuff,时间维度换算为回合)
// ---------------------------------------------------------------------------

void TurnBattleEngine::AddBuffToActor(BattleActorState& target, uint32_t buffTableId,
                                      uint64_t casterId, uint32_t depth, TurnResultS2C& result) {
    // 子 buff 递归深度上限,防表配环
    if (depth > kMaxSubBuffDepth || target.is_dead()) {
        return;
    }

    const auto* buffRow = dataProvider->FindBuff(buffTableId);
    if (buffRow == nullptr) {
        return;
    }

    // 免疫:目标现有 buff 的 immune_tag 覆盖新 buff 的任一 tag 即挡下
    if (IsImmuneToBuff(target, *buffRow)) {
        return;
    }

    // 驱散:新 buff 的 dispel_tag 命中现有 buff 的 tag 即移除之
    DispelBuffsByTag(target, *buffRow, result);

    // 纯驱散 buff 不落地(镜像 DispelBuffsOnAwake 的返回语义)
    if (buffRow->buff_type() == kBuffTypeDispel) {
        return;
    }

    // 同表同施法者:叠层/刷新,不新建条目
    if (StackOrRefreshExistingBuff(target, *buffRow, casterId, result)) {
        return;
    }

    const uint64_t newBuffId = nextBuffInstanceId++;
    auto* entry = target.add_buffs();
    entry->set_buff_id(newBuffId);
    entry->set_buff_table_id(buffTableId);
    entry->set_layer(1);
    entry->set_caster_id(casterId);
    // 持续时间换算:infinite_duration → 0 哨兵(无限);其余按秒→回合
    if (buffRow->infinite_duration() != 0) {
        entry->set_remain_rounds(0);
    } else if (buffRow->duration() > 0) {
        entry->set_remain_rounds(RoundsFromSeconds(buffRow->duration()));
    } else {
        // duration=0 且非无限:实时侧是"挂上即到期"的瞬时 buff,
        // 这里同口径:驱散/子 buff 已生效,条目本身立即移除
        entry->set_remain_rounds(1);
    }
    // 下面的子 buff 递归会往同一张 buff 列表增删条目,可能把本条目驱散掉;
    // 不能端着 entry 指针跨递归(镜像实时 AddSubBuffsWithoutCheck 的收口注释),
    // 之后一律用 newBuffId 重查
    entry = nullptr;

    auto* addEvent = AppendEvent(result, BATTLE_EVENT_BUFF_ADD, casterId, target.actor_id());
    addEvent->set_buff_table_id(buffTableId);
    addEvent->set_value(1);  // value 携带当前叠层数

    // 子 buff 挂给持有者本人(镜像 AddSubBuffsWithoutCheck 的宿主语义)
    for (const auto subBuffId : buffRow->sub_buff()) {
        AddBuffToActor(target, subBuffId, casterId, depth + 1, result);
    }

    // target_sub_buff 挂给"对面"——本 buff 由 caster 的技能带来,对面即 caster
    // (镜像 AddTargetSubBuffs:宿主为攻方时挂给受击方,方向对调)
    if (buffRow->target_sub_buff_size() > 0) {
        if (auto* casterActor = FindActor(casterId);
            casterActor != nullptr && !casterActor->is_dead()) {
            for (const auto targetSubBuffId : buffRow->target_sub_buff()) {
                AddBuffToActor(*casterActor, targetSubBuffId, target.actor_id(), depth + 1, result);
            }
        }
    }

    // 瞬时 buff:落地事件发过之后立即移除
    if (buffRow->infinite_duration() == 0 && buffRow->duration() <= 0) {
        const int index = FindBuffIndex(target, newBuffId);
        if (index >= 0) {
            RemoveBuffAt(target, index, result);
        }
    }
}

bool TurnBattleEngine::IsImmuneToBuff(const BattleActorState& target,
                                      const BuffTable& buffRow) const {
    // 镜像 IsTargetImmune:遍历目标现有 buff,行缺失只跳过该条
    for (const auto& existing : target.buffs()) {
        const auto* existingRow = dataProvider->FindBuff(existing.buff_table_id());
        if (existingRow == nullptr) {
            continue;
        }
        for (const auto& tagEntry : buffRow.tag()) {
            if (existingRow->immune_tag().contains(tagEntry.first)) {
                return true;
            }
        }
    }
    return false;
}

void TurnBattleEngine::DispelBuffsByTag(BattleActorState& target, const BuffTable& buffRow,
                                        TurnResultS2C& result) {
    if (buffRow.dispel_tag().empty()) {
        return;
    }

    // 先收集再移除,移除按下标降序避免位移
    std::vector<int> dispelIndexes;
    for (int index = 0; index < target.buffs_size(); ++index) {
        const auto* existingRow = dataProvider->FindBuff(target.buffs(index).buff_table_id());
        if (existingRow == nullptr) {
            continue;
        }
        for (const auto& dispelEntry : buffRow.dispel_tag()) {
            if (existingRow->tag().contains(dispelEntry.first)) {
                dispelIndexes.push_back(index);
                break;
            }
        }
    }

    for (auto it = dispelIndexes.rbegin(); it != dispelIndexes.rend(); ++it) {
        RemoveBuffAt(target, *it, result);
    }
}

bool TurnBattleEngine::StackOrRefreshExistingBuff(BattleActorState& target,
                                                  const BuffTable& buffRow, uint64_t casterId,
                                                  TurnResultS2C& result) {
    for (auto& existing : *target.mutable_buffs()) {
        if (existing.buff_table_id() != buffRow.id()) {
            continue;
        }
        // no_caster 的 buff 不区分施法者;其余按施法者隔离(镜像 processed_caster 语义)
        if (buffRow.no_caster() == 0 && existing.caster_id() != casterId) {
            continue;
        }

        if (existing.layer() < buffRow.max_layer()) {
            existing.set_layer(existing.layer() + 1);
        }
        // 刷新持续回合(实时 OnBuffRefresh 为 TODO;回合侧采用"重挂满时"口径)
        if (existing.remain_rounds() > 0 && buffRow.duration() > 0) {
            existing.set_remain_rounds(RoundsFromSeconds(buffRow.duration()));
        }

        auto* addEvent = AppendEvent(result, BATTLE_EVENT_BUFF_ADD, casterId, target.actor_id());
        addEvent->set_buff_table_id(buffRow.id());
        addEvent->set_value(existing.layer());
        return true;
    }
    return false;
}

void TurnBattleEngine::RemoveBuffAt(BattleActorState& target, int buffIndex,
                                    TurnResultS2C& result) {
    if (buffIndex < 0 || buffIndex >= target.buffs_size()) {
        return;
    }
    const auto buffTableId = target.buffs(buffIndex).buff_table_id();
    const auto casterId = target.buffs(buffIndex).caster_id();
    target.mutable_buffs()->DeleteSubrange(buffIndex, 1);

    auto* removeEvent = AppendEvent(result, BATTLE_EVENT_BUFF_REMOVE, casterId, target.actor_id());
    removeEvent->set_buff_table_id(buffTableId);
}

// ---------------------------------------------------------------------------
// 输出
// ---------------------------------------------------------------------------

BattleSettlementData TurnBattleEngine::BuildSettlement(uint64_t playerId) const {
    BattleSettlementData settlement;
    const auto settlementIt = settlements.find(playerId);
    if (settlementIt != settlements.end()) {
        settlement = settlementIt->second;  // 带上道具消耗账本
    }
    settlement.set_battle_id(createRequest.battle_id());
    settlement.set_player_id(playerId);
    settlement.set_outcome(outcome);
    settlement.set_total_rounds(CompletedRounds());

    const auto* actor = FindActor(playerId);
    if (actor == nullptr) {
        return settlement;
    }
    settlement.set_player_team_index(actor->team_index());
    settlement.set_health(actor->attributes().health());
    settlement.set_mana(actor->attributes().mana());
    settlement.set_is_dead(actor->is_dead());
    settlement.set_fled(actor->fled());

    // 出战宝宝的战后终值:随主人的结算一起回 scene(scene 是唯一应用者)
    for (const auto& other : actors) {
        if (other.actor_type() != BATTLE_ACTOR_TYPE_PET || other.owner_player_id() != playerId) {
            continue;
        }
        auto* petSettlement = settlement.add_pets();
        petSettlement->set_pet_id(other.actor_id());
        petSettlement->set_health(other.attributes().health());
        petSettlement->set_mana(other.attributes().mana());
        petSettlement->set_is_dead(other.is_dead());
    }

    // 奖励:仅在玩家侧(A 方=team 0)获胜时结算,给未逃跑的存活参战者。
    // 经验/金币 = 本场被击杀怪物 MonsterTable.exp_reward/gold_reward 之和;
    // 队伍 PVE 每个达成条件的成员各得全额(经典 MMO 组队口径)。
    // 逃跑或阵亡的玩家不发奖。掉落 items_gained 依赖掉落表,留待后续接入。
    if (outcome == BATTLE_OUTCOME_SIDE_A_WIN && actor->team_index() == 0 &&
        !actor->fled() && !actor->is_dead()) {
        uint64_t expSum = 0;
        uint64_t goldSum = 0;
        for (const auto& other : actors) {
            if (other.actor_type() != BATTLE_ACTOR_TYPE_MONSTER || !other.is_dead()) {
                continue;
            }
            const MonsterTable* row = dataProvider->FindMonster(other.monster_table_id());
            if (row != nullptr) {
                expSum += row->exp_reward();
                goldSum += row->gold_reward();
            }
        }
        settlement.set_exp_gain(expSum);
        settlement.set_gold_gain(goldSum);
    }
    return settlement;
}

BattleStateS2C TurnBattleEngine::BuildStateSnapshot() const {
    BattleStateS2C state;
    state.set_battle_id(createRequest.battle_id());
    state.set_round_index(roundIndex);
    state.set_outcome(outcome);
    // 引擎无时钟:action_deadline_ms 由 battle 节点按房间 timer 回填
    state.set_action_deadline_ms(0);

    for (const auto& actor : actors) {
        *state.add_actors() = actor;
    }

    if (outcome == BATTLE_OUTCOME_ONGOING) {
        for (const auto& actor : actors) {
            if (actor.actor_type() != BATTLE_ACTOR_TYPE_PLAYER || !IsActorActive(actor)) {
                continue;
            }
            // 挂机单位不待行动(与 AllPlayersReady 的就绪语义对齐,D12)
            if (actor.is_auto()) {
                continue;
            }
            if (pendingActions.find(actor.actor_id()) == pendingActions.end()) {
                state.add_pending_actor_ids(actor.actor_id());
            }
        }
    }
    return state;
}

// ---------------------------------------------------------------------------
// 查询辅助
// ---------------------------------------------------------------------------

BattleActorState* TurnBattleEngine::FindActor(uint64_t actorId) {
    for (auto& actor : actors) {
        if (actor.actor_id() == actorId) {
            return &actor;
        }
    }
    return nullptr;
}

const BattleActorState* TurnBattleEngine::FindActor(uint64_t actorId) const {
    for (const auto& actor : actors) {
        if (actor.actor_id() == actorId) {
            return &actor;
        }
    }
    return nullptr;
}

int TurnBattleEngine::FindBuffIndex(const BattleActorState& actor, uint64_t buffId) const {
    for (int index = 0; index < actor.buffs_size(); ++index) {
        if (actor.buffs(index).buff_id() == buffId) {
            return index;
        }
    }
    return -1;
}

bool TurnBattleEngine::IsActorActive(const BattleActorState& actor) const {
    return !actor.is_dead() && !actor.fled();
}

bool TurnBattleEngine::ActorHasBuffOfType(const BattleActorState& actor, uint32_t buffType) const {
    for (const auto& buff : actor.buffs()) {
        const auto* buffRow = dataProvider->FindBuff(buff.buff_table_id());
        if (buffRow != nullptr && buffRow->buff_type() == buffType) {
            return true;
        }
    }
    return false;
}

uint64_t TurnBattleEngine::MaxAliveEnemySpeed(const BattleActorState& actor) const {
    uint64_t maxSpeed = 0;
    for (const auto& other : actors) {
        if (other.team_index() == actor.team_index() || !IsActorActive(other)) {
            continue;
        }
        maxSpeed = std::max(maxSpeed, other.attributes().speed());
    }
    return maxSpeed;
}

std::vector<uint64_t> TurnBattleEngine::CollectAliveEnemyIds(const BattleActorState& actor) const {
    // actors 插入序稳定 → 候选列表顺序稳定 → RandIndex 选取确定性
    std::vector<uint64_t> enemyIds;
    for (const auto& other : actors) {
        if (other.team_index() != actor.team_index() && IsActorActive(other)) {
            enemyIds.push_back(other.actor_id());
        }
    }
    return enemyIds;
}

bool TurnBattleEngine::SideWiped(uint32_t teamIndex) const {
    for (const auto& actor : actors) {
        if (actor.team_index() == teamIndex && IsActorActive(actor)) {
            return false;
        }
    }
    return true;
}

uint32_t TurnBattleEngine::CompletedRounds() const {
    // 进行中:roundIndex 是收集中的回合;已结束:roundIndex 停在最后结算的回合
    return outcome == BATTLE_OUTCOME_ONGOING ? roundIndex - 1 : roundIndex;
}

BattleEventItem* TurnBattleEngine::AppendEvent(TurnResultS2C& result, eBattleEventType eventType,
                                               uint64_t sourceId, uint64_t targetId) {
    auto* event = result.add_events();
    event->set_event_type(eventType);
    event->set_source_id(sourceId);
    event->set_target_id(targetId);
    // 表现层分组(D2):同一行动/同一单位的回合末 tick 共用 group_id;
    // hit_index 由 ExecuteSkill 逐目标递增,其余场景为 0
    event->set_group_id(currentGroupId);
    event->set_hit_index(currentHitIndex);
    return event;
}

void TurnBattleEngine::BeginEventGroup() {
    ++currentGroupId;
    currentHitIndex = 0;
}

bool TurnBattleEngine::RollHit(const BattleActorState& caster, const BattleActorState& target) {
    (void)caster;
    (void)target;
    // 一期:Skill/Monster 表均无命中率/闪避列,命中率固定 kBaseHitRate=100,
    // 短路返回、不消耗随机数(与暴击 critChance=0 时的短路口径一致),既有回放基线不变。
    // 二期接表:hitRate = kBaseHitRate + 攻方命中列 - 守方闪避列,
    // 再 `Rand01() * 100.0 < hitRate` 掷骰——该随机数消费位于暴击掷骰之前,
    // 接入时须同步刷新单测回放基线(见 turn_battle_engine_test.cpp 说明)
    if (kBaseHitRate >= 100) {
        return true;
    }
    return Rand01() * 100.0 < static_cast<double>(kBaseHitRate);
}

// ---------------------------------------------------------------------------
// 确定性随机
// ---------------------------------------------------------------------------

uint64_t TurnBattleEngine::RandIndex(uint64_t count) {
    // 不走 std::uniform_int_distribution:其实现跨平台不定,取模在此量级偏差可忽略
    return rng() % count;
}

double TurnBattleEngine::Rand01() {
    // 取 mt19937_64 高 53 位映射到 [0,1),任何平台上同种子同序列
    return static_cast<double>(rng() >> 11) * (1.0 / 9007199254740992.0);
}

}  // namespace turnbattle
