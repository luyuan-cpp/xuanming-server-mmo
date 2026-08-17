#pragma once
#include <memory>
#include <boost/circular_buffer.hpp>
#include "entt/src/entt/entity/registry.hpp"
#include "grpc_client/grpc_call_tag.h"
#include "proto/db/db.grpc.pb.h"

#include "rpc/service_metadata/db_service_metadata.h"

using grpc::ClientContext;
using grpc::Status;
using grpc::ClientAsyncResponseReader;

using dbStubPtr = std::unique_ptr<db::Stub>;
#pragma region dbTest

struct AsyncdbTestGrpcClient {
    uint32_t messageId{ dbTestMessageId };
    ClientContext context;
    Status status;
    ::TestResponse reply;
    std::unique_ptr<ClientAsyncResponseReader<::TestResponse>> response_reader;
};

class ::TestRequest;
using AsyncdbTestHandlerFunctionType =
    std::function<void(const ClientContext&, const ::TestResponse&)>;
extern AsyncdbTestHandlerFunctionType AsyncdbTestHandler;

void SenddbTest(entt::registry& registry, entt::entity nodeEntity, const ::TestRequest& request);
void SenddbTest(entt::registry& registry, entt::entity nodeEntity, const ::TestRequest& request, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
void SenddbTest(entt::registry& registry, entt::entity nodeEntity, const google::protobuf::Message& message, const std::vector<std::string>& metaKeys, const std::vector<std::string>& metaValues);
#pragma endregion
void SetDbHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void SetDbIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);
void HandleDbCompletedQueueMessage(entt::registry& registry, entt::entity nodeEntity, grpc::CompletionQueue& completeQueueComp, GrpcTag* grpcTag);
void InitDbGrpcNode(const std::shared_ptr< ::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity);
