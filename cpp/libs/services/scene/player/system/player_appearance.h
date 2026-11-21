#pragma once

// 角色形象切换（商城 10 元角色形象卡）与 1 小时免费试用（bit 位图存储）。
// 设计文档: docs/design/character-appearance-switch.md。
//
// 事实源:
//   - 永久形象: PlayerProfileComp.appearance_id(角色唯一永久形象,不设衣柜;切形象扣 1 张角色形象卡并覆盖)
//   - 试用位图与试用态: PlayerProfileComp.appearance_trial_used_bits / trial_appearance_id / trial_expires_at
//   - 幂等防重: ClientRequestLedgerComp(kSwitchAppearance = 7, kStartAppearanceTrial = 8),随同一次存盘提交
//   - 选角目录同步: 存盘提交后 OnSaveCommitted 异步调用 DataService.UpdateRoleAppearance(瞬时失败由 Tick 退避重试)
//
// 线程模型: 只在 scene EventLoop 线程调用;不新起线程、不持有裸指针成员。

#include <cstdint>
#include <functional>
#include <string>

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

	// 消耗角色形象卡永久切换唯一形象(带 client_request_id 幂等防重)。成功返回 kSuccess。
	static uint32_t SwitchAppearance(entt::entity player,
									 const SwitchAppearanceRequest &request,
									 uint64_t nowUnixSec,
									 SwitchAppearanceResponse &response);

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
