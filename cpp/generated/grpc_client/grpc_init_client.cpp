#include "grpc_init_client.h"

#include <chrono>
#include <cstdint>
#include <proto/common/base/node.pb.h>
#include <proto/common/base/common.pb.h>

    void SetBattleNodeCallDeadline(std::chrono::milliseconds deadline);

    void SetPlayerBattleCallDeadline(std::chrono::milliseconds deadline);

namespace chatpb {
    void SetChatCallDeadline(std::chrono::milliseconds deadline);
}

namespace client_rpc_router {
    void SetClientRpcRouterCallDeadline(std::chrono::milliseconds deadline);
}

namespace data_service {
    void SetDataServiceCallDeadline(std::chrono::milliseconds deadline);
}

namespace etcdserverpb {
    void SetEtcdCallDeadline(std::chrono::milliseconds deadline);
}

namespace friendpb {
    void SetFriendCallDeadline(std::chrono::milliseconds deadline);
}

namespace guildpb {
    void SetGuildCallDeadline(std::chrono::milliseconds deadline);
}

namespace guildpb {
    void SetGuildInternalCallDeadline(std::chrono::milliseconds deadline);
}

namespace loginpb {
    void SetLoginCallDeadline(std::chrono::milliseconds deadline);
}

namespace mailpb {
    void SetMailCallDeadline(std::chrono::milliseconds deadline);
}

namespace mailpb {
    void SetMailAdminCallDeadline(std::chrono::milliseconds deadline);
}

namespace match {
    void SetMatchInternalCallDeadline(std::chrono::milliseconds deadline);
}

namespace match {
    void SetMatchServiceCallDeadline(std::chrono::milliseconds deadline);
}

namespace player_inbox {
    void SetPlayerInboxCallDeadline(std::chrono::milliseconds deadline);
}

namespace player_inbox {
    void SetPlayerInboxAdminCallDeadline(std::chrono::milliseconds deadline);
}

namespace playerlocator {
    void SetPlayerLocatorCallDeadline(std::chrono::milliseconds deadline);
}

namespace playerstore {
    void SetPlayerStoreCallDeadline(std::chrono::milliseconds deadline);
}

namespace rankpb {
    void SetRankCallDeadline(std::chrono::milliseconds deadline);
}

namespace rankpb {
    void SetRankAdminCallDeadline(std::chrono::milliseconds deadline);
}

namespace scene_manager {
    void SetSceneManagerServiceCallDeadline(std::chrono::milliseconds deadline);
}

namespace scene_node {
    void SetSceneNodeServiceCallDeadline(std::chrono::milliseconds deadline);
}

namespace teampb {
    void SetTeamCallDeadline(std::chrono::milliseconds deadline);
}

namespace teampb {
    void SetTeamAuthorityCallDeadline(std::chrono::milliseconds deadline);
}

namespace trade {
    void SetJubaozhaiCallDeadline(std::chrono::milliseconds deadline);
}

namespace trade {
    void SetTradeAdminCallDeadline(std::chrono::milliseconds deadline);
}

void SetGrpcCallDeadline(uint32_t nodeType, std::chrono::milliseconds deadline){
    if (static_cast<uint32_t>(common::base::eNodeType::BattleNodeService) == nodeType) {
        ::SetBattleNodeCallDeadline(deadline);
    }
    if (static_cast<uint32_t>(common::base::eNodeType::BattleNodeService) == nodeType) {
        ::SetPlayerBattleCallDeadline(deadline);
    }
    if (static_cast<uint32_t>(common::base::eNodeType::ChatNodeService) == nodeType) {
        chatpb::SetChatCallDeadline(deadline);
    }
    if (static_cast<uint32_t>(common::base::eNodeType::ClientRpcRouterNodeService) == nodeType) {
        client_rpc_router::SetClientRpcRouterCallDeadline(deadline);
    }
    if (static_cast<uint32_t>(common::base::eNodeType::DataServiceNodeService) == nodeType) {
        data_service::SetDataServiceCallDeadline(deadline);
    }
    if (static_cast<uint32_t>(common::base::eNodeType::EtcdNodeService) == nodeType) {
        etcdserverpb::SetEtcdCallDeadline(deadline);
    }
    if (static_cast<uint32_t>(common::base::eNodeType::FriendNodeService) == nodeType) {
        friendpb::SetFriendCallDeadline(deadline);
    }
    if (static_cast<uint32_t>(common::base::eNodeType::GuildNodeService) == nodeType) {
        guildpb::SetGuildCallDeadline(deadline);
    }
    if (static_cast<uint32_t>(common::base::eNodeType::GuildNodeService) == nodeType) {
        guildpb::SetGuildInternalCallDeadline(deadline);
    }
    if (static_cast<uint32_t>(common::base::eNodeType::LoginNodeService) == nodeType) {
        loginpb::SetLoginCallDeadline(deadline);
    }
    if (static_cast<uint32_t>(common::base::eNodeType::MailNodeService) == nodeType) {
        mailpb::SetMailCallDeadline(deadline);
    }
    if (static_cast<uint32_t>(common::base::eNodeType::MailNodeService) == nodeType) {
        mailpb::SetMailAdminCallDeadline(deadline);
    }
    if (static_cast<uint32_t>(common::base::eNodeType::MatchNodeService) == nodeType) {
        match::SetMatchInternalCallDeadline(deadline);
    }
    if (static_cast<uint32_t>(common::base::eNodeType::MatchNodeService) == nodeType) {
        match::SetMatchServiceCallDeadline(deadline);
    }
    if (static_cast<uint32_t>(common::base::eNodeType::PlayerInboxNodeService) == nodeType) {
        player_inbox::SetPlayerInboxCallDeadline(deadline);
    }
    if (static_cast<uint32_t>(common::base::eNodeType::PlayerInboxNodeService) == nodeType) {
        player_inbox::SetPlayerInboxAdminCallDeadline(deadline);
    }
    if (static_cast<uint32_t>(common::base::eNodeType::PlayerLocatorNodeService) == nodeType) {
        playerlocator::SetPlayerLocatorCallDeadline(deadline);
    }
    if (static_cast<uint32_t>(common::base::eNodeType::PlayerStoreNodeService) == nodeType) {
        playerstore::SetPlayerStoreCallDeadline(deadline);
    }
    if (static_cast<uint32_t>(common::base::eNodeType::RankNodeService) == nodeType) {
        rankpb::SetRankCallDeadline(deadline);
    }
    if (static_cast<uint32_t>(common::base::eNodeType::RankNodeService) == nodeType) {
        rankpb::SetRankAdminCallDeadline(deadline);
    }
    if (static_cast<uint32_t>(common::base::eNodeType::SceneManagerNodeService) == nodeType) {
        scene_manager::SetSceneManagerServiceCallDeadline(deadline);
    }
    if (static_cast<uint32_t>(common::base::eNodeType::SceneManagerNodeService) == nodeType) {
        scene_node::SetSceneNodeServiceCallDeadline(deadline);
    }
    if (static_cast<uint32_t>(common::base::eNodeType::TeamNodeService) == nodeType) {
        teampb::SetTeamCallDeadline(deadline);
    }
    if (static_cast<uint32_t>(common::base::eNodeType::TeamNodeService) == nodeType) {
        teampb::SetTeamAuthorityCallDeadline(deadline);
    }
    if (static_cast<uint32_t>(common::base::eNodeType::TradeNodeService) == nodeType) {
        trade::SetJubaozhaiCallDeadline(deadline);
    }
    if (static_cast<uint32_t>(common::base::eNodeType::TradeNodeService) == nodeType) {
        trade::SetTradeAdminCallDeadline(deadline);
    }
}
