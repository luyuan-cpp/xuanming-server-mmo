#pragma once

#include <cstdint>

#include <entt/src/entt/entity/entity.hpp>

class BattlePlayerSnapshot;
class ListSkillsResponse;
class SkillTable;
class UpgradeSkillRequest;
class UpgradeSkillResponse;

// 角色技能(docs/design/class-skills.md)。PlayerSkillListComp 的唯一写入口。
//
// 两类技能共用一张列表、按 Skill.class_id 区分:
//   * 职业技能(class_id = 1..4,四职业 32 个):有等级。已学为 rank >= 1;rank 0 的条目与不在列表里同义,都是未学
//     (修习复用这一条、初始种子原地补 1 重)。存储重数可能高于本版本 max_rank(新版本写入 / 上限下调):
//     原样携带不改写,按 min(存储重数, RankLimit) 生效。
//     职业、解锁等级、等级上限、价格全由 Skill 表 + classskill:: 纯规则决定,客户端只看服务端算好的技能书。
//   * 旧通用技能(class_id = 0,ID 1..14 的占位技能):不分等级,rank 读写都忽略(存档里写着什么原样携带,
//     下发时恒为 0)。只留给还没有职业技能的职业(5..9)。
// 加载与重算不按本版本的表改写持久值(docs/design/mixed-version-persist-field-protection.md §12.1):
// 本版本解释不了的内容(查不到表的 id、超上限的重数、归属变了的技能)原样携带,只在运行期不生效 / 按上限生效。
// 角色等级一律按 playerlevel::CappedLevel(LevelComp.level()) 参与计算,存储等级不改写。
//
// 组件归属:PlayerSkillListComp 由加载(PlayerDatabaseMessageFieldsUnmarshal)挂上;职业(PlayerUint32Comp)
// 与等级(LevelComp)同样由加载挂上。本类只取不建,缺组件按服务端故障处理。
// 写操作只发生在 scene 线程(EventLoop),扣费与改等级在同一个回调里完成、同属一条 player_database 记录,
// 由常规存盘整份落库:不存在"钱扣了、等级没涨"或反过来的中间态(与属性洗点 Reset 同一口径)。
class PlayerSkillSystem {
public:
	// 新号创建入口:按当前职业补初始技能。与 InitializeOnLoad 共用同一段逻辑,幂等。
	static void RegisterPlayer(entt::entity player);
	// 技能列表里的这一项在本版本是否生效:非 0 且技能表查得到。消费 PlayerSkillListComp 的路径
	// (施放、下发客户端、战斗快照……)一律只认生效的项 —— 列表里可能留着新版本技能表才有的技能。
	static bool IsActiveSkill(uint32_t skillTableId);

	// 加载期整理,必须在职业与等级组件挂上之后、属性重算(PlayerAttributeSystem::InitializeOnLoad)之前调用:
	// 先 SanitizeSkillList,再按 Class.skill 补初始技能(职业技能 1 重;已有 rank 0 的种子条目原地补 1 重)。
	// 被动加成随后由属性重算吃进去。
	static void InitializeOnLoad(entt::entity player);

	// 清理存档里的技能列表(加载期与单测用),规则见 class-skills.md §3.2:
	//   * 只剔除 ID 为 0 与重复 ID(保留首条),各打一行 WARN;
	//   * 其余条目原样保留(含条目与组件本层的未知字段):查不到表的 ID、rank 0 的职业技能、超过本版本 max_rank
	//     的重数、写着 rank 的旧通用技能、别的职业的技能都不改写,是否生效 / 生效几重由消费侧按本版本的表判定。
	// 查不到表的数量与超上限重数的数量每次加载汇总打一行 WARN。没有要剔除的条目时组件不被替换。
	static void SanitizeSkillList(entt::entity player);

	// 是否拥有且能用:旧通用技能在列表里即可;职业技能还要生效等级 > 0(职业对、等级够;rank 0 即未学)
	static bool HasSkill(entt::entity player, uint32_t skillTableId);

	// 职业技能的生效等级 = min(存储重数, 当前角色等级的上限),后者不超过本版本 max_rank;
	// 没学(含 rank 0)/ 不是本职业 / 旧通用技能返回 0
	static uint32_t EffectiveRank(entt::entity player, uint32_t skillTableId);

	// 技能书(ListSkills / UpgradeSkill 的回包):已拥有列表 + 当前职业完整目录 + 等级 + 余额。
	// 已拥有列表是显式投影(只带 skill_table_id 与职业技能的存储重数,不带内部 id 与未知字段);
	// player_level 是 CappedLevel 之后的等级;目录 rank 是存储重数(可能高于 max_rank,此时按满重阻断)。
	// 返回 kSuccess;实体无效 kEntityIsNull;缺必需组件 kServiceUnavailable(此时 response 只有 error 以外的空值)。
	static uint32_t BuildSkillBook(entt::entity player, ListSkillsResponse& response);

	// 修习(学习 = 0 → 1;rank 0 的已有条目原地复用)。整笔拒绝时填 error_message 且不扣钱、不改等级;
	// 部分成功时 upgraded_count < count、stop_tip_id 说明停在哪。无论成败 state 都是处理之后的完整技能书。
	// 存储重数 >= 本版本 max_rank(含超上限携带的)时 PlanUpgrade 一步不走,回 kSkillMaxRankReached、不扣钱。
	static void UpgradeSkill(entt::entity player, const UpgradeSkillRequest& request, UpgradeSkillResponse& response);

	// 战斗快照:skill_table_ids 放回合制能施放的(旧通用技能 + 已学的职业主动技能),
	// skill_ranks 放全部已学职业技能(含被动)的生效等级。
	static void FillBattleSnapshot(entt::entity player, BattlePlayerSnapshot& snapshot);

	// 被动加成之和(给属性重算用;scene 是这几项被动唯一的消费者,引擎不再叠加)。
	// 比例项 0.08 = +8%;critPercentPoints 是暴击率百分点(5.0 = +5 个百分点)。
	struct PassiveBonus {
		double defense = 0.0;
		double physicalAttack = 0.0;
		double magicAttack = 0.0;
		double maxHealth = 0.0;
		double maxMana = 0.0;
		double speed = 0.0;
		double critPercentPoints = 0.0;
	};
	static PassiveBonus CollectPassiveBonus(entt::entity player);

	// 行分类(只看表,不看玩家)
	static bool IsClassSkill(const SkillTable& row);
	static bool IsPassiveSkill(const SkillTable& row);
	// 回合制能不能施放:被动(passive_stat 非空)、以及老行的被动 / 开关 / 持续施法位都不行。
	// 与引擎 TurnBattleEngine::IsTurnBattleCastableSkill 同一判据,两个进程各守一道。
	static bool IsTurnBattleCastable(const SkillTable& row);
};
