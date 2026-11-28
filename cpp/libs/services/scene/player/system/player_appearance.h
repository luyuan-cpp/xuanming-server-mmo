#pragma once

// 角色形象永久收藏衣柜（已拥有免费切换 / 形象卡买断解锁）与 1 小时免费试用（bit 位图存储）。
// 设计文档: docs/design/character-appearance-switch.md。
//
// 事实源:
//   - 当前永久形象: PlayerProfileComp.appearance_id(同时隐式视作已拥有,兼容旧存档零迁移)
//   - 永久已拥有位图: PlayerProfileComp.appearance_owned_bits(按 CharacterAppearance 主键 bit_index 存位)
//   - 试用位图与试用态: PlayerProfileComp.appearance_trial_used_bits / trial_appearance_id / trial_expires_at
//   - 幂等防重: ClientRequestLedgerComp(kSwitchAppearance = 7, kStartAppearanceTrial = 8),随同一次存盘提交
//   - 选角目录同步: 存盘提交后 OnSaveCommitted 异步调用 DataService.UpdateRoleAppearance(瞬时失败由 Tick 退避重试)
//
// 线程模型: 只在 scene EventLoop 线程调用;不新起线程、不持有裸指针成员。

#include <cstdint>
#include <functional>
#include <string>
#include <string_view>

#include "engine/core/type_define/type_define.h"
#include "entt/src/entt/entity/entity.hpp"
#include "player/system/player_inbox_registry.h"

class ActorAppearanceChangedS2C;
class AppearanceStateNotify;
class BattlePlayerSnapshot;
class BuyAppearanceSeriesBundleRequest;
class BuyAppearanceSeriesBundleResponse;
class CastCityCelebrationRequest;
class CastCityCelebrationResponse;
class CityCelebrationNotify;
class ClaimCityCelebrationGiftRequest;
class ClaimCityCelebrationGiftResponse;
class EquipPetCosmeticRequest;
class EquipPetCosmeticResponse;
class EquipShowcaseCosmeticRequest;
class EquipShowcaseCosmeticResponse;
class EquipSkillVfxRequest;
class EquipSkillVfxResponse;
class PetInstance;
class PlayerAllData;
class PlayerAppearanceInfo;
class SendCosmeticGiftRequest;
class SendCosmeticGiftResponse;
class StartAppearanceTrialRequest;
class StartAppearanceTrialResponse;
class SwitchAppearanceRequest;
class SwitchAppearanceResponse;

namespace player_inbox
{
	class CosmeticGiftCreditPayload;
	class CosmeticGiftRefundPayload;
}

namespace data_service
{
	class UpdateRoleAppearanceRequest;
	class UpdateRoleAppearanceResponse;
}

// 派生运行时组件(不入库、不序列化):记录最近一次已提交落盘的永久外观,用于 OnSaveCommitted 判变。
struct PlayerAppearanceCommittedComp
{
	std::string permanentAppearanceId;
};

class PlayerAppearanceSystem
{
public:
	// 启动与热更校验 AppearanceRule 与 CharacterAppearance(含 bit_index 映射完整性)。
	static bool ValidateTables();

	// 返回此刻应对外展示的生效外观 ID(有效试用期内返回 trial_appearance_id,否则返回永久 appearance_id)。
	static std::string EffectiveAppearanceId(entt::entity player, uint64_t nowUnixSec);

	// 组装面板完整状态(永久/生效/试用/倒计时墙钟/规则价格/可用形象卡数/bit 位图/可选列表)。
	static void FillInfo(entt::entity player, uint64_t nowUnixSec, PlayerAppearanceInfo &info);

	// 切换永久形象:已拥有免费切换(不扣卡);未拥有时仅在 expect_free_switch=false 且 card_unlockable=1 时扣形象卡买断解锁。
	// 成功返回 kSuccess,带 client_request_id 幂等防重。
	static uint32_t SwitchAppearance(entt::entity player,
									 const SwitchAppearanceRequest &request,
									 uint64_t nowUnixSec,
									 SwitchAppearanceResponse &response);

	// 服务端内部权威授形入口(供排位/副本/活动解锁调用,不对外开放 RPC):
	// 仅在 appearance_owned_bits 置位并推送状态通知,不修改当前永久外观或进行中的试用;已拥有时幂等返回 kSuccess。
	static uint32_t GrantAppearance(entt::entity player,
									std::string_view appearanceId,
									uint64_t nowUnixSec,
									bool *alreadyOwned = nullptr);

	// 开启目标形象 1 小时免费试用(按 CharacterAppearance bit_index 记位,每形象每角色限 1 次,带幂等键)。
	static uint32_t StartTrial(entt::entity player,
							   const StartAppearanceTrialRequest &request,
							   uint64_t nowUnixSec,
							   StartAppearanceTrialResponse &response);

	// B4 激励视频槽位 3：授予指定未拥有角色 1 小时临时试穿（绝不置位永久拥有位图 appearance_owned_bits）。
	static uint32_t GrantAdTrial(entt::entity player,
								 std::string_view appearanceId,
								 uint64_t nowUnixSec,
								 bool dryRun = false);

	// 手动提前取消当前试用,立即还原永久形象。
	static uint32_t CancelTrial(entt::entity player, uint64_t nowUnixSec, PlayerAppearanceInfo &info);

	// 为指定灵宠实例穿戴或清除幻化外观(cosmetic_id=0 表示恢复原形;纯外观零数值,重复提交幂等收敛)。
	static uint32_t EquipPetCosmetic(entt::entity player,
									 const EquipPetCosmeticRequest &request,
									 uint64_t nowUnixSec,
									 EquipPetCosmeticResponse &response);

	// 为指定技能启用或清除技能特效外观(cosmetic_id=0 表示恢复默认;纯画面零数值,重复提交幂等收敛)。
	static uint32_t EquipSkillVfx(entt::entity player,
								  const EquipSkillVfxRequest &request,
								  uint64_t nowUnixSec,
								  EquipSkillVfxResponse &response);

	// Steam 式按缺口 8.5 折一键补齐收藏系列缺失角色（仅扣真实充值灵石，强制幂等键与缺失列表/报价防呆）。
	static uint32_t BuySeriesBundle(entt::entity player,
									const BuyAppearanceSeriesBundleRequest &request,
									uint64_t nowUnixSec,
									BuyAppearanceSeriesBundleResponse &response);

	// 批次 B2-1：穿戴/卸下排面装饰外观（阵纹/入场横幅/聊天气泡/分享卡底纹，耗 3417 排面装饰券或免费切换，纯视觉零数值）。
	static uint32_t EquipShowcaseCosmetic(entt::entity player,
										  const EquipShowcaseCosmeticRequest &request,
										  uint64_t nowUnixSec,
										  EquipShowcaseCosmeticResponse &response);

	// 服务端内部权威授予排面装饰外观（赠礼/手记/支持者包用，不自动穿戴，幂等置位）。
	static uint32_t GrantShowcaseCosmetic(entt::entity player,
										  uint32_t cosmeticId,
										  uint64_t nowUnixSec,
										  bool *alreadyOwned = nullptr);

	// 批次 B2-2：主城和平分线施放全城同乐社交气氛道具（3418 灵灯 / 3419 烟花 / 3420 仙茶席，同分线并发上限 5，持续 120s）。
	static uint32_t CastCityCelebration(entt::entity player,
										const CastCityCelebrationRequest &request,
										uint64_t nowUnixSec,
										CastCityCelebrationResponse &response);

	// 批次 B2-2：同分线 >= 10 级玩家领取全城同乐绑定银两（仅 CURRENCY_TYPE_BIND_GOLD=3，每场限 1 次，周封顶 3000）。
	static uint32_t ClaimCityCelebrationGift(entt::entity player,
											 const ClaimCityCelebrationGiftRequest &request,
											 uint64_t nowUnixSec,
											 ClaimCityCelebrationGiftResponse &response);

	// 批次 B2-3：好友纯绑定外观/手记典藏真实灵石赠礼（强制 7 天好友无拉黑 + 每周 5 笔 + 报价防呆 + 仅扣真实灵石）。
	static uint32_t SendCosmeticGift(entt::entity player,
									 const SendCosmeticGiftRequest &request,
									 uint64_t nowUnixSec,
									 SendCosmeticGiftResponse &response);

	// 批次 B2-3 PlayerInbox 执行器（登记在 player_inbox_registry.cpp kExecutors[7..9]）：
	static PlayerInboxExecResult ApplyCosmeticGiftCredit(entt::entity player, const std::string &payload);
	static PlayerInboxExecResult ApplyCosmeticGiftRefund(entt::entity player, const std::string &payload);
	static PlayerInboxExecResult ApplyCosmeticGiftRevoke(entt::entity player, const std::string &payload);

	// 服务端内部权威授予灵宠幻化权益(不自动穿戴,幂等置位并推送状态通知)。
	static uint32_t GrantPetCosmetic(entt::entity player,
									 uint32_t cosmeticId,
									 uint64_t nowUnixSec,
									 bool *alreadyOwned = nullptr);

	// 服务端内部权威授予技能特效权益(不自动启用,幂等置位并推送状态通知)。
	static uint32_t GrantSkillVfx(entt::entity player,
								  uint32_t cosmeticId,
								  uint64_t nowUnixSec,
								  bool *alreadyOwned = nullptr);

	// 解析灵宠实例当前有效的幻化外观(必须已拥有、已启用且物种适配;失效时自动回退 0 与空串)。
	static void ResolvePetCosmeticVisual(entt::entity player,
										 const PetInstance &pet,
										 uint32_t &outCosmeticId,
										 std::string &outArtKey);

	// 向玩家战斗快照写入已校验有效的技能特效覆盖映射(纯战斗画面表现零数值)。
	static void FillBattleSkillVfx(entt::entity player, BattlePlayerSnapshot &snapshot);

	// 生命周期钩子
	static void OnPlayerLoaded(entt::entity player, uint64_t nowUnixSec);
	static void OnSaveCommitted(Guid playerId, const PlayerAllData &committed, uint64_t nowMs);
	static void OnPlayerDestroyed(Guid playerId);

	// 1 秒节拍:扫描到期试用自动还原并广播,以及目录外观同步失败的退避重试。
	static void Tick(uint64_t nowUnixSec, uint64_t nowMs);

	// ── 测试注入接缝(生产代码不调用;置空即恢复默认生产实现) ──
	struct GiftSocialVerdict
	{
		bool available{false};
		bool hasBlock{false};
		bool friendsFor7Days{false};
		bool targetAlreadyOwned{false};
	};
	using DirectorySyncDone = std::function<void(bool callOk, const data_service::UpdateRoleAppearanceResponse *reply)>;
	using DirectorySyncTransport = std::function<bool(const data_service::UpdateRoleAppearanceRequest &request, DirectorySyncDone done)>;
	using AppearanceBroadcastSender = std::function<void(entt::entity player, const ActorAppearanceChangedS2C &msg)>;
	using StateNotifySender = std::function<void(entt::entity player, const AppearanceStateNotify &notify)>;
	using SaveRequester = std::function<void(entt::entity player, const char *reason)>;
	using CelebrationBroadcastSender = std::function<void(uint64_t sceneId, const CityCelebrationNotify &notify)>;
	using GiftSocialChecker = std::function<GiftSocialVerdict(Guid senderId, Guid targetId, uint32_t targetKind, uint32_t targetIdU32, std::string_view targetIdStr)>;
	using GiftCreditDispatcher = std::function<bool(Guid targetPlayerId, const player_inbox::CosmeticGiftCreditPayload &payload)>;
	using GiftRefundDispatcher = std::function<bool(Guid senderPlayerId, const player_inbox::CosmeticGiftRefundPayload &payload)>;

	struct TestHooks
	{
		DirectorySyncTransport directorySync;
		AppearanceBroadcastSender broadcast;
		StateNotifySender notify;
		SaveRequester save;
		CelebrationBroadcastSender celebrationBroadcast;
		GiftSocialChecker giftSocialCheck;
		GiftCreditDispatcher giftCreditDispatch;
		GiftRefundDispatcher giftRefundDispatch;
		uint32_t overrideSceneConfigId{0};
		uint64_t overrideSceneId{0};
	};
	static void SetTestHooks(TestHooks hooks);
	static void ResetTestState();
};
