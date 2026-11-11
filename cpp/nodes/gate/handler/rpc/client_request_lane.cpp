#include "client_request_lane.h"

#include <algorithm>
#include <chrono>
#include <cstdint>
#include <optional>
#include <sstream>
#include <string>
#include <utility>

#include "muduo/base/Logging.h"
#include "gate_codec.h"
#include "login_admission.h"
#include "session_details_builder.h"
#include "error_reporter/error_reporter.h"
#include "node/system/grpc_call_deadline.h"
#include "proto/common/base/message.pb.h"
#include "proto/common/base/node.pb.h"
#include "proto/common/base/session.pb.h"
#include "rpc/service_metadata/login_service_metadata.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/login_error_tip.pb.h"
#include <session/manager/session_manager.h>
#include "session/comp/router_lane.h"
#include "session/comp/router_lane_item.h"
#include "thread_context/trace_context_tls.h"

namespace gate_lane
{
namespace
{

// 在途索引的值。只存定位与回执所需的数据,不存连接(AGENTS.md §11.7)。
struct InflightCall
{
	SteadyTime expiresAt;
	SessionId sessionId{0};
	uint64_t incarnation{0};
	uint64_t clientRequestId{0};
	uint32_t messageId{0};
};

// 全部只在 gate 唯一的 EventLoop 线程上读写(会话表同为 thread_local,client_session_handler.cpp:542-546)。
thread_local InflightIndex<InflightCall> tlsInflight;
thread_local CallSeq tlsLastCallSeq = kNoCallSeq;
thread_local uint64_t tlsLastIncarnation = 0;
thread_local LaneCounters tlsCounters;
thread_local SteadyTime tlsLastSummaryAt{};
// 启动时注入一次(SetRouterSender / SetSendHooks),之后只读。
RouterSendFn gRouterSender = nullptr;
BeforeSendFn gBeforeSend = nullptr;
AfterSendFn gAfterSend = nullptr;

constexpr std::chrono::seconds kSummaryInterval{30};

CallSeq NextCallSeq()
{
	return ++tlsLastCallSeq;
}

} // namespace

// 以下两个函数导出给 scene 的 TCP(RPC0)回包路径共用(client_request_lane.h;docs/design/muduo-rpc-channel.md §8.1),
// 定义仍留在这里,紧挨着它们用到的计数。

// 带请求 id 与原消息号的错误信封(与路由服的拒绝信封同形,客户端按 id 对上请求)。连接已释放就不发。
void SendEnvelope(SessionInfo &session, uint64_t clientRequestId, uint32_t messageId, uint32_t tipId)
{
	const auto conn = session.conn.lock();
	if (!conn)
	{
		return;
	}
	MessageContent envelope;
	envelope.set_id(clientRequestId);
	envelope.set_message_id(messageId);
	envelope.mutable_error_message()->set_id(tipId);
	GetGateCodec().send(conn, envelope);
}

// 按 session_id 找会话并核对会话代次:session_id 被新连接复用之后,旧会话迟到的完成不得落到新会话上(F47)。
SessionInfo *FindLiveSession(SessionId sessionId, uint64_t incarnation)
{
	auto &sessions = tlsSessionManager.sessions();
	const auto it = sessions.find(sessionId);
	if (it == sessions.end())
	{
		return nullptr;
	}
	if (it->second.incarnation != incarnation)
	{
		++tlsCounters.incarnationMismatch;
		return nullptr;
	}
	return &it->second;
}

namespace
{

// 发出前身份复核失配的采样留痕。客户端驱动(离场 / 换角之后还有排在前面受理的请求),不是故障;不带 player_id(AGENTS.md §9)。
void LogStaleIdentitySampled(SessionId sessionId, uint32_t messageId)
{
	if ((tlsCounters.rejectedStaleIdentity & 0x3FF) == 1)
	{
		LOG_WARN << "[lane] Queued request no longer matches the session's authenticated binding; rejecting with "
					"kLoginSessionNotFound (sampled)"
				 << " latest_session_id=" << sessionId << " latest_message_id=" << messageId
				 << " rejected_stale_identity_total=" << tlsCounters.rejectedStaleIdentity;
	}
}

// lane 空闲时按收包顺序发出下一条。没发出的条目(排队超时、登录闸、身份复核、发出钩子拒绝、没有路由服、调用没构造出来)
// 当场回带请求 id 的信封并继续取下一条,不登记在途;循环次数 ≤ 队长(≤ 32)。
void Pump(SessionId sessionId, SessionInfo &session)
{
	const SteadyTime now = std::chrono::steady_clock::now();
	auto &lane = session.routerLane;
	while (auto item = lane.TakeNextIfIdle())
	{
		// 每条按它自己入队时的 trace 处理:Pump 可能在上一条的完成回调或过期扫描里跑,那时线程上是别的上下文
		// (docs/design/distributed-tracing.md §5.5)。回给客户端的拒绝信封和发出的 gRPC 都落在这条请求的 trace 上。
		const tracing::ScopedSpan itemTrace(item->trace);
		if (QueueWaitExceeded(item->enqueuedAt, now, kMaxLaneWait))
		{
			++tlsCounters.expiredInQueue;
			SendEnvelope(session, item->clientRequestId, item->messageId, kServiceUnavailable);
			continue;
		}
		// 本连接没有成功 Login 就不发 CreatePlayer / EnterGame / LeaveGame(login_admission.h)。确定未执行,
		// 回与 login 同一个「未登录」码,客户端照旧回登录。在发出时判、不在准入时判:连发的 Login → EnterGame 里,
		// EnterGame 轮到发出时 Login 的回包已经由 OnRouterReply 记过结果。
		if (gate_login_admission::RequiresSuccessfulLogin(item->messageId) && !session.loginSucceeded)
		{
			++tlsCounters.rejectedNotLoggedIn;
			SendEnvelope(session, item->clientRequestId, item->messageId, kLoginSessionNotFound);
			continue;
		}
		// 入队快照只能在原身份仍有效时签发:离场、换角或换连接之后失配,确定未执行,回与 login 同一个「未登录」码。
		// 解析失败(本模块自己序列化的字节,不应发生)同样按失配处理,fail-closed。
		SessionDetails admitted;
		if (!admitted.ParseFromString(item->sessionDetailsBin) ||
			!gate_session_details::MatchesAuthenticatedBinding(admitted, session))
		{
			++tlsCounters.rejectedStaleIdentity;
			LogStaleIdentitySampled(sessionId, item->messageId);
			SendEnvelope(session, item->clientRequestId, item->messageId, kLoginSessionNotFound);
			continue;
		}
		// 发出钩子(前):EnterGame 换角闸等按此刻的会话状态判 —— 排在它前面的 LeaveGame 已经发出并生效。
		if (gBeforeSend != nullptr)
		{
			if (const uint32_t rejectTip = gBeforeSend(sessionId, session, *item->request); rejectTip != 0)
			{
				++tlsCounters.rejectedBeforeSend;
				SendEnvelope(session, item->clientRequestId, item->messageId, rejectTip);
				continue;
			}
		}
		const RouterSendResult sendResult = gRouterSender != nullptr
												? gRouterSender(*item->request, item->sessionDetailsBin)
												: RouterSendResult::kNoRouter;
		if (sendResult == RouterSendResult::kNoRouter)
		{
			// 本地没有任何路由服(启动窗口 / 全部下线);发送函数已按 1024 采样留痕。
			++tlsCounters.noRouter;
			SendEnvelope(session, item->clientRequestId, item->messageId, kServiceUnavailable);
			continue;
		}
		if (sendResult == RouterSendResult::kCallNotBuilt)
		{
			// 调用没构造出来(签名失败等),通道没被碰过,不会有任何完成回调;发送函数已采样留痕。确定未执行。
			++tlsCounters.callNotBuilt;
			SendEnvelope(session, item->clientRequestId, item->messageId, kServiceUnavailable);
			continue;
		}
		lane.MarkSent(item->seq);
		const auto deadline = grpc_call_deadline::Get(common::base::eNodeType::ClientRpcRouterNodeService);
		tlsInflight.Insert(item->seq, InflightCall{
										  now + deadline + kInflightExpiryMargin,
										  sessionId,
										  session.incarnation,
										  item->clientRequestId,
										  item->messageId,
									  });
		// 确实发出过 Login.Login 的会话,断开时才需要通知 Login 清理 session-id 状态。在发出时置位、不在入队时:
		// 排队中、断线时作废的 Login 从没到达 login,入队就置位会让「发 Login 后立刻断开」放大断线 RPC。
		if (item->messageId == ClientPlayerLoginLoginMessageId)
		{
			session.loginStarted = true;
		}
		// 发出钩子(后):LeaveGame 的退出实体与清路由、EnterGame 的「离场后已再请求进场」都在这一刻按收包顺序生效。
		// 钩子之后 lane 已不空闲(这条在途),循环随即结束,排在后面的请求等它完成后再按新的会话状态复核。
		if (gAfterSend != nullptr)
		{
			gAfterSend(sessionId, session, *item->request);
		}
		++tlsCounters.sent;
	}
}

// 失败类完成(gRPC 失败,含路由服节点摘除 / 本地截止)的共同收尾:核对会话、认领队头、回 1003、放行下一条。
void FinishWithFailure(CallSeq seq, const InflightCall &call)
{
	SessionInfo *session = FindLiveSession(call.sessionId, call.incarnation);
	if (session == nullptr)
	{
		return;
	}
	if (!session->routerLane.Complete(seq))
	{
		// 不变量:在途索引里的序号必然是该会话 lane 的在途序号(Insert 只发生在 MarkSent 之后、断线时同步注销)。
		LOG_ERROR << "[lane] In-flight call does not match the session lane head; dropping completion"
				  << " session_id=" << call.sessionId << " seq=" << seq
				  << " lane_in_flight=" << session->routerLane.InFlightSeq();
		return;
	}
	SendEnvelope(*session, call.clientRequestId, call.messageId, kServiceUnavailable);
	Pump(call.sessionId, *session);
}

// 认领一次完成:恰好一次(extract)。取不到 = 已过期 / 已失败 / 断线时已注销,调用方丢弃这次完成。
// 正常运行时 lateDropped 只来自「本地截止先到、通道回调后到」与断线后的迟到完成;路由服摘除不再计入(W1-45)。
std::optional<InflightCall> Claim(CallSeq seq)
{
	auto call = tlsInflight.Extract(seq);
	if (!call)
	{
		++tlsCounters.lateDropped;
	}
	return call;
}

void LogSummaryIfDue(SteadyTime now)
{
	if (now - tlsLastSummaryAt < kSummaryInterval)
	{
		return;
	}
	tlsLastSummaryAt = now;
	const auto &c = tlsCounters;
	LOG_INFO << "[lane] summary admitted=" << c.admitted << " sent=" << c.sent << " replied=" << c.replied
			 << " failed=" << c.failed << " expired=" << c.expired
			 << " expired_in_queue=" << c.expiredInQueue << " no_router=" << c.noRouter
			 << " call_not_built=" << c.callNotBuilt
			 << " rejected_full=" << c.rejectedFull << " rejected_redirected=" << c.rejectedRedirected
			 << " late_dropped=" << c.lateDropped << " incarnation_mismatch=" << c.incarnationMismatch
			 << " dropped_on_close=" << c.droppedOnClose << " dropped_on_redirect=" << c.droppedOnRedirect
			 << " rejected_not_logged_in=" << c.rejectedNotLoggedIn
			 << " rejected_stale_identity=" << c.rejectedStaleIdentity
			 << " rejected_before_send=" << c.rejectedBeforeSend
			 << " in_flight=" << tlsInflight.Size() << " expiry_heap=" << tlsInflight.ExpiryHeapSize()
			 << " max_depth_seen=" << c.maxDepthSeen;
}

} // namespace

uint64_t NextIncarnation()
{
	return ++tlsLastIncarnation;
}

void SetRouterSender(RouterSendFn fn)
{
	gRouterSender = fn;
}

void SetSendHooks(BeforeSendFn beforeSend, AfterSendFn afterSend)
{
	gBeforeSend = beforeSend;
	gAfterSend = afterSend;
}

bool HasQueuedRequest(const SessionInfo &session, uint32_t messageId)
{
	return session.routerLane.AnyQueued([messageId](const RouterLaneItem &item) { return item.messageId == messageId; });
}

const LaneCounters &GetCounters()
{
	return tlsCounters;
}

void ResetForTesting()
{
	(void)tlsInflight.ExtractAll();
	tlsCounters = LaneCounters{};
	tlsLastSummaryAt = SteadyTime{};
	gRouterSender = nullptr;
	gBeforeSend = nullptr;
	gAfterSend = nullptr;
}

void Admit(SessionId sessionId, SessionInfo &session, const std::shared_ptr<ClientRequest> &request,
		   SessionDetails details)
{
	const uint64_t clientRequestId = request->id();
	const uint32_t messageId = request->message_id();

	// details 已在 BuildSessionDetails 归一过 player_id(D60 的唯一构造点);这里复用同一规则再归一一次只作纵深防御,
	// 规则本身是幂等的,不会改写已绑定的角色。
	details.set_player_id(gate_session_details::NormalizePlayerId(details.player_id()));
	// 前面还排着没发出的 LeaveGame:这条要等它发出之后才轮到,那时 LeaveGame 的发出钩子已经清掉原账号与原修订
	// (client_session_handler.cpp ApplyLoginTransition → scene_route_helper.h ResetSceneRoute)。快照取「客户端发这条请求时」
	// 的身份(§4.3),而客户端发它时已经请求离开,所以按离开之后的身份取。不这样做,发出前的身份复核必然失配,登出之后
	// 紧跟的 Login 会被误回「未登录」;在线域请求照旧由下游按缺原身份拒绝,与 LeaveGame 已经生效时完全相同。
	// 只清不补(fail-closed);清掉的字段与 ResetSceneRoute 清掉的身份字段一一对应,改那里时同改这里。
	if (HasQueuedRequest(session, ClientPlayerLoginLeaveGameMessageId))
	{
		details.clear_authenticated_account();
		details.set_owner_revision(0);
	}
	details.set_session_incarnation(session.incarnation);
	const CallSeq seq = NextCallSeq();
	details.set_gate_call_seq(seq);

	RouterLaneItem item;
	item.request = request;
	item.sessionDetailsBin = details.SerializeAsString();
	item.trace = tracing::Current();
	item.seq = seq;
	item.enqueuedAt = std::chrono::steady_clock::now();
	item.clientRequestId = clientRequestId;
	item.messageId = messageId;

	switch (session.routerLane.Admit(std::move(item), kMaxLaneDepth))
	{
	case AdmitResult::kQueued:
		break;
	case AdmitResult::kRejectedFull:
		// 正常客户端按功能单飞,排到 32 条只可能是在刷;不计非法包(超限本身不是协议错误),但要留趋势证据。
		++tlsCounters.rejectedFull;
		if ((tlsCounters.rejectedFull & 0x3FF) == 1)
		{
			LOG_WARN << "[lane] Session router lane is full; rejecting with 1008 (sampled)"
					 << " latest_session_id=" << sessionId << " depth_limit=" << kMaxLaneDepth
					 << " rejected_full_total=" << tlsCounters.rejectedFull;
		}
		SendEnvelope(session, clientRequestId, messageId, kRateLimitExceeded);
		return;
	case AdmitResult::kRejectedRedirected:
		++tlsCounters.rejectedRedirected;
		SendEnvelope(session, clientRequestId, messageId, kServiceUnavailable);
		return;
	}
	++tlsCounters.admitted;
	tlsCounters.maxDepthSeen = (std::max)(tlsCounters.maxDepthSeen, session.routerLane.Depth());
	Pump(sessionId, session);
}

bool OnRouterReply(const SessionDetails &echoed, const ::google::protobuf::Message &reply)
{
	const CallSeq seq = echoed.gate_call_seq();
	if (seq == kNoCallSeq)
	{
		return false;
	}
	const auto call = Claim(seq);
	if (!call)
	{
		// 这个请求已经有过回执(本地截止 / 失败),再下发就是同一请求两个回执。
		return true;
	}
	SessionInfo *session = FindLiveSession(call->sessionId, call->incarnation);
	if (session == nullptr)
	{
		return true;
	}
	if (!session->routerLane.Complete(seq))
	{
		LOG_ERROR << "[lane] Reply does not match the session lane head; dropping"
				  << " session_id=" << call->sessionId << " seq=" << seq
				  << " lane_in_flight=" << session->routerLane.InFlightSeq();
		return true;
	}
	++tlsCounters.replied;
	// 先记 Login 结果,再放行下一条:同一会话里排在 Login 后面的 CreatePlayer / EnterGame 在 Pump 时要看到它。
	// 会话已按代次核对过(FindLiveSession),旧连接迟到的 Login 回包不会记到撞号的新连接上。
	if (call->messageId == ClientPlayerLoginLoginMessageId && gate_login_admission::IsLoginReplySuccess(reply))
	{
		session->loginSucceeded = true;
	}
	if (const auto conn = session->conn.lock())
	{
		GetGateCodec().send(conn, reply);
	}
	Pump(call->sessionId, *session);
	return true;
}

bool OnRouterCallFailed(const SessionDetails &sent)
{
	const CallSeq seq = sent.gate_call_seq();
	if (seq == kNoCallSeq)
	{
		return false;
	}
	const auto call = Claim(seq);
	if (!call)
	{
		return true;
	}
	++tlsCounters.failed;
	FinishWithFailure(seq, *call);
	return true;
}

void ExpireDue(std::chrono::steady_clock::time_point now)
{
	auto expired = tlsInflight.ExtractExpired(now);
	if (!expired.empty())
	{
		tlsCounters.expired += expired.size();
		// 本地截止到期说明这次调用的完成没被认领:传输层回调丢了(「恰好一次」契约出缺陷;第 3 批前的 F29、
		// CQ 轮询 !ok 两个来源已随官方 CQ 删除),或路由服回显缺失 / 解析失败 / 不带 gate_call_seq(版本不符,
		// 对照 router_muduo_forward.cpp 的「no x-session-detail-bin echo」WARN)。正常运行时不该出现。
		std::ostringstream message;
		message << "in_flight_expired=" << expired.size() << " total=" << tlsCounters.expired;
		error_reporter::Record(kServiceUnavailable, "router_lane_expired", message.str());
		LOG_WARN << "[lane] In-flight router calls passed their local deadline without any callback; failing them with 1003"
				 << " count=" << expired.size() << " expired_total=" << tlsCounters.expired;
		for (const auto &[seq, call] : expired)
		{
			FinishWithFailure(seq, call);
		}
	}
	LogSummaryIfDue(now);
}

void OnSessionClosing(SessionInfo &session)
{
	auto &lane = session.routerLane;
	if (lane.InFlightSeq() != kNoCallSeq)
	{
		tlsInflight.Erase(lane.InFlightSeq());
	}
	tlsCounters.droppedOnClose += lane.DropQueued();
}

void OnSessionRedirected(SessionInfo &session)
{
	// 被丢的排队项从未发出,确定未执行:逐条回带请求 id 与原 message_id 的 1003(O4 每个请求恰好一个回执),客户端到新 gate
	// 上重试是安全的。以前只清队列不回执:旧连接最长还活 60 s(drain_migration.h kRedirectLinger),客户端只能干等 15 s 超时。
	// 每条按它入队时的 trace 回执,与 Pump 的拒绝信封同一口径。
	const auto dropped = session.routerLane.MarkRedirected();
	tlsCounters.droppedOnRedirect += dropped.size();
	for (const auto &item : dropped)
	{
		const tracing::ScopedSpan itemTrace(item.trace);
		SendEnvelope(session, item.clientRequestId, item.messageId, kServiceUnavailable);
	}
}

} // namespace gate_lane
