#include "system/turn_battle_engine.h"

#include <algorithm>
#include <cmath>
#include <cstdint>
#include <limits>
#include <map>
#include <optional>
#include <set>
#include <string>

#include "system/battle_result_activity.h"
#include "system/battle_script.h"
#include "system/combat_damage_rules.h"
#include "system/equipment_battle_rules.h"
#include "system/pet_cultivation_rules.h"
#include "system/season_rules.h"
#include "system/trial_journal_rules.h"
#include "modules/currency/constants/currency.h"
#include "muduo/base/Logging.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/skill_error_tip.pb.h"
#include "table/proto/tip/bag_error_tip.pb.h"
#include "table/proto/tip/match_error_tip.pb.h"

// 回合制战斗确定性引擎实现(设计文档 §5.1)。
//
// 技能流程镜像实时战斗 SkillSystem/BuffSystem(cpp/libs/services/scene/combat/),
// 差异点:
//   1. 一切时间维度换算为回合(RoundsFrom*,kRoundDurationMs=6000),无任何 timer;
//   2. 去掉 casting/recovery/channel 相位与对应校验(CheckCasting/CheckRecovery/CheckChannel);
//   3. 战斗随机走成员 RNG，奖励随机按种子与玩家号独立派生，禁 tlsRandom/rand();
//   4. 引擎内不打日志(纯逻辑库,校验失败以错误码/静默降级表达,由 battle 节点记日志);
//      唯一例外:Initialize 建房参数违规打 ERROR(编排层错误须运维可见,设计文档 §11 D14)。

namespace turnbattle {

namespace {

// 快照未携带 max_health/max_mana 时的保守回退:以当前值为上限
uint64_t FallbackMax(uint64_t declaredMax, uint64_t currentValue) {
    return declaredMax > 0 ? declaredMax : currentValue;
}

// 把副本怪物组按原顺序循环取成 targetCount 只:组 [a,b]、要 3 只 → [a,b,a];要 1 只 → [a]。
// 前置条件:group 非空(调用方已判)。不消耗 RNG —— 同一副本同一人数永远是同一批怪,回放基线不动
std::vector<uint32_t> CycleMonsterGroup(const std::vector<uint32_t>& group, uint32_t targetCount) {
    std::vector<uint32_t> cycled;
    cycled.reserve(targetCount);
    for (uint32_t index = 0; index < targetCount; ++index) {
        cycled.push_back(group[index % group.size()]);
    }
    return cycled;
}

// 百分点 / 等级这类 uint64 的饱和加法。战斗类属性来自另一进程的快照,数值再离谱也只该"封顶",
// 不该回绕成一个很小的数(那样满抗性会变成零抗性)
uint64_t SaturatingAdd(uint64_t lhs, uint64_t rhs) {
    const uint64_t sum = lhs + rhs;  // 无符号溢出是良定义的回绕:和比加数小 = 溢出了
    return sum < lhs ? UINT64_MAX : sum;
}

// ceil(value × percent / 100) 的整数算法。先拆商和余数再乘,value 再大也不溢出;
// 前提 percent <= 100(调用处的常量由 turn_battle_constants.h 的 static_assert 守住)
uint64_t PercentCeil(uint64_t value, uint32_t percent) {
    const uint64_t quotient = value / 100;
    const uint64_t remainder = value % 100;
    return quotient * percent + (remainder * percent + 99) / 100;
}

// 异常类 buff 在目标身上对应的单项抗性(整数百分点);该 buff 类型不算异常状态时返回空。
// buff 类型 → 抗性种类是 kAilmentBuffMappings 一张平铺表,这里只负责把种类落到 proto 字段上
std::optional<uint64_t> AilmentResistPercent(const CombatAttributes& combat, uint32_t buffType) {
    for (const auto& mapping : kAilmentBuffMappings) {
        if (mapping.buffType != buffType) {
            continue;
        }
        switch (mapping.resistKind) {
        case AilmentResistKind::kPoison:
            return combat.resist_poison();
        case AilmentResistKind::kFreeze:
            return combat.resist_freeze();
        case AilmentResistKind::kSleep:
            return combat.resist_sleep();
        case AilmentResistKind::kForget:
            return combat.resist_forget();
        case AilmentResistKind::kCount:
            break;
        }
        return std::nullopt;
    }
    return std::nullopt;
}

// Buff 表里标减益的 tag(职业技能的减益行都带它;驱散按它认)
constexpr char kDebuffTag[] = "Debuff";

// 气血比例(0 上限按满血算,治疗目标排序用)
double HealthRatio(const BattleActorState& actor) {
    if (actor.max_health() == 0) {
        return 1.0;
    }
    return static_cast<double>(actor.attributes().health()) / static_cast<double>(actor.max_health());
}

// 法力比例(0 上限按满蓝算:没有蓝条的单位不需要回蓝)
double ManaRatio(const BattleActorState& actor) {
    if (actor.max_mana() == 0) {
        return 1.0;
    }
    return static_cast<double>(actor.attributes().mana()) / static_cast<double>(actor.max_mana());
}

// 职业技能回蓝量:按目标最大法力的比例(× 辅助等级系数),按回蓝时刻 manaBefore 的缺口封顶
uint64_t ClassSkillManaRestored(const SkillTable& skillRow, uint32_t rank, const BattleActorState& target,
                                uint64_t manaBefore) {
    const uint64_t missing = target.max_mana() > manaBefore ? target.max_mana() - manaBefore : 0;
    return std::min(missing, classskill::CeilRatio(target.max_mana(), skillRow.restore_mana_percent() *
                                                                          classskill::SupportRankFactor(rank)));
}

// 增减益按比例作用的那项属性的快照值(未吃增减益)
uint64_t BaseClassBuffStat(const BattleActorState& actor, ClassBuffStat stat) {
    switch (stat) {
    case ClassBuffStat::kPhysicalAttack:
        return actor.physical_attack();
    case ClassBuffStat::kDefense:
        return actor.defense();
    case ClassBuffStat::kSpeed:
        return actor.attributes().speed();
    default:
        return 0;
    }
}

// 挂机选技的用途,按优先级从高到低排(class-skills.md §6.5)
enum class AutoSkillPurpose : uint8_t {
    kCriticalHeal = 0,
    kDispel,
    kHeal,
    kRestoreMana,
    kStatBuff,
    kDamage,
    kCount,
};

// 一手候选。gain 的单位随用途不同(治疗量 / 减益个数 / 净回蓝 / 属性点数 / 伤害),只在同一用途内比较
struct AutoSkillCandidate {
    AutoSkillPurpose purpose;
    uint64_t gain;
    uint64_t manaCost;
    uint32_t cooldownRounds;
    uint32_t skillId;
    uint64_t targetId;
};

// 全序:最后两键是技能号与目标号,所以选择结果与技能列表、protobuf map 的遍历顺序无关
bool IsBetterAutoSkill(const AutoSkillCandidate& lhs, const AutoSkillCandidate& rhs) {
    if (lhs.purpose != rhs.purpose) {
        return lhs.purpose < rhs.purpose;
    }
    if (lhs.gain != rhs.gain) {
        return lhs.gain > rhs.gain;
    }
    if (lhs.manaCost != rhs.manaCost) {
        return lhs.manaCost < rhs.manaCost;
    }
    if (lhs.cooldownRounds != rhs.cooldownRounds) {
        return lhs.cooldownRounds < rhs.cooldownRounds;
    }
    if (lhs.skillId != rhs.skillId) {
        return lhs.skillId < rhs.skillId;
    }
    return lhs.targetId < rhs.targetId;
}

// 固定的 SplitMix64 混合，不依赖 std::hash 或平台实现；同种子同玩家不受队伍、回合或击杀数影响。
uint64_t MixRewardSeed(uint64_t value) {
    value += UINT64_C(0x9e3779b97f4a7c15);
    value = (value ^ (value >> 30)) * UINT64_C(0xbf58476d1ce4e5b9);
    value = (value ^ (value >> 27)) * UINT64_C(0x94d049bb133111eb);
    return value ^ (value >> 31);
}

uint64_t RewardRandomIndex(std::mt19937_64& rewardRng, uint64_t count) {
    // 拒绝取模偏差，算法与随机消费顺序跨平台固定；count 已由奖励档校验保证非零。
    const uint64_t threshold = (uint64_t{0} - count) % count;
    uint64_t value = 0;
    do {
        value = rewardRng();
    } while (value < threshold);
    return value % count;
}

}  // namespace

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
            LOG_ERROR << "CreateBattle team size exceeds limit: battle_id=" << request.battle_id()
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
    everDiedActors.clear();
    enteredPlayers.clear();
    manualActionPlayers.clear();
    explicitAutoPlayers.clear();
    hadOperableTurnPlayers.clear();
    bossGuardHealTotal = 0;
    guardStanceDispelled = false;
    teamDealtPhysicalDirectDamage = {false, false};
    teamDealtMagicDirectDamage = {false, false};
    shieldBreakPlayers.clear();
    antiControlOrSeasonSkillPlayers.clear();
    castSeasonSkillPlayers.clear();
    seasonResonanceMaskByPlayer.clear();
    trialVowMask = 0;
    hasTacticalPuzzleContext = false;
    tacticalPuzzleContext.Clear();
    bossWindHitUnprotectedDeath = false;
    dealtYangBellPhysicalDamage = false;
    dealtYinDrumMagicDamage = false;
    bossTrialKind = kBossTrialKindNone;
    battleDifficulty = 0;
    bossWeeklyAffixId = bossaffix::kAffixNone;
    weeklyAffixWoodTeamBoostUntilRound = 0;
    weeklyAffixEarthTeamBoostUntilRound = 0;
    weeklyAffixWoodManaTriggeredActors.clear();
    initialAttackerHumanCount = 0;
    protectedGuardActorId = 0;
    initialPolarity = 0;
    activePolarity = 0;
    bossStanceType = 0;
    bossStanceRemainRounds = 0;
    lastChosenStanceType = 0;
    bossBarrierShield = 0;
    bossPhase = 0;
    nextPhase2CleaveTelegraphRound = 0;
    bossMomentumStacks = 0;
    bossDisheartenedUntilRound = 0;
    exposedActors.clear();
    sunderUntilRound.clear();
    attackerTriggeredStanceCounterThisRound.clear();
    roundPlayerDirectDamage.clear();
    cleaveParryCount = 0;
    healInterruptBacklashCount = 0;
    stanceBreakCount = 0;
    selfCleavesTargetedByPlayer.clear();
    selfCleavesParriedByPlayer.clear();
    activeTelegraphs.clear();
    lastResolvedTelegraphs.clear();
    telegraphStateVersion = 0;
    manualActionRoundsByPlayer.clear();
    effectiveContributionPlayers.clear();
    totalDamageDealtByPlayer.clear();
    totalHealingDoneByPlayer.clear();
    totalControlsLandedByPlayer.clear();
    primaryFocusDamageByPlayer.clear();
    totalShieldGrantedByPlayer.clear();
    totalDispelsLandedByPlayer.clear();
    damageTakenByPlayer.clear();
    lowHpExperiencedPlayers.clear();
    peakEscalationStacks = 0;
    teamPings[0].clear();
    teamPings[1].clear();
    roundPingCountByPlayer.clear();
    surrenderedTeam.reset();
    teamSurrenderVotes[0].Clear();
    teamSurrenderVotes[1].Clear();
    consecutiveIdleRoundsByPlayer.clear();
    teamAFormationId = 0;
    teamBFormationId = 0;
    hasTowerContext = false;
    towerContext.Clear();
    towerSigilTotals = {};
    boonRuntime = {};
    hasCustomDuelRules = false;
    customDuelRules.Clear();
    wuxingSynergyActiveThisRound = false;
    wuxingSynergyActorIds.clear();
    activeSeasonWeeklyAffixId = 0;
    seasonSealRuntimeByActor.clear();
    pendingMysticExtendCasterId = 0;
    pendingMysticExtendTargetId = 0;
    pendingMysticExtendSkillId = 0;
    pendingMysticExtendBonusRounds = 0;
    activeNextRoundBoostCasterId = 0;
    activeNextRoundBoostKind = 0;
    activeNextRoundBoostPct = 0;
    activeNextRoundBoostExtra = 0;
    aiControlledPlayers.clear();

    if (IsGuildWarBattle()) {
        // 帮会全天帮战 V2 强隔离：必须携带合法 BATTLE_ACTIVITY_KIND_GUILD_WAR 上下文，且不消耗局外背包道具。
        if (!request.has_activity_context() ||
            request.activity_context().kind() != BATTLE_ACTIVITY_KIND_GUILD_WAR ||
            !request.activity_context().has_guild_war()) {
            return false;
        }
        for (auto& snapshot : *createRequest.mutable_players()) {
            snapshot.clear_items();
        }
    }

    if (request.has_custom_duel_rules()) {
        const auto& rules = request.custom_duel_rules();
        if (!wuxing::IsValidCustomDuelRoundLimit(rules.round_limit_override())) {
            return false;
        }
        if (rules.skill_ban_slots_per_team() != 0u &&
            rules.skill_ban_slots_per_team() != kPrepSkillBanSlotsPerTeam) {
            return false;
        }
        if (rules.use_free_preset() && rules.skill_ban_slots_per_team() > 0u) {
            return false;
        }
        hasCustomDuelRules = true;
        customDuelRules = rules;
    }

    for (const auto& snapshot : request.players()) {
        if (!wuxing::IsValidFormationId(snapshot.formation_id())) {
            return false;
        }
        if (snapshot.formation_id() != 0) {
            if (snapshot.team_index() == 0 && teamAFormationId == 0) {
                teamAFormationId = snapshot.formation_id();
            } else if (snapshot.team_index() == 1 && teamBFormationId == 0) {
                teamBFormationId = snapshot.formation_id();
            }
        }
        if (snapshot.has_tower_context() && snapshot.tower_context().floor() != 0) {
            if (request.match_mode() != kMatchModePveSolo || hasTowerContext) {
                return false;
            }
            const auto& tc = snapshot.tower_context();
            const bool isV2 = tc.realm_version() >= wuxing::kRealmV2Version;
            const uint32_t maxFloor = isV2 ? wuxing::kRealmV2MaxNodes : wuxing::kTowerMaxFloors;
            if (tc.floor() < 1u || tc.floor() > maxFloor || tc.run_id() == 0u) {
                return false;
            }
            if (isV2 && !wuxing::IsRealmCombatNodeKind(tc.node_kind())) {
                return false;
            }
            for (const uint32_t sigilId : tc.active_sigil_ids()) {
                if (wuxing::IsValidTowerSigilId(sigilId)) {
                    const auto def = wuxing::FindTowerSigil(sigilId);
                    towerSigilTotals.damagePct += def.damagePct;
                    towerSigilTotals.critPct += def.critPct;
                    towerSigilTotals.critDamagePct += def.critDamagePct;
                    towerSigilTotals.shieldBreakBonusPct += def.shieldBreakBonusPct;
                    towerSigilTotals.healShieldPct += def.healShieldPct;
                    towerSigilTotals.petDamagePct += def.petDamagePct;
                    towerSigilTotals.roundHpRegenPct += def.roundHpRegenPct;
                    towerSigilTotals.roundManaRegenPct += def.roundManaRegenPct;
                    towerSigilTotals.controlResistPct += def.controlResistPct;
                    towerSigilTotals.onHitManaRegenPct += def.onHitManaRegenPct;
                    towerSigilTotals.damageTakenReducePct += def.damageTakenReducePct;
                    towerSigilTotals.first3RoundsDamagePct += def.first3RoundsDamagePct;
                    towerSigilTotals.singleTargetDamagePct += def.singleTargetDamagePct;
                    towerSigilTotals.critBurnMaxHpPct += def.critBurnMaxHpPct;
                    towerSigilTotals.openingShieldPct += def.openingShieldPct;
                    towerSigilTotals.reflectPct += def.reflectPct;
                } else if (isV2 && wuxing::IsValidRealmBoonId(sigilId)) {
                    boonRuntime.boonMask |= wuxing::RealmBoonBit(sigilId);
                } else {
                    return false;
                }
            }
            hasTowerContext = true;
            towerContext = tc;
            if (isV2) {
                const uint32_t computedResonance = wuxing::ComputeActiveResonanceElement(tc.active_sigil_ids());
                boonRuntime.resonanceElement = computedResonance;
                towerContext.set_active_resonance_element(computedResonance);
            }
        }
    }

    // 回合上限与玩法配置：优先 DungeonTable.round_limit，其次物法易势默认 16 回合，正式排位缺省收敛到 20 回合，其余沿用 time_limit 换算。
    maxRounds = kDefaultMaxRounds;
    bool hasExplicitRoundLimit = false;
    if (const auto* dungeonRow = dataProvider->FindDungeon(request.battle_config_id());
        dungeonRow != nullptr) {
        bossTrialKind = dungeonRow->boss_trial_kind();
        battleDifficulty = dungeonRow->difficulty();
        if (dungeonRow->round_limit() > 0) {
            maxRounds = dungeonRow->round_limit();
            hasExplicitRoundLimit = true;
        } else if (dungeonRow->time_limit() > 0) {
            maxRounds = RoundsFromMilliseconds(static_cast<uint64_t>(dungeonRow->time_limit()) * 1000);
        }
    }
    if (!bossaffix::IsValidOverride(request.boss_weekly_affix_id())) {
        return false;
    }
    if (IsBossTrialKind(bossTrialKind)) {
        bossPhase = 1;
        bossWeeklyAffixId = bossaffix::ResolveForBattle(
            true, hasTowerContext, request.created_at_ms(), request.boss_weekly_affix_id());
    }
    constexpr uint32_t kRankedDefaultMaxRounds = 20;
    if (IsRankedBattle() && !hasExplicitRoundLimit) {
        maxRounds = std::min(maxRounds, kRankedDefaultMaxRounds);
    }
    if (IsGuildWarBattle() && !hasExplicitRoundLimit) {
        maxRounds = std::min(maxRounds, kGuildWarDefaultMaxRounds);
    }
    if (IsJianghuDuelBattle()) {
        uint32_t duelRounds = kJianghuDuelDefaultMaxRounds;
        if (request.has_jianghu_duel_context() && request.jianghu_duel_context().round_limit_override() > 0u) {
            duelRounds = std::clamp(request.jianghu_duel_context().round_limit_override(), 1u, 100u);
        }
        maxRounds = duelRounds;
    }
    if (request.match_mode() == kMatchModePolarity5v5 && bossTrialKind == kBossTrialKindNone) {
        bossTrialKind = kBossTrialKindPolarity5v5;
        if (maxRounds == kDefaultMaxRounds) {
            maxRounds = kPolarityDefaultMaxRounds;
        }
    }
    if (hasCustomDuelRules && customDuelRules.round_limit_override() > 0u) {
        maxRounds = customDuelRules.round_limit_override();
    }

    // 万象残局坊（Dungeon 314）开局门禁：必须携带合法 TacticalPuzzleBattleContext 且为单人免费公平预设局。
    const bool isWorkshopDungeon = request.battle_config_id() == kTacticalPuzzleWorkshopDungeonId;
    if (request.has_tactical_puzzle_context() || isWorkshopDungeon) {
        if (!request.has_tactical_puzzle_context() ||
            !isWorkshopDungeon ||
            request.match_mode() != kMatchModePveSolo ||
            request.players_size() != 1 ||
            bossTrialKind != kBossTrialKindTacticalPuzzle ||
            !UsesFreePreset() ||
            hasTowerContext ||
            hasCustomDuelRules ||
            !IsValidTacticalPuzzleContext(request.tactical_puzzle_context())) {
            LOG_ERROR << "CreateBattle rejected invalid tactical puzzle context: battle_id=" << request.battle_id()
                      << " config_id=" << request.battle_config_id()
                      << " mode=" << request.match_mode()
                      << " boss_trial_kind=" << bossTrialKind;
            return false;
        }
        hasTacticalPuzzleContext = true;
        tacticalPuzzleContext = request.tactical_puzzle_context();
        maxRounds = tacticalPuzzleContext.spec().round_limit();
    }

    // 五行试炼志 V3 自选试炼誓约校验与冻结（bit0 禁控 / bit1 单宝 / bit2 无宠 / bit3 紧缩回合）。
    const uint32_t requestedVowMask =
        request.has_trial_context() ? request.trial_context().challenge_vow_mask() : 0u;
    if (!trialjournal::IsValidVowMask(requestedVowMask)) {
        LOG_ERROR << "CreateBattle invalid trial challenge_vow_mask: battle_id=" << request.battle_id()
                  << " mask=" << requestedVowMask;
        return false;
    }
    if (requestedVowMask != 0u) {
        const bool isPveRequest =
            request.match_mode() == kMatchModePveSolo || request.match_mode() == kMatchModePveTeam;
        if (!isPveRequest || !IsBossTrialKind(bossTrialKind) || hasTowerContext ||
            hasCustomDuelRules || IsNpcIsolatedEncounterKind(bossTrialKind)) {
            LOG_ERROR << "CreateBattle trial challenge_vow_mask rejected for unsupported battle: battle_id="
                      << request.battle_id() << " mode=" << request.match_mode()
                      << " boss_trial_kind=" << bossTrialKind << " mask=" << requestedVowMask;
            return false;
        }
        trialVowMask = requestedVowMask;
        if (HasTrialVow(trialjournal::kVowTightRounds)) {
            maxRounds = trialjournal::TightenRoundLimit(maxRounds);
        }
    }

    // 赛季全员对称周环境词缀：仅当未启用锦标赛五行地脉、五行幻境爬塔、自定义切磋、残局坊及瑞兽迎春活动时生效，
    // 且须全部玩家快照一致携带合法词缀 1..4，分歧或越界 fail-closed 退回 0，杜绝单边优势。
    if (!hasTowerContext && !hasCustomDuelRules && !hasTacticalPuzzleContext &&
        ActiveTournamentLeyline() == TOURNAMENT_LEYLINE_UNSPECIFIED &&
        bossTrialKind != kBossTrialKindFestivalBeast &&
        !(request.has_activity_context() &&
          request.activity_context().kind() == BATTLE_ACTIVITY_KIND_FESTIVAL_BEAST) &&
        request.players_size() > 0) {
        uint32_t unanimousAffix = 0u;
        bool consistent = true;
        for (const auto& snapshot : request.players()) {
            const uint32_t candidate = snapshot.season_weekly_affix_id();
            if (!seasonrules::IsValidWeeklyAffixId(candidate)) {
                consistent = false;
                break;
            }
            if (unanimousAffix == 0u) {
                unanimousAffix = candidate;
            } else if (unanimousAffix != candidate) {
                consistent = false;
                break;
            }
        }
        if (consistent && seasonrules::IsValidWeeklyAffixId(unanimousAffix)) {
            activeSeasonWeeklyAffixId = unanimousAffix;
        }
    }

    // 瑞兽迎春硬性准入门禁：boss_trial_kind=7 必须且仅能携带合法冻结的 FESTIVAL_BEAST 上下文、单人携单只锁定出战宝宝。
    const bool hasFestivalActivity =
        request.has_activity_context() &&
        request.activity_context().kind() == BATTLE_ACTIVITY_KIND_FESTIVAL_BEAST;
    if (bossTrialKind == kBossTrialKindFestivalBeast || hasFestivalActivity) {
        if (bossTrialKind != kBossTrialKindFestivalBeast || !hasFestivalActivity ||
            request.match_mode() != kMatchModePveSolo || request.players_size() != 1 ||
            request.players(0).pets_size() != 1 || !request.activity_context().has_festival_beast()) {
            return false;
        }
        const auto& fb = request.activity_context().festival_beast();
        if (fb.activity_instance_id() == 0 || fb.reward_identity().empty() || fb.required_wins() == 0 ||
            fb.locked_pet_id() == 0 || request.players(0).pets(0).pet_id() != fb.locked_pet_id() ||
            fb.boss_health() == 0 || fb.round_limit() == 0 || fb.cycle_rounds() == 0 ||
            fb.first_heavy_round() == 0 || fb.first_heavy_round() > fb.cycle_rounds() ||
            fb.telegraph_lead_rounds() == 0 || fb.telegraph_lead_rounds() >= fb.first_heavy_round() ||
            fb.heavy_damage_percent() == 0) {
            return false;
        }
        maxRounds = fb.round_limit();
    }

    prepActive = false;
    prepSkillBanSlotsPerTeam = 0;
    banCandidateSkillIds.clear();
    banCandidateSkillSet.clear();
    teamABannedSkillIds.clear();
    teamBBannedSkillIds.clear();
    bannedSkillIds.clear();
    bannedSkillSet.clear();
    nextPetActorIndex = 0;
    prepOptionsByPlayer.clear();
    validatedPetsByPlayer.clear();
    standbyPetPoolByPlayer.clear();
    summonPetRemainingByPlayer.clear();

    if (!InitPlayers(request)) {
        return false;
    }

    // 宝宝必须在玩家之后追加:阵位按插入序排,主人先站前排
    if (!InitPets(request)) {
        return false;
    }

    if (!InitTacticalPrep(request)) {
        return false;
    }

    const bool isPve =
        request.match_mode() == kMatchModePveSolo || request.match_mode() == kMatchModePveTeam;
    if (isPve) {
        const auto* rewardProfile = dataProvider->FindBattleReward(request.battle_config_id(), "");
        if (rewardProfile == nullptr || !rewardProfile->strategy_key().empty() ||
            !IsValidRewardProfile(*rewardProfile) || !InitMonsters(request)) {
            return false;
        }
        defaultRewardProfile = *rewardProfile;
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

    // 快照 buff 清洗必须等全部单位就位:caster_id 的域改写要能查到本局 actor
    for (auto& actor : actors) {
        if (actor.actor_type() == BATTLE_ACTOR_TYPE_PLAYER) {
            SanitizeSnapshotBuffs(actor);
        }
    }

    // 战斗剧本同样要等全部单位就位:按阵位给单位认领角色位、给怪物派生技能集(开局状态里就能看到)
    BindBattleScripts();
    BindDungeonMonsterAis();
    // 战前 15 秒明牌变阵期间延迟到 FinalizePrepPhase 锁阵后再初始化开场光环/被动，防止替换首发宠残留旧光环。
    if (!prepActive) {
        RebuildMidBattleStandbyPools();
        InitializeConfiguredAbilities();
    }
    InitSpecialEncounterState();
    if (hasTacticalPuzzleContext) {
        ApplyTacticalPuzzleSetup();
    }

    // 锦标赛 V2 土厚灵脉 + 地载阵 + 五行幻境土系符印：开场第 1 回合叠加最大气血护盾。
    leylineEarthShieldByActor.clear();
    const bool earthLeyline = ActiveTournamentLeyline() == TOURNAMENT_LEYLINE_EARTH;
    for (const auto& actor : actors) {
        if (!IsActorActive(actor) || actor.max_health() == 0) {
            continue;
        }
        uint64_t shieldPct = earthLeyline ? 8ULL : 0ULL;
        shieldPct += wuxing::FormationOf(TeamFormationId(actor.team_index())).openingShieldPct;
        if (hasTowerContext && actor.team_index() == 0) {
            shieldPct += towerSigilTotals.openingShieldPct;
            if (HasRealmBoon(wuxing::kBoonEarthShieldStrike)) {
                shieldPct += 18ULL;
            }
            if (HasRealmResonance(wuxing::kElementEarth)) {
                shieldPct += 10ULL;
            }
        }
        if (bossWeeklyAffixId == bossaffix::kAffixMetal && actor.team_index() == 1 &&
            actor.actor_type() == BATTLE_ACTOR_TYPE_MONSTER) {
            shieldPct += bossaffix::kMetalEnemyOpeningShieldPct;
        }
        if (shieldPct > 0) {
            leylineEarthShieldByActor[actor.actor_id()] =
                std::max<uint64_t>(1ULL, (actor.max_health() * shieldPct) / 100ULL);
        }
    }

    initialized = true;
    return true;
}

bool TurnBattleEngine::IsValidRewardProfile(const BattleRewardProfile& profile) const {
    // Provider 边界也兜住坏数据，避免测试替身或新数据源绕过生产加载校验后溢出或无界抽取。
    if (profile.strategy_key().size() > 64 || profile.item_draws() > 100 ||
        profile.random_items_size() > 256 ||
        (profile.item_draws() > 0 && profile.random_items_size() == 0)) {
        return false;
    }
    std::set<uint32_t> currencyTypes;
    for (const auto& currency : profile.currencies()) {
        if (!IsKnownCurrencyType(currency.currency_type()) ||
            currency.amount() > static_cast<uint64_t>(std::numeric_limits<int64_t>::max()) ||
            !currencyTypes.insert(currency.currency_type()).second) {
            return false;
        }
    }
    std::set<uint32_t> itemIds;
    for (const auto& item : profile.random_items()) {
        const auto* row = dataProvider->FindItem(item.item_table_id());
        if (item.item_table_id() == 0 || row == nullptr || row->max_stack_size() == 0 ||
            item.count() == 0 || item.weight() == 0 ||
            !itemIds.insert(item.item_table_id()).second ||
            static_cast<uint64_t>(item.count()) * profile.item_draws() > UINT32_MAX) {
            return false;
        }
    }
    return true;
}

bool TurnBattleEngine::InitPlayers(const CreateBattleRequest& request) {
    for (const auto& snapshot : request.players()) {
        if (snapshot.player_id() == 0 || snapshot.team_index() > 1) {
            return false;
        }
        // bit63 是引擎局内号(怪物 / 宝宝)的保留段:真实 player_id 不可能落进来,
        // 落进来就是编排层把别的东西当成了 player_id,放进来会与局内单位撞号
        if ((snapshot.player_id() & kEngineLocalActorIdFlag) != 0) {
            LOG_ERROR << "CreateBattle player_id is in the reserved range for engine-local IDs: battle_id="
                      << request.battle_id() << " player_id=" << snapshot.player_id();
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
        actor.set_appearance_id(snapshot.appearance_id());
        for (const auto& [skillId, vfxId] : snapshot.equipped_skill_vfx()) {
            if (skillId != 0 && vfxId != 0) {
                (*actor.mutable_equipped_skill_vfx())[skillId] = vfxId;
            }
        }
        if (IsRankedBattle()) {
            const auto& streaks = createRequest.ranked_context().member_win_streaks();
            if (const auto stIt = streaks.find(snapshot.player_id());
                stIt != streaks.end() && stIt->second >= kMinWinStreakHighlight) {
                actor.set_win_streak(stIt->second);
            }
        }
        actor.set_class_id(snapshot.class_id());
        actor.set_gender(snapshot.gender());
        actor.set_level(snapshot.level());
        *actor.mutable_attributes() = snapshot.base_attributes();
        actor.set_max_health(FallbackMax(snapshot.max_health(), snapshot.base_attributes().health()));
        actor.set_max_mana(FallbackMax(snapshot.max_mana(), snapshot.base_attributes().mana()));
        // 二级属性(属性加点系统):怪物不带 = 0,公式退化为老口径
        actor.set_physical_attack(snapshot.physical_attack());
        actor.set_magic_attack(snapshot.magic_attack());
        actor.set_defense(snapshot.defense());
        if (snapshot.has_equipment_effects()) {
            if (snapshot.team_index() == 0u && HasTrialVow(trialjournal::kVowSingleArtifact) &&
                snapshot.equipment_effects().artifacts_size() > 1) {
                auto trimmed = snapshot.equipment_effects();
                trimmed.mutable_artifacts()->DeleteSubrange(1, trimmed.artifacts_size() - 1);
                if (!InitializeEquipment(actor, trimmed)) return false;
            } else {
                if (!InitializeEquipment(actor, snapshot.equipment_effects())) return false;
            }
        }
        // 战斗类属性的装备加成(equipment-attributes.md §4.5):只有玩家快照带;怪物 / 宝宝不填 = 全 0。
        // 快照没带就不建子消息:老 scene 发来的快照下,单位状态的序列化字节与装备系统落地前相同
        if (snapshot.has_combat()) {
            *actor.mutable_combat() = snapshot.combat();
        }
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
        // 职业技能等级(class-skills.md §6.1):快照里的等级按 scene 同一规则复核 ——
        // 表里有这一行、是本单位职业的技能、夹到当前角色等级的上限;复核完为 0 的丢掉。
        // 同 exclusive_group 互斥战法按 skill_table_id 升序确定性只保留首条，作 fail-closed 第二道防线。
        std::vector<std::pair<uint32_t, uint32_t>> sortedRanks(snapshot.skill_ranks().begin(),
                                                               snapshot.skill_ranks().end());
        std::sort(sortedRanks.begin(), sortedRanks.end());
        std::set<uint32_t> occupiedExclusiveGroups;
        for (const auto& [skillTableId, rank] : sortedRanks) {
            const auto* skillRow = dataProvider->FindSkill(skillTableId);
            if (skillRow == nullptr ||
                !seasonrules::IsSkillActiveInSeason(skillRow->season_id(), snapshot.active_season_id()) ||
                (skillRow->battle_model() == 1 ? !ConfiguredSubjectAllowed(actor, *skillRow) :
                (!IsClassSkill(*skillRow) || skillRow->class_id() != actor.class_id()))) {
                continue;
            }
            const uint32_t effectiveRank = classskill::EffectiveRank(rank, actor.level(), skillRow->unlock_level(),
                                                                     skillRow->max_rank());
            if (effectiveRank == 0) {
                continue;
            }
            if (skillRow->exclusive_group() != 0 &&
                !occupiedExclusiveGroups.insert(skillRow->exclusive_group()).second) {
                continue;
            }
            (*actor.mutable_skill_ranks())[skillTableId] = effectiveRank;
        }
        // 技能只收回合制可施放的(G6):被动/开关/持续施法进来会被当成普通技能打出伤害。
        // scene 出快照时已过滤一遍,这里是引擎侧兜底 —— 快照来自另一进程,不能只信它。
        // 职业技能还要在上面复核出等级才算拥有。
        // actor.skill_table_ids 同时是客户端可见的"本局可用技能"列表。
        for (const auto skillTableId : snapshot.skill_table_ids()) {
            const auto* skillRow = dataProvider->FindSkill(skillTableId);
            if (skillRow == nullptr || !IsTurnBattleCastableSkill(*skillRow) ||
                !seasonrules::IsSkillActiveInSeason(skillRow->season_id(), snapshot.active_season_id())) {
                continue;
            }
            if (skillRow->battle_model() == 1 && (!ConfiguredSubjectAllowed(actor, *skillRow) ||
                !actor.skill_ranks().contains(skillTableId))) continue;
            if (IsClassSkill(*skillRow) && ClassSkillRank(actor, *skillRow) == 0) {
                continue;
            }
            actor.add_skill_table_ids(skillTableId);
        }
        if (snapshot.ai_controlled()) {
            // 守旗阵 AI 托管守擂快照：全程自动出手，不阻塞手动指令收集，不写个人结算表。
            actor.set_is_auto(true);
            aiControlledPlayers.insert(snapshot.player_id());
        }
        actors.emplace_back(std::move(actor));

        if (!snapshot.ai_controlled()) {
            auto& settlement = settlements[snapshot.player_id()];
            settlement.set_battle_id(request.battle_id());
            settlement.set_player_id(snapshot.player_id());
        }
    }
    return true;
}

bool TurnBattleEngine::BuildPetActor(const BattlePlayerSnapshot& snapshot, const BattlePetSnapshot& pet,
                                     uint64_t actorId, uint32_t formationSlot, BattleActorState& actor) const {
    const bool allowTempPet = UsesFreePreset();
    const bool isCompetitiveTemp = pet.competitive_temporary();
    if (isCompetitiveTemp && !allowTempPet) {
        LOG_ERROR << "CreateBattle competitive_temporary pet rejected when use_free_preset is false: battle_id="
                  << createRequest.battle_id() << " player_id=" << snapshot.player_id();
        return false;
    }
    if (pet.pet_id() == 0 && !(isCompetitiveTemp && allowTempPet)) {
        return false;
    }
    if (pet.owner_player_id() != 0 && pet.owner_player_id() != snapshot.player_id()) {
        LOG_ERROR << "CreateBattle pet ownership does not match its containing snapshot: battle_id="
                  << createRequest.battle_id() << " pet_id=" << pet.pet_id()
                  << " owner=" << pet.owner_player_id() << " snapshot_player=" << snapshot.player_id();
        return false;
    }
    if (!petcultivation::ValidSnapshot(pet.pet_cultivation())) {
        LOG_ERROR << "CreateBattle invalid pet_cultivation snapshot: battle_id=" << createRequest.battle_id()
                  << " pet_id=" << pet.pet_id();
        return false;
    }

    actor.Clear();
    actor.set_actor_id(actorId);
    actor.set_pet_id(pet.pet_id());
    actor.set_competitive_temporary(isCompetitiveTemp);
    actor.set_actor_type(BATTLE_ACTOR_TYPE_PET);
    actor.set_team_index(snapshot.team_index());
    actor.set_owner_player_id(snapshot.player_id());
    actor.set_pet_table_id(pet.pet_table_id());
    if (!pet.cosmetic_art_key().empty()) {
        actor.set_pet_cosmetic_art_key(pet.cosmetic_art_key());
    }
    *actor.mutable_pet_cultivation() = pet.pet_cultivation();
    actor.set_name(pet.pet_name());
    actor.set_level(pet.level());
    *actor.mutable_attributes() = pet.base_attributes();
    actor.set_max_health(FallbackMax(pet.max_health(), pet.base_attributes().health()));
    actor.set_max_mana(FallbackMax(pet.max_mana(), pet.base_attributes().mana()));
    actor.set_physical_attack(pet.physical_attack());
    actor.set_magic_attack(pet.magic_attack());
    actor.set_defense(pet.defense());
    actor.set_formation_slot(formationSlot);
    actor.set_is_auto(true);
    for (const auto& [id, rank] : pet.skill_ranks()) {
        const auto* row = dataProvider->FindSkill(id);
        if (row != nullptr && ConfiguredSubjectAllowed(actor, *row)) {
            const uint32_t valid = classskill::EffectiveRank(rank, actor.level(), row->unlock_level(), row->max_rank());
            if (valid > 0) (*actor.mutable_skill_ranks())[id] = valid;
        }
    }
    for (const auto skillTableId : pet.skill_table_ids()) {
        const auto* skillRow = dataProvider->FindSkill(skillTableId);
        if (skillRow == nullptr || !IsTurnBattleCastableSkill(*skillRow) || IsClassSkill(*skillRow)) {
            continue;
        }
        if (skillRow->battle_model() == 1) {
            if (!ConfiguredSubjectAllowed(actor, *skillRow)) continue;
            if (!actor.skill_ranks().contains(skillTableId)) {
                if (pet.skill_ranks().contains(skillTableId) || classskill::RankLimit(actor.level(),
                    skillRow->unlock_level(), skillRow->max_rank()) == 0) continue;
                (*actor.mutable_skill_ranks())[skillTableId] = 1;
            }
        }
        actor.add_skill_table_ids(skillTableId);
    }
    if (!actor.skill_ranks().empty()) actor.set_is_auto(false);
    return true;
}

bool TurnBattleEngine::InitPets(const CreateBattleRequest& request) {
    const auto petAlreadyJoined = [this](uint64_t petId) {
        return std::any_of(actors.begin(), actors.end(), [petId](const BattleActorState& joined) {
            return joined.actor_type() == BATTLE_ACTOR_TYPE_PET && joined.pet_id() == petId;
        });
    };

    for (const auto& snapshot : request.players()) {
        if (snapshot.team_index() == 0u && HasTrialVow(trialjournal::kVowNoPet)) {
            continue;
        }
        for (const auto& pet : snapshot.pets()) {
            // 同一只永久宝宝出现在两份快照里 = 编排层错误,拒绝开局
            if (pet.pet_id() != 0 && petAlreadyJoined(pet.pet_id())) {
                LOG_ERROR << "CreateBattle pet participates more than once: battle_id="
                          << request.battle_id() << " pet_id=" << pet.pet_id();
                return false;
            }
            BattleActorState actor;
            if (!BuildPetActor(snapshot, pet, kPetActorIdBase + nextPetActorIndex,
                               NextFormationSlot(snapshot.team_index()), actor)) {
                return false;
            }
            ++nextPetActorIndex;
            actors.emplace_back(std::move(actor));
        }
    }
    return true;
}

bool TurnBattleEngine::InitMonsters(const CreateBattleRequest& request) {
    // 参考等级取玩家侧最高等级(怪物属性表列缺失时的保守基准)
    uint32_t referenceLevel = 1;
    // 进攻方(team 0)玩家数 = 队伍人数。只数人头:宝宝是玩家带进来的单位,不让怪物跟着变多
    uint32_t attackerCount = 0;
    for (const auto& snapshot : request.players()) {
        referenceLevel = std::max(referenceLevel, snapshot.level());
        if (snapshot.team_index() == 0) {
            ++attackerCount;
        }
    }

    initialAttackerHumanCount = attackerCount;

    if (hasTacticalPuzzleContext) {
        const auto& spec = tacticalPuzzleContext.spec();
        uint32_t monsterIndex = 0u;
        for (const auto& enemy : spec.enemies()) {
            AppendMonsterActor(enemy.monster_table_id(), monsterIndex, referenceLevel);
            auto& spawned = actors.back();
            const uint64_t baseMaxHp = spawned.max_health();
            const uint64_t initHp = TacticalPuzzleScalePct(baseMaxHp, enemy.initial_hp_pct());
            if (initHp > baseMaxHp) {
                spawned.set_max_health(initHp);
            }
            spawned.mutable_attributes()->set_health(initHp);
            const uint64_t baseSpeed = spawned.attributes().speed();
            spawned.mutable_attributes()->set_speed(
                TacticalPuzzleApplySpeedDelta(baseSpeed, enemy.speed_delta_pct()));
            spawned.set_capture_pet_table_id(0);
            ++monsterIndex;
        }
        return monsterIndex > 0u;
    }

    const bool isBossTrialOrRescue = IsBossTrialOrRescueKind(bossTrialKind);
    auto monsterIds = dataProvider->GetDungeonMonsterIds(request.battle_config_id());
    if (monsterIds.empty()) {
        // DungeonTable 缺怪物组列:按玩家人数生成默认怪,保证 PVE 一定有对手
        monsterIds.assign(static_cast<size_t>(request.players_size()), 0);
    } else if (!isBossTrialOrRescue) {
        if (const uint32_t monstersPerPlayer =
                dataProvider->GetDungeonMonstersPerPlayer(request.battle_config_id());
            monstersPerPlayer > 0 && attackerCount > 0 &&
            !IsActivityBattle(request.activity_context())) {
            // 活动对局(帮会同道历练等,请求带 activity_context)不放大;首领试炼改走精简精英阵，不复制白板杂兵。
            const uint32_t scaledCount =
                attackerCount * std::min(monstersPerPlayer, kMaxScaledPveMonsterCount);
            monsterIds = CycleMonsterGroup(monsterIds, std::min(scaledCount, kMaxScaledPveMonsterCount));
        }
    }

    // 首领试炼 V2 精简精英阵（boss-trial-planning-ui.md §2.4）：
    // 单卫战阵 2..3人追加破军卫(109)、4..5人追加鼓噪卫(110)；双卫同阵 3..5人追加破军卫(109)；全场至多 4 怪。
    // 仅当 Monster 表配置了 109/110 时生成，保持最小内存单测（未配策应卫）完全兼容。
    if (IsBossTrialKind(bossTrialKind)) {
        if (bossTrialKind == kBossTrialKindBreakDualGuard) {
            if (attackerCount >= 3u && dataProvider->FindMonster(kBossTrialMonsterSupportBreaker) != nullptr) {
                monsterIds.push_back(kBossTrialMonsterSupportBreaker);
            }
        } else {
            if (attackerCount >= 2u && dataProvider->FindMonster(kBossTrialMonsterSupportBreaker) != nullptr) {
                monsterIds.push_back(kBossTrialMonsterSupportBreaker);
            }
            if (attackerCount >= 4u && dataProvider->FindMonster(kBossTrialMonsterSupportDrummer) != nullptr) {
                monsterIds.push_back(kBossTrialMonsterSupportDrummer);
            }
        }
    }

    const uint32_t hpScale = isBossTrialOrRescue ? std::max<uint32_t>(1u, attackerCount) : 1u;
    if (IsFestivalBeastBattle()) {
        if (monsterIds.size() != 1) {
            return false;
        }
        AppendMonsterActor(monsterIds[0], 0, referenceLevel);
        auto& spawned = actors.back();
        const auto& fb = request.activity_context().festival_beast();
        spawned.set_is_boss(true);
        if (!fb.boss_name().empty()) {
            spawned.set_name(fb.boss_name());
        }
        spawned.set_max_health(fb.boss_health());
        spawned.mutable_attributes()->set_health(fb.boss_health());
        spawned.mutable_attributes()->set_strength(0);
        spawned.set_physical_attack(fb.boss_physical_attack());
        spawned.set_magic_attack(0);
        spawned.mutable_attributes()->set_armor(0);
        spawned.set_defense(fb.boss_defense());
        spawned.mutable_attributes()->set_resistance(fb.boss_resistance());
        spawned.mutable_attributes()->set_speed(fb.boss_speed());
        spawned.mutable_attributes()->set_critchance(0);
        spawned.clear_combat();
        spawned.clear_equipment_effects();
        spawned.clear_pet_cultivation();
        spawned.clear_skill_table_ids();
        spawned.clear_runtime_skills();
        spawned.set_capture_pet_table_id(0);
        return true;
    }
    uint32_t monsterIndex = 0;
    for (const auto monsterTableId : monsterIds) {
        AppendMonsterActor(monsterTableId, monsterIndex, referenceLevel);
        auto& spawned = actors.back();
        const bool isSupportGuard = monsterTableId == kBossTrialMonsterSupportBreaker ||
                                    monsterTableId == kBossTrialMonsterSupportDrummer;
        uint64_t baseHp = spawned.max_health();
        // 困难·双卫同阵防奶墙（V5）：回春与护势两位核心护卫基础血量压缩至 75%。
        if (bossTrialKind == kBossTrialKindBreakDualGuard && !spawned.is_boss() && !isSupportGuard) {
            baseHp = std::max<uint64_t>(1ULL, (baseHp * kBossDualGuardHpKeepPct) / 100ULL);
        }
        const uint32_t unitHpScale = isSupportGuard
            ? std::max<uint32_t>(1u, (attackerCount + 1u) / 2u)
            : hpScale;
        const uint64_t scaledHp = baseHp * unitHpScale;
        uint64_t finalHp = scaledHp;
        if (hasTowerContext) {
            const uint32_t hpPct = IsWuxingRealmV2()
                ? wuxing::RealmMonsterHealthPct(towerContext.floor(), towerContext.node_kind())
                : wuxing::TowerMonsterHealthPct(towerContext.floor());
            const uint32_t atkPct = IsWuxingRealmV2()
                ? wuxing::RealmMonsterAttackPct(towerContext.floor(), towerContext.node_kind())
                : wuxing::TowerMonsterAttackPct(towerContext.floor());
            finalHp = std::max<uint64_t>(1ULL, (scaledHp * hpPct) / 100ULL);
            if (spawned.attributes().strength() > 0) {
                spawned.mutable_attributes()->set_strength(
                    std::max<uint64_t>(1ULL, (spawned.attributes().strength() * atkPct) / 100ULL));
            }
            if (spawned.physical_attack() > 0) {
                spawned.set_physical_attack(std::max<uint64_t>(1ULL, (spawned.physical_attack() * atkPct) / 100ULL));
            }
            if (spawned.magic_attack() > 0) {
                spawned.set_magic_attack(std::max<uint64_t>(1ULL, (spawned.magic_attack() * atkPct) / 100ULL));
            }
        }
        if (finalHp != spawned.max_health()) {
            spawned.set_max_health(finalHp);
            spawned.mutable_attributes()->set_health(finalHp);
        }
        // 护卫救援：非首领的护卫单位归属玩家侧（team 0）作为受保护目标，死亡即刻判负。
        if (bossTrialKind == kBossTrialKindGuardianRescue && !spawned.is_boss() && protectedGuardActorId == 0) {
            const uint32_t allySlot = NextFormationSlot(0);
            spawned.set_team_index(0);
            spawned.set_formation_slot(allySlot);
            protectedGuardActorId = spawned.actor_id();
        }
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
    if (row != nullptr && row->is_boss() != 0) {
        actor.set_is_boss(true);
    }
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

    // 只有明确开放的野外配置展示捕捉目标，活动与高阶宠物一律关闭。
    const auto* dungeon = dataProvider->FindDungeon(createRequest.battle_config_id());
    const auto* pet = row == nullptr ? nullptr : dataProvider->FindPet(row->capture_pet_table_id());
    if (IsPveMatch() && !IsActivityBattle(createRequest.activity_context()) && dungeon != nullptr &&
        dungeon->allow_pet_capture() == 1 && row != nullptr && row->health() > 0 &&
        pet != nullptr && pet->acquisition_tier() == 0 &&
        row->capture_rate() > 0 && row->capture_rate() <= 10000) {
        actor.set_capture_pet_table_id(pet->id());
        actor.set_name(pet->name());
        actor.set_capture_min_level(pet->unlock_level());
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
    if (!initialized || outcome != BATTLE_OUTCOME_ONGOING || prepActive) {
        return false;
    }

    // 传输层先核本人/本宠归属;引擎对人物和宝宝使用同一行动校验。
    const auto* actor = FindActor(actorId);
    const uint64_t controllerId = actor != nullptr && actor->actor_type() == BATTLE_ACTOR_TYPE_PET
        ? actor->owner_player_id()
        : actorId;
    if (actor != nullptr && !aiControlledPlayers.contains(controllerId) &&
        (actor->actor_type() == BATTLE_ACTOR_TYPE_PLAYER || actor->actor_type() == BATTLE_ACTOR_TYPE_PET) && IsActorActive(*actor)) {
        // 校验链不过的行动不落账,按未提交处理(回合超时默认普攻兜底);
        // 截止前允许重复提交改主意,最后一次为准
        if (CheckActionPrerequisites(*actor, action) == kSuccess) {
            pendingActions[actorId] = action;
            RecordManualActionForActor(*actor);
        }
    }

    return AllPlayersReady();
}

uint32_t TurnBattleEngine::ValidateAction(uint64_t actorId, const BattleAction& action) const {
    // 与 SubmitAction 的接受条件逐条对齐:两者判据不一致会让客户端收到"成功"却没落账。
    if (!initialized || outcome != BATTLE_OUTCOME_ONGOING) {
        return kInvalidParameter;
    }
    if (prepActive) {
        return kFeatureUnavailable;
    }
    const auto* actor = FindActor(actorId);
    if (actor == nullptr || (actor->actor_type() != BATTLE_ACTOR_TYPE_PLAYER && actor->actor_type() != BATTLE_ACTOR_TYPE_PET)) {
        return kInvalidParameter;
    }
    const uint64_t controllerId = actor->actor_type() == BATTLE_ACTOR_TYPE_PET ? actor->owner_player_id() : actorId;
    if (aiControlledPlayers.contains(controllerId)) {
        return kInvalidParameter;
    }
    if (!IsActorActive(*actor)) {
        return kThisEntityIsInvalid;
    }
    return CheckActionPrerequisites(*actor, action);
}

uint32_t TurnBattleEngine::SetActorAuto(uint64_t actorId, bool enabled) {
    // 只翻转状态位,不消耗 RNG、不触发结算:确定性由默认行动路径保证,
    // "开 auto 后立即结算"由节点在查 AllPlayersReady 后自行调 ResolveCurrentRound
    if (!initialized || outcome != BATTLE_OUTCOME_ONGOING) {
        return kInvalidParameter;
    }
    if (prepActive) {
        return kFeatureUnavailable;
    }
    if (aiControlledPlayers.contains(actorId)) {
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
    if (enabled) {
        explicitAutoPlayers.insert(actorId);
    }
    for (auto& pet : actors) if (pet.actor_type() == BATTLE_ACTOR_TYPE_PET && pet.owner_player_id() == actorId &&
        !pet.skill_ranks().empty()) pet.set_is_auto(enabled);
    return 0;
}

bool TurnBattleEngine::AllPlayersReady() const {
    if (prepActive) {
        return false;
    }
    for (const auto& actor : actors) {
        if ((actor.actor_type() != BATTLE_ACTOR_TYPE_PLAYER && actor.actor_type() != BATTLE_ACTOR_TYPE_PET) || !IsActorActive(actor)) {
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
    if (actor.actor_type() == BATTLE_ACTOR_TYPE_PET && action.action_type() != BATTLE_ACTION_ATTACK &&
        action.action_type() != BATTLE_ACTION_SKILL && action.action_type() != BATTLE_ACTION_DEFEND)
        return kInvalidParameter;

    switch (action.action_type()) {
    case BATTLE_ACTION_ATTACK:
        if (HasConfiguredState(actor, BattleStateKind::SealPhysical) || IsPhysicalForbidden(actor)) {
            return kSkillCannotBeCastInCurrentState;
        }
        return kSuccess;
    case BATTLE_ACTION_DEFEND:
        // 普攻目标失效时结算期会重选,防御无目标,状态检查通过即可
        return kSuccess;
    case BATTLE_ACTION_CAPTURE:
        return CheckCapture(actor, action.target_id());
    case BATTLE_ACTION_RECALL_PET: {
        const auto* pet = FindActor(action.target_id());
        return actor.actor_type() == BATTLE_ACTOR_TYPE_PLAYER && pet != nullptr &&
            pet->actor_type() == BATTLE_ACTOR_TYPE_PET && pet->owner_player_id() == actor.actor_id() &&
            pet->fled() && !pet->is_dead() && pet->withdraw_until_round() > 0 &&
            roundIndex >= pet->withdraw_until_round() ? kSuccess : kInvalidParameter;
    }
    case BATTLE_ACTION_SUMMON_PET:
        return CheckSummonPet(actor, action.summon_pet_id());
    case BATTLE_ACTION_FLEE: {
        // PVE 可逃,PVP 一期不可逃(设计文档 §5.1)
        return IsPveMatch() ? kSuccess : kInvalidParameter;
    }
    case BATTLE_ACTION_ITEM:
        return CheckItemUse(actor, action);
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
        // 类型白名单(G6):InitPlayers 已把不可施放的类型剔出列表,这里是第二道闸,
        // 防止日后有人绕过 skill_table_ids 直接塞技能 id
        if (!IsTurnBattleCastableSkill(*skillRow)) {
            return kSkillCannotBeCastInCurrentState;
        }
        if (IsControlBannedForActor(actor) && IsControlSkill(*skillRow)) {
            return kSkillCannotBeCastInCurrentState;
        }
        if (IsSkillBanned(skillRow->id())) {
            return kSkillBannedInBattle;
        }
        if (skillRow->battle_model() == 1) return CheckConfiguredSkill(actor, action, *skillRow);
        if (IsClassSkill(*skillRow)) {
            // 职业技能:列表里有还不够,等级必须复核为正;目标按阵营校验
            if (ClassSkillRank(actor, *skillRow) == 0) {
                return kInvalidParameter;
            }
            if (const auto result = ValidateClassSkillTarget(actor, *skillRow, action.target_id());
                result != kSuccess) {
                return result;
            }
        } else if (const auto result = ValidateSkillTarget(actor, *skillRow, action.target_id());
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

uint32_t TurnBattleEngine::CheckItemUse(const BattleActorState& actor,
                                       const BattleAction& action) const {
    if (IsFestivalBeastBattle()) {
        return kInvalidParameter;
    }
    // 提交期与出手期共用这一份判据:两处判据分叉会让"提交时说行、出手时落空"重新出现。
    const auto* itemRow = dataProvider->FindItem(action.item_table_id());
    if (itemRow == nullptr) {
        return kInvalidTableId;
    }
    if (itemRow->battle_usable() == 0) {
        // 不是战斗消耗品(装备/材料/任务道具):快照本来就不会带它进来,
        // 这里挡的是客户端自造 item_table_id
        return kInvalidParameter;
    }
    if (itemRow->battle_heal_hp() == 0 && itemRow->battle_heal_mp() == 0) {
        // 标了可用但两列效果都是 0(配表半填):放行等于"药被吃掉、回合被浪费",
        // 玩家资产白白损失。提交期就拒,ExecuteItem 共用本判据,自然也不会扣副本
        return kInvalidParameter;
    }
    if (FindItemEntry(actor.actor_id(), action.item_table_id()) == nullptr) {
        return kBagInsufficientItems;  // 副本里没有或已用尽
    }
    // PVP 限次:不限次用药可以把回合拖满,而打满 max_rounds 判进攻方负(§5.1)
    if (!IsPveMatch()) {
        const auto used = itemUseCounts.find(actor.actor_id());
        if (used != itemUseCounts.end() && used->second >= kMaxItemUsesPerBattlePvp) {
            return kInvalidParameter;
        }
    }
    // 目标:0 或自己 = 自疗;其余必须是同队存活单位(含队友的宝宝),不能给敌方用药
    if (action.target_id() != 0 && action.target_id() != actor.actor_id()) {
        const auto* target = FindActor(action.target_id());
        if (target == nullptr || !IsActorActive(*target)) {
            return kSkillInvalidTargetId;
        }
        if (target->team_index() != actor.team_index()) {
            return kSkillInvalidTarget;
        }
    }
    return kSuccess;
}

bool TurnBattleEngine::IsTurnBattleCastableSkill(const SkillTable& skillRow) {
    if (skillRow.battle_model() == 1) return skillRow.ability_kind() == 1;
    // 黑名单而不是白名单:表里存量技能未必都填了 skill_type,白名单会把没填的一律判死。
    // 被动技能没有施放动作;开关技能是状态切换;持续施法依赖引擎已删除的相位概念 ——
    // 这三类若被当成普通技能提交,会按 damage 表达式打出玩家本不该有的伤害。
    // 职业被动认 passive_stat(被动位号 0 在表里写不出来,见头文件)
    if (!skillRow.passive_stat().empty()) {
        return false;
    }
    for (const auto skillTypeBit : skillRow.skill_type()) {
        if (skillTypeBit == kSkillTypeBitPassive || skillTypeBit == kSkillTypeBitToggle ||
            skillTypeBit == kSkillTypeBitChannel) {
            return false;
        }
    }
    return true;
}

uint32_t TurnBattleEngine::CheckSkillCost(const BattleActorState& actor,
                                          const SkillTable& skillRow) const {
    // 耗蓝不足:提交期不落账(回合超时默认普攻兜底),出手期降级为普攻(ExecuteSkill)。
    // tip 表暂无"法力不足"专用码,借用"当前状态不可施放"(见任务 open_issues)
    if (SkillManaCost(actor, skillRow) > actor.attributes().mana()) {
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
    // 职业技能按技能各自冷却(Skill.cooldown_rounds,键就是 skill_table_id),不参与 cooldown_id 分组
    if (IsClassSkill(skillRow)) {
        const auto it = actor.skill_cooldown_rounds().find(skillRow.id());
        return it != actor.skill_cooldown_rounds().end() && it->second > 0 ? kSkillCooldownNotReady : kSuccess;
    }
    // 冷却按 cooldown_id 分组共享(镜像实时的 CooldownTimeListComp 键语义);
    // 状态 map 的键按 proto 契约是 skill_table_id,分组语义靠反查各在冷技能的 cooldown_id。
    // map 遍历序不定,但结果是"是否存在"布尔,不影响确定性
    for (const auto& coolingEntry : actor.skill_cooldown_rounds()) {
        if (coolingEntry.second == 0) {
            continue;
        }
        const auto* coolingRow = dataProvider->FindSkill(coolingEntry.first);
        // 在冷的职业技能 cooldown_id 都是 0,不能把 cooldown_id 同为 0 的老技能一起锁住
        if (coolingRow != nullptr && !IsClassSkill(*coolingRow) &&
            coolingRow->cooldown_id() == skillRow.cooldown_id()) {
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
        const auto cell = permissionRow->skill_type(columnIndex);
        // 0 是"这格没填"而不是任何一个 tip 码(tip 轴里 kSuccess=1000、kCommon_errorOK=0)。
        // 原样返回 0 会让节点把它写进 error_message.id,客户端读成"成功"却没落账 ——
        // 正是 G2 要消灭的静默降级。坏表按坏表报。
        if (cell == 0) {
            return kInvalidTableData;
        }
        if (cell != kSuccess) {
            return cell;
        }
    }
    return kSuccess;
}

uint32_t TurnBattleEngine::CheckState(const BattleActorState& actor) const {
    if (HasConfiguredState(actor, BattleStateKind::Freeze) || HasConfiguredState(actor, BattleStateKind::Sleep))
        return kSkillCannotBeCastStunRestriction;
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

    if (prepActive) {
        FinalizePrepPhase();
    }

    // 1. 未提交者填默认行动(宝宝与挂机玩家可选技能,其余玩家与怪物仍普攻)
    BeginConfiguredRound(result);
    // 人宠五行相生合击（每 3 回合自动共鸣，主角五行与出战存活灵宠五行满足相生即激活 +10% 伤害/治疗/护盾）。
    wuxingSynergyActiveThisRound = false;
    wuxingSynergyActorIds.clear();
    if (wuxing::IsSynergyRound(roundIndex)) {
        for (const auto& owner : actors) {
            if (owner.actor_type() != BATTLE_ACTOR_TYPE_PLAYER || !IsActorActive(owner)) {
                continue;
            }
            const uint32_t ownerElement = wuxing::ElementOfClass(owner.class_id());
            if (ownerElement == wuxing::kElementNone) {
                continue;
            }
            for (const auto& pet : actors) {
                if (pet.actor_type() != BATTLE_ACTOR_TYPE_PET || pet.owner_player_id() != owner.actor_id() ||
                    !IsActorActive(pet)) {
                    continue;
                }
                const uint32_t petElement = wuxing::ElementOfPet(pet.pet_table_id());
                if (wuxing::AreElementsMutuallyGenerating(ownerElement, petElement)) {
                    wuxingSynergyActiveThisRound = true;
                    wuxingSynergyActorIds.insert(owner.actor_id());
                    wuxingSynergyActorIds.insert(pet.actor_id());
                    auto* synergyEvent = AppendEvent(result, BATTLE_EVENT_WUXING_SYNERGY, owner.actor_id(), pet.actor_id());
                    synergyEvent->set_value(wuxing::kSynergyBonusPct);
                    break;
                }
            }
        }
    }
    // 锦标赛 V2 水润灵脉 + 云垂阵 + 五行幻境回合初法力/气血恢复（均封顶至最大值）。
    boonRuntime.freeManaUsedThisRound.clear();
    boonRuntime.firstAttackUsedThisRound.clear();
    boonRuntime.petAssistUsedThisRound.clear();
    const bool waterLeyline = ActiveTournamentLeyline() == TOURNAMENT_LEYLINE_WATER;
    for (auto& actor : actors) {
        if (!IsActorActive(actor)) {
            continue;
        }
        uint64_t manaRegenPct = waterLeyline ? 5ULL : 0ULL;
        manaRegenPct += wuxing::FormationOf(TeamFormationId(actor.team_index())).roundManaRegenPct;
        if (hasTowerContext && actor.team_index() == 0) {
            manaRegenPct += towerSigilTotals.roundManaRegenPct;
        }
        if (manaRegenPct > 0 && actor.max_mana() > 0 && actor.attributes().mana() < actor.max_mana()) {
            const uint64_t curMana = actor.attributes().mana();
            uint64_t regen = std::max<uint64_t>(1ULL, (actor.max_mana() * manaRegenPct) / 100ULL);
            if (actor.team_index() == 0 && HasRealmResonance(wuxing::kElementWater)) {
                regen = (regen * 130ULL) / 100ULL;
            }
            actor.mutable_attributes()->set_mana(std::min(actor.max_mana(), SaturatingAdd(curMana, regen)));
        }
        uint64_t hpRegenPct = (hasTowerContext && actor.team_index() == 0) ? towerSigilTotals.roundHpRegenPct : 0ULL;
        if (actor.team_index() == 0 && HasRealmResonance(wuxing::kElementWood)) {
            hpRegenPct += 6ULL;
        }
        if (hpRegenPct > 0 && actor.max_health() > 0 && actor.attributes().health() < actor.max_health()) {
            const uint64_t curHp = actor.attributes().health();
            const uint64_t regenHp = std::max<uint64_t>(1ULL, (actor.max_health() * hpRegenPct) / 100ULL);
            actor.mutable_attributes()->set_health(std::min(actor.max_health(), SaturatingAdd(curHp, regenHp)));
        }
    }
    ApplyWuxingRoundStartBoonEffects(result);
    FillDefaultActions();

    // 事件组编号每回合从 1 起(表现规格 D2);出手序在回合开始时一次排定并保留下来
    // (表现规格 D3:含回合中途死亡/逃离而被跳过者,节点透传到 TurnResultS2C.action_order)
    currentGroupId = 0;
    currentHitIndex = 0;
    lastActionOrder = BuildTurnOrder();
    roundStartSpeeds.clear();
    for (const auto& actor : actors) {
        if (IsActorActive(actor)) {
            roundStartSpeeds[actor.actor_id()] = EffectiveSpeed(actor);
        }
    }

    // 2. 速度降序、同速按 actor_id 稳定序,逐个执行;死亡/已逃单位跳过。
    //    本回合承担慢速蓄力重击或周期机制的试炼首领/护卫在慢速阶段统一结算，不在普通循环重复行动。
    for (const auto actorId : lastActionOrder) {
        if (IsEncounterSlowPhaseActor(actorId)) {
            continue;
        }
        auto* actor = FindActor(actorId);
        if (actor == nullptr || !IsActorActive(*actor)) {
            continue;
        }
        if (actor->actor_type() == BATTLE_ACTOR_TYPE_PLAYER) {
            const bool hardControlled =
                ActorHasBuffOfType(*actor, kBuffTypeStun) ||
                ActorHasBuffOfType(*actor, kBuffTypeFreeze) ||
                HasConfiguredState(*actor, BattleStateKind::Freeze) ||
                HasConfiguredState(*actor, BattleStateKind::Sleep) ||
                HasConfiguredState(*actor, BattleStateKind::Confusion);
            if (!hardControlled) {
                hadOperableTurnPlayers.insert(actor->actor_id());
            }
        }
        const auto actionIt = pendingActions.find(actorId);
        if (actionIt == pendingActions.end()) {
            continue;
        }
        // 每个行动一组:该行动产生的全部事件(含降级普攻/死亡/buff)共用 group_id
        BeginEventGroup();
        ExecuteAction(*actor, actionIt->second, result);
    }

    // 2.5 首领试炼慢速重击与护卫周期机制：排在全部常规行动之后、回合末 buff tick 与摘除防御之前，
    //     确保低速单位点击防御后必然在本回合享受到防御减半，且受控首领/护卫的预告动作被直接取消不回退普攻。
    ExecuteBossTrialSlowAndGuardianActions(result);

    // 3. 回合末 buff tick:周期效果 → 持续减一 → 到期移除
    TickBuffsAtRoundEnd(result);
    TickConfiguredBuffs(result);
    ApplyWuxingRoundEndBoonEffects(result);

    // 4. 冷却回合递减
    DecayCooldowns();

    // 5. 防御只覆盖本回合(含刚结算的回合末周期伤害),此刻统一摘除
    for (auto& actor : actors) {
        actor.set_is_defending(false);
        for (auto& artifact : *actor.mutable_artifact_states()) {
            if (artifact.pending_round() != 0 && artifact.pending_round() <= roundIndex) {
                artifact.clear_pending_round(); artifact.clear_pending_ailment_kind();
            }
        }
    }

    // 6. 胜负判定(含 max_rounds 打满进攻方判负、护卫救援护卫阵亡即负、物法易势满 16 回合平局)
    UpdateOutcome();

    // 6.5 首领试炼 V2 回合末结转：鼓噪卫蓄势、护势剩余回合递减、二阶段「决战姿态」切换（绝不在回合中途插队）。
    if (outcome == BATTLE_OUTCOME_ONGOING && IsBossTrialKind(bossTrialKind)) {
        ProcessBossTrialRoundEnd(result);
        UpdateOutcome();
    }

    // 6.8 群仙问道赛 V3 MVP 累计统计：基于本回合最终确定性事件流汇总每名玩家（含其宝宝）的伤害/治疗护盾/硬控。
    AccumulateRoundMvpMetrics(result);

    // 7. 终局奖励只生成一次，固定额度与独立随机结果写进结算账本供重复读取。
    RollRewards();

    // 记录每名玩家连续未手动提交回合数（用于多人投降自动排除挂机队友，零时钟依赖）。
    for (const auto& actor : actors) {
        if (actor.actor_type() != BATTLE_ACTOR_TYPE_PLAYER) {
            continue;
        }
        const auto it = manualActionRoundsByPlayer.find(actor.actor_id());
        const bool actedManuallyThisRound =
            it != manualActionRoundsByPlayer.end() && it->second.contains(roundIndex);
        if (actedManuallyThisRound) {
            consecutiveIdleRoundsByPlayer[actor.actor_id()] = 0;
        } else {
            ++consecutiveIdleRoundsByPlayer[actor.actor_id()];
        }
    }

    pendingActions.clear();
    if (outcome == BATTLE_OUTCOME_ONGOING) {
        ExpireSurrenderVotesForRoundAdvance();
        teamPings[0].clear();
        teamPings[1].clear();
        roundPingCountByPlayer.clear();
        ++roundIndex;
        attackerTriggeredStanceCounterThisRound.clear();
        weeklyAffixWoodManaTriggeredActors.clear();
        for (auto it = exposedActors.begin(); it != exposedActors.end(); ) {
            if (it->second.untilRound < roundIndex) {
                it = exposedActors.erase(it);
            } else {
                ++it;
            }
        }
        for (auto it = sunderUntilRound.begin(); it != sunderUntilRound.end(); ) {
            if (it->second < roundIndex) {
                it = sunderUntilRound.erase(it);
            } else {
                ++it;
            }
        }
        UpdatePolarityForRound();
        RefreshEncounterTelegraphs();
    } else {
        activeTelegraphs.clear();
    }

    *result.mutable_state() = BuildStateSnapshot();
    return result;
}

void TurnBattleEngine::FillDefaultActions() {
    // 有剧本的队先按剧本出手;未由剧本决定的怪物若挂了副本智能 AI 则走 HFSM + 行为树 + 定点效用打分;
    // 其余单位(无剧本/无 AI 怪物、宝宝、挂机或超时玩家)照下面的现行默认走。
    FillScriptActions();
    FillDungeonMonsterAiActions();

    for (const auto& actor : actors) {
        if (!IsActorActive(actor)) {
            continue;
        }
        if (pendingActions.find(actor.actor_id()) != pendingActions.end()) {
            continue;
        }
        // 护卫救援：玩家侧待保护护卫默认防御保命。
        if (protectedGuardActorId != 0 && actor.actor_id() == protectedGuardActorId) {
            BattleAction guardDefend;
            guardDefend.set_action_type(BATTLE_ACTION_DEFEND);
            pendingActions.emplace(actor.actor_id(), std::move(guardDefend));
            continue;
        }
        // 超时/掉线/怪物:默认普攻,目标置 0,结算时用引擎 RNG 随机存活敌方；禁物专精者兜底防御。
        BattleAction defaultAction;
        defaultAction.set_action_type(IsPhysicalForbidden(actor) ? BATTLE_ACTION_DEFEND : BATTLE_ACTION_ATTACK);
        defaultAction.set_target_id(0);
        if (actor.actor_type() == BATTLE_ACTOR_TYPE_PET || actor.is_auto()) {
            if (auto selected = SelectAutoConfiguredSkill(actor); selected.has_value()) {
                pendingActions.emplace(actor.actor_id(), *selected);
                continue;
            }
        }
        // 宝宝与挂机玩家自动选技能;非挂机玩家超时及怪物仍沿用原普攻路径。
        // 宝宝的技能按快照顺序、目标按单位插入序选择,挂机玩家见 SelectAutoClassSkill,选招都不消费 RNG。
        // 共用完整校验链,冷却/法力/沉默不满足时回落普攻,出手时还会再校验。
        if (actor.actor_type() == BATTLE_ACTOR_TYPE_PET && !actor.skill_table_ids().empty()) {
            const auto enemies = CollectAliveEnemyIds(actor);
            if (!enemies.empty()) {
                for (const auto skillId : actor.skill_table_ids()) {
                    BattleAction skillAction;
                    skillAction.set_action_type(BATTLE_ACTION_SKILL);
                    skillAction.set_skill_table_id(skillId);
                    skillAction.set_target_id(enemies.front());
                    if (CheckActionPrerequisites(actor, skillAction) == kSuccess) {
                        defaultAction = std::move(skillAction);
                        break;
                    }
                }
            }
        } else if (actor.actor_type() == BATTLE_ACTOR_TYPE_PLAYER && actor.is_auto()) {
            if (auto autoAction = SelectAutoClassSkill(actor); autoAction.has_value()) {
                defaultAction = std::move(*autoAction);
            }
        }
        pendingActions[actor.actor_id()] = defaultAction;
    }
}

void TurnBattleEngine::SetMonsterAiProfiles(std::map<uint32_t, DungeonMonsterAiProfile> profiles) {
    monsterAiProfiles = std::move(profiles);
}

std::optional<BossPhaseId> TurnBattleEngine::MonsterAiPhaseOf(uint64_t actorId) const {
    const auto it = monsterAiControllers.find(actorId);
    if (it == monsterAiControllers.end()) {
        return std::nullopt;
    }
    return it->second.CurrentPhase();
}

void TurnBattleEngine::BindDungeonMonsterAis() {
    monsterAiControllers.clear();
    if (monsterAiProfiles.empty()) {
        return;
    }
    const auto fallbackIt = monsterAiProfiles.find(0);
    for (auto& actor : actors) {
        if (actor.actor_type() != BATTLE_ACTOR_TYPE_MONSTER) {
            continue;
        }
        auto it = monsterAiProfiles.find(actor.monster_table_id());
        if (it == monsterAiProfiles.end()) {
            it = fallbackIt;
        }
        if (it == monsterAiProfiles.end()) {
            continue;
        }

        const auto& profile = it->second;
        // 与战斗剧本保持同一闸门:怪物只能携带零耗蓝且回合制可施放的老技能(升序去重)。
        std::set<uint32_t> validSkills;
        for (const uint32_t skillId : profile.monsterSkillIds) {
            if (skillId == 0) {
                continue;
            }
            const auto* skillRow = dataProvider->FindSkill(skillId);
            if (skillRow == nullptr || skillRow->class_id() != 0 ||
                !IsTurnBattleCastableSkill(*skillRow) || LegacySkillManaCost(*skillRow) != 0) {
                continue;
            }
            validSkills.insert(skillId);
        }
        for (const uint32_t skillId : validSkills) {
            const auto& owned = actor.skill_table_ids();
            if (std::find(owned.begin(), owned.end(), skillId) == owned.end()) {
                actor.add_skill_table_ids(skillId);
            }
        }
        monsterAiControllers.emplace(actor.actor_id(), DungeonMonsterAiController(profile));
    }
}

void TurnBattleEngine::FillDungeonMonsterAiActions() {
    if (monsterAiControllers.empty()) {
        return;
    }
    for (const auto& actor : actors) {
        if (actor.actor_type() != BATTLE_ACTOR_TYPE_MONSTER) {
            continue;
        }
        auto ctrlIt = monsterAiControllers.find(actor.actor_id());
        if (ctrlIt == monsterAiControllers.end()) {
            continue;
        }
        if (!IsActorActive(actor)) {
            ctrlIt->second.NotifyDead();
            continue;
        }
        if (pendingActions.find(actor.actor_id()) != pendingActions.end()) {
            continue;
        }
        // 护卫救援：玩家侧待保护护卫不走敌方怪物 AI。
        if (protectedGuardActorId != 0 && actor.actor_id() == protectedGuardActorId) {
            continue;
        }

        MonsterDecisionInput input;
        input.roundIndex = roundIndex;
        input.selfAlive = true;
        input.selfCanDefend = true;
        if (actor.max_health() > 0) {
            const uint64_t clampedHp = std::min(actor.attributes().health(), actor.max_health());
            input.selfHpPermyriad = static_cast<uint32_t>(
                (clampedHp * static_cast<uint64_t>(combat_ai::kUtilityScale)) / actor.max_health());
        } else {
            input.selfHpPermyriad = 0;
        }

        const uint64_t maxEnemySpeed = std::max<uint64_t>(1, MaxAliveEnemySpeed(actor));
        for (const auto& target : actors) {
            if (target.team_index() == actor.team_index() || !IsActorActive(target)) {
                continue;
            }

            int32_t missingHp = 0;
            if (target.max_health() > 0) {
                const uint64_t clampedHp = std::min(target.attributes().health(), target.max_health());
                missingHp = static_cast<int32_t>(
                    ((target.max_health() - clampedHp) * static_cast<uint64_t>(combat_ai::kUtilityScale)) /
                    target.max_health());
            }
            const bool controlled = ActorHasBuffOfType(target, kBuffTypeStun) ||
                                    ActorHasBuffOfType(target, kBuffTypeSilence) ||
                                    ActorHasBuffOfType(target, kBuffTypeFreeze);
            const int32_t uncontrolled = controlled ? 0 : combat_ai::kUtilityScale;
            const int32_t threat = static_cast<int32_t>(std::min<uint64_t>(
                static_cast<uint64_t>(combat_ai::kUtilityScale),
                (EffectiveSpeed(target) * static_cast<uint64_t>(combat_ai::kUtilityScale)) / maxEnemySpeed));
            const int32_t targetIsPlayer =
                (target.actor_type() == BATTLE_ACTOR_TYPE_PLAYER) ? combat_ai::kUtilityScale : 3000;

            auto pushCandidateIfLegal = [&](const BattleAction& action, int32_t isSkillFlag) {
                if (CheckActionPrerequisites(actor, action) != kSuccess) {
                    return;
                }
                MonsterActionCandidate cand;
                cand.action = action;
                cand.formationSlot = target.formation_slot();
                cand.features[kFeatureTargetMissingHp] = missingHp;
                cand.features[kFeatureTargetUncontrolled] = uncontrolled;
                cand.features[kFeatureTargetThreat] = threat;
                cand.features[kFeatureIsSkill] = isSkillFlag;
                cand.features[kFeaturePhaseUrgency] = 0; // 由行为树按当前 HFSM 阶段回填
                cand.features[kFeatureTargetIsPlayer] = targetIsPlayer;
                input.candidates.push_back(std::move(cand));
            };

            BattleAction attackAction;
            attackAction.set_action_type(BATTLE_ACTION_ATTACK);
            attackAction.set_target_id(target.actor_id());
            pushCandidateIfLegal(attackAction, 0);

            for (const uint32_t skillId : actor.skill_table_ids()) {
                BattleAction skillAction;
                skillAction.set_action_type(BATTLE_ACTION_SKILL);
                skillAction.set_skill_table_id(skillId);
                skillAction.set_target_id(target.actor_id());
                pushCandidateIfLegal(skillAction, combat_ai::kUtilityScale);
            }
        }

        if (auto decided = ctrlIt->second.DecideAction(input); decided.has_value()) {
            pendingActions.emplace(actor.actor_id(), std::move(*decided));
        }
    }
}

// ---------------------------------------------------------------------------
// 战斗剧本(docs/design/battle-script.md)
// ---------------------------------------------------------------------------

void TurnBattleEngine::BindBattleScripts() {
    bool seenTeam[2] = {false, false};
    for (const auto& teamScript : createRequest.team_scripts()) {
        const uint32_t teamIndex = teamScript.team_index();
        if (teamIndex > 1 || seenTeam[teamIndex]) {
            scriptRejections.push_back("team_index " + std::to_string(teamIndex) +
                                       ": out of range or more than one script for the team");
            continue;
        }
        seenTeam[teamIndex] = true;

        // 哪一方:队里有怪物就是 PVE 的怪物方,否则是玩家组成的一方(PVE 进攻方、PVP 两方)
        const bool monsterSide =
            std::any_of(actors.begin(), actors.end(), [teamIndex](const BattleActorState& actor) {
                return actor.team_index() == teamIndex && actor.actor_type() == BATTLE_ACTOR_TYPE_MONSTER;
            });
        // 第二道闸:match 已按同一规则校验过;这里按本节点的配表再校验,不合法整份忽略、该队出手照旧
        std::string error;
        auto normalized = NormalizeBattleScript(teamScript.script(), *dataProvider, error);
        if (!normalized.has_value() ||
            !CheckBattleScriptSide(*normalized, monsterSide ? BATTLE_SCRIPT_SIDE_MONSTER : BATTLE_SCRIPT_SIDE_PLAYER,
                                   *dataProvider, error)) {
            scriptRejections.push_back("team_index " + std::to_string(teamIndex) + ": " + error);
            continue;
        }
        auto script = std::make_shared<const BattleScript>(std::move(*normalized));

        // 认领:单位按阵位(插入序)逐个找第一个接收它、且还没满员的角色位(角色位已按 order 排好)
        std::map<uint32_t, uint32_t> roleMembers;  // role_id → 已认领数
        for (auto& actor : actors) {
            if (actor.team_index() != teamIndex) {
                continue;
            }
            for (const auto& role : script->roles()) {
                if (!ScriptRoleAccepts(role, actor)) {
                    continue;
                }
                auto& members = roleMembers[role.role_id()];
                if (role.max_members() != 0 && members >= role.max_members()) {
                    continue;
                }
                ++members;
                scriptRoleOf[actor.actor_id()] = role.role_id();
                // 怪物表没有技能列:认领了角色位的怪物,技能集就是该角色位规则引用的技能(升序去重)。
                // 玩家与宝宝的技能来自快照,剧本引用了他们没有的技能,出手前的校验链会拦下、落到下一条规则
                if (actor.actor_type() == BATTLE_ACTOR_TYPE_MONSTER) {
                    std::set<uint32_t> skillIds;
                    for (const auto& rule : role.rules()) {
                        if (rule.action() == BATTLE_ACTION_SKILL && rule.skill_id() != 0) {
                            skillIds.insert(rule.skill_id());
                        }
                    }
                    for (const auto skillId : skillIds) {
                        const auto& owned = actor.skill_table_ids();
                        if (std::find(owned.begin(), owned.end(), skillId) == owned.end()) {
                            actor.add_skill_table_ids(skillId);
                        }
                        if (const auto* row = dataProvider->FindSkill(skillId); row != nullptr && row->battle_model() == 1) {
                            (*actor.mutable_skill_ranks())[skillId] = 1;
                        }
                    }
                }
                break;
            }
        }
        teamScripts[teamIndex] = std::move(script);
    }
}

bool TurnBattleEngine::ScriptRoleAccepts(const BattleScriptRole& role, const BattleActorState& actor) {
    if (role.actor_type() != BATTLE_ACTOR_TYPE_NONE && role.actor_type() != actor.actor_type()) {
        return false;
    }
    if (role.class_id() != 0 &&
        (actor.actor_type() != BATTLE_ACTOR_TYPE_PLAYER || actor.class_id() != role.class_id())) {
        return false;
    }
    if (role.monster_id() != 0 &&
        (actor.actor_type() != BATTLE_ACTOR_TYPE_MONSTER || actor.monster_table_id() != role.monster_id())) {
        return false;
    }
    return true;
}

void TurnBattleEngine::FillScriptActions() {
    for (uint32_t teamIndex = 0; teamIndex < 2; ++teamIndex) {
        const auto& script = teamScripts[teamIndex];
        if (script == nullptr) {
            continue;
        }

        // 手动提交的行动先登记成本角色位的认领:跟随的角色位(组合技、集火)可以接着队友手选的目标打。
        // target_id 为 0 的(交给引擎选)没有确定目标,不登记
        ScriptClaims claims;
        for (const auto& actor : actors) {
            if (actor.team_index() != teamIndex) {
                continue;
            }
            const auto roleIt = scriptRoleOf.find(actor.actor_id());
            const auto actionIt = pendingActions.find(actor.actor_id());
            if (roleIt != scriptRoleOf.end() && actionIt != pendingActions.end() &&
                actionIt->second.target_id() != 0) {
                claims.emplace_back(roleIt->second, actionIt->second.target_id());
            }
        }

        // 决策顺序 = 角色位 order(脚本已排好),同一角色位内按阵位。先决策的先定目标,后决策的才能跟
        for (const auto& role : script->roles()) {
            for (const auto& actor : actors) {
                if (actor.team_index() != teamIndex || !IsActorActive(actor) ||
                    pendingActions.find(actor.actor_id()) != pendingActions.end()) {
                    continue;
                }
                const auto roleIt = scriptRoleOf.find(actor.actor_id());
                if (roleIt == scriptRoleOf.end() || roleIt->second != role.role_id()) {
                    continue;
                }
                auto action = SelectScriptAction(actor, role, claims);
                if (!action.has_value()) {
                    continue;  // 规则全落空:留给 FillDefaultActions 的现行默认
                }
                if (action->target_id() != 0) {
                    claims.emplace_back(role.role_id(), action->target_id());
                }
                pendingActions[actor.actor_id()] = std::move(*action);
            }
        }
    }
}

std::optional<BattleAction> TurnBattleEngine::SelectScriptAction(const BattleActorState& actor,
                                                                 const BattleScriptRole& role,
                                                                 const ScriptClaims& claims) const {
    for (const auto& rule : role.rules()) {
        const bool conditionsHold =
            std::all_of(rule.conditions().begin(), rule.conditions().end(),
                        [this, &actor](const BattleScriptCondition& condition) {
                            return ScriptConditionHolds(actor, condition);
                        });
        if (!conditionsHold) {
            continue;
        }

        BattleAction action;
        action.set_action_type(rule.action());
        action.set_skill_table_id(rule.skill_id());
        std::vector<uint64_t> targetIds;
        switch (rule.target().side()) {
        case BATTLE_SCRIPT_TARGET_SELF:
            // 防御没有目标;对自己放技能统一填自己:老技能的单体校验要非 0 目标,职业自身技能收 0 或自己
            targetIds.push_back(rule.action() == BATTLE_ACTION_DEFEND ? 0 : actor.actor_id());
            break;
        case BATTLE_SCRIPT_TARGET_ENGINE_PICK:
            targetIds.push_back(0);
            break;
        case BATTLE_SCRIPT_TARGET_ENEMY:
        case BATTLE_SCRIPT_TARGET_ALLY:
            targetIds = CollectScriptTargets(actor, rule.target(), role.role_id(), claims);
            break;
        default:
            break;  // 规范化已拒绝;万一漏进来,这条规则不出手
        }
        // 按顺序试每个候选:与手动提交同一条校验链,手动做不到的剧本也做不到
        for (const auto targetId : targetIds) {
            action.set_target_id(targetId);
            if (CheckActionPrerequisites(actor, action) == kSuccess) {
                return action;
            }
        }
    }
    return std::nullopt;
}

bool TurnBattleEngine::ScriptConditionHolds(const BattleActorState& actor,
                                            const BattleScriptCondition& condition) const {
    bool holds = false;
    switch (condition.kind()) {
    case BATTLE_SCRIPT_COND_SELF_HP_BELOW:
        holds = RatioBelowPermyriad(actor.attributes().health(), actor.max_health(), condition.p1());
        break;
    case BATTLE_SCRIPT_COND_ROUND_AT_LEAST:
        holds = roundIndex >= condition.p1();
        break;
    case BATTLE_SCRIPT_COND_ROUND_EVERY:
        holds = condition.p1() != 0 && roundIndex >= condition.p2() &&
                (roundIndex - condition.p2()) % condition.p1() == 0;
        break;
    case BATTLE_SCRIPT_COND_ENEMY_COUNT_AT_LEAST:
        holds = CollectAliveEnemyIds(actor).size() >= condition.p1();
        break;
    default:
        // 规范化已拒绝未知种类;万一漏进来,不论是否取反都不成立(不出手比乱出手安全)
        return false;
    }
    return holds != condition.negate();
}

bool TurnBattleEngine::ScriptFilterHolds(const BattleActorState& candidate, const BattleScriptTargetFilter& filter,
                                         uint32_t ownRoleId, const ScriptClaims& claims) const {
    // 只读出手方本来就看得到的状态(气血、buff、单位种类、职业、速度 —— 都在下发给客户端的状态里);
    // 对面的冷却、道具余量、装备概率这类被 RedactStateForViewer 清掉的字段一律不读
    bool holds = false;
    switch (filter.kind()) {
    case BATTLE_SCRIPT_FILTER_HP_BELOW:
        holds = RatioBelowPermyriad(candidate.attributes().health(), candidate.max_health(), filter.p1());
        break;
    case BATTLE_SCRIPT_FILTER_HAS_BUFF_TYPE:
        holds = ActorHasBuffOfType(candidate, filter.p1());
        break;
    case BATTLE_SCRIPT_FILTER_HAS_BUFF:
        holds = std::any_of(candidate.buffs().begin(), candidate.buffs().end(),
                            [&filter](const BattleBuffEntry& buff) { return buff.buff_table_id() == filter.p1(); });
        break;
    case BATTLE_SCRIPT_FILTER_IS_MONSTER:
        holds = candidate.actor_type() == BATTLE_ACTOR_TYPE_MONSTER && candidate.monster_table_id() == filter.p1();
        break;
    case BATTLE_SCRIPT_FILTER_IS_CLASS:
        holds = candidate.actor_type() == BATTLE_ACTOR_TYPE_PLAYER && candidate.class_id() == filter.p1();
        break;
    case BATTLE_SCRIPT_FILTER_IS_ACTOR_TYPE:
        holds = static_cast<uint32_t>(candidate.actor_type()) == filter.p1();
        break;
    case BATTLE_SCRIPT_FILTER_CLAIMED_BY_ROLE: {
        const uint32_t roleId = filter.p1() == 0 ? ownRoleId : filter.p1();
        holds = std::any_of(claims.begin(), claims.end(), [roleId, &candidate](const auto& claim) {
            return claim.first == roleId && claim.second == candidate.actor_id();
        });
        break;
    }
    default:
        return false;  // 同 ScriptConditionHolds:未知种类不论是否取反都不成立
    }
    return holds != filter.negate();
}

std::vector<uint64_t> TurnBattleEngine::CollectScriptTargets(const BattleActorState& actor,
                                                             const BattleScriptTarget& target, uint32_t ownRoleId,
                                                             const ScriptClaims& claims) const {
    const bool wantAlly = target.side() == BATTLE_SCRIPT_TARGET_ALLY;
    std::vector<const BattleActorState*> candidates;
    for (const auto& other : actors) {
        if (!IsActorActive(other) || (other.team_index() == actor.team_index()) != wantAlly) {
            continue;
        }
        const bool passes = std::all_of(target.filters().begin(), target.filters().end(),
                                        [&](const BattleScriptTargetFilter& filter) {
                                            return ScriptFilterHolds(other, filter, ownRoleId, claims);
                                        });
        if (passes) {
            candidates.push_back(&other);
        }
    }

    // 全序:排序键 → 阵位 → actor_id(无符号;怪物 / 宝宝的局内号带 bit63)。比较全是整数,不消耗随机数;
    // 速度用吃了增减益的值,与出手序同口径
    const auto order = target.order();
    std::sort(candidates.begin(), candidates.end(),
              [this, order](const BattleActorState* lhs, const BattleActorState* rhs) {
                  switch (order) {
                  case BATTLE_SCRIPT_ORDER_HP_LOWEST:
                      if (lhs->attributes().health() != rhs->attributes().health()) {
                          return lhs->attributes().health() < rhs->attributes().health();
                      }
                      break;
                  case BATTLE_SCRIPT_ORDER_HP_HIGHEST:
                      if (lhs->attributes().health() != rhs->attributes().health()) {
                          return lhs->attributes().health() > rhs->attributes().health();
                      }
                      break;
                  case BATTLE_SCRIPT_ORDER_HP_RATIO_LOWEST:
                      if (RatioLess(lhs->attributes().health(), lhs->max_health(), rhs->attributes().health(),
                                    rhs->max_health())) {
                          return true;
                      }
                      if (RatioLess(rhs->attributes().health(), rhs->max_health(), lhs->attributes().health(),
                                    lhs->max_health())) {
                          return false;
                      }
                      break;
                  case BATTLE_SCRIPT_ORDER_SPEED_HIGHEST: {
                      const uint64_t lhsSpeed = EffectiveSpeed(*lhs);
                      const uint64_t rhsSpeed = EffectiveSpeed(*rhs);
                      if (lhsSpeed != rhsSpeed) {
                          return lhsSpeed > rhsSpeed;
                      }
                      break;
                  }
                  default:  // FORMATION
                      break;
                  }
                  if (lhs->formation_slot() != rhs->formation_slot()) {
                      return lhs->formation_slot() < rhs->formation_slot();
                  }
                  return lhs->actor_id() < rhs->actor_id();
              });

    std::vector<uint64_t> targetIds;
    targetIds.reserve(candidates.size());
    for (const auto* candidate : candidates) {
        targetIds.push_back(candidate->actor_id());
    }
    return targetIds;
}

std::vector<uint64_t> TurnBattleEngine::BuildTurnOrder() const {
    std::vector<const BattleActorState*> ordered;
    ordered.reserve(actors.size());
    for (const auto& actor : actors) {
        if (IsActorActive(actor)) {
            ordered.push_back(&actor);
        }
    }
    // 先手优先级(职业技能 Skill.initiative)排在速度之前;速度吃职业技能的增减益。
    // 两者都在回合开始时一次算定:本回合里新挂上的速度增减益从下一回合起生效。
    // 没有职业技能的对局里优先级全 0、速度原样,出手序与改动前逐位一致
    std::map<uint64_t, std::pair<int32_t, uint64_t>> sortKeys;
    for (const auto* actor : ordered) {
        sortKeys[actor->actor_id()] = {ActionInitiative(*actor), EffectiveSpeed(*actor)};
    }
    const bool rankedTieBreak = IsRankedBattle() || UsesFreePreset();
    const uint64_t battleSeed = createRequest.seed();
    const uint32_t currentRound = roundIndex;
    std::sort(ordered.begin(), ordered.end(),
              [&sortKeys, rankedTieBreak, battleSeed, currentRound](const BattleActorState* lhs, const BattleActorState* rhs) {
                  const auto& lhsKey = sortKeys.at(lhs->actor_id());
                  const auto& rhsKey = sortKeys.at(rhs->actor_id());
                  if (lhsKey.first != rhsKey.first) {
                      return lhsKey.first > rhsKey.first;
                  }
                  if (lhsKey.second != rhsKey.second) {
                      return lhsKey.second > rhsKey.second;
                  }
                  if (rankedTieBreak) {
                      const uint64_t lhsTie = RankedTurnTieKey(battleSeed, currentRound, lhs->actor_id());
                      const uint64_t rhsTie = RankedTurnTieKey(battleSeed, currentRound, rhs->actor_id());
                      if (lhsTie != rhsTie) {
                          return lhsTie < rhsTie;
                      }
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
    if (ExecutePreparedCastIfReady(actor, result)) {
        return;
    }
    if (ExecuteConfiguredControl(actor, action, result)) return;
    // 回合中途被眩晕/冰冻:行动作废(不发事件,客户端以 buff 状态解释)
    if (ActorHasBuffOfType(actor, kBuffTypeStun) || ActorHasBuffOfType(actor, kBuffTypeFreeze)) {
        return;
    }

    switch (action.action_type()) {
    case BATTLE_ACTION_ATTACK:
        if (IsPhysicalForbidden(actor)) {
            break;
        }
        ExecuteAttack(actor, action.target_id(), result);
        break;
    case BATTLE_ACTION_SKILL:
        ExecuteSkill(actor, action, result);
        break;
    case BATTLE_ACTION_DEFEND:
        ExecuteDefend(actor, result);
        EquipmentDefend(actor, result);
        break;
    case BATTLE_ACTION_ITEM:
        ExecuteItem(actor, action, result);
        break;
    case BATTLE_ACTION_CAPTURE:
        ExecuteCapture(actor, action, result);
        break;
    case BATTLE_ACTION_RECALL_PET: {
        if (CheckActionPrerequisites(actor, action) != kSuccess) break;
        auto* pet = FindActor(action.target_id());
        pet->set_fled(false); pet->set_withdraw_until_round(0);
        auto* returned = AppendEvent(result, BATTLE_EVENT_PET_RETREAT, actor.actor_id(), pet->actor_id());
        returned->set_success(true); returned->set_value(0);
        returned->set_target_health_after(pet->attributes().health());
        // 本轮出手表已冻结,召回不插入行动,不重置冷却、被动或每战次数。
        break;
    }
    case BATTLE_ACTION_SUMMON_PET:
        ExecuteSummonPet(actor, action.summon_pet_id(), result);
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
    const bool eligibleEquipmentPrimary = targetId == 0 ||
        (target != nullptr && IsActorActive(*target) && target->team_index() != actor.team_index());
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

    // 首段。没打中就谈不上"被普攻打中":连击(D3)、反击(D4)的前提都不成立,本次行动到此为止
    // (一期命中率 100%,永不进入此分支)
    if (!ResolveBasicAttackHit(actor, *target, BATTLE_HIT_NORMAL, result, eligibleEquipmentPrimary ? targetId : 0)) {
        return;
    }

    // 连击(D3):每段结算完(含该段的反震)再判下一段。双方都还在场才掷骰 ——
    // 目标被打死、或出手者被反震弹死,都直接收手,不白白消耗随机数。
    // 追加段与首段同 group_id、hit_index 递增:客户端把它们并在同一拍里连着播
    for (uint32_t extraHit = 1; extraHit <= kMaxComboExtraHits; ++extraHit) {
        if (!IsActorActive(actor) || !IsActorActive(*target)) {
            break;
        }
        if (!RollPercent(actor.combat().combo_rate())) {
            break;
        }
        currentHitIndex = extraHit;
        ResolveBasicAttackHit(actor, *target, BATTLE_HIT_COMBO, result);
    }
    currentHitIndex = 0;

    // 单次物理攻击标记（S01 截锋令 / S09 授锋战印）覆盖整次普攻行动（含连击），行动末一次性消耗。
    if (IsActorActive(actor)) {
        ConsumeOneShotAttackModifiers(actor, combatdamage::kPhysicalDamage, result);
    }

    // 反击(D4):整次普攻(含连击追加段)结束后才判,一次普攻至多引发一次反击
    TryCounterAttack(actor, *target, result);
}

bool TurnBattleEngine::ResolveBasicAttackHit(BattleActorState& attacker, BattleActorState& target,
                                             eBattleHitKind hitKind, TurnResultS2C& result, uint64_t primaryTargetId) {
    // hit_kind 为 NORMAL(= 0,proto3 缺省值不上线)时,本段产出的事件字节与装备系统落地前逐位相同

    // 命中判定(D1):未命中只出 MISS,不出 DAMAGE(一期命中率 100%,永不进入此分支)
    if (!RollHit(attacker, target)) {
        auto* missEvent =
            AppendEvent(result, BATTLE_EVENT_MISS, attacker.actor_id(), target.actor_id());
        missEvent->set_value(0);
        missEvent->set_target_health_after(target.attributes().health());
        missEvent->set_target_mana_after(target.attributes().mana());
        missEvent->set_hit_kind(hitKind);
        return false;
    }

    bool isCritical = false;
    // 普攻是物理攻击:吃物伤、倍率 1、物理必杀率与目标的抗物理;技能按表选物伤 / 法伤(见 ApplySkillToTarget)
    double finalDamage = CalculateFinalDamage(attacker, target, kBasicAttackBaseDamage,
                                              EffectivePhysicalAttack(attacker), 1.0,
                                              combatdamage::kPhysicalDamage, isCritical);
    if (hitKind == BATTLE_HIT_COMBO) {
        // 连击追加段:独立结算(上面已经独立掷过必杀)后再打折。只有这一种段做缩放 ——
        // 首段与反击若也写成「× 100 / 100」,浮点舍入可能让伤害差出最后一位,平移既有回放基线。
        // 普攻基础伤害恒 > 0,打折后仍 > 0,ApplyDamage 的向上取整天然给出「至少 1、不超过目标当前气血」
        finalDamage *= static_cast<double>(kComboDamagePercent) / 100.0;
    }
    // 只把普通行动的首段交给下一次普攻监听。连击、反击不会消费本行动刚生成的触发 Buff。
    if (hitKind == BATTLE_HIT_NORMAL) {
        finalDamage = TriggerBeforeBasicAttackBuffs(attacker, target, finalDamage, result);
    }
    if (HasConfiguredBuffs(attacker) || HasConfiguredBuffs(target) ||
        attacker.has_equipment_effects() || target.has_equipment_effects() || attacker.has_pet_cultivation()) {
        EffectContext context;
        context.critical = isCritical;
        context.origin = hitKind == BATTLE_HIT_COUNTER ? BattleDamageOrigin::Mirror : BattleDamageOrigin::Direct;
        context.primaryTargetId = primaryTargetId;
        context.hitOrdinal = hitKind == BATTLE_HIT_COMBO ? currentHitIndex : 0;
        context.actualDamage = ResolveConfiguredDamage(attacker, target, finalDamage, combatdamage::kPhysicalDamage,
            context, result, hitKind);
        if (context.actualDamage > 0 && hitKind == BATTLE_HIT_NORMAL && IsActorActive(target) &&
            IsBossTrialKind(bossTrialKind) && attacker.actor_type() == BATTLE_ACTOR_TYPE_MONSTER &&
            attacker.team_index() == 1 && attacker.monster_table_id() == kBossTrialMonsterSupportBreaker) {
            sunderUntilRound[target.actor_id()] = roundIndex + 2u;
            EmitBossMechanicEvent(result, kBossMechanicBreakerSunder, attacker.actor_id(), target.actor_id(), 20ULL);
        }
        if (context.actualDamage > 0 && hitKind == BATTLE_HIT_NORMAL && IsActorActive(attacker))
            TriggerConfiguredBuffs(attacker, target, BattleTrigger::AfterBasic, combatdamage::kPhysicalDamage, context, result);
        if (hitKind != BATTLE_HIT_COUNTER) TryReflectDamage(attacker, target, context.actualDamage, result);
        return context.actualDamage > 0;
    }
    if (EscalationStacks() > 0) {
        finalDamage *= static_cast<double>(EscalationDamagePct()) / 100.0;
    }
    const uint64_t shieldCapBefore = CurrentTotalShieldOf(target);
    const bool hadShieldBefore = shieldCapBefore > 0u;
    uint64_t dealt = 0;
    if (HasActiveEncounterDamageModifiers()) {
        double scaled = finalDamage;
        if (ActorHasBuffOfType(target, kBuffTypeInvincibility)) scaled = 0.0;
        if (target.is_defending()) scaled *= 0.5;
        uint64_t remaining = combatdamage::DamageToHealth(scaled, UINT64_MAX);
        remaining = ApplyEncounterDamageModifiers(attacker, target, remaining, combatdamage::kPhysicalDamage,
                                                  hitKind != BATTLE_HIT_COUNTER, result,
                                                  /*isMultiTargetSkill=*/false, /*isPrimaryTarget=*/true, nullptr);
        dealt = std::min(target.attributes().health(), remaining);
        if (dealt > 0) target.mutable_attributes()->set_health(target.attributes().health() - dealt);
    } else {
        dealt = ApplyDamage(target, finalDamage);
    }
    const uint64_t shieldCapAfter = CurrentTotalShieldOf(target);
    const uint64_t shieldAbsorbed = shieldCapBefore > shieldCapAfter ? (shieldCapBefore - shieldCapAfter) : 0ULL;
    const bool shieldBroke = hadShieldBefore && !HasAnyActiveShield(target);
    if (hitKind != BATTLE_HIT_COUNTER) {
        RecordTrialDirectDamage(attacker, target, combatdamage::kPhysicalDamage, dealt);
    }
    if (dealt > 0) {
        RecordEffectiveContribution(attacker);
    }

    auto* damageEvent =
        AppendEvent(result, BATTLE_EVENT_DAMAGE, attacker.actor_id(), target.actor_id());
    damageEvent->set_value(dealt);
    damageEvent->set_is_critical(isCritical);
    damageEvent->set_target_health_after(target.attributes().health());
    damageEvent->set_target_mana_after(target.attributes().mana());
    damageEvent->set_hit_kind(hitKind);

    if (dealt > 0 && hitKind == BATTLE_HIT_NORMAL && target.attributes().health() > 0 &&
        IsBossTrialKind(bossTrialKind) && attacker.actor_type() == BATTLE_ACTOR_TYPE_MONSTER &&
        attacker.team_index() == 1 && attacker.monster_table_id() == kBossTrialMonsterSupportBreaker) {
        sunderUntilRound[target.actor_id()] = roundIndex + 2u;
        EmitBossMechanicEvent(result, kBossMechanicBreakerSunder, attacker.actor_id(), target.actor_id(), 20ULL);
    }

    if (target.attributes().health() == 0) {
        HandleDeath(target, attacker.actor_id(), result);
    }
    if ((dealt > 0 || shieldAbsorbed > 0) && hitKind != BATTLE_HIT_COUNTER) {
        ApplyWuxingPostDirectHitEffects(attacker, target, dealt, isCritical, result,
                                        shieldAbsorbed, shieldBroke, shieldCapBefore, 0u, false);
    }

    // 反震(D5)排在落伤害之后、连击续段判定之前。反击那一下不触发:
    // 反击不再引发连击 / 反击 / 反震,否则两个高反震 + 高反击的单位能在一次行动里来回弹
    if (hitKind != BATTLE_HIT_COUNTER) {
        TryReflectDamage(attacker, target, dealt, result);
    }
    return true;
}

double TurnBattleEngine::TriggerBeforeBasicAttackBuffs(BattleActorState& attacker,
                                                       BattleActorState& target, double rawDamage,
                                                       TurnResultS2C& result) {
    // 子效果会驱散父实例、改动同一容器或授予新的触发项。固定本次候选,避免迭代失效与自我连锁。
    std::vector<uint64_t> triggerIds;
    for (const auto& entry : attacker.buffs()) {
        const auto* buffRow = dataProvider->FindBuff(entry.buff_table_id());
        if (buffRow != nullptr && buffRow->buff_type() == kBuffTypeNextBasicAttack) {
            triggerIds.push_back(entry.buff_id());
        }
    }
    for (const auto buffId : triggerIds) {
        const int index = FindBuffIndex(attacker, buffId);
        if (index < 0) {
            continue;  // 先前触发的子效果已驱散本条,不能再用旧快照触发。
        }
        const auto entrySnapshot = attacker.buffs(index);
        const auto* buffRow = dataProvider->FindBuff(entrySnapshot.buff_table_id());
        if (buffRow == nullptr || buffRow->buff_type() != kBuffTypeNextBasicAttack) {
            continue;
        }
        const double bonusDamage = dataProvider->GetBuffBonusDamage(buffRow->id());
        if (std::isfinite(bonusDamage) && bonusDamage > 0.0) {
            // 附伤在公式和暴击之后加入,不再乘层数。饱和避免有限值相加溢成 inf 后被落血入口丢弃。
            const double maxDamage = std::numeric_limits<double>::max();
            rawDamage = bonusDamage > maxDamage - rawDamage ? maxDamage : rawDamage + bonusDamage;
        }

        // 已接受的这次触发用复制的来源上下文完成。父被自身子效果驱散也不改变受击对象或归属。
        for (const auto subBuffId : buffRow->sub_buff()) {
            AddBuffToActor(attacker, subBuffId, entrySnapshot.caster_id(), 1, result);
        }
        for (const auto subBuffId : buffRow->target_sub_buff()) {
            AddBuffToActor(target, subBuffId, entrySnapshot.caster_id(), 1, result);
        }
        const int remainingIndex = FindBuffIndex(attacker, buffId);
        if (remainingIndex >= 0) {
            RemoveBuffAt(attacker, remainingIndex, result);
        }
    }
    return rawDamage;
}

void TurnBattleEngine::TryReflectDamage(BattleActorState& attacker, BattleActorState& target,
                                        uint64_t dealt, TurnResultS2C& result) {
    // 先过完所有不耗随机数的条件,最后才掷骰:该段没扣到血、或受击者已被这一段打死,都不反震
    if (dealt == 0 || !IsActorActive(target) || !IsActorActive(attacker)) {
        return;
    }
    if (!RollPercent(target.combat().reflect_rate())) {
        return;
    }

    if (HasConfiguredBuffs(attacker) || HasConfiguredBuffs(target)) {
        EffectContext context; context.origin = BattleDamageOrigin::Reflect;
        ResolveConfiguredDamage(target, attacker, static_cast<double>(std::max<uint64_t>(1, PercentCeil(dealt, kReflectDamagePercent))),
            combatdamage::kPhysicalDamage, context, result, BATTLE_HIT_REFLECT);
        return;
    }

    // 弹回的是「已经结算完的实扣值」的一个比例:直接扣血,不再过减伤公式、不掷必杀、
    // 也不走 ApplyDamage(那里会按出手者的防御指令减半)
    const uint64_t healthBefore = attacker.attributes().health();
    uint64_t baseReflected = std::max<uint64_t>(1, PercentCeil(dealt, kReflectDamagePercent));
    if (target.team_index() == 0 && HasRealmBoon(wuxing::kBoonEarthDoubleReflect)) {
        baseReflected = SaturatingAdd(baseReflected, baseReflected);
        boonRuntime.armorBrokenUntilRound[attacker.actor_id()] = roundIndex + 2u;
    }
    const uint64_t reflected = std::min<uint64_t>(healthBefore, baseReflected);
    attacker.mutable_attributes()->set_health(healthBefore - reflected);

    auto* reflectEvent =
        AppendEvent(result, BATTLE_EVENT_DAMAGE, target.actor_id(), attacker.actor_id());
    reflectEvent->set_value(reflected);
    reflectEvent->set_hit_kind(BATTLE_HIT_REFLECT);
    reflectEvent->set_target_health_after(attacker.attributes().health());
    reflectEvent->set_target_mana_after(attacker.attributes().mana());

    if (attacker.attributes().health() == 0) {
        // 击杀归属受击者(怪物普攻被玩家弹死,照常记进玩家方的击杀簿)
        HandleDeath(attacker, target.actor_id(), result);
    }
}

void TurnBattleEngine::TryCounterAttack(BattleActorState& attacker, BattleActorState& defender,
                                        TurnResultS2C& result) {
    // 出手者可能刚被反震弹死,受击者可能已被打死:任何一方不在场都没有反击
    if (!IsActorActive(attacker) || !IsActorActive(defender)) {
        return;
    }
    // 「仍能行动」复用行动校验链的状态判定:眩晕 / 冰冻中的单位连自己的回合都出不了手,自然不能还手；
    // 灵法专修（ForbidPhysical）禁用物理反击，避免绕过禁物代价。
    if (CheckState(defender) != kSuccess || IsPhysicalForbidden(defender)) {
        return;
    }
    if (!RollPercent(defender.combat().counter_rate())) {
        return;
    }

    // 反击是受击者的另一次出手:另起一个事件组(group_id + 1、hit_index 归 0),
    // 客户端据此把它排成独立的一拍,而不是并进出手者那一拍的伤害里
    BeginEventGroup();
    auto* attackEvent =
        AppendEvent(result, BATTLE_EVENT_ATTACK, defender.actor_id(), attacker.actor_id());
    attackEvent->set_hit_kind(BATTLE_HIT_COUNTER);
    // 按普攻公式结算(可必杀);COUNTER 段不触发反震,这里也不再判连击与反击
    ResolveBasicAttackHit(defender, attacker, BATTLE_HIT_COUNTER, result);
}

void TurnBattleEngine::ExecuteSkill(BattleActorState& actor, const BattleAction& action,
                                    TurnResultS2C& result) {
    if (const auto* row = dataProvider->FindSkill(action.skill_table_id()); row != nullptr && row->battle_model() == 1) {
        ExecuteConfiguredSkill(actor, action, *row, result);
        return;
    }
    // 职业技能走自己的目标 / 冷却 / 耗蓝 / 效果规则(class-skills.md §6)
    if (const auto* classRow = dataProvider->FindSkill(action.skill_table_id());
        classRow != nullptr && IsClassSkill(*classRow)) {
        ExecuteClassSkill(actor, action, *classRow, result);
        return;
    }

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
    // 表达式只求值一次,多目标共用同一 base。
    // 所有技能上升(equipment-attributes.md §4.5):只抬高这一处的等级参数,相当于技能按更高的等级算伤害;
    // 角色等级本身不变 —— 目标的等级系数、buff 回血等其它读 level() 的地方都不吃这条加成。
    // 加成为 0 时参数就是 level() 本身,求值结果与改动前逐位一致
    const double baseDamage = dataProvider->GetSkillDamage(
        action.skill_table_id(),
        static_cast<double>(SaturatingAdd(actor.level(), actor.combat().skill_level_bonus())));

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
        const uint64_t attack = combatdamage::SelectAttack(
            skillRow.damage_type(), EffectivePhysicalAttack(actor), actor.magic_attack());
        // 伤害类型同时决定吃物理还是法术的必杀率 / 抗性(与上面选攻击用的是同一列)
        const double finalDamage =
            CalculateFinalDamage(actor, target, baseDamage, attack, skillRow.attack_multiplier(),
                                 skillRow.damage_type(), isCritical);
        if (HasConfiguredBuffs(actor) || HasConfiguredBuffs(target) || actor.has_equipment_effects() || target.has_equipment_effects() || actor.has_pet_cultivation()) {
            EffectContext context; context.skillId = action.skill_table_id(); context.critical = isCritical;
            context.primaryTargetId = action.target_id() != 0 ? action.target_id() : currentHitIndex == 0 ? target.actor_id() : 0;
            context.isMultiTargetSkill = IsAreaSkill(skillRow);
            ResolveConfiguredDamage(actor, target, finalDamage, skillRow.damage_type(), context, result);
        } else {
        const uint64_t shieldCapBefore = CurrentTotalShieldOf(target);
        const bool hadShieldBefore = shieldCapBefore > 0u;
        uint64_t dealt = 0;
        bool splashReduced = false;
        if (HasActiveEncounterDamageModifiers()) {
            double scaled = finalDamage;
            if (ActorHasBuffOfType(target, kBuffTypeInvincibility)) scaled = 0.0;
            if (target.is_defending()) scaled *= 0.5;
            uint64_t remaining = combatdamage::DamageToHealth(scaled, UINT64_MAX);
            const uint64_t chosenPrimary =
                action.target_id() != 0 ? action.target_id() : (currentHitIndex == 0 ? target.actor_id() : 0);
            const bool isPrimary = chosenPrimary == 0 || target.actor_id() == chosenPrimary;
            remaining = ApplyEncounterDamageModifiers(actor, target, remaining, skillRow.damage_type(), true, result,
                                                      IsAreaSkill(skillRow), isPrimary, &splashReduced);
            dealt = std::min(target.attributes().health(), remaining);
            if (dealt > 0) target.mutable_attributes()->set_health(target.attributes().health() - dealt);
        } else {
            dealt = ApplyDamage(target, finalDamage);
        }
        const uint64_t shieldCapAfter = CurrentTotalShieldOf(target);
        const uint64_t shieldAbsorbed = shieldCapBefore > shieldCapAfter ? (shieldCapBefore - shieldCapAfter) : 0ULL;
        const bool shieldBroke = hadShieldBefore && !HasAnyActiveShield(target);
        RecordTrialDirectDamage(actor, target, skillRow.damage_type(), dealt);
        if (dealt > 0) {
            RecordEffectiveContribution(actor);
        }

        auto* damageEvent =
            AppendEvent(result, BATTLE_EVENT_DAMAGE, actor.actor_id(), target.actor_id());
        damageEvent->set_skill_table_id(action.skill_table_id());
        damageEvent->set_value(dealt);
        damageEvent->set_is_critical(isCritical);
        if (splashReduced) damageEvent->set_is_splash_reduced(true);
        damageEvent->set_target_health_after(target.attributes().health());
        damageEvent->set_target_mana_after(target.attributes().mana());

        if (target.attributes().health() == 0) {
            HandleDeath(target, actor.actor_id(), result);
        }
        if (dealt > 0 || shieldAbsorbed > 0) {
            ApplyWuxingPostDirectHitEffects(actor, target, dealt, isCritical, result,
                                            shieldAbsorbed, shieldBroke, shieldCapBefore,
                                            action.skill_table_id(), IsAreaSkill(skillRow));
        }
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

uint64_t TurnBattleEngine::SkillManaCost(const BattleActorState& actor, const SkillTable& skillRow) const {
    uint64_t cost = 0;
    if (skillRow.battle_model() == 1) {
        const uint64_t original = classskill::ManaCost(actor.max_mana(), skillRow.mana_percent());
        const uint32_t category = EquipmentManaCategory(skillRow);
        const auto& reductions = actor.equipment_effects().mana_reduction_bps();
        const auto found = reductions.find(category);
        const double equipment = actor.actor_type() == BATTLE_ACTOR_TYPE_PLAYER && found != reductions.end() ? found->second : 0;
        cost = equipbattle::DiscountMana(original, equipment - 10000 * ConfiguredModifier(actor, BattleModifier::ManaCost));
    } else if (IsClassSkill(skillRow)) {
        // 职业技能按最大法力的比例(class-skills.md §6.3);多目标也只在出手时扣这一次
        cost = classskill::ManaCost(actor.max_mana(), skillRow.mana_percent());
    } else {
        cost = LegacySkillManaCost(skillRow);
    }
    if (bossWeeklyAffixId == bossaffix::kAffixWater && actor.team_index() == 0 &&
        IsWeeklyWaterDiscountSkill(skillRow)) {
        cost = bossaffix::DiscountWaterManaCost(cost);
    }
    if (const uint32_t discountPct = DefendManaDiscountResonancePct(actor, skillRow.id()); discountPct > 0u) {
        cost = seasonrules::ComputeManaDiscounted(cost, discountPct);
    }
    if (actor.team_index() == 0 && HasRealmBoon(wuxing::kBoonWaterFreeFirstCast) &&
        !boonRuntime.freeManaUsedThisRound.contains(actor.actor_id())) {
        return 0ULL;
    }
    return cost;
}

uint64_t TurnBattleEngine::LegacySkillManaCost(const SkillTable& skillRow) {
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
    const uint64_t manaCost = SkillManaCost(actor, skillRow);
    if (actor.team_index() == 0 && HasRealmBoon(wuxing::kBoonWaterFreeFirstCast)) {
        boonRuntime.freeManaUsedThisRound.insert(actor.actor_id());
    }
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
    manaEvent->set_mana_delta(-static_cast<int64_t>(manaBefore - manaAfter));
    manaEvent->set_target_health_after(actor.attributes().health());
    manaEvent->set_target_mana_after(manaAfter);
}

// ---------------------------------------------------------------------------
// 四职业技能(docs/design/class-skills.md §6)
// ---------------------------------------------------------------------------

uint32_t TurnBattleEngine::ClassSkillRank(const BattleActorState& actor, const SkillTable& skillRow) const {
    if (!IsClassSkill(skillRow) || skillRow.class_id() != actor.class_id()) {
        return 0;
    }
    const auto it = actor.skill_ranks().find(skillRow.id());
    return it == actor.skill_ranks().end() ? 0 : it->second;
}

uint32_t TurnBattleEngine::ValidateClassSkillTarget(const BattleActorState& actor, const SkillTable& skillRow,
                                                    uint64_t targetId) const {
    switch (static_cast<classskill::TargetTeam>(skillRow.target_team())) {
    case classskill::TargetTeam::kSelf:
        return targetId == 0 || targetId == actor.actor_id() ? kSuccess : kSkillInvalidTarget;
    case classskill::TargetTeam::kEnemy:
    case classskill::TargetTeam::kAlly: {
        if (targetId == 0) {
            return kSuccess;  // 由引擎按规则选
        }
        const auto* target = FindActor(targetId);
        if (target == nullptr) {
            return kSkillInvalidTargetId;
        }
        const bool sameTeam = target->team_index() == actor.team_index();
        const bool wantAlly = skillRow.target_team() == static_cast<uint32_t>(classskill::TargetTeam::kAlly);
        // 阵营不对一律拒:友方技能不能拿敌人当目标,反过来也一样(阵亡 / 逃离的单位阵营不变,照样判)
        return sameTeam == wantAlly ? kSuccess : kSkillInvalidTarget;
    }
    case classskill::TargetTeam::kNone:
        break;
    }
    return kInvalidTableData;  // 表里 target_team 没填 / 填了认不出的值
}

std::vector<uint64_t> TurnBattleEngine::SelectClassSkillTargets(const BattleActorState& actor,
                                                                const SkillTable& skillRow,
                                                                uint64_t primaryTargetId) const {
    const auto team = static_cast<classskill::TargetTeam>(skillRow.target_team());
    if (team == classskill::TargetTeam::kSelf) {
        if (!IsActorActive(actor)) {
            return {};
        }
        return {actor.actor_id()};
    }
    if (team != classskill::TargetTeam::kEnemy && team != classskill::TargetTeam::kAlly) {
        return {};
    }

    const bool wantAlly = team == classskill::TargetTeam::kAlly;
    std::vector<const BattleActorState*> candidates;
    for (const auto& other : actors) {
        // 只选存活未逃的:治疗不复活,阵亡的友方不会被补进来
        if (!IsActorActive(other) || (other.team_index() == actor.team_index()) != wantAlly) {
            continue;
        }
        candidates.push_back(&other);
    }
    const bool healing = skillRow.heal_multiplier() > 0 || skillRow.heal_per_level() > 0;
    std::sort(candidates.begin(), candidates.end(),
              [primaryTargetId, healing](const BattleActorState* lhs, const BattleActorState* rhs) {
                  const bool lhsPrimary = primaryTargetId != 0 && lhs->actor_id() == primaryTargetId;
                  const bool rhsPrimary = primaryTargetId != 0 && rhs->actor_id() == primaryTargetId;
                  if (lhsPrimary != rhsPrimary) {
                      return lhsPrimary;
                  }
                  if (healing) {
                      const double lhsRatio = HealthRatio(*lhs);
                      const double rhsRatio = HealthRatio(*rhs);
                      if (lhsRatio != rhsRatio) {
                          return lhsRatio < rhsRatio;
                      }
                  }
                  if (lhs->formation_slot() != rhs->formation_slot()) {
                      return lhs->formation_slot() < rhs->formation_slot();
                  }
                  return lhs->actor_id() < rhs->actor_id();
              });

    const size_t limit = std::max<size_t>(1, skillRow.max_targets());
    std::vector<uint64_t> targetIds;
    for (const auto* candidate : candidates) {
        if (targetIds.size() >= limit) {
            break;
        }
        targetIds.push_back(candidate->actor_id());
    }
    return targetIds;
}

void TurnBattleEngine::ExecuteClassSkill(BattleActorState& actor, const BattleAction& action,
                                         const SkillTable& skillRow, TurnResultS2C& result) {
    // 出手前重跑校验链(提交后到出手前,冷却 / 沉默 / 法力都可能已变化)
    if (CheckActionPrerequisites(actor, action) != kSuccess) {
        // 敌方技能放不出来,按老规则降级成普攻;友方 / 自身技能不降级 —— 降级就是去打一个随机敌人,
        // 与玩家的本意相反(class-skills.md §6.2),这一手作废、不出事件
        if (skillRow.target_team() == static_cast<uint32_t>(classskill::TargetTeam::kEnemy)) {
            ExecuteAttack(actor, action.target_id(), result);
        }
        return;
    }

    // 选目标不消耗随机数:选中的合法主目标优先,主目标已倒下时按规则补位
    const auto targetIds = SelectClassSkillTargets(actor, skillRow, action.target_id());
    if (targetIds.empty()) {
        return;  // 一个合法目标都没有:不扣蓝、不进冷却、不出事件
    }
    const uint32_t rank = ClassSkillRank(actor, skillRow);

    // 冷却按回合:+1 是因为本回合末的 DecayCooldowns 会先扣掉一回合,
    // cooldown_rounds = 3 意味着接下来整整 3 个回合放不了
    if (skillRow.cooldown_rounds() > 0) {
        (*actor.mutable_skill_cooldown_rounds())[skillRow.id()] = skillRow.cooldown_rounds() + 1;
    }

    auto* skillEvent = AppendEvent(result, BATTLE_EVENT_SKILL, actor.actor_id(), targetIds.front());
    skillEvent->set_skill_table_id(skillRow.id());

    // 耗蓝:多目标也只扣这一次(校验链已保证蓝够)
    ConsumeSkillMana(actor, skillRow, skillRow.id(), result);

    // 伤害基数沿用老技能的表达式求值(职业技能表里是常数 10),多目标共用
    double baseDamage = 0.0;
    if (skillRow.target_team() == static_cast<uint32_t>(classskill::TargetTeam::kEnemy)) {
        baseDamage = ClassSkillBaseDamage(actor, skillRow);
    }

    for (size_t hitIndex = 0; hitIndex < targetIds.size(); ++hitIndex) {
        auto* target = FindActor(targetIds[hitIndex]);
        if (target == nullptr || !IsActorActive(*target)) {
            continue;
        }
        currentHitIndex = static_cast<uint32_t>(hitIndex);
        ApplyClassSkillToTarget(actor, skillRow, rank, baseDamage, *target, result,
            action.target_id() != 0 ? action.target_id() : targetIds.front());
    }
    currentHitIndex = 0;
}

double TurnBattleEngine::ClassSkillBaseDamage(const BattleActorState& actor, const SkillTable& skillRow) const {
    return dataProvider->GetSkillDamage(
        skillRow.id(), static_cast<double>(SaturatingAdd(actor.level(), actor.combat().skill_level_bonus())));
}

double TurnBattleEngine::ClassSkillHealAmount(const BattleActorState& actor, const SkillTable& skillRow, uint32_t rank,
                                              const BattleActorState& target) const {
    double amount = (static_cast<double>(actor.magic_attack()) * skillRow.heal_multiplier() +
                     static_cast<double>(actor.level()) * skillRow.heal_per_level()) *
                    classskill::DamageRankFactor(rank) * (1.0 + OutgoingHealBonus(actor));
    if (skillRow.execute_heal_threshold() > 0 && HealthRatio(target) < skillRow.execute_heal_threshold()) {
        amount *= 1.0 + skillRow.execute_heal_bonus();
    }
    return amount;
}

void TurnBattleEngine::ApplyClassSkillToTarget(BattleActorState& actor, const SkillTable& skillRow, uint32_t rank,
                                               double baseDamage, BattleActorState& target,
                                               TurnResultS2C& result, uint64_t primaryTargetId) {
    const bool hostile = skillRow.target_team() == static_cast<uint32_t>(classskill::TargetTeam::kEnemy);
    if (hostile) {
        // 命中判定只对敌方技能做(一期命中率 100%,不消耗随机数)
        if (!RollHit(actor, target)) {
            auto* missEvent = AppendEvent(result, BATTLE_EVENT_MISS, actor.actor_id(), target.actor_id());
            missEvent->set_skill_table_id(skillRow.id());
            missEvent->set_value(0);
            missEvent->set_target_health_after(target.attributes().health());
            missEvent->set_target_mana_after(target.attributes().mana());
            return;
        }
        if (baseDamage > 0) {
            bool isCritical = false;
            const uint64_t attack = combatdamage::SelectAttack(skillRow.damage_type(), EffectivePhysicalAttack(actor),
                                                               actor.magic_attack());
            // 等级系数乘在最终伤害上(与暴击倍率同为线性,先后无关)
            const double finalDamage =
                CalculateFinalDamage(actor, target, baseDamage, attack, skillRow.attack_multiplier(),
                                     skillRow.damage_type(), isCritical) *
                classskill::DamageRankFactor(rank);
            const bool isMultiTarget =
                (actor.actor_type() == BATTLE_ACTOR_TYPE_PET ? skillRow.pet_max_targets() : skillRow.max_targets()) > 1u;
            if (HasConfiguredBuffs(actor) || HasConfiguredBuffs(target) || actor.has_equipment_effects() || target.has_equipment_effects() || actor.has_pet_cultivation()) {
                EffectContext context; context.skillId = skillRow.id(); context.rank = rank; context.critical = isCritical;
                context.primaryTargetId = primaryTargetId;
                context.isMultiTargetSkill = isMultiTarget;
                ResolveConfiguredDamage(actor, target, finalDamage, skillRow.damage_type(), context, result);
            } else {
            const uint64_t shieldCapBefore = CurrentTotalShieldOf(target);
            const bool hadShieldBefore = shieldCapBefore > 0u;
            uint64_t dealt = 0;
            bool splashReduced = false;
            if (HasActiveEncounterDamageModifiers()) {
                double scaled = finalDamage;
                if (ActorHasBuffOfType(target, kBuffTypeInvincibility)) scaled = 0.0;
                if (target.is_defending()) scaled *= 0.5;
                uint64_t remaining = combatdamage::DamageToHealth(scaled, UINT64_MAX);
                const bool isPrimary = primaryTargetId == 0 || target.actor_id() == primaryTargetId;
                remaining = ApplyEncounterDamageModifiers(actor, target, remaining, skillRow.damage_type(), true, result,
                                                          isMultiTarget, isPrimary, &splashReduced);
                dealt = std::min(target.attributes().health(), remaining);
                if (dealt > 0) target.mutable_attributes()->set_health(target.attributes().health() - dealt);
            } else {
                dealt = ApplyDamage(target, finalDamage);
            }
            const uint64_t shieldCapAfter = CurrentTotalShieldOf(target);
            const uint64_t shieldAbsorbed = shieldCapBefore > shieldCapAfter ? (shieldCapBefore - shieldCapAfter) : 0ULL;
            const bool shieldBroke = hadShieldBefore && !HasAnyActiveShield(target);
            RecordTrialDirectDamage(actor, target, skillRow.damage_type(), dealt);
            if (dealt > 0) {
                RecordEffectiveContribution(actor);
            }

            auto* damageEvent = AppendEvent(result, BATTLE_EVENT_DAMAGE, actor.actor_id(), target.actor_id());
            damageEvent->set_skill_table_id(skillRow.id());
            damageEvent->set_value(dealt);
            damageEvent->set_is_critical(isCritical);
            if (splashReduced) damageEvent->set_is_splash_reduced(true);
            damageEvent->set_target_health_after(target.attributes().health());
            damageEvent->set_target_mana_after(target.attributes().mana());
            if (target.attributes().health() == 0) {
                HandleDeath(target, actor.actor_id(), result);
            }
            if (dealt > 0 || shieldAbsorbed > 0) {
                ApplyWuxingPostDirectHitEffects(actor, target, dealt, isCritical, result,
                                                shieldAbsorbed, shieldBroke, shieldCapBefore,
                                                skillRow.id(), isMultiTarget);
            }
            }
        }
    }
    if (target.is_dead()) {
        return;  // 死了就不再挂任何效果
    }

    // 治疗 = (法伤 × 倍率 + 施法者等级 × 每级加值) × 等级系数 × (1 + 治疗加成被动),目标残血时再加成
    if (skillRow.heal_multiplier() > 0 || skillRow.heal_per_level() > 0) {
        if (HasConfiguredBuffs(actor) || HasConfiguredBuffs(target)) {
            EffectContext context; context.skillId = skillRow.id(); context.rank = rank;
            ResolveConfiguredHeal(actor, target, ClassSkillHealAmount(actor, skillRow, rank, target), context, result);
        } else {
        double rawHeal = ClassSkillHealAmount(actor, skillRow, rank, target);
        if (const uint32_t wuxingHealPct = WuxingHealShieldBonusPct(actor); wuxingHealPct > 0) {
            rawHeal *= (100.0 + wuxingHealPct) / 100.0;
        }
        const uint64_t healed = ApplyHeal(target, rawHeal);
        RecordTrialMonsterHeal(actor, target, healed);
        if (healed > 0) {
            RecordEffectiveContribution(actor);
        }
        // 满血也发一条(value = 0):演出要知道这一下奶给了谁
        auto* healEvent = AppendEvent(result, BATTLE_EVENT_HEAL, actor.actor_id(), target.actor_id());
        healEvent->set_skill_table_id(skillRow.id());
        healEvent->set_value(healed);
        healEvent->set_target_health_after(target.attributes().health());
        healEvent->set_target_mana_after(target.attributes().mana());
        }
    }

    // 回蓝:按目标最大法力的比例(× 辅助等级系数),按缺口封顶,事件只报实际回复量
    if (skillRow.restore_mana_percent() > 0) {
        const uint64_t manaBefore = target.attributes().mana();
        const uint64_t restored = ClassSkillManaRestored(skillRow, rank, target, manaBefore);
        target.mutable_attributes()->set_mana(manaBefore + restored);
        auto* manaEvent = AppendEvent(result, BATTLE_EVENT_MANA, actor.actor_id(), target.actor_id());
        manaEvent->set_skill_table_id(skillRow.id());
        manaEvent->set_value(restored);
        manaEvent->set_mana_delta(static_cast<int64_t>(restored));
        manaEvent->set_target_health_after(target.attributes().health());
        manaEvent->set_target_mana_after(target.attributes().mana());
    }

    if (skillRow.dispel_count() > 0) {
        bool clearedWeeklyDispel = false;
        uint32_t dispelledTotal = 0;
        if (actor.team_index() != target.team_index()) {
            TryDispelBossStance(actor, target, result);
        } else if (sunderUntilRound.erase(target.actor_id()) > 0) {
            RecordEffectiveContribution(actor);
            clearedWeeklyDispel = true;
            ++dispelledTotal;
        }
        const uint32_t clearedDebuffs = DispelDebuffs(target, skillRow.dispel_count(), result);
        if (clearedDebuffs > 0) {
            RecordEffectiveContribution(actor);
            clearedWeeklyDispel = true;
            dispelledTotal += clearedDebuffs;
            if (actor.actor_type() == BATTLE_ACTOR_TYPE_PLAYER && actor.team_index() == target.team_index()) {
                antiControlOrSeasonSkillPlayers.insert(actor.actor_id());
            }
        }
        if (dispelledTotal > 0) {
            RecordDispelLanded(actor.actor_id(), dispelledTotal);
        }
        if (clearedWeeklyDispel) {
            OnBossWeeklyAffixDispelTriggered(actor.actor_id(), result);
        }
    }

    // 增减益:幅度 = 表里的基础幅度 × 辅助等级系数,施加时算定后存进局内 buff
    const double statPercent = skillRow.buff_percent() * classskill::SupportRankFactor(rank);
    for (const auto effectBuffId : skillRow.effect()) {
        AddBuffToActor(target, effectBuffId, actor.actor_id(), 0, result, statPercent);
    }
}

int32_t TurnBattleEngine::ActionInitiative(const BattleActorState& actor) const {
    const auto it = pendingActions.find(actor.actor_id());
    if (it == pendingActions.end() || it->second.action_type() != BATTLE_ACTION_SKILL) {
        return 0;
    }
    const auto* skillRow = dataProvider->FindSkill(it->second.skill_table_id());
    if (skillRow == nullptr || (!IsClassSkill(*skillRow) && skillRow->battle_model() != 1) || skillRow->initiative() == 0) {
        return 0;
    }
    // 只有这一手真能放出去才抢先手:放不出来(法力不够 / 冷却 / 沉默)的不能借优先级插队
    return CheckActionPrerequisites(actor, it->second) == kSuccess ? skillRow->initiative() : 0;
}

double TurnBattleEngine::OutgoingHealBonus(const BattleActorState& actor, SupportEffectBreakdown* sources) const {
    // protobuf map 遍历序不固定;浮点求和按 skill_table_id 升序做,结果才逐位稳定
    const std::map<uint32_t, uint32_t> ranks(actor.skill_ranks().begin(), actor.skill_ranks().end());
    double bonus = 0.0;
    for (const auto& [skillTableId, rank] : ranks) {
        const auto* skillRow = dataProvider->FindSkill(skillTableId);
        if (skillRow == nullptr ||
            classskill::ParsePassiveStat(skillRow->passive_stat()) != classskill::PassiveStat::kOutgoingHeal) {
            continue;
        }
        const double value = skillRow->passive_percent() * classskill::PassiveRankFactor(rank);
        bonus += value;
        if (sources != nullptr) {
            auto* source = sources->add_sources(); source->set_kind(2); source->set_skill_table_id(skillTableId);
            source->set_effect_kind(1);
            source->set_name(skillRow->name()); source->set_infinite(true);
            source->set_raw_bonus_bps(value * 10000); source->set_effective_bonus_bps(value * 10000);
        }
    }
    return bonus;
}

double TurnBattleEngine::ClassBuffStatMultiplier(const BattleActorState& actor, ClassBuffStat stat,
                                                 double extraPercent) const {
    double strongestGain = std::max(0.0, extraPercent);
    double strongestLoss = std::min(0.0, extraPercent);
    bool found = extraPercent != 0.0;
    for (const auto& entry : actor.buffs()) {
        if (entry.stat_percent() == 0.0) {
            continue;
        }
        const auto* buffRow = dataProvider->FindBuff(entry.buff_table_id());
        if (buffRow == nullptr || ClassSkillStatOfBuffType(buffRow->buff_type()) != stat) {
            continue;
        }
        found = true;
        // 同方向同属性只取最强的一个,不累加
        strongestGain = std::max(strongestGain, entry.stat_percent());
        strongestLoss = std::min(strongestLoss, entry.stat_percent());
    }
    if (!found) {
        return 1.0;
    }
    return std::max(kMinClassBuffStatMultiplier, 1.0 + strongestGain + strongestLoss);
}

uint64_t TurnBattleEngine::EffectivePhysicalAttack(const BattleActorState& actor) const {
    double multiplier = ClassBuffStatMultiplier(actor, ClassBuffStat::kPhysicalAttack) *
        std::max(0.1, 1 + ConfiguredModifier(actor, BattleModifier::PhysicalAttack));
    if (actor.is_boss() && bossDisheartenedUntilRound >= roundIndex) {
        multiplier *= 0.85;
    }
    return multiplier == 1.0 ? actor.physical_attack() : classskill::FloorRatio(actor.physical_attack(), multiplier);
}

uint64_t TurnBattleEngine::EffectiveDefense(const BattleActorState& actor) const {
    double multiplier = ClassBuffStatMultiplier(actor, ClassBuffStat::kDefense) *
        std::max(0.1, 1 + ConfiguredModifier(actor, BattleModifier::Defense) +
            ConfiguredModifier(actor, BattleModifier::SelfGrowingDefense));
    if (const auto it = sunderUntilRound.find(actor.actor_id());
        it != sunderUntilRound.end() && it->second >= roundIndex) {
        multiplier *= 0.80;
    }
    return multiplier == 1.0 ? actor.defense() : classskill::FloorRatio(actor.defense(), multiplier);
}

uint64_t TurnBattleEngine::EffectiveSpeed(const BattleActorState& actor) const {
    double multiplier = ClassBuffStatMultiplier(actor, ClassBuffStat::kSpeed) *
        std::max(0.1, 1 + ConfiguredModifier(actor, BattleModifier::Speed));
    if (const uint32_t formationSpeedPct = wuxing::FormationOf(TeamFormationId(actor.team_index())).speedBonusPct;
        formationSpeedPct > 0) {
        multiplier *= (100.0 + formationSpeedPct) / 100.0;
    }
    return multiplier == 1.0 ? actor.attributes().speed() : classskill::FloorRatio(actor.attributes().speed(), multiplier);
}

bool TurnBattleEngine::IsDebuffEntry(const BattleBuffEntry& entry) const {
    const auto* buffRow = dataProvider->FindBuff(entry.buff_table_id());
    if (buffRow == nullptr) {
        return entry.stat_percent() < 0.0;
    }
    if (buffRow->battle_op() != 0) {
        if (buffRow->tag().contains(std::string(kDebuffTag))) return true;
        if (buffRow->tag().contains("Buff")) return false;
        const auto op = static_cast<BattleEffectOp>(buffRow->battle_op());
        if (op == BattleEffectOp::Control || op == BattleEffectOp::PeriodicDamage) return true;
        if (op == BattleEffectOp::Modifier || op == BattleEffectOp::GrowingAura) {
            const auto stat = static_cast<BattleModifier>(buffRow->modifier_stat());
            // 降低耗蓝或承伤是增益,不能把所有负比例都当作减益。
            return stat == BattleModifier::ManaCost || stat == BattleModifier::IncomingDamage
                ? buffRow->ratio() > 0 : buffRow->ratio() < 0;
        }
        return false;
    }
    if (entry.stat_percent() < 0.0) return true;
    if (buffRow->tag().contains(std::string(kDebuffTag))) {
        return true;
    }
    switch (buffRow->buff_type()) {
    case kBuffTypePoison:
    case kBuffTypeBurn:
    case kBuffTypeStun:
    case kBuffTypeSilence:
    case kBuffTypeFreeze:
        return true;
    default:
        return false;
    }
}

bool TurnBattleEngine::IsConfiguredListenerBuff(const BattleBuffEntry& entry) const {
    const auto* skill = dataProvider->FindSkill(entry.source_skill_id());
    return skill != nullptr && skill->battle_model() == 1 &&
        (skill->ability_kind() == 2 || skill->ability_kind() == 3) &&
        std::find(skill->init_effect().begin(), skill->init_effect().end(), entry.buff_table_id()) !=
            skill->init_effect().end();
}

uint32_t TurnBattleEngine::DispelDebuffs(BattleActorState& target, uint32_t count, TurnResultS2C& result) {
    uint32_t removed = 0;
    // 按身上的先后顺序驱散(先挂上的先走),顺序确定、不消耗随机数
    for (int index = 0; index < target.buffs_size() && removed < count;) {
        if (!IsConfiguredListenerBuff(target.buffs(index)) && IsDebuffEntry(target.buffs(index))) {
            RemoveBuffAt(target, index, result);
            ++removed;
        } else {
            ++index;
        }
    }
    return removed;
}

// ---------------------------------------------------------------------------
// 职业玩家挂机选技(docs/design/class-skills.md §6.5)
// ---------------------------------------------------------------------------

std::optional<BattleAction> TurnBattleEngine::SelectAutoClassSkill(const BattleActorState& actor) const {
    const double basicAttackDamage = AutoBasicAttackDamage(actor);
    std::optional<AutoSkillCandidate> best;
    for (const auto skillId : actor.skill_table_ids()) {
        if (IsSkillBanned(skillId)) {
            continue;
        }
        const auto* skillRow = dataProvider->FindSkill(skillId);
        // 只挑职业技能:旧通用技能不进挂机候选,默认行动不变
        if (skillRow == nullptr || !IsClassSkill(*skillRow)) {
            continue;
        }
        // 目标 0 = 出手时由引擎选,这一步验的是等级、阵营配置、冷却、沉默与法力;
        // 下面枚举的都是对应阵营的存活单位,不会再被目标校验拒绝
        BattleAction probe;
        probe.set_action_type(BATTLE_ACTION_SKILL);
        probe.set_skill_table_id(skillId);
        if (CheckActionPrerequisites(actor, probe) != kSuccess) {
            continue;
        }

        const auto consider = [&](AutoSkillPurpose purpose, uint64_t gain, uint64_t targetId) {
            if (gain == 0) {
                return;  // 没有实际收益的不算候选
            }
            const AutoSkillCandidate candidate{purpose, gain, SkillManaCost(actor, *skillRow),
                                               skillRow->cooldown_rounds(), skillId, targetId};
            if (!best.has_value() || IsBetterAutoSkill(candidate, *best)) {
                best = candidate;
            }
        };

        const uint32_t rank = ClassSkillRank(actor, *skillRow);
        const bool hostile = skillRow->target_team() == static_cast<uint32_t>(classskill::TargetTeam::kEnemy);
        if (!hostile) {
            // 治疗用目标 0:出手时按最新气血重排,不提前锁死可能已被别人奶满的目标。
            // 治疗的目标集按气血比例升序,首位就是最缺血的
            const auto healTargets = SelectClassSkillTargets(actor, *skillRow, 0);
            const auto* neediest = healTargets.empty() ? nullptr : FindActor(healTargets.front());
            if (neediest != nullptr) {
                const uint64_t healed = AutoHealGain(actor, *skillRow, rank, healTargets);
                const double neediestRatio = HealthRatio(*neediest);
                if (neediestRatio < kAutoCriticalHealRatio) {
                    consider(AutoSkillPurpose::kCriticalHeal, healed, 0);
                } else if (neediestRatio <= kAutoHealRatio) {
                    consider(AutoSkillPurpose::kHeal, healed, 0);
                }
            }
        }

        const double baseDamage = hostile ? ClassSkillBaseDamage(actor, *skillRow) : 0.0;
        for (const auto primaryId : CollectClassSkillPrimaryTargets(actor, *skillRow)) {
            const auto targetIds = SelectClassSkillTargets(actor, *skillRow, primaryId);
            if (hostile) {
                const uint64_t damage = AutoDamageGain(actor, *skillRow, rank, baseDamage, targetIds);
                // 必须严格强于普攻,否则白花蓝、白进冷却
                if (static_cast<double>(damage) > basicAttackDamage) {
                    consider(AutoSkillPurpose::kDamage, damage, primaryId);
                }
            } else {
                consider(AutoSkillPurpose::kDispel, AutoDispelGain(*skillRow, targetIds), primaryId);
                consider(AutoSkillPurpose::kRestoreMana, AutoRestoreManaGain(actor, *skillRow, rank, targetIds),
                         primaryId);
            }
            consider(AutoSkillPurpose::kStatBuff, AutoStatBuffGain(actor, *skillRow, rank, targetIds), primaryId);
        }
    }
    if (!best.has_value()) {
        return std::nullopt;
    }

    BattleAction action;
    action.set_action_type(BATTLE_ACTION_SKILL);
    action.set_skill_table_id(best->skillId);
    action.set_target_id(best->targetId);
    return action;
}

std::vector<uint64_t> TurnBattleEngine::CollectClassSkillPrimaryTargets(const BattleActorState& actor,
                                                                        const SkillTable& skillRow) const {
    const auto team = static_cast<classskill::TargetTeam>(skillRow.target_team());
    if (team == classskill::TargetTeam::kSelf) {
        return {actor.actor_id()};
    }
    const bool wantAlly = team == classskill::TargetTeam::kAlly;
    std::vector<uint64_t> targetIds;
    for (const auto& other : actors) {
        if (IsActorActive(other) && (other.team_index() == actor.team_index()) == wantAlly) {
            targetIds.push_back(other.actor_id());
        }
    }
    return targetIds;
}

uint64_t TurnBattleEngine::AutoHealGain(const BattleActorState& actor, const SkillTable& skillRow, uint32_t rank,
                                        const std::vector<uint64_t>& targetIds) const {
    uint64_t healed = 0;
    for (const auto targetId : targetIds) {
        if (const auto* target = FindActor(targetId); target != nullptr) {
            healed = SaturatingAdd(healed, LandedHeal(*target, ClassSkillHealAmount(actor, skillRow, rank, *target)));
        }
    }
    return healed;
}

uint64_t TurnBattleEngine::AutoDispelGain(const SkillTable& skillRow, const std::vector<uint64_t>& targetIds) const {
    uint64_t removable = 0;
    for (const auto targetId : targetIds) {
        const auto* target = FindActor(targetId);
        if (target == nullptr) {
            continue;
        }
        const auto debuffCount = std::count_if(target->buffs().begin(), target->buffs().end(),
                                               [this](const BattleBuffEntry& entry) {
                                                   return !IsConfiguredListenerBuff(entry) && IsDebuffEntry(entry);
                                               });
        const uint64_t targetRemovable =
            std::min(static_cast<uint64_t>(skillRow.dispel_count()), static_cast<uint64_t>(debuffCount));
        if (targetRemovable == 0 && targetId == targetIds.front()) {
            return 0;
        }
        removable += targetRemovable;
    }
    return removable;
}

uint64_t TurnBattleEngine::AutoRestoreManaGain(const BattleActorState& actor, const SkillTable& skillRow, uint32_t rank,
                                               const std::vector<uint64_t>& targetIds) const {
    const uint64_t manaCost = SkillManaCost(actor, skillRow);
    uint64_t restored = 0;
    bool anyLowMana = false;
    for (const auto targetId : targetIds) {
        const auto* target = FindActor(targetId);
        if (target == nullptr) {
            continue;
        }
        anyLowMana |= ManaRatio(*target) <= kAutoRestoreManaRatio;
        // 出手先扣蓝再回蓝:施法者自己的缺口要把这一手的耗蓝算进去
        const uint64_t mana = target->attributes().mana();
        const uint64_t manaBefore = targetId != actor.actor_id() ? mana : (mana > manaCost ? mana - manaCost : 0);
        restored = SaturatingAdd(restored, ClassSkillManaRestored(skillRow, rank, *target, manaBefore));
    }
    return anyLowMana && restored > manaCost ? restored - manaCost : 0;
}

uint64_t TurnBattleEngine::AutoStatBuffGain(const BattleActorState& actor, const SkillTable& skillRow, uint32_t rank,
                                            const std::vector<uint64_t>& targetIds) const {
    const double statPercent = skillRow.buff_percent() * classskill::SupportRankFactor(rank);
    const bool hostile = skillRow.target_team() == static_cast<uint32_t>(classskill::TargetTeam::kEnemy);
    if (hostile ? statPercent >= 0.0 : statPercent <= 0.0) {
        return 0;
    }
    uint64_t gain = 0;
    for (const auto targetId : targetIds) {
        const auto* target = FindActor(targetId);
        if (target == nullptr) {
            continue;
        }
        for (const auto effectBuffId : skillRow.effect()) {
            const auto* buffRow = dataProvider->FindBuff(effectBuffId);
            if (buffRow == nullptr || IsImmuneToBuff(*target, *buffRow)) {
                continue;
            }
            gain = SaturatingAdd(gain,
                                 ClassBuffStatGain(*target, ClassSkillStatOfBuffType(buffRow->buff_type()), statPercent));
        }
    }
    return gain;
}

uint64_t TurnBattleEngine::AutoDamageGain(const BattleActorState& actor, const SkillTable& skillRow, uint32_t rank,
                                          double baseDamage, const std::vector<uint64_t>& targetIds) const {
    const uint64_t attack =
        combatdamage::SelectAttack(skillRow.damage_type(), EffectivePhysicalAttack(actor), actor.magic_attack());
    uint64_t damage = 0;
    for (const auto targetId : targetIds) {
        const auto* target = FindActor(targetId);
        if (target == nullptr) {
            continue;
        }
        const double expected = ExpectedDamage(actor, *target, baseDamage, attack, skillRow.attack_multiplier(),
                                               skillRow.damage_type()) *
                                classskill::DamageRankFactor(rank);
        damage = SaturatingAdd(damage, LandedDamage(*target, expected));
    }
    return damage;
}

uint64_t TurnBattleEngine::ClassBuffStatGain(const BattleActorState& target, ClassBuffStat stat,
                                             double statPercent) const {
    // 比较的是挂上前后的倍率,不是净倍率与新幅度:身上 +30% −20% 时再挂 +20% 不改变任何东西
    const uint64_t baseValue = BaseClassBuffStat(target, stat);
    const uint64_t before = classskill::FloorRatio(baseValue, ClassBuffStatMultiplier(target, stat));
    const uint64_t after = classskill::FloorRatio(baseValue, ClassBuffStatMultiplier(target, stat, statPercent));
    if (stat == ClassBuffStat::kDefense && statPercent > 0.0 && HealthRatio(target) > kAutoDefenseBuffHealthRatio) {
        return 0;
    }
    if (stat == ClassBuffStat::kSpeed && !SpeedChangeFlipsOrder(target, before, after)) {
        return 0;
    }
    return before > after ? before - after : after - before;
}

bool TurnBattleEngine::SpeedChangeFlipsOrder(const BattleActorState& target, uint64_t speedBefore,
                                             uint64_t speedAfter) const {
    const auto actsFirst = [&target](uint64_t speed, const BattleActorState& other, uint64_t otherSpeed) {
        return speed != otherSpeed ? speed > otherSpeed : target.actor_id() < other.actor_id();
    };
    for (const auto& other : actors) {
        if (other.team_index() == target.team_index() || !IsActorActive(other)) {
            continue;
        }
        const uint64_t otherSpeed = EffectiveSpeed(other);
        if (actsFirst(speedBefore, other, otherSpeed) != actsFirst(speedAfter, other, otherSpeed)) {
            return true;
        }
    }
    return false;
}

double TurnBattleEngine::AutoBasicAttackDamage(const BattleActorState& actor) const {
    const auto enemyIds = CollectAliveEnemyIds(actor);
    if (enemyIds.empty()) {
        return 0.0;
    }
    // 连击期望:每段成立才续下一段,追加段打 kComboDamagePercent 折(同 ExecuteAttack)
    const double comboChance = std::min(static_cast<double>(actor.combat().combo_rate()) / 100.0, 1.0);
    double comboFactor = 1.0;
    double chainChance = 1.0;
    for (uint32_t extraHit = 1; extraHit <= kMaxComboExtraHits; ++extraHit) {
        chainChance *= comboChance;
        comboFactor += chainChance * static_cast<double>(kComboDamagePercent) / 100.0;
    }

    const uint64_t attack = EffectivePhysicalAttack(actor);
    uint64_t total = 0;
    for (const auto enemyId : enemyIds) {
        if (const auto* enemy = FindActor(enemyId); enemy != nullptr) {
            const double expected = ExpectedDamage(actor, *enemy, kBasicAttackBaseDamage, attack, 1.0,
                                                   combatdamage::kPhysicalDamage) *
                                    comboFactor;
            total = SaturatingAdd(total, LandedDamage(*enemy, expected));
        }
    }
    return static_cast<double>(total) / static_cast<double>(enemyIds.size());
}

void TurnBattleEngine::ExecuteDefend(BattleActorState& actor, TurnResultS2C& result) {
    actor.set_is_defending(true);
    auto* ev = AppendEvent(result, BATTLE_EVENT_DEFEND, actor.actor_id(), actor.actor_id());
    // 灵息充盈周（全员对称）：双方普通防御额外回复 3% 最大法力，按缺口封顶。
    if (const uint32_t manaRegenBps = seasonrules::DefendBonusManaRegenBps(activeSeasonWeeklyAffixId);
        manaRegenBps > 0u && actor.max_mana() > 0 && actor.attributes().mana() < actor.max_mana()) {
        const uint64_t curMana = actor.attributes().mana();
        const uint64_t missing = actor.max_mana() - curMana;
        const uint64_t gain = std::min(missing,
            std::max<uint64_t>(1ULL, (actor.max_mana() * manaRegenBps) / 10000ULL));
        actor.mutable_attributes()->set_mana(curMana + gain);
        ev->set_value(static_cast<int64_t>(gain));
    }
}

void TurnBattleEngine::ExecuteItem(BattleActorState& actor, const BattleAction& action,
                                   TurnResultS2C& result) {
    // 出手期重跑校验:提交到出手之间,目标可能被别人打死、PVP 限次可能已被同回合的
    // 另一次提交用掉。不过则先回落自疗(见下),连自疗都不合法才落空。
    // 注意始终不降级成普攻:玩家点的是"吃药",降级会打出他没点过的伤害;
    // 技能那边之所以降级,是因为技能本身就是攻击行为。
    BattleAction effective = action;
    if (CheckItemUse(actor, effective) != kSuccess) {
        // 目标在提交之后、出手之前被打死是最常见的一种:回落成自疗,而不是让整个回合
        // 静默蒸发(玩家点的是"吃药",药还在身上,自疗仍然是他本意的一部分)。
        // 回落后再校验一次:连自疗都不合法(道具用尽 / PVP 限次)才真正落空。
        if (effective.target_id() == 0 || effective.target_id() == actor.actor_id()) {
            return;
        }
        effective.set_target_id(actor.actor_id());
        if (CheckItemUse(actor, effective) != kSuccess) {
            return;
        }
    }
    const auto* itemRow = dataProvider->FindItem(effective.item_table_id());
    BattleItemEntry* itemEntry = FindItemEntry(actor.actor_id(), effective.item_table_id());
    const uint64_t targetId = effective.target_id() == 0 ? actor.actor_id() : effective.target_id();
    BattleActorState* target = FindActor(targetId);
    if (itemRow == nullptr || itemEntry == nullptr || target == nullptr) {
        return;  // CheckItemUse 已保证三者都在,这里是最后防线
    }

    // 先扣副本再落效果:效果可能因满血而为 0,但药是真用掉了(与实时侧口径一致)
    itemEntry->set_count(itemEntry->count() - 1);
    ++itemUseCounts[actor.actor_id()];

    // 结算账本累加消耗(scene 侧按实际持有夹紧扣除,见设计 §5.3)
    auto& settlement = settlements[actor.actor_id()];
    BattleItemEntry* consumedEntry = nullptr;
    for (auto& consumed : *settlement.mutable_items_consumed()) {
        if (consumed.item_table_id() == effective.item_table_id()) {
            consumedEntry = &consumed;
            break;
        }
    }
    if (consumedEntry == nullptr) {
        consumedEntry = settlement.add_items_consumed();
        consumedEntry->set_item_table_id(effective.item_table_id());
    }
    consumedEntry->set_count(consumedEntry->count() + 1);

    // 效果按 ItemTable 表列:回血、回蓝可同时非 0
    const uint64_t healed = itemRow->battle_heal_hp() > 0
                                ? ApplyHeal(*target, static_cast<double>(itemRow->battle_heal_hp()))
                                : 0;
    uint64_t restoredMana = 0;
    if (itemRow->battle_heal_mp() > 0 && !target->is_dead()) {
        const uint64_t manaBefore = target->attributes().mana();
        const uint64_t manaAfter =
            std::min<uint64_t>(target->max_mana(), manaBefore + itemRow->battle_heal_mp());
        restoredMana = manaAfter - manaBefore;
        target->mutable_attributes()->set_mana(manaAfter);
    }

    // value 取回血量;纯回蓝药回蓝量,客户端两个 after 字段都能拿到终值
    auto* itemEvent = AppendEvent(result, BATTLE_EVENT_ITEM, actor.actor_id(), target->actor_id());
    itemEvent->set_item_table_id(effective.item_table_id());
    itemEvent->set_value(healed > 0 ? healed : restoredMana);
    if (itemRow->battle_heal_mp() > 0) {
        // 回蓝药:带上实际回复量(满蓝时为 0),客户端据此区分回蓝与扣蓝
        itemEvent->set_mana_delta(static_cast<int64_t>(restoredMana));
    }
    itemEvent->set_target_health_after(target->attributes().health());
    itemEvent->set_target_mana_after(target->attributes().mana());
}

uint32_t TurnBattleEngine::CheckCapture(const BattleActorState& actor, uint64_t targetId) const {
    if (IsFestivalBeastBattle()) return kInvalidParameter;
    const auto* target = FindActor(targetId);
    if (!IsPveMatch() || actor.actor_type() != BATTLE_ACTOR_TYPE_PLAYER || actor.team_index() != 0 ||
        target == nullptr || target->actor_type() != BATTLE_ACTOR_TYPE_MONSTER ||
        target->team_index() == actor.team_index() || !IsActorActive(*target) ||
        target->capture_pet_table_id() == 0 || actor.level() < target->capture_min_level()) return kInvalidParameter;
    const auto taken = settlements.find(actor.actor_id());
    const auto captured = taken == settlements.end() ? 0 : taken->second.captured_pet_table_ids_size();
    for (const auto& snapshot : createRequest.players()) {
        if (snapshot.player_id() == actor.actor_id())
            return static_cast<uint32_t>(captured) < snapshot.pet_capture_slots() ? kSuccess : kInvalidParameter;
    }
    return kInvalidParameter;
}

void TurnBattleEngine::ExecuteCapture(BattleActorState& actor, const BattleAction& action, TurnResultS2C& result) {
    auto* event = AppendEvent(result, BATTLE_EVENT_CAPTURE, actor.actor_id(), action.target_id());
    // 同回合别人已捕捉或击杀目标时，本次消耗行动但不改选其它目标。
    if (CheckCapture(actor, action.target_id()) != kSuccess) return;
    auto* target = FindActor(action.target_id());
    const auto* row = dataProvider->FindMonster(target->monster_table_id());
    if (row == nullptr || target->max_health() == 0) return;
    const double lostRatio = 1.0 - static_cast<double>(target->attributes().health()) /
        static_cast<double>(target->max_health());
    const double chance = std::min(1.0, row->capture_rate() / 10000.0 * (1.0 + 2.0 * lostRatio));
    const bool success = chance >= 1.0 || Rand01() < chance;
    event->set_success(success);
    event->set_value(target->capture_pet_table_id());
    event->set_target_health_after(target->attributes().health());
    if (!success) return;
    target->set_captured_by_player_id(actor.actor_id());
    target->set_fled(true);
    settlements[actor.actor_id()].add_captured_pet_table_ids(target->capture_pet_table_id());
}

void TurnBattleEngine::ExecuteFlee(BattleActorState& actor, TurnResultS2C& result) {
    bool success = false;
    if (IsBossTrialKind(bossTrialKind) && actor.actor_type() == BATTLE_ACTOR_TYPE_PLAYER) {
        // 首领试炼确定退出（boss-trial-planning-ui.md §6.2）：无随机逃跑失败、不消耗 RNG，本人及宝宝原子离场。
        success = true;
    } else if (IsPveMatch()) {
        // 成功率基于速度差:base + 系数 * (自身速度 - 存活敌方最高速度),夹在上下限内
        const double speedDiff = static_cast<double>(EffectiveSpeed(actor)) -
                                 static_cast<double>(MaxAliveEnemySpeed(actor));
        const double chance = std::clamp(kFleeBaseChance + kFleeSpeedFactor * speedDiff,
                                         kFleeMinChance, kFleeMaxChance);
        success = Rand01() < chance;
    }

    auto* fleeEvent = AppendEvent(result, BATTLE_EVENT_FLEE, actor.actor_id(), actor.actor_id());
    fleeEvent->set_success(success);

    if (success) {
        actor.set_fled(true);
        // 首领试炼与瑞兽迎春离场时，玩家本人与名下出战宝宝一并离场（常规 PVE 保持单单位逃跑旧不变量）。
        if ((IsBossTrialKind(bossTrialKind) || IsFestivalBeastBattle()) &&
            actor.actor_type() == BATTLE_ACTOR_TYPE_PLAYER) {
            for (auto& other : actors) {
                if (other.actor_type() == BATTLE_ACTOR_TYPE_PET &&
                    other.owner_player_id() == actor.actor_id() && !other.fled()) {
                    const bool wasActive = IsActorActive(other);
                    other.set_fled(true);
                    if (wasActive) {
                        auto* petFlee = AppendEvent(result, BATTLE_EVENT_FLEE, other.actor_id(), other.actor_id());
                        petFlee->set_success(true);
                    }
                }
            }
        }
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
        if (buffRow->battle_op() != 0) continue;  // 现代状态统一走全场先伤害、后复苏的回合时钟。

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
        // 标记了施加回合的 Buff 当轮末不扣持续:属性增减益保持完整时长,下次普攻有完整的行动机会。
        if (entry->applied_round() != 0 && entry->applied_round() == roundIndex) {
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
        if (HasConfiguredBuffs(actor)) {
            auto* source = FindActor(entry.caster_id());
            EffectContext context; context.skillId = entry.source_skill_id();
            ResolveConfiguredHeal(source == nullptr ? actor : *source, actor, healAmount, context, result);
            break;
        }
        const uint64_t healed = ApplyHeal(actor, healAmount);
        if (healed > 0) {
            if (const auto* source = FindActor(entry.caster_id()); source != nullptr) {
                RecordTrialMonsterHeal(*source, actor, healed);
            }
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
        double rawDamage =
            buffRow.interval_effect(0) * static_cast<double>(std::max(1u, entry.layer()));
        if (HasConfiguredBuffs(actor)) {
            auto* source = FindActor(entry.caster_id());
            EffectContext context; context.origin = BattleDamageOrigin::Periodic;
            context.periodicState = buffRow.buff_type() == kBuffTypePoison ? 7 : 8;
            ResolveConfiguredDamage(source == nullptr ? actor : *source, actor, rawDamage,
                combatdamage::kMagicDamage, context, result);
            break;
        }
        if (bossWeeklyAffixId == bossaffix::kAffixWater && actor.team_index() == 1) {
            const auto* source = FindActor(entry.caster_id());
            if (source != nullptr && source->team_index() == 0) {
                rawDamage = rawDamage * (100.0 + bossaffix::kWaterChargeOrPeriodicDamageBonusPct) / 100.0;
            }
        }
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
            HandleDeath(actor, entry.caster_id(), result);
        }
        if (dealt > 0 && actor.team_index() == 1) {
            ApplyWuxingPeriodicSpread(entry.caster_id(), actor, dealt, result);
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
        const uint32_t step = (actor.team_index() == 0 && HasRealmResonance(wuxing::kElementWater)) ? 2u : 1u;
        auto* cooldownMap = actor.mutable_skill_cooldown_rounds();
        for (auto it = cooldownMap->begin(); it != cooldownMap->end(); ++it) {
            if (it->second > 0) {
                it->second = it->second > step ? (it->second - step) : 0u;
            }
        }
    }
}

void TurnBattleEngine::UpdateOutcome() {
    // 护卫救援：受保护护卫阵亡即刻判负（即便同回合或同行动击杀首领，保护目标阵亡仍判负）。
    if (bossTrialKind == kBossTrialKindGuardianRescue) {
        const auto* guard = FindActor(protectedGuardActorId);
        if (guard == nullptr || guard->is_dead()) {
            outcome = BATTLE_OUTCOME_SIDE_B_WIN;
            return;
        }
        bool anyPlayerActive = false;
        for (const auto& actor : actors) {
            if (actor.actor_type() == BATTLE_ACTOR_TYPE_PLAYER && actor.team_index() == 0 && IsActorActive(actor)) {
                anyPlayerActive = true;
                break;
            }
        }
        if (!anyPlayerActive) {
            outcome = BATTLE_OUTCOME_SIDE_B_WIN;
            return;
        }
        if (SideWiped(1)) {
            outcome = BATTLE_OUTCOME_SIDE_A_WIN;
            return;
        }
        if (roundIndex >= maxRounds) {
            outcome = BATTLE_OUTCOME_SIDE_B_WIN;
        }
        return;
    }

    const bool sideAWiped = SideWiped(0);
    const bool sideBWiped = SideWiped(1);

    if (sideAWiped && sideBWiped) {
        outcome = BATTLE_OUTCOME_DRAW;
    } else if (sideAWiped) {
        outcome = BATTLE_OUTCOME_SIDE_B_WIN;
    } else if (sideBWiped) {
        outcome = BATTLE_OUTCOME_SIDE_A_WIN;
    } else if (roundIndex >= maxRounds) {
        // 万象残局坊回合耗尽仍有存活敌方单位时权威判负（SIDE_B_WIN），绝不判平。
        if (hasTacticalPuzzleContext) {
            outcome = BATTLE_OUTCOME_SIDE_B_WIN;
        } else if (IsRankedBattle() || bossTrialKind == kBossTrialKindPolarity5v5 ||
                   createRequest.match_mode() == kMatchModePolarity5v5 || IsGuildWarBattle() ||
                   hasCustomDuelRules || IsJianghuDuelBattle()) {
            outcome = BATTLE_OUTCOME_DRAW;
        } else {
            outcome = BATTLE_OUTCOME_SIDE_B_WIN;
        }
    }
}

std::string TurnBattleEngine::SelectRewardStrategy(uint64_t playerId) const {
    // 尚未确定战斗策略识别规则，当前所有玩家均选通用档。
    (void)playerId;
    return {};
}

void TurnBattleEngine::RollRewards() {
    if (!IsPveMatch() || outcome != BATTLE_OUTCOME_SIDE_A_WIN || IsFestivalBeastBattle() || hasTowerContext || hasTacticalPuzzleContext) {
        return;
    }
    for (auto& [playerId, settlement] : settlements) {
        const auto* actor = FindActor(playerId);
        if (actor == nullptr || actor->team_index() != 0 || !IsActorActive(*actor)) {
            continue;
        }
        BattleRewardProfile profile = defaultRewardProfile;
        const auto strategyKey = SelectRewardStrategy(playerId);
        if (!strategyKey.empty()) {
            const auto* selected = dataProvider->FindBattleReward(createRequest.battle_config_id(), strategyKey);
            // 找不到策略档时使用开局冻结的通用档，不重新读取可能已改变的通用配置。
            if (selected != nullptr && selected->strategy_key() == strategyKey && IsValidRewardProfile(*selected)) {
                profile = *selected;
            }
        }
        settlement.set_exp_gain(profile.exp_gain());
        settlement.set_gold_gain(0);
        *settlement.mutable_currency_gains() = profile.currencies();

        uint64_t totalWeight = 0;
        for (const auto& item : profile.random_items()) {
            totalWeight += item.weight();
        }
        std::mt19937_64 rewardRng(MixRewardSeed(createRequest.seed() ^ MixRewardSeed(playerId)));
        // 有放回抽取；先按物品号汇总，输出次序不依赖被抽中的先后。
        std::map<uint32_t, uint32_t> itemCounts;
        for (uint32_t draw = 0; draw < profile.item_draws(); ++draw) {
            uint64_t roll = RewardRandomIndex(rewardRng, totalWeight);
            for (const auto& item : profile.random_items()) {
                if (roll < item.weight()) {
                    itemCounts[item.item_table_id()] += item.count();
                    break;
                }
                roll -= item.weight();
            }
        }
        for (const auto& [itemId, count] : itemCounts) {
            auto* entry = settlement.add_items_gained();
            entry->set_item_table_id(itemId);
            entry->set_count(count);
        }
    }
}

void TurnBattleEngine::SanitizeSnapshotBuffs(BattleActorState& actor) {
    // 快照 buff 来自 scene 的实时战斗世界,直接照搬会出三类问题(G5):
    //   ① 控制类(眩晕/冰冻/沉默)按表全量时长换算,场景里只剩半秒的控制进战斗会变成整整几回合;
    //   ② 瞬时 buff(非无限且 duration<=0)在实时侧是"挂上即到期",快照却把它当无限持续;
    //   ③ caster_id 是 scene 的 entt 实体整数,与局内 actor_id 不是一个域,
    //      恰好等于某个 player_id 时会被当成那名玩家(叠层隔离、BUFF_TICK 来源都用它)。
    std::vector<BattleBuffEntry> kept;
    kept.reserve(static_cast<size_t>(actor.buffs_size()));
    for (const auto& buff : actor.buffs()) {
        const auto* buffRow = dataProvider->FindBuff(buff.buff_table_id());
        if (buffRow == nullptr) {
            continue;  // 两端表版本不一致时宁可丢这条,也不让引擎带着空行跑
        }
        const uint32_t buffType = buffRow->buff_type();
        if (buffType == kBuffTypeStun || buffType == kBuffTypeFreeze ||
            buffType == kBuffTypeSilence) {
            continue;
        }
        if (buffRow->infinite_duration() == 0 && buffRow->duration() <= 0) {
            continue;
        }
        BattleBuffEntry entry = buff;
        // 局内字段只能由引擎自己写:快照带进来的一律清掉(场景里的 buff 没有职业技能幅度)
        entry.clear_stat_percent();
        entry.clear_applied_round();
        // caster 认不出来就置 0(= 无施法者):FindActor 找不到 0,叠层按"不同施法者"处理,
        // BUFF_TICK/BUFF_REMOVE 事件的 source 也不会指向一个不相干的玩家
        if (entry.caster_id() != 0 && FindActor(entry.caster_id()) == nullptr) {
            entry.set_caster_id(0);
        }
        // 夹到表全量时长:快照给 0(无限)而表本身有限时,按表纠正
        if (buffRow->infinite_duration() == 0) {
            const uint32_t tableRounds = RoundsFromSeconds(buffRow->duration());
            if (entry.remain_rounds() == 0 || entry.remain_rounds() > tableRounds) {
                entry.set_remain_rounds(tableRounds);
            }
        }
        kept.emplace_back(std::move(entry));
    }
    actor.clear_buffs();
    for (auto& entry : kept) {
        *actor.add_buffs() = std::move(entry);
    }
}

std::vector<BattleItemEntry> TurnBattleEngine::SelfItems(uint64_t playerId) const {
    std::vector<BattleItemEntry> items;
    for (const auto& snapshot : createRequest.players()) {
        if (snapshot.player_id() != playerId) {
            continue;
        }
        for (const auto& item : snapshot.items()) {
            if (item.count() == 0) {
                continue;  // 用光的条目不下发,客户端按"没有这一项"处理
            }
            items.emplace_back(item);
        }
        break;
    }
    std::sort(items.begin(), items.end(),
              [](const BattleItemEntry& lhs, const BattleItemEntry& rhs) {
                  return lhs.item_table_id() < rhs.item_table_id();
              });
    return items;
}

// ---------------------------------------------------------------------------
// 伤害 / 治疗 / 死亡
// ---------------------------------------------------------------------------

bool TurnBattleEngine::IsPveMatch() const {
    return createRequest.match_mode() == kMatchModePveSolo ||
           createRequest.match_mode() == kMatchModePveTeam;
}

double TurnBattleEngine::CalculateFinalDamage(const BattleActorState& caster,
                                              const BattleActorState& target, double baseDamage,
                                              uint64_t attack, double attackMultiplier,
                                              uint32_t damageType, bool& isCritical) {
    isCritical = false;

    double finalDamage = DamageBeforeCriticalRoll(caster, target, baseDamage, attack, attackMultiplier, damageType);
    double critChance = CriticalChance(caster, damageType);
    if (bossWeeklyAffixId == bossaffix::kAffixMetal && caster.team_index() == 0 && target.team_index() == 1 &&
        IsActorExposed(target.actor_id())) {
        critChance = std::min(1.0, critChance + static_cast<double>(bossaffix::kMetalExposedCritBonusPct) / 100.0);
    }

    // 暴击只走引擎 RNG;critChance 为 0 时不消耗随机数(与实时短路口径一致)。
    // 掷骰位置与改公式前相同,同种子回放的随机数消费序列不变。
    // 注意满 100 也掷骰(Rand01 ∈ [0,1) 恒小于 1,结果必为真):这是装备系统落地前的既有行为,
    // 改成短路会平移所有满暴击单位之后的随机序列,所以不与 RollPercent 的两端短路对齐
    if (critChance > 0.0 && Rand01() < critChance) {
        double critMultiplier = kCriticalDamageMultiplier;
        if (hasTowerContext && caster.team_index() == 0 && towerSigilTotals.critDamagePct > 0) {
            critMultiplier += static_cast<double>(towerSigilTotals.critDamagePct) / 100.0;
        }
        finalDamage *= critMultiplier;
        isCritical = true;
    }

    return std::max(finalDamage, 0.0);
}

double TurnBattleEngine::DamageBeforeCriticalRoll(const BattleActorState& caster, const BattleActorState& target,
                                                  double baseDamage, uint64_t attack, double attackMultiplier,
                                                  uint32_t damageType) const {
    // 战斗类属性按伤害类型分流(equipment-attributes.md §4.5,D6 / D7):
    // 物理吃目标的抗物理,法术吃抗法术,必杀率同样分流(CriticalChance)。
    // 未知类型两边都不吃,与 combatdamage::SelectAttack 对未知类型不给攻击同口径。
    // 加成全 0(怪物 / 宝宝 / 无装备玩家)时这里的和就是原来的 resistance,
    // 公式与随机数消耗和加这组属性之前逐位一致
    uint64_t resistBonus = 0;
    if (damageType == combatdamage::kPhysicalDamage) {
        resistBonus = target.combat().physical_resist();
    } else if (damageType == combatdamage::kMagicDamage) {
        resistBonus = target.combat().magic_resist();
    }

    const double penetration = std::clamp(ConfiguredModifier(caster, BattleModifier::DefensePenetration), 0.0, 0.5);
    const double defenseKeep = 1.0 - penetration;
    const uint64_t effectiveArmor = penetration > 0.0
        ? static_cast<uint64_t>(std::floor(static_cast<double>(target.attributes().armor()) * defenseKeep))
        : target.attributes().armor();
    const uint64_t effectiveDefense = penetration > 0.0
        ? static_cast<uint64_t>(std::floor(static_cast<double>(EffectiveDefense(target)) * defenseKeep))
        : EffectiveDefense(target);

    // 目标等级:玩家 / 宝宝取快照等级,怪物取参战玩家最高等级(InitMonsters)。
    // 抗性之和交给规则头夹取:它与护甲 / 防御共用 60% 常驻减伤封顶(规则头与实时技能共用,不在这里另夹)
    double damage = combatdamage::DamageBeforeCritical(
        baseDamage, caster.attributes().strength(), attack, attackMultiplier,
        effectiveArmor, effectiveDefense,
        SaturatingAdd(target.attributes().resistance(), resistBonus), target.level());
    const uint32_t equipmentResist = damageType == combatdamage::kPhysicalDamage ? target.equipment_effects().physical_resist_bps() :
        damageType == combatdamage::kMagicDamage ? target.equipment_effects().magic_resist_bps() : 0;
    if (equipmentResist > 0) {
        const uint64_t legacyResist = SaturatingAdd(target.attributes().resistance(), resistBonus);
        const double originalRatio = combatdamage::ReceivedRatio(effectiveArmor, effectiveDefense, legacyResist, target.level());
        damage *= equipbattle::ReceivedRatio(effectiveArmor, effectiveDefense,
            static_cast<double>(legacyResist) * 100 + equipmentResist, target.level()) / originalRatio;
    }
    if (!IsPveMatch()) {
        damage *= kPvpDamageScale;
    }
    // 锦标赛 V2 五行灵脉伤害缩放（与 Java 引擎定点数倍率逐项对齐）：金锐破防 +4%、火炽 +6%、土厚减伤 -5%。
    switch (ActiveTournamentLeyline()) {
    case TOURNAMENT_LEYLINE_METAL:
        damage *= 104.0 / 100.0;
        break;
    case TOURNAMENT_LEYLINE_FIRE:
        damage *= 106.0 / 100.0;
        break;
    case TOURNAMENT_LEYLINE_EARTH:
        damage *= 95.0 / 100.0;
        break;
    default:
        break;
    }
    // 赛季全员对称周环境词缀（先机守势周前 2 回合直伤 -8%；锋芒竞速周攻击无盾目标直伤 +5%）。
    if (const uint32_t openingReducePct =
            seasonrules::OpeningGuardDamageReducePct(activeSeasonWeeklyAffixId, roundIndex);
        openingReducePct > 0u) {
        damage *= (100.0 - openingReducePct) / 100.0;
    }
    if (const uint32_t sharpBonusPct = seasonrules::SharpTempoUnshieldedDamageBonusPct(
            activeSeasonWeeklyAffixId, HasAnyActiveShield(target));
        sharpBonusPct > 0u) {
        damage *= (100.0 + sharpBonusPct) / 100.0;
    }
    // 五行阵法 + 阵法互克 + 人宠五行相生合击 + 五行幻境局内符印（全确定性缩放，均 0 时零改动）。
    uint32_t attackerBonusPct = 0u;
    const auto srcForm = wuxing::FormationOf(TeamFormationId(caster.team_index()));
    const uint32_t dstFormId = TeamFormationId(target.team_index());
    attackerBonusPct += srcForm.damageBonusPct;
    attackerBonusPct += wuxing::FormationCounterBonusPct(srcForm.id, dstFormId);
    if (caster.actor_type() == BATTLE_ACTOR_TYPE_PET) {
        attackerBonusPct += srcForm.petDamageBonusPct;
    }
    if (IsWuxingSynergyActiveForActor(caster.actor_id())) {
        attackerBonusPct += wuxing::kSynergyBonusPct;
    }
    if (hasTowerContext && caster.team_index() == 0) {
        attackerBonusPct += towerSigilTotals.damagePct;
        if (caster.actor_type() == BATTLE_ACTOR_TYPE_PET) {
            attackerBonusPct += towerSigilTotals.petDamagePct;
        }
        if (roundIndex <= 3u) {
            attackerBonusPct += towerSigilTotals.first3RoundsDamagePct;
        }
        if (towerSigilTotals.shieldBreakBonusPct > 0u && HasAnyActiveShield(target)) {
            attackerBonusPct += towerSigilTotals.shieldBreakBonusPct;
        }
        if (HasRealmBoon(wuxing::kBoonMetalFirstPierce) &&
            !boonRuntime.firstAttackUsedThisRound.contains(caster.actor_id())) {
            attackerBonusPct += 35u;
        }
        if (HasRealmBoon(wuxing::kBoonWoodHealEmpower) &&
            boonRuntime.healShieldEmpoweredActors.contains(caster.actor_id())) {
            attackerBonusPct += 20u;
        }
        if (HasRealmBoon(wuxing::kBoonFireLowHpFury) && caster.max_health() > 0u) {
            const uint64_t hpPct = (caster.attributes().health() * 100ULL) / caster.max_health();
            if (hpPct < 60ULL) {
                attackerBonusPct += static_cast<uint32_t>(((60ULL - hpPct) * 35ULL) / 60ULL);
            }
        }
    }
    if (IsWuxingRealmV2()) {
        if (const auto it = boonRuntime.vulnerableUntilRound.find(target.actor_id());
            it != boonRuntime.vulnerableUntilRound.end() && it->second >= roundIndex) {
            attackerBonusPct += 18u;
        }
        if (const auto it = boonRuntime.armorBrokenUntilRound.find(target.actor_id());
            it != boonRuntime.armorBrokenUntilRound.end() && it->second >= roundIndex) {
            attackerBonusPct += 15u;
        }
    }
    if (attackerBonusPct > 0u) {
        damage *= (100.0 + attackerBonusPct) / 100.0;
    }
    if (caster.team_index() == 0 && HasRealmBoon(wuxing::kBoonEarthShieldStrike)) {
        const uint64_t curShield = CurrentTotalShieldOf(caster);
        if (curShield > 0u) {
            const uint64_t bonus = std::min(wuxing::kBoonShieldAttackBonusCap,
                                            std::max<uint64_t>(1ULL, (curShield * 25ULL) / 100ULL));
            damage += static_cast<double>(bonus);
        }
    }
    uint32_t defenderReducePct = wuxing::FormationOf(dstFormId).damageTakenReducePct;
    if (hasTowerContext && target.team_index() == 0) {
        defenderReducePct += towerSigilTotals.damageTakenReducePct;
    }
    if (defenderReducePct > 0u) {
        damage *= std::max(0.1, (100.0 - static_cast<double>(std::min<uint32_t>(defenderReducePct, 80u))) / 100.0);
    }
    return damage;
}

double TurnBattleEngine::CriticalChance(const BattleActorState& caster, uint32_t damageType) const {
    // 物理吃物理必杀率,法术吃法术必杀率,未知类型不吃(同 DamageBeforeCriticalRoll)
    uint64_t critRateBonus = 0;
    if (damageType == combatdamage::kPhysicalDamage) {
        critRateBonus = caster.combat().physical_crit_rate();
    } else if (damageType == combatdamage::kMagicDamage) {
        critRateBonus = caster.combat().magic_crit_rate();
    }
    // 锦标赛 V2 金锐灵脉：暴击率 +5%。
    if (ActiveTournamentLeyline() == TOURNAMENT_LEYLINE_METAL) {
        critRateBonus = SaturatingAdd(critRateBonus, 5ULL);
    }
    if (const uint32_t formCrit = wuxing::FormationOf(TeamFormationId(caster.team_index())).critBonusPct; formCrit > 0u) {
        critRateBonus = SaturatingAdd(critRateBonus, formCrit);
    }
    if (hasTowerContext && caster.team_index() == 0 && towerSigilTotals.critPct > 0u) {
        critRateBonus = SaturatingAdd(critRateBonus, towerSigilTotals.critPct);
    }
    if (caster.team_index() == 0 && HasRealmBoon(wuxing::kBoonFireLowHpFury) && caster.max_health() > 0u) {
        const uint64_t hpPct = (caster.attributes().health() * 100ULL) / caster.max_health();
        if (hpPct < 60ULL) {
            critRateBonus = SaturatingAdd(critRateBonus, ((60ULL - hpPct) * 20ULL) / 60ULL);
        }
    }
    // 暴击率为整数百分比口径,基础 + 加成之后换算并夹到 [0,1](与实时 CalculateFinalDamage 一致)
    const uint32_t equipmentCrit = damageType == combatdamage::kPhysicalDamage ? caster.equipment_effects().physical_crit_bps() :
        damageType == combatdamage::kMagicDamage ? caster.equipment_effects().magic_crit_bps() : 0;
    return std::clamp(static_cast<double>(SaturatingAdd(caster.attributes().critchance(), critRateBonus)) / 100.0 + equipmentCrit / 10000.0,
                      0.0, 1.0);
}

double TurnBattleEngine::ExpectedDamage(const BattleActorState& caster, const BattleActorState& target,
                                        double baseDamage, uint64_t attack, double attackMultiplier,
                                        uint32_t damageType) const {
    const double damage = DamageBeforeCriticalRoll(caster, target, baseDamage, attack, attackMultiplier, damageType);
    return damage * (1.0 + CriticalChance(caster, damageType) * (kCriticalDamageMultiplier - 1.0));
}

uint64_t TurnBattleEngine::LandedDamage(const BattleActorState& target, double rawDamage) const {
    if (rawDamage <= 0) {
        return 0;
    }
    // DEFEND:本回合受伤减半(含回合末周期伤害,防御摘除在 tick 之后)
    if (target.is_defending()) {
        rawDamage *= 0.5;
    }

    // 与实时 ApplyDamage 共用落血规则:向上取整、不超过当前气血、非有限数不落血
    return combatdamage::DamageToHealth(rawDamage, target.attributes().health());
}

uint64_t TurnBattleEngine::AbsorbTournamentEarthShield(uint64_t actorId, uint64_t& remaining) {
    if (remaining == 0 || leylineEarthShieldByActor.empty()) {
        return 0;
    }
    auto it = leylineEarthShieldByActor.find(actorId);
    if (it == leylineEarthShieldByActor.end() || it->second == 0) {
        return 0;
    }
    const uint64_t absorbed = std::min(remaining, it->second);
    it->second -= absorbed;
    remaining -= absorbed;
    if (it->second == 0) {
        leylineEarthShieldByActor.erase(it);
    }
    return absorbed;
}

uint64_t TurnBattleEngine::ApplyDamage(BattleActorState& target, double rawDamage) {
    uint64_t damage = LandedDamage(target, rawDamage);
    AbsorbTournamentEarthShield(target.actor_id(), damage);
    if (damage > 0) {
        target.mutable_attributes()->set_health(target.attributes().health() - damage);
    }
    return damage;
}

uint64_t TurnBattleEngine::LandedHeal(const BattleActorState& target, double rawHeal) const {
    // !(> 0) 同时挡住 NaN;治疗量按缺口封顶后再转整数,高血量 + 大倍率也不会越界
    if (!(rawHeal > 0) || !std::isfinite(rawHeal) || target.is_dead()) {
        return 0;
    }
    if (ActiveTournamentLeyline() == TOURNAMENT_LEYLINE_WOOD) {
        rawHeal *= 110.0 / 100.0;
    }
    if (EscalationStacks() > 0) {
        rawHeal *= static_cast<double>(EscalationSupportPct()) / 100.0;
    }
    const uint64_t healthBefore = target.attributes().health();
    const uint64_t missing = target.max_health() > healthBefore ? target.max_health() - healthBefore : 0;
    const long double cappedHeal = std::min<long double>(static_cast<long double>(rawHeal),
                                                         static_cast<long double>(missing));
    return static_cast<uint64_t>(cappedHeal);
}

uint64_t TurnBattleEngine::ApplyHeal(BattleActorState& target, double rawHeal) {
    const uint64_t healed = LandedHeal(target, rawHeal);
    if (healed > 0) {
        target.mutable_attributes()->set_health(target.attributes().health() + healed);
        if (target.team_index() == 0 && HasRealmBoon(wuxing::kBoonWoodHealEmpower)) {
            boonRuntime.healShieldEmpoweredActors.insert(target.actor_id());
        }
    }
    return healed;
}

void TurnBattleEngine::HandleDeath(BattleActorState& target, uint64_t sourceActorId,
                                    TurnResultS2C& result) {
    // 只在活体转入死亡时记一次；连续伤害或重复结算读取不能重记同一只怪。
    if (target.is_dead() || target.attributes().health() != 0) {
        return;
    }
    const auto* source = FindActor(sourceActorId);
    const auto* owner = source != nullptr && source->actor_type() == BATTLE_ACTOR_TYPE_PET
        ? FindActor(source->owner_player_id()) : source;
    // 毒/灼烧沿用施加者归属；无来源、怪物自伤/友伤、环境死亡不能冒充玩家击杀。
    // 施加者后来阵亡不抹掉已经生效的 DoT，其余存活队友仍按既有组队口径结算。
    if (target.actor_type() == BATTLE_ACTOR_TYPE_MONSTER && target.team_index() == 1 &&
        target.monster_table_id() != 0 && source != nullptr && source->team_index() == 0 &&
        owner != nullptr && owner->actor_type() == BATTLE_ACTOR_TYPE_PLAYER && owner->team_index() == 0) {
        auto& defeat = defeatedMonsters.emplace_back();
        defeat.set_monster_config_id(target.monster_table_id());
        defeat.set_count(1);
    }
    target.set_is_dead(true);
    target.set_is_defending(false);
    for (auto& artifact : *target.mutable_artifact_states()) {
        artifact.clear_pending_ailment_kind(); artifact.clear_pending_round();
    }
    if (!HasConfiguredBuffs(target)) {
        target.clear_buffs();
    } else {
        std::vector<uint64_t> remove;
        for (const auto& entry : target.buffs()) {
            const auto* row = dataProvider->FindBuff(entry.buff_table_id());
            if (row == nullptr || (row->keep_on_death() == 0 &&
                !IsConfiguredListenerBuff(entry) && !row->tag().contains("Passive"))) remove.push_back(entry.buff_id());
        }
        for (const auto id : remove) RemoveConfiguredBuff(target, id, result);
        ResetVolatileGrowingStacks(target);
    }
    everDiedActors.insert(target.actor_id());
    AppendEvent(result, BATTLE_EVENT_DEATH, target.actor_id(), target.actor_id());
    if (target.actor_type() == BATTLE_ACTOR_TYPE_MONSTER && target.team_index() == 1 &&
        IsBossTrialKind(bossTrialKind)) {
        HandleBossTrialMonsterDeath(target, result);
    }
}

void TurnBattleEngine::RecordTrialDirectDamage(const BattleActorState& source, const BattleActorState& target,
                                               uint32_t damageType, uint64_t dealtOrAbsorbed) {
    if (dealtOrAbsorbed == 0 || source.team_index() == target.team_index() || source.team_index() >= 2) {
        return;
    }
    if (damageType == combatdamage::kPhysicalDamage) {
        teamDealtPhysicalDirectDamage[source.team_index()] = true;
        if (source.team_index() == 0 && target.team_index() == 1) {
            auto& tally = roundPlayerDirectDamage[roundIndex];
            tally.first = SaturatingAdd(tally.first, dealtOrAbsorbed);
            if (bossTrialKind == kBossTrialKindWildBell && IsWildBellYangRound(roundIndex)) {
                dealtYangBellPhysicalDamage = true;
            }
        }
    } else if (damageType == combatdamage::kMagicDamage) {
        teamDealtMagicDirectDamage[source.team_index()] = true;
        if (source.team_index() == 0 && target.team_index() == 1) {
            auto& tally = roundPlayerDirectDamage[roundIndex];
            tally.second = SaturatingAdd(tally.second, dealtOrAbsorbed);
            if (bossTrialKind == kBossTrialKindWildBell && !IsWildBellYangRound(roundIndex)) {
                dealtYinDrumMagicDamage = true;
            }
        }
    }
}

void TurnBattleEngine::RecordTrialMonsterHeal(const BattleActorState& source, const BattleActorState& target,
                                              uint64_t healed) {
    if (healed == 0 || source.actor_type() != BATTLE_ACTOR_TYPE_MONSTER ||
        target.actor_type() != BATTLE_ACTOR_TYPE_MONSTER || !target.is_boss() ||
        source.actor_id() == target.actor_id()) {
        return;
    }
    const uint32_t guardMonsterId = createRequest.trial_context().guard_monster_table_id();
    if (guardMonsterId == 0 || source.monster_table_id() == guardMonsterId) {
        bossGuardHealTotal = SaturatingAdd(bossGuardHealTotal, healed);
    }
}

void TurnBattleEngine::RecordManualActionForActor(const BattleActorState& actor) {
    uint64_t ownerId = 0;
    if (actor.actor_type() == BATTLE_ACTOR_TYPE_PLAYER) {
        ownerId = actor.actor_id();
    } else if (actor.actor_type() == BATTLE_ACTOR_TYPE_PET) {
        ownerId = actor.owner_player_id();
    }
    if (ownerId != 0) {
        manualActionRoundsByPlayer[ownerId].insert(roundIndex);
        manualActionPlayers.insert(ownerId);
        enteredPlayers.insert(ownerId);
    }
}

void TurnBattleEngine::RecordEffectiveContribution(const BattleActorState& actor) {
    uint64_t ownerId = 0;
    if (actor.actor_type() == BATTLE_ACTOR_TYPE_PLAYER) {
        ownerId = actor.actor_id();
    } else if (actor.actor_type() == BATTLE_ACTOR_TYPE_PET) {
        ownerId = actor.owner_player_id();
    }
    if (ownerId != 0) {
        effectiveContributionPlayers.insert(ownerId);
    }
}

bool TurnBattleEngine::IsHardControlBuff(uint32_t buffTableId) const {
    if (buffTableId == 0 || dataProvider == nullptr) {
        return false;
    }
    const auto* row = dataProvider->FindBuff(buffTableId);
    if (row == nullptr) {
        return false;
    }
    if (row->buff_type() == kBuffTypeStun ||
        row->buff_type() == kBuffTypeSilence ||
        row->buff_type() == kBuffTypeFreeze) {
        return true;
    }
    if (row->battle_op() == static_cast<uint32_t>(BattleEffectOp::Control)) {
        const uint32_t sk = row->state_kind();
        return sk >= static_cast<uint32_t>(BattleStateKind::SealActive) &&
               sk <= static_cast<uint32_t>(BattleStateKind::Freeze);
    }
    return false;
}

void TurnBattleEngine::RecordDispelLanded(uint64_t casterActorId, uint32_t count) {
    if (casterActorId == 0 || count == 0) {
        return;
    }
    const auto* caster = FindActor(casterActorId);
    if (caster == nullptr) {
        return;
    }
    uint64_t ownerId = 0;
    if (caster->actor_type() == BATTLE_ACTOR_TYPE_PLAYER) {
        ownerId = caster->actor_id();
    } else if (caster->actor_type() == BATTLE_ACTOR_TYPE_PET) {
        ownerId = caster->owner_player_id();
    }
    if (ownerId != 0) {
        totalDispelsLandedByPlayer[ownerId] += count;
    }
}

void TurnBattleEngine::AccumulateRoundMvpMetrics(const TurnResultS2C& result) {
    peakEscalationStacks = std::max(peakEscalationStacks, EscalationStacks());
    auto resolveOwner = [](const BattleActorState* actor) -> uint64_t {
        if (actor == nullptr) {
            return 0;
        }
        if (actor->actor_type() == BATTLE_ACTOR_TYPE_PLAYER) {
            return actor->actor_id();
        }
        if (actor->actor_type() == BATTLE_ACTOR_TYPE_PET) {
            return actor->owner_player_id();
        }
        return 0;
    };
    auto noteLowHp = [&](const BattleActorState* target, uint64_t hpAfter) {
        if (target != nullptr && target->actor_type() == BATTLE_ACTOR_TYPE_PLAYER &&
            hpAfter > 0 && target->max_health() > 0 &&
            hpAfter <= UINT64_MAX / kClutchComebackLowHpDenominator &&
            hpAfter * kClutchComebackLowHpDenominator < target->max_health()) {
            lowHpExperiencedPlayers.insert(target->actor_id());
        }
    };
    for (const auto& ev : result.events()) {
        const auto* source = FindActor(ev.source_id());
        const auto* target = FindActor(ev.target_id());
        const uint64_t ownerId = resolveOwner(source);
        const uint64_t targetOwnerId = resolveOwner(target);
        switch (ev.event_type()) {
            case BATTLE_EVENT_DAMAGE: {
                const uint64_t dealt = SaturatingAdd(ev.value(), ev.absorbed());
                if (dealt > 0) {
                    if (ownerId != 0 && source != nullptr &&
                        (target == nullptr || target->team_index() != source->team_index())) {
                        totalDamageDealtByPlayer[ownerId] =
                            SaturatingAdd(totalDamageDealtByPlayer[ownerId], dealt);
                        if (ev.is_primary_focus()) {
                            primaryFocusDamageByPlayer[ownerId] =
                                SaturatingAdd(primaryFocusDamageByPlayer[ownerId], dealt);
                        }
                    }
                    if (targetOwnerId != 0 && target != nullptr &&
                        (source == nullptr || target->team_index() != source->team_index())) {
                        damageTakenByPlayer[targetOwnerId] =
                            SaturatingAdd(damageTakenByPlayer[targetOwnerId], dealt);
                    }
                }
                noteLowHp(target, ev.target_health_after());
                break;
            }
            case BATTLE_EVENT_HEAL:
                if (ownerId != 0 && source != nullptr && ev.value() > 0 &&
                    (target == nullptr || target->team_index() == source->team_index())) {
                    totalHealingDoneByPlayer[ownerId] =
                        SaturatingAdd(totalHealingDoneByPlayer[ownerId], ev.value());
                }
                break;
            case BATTLE_EVENT_SHIELD:
                if (ownerId != 0 && source != nullptr && ev.value() > 0 &&
                    (target == nullptr || target->team_index() == source->team_index())) {
                    totalHealingDoneByPlayer[ownerId] =
                        SaturatingAdd(totalHealingDoneByPlayer[ownerId], ev.value());
                    totalShieldGrantedByPlayer[ownerId] =
                        SaturatingAdd(totalShieldGrantedByPlayer[ownerId], ev.value());
                }
                break;
            case BATTLE_EVENT_BUFF_TICK:
                if (ev.value() > 0 && target != nullptr) {
                    if (source == nullptr || target->team_index() != source->team_index()) {
                        if (ownerId != 0) {
                            totalDamageDealtByPlayer[ownerId] =
                                SaturatingAdd(totalDamageDealtByPlayer[ownerId], ev.value());
                        }
                        if (targetOwnerId != 0) {
                            damageTakenByPlayer[targetOwnerId] =
                                SaturatingAdd(damageTakenByPlayer[targetOwnerId], ev.value());
                        }
                        noteLowHp(target, ev.target_health_after());
                    } else if (ownerId != 0) {
                        totalHealingDoneByPlayer[ownerId] =
                            SaturatingAdd(totalHealingDoneByPlayer[ownerId], ev.value());
                    }
                }
                break;
            case BATTLE_EVENT_BUFF_ADD:
                if (ownerId != 0 && source != nullptr && ev.buff_table_id() != 0 && target != nullptr &&
                    target->team_index() != source->team_index() &&
                    IsHardControlBuff(ev.buff_table_id())) {
                    ++totalControlsLandedByPlayer[ownerId];
                }
                break;
            default:
                break;
        }
    }
}

BattleHighlightKind TurnBattleEngine::ComputeHighlightKind(uint64_t playerId, uint32_t teamIndex,
                                                           bool playerWon, bool isDead) const {
    if (playerWon &&
        (peakEscalationStacks >= kClutchComebackMinEscalationStacks ||
         (!isDead && lowHpExperiencedPlayers.contains(playerId)))) {
        return BATTLE_HIGHLIGHT_CLUTCH_COMEBACK;
    }

    auto readU64 = [](const std::map<uint64_t, uint64_t>& m, uint64_t id) -> uint64_t {
        const auto it = m.find(id);
        return it != m.end() ? it->second : 0ULL;
    };
    auto readCtrlPlusDispel = [&](uint64_t id) -> uint32_t {
        uint32_t total = 0;
        if (const auto it = totalControlsLandedByPlayer.find(id); it != totalControlsLandedByPlayer.end()) {
            total += it->second;
        }
        if (const auto it = totalDispelsLandedByPlayer.find(id); it != totalDispelsLandedByPlayer.end()) {
            total += it->second;
        }
        return total;
    };

    uint64_t maxFocus = 0;
    uint32_t maxCtrl = 0;
    uint64_t maxSupport = 0;
    uint64_t maxTaken = 0;
    for (const auto& member : actors) {
        if (member.actor_type() != BATTLE_ACTOR_TYPE_PLAYER || member.team_index() != teamIndex) {
            continue;
        }
        const uint64_t pid = member.actor_id();
        maxFocus = std::max(maxFocus, readU64(primaryFocusDamageByPlayer, pid));
        maxCtrl = std::max(maxCtrl, readCtrlPlusDispel(pid));
        maxSupport = std::max(maxSupport, readU64(totalHealingDoneByPlayer, pid));
        maxTaken = std::max(maxTaken, readU64(damageTakenByPlayer, pid));
    }

    const uint64_t selfFocus = readU64(primaryFocusDamageByPlayer, playerId);
    if (selfFocus > 0 && selfFocus == maxFocus) {
        return BATTLE_HIGHLIGHT_PRIMARY_FOCUS;
    }
    const uint32_t selfCtrl = readCtrlPlusDispel(playerId);
    if (selfCtrl >= kHighlightControlMinCount && selfCtrl == maxCtrl) {
        return BATTLE_HIGHLIGHT_TACTICAL_CONTROL;
    }
    const uint64_t selfSupport = readU64(totalHealingDoneByPlayer, playerId);
    if (selfSupport > 0 && selfSupport == maxSupport) {
        return BATTLE_HIGHLIGHT_SAVIOR_SUPPORT;
    }
    const uint64_t selfTaken = readU64(damageTakenByPlayer, playerId);
    if (selfTaken > 0 && selfTaken == maxTaken) {
        return BATTLE_HIGHLIGHT_IRON_GUARD;
    }
    return BATTLE_HIGHLIGHT_NONE;
}

bool TurnBattleEngine::HasActiveEncounterDamageModifiers() const {
    return activePolarity != 0 || IsBossTrialKind(bossTrialKind) ||
           (hasTowerContext && towerSigilTotals.singleTargetDamagePct > 0u);
}

void TurnBattleEngine::EmitBossMechanicEvent(TurnResultS2C& result, uint32_t mechanicKind,
                                             uint64_t sourceId, uint64_t targetId, uint64_t value) {
    auto* ev = AppendEvent(result, BATTLE_EVENT_BOSS_MECHANIC, sourceId, targetId);
    ev->set_mechanic_kind(mechanicKind);
    ev->set_value(value);
}

void TurnBattleEngine::ApplyBossExposed(uint64_t actorId, uint32_t pct, uint32_t untilRound) {
    if (actorId == 0 || pct == 0) {
        return;
    }
    auto& state = exposedActors[actorId];
    // V2 防线：同目标同一时刻仅保留 1 份破绽，高值覆盖低值、绝不叠乘，同值仅续时至更晚回合末。
    if (pct > state.pct) {
        state.pct = pct;
        state.untilRound = std::max(state.untilRound, untilRound);
    } else if (pct == state.pct) {
        state.untilRound = std::max(state.untilRound, untilRound);
    } else {
        state.untilRound = std::max(state.untilRound, untilRound);
    }
}

bool TurnBattleEngine::ActorHasBossCleaveParryProtection(const BattleActorState& target,
                                                         uint32_t damageType) const {
    if (ActorHasBuffOfType(target, kBuffTypeInvincibility)) {
        return true;
    }
    for (const auto& entry : target.buffs()) {
        const auto* row = dataProvider->FindBuff(entry.buff_table_id());
        if (row == nullptr || row->battle_op() == 0) {
            continue;
        }
        const uint32_t filter = row->damage_filter();
        const bool typeMatches =
            filter == 0 ||
            (filter == 1 && damageType == combatdamage::kPhysicalDamage) ||
            (filter == 2 && damageType == combatdamage::kMagicDamage);
        if (!typeMatches) {
            continue;
        }
        const auto op = static_cast<BattleEffectOp>(row->battle_op());
        if (op == BattleEffectOp::Shield || op == BattleEffectOp::ManaShield ||
            op == BattleEffectOp::ShareDamage || op == BattleEffectOp::ReflectDamage) {
            return true;
        }
    }
    return false;
}

void TurnBattleEngine::TriggerRejuvBacklash(uint64_t sourceId, BattleActorState& boss, TurnResultS2C& result) {
    if (!IsActorActive(boss)) {
        return;
    }
    const uint64_t rawBacklash =
        std::max<uint64_t>(1ULL, (boss.max_health() * kBossHealBacklashPct) / 100ULL);
    const uint64_t dealt = std::min(boss.attributes().health(), rawBacklash);
    if (dealt > 0) {
        boss.mutable_attributes()->set_health(boss.attributes().health() - dealt);
    }
    ++healInterruptBacklashCount;
    ApplyBossExposed(boss.actor_id(), kBossExposedFullPct, roundIndex + 1u);
    EmitBossMechanicEvent(result, kBossMechanicHealBacklash, sourceId, boss.actor_id(), dealt);
    auto* dmgEv = AppendEvent(result, BATTLE_EVENT_DAMAGE, sourceId, boss.actor_id());
    dmgEv->set_value(dealt);
    dmgEv->set_target_health_after(boss.attributes().health());
    dmgEv->set_target_mana_after(boss.attributes().mana());
    if (boss.attributes().health() == 0) {
        HandleDeath(boss, sourceId, result);
    }
}

void TurnBattleEngine::HandleBossTrialMonsterDeath(BattleActorState& deadMonster, TurnResultS2C& result) {
    if (deadMonster.is_boss()) {
        return;
    }
    BattleActorState* boss = nullptr;
    for (auto& actor : actors) {
        if (actor.actor_type() == BATTLE_ACTOR_TYPE_MONSTER && actor.team_index() == 1 &&
            actor.is_boss() && IsActorActive(actor)) {
            boss = &actor;
            break;
        }
    }

    // 1. 护势护卫阵亡：联防阵自然瓦解。
    if (IsBossStanceTrialKind(bossTrialKind) && bossStanceType != 0 &&
        (deadMonster.monster_table_id() == kBossTrialMonsterGuardStance ||
         (bossTrialKind == kBossTrialKindBreakStance &&
          deadMonster.monster_table_id() != kBossTrialMonsterSupportBreaker &&
          deadMonster.monster_table_id() != kBossTrialMonsterSupportDrummer))) {
        bossStanceType = 0;
        bossStanceRemainRounds = 0;
    }

    // 2. 回春护卫在已发出治疗预告后、兑现前被击杀：立即触发断疗反噬（6% 首领血 + 破绽）并移除预告防重复触发。
    bool cancelledPendingHeal = false;
    for (auto it = activeTelegraphs.begin(); it != activeTelegraphs.end(); ) {
        if (it->source_actor_id() == deadMonster.actor_id() &&
            it->intent_kind() == kTelegraphKindGuardHeal &&
            it->execute_round() >= roundIndex) {
            BattleTelegraphEntry resolved = *it;
            resolved.set_status(kTelegraphStatusCancelledByControl);
            resolved.set_state_version(++telegraphStateVersion);
            resolved.set_label("回春护卫阵亡，治疗中断触发灵力反噬");
            *result.add_resolved_telegraphs() = resolved;
            lastResolvedTelegraphs.push_back(std::move(resolved));
            it = activeTelegraphs.erase(it);
            cancelledPendingHeal = true;
        } else {
            ++it;
        }
    }
    if (cancelledPendingHeal && boss != nullptr && IsActorActive(*boss)) {
        TriggerRejuvBacklash(deadMonster.actor_id(), *boss, result);
    }

    // 3. 策应卫阵亡：破军卫清 1 层蓄势；鼓噪卫清全部蓄势并对首领造成 4% 震荡反噬。
    if (deadMonster.monster_table_id() == kBossTrialMonsterSupportBreaker && boss != nullptr && IsActorActive(*boss)) {
        if (bossMomentumStacks > 0) {
            --bossMomentumStacks;
            EmitBossMechanicEvent(result, kBossMechanicSupportShock, deadMonster.actor_id(), boss->actor_id(), 1ULL);
        }
    } else if (deadMonster.monster_table_id() == kBossTrialMonsterSupportDrummer && boss != nullptr &&
               IsActorActive(*boss)) {
        bossMomentumStacks = 0;
        const uint64_t rawShock =
            std::max<uint64_t>(1ULL, (boss->max_health() * kBossDrummerDeathShockPct) / 100ULL);
        const uint64_t dealt = std::min(boss->attributes().health(), rawShock);
        if (dealt > 0) {
            boss->mutable_attributes()->set_health(boss->attributes().health() - dealt);
        }
        EmitBossMechanicEvent(result, kBossMechanicSupportShock, deadMonster.actor_id(), boss->actor_id(), dealt);
        auto* dmgEv = AppendEvent(result, BATTLE_EVENT_DAMAGE, deadMonster.actor_id(), boss->actor_id());
        dmgEv->set_value(dealt);
        dmgEv->set_target_health_after(boss->attributes().health());
        dmgEv->set_target_mana_after(boss->attributes().mana());
        if (boss->attributes().health() == 0) {
            HandleDeath(*boss, deadMonster.actor_id(), result);
        }
    }
}

void TurnBattleEngine::ProcessBossTrialRoundEnd(TurnResultS2C& result) {
    BattleActorState* boss = nullptr;
    bool anySpecialGuardAlive = false;
    for (auto& actor : actors) {
        if (actor.actor_type() != BATTLE_ACTOR_TYPE_MONSTER || actor.team_index() != 1 || !IsActorActive(actor)) {
            continue;
        }
        if (actor.is_boss()) {
            boss = &actor;
        } else {
            const uint32_t mid = actor.monster_table_id();
            if (mid != kBossTrialMonsterSupportBreaker && mid != kBossTrialMonsterSupportDrummer) {
                anySpecialGuardAlive = true;
            }
        }
    }
    if (boss == nullptr || !IsActorActive(*boss)) {
        return;
    }

    // 1. 存活且未受控的鼓噪卫回合末为首领鼓舞 +1 蓄势（至多 3 层）。
    for (const auto& actor : actors) {
        if (actor.actor_type() == BATTLE_ACTOR_TYPE_MONSTER && actor.team_index() == 1 &&
            actor.monster_table_id() == kBossTrialMonsterSupportDrummer && IsActorActive(actor) &&
            !IsActorControlLocked(actor)) {
            if (bossMomentumStacks < kBossMomentumMaxStacks) {
                ++bossMomentumStacks;
                EmitBossMechanicEvent(result, kBossMechanicMomentumGain, actor.actor_id(),
                                      boss->actor_id(), bossMomentumStacks);
            }
        }
    }

    // 2. 护势联防持续回合扣减（3 回合含施放当轮）。
    if (bossStanceRemainRounds > 0) {
        --bossStanceRemainRounds;
        if (bossStanceRemainRounds == 0) {
            bossStanceType = 0;
        }
    }

    // 2.5 首领试炼周常五行词缀回合末反向考验（仅真实首领试炼生效，零时钟确定性）。
    if (bossWeeklyAffixId == bossaffix::kAffixWood && bossaffix::IsWoodRegenRound(roundIndex) &&
        !IsActorControlLocked(*boss)) {
        const double incomingHealMod = ConfiguredModifier(*boss, BattleModifier::IncomingHeal);
        if (incomingHealMod >= 0.0 && boss->max_health() > 0 && boss->attributes().health() < boss->max_health()) {
            const uint64_t rawHeal =
                std::max<uint64_t>(1ULL, (boss->max_health() * bossaffix::kWoodBossRegenHpPct) / 100ULL);
            const uint64_t healed = ApplyHeal(*boss, static_cast<double>(rawHeal));
            if (healed > 0) {
                auto* healEv = AppendEvent(result, BATTLE_EVENT_HEAL, boss->actor_id(), boss->actor_id());
                healEv->set_value(healed);
                healEv->set_target_health_after(boss->attributes().health());
                healEv->set_target_mana_after(boss->attributes().mana());
                EmitBossMechanicEvent(result, kBossMechanicWeeklyAffixTrigger, boss->actor_id(),
                                      boss->actor_id(), healed);
            }
        }
    } else if (bossWeeklyAffixId == bossaffix::kAffixWater && bossaffix::IsWaterMomentumRound(roundIndex) &&
               !IsActorControlLocked(*boss) && bossMomentumStacks < kBossMomentumMaxStacks) {
        ++bossMomentumStacks;
        EmitBossMechanicEvent(result, kBossMechanicMomentumGain, boss->actor_id(),
                              boss->actor_id(), bossMomentumStacks);
    }

    // 3. 二阶段「决战姿态（破釜）」：全部特殊护卫阵亡或首领气血 <= 50%（火周提前至 <= 60%），整场仅触发一次且绝不回合中途连劈（V7）。
    const bool phaseTwoHpReached = bossWeeklyAffixId == bossaffix::kAffixFire
        ? bossaffix::IsHealthAtOrBelowPct(boss->attributes().health(), boss->max_health(),
                                          bossaffix::kFireBossPhaseTwoHpPct)
        : (boss->attributes().health() * 2ULL <= boss->max_health());
    if (bossPhase < 2u && (!anySpecialGuardAlive || phaseTwoHpReached)) {
        bossPhase = 2u;
        bool hasNextRoundCleave = false;
        for (const auto& entry : activeTelegraphs) {
            if (entry.source_actor_id() == boss->actor_id() && entry.execute_round() == roundIndex + 1u) {
                hasNextRoundCleave = true;
                break;
            }
        }
        nextPhase2CleaveTelegraphRound = hasNextRoundCleave ? (roundIndex + 2u) : (roundIndex + 1u);
        EmitBossMechanicEvent(result, kBossMechanicPhaseTwoEnrage, boss->actor_id(), boss->actor_id(), 2ULL);
    }
}

uint32_t TurnBattleEngine::ChooseDynamicStanceType(uint32_t currentRound) const {
    if (currentRound <= 1u) {
        return kBossStancePhysical;
    }
    if (battleDifficulty < 2u) {
        return (((currentRound - 1u) / 3u) % 2u == 0u)
            ? kBossStancePhysical
            : kBossStanceMagic;
    }
    // 困难·护势进阶 / 双卫同阵「顺势应变」：统计前 2 个完整回合玩家方物理/法术直接伤害总量，针对高侧开阵；平票确定性反切。
    uint64_t physTotal = 0;
    uint64_t magicTotal = 0;
    for (uint32_t r = (currentRound > 2u ? currentRound - 2u : 1u); r < currentRound; ++r) {
        if (const auto it = roundPlayerDirectDamage.find(r); it != roundPlayerDirectDamage.end()) {
            physTotal = SaturatingAdd(physTotal, it->second.first);
            magicTotal = SaturatingAdd(magicTotal, it->second.second);
        }
    }
    if (physTotal > magicTotal) {
        return kBossStancePhysical;
    }
    if (magicTotal > physTotal) {
        return kBossStanceMagic;
    }
    return lastChosenStanceType == kBossStancePhysical
        ? kBossStanceMagic
        : kBossStancePhysical;
}

uint64_t TurnBattleEngine::ApplyEncounterDamageModifiers(const BattleActorState& source, BattleActorState& target,
                                                         uint64_t damage, uint32_t damageType, bool isDirectPrimary,
                                                         TurnResultS2C& result, bool isMultiTargetSkill,
                                                         bool isPrimaryTarget, bool* outSplashReduced,
                                                         bool isWeeklyChargedOrPeriodic, bool isPursuit) {
    if (outSplashReduced != nullptr) {
        *outSplashReduced = false;
    }
    if (damage == 0) {
        return 0;
    }
    const uint32_t dmgStancePolarity = damageType == combatdamage::kPhysicalDamage
        ? kBossStancePhysical
        : (damageType == combatdamage::kMagicDamage ? kBossStanceMagic : kBossStanceNone);
    // 1. 物法易势 5V5 / 自定义切磋物法易势：与当前易势同类型的直接主伤害 ×1.15（常规减伤/防御后、护盾吸收前，整数向下取整一次）。
    if (isDirectPrimary && activePolarity != 0 && activePolarity == damageType) {
        damage = (damage * kPolarityDamageNum) / kPolarityDamageDen;
    }
    // 1.5 五行幻境火系灵品符印（402 焚心）：单体直伤额外 +12%。
    if (isDirectPrimary && !isMultiTargetSkill && hasTowerContext && source.team_index() == 0 &&
        towerSigilTotals.singleTargetDamagePct > 0u) {
        damage = (damage * (100ULL + towerSigilTotals.singleTargetDamagePct)) / 100ULL;
    }
    // 1.8 卷四·三境明雷野外首领机制（碧渊水宫·阳钟阴鼓 / 月汐湾·涨退潮，纯整数定点数倍率）。
    if (isDirectPrimary && bossTrialKind == kBossTrialKindWildBell &&
        source.team_index() == 0 && target.team_index() == 1) {
        const bool yangBell = IsWildBellYangRound(roundIndex);
        if ((yangBell && damageType == combatdamage::kPhysicalDamage) ||
            (!yangBell && damageType == combatdamage::kMagicDamage)) {
            damage = (damage * 115ULL) / 100ULL;
        } else if (damageType == combatdamage::kPhysicalDamage || damageType == combatdamage::kMagicDamage) {
            damage = (damage * 88ULL) / 100ULL;
        }
    }
    if (bossTrialKind == kBossTrialKindWildTide && source.team_index() == 0 &&
        target.team_index() == 1 && target.is_boss()) {
        damage = IsWildTideEbbRound(roundIndex)
            ? (damage * 120ULL) / 100ULL
            : (damage * 75ULL) / 100ULL;
    }
    // 2. 首领试炼 V2 阵型「主目标聚火 100% / 多目标直伤副目标溅射 60%」（仅限试炼敌方，零侵入常规 PVE/PVP，V6）。
    if (isDirectPrimary && IsBossTrialKind(bossTrialKind) && source.team_index() == 0 && target.team_index() == 1 &&
        isMultiTargetSkill && !isPrimaryTarget) {
        damage = (damage * kBossTrialSplashKeepPct) / 100ULL;
        if (outSplashReduced != nullptr) {
            *outSplashReduced = true;
        }
    }
    // 3. 破阵·护势联防阵（单卫与双卫同阵）：庇护全体存活敌方，同时对同系直伤触发轻反制叠「蓄势」（毒火不触发）。
    if (isDirectPrimary && IsBossStanceTrialKind(bossTrialKind) && target.team_index() == 1 &&
        bossStanceType != kBossStanceNone && bossStanceRemainRounds > 0 && bossStanceType == dmgStancePolarity) {
        const uint64_t keepPct = (bossTrialKind == kBossTrialKindBreakDualGuard)
            ? 65ULL
            : (battleDifficulty >= 2u ? 60ULL : 65ULL);
        damage = (damage * keepPct) / 100ULL;
        if (source.team_index() == 0 && !attackerTriggeredStanceCounterThisRound.contains(source.actor_id())) {
            attackerTriggeredStanceCounterThisRound.insert(source.actor_id());
            if (bossMomentumStacks < kBossMomentumMaxStacks) {
                ++bossMomentumStacks;
                uint64_t bossId = target.actor_id();
                for (const auto& a : actors) {
                    if (a.actor_type() == BATTLE_ACTOR_TYPE_MONSTER && a.team_index() == 1 &&
                        a.is_boss() && IsActorActive(a)) {
                        bossId = a.actor_id();
                        break;
                    }
                }
                EmitBossMechanicEvent(result, kBossMechanicMomentumGain, target.actor_id(), bossId, bossMomentumStacks);
            }
        }
    }
    // 4. 首领试炼 V2 统一「破绽 (+35%/+20%)」+ 二阶段「决战姿态 (+15%)」加算易伤（覆盖直接伤害与持续毒火，V2）。
    if (IsBossTrialKind(bossTrialKind) && source.team_index() != target.team_index() && target.team_index() == 1) {
        uint32_t vulnPct = 0;
        if (const auto it = exposedActors.find(target.actor_id());
            it != exposedActors.end() && it->second.untilRound >= roundIndex) {
            vulnPct += it->second.pct;
        }
        if (target.is_boss() && bossPhase >= 2u) {
            vulnPct += kBossPhaseTwoVulnPct;
        }
        if (vulnPct > 0) {
            damage = (damage * (100ULL + vulnPct)) / 100ULL;
        }
    }
    // 4.5 首领试炼周常五行词缀正向增伤与破绽灼伤（仅真实首领试炼生效，整数定点数倍率）。
    if (bossWeeklyAffixId != bossaffix::kAffixNone && source.team_index() == 0 && target.team_index() == 1) {
        switch (bossWeeklyAffixId) {
        case bossaffix::kAffixMetal:
            if (isDirectPrimary && isPrimaryTarget && IsActorExposed(target.actor_id())) {
                damage = (damage * (100ULL + bossaffix::kMetalExposedPrimaryBonusPct)) / 100ULL;
            }
            break;
        case bossaffix::kAffixWood:
            if (weeklyAffixWoodTeamBoostUntilRound >= roundIndex) {
                damage = (damage * (100ULL + bossaffix::kWoodTeamDamageBonusPct)) / 100ULL;
            }
            break;
        case bossaffix::kAffixWater:
            if (isWeeklyChargedOrPeriodic) {
                damage = (damage * (100ULL + bossaffix::kWaterChargeOrPeriodicDamageBonusPct)) / 100ULL;
            }
            break;
        case bossaffix::kAffixFire:
            if ((source.actor_type() == BATTLE_ACTOR_TYPE_PET && isDirectPrimary) || isPursuit) {
                damage = (damage * (100ULL + bossaffix::kFirePetOrFollowUpDamageBonusPct)) / 100ULL;
            }
            if (isDirectPrimary && IsActorExposed(target.actor_id()) && target.max_health() > 0) {
                const uint64_t rawBurn =
                    std::max<uint64_t>(1ULL, (target.max_health() * bossaffix::kFireExposedBurnMaxHpPct) / 100ULL);
                damage = SaturatingAdd(damage, std::min(rawBurn, bossaffix::kFireExposedBurnCap));
            }
            break;
        case bossaffix::kAffixEarth:
            if (weeklyAffixEarthTeamBoostUntilRound >= roundIndex) {
                damage = (damage * (100ULL + bossaffix::kEarthParryNextRoundDamageBonusPct)) / 100ULL;
            }
            break;
        default:
            break;
        }
    }
    // 5. 灵障首领：灵障护盾在生命扣血之前优先吸收伤害，击破时记录破盾者与有效贡献。
    if (bossTrialKind == kBossTrialKindBarrier && target.is_boss() && bossBarrierShield > 0 && damage > 0) {
        const uint64_t absorbed = std::min<uint64_t>(damage, bossBarrierShield);
        bossBarrierShield -= absorbed;
        damage -= absorbed;
        if (absorbed > 0) {
            RecordTrialDirectDamage(source, target, damageType, absorbed);
            RecordEffectiveContribution(source);
            auto* shieldEvent = AppendEvent(result, BATTLE_EVENT_SHIELD, target.actor_id(), target.actor_id());
            shieldEvent->set_absorbed(absorbed);
            shieldEvent->set_shield_after(bossBarrierShield);
            if (bossBarrierShield == 0) {
                if (source.actor_type() == BATTLE_ACTOR_TYPE_PLAYER) {
                    shieldBreakPlayers.insert(source.actor_id());
                } else if (source.actor_type() == BATTLE_ACTOR_TYPE_PET && source.owner_player_id() != 0) {
                    shieldBreakPlayers.insert(source.owner_player_id());
                }
                // 灵障碎障破绽：击破首领灵障护盾立即触发完整「破绽（+35%，至下回合末）」，将破盾转化为全队爆发窗口。
                ApplyBossExposed(target.actor_id(), kBossExposedFullPct, roundIndex + 1u);
                ++stanceBreakCount;
                EmitBossMechanicEvent(result, kBossMechanicStanceBreak, source.actor_id(),
                                      target.actor_id(), kBossExposedFullPct);
            }
        }
    }
    return damage;
}

bool TurnBattleEngine::IsActorControlLocked(const BattleActorState& actor) const {
    return CheckState(actor) != kSuccess ||
        ActorHasBuffOfType(actor, kBuffTypeStun) ||
        ActorHasBuffOfType(actor, kBuffTypeFreeze) ||
        ActorHasBuffOfType(actor, kBuffTypeSilence) ||
        HasConfiguredState(actor, BattleStateKind::Freeze) ||
        HasConfiguredState(actor, BattleStateKind::Sleep) ||
        HasConfiguredState(actor, BattleStateKind::Confusion) ||
        HasConfiguredState(actor, BattleStateKind::SealActive) ||
        HasConfiguredState(actor, BattleStateKind::SealMagic) ||
        HasConfiguredState(actor, BattleStateKind::SealPhysical);
}

bool TurnBattleEngine::IsFestivalBeastBattle() const {
    return bossTrialKind == kBossTrialKindFestivalBeast &&
        createRequest.has_activity_context() &&
        createRequest.activity_context().kind() == BATTLE_ACTIVITY_KIND_FESTIVAL_BEAST &&
        createRequest.activity_context().has_festival_beast();
}

bool TurnBattleEngine::IsEncounterSlowPhaseActor(uint64_t actorId) const {
    for (const auto& entry : activeTelegraphs) {
        if (entry.execute_round() == roundIndex && entry.source_actor_id() == actorId) {
            return true;
        }
    }
    return false;
}

void TurnBattleEngine::InitSpecialEncounterState() {
    if (bossTrialKind == kBossTrialKindNone && !HasCustomDuelPolarity()) {
        return;
    }
    if (bossTrialKind == kBossTrialKindPolarity5v5 || createRequest.match_mode() == kMatchModePolarity5v5 ||
        HasCustomDuelPolarity()) {
        initialPolarity = (RandIndex(2) == 0) ? combatdamage::kPhysicalDamage : combatdamage::kMagicDamage;
        UpdatePolarityForRound();
        return;
    }
    RefreshEncounterTelegraphs();
}

void TurnBattleEngine::UpdatePolarityForRound() {
    if (bossTrialKind != kBossTrialKindPolarity5v5 && createRequest.match_mode() != kMatchModePolarity5v5 &&
        !HasCustomDuelPolarity()) {
        activePolarity = 0;
        return;
    }
    if (initialPolarity == 0) {
        initialPolarity = combatdamage::kPhysicalDamage;
    }
    const uint32_t block = (roundIndex > 0 ? (roundIndex - 1) : 0) / 2;
    activePolarity = (block % 2 == 0)
        ? initialPolarity
        : (initialPolarity == combatdamage::kPhysicalDamage ? combatdamage::kMagicDamage
                                                            : combatdamage::kPhysicalDamage);
}

uint32_t TurnBattleEngine::NextPolaritySwitchRound() const {
    if (activePolarity == 0) {
        return 0;
    }
    const uint32_t nextSwitch = ((roundIndex > 0 ? (roundIndex - 1) : 0) / 2 + 1) * 2 + 1;
    return nextSwitch <= maxRounds ? nextSwitch : 0;
}

void TurnBattleEngine::RefreshEncounterTelegraphs() {
    // 清理已经过期或属于更早回合的预告，保留本回合待执行的预告，并在预告回合生成下一回合的预告。
    std::vector<BattleTelegraphEntry> kept;
    for (auto entry : activeTelegraphs) {
        if (entry.execute_round() >= roundIndex) {
            const uint32_t nextStatus = entry.execute_round() == roundIndex
                                            ? kTelegraphStatusExecuting
                                            : kTelegraphStatusUpcoming;
            if (entry.status() != nextStatus) {
                entry.set_status(nextStatus);
                entry.set_state_version(++telegraphStateVersion);
            }
            kept.push_back(std::move(entry));
        }
    }
    activeTelegraphs = std::move(kept);

    const bool isFestivalBeast = IsFestivalBeastBattle();
    if (!isFestivalBeast && !IsBossTrialOrRescueKind(bossTrialKind)) {
        return;
    }

    const BattleActorState* boss = nullptr;
    const BattleActorState* rejuvGuard = nullptr;
    const BattleActorState* stanceGuard = nullptr;
    const BattleActorState* otherGuard = nullptr;
    for (const auto& actor : actors) {
        if (actor.actor_type() != BATTLE_ACTOR_TYPE_MONSTER || actor.team_index() != 1 || !IsActorActive(actor)) {
            continue;
        }
        if (actor.is_boss()) {
            boss = &actor;
            continue;
        }
        const uint32_t mid = actor.monster_table_id();
        if (mid == kBossTrialMonsterSupportBreaker || mid == kBossTrialMonsterSupportDrummer) {
            continue;
        }
        if (bossTrialKind == kBossTrialKindBreakDualGuard) {
            if (mid == kBossTrialMonsterGuardRejuv && rejuvGuard == nullptr) {
                rejuvGuard = &actor;
            } else if (mid == kBossTrialMonsterGuardStance && stanceGuard == nullptr) {
                stanceGuard = &actor;
            } else if (rejuvGuard == nullptr) {
                rejuvGuard = &actor;
            } else if (stanceGuard == nullptr) {
                stanceGuard = &actor;
            }
        } else if (bossTrialKind == kBossTrialKindBreakRejuv) {
            if (rejuvGuard == nullptr) rejuvGuard = &actor;
        } else if (bossTrialKind == kBossTrialKindBreakStance) {
            if (stanceGuard == nullptr) stanceGuard = &actor;
        } else {
            if (otherGuard == nullptr) otherGuard = &actor;
        }
    }

    const auto hasEntryFor = [&](uint64_t sourceId, uint32_t execRound) {
        for (const auto& e : activeTelegraphs) {
            if (e.source_actor_id() == sourceId && e.execute_round() == execRound) return true;
        }
        return false;
    };

    const auto pickEnemyTarget = [&](const BattleActorState& caster) -> uint64_t {
        const auto targets = CollectAliveEnemyIds(caster);
        if (targets.empty()) return 0;
        // 护卫救援：优先对受保护护卫或存活玩家确定性点名（偶数蓄力直接威胁护卫，促使玩家保卫/控场）。
        if (bossTrialKind == kBossTrialKindGuardianRescue && protectedGuardActorId != 0) {
            if (const auto* allyGuard = FindActor(protectedGuardActorId);
                allyGuard != nullptr && IsActorActive(*allyGuard)) {
                return protectedGuardActorId;
            }
        }
        return targets[static_cast<size_t>(RandIndex(targets.size()))];
    };

    const auto ownerOfActor = [&](uint64_t actorId) -> uint64_t {
        const auto* a = FindActor(actorId);
        if (a == nullptr) return 0;
        return a->actor_type() == BATTLE_ACTOR_TYPE_PET ? a->owner_player_id() : a->actor_id();
    };

    const auto pickCleaveTargets = [&](const BattleActorState& caster, bool dualTarget) -> std::vector<uint64_t> {
        const auto targets = CollectAliveEnemyIds(caster);
        if (targets.empty()) return {};
        const uint64_t first = pickEnemyTarget(caster);
        std::vector<uint64_t> chosen;
        if (first != 0) chosen.push_back(first);
        if (!dualTarget || targets.size() < 2 || chosen.empty()) {
            return chosen;
        }
        const uint64_t firstOwner = ownerOfActor(first);
        std::vector<uint64_t> diffOwnerPool;
        std::vector<uint64_t> fallbackPool;
        for (const uint64_t id : targets) {
            if (id == first) continue;
            if (firstOwner != 0 && ownerOfActor(id) != firstOwner) {
                diffOwnerPool.push_back(id);
            } else {
                fallbackPool.push_back(id);
            }
        }
        const auto& pool = !diffOwnerPool.empty() ? diffOwnerPool : fallbackPool;
        if (!pool.empty()) {
            chosen.push_back(pool[static_cast<size_t>(RandIndex(pool.size()))]);
        }
        return chosen;
    };

    // 0. 瑞兽迎春：在 R2/5/8/11 操作窗口开始前确定性预告下一回合（R3/6/9/12）重击目标，仅抽一次。
    if (isFestivalBeast) {
        const auto& fb = createRequest.activity_context().festival_beast();
        const uint32_t execRound = roundIndex + fb.telegraph_lead_rounds();
        if (boss != nullptr && execRound <= maxRounds && execRound >= fb.first_heavy_round() &&
            (execRound - fb.first_heavy_round()) % fb.cycle_rounds() == 0 &&
            !hasEntryFor(boss->actor_id(), execRound)) {
            const uint64_t targetId = pickEnemyTarget(*boss);
            if (targetId != 0) {
                BattleTelegraphEntry entry;
                entry.set_source_actor_id(boss->actor_id());
                entry.set_target_actor_id(targetId);
                entry.add_target_actor_ids(targetId);
                entry.set_telegraph_round(roundIndex);
                entry.set_execute_round(execRound);
                entry.set_intent_kind(kTelegraphKindFestivalHeavyStrike);
                entry.set_status(kTelegraphStatusUpcoming);
                entry.set_state_version(++telegraphStateVersion);
                entry.set_label("春禧重击蓄力：下回合慢速物理重击锁定目标，防御可减半、控场可打断");
                activeTelegraphs.push_back(std::move(entry));
            }
        }
        return;
    }

    // 1. 首领蓄力慢速重击：一阶段 R1/4/7/10/13 预告；二阶段决战姿态按 nextPhase2CleaveTelegraphRound 每 2 回合循环。
    const bool bossTelegraphDue = bossPhase >= 2u
        ? (nextPhase2CleaveTelegraphRound != 0u && roundIndex == nextPhase2CleaveTelegraphRound)
        : (roundIndex % 3u == 1u);
    if (boss != nullptr && bossTelegraphDue && roundIndex + 1u <= maxRounds &&
        !hasEntryFor(boss->actor_id(), roundIndex + 1u)) {
        BattleTelegraphEntry entry;
        entry.set_source_actor_id(boss->actor_id());
        entry.set_telegraph_round(roundIndex);
        entry.set_execute_round(roundIndex + 1u);
        entry.set_status(kTelegraphStatusUpcoming);
        entry.set_state_version(++telegraphStateVersion);
        if (bossTrialKind == kBossTrialKindBarrier) {
            const uint64_t tid = pickEnemyTarget(*boss);
            entry.set_intent_kind(kTelegraphKindBossMagicStrike);
            entry.set_target_actor_id(tid);
            if (tid != 0) entry.add_target_actor_ids(tid);
            entry.set_label("灵障重击：下回合慢速法术重击，防御或护罩化解可触发首领破绽");
        } else if (bossTrialKind == kBossTrialKindVenom) {
            entry.set_intent_kind(kTelegraphKindBossVenomAoe);
            entry.set_target_actor_id(0);
            entry.set_label("玄毒冲击：下回合慢速群体法术伤害，防御可减半、控场打断触发破绽");
        } else if (bossTrialKind == kBossTrialKindWildWind) {
            const bool dualTarget = initialAttackerHumanCount >= 3u;
            const auto targets = pickCleaveTargets(*boss, dualTarget);
            entry.set_intent_kind(kTelegraphKindWildWindGust);
            if (!targets.empty()) {
                entry.set_target_actor_id(targets.front());
                for (const uint64_t tid : targets) {
                    entry.add_target_actor_ids(tid);
                }
            }
            entry.set_label("风眼引风：下回合慢速风袭锁定目标，防御或护盾化解可免折损并触发首领破绽");
        } else {
            const bool dualTarget = IsBossTrialKind(bossTrialKind) && initialAttackerHumanCount >= 3u;
            const auto targets = pickCleaveTargets(*boss, dualTarget);
            entry.set_intent_kind(kTelegraphKindSlowCleave);
            if (!targets.empty()) {
                entry.set_target_actor_id(targets.front());
                for (const uint64_t tid : targets) {
                    entry.add_target_actor_ids(tid);
                }
            }
            entry.set_label(targets.size() >= 2u
                                ? "裂地双劈：下回合慢速重劈双目标，全员防御/护盾化解触发完整破绽"
                                : "蓄力重劈：下回合慢速物理重劈，防御/护盾/反震化解触发首领破绽");
        }
        activeTelegraphs.push_back(std::move(entry));
        if (bossPhase >= 2u) {
            nextPhase2CleaveTelegraphRound = roundIndex + 2u;
        }
    }

    // 2. 敌方护卫周期机制预告（支持单卫与双卫同阵独立并行预告）。
    if (boss != nullptr) {
        if (rejuvGuard != nullptr && IsBossRejuvTrialKind(bossTrialKind)) {
            // 普通回春与双卫同阵均为 R2/5/8 预告 -> R3/6/9 兑现（错峰防死锁）；困难单卫回春(B1)为 R1/4/7 -> R2/5/8 与重劈重叠。
            const uint32_t triggerMod =
                (bossTrialKind == kBossTrialKindBreakRejuv && battleDifficulty >= 2u) ? 1u : 2u;
            if (roundIndex % 3u == triggerMod && roundIndex + 1u <= maxRounds &&
                !hasEntryFor(rejuvGuard->actor_id(), roundIndex + 1u)) {
                BattleTelegraphEntry entry;
                entry.set_source_actor_id(rejuvGuard->actor_id());
                entry.set_target_actor_id(boss->actor_id());
                entry.add_target_actor_ids(boss->actor_id());
                entry.set_telegraph_round(roundIndex);
                entry.set_execute_round(roundIndex + 1u);
                entry.set_intent_kind(kTelegraphKindGuardHeal);
                entry.set_status(kTelegraphStatusUpcoming);
                entry.set_state_version(++telegraphStateVersion);
                entry.set_label(battleDifficulty >= 2u
                                    ? "回春祈愈（进阶）：下回合为首领回复10%血并净1减益；打断或击杀触发反噬破绽"
                                    : "回春祈愈：下回合为首领回复8%最大生命；控断或击杀触发反噬破绽");
                activeTelegraphs.push_back(std::move(entry));
            }
        }
        if (stanceGuard != nullptr && IsBossStanceTrialKind(bossTrialKind)) {
            // 护势护卫：R1/4/7/10/13 当回合展开联防阵；困难单卫/双卫按前2回合物法直伤动态「顺势应变」。
            if (roundIndex % 3u == 1u && !hasEntryFor(stanceGuard->actor_id(), roundIndex)) {
                const uint32_t chosenType = ChooseDynamicStanceType(roundIndex);
                const bool isPhysical = chosenType == kBossStancePhysical;
                BattleTelegraphEntry entry;
                entry.set_source_actor_id(stanceGuard->actor_id());
                entry.set_target_actor_id(boss->actor_id());
                entry.add_target_actor_ids(boss->actor_id());
                entry.set_telegraph_round(roundIndex);
                entry.set_execute_round(roundIndex);
                entry.set_intent_kind(isPhysical ? kTelegraphKindGuardStancePhys : kTelegraphKindGuardStanceMagic);
                entry.set_status(kTelegraphStatusExecuting);
                entry.set_state_version(++telegraphStateVersion);
                entry.set_label(isPhysical
                                    ? "物防联防阵：本回合展开全体物理减伤阵（可驱散崩势，物理直伤叠蓄势）"
                                    : "法防联防阵：本回合展开全体法术减伤阵（可驱散崩势，法术直伤叠蓄势）");
                activeTelegraphs.push_back(std::move(entry));
            }
        }
        if (otherGuard != nullptr && bossTrialKind == kBossTrialKindBarrier) {
            if (roundIndex % 3u == 1u && !hasEntryFor(otherGuard->actor_id(), roundIndex)) {
                BattleTelegraphEntry entry;
                entry.set_source_actor_id(otherGuard->actor_id());
                entry.set_target_actor_id(boss->actor_id());
                entry.add_target_actor_ids(boss->actor_id());
                entry.set_telegraph_round(roundIndex);
                entry.set_execute_round(roundIndex);
                entry.set_intent_kind(kTelegraphKindGuardShield);
                entry.set_status(kTelegraphStatusExecuting);
                entry.set_state_version(++telegraphStateVersion);
                entry.set_label("灵障护佑：本回合为首领附加灵障护盾");
                activeTelegraphs.push_back(std::move(entry));
            }
        } else if (otherGuard != nullptr && bossTrialKind == kBossTrialKindVenom) {
            if (roundIndex % 3u == 1u && roundIndex + 1u <= maxRounds &&
                !hasEntryFor(otherGuard->actor_id(), roundIndex + 1u)) {
                const uint64_t tid = pickEnemyTarget(*otherGuard);
                BattleTelegraphEntry entry;
                entry.set_source_actor_id(otherGuard->actor_id());
                entry.set_target_actor_id(tid);
                if (tid != 0) entry.add_target_actor_ids(tid);
                entry.set_telegraph_round(roundIndex);
                entry.set_execute_round(roundIndex + 1u);
                entry.set_intent_kind(kTelegraphKindGuardBind);
                entry.set_status(kTelegraphStatusUpcoming);
                entry.set_state_version(++telegraphStateVersion);
                entry.set_label("缚灵锁脉：下回合封控目标并消耗法力");
                activeTelegraphs.push_back(std::move(entry));
            }
        }
    }
}

void TurnBattleEngine::ExecuteBossTrialSlowAndGuardianActions(TurnResultS2C& result) {
    if (activeTelegraphs.empty()) {
        return;
    }
    const std::vector<BattleTelegraphEntry> due = activeTelegraphs;
    std::vector<BattleTelegraphEntry> roundResolved;
    const auto recordResolved = [&](const BattleTelegraphEntry& base, uint32_t status, const char* label) {
        BattleTelegraphEntry resolved = base;
        resolved.set_status(status);
        resolved.set_state_version(++telegraphStateVersion);
        if (label != nullptr && label[0] != '\0') {
            resolved.set_label(label);
        }
        *result.add_resolved_telegraphs() = resolved;
        roundResolved.push_back(std::move(resolved));
    };

    const auto findAliveBoss = [&]() -> BattleActorState* {
        for (auto& actor : actors) {
            if (actor.actor_type() == BATTLE_ACTOR_TYPE_MONSTER && actor.team_index() == 1 &&
                actor.is_boss() && IsActorActive(actor)) {
                return &actor;
            }
        }
        return nullptr;
    };

    for (const auto& entry : due) {
        if (entry.execute_round() != roundIndex) {
            continue;
        }
        auto* source = FindActor(entry.source_actor_id());
        if (source == nullptr || !IsActorActive(*source)) {
            if (entry.intent_kind() == kTelegraphKindFestivalHeavyStrike) {
                recordResolved(entry, kTelegraphStatusCancelledByControl, "首领已倒地，春禧重击取消");
            }
            continue;
        }
        // 受控（眩晕/冰冻/昏睡/混乱/封印）直接取消蓄力或护卫机制，绝不回退普攻！
        if (IsActorControlLocked(*source)) {
            if (entry.intent_kind() == kTelegraphKindFestivalHeavyStrike) {
                recordResolved(entry, kTelegraphStatusCancelledByControl, "春禧重击受控取消");
            } else if (IsBossTrialKind(bossTrialKind)) {
                if (entry.intent_kind() == kTelegraphKindSlowCleave ||
                    entry.intent_kind() == kTelegraphKindBossMagicStrike ||
                    entry.intent_kind() == kTelegraphKindBossVenomAoe ||
                    entry.intent_kind() == kTelegraphKindWildWindGust) {
                    ApplyBossExposed(source->actor_id(), kBossExposedFullPct, roundIndex + 1u);
                    ++cleaveParryCount;
                    EmitBossMechanicEvent(result, kBossMechanicCleaveParryExpose, source->actor_id(),
                                          source->actor_id(), kBossExposedFullPct);
                    recordResolved(entry, kTelegraphStatusCancelledByControl,
                                   entry.intent_kind() == kTelegraphKindWildWindGust
                                       ? "风眼强袭受控取消，首领陷入破绽"
                                       : "重击受控取消，首领陷入破绽");
                } else if (entry.intent_kind() == kTelegraphKindGuardHeal) {
                    if (auto* boss = findAliveBoss(); boss != nullptr) {
                        TriggerRejuvBacklash(source->actor_id(), *boss, result);
                    }
                    recordResolved(entry, kTelegraphStatusCancelledByControl, "回春被控打断，触发灵力反噬");
                } else if (entry.intent_kind() == kTelegraphKindGuardStancePhys ||
                           entry.intent_kind() == kTelegraphKindGuardStanceMagic) {
                    ++stanceBreakCount;
                    ApplyBossExposed(source->actor_id(), kBossExposedFullPct, roundIndex + 1u);
                    if (auto* boss = findAliveBoss(); boss != nullptr) {
                        ApplyBossExposed(boss->actor_id(), kBossExposedFullPct, roundIndex + 1u);
                        EmitBossMechanicEvent(result, kBossMechanicStanceBreak, source->actor_id(),
                                              boss->actor_id(), kBossExposedFullPct);
                    }
                    recordResolved(entry, kTelegraphStatusCancelledByControl, "切阵被控打断，触发崩势破绽");
                } else if (entry.intent_kind() == kTelegraphKindGuardShield ||
                           entry.intent_kind() == kTelegraphKindGuardBind) {
                    ++stanceBreakCount;
                    if (auto* boss = findAliveBoss(); boss != nullptr) {
                        ApplyBossExposed(boss->actor_id(), kBossExposedPartialPct, roundIndex + 1u);
                        EmitBossMechanicEvent(result, kBossMechanicStanceBreak, source->actor_id(),
                                              boss->actor_id(), kBossExposedPartialPct);
                    }
                    recordResolved(entry, kTelegraphStatusCancelledByControl,
                                   entry.intent_kind() == kTelegraphKindGuardShield
                                       ? "灵障护佑被控打断，首领陷入破绽"
                                       : "缚灵锁脉被控打断，首领陷入破绽");
                }
            }
            continue;
        }

        const auto dealBossStrike = [&](BattleActorState& target, uint32_t damageType, double scale) {
            bool isCritical = false;
            const uint64_t attack = damageType == combatdamage::kMagicDamage
                ? std::max(source->magic_attack(), EffectivePhysicalAttack(*source))
                : EffectivePhysicalAttack(*source);
            const double rawDamage = CalculateFinalDamage(*source, target, kBasicAttackBaseDamage * scale,
                                                          attack, scale, damageType, isCritical);
            if (HasConfiguredBuffs(*source) || HasConfiguredBuffs(target) ||
                source->has_equipment_effects() || target.has_equipment_effects()) {
                EffectContext ctx;
                ctx.critical = isCritical;
                ctx.origin = BattleDamageOrigin::Direct;
                ctx.primaryTargetId = target.actor_id();
                ResolveConfiguredDamage(*source, target, rawDamage, damageType, ctx, result);
            } else {
                const uint64_t dealt = ApplyDamage(target, rawDamage);
                auto* dmgEv = AppendEvent(result, BATTLE_EVENT_DAMAGE, source->actor_id(), target.actor_id());
                dmgEv->set_value(dealt);
                dmgEv->set_is_critical(isCritical);
                dmgEv->set_target_health_after(target.attributes().health());
                dmgEv->set_target_mana_after(target.attributes().mana());
                if (target.attributes().health() == 0) {
                    HandleDeath(target, source->actor_id(), result);
                }
            }
        };

        switch (entry.intent_kind()) {
        case kTelegraphKindFestivalHeavyStrike: {
            auto* target = FindActor(entry.target_actor_id());
            if (target == nullptr || !IsActorActive(*target)) {
                recordResolved(entry, kTelegraphStatusCancelledTargetInvalid, "目标已失效，春禧重击取消");
                break;
            }
            BeginEventGroup();
            AppendEvent(result, BATTLE_EVENT_ATTACK, source->actor_id(), target->actor_id());
            const double scale = IsFestivalBeastBattle()
                ? static_cast<double>(createRequest.activity_context().festival_beast().heavy_damage_percent()) / 100.0
                : 1.8;
            dealBossStrike(*target, combatdamage::kPhysicalDamage, scale);
            recordResolved(entry, kTelegraphStatusExecuted, "春禧重击已发动");
            break;
        }
        case kTelegraphKindSlowCleave:
        case kTelegraphKindBossMagicStrike:
        case kTelegraphKindWildWindGust: {
            std::vector<uint64_t> targetIds;
            if (entry.target_actor_ids_size() > 0) {
                for (const uint64_t tid : entry.target_actor_ids()) {
                    targetIds.push_back(tid);
                }
            } else if (entry.target_actor_id() != 0) {
                targetIds.push_back(entry.target_actor_id());
            }
            if (targetIds.empty()) {
                break;
            }
            const bool isWindGust = entry.intent_kind() == kTelegraphKindWildWindGust;
            const bool isMagic = entry.intent_kind() == kTelegraphKindBossMagicStrike || isWindGust;
            const uint32_t dmgType = isMagic ? combatdamage::kMagicDamage : combatdamage::kPhysicalDamage;
            double scale = isWindGust
                ? 1.75
                : (isMagic
                       ? (battleDifficulty >= 2u ? 2.0 : 1.7)
                       : (battleDifficulty >= 2u ? 2.1 : 1.8));
            if (IsBossTrialKind(bossTrialKind)) {
                if (bossMomentumStacks > 0) {
                    scale *= (1.0 + static_cast<double>(bossMomentumStacks * kBossMomentumPerStackPct) / 100.0);
                }
                if (bossPhase >= 2u) {
                    scale *= 1.10;
                }
                if (bossWeeklyAffixId == bossaffix::kAffixEarth) {
                    scale *= bossaffix::kEarthBossCleaveMultiplier;
                }
            }

            uint32_t validTargets = 0;
            uint32_t invalidTargets = 0;
            uint32_t parriedTargets = 0;
            for (const uint64_t targetId : targetIds) {
                auto* target = FindActor(targetId);
                uint64_t ownerId = 0;
                if (target != nullptr) {
                    ownerId = target->actor_type() == BATTLE_ACTOR_TYPE_PET
                        ? target->owner_player_id()
                        : (target->actor_type() == BATTLE_ACTOR_TYPE_PLAYER ? target->actor_id() : 0ULL);
                }
                if (ownerId != 0 && IsBossTrialKind(bossTrialKind)) {
                    ++selfCleavesTargetedByPlayer[ownerId];
                }
                if (target == nullptr || !IsActorActive(*target)) {
                    // V1 防线：目标倒地或主动退出导致的失效取消，绝不计为化劲成功、绝不触发破绽。
                    ++invalidTargets;
                    continue;
                }
                ++validTargets;
                const bool wasDefending = target->is_defending();
                const bool hadProtection = ActorHasBossCleaveParryProtection(*target, dmgType);
                const int eventsBefore = result.events_size();
                BeginEventGroup();
                AppendEvent(result, BATTLE_EVENT_ATTACK, source->actor_id(), target->actor_id());
                dealBossStrike(*target, dmgType, scale);
                bool triggeredShieldOrReflect = false;
                for (int i = eventsBefore; i < result.events_size(); ++i) {
                    const auto& ev = result.events(i);
                    if (ev.event_type() == BATTLE_EVENT_SHIELD && ev.target_id() == target->actor_id() &&
                        ev.absorbed() > 0) {
                        triggeredShieldOrReflect = true;
                        break;
                    }
                    if (ev.event_type() == BATTLE_EVENT_DAMAGE && ev.hit_kind() == BATTLE_HIT_REFLECT &&
                        ev.source_id() == target->actor_id()) {
                        triggeredShieldOrReflect = true;
                        break;
                    }
                }
                const bool parried = wasDefending || hadProtection || triggeredShieldOrReflect;
                if (parried) {
                    ++parriedTargets;
                    if (ownerId != 0 && IsBossTrialKind(bossTrialKind)) {
                        ++selfCleavesParriedByPlayer[ownerId];
                    }
                } else if (isWindGust && target->attributes().health() == 0) {
                    bossWindHitUnprotectedDeath = true;
                }
            }
            if (IsBossTrialKind(bossTrialKind)) {
                bossMomentumStacks = 0;
                if (validTargets == 0) {
                    recordResolved(entry, kTelegraphStatusCancelledTargetInvalid, "目标已失效，重劈取消");
                } else {
                    if (parriedTargets == targetIds.size() && invalidTargets == 0) {
                        ApplyBossExposed(source->actor_id(), kBossExposedFullPct, roundIndex + 1u);
                        ++cleaveParryCount;
                        EmitBossMechanicEvent(result, kBossMechanicCleaveParryExpose, source->actor_id(),
                                              source->actor_id(), kBossExposedFullPct);
                        OnBossWeeklyAffixParryTriggered(source->actor_id(), result);
                    } else if (targetIds.size() >= 2u && parriedTargets >= 1u) {
                        // V3 防线：多人裂地双劈 1/2 化解给予轻度破绽 +20%，保留认真防守队友的正反馈。
                        ApplyBossExposed(source->actor_id(), kBossExposedPartialPct, roundIndex + 1u);
                        ++cleaveParryCount;
                        EmitBossMechanicEvent(result, kBossMechanicCleaveParryExpose, source->actor_id(),
                                              source->actor_id(), kBossExposedPartialPct);
                        OnBossWeeklyAffixParryTriggered(source->actor_id(), result);
                    }
                    recordResolved(entry, kTelegraphStatusExecuted, "慢速重劈已发动");
                }
            }
            break;
        }
        case kTelegraphKindBossVenomAoe: {
            const auto targets = CollectAliveEnemyIds(*source);
            if (targets.empty()) {
                break;
            }
            BeginEventGroup();
            double scale = battleDifficulty >= 2u ? 1.05 : 0.85;
            if (IsBossTrialKind(bossTrialKind)) {
                if (bossMomentumStacks > 0) {
                    scale *= (1.0 + static_cast<double>(bossMomentumStacks * kBossMomentumPerStackPct) / 100.0);
                }
                if (bossPhase >= 2u) {
                    scale *= 1.10;
                }
                if (bossWeeklyAffixId == bossaffix::kAffixEarth) {
                    scale *= bossaffix::kEarthBossCleaveMultiplier;
                }
            }
            uint32_t validTargets = 0;
            uint32_t parriedTargets = 0;
            for (const uint64_t targetId : targets) {
                auto* target = FindActor(targetId);
                if (target == nullptr || !IsActorActive(*target)) {
                    continue;
                }
                ++validTargets;
                const uint64_t ownerId = target->actor_type() == BATTLE_ACTOR_TYPE_PET
                    ? target->owner_player_id()
                    : (target->actor_type() == BATTLE_ACTOR_TYPE_PLAYER ? target->actor_id() : 0ULL);
                if (ownerId != 0 && IsBossTrialKind(bossTrialKind)) {
                    ++selfCleavesTargetedByPlayer[ownerId];
                }
                const bool wasDefending = target->is_defending();
                const bool hadProtection = ActorHasBossCleaveParryProtection(*target, combatdamage::kMagicDamage);
                const int eventsBefore = result.events_size();
                dealBossStrike(*target, combatdamage::kMagicDamage, scale);
                bool triggeredShieldOrReflect = false;
                for (int i = eventsBefore; i < result.events_size(); ++i) {
                    const auto& ev = result.events(i);
                    if (ev.event_type() == BATTLE_EVENT_SHIELD && ev.target_id() == target->actor_id() &&
                        ev.absorbed() > 0) {
                        triggeredShieldOrReflect = true;
                        break;
                    }
                    if (ev.event_type() == BATTLE_EVENT_DAMAGE && ev.hit_kind() == BATTLE_HIT_REFLECT &&
                        ev.source_id() == target->actor_id()) {
                        triggeredShieldOrReflect = true;
                        break;
                    }
                }
                if (wasDefending || hadProtection || triggeredShieldOrReflect) {
                    ++parriedTargets;
                    if (ownerId != 0 && IsBossTrialKind(bossTrialKind)) {
                        ++selfCleavesParriedByPlayer[ownerId];
                    }
                }
            }
            if (IsBossTrialKind(bossTrialKind)) {
                bossMomentumStacks = 0;
                if (validTargets > 0 && parriedTargets == validTargets) {
                    ApplyBossExposed(source->actor_id(), kBossExposedFullPct, roundIndex + 1u);
                    ++cleaveParryCount;
                    EmitBossMechanicEvent(result, kBossMechanicCleaveParryExpose, source->actor_id(),
                                          source->actor_id(), kBossExposedFullPct);
                    OnBossWeeklyAffixParryTriggered(source->actor_id(), result);
                } else if (validTargets >= 2u && parriedTargets * 2u >= validTargets) {
                    ApplyBossExposed(source->actor_id(), kBossExposedPartialPct, roundIndex + 1u);
                    ++cleaveParryCount;
                    EmitBossMechanicEvent(result, kBossMechanicCleaveParryExpose, source->actor_id(),
                                          source->actor_id(), kBossExposedPartialPct);
                    OnBossWeeklyAffixParryTriggered(source->actor_id(), result);
                }
            }
            recordResolved(entry, kTelegraphStatusExecuted, "玄毒冲击已发动");
            break;
        }
        case kTelegraphKindGuardHeal: {
            auto* boss = FindActor(entry.target_actor_id());
            if (boss == nullptr || !IsActorActive(*boss)) {
                break;
            }
            const double incomingHealMod = ConfiguredModifier(*boss, BattleModifier::IncomingHeal);
            const bool antiHealSuppressed = incomingHealMod < 0.0;
            const double received = std::max(0.0, 1.0 + incomingHealMod);
            BeginEventGroup();
            uint64_t healed = 0;
            if (received > 0.0) {
                const uint64_t pct = battleDifficulty >= 2u ? 10ULL : 8ULL;
                const uint64_t rawHeal = std::max<uint64_t>(1ULL, (boss->max_health() * pct) / 100ULL);
                healed = ApplyHeal(*boss, static_cast<double>(rawHeal) * received);
                RecordTrialMonsterHeal(*source, *boss, healed);
                auto* healEv = AppendEvent(result, BATTLE_EVENT_HEAL, source->actor_id(), boss->actor_id());
                healEv->set_value(healed);
                healEv->set_target_health_after(boss->attributes().health());
                healEv->set_target_mana_after(boss->attributes().mana());
            }
            if (antiHealSuppressed) {
                // A35 断脉咒压制回春：阻止困难净化，并使首领陷入 1 回合「气馁」（物攻 -15%）。
                bossDisheartenedUntilRound = roundIndex + 1u;
                EmitBossMechanicEvent(result, kBossMechanicAntiHealDishearten, source->actor_id(),
                                      boss->actor_id(), 15ULL);
            } else if (battleDifficulty >= 2u && healed > 0) {
                // 困难回春进阶（方案 B1 / 双卫同阵）：有效治疗顺手净化首领 1 项可清除有害状态。
                uint64_t removeConfiguredId = 0;
                int removeLegacyIdx = -1;
                for (int idx = 0; idx < boss->buffs_size(); ++idx) {
                    const auto& b = boss->buffs(idx);
                    if (!IsDebuffEntry(b) || IsConfiguredListenerBuff(b)) {
                        continue;
                    }
                    const auto* bRow = dataProvider->FindBuff(b.buff_table_id());
                    if (bRow != nullptr && bRow->battle_op() != 0) {
                        removeConfiguredId = b.buff_id();
                        break;
                    }
                    if (removeLegacyIdx < 0) {
                        removeLegacyIdx = idx;
                    }
                }
                if (removeConfiguredId != 0) {
                    RemoveConfiguredBuff(*boss, removeConfiguredId, result);
                    EmitBossMechanicEvent(result, kBossMechanicRejuvCleanse, source->actor_id(),
                                          boss->actor_id(), 1ULL);
                } else if (removeLegacyIdx >= 0) {
                    RemoveBuffAt(*boss, removeLegacyIdx, result);
                    EmitBossMechanicEvent(result, kBossMechanicRejuvCleanse, source->actor_id(),
                                          boss->actor_id(), 1ULL);
                }
            }
            recordResolved(entry, kTelegraphStatusExecuted, "回春祈愈已发动");
            break;
        }
        case kTelegraphKindGuardStancePhys:
        case kTelegraphKindGuardStanceMagic: {
            auto* boss = FindActor(entry.target_actor_id());
            if (boss == nullptr || !IsActorActive(*boss)) {
                break;
            }
            BeginEventGroup();
            bossStanceType = entry.intent_kind() == kTelegraphKindGuardStancePhys
                ? kBossStancePhysical
                : kBossStanceMagic;
            bossStanceRemainRounds = 3u;
            lastChosenStanceType = bossStanceType;
            auto* stanceEv = AppendEvent(result, BATTLE_EVENT_BUFF_ADD, source->actor_id(), boss->actor_id());
            stanceEv->set_value(bossStanceType);
            // 困难·护势进阶（方案 B2）：切阵成功瞬间额外给首领叠 1 层「蓄势」。
            if (bossTrialKind == kBossTrialKindBreakStance && battleDifficulty >= 2u &&
                bossMomentumStacks < kBossMomentumMaxStacks) {
                ++bossMomentumStacks;
                EmitBossMechanicEvent(result, kBossMechanicMomentumGain, source->actor_id(),
                                      boss->actor_id(), bossMomentumStacks);
            }
            recordResolved(entry, kTelegraphStatusExecuted,
                           bossStanceType == kBossStancePhysical ? "物防联防阵已展开" : "法防联防阵已展开");
            break;
        }
        case kTelegraphKindGuardShield: {
            auto* boss = FindActor(entry.target_actor_id());
            if (boss == nullptr || !IsActorActive(*boss)) {
                break;
            }
            BeginEventGroup();
            const uint64_t pct = battleDifficulty >= 2u ? 18ULL : 12ULL;
            const uint64_t addShield = std::max<uint64_t>(1ULL, (boss->max_health() * pct) / 100ULL);
            bossBarrierShield = SaturatingAdd(bossBarrierShield, addShield);
            auto* shieldEv = AppendEvent(result, BATTLE_EVENT_SHIELD, source->actor_id(), boss->actor_id());
            shieldEv->set_value(addShield);
            shieldEv->set_shield_after(bossBarrierShield);
            recordResolved(entry, kTelegraphStatusExecuted, "灵障护盾已附加");
            break;
        }
        case kTelegraphKindGuardBind: {
            auto* target = FindActor(entry.target_actor_id());
            if (target == nullptr || !IsActorActive(*target)) {
                break;
            }
            BeginEventGroup();
            const uint64_t resist = std::min<uint64_t>(
                100ULL,
                static_cast<uint64_t>(target->combat().resist_forget()) + target->combat().resist_all_ailment());
            if (RollPercent(resist)) {
                AppendEvent(result, BATTLE_EVENT_RESIST, source->actor_id(), target->actor_id());
                if (target->actor_type() == BATTLE_ACTOR_TYPE_PLAYER) {
                    antiControlOrSeasonSkillPlayers.insert(target->actor_id());
                } else if (target->actor_type() == BATTLE_ACTOR_TYPE_PET && target->owner_player_id() != 0) {
                    antiControlOrSeasonSkillPlayers.insert(target->owner_player_id());
                }
                recordResolved(entry, kTelegraphStatusExecuted, "缚灵锁脉已被抵抗");
                break;
            }
            // 缚灵吸蓝冲击（15% 最大法力），不依赖固定 Buff 表主键。
            const uint64_t drain = std::min<uint64_t>(
                target->attributes().mana(),
                std::max<uint64_t>(1ULL, (target->max_mana() * 15ULL) / 100ULL));
            if (drain > 0) {
                target->mutable_attributes()->set_mana(target->attributes().mana() - drain);
                auto* manaEv = AppendEvent(result, BATTLE_EVENT_MANA, source->actor_id(), target->actor_id());
                manaEv->set_value(drain);
                manaEv->set_mana_delta(-static_cast<int64_t>(drain));
                manaEv->set_target_health_after(target->attributes().health());
                manaEv->set_target_mana_after(target->attributes().mana());
            }
            recordResolved(entry, kTelegraphStatusExecuted, "缚灵锁脉已发动");
            break;
        }
        default:
            break;
        }
    }
    if (!roundResolved.empty()) {
        lastResolvedTelegraphs = std::move(roundResolved);
    }
}

void TurnBattleEngine::TryDispelBossStance(const BattleActorState& source, const BattleActorState& target,
                                           TurnResultS2C& result) {
    if (!IsBossTrialKind(bossTrialKind) || source.team_index() == target.team_index() || target.team_index() != 1) {
        return;
    }
    bool dispelledAny = false;
    if (IsBossStanceTrialKind(bossTrialKind) && bossStanceType != 0) {
        bossStanceType = 0;
        bossStanceRemainRounds = 0;
        guardStanceDispelled = true;
        ++stanceBreakCount;
        dispelledAny = true;
        // 崩势破盾：首领与存活护势护卫同时陷入「破绽（+35%，至下回合末）」。
        for (const auto& a : actors) {
            if (a.actor_type() != BATTLE_ACTOR_TYPE_MONSTER || a.team_index() != 1 || !IsActorActive(a)) {
                continue;
            }
            if (a.is_boss() || a.monster_table_id() == kBossTrialMonsterGuardStance ||
                (bossTrialKind == kBossTrialKindBreakStance &&
                 a.monster_table_id() != kBossTrialMonsterSupportBreaker &&
                 a.monster_table_id() != kBossTrialMonsterSupportDrummer)) {
                ApplyBossExposed(a.actor_id(), kBossExposedFullPct, roundIndex + 1u);
            }
        }
        EmitBossMechanicEvent(result, kBossMechanicStanceBreak, source.actor_id(), target.actor_id(),
                              kBossExposedFullPct);
    }
    if (bossMomentumStacks > 0) {
        bossMomentumStacks = 0;
        dispelledAny = true;
    }
    if (dispelledAny) {
        RecordEffectiveContribution(source);
        OnBossWeeklyAffixDispelTriggered(source.actor_id(), result);
    }
}

bool TurnBattleEngine::IsActorExposed(uint64_t actorId) const {
    if (actorId == 0u) {
        return false;
    }
    const auto it = exposedActors.find(actorId);
    return it != exposedActors.end() && it->second.pct > 0u && it->second.untilRound >= roundIndex;
}

bool TurnBattleEngine::IsWeeklyWaterDiscountSkill(const SkillTable& skillRow) const {
    for (const uint32_t effectBuffId : skillRow.effect()) {
        const auto* buffRow = dataProvider->FindBuff(effectBuffId);
        if (buffRow == nullptr) {
            continue;
        }
        const auto op = static_cast<BattleEffectOp>(buffRow->battle_op());
        if (op == BattleEffectOp::PrepareCast || op == BattleEffectOp::PeriodicDamage ||
            op == BattleEffectOp::ConsumePeriodic || op == BattleEffectOp::ExtendPeriodic ||
            buffRow->buff_type() == kBuffTypePoison || buffRow->buff_type() == kBuffTypeBurn) {
            return true;
        }
    }
    return false;
}

void TurnBattleEngine::OnBossWeeklyAffixDispelTriggered(uint64_t casterActorId, TurnResultS2C& result) {
    if (bossWeeklyAffixId != bossaffix::kAffixWood || casterActorId == 0u) {
        return;
    }
    auto* caster = FindActor(casterActorId);
    if (caster == nullptr || caster->team_index() != 0u || !IsActorActive(*caster)) {
        return;
    }
    weeklyAffixWoodTeamBoostUntilRound = std::max(weeklyAffixWoodTeamBoostUntilRound, roundIndex + 1u);
    if (caster->max_mana() == 0ULL || weeklyAffixWoodManaTriggeredActors.contains(casterActorId)) {
        return;
    }
    weeklyAffixWoodManaTriggeredActors.insert(casterActorId);
    const uint64_t rawGain =
        std::max<uint64_t>(1ULL, (caster->max_mana() * bossaffix::kWoodDispelManaRegenPct) / 100ULL);
    const uint64_t curMana = caster->attributes().mana();
    const uint64_t missing = caster->max_mana() > curMana ? (caster->max_mana() - curMana) : 0ULL;
    const uint64_t gained = std::min(rawGain, missing);
    if (gained > 0ULL) {
        caster->mutable_attributes()->set_mana(curMana + gained);
        auto* manaEv = AppendEvent(result, BATTLE_EVENT_MANA, casterActorId, casterActorId);
        manaEv->set_value(gained);
        manaEv->set_mana_delta(static_cast<int64_t>(gained));
        manaEv->set_target_health_after(caster->attributes().health());
        manaEv->set_target_mana_after(caster->attributes().mana());
    }
    EmitBossMechanicEvent(result, kBossMechanicWeeklyAffixTrigger, casterActorId, casterActorId, gained);
}

void TurnBattleEngine::OnBossWeeklyAffixParryTriggered(uint64_t bossActorId, TurnResultS2C& result) {
    if (bossWeeklyAffixId != bossaffix::kAffixEarth) {
        return;
    }
    weeklyAffixEarthTeamBoostUntilRound = std::max(weeklyAffixEarthTeamBoostUntilRound, roundIndex + 1u);
    uint64_t shieldedCount = 0ULL;
    for (const auto& actor : actors) {
        if (actor.team_index() != 0u || !IsActorActive(actor) || actor.max_health() == 0ULL) {
            continue;
        }
        const uint64_t gain =
            std::max<uint64_t>(1ULL, (actor.max_health() * bossaffix::kEarthParryTeamShieldPct) / 100ULL);
        auto& pool = leylineEarthShieldByActor[actor.actor_id()];
        pool = SaturatingAdd(pool, gain);
        auto* shieldEv = AppendEvent(result, BATTLE_EVENT_SHIELD, actor.actor_id(), actor.actor_id());
        shieldEv->set_shield_after(pool);
        ++shieldedCount;
    }
    if (shieldedCount > 0ULL) {
        EmitBossMechanicEvent(result, kBossMechanicWeeklyAffixTrigger, bossActorId, bossActorId, shieldedCount);
    }
}

// ---------------------------------------------------------------------------
// effect[] → buff(镜像 BuffSystem::AddOrUpdateBuff,时间维度换算为回合)
// ---------------------------------------------------------------------------

void TurnBattleEngine::AddBuffToActor(BattleActorState& target, uint32_t buffTableId,
                                      uint64_t casterId, uint32_t depth, TurnResultS2C& result,
                                      double statPercent) {
    // 子 buff 递归深度上限,防表配环
    if (depth > kMaxSubBuffDepth || target.is_dead()) {
        return;
    }

    const auto* buffRow = dataProvider->FindBuff(buffTableId);
    if (buffRow == nullptr) {
        return;
    }

    const auto* controlCaster = FindActor(casterId);
    const bool extendControlBoon = IsHardControlBuff(buffTableId) &&
                                   controlCaster != nullptr && controlCaster->team_index() == 0 &&
                                   target.team_index() != 0 &&
                                   HasRealmBoon(wuxing::kBoonWaterExtendControl);
    // 免疫:目标现有 buff 的 immune_tag 覆盖新 buff 的任一 tag 即挡下
    if (IsImmuneToBuff(target, *buffRow)) {
        if (extendControlBoon) {
            boonRuntime.vulnerableUntilRound[target.actor_id()] = roundIndex + 2u;
        }
        return;
    }
    if (IsHardControlBuff(buffTableId)) {
        if (controlCaster != nullptr && controlCaster->team_index() != target.team_index() &&
            IsControlBannedForActor(*controlCaster)) {
            auto* resistEvent = AppendEvent(result, BATTLE_EVENT_RESIST, casterId, target.actor_id());
            resistEvent->set_buff_table_id(buffTableId);
            return;
        }
    }

    // 抗异常(equipment-attributes.md §4.5,D8):排在免疫之后 —— 已经免疫就没有"抵抗"可言,
    // 不该为它掷骰;排在驱散之前 —— 被抵抗的 buff 等于没挂上,不能顺手把目标身上的东西驱掉。
    // 子 buff 是整条递归进来的,父 buff 被抵抗时它们也一并不落地。
    // 快照带入的 buff 不经过本函数(InitPlayers 直接拷贝 + SanitizeSnapshotBuffs),不会被判抵抗
    if (RollAilmentResist(target, *buffRow, casterId)) {
        if (extendControlBoon) {
            boonRuntime.vulnerableUntilRound[target.actor_id()] = roundIndex + 2u;
        }
        auto* resistEvent = AppendEvent(result, BATTLE_EVENT_RESIST, casterId, target.actor_id());
        resistEvent->set_buff_table_id(buffTableId);
        return;
    }

    // 驱散:新 buff 的 dispel_tag 命中现有 buff 的 tag 即移除之
    const uint32_t removedByTag = DispelBuffsByTag(target, *buffRow, result);
    if (removedByTag > 0) {
        RecordDispelLanded(casterId, removedByTag);
    }
    if (const auto* caster = FindActor(casterId); caster != nullptr) {
        if (buffRow->buff_type() == kBuffTypeDispel && caster->team_index() != target.team_index()) {
            TryDispelBossStance(*caster, target, result);
        }
        RecordEffectiveContribution(*caster);
    }

    // 纯驱散 buff 不落地(镜像 DispelBuffsOnAwake 的返回语义)
    if (buffRow->buff_type() == kBuffTypeDispel) {
        return;
    }

    // 同表同施法者:叠层/刷新,不新建条目
    if (StackOrRefreshExistingBuff(target, *buffRow, casterId, result, statPercent)) {
        return;
    }

    const uint64_t newBuffId = nextBuffInstanceId++;
    auto* entry = target.add_buffs();
    entry->set_buff_id(newBuffId);
    entry->set_buff_table_id(buffTableId);
    entry->set_layer(1);
    entry->set_caster_id(casterId);
    if (statPercent != 0.0) {
        // 职业技能增减益:幅度按施法者此刻的等级算定后就不再变。
        entry->set_stat_percent(statPercent);
    }
    if (statPercent != 0.0 || buffRow->buff_type() == kBuffTypeNextBasicAttack) {
        // 下一次普攻按完整回合保留:5 秒配置换算为 1 回合,不能刚挂上便在本轮末到期。
        entry->set_applied_round(roundIndex);
    }
    // 持续时间换算:infinite_duration → 0 哨兵(无限);其余按秒→回合
    if (buffRow->infinite_duration() != 0) {
        entry->set_remain_rounds(0);
    } else if (buffRow->duration() > 0) {
        entry->set_remain_rounds(RoundsFromSeconds(buffRow->duration()) + (extendControlBoon ? 1u : 0u));
    } else {
        // duration=0 且非无限:实时侧是"挂上即到期"的瞬时 buff,
        // 这里同口径:驱散/子 buff 已生效,条目本身立即移除
        entry->set_remain_rounds(1);
    }
    // 下面的子 buff 递归会往同一张 buff 列表增删条目,可能把本条目驱散掉;
    // 不能端着 entry 指针跨递归(镜像实时 AddConfiguredSubBuffs 的收口注释),
    // 之后一律用 newBuffId 重查
    entry = nullptr;

    auto* addEvent = AppendEvent(result, BATTLE_EVENT_BUFF_ADD, casterId, target.actor_id());
    addEvent->set_buff_table_id(buffTableId);
    addEvent->set_value(1);  // value 携带当前叠层数

    OnBuffStart(target, newBuffId, *buffRow, depth, result);

    // 瞬时 buff:落地事件发过之后立即移除
    if (buffRow->infinite_duration() == 0 && buffRow->duration() <= 0) {
        const int index = FindBuffIndex(target, newBuffId);
        if (index >= 0) {
            RemoveBuffAt(target, index, result);
        }
    }
}

void TurnBattleEngine::OnBuffStart(BattleActorState& target, uint64_t buffId,
                                   const BuffTable& buffRow, uint32_t depth,
                                   TurnResultS2C& result) {
    // 该类型的子效果属于下次普攻伤害事件,不能在挂载时就提前生效。
    if (buffRow.buff_type() == kBuffTypeNextBasicAttack) {
        return;
    }
    const int index = FindBuffIndex(target, buffId);
    if (index < 0) {
        return;
    }
    const uint64_t casterId = target.buffs(index).caster_id();
    for (const auto subBuffId : buffRow.sub_buff()) {
        if (FindBuffIndex(target, buffId) < 0) {
            return;  // Start 还未结束,父实例已被前一个子效果驱散。
        }
        AddBuffToActor(target, subBuffId, casterId, depth + 1, result);
    }
    // target_sub_buff 需要事件提供实际受击目标,这里没有这个上下文,不能反挂给施加者。
}

bool TurnBattleEngine::RollAilmentResist(const BattleActorState& target, const BuffTable& buffRow,
                                         uint64_t casterId) {
    // 只有敌方施加的才判抵抗:自己或队友挂的(哪怕类型是眩晕 / 沉默)是己方的选择,不该被自己的装备挡掉;
    // 找不到施加者(casterId = 0 或已不在本局)也不判 —— 没有"忽视抗异常"可取,也谈不上敌我
    const auto* caster = FindActor(casterId);
    if (caster == nullptr || caster->team_index() == target.team_index()) {
        return false;
    }
    const auto specificResist = AilmentResistPercent(target.combat(), buffRow.buff_type());
    if (!specificResist.has_value()) {
        return false;  // 不是异常状态(增益、驱散、灼烧等)
    }

    // 有效抵抗率 = max(0, 单项 + 所有抗异常 − 施加者的忽视所有抗异常);RollPercent 负责夹到 100,
    // 且为 0 时不消耗随机数(没有抗性装备的目标,事件流与本功能落地前逐位一致)
    const uint64_t totalResist = SaturatingAdd(
        SaturatingAdd(*specificResist, target.combat().resist_all_ailment()),
        WuxingControlResistPct(target));
    const uint64_t ignored = caster->combat().ignore_ailment_resist();
    const uint64_t effectiveResist = totalResist > ignored ? totalResist - ignored : 0;
    return RollPercent(effectiveResist);
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

uint32_t TurnBattleEngine::DispelBuffsByTag(BattleActorState& target, const BuffTable& buffRow,
                                            TurnResultS2C& result) {
    if (buffRow.dispel_tag().empty()) {
        return 0;
    }

    // 先收集再移除,移除按下标降序避免位移
    std::vector<int> dispelIndexes;
    for (int index = 0; index < target.buffs_size(); ++index) {
        if (IsConfiguredListenerBuff(target.buffs(index))) continue;
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
    return static_cast<uint32_t>(dispelIndexes.size());
}

bool TurnBattleEngine::StackOrRefreshExistingBuff(BattleActorState& target,
                                                  const BuffTable& buffRow, uint64_t casterId,
                                                  TurnResultS2C& result, double statPercent) {
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
        // 刷新持续回合:"重挂满时",与实时侧 BuffSystem::OnBuffRefresh 同一口径
        if (existing.remain_rounds() > 0 && buffRow.duration() > 0) {
            existing.set_remain_rounds(RoundsFromSeconds(buffRow.duration()));
        }
        if (statPercent != 0.0) {
            // 同一行(同方向同属性)重挂:取更强的幅度。
            if (std::abs(statPercent) > std::abs(existing.stat_percent())) {
                existing.set_stat_percent(statPercent);
            }
        }
        if (statPercent != 0.0 || buffRow.buff_type() == kBuffTypeNextBasicAttack) {
            // 刷新同样给足完整持续回合,不让本轮末立刻扣掉新授予的行动机会。
            existing.set_applied_round(roundIndex);
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
    const auto* row = dataProvider->FindBuff(buffTableId);
    if (row != nullptr && row->battle_op() != 0) {
        RemoveConfiguredBuff(target, target.buffs(buffIndex).buff_id(), result);
        return;
    }
    const auto casterId = target.buffs(buffIndex).caster_id();
    target.mutable_buffs()->DeleteSubrange(buffIndex, 1);

    auto* removeEvent = AppendEvent(result, BATTLE_EVENT_BUFF_REMOVE, casterId, target.actor_id());
    removeEvent->set_buff_table_id(buffTableId);
}

// ---------------------------------------------------------------------------
// 输出
// ---------------------------------------------------------------------------

void TurnBattleEngine::MarkPlayerEntered(uint64_t playerId) {
    if (playerId == 0) {
        return;
    }
    if (const auto* actor = FindActor(playerId); actor != nullptr && actor->actor_type() == BATTLE_ACTOR_TYPE_PLAYER) {
        enteredPlayers.insert(playerId);
    }
}

BattleParticipationFact TurnBattleEngine::BuildParticipationFact(uint64_t playerId) const {
    BattleParticipationFact fact;
    fact.set_player_id(playerId);
    const bool entered = enteredPlayers.contains(playerId);
    const bool manual = manualActionPlayers.contains(playerId);
    const bool explicitAuto = explicitAutoPlayers.contains(playerId);
    const bool noOpp = entered && !hadOperableTurnPlayers.contains(playerId);
    fact.set_entered_room(entered);
    fact.set_manual_action_accepted(manual);
    fact.set_explicit_auto_enabled(explicitAuto);
    fact.set_no_action_opportunity(noOpp);
    return fact;
}

bool TurnBattleEngine::DidPlayerParticipate(uint64_t playerId) const {
    if (CompletedRounds() == 0) {
        return false;
    }
    const auto* actor = FindActor(playerId);
    if (actor == nullptr || actor->fled()) {
        return false;
    }
    const auto fact = BuildParticipationFact(playerId);
    if (fact.manual_action_accepted()) {
        return true;
    }
    return fact.entered_room() && (fact.explicit_auto_enabled() || fact.no_action_opportunity());
}

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
    if (!IsPveMatch()) {
        const auto manualIt = manualActionRoundsByPlayer.find(playerId);
        settlement.set_manual_action_rounds(
            manualIt != manualActionRoundsByPlayer.end() ? static_cast<uint32_t>(manualIt->second.size()) : 0u);
        settlement.set_effective_combat_contribution(effectiveContributionPlayers.contains(playerId));
    }
    settlement.set_battle_difficulty(battleDifficulty);
    settlement.set_boss_trial_kind(bossTrialKind);
    // BuildSettlement 可重复读取；只复制实际击杀簿，不按初始阵位推断先后。
    settlement.clear_defeated_monsters();

    const auto* actor = FindActor(playerId);
    if (actor == nullptr) {
        return settlement;
    }
    settlement.set_player_team_index(actor->team_index());
    settlement.set_health(actor->attributes().health());
    settlement.set_mana(actor->attributes().mana());
    settlement.set_is_dead(actor->is_dead());
    settlement.set_fled(actor->fled());
    settlement.set_surrendered(surrenderedTeam.has_value() && actor->team_index() == *surrenderedTeam);
    if (const auto dmgIt = totalDamageDealtByPlayer.find(playerId); dmgIt != totalDamageDealtByPlayer.end()) {
        settlement.set_total_damage_dealt(dmgIt->second);
    }
    if (const auto healIt = totalHealingDoneByPlayer.find(playerId); healIt != totalHealingDoneByPlayer.end()) {
        settlement.set_total_healing_done(healIt->second);
    }
    if (const auto ctrlIt = totalControlsLandedByPlayer.find(playerId); ctrlIt != totalControlsLandedByPlayer.end()) {
        settlement.set_total_controls_landed(ctrlIt->second);
    }
    if (const auto pfIt = primaryFocusDamageByPlayer.find(playerId); pfIt != primaryFocusDamageByPlayer.end()) {
        settlement.set_primary_focus_damage_dealt(pfIt->second);
    }
    if (const auto shIt = totalShieldGrantedByPlayer.find(playerId); shIt != totalShieldGrantedByPlayer.end()) {
        settlement.set_total_shield_granted(shIt->second);
    }
    if (const auto dispIt = totalDispelsLandedByPlayer.find(playerId); dispIt != totalDispelsLandedByPlayer.end()) {
        settlement.set_total_dispels_landed(dispIt->second);
    }
    if (const auto takenIt = damageTakenByPlayer.find(playerId); takenIt != damageTakenByPlayer.end()) {
        settlement.set_damage_taken(takenIt->second);
    }
    const bool playerWon =
        (actor->team_index() == 0 && outcome == BATTLE_OUTCOME_SIDE_A_WIN) ||
        (actor->team_index() == 1 && outcome == BATTLE_OUTCOME_SIDE_B_WIN);
    settlement.set_highlight_kind(
        ComputeHighlightKind(playerId, actor->team_index(), playerWon, actor->is_dead()));
    if (playerWon) {
        uint32_t maxEnemyStreak = 0;
        for (const auto& other : actors) {
            if (other.actor_type() == BATTLE_ACTOR_TYPE_PLAYER &&
                other.team_index() != actor->team_index() &&
                other.win_streak() >= kMinWinStreakHighlight) {
                maxEnemyStreak = std::max(maxEnemyStreak, other.win_streak());
            }
        }
        settlement.set_ended_opponent_streak(maxEnemyStreak);
    }
    settlement.set_player_display_name(actor->name());
    if (IsRankedBattle() && playerWon && CompletedRounds() > 0u) {
        const RankedRivalryEncounter* bestRevenge = nullptr;
        std::string bestRevengeName;
        for (const auto& enc : createRequest.ranked_context().rivalry_encounters()) {
            if (enc.player_id() != playerId || !enc.revenge_target() || enc.opponent_id() == 0) {
                continue;
            }
            const auto* opp = FindActor(enc.opponent_id());
            if (opp == nullptr || opp->actor_type() != BATTLE_ACTOR_TYPE_PLAYER ||
                opp->team_index() == actor->team_index()) {
                continue;
            }
            const bool better = bestRevenge == nullptr ||
                                enc.losses() > bestRevenge->losses() ||
                                (enc.losses() == bestRevenge->losses() && enc.wins() > bestRevenge->wins()) ||
                                (enc.losses() == bestRevenge->losses() && enc.wins() == bestRevenge->wins() &&
                                 enc.opponent_id() < bestRevenge->opponent_id());
            if (better) {
                bestRevenge = &enc;
                bestRevengeName = !enc.opponent_name().empty() ? enc.opponent_name() : opp->name();
            }
        }
        if (bestRevenge != nullptr) {
            settlement.set_revenged_rival(true);
            settlement.set_revenged_opponent_name(bestRevengeName);
            settlement.set_revenged_wins_after(bestRevenge->wins() + 1u);
            settlement.set_revenged_losses_after(bestRevenge->losses());
        }
    }
    if (actor->equipment_effects().has_season_seal()) {
        settlement.set_final_season_seal_id(actor->equipment_effects().season_seal().seal_id());
        if (const auto rtIt = seasonSealRuntimeByActor.find(playerId); rtIt != seasonSealRuntimeByActor.end()) {
            settlement.set_season_seal_contribution_value(rtIt->second.contributionValue);
            settlement.set_season_seal_trigger_count(rtIt->second.triggerCount);
            settlement.set_season_seal_resonance_count(rtIt->second.resonanceCount);
        }
    }
    if (IsRankedBattle() && !UsesFreePreset()) {
        uint32_t starterPetTableId = 0;
        if (const auto prepIt = prepOptionsByPlayer.find(playerId);
            prepIt != prepOptionsByPlayer.end() && prepIt->second.selectedPetId != 0) {
            for (const auto& pet : prepIt->second.availablePets) {
                if (pet.pet_id() == prepIt->second.selectedPetId) {
                    starterPetTableId = pet.pet_table_id();
                    break;
                }
            }
        }
        if (starterPetTableId == 0) {
            for (const auto& other : actors) {
                if (other.actor_type() == BATTLE_ACTOR_TYPE_PET && other.owner_player_id() == playerId &&
                    other.pet_table_id() != 0) {
                    starterPetTableId = other.pet_table_id();
                    break;
                }
            }
        }
        settlement.set_starter_pet_table_id(starterPetTableId);
        settlement.clear_core_skill_ids();
        std::set<uint32_t> seenSkills;
        for (const uint32_t sid : actor->skill_table_ids()) {
            if (sid == 0 || !seenSkills.insert(sid).second) {
                continue;
            }
            settlement.add_core_skill_ids(sid);
            if (settlement.core_skill_ids_size() >= 4) {
                break;
            }
        }
        // 论剑 V7：正式 1V1 排位正常终局透传对阵对手身份，供结算页 15 秒切磋「不服再战」（零排位分）。
        if (outcome != BATTLE_OUTCOME_ONGOING && CompletedRounds() > 0u) {
            uint32_t allyHumans = 0;
            uint32_t enemyHumans = 0;
            const BattleActorState* soleEnemy = nullptr;
            for (const auto& other : actors) {
                if (other.actor_type() != BATTLE_ACTOR_TYPE_PLAYER) {
                    continue;
                }
                if (other.team_index() == actor->team_index()) {
                    ++allyHumans;
                } else {
                    ++enemyHumans;
                    soleEnemy = &other;
                }
            }
            if (allyHumans == 1u && enemyHumans == 1u && soleEnemy != nullptr && soleEnemy->actor_id() != 0) {
                settlement.set_rematch_opponent_id(soleEnemy->actor_id());
                settlement.set_rematch_opponent_name(soleEnemy->name());
            }
        }
    }

    // 出战宝宝的战后终值:随主人的结算一起回 scene(scene 是唯一应用者);论剑临时竞技宠不回写永久宠物。
    for (const auto& other : actors) {
        if (other.actor_type() != BATTLE_ACTOR_TYPE_PET || other.owner_player_id() != playerId ||
            other.competitive_temporary()) {
            continue;
        }
        auto* petSettlement = settlement.add_pets();
        petSettlement->set_pet_id(other.pet_id());  // 真实 pet_id;actor_id 只是局内号
        petSettlement->set_health(other.attributes().health());
        petSettlement->set_mana(other.attributes().mana());
        petSettlement->set_is_dead(other.is_dead());
    }

    settlement.set_use_free_preset(UsesFreePreset());
    if (IsFestivalBeastBattle()) {
        *settlement.mutable_festival_beast() = createRequest.activity_context().festival_beast();
    }
    if (hasTowerContext) {
        auto* towerSettle = settlement.mutable_tower_settlement();
        towerSettle->set_week_key(towerContext.week_key());
        towerSettle->set_run_id(towerContext.run_id());
        towerSettle->set_floor(towerContext.floor());
        towerSettle->set_total_rounds(CompletedRounds());
        towerSettle->set_realm_version(towerContext.realm_version());
        towerSettle->set_node_kind(towerContext.node_kind());
        towerSettle->set_use_free_preset(towerContext.use_free_preset());
    }
    if (IsRankedBattle()) {
        settlement.set_competitive_ranked(true);
        settlement.set_season_id(createRequest.ranked_context().season_id());
        settlement.set_ranked_rule_version(createRequest.ranked_context().rule_version());
        settlement.set_participated(DidPlayerParticipate(playerId));
    }
    if (createRequest.has_jianghu_duel_context()) {
        *settlement.mutable_jianghu_duel() = createRequest.jianghu_duel_context();
    }

    // 奖励已由 RollRewards 终局冻结；这里只带出实际击杀事实，读取结算不查配置、不掷随机。
    // 瑞兽迎春与五行幻境爬塔关闭常规任务击杀/掉落收益，不附带 defeated_monsters。
    if (IsPveMatch() && !IsFestivalBeastBattle() && !hasTowerContext && outcome == BATTLE_OUTCOME_SIDE_A_WIN &&
        actor->team_index() == 0 && IsActorActive(*actor)) {
        for (const auto& defeat : defeatedMonsters) {
            *settlement.add_defeated_monsters() = defeat;
        }
    }

    // 五行试炼志 V2：冻结权威战法事实与轻量阵容快照（不查配置表、不掷随机）。
    if (createRequest.has_trial_context() && createRequest.trial_context().journal_entry_id() != 0) {
        const auto& ctx = createRequest.trial_context();
        auto* facts = settlement.mutable_trial_facts();
        facts->set_journal_entry_id(ctx.journal_entry_id());
        facts->set_trial_rule_version(ctx.trial_rule_version());
        facts->set_season_id(ctx.season_id());

        uint32_t humanCount = 0;
        std::vector<std::pair<uint64_t, std::string>> teammates;
        for (const auto& snapshot : createRequest.players()) {
            if (snapshot.team_index() != actor->team_index()) {
                continue;
            }
            ++humanCount;
            if (snapshot.player_id() == playerId) {
                uint32_t livePetTableId = 0;
                for (const auto& candidate : actors) {
                    if (candidate.actor_type() == BATTLE_ACTOR_TYPE_PET && candidate.owner_player_id() == playerId) {
                        livePetTableId = candidate.pet_table_id();
                        break;
                    }
                }
                if (livePetTableId != 0 && !UsesFreePreset()) {
                    facts->set_used_pet_table_id(livePetTableId);
                } else if (!HasTrialVow(trialjournal::kVowNoPet) && !snapshot.pets().empty()) {
                    const auto& pet = snapshot.pets(0);
                    facts->set_used_pet_table_id(pet.combat_pet_table_id() != 0 ? pet.combat_pet_table_id()
                                                                                : pet.pet_table_id());
                }
            } else {
                teammates.emplace_back(snapshot.player_id(), snapshot.player_name());
            }
        }
        facts->set_initial_human_count(humanCount);
        std::sort(teammates.begin(), teammates.end(),
                  [](const auto& lhs, const auto& rhs) { return lhs.first < rhs.first; });
        const size_t maxNames = std::min<size_t>(teammates.size(), trialjournal::kMaxCoopTeammates);
        for (size_t i = 0; i < maxNames; ++i) {
            if (!teammates[i].second.empty()) {
                facts->add_teammate_names(teammates[i].second);
            }
        }

        bool selfOrPetDied = everDiedActors.contains(playerId);
        bool guardSurvived = false;
        if (protectedGuardActorId != 0) {
            const auto* protectedGuard = FindActor(protectedGuardActorId);
            guardSurvived = protectedGuard != nullptr && !protectedGuard->is_dead();
        }
        for (const auto& other : actors) {
            if (other.actor_type() == BATTLE_ACTOR_TYPE_PET && other.owner_player_id() == playerId &&
                everDiedActors.contains(other.actor_id())) {
                selfOrPetDied = true;
            }
            if (protectedGuardActorId == 0 &&
                other.actor_type() == BATTLE_ACTOR_TYPE_MONSTER && other.team_index() != actor->team_index() &&
                !other.is_boss() && !other.is_dead() &&
                (ctx.guard_monster_table_id() == 0 || other.monster_table_id() == ctx.guard_monster_table_id())) {
                guardSurvived = true;
            }
        }
        const uint32_t myTeam = actor->team_index();
        facts->set_self_or_pet_ever_died(selfOrPetDied);
        facts->set_boss_guard_heal_total(bossGuardHealTotal);
        facts->set_guard_stance_dispelled(guardStanceDispelled);
        facts->set_team_dealt_physical_direct_damage(myTeam < 2 && teamDealtPhysicalDirectDamage[myTeam]);
        facts->set_team_dealt_magic_direct_damage(myTeam < 2 && teamDealtMagicDirectDamage[myTeam]);
        facts->set_guard_survived(guardSurvived);
        facts->set_self_triggered_shield_break(shieldBreakPlayers.contains(playerId));
        facts->set_self_triggered_anti_control_or_season_skill(
            antiControlOrSeasonSkillPlayers.contains(playerId));
        facts->set_cleave_parry_count(cleaveParryCount);
        facts->set_heal_interrupt_backlash_count(healInterruptBacklashCount);
        facts->set_stance_break_count(stanceBreakCount);
        const auto targetedIt = selfCleavesTargetedByPlayer.find(playerId);
        const auto parriedIt = selfCleavesParriedByPlayer.find(playerId);
        const uint32_t selfTargeted = targetedIt != selfCleavesTargetedByPlayer.end() ? targetedIt->second : 0u;
        const uint32_t selfParried = parriedIt != selfCleavesParriedByPlayer.end() ? parriedIt->second : 0u;
        facts->set_self_parried_cleave_at_least_once(selfParried > 0u);
        facts->set_all_self_cleaves_parried(selfTargeted > 0u && selfParried == selfTargeted);
        facts->set_boss_wind_hit_unprotected_death(bossWindHitUnprotectedDeath);
        facts->set_dealt_yang_bell_physical_damage(dealtYangBellPhysicalDamage);
        facts->set_dealt_yin_drum_magic_damage(dealtYinDrumMagicDamage);
        facts->set_finished_in_ebb_tide(bossTrialKind == kBossTrialKindWildTide &&
                                        outcome == BATTLE_OUTCOME_SIDE_A_WIN &&
                                        IsWildTideEbbRound(roundIndex));
        facts->set_challenge_vow_mask(trialVowMask);
        facts->set_self_cast_season_skill(castSeasonSkillPlayers.contains(playerId));
        const auto resMaskIt = seasonResonanceMaskByPlayer.find(playerId);
        const uint32_t resMask = resMaskIt != seasonResonanceMaskByPlayer.end() ? resMaskIt->second : 0u;
        facts->set_self_triggered_season_resonance(resMask != 0u);
        facts->set_self_season_resonance_mask(resMask);

        std::vector<uint32_t> artifactEffects;
        for (const auto& artifact : actor->equipment_effects().artifacts()) {
            if (artifact.effect_id() >= trialjournal::kMinArtifactEffectId &&
                artifact.effect_id() <= trialjournal::kMaxArtifactEffectId) {
                artifactEffects.push_back(artifact.effect_id());
            }
        }
        std::sort(artifactEffects.begin(), artifactEffects.end());
        if (artifactEffects.size() > trialjournal::kMaxArtifactSlots) {
            artifactEffects.resize(trialjournal::kMaxArtifactSlots);
        }
        for (const uint32_t effectId : artifactEffects) {
            facts->add_used_artifact_effect_ids(effectId);
        }
        if (actor->equipment_effects().has_season_seal()) {
            facts->set_used_season_seal_id(actor->equipment_effects().season_seal().seal_id());
        }
    }
    if (hasTacticalPuzzleContext) {
        *settlement.mutable_tactical_puzzle_context() = tacticalPuzzleContext;
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
        auto* visible = state.add_actors();
        *visible = actor;
        if (actor.actor_type() == BATTLE_ACTOR_TYPE_PLAYER)
            BuildSupportEffects(actor, *visible->mutable_support_effects());
    }

    if (activePolarity != 0) {
        state.set_active_polarity(activePolarity);
        state.set_next_polarity_switch_round(NextPolaritySwitchRound());
    }
    if (protectedGuardActorId != 0) {
        state.set_protected_guard_actor_id(protectedGuardActorId);
    }
    if (bossTrialKind != kBossTrialKindNone) {
        state.set_boss_trial_kind(bossTrialKind);
        state.set_battle_difficulty(battleDifficulty);
    }
    if (IsBossTrialKind(bossTrialKind)) {
        state.set_boss_phase(bossPhase);
        state.set_boss_momentum_stacks(bossMomentumStacks);
        if (bossStanceType != 0 && bossStanceRemainRounds > 0) {
            state.set_boss_stance_type(bossStanceType);
            state.set_boss_stance_remain_rounds(bossStanceRemainRounds);
        }
        for (const auto& a : actors) {
            if (a.actor_type() == BATTLE_ACTOR_TYPE_MONSTER && a.team_index() == 1 && a.is_boss()) {
                if (const auto it = exposedActors.find(a.actor_id());
                    it != exposedActors.end() && it->second.untilRound >= roundIndex) {
                    state.set_boss_exposed_pct(it->second.pct);
                    state.set_boss_exposed_until_round(it->second.untilRound);
                }
                break;
            }
        }
    }
    state.set_round_limit(maxRounds);
    if (const uint32_t escStart = EscalationStartRound(); escStart > 0) {
        state.set_escalation_start_round(escStart);
        state.set_escalation_stacks(EscalationStacks());
    }
    if (const uint32_t surrenderUnlock = SurrenderUnlockRound(); surrenderUnlock > 0) {
        state.set_surrender_unlock_round(surrenderUnlock);
    }
    if (const uint32_t summonUnlock = SummonPetUnlockRound(); summonUnlock > 0u) {
        state.set_summon_pet_unlock_round(summonUnlock);
    }
    if (createRequest.has_tournament_context() && createRequest.tournament_context().match_id() != 0) {
        *state.mutable_tournament_context() = createRequest.tournament_context();
    }
    if (teamAFormationId != 0u) {
        state.set_team_a_formation_id(teamAFormationId);
    }
    if (teamBFormationId != 0u) {
        state.set_team_b_formation_id(teamBFormationId);
    }
    if (hasTowerContext) {
        *state.mutable_tower_context() = towerContext;
    }
    if (hasCustomDuelRules) {
        *state.mutable_custom_duel_rules() = customDuelRules;
    }
    if (wuxingSynergyActiveThisRound) {
        state.set_wuxing_synergy_active_this_round(true);
    }
    if (bossWeeklyAffixId != bossaffix::kAffixNone) {
        state.set_boss_weekly_affix_id(bossWeeklyAffixId);
    }
    if (activeSeasonWeeklyAffixId != 0u) {
        state.set_season_weekly_affix_id(activeSeasonWeeklyAffixId);
    }
    if (trialVowMask != 0u) {
        state.set_trial_vow_mask(trialVowMask);
    }
    if (hasTacticalPuzzleContext) {
        *state.mutable_tactical_puzzle_context() = tacticalPuzzleContext;
    }
    for (const auto skillId : bannedSkillIds) {
        state.add_banned_skill_ids(skillId);
    }
    for (const auto skillId : teamABannedSkillIds) {
        state.add_team_a_banned_skill_ids(skillId);
    }
    for (const auto skillId : teamBBannedSkillIds) {
        state.add_team_b_banned_skill_ids(skillId);
    }
    if (teamAProtectedSkillId != 0u) {
        state.set_team_a_protected_skill_id(teamAProtectedSkillId);
    }
    if (teamBProtectedSkillId != 0u) {
        state.set_team_b_protected_skill_id(teamBProtectedSkillId);
    }
    for (const auto skillId : counteredBanSkillIds) {
        state.add_countered_ban_skill_ids(skillId);
    }

    for (const auto& resolved : lastResolvedTelegraphs) {
        *state.add_last_resolved_telegraphs() = resolved;
    }

    if (outcome == BATTLE_OUTCOME_ONGOING) {
        for (const auto& entry : activeTelegraphs) {
            *state.add_telegraphs() = entry;
        }
        if (prepActive) {
            state.mutable_prep_state()->set_active(true);
            state.mutable_prep_state()->set_duration_seconds(PrepDurationSeconds());
            state.mutable_prep_state()->set_skill_ban_slots_per_team(prepSkillBanSlotsPerTeam);
            state.mutable_prep_state()->set_skill_protect_slots_per_team(prepSkillProtectSlotsPerTeam);
        } else {
            for (const auto& actor : actors) {
                if ((actor.actor_type() != BATTLE_ACTOR_TYPE_PLAYER && actor.actor_type() != BATTLE_ACTOR_TYPE_PET) || !IsActorActive(actor)) {
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
    }
    return state;
}

void TurnBattleEngine::StripEngineOnlyState(BattleStateS2C& state) {
    // 不分视角一律清空:对手的装备概率不能暴露(知道对面 10% 反震就能算着打),
    // 自己的在属性面板上本来就看得到,战斗状态里再带一份只是多一个泄漏面
    for (auto& actor : *state.mutable_actors()) {
        actor.clear_combat();
        actor.clear_ability_use_counts();
        actor.clear_round_effect_usage();
        actor.clear_runtime_skills();
        actor.clear_equipment_effects();
        actor.clear_pet_cultivation();
        actor.clear_artifact_states();
        actor.clear_season_seal_state();
        actor.clear_entry_max_health();
        actor.clear_entry_max_mana();
    }
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

BattleItemEntry* TurnBattleEngine::FindItemEntry(uint64_t playerId, uint32_t itemTableId) {
    // 剩余数的唯一真相是开局请求副本:ExecuteItem 在它上面原地递减,
    // BattleActorState 不承载道具(道具属于玩家资产,不是战斗单位状态)
    for (auto& snapshot : *createRequest.mutable_players()) {
        if (snapshot.player_id() != playerId) {
            continue;
        }
        for (auto& item : *snapshot.mutable_items()) {
            if (item.item_table_id() == itemTableId && item.count() > 0) {
                return &item;
            }
        }
        return nullptr;  // 本人快照已找到,不必再扫别人
    }
    return nullptr;
}

const BattleItemEntry* TurnBattleEngine::FindItemEntry(uint64_t playerId,
                                                       uint32_t itemTableId) const {
    for (const auto& snapshot : createRequest.players()) {
        if (snapshot.player_id() != playerId) {
            continue;
        }
        for (const auto& item : snapshot.items()) {
            if (item.item_table_id() == itemTableId && item.count() > 0) {
                return &item;
            }
        }
        return nullptr;
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
        maxSpeed = std::max(maxSpeed, EffectiveSpeed(other));
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
    // 进行中或回合收令期投降终局:roundIndex 是尚未结算的回合;其余已结束停在最后结算的回合
    if (outcome == BATTLE_OUTCOME_ONGOING || surrenderedTeam.has_value()) {
        return roundIndex > 0 ? roundIndex - 1 : 0;
    }
    return roundIndex;
}

std::optional<uint32_t> TurnBattleEngine::TeamIndexOfPlayer(uint64_t playerId) const {
    const auto* actor = FindActor(playerId);
    if (actor == nullptr || actor->actor_type() != BATTLE_ACTOR_TYPE_PLAYER || actor->team_index() > 1) {
        return std::nullopt;
    }
    return actor->team_index();
}

void TurnBattleEngine::ExpireSurrenderVotesForRoundAdvance() {
    for (uint32_t team = 0; team < 2; ++team) {
        auto& vote = teamSurrenderVotes[team];
        if (!vote.active()) {
            continue;
        }
        vote.set_active(false);
        vote.set_rejected(true);
        vote.set_passed(false);
        vote.clear_agreed_player_ids();
        vote.set_next_allowed_round(roundIndex + kPvpSurrenderExpireCooldownRounds);
    }
}

uint32_t TurnBattleEngine::SubmitSurrenderVote(uint64_t playerId, bool agree,
                                               BattleSurrenderVoteState* outVoteState) {
    if (!initialized || outcome != BATTLE_OUTCOME_ONGOING) {
        return kInvalidParameter;
    }
    if (prepActive) {
        return kFeatureUnavailable;
    }
    if (IsPveMatch()) {
        return kMatchBattleSurrenderNotAllowed;
    }
    const auto* actor = FindActor(playerId);
    if (actor == nullptr || actor->actor_type() != BATTLE_ACTOR_TYPE_PLAYER || actor->team_index() > 1) {
        return kInvalidParameter;
    }
    if (roundIndex < kPvpSurrenderUnlockRound) {
        return kMatchBattleSurrenderTooEarly;
    }

    const uint32_t teamIdx = actor->team_index();
    auto& vote = teamSurrenderVotes[teamIdx];

    if (!vote.active()) {
        if (!agree) {
            return kInvalidParameter;
        }
        if (vote.next_allowed_round() > 0 && roundIndex < vote.next_allowed_round()) {
            return kMatchBattleSurrenderCooldown;
        }
        uint32_t activeVoters = 0;
        for (const auto& member : actors) {
            if (member.actor_type() != BATTLE_ACTOR_TYPE_PLAYER || member.team_index() != teamIdx) {
                continue;
            }
            const auto idleIt = consecutiveIdleRoundsByPlayer.find(member.actor_id());
            const uint32_t idleRounds =
                idleIt != consecutiveIdleRoundsByPlayer.end() ? idleIt->second : 0u;
            if (member.actor_id() == playerId || idleRounds < kPvpSurrenderIdleExcludeRounds) {
                ++activeVoters;
            }
        }
        vote.Clear();
        vote.set_active(true);
        vote.set_team_index(teamIdx);
        vote.set_initiator_player_id(playerId);
        vote.set_initiated_round(roundIndex);
        vote.add_agreed_player_ids(playerId);
        vote.set_required_agrees(std::max<uint32_t>(1u, activeVoters));
    } else {
        for (const uint64_t agreedId : vote.agreed_player_ids()) {
            if (agreedId == playerId) {
                return kMatchBattleSurrenderAlreadyVoted;
            }
        }
        if (!agree) {
            vote.set_active(false);
            vote.set_rejected(true);
            vote.set_passed(false);
            vote.clear_agreed_player_ids();
            vote.set_next_allowed_round(roundIndex + kPvpSurrenderRejectCooldownRounds);
            if (outVoteState != nullptr) {
                *outVoteState = vote;
            }
            return 0u;
        }
        vote.add_agreed_player_ids(playerId);
        auto* ids = vote.mutable_agreed_player_ids();
        std::sort(ids->begin(), ids->end());
    }

    if (vote.active() && static_cast<uint32_t>(vote.agreed_player_ids_size()) >= vote.required_agrees()) {
        vote.set_active(false);
        vote.set_passed(true);
        vote.set_rejected(false);
        surrenderedTeam = teamIdx;
        outcome = (teamIdx == 0u) ? BATTLE_OUTCOME_SIDE_B_WIN : BATTLE_OUTCOME_SIDE_A_WIN;
        activeTelegraphs.clear();
        RollRewards();
    }

    if (outVoteState != nullptr) {
        *outVoteState = vote;
    }
    return 0u;
}

void TurnBattleEngine::FillSurrenderVoteForViewer(BattleStateS2C& state, uint64_t viewerPlayerId) const {
    state.clear_surrender_vote();
    const auto team = TeamIndexOfPlayer(viewerPlayerId);
    if (!team.has_value()) {
        return;
    }
    const auto& vote = teamSurrenderVotes[*team];
    if (vote.active() || vote.passed() || vote.rejected() ||
        (vote.next_allowed_round() > 0 && roundIndex < vote.next_allowed_round())) {
        *state.mutable_surrender_vote() = vote;
    }
}

bool TurnBattleEngine::InitTacticalPrep(const CreateBattleRequest& request) {
    prepActive = false;
    prepSkillBanSlotsPerTeam = 0;
    prepSkillProtectSlotsPerTeam = 0;
    banCandidateSkillIds.clear();
    banCandidateSkillSet.clear();
    teamABannedSkillIds.clear();
    teamBBannedSkillIds.clear();
    teamAProtectedSkillId = 0;
    teamBProtectedSkillId = 0;
    counteredBanSkillIds.clear();
    bannedSkillIds.clear();
    bannedSkillSet.clear();
    prepOptionsByPlayer.clear();
    validatedPetsByPlayer.clear();
    standbyPetPoolByPlayer.clear();
    summonPetRemainingByPlayer.clear();

    // 正式自养论剑与赛季战队锦标赛开启战前明牌变阵（换印/换宠 + 可选 3 Ban）；
    // 道友切磋坊 1V1 开启 3 Ban 时仅跑 25 秒纯禁招（零 scene 快照改动）；其余模式零打扰。
    const bool isFormalCompetitive =
        !IsPveMatch() && !UsesFreePreset() &&
        (IsRankedBattle() || request.match_mode() == kMatchModeSeasonTournament);
    uint32_t requestedBanSlots = 0u;
    if (!IsPveMatch() && !UsesFreePreset()) {
        if (request.match_mode() == kMatchModeSeasonTournament && request.has_tournament_context()) {
            requestedBanSlots = request.tournament_context().skill_ban_slots_per_team();
        } else if (IsRankedBattle() && request.has_ranked_context()) {
            requestedBanSlots = request.ranked_context().skill_ban_slots_per_team();
        } else if (hasCustomDuelRules && !customDuelRules.use_free_preset()) {
            requestedBanSlots = customDuelRules.skill_ban_slots_per_team();
        }
    }
    if (requestedBanSlots > kPrepSkillBanSlotsPerTeam) {
        LOG_ERROR << "CreateBattle invalid skill_ban_slots_per_team: battle_id=" << request.battle_id()
                  << " slots=" << requestedBanSlots;
        return false;
    }
    const bool isCustomDuelBan = !isFormalCompetitive && requestedBanSlots > 0u;
    if (!isFormalCompetitive && !isCustomDuelBan) {
        return true;
    }

    if (requestedBanSlots > 0u) {
        uint32_t battleActiveSeasonId = 0u;
        for (const auto& snapshot : request.players()) {
            if (snapshot.active_season_id() != 0u) {
                battleActiveSeasonId = snapshot.active_season_id();
                break;
            }
        }
        const auto rawCandidates = dataProvider->GetBanEligibleSkillIds(battleActiveSeasonId);
        for (const auto skillId : rawCandidates) {
            const auto* row = dataProvider->FindSkill(skillId);
            if (row == nullptr || row->battle_model() != 1u || row->ability_kind() != 1u ||
                row->ban_pool_category() < 1u || row->ban_pool_category() > 4u) {
                continue;
            }
            if (row->season_id() != 0u &&
                (battleActiveSeasonId == 0u || row->season_id() != battleActiveSeasonId)) {
                continue;
            }
            if (IsControlBannedByCustomDuel() && IsControlSkill(*row)) {
                continue;
            }
            if (banCandidateSkillSet.insert(skillId).second) {
                banCandidateSkillIds.push_back(skillId);
            }
        }
        std::sort(banCandidateSkillIds.begin(), banCandidateSkillIds.end());
        if (!banCandidateSkillIds.empty()) {
            prepSkillBanSlotsPerTeam = requestedBanSlots;
            prepSkillProtectSlotsPerTeam = kPrepSkillProtectSlotsPerTeam;
        }
    }

    std::set<uint64_t> allBattlePetIds;
    for (const auto& actor : actors) {
        if (actor.actor_type() == BATTLE_ACTOR_TYPE_PET && actor.pet_id() != 0) {
            allBattlePetIds.insert(actor.pet_id());
        }
    }

    bool hasAnyChoice = false;
    for (const auto& snapshot : request.players()) {
        PrepPlayerOptions opt;
        opt.teamIndex = snapshot.team_index();
        opt.activeSeasonId = snapshot.active_season_id();
        opt.aiControlled = snapshot.ai_controlled();
        opt.isTeamCaptain = snapshot.is_team_captain();
        opt.ready = snapshot.ai_controlled();

        if (isFormalCompetitive) {
            std::set<uint32_t> seenSealIds;
            if (snapshot.has_equipment_effects() && snapshot.equipment_effects().has_season_seal()) {
                const auto& equippedSeal = snapshot.equipment_effects().season_seal();
                opt.selectedSealId = equippedSeal.seal_id();
                if (seenSealIds.insert(equippedSeal.seal_id()).second) {
                    opt.availableSeals.push_back(equippedSeal);
                }
            }
            for (const auto& seal : snapshot.available_season_seals()) {
                if (!seasonrules::ValidSealDefinition(seal.season_id(), seal.seal_id(), seal.effect_id(),
                                                      seal.bonus_bps(), seal.cap_bps(), seal.per_round_limit(),
                                                      seal.battle_limit(), seal.active_rounds(),
                                                      seal.health_threshold_bps(),
                                                      seal.resonance_skill_id(), seal.resonance_kind(),
                                                      seal.resonance_param())) {
                    LOG_ERROR << "CreateBattle invalid available_season_seal: battle_id=" << request.battle_id()
                              << " player_id=" << snapshot.player_id() << " seal_id=" << seal.seal_id();
                    return false;
                }
                if (snapshot.active_season_id() != 0 && seal.season_id() != snapshot.active_season_id()) {
                    LOG_ERROR << "CreateBattle available_season_seal season mismatch: battle_id=" << request.battle_id()
                              << " player_id=" << snapshot.player_id() << " seal_season=" << seal.season_id()
                              << " active_season=" << snapshot.active_season_id();
                    return false;
                }
                if (seenSealIds.insert(seal.seal_id()).second) {
                    opt.availableSeals.push_back(seal);
                }
            }
            std::sort(opt.availableSeals.begin(), opt.availableSeals.end(),
                      [](const SeasonSealEffect& lhs, const SeasonSealEffect& rhs) {
                          return lhs.seal_id() < rhs.seal_id();
                      });

            if (!snapshot.pets().empty()) {
                opt.availablePets.push_back(snapshot.pets(0));
                opt.selectedPetId = snapshot.pets(0).pet_id();
            }
            for (const auto& standby : snapshot.standby_pets()) {
                if (standby.competitive_temporary() || standby.pet_id() == 0) {
                    LOG_ERROR << "CreateBattle invalid standby pet id or temporary flag: battle_id="
                              << request.battle_id() << " player_id=" << snapshot.player_id();
                    return false;
                }
                if (standby.owner_player_id() != 0 && standby.owner_player_id() != snapshot.player_id()) {
                    LOG_ERROR << "CreateBattle standby pet ownership mismatch: battle_id=" << request.battle_id()
                              << " player_id=" << snapshot.player_id() << " pet_id=" << standby.pet_id()
                              << " owner=" << standby.owner_player_id();
                    return false;
                }
                if (!petcultivation::ValidSnapshot(standby.pet_cultivation())) {
                    LOG_ERROR << "CreateBattle invalid standby pet_cultivation: battle_id=" << request.battle_id()
                              << " player_id=" << snapshot.player_id() << " pet_id=" << standby.pet_id();
                    return false;
                }
                if (standby.pet_id() == opt.selectedPetId) {
                    continue;
                }
                if (!allBattlePetIds.insert(standby.pet_id()).second) {
                    LOG_ERROR << "CreateBattle duplicate standby pet_id across battle: battle_id=" << request.battle_id()
                              << " player_id=" << snapshot.player_id() << " pet_id=" << standby.pet_id();
                    return false;
                }
                opt.availablePets.push_back(standby);
            }
        }

        if (!opt.aiControlled &&
            (prepSkillBanSlotsPerTeam > 0u ||
             opt.availableSeals.size() > 1 || opt.availablePets.size() > 1 ||
             (opt.selectedPetId == 0 && !opt.availablePets.empty()))) {
            hasAnyChoice = true;
        }
        if (isFormalCompetitive && !opt.aiControlled && !opt.availablePets.empty()) {
            validatedPetsByPlayer[snapshot.player_id()] = opt.availablePets;
        }
        prepOptionsByPlayer.emplace(snapshot.player_id(), std::move(opt));
    }

    if (hasAnyChoice) {
        prepActive = true;
    } else {
        prepSkillBanSlotsPerTeam = 0;
        prepSkillProtectSlotsPerTeam = 0;
        banCandidateSkillIds.clear();
        banCandidateSkillSet.clear();
        prepOptionsByPlayer.clear();
    }
    return true;
}

std::vector<uint32_t> TurnBattleEngine::AggregateTeamBans(uint32_t teamIndex) const {
    std::vector<uint32_t> result;
    if (prepSkillBanSlotsPerTeam == 0u) {
        return result;
    }
    struct BanVoteEntry {
        uint32_t skillId = 0;
        uint32_t votes = 0;
        uint32_t bestSlotRank = UINT32_MAX;
        bool nominatedByCaptain = false;
    };
    std::map<uint32_t, BanVoteEntry> tally;
    for (const auto& [playerId, opt] : prepOptionsByPlayer) {
        (void)playerId;
        if (opt.aiControlled || opt.teamIndex != teamIndex) {
            continue;
        }
        for (size_t idx = 0; idx < opt.nominatedBanSkillIds.size(); ++idx) {
            const uint32_t sid = opt.nominatedBanSkillIds[idx];
            auto& entry = tally[sid];
            entry.skillId = sid;
            ++entry.votes;
            const auto rank = static_cast<uint32_t>(idx);
            if (rank < entry.bestSlotRank) {
                entry.bestSlotRank = rank;
            }
            if (opt.isTeamCaptain) {
                entry.nominatedByCaptain = true;
            }
        }
    }
    if (tally.empty()) {
        return result;
    }
    std::vector<BanVoteEntry> ordered;
    ordered.reserve(tally.size());
    for (const auto& [sid, entry] : tally) {
        (void)sid;
        ordered.push_back(entry);
    }
    const uint64_t seed = createRequest.seed();
    std::sort(ordered.begin(), ordered.end(), [seed](const BanVoteEntry& lhs, const BanVoteEntry& rhs) {
        if (lhs.votes != rhs.votes) return lhs.votes > rhs.votes;
        if (lhs.bestSlotRank != rhs.bestSlotRank) return lhs.bestSlotRank < rhs.bestSlotRank;
        if (lhs.nominatedByCaptain != rhs.nominatedByCaptain) return lhs.nominatedByCaptain;
        const uint64_t lh = SplitMix64(seed ^ static_cast<uint64_t>(lhs.skillId));
        const uint64_t rh = SplitMix64(seed ^ static_cast<uint64_t>(rhs.skillId));
        if (lh != rh) return lh < rh;
        return lhs.skillId < rhs.skillId;
    });
    const size_t limit = std::min(ordered.size(), static_cast<size_t>(prepSkillBanSlotsPerTeam));
    result.reserve(limit);
    for (size_t i = 0; i < limit; ++i) {
        result.push_back(ordered[i].skillId);
    }
    return result;
}

uint32_t TurnBattleEngine::AggregateTeamProtect(uint32_t teamIndex) const {
    if (prepSkillProtectSlotsPerTeam == 0u) {
        return 0u;
    }
    struct ProtectVoteEntry {
        uint32_t skillId = 0;
        uint32_t votes = 0;
        bool nominatedByCaptain = false;
    };
    std::map<uint32_t, ProtectVoteEntry> tally;
    for (const auto& [playerId, opt] : prepOptionsByPlayer) {
        (void)playerId;
        if (opt.aiControlled || opt.teamIndex != teamIndex || opt.nominatedProtectSkillId == 0u) {
            continue;
        }
        auto& entry = tally[opt.nominatedProtectSkillId];
        entry.skillId = opt.nominatedProtectSkillId;
        ++entry.votes;
        if (opt.isTeamCaptain) {
            entry.nominatedByCaptain = true;
        }
    }
    if (tally.empty()) {
        return 0u;
    }
    std::vector<ProtectVoteEntry> ordered;
    ordered.reserve(tally.size());
    for (const auto& [sid, entry] : tally) {
        (void)sid;
        ordered.push_back(entry);
    }
    const uint64_t seed = createRequest.seed();
    std::sort(ordered.begin(), ordered.end(), [seed](const ProtectVoteEntry& lhs, const ProtectVoteEntry& rhs) {
        if (lhs.votes != rhs.votes) return lhs.votes > rhs.votes;
        if (lhs.nominatedByCaptain != rhs.nominatedByCaptain) return lhs.nominatedByCaptain;
        const uint64_t lh = SplitMix64(seed ^ kPrepProtectHashSalt ^ static_cast<uint64_t>(lhs.skillId));
        const uint64_t rh = SplitMix64(seed ^ kPrepProtectHashSalt ^ static_cast<uint64_t>(rhs.skillId));
        if (lh != rh) return lh < rh;
        return lhs.skillId < rhs.skillId;
    });
    return ordered.front().skillId;
}

bool TurnBattleEngine::AllPrepPlayersReady() const {
    if (!prepActive) {
        return true;
    }
    for (const auto& [playerId, opt] : prepOptionsByPlayer) {
        (void)playerId;
        if (!opt.aiControlled && !opt.ready) {
            return false;
        }
    }
    return true;
}

uint32_t TurnBattleEngine::UpdateBattlePrep(uint64_t playerId, uint32_t selectedSealId,
                                            uint64_t selectedPetId, bool ready,
                                            BattlePrepState* outPrepState,
                                            bool updateBannedSkills,
                                            const std::vector<uint32_t>& bannedSkillIdsInput,
                                            bool updateProtectedSkill,
                                            uint32_t protectedSkillIdInput) {
    if (!initialized || outcome != BATTLE_OUTCOME_ONGOING || !prepActive) {
        return kFeatureUnavailable;
    }
    auto it = prepOptionsByPlayer.find(playerId);
    if (it == prepOptionsByPlayer.end() || it->second.aiControlled) {
        return kInvalidParameter;
    }
    auto& opt = it->second;

    if (opt.ready) {
        const bool sealChanged = selectedSealId != 0 && selectedSealId != opt.selectedSealId;
        const bool petChanged = selectedPetId != 0 && selectedPetId != opt.selectedPetId;
        const bool banChanged = updateBannedSkills && bannedSkillIdsInput != opt.nominatedBanSkillIds;
        const bool protectChanged = updateProtectedSkill && protectedSkillIdInput != opt.nominatedProtectSkillId;
        if (sealChanged || petChanged || banChanged || protectChanged) {
            return kFeatureUnavailable;
        }
        if (outPrepState != nullptr) {
            PopulatePrepStateForViewer(*outPrepState, playerId, 0);
        }
        return 0u;
    }

    if (selectedSealId != 0) {
        const bool found = std::any_of(opt.availableSeals.begin(), opt.availableSeals.end(),
                                       [selectedSealId](const SeasonSealEffect& seal) {
                                           return seal.seal_id() == selectedSealId;
                                       });
        if (!found) {
            return kInvalidParameter;
        }
    }
    if (selectedPetId != 0) {
        const bool found = std::any_of(opt.availablePets.begin(), opt.availablePets.end(),
                                       [selectedPetId](const BattlePetSnapshot& pet) {
                                           return pet.pet_id() == selectedPetId;
                                       });
        if (!found) {
            return kInvalidParameter;
        }
    }
    if (updateBannedSkills) {
        if (prepSkillBanSlotsPerTeam == 0u) {
            if (!bannedSkillIdsInput.empty()) {
                return kFeatureUnavailable;
            }
        } else {
            if (bannedSkillIdsInput.size() > prepSkillBanSlotsPerTeam) {
                return kInvalidParameter;
            }
            std::set<uint32_t> seenBans;
            for (const auto sid : bannedSkillIdsInput) {
                if (!banCandidateSkillSet.contains(sid) || !seenBans.insert(sid).second) {
                    return kInvalidParameter;
                }
            }
        }
    }
    if (updateProtectedSkill) {
        if (prepSkillProtectSlotsPerTeam == 0u) {
            if (protectedSkillIdInput != 0u) {
                return kFeatureUnavailable;
            }
        } else if (protectedSkillIdInput != 0u && !banCandidateSkillSet.contains(protectedSkillIdInput)) {
            return kInvalidParameter;
        }
    }
    const auto& resultingBans = updateBannedSkills ? bannedSkillIdsInput : opt.nominatedBanSkillIds;
    const uint32_t resultingProtect = updateProtectedSkill ? protectedSkillIdInput : opt.nominatedProtectSkillId;
    if (resultingProtect != 0u &&
        std::find(resultingBans.begin(), resultingBans.end(), resultingProtect) != resultingBans.end()) {
        return kInvalidParameter;
    }

    if (selectedSealId != 0) {
        opt.selectedSealId = selectedSealId;
    }
    if (selectedPetId != 0) {
        opt.selectedPetId = selectedPetId;
    }
    if (updateBannedSkills) {
        opt.nominatedBanSkillIds = bannedSkillIdsInput;
    }
    if (updateProtectedSkill) {
        opt.nominatedProtectSkillId = protectedSkillIdInput;
    }
    if (ready) {
        opt.ready = true;
    }

    if (outPrepState != nullptr) {
        PopulatePrepStateForViewer(*outPrepState, playerId, 0);
    }
    return 0u;
}

bool TurnBattleEngine::FinalizePrepPhase() {
    if (!prepActive) {
        return false;
    }

    if (prepSkillBanSlotsPerTeam > 0u) {
        teamABannedSkillIds = AggregateTeamBans(0u);
        teamBBannedSkillIds = AggregateTeamBans(1u);
        teamAProtectedSkillId = prepSkillProtectSlotsPerTeam > 0u ? AggregateTeamProtect(0u) : 0u;
        teamBProtectedSkillId = prepSkillProtectSlotsPerTeam > 0u ? AggregateTeamProtect(1u) : 0u;
        bannedSkillSet.clear();
        bannedSkillIds.clear();
        counteredBanSkillIds.clear();
        std::set<uint32_t> counteredSet;
        for (const auto sid : teamABannedSkillIds) {
            if (teamBProtectedSkillId != 0u && sid == teamBProtectedSkillId) {
                counteredSet.insert(sid);
            } else {
                bannedSkillSet.insert(sid);
            }
        }
        for (const auto sid : teamBBannedSkillIds) {
            if (teamAProtectedSkillId != 0u && sid == teamAProtectedSkillId) {
                counteredSet.insert(sid);
            } else {
                bannedSkillSet.insert(sid);
            }
        }
        // 双边同禁仅单边护招时，另一方未受反制的禁招仍然生效，确保 countered 与 banned 严格互斥。
        for (const auto sid : bannedSkillSet) {
            counteredSet.erase(sid);
        }
        bannedSkillIds.assign(bannedSkillSet.begin(), bannedSkillSet.end());
        counteredBanSkillIds.assign(counteredSet.begin(), counteredSet.end());
    }

    for (const auto& [playerId, opt] : prepOptionsByPlayer) {
        // 1. 仅替换单槽赛季印机制（轻度气血/防御万分比由赛季进度决定，零面板漂移）。
        if (opt.selectedSealId != 0) {
            if (auto* actor = FindActor(playerId); actor != nullptr) {
                for (const auto& seal : opt.availableSeals) {
                    if (seal.seal_id() == opt.selectedSealId) {
                        if (!actor->equipment_effects().has_season_seal() ||
                            actor->equipment_effects().season_seal().seal_id() != opt.selectedSealId) {
                            *actor->mutable_equipment_effects()->mutable_season_seal() = seal;
                            actor->clear_season_seal_state();
                            seasonSealRuntimeByActor.erase(playerId);
                        }
                        break;
                    }
                }
            }
        }

        // 2. 替换或补发首发灵宠（原地保留局内 actor_id 与阵位，完整重建属性与技能）。
        if (opt.selectedPetId != 0) {
            const BattlePetSnapshot* chosenPet = nullptr;
            for (const auto& pet : opt.availablePets) {
                if (pet.pet_id() == opt.selectedPetId) {
                    chosenPet = &pet;
                    break;
                }
            }
            const BattlePlayerSnapshot* ownerSnap = nullptr;
            for (const auto& snap : createRequest.players()) {
                if (snap.player_id() == playerId) {
                    ownerSnap = &snap;
                    break;
                }
            }
            if (chosenPet != nullptr && ownerSnap != nullptr) {
                BattleActorState* existingPet = nullptr;
                for (auto& actor : actors) {
                    if (actor.actor_type() == BATTLE_ACTOR_TYPE_PET && actor.owner_player_id() == playerId) {
                        existingPet = &actor;
                        break;
                    }
                }
                if (existingPet != nullptr) {
                    if (existingPet->pet_id() != chosenPet->pet_id()) {
                        BattleActorState rebuilt;
                        if (BuildPetActor(*ownerSnap, *chosenPet, existingPet->actor_id(),
                                          existingPet->formation_slot(), rebuilt)) {
                            *existingPet = std::move(rebuilt);
                        }
                    }
                } else {
                    BattleActorState added;
                    if (BuildPetActor(*ownerSnap, *chosenPet, kPetActorIdBase + nextPetActorIndex,
                                      NextFormationSlot(opt.teamIndex), added)) {
                        ++nextPetActorIndex;
                        actors.emplace_back(std::move(added));
                    }
                }
            }
        }
    }

    prepActive = false;
    roundPingCountByPlayer.clear();
    RebuildMidBattleStandbyPools();
    // 在最终亮牌阵容上一次性初始化全部开场光环/成长被动，杜绝被换掉宠物的残留光环。
    InitializeConfiguredAbilities();
    return true;
}

void TurnBattleEngine::PopulatePetOption(const BattlePetSnapshot& pet, BattlePrepPetOption& dst) {
    dst.set_pet_id(pet.pet_id());
    dst.set_pet_table_id(pet.pet_table_id());
    dst.set_pet_name(pet.pet_name());
    dst.set_level(pet.level());
    dst.set_max_health(FallbackMax(pet.max_health(), pet.base_attributes().health()));
    dst.set_speed(pet.base_attributes().speed());
    dst.set_physical_attack(pet.physical_attack());
    dst.set_magic_attack(pet.magic_attack());
    dst.set_defense(pet.defense());
    for (const auto skillId : pet.skill_table_ids()) {
        dst.add_skill_table_ids(skillId);
    }
}

void TurnBattleEngine::RebuildMidBattleStandbyPools() {
    standbyPetPoolByPlayer.clear();
    summonPetRemainingByPlayer.clear();
    if (!IsSummonPetEnabled()) {
        return;
    }
    for (const auto& [playerId, allPets] : validatedPetsByPlayer) {
        uint64_t activePetId = 0;
        for (const auto& actor : actors) {
            if (actor.actor_type() == BATTLE_ACTOR_TYPE_PET && actor.owner_player_id() == playerId &&
                actor.pet_id() != 0) {
                activePetId = actor.pet_id();
                break;
            }
        }
        std::vector<BattlePetSnapshot> reserve;
        for (const auto& pet : allPets) {
            if (pet.pet_id() != 0 && pet.pet_id() != activePetId) {
                reserve.push_back(pet);
            }
        }
        if (!reserve.empty()) {
            standbyPetPoolByPlayer.emplace(playerId, std::move(reserve));
            summonPetRemainingByPlayer[playerId] = 1u;
        }
    }
}

uint32_t TurnBattleEngine::SummonPetRemaining(uint64_t playerId) const {
    if (prepActive || !IsSummonPetEnabled()) {
        return 0u;
    }
    const auto it = summonPetRemainingByPlayer.find(playerId);
    return it == summonPetRemainingByPlayer.end() ? 0u : it->second;
}

uint32_t TurnBattleEngine::CheckSummonPet(const BattleActorState& actor, uint64_t summonPetId) const {
    if (actor.actor_type() != BATTLE_ACTOR_TYPE_PLAYER) {
        return kInvalidParameter;
    }
    if (!IsSummonPetEnabled() || roundIndex < kPvpSummonPetUnlockRound) {
        return kFeatureUnavailable;
    }
    if (SummonPetRemaining(actor.actor_id()) == 0u) {
        return kFeatureUnavailable;
    }
    if (summonPetId == 0u) {
        return kInvalidParameter;
    }
    const auto poolIt = standbyPetPoolByPlayer.find(actor.actor_id());
    if (poolIt == standbyPetPoolByPlayer.end()) {
        return kFeatureUnavailable;
    }
    const bool inPool = std::any_of(poolIt->second.begin(), poolIt->second.end(),
                                    [summonPetId](const BattlePetSnapshot& pet) {
                                        return pet.pet_id() == summonPetId;
                                    });
    if (!inPool) {
        return kInvalidParameter;
    }
    for (const auto& other : actors) {
        if (other.actor_type() == BATTLE_ACTOR_TYPE_PET && other.owner_player_id() == actor.actor_id() &&
            other.pet_id() == summonPetId) {
            return kInvalidParameter;
        }
    }
    return kSuccess;
}

void TurnBattleEngine::ExecuteSummonPet(BattleActorState& actor, uint64_t summonPetId,
                                        TurnResultS2C& result) {
    if (CheckSummonPet(actor, summonPetId) != kSuccess) {
        return;
    }
    const uint64_t ownerId = actor.actor_id();
    const uint32_t ownerTeam = actor.team_index();

    const BattlePlayerSnapshot* ownerSnap = nullptr;
    for (const auto& snap : createRequest.players()) {
        if (snap.player_id() == ownerId) {
            ownerSnap = &snap;
            break;
        }
    }
    if (ownerSnap == nullptr) {
        return;
    }

    const auto poolIt = standbyPetPoolByPlayer.find(ownerId);
    if (poolIt == standbyPetPoolByPlayer.end()) {
        return;
    }
    BattlePetSnapshot chosenPet;
    bool foundPet = false;
    for (const auto& pet : poolIt->second) {
        if (pet.pet_id() == summonPetId) {
            chosenPet = pet;
            foundPet = true;
            break;
        }
    }
    if (!foundPet) {
        return;
    }

    size_t existingIndex = actors.size();
    for (size_t i = 0; i < actors.size(); ++i) {
        if (actors[i].actor_type() == BATTLE_ACTOR_TYPE_PET && actors[i].owner_player_id() == ownerId) {
            existingIndex = i;
            break;
        }
    }
    const uint32_t formationSlot = existingIndex < actors.size()
        ? actors[existingIndex].formation_slot()
        : NextFormationSlot(ownerTeam);
    const uint64_t newActorId = kPetActorIdBase + nextPetActorIndex;

    BattleActorState rebuilt;
    if (!BuildPetActor(*ownerSnap, chosenPet, newActorId, formationSlot, rebuilt)) {
        return;
    }
    ++nextPetActorIndex;

    BattleActorState* summonedActor = nullptr;
    if (existingIndex < actors.size()) {
        // 清理旧宠可能已提交的本回合行动，并用全新 actor_id 原位替换（旧宠 require_living_caster 光环同步失效）。
        pendingActions.erase(actors[existingIndex].actor_id());
        actors[existingIndex] = std::move(rebuilt);
        summonedActor = &actors[existingIndex];
    } else {
        actors.emplace_back(std::move(rebuilt));
        summonedActor = &actors.back();
    }

    summonPetRemainingByPlayer[ownerId] = 0u;
    standbyPetPoolByPlayer.erase(ownerId);
    effectiveContributionPlayers.insert(ownerId);

    auto* ev = AppendEvent(result, BATTLE_EVENT_SUMMON_PET, ownerId, newActorId);
    ev->set_skill_table_id(summonedActor->pet_table_id());
    ev->set_success(true);
    ev->set_target_health_after(summonedActor->attributes().health());
    ev->set_target_mana_after(summonedActor->attributes().mana());

    // 仅初始化新唤出灵宠的运行技能与开场被动/光环，新宠从下一回合起按速度正常排表出手。
    InitializeConfiguredAbilitiesForActor(*summonedActor, result);
}

void TurnBattleEngine::FillSummonStateForViewer(BattleStateS2C& state, uint64_t viewerPlayerId) const {
    state.clear_self_standby_pets();
    state.clear_self_summon_pet_remaining();
    if (viewerPlayerId == 0u || prepActive || !IsSummonPetEnabled()) {
        return;
    }
    const uint32_t remaining = SummonPetRemaining(viewerPlayerId);
    state.set_self_summon_pet_remaining(remaining);
    if (remaining == 0u) {
        return;
    }
    const auto poolIt = standbyPetPoolByPlayer.find(viewerPlayerId);
    if (poolIt == standbyPetPoolByPlayer.end()) {
        return;
    }
    for (const auto& pet : poolIt->second) {
        PopulatePetOption(pet, *state.add_self_standby_pets());
    }
}

void TurnBattleEngine::PopulatePrepStateForViewer(BattlePrepState& out, uint64_t viewerPlayerId,
                                                  uint64_t deadlineMs) const {
    out.Clear();
    if (!prepActive) {
        return;
    }
    out.set_active(true);
    out.set_deadline_ms(deadlineMs);
    out.set_duration_seconds(PrepDurationSeconds());
    out.set_skill_ban_slots_per_team(prepSkillBanSlotsPerTeam);
    out.set_skill_protect_slots_per_team(prepSkillProtectSlotsPerTeam);
    if (viewerPlayerId == 0) {
        return;
    }

    const auto selfIt = prepOptionsByPlayer.find(viewerPlayerId);
    if (selfIt == prepOptionsByPlayer.end()) {
        return;
    }
    const auto& selfOpt = selfIt->second;

    for (const auto& seal : selfOpt.availableSeals) {
        auto* dst = out.add_self_available_seals();
        dst->set_seal_id(seal.seal_id());
        dst->set_effect_id(seal.effect_id());
        dst->set_name(seal.name());
    }
    for (const auto& pet : selfOpt.availablePets) {
        PopulatePetOption(pet, *out.add_self_available_pets());
    }
    for (const auto sid : banCandidateSkillIds) {
        out.add_ban_candidate_skill_ids(sid);
    }

    // 同队互见、对手严密盲选：仅投影与收信者同 teamIndex 的成员状态与聚合禁招。
    for (const auto& [memberId, opt] : prepOptionsByPlayer) {
        if (opt.teamIndex != selfOpt.teamIndex) {
            continue;
        }
        auto* ps = out.add_team_states();
        ps->set_player_id(memberId);
        ps->set_team_index(opt.teamIndex);
        ps->set_selected_seal_id(opt.selectedSealId);
        ps->set_selected_pet_id(opt.selectedPetId);
        ps->set_ready(opt.ready);
        for (const auto sid : opt.nominatedBanSkillIds) {
            ps->add_nominated_ban_skill_ids(sid);
        }
        if (opt.nominatedProtectSkillId != 0u) {
            ps->set_nominated_protect_skill_id(opt.nominatedProtectSkillId);
        }
        if (opt.selectedPetId != 0) {
            for (const auto& pet : opt.availablePets) {
                if (pet.pet_id() == opt.selectedPetId) {
                    ps->set_selected_pet_table_id(pet.pet_table_id());
                    ps->set_selected_pet_name(pet.pet_name());
                    break;
                }
            }
        }
    }
    for (const auto sid : AggregateTeamBans(selfOpt.teamIndex)) {
        out.add_team_effective_ban_skill_ids(sid);
    }
    if (prepSkillProtectSlotsPerTeam > 0u) {
        const uint32_t effectiveProtect = AggregateTeamProtect(selfOpt.teamIndex);
        if (effectiveProtect != 0u) {
            out.set_team_effective_protected_skill_id(effectiveProtect);
        }
    }
    if (!banCandidateSkillSet.empty()) {
        std::set<uint32_t> teammateEquipped;
        for (const auto& [memberId, opt] : prepOptionsByPlayer) {
            if (opt.teamIndex != selfOpt.teamIndex || memberId == viewerPlayerId) {
                continue;
            }
            if (const auto* memberActor = FindActor(memberId); memberActor != nullptr) {
                for (const auto sid : memberActor->skill_table_ids()) {
                    if (banCandidateSkillSet.contains(sid)) {
                        teammateEquipped.insert(sid);
                    }
                }
            }
            bool petHandled = false;
            if (opt.selectedPetId != 0u) {
                for (const auto& pet : opt.availablePets) {
                    if (pet.pet_id() == opt.selectedPetId) {
                        for (const auto sid : pet.skill_table_ids()) {
                            if (banCandidateSkillSet.contains(sid)) {
                                teammateEquipped.insert(sid);
                            }
                        }
                        petHandled = true;
                        break;
                    }
                }
            }
            if (!petHandled) {
                for (const auto& actor : actors) {
                    if (actor.actor_type() == BATTLE_ACTOR_TYPE_PET && actor.owner_player_id() == memberId) {
                        for (const auto sid : actor.skill_table_ids()) {
                            if (banCandidateSkillSet.contains(sid)) {
                                teammateEquipped.insert(sid);
                            }
                        }
                    }
                }
            }
        }
        for (const auto sid : teammateEquipped) {
            out.add_teammate_equipped_skill_ids(sid);
        }
    }
    // 论剑 V6 宿敌交手提示：仅投影本人视角的敌方宿敌，观众与第三方清空。
    for (const auto& enc : createRequest.ranked_context().rivalry_encounters()) {
        if (enc.player_id() != viewerPlayerId || enc.opponent_id() == 0) {
            continue;
        }
        const auto* opp = FindActor(enc.opponent_id());
        if (opp == nullptr || opp->actor_type() != BATTLE_ACTOR_TYPE_PLAYER ||
            opp->team_index() == selfOpt.teamIndex) {
            continue;
        }
        auto* dst = out.add_self_rivalries();
        *dst = enc;
        if (dst->opponent_name().empty() && !opp->name().empty()) {
            dst->set_opponent_name(opp->name());
        }
    }
}

void TurnBattleEngine::FillPrepStateForViewer(BattleStateS2C& state, uint64_t viewerPlayerId,
                                              uint64_t deadlineMs) const {
    state.clear_prep_state();
    if (!prepActive) {
        return;
    }
    PopulatePrepStateForViewer(*state.mutable_prep_state(), viewerPlayerId, deadlineMs);
}

uint32_t TurnBattleEngine::SubmitBattlePing(uint64_t playerId, uint64_t targetActorId, BattlePingKind kind,
                                            BattleTacticalPing* outPing) {
    if (!initialized || outcome != BATTLE_OUTCOME_ONGOING) {
        return kFeatureUnavailable;
    }
    const auto* sender = FindActor(playerId);
    if (sender == nullptr || sender->actor_type() != BATTLE_ACTOR_TYPE_PLAYER || sender->fled()) {
        return kInvalidParameter;
    }
    const uint32_t senderTeam = sender->team_index();
    if (senderTeam > 1u) {
        return kInvalidParameter;
    }
    uint32_t teamPlayerCount = 0;
    for (const auto& actor : actors) {
        if (actor.actor_type() == BATTLE_ACTOR_TYPE_PLAYER && actor.team_index() == senderTeam) {
            ++teamPlayerCount;
        }
    }
    if (teamPlayerCount < 2u) {
        return kFeatureUnavailable;
    }
    if (kind != BATTLE_PING_FOCUS && kind != BATTLE_PING_CONTROL &&
        kind != BATTLE_PING_PROTECT && kind != BATTLE_PING_DISPEL) {
        return kInvalidParameter;
    }
    const auto* target = FindActor(targetActorId);
    if (target == nullptr || !IsActorActive(*target)) {
        return kInvalidParameter;
    }
    if ((kind == BATTLE_PING_FOCUS || kind == BATTLE_PING_CONTROL) && target->team_index() == senderTeam) {
        return kInvalidParameter;
    }
    if (kind == BATTLE_PING_PROTECT && target->team_index() != senderTeam) {
        return kInvalidParameter;
    }

    auto& usedCount = roundPingCountByPlayer[playerId];
    if (usedCount >= kMaxBattlePingsPerRound) {
        return kRateLimitExceeded;
    }
    ++usedCount;

    BattleTacticalPing ping;
    ping.set_sender_player_id(playerId);
    ping.set_target_actor_id(targetActorId);
    ping.set_kind(kind);
    ping.set_round_index(prepActive ? 0u : roundIndex);
    teamPings[senderTeam][playerId] = ping;
    if (outPing != nullptr) {
        *outPing = ping;
    }
    return 0u;
}

void TurnBattleEngine::FillTeamPingsForViewer(BattleStateS2C& state, uint64_t viewerPlayerId) const {
    state.clear_team_pings();
    if (viewerPlayerId == 0 || outcome != BATTLE_OUTCOME_ONGOING) {
        return;
    }
    const auto team = TeamIndexOfPlayer(viewerPlayerId);
    if (!team.has_value() || *team > 1u) {
        return;
    }
    for (const auto& [senderId, ping] : teamPings[*team]) {
        (void)senderId;
        *state.add_team_pings() = ping;
    }
}

BattleEventItem* TurnBattleEngine::AppendEvent(TurnResultS2C& result, eBattleEventType eventType,
                                               uint64_t sourceId, uint64_t targetId) {
    auto* event = result.add_events();
    event->set_event_type(eventType);
    event->set_source_id(sourceId);
    event->set_target_id(targetId);
    // 表现层分组(D2):同一行动/同一单位的回合末 tick 共用 group_id;
    // hit_index 由 ExecuteSkill 逐目标递增、由 ExecuteAttack 在连击追加段递增,其余场景为 0
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

bool TurnBattleEngine::RollPercent(uint64_t percent) {
    // 两端短路、不消耗随机数(与 RollHit 同口径):
    //   0     —— 没有这项属性的单位(怪物、宝宝、无装备玩家)不能平移既有同种子回放;
    //   >=100 —— 必然成立的事不必掷骰;同时把超过 100 的配置自然夹到 100。
    // 必杀不走这里(见 CalculateFinalDamage:它要保持落地前"满 100 也掷"的消耗)
    if (percent == 0) {
        return false;
    }
    if (percent >= 100) {
        return true;
    }
    return Rand01() * 100.0 < static_cast<double>(percent);
}

void TurnBattleEngine::AttachTacticalPuzzleInitialBuff(BattleActorState& target, uint32_t buffTableId,
                                                       uint64_t casterId, uint64_t shieldCapacity,
                                                       uint64_t periodicDamage) {
    if (buffTableId == 0u) {
        return;
    }
    const auto* row = dataProvider->FindBuff(buffTableId);
    if (row == nullptr) {
        return;
    }
    auto* added = target.add_buffs();
    added->set_buff_id(nextBuffInstanceId++);
    added->set_buff_table_id(row->id());
    added->set_caster_id(casterId);
    added->set_layer(1);
    const auto op = static_cast<BattleEffectOp>(row->battle_op());
    if (op == BattleEffectOp::Shield) {
        added->set_remaining_capacity(shieldCapacity);
        added->set_remain_rounds(maxRounds);
    } else if (op == BattleEffectOp::PeriodicDamage) {
        added->set_snapshot_value(periodicDamage);
        const uint32_t ticks = row->tick_count() > 0u ? row->tick_count() : maxRounds;
        added->set_remaining_ticks(ticks);
        added->set_remain_rounds(ticks);
    } else {
        const uint32_t dur = row->duration_rounds() > 0u ? row->duration_rounds() : maxRounds;
        added->set_remain_rounds(dur);
    }
    added->set_remaining_hits(row->hit_limit());
    added->set_applied_round(1);
    added->set_tick_round(row->tick_on_apply() ? 1u : 2u);
}

void TurnBattleEngine::ApplyTacticalPuzzleSetup() {
    const auto& spec = tacticalPuzzleContext.spec();
    teamAFormationId = spec.formation_id();
    teamBFormationId = 0u;
    if (spec.banned_skill_id() != 0u) {
        bannedSkillIds.push_back(spec.banned_skill_id());
        bannedSkillSet.insert(spec.banned_skill_id());
    }

    uint64_t firstEnemyId = 0u;
    for (const auto& actor : actors) {
        if (actor.team_index() == 1u && actor.actor_type() == BATTLE_ACTOR_TYPE_MONSTER) {
            firstEnemyId = actor.actor_id();
            break;
        }
    }

    uint32_t enemyIdx = 0u;
    for (auto& actor : actors) {
        if (actor.team_index() == 0u &&
            (actor.actor_type() == BATTLE_ACTOR_TYPE_PLAYER || actor.actor_type() == BATTLE_ACTOR_TYPE_PET)) {
            actor.mutable_attributes()->set_health(
                TacticalPuzzleScalePct(actor.max_health(), spec.player_initial_hp_pct()));
            if (actor.max_mana() > 0u) {
                actor.mutable_attributes()->set_mana(
                    TacticalPuzzleScalePct(actor.max_mana(), spec.player_initial_mp_pct()));
            }
            if (actor.actor_type() == BATTLE_ACTOR_TYPE_PLAYER) {
                if (spec.player_initial_shield_pct() > 0u) {
                    const uint64_t cap = TacticalPuzzleScalePct(actor.max_health(), spec.player_initial_shield_pct());
                    AttachTacticalPuzzleInitialBuff(actor, kTacticalPuzzleShieldBuffId, actor.actor_id(), cap, 0u);
                }
                if (const uint32_t debuffId = TacticalPuzzlePlayerDebuffTableId(spec.player_initial_debuff_kind());
                    debuffId != 0u) {
                    const uint64_t dot = TacticalPuzzleScalePct(actor.max_health(), kTacticalPuzzlePoisonMaxHpPct);
                    AttachTacticalPuzzleInitialBuff(actor, debuffId, firstEnemyId != 0u ? firstEnemyId : actor.actor_id(),
                                                    0u, dot);
                }
            }
        } else if (actor.team_index() == 1u && actor.actor_type() == BATTLE_ACTOR_TYPE_MONSTER) {
            if (enemyIdx < static_cast<uint32_t>(spec.enemies_size())) {
                const auto& enemySpec = spec.enemies(static_cast<int>(enemyIdx));
                if (enemySpec.initial_shield_pct() > 0u) {
                    const uint64_t cap = TacticalPuzzleScalePct(actor.max_health(), enemySpec.initial_shield_pct());
                    AttachTacticalPuzzleInitialBuff(actor, kTacticalPuzzleShieldBuffId, actor.actor_id(), cap, 0u);
                }
                if (const uint32_t buffId = TacticalPuzzleEnemyBuffTableId(enemySpec.initial_buff_kind());
                    buffId != 0u) {
                    AttachTacticalPuzzleInitialBuff(actor, buffId, actor.actor_id(), 0u, 0u);
                }
            }
            ++enemyIdx;
        }
    }
}

bool TurnBattleEngine::HasAnyActiveShield(const BattleActorState& target) const {
    if (bossTrialKind == kBossTrialKindBarrier && target.is_boss() && bossBarrierShield > 0) {
        return true;
    }
    if (const auto it = leylineEarthShieldByActor.find(target.actor_id());
        it != leylineEarthShieldByActor.end() && it->second > 0) {
        return true;
    }
    for (const auto& entry : target.buffs()) {
        if (entry.remaining_capacity() == 0) {
            continue;
        }
        const auto* row = dataProvider->FindBuff(entry.buff_table_id());
        if (row != nullptr && row->battle_op() == static_cast<uint32_t>(BattleEffectOp::Shield)) {
            return true;
        }
    }
    return false;
}

uint32_t TurnBattleEngine::WuxingHealShieldBonusPct(const BattleActorState& caster) const {
    uint32_t pct = wuxing::FormationOf(TeamFormationId(caster.team_index())).healShieldBonusPct;
    if (IsWuxingSynergyActiveForActor(caster.actor_id())) {
        pct += wuxing::kSynergyBonusPct;
    }
    if (hasTowerContext && caster.team_index() == 0) {
        pct += towerSigilTotals.healShieldPct;
    }
    return pct;
}

uint32_t TurnBattleEngine::WuxingControlResistPct(const BattleActorState& target) const {
    uint32_t pct = wuxing::FormationOf(TeamFormationId(target.team_index())).controlResistPct;
    if (hasTowerContext && target.team_index() == 0) {
        pct += towerSigilTotals.controlResistPct;
    }
    return pct;
}

bool TurnBattleEngine::IsControlSkill(const SkillTable& skillRow) const {
    for (const uint32_t effectBuffId : skillRow.effect()) {
        if (IsHardControlBuff(effectBuffId)) {
            return true;
        }
        const auto* buffRow = dataProvider->FindBuff(effectBuffId);
        if (buffRow != nullptr &&
            ((buffRow->battle_op() == static_cast<uint32_t>(BattleEffectOp::Control) &&
              buffRow->state_kind() >= 1 && buffRow->state_kind() <= 6) ||
             buffRow->buff_type() == kBuffTypeStun ||
             buffRow->buff_type() == kBuffTypeFreeze ||
             buffRow->buff_type() == kBuffTypeSilence)) {
            return true;
        }
    }
    return false;
}

uint64_t TurnBattleEngine::CurrentTotalShieldOf(const BattleActorState& target) const {
    uint64_t total = 0ULL;
    if (bossTrialKind == kBossTrialKindBarrier && target.is_boss() && bossBarrierShield > 0) {
        total = SaturatingAdd(total, bossBarrierShield);
    }
    if (const auto it = leylineEarthShieldByActor.find(target.actor_id());
        it != leylineEarthShieldByActor.end() && it->second > 0) {
        total = SaturatingAdd(total, it->second);
    }
    for (const auto& entry : target.buffs()) {
        if (entry.remaining_capacity() == 0) {
            continue;
        }
        const auto* row = dataProvider->FindBuff(entry.buff_table_id());
        if (row != nullptr && row->battle_op() == static_cast<uint32_t>(BattleEffectOp::Shield)) {
            total = SaturatingAdd(total, entry.remaining_capacity());
        }
    }
    return total;
}

void TurnBattleEngine::ApplyWuxingPeriodicSpread(uint64_t sourceActorId, const BattleActorState& victim,
                                                 uint64_t dealt, TurnResultS2C& result) {
    if (dealt == 0 || !HasRealmBoon(wuxing::kBoonWoodDotSpread) ||
        boonRuntime.procDepth >= wuxing::kBoonMaxProcDepth) {
        return;
    }
    const uint64_t spread = std::min<uint64_t>(wuxing::kBoonRoundEndBurnCap, std::max<uint64_t>(1ULL, dealt / 2ULL));
    ++boonRuntime.procDepth;
    for (auto& other : actors) {
        if (other.actor_id() == victim.actor_id() || other.team_index() != 1 || !IsActorActive(other)) {
            continue;
        }
        const uint64_t applied = std::min(other.attributes().health(), spread);
        if (applied == 0) {
            continue;
        }
        other.mutable_attributes()->set_health(other.attributes().health() - applied);
        auto* ev = AppendEvent(result, BATTLE_EVENT_DAMAGE, sourceActorId, other.actor_id());
        ev->set_value(applied);
        ev->set_target_health_after(other.attributes().health());
        ev->set_target_mana_after(other.attributes().mana());
        if (other.attributes().health() == 0) {
            HandleDeath(other, sourceActorId, result);
        }
    }
    --boonRuntime.procDepth;
}

void TurnBattleEngine::ApplyWuxingRoundStartBoonEffects(TurnResultS2C& result) {
    if (!HasRealmResonance(wuxing::kElementWood) || boonRuntime.procDepth >= wuxing::kBoonMaxProcDepth) {
        return;
    }
    // 3 木共鸣·生生不息：敌方持续灼伤/玄毒在回合初额外结算 1 次 50% 伤害。
    uint64_t ownerId = 0ULL;
    for (const auto& a : actors) {
        if (a.team_index() == 0 && a.actor_type() == BATTLE_ACTOR_TYPE_PLAYER) {
            ownerId = a.actor_id();
            break;
        }
    }
    ++boonRuntime.procDepth;
    for (auto& enemy : actors) {
        if (enemy.team_index() != 1 || !IsActorActive(enemy)) {
            continue;
        }
        uint64_t extra = 0ULL;
        if (const auto it = boonRuntime.fireBurnStacks.find(enemy.actor_id());
            it != boonRuntime.fireBurnStacks.end() && it->second > 0u) {
            const uint64_t stackBurn = std::min(wuxing::kBoonRoundEndBurnCap,
                std::max<uint64_t>(1ULL, (enemy.max_health() * 4ULL * it->second) / 100ULL));
            extra = SaturatingAdd(extra, std::max<uint64_t>(1ULL, stackBurn / 2ULL));
        }
        for (const auto& b : enemy.buffs()) {
            const auto* row = dataProvider->FindBuff(b.buff_table_id());
            if (row == nullptr) {
                continue;
            }
            if (row->buff_type() == kBuffTypePoison || row->buff_type() == kBuffTypeBurn ||
                row->battle_op() == static_cast<uint32_t>(BattleEffectOp::PeriodicDamage)) {
                const uint64_t tickVal = b.snapshot_value() > 0u
                    ? b.snapshot_value()
                    : (row->interval_effect_size() > 0 ? static_cast<uint64_t>(std::max(1.0, row->interval_effect(0))) : 0ULL);
                if (tickVal > 0u) {
                    extra = SaturatingAdd(extra, std::max<uint64_t>(1ULL, tickVal / 2ULL));
                }
            }
        }
        if (extra == 0ULL) {
            continue;
        }
        const uint64_t applied = std::min({enemy.attributes().health(), wuxing::kBoonRoundEndBurnCap, extra});
        if (applied > 0ULL) {
            enemy.mutable_attributes()->set_health(enemy.attributes().health() - applied);
            auto* ev = AppendEvent(result, BATTLE_EVENT_DAMAGE, ownerId, enemy.actor_id());
            ev->set_value(applied);
            ev->set_target_health_after(enemy.attributes().health());
            ev->set_target_mana_after(enemy.attributes().mana());
            if (enemy.attributes().health() == 0) {
                HandleDeath(enemy, ownerId, result);
            }
        }
    }
    --boonRuntime.procDepth;
}

void TurnBattleEngine::ApplyWuxingRoundEndBoonEffects(TurnResultS2C& result) {
    if (!HasRealmBoon(wuxing::kBoonFireRoundEndBurn) || boonRuntime.procDepth >= wuxing::kBoonMaxProcDepth) {
        return;
    }
    uint64_t ownerId = 0ULL;
    BattleActorState* highestHpEnemy = nullptr;
    for (auto& actor : actors) {
        if (actor.team_index() == 0 && actor.actor_type() == BATTLE_ACTOR_TYPE_PLAYER && ownerId == 0ULL) {
            ownerId = actor.actor_id();
        }
        if (actor.team_index() == 1 && IsActorActive(actor)) {
            if (highestHpEnemy == nullptr ||
                actor.attributes().health() > highestHpEnemy->attributes().health() ||
                (actor.attributes().health() == highestHpEnemy->attributes().health() &&
                 actor.actor_id() < highestHpEnemy->actor_id())) {
                highestHpEnemy = &actor;
            }
        }
    }
    if (highestHpEnemy == nullptr) {
        return;
    }
    uint32_t& stacks = boonRuntime.fireBurnStacks[highestHpEnemy->actor_id()];
    if (stacks < 3u) {
        ++stacks;
    }
    const uint64_t rawBurn = std::max<uint64_t>(1ULL, (highestHpEnemy->max_health() * 4ULL * stacks) / 100ULL);
    const uint64_t burn = std::min({highestHpEnemy->attributes().health(), wuxing::kBoonRoundEndBurnCap, rawBurn});
    if (burn == 0ULL) {
        return;
    }
    highestHpEnemy->mutable_attributes()->set_health(highestHpEnemy->attributes().health() - burn);
    auto* ev = AppendEvent(result, BATTLE_EVENT_DAMAGE, ownerId, highestHpEnemy->actor_id());
    ev->set_value(burn);
    ev->set_target_health_after(highestHpEnemy->attributes().health());
    ev->set_target_mana_after(highestHpEnemy->attributes().mana());
    if (highestHpEnemy->attributes().health() == 0) {
        HandleDeath(*highestHpEnemy, ownerId, result);
    }
    ApplyWuxingPeriodicSpread(ownerId, *highestHpEnemy, burn, result);
}

void TurnBattleEngine::ApplyWuxingPostDirectHitEffects(BattleActorState& attacker, BattleActorState& target,
                                                       uint64_t dealt, bool isCritical, TurnResultS2C& result,
                                                       uint64_t shieldAbsorbed, bool shieldBroke,
                                                       uint64_t brokenShieldCap, uint32_t skillTableId,
                                                       bool isMultiTarget) {
    if (!hasTowerContext || (dealt == 0 && shieldAbsorbed == 0)) {
        return;
    }
    // 消耗本回合首攻与治疗增伤标记（仅一次行动主段生效）。
    if (attacker.team_index() == 0 && boonRuntime.procDepth == 0u) {
        if (HasRealmBoon(wuxing::kBoonMetalFirstPierce)) {
            boonRuntime.firstAttackUsedThisRound.insert(attacker.actor_id());
        }
        if (HasRealmBoon(wuxing::kBoonWoodHealEmpower)) {
            boonRuntime.healShieldEmpoweredActors.erase(attacker.actor_id());
        }
    }

    // 火系仙品符印（V1 403 业火）：暴击附带目标 3% 最大气血真伤（单次封顶 1500）。
    if (dealt > 0 && attacker.team_index() == 0 && isCritical && towerSigilTotals.critBurnMaxHpPct > 0 && IsActorActive(target)) {
        const uint64_t rawBurn =
            std::max<uint64_t>(1ULL, (target.max_health() * towerSigilTotals.critBurnMaxHpPct) / 100ULL);
        const uint64_t burn = std::min({rawBurn, wuxing::kFireCritBurnCapPerHit, target.attributes().health()});
        if (burn > 0) {
            target.mutable_attributes()->set_health(target.attributes().health() - burn);
            auto* ev = AppendEvent(result, BATTLE_EVENT_DAMAGE, attacker.actor_id(), target.actor_id());
            ev->set_value(burn);
            ev->set_target_health_after(target.attributes().health());
            ev->set_target_mana_after(target.attributes().mana());
            if (target.attributes().health() == 0) {
                HandleDeath(target, attacker.actor_id(), result);
            }
        }
    }
    // 水系灵品符印（V1 302 归元）：受击回复 4% 最大法力。
    if (dealt > 0 && target.team_index() == 0 && IsActorActive(target) && towerSigilTotals.onHitManaRegenPct > 0 &&
        target.max_mana() > 0 && target.attributes().mana() < target.max_mana()) {
        const uint64_t gain =
            std::max<uint64_t>(1ULL, (target.max_mana() * towerSigilTotals.onHitManaRegenPct) / 100ULL);
        target.mutable_attributes()->set_mana(std::min(target.max_mana(), SaturatingAdd(target.attributes().mana(), gain)));
    }
    // 土系反震（V1 502/503 + V2 641 镇岳倍反 ×2 破甲 + 3 土共鸣护盾承伤亦反震）。
    const uint64_t reflectBase = HasRealmResonance(wuxing::kElementEarth) ? SaturatingAdd(dealt, shieldAbsorbed) : dealt;
    uint32_t reflectPct = towerSigilTotals.reflectPct;
    if (HasRealmBoon(wuxing::kBoonEarthDoubleReflect)) {
        reflectPct += 12u;
    }
    if (target.team_index() == 0 && IsActorActive(target) && IsActorActive(attacker) &&
        attacker.team_index() != 0 && reflectBase > 0 && reflectPct > 0) {
        uint64_t rawReflect = std::max<uint64_t>(1ULL, (reflectBase * reflectPct) / 100ULL);
        if (HasRealmBoon(wuxing::kBoonEarthDoubleReflect)) {
            rawReflect = SaturatingAdd(rawReflect, rawReflect);
            boonRuntime.armorBrokenUntilRound[attacker.actor_id()] = roundIndex + 2u;
        }
        const uint64_t reflected = std::min(attacker.attributes().health(), rawReflect);
        if (reflected > 0) {
            attacker.mutable_attributes()->set_health(attacker.attributes().health() - reflected);
            auto* ev = AppendEvent(result, BATTLE_EVENT_DAMAGE, target.actor_id(), attacker.actor_id());
            ev->set_value(reflected);
            ev->set_hit_kind(BATTLE_HIT_REFLECT);
            ev->set_target_health_after(attacker.attributes().health());
            ev->set_target_mana_after(attacker.attributes().mana());
            if (attacker.attributes().health() == 0) {
                HandleDeath(attacker, target.actor_id(), result);
            }
        }
    }

    if (!IsWuxingRealmV2() || boonRuntime.procDepth >= wuxing::kBoonMaxProcDepth) {
        return;
    }

    // 全部 V2 奇策与共鸣派生段均受 procDepth 守卫（最大深度 1），从根源杜绝连招循环递归。
    ++boonRuntime.procDepth;
    auto dealTrueProcDamage = [&](uint64_t srcId, BattleActorState& dst, uint64_t dmg) {
        if (dmg == 0ULL || !IsActorActive(dst)) {
            return;
        }
        const uint64_t applied = std::min(dst.attributes().health(), dmg);
        if (applied == 0ULL) {
            return;
        }
        dst.mutable_attributes()->set_health(dst.attributes().health() - applied);
        auto* ev = AppendEvent(result, BATTLE_EVENT_DAMAGE, srcId, dst.actor_id());
        ev->set_value(applied);
        ev->set_target_health_after(dst.attributes().health());
        ev->set_target_mana_after(dst.attributes().mana());
        if (dst.attributes().health() == 0) {
            HandleDeath(dst, srcId, result);
        }
    };

    // 642 绝境磐石：主角或出战灵宠每战首次气血跌破 35% 自动获得 25% 最大气血护盾。
    if (target.team_index() == 0 && IsActorActive(target) && HasRealmBoon(wuxing::kBoonEarthClutchShield) &&
        target.max_health() > 0 && target.attributes().health() * 100ULL < target.max_health() * 35ULL &&
        boonRuntime.emergencyShieldTriggered.insert(target.actor_id()).second) {
        const uint64_t shieldCap = std::max<uint64_t>(1ULL, (target.max_health() * 25ULL) / 100ULL);
        leylineEarthShieldByActor[target.actor_id()] =
            SaturatingAdd(leylineEarthShieldByActor[target.actor_id()], shieldCap);
        if (HasRealmBoon(wuxing::kBoonWoodHealEmpower)) {
            boonRuntime.healShieldEmpoweredActors.insert(target.actor_id());
        }
    }

    // 601 破盾返元：击碎敌方任意护盾时返还施法者 50% 最大法力并追加 60% 威力斩击。
    if (shieldBroke && attacker.team_index() == 0 && target.team_index() == 1 &&
        HasRealmBoon(wuxing::kBoonMetalBreakRefund) && IsActorActive(attacker)) {
        if (attacker.max_mana() > 0) {
            const uint64_t refund = std::max<uint64_t>(1ULL, attacker.max_mana() / 2ULL);
            attacker.mutable_attributes()->set_mana(
                std::min(attacker.max_mana(), SaturatingAdd(attacker.attributes().mana(), refund)));
        }
        if (IsActorActive(target)) {
            const uint64_t atk = std::max({EffectivePhysicalAttack(attacker), attacker.magic_attack(), 1ULL});
            const uint64_t bonusStrike = std::max<uint64_t>(1ULL, (atk * 60ULL) / 100ULL);
            dealTrueProcDamage(attacker.actor_id(), target, bonusStrike);
        }
    }

    // 631 焚盾爆炎：任意护盾碎裂时对敌方全体造成护盾上限 30% 火焰真伤（单次封顶 1800）。
    if (shieldBroke && brokenShieldCap > 0 && HasRealmBoon(wuxing::kBoonFireShieldBlast)) {
        const uint64_t blast = std::min(wuxing::kBoonShieldShatterDamageCap,
                                        std::max<uint64_t>(1ULL, (brokenShieldCap * 30ULL) / 100ULL));
        const uint64_t srcId = attacker.team_index() == 0 ? attacker.actor_id() : target.actor_id();
        for (auto& enemy : actors) {
            if (enemy.team_index() == 1 && IsActorActive(enemy)) {
                dealTrueProcDamage(srcId, enemy, blast);
            }
        }
    }

    // 602 绝锋连斩：单体暴击向另一存活敌方溅射 40% 实伤。
    if (dealt > 0 && attacker.team_index() == 0 && isCritical && !isMultiTarget &&
        HasRealmBoon(wuxing::kBoonMetalCritSplash)) {
        for (auto& other : actors) {
            if (other.team_index() == 1 && other.actor_id() != target.actor_id() && IsActorActive(other)) {
                dealTrueProcDamage(attacker.actor_id(), other, std::max<uint64_t>(1ULL, (dealt * 40ULL) / 100ULL));
                break;
            }
        }
    }

    // 613 灵宠协战：主角普攻或单体技能后，存活出战灵宠免费追击 1 次 70% 威力普攻（每回合限 1 次）。
    if (attacker.team_index() == 0 && attacker.actor_type() == BATTLE_ACTOR_TYPE_PLAYER && !isMultiTarget &&
        HasRealmBoon(wuxing::kBoonWoodPetFollowUp) &&
        !boonRuntime.petAssistUsedThisRound.contains(attacker.actor_id())) {
        BattleActorState* pet = nullptr;
        for (auto& candidate : actors) {
            if (candidate.actor_type() == BATTLE_ACTOR_TYPE_PET && candidate.owner_player_id() == attacker.actor_id() &&
                IsActorActive(candidate)) {
                pet = &candidate;
                break;
            }
        }
        if (pet != nullptr) {
            BattleActorState* assistTarget = IsActorActive(target) ? &target : nullptr;
            if (assistTarget == nullptr) {
                for (auto& enemy : actors) {
                    if (enemy.team_index() == 1 && IsActorActive(enemy)) {
                        assistTarget = &enemy;
                        break;
                    }
                }
            }
            if (assistTarget != nullptr) {
                boonRuntime.petAssistUsedThisRound.insert(attacker.actor_id());
                const uint64_t petAtk = std::max({EffectivePhysicalAttack(*pet), pet->magic_attack(), 1ULL});
                const uint64_t assistDmg = std::max<uint64_t>(1ULL, (petAtk * 70ULL) / 100ULL);
                dealTrueProcDamage(pet->actor_id(), *assistTarget, assistDmg);
            }
        }
    }

    // 623 镜花水月：主角每第 3 次主动进攻技能额外以 55% 威力连发 1 次。
    if (dealt > 0 && attacker.team_index() == 0 && attacker.actor_type() == BATTLE_ACTOR_TYPE_PLAYER &&
        skillTableId != 0u && HasRealmBoon(wuxing::kBoonWaterSkillEcho)) {
        const uint32_t casts = ++boonRuntime.skillCastCounts[attacker.actor_id()];
        if (casts % 3u == 0u && IsActorActive(target)) {
            dealTrueProcDamage(attacker.actor_id(), target, std::max<uint64_t>(1ULL, (dealt * 55ULL) / 100ULL));
        }
    }

    // 3 火共鸣·焚城烈焰：己方直击附带 15% 溅射灼伤至另一存活敌方。
    if (dealt > 0 && attacker.team_index() == 0 && HasRealmResonance(wuxing::kElementFire)) {
        for (auto& other : actors) {
            if (other.team_index() == 1 && other.actor_id() != target.actor_id() && IsActorActive(other)) {
                dealTrueProcDamage(attacker.actor_id(), other, std::max<uint64_t>(1ULL, (dealt * 15ULL) / 100ULL));
                break;
            }
        }
    }

    // 3 金共鸣·破军锋芒：敌方受击后气血跌至斩杀阈值（首领 12% / 非首领 20%）直接斩杀。
    if (attacker.team_index() == 0 && target.team_index() == 1 && IsActorActive(target) &&
        HasRealmResonance(wuxing::kElementMetal) && target.max_health() > 0) {
        const uint64_t execPct = target.is_boss() ? 12ULL : 20ULL;
        if (target.attributes().health() * 100ULL <= target.max_health() * execPct) {
            dealTrueProcDamage(attacker.actor_id(), target, target.attributes().health());
        }
    }

    --boonRuntime.procDepth;
}

}  // namespace turnbattle
