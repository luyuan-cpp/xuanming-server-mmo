#pragma once

#include <functional>
#include "entt/src/entt/entity/registry.hpp"
#include <grpcpp/grpcpp.h>
#include <google/protobuf/message.h>
#include "muduo/base/Logging.h"
#include "grpc_client/grpc_call_tag.h"
#include <proto/common/base/node.pb.h>
#include <proto/common/base/common.pb.h>

using grpc::ClientContext;
using grpc::Status;
using grpc::ClientAsyncResponseReader;

namespace NodeUtils
{
	common::base::eNodeType GetServiceTypeFromPrefix(const std::string& prefix);
	entt::registry& GetRegistryForNodeType(uint32_t nodeType);
	std::string GetRegistryName(const entt::registry& registry);
	common::base::eNodeType GetRegistryType(const entt::registry& registry);
	bool IsSameNode(const std::string& uuid1, const std::string& uuid2);
	bool IsNodeConnected(uint32_t nodeType, const NodeInfo& info);
};

    void SetBattleNodeHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
    void SetBattleNodeIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
    void InitBattleNodeGrpcNode(const std::shared_ptr< ::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity);
    void HandleBattleNodeCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag);

    void SetPlayerBattleHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
    void SetPlayerBattleIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
    void InitPlayerBattleGrpcNode(const std::shared_ptr< ::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity);
    void HandlePlayerBattleCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag);

namespace chatpb {
    void SetChatHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
    void SetChatIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
    void InitChatGrpcNode(const std::shared_ptr< ::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity);
    void HandleChatCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag);
}

namespace data_service {
    void SetDataServiceHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
    void SetDataServiceIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
    void InitDataServiceGrpcNode(const std::shared_ptr< ::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity);
    void HandleDataServiceCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag);
}

namespace etcdserverpb {
    void SetEtcdHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
    void SetEtcdIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
    void InitEtcdGrpcNode(const std::shared_ptr< ::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity);
    void HandleEtcdCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag);
}

namespace friendpb {
    void SetFriendHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
    void SetFriendIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
    void InitFriendGrpcNode(const std::shared_ptr< ::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity);
    void HandleFriendCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag);
}

namespace guildpb {
    void SetGuildHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
    void SetGuildIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
    void InitGuildGrpcNode(const std::shared_ptr< ::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity);
    void HandleGuildCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag);
}

namespace loginpb {
    void SetLoginHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
    void SetLoginIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
    void InitLoginGrpcNode(const std::shared_ptr< ::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity);
    void HandleLoginCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag);
}

namespace match {
    void SetMatchServiceHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
    void SetMatchServiceIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
    void InitMatchServiceGrpcNode(const std::shared_ptr< ::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity);
    void HandleMatchServiceCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag);
}

namespace scene_manager {
    void SetSceneManagerServiceHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
    void SetSceneManagerServiceIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
    void InitSceneManagerServiceGrpcNode(const std::shared_ptr< ::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity);
    void HandleSceneManagerServiceCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag);
}

namespace scene_node {
    void SetSceneNodeServiceHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
    void SetSceneNodeServiceIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
    void InitSceneNodeServiceGrpcNode(const std::shared_ptr< ::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity);
    void HandleSceneNodeServiceCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag);
}

void SetIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler){

    ::SetBattleNodeIfEmptyHandler(handler);

    ::SetPlayerBattleIfEmptyHandler(handler);

    chatpb::SetChatIfEmptyHandler(handler);

    data_service::SetDataServiceIfEmptyHandler(handler);

    etcdserverpb::SetEtcdIfEmptyHandler(handler);

    friendpb::SetFriendIfEmptyHandler(handler);

    guildpb::SetGuildIfEmptyHandler(handler);

    loginpb::SetLoginIfEmptyHandler(handler);

    match::SetMatchServiceIfEmptyHandler(handler);

    scene_manager::SetSceneManagerServiceIfEmptyHandler(handler);

    scene_node::SetSceneNodeServiceIfEmptyHandler(handler);

}

void SetHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler){

    ::SetBattleNodeHandler(handler);

    ::SetPlayerBattleHandler(handler);

    chatpb::SetChatHandler(handler);

    data_service::SetDataServiceHandler(handler);

    etcdserverpb::SetEtcdHandler(handler);

    friendpb::SetFriendHandler(handler);

    guildpb::SetGuildHandler(handler);

    loginpb::SetLoginHandler(handler);

    match::SetMatchServiceHandler(handler);

    scene_manager::SetSceneManagerServiceHandler(handler);

    scene_node::SetSceneNodeServiceHandler(handler);

}

void HandleCompletedQueueMessage(entt::registry& registry){
    auto nodeType = NodeUtils::GetRegistryType(registry);
    auto&& view = registry.view<grpc::CompletionQueue>();
    for (auto&& [e, completeQueueComp] : view.each()) {
        void* got_tag = nullptr;
        bool ok = false;
        gpr_timespec tm = {0, 0, GPR_CLOCK_MONOTONIC};
        while (completeQueueComp.AsyncNext(&got_tag, &ok, tm) == grpc::CompletionQueue::GOT_EVENT) {
            if (!ok) {
                LOG_ERROR << "RPC failed";
                return;
            }
            GrpcTag* grpcTag(reinterpret_cast<GrpcTag*>(got_tag));
            const auto messageId = grpcTag->messageId;
            if (common::base::eNodeType::BattleNodeService == nodeType &&
                (messageId == 146u || messageId == 147u || messageId == 159u || messageId == 160u)) {
                ::HandleBattleNodeCompletedQueueMessage(registry, e, completeQueueComp, grpcTag);
            }
            else if (common::base::eNodeType::BattleNodeService == nodeType &&
                (messageId == 139u || messageId == 140u || messageId == 143u || messageId == 144u || messageId == 149u || messageId == 150u || messageId == 158u || messageId == 161u || messageId == 162u || messageId == 165u || messageId == 166u)) {
                ::HandlePlayerBattleCompletedQueueMessage(registry, e, completeQueueComp, grpcTag);
            }
            else if (common::base::eNodeType::ChatNodeService == nodeType &&
                (messageId == 28u || messageId == 61u)) {
                chatpb::HandleChatCompletedQueueMessage(registry, e, completeQueueComp, grpcTag);
            }
            else if (common::base::eNodeType::DataServiceNodeService == nodeType &&
                (messageId == 86u || messageId == 87u || messageId == 88u || messageId == 89u || messageId == 90u || messageId == 91u || messageId == 92u || messageId == 93u || messageId == 96u || messageId == 97u || messageId == 98u || messageId == 99u || messageId == 100u || messageId == 101u || messageId == 105u || messageId == 108u || messageId == 114u || messageId == 129u)) {
                data_service::HandleDataServiceCompletedQueueMessage(registry, e, completeQueueComp, grpcTag);
            }
            else if (common::base::eNodeType::EtcdNodeService == nodeType &&
                (messageId == 0u || messageId == 4u || messageId == 5u || messageId == 6u || messageId == 13u || messageId == 20u || messageId == 22u || messageId == 25u || messageId == 59u || messageId == 73u || messageId == 80u)) {
                etcdserverpb::HandleEtcdCompletedQueueMessage(registry, e, completeQueueComp, grpcTag);
            }
            else if (common::base::eNodeType::FriendNodeService == nodeType &&
                (messageId == 2u || messageId == 7u || messageId == 11u || messageId == 12u || messageId == 53u || messageId == 76u || messageId == 119u || messageId == 120u)) {
                friendpb::HandleFriendCompletedQueueMessage(registry, e, completeQueueComp, grpcTag);
            }
            else if (common::base::eNodeType::GuildNodeService == nodeType &&
                (messageId == 8u || messageId == 15u || messageId == 19u || messageId == 27u || messageId == 29u || messageId == 35u || messageId == 38u || messageId == 39u || messageId == 52u || messageId == 60u)) {
                guildpb::HandleGuildCompletedQueueMessage(registry, e, completeQueueComp, grpcTag);
            }
            else if (common::base::eNodeType::LoginNodeService == nodeType &&
                (messageId == 14u || messageId == 17u || messageId == 26u || messageId == 48u || messageId == 58u || messageId == 111u || messageId == 118u || messageId == 127u || messageId == 138u)) {
                loginpb::HandleLoginCompletedQueueMessage(registry, e, completeQueueComp, grpcTag);
            }
            else if (common::base::eNodeType::MatchNodeService == nodeType &&
                (messageId == 148u || messageId == 151u || messageId == 152u || messageId == 153u || messageId == 154u || messageId == 156u || messageId == 157u || messageId == 163u || messageId == 164u)) {
                match::HandleMatchServiceCompletedQueueMessage(registry, e, completeQueueComp, grpcTag);
            }
            else if (common::base::eNodeType::SceneManagerNodeService == nodeType &&
                (messageId == 16u || messageId == 44u || messageId == 46u || messageId == 85u)) {
                scene_manager::HandleSceneManagerServiceCompletedQueueMessage(registry, e, completeQueueComp, grpcTag);
            }
            else if (common::base::eNodeType::SceneManagerNodeService == nodeType &&
                (messageId == 122u || messageId == 123u || messageId == 128u || messageId == 142u || messageId == 145u)) {
                scene_node::HandleSceneNodeServiceCompletedQueueMessage(registry, e, completeQueueComp, grpcTag);
            }
        }
    }
}

void InitGrpcNode(const std::shared_ptr< ::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity){
    auto nodeType = NodeUtils::GetRegistryType(registry);
    registry.emplace<grpc::CompletionQueue>(nodeEntity);
    if (common::base::eNodeType::BattleNodeService == nodeType) {
        ::InitBattleNodeGrpcNode(channel, registry, nodeEntity);
    }
    if (common::base::eNodeType::BattleNodeService == nodeType) {
        ::InitPlayerBattleGrpcNode(channel, registry, nodeEntity);
    }
    if (common::base::eNodeType::ChatNodeService == nodeType) {
        chatpb::InitChatGrpcNode(channel, registry, nodeEntity);
    }
    if (common::base::eNodeType::DataServiceNodeService == nodeType) {
        data_service::InitDataServiceGrpcNode(channel, registry, nodeEntity);
    }
    if (common::base::eNodeType::EtcdNodeService == nodeType) {
        etcdserverpb::InitEtcdGrpcNode(channel, registry, nodeEntity);
    }
    if (common::base::eNodeType::FriendNodeService == nodeType) {
        friendpb::InitFriendGrpcNode(channel, registry, nodeEntity);
    }
    if (common::base::eNodeType::GuildNodeService == nodeType) {
        guildpb::InitGuildGrpcNode(channel, registry, nodeEntity);
    }
    if (common::base::eNodeType::LoginNodeService == nodeType) {
        loginpb::InitLoginGrpcNode(channel, registry, nodeEntity);
    }
    if (common::base::eNodeType::MatchNodeService == nodeType) {
        match::InitMatchServiceGrpcNode(channel, registry, nodeEntity);
    }
    if (common::base::eNodeType::SceneManagerNodeService == nodeType) {
        scene_manager::InitSceneManagerServiceGrpcNode(channel, registry, nodeEntity);
    }
    if (common::base::eNodeType::SceneManagerNodeService == nodeType) {
        scene_node::InitSceneNodeServiceGrpcNode(channel, registry, nodeEntity);
    }
}
