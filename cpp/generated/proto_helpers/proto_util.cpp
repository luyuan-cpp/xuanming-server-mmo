#include "proto_util.h"
#include <unordered_set>
#include "proto/common/base/node.pb.h"

bool IsTcpNodeType(uint32_t nodeType) {
	static const std::unordered_set<uint32_t> validTypes = {
		common::base::eNodeType::UnknownNodeService,
		common::base::eNodeType::SceneNodeService,
		common::base::eNodeType::GateNodeService,
		common::base::eNodeType::LoginNodeService,
		common::base::eNodeType::RedisNodeService,
		common::base::eNodeType::EtcdNodeService,
		common::base::eNodeType::MailNodeService,
		common::base::eNodeType::ChatNodeService,
		common::base::eNodeType::TeamNodeService,
		common::base::eNodeType::ActivityNodeService,
		common::base::eNodeType::TradeNodeService,
		common::base::eNodeType::RankNodeService,
		common::base::eNodeType::TaskNodeService,
		common::base::eNodeType::GuildNodeService,
		common::base::eNodeType::MatchNodeService,
		common::base::eNodeType::AiNodeService,
		common::base::eNodeType::LogNodeService,
		common::base::eNodeType::PaymentNodeService,
		common::base::eNodeType::SecurityNodeService,
		common::base::eNodeType::CrossServerNodeService,
		common::base::eNodeType::AnalyticsNodeService,
		common::base::eNodeType::GmNodeService,
		common::base::eNodeType::PlayerLocatorNodeService,
		common::base::eNodeType::SceneManagerNodeService,
		common::base::eNodeType::DataServiceNodeService,
		common::base::eNodeType::FriendNodeService,
		common::base::eNodeType::BattleNodeService,
	};

	return validTypes.contains(nodeType);
}