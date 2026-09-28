#include "muduo/base/Logging.h"

#include "login_grpc_client.h"
#include "proto/common/constants/etcd_grpc.pb.h"
#include "core/utils/encode/base64.h"
#include <atomic>
#include <chrono>
#include <boost/pool/object_pool.hpp>
#include "grpc_call_tag.h"

namespace {
boost::object_pool<GrpcTag> tagPool;
// 本文件所有 unary 调用的 deadline(毫秒)。启动时 SetLoginCallDeadline 按目标节点类型写入
// (Node::Initialize → grpc_call_deadline::Apply);原子量:与应答处理器一样是进程级全局。
std::atomic<uint32_t> callDeadlineMs{kDefaultGrpcCallDeadlineMs};

std::chrono::system_clock::time_point NextCallDeadline() {
    return std::chrono::system_clock::now() +
        std::chrono::milliseconds(callDeadlineMs.load(std::memory_order_relaxed));
}
}

namespace loginpb {
struct LoginCompleteQueue {
    grpc::CompletionQueue cq;
};
#pragma region ClientPlayerLoginLogin
boost::object_pool<AsyncClientPlayerLoginLoginGrpcClient> ClientPlayerLoginLoginPool;
using AsyncClientPlayerLoginLoginHandlerFunctionType =
    std::function<void(const ClientContext&, const ::loginpb::LoginResponse&)>;
AsyncClientPlayerLoginLoginHandlerFunctionType AsyncClientPlayerLoginLoginHandler;
AsyncClientPlayerLoginLoginFailedHandlerFunctionType AsyncClientPlayerLoginLoginFailedHandler;

void AsyncCompleteGrpcClientPlayerLoginLogin(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerLoginLoginGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerLoginLoginHandler) {
            AsyncClientPlayerLoginLoginHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC ClientPlayerLogin.Login reply dropped: AsyncClientPlayerLoginLoginHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncClientPlayerLoginLoginFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "ClientPlayerLogin.Login", call->context, call->status, call->sentMetadata};
        AsyncClientPlayerLoginLoginFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC ClientPlayerLogin.Login failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	ClientPlayerLoginLoginPool.destroy(call);
}

void SendClientPlayerLoginLogin(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::LoginRequest& request) {

    SendClientPlayerLoginLogin(registry, nodeEntity, request, {}, {});

}

void SendClientPlayerLoginLogin(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::LoginRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerLoginLoginPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    call->sentMetadata.reserve(count);
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
        call->sentMetadata.emplace_back(metaKeys[i], metaValues[i]);
    }
    call->request = request;
    call->context.set_deadline(NextCallDeadline());

    call->response_reader = registry
        .get<ClientPlayerLoginStubPtr>(nodeEntity)
        ->PrepareAsyncLogin(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerLoginLoginMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerLoginLogin(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::loginpb::LoginRequest& derived = static_cast<const ::loginpb::LoginRequest&>(message);
    SendClientPlayerLoginLogin(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerLoginCreatePlayer
boost::object_pool<AsyncClientPlayerLoginCreatePlayerGrpcClient> ClientPlayerLoginCreatePlayerPool;
using AsyncClientPlayerLoginCreatePlayerHandlerFunctionType =
    std::function<void(const ClientContext&, const ::loginpb::CreatePlayerResponse&)>;
AsyncClientPlayerLoginCreatePlayerHandlerFunctionType AsyncClientPlayerLoginCreatePlayerHandler;
AsyncClientPlayerLoginCreatePlayerFailedHandlerFunctionType AsyncClientPlayerLoginCreatePlayerFailedHandler;

void AsyncCompleteGrpcClientPlayerLoginCreatePlayer(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerLoginCreatePlayerGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerLoginCreatePlayerHandler) {
            AsyncClientPlayerLoginCreatePlayerHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC ClientPlayerLogin.CreatePlayer reply dropped: AsyncClientPlayerLoginCreatePlayerHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncClientPlayerLoginCreatePlayerFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "ClientPlayerLogin.CreatePlayer", call->context, call->status, call->sentMetadata};
        AsyncClientPlayerLoginCreatePlayerFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC ClientPlayerLogin.CreatePlayer failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	ClientPlayerLoginCreatePlayerPool.destroy(call);
}

void SendClientPlayerLoginCreatePlayer(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::CreatePlayerRequest& request) {

    SendClientPlayerLoginCreatePlayer(registry, nodeEntity, request, {}, {});

}

void SendClientPlayerLoginCreatePlayer(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::CreatePlayerRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerLoginCreatePlayerPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    call->sentMetadata.reserve(count);
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
        call->sentMetadata.emplace_back(metaKeys[i], metaValues[i]);
    }
    call->request = request;
    call->context.set_deadline(NextCallDeadline());

    call->response_reader = registry
        .get<ClientPlayerLoginStubPtr>(nodeEntity)
        ->PrepareAsyncCreatePlayer(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerLoginCreatePlayerMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerLoginCreatePlayer(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::loginpb::CreatePlayerRequest& derived = static_cast<const ::loginpb::CreatePlayerRequest&>(message);
    SendClientPlayerLoginCreatePlayer(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerLoginEnterGame
boost::object_pool<AsyncClientPlayerLoginEnterGameGrpcClient> ClientPlayerLoginEnterGamePool;
using AsyncClientPlayerLoginEnterGameHandlerFunctionType =
    std::function<void(const ClientContext&, const ::loginpb::EnterGameResponse&)>;
AsyncClientPlayerLoginEnterGameHandlerFunctionType AsyncClientPlayerLoginEnterGameHandler;
AsyncClientPlayerLoginEnterGameFailedHandlerFunctionType AsyncClientPlayerLoginEnterGameFailedHandler;

void AsyncCompleteGrpcClientPlayerLoginEnterGame(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerLoginEnterGameGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerLoginEnterGameHandler) {
            AsyncClientPlayerLoginEnterGameHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC ClientPlayerLogin.EnterGame reply dropped: AsyncClientPlayerLoginEnterGameHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncClientPlayerLoginEnterGameFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "ClientPlayerLogin.EnterGame", call->context, call->status, call->sentMetadata};
        AsyncClientPlayerLoginEnterGameFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC ClientPlayerLogin.EnterGame failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	ClientPlayerLoginEnterGamePool.destroy(call);
}

void SendClientPlayerLoginEnterGame(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::EnterGameRequest& request) {

    SendClientPlayerLoginEnterGame(registry, nodeEntity, request, {}, {});

}

void SendClientPlayerLoginEnterGame(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::EnterGameRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerLoginEnterGamePool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    call->sentMetadata.reserve(count);
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
        call->sentMetadata.emplace_back(metaKeys[i], metaValues[i]);
    }
    call->request = request;
    call->context.set_deadline(NextCallDeadline());

    call->response_reader = registry
        .get<ClientPlayerLoginStubPtr>(nodeEntity)
        ->PrepareAsyncEnterGame(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerLoginEnterGameMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerLoginEnterGame(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::loginpb::EnterGameRequest& derived = static_cast<const ::loginpb::EnterGameRequest&>(message);
    SendClientPlayerLoginEnterGame(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerLoginLeaveGame
boost::object_pool<AsyncClientPlayerLoginLeaveGameGrpcClient> ClientPlayerLoginLeaveGamePool;
using AsyncClientPlayerLoginLeaveGameHandlerFunctionType =
    std::function<void(const ClientContext&, const ::loginpb::LoginEmptyResponse&)>;
AsyncClientPlayerLoginLeaveGameHandlerFunctionType AsyncClientPlayerLoginLeaveGameHandler;
AsyncClientPlayerLoginLeaveGameFailedHandlerFunctionType AsyncClientPlayerLoginLeaveGameFailedHandler;

void AsyncCompleteGrpcClientPlayerLoginLeaveGame(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerLoginLeaveGameGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerLoginLeaveGameHandler) {
            AsyncClientPlayerLoginLeaveGameHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC ClientPlayerLogin.LeaveGame reply dropped: AsyncClientPlayerLoginLeaveGameHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncClientPlayerLoginLeaveGameFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "ClientPlayerLogin.LeaveGame", call->context, call->status, call->sentMetadata};
        AsyncClientPlayerLoginLeaveGameFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC ClientPlayerLogin.LeaveGame failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	ClientPlayerLoginLeaveGamePool.destroy(call);
}

void SendClientPlayerLoginLeaveGame(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::LeaveGameRequest& request) {

    SendClientPlayerLoginLeaveGame(registry, nodeEntity, request, {}, {});

}

void SendClientPlayerLoginLeaveGame(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::LeaveGameRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerLoginLeaveGamePool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    call->sentMetadata.reserve(count);
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
        call->sentMetadata.emplace_back(metaKeys[i], metaValues[i]);
    }
    call->request = request;
    call->context.set_deadline(NextCallDeadline());

    call->response_reader = registry
        .get<ClientPlayerLoginStubPtr>(nodeEntity)
        ->PrepareAsyncLeaveGame(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerLoginLeaveGameMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerLoginLeaveGame(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::loginpb::LeaveGameRequest& derived = static_cast<const ::loginpb::LeaveGameRequest&>(message);
    SendClientPlayerLoginLeaveGame(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerLoginDisconnect
boost::object_pool<AsyncClientPlayerLoginDisconnectGrpcClient> ClientPlayerLoginDisconnectPool;
using AsyncClientPlayerLoginDisconnectHandlerFunctionType =
    std::function<void(const ClientContext&, const ::loginpb::LoginEmptyResponse&)>;
AsyncClientPlayerLoginDisconnectHandlerFunctionType AsyncClientPlayerLoginDisconnectHandler;
AsyncClientPlayerLoginDisconnectFailedHandlerFunctionType AsyncClientPlayerLoginDisconnectFailedHandler;

void AsyncCompleteGrpcClientPlayerLoginDisconnect(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerLoginDisconnectGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerLoginDisconnectHandler) {
            AsyncClientPlayerLoginDisconnectHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC ClientPlayerLogin.Disconnect reply dropped: AsyncClientPlayerLoginDisconnectHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncClientPlayerLoginDisconnectFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "ClientPlayerLogin.Disconnect", call->context, call->status, call->sentMetadata};
        AsyncClientPlayerLoginDisconnectFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC ClientPlayerLogin.Disconnect failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	ClientPlayerLoginDisconnectPool.destroy(call);
}

void SendClientPlayerLoginDisconnect(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::LoginNodeDisconnectRequest& request) {

    SendClientPlayerLoginDisconnect(registry, nodeEntity, request, {}, {});

}

void SendClientPlayerLoginDisconnect(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::LoginNodeDisconnectRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerLoginDisconnectPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    call->sentMetadata.reserve(count);
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
        call->sentMetadata.emplace_back(metaKeys[i], metaValues[i]);
    }
    call->request = request;
    call->context.set_deadline(NextCallDeadline());

    call->response_reader = registry
        .get<ClientPlayerLoginStubPtr>(nodeEntity)
        ->PrepareAsyncDisconnect(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerLoginDisconnectMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerLoginDisconnect(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::loginpb::LoginNodeDisconnectRequest& derived = static_cast<const ::loginpb::LoginNodeDisconnectRequest&>(message);
    SendClientPlayerLoginDisconnect(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region ClientPlayerLoginRefreshToken
boost::object_pool<AsyncClientPlayerLoginRefreshTokenGrpcClient> ClientPlayerLoginRefreshTokenPool;
using AsyncClientPlayerLoginRefreshTokenHandlerFunctionType =
    std::function<void(const ClientContext&, const ::loginpb::RefreshTokenResponse&)>;
AsyncClientPlayerLoginRefreshTokenHandlerFunctionType AsyncClientPlayerLoginRefreshTokenHandler;
AsyncClientPlayerLoginRefreshTokenFailedHandlerFunctionType AsyncClientPlayerLoginRefreshTokenFailedHandler;

void AsyncCompleteGrpcClientPlayerLoginRefreshToken(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncClientPlayerLoginRefreshTokenGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncClientPlayerLoginRefreshTokenHandler) {
            AsyncClientPlayerLoginRefreshTokenHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC ClientPlayerLogin.RefreshToken reply dropped: AsyncClientPlayerLoginRefreshTokenHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncClientPlayerLoginRefreshTokenFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "ClientPlayerLogin.RefreshToken", call->context, call->status, call->sentMetadata};
        AsyncClientPlayerLoginRefreshTokenFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC ClientPlayerLogin.RefreshToken failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	ClientPlayerLoginRefreshTokenPool.destroy(call);
}

void SendClientPlayerLoginRefreshToken(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::RefreshTokenRequest& request) {

    SendClientPlayerLoginRefreshToken(registry, nodeEntity, request, {}, {});

}

void SendClientPlayerLoginRefreshToken(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::RefreshTokenRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(ClientPlayerLoginRefreshTokenPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    call->sentMetadata.reserve(count);
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
        call->sentMetadata.emplace_back(metaKeys[i], metaValues[i]);
    }
    call->request = request;
    call->context.set_deadline(NextCallDeadline());

    call->response_reader = registry
        .get<ClientPlayerLoginStubPtr>(nodeEntity)
        ->PrepareAsyncRefreshToken(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(ClientPlayerLoginRefreshTokenMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendClientPlayerLoginRefreshToken(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::loginpb::RefreshTokenRequest& derived = static_cast<const ::loginpb::RefreshTokenRequest&>(message);
    SendClientPlayerLoginRefreshToken(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region LoginPreGateAssignGate
boost::object_pool<AsyncLoginPreGateAssignGateGrpcClient> LoginPreGateAssignGatePool;
using AsyncLoginPreGateAssignGateHandlerFunctionType =
    std::function<void(const ClientContext&, const ::loginpb::AssignGateResponse&)>;
AsyncLoginPreGateAssignGateHandlerFunctionType AsyncLoginPreGateAssignGateHandler;
AsyncLoginPreGateAssignGateFailedHandlerFunctionType AsyncLoginPreGateAssignGateFailedHandler;

void AsyncCompleteGrpcLoginPreGateAssignGate(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncLoginPreGateAssignGateGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncLoginPreGateAssignGateHandler) {
            AsyncLoginPreGateAssignGateHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC LoginPreGate.AssignGate reply dropped: AsyncLoginPreGateAssignGateHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncLoginPreGateAssignGateFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "LoginPreGate.AssignGate", call->context, call->status, call->sentMetadata};
        AsyncLoginPreGateAssignGateFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC LoginPreGate.AssignGate failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	LoginPreGateAssignGatePool.destroy(call);
}

void SendLoginPreGateAssignGate(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::AssignGateRequest& request) {

    SendLoginPreGateAssignGate(registry, nodeEntity, request, {}, {});

}

void SendLoginPreGateAssignGate(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::AssignGateRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(LoginPreGateAssignGatePool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    call->sentMetadata.reserve(count);
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
        call->sentMetadata.emplace_back(metaKeys[i], metaValues[i]);
    }
    call->request = request;
    call->context.set_deadline(NextCallDeadline());

    call->response_reader = registry
        .get<LoginPreGateStubPtr>(nodeEntity)
        ->PrepareAsyncAssignGate(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(LoginPreGateAssignGateMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendLoginPreGateAssignGate(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::loginpb::AssignGateRequest& derived = static_cast<const ::loginpb::AssignGateRequest&>(message);
    SendLoginPreGateAssignGate(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region LoginPreGateQueryQueueStatus
boost::object_pool<AsyncLoginPreGateQueryQueueStatusGrpcClient> LoginPreGateQueryQueueStatusPool;
using AsyncLoginPreGateQueryQueueStatusHandlerFunctionType =
    std::function<void(const ClientContext&, const ::loginpb::QueryQueueStatusResponse&)>;
AsyncLoginPreGateQueryQueueStatusHandlerFunctionType AsyncLoginPreGateQueryQueueStatusHandler;
AsyncLoginPreGateQueryQueueStatusFailedHandlerFunctionType AsyncLoginPreGateQueryQueueStatusFailedHandler;

void AsyncCompleteGrpcLoginPreGateQueryQueueStatus(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncLoginPreGateQueryQueueStatusGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncLoginPreGateQueryQueueStatusHandler) {
            AsyncLoginPreGateQueryQueueStatusHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC LoginPreGate.QueryQueueStatus reply dropped: AsyncLoginPreGateQueryQueueStatusHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncLoginPreGateQueryQueueStatusFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "LoginPreGate.QueryQueueStatus", call->context, call->status, call->sentMetadata};
        AsyncLoginPreGateQueryQueueStatusFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC LoginPreGate.QueryQueueStatus failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	LoginPreGateQueryQueueStatusPool.destroy(call);
}

void SendLoginPreGateQueryQueueStatus(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::QueryQueueStatusRequest& request) {

    SendLoginPreGateQueryQueueStatus(registry, nodeEntity, request, {}, {});

}

void SendLoginPreGateQueryQueueStatus(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::QueryQueueStatusRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(LoginPreGateQueryQueueStatusPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    call->sentMetadata.reserve(count);
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
        call->sentMetadata.emplace_back(metaKeys[i], metaValues[i]);
    }
    call->request = request;
    call->context.set_deadline(NextCallDeadline());

    call->response_reader = registry
        .get<LoginPreGateStubPtr>(nodeEntity)
        ->PrepareAsyncQueryQueueStatus(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(LoginPreGateQueryQueueStatusMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendLoginPreGateQueryQueueStatus(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::loginpb::QueryQueueStatusRequest& derived = static_cast<const ::loginpb::QueryQueueStatusRequest&>(message);
    SendLoginPreGateQueryQueueStatus(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion
#pragma region LoginAdminRemovePlayersFromAccounts
boost::object_pool<AsyncLoginAdminRemovePlayersFromAccountsGrpcClient> LoginAdminRemovePlayersFromAccountsPool;
using AsyncLoginAdminRemovePlayersFromAccountsHandlerFunctionType =
    std::function<void(const ClientContext&, const ::loginpb::RemovePlayersFromAccountsResponse&)>;
AsyncLoginAdminRemovePlayersFromAccountsHandlerFunctionType AsyncLoginAdminRemovePlayersFromAccountsHandler;
AsyncLoginAdminRemovePlayersFromAccountsFailedHandlerFunctionType AsyncLoginAdminRemovePlayersFromAccountsFailedHandler;

void AsyncCompleteGrpcLoginAdminRemovePlayersFromAccounts(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& cq, void* got_tag) {
    auto call(
        static_cast<AsyncLoginAdminRemovePlayersFromAccountsGrpcClient*>(got_tag));
    if (call->status.ok()) {
        if (AsyncLoginAdminRemovePlayersFromAccountsHandler) {
            AsyncLoginAdminRemovePlayersFromAccountsHandler(call->context, call->reply);
        } else {
            // 应答到了却没人收:2026-04 起换图应答就是这样静默丢了约 5 个月。每个方法每线程报一次;
            // 确实不需要应答的调用方显式装一个空处理器。
            thread_local bool reportedMissingHandler = false;
            if (!reportedMissingHandler) {
                reportedMissingHandler = true;
                LOG_ERROR << "gRPC LoginAdmin.RemovePlayersFromAccounts reply dropped: AsyncLoginAdminRemovePlayersFromAccountsHandler is not installed"
                          << " (install one, or an empty one if the reply is intentionally ignored)";
            }
        }
    } else if (AsyncLoginAdminRemovePlayersFromAccountsFailedHandler) {
        const GrpcCallFailure failure{call->messageId, "LoginAdmin.RemovePlayersFromAccounts", call->context, call->status, call->sentMetadata};
        AsyncLoginAdminRemovePlayersFromAccountsFailedHandler(failure, call->request);
    } else {
        LOG_ERROR << "gRPC LoginAdmin.RemovePlayersFromAccounts failed: code=" << static_cast<int>(call->status.error_code())
                  << " msg=" << call->status.error_message();
    }

	LoginAdminRemovePlayersFromAccountsPool.destroy(call);
}

void SendLoginAdminRemovePlayersFromAccounts(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::RemovePlayersFromAccountsRequest& request) {

    SendLoginAdminRemovePlayersFromAccounts(registry, nodeEntity, request, {}, {});

}

void SendLoginAdminRemovePlayersFromAccounts(entt::registry& registry, entt::entity nodeEntity, const ::loginpb::RemovePlayersFromAccountsRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){

    auto call(LoginAdminRemovePlayersFromAccountsPool.construct());
    auto& cq = registry.get<grpc::CompletionQueue>(nodeEntity);

    const size_t count = std::min(metaKeys.size(), metaValues.size());
    call->sentMetadata.reserve(count);
    for (size_t i = 0; i < count; ++i) {
        call->context.AddMetadata(metaKeys[i], Base64Encode(metaValues[i]));
        call->sentMetadata.emplace_back(metaKeys[i], metaValues[i]);
    }
    call->request = request;
    call->context.set_deadline(NextCallDeadline());

    call->response_reader = registry
        .get<LoginAdminStubPtr>(nodeEntity)
        ->PrepareAsyncRemovePlayersFromAccounts(&call->context, call->request,
                                           &cq);
    call->response_reader->StartCall();
    GrpcTag* got_tag(tagPool.construct(LoginAdminRemovePlayersFromAccountsMessageId, (void*)call));
    call->response_reader->Finish(&call->reply, &call->status, (void*)got_tag);

}

void SendLoginAdminRemovePlayersFromAccounts(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues){
    const ::loginpb::RemovePlayersFromAccountsRequest& derived = static_cast<const ::loginpb::RemovePlayersFromAccountsRequest&>(message);
    SendLoginAdminRemovePlayersFromAccounts(registry, nodeEntity, derived, metaKeys, metaValues);
}
#pragma endregion

void HandleLoginCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag) {
        switch (grpcTag->messageId) {
        case ClientPlayerLoginLoginMessageId:
            AsyncCompleteGrpcClientPlayerLoginLogin(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerLoginCreatePlayerMessageId:
            AsyncCompleteGrpcClientPlayerLoginCreatePlayer(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerLoginEnterGameMessageId:
            AsyncCompleteGrpcClientPlayerLoginEnterGame(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerLoginLeaveGameMessageId:
            AsyncCompleteGrpcClientPlayerLoginLeaveGame(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerLoginDisconnectMessageId:
            AsyncCompleteGrpcClientPlayerLoginDisconnect(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case ClientPlayerLoginRefreshTokenMessageId:
            AsyncCompleteGrpcClientPlayerLoginRefreshToken(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case LoginPreGateAssignGateMessageId:
            AsyncCompleteGrpcLoginPreGateAssignGate(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case LoginPreGateQueryQueueStatusMessageId:
            AsyncCompleteGrpcLoginPreGateQueryQueueStatus(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        case LoginAdminRemovePlayersFromAccountsMessageId:
            AsyncCompleteGrpcLoginAdminRemovePlayersFromAccounts(registry, nodeEntity, completeQueueComp, grpcTag->valuePtr);
			tagPool.destroy(grpcTag);
            break;
        default:
            break;
        }
}

void SetLoginHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler) {

    AsyncClientPlayerLoginLoginHandler = handler;
    AsyncClientPlayerLoginCreatePlayerHandler = handler;
    AsyncClientPlayerLoginEnterGameHandler = handler;
    AsyncClientPlayerLoginLeaveGameHandler = handler;
    AsyncClientPlayerLoginDisconnectHandler = handler;
    AsyncClientPlayerLoginRefreshTokenHandler = handler;
    AsyncLoginPreGateAssignGateHandler = handler;
    AsyncLoginPreGateQueryQueueStatusHandler = handler;
    AsyncLoginAdminRemovePlayersFromAccountsHandler = handler;
}

void SetLoginIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler) {

    if (!AsyncClientPlayerLoginLoginHandler) {
        AsyncClientPlayerLoginLoginHandler = handler;
    }
    if (!AsyncClientPlayerLoginCreatePlayerHandler) {
        AsyncClientPlayerLoginCreatePlayerHandler = handler;
    }
    if (!AsyncClientPlayerLoginEnterGameHandler) {
        AsyncClientPlayerLoginEnterGameHandler = handler;
    }
    if (!AsyncClientPlayerLoginLeaveGameHandler) {
        AsyncClientPlayerLoginLeaveGameHandler = handler;
    }
    if (!AsyncClientPlayerLoginDisconnectHandler) {
        AsyncClientPlayerLoginDisconnectHandler = handler;
    }
    if (!AsyncClientPlayerLoginRefreshTokenHandler) {
        AsyncClientPlayerLoginRefreshTokenHandler = handler;
    }
    if (!AsyncLoginPreGateAssignGateHandler) {
        AsyncLoginPreGateAssignGateHandler = handler;
    }
    if (!AsyncLoginPreGateQueryQueueStatusHandler) {
        AsyncLoginPreGateQueryQueueStatusHandler = handler;
    }
    if (!AsyncLoginAdminRemovePlayersFromAccountsHandler) {
        AsyncLoginAdminRemovePlayersFromAccountsHandler = handler;
    }
}

void SetLoginFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler) {
    AsyncClientPlayerLoginLoginFailedHandler = handler;
    AsyncClientPlayerLoginCreatePlayerFailedHandler = handler;
    AsyncClientPlayerLoginEnterGameFailedHandler = handler;
    AsyncClientPlayerLoginLeaveGameFailedHandler = handler;
    AsyncClientPlayerLoginDisconnectFailedHandler = handler;
    AsyncClientPlayerLoginRefreshTokenFailedHandler = handler;
    AsyncLoginPreGateAssignGateFailedHandler = handler;
    AsyncLoginPreGateQueryQueueStatusFailedHandler = handler;
    AsyncLoginAdminRemovePlayersFromAccountsFailedHandler = handler;
}

void SetLoginIfEmptyFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler) {
    if (!AsyncClientPlayerLoginLoginFailedHandler) {
        AsyncClientPlayerLoginLoginFailedHandler = handler;
    }
    if (!AsyncClientPlayerLoginCreatePlayerFailedHandler) {
        AsyncClientPlayerLoginCreatePlayerFailedHandler = handler;
    }
    if (!AsyncClientPlayerLoginEnterGameFailedHandler) {
        AsyncClientPlayerLoginEnterGameFailedHandler = handler;
    }
    if (!AsyncClientPlayerLoginLeaveGameFailedHandler) {
        AsyncClientPlayerLoginLeaveGameFailedHandler = handler;
    }
    if (!AsyncClientPlayerLoginDisconnectFailedHandler) {
        AsyncClientPlayerLoginDisconnectFailedHandler = handler;
    }
    if (!AsyncClientPlayerLoginRefreshTokenFailedHandler) {
        AsyncClientPlayerLoginRefreshTokenFailedHandler = handler;
    }
    if (!AsyncLoginPreGateAssignGateFailedHandler) {
        AsyncLoginPreGateAssignGateFailedHandler = handler;
    }
    if (!AsyncLoginPreGateQueryQueueStatusFailedHandler) {
        AsyncLoginPreGateQueryQueueStatusFailedHandler = handler;
    }
    if (!AsyncLoginAdminRemovePlayersFromAccountsFailedHandler) {
        AsyncLoginAdminRemovePlayersFromAccountsFailedHandler = handler;
    }
}

void SetLoginCallDeadline(std::chrono::milliseconds deadline) {
    callDeadlineMs.store(static_cast<uint32_t>(deadline.count()), std::memory_order_relaxed);
}

void InitLoginGrpcNode(const std::shared_ptr<::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity) {

    registry.emplace<ClientPlayerLoginStubPtr>(nodeEntity, ClientPlayerLogin::NewStub(channel));
    registry.emplace<LoginPreGateStubPtr>(nodeEntity, LoginPreGate::NewStub(channel));
    registry.emplace<LoginAdminStubPtr>(nodeEntity, LoginAdmin::NewStub(channel));

}

}// namespace loginpb
