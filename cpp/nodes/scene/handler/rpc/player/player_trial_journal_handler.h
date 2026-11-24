#pragma once

#include <type_traits>
#include <utility>

#include "proto/scene/player_trial_journal.pb.h"

#include "rpc/player_service_interface.h"

#include "macros/return_define.h"

class SceneTrialJournalClientPlayerHandler : public ::PlayerService
{
public:
    using PlayerService::PlayerService;

    static void GetTrialJournal(entt::entity player,
        const ::GetTrialJournalRequest* request,
        ::GetTrialJournalResponse* response);
    static void ClaimTrialJournalMilestone(entt::entity player,
        const ::ClaimTrialJournalMilestoneRequest* request,
        ::ClaimTrialJournalMilestoneResponse* response);
    static void RedeemBoundArtifactVoucher(entt::entity player,
        const ::RedeemBoundArtifactVoucherRequest* request,
        ::RedeemBoundArtifactVoucherResponse* response);

    void CallMethod(const ::google::protobuf::MethodDescriptor* method,
        entt::entity player,
        const ::google::protobuf::Message* request,
        ::google::protobuf::Message* response) override
    {
        switch (method->index())
        {
        case 0:
			{
            static_assert(std::is_same_v<decltype(std::declval<::GetTrialJournalResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            GetTrialJournal(player,
                static_cast<const ::GetTrialJournalRequest*>(request),
                static_cast<::GetTrialJournalResponse*>(response));
			}
            break;
        case 1:
			{
            static_assert(std::is_same_v<decltype(std::declval<::ClaimTrialJournalMilestoneResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            ClaimTrialJournalMilestone(player,
                static_cast<const ::ClaimTrialJournalMilestoneRequest*>(request),
                static_cast<::ClaimTrialJournalMilestoneResponse*>(response));
			}
            break;
        case 2:
			{
            static_assert(std::is_same_v<decltype(std::declval<::RedeemBoundArtifactVoucherResponse&>().mutable_error_message()), ::TipInfoMessage*>,
                "玩家 RPC 非 Empty 应答必须提供 TipInfoMessage error_message");
            RedeemBoundArtifactVoucher(player,
                static_cast<const ::RedeemBoundArtifactVoucherRequest*>(request),
                static_cast<::RedeemBoundArtifactVoucherResponse*>(response));
			}
            break;
        default:
            break;
        }
    }

};
