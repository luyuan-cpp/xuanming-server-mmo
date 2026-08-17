#pragma once
#include <cstdint>
#include <map>
#include <memory>
#include <random>
#include <vector>

// 回合制战斗确定性引擎(设计文档 §5.1)。
//
// 纯逻辑库:零网络/零 ECS/零 timer 依赖。输入 = 快照 + 指令流 + 随机种子,
// 输出纯函数;所有随机只走成员 RNG(std::mt19937_64(seed)),
// 相同输入必产出逐字节相同的事件流(观战/回放/断线重连依赖,宪法 §7 新增不变量 5)。
//
// 依赖的 proto 产物(proto/battle/*.proto 重生成后落到 generated/proto/battle/ 下,
// 路径按 scene 等既有域的生成规律推断):
//   battle_data.pb.h   —— BattleActorState / BattleAction / BattleEventItem 等共享结构
//   battle_node.pb.h   —— CreateBattleRequest(match → battle 的开局契约)
//   player_battle.pb.h —— TurnResultS2C / BattleStateS2C(客户端协议)
#include "proto/battle/battle_data.pb.h"
#include "proto/battle/battle_node.pb.h"
#include "proto/battle/player_battle.pb.h"

#include "constants/turn_battle_constants.h"
#include "data/battle_data_provider.h"

namespace turnbattle {

class TurnBattleEngine {
public:
    // 默认构造:生产环境,数据源为全局表管理器
    TurnBattleEngine();

    // 注入构造:单测用,喂内存数据源,隔离 Excel 依赖
    explicit TurnBattleEngine(std::shared_ptr<BattleDataProvider> dataProvider);

    // 快照 + 怪物侧 + 配置初始化;返回是否合法
    bool Initialize(const CreateBattleRequest& request);

    // 收行动;全员就绪返回 true。
    // 非法行动(死亡/已逃/技能校验链不过)不落账,按未提交处理,
    // 由回合超时的默认普攻兜底(设计文档 §5.1 超时/掉线默认行动)
    bool SubmitAction(uint64_t actorId, const BattleAction& action);

    // 未提交者填默认行动并结算本回合
    TurnResultS2C ResolveCurrentRound();

    // ONGOING / SIDE_A_WIN / SIDE_B_WIN / DRAW
    eBattleOutcome Outcome() const { return outcome; }

    // 单个玩家的结算数据(battle → scene,scene 是唯一应用者)
    BattleSettlementData BuildSettlement(uint64_t playerId) const;

    // 重连补拉的全量状态。action_deadline_ms 引擎不填(引擎无时钟),
    // 由 battle 节点按房间 timer 回填
    BattleStateS2C BuildStateSnapshot() const;

private:
    // ---- 初始化 ----

    bool InitPlayers(const CreateBattleRequest& request);
    bool InitMonsters(const CreateBattleRequest& request);
    void AppendMonsterActor(uint32_t monsterTableId, uint32_t monsterIndex, uint32_t referenceLevel);

    // ---- 行动校验链(镜像 SkillSystem::CheckSkillPrerequisites,去掉相位校验) ----

    uint32_t CheckActionPrerequisites(const BattleActorState& actor, const BattleAction& action) const;
    uint32_t ValidateSkillTarget(const BattleActorState& actor, const SkillTable& skillRow, uint64_t targetId) const;
    uint32_t CheckCooldown(const BattleActorState& actor, const SkillTable& skillRow) const;
    uint32_t CheckPlayerLevel(const BattleActorState& actor, const SkillTable& skillRow) const;
    uint32_t CheckBuff(const BattleActorState& actor, const SkillTable& skillRow) const;
    uint32_t CheckState(const BattleActorState& actor) const;

    // ---- 回合结算 ----

    void FillDefaultActions();
    std::vector<uint64_t> BuildTurnOrder() const;
    void ExecuteAction(BattleActorState& actor, const BattleAction& action, TurnResultS2C& result);
    void ExecuteAttack(BattleActorState& actor, uint64_t targetId, TurnResultS2C& result);
    void ExecuteSkill(BattleActorState& actor, const BattleAction& action, TurnResultS2C& result);
    void ExecuteDefend(BattleActorState& actor, TurnResultS2C& result);
    void ExecuteItem(BattleActorState& actor, const BattleAction& action, TurnResultS2C& result);
    void ExecuteFlee(BattleActorState& actor, TurnResultS2C& result);
    void TickBuffsAtRoundEnd(TurnResultS2C& result);
    void TickActorBuffs(BattleActorState& actor, TurnResultS2C& result);
    void ApplyBuffIntervalEffect(BattleActorState& actor, const BattleBuffEntry& entry,
                                 const BuffTable& buffRow, TurnResultS2C& result);
    void DecayCooldowns();
    void UpdateOutcome();

    // ---- 伤害/治疗/buff ----

    // 伤害公式镜像实时战斗 CalculateFinalDamage:
    // base*(1+strength*0.1) - armor,再 *(1-resistance*0.01),
    // critchance/100 概率 ×2,饱和到 0。isCritical 回传是否暴击(事件流展示用)
    double CalculateFinalDamage(const BattleActorState& caster, const BattleActorState& target,
                                double baseDamage, bool& isCritical);
    // 落伤害:DEFEND 减半,ceil 取整,饱和到 0 HP;返回实际扣血
    uint64_t ApplyDamage(BattleActorState& target, double rawDamage);
    uint64_t ApplyHeal(BattleActorState& target, double rawHeal);
    void HandleDeath(BattleActorState& target, TurnResultS2C& result);

    // effect[] → buff:镜像 BuffSystem::AddOrUpdateBuff 的
    // 免疫检查 → 驱散 → 叠层/刷新 → 新建 → 子 buff 语义,时间维度换算为回合
    void AddBuffToActor(BattleActorState& target, uint32_t buffTableId, uint64_t casterId,
                        uint32_t depth, TurnResultS2C& result);
    bool IsImmuneToBuff(const BattleActorState& target, const BuffTable& buffRow) const;
    void DispelBuffsByTag(BattleActorState& target, const BuffTable& buffRow, TurnResultS2C& result);
    bool StackOrRefreshExistingBuff(BattleActorState& target, const BuffTable& buffRow,
                                    uint64_t casterId, TurnResultS2C& result);
    void RemoveBuffAt(BattleActorState& target, int buffIndex, TurnResultS2C& result);

    // ---- 查询辅助 ----

    BattleActorState* FindActor(uint64_t actorId);
    const BattleActorState* FindActor(uint64_t actorId) const;
    // 按 buff 实例 id 找下标,找不到返回 -1(tick 过程中条目会增删,须按 id 重查)
    int FindBuffIndex(const BattleActorState& actor, uint64_t buffId) const;
    bool IsActorActive(const BattleActorState& actor) const;  // 存活且未逃跑
    bool ActorHasBuffOfType(const BattleActorState& actor, uint32_t buffType) const;
    uint64_t MaxAliveEnemySpeed(const BattleActorState& actor) const;
    std::vector<uint64_t> CollectAliveEnemyIds(const BattleActorState& actor) const;
    bool AllPlayersReady() const;
    bool SideWiped(uint32_t teamIndex) const;
    uint32_t CompletedRounds() const;
    BattleEventItem* AppendEvent(TurnResultS2C& result, eBattleEventType eventType,
                                 uint64_t sourceId, uint64_t targetId);

    // ---- 确定性随机(禁 tlsRandom / rand(),宪法 §7 新增不变量 5) ----

    // [0, count) 均匀整数(count 必须 > 0)
    uint64_t RandIndex(uint64_t count);
    // [0, 1) 实数。不走 std::uniform_real_distribution(其实现跨平台不定),
    // 直接取 mt19937_64 的高 53 位,保证任何平台上同种子同序列
    double Rand01();

private:
    std::shared_ptr<BattleDataProvider> dataProvider;

    // 引擎状态全部用 proto 消息承载(宪法 §3:不手写与 proto 重复的并行 struct)
    CreateBattleRequest createRequest;                    // 开局请求副本(种子/模式/配置)
    std::vector<BattleActorState> actors;                 // 全部战斗单位,插入序稳定
    std::map<uint64_t, BattleAction> pendingActions;      // actor_id → 本回合行动(有序容器保确定性)
    std::map<uint64_t, BattleSettlementData> settlements; // player_id → 结算累积(道具消耗账本等)

    std::mt19937_64 rng;
    uint32_t roundIndex = 1;       // 当前收集中的回合序号,从 1 开始
    uint32_t maxRounds = kDefaultMaxRounds;
    eBattleOutcome outcome = BATTLE_OUTCOME_ONGOING;
    uint64_t nextBuffInstanceId = 1;  // 局内 buff 实例 id,确定性自增(不用全局 id 生成器)
    bool initialized = false;
};

}  // namespace turnbattle
