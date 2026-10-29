#pragma once
#include <memory>
#include <string>
#include <array>
#include <functional>
#include <vector>
#include <entt/src/entt/entity/registry.hpp>
#include <google/protobuf/message.h>
#include "core/network/grpc_client_call.h"
// RpcMethodMeta 已不含 Service 成员;service.h 仍保留:多处靠本头传递拿到 Service / Closure / RpcController。
#include <google/protobuf/service.h>

struct RpcMethodMeta {
	const char* serviceName{nullptr};
	const char* methodName{nullptr};
	std::unique_ptr<::google::protobuf::Message> requestProto;
	std::unique_ptr<::google::protobuf::Message> responseProto;
	uint32_t protocol{0};
	uint32_t targetNodeType{0};
	std::function<void (
		entt::registry& registry,
		entt::entity nodeEntity,
		const google::protobuf::Message& message,
		GrpcMessageDone done,
		const GrpcCallOptions& options
	)> sender;
};

constexpr uint32_t kMaxRpcMethodCount = 346;
constexpr uint32_t kMaxEventCount = 51;

extern std::array<RpcMethodMeta, kMaxRpcMethodCount> gRpcMethodRegistry;

void InitMessageInfo();
bool IsValidEventId(uint32_t eventId);
bool IsClientMessageId(uint32_t messageId);
