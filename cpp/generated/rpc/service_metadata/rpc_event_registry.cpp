#include <array>
#include "rpc_event_registry.h"
#include "proto/common/base/node.pb.h"
#include "thread_context/ecs_context.h"

#include "proto/battle/player_battle.pb.h"
#include "proto/battle/battle_node.pb.h"
#include "proto/chat/chat.pb.h"
#include "proto/client_rpc_router/client_rpc_router.pb.h"
#include "proto/data_service/data_service.pb.h"
#include "proto/etcd/etcd.pb.h"
#include "proto/friend/friend.pb.h"
#include "proto/gate/gate_service.pb.h"
#include "proto/guild/guild.pb.h"
#include "proto/login/login.pb.h"
#include "proto/match/match_service.pb.h"
#include "proto/scene/client_player_common.pb.h"
#include "proto/scene/player_attribute.pb.h"
#include "proto/scene/player_currency.pb.h"
#include "proto/scene/player_lifecycle.pb.h"
#include "proto/scene/player_movement.pb.h"
#include "proto/scene/player_pet.pb.h"
#include "proto/scene/player_rollback.pb.h"
#include "proto/scene/player_scene.pb.h"
#include "proto/scene/player_skill.pb.h"
#include "proto/scene/player_state_attribute_sync.pb.h"
#include "proto/scene/s2s_player_scene.pb.h"
#include "proto/scene/scene.pb.h"
#include "proto/scene/scene_admin.pb.h"
#include "proto/scene_manager/scene_manager_service.pb.h"
#include "proto/scene_manager/scene_node_service.pb.h"

#include "rpc/service_metadata/player_battle_service_metadata.h"
#include "rpc/service_metadata/battle_node_service_metadata.h"
#include "rpc/service_metadata/chat_service_metadata.h"
#include "rpc/service_metadata/client_rpc_router_service_metadata.h"
#include "rpc/service_metadata/data_service_service_metadata.h"
#include "rpc/service_metadata/etcd_service_metadata.h"
#include "rpc/service_metadata/friend_service_metadata.h"
#include "rpc/service_metadata/gate_service_service_metadata.h"
#include "rpc/service_metadata/guild_service_metadata.h"
#include "rpc/service_metadata/login_service_metadata.h"
#include "rpc/service_metadata/match_service_service_metadata.h"
#include "rpc/service_metadata/client_player_common_service_metadata.h"
#include "rpc/service_metadata/player_attribute_service_metadata.h"
#include "rpc/service_metadata/player_currency_service_metadata.h"
#include "rpc/service_metadata/player_lifecycle_service_metadata.h"
#include "rpc/service_metadata/player_movement_service_metadata.h"
#include "rpc/service_metadata/player_pet_service_metadata.h"
#include "rpc/service_metadata/player_rollback_service_metadata.h"
#include "rpc/service_metadata/player_scene_service_metadata.h"
#include "rpc/service_metadata/player_skill_service_metadata.h"
#include "rpc/service_metadata/player_state_attribute_sync_service_metadata.h"
#include "rpc/service_metadata/s2s_player_scene_service_metadata.h"
#include "rpc/service_metadata/scene_service_metadata.h"
#include "rpc/service_metadata/scene_admin_service_metadata.h"
#include "rpc/service_metadata/scene_manager_service_service_metadata.h"
#include "rpc/service_metadata/scene_node_service_service_metadata.h"

#include "proto/common/event/mission_event.pb.h"
#include "proto/common/event/scene_event.pb.h"
#include "proto/common/event/battle_event.pb.h"
#include "proto/common/event/combat_event.pb.h"
#include "proto/common/event/buff_event.pb.h"
#include "proto/common/event/actor_combat_state_event.pb.h"
#include "proto/common/event/node_event.pb.h"
#include "proto/contracts/kafka/match_event.pb.h"
#include "proto/contracts/kafka/gate_event.pb.h"
#include "proto/contracts/kafka/player_event.pb.h"
#include "proto/common/event/actor_event.pb.h"
#include "proto/common/event/npc_event.pb.h"
#include "proto/common/event/player_event.pb.h"
#include "proto/common/event/server_event.pb.h"
#include "proto/common/event/player_migration_event.pb.h"
#include "proto/common/event/skill_event.pb.h"
#include "common_event_mission_event_event_id.h"
#include "common_event_scene_event_event_id.h"
#include "common_event_battle_event_event_id.h"
#include "common_event_combat_event_event_id.h"
#include "common_event_buff_event_event_id.h"
#include "common_event_actor_combat_state_event_event_id.h"
#include "common_event_node_event_event_id.h"
#include "contracts_kafka_match_event_event_id.h"
#include "contracts_kafka_gate_event_event_id.h"
#include "contracts_kafka_player_event_event_id.h"
#include "common_event_actor_event_event_id.h"
#include "common_event_npc_event_event_id.h"
#include "common_event_player_event_event_id.h"
#include "common_event_server_event_event_id.h"
#include "common_event_player_migration_event_event_id.h"
#include "common_event_skill_event_event_id.h"

class GateImpl final : public Gate {};
class SceneClientPlayerCommonImpl final : public SceneClientPlayerCommon {};
class SceneAttributeClientPlayerImpl final : public SceneAttributeClientPlayer {};
class SceneCurrencyClientPlayerImpl final : public SceneCurrencyClientPlayer {};
class ScenePlayerImpl final : public ScenePlayer {};
class SceneMovementClientPlayerImpl final : public SceneMovementClientPlayer {};
class ScenePetClientPlayerImpl final : public ScenePetClientPlayer {};
class SceneRollbackClientPlayerImpl final : public SceneRollbackClientPlayer {};
class SceneSceneClientPlayerImpl final : public SceneSceneClientPlayer {};
class SceneSkillClientPlayerImpl final : public SceneSkillClientPlayer {};
class ScenePlayerSyncImpl final : public ScenePlayerSync {};
class SceneScenePlayerImpl final : public SceneScenePlayer {};
class SceneImpl final : public Scene {};
class SceneSceneImpl final : public SceneScene {};

void SendBattleClientPlayerSubmitBattleAction(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );
void SendBattleClientPlayerGetBattleState(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );
void SendBattleClientPlayerNotifyBattleStart(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );
void SendBattleClientPlayerNotifyTurnResult(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );
void SendBattleClientPlayerNotifyBattleEnd(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );
void SendBattleClientPlayerNotifyBattleReconnect(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );
void SendBattleClientPlayerStopWatchBattle(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );
void SendBattleClientPlayerSetAutoBattle(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );
void SendBattleClientPlayerNotifySpectateState(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );
void SendBattleClientPlayerNotifySpectateTurnResult(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );
void SendBattleClientPlayerNotifySpectateEnd(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );
void SendBattleClientPlayerNotifyBattleAssigned(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );
void SendBattleNodeCreateBattle(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );
void SendBattleNodeDestroyBattle(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );
void SendBattleNodeAddObserver(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );
void SendBattleNodeRemoveObserver(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );
void SendBattleNodeIssueBattleTicket(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );
namespace chatpb{void SendClientPlayerChatSendChat(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace chatpb{void SendClientPlayerChatPullChatHistory(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace client_rpc_router{void SendClientRpcRouterForward(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace data_service{void SendDataServiceLoadPlayerData(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace data_service{void SendDataServiceSavePlayerData(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace data_service{void SendDataServiceGetPlayerField(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace data_service{void SendDataServiceSetPlayerField(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace data_service{void SendDataServiceRegisterPlayerZone(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace data_service{void SendDataServiceGetPlayerHomeZone(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace data_service{void SendDataServiceBatchGetPlayerHomeZone(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace data_service{void SendDataServiceRemapHomeZoneForMerge(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace data_service{void SendDataServiceDeletePlayerData(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace data_service{void SendDataServiceCreatePlayerSnapshot(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace data_service{void SendDataServiceListPlayerSnapshots(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace data_service{void SendDataServiceGetPlayerSnapshotDiff(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace data_service{void SendDataServiceRollbackPlayer(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace data_service{void SendDataServiceRollbackZone(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace data_service{void SendDataServiceRollbackAll(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace data_service{void SendDataServiceBatchRecallItems(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace data_service{void SendDataServiceQueryTransactionLog(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace data_service{void SendDataServiceCreateEventSnapshot(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace data_service{void SendDataServiceAllocateIdSegment(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace etcdserverpb{void SendKVRange(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace etcdserverpb{void SendKVPut(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace etcdserverpb{void SendKVDeleteRange(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace etcdserverpb{void SendKVTxn(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace etcdserverpb{void SendKVCompact(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace etcdserverpb{void SendWatchWatch(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace etcdserverpb{void SendLeaseLeaseGrant(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace etcdserverpb{void SendLeaseLeaseRevoke(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace etcdserverpb{void SendLeaseLeaseKeepAlive(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace etcdserverpb{void SendLeaseLeaseTimeToLive(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace etcdserverpb{void SendLeaseLeaseLeases(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace friendpb{void SendFriendServiceAddFriend(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace friendpb{void SendFriendServiceAcceptFriend(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace friendpb{void SendFriendServiceRejectFriend(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace friendpb{void SendFriendServiceRemoveFriend(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace friendpb{void SendFriendServiceGetFriendList(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace friendpb{void SendFriendServiceGetPendingRequests(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace friendpb{void SendFriendServiceNotifyOnline(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace friendpb{void SendFriendServiceNotifyOffline(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace guildpb{void SendGuildServiceCreateGuild(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace guildpb{void SendGuildServiceGetGuild(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace guildpb{void SendGuildServiceGetPlayerGuild(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace guildpb{void SendGuildServiceJoinGuild(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace guildpb{void SendGuildServiceLeaveGuild(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace guildpb{void SendGuildServiceDisbandGuild(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace guildpb{void SendGuildServiceSetAnnouncement(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace guildpb{void SendGuildServiceUpdateGuildScore(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace guildpb{void SendGuildServiceGetGuildRank(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace guildpb{void SendGuildServiceGetGuildRankByGuild(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace loginpb{void SendClientPlayerLoginLogin(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace loginpb{void SendClientPlayerLoginCreatePlayer(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace loginpb{void SendClientPlayerLoginEnterGame(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace loginpb{void SendClientPlayerLoginLeaveGame(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace loginpb{void SendClientPlayerLoginDisconnect(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace loginpb{void SendClientPlayerLoginRefreshToken(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace loginpb{void SendLoginPreGateAssignGate(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace loginpb{void SendLoginPreGateQueryQueueStatus(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace loginpb{void SendLoginAdminRemovePlayersFromAccounts(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace match{void SendMatchServiceJoinQueue(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace match{void SendMatchServiceCancelQueue(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace match{void SendMatchServiceGetQueueStatus(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace match{void SendMatchServiceChallengePlayer(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace match{void SendMatchServiceRespondChallenge(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace match{void SendMatchServiceNotifyChallengeInvite(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace match{void SendMatchServiceNotifyChallengeResult(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace match{void SendMatchServiceWatchBattle(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace match{void SendMatchServiceRequestBattleTicket(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace match{void SendMatchServiceListWatchableBattles(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace scene_manager{void SendSceneManagerCreateScene(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace scene_manager{void SendSceneManagerDestroyScene(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace scene_manager{void SendSceneManagerEnterScene(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace scene_manager{void SendSceneManagerLeaveScene(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace scene_node{void SendSceneNodeGrpcCreateScene(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace scene_node{void SendSceneNodeGrpcDestroyScene(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace scene_node{void SendSceneNodeGrpcReleasePlayer(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace scene_node{void SendSceneNodeGrpcPrepareBattle(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}
namespace scene_node{void SendSceneNodeGrpcCancelBattlePrepare(entt::registry& , entt::entity , const google::protobuf::Message& , const std::vector<std::string>& , const std::vector<std::string>& );}

// 容量以 rpc_event_registry.h 的 kMaxRpcMethodCount 为准;static_assert 把
// "半途 regen 导致头文件容量落后于本轮 message id 数"的事故(2026-09-01,
// InitMessageInfo 越界写导致节点启动断言)变成编译错误而不是运行期崩溃。
static_assert(kMaxRpcMethodCount == 190,
    "kMaxRpcMethodCount out of sync with this generation run - rerun the full proto generator");
std::array<RpcMethodMeta, kMaxRpcMethodCount> gRpcMethodRegistry;

void InitMessageInfo()
{
    // --- BattleClientPlayer ---
    gRpcMethodRegistry[BattleClientPlayerSubmitBattleActionMessageId] = RpcMethodMeta{
        "BattleClientPlayer", "SubmitBattleAction",
        std::make_unique<::SubmitBattleActionRequest>(),
        std::make_unique<::SubmitBattleActionResponse>(),
        nullptr, 1, common::base::eNodeType::BattleNodeService, ::SendBattleClientPlayerSubmitBattleAction};
    gRpcMethodRegistry[BattleClientPlayerGetBattleStateMessageId] = RpcMethodMeta{
        "BattleClientPlayer", "GetBattleState",
        std::make_unique<::GetBattleStateRequest>(),
        std::make_unique<::BattleStateS2C>(),
        nullptr, 1, common::base::eNodeType::BattleNodeService, ::SendBattleClientPlayerGetBattleState};
    gRpcMethodRegistry[BattleClientPlayerNotifyBattleStartMessageId] = RpcMethodMeta{
        "BattleClientPlayer", "NotifyBattleStart",
        std::make_unique<::BattleStartS2C>(),
        std::make_unique<::Empty>(),
        nullptr, 1, common::base::eNodeType::BattleNodeService, ::SendBattleClientPlayerNotifyBattleStart};
    gRpcMethodRegistry[BattleClientPlayerNotifyTurnResultMessageId] = RpcMethodMeta{
        "BattleClientPlayer", "NotifyTurnResult",
        std::make_unique<::TurnResultS2C>(),
        std::make_unique<::Empty>(),
        nullptr, 1, common::base::eNodeType::BattleNodeService, ::SendBattleClientPlayerNotifyTurnResult};
    gRpcMethodRegistry[BattleClientPlayerNotifyBattleEndMessageId] = RpcMethodMeta{
        "BattleClientPlayer", "NotifyBattleEnd",
        std::make_unique<::BattleEndS2C>(),
        std::make_unique<::Empty>(),
        nullptr, 1, common::base::eNodeType::BattleNodeService, ::SendBattleClientPlayerNotifyBattleEnd};
    gRpcMethodRegistry[BattleClientPlayerNotifyBattleReconnectMessageId] = RpcMethodMeta{
        "BattleClientPlayer", "NotifyBattleReconnect",
        std::make_unique<::BattleReconnectS2C>(),
        std::make_unique<::Empty>(),
        nullptr, 1, common::base::eNodeType::BattleNodeService, ::SendBattleClientPlayerNotifyBattleReconnect};
    gRpcMethodRegistry[BattleClientPlayerStopWatchBattleMessageId] = RpcMethodMeta{
        "BattleClientPlayer", "StopWatchBattle",
        std::make_unique<::StopWatchBattleRequest>(),
        std::make_unique<::StopWatchBattleResponse>(),
        nullptr, 1, common::base::eNodeType::BattleNodeService, ::SendBattleClientPlayerStopWatchBattle};
    gRpcMethodRegistry[BattleClientPlayerSetAutoBattleMessageId] = RpcMethodMeta{
        "BattleClientPlayer", "SetAutoBattle",
        std::make_unique<::SetAutoBattleRequest>(),
        std::make_unique<::SetAutoBattleResponse>(),
        nullptr, 1, common::base::eNodeType::BattleNodeService, ::SendBattleClientPlayerSetAutoBattle};
    gRpcMethodRegistry[BattleClientPlayerNotifySpectateStateMessageId] = RpcMethodMeta{
        "BattleClientPlayer", "NotifySpectateState",
        std::make_unique<::SpectateStateS2C>(),
        std::make_unique<::Empty>(),
        nullptr, 1, common::base::eNodeType::BattleNodeService, ::SendBattleClientPlayerNotifySpectateState};
    gRpcMethodRegistry[BattleClientPlayerNotifySpectateTurnResultMessageId] = RpcMethodMeta{
        "BattleClientPlayer", "NotifySpectateTurnResult",
        std::make_unique<::TurnResultS2C>(),
        std::make_unique<::Empty>(),
        nullptr, 1, common::base::eNodeType::BattleNodeService, ::SendBattleClientPlayerNotifySpectateTurnResult};
    gRpcMethodRegistry[BattleClientPlayerNotifySpectateEndMessageId] = RpcMethodMeta{
        "BattleClientPlayer", "NotifySpectateEnd",
        std::make_unique<::SpectateEndS2C>(),
        std::make_unique<::Empty>(),
        nullptr, 1, common::base::eNodeType::BattleNodeService, ::SendBattleClientPlayerNotifySpectateEnd};
    gRpcMethodRegistry[BattleClientPlayerNotifyBattleAssignedMessageId] = RpcMethodMeta{
        "BattleClientPlayer", "NotifyBattleAssigned",
        std::make_unique<::BattleAssignedS2C>(),
        std::make_unique<::Empty>(),
        nullptr, 1, common::base::eNodeType::BattleNodeService, ::SendBattleClientPlayerNotifyBattleAssigned};

    // --- BattleNode ---
    gRpcMethodRegistry[BattleNodeCreateBattleMessageId] = RpcMethodMeta{
        "BattleNode", "CreateBattle",
        std::make_unique<::CreateBattleRequest>(),
        std::make_unique<::CreateBattleResponse>(),
        nullptr, 1, common::base::eNodeType::BattleNodeService, ::SendBattleNodeCreateBattle};
    gRpcMethodRegistry[BattleNodeDestroyBattleMessageId] = RpcMethodMeta{
        "BattleNode", "DestroyBattle",
        std::make_unique<::DestroyBattleRequest>(),
        std::make_unique<::Empty>(),
        nullptr, 1, common::base::eNodeType::BattleNodeService, ::SendBattleNodeDestroyBattle};
    gRpcMethodRegistry[BattleNodeAddObserverMessageId] = RpcMethodMeta{
        "BattleNode", "AddObserver",
        std::make_unique<::AddObserverRequest>(),
        std::make_unique<::AddObserverResponse>(),
        nullptr, 1, common::base::eNodeType::BattleNodeService, ::SendBattleNodeAddObserver};
    gRpcMethodRegistry[BattleNodeRemoveObserverMessageId] = RpcMethodMeta{
        "BattleNode", "RemoveObserver",
        std::make_unique<::RemoveObserverRequest>(),
        std::make_unique<::Empty>(),
        nullptr, 1, common::base::eNodeType::BattleNodeService, ::SendBattleNodeRemoveObserver};
    gRpcMethodRegistry[BattleNodeIssueBattleTicketMessageId] = RpcMethodMeta{
        "BattleNode", "IssueBattleTicket",
        std::make_unique<::IssueBattleTicketRequest>(),
        std::make_unique<::IssueBattleTicketResponse>(),
        nullptr, 1, common::base::eNodeType::BattleNodeService, ::SendBattleNodeIssueBattleTicket};

    // --- ClientPlayerChat ---
    gRpcMethodRegistry[ClientPlayerChatSendChatMessageId] = RpcMethodMeta{
        "ClientPlayerChat", "SendChat",
        std::make_unique<::chatpb::SendChatRequest>(),
        std::make_unique<::chatpb::SendChatResponse>(),
        nullptr, 1, common::base::eNodeType::ChatNodeService, chatpb::SendClientPlayerChatSendChat};
    gRpcMethodRegistry[ClientPlayerChatPullChatHistoryMessageId] = RpcMethodMeta{
        "ClientPlayerChat", "PullChatHistory",
        std::make_unique<::chatpb::PullChatHistoryRequest>(),
        std::make_unique<::chatpb::PullChatHistoryResponse>(),
        nullptr, 1, common::base::eNodeType::ChatNodeService, chatpb::SendClientPlayerChatPullChatHistory};

    // --- ClientRpcRouter ---
    gRpcMethodRegistry[ClientRpcRouterForwardMessageId] = RpcMethodMeta{
        "ClientRpcRouter", "Forward",
        std::make_unique<::client_rpc_router::ForwardRequest>(),
        std::make_unique<::MessageContent>(),
        nullptr, 1, common::base::eNodeType::ClientRpcRouterNodeService, client_rpc_router::SendClientRpcRouterForward};

    // --- DataService ---
    gRpcMethodRegistry[DataServiceLoadPlayerDataMessageId] = RpcMethodMeta{
        "DataService", "LoadPlayerData",
        std::make_unique<::data_service::LoadPlayerDataRequest>(),
        std::make_unique<::data_service::LoadPlayerDataResponse>(),
        nullptr, 1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceLoadPlayerData};
    gRpcMethodRegistry[DataServiceSavePlayerDataMessageId] = RpcMethodMeta{
        "DataService", "SavePlayerData",
        std::make_unique<::data_service::SavePlayerDataRequest>(),
        std::make_unique<::data_service::SavePlayerDataResponse>(),
        nullptr, 1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceSavePlayerData};
    gRpcMethodRegistry[DataServiceGetPlayerFieldMessageId] = RpcMethodMeta{
        "DataService", "GetPlayerField",
        std::make_unique<::data_service::GetPlayerFieldRequest>(),
        std::make_unique<::data_service::GetPlayerFieldResponse>(),
        nullptr, 1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceGetPlayerField};
    gRpcMethodRegistry[DataServiceSetPlayerFieldMessageId] = RpcMethodMeta{
        "DataService", "SetPlayerField",
        std::make_unique<::data_service::SetPlayerFieldRequest>(),
        std::make_unique<::data_service::SetPlayerFieldResponse>(),
        nullptr, 1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceSetPlayerField};
    gRpcMethodRegistry[DataServiceRegisterPlayerZoneMessageId] = RpcMethodMeta{
        "DataService", "RegisterPlayerZone",
        std::make_unique<::data_service::RegisterPlayerZoneRequest>(),
        std::make_unique<::google::protobuf::Empty>(),
        nullptr, 1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceRegisterPlayerZone};
    gRpcMethodRegistry[DataServiceGetPlayerHomeZoneMessageId] = RpcMethodMeta{
        "DataService", "GetPlayerHomeZone",
        std::make_unique<::data_service::GetPlayerHomeZoneRequest>(),
        std::make_unique<::data_service::GetPlayerHomeZoneResponse>(),
        nullptr, 1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceGetPlayerHomeZone};
    gRpcMethodRegistry[DataServiceBatchGetPlayerHomeZoneMessageId] = RpcMethodMeta{
        "DataService", "BatchGetPlayerHomeZone",
        std::make_unique<::data_service::BatchGetPlayerHomeZoneRequest>(),
        std::make_unique<::data_service::BatchGetPlayerHomeZoneResponse>(),
        nullptr, 1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceBatchGetPlayerHomeZone};
    gRpcMethodRegistry[DataServiceRemapHomeZoneForMergeMessageId] = RpcMethodMeta{
        "DataService", "RemapHomeZoneForMerge",
        std::make_unique<::data_service::RemapHomeZoneForMergeRequest>(),
        std::make_unique<::data_service::RemapHomeZoneForMergeResponse>(),
        nullptr, 1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceRemapHomeZoneForMerge};
    gRpcMethodRegistry[DataServiceDeletePlayerDataMessageId] = RpcMethodMeta{
        "DataService", "DeletePlayerData",
        std::make_unique<::data_service::DeletePlayerDataRequest>(),
        std::make_unique<::data_service::DeletePlayerDataResponse>(),
        nullptr, 1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceDeletePlayerData};
    gRpcMethodRegistry[DataServiceCreatePlayerSnapshotMessageId] = RpcMethodMeta{
        "DataService", "CreatePlayerSnapshot",
        std::make_unique<::data_service::CreatePlayerSnapshotRequest>(),
        std::make_unique<::data_service::CreatePlayerSnapshotResponse>(),
        nullptr, 1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceCreatePlayerSnapshot};
    gRpcMethodRegistry[DataServiceListPlayerSnapshotsMessageId] = RpcMethodMeta{
        "DataService", "ListPlayerSnapshots",
        std::make_unique<::data_service::ListPlayerSnapshotsRequest>(),
        std::make_unique<::data_service::ListPlayerSnapshotsResponse>(),
        nullptr, 1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceListPlayerSnapshots};
    gRpcMethodRegistry[DataServiceGetPlayerSnapshotDiffMessageId] = RpcMethodMeta{
        "DataService", "GetPlayerSnapshotDiff",
        std::make_unique<::data_service::GetPlayerSnapshotDiffRequest>(),
        std::make_unique<::data_service::GetPlayerSnapshotDiffResponse>(),
        nullptr, 1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceGetPlayerSnapshotDiff};
    gRpcMethodRegistry[DataServiceRollbackPlayerMessageId] = RpcMethodMeta{
        "DataService", "RollbackPlayer",
        std::make_unique<::data_service::RollbackPlayerRequest>(),
        std::make_unique<::data_service::RollbackPlayerResponse>(),
        nullptr, 1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceRollbackPlayer};
    gRpcMethodRegistry[DataServiceRollbackZoneMessageId] = RpcMethodMeta{
        "DataService", "RollbackZone",
        std::make_unique<::data_service::RollbackZoneRequest>(),
        std::make_unique<::data_service::RollbackZoneResponse>(),
        nullptr, 1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceRollbackZone};
    gRpcMethodRegistry[DataServiceRollbackAllMessageId] = RpcMethodMeta{
        "DataService", "RollbackAll",
        std::make_unique<::data_service::RollbackAllRequest>(),
        std::make_unique<::data_service::RollbackAllResponse>(),
        nullptr, 1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceRollbackAll};
    gRpcMethodRegistry[DataServiceBatchRecallItemsMessageId] = RpcMethodMeta{
        "DataService", "BatchRecallItems",
        std::make_unique<::data_service::BatchRecallItemsRequest>(),
        std::make_unique<::data_service::BatchRecallItemsResponse>(),
        nullptr, 1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceBatchRecallItems};
    gRpcMethodRegistry[DataServiceQueryTransactionLogMessageId] = RpcMethodMeta{
        "DataService", "QueryTransactionLog",
        std::make_unique<::data_service::QueryTransactionLogRequest>(),
        std::make_unique<::data_service::QueryTransactionLogResponse>(),
        nullptr, 1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceQueryTransactionLog};
    gRpcMethodRegistry[DataServiceCreateEventSnapshotMessageId] = RpcMethodMeta{
        "DataService", "CreateEventSnapshot",
        std::make_unique<::data_service::CreateEventSnapshotRequest>(),
        std::make_unique<::data_service::CreateEventSnapshotResponse>(),
        nullptr, 1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceCreateEventSnapshot};
    gRpcMethodRegistry[DataServiceAllocateIdSegmentMessageId] = RpcMethodMeta{
        "DataService", "AllocateIdSegment",
        std::make_unique<::data_service::AllocateIdSegmentRequest>(),
        std::make_unique<::data_service::AllocateIdSegmentResponse>(),
        nullptr, 1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceAllocateIdSegment};

    // --- KV ---
    gRpcMethodRegistry[KVRangeMessageId] = RpcMethodMeta{
        "KV", "Range",
        std::make_unique<::etcdserverpb::RangeRequest>(),
        std::make_unique<::etcdserverpb::RangeResponse>(),
        nullptr, 0, common::base::eNodeType::EtcdNodeService, etcdserverpb::SendKVRange};
    gRpcMethodRegistry[KVPutMessageId] = RpcMethodMeta{
        "KV", "Put",
        std::make_unique<::etcdserverpb::PutRequest>(),
        std::make_unique<::etcdserverpb::PutResponse>(),
        nullptr, 0, common::base::eNodeType::EtcdNodeService, etcdserverpb::SendKVPut};
    gRpcMethodRegistry[KVDeleteRangeMessageId] = RpcMethodMeta{
        "KV", "DeleteRange",
        std::make_unique<::etcdserverpb::DeleteRangeRequest>(),
        std::make_unique<::etcdserverpb::DeleteRangeResponse>(),
        nullptr, 0, common::base::eNodeType::EtcdNodeService, etcdserverpb::SendKVDeleteRange};
    gRpcMethodRegistry[KVTxnMessageId] = RpcMethodMeta{
        "KV", "Txn",
        std::make_unique<::etcdserverpb::TxnRequest>(),
        std::make_unique<::etcdserverpb::TxnResponse>(),
        nullptr, 0, common::base::eNodeType::EtcdNodeService, etcdserverpb::SendKVTxn};
    gRpcMethodRegistry[KVCompactMessageId] = RpcMethodMeta{
        "KV", "Compact",
        std::make_unique<::etcdserverpb::CompactionRequest>(),
        std::make_unique<::etcdserverpb::CompactionResponse>(),
        nullptr, 0, common::base::eNodeType::EtcdNodeService, etcdserverpb::SendKVCompact};

    // --- Watch ---
    gRpcMethodRegistry[WatchWatchMessageId] = RpcMethodMeta{
        "Watch", "Watch",
        std::make_unique<::etcdserverpb::WatchRequest>(),
        std::make_unique<::etcdserverpb::WatchResponse>(),
        nullptr, 0, common::base::eNodeType::EtcdNodeService, etcdserverpb::SendWatchWatch};

    // --- Lease ---
    gRpcMethodRegistry[LeaseLeaseGrantMessageId] = RpcMethodMeta{
        "Lease", "LeaseGrant",
        std::make_unique<::etcdserverpb::LeaseGrantRequest>(),
        std::make_unique<::etcdserverpb::LeaseGrantResponse>(),
        nullptr, 0, common::base::eNodeType::EtcdNodeService, etcdserverpb::SendLeaseLeaseGrant};
    gRpcMethodRegistry[LeaseLeaseRevokeMessageId] = RpcMethodMeta{
        "Lease", "LeaseRevoke",
        std::make_unique<::etcdserverpb::LeaseRevokeRequest>(),
        std::make_unique<::etcdserverpb::LeaseRevokeResponse>(),
        nullptr, 0, common::base::eNodeType::EtcdNodeService, etcdserverpb::SendLeaseLeaseRevoke};
    gRpcMethodRegistry[LeaseLeaseKeepAliveMessageId] = RpcMethodMeta{
        "Lease", "LeaseKeepAlive",
        std::make_unique<::etcdserverpb::LeaseKeepAliveRequest>(),
        std::make_unique<::etcdserverpb::LeaseKeepAliveResponse>(),
        nullptr, 0, common::base::eNodeType::EtcdNodeService, etcdserverpb::SendLeaseLeaseKeepAlive};
    gRpcMethodRegistry[LeaseLeaseTimeToLiveMessageId] = RpcMethodMeta{
        "Lease", "LeaseTimeToLive",
        std::make_unique<::etcdserverpb::LeaseTimeToLiveRequest>(),
        std::make_unique<::etcdserverpb::LeaseTimeToLiveResponse>(),
        nullptr, 0, common::base::eNodeType::EtcdNodeService, etcdserverpb::SendLeaseLeaseTimeToLive};
    gRpcMethodRegistry[LeaseLeaseLeasesMessageId] = RpcMethodMeta{
        "Lease", "LeaseLeases",
        std::make_unique<::etcdserverpb::LeaseLeasesRequest>(),
        std::make_unique<::etcdserverpb::LeaseLeasesResponse>(),
        nullptr, 0, common::base::eNodeType::EtcdNodeService, etcdserverpb::SendLeaseLeaseLeases};

    // --- FriendService ---
    gRpcMethodRegistry[FriendServiceAddFriendMessageId] = RpcMethodMeta{
        "FriendService", "AddFriend",
        std::make_unique<::friendpb::AddFriendRequest>(),
        std::make_unique<::friendpb::AddFriendResponse>(),
        nullptr, 1, common::base::eNodeType::FriendNodeService, friendpb::SendFriendServiceAddFriend};
    gRpcMethodRegistry[FriendServiceAcceptFriendMessageId] = RpcMethodMeta{
        "FriendService", "AcceptFriend",
        std::make_unique<::friendpb::AcceptFriendRequest>(),
        std::make_unique<::friendpb::AcceptFriendResponse>(),
        nullptr, 1, common::base::eNodeType::FriendNodeService, friendpb::SendFriendServiceAcceptFriend};
    gRpcMethodRegistry[FriendServiceRejectFriendMessageId] = RpcMethodMeta{
        "FriendService", "RejectFriend",
        std::make_unique<::friendpb::RejectFriendRequest>(),
        std::make_unique<::friendpb::RejectFriendResponse>(),
        nullptr, 1, common::base::eNodeType::FriendNodeService, friendpb::SendFriendServiceRejectFriend};
    gRpcMethodRegistry[FriendServiceRemoveFriendMessageId] = RpcMethodMeta{
        "FriendService", "RemoveFriend",
        std::make_unique<::friendpb::RemoveFriendRequest>(),
        std::make_unique<::friendpb::RemoveFriendResponse>(),
        nullptr, 1, common::base::eNodeType::FriendNodeService, friendpb::SendFriendServiceRemoveFriend};
    gRpcMethodRegistry[FriendServiceGetFriendListMessageId] = RpcMethodMeta{
        "FriendService", "GetFriendList",
        std::make_unique<::friendpb::GetFriendListRequest>(),
        std::make_unique<::friendpb::GetFriendListResponse>(),
        nullptr, 1, common::base::eNodeType::FriendNodeService, friendpb::SendFriendServiceGetFriendList};
    gRpcMethodRegistry[FriendServiceGetPendingRequestsMessageId] = RpcMethodMeta{
        "FriendService", "GetPendingRequests",
        std::make_unique<::friendpb::GetPendingRequestsRequest>(),
        std::make_unique<::friendpb::GetPendingRequestsResponse>(),
        nullptr, 1, common::base::eNodeType::FriendNodeService, friendpb::SendFriendServiceGetPendingRequests};
    gRpcMethodRegistry[FriendServiceNotifyOnlineMessageId] = RpcMethodMeta{
        "FriendService", "NotifyOnline",
        std::make_unique<::friendpb::NotifyOnlineRequest>(),
        std::make_unique<::friendpb::NotifyOnlineResponse>(),
        nullptr, 1, common::base::eNodeType::FriendNodeService, friendpb::SendFriendServiceNotifyOnline};
    gRpcMethodRegistry[FriendServiceNotifyOfflineMessageId] = RpcMethodMeta{
        "FriendService", "NotifyOffline",
        std::make_unique<::friendpb::NotifyOfflineRequest>(),
        std::make_unique<::friendpb::NotifyOfflineResponse>(),
        nullptr, 1, common::base::eNodeType::FriendNodeService, friendpb::SendFriendServiceNotifyOffline};

    // --- Gate ---
    gRpcMethodRegistry[GatePlayerEnterGameNodeMessageId] = RpcMethodMeta{
        "Gate", "PlayerEnterGameNode",
        std::make_unique<::RegisterGameNodeSessionRequest>(),
        std::make_unique<::RegisterGameNodeSessionResponse>(),
        std::make_unique<GateImpl>(), 0, common::base::eNodeType::GateNodeService};
    gRpcMethodRegistry[GateSendMessageToPlayerMessageId] = RpcMethodMeta{
        "Gate", "SendMessageToPlayer",
        std::make_unique<::NodeRouteMessageRequest>(),
        std::make_unique<::Empty>(),
        std::make_unique<GateImpl>(), 0, common::base::eNodeType::GateNodeService};
    gRpcMethodRegistry[GateRouteNodeMessageMessageId] = RpcMethodMeta{
        "Gate", "RouteNodeMessage",
        std::make_unique<::RouteMessageRequest>(),
        std::make_unique<::RouteMessageResponse>(),
        std::make_unique<GateImpl>(), 0, common::base::eNodeType::GateNodeService};
    gRpcMethodRegistry[GateRoutePlayerMessageMessageId] = RpcMethodMeta{
        "Gate", "RoutePlayerMessage",
        std::make_unique<::RoutePlayerMessageRequest>(),
        std::make_unique<::RoutePlayerMessageResponse>(),
        std::make_unique<GateImpl>(), 0, common::base::eNodeType::GateNodeService};
    gRpcMethodRegistry[GateBroadcastToPlayersMessageId] = RpcMethodMeta{
        "Gate", "BroadcastToPlayers",
        std::make_unique<::BroadcastToPlayersRequest>(),
        std::make_unique<::Empty>(),
        std::make_unique<GateImpl>(), 0, common::base::eNodeType::GateNodeService};
    gRpcMethodRegistry[GateBroadcastToSceneMessageId] = RpcMethodMeta{
        "Gate", "BroadcastToScene",
        std::make_unique<::BroadcastToSceneRequest>(),
        std::make_unique<::Empty>(),
        std::make_unique<GateImpl>(), 0, common::base::eNodeType::GateNodeService};
    gRpcMethodRegistry[GateBroadcastToAllMessageId] = RpcMethodMeta{
        "Gate", "BroadcastToAll",
        std::make_unique<::BroadcastToAllRequest>(),
        std::make_unique<::Empty>(),
        std::make_unique<GateImpl>(), 0, common::base::eNodeType::GateNodeService};
    gRpcMethodRegistry[GateNodeHandshakeMessageId] = RpcMethodMeta{
        "Gate", "NodeHandshake",
        std::make_unique<::NodeHandshakeRequest>(),
        std::make_unique<::NodeHandshakeResponse>(),
        std::make_unique<GateImpl>(), 0, common::base::eNodeType::GateNodeService};
    gRpcMethodRegistry[GateBindSessionToGateMessageId] = RpcMethodMeta{
        "Gate", "BindSessionToGate",
        std::make_unique<::BindSessionToGateRequest>(),
        std::make_unique<::BindSessionToGateResponse>(),
        std::make_unique<GateImpl>(), 0, common::base::eNodeType::GateNodeService};
    gRpcMethodRegistry[GateGmGracefulShutdownMessageId] = RpcMethodMeta{
        "Gate", "GmGracefulShutdown",
        std::make_unique<::GmGracefulShutdownRequest>(),
        std::make_unique<::GmGracefulShutdownResponse>(),
        std::make_unique<GateImpl>(), 0, common::base::eNodeType::GateNodeService};

    // --- GuildService ---
    gRpcMethodRegistry[GuildServiceCreateGuildMessageId] = RpcMethodMeta{
        "GuildService", "CreateGuild",
        std::make_unique<::guildpb::CreateGuildRequest>(),
        std::make_unique<::guildpb::CreateGuildResponse>(),
        nullptr, 1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceCreateGuild};
    gRpcMethodRegistry[GuildServiceGetGuildMessageId] = RpcMethodMeta{
        "GuildService", "GetGuild",
        std::make_unique<::guildpb::GetGuildRequest>(),
        std::make_unique<::guildpb::GetGuildResponse>(),
        nullptr, 1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceGetGuild};
    gRpcMethodRegistry[GuildServiceGetPlayerGuildMessageId] = RpcMethodMeta{
        "GuildService", "GetPlayerGuild",
        std::make_unique<::guildpb::GetPlayerGuildRequest>(),
        std::make_unique<::guildpb::GetPlayerGuildResponse>(),
        nullptr, 1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceGetPlayerGuild};
    gRpcMethodRegistry[GuildServiceJoinGuildMessageId] = RpcMethodMeta{
        "GuildService", "JoinGuild",
        std::make_unique<::guildpb::JoinGuildRequest>(),
        std::make_unique<::guildpb::JoinGuildResponse>(),
        nullptr, 1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceJoinGuild};
    gRpcMethodRegistry[GuildServiceLeaveGuildMessageId] = RpcMethodMeta{
        "GuildService", "LeaveGuild",
        std::make_unique<::guildpb::LeaveGuildRequest>(),
        std::make_unique<::guildpb::LeaveGuildResponse>(),
        nullptr, 1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceLeaveGuild};
    gRpcMethodRegistry[GuildServiceDisbandGuildMessageId] = RpcMethodMeta{
        "GuildService", "DisbandGuild",
        std::make_unique<::guildpb::DisbandGuildRequest>(),
        std::make_unique<::guildpb::DisbandGuildResponse>(),
        nullptr, 1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceDisbandGuild};
    gRpcMethodRegistry[GuildServiceSetAnnouncementMessageId] = RpcMethodMeta{
        "GuildService", "SetAnnouncement",
        std::make_unique<::guildpb::SetAnnouncementRequest>(),
        std::make_unique<::guildpb::SetAnnouncementResponse>(),
        nullptr, 1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceSetAnnouncement};
    gRpcMethodRegistry[GuildServiceUpdateGuildScoreMessageId] = RpcMethodMeta{
        "GuildService", "UpdateGuildScore",
        std::make_unique<::guildpb::UpdateGuildScoreRequest>(),
        std::make_unique<::guildpb::UpdateGuildScoreResponse>(),
        nullptr, 1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceUpdateGuildScore};
    gRpcMethodRegistry[GuildServiceGetGuildRankMessageId] = RpcMethodMeta{
        "GuildService", "GetGuildRank",
        std::make_unique<::guildpb::GetGuildRankRequest>(),
        std::make_unique<::guildpb::GetGuildRankResponse>(),
        nullptr, 1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceGetGuildRank};
    gRpcMethodRegistry[GuildServiceGetGuildRankByGuildMessageId] = RpcMethodMeta{
        "GuildService", "GetGuildRankByGuild",
        std::make_unique<::guildpb::GetGuildRankByGuildRequest>(),
        std::make_unique<::guildpb::GetGuildRankByGuildResponse>(),
        nullptr, 1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceGetGuildRankByGuild};

    // --- ClientPlayerLogin ---
    gRpcMethodRegistry[ClientPlayerLoginLoginMessageId] = RpcMethodMeta{
        "ClientPlayerLogin", "Login",
        std::make_unique<::loginpb::LoginRequest>(),
        std::make_unique<::loginpb::LoginResponse>(),
        nullptr, 1, common::base::eNodeType::LoginNodeService, loginpb::SendClientPlayerLoginLogin};
    gRpcMethodRegistry[ClientPlayerLoginCreatePlayerMessageId] = RpcMethodMeta{
        "ClientPlayerLogin", "CreatePlayer",
        std::make_unique<::loginpb::CreatePlayerRequest>(),
        std::make_unique<::loginpb::CreatePlayerResponse>(),
        nullptr, 1, common::base::eNodeType::LoginNodeService, loginpb::SendClientPlayerLoginCreatePlayer};
    gRpcMethodRegistry[ClientPlayerLoginEnterGameMessageId] = RpcMethodMeta{
        "ClientPlayerLogin", "EnterGame",
        std::make_unique<::loginpb::EnterGameRequest>(),
        std::make_unique<::loginpb::EnterGameResponse>(),
        nullptr, 1, common::base::eNodeType::LoginNodeService, loginpb::SendClientPlayerLoginEnterGame};
    gRpcMethodRegistry[ClientPlayerLoginLeaveGameMessageId] = RpcMethodMeta{
        "ClientPlayerLogin", "LeaveGame",
        std::make_unique<::loginpb::LeaveGameRequest>(),
        std::make_unique<::loginpb::LoginEmptyResponse>(),
        nullptr, 1, common::base::eNodeType::LoginNodeService, loginpb::SendClientPlayerLoginLeaveGame};
    gRpcMethodRegistry[ClientPlayerLoginDisconnectMessageId] = RpcMethodMeta{
        "ClientPlayerLogin", "Disconnect",
        std::make_unique<::loginpb::LoginNodeDisconnectRequest>(),
        std::make_unique<::loginpb::LoginEmptyResponse>(),
        nullptr, 1, common::base::eNodeType::LoginNodeService, loginpb::SendClientPlayerLoginDisconnect};
    gRpcMethodRegistry[ClientPlayerLoginRefreshTokenMessageId] = RpcMethodMeta{
        "ClientPlayerLogin", "RefreshToken",
        std::make_unique<::loginpb::RefreshTokenRequest>(),
        std::make_unique<::loginpb::RefreshTokenResponse>(),
        nullptr, 1, common::base::eNodeType::LoginNodeService, loginpb::SendClientPlayerLoginRefreshToken};

    // --- LoginPreGate ---
    gRpcMethodRegistry[LoginPreGateAssignGateMessageId] = RpcMethodMeta{
        "LoginPreGate", "AssignGate",
        std::make_unique<::loginpb::AssignGateRequest>(),
        std::make_unique<::loginpb::AssignGateResponse>(),
        nullptr, 1, common::base::eNodeType::LoginNodeService, loginpb::SendLoginPreGateAssignGate};
    gRpcMethodRegistry[LoginPreGateQueryQueueStatusMessageId] = RpcMethodMeta{
        "LoginPreGate", "QueryQueueStatus",
        std::make_unique<::loginpb::QueryQueueStatusRequest>(),
        std::make_unique<::loginpb::QueryQueueStatusResponse>(),
        nullptr, 1, common::base::eNodeType::LoginNodeService, loginpb::SendLoginPreGateQueryQueueStatus};

    // --- LoginAdmin ---
    gRpcMethodRegistry[LoginAdminRemovePlayersFromAccountsMessageId] = RpcMethodMeta{
        "LoginAdmin", "RemovePlayersFromAccounts",
        std::make_unique<::loginpb::RemovePlayersFromAccountsRequest>(),
        std::make_unique<::loginpb::RemovePlayersFromAccountsResponse>(),
        nullptr, 1, common::base::eNodeType::LoginNodeService, loginpb::SendLoginAdminRemovePlayersFromAccounts};

    // --- MatchService ---
    gRpcMethodRegistry[MatchServiceJoinQueueMessageId] = RpcMethodMeta{
        "MatchService", "JoinQueue",
        std::make_unique<::match::JoinQueueRequest>(),
        std::make_unique<::match::JoinQueueResponse>(),
        nullptr, 1, common::base::eNodeType::MatchNodeService, match::SendMatchServiceJoinQueue};
    gRpcMethodRegistry[MatchServiceCancelQueueMessageId] = RpcMethodMeta{
        "MatchService", "CancelQueue",
        std::make_unique<::match::CancelQueueRequest>(),
        std::make_unique<::Empty>(),
        nullptr, 1, common::base::eNodeType::MatchNodeService, match::SendMatchServiceCancelQueue};
    gRpcMethodRegistry[MatchServiceGetQueueStatusMessageId] = RpcMethodMeta{
        "MatchService", "GetQueueStatus",
        std::make_unique<::match::GetQueueStatusRequest>(),
        std::make_unique<::match::GetQueueStatusResponse>(),
        nullptr, 1, common::base::eNodeType::MatchNodeService, match::SendMatchServiceGetQueueStatus};
    gRpcMethodRegistry[MatchServiceChallengePlayerMessageId] = RpcMethodMeta{
        "MatchService", "ChallengePlayer",
        std::make_unique<::match::ChallengePlayerRequest>(),
        std::make_unique<::match::ChallengePlayerResponse>(),
        nullptr, 1, common::base::eNodeType::MatchNodeService, match::SendMatchServiceChallengePlayer};
    gRpcMethodRegistry[MatchServiceRespondChallengeMessageId] = RpcMethodMeta{
        "MatchService", "RespondChallenge",
        std::make_unique<::match::RespondChallengeRequest>(),
        std::make_unique<::match::RespondChallengeResponse>(),
        nullptr, 1, common::base::eNodeType::MatchNodeService, match::SendMatchServiceRespondChallenge};
    gRpcMethodRegistry[MatchServiceNotifyChallengeInviteMessageId] = RpcMethodMeta{
        "MatchService", "NotifyChallengeInvite",
        std::make_unique<::match::ChallengeInviteS2C>(),
        std::make_unique<::Empty>(),
        nullptr, 1, common::base::eNodeType::MatchNodeService, match::SendMatchServiceNotifyChallengeInvite};
    gRpcMethodRegistry[MatchServiceNotifyChallengeResultMessageId] = RpcMethodMeta{
        "MatchService", "NotifyChallengeResult",
        std::make_unique<::match::ChallengeResultS2C>(),
        std::make_unique<::Empty>(),
        nullptr, 1, common::base::eNodeType::MatchNodeService, match::SendMatchServiceNotifyChallengeResult};
    gRpcMethodRegistry[MatchServiceWatchBattleMessageId] = RpcMethodMeta{
        "MatchService", "WatchBattle",
        std::make_unique<::match::WatchBattleRequest>(),
        std::make_unique<::match::WatchBattleResponse>(),
        nullptr, 1, common::base::eNodeType::MatchNodeService, match::SendMatchServiceWatchBattle};
    gRpcMethodRegistry[MatchServiceRequestBattleTicketMessageId] = RpcMethodMeta{
        "MatchService", "RequestBattleTicket",
        std::make_unique<::RequestBattleTicketRequest>(),
        std::make_unique<::RequestBattleTicketResponse>(),
        nullptr, 1, common::base::eNodeType::MatchNodeService, match::SendMatchServiceRequestBattleTicket};
    gRpcMethodRegistry[MatchServiceListWatchableBattlesMessageId] = RpcMethodMeta{
        "MatchService", "ListWatchableBattles",
        std::make_unique<::match::ListWatchableBattlesRequest>(),
        std::make_unique<::match::ListWatchableBattlesResponse>(),
        nullptr, 1, common::base::eNodeType::MatchNodeService, match::SendMatchServiceListWatchableBattles};

    // --- SceneClientPlayerCommon ---
    gRpcMethodRegistry[SceneClientPlayerCommonSendTipToClientMessageId] = RpcMethodMeta{
        "SceneClientPlayerCommon", "SendTipToClient",
        std::make_unique<::TipInfoMessage>(),
        std::make_unique<::Empty>(),
        std::make_unique<SceneClientPlayerCommonImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneClientPlayerCommonKickPlayerMessageId] = RpcMethodMeta{
        "SceneClientPlayerCommon", "KickPlayer",
        std::make_unique<::GameKickPlayerRequest>(),
        std::make_unique<::Empty>(),
        std::make_unique<SceneClientPlayerCommonImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneClientPlayerCommonRedirectToGateMessageId] = RpcMethodMeta{
        "SceneClientPlayerCommon", "RedirectToGate",
        std::make_unique<::RedirectToGateNotify>(),
        std::make_unique<::Empty>(),
        std::make_unique<SceneClientPlayerCommonImpl>(), 0, common::base::eNodeType::SceneNodeService};

    // --- SceneAttributeClientPlayer ---
    gRpcMethodRegistry[SceneAttributeClientPlayerGetAttributePanelMessageId] = RpcMethodMeta{
        "SceneAttributeClientPlayer", "GetAttributePanel",
        std::make_unique<::GetAttributePanelRequest>(),
        std::make_unique<::GetAttributePanelResponse>(),
        std::make_unique<SceneAttributeClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneAttributeClientPlayerAllocateAttributePointsMessageId] = RpcMethodMeta{
        "SceneAttributeClientPlayer", "AllocateAttributePoints",
        std::make_unique<::AllocateAttributePointsRequest>(),
        std::make_unique<::AllocateAttributePointsResponse>(),
        std::make_unique<SceneAttributeClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneAttributeClientPlayerResetAttributePointsMessageId] = RpcMethodMeta{
        "SceneAttributeClientPlayer", "ResetAttributePoints",
        std::make_unique<::ResetAttributePointsRequest>(),
        std::make_unique<::ResetAttributePointsResponse>(),
        std::make_unique<SceneAttributeClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneAttributeClientPlayerAutoAllocateAttributePointsMessageId] = RpcMethodMeta{
        "SceneAttributeClientPlayer", "AutoAllocateAttributePoints",
        std::make_unique<::AutoAllocateAttributePointsRequest>(),
        std::make_unique<::AutoAllocateAttributePointsResponse>(),
        std::make_unique<SceneAttributeClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneAttributeClientPlayerCreateAttributeSchemeMessageId] = RpcMethodMeta{
        "SceneAttributeClientPlayer", "CreateAttributeScheme",
        std::make_unique<::CreateAttributeSchemeRequest>(),
        std::make_unique<::CreateAttributeSchemeResponse>(),
        std::make_unique<SceneAttributeClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneAttributeClientPlayerSwitchAttributeSchemeMessageId] = RpcMethodMeta{
        "SceneAttributeClientPlayer", "SwitchAttributeScheme",
        std::make_unique<::SwitchAttributeSchemeRequest>(),
        std::make_unique<::SwitchAttributeSchemeResponse>(),
        std::make_unique<SceneAttributeClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneAttributeClientPlayerRenameAttributeSchemeMessageId] = RpcMethodMeta{
        "SceneAttributeClientPlayer", "RenameAttributeScheme",
        std::make_unique<::RenameAttributeSchemeRequest>(),
        std::make_unique<::RenameAttributeSchemeResponse>(),
        std::make_unique<SceneAttributeClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneAttributeClientPlayerNotifyAttributePanelChangedMessageId] = RpcMethodMeta{
        "SceneAttributeClientPlayer", "NotifyAttributePanelChanged",
        std::make_unique<::AttributePanelChangedS2C>(),
        std::make_unique<::Empty>(),
        std::make_unique<SceneAttributeClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneAttributeClientPlayerGmSetPlayerLevelMessageId] = RpcMethodMeta{
        "SceneAttributeClientPlayer", "GmSetPlayerLevel",
        std::make_unique<::GmSetPlayerLevelRequest>(),
        std::make_unique<::GmSetPlayerLevelResponse>(),
        std::make_unique<SceneAttributeClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};

    // --- SceneCurrencyClientPlayer ---
    gRpcMethodRegistry[SceneCurrencyClientPlayerGmAddCurrencyMessageId] = RpcMethodMeta{
        "SceneCurrencyClientPlayer", "GmAddCurrency",
        std::make_unique<::GmAddCurrencyRequest>(),
        std::make_unique<::GmAddCurrencyResponse>(),
        std::make_unique<SceneCurrencyClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneCurrencyClientPlayerGmDeductCurrencyMessageId] = RpcMethodMeta{
        "SceneCurrencyClientPlayer", "GmDeductCurrency",
        std::make_unique<::GmDeductCurrencyRequest>(),
        std::make_unique<::GmDeductCurrencyResponse>(),
        std::make_unique<SceneCurrencyClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneCurrencyClientPlayerGetCurrencyListMessageId] = RpcMethodMeta{
        "SceneCurrencyClientPlayer", "GetCurrencyList",
        std::make_unique<::GetCurrencyListRequest>(),
        std::make_unique<::GetCurrencyListResponse>(),
        std::make_unique<SceneCurrencyClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneCurrencyClientPlayerGmBlockCurrencyMessageId] = RpcMethodMeta{
        "SceneCurrencyClientPlayer", "GmBlockCurrency",
        std::make_unique<::GmBlockCurrencyRequest>(),
        std::make_unique<::GmBlockCurrencyResponse>(),
        std::make_unique<SceneCurrencyClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneCurrencyClientPlayerGmUnblockCurrencyMessageId] = RpcMethodMeta{
        "SceneCurrencyClientPlayer", "GmUnblockCurrency",
        std::make_unique<::GmUnblockCurrencyRequest>(),
        std::make_unique<::GmUnblockCurrencyResponse>(),
        std::make_unique<SceneCurrencyClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};

    // --- ScenePlayer ---
    gRpcMethodRegistry[ScenePlayerGateLoginNotifyMessageId] = RpcMethodMeta{
        "ScenePlayer", "GateLoginNotify",
        std::make_unique<::GateLoginNotifyRequest>(),
        std::make_unique<::google::protobuf::Empty>(),
        std::make_unique<ScenePlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[ScenePlayerExitGameMessageId] = RpcMethodMeta{
        "ScenePlayer", "ExitGame",
        std::make_unique<::GameNodeExitGameRequest>(),
        std::make_unique<::google::protobuf::Empty>(),
        std::make_unique<ScenePlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};

    // --- SceneMovementClientPlayer ---
    gRpcMethodRegistry[SceneMovementClientPlayerMoveStartMessageId] = RpcMethodMeta{
        "SceneMovementClientPlayer", "MoveStart",
        std::make_unique<::MoveStartC2S>(),
        std::make_unique<::Empty>(),
        std::make_unique<SceneMovementClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneMovementClientPlayerMoveStopMessageId] = RpcMethodMeta{
        "SceneMovementClientPlayer", "MoveStop",
        std::make_unique<::MoveStopC2S>(),
        std::make_unique<::Empty>(),
        std::make_unique<SceneMovementClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneMovementClientPlayerMoveSyncMessageId] = RpcMethodMeta{
        "SceneMovementClientPlayer", "MoveSync",
        std::make_unique<::MoveSyncC2S>(),
        std::make_unique<::Empty>(),
        std::make_unique<SceneMovementClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneMovementClientPlayerTeleportRequestMessageId] = RpcMethodMeta{
        "SceneMovementClientPlayer", "TeleportRequest",
        std::make_unique<::TeleportRequestC2S>(),
        std::make_unique<::TeleportRequestC2SResponse>(),
        std::make_unique<SceneMovementClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneMovementClientPlayerNotifyMoveAckMessageId] = RpcMethodMeta{
        "SceneMovementClientPlayer", "NotifyMoveAck",
        std::make_unique<::MoveAckS2C>(),
        std::make_unique<::Empty>(),
        std::make_unique<SceneMovementClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneMovementClientPlayerNotifyActorMoveMessageId] = RpcMethodMeta{
        "SceneMovementClientPlayer", "NotifyActorMove",
        std::make_unique<::ActorMoveS2C>(),
        std::make_unique<::Empty>(),
        std::make_unique<SceneMovementClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneMovementClientPlayerNotifyActorMoveListMessageId] = RpcMethodMeta{
        "SceneMovementClientPlayer", "NotifyActorMoveList",
        std::make_unique<::ActorMoveListS2C>(),
        std::make_unique<::Empty>(),
        std::make_unique<SceneMovementClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneMovementClientPlayerNotifyTeleportMessageId] = RpcMethodMeta{
        "SceneMovementClientPlayer", "NotifyTeleport",
        std::make_unique<::TeleportS2C>(),
        std::make_unique<::Empty>(),
        std::make_unique<SceneMovementClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};

    // --- ScenePetClientPlayer ---
    gRpcMethodRegistry[ScenePetClientPlayerGetPetListMessageId] = RpcMethodMeta{
        "ScenePetClientPlayer", "GetPetList",
        std::make_unique<::GetPetListRequest>(),
        std::make_unique<::GetPetListResponse>(),
        std::make_unique<ScenePetClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[ScenePetClientPlayerSummonPetMessageId] = RpcMethodMeta{
        "ScenePetClientPlayer", "SummonPet",
        std::make_unique<::SummonPetRequest>(),
        std::make_unique<::SummonPetResponse>(),
        std::make_unique<ScenePetClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[ScenePetClientPlayerRecallPetMessageId] = RpcMethodMeta{
        "ScenePetClientPlayer", "RecallPet",
        std::make_unique<::RecallPetRequest>(),
        std::make_unique<::RecallPetResponse>(),
        std::make_unique<ScenePetClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[ScenePetClientPlayerAllocatePetPointsMessageId] = RpcMethodMeta{
        "ScenePetClientPlayer", "AllocatePetPoints",
        std::make_unique<::AllocatePetPointsRequest>(),
        std::make_unique<::AllocatePetPointsResponse>(),
        std::make_unique<ScenePetClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[ScenePetClientPlayerResetPetPointsMessageId] = RpcMethodMeta{
        "ScenePetClientPlayer", "ResetPetPoints",
        std::make_unique<::ResetPetPointsRequest>(),
        std::make_unique<::ResetPetPointsResponse>(),
        std::make_unique<ScenePetClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[ScenePetClientPlayerAutoAllocatePetPointsMessageId] = RpcMethodMeta{
        "ScenePetClientPlayer", "AutoAllocatePetPoints",
        std::make_unique<::AutoAllocatePetPointsRequest>(),
        std::make_unique<::AutoAllocatePetPointsResponse>(),
        std::make_unique<ScenePetClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[ScenePetClientPlayerRenamePetMessageId] = RpcMethodMeta{
        "ScenePetClientPlayer", "RenamePet",
        std::make_unique<::RenamePetRequest>(),
        std::make_unique<::RenamePetResponse>(),
        std::make_unique<ScenePetClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[ScenePetClientPlayerNotifyPetListChangedMessageId] = RpcMethodMeta{
        "ScenePetClientPlayer", "NotifyPetListChanged",
        std::make_unique<::PetListChangedS2C>(),
        std::make_unique<::Empty>(),
        std::make_unique<ScenePetClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[ScenePetClientPlayerGmGrantPetMessageId] = RpcMethodMeta{
        "ScenePetClientPlayer", "GmGrantPet",
        std::make_unique<::GmGrantPetRequest>(),
        std::make_unique<::GmGrantPetResponse>(),
        std::make_unique<ScenePetClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};

    // --- SceneRollbackClientPlayer ---
    gRpcMethodRegistry[SceneRollbackClientPlayerGmAttachDebtMessageId] = RpcMethodMeta{
        "SceneRollbackClientPlayer", "GmAttachDebt",
        std::make_unique<::GmAttachDebtRequest>(),
        std::make_unique<::GmAttachDebtResponse>(),
        std::make_unique<SceneRollbackClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneRollbackClientPlayerGmWaiveDebtMessageId] = RpcMethodMeta{
        "SceneRollbackClientPlayer", "GmWaiveDebt",
        std::make_unique<::GmWaiveDebtRequest>(),
        std::make_unique<::GmWaiveDebtResponse>(),
        std::make_unique<SceneRollbackClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneRollbackClientPlayerGmAdjustDebtMessageId] = RpcMethodMeta{
        "SceneRollbackClientPlayer", "GmAdjustDebt",
        std::make_unique<::GmAdjustDebtRequest>(),
        std::make_unique<::GmAdjustDebtResponse>(),
        std::make_unique<SceneRollbackClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneRollbackClientPlayerGmFreezeDebtMessageId] = RpcMethodMeta{
        "SceneRollbackClientPlayer", "GmFreezeDebt",
        std::make_unique<::GmFreezeDebtRequest>(),
        std::make_unique<::GmFreezeDebtResponse>(),
        std::make_unique<SceneRollbackClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneRollbackClientPlayerGmQueryDebtMessageId] = RpcMethodMeta{
        "SceneRollbackClientPlayer", "GmQueryDebt",
        std::make_unique<::GmQueryDebtRequest>(),
        std::make_unique<::GmQueryDebtResponse>(),
        std::make_unique<SceneRollbackClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneRollbackClientPlayerGmCreateSnapshotMessageId] = RpcMethodMeta{
        "SceneRollbackClientPlayer", "GmCreateSnapshot",
        std::make_unique<::GmCreateSnapshotRequest>(),
        std::make_unique<::GmCreateSnapshotResponse>(),
        std::make_unique<SceneRollbackClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneRollbackClientPlayerGmListSnapshotsMessageId] = RpcMethodMeta{
        "SceneRollbackClientPlayer", "GmListSnapshots",
        std::make_unique<::GmListSnapshotsRequest>(),
        std::make_unique<::GmListSnapshotsResponse>(),
        std::make_unique<SceneRollbackClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneRollbackClientPlayerGmPreviewRollbackMessageId] = RpcMethodMeta{
        "SceneRollbackClientPlayer", "GmPreviewRollback",
        std::make_unique<::GmPreviewRollbackRequest>(),
        std::make_unique<::GmPreviewRollbackResponse>(),
        std::make_unique<SceneRollbackClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneRollbackClientPlayerGmExecuteRollbackMessageId] = RpcMethodMeta{
        "SceneRollbackClientPlayer", "GmExecuteRollback",
        std::make_unique<::GmExecuteRollbackRequest>(),
        std::make_unique<::GmExecuteRollbackResponse>(),
        std::make_unique<SceneRollbackClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneRollbackClientPlayerGmQueryTransactionLogMessageId] = RpcMethodMeta{
        "SceneRollbackClientPlayer", "GmQueryTransactionLog",
        std::make_unique<::GmQueryTransactionLogRequest>(),
        std::make_unique<::GmQueryTransactionLogResponse>(),
        std::make_unique<SceneRollbackClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneRollbackClientPlayerGmTraceItemMessageId] = RpcMethodMeta{
        "SceneRollbackClientPlayer", "GmTraceItem",
        std::make_unique<::GmTraceItemRequest>(),
        std::make_unique<::GmTraceItemResponse>(),
        std::make_unique<SceneRollbackClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneRollbackClientPlayerGmClawbackItemMessageId] = RpcMethodMeta{
        "SceneRollbackClientPlayer", "GmClawbackItem",
        std::make_unique<::GmClawbackItemRequest>(),
        std::make_unique<::GmClawbackItemResponse>(),
        std::make_unique<SceneRollbackClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};

    // --- SceneSceneClientPlayer ---
    gRpcMethodRegistry[SceneSceneClientPlayerEnterSceneMessageId] = RpcMethodMeta{
        "SceneSceneClientPlayer", "EnterScene",
        std::make_unique<::EnterSceneC2SRequest>(),
        std::make_unique<::EnterSceneC2SResponse>(),
        std::make_unique<SceneSceneClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneSceneClientPlayerNotifyEnterSceneMessageId] = RpcMethodMeta{
        "SceneSceneClientPlayer", "NotifyEnterScene",
        std::make_unique<::EnterSceneS2C>(),
        std::make_unique<::Empty>(),
        std::make_unique<SceneSceneClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneSceneClientPlayerSceneInfoC2SMessageId] = RpcMethodMeta{
        "SceneSceneClientPlayer", "SceneInfoC2S",
        std::make_unique<::SceneInfoRequest>(),
        std::make_unique<::Empty>(),
        std::make_unique<SceneSceneClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneSceneClientPlayerNotifySceneInfoMessageId] = RpcMethodMeta{
        "SceneSceneClientPlayer", "NotifySceneInfo",
        std::make_unique<::SceneInfoS2C>(),
        std::make_unique<::Empty>(),
        std::make_unique<SceneSceneClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneSceneClientPlayerNotifyActorCreateMessageId] = RpcMethodMeta{
        "SceneSceneClientPlayer", "NotifyActorCreate",
        std::make_unique<::ActorCreateS2C>(),
        std::make_unique<::Empty>(),
        std::make_unique<SceneSceneClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneSceneClientPlayerNotifyActorDestroyMessageId] = RpcMethodMeta{
        "SceneSceneClientPlayer", "NotifyActorDestroy",
        std::make_unique<::ActorDestroyS2C>(),
        std::make_unique<::Empty>(),
        std::make_unique<SceneSceneClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneSceneClientPlayerNotifyActorListCreateMessageId] = RpcMethodMeta{
        "SceneSceneClientPlayer", "NotifyActorListCreate",
        std::make_unique<::ActorListCreateS2C>(),
        std::make_unique<::Empty>(),
        std::make_unique<SceneSceneClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneSceneClientPlayerNotifyActorListDestroyMessageId] = RpcMethodMeta{
        "SceneSceneClientPlayer", "NotifyActorListDestroy",
        std::make_unique<::ActorListDestroyS2C>(),
        std::make_unique<::Empty>(),
        std::make_unique<SceneSceneClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};

    // --- SceneSkillClientPlayer ---
    gRpcMethodRegistry[SceneSkillClientPlayerReleaseSkillMessageId] = RpcMethodMeta{
        "SceneSkillClientPlayer", "ReleaseSkill",
        std::make_unique<::ReleaseSkillRequest>(),
        std::make_unique<::ReleaseSkillResponse>(),
        std::make_unique<SceneSkillClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneSkillClientPlayerNotifySkillUsedMessageId] = RpcMethodMeta{
        "SceneSkillClientPlayer", "NotifySkillUsed",
        std::make_unique<::SkillUsedS2C>(),
        std::make_unique<::Empty>(),
        std::make_unique<SceneSkillClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneSkillClientPlayerNotifySkillInterruptedMessageId] = RpcMethodMeta{
        "SceneSkillClientPlayer", "NotifySkillInterrupted",
        std::make_unique<::SkillInterruptedS2C>(),
        std::make_unique<::Empty>(),
        std::make_unique<SceneSkillClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneSkillClientPlayerListSkillsMessageId] = RpcMethodMeta{
        "SceneSkillClientPlayer", "ListSkills",
        std::make_unique<::ListSkillsRequest>(),
        std::make_unique<::ListSkillsResponse>(),
        std::make_unique<SceneSkillClientPlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};

    // --- ScenePlayerSync ---
    gRpcMethodRegistry[ScenePlayerSyncSyncBaseAttributeMessageId] = RpcMethodMeta{
        "ScenePlayerSync", "SyncBaseAttribute",
        std::make_unique<::ActorBaseAttributesS2C>(),
        std::make_unique<::Empty>(),
        std::make_unique<ScenePlayerSyncImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[ScenePlayerSyncSyncAttribute2FramesMessageId] = RpcMethodMeta{
        "ScenePlayerSync", "SyncAttribute2Frames",
        std::make_unique<::AttributeDelta2FramesS2C>(),
        std::make_unique<::Empty>(),
        std::make_unique<ScenePlayerSyncImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[ScenePlayerSyncSyncAttribute5FramesMessageId] = RpcMethodMeta{
        "ScenePlayerSync", "SyncAttribute5Frames",
        std::make_unique<::AttributeDelta5FramesS2C>(),
        std::make_unique<::Empty>(),
        std::make_unique<ScenePlayerSyncImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[ScenePlayerSyncSyncAttribute10FramesMessageId] = RpcMethodMeta{
        "ScenePlayerSync", "SyncAttribute10Frames",
        std::make_unique<::AttributeDelta10FramesS2C>(),
        std::make_unique<::Empty>(),
        std::make_unique<ScenePlayerSyncImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[ScenePlayerSyncSyncAttribute30FramesMessageId] = RpcMethodMeta{
        "ScenePlayerSync", "SyncAttribute30Frames",
        std::make_unique<::AttributeDelta30FramesS2C>(),
        std::make_unique<::Empty>(),
        std::make_unique<ScenePlayerSyncImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[ScenePlayerSyncSyncAttribute60FramesMessageId] = RpcMethodMeta{
        "ScenePlayerSync", "SyncAttribute60Frames",
        std::make_unique<::AttributeDelta60FramesS2C>(),
        std::make_unique<::Empty>(),
        std::make_unique<ScenePlayerSyncImpl>(), 0, common::base::eNodeType::SceneNodeService};

    // --- SceneScenePlayer ---
    gRpcMethodRegistry[SceneScenePlayerEnterSceneMessageId] = RpcMethodMeta{
        "SceneScenePlayer", "EnterScene",
        std::make_unique<::GsEnterSceneRequest>(),
        std::make_unique<::google::protobuf::Empty>(),
        std::make_unique<SceneScenePlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneScenePlayerLeaveSceneMessageId] = RpcMethodMeta{
        "SceneScenePlayer", "LeaveScene",
        std::make_unique<::GsLeaveSceneRequest>(),
        std::make_unique<::google::protobuf::Empty>(),
        std::make_unique<SceneScenePlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneScenePlayerEnterSceneS2CMessageId] = RpcMethodMeta{
        "SceneScenePlayer", "EnterSceneS2C",
        std::make_unique<::EnterSceneS2CRequest>(),
        std::make_unique<::EnterSceneS2CResponse>(),
        std::make_unique<SceneScenePlayerImpl>(), 0, common::base::eNodeType::SceneNodeService};

    // --- Scene ---
    gRpcMethodRegistry[ScenePlayerEnterGameNodeMessageId] = RpcMethodMeta{
        "Scene", "PlayerEnterGameNode",
        std::make_unique<::PlayerEnterGameNodeRequest>(),
        std::make_unique<::Empty>(),
        std::make_unique<SceneImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneSendMessageToPlayerMessageId] = RpcMethodMeta{
        "Scene", "SendMessageToPlayer",
        std::make_unique<::NodeRouteMessageRequest>(),
        std::make_unique<::NodeRouteMessageResponse>(),
        std::make_unique<SceneImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneProcessClientPlayerMessageMessageId] = RpcMethodMeta{
        "Scene", "ProcessClientPlayerMessage",
        std::make_unique<::ProcessClientPlayerMessageRequest>(),
        std::make_unique<::ProcessClientPlayerMessageResponse>(),
        std::make_unique<SceneImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneInvokePlayerServiceMessageId] = RpcMethodMeta{
        "Scene", "InvokePlayerService",
        std::make_unique<::NodeRouteMessageRequest>(),
        std::make_unique<::NodeRouteMessageResponse>(),
        std::make_unique<SceneImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneRouteNodeStringMsgMessageId] = RpcMethodMeta{
        "Scene", "RouteNodeStringMsg",
        std::make_unique<::RouteMessageRequest>(),
        std::make_unique<::RouteMessageResponse>(),
        std::make_unique<SceneImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneRoutePlayerStringMsgMessageId] = RpcMethodMeta{
        "Scene", "RoutePlayerStringMsg",
        std::make_unique<::RoutePlayerMessageRequest>(),
        std::make_unique<::RoutePlayerMessageResponse>(),
        std::make_unique<SceneImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneUpdateSessionDetailMessageId] = RpcMethodMeta{
        "Scene", "UpdateSessionDetail",
        std::make_unique<::RegisterPlayerSessionRequest>(),
        std::make_unique<::Empty>(),
        std::make_unique<SceneImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneCreateSceneMessageId] = RpcMethodMeta{
        "Scene", "CreateScene",
        std::make_unique<::CreateSceneRequest>(),
        std::make_unique<::CreateSceneResponse>(),
        std::make_unique<SceneImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneDestroySceneMessageId] = RpcMethodMeta{
        "Scene", "DestroyScene",
        std::make_unique<::DestroySceneRequest>(),
        std::make_unique<::Empty>(),
        std::make_unique<SceneImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[ScenePrepareBattleMessageId] = RpcMethodMeta{
        "Scene", "PrepareBattle",
        std::make_unique<::PrepareBattleRequest>(),
        std::make_unique<::PrepareBattleResponse>(),
        std::make_unique<SceneImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneCancelBattlePrepareMessageId] = RpcMethodMeta{
        "Scene", "CancelBattlePrepare",
        std::make_unique<::CancelBattlePrepareRequest>(),
        std::make_unique<::Empty>(),
        std::make_unique<SceneImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneNodeHandshakeMessageId] = RpcMethodMeta{
        "Scene", "NodeHandshake",
        std::make_unique<::NodeHandshakeRequest>(),
        std::make_unique<::NodeHandshakeResponse>(),
        std::make_unique<SceneImpl>(), 0, common::base::eNodeType::SceneNodeService};

    // --- SceneScene ---
    gRpcMethodRegistry[SceneSceneTestMessageId] = RpcMethodMeta{
        "SceneScene", "Test",
        std::make_unique<::GameSceneTest>(),
        std::make_unique<::Empty>(),
        std::make_unique<SceneSceneImpl>(), 0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneSceneGmGracefulShutdownMessageId] = RpcMethodMeta{
        "SceneScene", "GmGracefulShutdown",
        std::make_unique<::GmGracefulShutdownRequest>(),
        std::make_unique<::GmGracefulShutdownResponse>(),
        std::make_unique<SceneSceneImpl>(), 0, common::base::eNodeType::SceneNodeService};

    // --- SceneManager ---
    gRpcMethodRegistry[SceneManagerCreateSceneMessageId] = RpcMethodMeta{
        "SceneManager", "CreateScene",
        std::make_unique<::scene_manager::CreateSceneRequest>(),
        std::make_unique<::scene_manager::CreateSceneResponse>(),
        nullptr, 1, common::base::eNodeType::SceneManagerNodeService, scene_manager::SendSceneManagerCreateScene};
    gRpcMethodRegistry[SceneManagerDestroySceneMessageId] = RpcMethodMeta{
        "SceneManager", "DestroyScene",
        std::make_unique<::scene_manager::DestroySceneRequest>(),
        std::make_unique<::Empty>(),
        nullptr, 1, common::base::eNodeType::SceneManagerNodeService, scene_manager::SendSceneManagerDestroyScene};
    gRpcMethodRegistry[SceneManagerEnterSceneMessageId] = RpcMethodMeta{
        "SceneManager", "EnterScene",
        std::make_unique<::scene_manager::EnterSceneRequest>(),
        std::make_unique<::scene_manager::EnterSceneResponse>(),
        nullptr, 1, common::base::eNodeType::SceneManagerNodeService, scene_manager::SendSceneManagerEnterScene};
    gRpcMethodRegistry[SceneManagerLeaveSceneMessageId] = RpcMethodMeta{
        "SceneManager", "LeaveScene",
        std::make_unique<::scene_manager::LeaveSceneRequest>(),
        std::make_unique<::Empty>(),
        nullptr, 1, common::base::eNodeType::SceneManagerNodeService, scene_manager::SendSceneManagerLeaveScene};

    // --- SceneNodeGrpc ---
    gRpcMethodRegistry[SceneNodeGrpcCreateSceneMessageId] = RpcMethodMeta{
        "SceneNodeGrpc", "CreateScene",
        std::make_unique<::CreateSceneRequest>(),
        std::make_unique<::CreateSceneResponse>(),
        nullptr, 1, common::base::eNodeType::SceneManagerNodeService, scene_node::SendSceneNodeGrpcCreateScene};
    gRpcMethodRegistry[SceneNodeGrpcDestroySceneMessageId] = RpcMethodMeta{
        "SceneNodeGrpc", "DestroyScene",
        std::make_unique<::DestroySceneRequest>(),
        std::make_unique<::Empty>(),
        nullptr, 1, common::base::eNodeType::SceneManagerNodeService, scene_node::SendSceneNodeGrpcDestroyScene};
    gRpcMethodRegistry[SceneNodeGrpcReleasePlayerMessageId] = RpcMethodMeta{
        "SceneNodeGrpc", "ReleasePlayer",
        std::make_unique<::scene_node::ReleasePlayerRequest>(),
        std::make_unique<::Empty>(),
        nullptr, 1, common::base::eNodeType::SceneManagerNodeService, scene_node::SendSceneNodeGrpcReleasePlayer};
    gRpcMethodRegistry[SceneNodeGrpcPrepareBattleMessageId] = RpcMethodMeta{
        "SceneNodeGrpc", "PrepareBattle",
        std::make_unique<::PrepareBattleRequest>(),
        std::make_unique<::PrepareBattleResponse>(),
        nullptr, 1, common::base::eNodeType::SceneManagerNodeService, scene_node::SendSceneNodeGrpcPrepareBattle};
    gRpcMethodRegistry[SceneNodeGrpcCancelBattlePrepareMessageId] = RpcMethodMeta{
        "SceneNodeGrpc", "CancelBattlePrepare",
        std::make_unique<::CancelBattlePrepareRequest>(),
        std::make_unique<::Empty>(),
        nullptr, 1, common::base::eNodeType::SceneManagerNodeService, scene_node::SendSceneNodeGrpcCancelBattlePrepare};
}

bool IsClientMessageId(uint32_t messageId)
{
	switch (messageId) {
	case BattleClientPlayerSubmitBattleActionMessageId:
	case BattleClientPlayerGetBattleStateMessageId:
	case BattleClientPlayerNotifyBattleStartMessageId:
	case BattleClientPlayerNotifyTurnResultMessageId:
	case BattleClientPlayerNotifyBattleEndMessageId:
	case BattleClientPlayerNotifyBattleReconnectMessageId:
	case BattleClientPlayerStopWatchBattleMessageId:
	case BattleClientPlayerSetAutoBattleMessageId:
	case BattleClientPlayerNotifySpectateStateMessageId:
	case BattleClientPlayerNotifySpectateTurnResultMessageId:
	case BattleClientPlayerNotifySpectateEndMessageId:
	case BattleClientPlayerNotifyBattleAssignedMessageId:
	case ClientPlayerChatSendChatMessageId:
	case ClientPlayerChatPullChatHistoryMessageId:
	case ClientPlayerLoginLoginMessageId:
	case ClientPlayerLoginCreatePlayerMessageId:
	case ClientPlayerLoginEnterGameMessageId:
	case ClientPlayerLoginLeaveGameMessageId:
	case ClientPlayerLoginDisconnectMessageId:
	case ClientPlayerLoginRefreshTokenMessageId:
	case MatchServiceJoinQueueMessageId:
	case MatchServiceCancelQueueMessageId:
	case MatchServiceGetQueueStatusMessageId:
	case MatchServiceChallengePlayerMessageId:
	case MatchServiceRespondChallengeMessageId:
	case MatchServiceNotifyChallengeInviteMessageId:
	case MatchServiceNotifyChallengeResultMessageId:
	case MatchServiceWatchBattleMessageId:
	case MatchServiceRequestBattleTicketMessageId:
	case MatchServiceListWatchableBattlesMessageId:
	case SceneClientPlayerCommonSendTipToClientMessageId:
	case SceneClientPlayerCommonKickPlayerMessageId:
	case SceneClientPlayerCommonRedirectToGateMessageId:
	case SceneAttributeClientPlayerGetAttributePanelMessageId:
	case SceneAttributeClientPlayerAllocateAttributePointsMessageId:
	case SceneAttributeClientPlayerResetAttributePointsMessageId:
	case SceneAttributeClientPlayerAutoAllocateAttributePointsMessageId:
	case SceneAttributeClientPlayerCreateAttributeSchemeMessageId:
	case SceneAttributeClientPlayerSwitchAttributeSchemeMessageId:
	case SceneAttributeClientPlayerRenameAttributeSchemeMessageId:
	case SceneAttributeClientPlayerNotifyAttributePanelChangedMessageId:
	case SceneAttributeClientPlayerGmSetPlayerLevelMessageId:
	case SceneCurrencyClientPlayerGmAddCurrencyMessageId:
	case SceneCurrencyClientPlayerGmDeductCurrencyMessageId:
	case SceneCurrencyClientPlayerGetCurrencyListMessageId:
	case SceneCurrencyClientPlayerGmBlockCurrencyMessageId:
	case SceneCurrencyClientPlayerGmUnblockCurrencyMessageId:
	case SceneMovementClientPlayerMoveStartMessageId:
	case SceneMovementClientPlayerMoveStopMessageId:
	case SceneMovementClientPlayerMoveSyncMessageId:
	case SceneMovementClientPlayerTeleportRequestMessageId:
	case SceneMovementClientPlayerNotifyMoveAckMessageId:
	case SceneMovementClientPlayerNotifyActorMoveMessageId:
	case SceneMovementClientPlayerNotifyActorMoveListMessageId:
	case SceneMovementClientPlayerNotifyTeleportMessageId:
	case ScenePetClientPlayerGetPetListMessageId:
	case ScenePetClientPlayerSummonPetMessageId:
	case ScenePetClientPlayerRecallPetMessageId:
	case ScenePetClientPlayerAllocatePetPointsMessageId:
	case ScenePetClientPlayerResetPetPointsMessageId:
	case ScenePetClientPlayerAutoAllocatePetPointsMessageId:
	case ScenePetClientPlayerRenamePetMessageId:
	case ScenePetClientPlayerNotifyPetListChangedMessageId:
	case ScenePetClientPlayerGmGrantPetMessageId:
	case SceneSceneClientPlayerEnterSceneMessageId:
	case SceneSceneClientPlayerNotifyEnterSceneMessageId:
	case SceneSceneClientPlayerSceneInfoC2SMessageId:
	case SceneSceneClientPlayerNotifySceneInfoMessageId:
	case SceneSceneClientPlayerNotifyActorCreateMessageId:
	case SceneSceneClientPlayerNotifyActorDestroyMessageId:
	case SceneSceneClientPlayerNotifyActorListCreateMessageId:
	case SceneSceneClientPlayerNotifyActorListDestroyMessageId:
	case SceneSkillClientPlayerReleaseSkillMessageId:
	case SceneSkillClientPlayerNotifySkillUsedMessageId:
	case SceneSkillClientPlayerNotifySkillInterruptedMessageId:
	case SceneSkillClientPlayerListSkillsMessageId:
		return true;
	default:
		return false;
	}
}

bool IsValidEventId(uint32_t eventId)
{
	return eventId < kMaxEventCount;
}

bool DispatchProtoEvent(uint32_t eventId, const std::string& payload)
{
	switch (eventId) {
	case AcceptMissionEventEventId: {
		AcceptMissionEvent event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case AfterEnterSceneEventId: {
		AfterEnterScene event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case AfterLeaveSceneEventId: {
		AfterLeaveScene event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case BattleConfirmedEventEventId: {
		BattleConfirmedEvent event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case BattleSettlementEventEventId: {
		BattleSettlementEvent event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case BeKillEventEventId: {
		BeKillEvent event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case BeforeEnterSceneEventId: {
		BeforeEnterScene event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case BeforeLeaveSceneEventId: {
		BeforeLeaveScene event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case BuffTestEventEventId: {
		BuffTestEvent event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case CombatStateAddedEventEventId: {
		CombatStateAddedEvent event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case CombatStateRemovedEventEventId: {
		CombatStateRemovedEvent event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case ConditionEventEventId: {
		ConditionEvent event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case ConnectToNodeEventEventId: {
		ConnectToNodeEvent event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case ContractsKafkaBattleResultEventEventId: {
		contracts::kafka::BattleResultEvent event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case ContractsKafkaBattleResultTeamEventId: {
		contracts::kafka::BattleResultTeam event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case ContractsKafkaBindBattleEventEventId: {
		contracts::kafka::BindBattleEvent event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case ContractsKafkaBindSessionEventEventId: {
		contracts::kafka::BindSessionEvent event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case ContractsKafkaBroadcastToAllEventEventId: {
		contracts::kafka::BroadcastToAllEvent event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case ContractsKafkaBroadcastToPlayersEventEventId: {
		contracts::kafka::BroadcastToPlayersEvent event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case ContractsKafkaBroadcastToSceneEventEventId: {
		contracts::kafka::BroadcastToSceneEvent event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case ContractsKafkaKickPlayerEventEventId: {
		contracts::kafka::KickPlayerEvent event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case ContractsKafkaPlayerDisconnectedEventEventId: {
		contracts::kafka::PlayerDisconnectedEvent event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case ContractsKafkaPlayerLeaseExpiredEventEventId: {
		contracts::kafka::PlayerLeaseExpiredEvent event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case ContractsKafkaPlayerLifecycleCommandEventId: {
		contracts::kafka::PlayerLifecycleCommand event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case ContractsKafkaPushToPlayerEventEventId: {
		contracts::kafka::PushToPlayerEvent event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case ContractsKafkaRedirectToGateEventEventId: {
		contracts::kafka::RedirectToGateEvent event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case ContractsKafkaRoutePlayerEventEventId: {
		contracts::kafka::RoutePlayerEvent event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case ContractsKafkaUnbindBattleEventEventId: {
		contracts::kafka::UnbindBattleEvent event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case InitializeActorCompsEventEventId: {
		InitializeActorCompsEvent event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case InitializeNpcCompsEventEventId: {
		InitializeNpcCompsEvent event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case InitializePlayerCompsEventEventId: {
		InitializePlayerCompsEvent event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case InterruptCurrentStateEventEventId: {
		InterruptCurrentStateEvent event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case OnAcceptedMissionEventEventId: {
		OnAcceptedMissionEvent event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case OnConnect2LoginEventId: {
		OnConnect2Login event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case OnMissionAwardEventEventId: {
		OnMissionAwardEvent event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case OnNodeAddEventEventId: {
		OnNodeAddEvent event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case OnNodeConnectedEventEventId: {
		OnNodeConnectedEvent event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case OnNodeRemoveEventEventId: {
		OnNodeRemoveEvent event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case OnSceneCreatedEventId: {
		OnSceneCreated event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case OnSceneDestroyedEventId: {
		OnSceneDestroyed event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case OnServerStartEventId: {
		OnServerStart event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case PlayerLoginEventEventId: {
		PlayerLoginEvent event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case PlayerMigrationAckEventEventId: {
		PlayerMigrationAckEvent event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case PlayerMigrationEventEventId: {
		PlayerMigrationEvent event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case PlayerUpgradeEventEventId: {
		PlayerUpgradeEvent event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case RegisterPlayerEventEventId: {
		RegisterPlayerEvent event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case S2CEnterSceneEventId: {
		S2CEnterScene event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	case SkillExecutedEventEventId: {
		SkillExecutedEvent event;
		if (!event.ParseFromString(payload)) {
			return false;
		}
		tlsEcs.dispatcher.trigger(event);
		return true;
	}
	default:
		return false;
	}
}
