#include "battle_terminal_query.h"

#include <chrono>
#include <cstddef>
#include <cstdlib>
#include <string>
#include <utility>
#include <vector>

#include <muduo/base/Logging.h>

#include "core/network/grpc_client_call.h"
#include "core/security/service_call_auth.h"
#include "core/utils/random/random.h"
#include "grpc_client/match/match_internal_grpc_client.h"
#include "muduo_grpc/channel.h"
#include "node/system/node/muduo_grpc_channel_slot.h"
#include "proto/match/match_internal.pb.h"
#include "thread_context/node_context_manager.h"
#include "time/system/time.h"

namespace battle_terminal_query
{
	using battle_freeze_hold::TerminalAnswer;

	namespace
	{
		// 就绪的 match 节点里随机挑一个。任一副本读的都是同一份 SQL 终局;随机而不是按局号固定:
		// 某个副本卡住时,下一拍的查询大概率换到别的副本上。
		entt::entity PickReadyMatchNode(entt::registry& registry)
		{
			std::vector<entt::entity> ready;
			for (const auto node : registry.view<NodeInfo, MuduoGrpcChannelPtr>())
			{
				const auto& channel = registry.get<MuduoGrpcChannelPtr>(node);
				if (channel && channel->GetState(true) == muduo_grpc::ConnectivityState::kReady)
				{
					ready.push_back(node);
				}
			}
			if (ready.empty())
			{
				return entt::null;
			}
			return ready[tlsRandom.Rand<std::size_t>(0, ready.size() - 1)];
		}

		std::string SceneSecret()
		{
			const char* secret = std::getenv(kSceneSecretEnv);
			return secret != nullptr ? std::string(secret) : std::string();
		}
	} // namespace

	TerminalAnswer ClassifyReply(const bool callOk, const match::GetBattleTerminalDecisionResponse* reply,
								 const std::string_view expectedDigest, const std::string_view secret, const uint64_t nowMs)
	{
		if (!callOk || reply == nullptr || secret.empty() || expectedDigest.empty() ||
			reply->request_digest() != expectedDigest)
		{
			return TerminalAnswer::kUnknown;
		}
		if (!service_call_auth::Verify(std::string(kMethod) + "/reply", *reply, reply->auth(), kMatchCaller, secret, nowMs))
		{
			return TerminalAnswer::kUnknown;
		}
		if (reply->retry())
		{
			return TerminalAnswer::kUnknown; // match 这一次没读成 SQL:不是结论
		}
		switch (reply->decision())
		{
		case match::BATTLE_OUTCOME_DECISION_UNKNOWN:
			// 还没有终局决定(NONE):有可裁决的 v1 原件就是「还没落定」,没有才是「永远不会有结算」(battle_freeze_hold_rules.h)。
			return reply->terminal_expected() ? TerminalAnswer::kPending : TerminalAnswer::kNoOriginal;
		case match::BATTLE_OUTCOME_DECISION_COMPLETE:
			return TerminalAnswer::kComplete;
		case match::BATTLE_OUTCOME_DECISION_ABORT:
			return TerminalAnswer::kAbort;
		default:
			return TerminalAnswer::kUnknown; // 新版本 match 加了本二进制不认识的决定:不替它猜
		}
	}

	bool Send(const uint64_t battleId, const uint32_t deadlineMs, Done done)
	{
		if (battleId == 0 || !done)
		{
			return false;
		}
		const std::string secret = SceneSecret();
		if (secret.empty())
		{
			LOG_ERROR << "[PlayerBattle] Battle terminal query not sent: " << kSceneSecretEnv
					  << " is not set, battle_id=" << battleId << "(freeze is held as unknown until the cap)";
			return false;
		}
		auto& registry = tlsNodeContextManager.GetRegistry(common::base::MatchNodeService);
		const auto node = PickReadyMatchNode(registry);
		if (node == entt::null)
		{
			LOG_WARN << "[PlayerBattle] Battle terminal query not sent: no ready match node, battle_id=" << battleId;
			return false;
		}
		match::GetBattleTerminalDecisionRequest request;
		request.set_battle_id(battleId);
		if (!service_call_auth::Sign(kMethod, request, *request.mutable_auth(), kSceneCaller, secret,
									 TimeSystem::NowMillisecondsUTC()))
		{
			LOG_ERROR << "[PlayerBattle] Battle terminal query not sent: signing failed(scene secret shorter than 32 bytes?), battle_id="
					  << battleId;
			return false;
		}
		std::string digest = service_call_auth::Digest(request);
		if (digest.empty())
		{
			return false;
		}
		GrpcCallOptions options;
		options.deadline = std::chrono::milliseconds(deadlineMs);
		// 回调只捕获 id、摘要与 done(不捕获实体句柄,GrpcDone 契约)。密钥在回调里重读:轮换后以当前值为准。
		match::SendMatchInternalGetBattleTerminalDecision(
			registry, node, request,
			[battleId, digest = std::move(digest), done = std::move(done)](
				const GrpcCallOutcome& outcome, const match::GetBattleTerminalDecisionResponse* reply) {
				const TerminalAnswer answer =
					ClassifyReply(outcome.ok(), reply, digest, SceneSecret(), TimeSystem::NowMillisecondsUTC());
				if (answer == TerminalAnswer::kUnknown)
				{
					LOG_WARN << "[PlayerBattle] metric=battle_terminal_query_unknown battle_id=" << battleId
							 << " code=" << outcome.code << " reply_present=" << (reply != nullptr)
							 << " retry=" << (reply != nullptr && reply->retry()) << " message=" << outcome.message;
				}
				done(answer);
			},
			options);
		return true;
	}
} // namespace battle_terminal_query
