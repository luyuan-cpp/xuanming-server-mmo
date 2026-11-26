#pragma once

// 江湖恩怨与天墉生死状一期(docs/planning/battle-jianghu-grudge-duel-bounty-v1.md):
//   - 本人「闭关免战」开关查询与切换
//   - 30 分钟战败社交印记与清风洗尘符洒脱徽记展示(按角色持有代次 owner_revision 自动防过户背仇)
//   - 洗尘符(Item 3416) / 1000 银两幂等清除战败印记(挂 ClientRequestLedgerComp 防连点重扣)
//   - match -> scene 生死状资格与资费预检(CheckJianghuDuelEligibility)
//   - 备战期发起方队长资费预留/扣除(金帖走 BagService::PlaceHold,普通战书扣流通银两)、取消退还与结算写印记/复仇清除
// 线程:只在 scene 主 EventLoop 线程调用,无全局可变状态。

#include <cstdint>

#include "entt/src/entt/entity/entity.hpp"
#include "modules/bag/item_hold.h"

class BattleSettlementData;
class CheckJianghuDuelEligibilityRequest;
class CheckJianghuDuelEligibilityResponse;
class ClearJianghuDefeatMarkRequest;
class ClearJianghuDefeatMarkResponse;
class JianghuDuelPrepareFee;
class JianghuProfileView;
class PlayerJianghuComp;

class PlayerJianghuSystem
{
public:
	// 填充江湖个人面板视图(只读,绝不创建组件)。
	static uint32_t GetProfile(entt::entity player, uint64_t nowMs, ::JianghuProfileView &out);

	// 切换「闭关免战」开关(写 PROFILE 组并触发关键进度存盘)。
	static uint32_t SetRefuseStrangers(entt::entity player, bool refuseStrangers, uint64_t nowMs,
									   ::JianghuProfileView &out);

	// 幂等清除当前生效的 30 分钟战败社交印记(1000 流通银两普通清除,或消耗 1 张 Item 3416 清风洗尘符并附赠洒脱徽记)。
	static uint32_t ClearDefeatMark(entt::entity player, const ::ClearJianghuDefeatMarkRequest &request, uint64_t nowMs,
									::ClearJianghuDefeatMarkResponse &response);

	// match -> scene 发起约战前的只读资格与资费预检(无副作用)。
	static void CheckDuelEligibility(const ::CheckJianghuDuelEligibilityRequest &request,
									 ::CheckJianghuDuelEligibilityResponse &response);

	// PrepareBattle 阶段校验发起方队长生死状资费:
	// - 金帖(writ_tier=2):校验背包可动用持有量并加入 heldLines(走 BagService::PlaceHold 冻结,开打结算扣、取消/作废释)
	// - 普通战书(writ_tier=1):校验流通银两余额并输出 outSilverToDeduct(随后由 CommitPrepareSilverFee 同步扣除)
	static uint32_t ValidateAndCollectPrepareFee(entt::entity player, uint64_t battleId,
												 const ::JianghuDuelPrepareFee &fee,
												 item_hold::HoldLines &heldLines,
												 uint64_t &outSilverToDeduct);

	// PrepareBattle 在 PlaceHold 成功之后、挂 InBattleComp 之前提交普通战书流通银两预扣。
	static uint32_t CommitPrepareSilverFee(entt::entity player, uint64_t battleId, uint64_t silverToDeduct);

	// CancelBattlePrepare 备战取消时收尾待退普通银两记录:
	// skipSilverRefund=false 时若本局确曾预扣则当场退还流通银两;true 表示 match 已接管邮件退款,仅清记录防双退。
	static void OnBattlePrepareCancelled(entt::entity player, uint64_t battleId, bool skipSilverRefund);

	// ApplyVoidSettlement 作废结算时退还尚未核销的普通战书银两并清理待结记录(金帖由 ReleaseBattleItemHold 释放)。
	static void OnBattleVoid(entt::entity player, uint64_t battleId);

	// ApplySettlementToEntity 候选制备:纯计算本局生死状结算后的 PlayerJianghuComp 候选态、是否消耗预留金帖以及 0 回合退银两。
	// 返回 kSuccess 表示制备成功;不直接改写实体组件。
	static uint32_t PrepareBattleSettlement(entt::entity player, const ::PlayerJianghuComp &current,
											const ::BattleSettlementData &settlement, uint64_t nowMs,
											::PlayerJianghuComp &candidateOut, bool &changedOut,
											uint32_t &goldWritToConsumeOut,
											uint64_t *silverRefundOut = nullptr);
};
