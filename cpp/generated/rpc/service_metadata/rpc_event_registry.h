#pragma once
#include <memory>
#include <string>
#include <array>
#include <functional>
#include <vector>
#include <entt/src/entt/entity/registry.hpp>
#include <google/protobuf/message.h>
#include <google/protobuf/service.h>

struct RpcMethodMeta {
	const char* serviceName{nullptr};
	const char* methodName{nullptr};
	std::unique_ptr<::google::protobuf::Message> requestProto;
	std::unique_ptr<::google::protobuf::Message> responseProto;
	std::unique_ptr<::google::protobuf::Service> handler;
	uint32_t protocol{0};
	uint32_t targetNodeType{0};
	std::function<void (
		entt::registry& registry,
		entt::entity nodeEntity,
		const google::protobuf::Message& message,
		const std::vector<std::string>& metaKeys,
		const std::vector<std::string>& metaValues
	)> sender;
};

constexpr uint32_t kMaxRpcMethodCount = 194;
constexpr uint32_t kMaxEventCount = 48;

extern std::array<RpcMethodMeta, kMaxRpcMethodCount> gRpcMethodRegistry;

void InitMessageInfo();
bool IsValidEventId(uint32_t eventId);
bool IsClientMessageId(uint32_t messageId);
bool DispatchProtoEvent(uint32_t eventId, const std::string& payload);
