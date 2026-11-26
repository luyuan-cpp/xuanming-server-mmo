#pragma once

// 零拒审激励视频广告系统(docs/design/rewarded-ad-system.md):
// 发放物理不可交易的绑定银两(kCurrencyBindGold = 3),默认关闭(enabled = 0),
// 复用 ClientRequestLedgerComp(WALLET 存盘组)实现断线重连补领与幂等防重。
// 线程:只在 scene 主 EventLoop 线程调用,无全局可变业务状态。

#include <cstdint>

#include "entt/src/entt/entity/entity.hpp"

class GetRewardedAdInfoRequest;
class GetRewardedAdInfoResponse;
class BeginRewardedAdSessionRequest;
class BeginRewardedAdSessionResponse;
class ClaimRewardedAdRequest;
class ClaimRewardedAdResponse;

class PlayerRewardedAdSystem
{
public:
	// 启动与表热更后校验 RewardedAdSlot 配置表(强断言仅发 CURRENCY_TYPE_BIND_GOLD)。
	// 非法行打英文 ERROR 并在运行期 fail-closed 拒绝,不崩服。
	static bool ValidateTables();

	// 查询可用激励视频槽位与双银两余额。纯只读,绝不创建组件。
	static uint32_t GetInfo(entt::entity player, const ::GetRewardedAdInfoRequest &request,
							uint64_t nowMs, ::GetRewardedAdInfoResponse &response);

	// 开播前签发一次性随机短效会话(落盘 WALLET 组抗 30 秒看广告断线重连)。
	static uint32_t BeginSession(entt::entity player, const ::BeginRewardedAdSessionRequest &request,
								 uint64_t nowMs, ::BeginRewardedAdSessionResponse &response);

	// 完播后幂等核销会话、发放绑定银两、记账并触发关键进度原子存盘。
	static uint32_t Claim(entt::entity player, const ::ClaimRewardedAdRequest &request,
						  uint64_t nowMs, ::ClaimRewardedAdResponse &response);
};
