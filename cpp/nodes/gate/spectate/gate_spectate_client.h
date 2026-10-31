#pragma once

// SpectateHub 在 gate 进程里的装配层(docs/design/battle-spectate-tiered-fanout.md §15.2 / §15.5):
//   - 把 hub 接上真实的会话表、主 loop 与时钟(sendFrame 在这里:查会话 → lock 弱引用 → 软水位 → 写连接);
//   - 处理客户端两条本地消息 SubscribeSpectate / UnsubscribeSpectate;
//   - 给会话关闭路径、Kafka 回调和巡检定时器一个不捕获对象的入口。
//
// 线程模型:全部只能在 gate 主 loop 线程调用,状态是 thread_local,与 tlsSessionManager 同一归属。
// 生命周期(AGENTS.md §11.7):这里只存 weak_ptr<SpectateHub>,hub 由 GateRuntimeContext 持有;
// 定时器和 Kafka 回调都不捕获对象,hub 没了这些入口自动变成空操作。
//
// 接线没有单测夹具(与 gate 其它 handler 同样),靠 robot 冒烟覆盖;可判定的逻辑都在 hub 与 ticket 里。

#include <memory>
#include <string>

#include "engine/core/type_define/type_define.h"

class ClientRequest;
struct SessionInfo;

namespace contracts::kafka
{
	class SpectateFrameEvent;
}

namespace muduo::net
{
	class EventLoop;
}

namespace gate_spectate
{
	class SpectateHub;

	// 巡检间隔(秒)。比空闲判定 30s 短得多就够了,收尾晚几秒没有影响。
	inline constexpr double kSweepIntervalSeconds = 5.0;

	// 建 hub 并记下验签密钥。main.cpp 在主 loop 线程启动时调用一次,返回值由 GateRuntimeContext 持有。
	// tokenSecret 为空时进程可以起来(dev / test,启动门禁已留痕),但所有订阅都被拒绝(fail-closed)。
	std::shared_ptr<SpectateHub> InstallHub(muduo::net::EventLoop &loop, std::string tokenSecret);

	// 客户端发来的 GateNodeService 消息。前置条件:会话已验证,白名单 / 体积 / 限流 / GM 几道闸都已通过。
	// 回执与失败都带请求 id;成功的订阅回执先于首帧到达客户端。
	void HandleClientRequest(SessionId sessionId, SessionInfo &session, const ClientRequest &request);

	// 摘掉这个会话的观战订阅。两种调用方:会话即将 erase(所有摘除会话的路径都要调用),以及回选角(LeaveGame,TCP 不断,
	// gate_scene_entry::OnClientLeaveGame)。只动订阅表,不发帧、不碰连接。
	void OnSessionClosed(SessionId sessionId);

	// Kafka 消费回调的入口(已在主 loop 上)。
	void OnKafkaFrame(const contracts::kafka::SpectateFrameEvent &event);

	// 巡检定时器的入口。
	void SweepIdleNow();
} // namespace gate_spectate
