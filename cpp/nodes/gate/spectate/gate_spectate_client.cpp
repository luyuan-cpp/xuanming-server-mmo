#include "gate_spectate_client.h"

#include <chrono>
#include <cstddef>
#include <cstdint>
#include <utility>

#include "muduo/base/Logging.h"
#include "muduo/net/EventLoop.h"
#include "muduo/net/TcpConnection.h"

#include "gate_codec.h"
#include "gate_spectate_hub.h"
#include "gate_spectate_ticket.h"
#include "handler/gate_push_delivery.h"
#include "handler/rpc/client_request_lane.h"
#include "proto/common/base/message.pb.h"
#include "proto/contracts/kafka/gate_event.pb.h"
#include "proto/gate/gate_client_spectate.pb.h"
#include "rpc/service_metadata/gate_client_spectate_service_metadata.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/proto/tip/match_error_tip.pb.h"
#include <session/manager/session_manager.h>

namespace gate_spectate
{
	namespace
	{
		// 软水位:超过就跳过状态帧(§7)。挡在 2MB 高水位前面:高水位回调延后执行且只触发一次,
		// 一轮大扇出里单连接缓冲可能远超 2MB 后才被关,观战帧不该成为踢掉大厅连接的原因。
		constexpr std::size_t kSoftWatermarkBytes = 256 * 1024;

		struct InstalledHub
		{
			std::weak_ptr<SpectateHub> hub;
			std::string tokenSecret;
		};
		thread_local InstalledHub tlsInstalled;

		uint64_t NowWallMs()
		{
			return static_cast<uint64_t>(std::chrono::duration_cast<std::chrono::milliseconds>(
											 std::chrono::system_clock::now().time_since_epoch())
											 .count());
		}

		// 强引用只活在本次发送内,不留存(AGENTS.md §11.7)。
		SendResult SendToLiveSession(SessionId sessionId, const gate_push::EncodedFrame &frame, FrameDelivery delivery)
		{
			const auto &sessions = tlsSessionManager.sessions();
			const auto it = sessions.find(sessionId);
			if (it == sessions.end())
			{
				return SendResult::kSessionGone;
			}
			const auto conn = it->second.conn.lock();
			if (!conn)
			{
				return SendResult::kSessionGone;
			}
			if (delivery == FrameDelivery::kBestEffort && conn->outputBuffer()->readableBytes() > kSoftWatermarkBytes)
			{
				return SendResult::kBufferFull;
			}
			frame.SendTo(conn);
			return SendResult::kSent;
		}

		// 成功回执:带请求 id 与原消息号,客户端按 id 认领(client-rpc-callback.md §3)。
		void SendReceipt(SessionInfo &session, const ClientRequest &request, const google::protobuf::Message &response)
		{
			const auto conn = session.conn.lock();
			if (!conn)
			{
				return;
			}
			MessageContent content;
			content.set_id(request.id());
			content.set_message_id(request.message_id());
			content.set_serialized_message(response.SerializeAsString());
			GetGateCodec().send(conn, content);
		}

		void HandleSubscribe(SessionId sessionId, SessionInfo &session, const ClientRequest &request)
		{
			SubscribeSpectateRequest body;
			if (!body.ParseFromString(request.body()))
			{
				gate_lane::SendEnvelope(session, request.id(), request.message_id(), kRequestMessageParseError);
				return;
			}
			const auto hub = tlsInstalled.hub.lock();
			if (!hub)
			{
				// 只会出现在关机收尾的窗口里:确定未执行,客户端重试安全。
				gate_lane::SendEnvelope(session, request.id(), request.message_id(), kServiceUnavailable);
				return;
			}

			const bool boundToPlayer = session.playerId != kInvalidGuid && session.playerId != 0;
			const TicketCheck check = VerifySpectateTicket(tlsInstalled.tokenSecret, body.spectate_ticket(),
														   boundToPlayer ? session.playerId : 0, body.battle_id(), NowWallMs());
			if (!check.ok())
			{
				hub->NoteTicketRejected();
				// 过期是正常现象(客户端慢了一步),伪造也不该按公网帧量逐条写盘:第一条必打,此后每 1024 条一行。
				const uint64_t rejected = hub->Counters().ticketRejects;
				if ((rejected & 0x3FF) == 1)
				{
					LOG_WARN << "Spectate ticket rejected (sampled), reason=" << TicketVerdictName(check.verdict)
							 << " latest_session_id=" << sessionId << " rejected_total=" << rejected;
				}
				gate_lane::SendEnvelope(session, request.id(), request.message_id(), kMatchSpectateTicketInvalid);
				return;
			}

			// 回执在前、首帧在后:客户端收到回执才知道订阅成了,之后的 161 才有归属。
			SendReceipt(session, request, SubscribeSpectateResponse{});
			hub->Subscribe(sessionId, check.payload);
		}

		void HandleUnsubscribe(SessionId sessionId, SessionInfo &session, const ClientRequest &request)
		{
			UnsubscribeSpectateRequest body;
			if (!body.ParseFromString(request.body()))
			{
				gate_lane::SendEnvelope(session, request.id(), request.message_id(), kRequestMessageParseError);
				return;
			}
			if (const auto hub = tlsInstalled.hub.lock())
			{
				hub->Unsubscribe(sessionId, body.battle_id());
			}
			SendReceipt(session, request, UnsubscribeSpectateResponse{});
		}
	} // namespace

	std::shared_ptr<SpectateHub> InstallHub(muduo::net::EventLoop &loop, std::string tokenSecret)
	{
		HubPorts ports;
		ports.sendFrame = &SendToLiveSession;
		// loop 比 hub 活得久:EventLoop 是 RunNodeMain 的局部变量,hub 随 GateRuntimeContext 在它之前析构。
		ports.defer = [&loop](std::function<void()> task) { loop.queueInLoop(std::move(task)); };
		ports.steadyNow = [] { return std::chrono::steady_clock::now(); };

		auto hub = std::make_shared<SpectateHub>(std::move(ports), HubLimits{});
		tlsInstalled.hub = hub;
		tlsInstalled.tokenSecret = std::move(tokenSecret);
		return hub;
	}

	void HandleClientRequest(SessionId sessionId, SessionInfo &session, const ClientRequest &request)
	{
		switch (request.message_id())
		{
		case GateClientSpectateSubscribeSpectateMessageId:
			HandleSubscribe(sessionId, session, request);
			return;
		case GateClientSpectateUnsubscribeSpectateMessageId:
			HandleUnsubscribe(sessionId, session, request);
			return;
		default:
			break;
		}
		// 目标节点是 gate 的客户端消息目前只有上面两条;以后新增却没接到这里时,回确定未执行,不让客户端干等超时。
		static uint64_t unhandledCount = 0;
		if ((unhandledCount++ & 0x3FF) == 0)
		{
			LOG_ERROR << "Gate-local client message has no handler (sampled), latest_message_id=" << request.message_id()
					  << " unhandled_total=" << unhandledCount;
		}
		gate_lane::SendEnvelope(session, request.id(), request.message_id(), kServiceUnavailable);
	}

	void OnSessionClosed(SessionId sessionId)
	{
		if (const auto hub = tlsInstalled.hub.lock())
		{
			hub->OnSessionClosed(sessionId);
		}
	}

	void OnKafkaFrame(const contracts::kafka::SpectateFrameEvent &event)
	{
		if (const auto hub = tlsInstalled.hub.lock())
		{
			hub->OnFrame(event);
		}
	}

	void SweepIdleNow()
	{
		if (const auto hub = tlsInstalled.hub.lock())
		{
			hub->SweepIdle(std::chrono::steady_clock::now(), NowWallMs());
		}
	}
} // namespace gate_spectate
