#include "scene_manager_response_handler.h"

#include "grpc_client/scene_manager/scene_manager_service_grpc_client.h"
#include "muduo/base/Logging.h"

#include "engine/thread_context/node_context_manager.h"
#include "network/network_utils.h"
#include "network/node_utils.h"
#include "proto/common/base/node.pb.h"
#include "proto/common/component/player_network_comp.pb.h"
#include "proto/scene_manager/scene_manager_service.pb.h"
#include "services/scene/player/system/player_lifecycle.h"
#include "thread_context/ecs_context.h"

void InitSceneManagerReply()
{
    scene_manager::AsyncSceneManagerEnterSceneHandler =
        [](const grpc::ClientContext& /*ctx*/, const ::scene_manager::EnterSceneResponse& resp)
    {
        // 应答回调里没有请求上下文,靠 scene_manager 回显的 player_id 把结果对回玩家。
        // 跨 zone 传送的源端要据此收尾(放行 → 销毁本地实体;失败 → 核实后解冻),成功与
        // 失败都要送到;没有传送意图的玩家在里面是 no-op。player_id == 0 = 旧版 scene_manager
        // 未回显,传送只能靠应答看门狗收敛。
        if (resp.player_id() != 0)
        {
            PlayerLifecycleSystem::HandleTravelEnterSceneReply(resp.player_id(), resp);
        }

        if (resp.error_code() != 0)
        {
            LOG_ERROR << "SceneManager.EnterScene error: player=" << resp.player_id()
                      << " code=" << resp.error_code()
                      << " msg=" << resp.error_message();
            return;
        }

        if (resp.has_redirect())
        {
            LOG_INFO << "SceneManager.EnterScene returned cross-zone redirect to "
                     << resp.redirect().target_gate_ip() << ":"
                     << resp.redirect().target_gate_port();
        }
    };

    // CreateScene response handler. Currently only used for the mirror flow:
    // when a SceneNode-initiated mirror create succeeds, SceneManager echoes
    // creator_ids back so we can dispatch the follow-up EnterScene without
    // keeping per-call state on the SceneNode side. For non-mirror creates
    // (system bootstrapping, admin tools, etc.) creator_ids is empty and the
    // handler is a no-op — those callers don't expect an auto-enter.
    scene_manager::AsyncSceneManagerCreateSceneHandler =
        [](const grpc::ClientContext& /*ctx*/, const ::scene_manager::CreateSceneResponse& resp)
    {
        if (resp.error_code() != 0)
        {
            LOG_ERROR << "SceneManager.CreateScene error: code=" << resp.error_code()
                      << " msg=" << resp.error_message()
                      << " creators=" << resp.creator_ids_size();
            return;
        }

        if (resp.scene_id() == 0)
        {
            LOG_ERROR << "SceneManager.CreateScene returned scene_id=0; treating as failure";
            return;
        }

        if (resp.creator_ids_size() == 0)
        {
            // System-initiated create (no creator). Nothing more to do here.
            return;
        }

        auto smEntity = GetSceneManagerEntity(resp.creator_ids(0));
        if (smEntity == entt::null)
        {
            LOG_WARN << "CreateScene reply: no SceneManager node available to follow-up EnterScene "
                        "for scene " << resp.scene_id();
            return;
        }
        auto& smRegistry = tlsNodeContextManager.GetRegistry(eNodeType::SceneManagerNodeService);

        for (int i = 0; i < resp.creator_ids_size(); ++i)
        {
            const uint64_t playerId = resp.creator_ids(i);
            auto playerEntity = tlsEcs.GetPlayer(playerId);
            if (playerEntity == entt::null)
            {
                // Player may have disconnected between our CreateScene fire and
                // the reply. Drop the follow-up — when they reconnect their
                // client will issue a normal EnterScene.
                LOG_WARN << "CreateScene reply: creator " << playerId
                         << " not present on this node, skipping auto-enter for scene "
                         << resp.scene_id();
                continue;
            }

            const auto* playerSessionPB =
                tlsEcs.actorRegistry.try_get<PlayerSessionSnapshotComp>(playerEntity);
            if (playerSessionPB == nullptr)
            {
                LOG_ERROR << "CreateScene reply: PlayerSessionSnapshotComp missing for creator "
                          << playerId;
                continue;
            }

            NodeId gateNodeId = GetGateNodeId(playerSessionPB->gate_session_id());
            std::string gateInstanceId;
            if (auto gateEntityOpt = ResolveLocalZoneGateEntity(playerSessionPB->gate_session_id()); gateEntityOpt)
            {
                auto& gateRegistry = tlsNodeContextManager.GetRegistry(eNodeType::GateNodeService);
                if (const auto* gateNodeInfo = gateRegistry.try_get<NodeInfo>(*gateEntityOpt))
                {
                    gateInstanceId = gateNodeInfo->node_uuid();
                }
            }

            ::scene_manager::EnterSceneRequest req;
            req.set_player_id(playerId);
            req.set_scene_id(resp.scene_id());
            req.set_session_id(playerSessionPB->gate_session_id());
            req.set_gate_id(std::to_string(gateNodeId));
            req.set_gate_instance_id(gateInstanceId);
            req.set_gate_zone_id(GetZoneId());
            req.set_zone_id(GetZoneId());

            scene_manager::SendSceneManagerEnterScene(smRegistry, smEntity, req);

            LOG_INFO << "CreateScene reply: dispatched EnterScene player=" << playerId
                     << " mirror_scene=" << resp.scene_id()
                     << " mirror_node=" << resp.node_id();
        }
    };
}
