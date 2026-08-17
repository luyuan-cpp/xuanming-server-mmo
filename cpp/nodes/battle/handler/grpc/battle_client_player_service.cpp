#include "battle_client_player_service.h"

#include <future>
#include <limits>
#include <string>
#include <vector>

#include "muduo/base/Logging.h"

#include "core/utils/encode/base64.h"
#include "network/network_utils.h" // kSessionBinMetaKey = "x-session-detail-bin"

#include "logic/battle_room_manager.h"
#include "proto/common/base/session.pb.h"

namespace
{

    // 从请求 metadata 解出 gate 注入的会话身份。
    // 值是 gate 侧 AddMetadata(key, Base64Encode(SessionDetails 序列化)) 的产物
    // (见 cpp/generated/grpc_client 的发送封装),所以这里先 Base64 再 protobuf,
    // 与 go/login SessionInterceptor 的解法一致。
    // rawValue 原样带出,用于回写响应 initial metadata。
    bool ReadSessionDetails(const grpc::ServerContext &context,
                            ::SessionDetails &sessionDetails, std::string &rawValue)
    {
        const auto &metadata = context.client_metadata();
        const auto it = metadata.find(kSessionBinMetaKey);
        if (it == metadata.end())
        {
            return false;
        }

        rawValue.assign(it->second.data(), it->second.size());
        const std::vector<uint8_t> decoded = Base64Decode(rawValue);
        if (decoded.empty() || decoded.size() > static_cast<size_t>(std::numeric_limits<int>::max()))
        {
            LOG_ERROR << "battle 会话 metadata Base64 解码失败, size=" << rawValue.size();
            return false;
        }

        if (!sessionDetails.ParseFromArray(decoded.data(), static_cast<int>(decoded.size())))
        {
            LOG_ERROR << "battle 会话 metadata SessionDetails 反序列化失败";
            return false;
        }
        return true;
    }

} // namespace

BattleClientPlayerGrpcImpl::BattleClientPlayerGrpcImpl(muduo::net::EventLoop &loop)
    : loop_(loop)
{
}

grpc::Status BattleClientPlayerGrpcImpl::SubmitBattleAction(grpc::ServerContext *context,
    const ::SubmitBattleActionRequest *request,
    ::SubmitBattleActionResponse *response)
{
    ::SessionDetails sessionDetails;
    std::string rawMeta;
    if (!ReadSessionDetails(*context, sessionDetails, rawMeta))
    {
        // 权威身份缺失即不可信来源,fail-closed;正常拓扑下 gate 一定注入
        return grpc::Status(grpc::StatusCode::UNAUTHENTICATED, "missing x-session-detail-bin");
    }
    // 回写会话头:gate 的响应桥接(SetIfEmptyHandler)靠它找回客户端会话
    context->AddInitialMetadata(kSessionBinMetaKey, rawMeta);

    std::promise<void> promise;
    auto future = promise.get_future();

    loop_.runInLoop([request, response, &sessionDetails, &promise]
                    {
        BattleRoomManager::Instance().HandleSubmitBattleAction(sessionDetails, *request, *response);
        promise.set_value(); });

    future.get();
    return grpc::Status::OK;
}

grpc::Status BattleClientPlayerGrpcImpl::GetBattleState(grpc::ServerContext *context,
    const ::GetBattleStateRequest *request,
    ::BattleStateS2C *response)
{
    ::SessionDetails sessionDetails;
    std::string rawMeta;
    if (!ReadSessionDetails(*context, sessionDetails, rawMeta))
    {
        return grpc::Status(grpc::StatusCode::UNAUTHENTICATED, "missing x-session-detail-bin");
    }
    context->AddInitialMetadata(kSessionBinMetaKey, rawMeta);

    std::promise<void> promise;
    auto future = promise.get_future();

    loop_.runInLoop([request, response, &sessionDetails, &promise]
                    {
        BattleRoomManager::Instance().HandleGetBattleState(sessionDetails, *request, *response);
        promise.set_value(); });

    future.get();
    return grpc::Status::OK;
}
