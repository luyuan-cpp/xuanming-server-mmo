#include "client_scene_lane.h"

#include <algorithm>
#include <chrono>
#include <cstdint>
#include <optional>
#include <unordered_map>
#include <utility>
#include <vector>

#include "muduo/base/Logging.h"
#include "client_request_lane.h"
#include "gate_codec.h"
#include "scene_client_call_reply.h"
#include "network/rpc_status.h"
#include "proto/common/base/message.pb.h"
#include "proto/scene/scene.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include <session/manager/session_manager.h>
#include "session/comp/scene_lane.h"
#include "session/comp/scene_lane_item.h"
#include "thread_context/trace_context_tls.h"

namespace gate_scene_lane
{
namespace
{

// 全部只在 gate 唯一的 EventLoop 线程上读写(会话表同为 thread_local)。
thread_local CallSeq tlsLastCallSeq = kNoCallSeq;
thread_local SceneLaneCounters tlsCounters;
thread_local SteadyTime tlsLastSummaryAt{};
// 队头在等重投的会话:session_id → 会话代次。Tick 只扫这张表,不扫全部会话;会话没了或代次变了就摘掉。
// 不存连接、不存指针(AGENTS.md §11.7),每次都按 session_id 重新找会话。
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

// 这一条没被受理、确定未执行(scene 答复稍后重投,或 gate 这边进场转发还欠着):
//   - 单向消息有时效,不等,直接丢;
//   - 从收包起已经等过上限:回执,不再等;
//   - 否则放回队头,等下一次重投;排在它后面的单向消息一并清掉。
void HoldOrExpire(SessionInfo &session, SceneLaneItem item, SteadyTime now)
{
	if (item.oneWay)
	{
		++tlsCounters.droppedOneWay;
		return;
	}
	++tlsCounters.retryVerdicts;
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
	tlsCounters.droppedOneWay += session.sceneLane.HoldForRetry(std::move(item), now + kRetryInterval);
}

// 按收包顺序发出:没有在途、且队头不在等重投时,逐条取出发给 scene。请求-响应类发出一条就停(等它完成);
// 单向消息发出后继续取下一条;没发出的条目当场回执或丢弃。循环次数 ≤ 队长(≤ kMaxSceneLaneDepth)。
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
				++tlsCounters.droppedOneWay;
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
		const CallSeq callSeq = NextCallSeq();
		const SceneSendResult result = gSceneSender != nullptr
										   ? gSceneSender(sessionId, session, *item->request, item->oneWay, callSeq)
										   : SceneSendResult::kNoScene;
		if (result == SceneSendResult::kSent)
		{
			++tlsCounters.sent;
			if (!lane.MarkSent(std::move(*item), callSeq, now + gSceneCallTimeout + kInflightLostMargin))
			{
				// 不变量:TakeNextIfReady 只在没有在途时交出条目,MarkSent 不应失败。
				LOG_ERROR << "[scene_lane] MarkSent refused although the lane was idle; the request will get no reply"
						  << " session_id=" << sessionId << " lane_in_flight=" << lane.InFlightSeq();
			}
			break;
		}
		if (result == SceneSendResult::kSentOneWay)
		{
			++tlsCounters.sentOneWay;
			continue;
		}
		if (result == SceneSendResult::kRetryLater)
		{
			// 放回队头之后 TakeNextIfReady 返回空,循环随即结束;被丢弃或已回执时继续取下一条。
			HoldOrExpire(session, std::move(*item), now);
			continue;
		}
		// kNoScene:会话没有可用的 scene 节点,等也没用。确定未发出,回带请求 id 的 1003;单向消息同样回
		// (与改造前 HandleTcpNodeMessage 的「Scene not ready」一支相同,client-request-ordering.md §4.7)。
		++tlsCounters.noScene;
		gate_lane::SendEnvelope(session, item->clientRequestId, item->messageId, kServiceUnavailable);
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
	LOG_INFO << "[scene_lane] summary admitted=" << c.admitted << " one_way_direct=" << c.oneWayDirect
			 << " sent=" << c.sent << " sent_one_way=" << c.sentOneWay << " replied=" << c.replied
			 << " failed=" << c.failed << " no_content=" << c.noContent
			 << " dropped_uncorrelated=" << c.droppedUncorrelated << " held=" << c.held
			 << " retry_verdicts=" << c.retryVerdicts << " expired_on_hold=" << c.expiredOnHold
			 << " expired_in_queue=" << c.expiredInQueue << " no_scene=" << c.noScene
			 << " rejected_full=" << c.rejectedFull << " dropped_one_way=" << c.droppedOneWay
			 << " inflight_lost=" << c.inflightLost << " late_dropped=" << c.lateDropped
			 << " holding_sessions=" << tlsHolding.size() << " max_depth_seen=" << c.maxDepthSeen;
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

void ResetForTesting()
{
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

	// 纵深防御:在途调用过了「RPC0 超时 + 余量」仍没有完成回调(RPC0 承诺恰好一次,正常不会发生)。判丢并回 1003
	// (结果未知,与 RPC0 超时同一口径),否则这条会话的 scene 请求会一直卡在它后面。客户端还在发消息才会走到这里;
	// 不发消息的连接由心跳判死、断线后 lane 随会话销毁。
	if (auto lost = lane.ExpireInFlightIfLost(now))
	{
		++tlsCounters.inflightLost;
		LOG_ERROR << "[scene_lane] In-flight scene call got no completion within its timeout plus margin; failing it with 1003"
				  << " session_id=" << sessionId << " message_id=" << lost->messageId
				  << " inflight_lost_total=" << tlsCounters.inflightLost;
		gate_lane::SendEnvelope(session, lost->clientRequestId, lost->messageId, kServiceUnavailable);
	}

	// 快路径:lane 完全空(没有在途、没有排队)时,单向消息照旧直接发出,不建排队项。移动是 gate 最热的上行,
	// 这里不能多出一次入队出队。
	if (oneWay && lane.Empty())
	{
		const SceneSendResult result = gSceneSender != nullptr
										   ? gSceneSender(sessionId, session, *request, true, kNoCallSeq)
										   : SceneSendResult::kNoScene;
		if (result == SceneSendResult::kSentOneWay)
		{
			++tlsCounters.oneWayDirect;
		}
		else if (result == SceneSendResult::kNoScene)
		{
			++tlsCounters.noScene;
			gate_lane::SendEnvelope(session, request->id(), request->message_id(), kServiceUnavailable);
		}
		else
		{
			// 进场转发还欠着:单向消息有时效,不等。
			++tlsCounters.droppedOneWay;
		}
		return;
	}

	SceneLaneItem item;
	item.request = request;
	item.trace = tracing::Current();
	item.admittedAt = now;
	item.clientRequestId = request->id();
	item.messageId = request->message_id();
	item.oneWay = oneWay;

	switch (lane.Admit(std::move(item), kMaxSceneLaneDepth))
	{
	case AdmitResult::kQueued:
		++tlsCounters.admitted;
		tlsCounters.maxDepthSeen = (std::max)(tlsCounters.maxDepthSeen, lane.Depth());
		break;
	case AdmitResult::kRejectedFull:
		if (oneWay)
		{
			++tlsCounters.droppedOneWay;
			break;
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
		break;
	case AdmitResult::kDroppedOneWayOnHold:
		++tlsCounters.droppedOneWay;
		break;
	}
	// 三种结果都放行一次:上面判丢之后 lane 可能刚空出来。
	Pump(sessionId, session, now);
}

void OnSceneCallDone(SessionId sessionId, uint64_t incarnation, CallSeq callSeq, const RpcStatus &status,
					 const ProcessClientPlayerMessageResponse *response, SteadyTime now)
{
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
		HoldOrExpire(*session, std::move(*item), now);
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
			// 请求确定未执行,而且等也没用(请求解析失败、旧会话、身份栅栏等)。早退的具体原因在 scene 自己的日志里。
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

void Tick(SteadyTime now)
{
	if (!tlsHolding.empty())
	{
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
	LogSummaryIfDue(now);
}

} // namespace gate_scene_lane
