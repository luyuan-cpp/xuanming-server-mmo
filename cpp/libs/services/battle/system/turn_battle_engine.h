#pragma once
#include <array>
#include <map>
#include <memory>
#include <optional>
#include <random>
#include <set>
#include <string>
#include <vector>

// 回合制战斗确定性引擎(设计文档 §5.1)。
//
// 纯逻辑库:零网络/零 ECS/零 timer 依赖。输入 = 快照 + 指令流 + 随机种子,
// 输出纯函数;战斗随机走成员 RNG(std::mt19937_64(seed)),奖励使用种子与玩家号派生的独立 RNG,
// 相同输入必产出逐字节相同的事件流(观战/回放/断线重连依赖,宪法 §7 新增不变量 5)。
//
// 依赖的 proto 产物(proto/battle/*.proto 重生成后落到 generated/proto/battle/ 下,
// 路径按 scene 等既有域的生成规律推断):
//   battle_data.pb.h   —— BattleActorState / BattleAction / BattleEventItem 等共享结构
//   battle_node.pb.h   —— CreateBattleRequest(match → battle 的开局契约)
//   player_battle.pb.h —— TurnResultS2C / BattleStateS2C(客户端协议)
#include "proto/battle/battle_data.pb.h"
#include "proto/battle/battle_node.pb.h"
#include "proto/battle/battle_script.pb.h"
#include "proto/battle/player_battle.pb.h"

#include "constants/turn_battle_constants.h"
#include "constants/battle_effect_constants.h"
#include "constants/boss_trial_weekly_affix_rules.h"
#include "constants/wuxing_expansion_rules.h"
#include "data/battle_data_provider.h"
#include "system/class_skill_rules.h"
#include "system/dungeon_monster_ai.h"

namespace turnbattle {

class TurnBattleEngine {
public:
    // 数据源显式注入：生产使用开局前已加载配置的表数据源，单测使用内存数据源。
    explicit TurnBattleEngine(std::shared_ptr<BattleDataProvider> dataProvider);

    // 快照 + 怪物侧 + 配置初始化;返回是否合法
    bool Initialize(const CreateBattleRequest& request);

    // 收行动;全员就绪返回 true。
    // 非法行动(死亡/已逃/技能校验链不过)不落账,按未提交处理,
    // 由回合超时的默认普攻兜底(设计文档 §5.1 超时/掉线默认行动)
    bool SubmitAction(uint64_t actorId, const BattleAction& action);
    // 主人可控制自身或本场自己的宝宝,每个单位每回合仍只有一个普通指令。
    bool SubmitActorAction(uint64_t ownerPlayerId, uint64_t actorId, const BattleAction& action);
    uint32_t ValidateActorAction(uint64_t ownerPlayerId, uint64_t actorId, const BattleAction& action) const;

    // 行动合法性查询(零副作用)。节点在 SubmitAction 之前调它,把 tip 码回给提交者,
    // 不再"一律回 OK"(AGENTS §11.3:玩家资产路径 fail-closed,拒绝要让玩家知道原因)。
    // 返回 kSuccess 表示本次提交会被引擎接受;其余为 tip 码,与节点 error_message.id 同域。
    uint32_t ValidateAction(uint64_t actorId, const BattleAction& action) const;

    // 自动战斗开关(设计文档 §11 D12):仅存活未逃的玩家单位可设,
    // 写 BattleActorState.is_auto。返回 0 成功,非 0 为 tip 错误码
    // (与节点 error_message.id 同域,节点侧直接透传)。
    // 零随机/零时钟:auto 单位出手复用默认行动路径(FillDefaultActions,
    // 挂机玩家选职业主动技能见 SelectAutoClassSkill),确定性不受影响
    uint32_t SetActorAuto(uint64_t actorId, bool enabled);

    // 全体存活玩家是否已就绪(挂机单位视为已就绪,D12)。
    // 公开给 battle 节点:SetAutoBattle 后立即结算、全自动房间快节奏 timer(D13)都要查
    bool AllPlayersReady() const;

    // 未提交者填默认行动并结算本回合
    TurnResultS2C ResolveCurrentRound();

    // 最近一次 ResolveCurrentRound 的出手序(actor_id,速度降序、同速 actor_id 升序),
    // 按回合开始时的存活未逃单位排定,回合中途死亡/逃离而被跳过者也在列
    // (表现规格 D3:battle 节点组 TurnResultS2C.action_order 时透传)
    const std::vector<uint64_t>& LastActionOrder() const { return lastActionOrder; }

    // ONGOING / SIDE_A_WIN / SIDE_B_WIN / DRAW
    eBattleOutcome Outcome() const { return outcome; }

    // 单个玩家的结算数据(battle → scene,scene 是唯一应用者)
    BattleSettlementData BuildSettlement(uint64_t playerId) const;

    // 正式人宠会武(PVP_TEAM): season_id != 0 时计正式排位分、冻结参与事实并执行回合上限判平。
    bool IsRankedBattle() const {
        return createRequest.has_ranked_context() && createRequest.ranked_context().season_id() != 0;
    }
    // 免费 3×3 预设娱乐场(护卫救援 / 物法易势 5V5): 仅 use_free_preset=true 时放行临时竞技宠并禁止回写场外宠物。
    bool UsesFreePreset() const {
        return createRequest.has_ranked_context() && createRequest.ranked_context().use_free_preset();
    }
    // 帮会全天帮战 V2（9=真人帮战 / 10=守旗阵踢馆 AI 托管）：真实养成入场、第 12 回合起决胜煞气、打满判平。
    bool IsGuildWarBattle() const {
        return createRequest.match_mode() == kMatchModeGuildWarPvp ||
               createRequest.match_mode() == kMatchModeGuildWarSiege;
    }
    bool IsAiControlledPlayer(uint64_t playerId) const {
        return aiControlledPlayers.contains(playerId);
    }
    // 竞技 PVP（正式会武 / 物法易势 5V5 / 赛季锦标赛 / 帮会帮战）：启用主目标凝锋与后期战意渐盛，绝不影响 PVE。
    bool IsCompetitivePvpBattle() const {
        return !IsPveMatch() &&
               (IsRankedBattle() ||
                bossTrialKind == kBossTrialKindPolarity5v5 ||
                createRequest.match_mode() == kMatchModePolarity5v5 ||
                createRequest.match_mode() == kMatchModeSeasonTournament ||
                IsGuildWarBattle());
    }
    // 全服赛季战队锦标赛 V2 / 五行幻境爬塔灵脉阵眼（确定性定点数倍率，零浮点漂移）。
    TournamentLeylineElement ActiveTournamentLeyline() const {
        if (hasTowerContext && towerContext.leyline_element() != TOURNAMENT_LEYLINE_UNSPECIFIED) {
            return towerContext.leyline_element();
        }
        return createRequest.has_tournament_context()
            ? createRequest.tournament_context().leyline_element()
            : TOURNAMENT_LEYLINE_UNSPECIFIED;
    }
    bool IsWuxingTowerBattle() const { return hasTowerContext; }
    bool HasCustomDuelPolarity() const {
        return hasCustomDuelRules && customDuelRules.enable_polarity_rotation();
    }
    bool IsControlBannedByCustomDuel() const {
        return hasCustomDuelRules && customDuelRules.ban_control_skills();
    }
    uint32_t TeamFormationId(uint32_t teamIndex) const {
        return teamIndex == 0u ? teamAFormationId : (teamIndex == 1u ? teamBFormationId : 0u);
    }
    bool IsWuxingSynergyActiveForActor(uint64_t actorId) const {
        return wuxingSynergyActorIds.contains(actorId);
    }
    uint64_t TournamentEarthShieldOf(uint64_t actorId) const {
        const auto it = leylineEarthShieldByActor.find(actorId);
        return it == leylineEarthShieldByActor.end() ? 0ULL : it->second;
    }
    uint32_t MaxRounds() const { return maxRounds; }
    uint32_t EscalationStartRound() const {
        if (IsGuildWarBattle()) {
            return maxRounds >= kGuildWarEscalationStartRound ? kGuildWarEscalationStartRound : 0u;
        }
        return (IsCompetitivePvpBattle() && maxRounds >= kPvpEscalationMinMaxRounds)
            ? (maxRounds - kPvpEscalationLeadRounds)
            : 0u;
    }
    uint32_t EscalationStacks() const {
        const uint32_t start = EscalationStartRound();
        return (start > 0u && roundIndex >= start) ? (roundIndex - start + 1u) : 0u;
    }
    uint32_t EscalationDamagePct() const {
        const uint32_t step = IsGuildWarBattle() ? kGuildWarEscalationDamageStepPct : kPvpEscalationDamageStepPct;
        return 100u + EscalationStacks() * step;
    }
    uint32_t EscalationSupportPct() const {
        const uint32_t step = IsGuildWarBattle() ? kGuildWarEscalationSupportStepPct : kPvpEscalationSupportStepPct;
        const uint32_t drop = EscalationStacks() * step;
        const uint32_t maxDrop = 100u - kPvpEscalationSupportFloorPct;
        return drop >= maxDrop ? kPvpEscalationSupportFloorPct : (100u - drop);
    }
    uint32_t SurrenderUnlockRound() const {
        return IsPveMatch() ? 0u : kPvpSurrenderUnlockRound;
    }
    // PVP 第 5 回合起投降 / 同队投降表态；返回 0 成功并可选回填本队最新状态，非 0 为 tip 错误码。
    uint32_t SubmitSurrenderVote(uint64_t playerId, bool agree, BattleSurrenderVoteState* outVoteState = nullptr);
    // 仅向同队参战者回填本队投降投票或冷却态（对手与观众视角由节点清空，绝不泄密）。
    void FillSurrenderVoteForViewer(BattleStateS2C& state, uint64_t viewerPlayerId) const;
    // 论剑 V3 战前 15 秒明牌变阵（换赛季印 / 换首发宠 / 锁定出战）。
    bool IsInPrepPhase() const { return prepActive; }
    bool AllPrepPlayersReady() const;
    uint32_t UpdateBattlePrep(uint64_t playerId, uint32_t selectedSealId, uint64_t selectedPetId,
                              bool ready, BattlePrepState* outPrepState = nullptr);
    bool FinalizePrepPhase();
    void FillPrepStateForViewer(BattleStateS2C& state, uint64_t viewerPlayerId, uint64_t deadlineMs) const;
    // 论剑 V4 队内一键战术标点：每名玩家每回合（含变阵期独立计 1 轮）至多 2 次，仅同队可见。
    uint32_t SubmitBattlePing(uint64_t playerId, uint64_t targetActorId, BattlePingKind kind,
                              BattleTacticalPing* outPing = nullptr);
    void FillTeamPingsForViewer(BattleStateS2C& state, uint64_t viewerPlayerId) const;
    // 查询指定参战者的阵营（0/1），不存在返回 std::nullopt。
    std::optional<uint32_t> TeamIndexOfPlayer(uint64_t playerId) const;
    void MarkPlayerEntered(uint64_t playerId);
    BattleParticipationFact BuildParticipationFact(uint64_t playerId) const;
    bool DidPlayerParticipate(uint64_t playerId) const;

    // 论剑同速确定性决胜(SplitMix64): 仅看开局冻结 seed、回合号与 actor_id,重连与回放逐回合完全一致。
    static uint64_t SplitMix64(uint64_t x) {
        x += 0x9E3779B97F4A7C15ULL;
        x = (x ^ (x >> 30)) * 0xBF58476D1CE4E5B9ULL;
        x = (x ^ (x >> 27)) * 0x94D049BB133111EBULL;
        return x ^ (x >> 31);
    }
    static uint64_t RankedTurnTieKey(uint64_t seed, uint32_t roundIdx, uint64_t actorId) {
        return SplitMix64(seed ^ SplitMix64(static_cast<uint64_t>(roundIdx)) ^ SplitMix64(actorId));
    }

    // 重连补拉的全量状态。action_deadline_ms 引擎不填(引擎无时钟),
    // 由 battle 节点按房间 timer 回填。
    // 注意:这是"全知"版本,含全员冷却与全员战斗类属性(combat)。下发给客户端前必须由节点按收信人裁剪
    // (BattleRoomManager::RedactStateForViewer),否则对手/观众能直接读到别人的冷却与装备概率。
    BattleStateS2C BuildStateSnapshot() const;

    // 下发前清洗:把只供引擎判定用、任何客户端都不该看到的字段从一份出站状态里剔掉。
    // 目前只有 BattleActorState.combat(装备带来的必杀 / 连击 / 反击 / 反震 / 抗性概率):
    // 对手的不能暴露,自己的看属性面板,所以不分视角一律清空(equipment-attributes.md §4.5)。
    // 只动传入的拷贝,不碰引擎状态;放在引擎库里是为了让"哪些字段只属于引擎"只有一处定义,
    // 且引擎单测能直接覆盖(节点的 RedactStateForViewer 对每一份出站快照都调它)。
    static void StripEngineOnlyState(BattleStateS2C& state);

    // 某玩家当前剩余的战斗道具副本(item_table_id → 剩余个数),按 item_table_id 升序。
    // 数量只存在于引擎私有的开局快照副本里(ExecuteItem 原地递减),节点按视角回填
    // BattleStateS2C.self_items 时用它。玩家不在本局返回空表。
    std::vector<BattleItemEntry> SelfItems(uint64_t playerId) const;

    // 当前对局是否属于首领试炼（1..4 / 8），供房间管理器应用 18 秒紧凑指令窗等试炼策略。
    bool IsBossTrialBattle() const { return IsBossTrialKind(bossTrialKind); }
    // 本局冻结的首领试炼周常五行词缀（0=无/非首领试炼或爬塔，1金/2木/3水/4火/5土）。
    uint32_t BossWeeklyAffixId() const { return bossWeeklyAffixId; }

    // 开局时被忽略的战斗剧本及原因(英文,每条一句;CreateBattleRequest.team_scripts 里不合法的条目)。
    // 引擎不打日志,由节点在 Initialize 成功后逐条记 ERROR;被忽略的队出手照旧,对局照常进行
    const std::vector<std::string>& ScriptRejections() const { return scriptRejections; }

    // 副本怪物智能决策配置(key = monster_table_id, 0 为缺省兜底配置;须在 Initialize 前设置)。
    // 不设置时不启用,保持存量无 AI 对局的逐字节确定性回放完全不变。
    void SetMonsterAiProfiles(std::map<uint32_t, DungeonMonsterAiProfile> profiles);
    // 只读查询指定怪物当前的 Boss HFSM 阶段(未绑定 AI 时返回 std::nullopt)。
    std::optional<BossPhaseId> MonsterAiPhaseOf(uint64_t actorId) const;

    // 回合制可施放的技能类型**黑名单**:拒绝 Passive(被动) / Toggle(开关) /
    // Channel(持续施法,引擎已删相位概念);skill_type 为空或只含其它位号一律放行
    // (存量表未必每行都填了 skill_type,白名单会把没填的全判死)。
    // 表里 skill_type 存的是位号(0..5),不是掩码。职业被动另按 passive_stat 非空判定
    // (被动位号是 0,导表器把重复槽位里的 0 当空格丢掉,skill_type 写不出它)。
    // 与 scene 侧 PlayerSkillSystem::IsTurnBattleCastable 逐条同源:改一处必须同改另一处。
    // 只看表行、不读引擎状态;公开给战斗剧本的开局前校验(battle_script.h),判据只此一份。
    static bool IsTurnBattleCastableSkill(const SkillTable& skillRow);

    // 老技能(class_id == 0)的耗蓝:cost_resource[] 里法力资源的 cost 之和。职业技能按最大法力比例算,不走这里。
    // 公开给战斗剧本校验「怪物只能放零耗蓝的技能」(怪物没有法力列),与出手时的耗蓝同一份算法
    static uint64_t LegacySkillManaCost(const SkillTable& skillRow);

private:
    // 单次效果调用的临时上下文,不保存战斗状态;状态始终由BattleActorState/BattleBuffEntry承载。
    struct EffectContext {
        uint32_t skillId = 0;
        uint32_t rank = 1;
        uint64_t paidHealth = 0;
        uint64_t paidMana = 0;
        uint64_t actualDamage = 0;
        uint64_t instigatorId = 0; // 催化持续伤害的行动者,原始伤害归属仍为Buff来源。
        BattleDamageOrigin origin = BattleDamageOrigin::Direct;
        double scale = 1.0;
        uint32_t hitOrdinal = 0;  // 同一目标的段序,与表现层多目标索引分开。
        uint32_t periodicState = 0;
        bool critical = false;
        uint64_t primaryTargetId = 0;
        uint32_t healOrdinal = 0;
        uint32_t shieldOrdinal = 0;
        bool equipmentHeal = false;
        bool healingSnapshot = false;
        bool isMultiTargetSkill = false;
        bool primaryFocusBonus = false;
        bool isPrimaryFocus = false;
        bool isPreparedCast = false;
    };
    bool ConfiguredSubjectAllowed(const BattleActorState& actor, const SkillTable& row) const;
    uint32_t ConfiguredRank(const BattleActorState& actor, const SkillTable& row) const;
    uint32_t CheckConfiguredSkill(const BattleActorState& actor, const BattleAction& action,
                                  const SkillTable& row) const;
    bool ConfiguredTargetAllowed(const BattleActorState& actor, const BattleActorState& target,
                                  const SkillTable& row) const;
    std::vector<uint64_t> SelectConfiguredTargets(const BattleActorState& actor, const SkillTable& row,
                                                 uint64_t primaryId);
    void ExecuteConfiguredSkill(BattleActorState& actor, const BattleAction& action,
                                 const SkillTable& row, TurnResultS2C& result,
                                 BattleDamageOrigin origin = BattleDamageOrigin::Direct,
                                 double scale = 1.0, uint32_t rankOverride = 0);
    bool HasValidConfiguredTarget(const BattleActorState& caster, const SkillTable& row) const;
    bool IsSkillCopyableFor(const BattleActorState& caster, const SkillTable& row) const;
    bool HasCopyableSkillFor(const BattleActorState& caster, const BattleActorState& target) const;
    std::optional<uint32_t> SelectCopiedSkillId(const BattleActorState& caster, const BattleActorState& target);
    double ConfiguredValue(const BattleActorState& caster, const BattleActorState& target,
                            const BuffTable& row, const EffectContext& context) const;
    void ApplyConfiguredEffect(BattleActorState& caster, BattleActorState& target, uint32_t buffId,
                                const EffectContext& context, TurnResultS2C& result, uint32_t depth = 0);
    uint64_t ResolveConfiguredDamage(BattleActorState& source, BattleActorState& target, double damage,
                                     uint32_t damageType, const EffectContext& context,
                                     TurnResultS2C& result, eBattleHitKind hitKind = BATTLE_HIT_NORMAL);
    uint64_t ResolveConfiguredHeal(BattleActorState& source, BattleActorState& target, double amount,
                                   const EffectContext& context, TurnResultS2C& result);
    void TriggerConfiguredBuffs(BattleActorState& owner, BattleActorState& other, BattleTrigger trigger,
                                uint32_t damageType, const EffectContext& context, TurnResultS2C& result);
    double ConfiguredModifier(const BattleActorState& actor, BattleModifier kind,
                              SupportEffectBreakdown* sources = nullptr) const;
    void BuildSupportEffects(const BattleActorState& actor, SupportEffectSummary& output) const;
    bool HasConfiguredBuffs(const BattleActorState& actor) const;
    bool HasConfiguredState(const BattleActorState& actor, BattleStateKind state) const;
    bool HasBuffTag(const BattleActorState& actor, const std::string& tag) const;
    void InitializeConfiguredAbilities();
    void TickConfiguredBuffs(TurnResultS2C& result);
    void BeginConfiguredRound(TurnResultS2C& result);
    bool ExecuteConfiguredControl(BattleActorState& actor, const BattleAction& action, TurnResultS2C& result);
    std::optional<BattleAction> SelectAutoConfiguredSkill(const BattleActorState& actor) const;
    void RemoveConfiguredBuff(BattleActorState& actor, uint64_t buffId, TurnResultS2C& result);
    uint64_t EffectiveMagicAttack(const BattleActorState& actor) const;
    double EffectiveMitigationScore(const BattleActorState& actor, bool isMagic) const;
    bool IsPhysicalForbidden(const BattleActorState& actor) const;
    bool IsMagicForbidden(const BattleActorState& actor) const;
    bool TryConsumeControlImmunity(BattleActorState& caster, BattleActorState& target,
                                   const BuffTable& row, TurnResultS2C& result);
    void IncrementHitVulnerability(BattleActorState& target);
    double HitVulnerabilityBonus(const BattleActorState& target) const;
    void ResetVolatileGrowingStacks(BattleActorState& actor);
    bool ExecutePreparedCastIfReady(BattleActorState& actor, TurnResultS2C& result);
    void SyncChargingSkillId(BattleActorState& actor) const;
    void ConsumeOneShotAttackModifiers(BattleActorState& source, uint32_t damageType, TurnResultS2C& result);

    // 装备静态来源来自 scene，次数只由本局状态拥有；按 item_id 排序使双同名预算分配稳定。
    bool InitializeEquipment(BattleActorState& actor, const EquipmentEffects& effects);
    uint32_t EquipmentManaCategory(const SkillTable& skill) const;
    std::vector<int> AvailableArtifacts(const BattleActorState& actor, uint32_t effect) const;
    void UseArtifact(BattleActorState& actor, int index, uint64_t targetId, uint64_t value,
                       TurnResultS2C& result);
    bool SeasonSealAvailable(const BattleActorState& actor, uint32_t effect) const;
    void UseSeasonSeal(BattleActorState& actor, uint64_t targetId, uint64_t value,
                       TurnResultS2C& result);
    void EquipmentDefend(BattleActorState& actor, TurnResultS2C& result);
    uint64_t EquipmentReduceDamage(BattleActorState& source, BattleActorState& target,
                                    uint64_t damage, const EffectContext& context, TurnResultS2C& result);
    void EquipmentBreakShields(BattleActorState& source, BattleActorState& target,
                                uint64_t damage, uint32_t type, const EffectContext& context,
                                const std::vector<uint64_t>& shields, TurnResultS2C& result);
    bool EquipmentControlRoll(BattleActorState& caster, BattleActorState& target, const BuffTable& row,
                                const EffectContext& context, TurnResultS2C& result);
    double EquipmentOutgoingHeal(const BattleActorState& source) const;
    double EquipmentShield(BattleActorState& source, BattleActorState& target, double base,
                             const EffectContext& context, std::vector<int>& contributions) const;
    void CommitEquipmentShield(BattleActorState& source, uint64_t targetId,
                                const std::vector<int>& contributions, TurnResultS2C& result);

    friend class TurnBattleEngineDeathTestAccess; // 内存测试注入周期伤害来源，不新增业务控制接口
    friend class TurnBattleEngineEquipmentTestAccess; // 回放重排/缺失运行状态回归，不开放业务写接口。
    // ---- 初始化 ----

    bool InitPlayers(const CreateBattleRequest& request);
    bool InitMonsters(const CreateBattleRequest& request);
    bool IsValidRewardProfile(const BattleRewardProfile& profile) const;
    // 出战宝宝:与主人同队的独立行动单位(player-pet.md §5)。
    // 新技能宝宝支持主人独立提交行动;旧快照由 FillDefaultActions 选可用技能或普攻,
    // 因此不进 AllPlayersReady 的就绪判定 —— 带宝宝不会拖慢任何一个回合。
    bool InitPets(const CreateBattleRequest& request);
    void AppendMonsterActor(uint32_t monsterTableId, uint32_t monsterIndex, uint32_t referenceLevel);
    // 阵位分配(表现规格 D4):该队下一个空位 = 该队已有单位数(插入序即阵位序)
    uint32_t NextFormationSlot(uint32_t teamIndex) const;

    // ---- 行动校验链(镜像 SkillSystem::CheckSkillPrerequisites,去掉相位校验) ----

    uint32_t CheckActionPrerequisites(const BattleActorState& actor, const BattleAction& action) const;
    uint32_t ValidateSkillTarget(const BattleActorState& actor, const SkillTable& skillRow, uint64_t targetId) const;
    uint32_t CheckCooldown(const BattleActorState& actor, const SkillTable& skillRow) const;
    uint32_t CheckPlayerLevel(const BattleActorState& actor, const SkillTable& skillRow) const;
    uint32_t CheckBuff(const BattleActorState& actor, const SkillTable& skillRow) const;
    uint32_t CheckState(const BattleActorState& actor) const;
    // 耗蓝校验:耗蓝量(SkillManaCost)须 <= 当前法力
    uint32_t CheckSkillCost(const BattleActorState& actor, const SkillTable& skillRow) const;
    // ITEM 校验:表行存在且 battle_usable、快照副本里还有余量、目标合法(自己或同队存活单位)、
    // PVP 场次未超每人上限。这是提交期与出手期共用的唯一判据。
    uint32_t CheckItemUse(const BattleActorState& actor, const BattleAction& action) const;
    // ---- 四职业技能(docs/design/class-skills.md §6;Skill.class_id != 0 的行) ----

    static bool IsClassSkill(const SkillTable& skillRow) { return skillRow.class_id() != 0; }
    // 本局生效等级:快照 skill_ranks 经 InitPlayers 复核后的值;不是本单位职业的技能 / 没学为 0
    uint32_t ClassSkillRank(const BattleActorState& actor, const SkillTable& skillRow) const;
    // 目标校验(提交期与出手期共用):自身技能只收 0 或自己;敌方 / 友方技能收 0(由引擎按规则选)
    // 或对应阵营的单位 id。选中的单位此刻已阵亡 / 逃离不算错 —— 出手时按规则另选,不降级成别的行动
    uint32_t ValidateClassSkillTarget(const BattleActorState& actor, const SkillTable& skillRow,
                                      uint64_t targetId) const;
    // 目标集(不消耗随机数):自身技能只有自己;否则在存活的对应阵营单位里,选中的合法主目标排第一,
    // 其余治疗技能按气血比例升序、其它按阵位,再按 actor_id;取前 max(1, max_targets) 个
    std::vector<uint64_t> SelectClassSkillTargets(const BattleActorState& actor, const SkillTable& skillRow,
                                                  uint64_t primaryTargetId) const;
    // 出手:选目标(选不到就整手作废、不扣蓝不进冷却)→ 冷却 → SKILL → 耗蓝 → 逐目标落地
    void ExecuteClassSkill(BattleActorState& actor, const BattleAction& action, const SkillTable& skillRow,
                           TurnResultS2C& result);
    // 单目标落地:敌方技能 命中 → 伤害(× 等级系数)→ 死亡;之后(目标仍活着)治疗 → 回蓝 → 驱散 → 增减益
    void ApplyClassSkillToTarget(BattleActorState& actor, const SkillTable& skillRow, uint32_t rank,
                                 double baseDamage, BattleActorState& target, TurnResultS2C& result, uint64_t primaryTargetId);
    // 伤害基数:表达式求值(职业技能表里是常数 10),等级参数吃"所有技能上升"。出手与挂机估算共用
    double ClassSkillBaseDamage(const BattleActorState& actor, const SkillTable& skillRow) const;
    // 治疗量(未按缺口封顶):(法伤 × 倍率 + 施法者等级 × 每级加值) × 等级系数 × (1 + 治疗加成被动),
    // 目标气血比例低于残血阈值再乘 1 + execute_heal_bonus。出手与挂机估算共用
    double ClassSkillHealAmount(const BattleActorState& actor, const SkillTable& skillRow, uint32_t rank,
                                const BattleActorState& target) const;
    // 本回合行动的先手优先级:只有通过校验的职业技能行动才带 Skill.initiative,其余为 0
    int32_t ActionInitiative(const BattleActorState& actor) const;
    // 战斗专用被动"治疗加成"之和(按 skill_table_id 升序累加,浮点求和顺序固定)
    double OutgoingHealBonus(const BattleActorState& actor, SupportEffectBreakdown* sources = nullptr) const;
    // 职业技能增减益对某项属性的倍率:1 + 同向最强增益 + 同向最强减益,下限 kMinClassBuffStatMultiplier。
    // 身上没有这类 buff 时恰好返回 1.0。extraPercent != 0 时按"再挂一个这么大幅度的"算,不改状态
    // (挂机选技比较挂上前后的差;已有同向等强或更强的效果时倍率不变)
    double ClassBuffStatMultiplier(const BattleActorState& actor, ClassBuffStat stat,
                                   double extraPercent = 0.0) const;
    // 吃增减益之后的物伤 / 防御 / 速度。没有这类 buff 时原样返回快照值(不经浮点),老对局逐位不变
    uint64_t EffectivePhysicalAttack(const BattleActorState& actor) const;
    uint64_t EffectiveDefense(const BattleActorState& actor) const;
    uint64_t EffectiveSpeed(const BattleActorState& actor) const;
    // 新模型优先显式善恶标签,未标时按属性方向判定;旧模型保留原有语义。
    bool IsDebuffEntry(const BattleBuffEntry& entry) const;
    // 仅源技能init_effect中的被动/追击入口不可驱散,它们生成的子效果不继承保护。
    bool IsConfiguredListenerBuff(const BattleBuffEntry& entry) const;
    // 按身上顺序移除至多 count 个减益,返回实际移除个数
    uint32_t DispelDebuffs(BattleActorState& target, uint32_t count, TurnResultS2C& result);

    // ---- 职业玩家挂机选技(docs/design/class-skills.md §6.5) ----

    // 给挂机玩家选这一回合的职业主动技能;没有值得放的返回空,调用方回落普攻。
    // 只读当前战斗状态:不扣蓝、不进冷却、不发事件、不消耗随机数,也不读同轮他人已提交的指令。
    // 候选按 用途优先级 → 有效收益 → 耗蓝 → 冷却 → 技能号 → 目标号 排成全序,结果不依赖技能列表的遍历顺序
    std::optional<BattleAction> SelectAutoClassSkill(const BattleActorState& actor) const;
    // 可作主目标的单位:自身技能只有自己;否则是存活未逃的对应阵营单位
    std::vector<uint64_t> CollectClassSkillPrimaryTargets(const BattleActorState& actor,
                                                          const SkillTable& skillRow) const;
    // 下面五个是各用途在实际目标集上的预计收益,targetIds 来自 SelectClassSkillTargets,0 = 没有收益。
    // 按各目标缺口封顶的治疗量
    uint64_t AutoHealGain(const BattleActorState& actor, const SkillTable& skillRow, uint32_t rank,
                          const std::vector<uint64_t>& targetIds) const;
    // 能移除的减益个数;主目标(首位)自己没有减益时为 0,显式目标必须真带减益
    uint64_t AutoDispelGain(const SkillTable& skillRow, const std::vector<uint64_t>& targetIds) const;
    // 净回蓝 = 实际回蓝 − 本次耗蓝;目标法力都高于 kAutoRestoreManaRatio 时为 0
    uint64_t AutoRestoreManaGain(const BattleActorState& actor, const SkillTable& skillRow, uint32_t rank,
                                 const std::vector<uint64_t>& targetIds) const;
    // 增减益实际改变的属性点数(敌方技能只算减益,友方 / 自身技能只算增益)
    uint64_t AutoStatBuffGain(const BattleActorState& actor, const SkillTable& skillRow, uint32_t rank,
                              const std::vector<uint64_t>& targetIds) const;
    // 按剩余气血封顶的预计伤害,暴击取期望
    uint64_t AutoDamageGain(const BattleActorState& actor, const SkillTable& skillRow, uint32_t rank,
                            double baseDamage, const std::vector<uint64_t>& targetIds) const;
    // 单个目标单项属性挂上 statPercent 后改变的点数。属性本身为 0、已有同向等强或更强的效果都是 0;
    // 防御增益只给气血不高于 kAutoDefenseBuffHealthRatio 的目标,速度增减益要能翻转出手先后
    uint64_t ClassBuffStatGain(const BattleActorState& target, ClassBuffStat stat, double statPercent) const;
    // 速度从 speedBefore 变成 speedAfter,是否翻转 target 与对面任一存活单位的先后(同速按 actor_id,同 BuildTurnOrder)
    bool SpeedChangeFlipsOrder(const BattleActorState& target, uint64_t speedBefore, uint64_t speedAfter) const;
    // 普攻的比较基线:暴击与连击取期望、按剩余气血封顶,默认普攻目标随机,所以取各存活敌人的平均
    double AutoBasicAttackDamage(const BattleActorState& actor) const;

    // ---- 战斗剧本(docs/design/battle-script.md) ----

    // 本回合各角色位已选的目标:(role_id, target_id)。手动提交的行动也登记,跟随的角色位可以接着打。
    // 只活在一次 FillScriptActions 里,按决策顺序追加;一队至多 10 个单位,线性查找
    using ScriptClaims = std::vector<std::pair<uint32_t, uint64_t>>;

    // 开局:取建房原件里每队的剧本,按本节点配表再校验(不合法记进 scriptRejections 并忽略),
    // 按角色位 order 给本队单位认领角色位;认领了角色位的怪物把该角色位规则里的技能并进 skill_table_ids
    // (怪物表没有技能列,怪物的技能集由剧本派生)
    void BindBattleScripts();
    // 每回合 FillDefaultActions 开头:按角色位 order 替本队还没有行动的单位选行动
    // (挂机 / 超时的玩家、宝宝、怪物);已提交的手动行动一律不动
    void FillScriptActions();
    // 按规则 priority 取第一条能执行的:条件全成立、挑得出目标、过 CheckActionPrerequisites;都不行返回空。
    // 只读当前状态,不消耗随机数
    std::optional<BattleAction> SelectScriptAction(const BattleActorState& actor, const BattleScriptRole& role,
                                                   const ScriptClaims& claims) const;
    bool ScriptConditionHolds(const BattleActorState& actor, const BattleScriptCondition& condition) const;
    bool ScriptFilterHolds(const BattleActorState& candidate, const BattleScriptTargetFilter& filter,
                           uint32_t ownRoleId, const ScriptClaims& claims) const;
    // ENEMY / ALLY 的候选:存活未逃、筛选全过,按 order 排好(同分按阵位、再按 actor_id 无符号升序)
    std::vector<uint64_t> CollectScriptTargets(const BattleActorState& actor, const BattleScriptTarget& target,
                                               uint32_t ownRoleId, const ScriptClaims& claims) const;
    static bool ScriptRoleAccepts(const BattleScriptRole& role, const BattleActorState& actor);

    // ---- 回合结算 ----

    void FillDefaultActions();
    std::vector<uint64_t> BuildTurnOrder() const;
    void ExecuteAction(BattleActorState& actor, const BattleAction& action, TurnResultS2C& result);
    // 普攻:选目标 → ATTACK 事件 → 首段 → (连击率成立)追加段 → (受击者反击率成立)反击。
    // 连击 / 反震 / 反击只挂在这条路径上(含技能校验失败降级成的普攻),技能不触发
    void ExecuteAttack(BattleActorState& actor, uint64_t targetId, TurnResultS2C& result);
    // 普攻的一段,固定顺序:命中 → 必杀 → 首段 Buff 伤害事件 → 落伤害(致死走 HandleDeath)→ 反震。
    // hitKind 标在本段产出的 MISS / DAMAGE 事件上,并决定本段的规则:
    //   NORMAL  首段,伤害原样落地;
    //   COMBO   连击追加段,伤害 × kComboDamagePercent%;
    //   COUNTER 反击,伤害原样落地,且**不**触发对方的反震(反击不再引发任何连锁)。
    // 返回本段是否命中。出手者可能在本段被反震弹死,调用方用 IsActorActive(attacker) 判断能否继续
    bool ResolveBasicAttackHit(BattleActorState& attacker, BattleActorState& target,
                               eBattleHitKind hitKind, TurnResultS2C& result, uint64_t primaryTargetId = 0);
    // 普攻首段落血前消费“下次普攻”Buff。先快照实例 ID,再逐个重查;触发中新加的条目留给下次普通行动。
    // 复制施加者上下文后完成本次子效果,不持有跨 AddBuffToActor 的容器引用;返回附伤后的原始伤害。
    double TriggerBeforeBasicAttackBuffs(BattleActorState& attacker, BattleActorState& target,
                                         double rawDamage, TurnResultS2C& result);
    // 反震:target 刚被 attacker 的一段普攻实扣 dealt 点且仍存活时,按 target 的 reflect_rate 掷骰;
    // 成立则 attacker 受到 ceil(dealt × kReflectDamagePercent%)(至少 1,不超过其当前气血;
    // 不吃减伤、不必杀、不受防御指令影响),发 DAMAGE(source=target,target=attacker,hit_kind=REFLECT)。
    // attacker 被弹死走 HandleDeath,击杀归属 target
    void TryReflectDamage(BattleActorState& attacker, BattleActorState& target, uint64_t dealt,
                          TurnResultS2C& result);
    // 反击:整次普攻结束后,attacker 与 defender 都在场、defender 当前能行动(CheckState)时,
    // 按 defender 的 counter_rate 掷骰;成立则 defender 另起一个事件组对 attacker 打一次普攻
    // (ATTACK + DAMAGE,hit_kind=COUNTER,可必杀)。反击不再触发连击 / 反击 / 反震
    void TryCounterAttack(BattleActorState& attacker, BattleActorState& defender,
                          TurnResultS2C& result);
    void ExecuteSkill(BattleActorState& actor, const BattleAction& action, TurnResultS2C& result);
    // 技能对单个目标落地:命中判定 → 伤害 → 死亡 → effect[] buff;
    // 多目标(AOE)技能对每个目标依次调用,hit_index 由调用方递增
    void ApplySkillToTarget(BattleActorState& actor, const BattleAction& action,
                            const SkillTable& skillRow, double baseDamage,
                            BattleActorState& target, TurnResultS2C& result);
    void ExecuteDefend(BattleActorState& actor, TurnResultS2C& result);
    void ExecuteItem(BattleActorState& actor, const BattleAction& action, TurnResultS2C& result);
    void ExecuteFlee(BattleActorState& actor, TurnResultS2C& result);
    uint32_t CheckCapture(const BattleActorState& actor, uint64_t targetId) const;
    void ExecuteCapture(BattleActorState& actor, const BattleAction& action, TurnResultS2C& result);
    void TickBuffsAtRoundEnd(TurnResultS2C& result);
    void TickActorBuffs(BattleActorState& actor, TurnResultS2C& result);
    void ApplyBuffIntervalEffect(BattleActorState& actor, const BattleBuffEntry& entry,
                                 const BuffTable& buffRow, TurnResultS2C& result);
    void DecayCooldowns();
    void UpdateOutcome();
    // 终局一次性冻结每人的配置奖励；数量不随人数或击杀数变化，奖励随机不消费战斗 RNG。
    void RollRewards();
    // 服务端策略识别入口：当前返回空通用档，未来可根据该玩家的实际战斗行为选策略键。
    std::string SelectRewardStrategy(uint64_t playerId) const;
    // 快照带入的 buff 清洗(G5):丢掉表缺失/控制类/瞬时类条目,把 caster_id 从
    // scene 的 entt 整数域改写到局内 actor_id 域(认不出来的一律置 0)。
    void SanitizeSnapshotBuffs(BattleActorState& actor);

    // ---- 伤害/治疗/buff ----

    // 伤害公式与实时技能共用 combat_damage_rules.h(比例减伤,常驻减伤封顶 60%):
    // attack 普攻取物伤、技能按 SkillTable.damage_type 取物伤或法伤;attackMultiplier 取 attack_multiplier。
    // damageType 用 combatdamage::kPhysicalDamage / kMagicDamage(普攻 = 物理,技能 = SkillTable.damage_type),
    // 决定吃哪一组战斗类属性:暴击率 = critchance + 出手者的 物理 / 法术必杀率(夹到 [0, 100]),
    // 抗性 = resistance + 目标的 抗物理 / 抗法术(规则头内部夹取);未知类型两边都不吃。
    // 非 PVE 对局再乘 kPvpDamageScale,之后按暴击率 × kCriticalDamageMultiplier。isCritical 回传是否暴击。
    // 战斗类属性全 0 时,结果与随机数消耗和加这个参数之前逐位一致
    double CalculateFinalDamage(const BattleActorState& caster, const BattleActorState& target,
                                double baseDamage, uint64_t attack, double attackMultiplier,
                                uint32_t damageType, bool& isCritical);
    // CalculateFinalDamage 掷暴击之前的部分(公式伤害 × PVP 系数)与合计暴击率,结算与挂机估算共用一份公式
    double DamageBeforeCriticalRoll(const BattleActorState& caster, const BattleActorState& target,
                                    double baseDamage, uint64_t attack, double attackMultiplier,
                                    uint32_t damageType) const;
    double CriticalChance(const BattleActorState& caster, uint32_t damageType) const;
    // 暴击按概率取期望的伤害:不掷骰、不消耗随机数(挂机选技用,真实结算走 CalculateFinalDamage)
    double ExpectedDamage(const BattleActorState& caster, const BattleActorState& target,
                          double baseDamage, uint64_t attack, double attackMultiplier,
                          uint32_t damageType) const;
    // 当前对局是否 PVE(单人 / 组队):决定能否逃跑、是否乘 PVP 伤害系数
    bool IsPveMatch() const;
    // 命中判定骨架(表现规格 D1):命中率 = kBaseHitRate(一期表无命中/闪避列)。
    // 命中率 >= 100 直接返回 true 且不消耗随机数,保证既有回放基线不变;
    // 二期接表后在此处减去目标闪避并用 Rand01 掷骰
    bool RollHit(const BattleActorState& caster, const BattleActorState& target);
    // 技能耗蓝量:职业技能 = ceil(最大法力 × mana_percent)(比例 > 0 时至少 1);
    // 老技能 = cost_resource[] 中 cost_resource_id == kSkillCostResourceMana 的 cost 之和
    uint64_t SkillManaCost(const BattleActorState& actor, const SkillTable& skillRow) const;
    // 扣蓝并产出 BATTLE_EVENT_MANA(source=target=施法者,value=实际消耗,mana_delta=-实际消耗,
    // target_mana_after=扣后法力);耗蓝为 0 不产出事件
    void ConsumeSkillMana(BattleActorState& actor, const SkillTable& skillRow, uint32_t skillTableId,
                          TurnResultS2C& result);
    // targeting_mode 含 AOE 位:对全部存活敌方生效
    bool IsAreaSkill(const SkillTable& skillRow) const;
    // 落伤害:DEFEND 减半,ceil 取整,饱和到 0 HP;返回实际扣血
    uint64_t ApplyDamage(BattleActorState& target, double rawDamage);
    uint64_t ApplyHeal(BattleActorState& target, double rawHeal);
    // 这一下实际会扣 / 回多少气血,不改状态(落地与挂机估算共用)
    uint64_t LandedDamage(const BattleActorState& target, double rawDamage) const;
    uint64_t LandedHeal(const BattleActorState& target, double rawHeal) const;
    void HandleDeath(BattleActorState& target, uint64_t sourceActorId, TurnResultS2C& result);

    // effect[] → buff:镜像 BuffSystem::AddOrUpdateBuff 的
    // 免疫检查 → (回合制新增)抗异常 → 驱散 → 叠层/刷新 → 新建 → 子 buff 语义,时间维度换算为回合。
    // 被抵抗时只发 BATTLE_EVENT_RESIST(source=施加者,target=抵抗者),不加 buff、不走驱散。
    // statPercent != 0 是职业技能的增减益:幅度写进条目 stat_percent、施加回合写进 applied_round
    // (本回合末不扣持续);下次普攻 Buff 同样保留完整回合;子 buff 不继承幅度
    void AddBuffToActor(BattleActorState& target, uint32_t buffTableId, uint64_t casterId,
                        uint32_t depth, TurnResultS2C& result, double statPercent = 0.0);
    // 挂载完成后的 Start:普通类型展开宿主子 Buff;“下次普攻”延后到伤害事件。
    // 父实例中途被子效果驱散则停止其余 Start 子项;target_sub_buff 只由带实际受击目标的事件解释。
    void OnBuffStart(BattleActorState& target, uint64_t buffId, const BuffTable& buffRow,
                      uint32_t depth, TurnResultS2C& result);
    bool IsImmuneToBuff(const BattleActorState& target, const BuffTable& buffRow) const;
    // 抗异常判定(非 const:可能消耗一个随机数)。只在三个条件同时成立时才可能抵抗:
    // 施加者是本局单位、与目标不同队(自己或队友挂的不判)、buff 类型在 kAilmentBuffMappings 里。
    // 有效抵抗率 = max(0, 单项 + resist_all_ailment − 施加者.ignore_ailment_resist);
    // 为 0(必不抵抗)或 >= 100(必抵抗)都不掷骰,只有 1..99 消耗一个随机数(RollPercent 两端短路)
    bool RollAilmentResist(const BattleActorState& target, const BuffTable& buffRow,
                           uint64_t casterId);
    uint32_t DispelBuffsByTag(BattleActorState& target, const BuffTable& buffRow, TurnResultS2C& result);
    // 重挂持续刷新为满。职业属性 Buff 幅度取更强者;属性 / 下次普攻 Buff 的施加回合记为本回合。
    bool StackOrRefreshExistingBuff(BattleActorState& target, const BuffTable& buffRow,
                                    uint64_t casterId, TurnResultS2C& result, double statPercent);
    void RemoveBuffAt(BattleActorState& target, int buffIndex, TurnResultS2C& result);

    // ---- 查询辅助 ----

    BattleActorState* FindActor(uint64_t actorId);
    const BattleActorState* FindActor(uint64_t actorId) const;
    // 开局快照副本里某玩家的某个道具条目(剩余数的唯一真相)。同一 item_table_id
    // 出现多条时取第一条余量 > 0 的;找不到返回 nullptr。
    BattleItemEntry* FindItemEntry(uint64_t playerId, uint32_t itemTableId);
    const BattleItemEntry* FindItemEntry(uint64_t playerId, uint32_t itemTableId) const;
    // 按 buff 实例 id 找下标,找不到返回 -1(tick 过程中条目会增删,须按 id 重查)
    int FindBuffIndex(const BattleActorState& actor, uint64_t buffId) const;
    bool IsActorActive(const BattleActorState& actor) const;  // 存活且未逃跑
    bool ActorHasBuffOfType(const BattleActorState& actor, uint32_t buffType) const;
    uint64_t MaxAliveEnemySpeed(const BattleActorState& actor) const;
    std::vector<uint64_t> CollectAliveEnemyIds(const BattleActorState& actor) const;
    bool SideWiped(uint32_t teamIndex) const;
    uint32_t CompletedRounds() const;
    // 追加事件并盖上当前事件组 group_id / hit_index(表现规格 D2)
    BattleEventItem* AppendEvent(TurnResultS2C& result, eBattleEventType eventType,
                                 uint64_t sourceId, uint64_t targetId);
    // 开启新事件组:group_id 每回合从 1 起递增,hit_index 归 0。
    // 每个行动(普攻/技能/道具/防御/逃跑)一组;回合末每个单位的 buff tick 各一组;
    // 反击是受击者的另一次出手,也另起一组(客户端按 group_id 分拍,同组的 DAMAGE 会并成一拍)。
    // 连击追加段与反震留在原行动的组里,靠 hit_index / hit_kind 区分
    void BeginEventGroup();

    // ---- 确定性随机(禁 tlsRandom / rand(),宪法 §7 新增不变量 5) ----

    // [0, count) 均匀整数(count 必须 > 0)
    uint64_t RandIndex(uint64_t count);
    // [0, 1) 实数。不走 std::uniform_real_distribution(其实现跨平台不定),
    // 直接取 mt19937_64 的高 53 位,保证任何平台上同种子同序列
    double Rand01();
    // 整数百分点的概率判定(连击 / 反击 / 反震 / 抗异常共用):percent <= 0 恒 false、>= 100 恒 true,
    // 这两端都**不消耗随机数**;1..99 消耗恰好一个 Rand01。
    // 0 端短路是硬约束:属性全 0 的单位(怪物、宝宝、无装备玩家)不能平移既有同种子回放
    bool RollPercent(uint64_t percent);

private:
    std::shared_ptr<BattleDataProvider> dataProvider;

    // 引擎状态全部用 proto 消息承载(宪法 §3:不手写与 proto 重复的并行 struct)
    CreateBattleRequest createRequest;                    // 开局请求副本(种子/模式/配置)
    BattleRewardProfile defaultRewardProfile;             // PVE 开局冻结的通用奖励档
    std::vector<BattleMonsterDefeat> defeatedMonsters;    // 按实际死亡转移顺序记录玩家方击杀，不能按阵位重建
    std::vector<BattleActorState> actors;                 // 全部战斗单位,插入序稳定
    std::map<uint64_t, BattleAction> pendingActions;      // actor_id → 本回合行动(有序容器保确定性)
    std::map<uint64_t, BattleSettlementData> settlements; // player_id → 结算累积(道具消耗账本等)
    std::map<uint64_t, uint32_t> itemUseCounts;           // player_id → 本场已用道具次数(PVP 限次用,有序容器保确定性)
    // 战斗剧本:下标是 team_index,空 = 该队没有剧本、出手全走现行默认。开局取自建房原件、校验后整场不变
    std::shared_ptr<const BattleScript> teamScripts[2];
    std::map<uint64_t, uint32_t> scriptRoleOf;            // actor_id → 认领的 role_id(有序容器保确定性)
    std::vector<std::string> scriptRejections;            // 开局被忽略的剧本及原因(见 ScriptRejections)
    // 副本怪物 HFSM + 行为树 + 整数效用 AI(有序容器保确定性,不消耗 RNG)
    std::map<uint32_t, DungeonMonsterAiProfile> monsterAiProfiles;
    std::map<uint64_t, DungeonMonsterAiController> monsterAiControllers;

    void BindDungeonMonsterAis();
    void FillDungeonMonsterAiActions();
    void RecordTrialDirectDamage(const BattleActorState& source, const BattleActorState& target,
                                 uint32_t damageType, uint64_t dealtOrAbsorbed);
    void RecordTrialMonsterHeal(const BattleActorState& source, const BattleActorState& target,
                                uint64_t healed);
    void RecordManualActionForActor(const BattleActorState& actor);
    void RecordEffectiveContribution(const BattleActorState& actor);
    uint64_t ApplyEncounterDamageModifiers(const BattleActorState& source, BattleActorState& target,
                                           uint64_t damage, uint32_t damageType, bool isDirectPrimary,
                                           TurnResultS2C& result, bool isMultiTargetSkill = false,
                                           bool isPrimaryTarget = true, bool* outSplashReduced = nullptr,
                                           bool isWeeklyChargedOrPeriodic = false, bool isPursuit = false);
    bool IsActorExposed(uint64_t actorId) const;
    bool IsWeeklyWaterDiscountSkill(const SkillTable& skillRow) const;
    void OnBossWeeklyAffixDispelTriggered(uint64_t casterActorId, TurnResultS2C& result);
    void OnBossWeeklyAffixParryTriggered(uint64_t bossActorId, TurnResultS2C& result);
    bool HasActiveEncounterDamageModifiers() const;
    bool IsActorControlLocked(const BattleActorState& actor) const;
    bool IsFestivalBeastBattle() const;
    bool IsEncounterSlowPhaseActor(uint64_t actorId) const;
    bool ActorHasBossCleaveParryProtection(const BattleActorState& target, uint32_t damageType) const;
    void ApplyBossExposed(uint64_t actorId, uint32_t pct, uint32_t untilRound);
    void EmitBossMechanicEvent(TurnResultS2C& result, uint32_t mechanicKind, uint64_t sourceId,
                               uint64_t targetId, uint64_t value);
    void TriggerRejuvBacklash(uint64_t sourceId, BattleActorState& boss, TurnResultS2C& result);
    void HandleBossTrialMonsterDeath(BattleActorState& deadMonster, TurnResultS2C& result);
    void ProcessBossTrialRoundEnd(TurnResultS2C& result);
    uint32_t ChooseDynamicStanceType(uint32_t currentRound) const;
    void InitSpecialEncounterState();
    void UpdatePolarityForRound();
    uint32_t NextPolaritySwitchRound() const;
    void RefreshEncounterTelegraphs();
    void ExecuteBossTrialSlowAndGuardianActions(TurnResultS2C& result);
    void TryDispelBossStance(const BattleActorState& source, const BattleActorState& target, TurnResultS2C& result);

    struct BossExposedState {
        uint32_t pct = 0;
        uint32_t untilRound = 0;
    };

    // 五行试炼志 V2 局内权威战法事实（仅内存确定性累积，不消耗 RNG）
    std::set<uint64_t> everDiedActors;
    uint64_t bossGuardHealTotal = 0;
    bool guardStanceDispelled = false;
    std::array<bool, 2> teamDealtPhysicalDirectDamage{false, false};
    std::array<bool, 2> teamDealtMagicDirectDamage{false, false};
    std::set<uint64_t> shieldBreakPlayers;
    std::set<uint64_t> antiControlOrSeasonSkillPlayers;

    // 赛季首领试炼、护卫救援、物法易势 5V5 与防挂机战中事实（全部确定性）
    uint32_t bossTrialKind = 0;
    uint32_t battleDifficulty = 0;
    uint32_t bossWeeklyAffixId = 0;
    uint32_t weeklyAffixWoodTeamBoostUntilRound = 0;
    uint32_t weeklyAffixEarthTeamBoostUntilRound = 0;
    std::set<uint64_t> weeklyAffixWoodManaTriggeredActors;
    uint32_t initialAttackerHumanCount = 0;
    uint64_t protectedGuardActorId = 0;
    uint32_t initialPolarity = 0;
    uint32_t activePolarity = 0;
    uint32_t bossStanceType = 0;
    uint32_t bossStanceRemainRounds = 0;
    uint32_t lastChosenStanceType = 0;
    uint64_t bossBarrierShield = 0;
    uint32_t bossPhase = 0;
    uint32_t nextPhase2CleaveTelegraphRound = 0;
    uint32_t bossMomentumStacks = 0;
    uint32_t bossDisheartenedUntilRound = 0;
    std::map<uint64_t, BossExposedState> exposedActors;
    std::map<uint64_t, uint32_t> sunderUntilRound;
    std::set<uint64_t> attackerTriggeredStanceCounterThisRound;
    std::map<uint32_t, std::pair<uint64_t, uint64_t>> roundPlayerDirectDamage;
    uint32_t cleaveParryCount = 0;
    uint32_t healInterruptBacklashCount = 0;
    uint32_t stanceBreakCount = 0;
    std::map<uint64_t, uint32_t> selfCleavesTargetedByPlayer;
    std::map<uint64_t, uint32_t> selfCleavesParriedByPlayer;
    std::vector<BattleTelegraphEntry> activeTelegraphs;
    std::vector<BattleTelegraphEntry> lastResolvedTelegraphs;
    uint64_t telegraphStateVersion = 0;
    std::map<uint64_t, std::set<uint32_t>> manualActionRoundsByPlayer;
    std::set<uint64_t> effectiveContributionPlayers;
    // 群仙问道赛 V3 MVP & 论剑 V4 复盘高光累计统计（本人及本人出战宝宝，每回合末从权威事件流确定性汇总）
    std::map<uint64_t, uint64_t> totalDamageDealtByPlayer;
    std::map<uint64_t, uint64_t> totalHealingDoneByPlayer;
    std::map<uint64_t, uint32_t> totalControlsLandedByPlayer;
    std::map<uint64_t, uint64_t> primaryFocusDamageByPlayer;
    std::map<uint64_t, uint64_t> totalShieldGrantedByPlayer;
    std::map<uint64_t, uint32_t> totalDispelsLandedByPlayer;
    std::map<uint64_t, uint64_t> damageTakenByPlayer;
    std::set<uint64_t> lowHpExperiencedPlayers;
    uint32_t peakEscalationStacks = 0;
    void AccumulateRoundMvpMetrics(const TurnResultS2C& result);
    void RecordDispelLanded(uint64_t casterActorId, uint32_t count);
    BattleHighlightKind ComputeHighlightKind(uint64_t playerId, uint32_t teamIndex,
                                             bool playerWon, bool isDead) const;
    bool IsHardControlBuff(uint32_t buffTableId) const;
    // 论剑 V4 队内一键战术标点（按阵营隔离，每名队友至多保留 1 条最新意图，回合推进清空）
    std::array<std::map<uint64_t, BattleTacticalPing>, 2> teamPings;
    std::map<uint64_t, uint32_t> roundPingCountByPlayer;
    // 论剑排位有效参与事实(有序集合保确定性;终局前由 BuildParticipationFact 冻结进 BattleResultEvent)
    std::set<uint64_t> enteredPlayers;
    std::set<uint64_t> manualActionPlayers;
    std::set<uint64_t> explicitAutoPlayers;
    std::set<uint64_t> hadOperableTurnPlayers;
    // PVP 延迟投降状态（按阵营隔离，连续未手动回合数用于排除纯挂机队友防绑架，零时钟依赖）
    std::optional<uint32_t> surrenderedTeam;
    std::array<BattleSurrenderVoteState, 2> teamSurrenderVotes;
    std::map<uint64_t, uint32_t> consecutiveIdleRoundsByPlayer;
    void ExpireSurrenderVotesForRoundAdvance();

    // 论剑 V3 战前 15 秒明牌变阵（确定性纯状态，无时钟依赖；定时器由 BattleRoomManager 持有）
    struct PrepPlayerOptions {
        uint32_t teamIndex = 0;
        uint32_t activeSeasonId = 0;
        bool aiControlled = false;
        uint32_t selectedSealId = 0;
        uint64_t selectedPetId = 0;
        bool ready = false;
        std::vector<SeasonSealEffect> availableSeals;
        std::vector<BattlePetSnapshot> availablePets;
    };
    bool prepActive = false;
    uint64_t nextPetActorIndex = 0;
    std::map<uint64_t, PrepPlayerOptions> prepOptionsByPlayer;
    bool InitTacticalPrep(const CreateBattleRequest& request);
    bool BuildPetActor(const BattlePlayerSnapshot& ownerSnapshot, const BattlePetSnapshot& pet,
                       uint64_t actorId, uint32_t formationSlot, BattleActorState& outActor) const;
    void PopulatePrepStateForViewer(BattlePrepState& out, uint64_t viewerPlayerId, uint64_t deadlineMs) const;

    std::mt19937_64 rng;
    uint32_t roundIndex = 1;       // 当前收集中的回合序号,从 1 开始
    uint32_t maxRounds = kDefaultMaxRounds;
    eBattleOutcome outcome = BATTLE_OUTCOME_ONGOING;
    uint64_t nextBuffInstanceId = 1;  // 局内 buff 实例 id,确定性自增(不用全局 id 生成器)
    bool initialized = false;

    // ---- 表现层附加数据(不参与判定) ----
    std::vector<uint64_t> lastActionOrder;  // 最近一回合出手序(D3)
    std::map<uint64_t, uint64_t> roundStartSpeeds;  // 本回合排速冻结值（供低速定向策略读取，不随回合内加减速漂移）
    std::map<uint64_t, uint64_t> leylineEarthShieldByActor;  // 锦标赛 V2 土厚灵脉 / 地载阵 / 土系符印首回合气血护盾余额
    uint64_t AbsorbTournamentEarthShield(uint64_t actorId, uint64_t& remaining);

    // 五行阵法 × 人宠相生 × 五行幻境爬塔 × 道友切磋坊自定义规则（开局冻结，确定性零时钟）
    struct TowerSigilAggregate {
        uint32_t damagePct = 0;
        uint32_t critPct = 0;
        uint32_t critDamagePct = 0;
        uint32_t shieldBreakBonusPct = 0;
        uint32_t healShieldPct = 0;
        uint32_t petDamagePct = 0;
        uint32_t roundHpRegenPct = 0;
        uint32_t roundManaRegenPct = 0;
        uint32_t controlResistPct = 0;
        uint32_t onHitManaRegenPct = 0;
        uint32_t damageTakenReducePct = 0;
        uint32_t first3RoundsDamagePct = 0;
        uint32_t singleTargetDamagePct = 0;
        uint32_t critBurnMaxHpPct = 0;
        uint32_t openingShieldPct = 0;
        uint32_t reflectPct = 0;
    };
    uint32_t teamAFormationId = 0;
    uint32_t teamBFormationId = 0;
    bool hasTowerContext = false;
    ::WuxingTowerBattleContext towerContext;
    TowerSigilAggregate towerSigilTotals;
    bool hasCustomDuelRules = false;
    ::CustomDuelRules customDuelRules;
    bool wuxingSynergyActiveThisRound = false;
    std::set<uint64_t> wuxingSynergyActorIds;
    uint32_t activeSeasonWeeklyAffixId = 0; // 赛季全员对称周环境词缀（开局由全体快照一致冻结，0=无/禁用）
    std::set<uint64_t> aiControlledPlayers; // 守旗阵 AI 托管守擂玩家：全程自动出手、不阻塞回合、不回写个人资产。

    bool HasAnyActiveShield(const BattleActorState& target) const;
    uint32_t WuxingHealShieldBonusPct(const BattleActorState& caster) const;
    uint32_t WuxingControlResistPct(const BattleActorState& target) const;
    bool IsControlSkill(const SkillTable& skillRow) const;
    void ApplyWuxingPostDirectHitEffects(BattleActorState& attacker, BattleActorState& target,
                                         uint64_t dealt, bool isCritical, TurnResultS2C& result);

    uint32_t currentGroupId = 0;            // 当前事件组 id,每回合结算开始归 0(D2)
    uint32_t currentHitIndex = 0;           // 当前事件组内的段序/目标序(D2)
};

}  // namespace turnbattle
