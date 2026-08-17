
#include "scene_handler.h"

///<<< BEGIN WRITING YOUR CODE

#include "agones/agones_scene_lifecycle.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "node/system/node/node.h"
#include "player/system/player_lifecycle.h"
#include "player/system/player_scene.h"
#include "battle/system/player_battle.h"
#include "rpc/player_service_interface.h"
#include "network/rpc_session.h"
#include "network/network_constants.h"
#include "network/error_handling_system.h"
#include "proto/common/component/player_async_comp.pb.h"
#include "proto/common/component/player_network_comp.pb.h"
#include "proto/common/base/node.pb.h"
#include "rpc/service_metadata/rpc_event_registry.h"
#include "type_alias/player_session_type_alias.h"
#include "core/utils/proto/proto_field_checker.h"
#include "network/network_utils.h"
#include "thread_context/node_context_manager.h"
#include "core/system/redis.h"
#include "rpc/service_metadata/scene_service_metadata.h"
#include "player/comp/afk_comp.h"
#include "frame/manager/frame_time.h"
#include "rpc/service_metadata/gate_service_service_metadata.h"
#include "rpc/service_metadata/player_lifecycle_service_metadata.h"
#include "proto/db/proto_option.pb.h"
#include "modules/scene/comp/scene_node_comp.h"
#include "proto/common/event/scene_event.pb.h"
#include <unordered_map>
#include <string>

using MessageUniquePtr = std::unique_ptr<google::protobuf::Message>;

using namespace muduo::net;

namespace
{

	MessagePriority GetMethodPriority(const google::protobuf::MethodDescriptor *method)
	{
		if (!method)
		{
			return MESSAGE_PRIORITY_NORMAL;
		}

		const auto &options = method->options();
		if (!options.HasExtension(OptionMessagePriority))
		{
			return MESSAGE_PRIORITY_NORMAL;
		}

		return options.GetExtension(OptionMessagePriority);
	}

	MessagePriority ResolveRoutedMessagePriority(const RoutePlayerMessageRequest &request)
	{
		ClientRequest clientRequest;
		if (!clientRequest.ParseFromString(request.body()))
		{
			return GetMethodPriority(::Scene::descriptor()->FindMethodByName("RoutePlayerStringMsg"));
		}

		if (clientRequest.message_id() >= gRpcMethodRegistry.size())
		{
			return GetMethodPriority(::Scene::descriptor()->FindMethodByName("RoutePlayerStringMsg"));
		}

		const auto &messageInfo = gRpcMethodRegistry[clientRequest.message_id()];
		const auto serviceIt = gPlayerService.find(messageInfo.serviceName);
		if (serviceIt == gPlayerService.end())
		{
			return GetMethodPriority(::Scene::descriptor()->FindMethodByName("RoutePlayerStringMsg"));
		}

		google::protobuf::Service *service = serviceIt->second->service();
		if (!service || !service->GetDescriptor())
		{
			return GetMethodPriority(::Scene::descriptor()->FindMethodByName("RoutePlayerStringMsg"));
		}

		return GetMethodPriority(service->GetDescriptor()->FindMethodByName(messageInfo.methodName));
	}

	bool IsImportantPriority(const MessagePriority priority)
	{
		return priority == MESSAGE_PRIORITY_IMPORTANT;
	}

} // namespace

///<<< END WRITING YOUR CODE

void SceneHandler::PlayerEnterGameNode(::google::protobuf::RpcController* controller, const ::PlayerEnterGameNodeRequest* request,
	::Empty* response,
	::google::protobuf::Closure* done)
{
	///<<< BEGIN WRITING YOUR CODE
	LOG_DEBUG << "Handling PlayerEnterGameNode for player: " << request->player_id()
	          << " session=" << request->session_id()
	          << " enter_gs_type=" << request->enter_gs_type()
	          << " scene_id=" << request->scene_id();

	// 1. Clear existing player session (handles rapid reconnect / account takeover)
	PlayerLifecycleSystem::RemovePlayerSessionSilently(request->player_id());

	auto playerIt = tlsEcs.playerList.find(request->player_id());

	// Carry session/login/scene context through the async load pipeline.
	PlayerGameNodeEntryInfoComp enterInfo;
	enterInfo.set_session_id(request->session_id());
	enterInfo.set_enter_gs_type(request->enter_gs_type());
	enterInfo.set_scene_id(request->scene_id());

	// 2. If player is already online, enter scene directly
	if (playerIt != tlsEcs.playerList.end())
	{
		PlayerLifecycleSystem::EnterScene(playerIt->second, enterInfo);
		return;
	}

	// 3. Player not online — kick off Redis-only async load.
	//    Player data is expected to already be in Redis, preloaded by Login via
	//    Kafka -> DB Service -> Redis. On Redis NIL, AsyncLoad retries with
	//    millisecond-grade exponential backoff (500ms/1s/2s/4s/8s/16s) up to
	//    kMaxLoadRetries; the first retry is fast so a healthy preload barely
	//    impacts player-perceived login latency.
	//    After load completes, HandlePlayerAsyncLoaded -> InitPlayerFromAllData -> EnterScene
	//    will use enterInfo to bind session, find scene, and process the login.

	// Pre-register session so that client messages arriving during async load
	// hit the "player not loaded" guard instead of "session id not found".
	// insert_or_assign defends against unlikely session_id reuse (snowflake collision).
	if (request->session_id() != 0)
	{
		SessionMap().insert_or_assign(request->session_id(), request->player_id());
	}

	// Store enter info in PendingEnterMap. If a load is already in flight
	// (rapid disconnect+reconnect), this overwrites the old entry so the
	// callback will use the latest session/scene context.
	// First, clean up the stale session from the previous in-flight load
	// to prevent late ExitGame(old_session) from killing the new player.
	auto& pendingMap = PlayerLifecycleSystem::GetPendingEnterMap();
	auto pendingIt = pendingMap.find(request->player_id());
	if (pendingIt != pendingMap.end()
		&& pendingIt->second.session_id() != 0
		&& pendingIt->second.session_id() != request->session_id())
	{
		SessionMap().erase(pendingIt->second.session_id());
	}
	pendingMap[request->player_id()] = enterInfo;

	tlsRedisSystem.GetPlayerDataRedis()->AsyncLoad(request->player_id());
	///<<< END WRITING YOUR CODE
}

void SceneHandler::SendMessageToPlayer(::google::protobuf::RpcController* controller, const ::NodeRouteMessageRequest* request,
	::NodeRouteMessageResponse* response,
	::google::protobuf::Closure* done)
{
	///<<< BEGIN WRITING YOUR CODE

	LOG_TRACE << "Handling message routing for session ID: " << request->header().session_id()
			  << ", message ID: " << request->message_content().message_id();

	const auto it = SessionMap().find(request->header().session_id());
	if (it == SessionMap().end())
	{
		LOG_ERROR << "Session ID not found: " << request->header().session_id()
				  << ", message ID: " << request->message_content().message_id();
		return;
	}

	const auto playerIt = tlsEcs.playerList.find(it->second);
	if (playerIt == tlsEcs.playerList.end())
	{
		LOG_ERROR << "Player ID not found in common logic: " << it->second;
		return;
	}

	const auto &player = playerIt->second;

	// Guard: reject messages from a superseded session (same as ProcessClientPlayerMessage).
	const auto *snapshot = tlsEcs.actorRegistry.try_get<PlayerSessionSnapshotComp>(player);
	if (snapshot && snapshot->gate_session_id() != request->header().session_id())
	{
		LOG_INFO << "SendMessageToPlayer: stale session " << request->header().session_id()
				 << " (current=" << snapshot->gate_session_id()
				 << "), dropping message_id=" << request->message_content().message_id();
		SessionMap().erase(it);
		return;
	}

	if (request->message_content().message_id() >= gRpcMethodRegistry.size())
	{
		LOG_ERROR << "Invalid message ID: " << request->message_content().message_id();
		return;
	}

	const auto &messageInfo = gRpcMethodRegistry[request->message_content().message_id()];
	const auto serviceIt = gPlayerService.find(messageInfo.serviceName);
	if (serviceIt == gPlayerService.end())
	{
		LOG_ERROR << "PlayerService not found for message ID: " << request->message_content().message_id();
		return;
	}

	const auto &serviceHandler = serviceIt->second;
	google::protobuf::Service *service = serviceHandler->service();
	const google::protobuf::MethodDescriptor *method = service->GetDescriptor()->FindMethodByName(messageInfo.methodName);
	if (method == nullptr)
	{
		LOG_ERROR << "Method not found in PlayerService: " << messageInfo.methodName;
		return;
	}

	const MessageUniquePtr playerRequest(service->GetRequestPrototype(method).New());
	if (!playerRequest->ParsePartialFromArray(request->message_content().serialized_message().data(),
											  int32_t(request->message_content().serialized_message().size())))
	{
		LOG_ERROR << "Failed to parse request message for message ID: " << request->message_content().message_id()
				  << " session=" << request->header().session_id();
		return;
	}

	std::string errorDetails;
	if (ProtoFieldChecker::CheckFieldSizes(*playerRequest, kProtoFieldCheckerThreshold, errorDetails))
	{
		LOG_ERROR << errorDetails << " message_id=" << request->message_content().message_id()
				  << " session=" << request->header().session_id();
		return;
	}
	if (ProtoFieldChecker::CheckForNegativeInts(*playerRequest, errorDetails))
	{
		LOG_ERROR << errorDetails << " message_id=" << request->message_content().message_id()
				  << " session=" << request->header().session_id();
		return;
	}

	const MessageUniquePtr playerResponse(service->GetResponsePrototype(method).New());
	serviceHandler->CallMethod(method, player, playerRequest.get(), playerResponse.get());

	response->mutable_header()->set_session_id(request->header().session_id());
	response->mutable_message_content()->set_message_id(request->message_content().message_id());

	if (Empty::GetDescriptor() == playerResponse->GetDescriptor())
	{
		return;
	}

	response->mutable_message_content()->set_serialized_message(playerResponse->SerializeAsString());
	///<<< END WRITING YOUR CODE
}

void SceneHandler::ProcessClientPlayerMessage(::google::protobuf::RpcController* controller, const ::ProcessClientPlayerMessageRequest* request,
	::ProcessClientPlayerMessageResponse* response,
	::google::protobuf::Closure* done)
{
	///<<< BEGIN WRITING YOUR CODE
	if (!request)
		return;

	const auto &msg = request->message_content();
	const SessionId sessionId = request->session_id();

	if (msg.message_id() >= gRpcMethodRegistry.size())
	{
		LOG_ERROR << "ProcessClientPlayerMessage: message_id not found " << msg.message_id();
		return;
	}

	const auto &messageInfo = gRpcMethodRegistry.at(msg.message_id());
	const auto serviceIt = gPlayerService.find(messageInfo.serviceName);
	if (serviceIt == gPlayerService.end())
	{
		LOG_ERROR << "ProcessClientPlayerMessage: player service not found for message id " << msg.message_id();
		return;
	}

	google::protobuf::Service *service = serviceIt->second->service();
	const google::protobuf::MethodDescriptor *method = service->GetDescriptor()->FindMethodByName(messageInfo.methodName);
	if (method == nullptr)
	{
		LOG_ERROR << "ProcessClientPlayerMessage: method not found " << messageInfo.methodName;
		return;
	}

	const auto it = SessionMap().find(sessionId);
	if (it == SessionMap().end())
	{
		LOG_ERROR << "ProcessClientPlayerMessage: session id not found " << sessionId << ", message id " << msg.message_id();
		return;
	}

	const auto player = tlsEcs.GetPlayer(it->second);
	if (player == entt::null)
	{
		// Player entity not yet created (async Redis load in progress).
		// If this is an ExitGame request, erase the pre-registered session
		// so HandlePlayerAsyncLoaded knows the load was cancelled.
		if (msg.message_id() == ScenePlayerExitGameMessageId)
		{
			LOG_INFO << "ProcessClientPlayerMessage: ExitGame during async load, "
						"cancelling session for player_id=" << it->second;
			SessionMap().erase(it);
		}
		else
		{
			LOG_WARN << "ProcessClientPlayerMessage: player not loaded for player_id=" << it->second
					 << " message_id=" << msg.message_id() << " (likely still loading from Redis)";
		}
		return;
	}

	// Guard: reject messages from a superseded session.
	// After reconnect, SessionMap may briefly contain both old and new entries
	// pointing to the same player_id.  If a late message (especially ExitGame)
	// arrives on the OLD session, it must NOT be dispatched to the player.
	const auto *snapshot = tlsEcs.actorRegistry.try_get<PlayerSessionSnapshotComp>(player);
	if (snapshot && snapshot->gate_session_id() != sessionId)
	{
		LOG_INFO << "ProcessClientPlayerMessage: stale session " << sessionId
				 << " (current=" << snapshot->gate_session_id()
				 << ") for player_id=" << it->second
				 << ", dropping message_id=" << msg.message_id();
		SessionMap().erase(it);
		return;
	}

	// Update last-active frame for AFK detection.
	tlsEcs.actorRegistry.get<LastActiveFrameComp>(player).frame =
		tlsFrameTimeManager.frameTime.current_frame();

	// Parse request and validate field constraints
	const MessageUniquePtr playerRequest(service->GetRequestPrototype(method).New());
	if (!playerRequest->ParsePartialFromArray(msg.serialized_message().data(),
											  static_cast<int32_t>(msg.serialized_message().size())))
	{
		LOG_ERROR << "ProcessClientPlayerMessage: failed to parse client-sent request message_id=" << msg.message_id()
				  << " session=" << sessionId;
		return;
	}

	std::string errorDetails;
	if (ProtoFieldChecker::CheckFieldSizes(*playerRequest, kProtoFieldCheckerThreshold, errorDetails))
	{
		LOG_ERROR << "ProcessClientPlayerMessage: " << errorDetails << " message_id=" << msg.message_id()
				  << " session=" << sessionId;
		return;
	}
	if (ProtoFieldChecker::CheckForNegativeInts(*playerRequest, errorDetails))
	{
		LOG_ERROR << "ProcessClientPlayerMessage: " << errorDetails << " message_id=" << msg.message_id()
				  << " session=" << sessionId;
		return;
	}

	// Dispatch to the concrete player service method
	const MessageUniquePtr playerResponse(service->GetResponsePrototype(method).New());
	serviceIt->second->CallMethod(method, player, playerRequest.get(), playerResponse.get());

	// Populate synchronous response (if caller expects one)
	if (response != nullptr && Empty::GetDescriptor() != playerResponse->GetDescriptor())
	{
		response->mutable_message_content()->set_message_id(msg.message_id());
		response->mutable_message_content()->set_id(msg.id());
		response->set_session_id(sessionId);

		// Serialize response
		const auto byte_size = playerResponse->ByteSizeLong();
		response->mutable_message_content()->mutable_serialized_message()->resize(byte_size);
		if (!playerResponse->SerializePartialToArray(response->mutable_message_content()->mutable_serialized_message()->data(),
													 static_cast<int32_t>(byte_size)))
		{
			LOG_ERROR << "ProcessClientPlayerMessage: Failed to serialize response for message ID: " << msg.message_id();
			// Optionally fill error message here
		}
	}

	///<<< END WRITING YOUR CODE
}

void SceneHandler::InvokePlayerService(::google::protobuf::RpcController* controller, const ::NodeRouteMessageRequest* request,
	::NodeRouteMessageResponse* response,
	::google::protobuf::Closure* done)
{
	///<<< BEGIN WRITING YOUR CODE
	const auto it = SessionMap().find(request->header().session_id());
	if (it == SessionMap().end())
	{
		LOG_ERROR << "session id not found " << request->header().session_id() << ","
				  << " message id " << request->message_content().message_id();
		SendErrorToClient(*request, *response, kSessionNotFound);
		return;
	}

	const auto player = tlsEcs.GetPlayer(it->second);
	if (player == entt::null)
	{
		LOG_ERROR << "GatePlayerService player not loading";
		SendErrorToClient(*request, *response, kPlayerNotFoundInSession);
		return;
	}

	// Guard: reject messages from a superseded session.
	const auto *snapshot = tlsEcs.actorRegistry.try_get<PlayerSessionSnapshotComp>(player);
	if (snapshot && snapshot->gate_session_id() != request->header().session_id())
	{
		LOG_INFO << "InvokePlayerService: stale session " << request->header().session_id()
				 << " (current=" << snapshot->gate_session_id()
				 << "), dropping message_id=" << request->message_content().message_id();
		SessionMap().erase(it);
		return;
	}

	if (request->message_content().message_id() >= gRpcMethodRegistry.size())
	{
		LOG_ERROR << "message_id not found " << request->message_content().message_id();
		SendErrorToClient(*request, *response, kMessageIdNotFound);
		return;
	}

	const auto &messageInfo = gRpcMethodRegistry[request->message_content().message_id()];
	const auto serviceIt = gPlayerService.find(messageInfo.serviceName);
	if (serviceIt == gPlayerService.end())
	{
		LOG_ERROR << "PlayerService service not found " << request->header().session_id()
				  << "," << request->message_content().message_id();
		return;
	}

	const auto &serviceHandler = serviceIt->second;
	google::protobuf::Service *service = serviceHandler->service();
	const google::protobuf::MethodDescriptor *method = service->GetDescriptor()->FindMethodByName(messageInfo.methodName);
	if (method == nullptr)
	{
		LOG_ERROR << "PlayerService method not found " << request->message_content().message_id();
		return;
	}

	MessageUniquePtr playerRequest(service->GetRequestPrototype(method).New());
	if (!playerRequest->ParsePartialFromArray(request->message_content().serialized_message().data(), int32_t(request->message_content().serialized_message().size())))
	{
		LOG_ERROR << "ParsePartialFromArray " << request->message_content().message_id();
		SendErrorToClient(*request, *response, kRequestMessageParseError);
		return;
	}

	std::string errorDetails;

	// Validate field sizes
	if (ProtoFieldChecker::CheckFieldSizes(*playerRequest, kProtoFieldCheckerThreshold, errorDetails))
	{
		LOG_ERROR << errorDetails << " Failed to check request for message ID: "
				  << request->message_content().message_id();
		SendErrorToClient(*request, *response, kArraySizeTooLargeInMessage);
		return;
	}

	// Validate no negative integers
	if (ProtoFieldChecker::CheckForNegativeInts(*playerRequest, errorDetails))
	{
		LOG_ERROR << errorDetails << " Failed to check request for message ID: "
				  << request->message_content().message_id();
		SendErrorToClient(*request, *response, kNegativeValueInMessage);
		return;
	}

	MessageUniquePtr playerResponse(service->GetResponsePrototype(method).New());
	serviceHandler->CallMethod(method, player, playerRequest.get(), playerResponse.get());

	response->mutable_header()->set_session_id(request->header().session_id());
	response->mutable_message_content()->set_message_id(request->message_content().message_id());

	if (Empty::GetDescriptor() == playerResponse->GetDescriptor())
	{
		return;
	}

	auto &tipInfoMessage = tlsEcs.globalRegistry.get_or_emplace<TipInfoMessage>(tlsEcs.GlobalEntity());
	response->mutable_message_content()->mutable_error_message()->CopyFrom(tipInfoMessage);
	tipInfoMessage.Clear();

	const auto byte_size = playerResponse->ByteSizeLong();
	response->mutable_message_content()->mutable_serialized_message()->resize(byte_size);
	if (!playerResponse->SerializePartialToArray(response->mutable_message_content()->mutable_serialized_message()->data(),
												 static_cast<int32_t>(byte_size)))
	{
		LOG_ERROR << "Failed to serialize response for message ID: " << request->message_content().message_id();
		SendErrorToClient(*request, *response, kResponseMessageParseError);
		return;
	}

	///<<< END WRITING YOUR CODE
}

void SceneHandler::RouteNodeStringMsg(::google::protobuf::RpcController* controller, const ::RouteMessageRequest* request,
	::RouteMessageResponse* response,
	::google::protobuf::Closure* done)
{
	///<<< BEGIN WRITING YOUR CODE
	///<<< END WRITING YOUR CODE
}

void SceneHandler::RoutePlayerStringMsg(::google::protobuf::RpcController* controller, const ::RoutePlayerMessageRequest* request,
	::RoutePlayerMessageResponse* response,
	::google::protobuf::Closure* done)
{
	///<<< BEGIN WRITING YOUR CODE
	if (!request || !response)
	{
		return;
	}

	// Always mirror request in response so callers can correlate route result.
	response->set_body(request->body());
	response->mutable_player_info()->CopyFrom(request->player_info());
	response->mutable_node_list()->CopyFrom(request->node_list());

	// Local fast path: player already loaded on this scene and no further route nodes.
	const Guid playerId = request->player_info().player_id();
	const MessagePriority routedPriority = ResolveRoutedMessagePriority(*request);
	const bool importantRoute = IsImportantPriority(routedPriority);
	if (playerId != kInvalidGuid)
	{
		const auto localPlayer = tlsEcs.GetPlayer(playerId);
		if (localPlayer != entt::null && request->node_list_size() == 0)
		{
			// Cross-zone Frozen gate (cross-zone-readiness-audit.md §11.3).
			// If the player is mid-cross-zone-migration the source-side
			// entity is alive only to wait for the destination's ACK; any
			// client message dispatched to it would mutate state that's
			// about to be DestroyPlayer'd. Drop the message and let the
			// client retry once the migration completes (the gate session
			// is preserved across Frozen, so client stays connected).
			//
			// IMPORTANT messages are logged as ERROR so ops can see if the
			// destination zone is taking too long to ACK and clients are
			// piling up retries — that's a reaper / ACK failure signal.
			if (PlayerLifecycleSystem::IsCrossZoneFrozen(localPlayer))
			{
				if (importantRoute)
				{
					LOG_ERROR << "RoutePlayerStringMsg dropped: player frozen for cross-zone migration "
							  << "(IMPORTANT, may need client retry), player_id=" << playerId;
				}
				else
				{
					LOG_DEBUG << "RoutePlayerStringMsg dropped: player frozen for cross-zone migration "
							  << "(NORMAL, silent drop), player_id=" << playerId;
				}
				return;
			}

			LOG_TRACE << "RoutePlayerStringMsg local hit, player_id=" << playerId;
			return;
		}
	}

	if (request->node_list_size() == 0)
	{
		if (importantRoute)
		{
			LOG_ERROR << "RoutePlayerStringMsg IMPORTANT route dropped: no next node, player_id=" << playerId;
		}
		else
		{
			LOG_WARN << "RoutePlayerStringMsg NORMAL route dropped: no next node, player_id=" << playerId;
		}
		return;
	}

	RoutePlayerMessageRequest nextRequest(*request);
	// Consume the current hop before forwarding.
	nextRequest.mutable_node_list()->DeleteSubrange(0, 1);

	const auto &nextNode = request->node_list(0);
	entt::entity nextNodeEntity{nextNode.node_id()};
	auto &nextRegistry = tlsNodeContextManager.GetRegistry(nextNode.node_type());
	if (!nextRegistry.valid(nextNodeEntity))
	{
		LOG_ERROR << "RoutePlayerStringMsg route dropped: next node not found, node_type=" << nextNode.node_type()
				  << ", node_id=" << nextNode.node_id() << ", player_id=" << playerId;
		return;
	}

	const auto nextSession = nextRegistry.try_get<RpcSession>(nextNodeEntity);
	if (!nextSession)
	{
		LOG_ERROR << "RoutePlayerStringMsg route dropped: next node session missing, node_type=" << nextNode.node_type()
				  << ", node_id=" << nextNode.node_id() << ", player_id=" << playerId;
		return;
	}

	switch (nextNode.node_type())
	{
	case eNodeType::SceneNodeService:
		nextSession->SendRequest(SceneRoutePlayerStringMsgMessageId, nextRequest);
		return;
	case eNodeType::GateNodeService:
		nextSession->SendRequest(GateRoutePlayerMessageMessageId, nextRequest);
		return;
	default:
		LOG_WARN << "RoutePlayerStringMsg route dropped: unsupported next node_type=" << nextNode.node_type()
				 << ", node_id=" << nextNode.node_id() << ", player_id=" << playerId;
		return;
	}
	///<<< END WRITING YOUR CODE
}

void SceneHandler::UpdateSessionDetail(::google::protobuf::RpcController* controller, const ::RegisterPlayerSessionRequest* request,
	::Empty* response,
	::google::protobuf::Closure* done)
{
	///<<< BEGIN WRITING YOUR CODE
	PlayerLifecycleSystem::RemovePlayerSession(request->player_id());

	if (!ResolveLocalZoneGateEntity(request->session_id()))
	{
		LOG_ERROR << "Gate not found " << GetGateNodeId(request->session_id());
		return;
	}

	const auto player = tlsEcs.GetPlayer(request->player_id());
	if (!tlsEcs.actorRegistry.valid(player))
	{
		LOG_ERROR << "Player not found " << request->player_id();
		return;
	}

	SessionMap().emplace(request->session_id(), request->player_id());

	tlsEcs.actorRegistry.get<PlayerSessionSnapshotComp>(player).set_gate_session_id(request->session_id());

	PlayerLifecycleSystem::HandleBindPlayerToGateOK(player);
	///<<< END WRITING YOUR CODE
}

void SceneHandler::CreateScene(::google::protobuf::RpcController* controller, const ::CreateSceneRequest* request,
	::CreateSceneResponse* response,
	::google::protobuf::Closure* done)
{
	///<<< BEGIN WRITING YOUR CODE
	if (request->config_id() == 0 || request->scene_id() == 0)
	{
		LOG_ERROR << "CreateScene: config_id=" << request->config_id()
				  << " scene_id=" << request->scene_id() << ", both must be non-zero";
		return;
	}

	// Agones high-density mode: allocate gate.
	//
	// This handler runs ON the muduo EventLoop, so the blocking variant is NOT
	// allowed here -- one HTTP hiccup would stall the whole logic frame. The
	// non-blocking variant kicks an async allocate and returns false right
	// away; this create fails closed and the caller's retry normally lands
	// after the process is already Allocated. (SceneManager uses the gRPC
	// path, which does get the bounded blocking variant.)
	auto createPermit = agones::SceneLifecycle::Instance().AcquireCreatePermitNonBlocking();
	if (!createPermit)
	{
		LOG_ERROR << "CreateScene: rejected, Agones allocate not confirmed yet (state="
				  << agones::ToString(agones::SceneLifecycle::Instance().State())
				  << "), scene_id=" << request->scene_id() << "; retry expected";
		return;
	}

	// Idempotent: deduplicate by scene_id (Go-allocated, globally unique).
	{
		auto view = tlsEcs.sceneRegistry.view<SceneInfoComp>();
		for (auto entity : view)
		{
			const auto &info = view.get<SceneInfoComp>(entity);
			if (info.scene_id() == request->scene_id())
			{
				response->mutable_scene_info()->CopyFrom(info);
				LOG_INFO << "CreateScene: scene_id=" << request->scene_id()
						 << " already exists (entity=" << entt::to_integral(entity) << "), returning existing";
				return;
			}
		}
	}

	auto sceneEntity = tlsEcs.sceneRegistry.create();

	// Keep this block in sync with SceneNodeGrpcImpl::HandleCreateScene. Both
	// entrypoints write the same fields so the mirror-co-location pipeline
	// works no matter which transport the caller uses.
	auto &sceneInfo = tlsEcs.sceneRegistry.emplace<SceneInfoComp>(sceneEntity);
	sceneInfo.set_scene_config_id(request->config_id());
	sceneInfo.set_scene_id(request->scene_id());
	sceneInfo.set_mirror_config_id(request->mirror_config_id());
	sceneInfo.set_dungeon_config_id(request->dungeon_config_id());

	for (const auto creator_id : request->creator_ids())
	{
		sceneInfo.mutable_creators()->insert({creator_id, true});
	}

	tlsEcs.sceneRegistry.emplace<ScenePlayers>(sceneEntity);

	OnSceneCreated event;
	event.set_entity(entt::to_integral(sceneEntity));
	tlsEcs.dispatcher.trigger(event);

	response->mutable_scene_info()->CopyFrom(sceneInfo);

	LOG_INFO << "CreateScene: created scene entity=" << entt::to_integral(sceneEntity)
			 << " config_id=" << request->config_id()
			 << " scene_id=" << sceneInfo.scene_id()
			 << " mirror_config_id=" << sceneInfo.mirror_config_id()
			 << " dungeon_config_id=" << sceneInfo.dungeon_config_id();
	///<<< END WRITING YOUR CODE
}

void SceneHandler::DestroyScene(::google::protobuf::RpcController* controller, const ::DestroySceneRequest* request,
	::Empty* response,
	::google::protobuf::Closure* done)
{
	///<<< BEGIN WRITING YOUR CODE
	const auto sceneId = request->scene_id();
	if (sceneId == 0)
	{
		LOG_ERROR << "DestroyScene: scene_id is 0";
		return;
	}

	// Find the scene entity by scene_id.
	entt::entity targetEntity = entt::null;
	auto view = tlsEcs.sceneRegistry.view<SceneInfoComp>();
	for (auto entity : view)
	{
		const auto &info = view.get<SceneInfoComp>(entity);
		if (info.scene_id() == sceneId)
		{
			targetEntity = entity;
			break;
		}
	}

	if (targetEntity == entt::null)
	{
		LOG_WARN << "DestroyScene: scene entity not found for scene_id=" << sceneId;
		return;
	}

	// Drain-then-destroy. Keep in sync with SceneNodeGrpcImpl::HandleDestroyScene
	// -- see the long comment there for why we return without destroying.
	if (const std::size_t relocating = PlayerLifecycleSystem::BeginSceneDrain(targetEntity); relocating > 0)
	{
		LOG_WARN << "DestroyScene: scene_id=" << sceneId << " still has residents; relocating "
				 << relocating << " player(s) to the main world, entity kept until drained";
		return;
	}

	// Fire scene-destroyed event before entity removal.
	OnSceneDestroyed event;
	event.set_entity(entt::to_integral(targetEntity));
	tlsEcs.dispatcher.trigger(event);

	// Destroy the ECS entity (removes all components).
	tlsEcs.sceneRegistry.destroy(targetEntity);

	LOG_INFO << "DestroyScene: destroyed scene entity for scene_id=" << sceneId;
	///<<< END WRITING YOUR CODE}
}

void SceneHandler::PrepareBattle(::google::protobuf::RpcController* controller, const ::PrepareBattleRequest* request,
	::PrepareBattleResponse* response,
	::google::protobuf::Closure* done)
{
///<<< BEGIN WRITING YOUR CODE
	// match -> scene 备战冻结(muduo RPC 入口,已在 loop 线程,直接委托;
	// match Go 侧实际走 SceneNodeGrpc 的 gRPC 面,本面供 C++ 节点间调用)
	PlayerBattleSystem::PrepareBattle(*request, *response);
///<<< END WRITING YOUR CODE}
}

void SceneHandler::CancelBattlePrepare(::google::protobuf::RpcController* controller, const ::CancelBattlePrepareRequest* request,
	::Empty* response,
	::google::protobuf::Closure* done)
{
///<<< BEGIN WRITING YOUR CODE
	// gather 失败补偿解冻,幂等
	PlayerBattleSystem::CancelBattlePrepare(*request);
///<<< END WRITING YOUR CODE}
}

void SceneHandler::NodeHandshake(::google::protobuf::RpcController* controller, const ::NodeHandshakeRequest* request,
	::NodeHandshakeResponse* response,
	::google::protobuf::Closure* done)
{
	///<<< BEGIN WRITING YOUR CODE
	gNode->GetNodeRegistrationManager().OnNodeHandshake(*request, *response);
	///<<< END WRITING YOUR CODE
}
