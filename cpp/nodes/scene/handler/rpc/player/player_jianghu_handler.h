#pragma once

#include <type_traits>
#include <utility>

#include "proto/scene/player_jianghu.pb.h"

#include "rpc/player_service_interface.h"

#include "macros/return_define.h"

class SceneJianghuClientPlayerHandler : public ::PlayerService
{
public:
    using PlayerService::PlayerService;

    static void GetJianghuProfile(entt::entity player,
        const ::GetJianghuProfileRequest* request,
        ::GetJianghuProfileResponse* response);
    static void SetJianghuRefuseStrangers(entt::entity player,
        const ::SetJianghuRefuseStrangersRequest* request,
        ::SetJianghuRefuseStrangersResponse* response);
    static void ClearJianghuDefeatMark(entt::entity player,
        const ::ClearJianghuDefeatMarkRequest* request,
        ::ClearJianghuDefeatMarkResponse* response);

    void CallMethod(const ::google::protobuf::MethodDescriptor* method,
        entt::entity player,
        const ::google::protobuf::Message* request,
        ::google::protobuf::Message* response) override
    {
        switch (method->index())
        {
        case 0:
			{
            static_assert(std::is_same_v<decltype(std::declval<::GetJianghuProfileResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            GetJianghuProfile(player,
                static_cast<const ::GetJianghuProfileRequest*>(request),
                static_cast<::GetJianghuProfileResponse*>(response));
			}
            break;
        case 1:
			{
            static_assert(std::is_same_v<decltype(std::declval<::SetJianghuRefuseStrangersResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            SetJianghuRefuseStrangers(player,
                static_cast<const ::SetJianghuRefuseStrangersRequest*>(request),
                static_cast<::SetJianghuRefuseStrangersResponse*>(response));
			}
            break;
        case 2:
			{
            static_assert(std::is_same_v<decltype(std::declval<::ClearJianghuDefeatMarkResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            ClearJianghuDefeatMark(player,
                static_cast<const ::ClearJianghuDefeatMarkRequest*>(request),
                static_cast<::ClearJianghuDefeatMarkResponse*>(response));
			}
            break;
        default:
            break;
        }
    }

};
