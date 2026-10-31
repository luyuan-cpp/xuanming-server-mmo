#include "gate_push_delivery.h"

#include <array>
#include <cassert>
#include <cstddef>

#include "muduo/base/Logging.h"
#include "muduo/net/Buffer.h"

#include "gate_codec.h"
#include "network/broadcast_target_codec.h"
#include "proto/common/base/message.pb.h"
#include "proto/contracts/kafka/gate_event.pb.h"
#include "proto/gate/gate_service.pb.h"
#include <session/manager/session_manager.h>
#include <session/system/session_identity_fence.h>

namespace gate_push
{
	namespace
	{
		constexpr std::size_t kPathCount = static_cast<std::size_t>(Path::kCount);

		thread_local std::array<DeliveryCounters, kPathCount> tlsCounters{};
		thread_local std::array<bool, kPathCount> tlsUnfencedLogged{};

		std::size_t IndexOf(Path path)
		{
			assert(path < Path::kCount);
			return static_cast<std::size_t>(path);
		}

		DeliveryCounters &CountersOf(Path path)
		{
			return tlsCounters[IndexOf(path)];
		}

		const char *PathName(Path path)
		{
			switch (path)
			{
			case Path::kRpc:
				return "rpc";
			case Path::kKafka:
				return "kafka";
			case Path::kCount:
				break;
			}
			return "unknown";
		}

		// metric= 行的 reason 标签。封闭枚举,保证标签低基数。
		enum class DropReason : uint8_t
		{
			kFence,
			kInvalid,
			kScene0,
			kCount,
		};

		const char *DropReasonName(DropReason reason)
		{
			switch (reason)
			{
			case DropReason::kFence:
				return "fence";
			case DropReason::kInvalid:
				return "invalid";
			case DropReason::kScene0:
				return "scene0";
			case DropReason::kCount:
				break;
			}
			return "unknown";
		}

		// 本入口的全部计数,附在采样明细行末尾:推送计数还没有登记进节点 /metrics,
		// 这是它们目前唯一的出口,一行就能看出丢弃在总量里的占比。
		muduo::LogStream &operator<<(muduo::LogStream &stream, const DeliveryCounters &counters)
		{
			stream << "delivered=" << counters.delivered << " unfenced=" << counters.deliveredUnfenced
				   << " dropped_fence=" << counters.droppedFence << " session_not_found=" << counters.sessionNotFound
				   << " no_connection=" << counters.noConnection << " invalid=" << counters.invalidMessage
				   << " scene0=" << counters.droppedScene0;
			return stream;
		}

		// 丢弃类采样:第一条必打,此后每 1024 条一行(与 client_request_lane.cpp rejected_full 同一口径)。
		// 坏生产者或会话抖动下逐条打会把 gate 日志打爆(曾因逐条 WARN 涨到 ~2GB,并偷走 IO 线程 CPU)。
		bool IsSampled(uint64_t total)
		{
			return (total & 0x3FF) == 1;
		}

		// metric= 行只带低基数的 path / reason 与累计值,日志侧据此提取计数;会话号、玩家号、消息号
		// 只进调用方紧接着打的明细行,不进 metric= 行,免得被当成指标标签。
		void LogDropMetric(Path path, DropReason reason, uint64_t total)
		{
			LOG_WARN << "metric=gate_push_dropped path=" << PathName(path) << " reason=" << DropReasonName(reason)
					 << " total=" << total;
		}

		// 身份栅栏失配(R13 / R14):这是真正要报警的那一条 —— 要么 routing node_id 被复用后发送方反查到了
		// 继任 gate,要么单个 gate 内 session 序号回绕后同号 session 已易主,两种都意味着这条推送本来会写进
		// 别人的 socket。明细带上两个玩家 id 才能事后判断是哪一种。调用方已计数。
		void LogFenceMismatch(Path path, uint32_t sessionId, uint64_t sessionPlayerId, uint64_t targetPlayerId,
							  uint32_t messageId)
		{
			const auto &counters = CountersOf(path);
			if (!IsSampled(counters.droppedFence))
			{
				return;
			}
			LogDropMetric(path, DropReason::kFence, counters.droppedFence);
			LOG_WARN << "Gate push dropped: target player mismatch (sampled). path=" << PathName(path)
					 << " session_id=" << sessionId << " session_player_id=" << sessionPlayerId
					 << " target_player_id=" << targetPlayerId << " message_id=" << messageId << " " << counters;
		}

		// 兼容位:发送方没带目标玩家(0)。每条入口每线程只记一次 INFO —— 迁移窗口里这条对每一条推送都成立,
		// 逐条打就是把 gate 日志打爆。看到它 = 该入口有生产者没填目标玩家,或 login 发了 Bind 之前的失败提示(显式 0);
		// 此后的条数见采样明细里的 unfenced。
		void LogFenceUnfencedOnce(Path path)
		{
			auto &logged = tlsUnfencedLogged[IndexOf(path)];
			if (logged)
			{
				return;
			}
			logged = true;
			LOG_INFO << "Gate push without target player id (legacy sender), path=" << PathName(path)
					 << ". The identity fence is inactive for such pushes; this is logged once per path and thread. "
					 << "See docs/design/routing-identity-audit-20260908.md R13.";
		}

		// 会话还在、连接却已释放:会话在 DOWN 回调里同步摘除,稳态下 lock() 必成功,出现即是摘除路径的缺陷。
		// 调用方已计数;采样打 ERROR。
		void LogNoConnection(Path path, uint32_t sessionId, uint32_t messageId)
		{
			const auto &counters = CountersOf(path);
			if (!IsSampled(counters.noConnection))
			{
				return;
			}
			LOG_ERROR << "Gate push dropped: session has no connection (sampled). path=" << PathName(path)
					  << " session_id=" << sessionId << " message_id=" << messageId << " " << counters;
		}

		// 单播 / 场景 / 全服推送缺 message_content:照发就是给客户端推一个空帧(§14 顺带发现的同一类问题)。
		// 整条拒绝,计一次。kind 来自进程内常量。
		void RejectMissingContent(Path path, const char *kind)
		{
			auto &counters = CountersOf(path);
			++counters.invalidMessage;
			if (!IsSampled(counters.invalidMessage))
			{
				return;
			}
			LogDropMetric(path, DropReason::kInvalid, counters.invalidMessage);
			LOG_WARN << "Gate push dropped: " << kind << " without message_content (sampled). path=" << PathName(path)
					 << " " << counters;
		}

		// 一个指名目标:查会话 → lock 连接 → 身份栅栏。可写时返回锁住的连接,否则计数并返回空。
		// 强引用只活在本次投递内,不留存(AGENTS.md §11.7)。顺序沿用原 RPC 实现:先确认会话与连接都在,再比对玩家。
		muduo::net::TcpConnectionPtr AcquireFencedConnection(Path path, uint32_t sessionId, uint64_t targetPlayerId,
															  uint32_t messageId)
		{
			auto &counters = CountersOf(path);
			const auto &sessions = tlsSessionManager.sessions();
			const auto it = sessions.find(sessionId);
			if (it == sessions.end())
			{
				// 断线竞态的常态:发送方推送时会话已在本 gate 关闭(例如同帧 AOI 广播里刚断线的会话)。
				// LOG_DEBUG(按级别过滤):逐条 WARN 曾在会话抖动下把 gate 日志打到 ~2GB。
				++counters.sessionNotFound;
				LOG_DEBUG << "Gate push: session not found (disconnect race), path=" << PathName(path)
						  << " session_id=" << sessionId << " message_id=" << messageId;
				return nullptr;
			}
			const SessionInfo &session = it->second;
			auto conn = session.conn.lock();
			if (!conn)
			{
				++counters.noConnection;
				LogNoConnection(path, sessionId, messageId);
				return nullptr;
			}
			switch (gate_session_fence::ClassifyPush(session.playerId, targetPlayerId))
			{
			case gate_session_fence::PushVerdict::kDrop:
				++counters.droppedFence;
				LogFenceMismatch(path, sessionId, session.playerId, targetPlayerId, messageId);
				return nullptr;
			case gate_session_fence::PushVerdict::kDeliverUnfenced:
				++counters.deliveredUnfenced;
				LogFenceUnfencedOnce(path);
				return conn;
			case gate_session_fence::PushVerdict::kDeliver:
				++counters.delivered;
				return conn;
			}
			return nullptr;
		}

		// 指名广播整条不合法:缺 message_content(会给每个目标推一个空帧),或 player_list 非空却与目标数
		// 不齐(无法按位置对应,逐项比对会把每个人的身份比成别人的)。整条拒绝,计一次。
		template <typename BroadcastMsg>
		void RejectInvalidTargets(Path path, const char *problem, const BroadcastMsg &msg)
		{
			auto &counters = CountersOf(path);
			++counters.invalidMessage;
			if (!IsSampled(counters.invalidMessage))
			{
				return;
			}
			LogDropMetric(path, DropReason::kInvalid, counters.invalidMessage);
			LOG_WARN << "Gate push dropped: invalid targeted broadcast, " << problem << " (sampled). path="
					 << PathName(path) << " message_id=" << msg.message_content().message_id()
					 << " target_count=" << broadcast_targets::TargetCount(msg)
					 << " player_list_size=" << msg.player_list_size() << " " << counters;
		}

		template <typename BroadcastMsg>
		void DeliverToTargetsImpl(Path path, const BroadcastMsg &msg)
		{
			if (!msg.has_message_content())
			{
				RejectInvalidTargets(path, "missing message_content", msg);
				return;
			}
			if (!broadcast_targets::PlayerListAligned(msg))
			{
				RejectInvalidTargets(path, "player_list does not match the target count", msg);
				return;
			}
			const auto &content = msg.message_content();
			const EncodedFrame frame(content);
			// (session_id, player_id) 的解码与顺序契约收在 broadcast_target_codec.h,与 scene / Go 的编码共用(R13)。
			broadcast_targets::ForEach(msg,
									   [&](uint32_t sessionId, uint64_t targetPlayerId)
									   {
										   if (const auto conn = AcquireFencedConnection(path, sessionId, targetPlayerId,
																						 content.message_id()))
										   {
											   frame.SendTo(conn);
										   }
									   });
		}

		// 已绑定玩家:BindSession 之前 playerId 是 kInvalidGuid;0 同样按未绑定处理(与 client_session_handler.cpp 同一判据)。
		bool IsBoundToPlayer(const SessionInfo &session)
		{
			return session.playerId != kInvalidGuid && session.playerId != 0;
		}

		// 场景 / 全服广播:没有目标玩家可比对,不过身份栅栏;只推已绑定玩家的会话(未登录的连接不收游戏广播)。
		// 遍历中写连接是安全的:muduo 的 send 不会同步触发断开回调(高水位回调也是排队执行),会话表不会在遍历中被改。
		template <typename SessionFilter>
		void FanOutToBoundSessions(Path path, const MessageContent &content, SessionFilter &&wanted)
		{
			auto &counters = CountersOf(path);
			const EncodedFrame frame(content);
			for (const auto &[sessionId, session] : tlsSessionManager.sessions())
			{
				if (!IsBoundToPlayer(session) || !wanted(session))
				{
					continue;
				}
				const auto conn = session.conn.lock();
				if (!conn)
				{
					++counters.noConnection;
					LogNoConnection(path, sessionId, content.message_id());
					continue;
				}
				++counters.delivered;
				frame.SendTo(conn);
			}
		}
	} // namespace

	const DeliveryCounters &Counters(Path path)
	{
		return CountersOf(path);
	}

	void DeliverToSession(Path path, uint32_t sessionId, uint64_t targetPlayerId, const MessageContent *content)
	{
		if (content == nullptr)
		{
			RejectMissingContent(path, "unicast");
			return;
		}
		if (const auto conn = AcquireFencedConnection(path, sessionId, targetPlayerId, content->message_id()))
		{
			// 单播只有一个连接,没有可复用的字节,照常经 gate codec 编码发送。
			GetGateCodec().send(conn, *content);
		}
	}

	void DeliverToTargets(Path path, const BroadcastToPlayersRequest &request)
	{
		DeliverToTargetsImpl(path, request);
	}

	void DeliverToTargets(Path path, const contracts::kafka::BroadcastToPlayersEvent &event)
	{
		DeliverToTargetsImpl(path, event);
	}

	void DeliverToScene(Path path, uint64_t sceneId, const MessageContent *content)
	{
		if (content == nullptr)
		{
			RejectMissingContent(path, "scene broadcast");
			return;
		}
		if (sceneId == 0)
		{
			auto &counters = CountersOf(path);
			++counters.droppedScene0;
			if (IsSampled(counters.droppedScene0))
			{
				LogDropMetric(path, DropReason::kScene0, counters.droppedScene0);
				LOG_WARN << "Gate push dropped: scene broadcast without scene_id (sampled). path=" << PathName(path)
						 << " message_id=" << content->message_id() << " " << counters;
			}
			return;
		}
		FanOutToBoundSessions(path, *content,
							  [sceneId](const SessionInfo &session) { return session.sceneId == sceneId; });
	}

	void DeliverToAll(Path path, const MessageContent *content)
	{
		if (content == nullptr)
		{
			RejectMissingContent(path, "broadcast to all");
			return;
		}
		FanOutToBoundSessions(path, *content, [](const SessionInfo &) { return true; });
	}
} // namespace gate_push
