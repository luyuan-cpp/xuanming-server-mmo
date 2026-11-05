#include "router_muduo_forward.h"

#include <chrono>
#include <cstdint>
#include <optional>
#include <string>
#include <utility>

#include "muduo/base/Logging.h"

#include "core/utils/encode/base64.h"
#include "muduo_grpc/channel.h"
#include "network/network_utils.h"
#include "node/system/grpc_call_deadline.h"
#include "node/system/node/muduo_grpc_channel_slot.h"
#include "node/system/node/node_util.h"
#include "proto/client_rpc_router/client_rpc_router.pb.h"
#include "proto/common/base/message.pb.h"
#include "proto/common/base/node.pb.h"
#include "proto/common/base/session.pb.h"
#include "rpc_reply_bridge.h"
#include "core/security/caller_auth.h"
#include "rpc/service_metadata/rpc_event_registry.h"
#include "session/system/session.h"
#include "thread_context/node_context_manager.h"

namespace gate_router_muduo
{

namespace
{

// 与生成客户端的 client_rpc_router::kClientRpcRouterForwardGrpcPath 逐字一致(proto package client_rpc_router,线上契约)。
constexpr char kForwardPath[] = "/client_rpc_router.ClientRpcRouter/Forward";
// 与生成客户端日志里的 <Service>.<Method> 同形,两条出站路径的失败日志可以对照。
constexpr char kForwardMethodName[] = "ClientRpcRouter.Forward";

void LogMissingSessionEchoSampled()
{
    // 路由服对成功与信封错误都会在响应头回显 x-session-detail-bin(client-rpc-router.md D30);
    // 缺了说明对端不是预期的路由服版本,这里留一行采样证据。
    static uint64_t missingCount = 0;
    if ((missingCount++ & 0x3FF) == 0)
    {
        LOG_WARN << "Router Forward reply has no " << kSessionBinMetaKey
                 << " echo in initial metadata; reply dropped (sampled), dropped_total=" << missingCount;
    }
}

} // namespace

void SendForward(muduo_grpc::Channel &channel, const client_rpc_router::ForwardRequest &request,
                 const std::string &sessionDetailsBin)
{
    muduo_grpc::UnaryCallRequest call;
    call.method = kForwardPath;
    call.message = request.SerializeAsString();
    call.deadline = std::chrono::steady_clock::now() +
                    grpc_call_deadline::Get(eNodeType::ClientRpcRouterNodeService);
    // 应用层值 = 标准 base64(带 padding)的 SessionDetails 原始字节 —— 与生成客户端
    // (grpc_async_client.cpp.tmpl 的 Start<Svc><Method>Call)的 Base64Encode 同一层;线上的 -bin 编码由传输层再做一层。
    // 两层都不能省(§4.2);字节本身原样转发,不解析、不重新序列化(路由 lane 的字段要原值往返)。
    call.metadata.emplace_back(kSessionBinMetaKey, Base64Encode(sessionDetailsBin));
	const auto messageId = request.request().message_id();
	if (messageId >= gRpcMethodRegistry.size() ||
		!caller_auth::Append(call.metadata, gRpcMethodRegistry[messageId].grpcFullMethod,
			sessionDetailsBin, std::getenv(caller_auth::kSecretEnv)))
	{
		gate_rpc_reply_bridge::NotifyCallFailure(kForwardMethodName,
			static_cast<int>(muduo_grpc::StatusCode::kInternal), "内部会话签名失败", &sessionDetailsBin, nullptr);
		return;
	}

    channel.StartUnaryCall(
        std::move(call),
        [sentSessionDetails = sessionDetailsBin](muduo_grpc::UnaryCallResult &&result)
        {
            if (!result.status.ok())
            {
                gate_rpc_reply_bridge::NotifyCallFailure(kForwardMethodName,
                                                         static_cast<int>(result.status.code_value()),
                                                         result.status.message(), &sentSessionDetails, nullptr);
                return;
            }
            MessageContent reply;
            if (!reply.ParseFromString(result.response))
            {
                // 生成客户端应答解析失败同样报 INTERNAL 并走失败分支,这里保持一致。
                gate_rpc_reply_bridge::NotifyCallFailure(
                    kForwardMethodName, static_cast<int>(muduo_grpc::StatusCode::kInternal),
                    "failed to parse MessageContent from Forward response", &sentSessionDetails, nullptr);
                return;
            }
            // 按**响应头回显**的会话交成功桥(不是本地保存的那份)。
            const std::string *echoed = muduo_grpc::FindMetadata(result.initialMetadata, kSessionBinMetaKey);
            if (echoed == nullptr)
            {
                LogMissingSessionEchoSampled();
                return;
            }
            const auto replySession = ParseSessionDetailsMetadataValue(*echoed);
            if (!replySession)
            {
                return;
            }
            gate_rpc_reply_bridge::DeliverReply(*replySession, reply, nullptr);
        });
}

} // namespace gate_router_muduo
