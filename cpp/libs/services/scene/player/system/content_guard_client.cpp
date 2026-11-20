#include "player/system/content_guard_client.h"

#include <cstddef>
#include <utility>
#include <vector>

#include "muduo/base/Logging.h"
#include "muduo_grpc/channel.h" // 选点读 muduo_grpc::Channel::GetState / ConnectivityState

#include "core/network/grpc_client_call.h"
#include "grpc_client/content_guard/content_guard_grpc_client.h"
#include "node/system/node/muduo_grpc_channel_slot.h"
#include "proto/common/base/common.pb.h" // NodeInfo
#include "proto/common/base/node.pb.h"	 // ContentGuardNodeService
#include "thread_context/node_context_manager.h"
#include "utils/random/random.h"

namespace
{
	// 只在候选里挑挂着 muduo_grpc 通道且通道 READY 的节点(与 player_inbox_system.cpp 的 PickReadyPlayerInboxNode 同一写法):
	// 通道不 READY 时发出去,要么立刻失败、要么占着在途状态等到 deadline;直接回「发不出去」,让调用方当场回提示。
	// GetState(true) 顺手把断开后回到 IDLE 的通道踢回 CONNECTING。内容安全是全服一份、多副本,任一副本都行,不比对 zone。
	entt::entity PickReadyContentGuardNode()
	{
		auto &registry = tlsNodeContextManager.GetRegistry(common::base::ContentGuardNodeService);
		std::vector<entt::entity> ready;
		for (auto entity : registry.view<NodeInfo, MuduoGrpcChannelPtr>())
		{
			const auto &channel = registry.get<MuduoGrpcChannelPtr>(entity);
			if (channel && channel->GetState(/*tryToConnect=*/true) == muduo_grpc::ConnectivityState::kReady)
			{
				ready.push_back(entity);
			}
		}
		if (ready.empty())
		{
			return entt::null;
		}
		return ready[tlsRandom.Rand<std::size_t>(0, ready.size() - 1)];
	}

	bool DefaultSend(const content_guard::CheckTextRequest &request, ContentGuardClient::Done done)
	{
		const entt::entity node = PickReadyContentGuardNode();
		if (node == entt::null)
		{
			return false;
		}
		content_guard::SendContentGuardCheckText(
			tlsNodeContextManager.GetRegistry(common::base::ContentGuardNodeService), node, request,
			[scene = static_cast<int>(request.scene()), done = std::move(done)](
				const GrpcCallOutcome &outcome, const content_guard::CheckTextResponse *reply)
			{
				if (!outcome.ok())
				{
					// 只记场景与状态码,不记被检文字。
					LOG_ERROR << "[ContentGuard] check failed, treating the text as unusable: scene=" << scene
							  << " grpc_code=" << outcome.code << " message=" << outcome.message;
				}
				done(ContentGuardClient::FromReply(outcome.ok(), reply));
			});
		return true;
	}

	ContentGuardClient::SendFn gSendFn = &DefaultSend;
} // namespace

bool ContentGuardClient::Check(content_guard::TextScene scene, uint64_t playerId, const std::string &text, Done done)
{
	content_guard::CheckTextRequest request;
	request.set_scene(scene);
	request.set_text(text);
	request.set_player_id(playerId);
	return gSendFn(request, std::move(done));
}

ContentGuardClient::Result ContentGuardClient::FromReply(bool callOk, const content_guard::CheckTextResponse *reply)
{
	Result result;
	if (!callOk || reply == nullptr)
	{
		result.verdict = Verdict::kUnavailable;
		return result;
	}
	result.category = static_cast<uint32_t>(reply->category());
	switch (reply->verdict())
	{
	case content_guard::TEXT_VERDICT_PASS:
		result.verdict = Verdict::kPass;
		return result;
	case content_guard::TEXT_VERDICT_MASKED:
		if (!reply->masked_text().empty())
		{
			result.verdict = Verdict::kMasked;
			result.maskedText = reply->masked_text();
			return result;
		}
		break;
	default:
		break;
	}
	result.verdict = Verdict::kReject;
	return result;
}

void ContentGuardClient::SetTransportForTest(SendFn send)
{
	gSendFn = (send == nullptr) ? &DefaultSend : send;
}
