#include "client_scene_lane.h"

#include <algorithm>
#include <chrono>
#include <cstdint>
#include <optional>
#include <unordered_map>
#include <utility>
#include <vector>

#include "muduo/base/Logging.h"
#include "entt/src/entt/entity/entity.hpp"
#include "client_request_lane.h"
#include "gate_codec.h"
#include "scene_client_call_reply.h"
#include "network/rpc_status.h"
#include "proto/common/base/message.pb.h"
#include "proto/common/base/node.pb.h"
#include "proto/scene/scene.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include <session/manager/session_manager.h>
#include "session/comp/scene_lane.h"
#include "session/comp/scene_lane_item.h"
#include "thread_context/trace_context_tls.h"
#include "utils/data_structures/inflight_index.h"

namespace gate_scene_lane
{
namespace
{

// 在途索引的值。只存定位会话所需的数据,不存连接(AGENTS.md §11.7);回执要用的请求号在 lane 保管的那一条里。
struct InflightCall
{
	SteadyTime expiresAt;
	SessionId sessionId{0};
	uint64_t incarnation{0};
};

// 全部只在 gate 唯一的 EventLoop 线程上读写(会话表同为 thread_local)。
// 在途索引:按调用序号登记每次发出的请求及其本地判丢截止,Tick 主动扫描过期项(engine/core 的 InflightIndex,
// 与路由 lane、RpcChannel 共用同一份实现)。完成回调到达时注销;会话先关闭的调用由它迟到的回调注销。
thread_local InflightIndex<InflightCall> tlsInflight;
thread_local CallSeq tlsLastCallSeq = kNoCallSeq;
thread_local SceneLaneCounters tlsCounters;
thread_local SteadyTime tlsLastSummaryAt{};
// 队头在等重投的会话:session_id → 会话代次。Tick 只扫这张表,不扫全部会话;会话没了或代次变了就摘掉。
// 不存连接、不存指针,每次都按 session_id 重新找会话。
thread_local std::unordered_map<SessionId, uint64_t> tlsHolding;
// 启动时注入一次(SetSceneSender),之后只读。
SceneSendFn gSceneSender = nullptr;
std::chrono::milliseconds gSceneCallTimeout{0};

constexpr std::chrono::seconds kSummaryInterval{30};

CallSeq NextCallSeq()
{
	return ++tlsLastCallSeq;
}

// 「确定没有执行」的回执:带请求 id 与原消息号的 1003,客户端重试安全。单向消息本来就没有回包;请求号为 0 的请求
// 没法按 id 认领(与 scene_client_call_reply.h 对 scene 早退的口径相同),两者都不回。
void ReplyNotExecuted(SessionInfo &session, const SceneLaneItem &item)
{
	if (item.oneWay || item.clientRequestId == 0)
	{
		return;
	}
	gate_lane::SendEnvelope(session, item.clientRequestId, item.messageId, kServiceUnavailable);
}

// 会话此刻绑的场景路由(场景实例 + scene 节点实体)。与收包时记下的不同,说明这条消息排着的时候换过场景或换过节点。
SceneRouteStamp CurrentSceneRoute(const SessionInfo &session)
{
	return SceneRouteStamp{
		session.sceneId,
		static_cast<uint64_t>(entt::to_integral(session.GetNodeEntity(common::base::SceneNodeService))),
	};
}

// 交给发送函数;发出成功且信封标了「换过路由」时计数。
SceneSendResult Send(SessionId sessionId, const SessionInfo &session, const ClientRequest &request, const SceneSendArgs &args)
{
	const SceneSendResult result =
		gSceneSender != nullptr ? gSceneSender(sessionId, session, request, args) : SceneSendResult::kNoScene;
	if (result == SceneSendResult::kSent && args.receivedOnPreviousRoute)
	{
		++tlsCounters.sentOnNewRoute;
	}
	return result;
}

// 发一条单向消息。没有可用的 scene 节点时确定未发出,回 1003 信封(单向消息的请求号为 0;与改造前 gate 转发
// scene 消息时「Scene not ready」一支相同,client-request-ordering.md §4.7)。
// receivedOnPreviousRoute:收到它时会话绑的场景路由与此刻不同(设计 S13),scene 不会在新场景里执行它。
void SendOneWay(SessionId sessionId, SessionInfo &session, const ClientRequest &request, bool receivedOnPreviousRoute)
{
	SceneSendArgs args;
	args.oneWay = true;
	args.receivedOnPreviousRoute = receivedOnPreviousRoute;
	if (Send(sessionId, session, request, args) == SceneSendResult::kSent)
	{
		++tlsCounters.oneWaySent;
		return;
	}
	++tlsCounters.noScene;
	gate_lane::SendEnvelope(session, request.id(), request.message_id(), kServiceUnavailable);
}

// 维护等重投登记表:会话的队头在等重投就登记,否则摘掉(只摘本会话代次登记的那一条)。
void TrackHolding(SessionId sessionId, const SessionInfo &session)
{
	if (session.sceneLane.Holding())
	{
		tlsHolding[sessionId] = session.incarnation;
		return;
	}
	if (const auto it = tlsHolding.find(sessionId); it != tlsHolding.end() && it->second == session.incarnation)
	{
		tlsHolding.erase(it);
	}
}

// scene 答复这一条请求「确定未执行、稍后重投」:
//   - 会话已被重定向到别的 gate:玩家正在离开,这里等不到受理,当场回执,客户端到新 gate 上重发;
//   - 从收包起已经等过上限:回执,不再等;
//   - 否则放回队头,等下一次重投。排在它后面的请求保持收包顺序,一条都不发;排在它后面的单向消息马上发出
//     (scene_lane.h HoldForRetry 写了理由)。
void HoldOrExpire(SessionId sessionId, SessionInfo &session, SceneLaneItem item, SteadyTime now)
{
	++tlsCounters.retryVerdicts;
	if (session.redirectedAt.has_value())
	{
		++tlsCounters.droppedOnRedirect;
		ReplyNotExecuted(session, item);
		return;
	}
	if (WaitExceeded(item.admittedAt, now, kMaxSceneLaneWait))
	{
		++tlsCounters.expiredOnHold;
		ReplyNotExecuted(session, item);
		return;
	}
	if (item.holds++ == 0)
	{
		++tlsCounters.held;
	}
	const SceneRouteStamp route = CurrentSceneRoute(session);
	for (const auto &oneWay : session.sceneLane.HoldForRetry(std::move(item), now + kRetryInterval))
	{
		const tracing::ScopedSpan itemTrace(oneWay.trace);
		SendOneWay(sessionId, session, *oneWay.request, oneWay.receiptRoute != route);
	}
}

// 按收包顺序发出,直到遇到不能发的那条为止:
//   - 单向消息:它前面的请求都已发出,发;
//   - 请求:没有在途、不在等重投(或重投时刻已到)才发,发出后成为在途,继续把紧跟在它后面的单向消息发出去
//     (它们在连接上排在这条请求之后,scene 按序执行),遇到下一条请求就停。
// 没发出的条目(等待超时、会话没有 scene 节点)当场回执或丢弃,继续取下一条。循环次数 ≤ 队长(≤ kMaxSceneLaneDepth)。
void Pump(SessionId sessionId, SessionInfo &session, SteadyTime now)
{
	auto &lane = session.sceneLane;
	while (auto item = lane.TakeNextIfReady(now))
	{
		// 每条按它收包时的 trace 处理:Pump 可能在上一条的完成回调或定时重投里跑,那时线程上是别的上下文。
		const tracing::ScopedSpan itemTrace(item->trace);
		if (WaitExceeded(item->admittedAt, now, kMaxSceneLaneWait))
		{
			if (item->oneWay)
			{
				++tlsCounters.oneWayDropped;
			}
			else if (item->holds > 0)
			{
				++tlsCounters.expiredOnHold;
			}
			else
			{
				++tlsCounters.expiredInQueue;
			}
			ReplyNotExecuted(session, *item);
			continue;
		}
		// 排着的时候换过场景或换过节点:信封上标出来,由 scene 决定这条消息在新场景里还执不执行(设计 S13)。
		// 被扣住后重投到新节点的请求也走这里:它在白名单里,scene 照常执行。
		const bool receivedOnPreviousRoute = item->receiptRoute != CurrentSceneRoute(session);
		if (item->oneWay)
		{
			SendOneWay(sessionId, session, *item->request, receivedOnPreviousRoute);
			continue;
		}
		SceneSendArgs args;
		args.callSeq = NextCallSeq();
		args.receivedOnPreviousRoute = receivedOnPreviousRoute;
		const CallSeq callSeq = args.callSeq;
		const SceneSendResult result = Send(sessionId, session, *item->request, args);
		if (result == SceneSendResult::kNoScene)
		{
			// 会话没有可用的 scene 节点(没进游戏、路由已清、节点还没连上)。确定未发出,回带请求 id 的 1003,
			// 与改造前「Scene not ready」一支相同(client-request-ordering.md §4.7)。
			++tlsCounters.noScene;
			gate_lane::SendEnvelope(session, item->clientRequestId, item->messageId, kServiceUnavailable);
			continue;
		}
		++tlsCounters.sent;
		if (!lane.MarkSent(std::move(*item), callSeq))
		{
			// 不变量:TakeNextIfReady 只在没有在途时交出请求,MarkSent 不应失败。
			LOG_ERROR << "[scene_lane] MarkSent refused although the lane was idle; the request will get no reply"
					  << " session_id=" << sessionId << " lane_in_flight=" << lane.InFlightSeq();
			break;
		}
		tlsInflight.Insert(callSeq, InflightCall{now + gSceneCallTimeout + kInflightLostMargin, sessionId, session.incarnation});
	}
	TrackHolding(sessionId, session);
}

void LogSummaryIfDue(SteadyTime now)
{
	if (now - tlsLastSummaryAt < kSummaryInterval)
	{
		return;
	}
	tlsLastSummaryAt = now;
	const auto &c = tlsCounters;
	LOG_INFO << "[scene_lane] summary admitted=" << c.admitted << " sent=" << c.sent << " replied=" << c.replied
			 << " failed=" << c.failed << " no_content=" << c.noContent
			 << " dropped_uncorrelated=" << c.droppedUncorrelated << " held=" << c.held
			 << " retry_verdicts=" << c.retryVerdicts << " expired_on_hold=" << c.expiredOnHold
			 << " expired_in_queue=" << c.expiredInQueue << " no_scene=" << c.noScene
			 << " rejected_full=" << c.rejectedFull << " dropped_on_redirect=" << c.droppedOnRedirect
			 << " one_way_direct=" << c.oneWayDirect << " one_way_queued=" << c.oneWayQueued
			 << " one_way_sent=" << c.oneWaySent << " one_way_dropped=" << c.oneWayDropped
			 << " sent_on_new_route=" << c.sentOnNewRoute
			 << " inflight_lost=" << c.inflightLost << " late_dropped=" << c.lateDropped
			 << " in_flight=" << tlsInflight.Size() << " expiry_heap=" << tlsInflight.ExpiryHeapSize()
			 << " holding_sessions=" << tlsHolding.size() << " max_depth_seen=" << c.maxDepthSeen;
}

// 在途调用过了「RPC0 超时 + 余量」仍没有完成回调(RPC0 承诺恰好一次,正常不会发生):判丢。
// 会话还在就回 1003(结果未知,与 RPC0 超时同一口径)并放行后面的消息;会话已关闭就只计数。
// 之后迟到的回调因序号对不上被丢弃(OnSceneCallDone),不会出现第二个回执。
void FailLostCalls(SteadyTime now)
{
	for (const auto &[callSeq, call] : tlsInflight.ExtractExpired(now))
	{
		++tlsCounters.inflightLost;
		LOG_ERROR << "[scene_lane] In-flight scene call got no completion within its timeout plus margin"
				  << " session_id=" << call.sessionId << " call_seq=" << callSeq
				  << " inflight_lost_total=" << tlsCounters.inflightLost;
		SessionInfo *session = gate_lane::FindLiveSession(call.sessionId, call.incarnation);
		if (session == nullptr)
		{
			continue;
		}
		const auto item = session->sceneLane.Complete(callSeq);
		if (!item)
		{
			continue; // 在途索引与 lane 不一致:只可能是缺陷,上面已经留痕
		}
		gate_lane::SendEnvelope(*session, item->clientRequestId, item->messageId, kServiceUnavailable);
		Pump(call.sessionId, *session, now);
	}
}

// 到点重投:只扫队头在等重投的会话。
void RetryHeldSessions(SteadyTime now)
{
	if (tlsHolding.empty())
	{
		return;
	}
	// 先拷出来再逐个处理:Pump 会改登记表。
	const std::vector<std::pair<SessionId, uint64_t>> holding(tlsHolding.begin(), tlsHolding.end());
	auto &sessions = tlsSessionManager.sessions();
	for (const auto &[sessionId, incarnation] : holding)
	{
		const auto it = sessions.find(sessionId);
		if (it == sessions.end() || it->second.incarnation != incarnation)
		{
			// 会话已关闭,或号已被新连接复用(新会话若也在等重投,登记的是它自己的代次,不摘)。
			if (const auto held = tlsHolding.find(sessionId); held != tlsHolding.end() && held->second == incarnation)
			{
				tlsHolding.erase(held);
			}
			continue;
		}
		Pump(sessionId, it->second, now);
	}
}

} // namespace

void SetSceneSender(SceneSendFn fn, std::chrono::milliseconds callTimeout)
{
	gSceneSender = fn;
	gSceneCallTimeout = callTimeout;
}

const SceneLaneCounters &GetCounters()
{
	return tlsCounters;
}

std::size_t HoldingSessionCount()
{
	return tlsHolding.size();
}

std::size_t InFlightCount()
{
	return tlsInflight.Size();
}

void ResetForTesting()
{
	(void)tlsInflight.ExtractAll();
	tlsCounters = SceneLaneCounters{};
	tlsLastSummaryAt = SteadyTime{};
	tlsHolding.clear();
	gSceneSender = nullptr;
	gSceneCallTimeout = std::chrono::milliseconds{0};
}

void Admit(SessionId sessionId, SessionInfo &session, const std::shared_ptr<ClientRequest> &request, bool oneWay,
		   SteadyTime now)
{
	auto &lane = session.sceneLane;

	// 单向消息前面没有还没发出的请求(或队头那条在等重投):不排队,马上发。移动是 gate 最热的上行,
	// 绝大多数时候走这里,不建排队项,与改造前的开销相同。
	if (oneWay && lane.OneWayMaySkipQueue())
	{
		++tlsCounters.oneWayDirect;
		SendOneWay(sessionId, session, *request, /*receivedOnPreviousRoute=*/false);
		return;
	}

	SceneLaneItem item;
	item.request = request;
	item.trace = tracing::Current();
	item.admittedAt = now;
	item.receiptRoute = CurrentSceneRoute(session);
	item.clientRequestId = request->id();
	item.messageId = request->message_id();
	item.oneWay = oneWay;

	if (lane.Admit(std::move(item), kMaxSceneLaneDepth) == AdmitResult::kRejectedFull)
	{
		if (oneWay)
		{
			++tlsCounters.oneWayDropped;
			return;
		}
		// 正常客户端排不到 64 条,只可能是在刷,或 scene 长时间不回;不计非法包(超限本身不是协议错误),留趋势证据。
		++tlsCounters.rejectedFull;
		if ((tlsCounters.rejectedFull & 0x3FF) == 1)
		{
			LOG_WARN << "[scene_lane] Session scene lane is full; rejecting with 1008 (sampled)"
					 << " latest_session_id=" << sessionId << " depth_limit=" << kMaxSceneLaneDepth
					 << " rejected_full_total=" << tlsCounters.rejectedFull;
		}
		gate_lane::SendEnvelope(session, request->id(), request->message_id(), kRateLimitExceeded);
		return;
	}
	if (oneWay)
	{
		++tlsCounters.oneWayQueued;
	}
	else
	{
		++tlsCounters.admitted;
	}
	tlsCounters.maxDepthSeen = (std::max)(tlsCounters.maxDepthSeen, lane.Depth());
	Pump(sessionId, session, now);
}

void OnSceneCallDone(SessionId sessionId, uint64_t incarnation, CallSeq callSeq, const RpcStatus &status,
					 const ProcessClientPlayerMessageResponse *response, SteadyTime now)
{
	// 完成到了就注销在途登记,不管会话还在不在:会话先关闭的调用靠这里注销,不会被当成丢失。
	tlsInflight.Erase(callSeq);

	// 会话按 (session_id, 会话代次) 复核:session_id 被新连接复用之后,旧会话迟到的回包不得写进新会话的 socket。
	SessionInfo *session = gate_lane::FindLiveSession(sessionId, incarnation);
	if (session == nullptr)
	{
		return; // 会话已关闭或号已复用:没有客户端可回(断线后的回包本来就无处可去)
	}
	auto item = session->sceneLane.Complete(callSeq);
	if (!item)
	{
		// 本地已判丢并回过 1003 的调用,回调这时才到:丢弃,保证每个请求恰好一个回执。
		++tlsCounters.lateDropped;
		return;
	}
	const tracing::ScopedSpan itemTrace(item->trace);

	using gate_scene_client_reply::Disposition;
	switch (gate_scene_client_reply::Decide(status, response, item->clientRequestId))
	{
	case Disposition::kForwardContent:
		++tlsCounters.replied;
		if (const auto clientConn = session->conn.lock())
		{
			GetGateCodec().send(clientConn, response->message_content());
		}
		break;
	case Disposition::kRetryLater:
		// scene 明确答复「确定未执行、稍后重投」:玩家归属正在交接,或还没在目标节点就绪。放回队头,等下一次重投。
		HoldOrExpire(sessionId, *session, std::move(*item), now);
		break;
	case Disposition::kServiceUnavailableEnvelope:
		if (!status.ok())
		{
			// 确定未执行(没发出去 / 对端派发前拒绝)或结果未知(超时 / 断连 / 关机):都回带请求 id 的 1003,
			// 客户端不必干等自己的 15s 超时。结果未知时下游可能已执行,有副作用的请求靠业务侧栅栏与幂等兜底,gate 不重投。
			if ((tlsCounters.failed++ & 0x3FF) == 0)
			{
				LOG_WARN << "[scene_lane] Scene call failed (sampled): status=" << status.name()
						 << ", remote_error=" << status.remoteError() << ", message_id=" << item->messageId
						 << ", failed_total=" << tlsCounters.failed;
			}
		}
		else
		{
			// scene 派发前早退、应答里没有 message_content 也没有 RETRY_LATER(docs/design/client-rpc-callback.md §7 R1):
			// 请求确定未执行,而且等也没用(请求解析失败、旧会话、身份栅栏、绑在当前场景上的请求等)。
			// 早退的具体原因在 scene 自己的日志里。
			if ((tlsCounters.noContent++ & 0x3FF) == 0)
			{
				LOG_WARN << "[scene_lane] Scene reply has no message content (sampled): message_id=" << item->messageId
						 << ", no_content_total=" << tlsCounters.noContent;
			}
		}
		gate_lane::SendEnvelope(*session, item->clientRequestId, item->messageId, kServiceUnavailable);
		break;
	case Disposition::kDrop:
		++tlsCounters.droppedUncorrelated;
		break;
	}
	Pump(sessionId, *session, now);
}

void OnSessionRedirected(SessionId sessionId, SessionInfo &session)
{
	// 每条按它收包时的 trace 处置,与 Pump 的回执同一口径。
	for (const auto &item : session.sceneLane.TakeAllQueued())
	{
		const tracing::ScopedSpan itemTrace(item.trace);
		if (item.oneWay)
		{
			++tlsCounters.oneWayDropped;
			continue;
		}
		++tlsCounters.droppedOnRedirect;
		ReplyNotExecuted(session, item);
	}
	TrackHolding(sessionId, session);
}

void Tick(SteadyTime now)
{
	FailLostCalls(now);
	RetryHeldSessions(now);
	LogSummaryIfDue(now);
}

} // namespace gate_scene_lane
