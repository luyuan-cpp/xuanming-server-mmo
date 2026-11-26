#pragma once

// 角色平滑升级曲线与全玩法经验/银两周软上限纯规则（零 ECS / 零表管理器 I/O，可独立单测）。
// 设计文档：docs/planning/player-leveling-and-battle-economy-v1.md。

#include <algorithm>
#include <cstdint>
#include <limits>
#include <vector>

#include "player_level_rules.h"
#include "proto/battle/battle_data.pb.h"
#include "proto/common/component/client_request_ledger_comp.pb.h"
#include "proto/match/match_service.pb.h"

namespace battleprogression {

inline constexpr uint32_t kWuxingTowerLookupMode = 99u;
inline constexpr uint32_t kOutcomeTierWin = 1u;
inline constexpr uint32_t kOutcomeTierQualifiedNonWin = 2u;
inline constexpr uint32_t kPvpMinRoundsForLoss = 3u;
inline constexpr uint32_t kPvpMinManualRoundsForLoss = 2u;
inline constexpr uint32_t kPermyriadBase = 10000u;

struct LevelExpRow {
	uint32_t level{0};
	uint64_t requiredTotalExp{0};
};

struct ModeRewardRow {
	uint32_t matchMode{0};
	uint32_t difficulty{0};
	uint32_t outcomeTier{0};
	uint64_t expGain{0};
	uint64_t silverGain{0};
};

struct WeeklySilverRule {
	uint64_t softCap{30000};
	uint32_t diminishedRatePermyriad{2000};
	uint64_t hardCap{45000};
};

struct LevelExpDisplay {
	uint64_t totalExp{0};
	uint64_t currentLevelExp{0};
	uint64_t nextLevelExp{0};
	uint64_t nextLevelTotalExp{0};
	bool isMaxLevel{false};
};

struct LevelProgressResult {
	uint32_t storedLevelBefore{1};
	uint32_t storedLevelAfter{1};
	uint32_t levelBefore{1};
	uint32_t levelAfter{1};
	uint64_t totalExpAfter{0};
	uint64_t nextLevelTotalExp{0};
	bool leveledUp{false};
};

struct WeeklySilverEval {
	uint64_t creditedSilver{0};
	uint64_t usedAfter{0};
	bool diminished{false};
};

struct BattleProgressionDelta {
	uint64_t expGain{0};
	uint64_t silverGain{0};
	uint32_t levelBefore{0};
	uint32_t levelAfter{0};
	uint64_t totalExpAfter{0};
	uint64_t nextLevelTotalExp{0};
	uint64_t weeklySilverUsedAfter{0};
	uint64_t weeklySilverSoftCap{0};
	bool silverDiminished{false};
	bool leveledUp{false};
	bool ledgerChanged{false};
};

// 校验 1..kMaxLevel 经验表完整性：连续无空洞、1 级恒为 0、后续严格单调递增。
inline bool ValidateLevelExpRows(const std::vector<LevelExpRow>& rows) {
	if (rows.size() != playerlevel::kMaxLevel) {
		return false;
	}
	uint64_t prevExp = 0;
	for (uint32_t expected = 1; expected <= playerlevel::kMaxLevel; ++expected) {
		const auto& row = rows[expected - 1];
		if (row.level != expected) {
			return false;
		}
		if (expected == 1) {
			if (row.requiredTotalExp != 0) {
				return false;
			}
		} else {
			if (row.requiredTotalExp <= prevExp) {
				return false;
			}
		}
		prevExp = row.requiredTotalExp;
	}
	return true;
}

inline bool ValidateWeeklySilverRule(const WeeklySilverRule& rule) {
	return rule.diminishedRatePermyriad <= kPermyriadBase &&
		   rule.hardCap >= rule.softCap;
}

// 计算面板 / 战果展示用的本级经验进度（绝不因 GM 调级后 totalExp < req(level) 发生无符号下溢）。
inline LevelExpDisplay ComputeLevelExpDisplay(uint32_t effectiveLevel, uint64_t totalExp,
											  const std::vector<LevelExpRow>& sortedRows) {
	LevelExpDisplay out;
	out.totalExp = totalExp;
	const uint32_t lvl = playerlevel::CappedLevel(effectiveLevel);
	if (lvl >= playerlevel::kMaxLevel) {
		out.isMaxLevel = true;
		return out;
	}
	if (!ValidateLevelExpRows(sortedRows)) {
		return out;
	}
	const uint64_t curReq = sortedRows[lvl - 1].requiredTotalExp;
	const uint64_t nextReq = sortedRows[lvl].requiredTotalExp;
	out.nextLevelTotalExp = nextReq;
	out.nextLevelExp = nextReq > curReq ? (nextReq - curReq) : 0;
	if (totalExp >= curReq && out.nextLevelExp > 0) {
		out.currentLevelExp = std::min<uint64_t>(totalExp - curReq, out.nextLevelExp);
	}
	return out;
}

// 按累计总经验推进存储等级：
//   - total_exp 溢出返回 false（整笔结算延后）；
//   - storedLevel >= kMaxLevel（含混版本超上限携带档）只加经验、绝不降级；
//   - 始终满足 newStoredLevel >= max(1, storedLevel)（GM 调高级号绝不回退）；
//   - sortedRows 为空（单测未加载表）时仅累计经验不改级，保持历史单测兼容。
inline bool ResolveLevelForExp(uint32_t storedLevel, uint64_t currentTotalExp, uint64_t expGain,
							   const std::vector<LevelExpRow>& sortedRows, LevelProgressResult& out) {
	out = LevelProgressResult{};
	if (expGain > std::numeric_limits<uint64_t>::max() - currentTotalExp) {
		return false;
	}
	out.storedLevelBefore = storedLevel;
	out.levelBefore = playerlevel::CappedLevel(storedLevel);
	out.totalExpAfter = currentTotalExp + expGain;

	if (sortedRows.empty() || !ValidateLevelExpRows(sortedRows) || storedLevel >= playerlevel::kMaxLevel) {
		out.storedLevelAfter = storedLevel;
		out.levelAfter = playerlevel::CappedLevel(storedLevel);
	} else {
		uint32_t targetLevel = 1;
		auto it = std::upper_bound(
			sortedRows.begin(), sortedRows.end(), out.totalExpAfter,
			[](uint64_t exp, const LevelExpRow& row) { return exp < row.requiredTotalExp; });
		if (it != sortedRows.begin()) {
			targetLevel = std::prev(it)->level;
		}
		targetLevel = std::min<uint32_t>(targetLevel, playerlevel::kMaxLevel);
		const uint32_t baseStored = storedLevel == 0 ? 1u : storedLevel;
		out.storedLevelAfter = std::max<uint32_t>(baseStored, targetLevel);
		out.levelAfter = playerlevel::CappedLevel(out.storedLevelAfter);
		out.leveledUp = out.storedLevelAfter > baseStored;
	}
	const auto display = ComputeLevelExpDisplay(out.levelAfter, out.totalExpAfter, sortedRows);
	out.nextLevelTotalExp = display.nextLevelTotalExp;
	return true;
}

inline bool IsPvpProgressionMode(uint32_t matchMode) {
	return matchMode == match::MATCH_MODE_5V5 ||
		   matchMode == match::MATCH_MODE_1V1 ||
		   matchMode == match::MATCH_MODE_PVP_TEAM ||
		   matchMode == match::MATCH_MODE_POLARITY_5V5 ||
		   matchMode == match::MATCH_MODE_GUILD_WAR_PVP ||
		   matchMode == match::MATCH_MODE_SEASON_TOURNAMENT;
}

inline bool IsGuildWarMode(uint32_t matchMode) {
	return matchMode == match::MATCH_MODE_GUILD_WAR_PVP ||
		   matchMode == match::MATCH_MODE_GUILD_WAR_SIEGE;
}

// 判定单局是否具备基础经验/银两资格，并返回查表键 (lookupMode, difficulty, outcomeTier)。
// 返回 false 表示本局恒为 0（作废/逃离/0 回合/瑞兽迎春/切磋/PVE 负/不达标 PVP 负平）。
inline bool ResolveBattleLookupKey(const ::BattleSettlementData& settlement,
								   uint32_t& lookupModeOut, uint32_t& difficultyOut,
								   uint32_t& outcomeTierOut) {
	lookupModeOut = 0;
	difficultyOut = 0;
	outcomeTierOut = 0;
	if (settlement.outcome() == ::BATTLE_OUTCOME_VOID ||
		settlement.outcome() == ::BATTLE_OUTCOME_ONGOING ||
		settlement.total_rounds() == 0 ||
		settlement.fled()) {
		return false;
	}
	// 瑞兽迎春按契约关闭普通经验与银两。
	if (settlement.has_festival_beast() && settlement.festival_beast().activity_instance_id() != 0) {
		return false;
	}
	const uint32_t mode = settlement.match_mode();
	if (mode == match::MATCH_MODE_UNSPECIFIED || mode == match::MATCH_MODE_PVP_CHALLENGE) {
		return false;
	}
	const bool won =
		(settlement.outcome() == ::BATTLE_OUTCOME_SIDE_A_WIN && settlement.player_team_index() == 0) ||
		(settlement.outcome() == ::BATTLE_OUTCOME_SIDE_B_WIN && settlement.player_team_index() == 1);

	const bool isWuxingTower =
		settlement.has_tower_settlement() && settlement.tower_settlement().floor() != 0;
	if (isWuxingTower) {
		if (!won) {
			return false;
		}
		lookupModeOut = kWuxingTowerLookupMode;
		difficultyOut = 0;
		outcomeTierOut = kOutcomeTierWin;
		return true;
	}

	if (IsPvpProgressionMode(mode)) {
		lookupModeOut = mode;
		difficultyOut = 0;
		if (won) {
			outcomeTierOut = kOutcomeTierWin;
			return true;
		}
		// PVP 平局 / 负局走与装备材料同源的四重防挂机门槛。
		if (settlement.total_rounds() >= kPvpMinRoundsForLoss &&
			settlement.manual_action_rounds() >= kPvpMinManualRoundsForLoss &&
			settlement.effective_combat_contribution()) {
			outcomeTierOut = kOutcomeTierQualifiedNonWin;
			return true;
		}
		return false;
	}

	// PVE_SOLO / PVE_TEAM / GUILD_WAR_SIEGE：仅胜局产出。
	if (!won) {
		return false;
	}
	lookupModeOut = mode;
	difficultyOut = settlement.battle_difficulty();
	outcomeTierOut = kOutcomeTierWin;
	return true;
}

inline const ModeRewardRow* FindModeReward(const std::vector<ModeRewardRow>& rows,
										   uint32_t lookupMode, uint32_t difficulty,
										   uint32_t outcomeTier) {
	const ModeRewardRow* fallback = nullptr;
	for (const auto& row : rows) {
		if (row.matchMode != lookupMode || row.outcomeTier != outcomeTier) {
			continue;
		}
		if (row.difficulty == difficulty) {
			return &row;
		}
		if (row.difficulty == 0 && fallback == nullptr) {
			fallback = &row;
		}
	}
	return fallback;
}

// 对原始银两施加每周软上限 + 衰减比例 + 硬封顶（精确跨边界拆分）。
inline WeeklySilverEval EvaluateWeeklySilverCap(uint64_t rawSilver, uint64_t usedThisWeek,
												const WeeklySilverRule& rule) {
	WeeklySilverEval out;
	out.usedAfter = usedThisWeek;
	if (rawSilver == 0) {
		out.diminished = (rule.softCap > 0 && usedThisWeek >= rule.softCap);
		return out;
	}
	if (rule.hardCap > 0 && usedThisWeek >= rule.hardCap) {
		out.creditedSilver = 0;
		out.usedAfter = usedThisWeek;
		out.diminished = true;
		return out;
	}
	const uint64_t fullRoom = rule.softCap > usedThisWeek ? (rule.softCap - usedThisWeek) : 0;
	const uint64_t fullPart = std::min<uint64_t>(rawSilver, fullRoom);
	const uint64_t overflowPart = rawSilver - fullPart;
	// 防止乘法溢出：rawSilver 与 rate 均在业务范围内，使用 __int128 不可在 MSVC 用，改为安全拆分。
	uint64_t diminishedPart = 0;
	if (overflowPart > 0 && rule.diminishedRatePermyriad > 0) {
		const uint64_t q = overflowPart / kPermyriadBase;
		const uint64_t r = overflowPart % kPermyriadBase;
		diminishedPart = q * rule.diminishedRatePermyriad + (r * rule.diminishedRatePermyriad) / kPermyriadBase;
	}
	uint64_t total = fullPart + diminishedPart;
	if (rule.hardCap > 0) {
		const uint64_t hardRoom = rule.hardCap > usedThisWeek ? (rule.hardCap - usedThisWeek) : 0;
		total = std::min<uint64_t>(total, hardRoom);
	}
	out.creditedSilver = total;
	out.usedAfter = usedThisWeek + total;
	out.diminished = (overflowPart > 0) || (rule.hardCap > 0 && out.usedAfter >= rule.hardCap);
	return out;
}

// 统一在当前周账本上计算本局最终经验、实得银两与更新后的周账本候选。
inline void PrepareEconomyLedger(const ::BattleEconomyLedger& current, uint64_t currentWeekStartMs,
								 const ::BattleSettlementData& settlement,
								 uint64_t explicitExp, uint64_t explicitSilver,
								 const std::vector<ModeRewardRow>& rewardRows,
								 const WeeklySilverRule* rule,
								 ::BattleEconomyLedger& candidateOut,
								 BattleProgressionDelta& deltaOut) {
	candidateOut = current;
	deltaOut = BattleProgressionDelta{};

	// 抗时钟回拨：仅当新周起点严格晚于账本记录周起点时清零本周计数。
	bool weekReset = false;
	if (currentWeekStartMs > candidateOut.week_starts_at_ms()) {
		candidateOut.set_week_starts_at_ms(currentWeekStartMs);
		candidateOut.set_battle_silver_this_week(0);
		candidateOut.set_battle_exp_this_week(0);
		weekReset = true;
	}

	uint64_t tableExp = 0;
	uint64_t tableSilver = 0;
	uint32_t lookupMode = 0, difficulty = 0, outcomeTier = 0;
	if (!rewardRows.empty() && ResolveBattleLookupKey(settlement, lookupMode, difficulty, outcomeTier)) {
		if (const auto* row = FindModeReward(rewardRows, lookupMode, difficulty, outcomeTier); row != nullptr) {
			tableExp = row->expGain;
			// 纵深防御：帮战 V2 银两由 go/guild 独立发放，此处恒压为 0 杜绝双发。
			tableSilver = IsGuildWarMode(settlement.match_mode()) ? 0 : row->silverGain;
		}
	}

	// 向后兼容旧 PVE BattleRewardProfile 与既有单测：未配表时保留显式值，配表时取两者较大值不双发。
	const uint64_t finalExp = std::max<uint64_t>(explicitExp, tableExp);
	const uint64_t rawSilver = IsGuildWarMode(settlement.match_mode())
		? explicitSilver
		: std::max<uint64_t>(explicitSilver, tableSilver);

	uint64_t creditedSilver = rawSilver;
	bool diminished = false;
	uint64_t usedAfter = candidateOut.battle_silver_this_week();
	uint64_t softCap = 0;

	if (rule != nullptr && ValidateWeeklySilverRule(*rule) && (tableSilver > 0 || rawSilver > 0)) {
		softCap = rule->softCap;
		// 只有命中配置表银两路径时才纳入周软上限账本；若仅是未加载配置表的历史单测显式金币，原样保留不破坏旧断言。
		if (tableSilver > 0) {
			const auto eval = EvaluateWeeklySilverCap(rawSilver, candidateOut.battle_silver_this_week(), *rule);
			creditedSilver = eval.creditedSilver;
			usedAfter = eval.usedAfter;
			diminished = eval.diminished;
			if (creditedSilver > 0) {
				candidateOut.set_battle_silver_this_week(usedAfter);
			}
		}
	}

	if (finalExp > 0) {
		const uint64_t curWeekExp = candidateOut.battle_exp_this_week();
		candidateOut.set_battle_exp_this_week(
			finalExp > (std::numeric_limits<uint64_t>::max() - curWeekExp)
				? std::numeric_limits<uint64_t>::max()
				: (curWeekExp + finalExp));
	}

	deltaOut.expGain = finalExp;
	deltaOut.silverGain = creditedSilver;
	deltaOut.weeklySilverUsedAfter = usedAfter;
	deltaOut.weeklySilverSoftCap = softCap;
	deltaOut.silverDiminished = diminished;
	deltaOut.ledgerChanged = weekReset || finalExp > 0 || (tableSilver > 0 && creditedSilver > 0);
}

}  // namespace battleprogression
