#include "player_skill.h"

#include <algorithm>
#include <unordered_set>
#include <vector>

#include "muduo/base/Logging.h"
#include "engine/core/type_define/player_guid_comp.h"
#include "battle/system/player_battle.h"
#include "modules/currency/constants/currency.h"
#include "modules/currency/system/currency_system.h"
#include "player/comp/player_frozen_comp.h"
#include "player/comp/player_ownership_comp.h"  // PlayerTravelHandoffComp
#include "player/system/player_attribute.h"
#include "player/system/player_level_rules.h"
#include "services/battle/constants/turn_battle_constants.h"
#include "services/battle/system/class_skill_rules.h"
#include "table/code/buff_table.h"
#include "table/code/class_table.h"
#include "table/code/skill_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/skill_error_tip.pb.h"
#include "proto/battle/battle_data.pb.h"
#include "proto/common/component/actor_comp.pb.h"
#include "proto/common/component/player_comp.pb.h"
#include "proto/common/component/player_skill_comp.pb.h"
#include "proto/scene/player_skill.pb.h"
#include <thread_context/ecs_context.h>

namespace
{
	uint64_t GuidForLog(entt::entity player)
	{
		const auto *guid = tlsEcs.actorRegistry.try_get<PlayerGuidComp>(player);
		return guid != nullptr ? guid->value : 0;
	}

	uint32_t PlayerClassId(entt::entity player)
	{
		const auto *identity = tlsEcs.actorRegistry.try_get<PlayerUint32Comp>(player);
		return identity != nullptr ? identity->class_() : 0;
	}

	// 玩法用的角色等级 = CappedLevel(存储等级):新号加载时 LevelComp 还是 0(InitPlayerFromAllData 随后才写 1)
	// 按 1 级算;超过本版本 kMaxLevel 的存储等级(新版本写入 / 上限下调)按上限算,存储值原样携带不改写。
	// 缺组件按 1 级算(既有行为)。
	uint32_t PlayerLevel(entt::entity player)
	{
		const auto *level = tlsEcs.actorRegistry.try_get<LevelComp>(player);
		return level != nullptr ? playerlevel::CappedLevel(level->level()) : 1;
	}

	const SkillTable *FindSkillRow(uint32_t skillTableId)
	{
		if (skillTableId == 0)
		{
			return nullptr;
		}
		return SkillTableManager::Instance().FindByIdSilent(skillTableId).first;
	}

	// 这个职业有没有职业技能。有的职业(1..4)不再使用旧通用占位技能。
	bool ClassHasClassSkills(uint32_t classId)
	{
		if (classId == 0)
		{
			return false;
		}
		for (const auto &row : SkillTableManager::Instance().FindAll().data())
		{
			if (row.class_id() == classId)
			{
				return true;
			}
		}
		return false;
	}

	// 条目对当前职业是否生效(class-skills.md §3.2)。不生效的条目留在存档里、但所有读路径都当它不存在:
	// 别的职业的职业技能(转职后)与职业技能上线前发下的旧占位技能都属于这一类 —— 不删,是为了不静默丢掉
	// 玩家已经花过钱的等级,以后做转职 / 返还时还查得到。
	bool IsActiveFor(const SkillTable &row, uint32_t classId, bool classHasClassSkills)
	{
		if (PlayerSkillSystem::IsClassSkill(row))
		{
			return row.class_id() == classId;
		}
		return !classHasClassSkills;
	}

	PlayerSkillComp *FindEntry(PlayerSkillListComp &list, uint32_t skillTableId)
	{
		for (auto &skill : *list.mutable_skill_list())
		{
			if (skill.skill_table_id() == skillTableId)
			{
				return &skill;
			}
		}
		return nullptr;
	}

	const PlayerSkillComp *FindEntry(const PlayerSkillListComp &list, uint32_t skillTableId)
	{
		for (const auto &skill : list.skill_list())
		{
			if (skill.skill_table_id() == skillTableId)
			{
				return &skill;
			}
		}
		return nullptr;
	}

	uint32_t StoredRank(const PlayerSkillListComp &list, uint32_t skillTableId)
	{
		const auto *entry = FindEntry(list, skillTableId);
		return entry != nullptr ? entry->rank() : 0;
	}

	// 生效等级 = min(存储重数, RankLimit(角色等级)),RankLimit 不超过本版本 max_rank:存储重数高于上限
	// (新版本写入 / 上限下调)时原样携带、按上限生效;rank 0 与未学同义,生效为 0
	uint32_t EffectiveRankOf(const SkillTable &row, uint32_t storedRank, uint32_t playerLevel)
	{
		return classskill::EffectiveRank(storedRank, playerLevel, row.unlock_level(), row.max_rank());
	}

	// 一条已拥有的条目现在能不能用:生效 + (旧通用技能,或职业技能生效等级 > 0)
	bool IsUsable(const SkillTable &row, const PlayerSkillComp &entry, uint32_t classId,
				  bool classHasClassSkills, uint32_t playerLevel)
	{
		if (!IsActiveFor(row, classId, classHasClassSkills))
		{
			return false;
		}
		return !PlayerSkillSystem::IsClassSkill(row) || EffectiveRankOf(row, entry.rank(), playerLevel) > 0;
	}

	// 战斗 / 结算在途、跨 zone 冻结、交接已发起:技能等级是持久化数据,这几个窗口里不写
	// (与属性加点 CheckWritable 同口径,外加交接窗口:交接标记一挂,存盘就会跳过这名玩家)
	uint32_t CheckSkillWritable(entt::entity player)
	{
		if (PlayerBattleSystem::IsInBattle(player))
		{
			return kSkillUpgradeBusy;
		}
		if (tlsEcs.actorRegistry.any_of<PlayerFrozenComp, PlayerTravelHandoffComp>(player))
		{
			return kSkillUpgradeBusy;
		}
		return 0;
	}

	uint32_t StopTipOf(classskill::UpgradeStop stop)
	{
		switch (stop)
		{
		case classskill::UpgradeStop::kMaxRank:
			return kSkillMaxRankReached;
		case classskill::UpgradeStop::kLevelLimit:
			return kSkillLevelNotEnough;
		case classskill::UpgradeStop::kGold:
			return kSkillGoldNotEnough;
		case classskill::UpgradeStop::kNone:
		case classskill::UpgradeStop::kCount:
			break;
		}
		return 0;
	}

	// 附带增减益的持续回合(技能书展示用):effect[] 第一个 buff 行的时长换成回合
	uint32_t DurationRoundsOf(const SkillTable &row)
	{
		for (const auto buffId : row.effect())
		{
			const auto *buffRow = BuffTableManager::Instance().FindByIdSilent(buffId).first;
			if (buffRow == nullptr || buffRow->infinite_duration() != 0 || buffRow->duration() <= 0)
			{
				continue;
			}
			return turnbattle::RoundsFromSeconds(buffRow->duration());
		}
		return 0;
	}

	// 按 Class.skill 补初始技能,返回补了几条。职业技能补 1 重(仍须到解锁等级),旧通用技能不分等级。
	// rank 0 的职业技能条目与未学同义:种子已有这样一条时原地补到 1 重(复用条目,条目上的未知字段与位置不动),
	// 不另加一条。已学(rank >= 1)的职业技能与已在列表里的旧通用技能都不碰。幂等。
	uint32_t GrantInitialSkills(entt::entity player, PlayerSkillListComp &list)
	{
		const auto classId = PlayerClassId(player);
		const auto *classRow = ClassTableManager::Instance().FindByIdSilent(classId).first;
		if (classRow == nullptr)
		{
			// 职业 0 / 表里没有的职业:没有种子可补(旧实现会遍历所有职业的种子,正是要修掉的行为)
			return 0;
		}
		const auto level = PlayerLevel(player);
		uint32_t granted = 0;
		for (const auto skillTableId : classRow->skill())
		{
			const auto *row = FindSkillRow(skillTableId);
			if (row == nullptr)
			{
				if (skillTableId != 0)
				{
					LOG_WARN << "[PlayerSkill] Class seed skill missing from Skill table: class_id=" << classId
							 << " skill_table_id=" << skillTableId;
				}
				continue;
			}
			const bool classSkill = PlayerSkillSystem::IsClassSkill(*row);
			auto *existing = FindEntry(list, skillTableId);
			if (existing != nullptr && (!classSkill || existing->rank() > 0))
			{
				continue;
			}
			if (classSkill && (row->class_id() != classId || EffectiveRankOf(*row, 1, level) == 0))
			{
				// 种子配成别的职业的技能 / 初始技能解锁等级配高了:是表错,不发(已有的 rank 0 条目也原样不动)
				LOG_WARN << "[PlayerSkill] Class seed skill cannot be learned: class_id=" << classId
						 << " skill_table_id=" << skillTableId << " skill_class=" << row->class_id()
						 << " unlock_level=" << row->unlock_level() << " level=" << level;
				continue;
			}
			auto *entry = existing;
			if (entry == nullptr)
			{
				entry = list.add_skill_list();
				entry->set_skill_table_id(skillTableId);
			}
			if (classSkill)
			{
				entry->set_rank(1);
			}
			++granted;
			LOG_INFO << "[PlayerSkill] Initial skill granted: player_id=" << GuidForLog(player)
					 << " class_id=" << classId << " skill_table_id=" << skillTableId << " rank=" << entry->rank()
					 << " reused_entry=" << (existing != nullptr);
		}
		return granted;
	}
} // namespace

bool PlayerSkillSystem::IsClassSkill(const SkillTable &row)
{
	return row.class_id() != 0;
}

bool PlayerSkillSystem::IsPassiveSkill(const SkillTable &row)
{
	return !row.passive_stat().empty();
}

bool PlayerSkillSystem::IsTurnBattleCastable(const SkillTable &row)
{
	// 被动技能没有施放动作、开关技能是状态切换、持续施法依赖引擎已删除的相位概念;
	// 三者若进了快照,引擎会把它们当普通技能按 damage 表达式打出伤害。
	// 职业被动只能靠 passive_stat 认:被动位号是 0,导表器会把重复槽位里的 0 当空格丢掉,skill_type 里写不出来。
	if (IsPassiveSkill(row))
	{
		return false;
	}
	// 表里存的是位号(0..5)不是掩码,别和 scene 侧 eSkillType 的 1<<n 混用
	for (const auto skillTypeBit : row.skill_type())
	{
		if (skillTypeBit == turnbattle::kSkillTypeBitPassive ||
			skillTypeBit == turnbattle::kSkillTypeBitToggle ||
			skillTypeBit == turnbattle::kSkillTypeBitChannel)
		{
			return false;
		}
	}
	return true;
}

bool PlayerSkillSystem::IsActiveSkill(const uint32_t skillTableId)
{
	return skillTableId != 0 && SkillTableManager::Instance().Exists(skillTableId);
}

void PlayerSkillSystem::RegisterPlayer(entt::entity player)
{
	auto *list = tlsEcs.actorRegistry.try_get<PlayerSkillListComp>(player);
	if (list == nullptr)
	{
		LOG_ERROR << "[PlayerSkill] RegisterPlayer: PlayerSkillListComp missing (load path incomplete?), player_id="
				  << GuidForLog(player);
		return;
	}
	// 加载时 InitializeOnLoad 已经补过一次,这里通常什么都不加;真加了(种子表在两步之间热更)就补算被动
	if (GrantInitialSkills(player, *list) > 0)
	{
		PlayerAttributeSystem::Recalculate(player, PlayerAttributeSystem::RecalcReason::kSkillChanged);
	}
}

void PlayerSkillSystem::InitializeOnLoad(entt::entity player)
{
	auto *list = tlsEcs.actorRegistry.try_get<PlayerSkillListComp>(player);
	if (list == nullptr)
	{
		LOG_ERROR << "[PlayerSkill] InitializeOnLoad: PlayerSkillListComp missing, player_id=" << GuidForLog(player);
		return;
	}
	SanitizeSkillList(player);
	GrantInitialSkills(player, *list);
}

bool PlayerSkillSystem::HasSkill(entt::entity player, const uint32_t skillTableId)
{
	if (skillTableId == 0 || !tlsEcs.actorRegistry.valid(player))
	{
		return false;
	}
	const auto *list = tlsEcs.actorRegistry.try_get<PlayerSkillListComp>(player);
	const auto *row = FindSkillRow(skillTableId);
	if (list == nullptr || row == nullptr)
	{
		return false;
	}
	const auto *entry = FindEntry(*list, skillTableId);
	if (entry == nullptr)
	{
		return false;
	}
	const auto classId = PlayerClassId(player);
	return IsUsable(*row, *entry, classId, ClassHasClassSkills(classId), PlayerLevel(player));
}

uint32_t PlayerSkillSystem::EffectiveRank(entt::entity player, const uint32_t skillTableId)
{
	if (!tlsEcs.actorRegistry.valid(player))
	{
		return 0;
	}
	const auto *list = tlsEcs.actorRegistry.try_get<PlayerSkillListComp>(player);
	const auto *row = FindSkillRow(skillTableId);
	if (list == nullptr || row == nullptr || !IsClassSkill(*row) || row->class_id() != PlayerClassId(player))
	{
		return 0;
	}
	return EffectiveRankOf(*row, StoredRank(*list, skillTableId), PlayerLevel(player));
}

void PlayerSkillSystem::SanitizeSkillList(entt::entity player)
{
	auto *skillList = tlsEcs.actorRegistry.try_get<PlayerSkillListComp>(player);
	if (skillList == nullptr)
	{
		return;
	}

	// 只剔除与版本无关的坏记录(ID 为 0、重复 ID 保留首条);其余条目一律原样携带,加载期不按本版本的表改写
	// (docs/design/mixed-version-persist-field-protection.md §12.1 D5 / D6):以前这里会把职业技能重数压回 max_rank、
	// 删掉 rank 0 的职业技能、把通用技能的 rank 清 0,随下一次存盘落库;新版本调大上限或改了归属后回滚 scene,
	// 玩家修过的重数就再也回不来。现在这些值只在运行期按本版本的表解释:
	//   * 生效等级 = min(存储重数, RankLimit),上限之上的部分不生效(EffectiveRankOf);
	//   * rank 0 的职业技能与未学同义(修习复用这一条,初始种子原地补 1 重,见 GrantInitialSkills);
	//   * 旧通用技能读写都忽略 rank。
	PlayerSkillListComp sanitizedSkillList;
	std::unordered_set<uint32_t> seenSkillIds;
	bool changed = false;
	uint32_t unknownSkills = 0;
	uint32_t ranksAboveMax = 0;

	for (const auto &skill : skillList->skill_list())
	{
		const auto skillId = skill.skill_table_id();
		if (skillId == 0)
		{
			LOG_WARN << "Removing player skill with skill_table_id=0 entity=" << entt::to_integral(player);
			changed = true;
			continue;
		}

		if (!seenSkillIds.emplace(skillId).second)
		{
			LOG_WARN << "Removing duplicate player skill_table_id=" << skillId
					 << " entity=" << entt::to_integral(player);
			changed = true;
			continue;
		}

		sanitizedSkillList.add_skill_list()->CopyFrom(skill);

		// 本版本技能表查不到的技能**保留但不生效**(docs/design/mixed-version-persist-field-protection.md §12,第 5 批):
		// 新版本加了技能后回滚 scene,旧表里没有这一行;以前这里直接删掉,旧版本一存盘玩家就永久失去这个技能。
		// 生效与否由消费侧各自按表判定:HasSkill、BuildSkillBook、战斗快照、被动加成都用 FindSkillRow 查不到即跳过,
		// 与 IsActiveSkill 同一口径。
		const auto *row = FindSkillRow(skillId);
		if (row == nullptr)
		{
			++unknownSkills;
			continue;
		}
		// 存储重数高于本版本 max_rank:可能是新版本调大上限后写入的,也可能是本版本把上限调低了。
		// 两种都原样携带、按上限生效,只计数告警
		if (IsClassSkill(*row) && skill.rank() > row->max_rank())
		{
			++ranksAboveMax;
		}
	}

	if (unknownSkills > 0 || ranksAboveMax > 0)
	{
		// 每次加载只打一行汇总:混版本窗口里这是正常现象;表被裁掉技能 / 下调上限时同一行也足够定位。
		LOG_WARN << "Carrying player skills this version's SkillTable cannot fully apply: unknown_skill_ids="
				 << unknownSkills << " (inactive) ranks_above_max_rank=" << ranksAboveMax
				 << " (effective capped at max_rank) entity=" << entt::to_integral(player);
	}

	if (changed)
	{
		// 整体替换会丢掉组件本层的未知字段(新版本给 PlayerSkillListComp 加的),先并过来再换
		// (docs/design/mixed-version-persist-field-protection.md §4.2 L5)。逐条技能经 CopyFrom 已各自带着。
		sanitizedSkillList.mutable_unknown_fields()->MergeFrom(skillList->unknown_fields());
		skillList->Swap(&sanitizedSkillList);
	}
}

uint32_t PlayerSkillSystem::BuildSkillBook(entt::entity player, ListSkillsResponse &response)
{
	response.Clear();
	if (!tlsEcs.actorRegistry.valid(player))
	{
		return kEntityIsNull;
	}
	if (!tlsEcs.actorRegistry.all_of<PlayerSkillListComp, PlayerUint32Comp, LevelComp>(player))
	{
		LOG_ERROR << "[PlayerSkill] Skill book input missing: player_id=" << GuidForLog(player)
				  << " has_skills=" << tlsEcs.actorRegistry.all_of<PlayerSkillListComp>(player)
				  << " has_class=" << tlsEcs.actorRegistry.all_of<PlayerUint32Comp>(player)
				  << " has_level=" << tlsEcs.actorRegistry.all_of<LevelComp>(player);
		return kServiceUnavailable;
	}

	const auto &list = tlsEcs.actorRegistry.get<PlayerSkillListComp>(player);
	const auto classId = PlayerClassId(player);
	const auto level = PlayerLevel(player);
	const bool classHasClassSkills = ClassHasClassSkills(classId);
	const uint64_t balance = CurrencySystem::GetBalance(player, kCurrencyGold);

	// skill_list 只放现在能用的条目:robot / 旧客户端拿它当"可施放技能",不生效的条目放进去会被拿去施放然后被拒。
	// 字段本身必须在场(哪怕是空表):robot 把缺字段读成"技能被清空"。
	// 显式投影、不 CopyFrom 存档条目:只带 skill_table_id,职业技能再带存储重数(与 catalog.rank 同口径)。
	// 存档条目的内部 id 与本版本解释不了的未知字段(新版本加的)不下发;旧通用技能的 rank 读写都忽略,回包里恒为 0。
	auto *owned = response.mutable_skill_list();
	for (const auto &entry : list.skill_list())
	{
		const auto *row = FindSkillRow(entry.skill_table_id());
		if (row == nullptr || !IsUsable(*row, entry, classId, classHasClassSkills, level))
		{
			continue;
		}
		auto *ownedEntry = owned->add_skill_list();
		ownedEntry->set_skill_table_id(entry.skill_table_id());
		if (IsClassSkill(*row))
		{
			ownedEntry->set_rank(entry.rank());
		}
	}
	response.set_class_id(classId);
	response.set_player_level(level);
	response.set_currency_balance(balance);

	std::vector<const SkillTable *> rows;
	for (const auto &row : SkillTableManager::Instance().FindAll().data())
	{
		if (IsClassSkill(row) && row.class_id() == classId)
		{
			rows.push_back(&row);
		}
	}
	std::sort(rows.begin(), rows.end(),
			  [](const SkillTable *lhs, const SkillTable *rhs) { return lhs->id() < rhs->id(); });

	const uint32_t busyTip = CheckSkillWritable(player);
	for (const auto *row : rows)
	{
		// rank 是存储重数,也是 UpgradeSkill 比较 expected_rank 的口径。它可能高于本版本 max_rank(新版本写入 /
		// 上限下调):此时 next_rank_cost = 0、阻断原因为满重,与 UpgradeSkill 经 PlanUpgrade 停在满重一致
		const auto rank = StoredRank(list, row->id());
		const auto rankLimit = classskill::RankLimit(level, row->unlock_level(), row->max_rank());
		const uint64_t nextCost = rank < row->max_rank() ? classskill::UpgradeCost(rank + 1, row->unlock_level()) : 0;

		auto *view = response.add_catalog();
		view->set_skill_table_id(row->id());
		view->set_name(row->name());
		view->set_description(row->description());
		view->set_icon_key(row->icon_key());
		view->set_class_id(row->class_id());
		view->set_category(row->category());
		view->set_passive(IsPassiveSkill(*row));
		view->set_target_team(row->target_team());
		view->set_max_targets(row->max_targets());
		view->set_unlock_level(row->unlock_level());
		view->set_max_rank(row->max_rank());
		view->set_rank(rank);
		view->set_rank_limit(rankLimit);
		view->set_mana_percent(row->mana_percent());
		view->set_cooldown_rounds(row->cooldown_rounds());
		view->set_next_rank_cost(nextCost);
		view->set_duration_rounds(DurationRoundsOf(*row));

		// 不能修习的原因,优先级与 UpgradeSkill 的判定顺序一致
		uint32_t blockTip = 0;
		if (busyTip != 0)
		{
			blockTip = busyTip;
		}
		else if (rank >= row->max_rank())
		{
			blockTip = kSkillMaxRankReached;
		}
		else if (rank >= rankLimit)
		{
			blockTip = kSkillLevelNotEnough;
		}
		else if (nextCost > balance)
		{
			blockTip = kSkillGoldNotEnough;
		}
		view->set_upgrade_block_tip_id(blockTip);
	}
	return kSuccess;
}

void PlayerSkillSystem::UpgradeSkill(entt::entity player, const UpgradeSkillRequest &request,
									 UpgradeSkillResponse &response)
{
	response.Clear();
	// 每个出口都回一份处理之后的完整技能书:客户端整体替换,不在本地推算等级与余额
	const auto finish = [&](uint32_t errorTip) {
		if (errorTip != 0)
		{
			response.mutable_error_message()->set_id(errorTip);
		}
		if (const auto bookErr = BuildSkillBook(player, *response.mutable_state()); bookErr != kSuccess)
		{
			response.clear_state();
			if (errorTip == 0)
			{
				response.mutable_error_message()->set_id(bookErr);
			}
		}
	};

	if (!tlsEcs.actorRegistry.valid(player))
	{
		response.mutable_error_message()->set_id(kEntityIsNull);
		return;
	}
	if (!tlsEcs.actorRegistry.all_of<PlayerSkillListComp, PlayerUint32Comp, LevelComp>(player))
	{
		finish(kServiceUnavailable);  // BuildSkillBook 会记 ERROR
		return;
	}
	// 纯参数校验先于状态校验:非法次数永远不可能成功,不该被报成"稍后再试"
	if (!classskill::IsValidUpgradeCount(request.count()))
	{
		finish(kSkillUpgradeCountInvalid);
		return;
	}
	const auto *row = FindSkillRow(request.skill_table_id());
	if (row == nullptr)
	{
		finish(kInvalidTableId);
		return;
	}
	const auto classId = PlayerClassId(player);
	if (!IsClassSkill(*row) || row->class_id() != classId)
	{
		finish(kSkillNotInClass);
		return;
	}
	if (const auto busyTip = CheckSkillWritable(player); busyTip != 0)
	{
		finish(busyTip);
		return;
	}

	auto &list = tlsEcs.actorRegistry.get<PlayerSkillListComp>(player);
	const auto storedRank = StoredRank(list, row->id());
	// 乐观比较:客户端按它看到的等级提交。对不上说明这是旧请求的重放或别处已经改过,整笔拒绝、不扣钱
	if (storedRank != request.expected_rank())
	{
		finish(kSkillRankChanged);
		return;
	}

	// 存储重数可能高于本版本 max_rank(新版本写入 / 上限下调,加载期原样携带)。这里不另加守卫:
	// PlanUpgrade 每一步先判"已到 max_rank",存储重数 >= max_rank 时第一步就停在 kMaxRank,
	// steps = 0、totalCost = 0,走下面的整笔拒绝(不扣钱、不改等级,也不会算 rank + 1 回绕)。
	const auto level = PlayerLevel(player);
	const auto rankLimit = classskill::RankLimit(level, row->unlock_level(), row->max_rank());
	const auto plan = classskill::PlanUpgrade(storedRank, row->max_rank(), rankLimit, row->unlock_level(),
											  CurrencySystem::GetBalance(player, kCurrencyGold), request.count());
	if (plan.steps == 0)
	{
		finish(StopTipOf(plan.stop));
		return;
	}

	// 扣费与改等级在同一个 scene 回调里先后完成:扣费失败(冻结 / 封禁 / 余额被并发花掉)什么都不改;
	// 扣费成功之后改等级不会失败。两者同属一条 player_database 记录,由常规存盘一起落库。
	if (const auto err = CurrencySystem::DeductCurrency(player, kCurrencyGold, static_cast<int64_t>(plan.totalCost),
														 TX_CURRENCY_DEDUCT, row->id());
		err != kSuccess)
	{
		finish(err);
		return;
	}
	// 已有条目(含 rank 0 的未学条目)原地改等级,保留其未知字段;没有才新增,列表里不会出现重复 id
	auto *entry = FindEntry(list, row->id());
	if (entry == nullptr)
	{
		entry = list.add_skill_list();
		entry->set_skill_table_id(row->id());
	}
	entry->set_rank(plan.finalRank);

	response.set_upgraded_count(plan.steps);
	if (plan.steps < request.count())
	{
		response.set_stop_tip_id(StopTipOf(plan.stop));
	}
	LOG_INFO << "[PlayerSkill] Skill upgraded: player_id=" << GuidForLog(player) << " skill_table_id=" << row->id()
			 << " rank=" << storedRank << " -> " << plan.finalRank << " cost_gold=" << plan.totalCost
			 << " requested=" << request.count();

	// 被动吃进派生属性:立刻重算并推属性面板。上限变化按比例保持当前 HP/MP(kSkillChanged 与换装同口径),
	// 反复修习 / 重登都不会凭空回血回蓝
	if (IsPassiveSkill(*row))
	{
		PlayerAttributeSystem::Recalculate(player, PlayerAttributeSystem::RecalcReason::kSkillChanged);
		PlayerAttributeSystem::PushPanel(player);
	}
	finish(0);
}

void PlayerSkillSystem::FillBattleSnapshot(entt::entity player, BattlePlayerSnapshot &snapshot)
{
	// 这里是第一道过滤,引擎 InitPlayers 还有一道(快照来自本进程也不能单点信任)。
	const auto *list = tlsEcs.actorRegistry.try_get<PlayerSkillListComp>(player);
	if (list == nullptr)
	{
		return;
	}
	const auto classId = PlayerClassId(player);
	const auto level = PlayerLevel(player);
	const bool classHasClassSkills = ClassHasClassSkills(classId);
	for (const auto &skill : list->skill_list())
	{
		const auto *row = FindSkillRow(skill.skill_table_id());
		if (row == nullptr || !IsUsable(*row, skill, classId, classHasClassSkills, level))
		{
			continue;
		}
		if (IsClassSkill(*row))
		{
			(*snapshot.mutable_skill_ranks())[row->id()] = EffectiveRankOf(*row, skill.rank(), level);
		}
		if (IsTurnBattleCastable(*row))
		{
			snapshot.add_skill_table_ids(row->id());
		}
	}
}

PlayerSkillSystem::PassiveBonus PlayerSkillSystem::CollectPassiveBonus(entt::entity player)
{
	PassiveBonus bonus;
	const auto *list = tlsEcs.actorRegistry.try_get<PlayerSkillListComp>(player);
	if (list == nullptr)
	{
		return bonus;
	}
	const auto classId = PlayerClassId(player);
	const auto level = PlayerLevel(player);
	for (const auto &skill : list->skill_list())
	{
		const auto *row = FindSkillRow(skill.skill_table_id());
		if (row == nullptr || !IsClassSkill(*row) || !IsPassiveSkill(*row) || row->class_id() != classId)
		{
			continue;
		}
		const auto rank = EffectiveRankOf(*row, skill.rank(), level);
		if (rank == 0)
		{
			continue;
		}
		const double value = row->passive_percent() * classskill::PassiveRankFactor(rank);
		switch (classskill::ParsePassiveStat(row->passive_stat()))
		{
		case classskill::PassiveStat::kDefense:
			bonus.defense += value;
			break;
		case classskill::PassiveStat::kPhysicalAttack:
			bonus.physicalAttack += value;
			break;
		case classskill::PassiveStat::kMagicAttack:
			bonus.magicAttack += value;
			break;
		case classskill::PassiveStat::kMaxHealth:
			bonus.maxHealth += value;
			break;
		case classskill::PassiveStat::kMaxMana:
			bonus.maxMana += value;
			break;
		case classskill::PassiveStat::kSpeed:
			bonus.speed += value;
			break;
		case classskill::PassiveStat::kCrit:
			bonus.critPercentPoints += value * 100.0;
			break;
		case classskill::PassiveStat::kOutgoingHeal:
			// 战斗专用:唯一消费者是回合引擎(按快照 skill_ranks 现算),这里不进派生属性
			break;
		case classskill::PassiveStat::kNone:
		case classskill::PassiveStat::kCount:
			LOG_WARN << "[PlayerSkill] Unknown passive_stat ignored: skill_table_id=" << row->id()
					 << " passive_stat=" << row->passive_stat();
			break;
		}
	}
	return bonus;
}
