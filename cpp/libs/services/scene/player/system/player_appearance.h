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

class ActorAppearanceChangedS2C;
class AppearanceStateNotify;
class PlayerAllData;
class PlayerAppearanceInfo;
class StartAppearanceTrialRequest;
class StartAppearanceTrialResponse;
class SwitchAppearanceRequest;
class SwitchAppearanceResponse;

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

	// 手动提前取消当前试用,立即还原永久形象。
	static uint32_t CancelTrial(entt::entity player, uint64_t nowUnixSec, PlayerAppearanceInfo &info);

	// 生命周期钩子
	static void OnPlayerLoaded(entt::entity player, uint64_t nowUnixSec);
	static void OnSaveCommitted(Guid playerId, const PlayerAllData &committed, uint64_t nowMs);
	static void OnPlayerDestroyed(Guid playerId);

	// 1 秒节拍:扫描到期试用自动还原并广播,以及目录外观同步失败的退避重试。
	static void Tick(uint64_t nowUnixSec, uint64_t nowMs);

	// ── 测试注入接缝(生产代码不调用;置空即恢复默认生产实现) ──
	using DirectorySyncDone = std::function<void(bool callOk, const data_service::UpdateRoleAppearanceResponse *reply)>;
	using DirectorySyncTransport = std::function<bool(const data_service::UpdateRoleAppearanceRequest &request, DirectorySyncDone done)>;
	using AppearanceBroadcastSender = std::function<void(entt::entity player, const ActorAppearanceChangedS2C &msg)>;
	using StateNotifySender = std::function<void(entt::entity player, const AppearanceStateNotify &notify)>;
	using SaveRequester = std::function<void(entt::entity player, const char *reason)>;

	struct TestHooks
	{
		DirectorySyncTransport directorySync;
		AppearanceBroadcastSender broadcast;
		StateNotifySender notify;
		SaveRequester save;
	};
	static void SetTestHooks(TestHooks hooks);
	static void ResetTestState();
};
