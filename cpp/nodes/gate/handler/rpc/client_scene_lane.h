#pragma once

// gate 场景 lane 的装配层(docs/design/client-scene-request-handoff-hold.md §4)。
//
// 职责:同一会话发往 scene 的客户端消息(PROTOCOL_TCP)按收包顺序发出。
//   - 请求-响应消息(内层响应不是 Empty)单飞:一次只放一条在途。scene 明确答复「确定未执行、稍后重投」(玩家归属正在交接,
//     或还没在目标节点就绪)时,这一条放回队头、按 kRetryInterval 重投,直到被受理或从收包起等了 kMaxSceneLaneWait。
//     换场景节点的窗口里客户端请求因此不丢、不乱序,也不必由客户端重发;连续换几次节点时队列一直留在 gate 的会话上。
//   - 单向消息(内层响应为 Empty,移动等)不等完成:排在它前面的请求都已发出就发,与改造前一样按收包顺序上连接;
//     队头请求在等重投时直接发出、不等它(scene_lane.h HoldForRetry 写了理由)。
//
// 只有 scene 明确答复 RETRY_LATER 的请求才重投:那是「确定未执行、等一等就能受理、换节点执行含义也不变」,三样都由 scene 判
// (nodes/scene/handler/rpc/player/player_handoff_gate.h 的白名单)。gate 不认识业务方法,自己不决定重投谁。
// RPC0 失败(超时、断连等,结果未知或对端派发前拒绝)一律不重投,照旧回带请求 id 的 1003。
//
// 纯逻辑(排队、单飞、放回队头)在 services/gate/session/comp/scene_lane.h,本文件只做装配与副作用
// (发出、回执、在途判丢、计数、日志)。全部在 gate 唯一的 EventLoop 线程上调用,不加锁,不新增线程。
// 生命周期(AGENTS.md §11.7):排队项、在途索引与回调都不持有 TcpConnectionPtr 或裸 this;每次下发都先按 session_id
// 找会话、核对会话代次,再 conn.lock()。会话 erase 时 lane 随 SessionInfo 一起销毁,不需要额外清理:在途调用的完成回调
// 找不到会话就丢弃(同时注销在途索引),等重投登记表里的残项由 Tick 摘掉。

#include <chrono>
#include <cstddef>
#include <cstdint>
#include <memory>

#include "engine/core/type_define/type_define.h"
#include "session/comp/scene_lane.h"

class ClientRequest;
class ProcessClientPlayerMessageResponse;
class RpcStatus;
struct SessionInfo;

namespace gate_scene_lane
{

// 每会话排队 + 在途的总上限。请求超限回带请求 id 的 1008(kRateLimitExceeded),不计非法包;单向消息超限直接丢。
// 比路由 lane 的 32 大:进场后客户端会连发一批面板查询,单飞之后它们要排队;排在请求后面的移动也占名额。
inline constexpr std::size_t kMaxSceneLaneDepth = 64;
// 从收包起的等待上限(排队 + 等重投):超过就不再发出,请求回带请求 id 的 1003(明确「没有执行」,客户端重试安全),
// 单向消息丢弃。取预算链允许的上限(定案,不等压测):
// 不变式 I6:gate→scene 调用超时(12000)+ 本值 + 1000 ≤ 客户端请求超时 15000(client-scene-request-handoff-hold.md §5);
// client_session_handler.cpp 有 static_assert 守着。
inline constexpr std::chrono::milliseconds kMaxSceneLaneWait{2000};
// 重投间隔。重投由 Tick 驱动(与路由 lane 的过期扫描同一个 0.25 s 定时器),实际间隔是「不小于本值的下一拍」。
inline constexpr std::chrono::milliseconds kRetryInterval{200};
// 在途调用的本地判丢截止 = 发出时刻 + gate→scene 调用超时 + 本余量,由 Tick 主动扫描(必须主动扫:队头卡住时客户端在等
// 回包,不会再发新请求来触发惰性清理,与路由 lane 同一条教训)。RPC0 承诺完成回调恰好一次、超时由通道自己报,
// 所以判丢只是纵深防御:回调万一丢了,这条会话的 scene 请求不能永远卡在它后面。正常运行时 inflight_lost 为 0。
inline constexpr std::chrono::milliseconds kInflightLostMargin{1000};

// 发送函数的结果。
enum class SceneSendResult : uint8_t
{
	kSent,	  // 已交给 scene 的通道。请求-响应消息之后恰好一次调用 OnSceneCallDone(带同一个 callSeq);单向消息没有完成
	kNoScene, // 确定没发出:会话没有绑定 scene 节点(没进游戏、路由已清),或节点没有可用通道
};

// 一次发送要带给 scene 的东西(除客户端原包外)。
struct SceneSendArgs
{
	bool oneWay{false};			// 单向发出(scene 不回包),callSeq 不使用;否则以 callSeq 登记每次调用回调
	CallSeq callSeq{kNoCallSeq}; // 请求-响应调用的序号,完成回调按它认领
	// 收到这条消息时会话绑的场景路由与此刻不同(它排着等过一次换场景或换节点):写进转发信封的
	// received_on_previous_route,scene 据此不在新场景里执行含义绑在旧场景上的消息(设计 S13)。
	bool receivedOnPreviousRoute{false};
};

// 发往 scene 的发送函数,由 client_session_handler.cpp 在启动时注入(它持有节点注册表与 RPC 通道的细节)。
// 契约:返回之前不得同步调用 OnSceneCallDone(RpcClient::CallMethod 的完成一律异步投递,满足)。
using SceneSendFn = SceneSendResult (*)(SessionId sessionId, const SessionInfo &session, const ClientRequest &request,
										const SceneSendArgs &args);
// callTimeout 是发送函数给请求-响应调用设的 RPC0 超时,用来算本地判丢截止。
void SetSceneSender(SceneSendFn fn, std::chrono::milliseconds callTimeout);

// 准入。前置条件:会话存在且已验证;白名单 / 体积 / 限流 / GM / 热关停 / LeaveGame 屏障几道闸都已通过;协议是 PROTOCOL_TCP。
// oneWay:内层方法的响应是 Empty。
// now:单调时钟的当前时刻。三个入口都由调用方传入、本模块不自己读时钟,单测因此可以确定地推进时间。
void Admit(SessionId sessionId, SessionInfo &session, const std::shared_ptr<ClientRequest> &request, bool oneWay,
		   SteadyTime now);

// 请求-响应调用的完成(发送函数登记的 RPC0 回调转过来)。response 在 status 失败时为空。
// 认领在途那一条(恰好一次),按 scene_client_call_reply.h 的 Decide 处置:原样下发 / 回 1003 / 不下发 / 放回队头等重投,
// 然后放行同一会话的下一条。会话已关闭或号已复用、序号对不上(本地已判丢)都丢弃这次完成。
void OnSceneCallDone(SessionId sessionId, uint64_t incarnation, CallSeq callSeq, const RpcStatus &status,
					 const ProcessClientPlayerMessageResponse *response, SteadyTime now);

// 会话被重定向到别的 gate(跨 zone 传送推 msg 124;排空迁移在场景 lane 不空时不迁,走不到这里):
// 排队中与等重投的请求不再发出,逐条回带请求 id 的 1003(确定未执行,到新 gate 重试安全),计 dropped_on_redirect;
// 排队中的单向消息丢弃。在途那一条的回包照常下发,但它之后若被答复「稍后重投」也不再等,当场回 1003。
// 必须在推 124 之前调用(gate_redirect_delivery.cpp,与路由 lane 的 OnSessionRedirected 同一位置、同一理由):
// 回执与 124 走同一条连接顺序写出,客户端先收齐回执再换连接。重定向之后新到的 scene 消息由
// client_session_handler.cpp 的 RejectedBehindQueuedLeaveGame 在收包时回 1003,不进 lane。
void OnSessionRedirected(SessionId sessionId, SessionInfo &session);

// 定时驱动(main.cpp 与 gate_lane::ExpireDue 挂在同一个定时器上,回调不捕获对象):
//   1. 在途判丢:过了本地截止仍没有完成回调的调用,回 1003 并放行后面的消息;
//   2. 重投:只扫队头在等重投的会话;
//   3. 每 30s 打一行 [scene_lane] 汇总。
void Tick(SteadyTime now);

// 只用于每 30s 一行的 [scene_lane] 汇总日志(gate 没有 Prometheus 出口)。不按会话、玩家或消息号分维度(AGENTS.md §9)。
// 对账:
//   - admitted 的请求最终恰好落进 replied / failed / no_content / dropped_uncorrelated / expired_in_queue / expired_on_hold /
//     no_scene / dropped_on_redirect / inflight_lost 之一,或仍在排队 / 在途,或随会话关闭消失;
//   - 单向消息要么不排队直接发出(one_way_direct),要么排队(one_way_queued)后发出或丢弃;
//     发出成功计 one_way_sent,发不出计 no_scene,丢弃计 one_way_dropped。
struct SceneLaneCounters
{
	uint64_t admitted{0};			 // 排进队列的请求-响应消息
	uint64_t sent{0};				 // 请求-响应消息的发出次数(含重投)
	uint64_t replied{0};			 // scene 的应答原样下发
	uint64_t failed{0};				 // RPC0 失败(超时 / 断连 / 对端拒绝等),回 1003
	uint64_t noContent{0};			 // scene 派发前早退且不是 RETRY_LATER,回 1003
	uint64_t droppedUncorrelated{0}; // 同上但请求号为 0,没法关联,不下发
	uint64_t held{0};				 // 至少被放回队头等过一次重投的请求数
	uint64_t retryVerdicts{0};		 // scene 答复「稍后重投」的次数
	uint64_t expiredOnHold{0};		 // 等重投超过 kMaxSceneLaneWait,回 1003
	uint64_t expiredInQueue{0};		 // 排在别的请求后面超过 kMaxSceneLaneWait,回 1003
	uint64_t noScene{0};			 // 轮到发出时会话没有可用的 scene 节点,回 1003(单向消息同样回)
	uint64_t rejectedFull{0};		 // lane 满,回 1008
	uint64_t droppedOnRedirect{0};	 // 会话被重定向时还没被受理的请求,回 1003
	uint64_t oneWayDirect{0};		 // 不排队直接发出的单向消息(前面没有未发出的请求,或队头在等重投)
	uint64_t oneWayQueued{0};		 // 排在未发出的请求后面的单向消息
	uint64_t oneWaySent{0};			 // 发出成功的单向消息(含直接发出的)
	uint64_t oneWayDropped{0};		 // 丢弃的单向消息:排队超过等待上限、lane 满、会话被重定向
	uint64_t sentOnNewRoute{0};		 // 发出时会话已换过场景路由、信封标了 received_on_previous_route 的消息(含重投)
	uint64_t inflightLost{0};		 // 在途调用过了本地判丢截止(纵深防御,正常为 0)
	uint64_t lateDropped{0};		 // 完成回调到达时序号已对不上(本地已判丢),丢弃
	std::size_t maxDepthSeen{0};
};

// 当前线程(gate 的 EventLoop)的累计计数。单测据此核对分支。
const SceneLaneCounters &GetCounters();
// 队头在等重投的会话数、在途索引里的调用数(汇总日志与单测用)。
std::size_t HoldingSessionCount();
std::size_t InFlightCount();

// 只给单测用:清空计数、在途索引与等重投登记表,摘掉注入的发送函数。调用序号的发号器不回退。
void ResetForTesting();

} // namespace gate_scene_lane
