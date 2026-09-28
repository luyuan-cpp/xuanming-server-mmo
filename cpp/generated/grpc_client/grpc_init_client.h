#pragma once

#include <chrono>
#include <cstdint>
#include <functional>
#include "entt/src/entt/entity/registry.hpp"
#include <grpcpp/grpcpp.h>
#include <google/protobuf/message.h>
#include "grpc_client/grpc_call_tag.h"

using grpc::ClientContext;
using grpc::Status;
using grpc::ClientAsyncResponseReader;

void SetIfEmptyHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);

void SetHandler(const std::function<void(const ClientContext&, const ::google::protobuf::Message& reply)>& handler);

// 失败处理器(unary 调用以非 OK 状态结束时),与 SetIfEmptyHandler / SetHandler 平行。流式方法不受影响。
void SetIfEmptyFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler);

void SetFailedHandler(const std::function<void(const GrpcCallFailure&, const ::google::protobuf::Message& request)>& handler);

// 发往 nodeType 类节点的所有 unary 调用此后使用的 deadline(流式调用不设)。
// 由 grpc_call_deadline::Apply 在 Node::Initialize 里按 BaseDeployConfig.grpc_client 调用。
void SetGrpcCallDeadline(uint32_t nodeType, std::chrono::milliseconds deadline);

void HandleCompletedQueueMessage(entt::registry& registry);

void InitGrpcNode(const std::shared_ptr< ::grpc::ChannelInterface>& channel, entt::registry& registry, entt::entity nodeEntity);