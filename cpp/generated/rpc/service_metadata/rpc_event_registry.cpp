#include <array>
#include "rpc_event_registry.h"
#include "proto/common/base/node.pb.h"

#include "proto/battle/player_battle.pb.h"
#include "proto/battle/battle_node.pb.h"
#include "proto/chat/chat.pb.h"
#include "proto/client_rpc_router/client_rpc_router.pb.h"
#include "proto/data_service/data_service.pb.h"
#include "proto/etcd/etcd.pb.h"
#include "proto/friend/friend.pb.h"
#include "proto/gate/gate_client_spectate.pb.h"
#include "proto/gate/gate_service.pb.h"
#include "proto/guild/guild.pb.h"
#include "proto/guild/guild_internal.pb.h"
#include "proto/login/login.pb.h"
#include "proto/mail/mail.pb.h"
#include "proto/mail/mail_admin.pb.h"
#include "proto/match/match_internal.pb.h"
#include "proto/match/match_service.pb.h"
#include "proto/player_inbox/player_inbox.pb.h"
#include "proto/player_inbox/player_inbox_admin.pb.h"
#include "proto/player_locator/player_locator.pb.h"
#include "proto/rank/rank.pb.h"
#include "proto/rank/rank_admin.pb.h"
#include "proto/scene/client_player_common.pb.h"
#include "proto/scene/player_activity.pb.h"
#include "proto/scene/player_attribute.pb.h"
#include "proto/scene/player_bag.pb.h"
#include "proto/scene/player_currency.pb.h"
#include "proto/scene/player_lifecycle.pb.h"
#include "proto/scene/player_mission.pb.h"
#include "proto/scene/player_movement.pb.h"
#include "proto/scene/player_mystery_merchant.pb.h"
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
#include "proto/team/team.pb.h"
#include "proto/team/team_authority.pb.h"
#include "proto/trade/jubaozhai.pb.h"
#include "proto/trade/trade_admin.pb.h"

#include "rpc/service_metadata/player_battle_service_metadata.h"
#include "rpc/service_metadata/battle_node_service_metadata.h"
#include "rpc/service_metadata/chat_service_metadata.h"
#include "rpc/service_metadata/client_rpc_router_service_metadata.h"
#include "rpc/service_metadata/data_service_service_metadata.h"
#include "rpc/service_metadata/etcd_service_metadata.h"
#include "rpc/service_metadata/friend_service_metadata.h"
#include "rpc/service_metadata/gate_client_spectate_service_metadata.h"
#include "rpc/service_metadata/gate_service_service_metadata.h"
#include "rpc/service_metadata/guild_service_metadata.h"
#include "rpc/service_metadata/guild_internal_service_metadata.h"
#include "rpc/service_metadata/login_service_metadata.h"
#include "rpc/service_metadata/mail_service_metadata.h"
#include "rpc/service_metadata/mail_admin_service_metadata.h"
#include "rpc/service_metadata/match_internal_service_metadata.h"
#include "rpc/service_metadata/match_service_service_metadata.h"
#include "rpc/service_metadata/player_inbox_service_metadata.h"
#include "rpc/service_metadata/player_inbox_admin_service_metadata.h"
#include "rpc/service_metadata/player_locator_service_metadata.h"
#include "rpc/service_metadata/rank_service_metadata.h"
#include "rpc/service_metadata/rank_admin_service_metadata.h"
#include "rpc/service_metadata/client_player_common_service_metadata.h"
#include "rpc/service_metadata/player_activity_service_metadata.h"
#include "rpc/service_metadata/player_attribute_service_metadata.h"
#include "rpc/service_metadata/player_bag_service_metadata.h"
#include "rpc/service_metadata/player_currency_service_metadata.h"
#include "rpc/service_metadata/player_lifecycle_service_metadata.h"
#include "rpc/service_metadata/player_mission_service_metadata.h"
#include "rpc/service_metadata/player_movement_service_metadata.h"
#include "rpc/service_metadata/player_mystery_merchant_service_metadata.h"
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
#include "rpc/service_metadata/team_service_metadata.h"
#include "rpc/service_metadata/team_authority_service_metadata.h"
#include "rpc/service_metadata/jubaozhai_service_metadata.h"
#include "rpc/service_metadata/trade_admin_service_metadata.h"

#include "common_event_battle_event_event_id.h"
#include "contracts_kafka_match_event_event_id.h"
#include "contracts_kafka_gate_event_event_id.h"
#include "contracts_kafka_player_event_event_id.h"
#include "common_event_player_inbox_event_event_id.h"
#include "common_event_team_event_event_id.h"

void SendBattleClientPlayerSubmitBattleActionErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );
void SendBattleClientPlayerGetBattleStateErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );
void SendBattleClientPlayerNotifyBattleStartErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );
void SendBattleClientPlayerNotifyTurnResultErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );
void SendBattleClientPlayerNotifyBattleEndErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );
void SendBattleClientPlayerNotifyBattleReconnectErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );
void SendBattleClientPlayerStopWatchBattleErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );
void SendBattleClientPlayerSetAutoBattleErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );
void SendBattleClientPlayerNotifySpectateStateErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );
void SendBattleClientPlayerNotifySpectateTurnResultErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );
void SendBattleClientPlayerNotifySpectateEndErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );
void SendBattleClientPlayerNotifyBattleAssignedErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );
void SendBattleNodeQueryObserverErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );
void SendBattleNodeQueryBattleCreationErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );
void SendBattleNodeCreateBattleErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );
void SendBattleNodeDestroyBattleErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );
void SendBattleNodeAddObserverErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );
void SendBattleNodeRemoveObserverErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );
void SendBattleNodeIssueBattleTicketErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );
void SendBattleNodeAttachRelayGateErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );
void SendBattleNodePromoteSpectateBroadcastErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );
namespace chatpb{void SendClientPlayerChatSendChatErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace chatpb{void SendClientPlayerChatPullChatHistoryErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace client_rpc_router{void SendClientRpcRouterForwardErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceReadAccountDirectoryErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceCreateAccountRoleErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceLookupAccountRoleErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceRepairAccountRoleErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceRemoveAccountRolesErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceBeginAccountRoleTradeErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceConfirmAccountRoleTradeFreezeErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceCancelAccountRoleTradeErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceTransferAccountRoleErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceRegisterRoleAdmissionErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceCloseRoleAdmissionsErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceAckRoleTradeDrainErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceLoadPlayerDataErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceSavePlayerDataErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceGetPlayerFieldErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceSetPlayerFieldErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceRegisterPlayerZoneErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceGetPlayerHomeZoneErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceBatchGetPlayerHomeZoneErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceRemapHomeZoneForMergeErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceDeletePlayerDataErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceCreatePlayerSnapshotErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceListPlayerSnapshotsErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceGetPlayerSnapshotDiffErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceRollbackPlayerErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceRollbackZoneErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceRollbackAllErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceBatchRecallItemsErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceQueryTransactionLogErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceCreateEventSnapshotErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceAllocateIdSegmentErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceReservePlayerNameErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceReleasePlayerNameErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceBatchGetPlayerNameErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceGetPlayerAssetOpLedgerErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceGetPlayerInboxLedgerErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace etcdserverpb{void SendKVRangeErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace etcdserverpb{void SendKVPutErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace etcdserverpb{void SendKVDeleteRangeErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace etcdserverpb{void SendKVTxnErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace etcdserverpb{void SendKVCompactErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace etcdserverpb{void SendLeaseLeaseGrantErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace etcdserverpb{void SendLeaseLeaseRevokeErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace etcdserverpb{void SendLeaseLeaseTimeToLiveErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace etcdserverpb{void SendLeaseLeaseLeasesErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace friendpb{void SendClientPlayerFriendAddFriendErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace friendpb{void SendClientPlayerFriendAcceptFriendErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace friendpb{void SendClientPlayerFriendRejectFriendErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace friendpb{void SendClientPlayerFriendRemoveFriendErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace friendpb{void SendClientPlayerFriendGetFriendListErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace friendpb{void SendClientPlayerFriendGetPendingRequestsErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace friendpb{void SendClientPlayerFriendBlockErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace friendpb{void SendClientPlayerFriendUnblockErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace friendpb{void SendClientPlayerFriendListBlocksErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace friendpb{void SendClientPlayerFriendRecommendFriendsErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace friendpb{void SendClientPlayerFriendNotifyFriendEventErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace guildpb{void SendGuildServiceCreateGuildErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace guildpb{void SendGuildServiceGetGuildErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace guildpb{void SendGuildServiceGetPlayerGuildErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace guildpb{void SendGuildServiceLeaveGuildErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace guildpb{void SendGuildServiceDisbandGuildErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace guildpb{void SendGuildServiceSetAnnouncementErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace guildpb{void SendGuildServiceSetGuildMemberRoleErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace guildpb{void SendGuildServiceKickGuildMemberErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace guildpb{void SendGuildServiceTransferGuildLeaderErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace guildpb{void SendGuildServiceApplyJoinGuildErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace guildpb{void SendGuildServiceCancelGuildApplicationErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace guildpb{void SendGuildServiceListMyGuildApplicationsErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace guildpb{void SendGuildServiceListGuildApplicationsErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace guildpb{void SendGuildServiceReviewGuildApplicationErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace guildpb{void SendGuildServiceNotifyGuildChangedErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace guildpb{void SendGuildServiceUpdateGuildScoreErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace guildpb{void SendGuildServiceGetGuildRankErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace guildpb{void SendGuildServiceGetGuildRankByGuildErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace guildpb{void SendGuildServiceGetGuildDonateOptionsErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace guildpb{void SendGuildServiceDonateToGuildErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace guildpb{void SendGuildServiceUpgradeGuildErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace guildpb{void SendGuildServiceGetGuildShopErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace guildpb{void SendGuildServiceBuyGuildShopGoodsErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace guildpb{void SendGuildServiceGetGuildActivitiesErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace guildpb{void SendGuildServiceLightGuildLanternErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace guildpb{void SendGuildServiceClaimGuildReunionErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace guildpb{void SendGuildServiceStartGuildTrialErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace guildpb{void SendGuildServiceRespondGuildTrialInviteErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace guildpb{void SendGuildInternalListAppliedAssetOpsSinceErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace loginpb{void SendClientPlayerLoginLoginErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace loginpb{void SendClientPlayerLoginCreatePlayerErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace loginpb{void SendClientPlayerLoginEnterGameErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace loginpb{void SendClientPlayerLoginLeaveGameErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace loginpb{void SendClientPlayerLoginDisconnectErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace loginpb{void SendClientPlayerLoginRefreshTokenErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace loginpb{void SendLoginPreGateAssignGateErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace loginpb{void SendLoginPreGateQueryQueueStatusErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace loginpb{void SendLoginAdminRemovePlayersFromAccountsErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace mailpb{void SendClientPlayerMailListMailsErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace mailpb{void SendClientPlayerMailReadMailErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace mailpb{void SendClientPlayerMailClaimMailAttachmentsErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace mailpb{void SendClientPlayerMailDeleteMailsErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace mailpb{void SendClientPlayerMailClearReadMailsErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace mailpb{void SendClientPlayerMailNotifyMailChangedErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace mailpb{void SendMailAdminSendPersonalMailErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace mailpb{void SendMailAdminSendSystemMailErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace mailpb{void SendMailAdminSendGuildMailErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace mailpb{void SendMailAdminImportMailRecipientsErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace mailpb{void SendMailAdminSealMailCampaignErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace match{void SendMatchInternalStartActivityBattleErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace match{void SendMatchInternalCancelActivityBattleErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace match{void SendMatchInternalCommitBattleOutcomeErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace match{void SendMatchInternalSetSpectateFeaturedErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace match{void SendMatchServiceGetNetworkProbePlanErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace match{void SendMatchServiceCommitNetworkProbeErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace match{void SendMatchServiceJoinQueueErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace match{void SendMatchServiceCancelQueueErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace match{void SendMatchServiceGetQueueStatusErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace match{void SendMatchServiceChallengePlayerErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace match{void SendMatchServiceRespondChallengeErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace match{void SendMatchServiceNotifyChallengeInviteErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace match{void SendMatchServiceNotifyChallengeResultErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace match{void SendMatchServiceWatchBattleErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace match{void SendMatchServiceRequestBattleTicketErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace match{void SendMatchServiceListWatchableBattlesErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace player_inbox{void SendPlayerInboxFetchItemsErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace player_inbox{void SendPlayerInboxAckItemsErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace player_inbox{void SendPlayerInboxEnqueueBattleSettlementErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace player_inbox{void SendPlayerInboxLookupBattleSettlementErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace player_inbox{void SendPlayerInboxAdminGmSanctionPlayerErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace player_inbox{void SendPlayerInboxAdminGetSummaryErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace player_inbox{void SendPlayerInboxAdminListItemsErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace player_inbox{void SendPlayerInboxAdminLookupByRequestErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace player_inbox{void SendPlayerInboxAdminVoidItemErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace player_inbox{void SendPlayerInboxAdminReconcilePlayerErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace playerlocator{void SendPlayerLocatorSetLocationErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace playerlocator{void SendPlayerLocatorGetLocationErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace playerlocator{void SendPlayerLocatorMarkOfflineErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace playerlocator{void SendPlayerLocatorSetSessionErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace playerlocator{void SendPlayerLocatorGetSessionErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace playerlocator{void SendPlayerLocatorSetDisconnectingErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace playerlocator{void SendPlayerLocatorReconnectErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace rankpb{void SendClientPlayerRankGetRankPageErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace rankpb{void SendClientPlayerRankGetRankAroundErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace rankpb{void SendRankAdminOpenRankGenerationErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace rankpb{void SendRankAdminSubmitRankScoresErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace rankpb{void SendRankAdminFreezeRankGenerationErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace rankpb{void SendRankAdminRemoveRankEntryErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace rankpb{void SendRankAdminDeleteRankGenerationErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace scene_manager{void SendSceneManagerCreateSceneErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace scene_manager{void SendSceneManagerDestroySceneErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace scene_manager{void SendSceneManagerEnterSceneErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace scene_manager{void SendSceneManagerLeaveSceneErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace scene_node{void SendSceneNodeGrpcCreateSceneErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace scene_node{void SendSceneNodeGrpcDestroySceneErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace scene_node{void SendSceneNodeGrpcReleasePlayerErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace scene_node{void SendSceneNodeGrpcPrepareBattleErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace scene_node{void SendSceneNodeGrpcCancelBattlePrepareErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace scene_node{void SendSceneNodeGrpcSealAccountRoleTradeErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace scene_node{void SendSceneNodeGrpcAssetDebitErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace scene_node{void SendSceneNodeGrpcAssetAbortDebitErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace scene_node{void SendSceneNodeGrpcAssetCreditErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace scene_node{void SendSceneNodeGrpcAssetAckReceiptErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace teampb{void SendClientPlayerTeamCreateTeamErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace teampb{void SendClientPlayerTeamGetMyTeamErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace teampb{void SendClientPlayerTeamApplyJoinTeamErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace teampb{void SendClientPlayerTeamHandleApplicationErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace teampb{void SendClientPlayerTeamInviteToTeamErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace teampb{void SendClientPlayerTeamRespondInviteErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace teampb{void SendClientPlayerTeamListMyInvitesErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace teampb{void SendClientPlayerTeamLeaveTeamErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace teampb{void SendClientPlayerTeamKickMemberErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace teampb{void SendClientPlayerTeamTransferLeaderErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace teampb{void SendClientPlayerTeamDisbandTeamErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace teampb{void SendClientPlayerTeamStartTeamMatchErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace teampb{void SendClientPlayerTeamNotifyTeamSnapshotErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace teampb{void SendClientPlayerTeamNotifyTeamInviteErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace teampb{void SendClientPlayerTeamNotifyTeamEventErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace teampb{void SendTeamAuthorityReadMembershipErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace trade{void SendClientPlayerJubaozhaiBrowseListingsErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace trade{void SendClientPlayerJubaozhaiGetListingDetailErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace trade{void SendClientPlayerJubaozhaiSetFavoriteErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace trade{void SendClientPlayerJubaozhaiGetMyShelfErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace trade{void SendClientPlayerJubaozhaiListOfficialSkusErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace trade{void SendClientPlayerJubaozhaiCreateOfficialOrderErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace trade{void SendClientPlayerJubaozhaiGetCommerceOrderErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace trade{void SendClientPlayerJubaozhaiCancelCommerceOrderErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace trade{void SendClientPlayerJubaozhaiListCommerceOrdersErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace trade{void SendClientPlayerJubaozhaiCreatePlayerTradeErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace trade{void SendClientPlayerJubaozhaiConfirmPlayerTradeErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace trade{void SendClientPlayerJubaozhaiCreateConsignmentErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace trade{void SendClientPlayerJubaozhaiBuyConsignmentErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace trade{void SendClientPlayerJubaozhaiCancelConsignmentErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace trade{void SendClientPlayerJubaozhaiPlaceMarketOrderErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace trade{void SendClientPlayerJubaozhaiCancelMarketOrderErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace trade{void SendClientPlayerJubaozhaiListMarketOrdersErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace trade{void SendClientPlayerJubaozhaiGetMarketDepthErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace trade{void SendClientPlayerJubaozhaiCreateAuctionLotErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace trade{void SendClientPlayerJubaozhaiPlaceAuctionBidErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace trade{void SendClientPlayerJubaozhaiGetAuctionLotErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace trade{void SendClientPlayerJubaozhaiCancelAuctionLotErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace trade{void SendClientPlayerJubaozhaiBrowseCommerceOffersErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace trade{void SendClientPlayerJubaozhaiListTradeMarketsErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace trade{void SendClientPlayerJubaozhaiCreateRmbConsignmentErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace trade{void SendClientPlayerJubaozhaiGetRmbConsignmentErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace trade{void SendClientPlayerJubaozhaiListRmbConsignmentsErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace trade{void SendClientPlayerJubaozhaiBuyRmbConsignmentErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace trade{void SendClientPlayerJubaozhaiCancelRmbConsignmentErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace trade{void SendClientPlayerJubaozhaiGetRmbOrderErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace trade{void SendClientPlayerJubaozhaiListRmbOrdersErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace trade{void SendClientPlayerJubaozhaiCancelRmbOrderErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace trade{void SendClientPlayerJubaozhaiListSellerIncomesErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace trade{void SendClientPlayerJubaozhaiCreateWithdrawalErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace trade{void SendClientPlayerJubaozhaiGetWithdrawalErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace trade{void SendTradeAdminSeedListingErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}

// 容量以 rpc_event_registry.h 的 kMaxRpcMethodCount 为准;static_assert 把
// "半途 regen 导致头文件容量落后于本轮 message id 数"的事故(2026-09-01,
// InitMessageInfo 越界写导致节点启动断言)变成编译错误而不是运行期崩溃。
static_assert(kMaxRpcMethodCount == 354,
    "kMaxRpcMethodCount out of sync with this generation run - rerun the full proto generator");
std::array<RpcMethodMeta, kMaxRpcMethodCount> gRpcMethodRegistry;

void InitMessageInfo()
{
    // --- BattleClientPlayer ---
    gRpcMethodRegistry[BattleClientPlayerSubmitBattleActionMessageId] = RpcMethodMeta{
        "BattleClientPlayer", "SubmitBattleAction",
        std::make_unique<::SubmitBattleActionRequest>(),
        std::make_unique<::SubmitBattleActionResponse>(),
        1, common::base::eNodeType::BattleNodeService, ::SendBattleClientPlayerSubmitBattleActionErased};
    gRpcMethodRegistry[BattleClientPlayerGetBattleStateMessageId] = RpcMethodMeta{
        "BattleClientPlayer", "GetBattleState",
        std::make_unique<::GetBattleStateRequest>(),
        std::make_unique<::BattleStateS2C>(),
        1, common::base::eNodeType::BattleNodeService, ::SendBattleClientPlayerGetBattleStateErased};
    gRpcMethodRegistry[BattleClientPlayerNotifyBattleStartMessageId] = RpcMethodMeta{
        "BattleClientPlayer", "NotifyBattleStart",
        std::make_unique<::BattleStartS2C>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::BattleNodeService, ::SendBattleClientPlayerNotifyBattleStartErased};
    gRpcMethodRegistry[BattleClientPlayerNotifyTurnResultMessageId] = RpcMethodMeta{
        "BattleClientPlayer", "NotifyTurnResult",
        std::make_unique<::TurnResultS2C>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::BattleNodeService, ::SendBattleClientPlayerNotifyTurnResultErased};
    gRpcMethodRegistry[BattleClientPlayerNotifyBattleEndMessageId] = RpcMethodMeta{
        "BattleClientPlayer", "NotifyBattleEnd",
        std::make_unique<::BattleEndS2C>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::BattleNodeService, ::SendBattleClientPlayerNotifyBattleEndErased};
    gRpcMethodRegistry[BattleClientPlayerNotifyBattleReconnectMessageId] = RpcMethodMeta{
        "BattleClientPlayer", "NotifyBattleReconnect",
        std::make_unique<::BattleReconnectS2C>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::BattleNodeService, ::SendBattleClientPlayerNotifyBattleReconnectErased};
    gRpcMethodRegistry[BattleClientPlayerStopWatchBattleMessageId] = RpcMethodMeta{
        "BattleClientPlayer", "StopWatchBattle",
        std::make_unique<::StopWatchBattleRequest>(),
        std::make_unique<::StopWatchBattleResponse>(),
        1, common::base::eNodeType::BattleNodeService, ::SendBattleClientPlayerStopWatchBattleErased};
    gRpcMethodRegistry[BattleClientPlayerSetAutoBattleMessageId] = RpcMethodMeta{
        "BattleClientPlayer", "SetAutoBattle",
        std::make_unique<::SetAutoBattleRequest>(),
        std::make_unique<::SetAutoBattleResponse>(),
        1, common::base::eNodeType::BattleNodeService, ::SendBattleClientPlayerSetAutoBattleErased};
    gRpcMethodRegistry[BattleClientPlayerNotifySpectateStateMessageId] = RpcMethodMeta{
        "BattleClientPlayer", "NotifySpectateState",
        std::make_unique<::SpectateStateS2C>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::BattleNodeService, ::SendBattleClientPlayerNotifySpectateStateErased};
    gRpcMethodRegistry[BattleClientPlayerNotifySpectateTurnResultMessageId] = RpcMethodMeta{
        "BattleClientPlayer", "NotifySpectateTurnResult",
        std::make_unique<::TurnResultS2C>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::BattleNodeService, ::SendBattleClientPlayerNotifySpectateTurnResultErased};
    gRpcMethodRegistry[BattleClientPlayerNotifySpectateEndMessageId] = RpcMethodMeta{
        "BattleClientPlayer", "NotifySpectateEnd",
        std::make_unique<::SpectateEndS2C>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::BattleNodeService, ::SendBattleClientPlayerNotifySpectateEndErased};
    gRpcMethodRegistry[BattleClientPlayerNotifyBattleAssignedMessageId] = RpcMethodMeta{
        "BattleClientPlayer", "NotifyBattleAssigned",
        std::make_unique<::BattleAssignedS2C>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::BattleNodeService, ::SendBattleClientPlayerNotifyBattleAssignedErased};

    // --- BattleNode ---
    gRpcMethodRegistry[BattleNodeQueryObserverMessageId] = RpcMethodMeta{
        "BattleNode", "QueryObserver",
        std::make_unique<::QueryObserverRequest>(),
        std::make_unique<::QueryObserverResponse>(),
        1, common::base::eNodeType::BattleNodeService, ::SendBattleNodeQueryObserverErased};
    gRpcMethodRegistry[BattleNodeQueryBattleCreationMessageId] = RpcMethodMeta{
        "BattleNode", "QueryBattleCreation",
        std::make_unique<::QueryBattleCreationRequest>(),
        std::make_unique<::QueryBattleCreationResponse>(),
        1, common::base::eNodeType::BattleNodeService, ::SendBattleNodeQueryBattleCreationErased};
    gRpcMethodRegistry[BattleNodeCreateBattleMessageId] = RpcMethodMeta{
        "BattleNode", "CreateBattle",
        std::make_unique<::CreateBattleRequest>(),
        std::make_unique<::CreateBattleResponse>(),
        1, common::base::eNodeType::BattleNodeService, ::SendBattleNodeCreateBattleErased};
    gRpcMethodRegistry[BattleNodeDestroyBattleMessageId] = RpcMethodMeta{
        "BattleNode", "DestroyBattle",
        std::make_unique<::DestroyBattleRequest>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::BattleNodeService, ::SendBattleNodeDestroyBattleErased};
    gRpcMethodRegistry[BattleNodeAddObserverMessageId] = RpcMethodMeta{
        "BattleNode", "AddObserver",
        std::make_unique<::AddObserverRequest>(),
        std::make_unique<::AddObserverResponse>(),
        1, common::base::eNodeType::BattleNodeService, ::SendBattleNodeAddObserverErased};
    gRpcMethodRegistry[BattleNodeRemoveObserverMessageId] = RpcMethodMeta{
        "BattleNode", "RemoveObserver",
        std::make_unique<::RemoveObserverRequest>(),
        std::make_unique<::RemoveObserverResponse>(),
        1, common::base::eNodeType::BattleNodeService, ::SendBattleNodeRemoveObserverErased};
    gRpcMethodRegistry[BattleNodeIssueBattleTicketMessageId] = RpcMethodMeta{
        "BattleNode", "IssueBattleTicket",
        std::make_unique<::IssueBattleTicketRequest>(),
        std::make_unique<::IssueBattleTicketResponse>(),
        1, common::base::eNodeType::BattleNodeService, ::SendBattleNodeIssueBattleTicketErased};
    gRpcMethodRegistry[BattleNodeAttachRelayGateMessageId] = RpcMethodMeta{
        "BattleNode", "AttachRelayGate",
        std::make_unique<::AttachRelayGateRequest>(),
        std::make_unique<::AttachRelayGateResponse>(),
        1, common::base::eNodeType::BattleNodeService, ::SendBattleNodeAttachRelayGateErased};
    gRpcMethodRegistry[BattleNodePromoteSpectateBroadcastMessageId] = RpcMethodMeta{
        "BattleNode", "PromoteSpectateBroadcast",
        std::make_unique<::PromoteSpectateBroadcastRequest>(),
        std::make_unique<::PromoteSpectateBroadcastResponse>(),
        1, common::base::eNodeType::BattleNodeService, ::SendBattleNodePromoteSpectateBroadcastErased};

    // --- ClientPlayerChat ---
    gRpcMethodRegistry[ClientPlayerChatSendChatMessageId] = RpcMethodMeta{
        "ClientPlayerChat", "SendChat",
        std::make_unique<::chatpb::SendChatRequest>(),
        std::make_unique<::chatpb::SendChatResponse>(),
        1, common::base::eNodeType::ChatNodeService, chatpb::SendClientPlayerChatSendChatErased};
    gRpcMethodRegistry[ClientPlayerChatPullChatHistoryMessageId] = RpcMethodMeta{
        "ClientPlayerChat", "PullChatHistory",
        std::make_unique<::chatpb::PullChatHistoryRequest>(),
        std::make_unique<::chatpb::PullChatHistoryResponse>(),
        1, common::base::eNodeType::ChatNodeService, chatpb::SendClientPlayerChatPullChatHistoryErased};

    // --- ClientRpcRouter ---
    gRpcMethodRegistry[ClientRpcRouterForwardMessageId] = RpcMethodMeta{
        "ClientRpcRouter", "Forward",
        std::make_unique<::client_rpc_router::ForwardRequest>(),
        std::make_unique<::MessageContent>(),
        1, common::base::eNodeType::ClientRpcRouterNodeService, client_rpc_router::SendClientRpcRouterForwardErased};

    // --- DataService ---
    gRpcMethodRegistry[DataServiceReadAccountDirectoryMessageId] = RpcMethodMeta{
        "DataService", "ReadAccountDirectory",
        std::make_unique<::data_service::ReadAccountDirectoryRequest>(),
        std::make_unique<::data_service::AccountDirectoryResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceReadAccountDirectoryErased};
    gRpcMethodRegistry[DataServiceCreateAccountRoleMessageId] = RpcMethodMeta{
        "DataService", "CreateAccountRole",
        std::make_unique<::data_service::CreateAccountRoleRequest>(),
        std::make_unique<::data_service::AccountDirectoryResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceCreateAccountRoleErased};
    gRpcMethodRegistry[DataServiceLookupAccountRoleMessageId] = RpcMethodMeta{
        "DataService", "LookupAccountRole",
        std::make_unique<::data_service::LookupAccountRoleRequest>(),
        std::make_unique<::data_service::AccountRoleOwnership>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceLookupAccountRoleErased};
    gRpcMethodRegistry[DataServiceRepairAccountRoleMessageId] = RpcMethodMeta{
        "DataService", "RepairAccountRole",
        std::make_unique<::data_service::RepairAccountRoleRequest>(),
        std::make_unique<::data_service::AccountDirectoryResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceRepairAccountRoleErased};
    gRpcMethodRegistry[DataServiceRemoveAccountRolesMessageId] = RpcMethodMeta{
        "DataService", "RemoveAccountRoles",
        std::make_unique<::data_service::RemoveAccountRolesRequest>(),
        std::make_unique<::google::protobuf::Empty>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceRemoveAccountRolesErased};
    gRpcMethodRegistry[DataServiceBeginAccountRoleTradeMessageId] = RpcMethodMeta{
        "DataService", "BeginAccountRoleTrade",
        std::make_unique<::data_service::BeginAccountRoleTradeRequest>(),
        std::make_unique<::data_service::AccountRoleTradeReceipt>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceBeginAccountRoleTradeErased};
    gRpcMethodRegistry[DataServiceConfirmAccountRoleTradeFreezeMessageId] = RpcMethodMeta{
        "DataService", "ConfirmAccountRoleTradeFreeze",
        std::make_unique<::data_service::ConfirmAccountRoleTradeFreezeRequest>(),
        std::make_unique<::data_service::AccountRoleTradeReceipt>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceConfirmAccountRoleTradeFreezeErased};
    gRpcMethodRegistry[DataServiceCancelAccountRoleTradeMessageId] = RpcMethodMeta{
        "DataService", "CancelAccountRoleTrade",
        std::make_unique<::data_service::CancelAccountRoleTradeRequest>(),
        std::make_unique<::data_service::AccountRoleTradeReceipt>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceCancelAccountRoleTradeErased};
    gRpcMethodRegistry[DataServiceTransferAccountRoleMessageId] = RpcMethodMeta{
        "DataService", "TransferAccountRole",
        std::make_unique<::data_service::TransferAccountRoleRequest>(),
        std::make_unique<::data_service::AccountRoleTransferReceipt>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceTransferAccountRoleErased};
    gRpcMethodRegistry[DataServiceRegisterRoleAdmissionMessageId] = RpcMethodMeta{
        "DataService", "RegisterRoleAdmission",
        std::make_unique<::data_service::RegisterRoleAdmissionRequest>(),
        std::make_unique<::data_service::RegisterRoleAdmissionResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceRegisterRoleAdmissionErased};
    gRpcMethodRegistry[DataServiceCloseRoleAdmissionsMessageId] = RpcMethodMeta{
        "DataService", "CloseRoleAdmissions",
        std::make_unique<::data_service::CloseRoleAdmissionsRequest>(),
        std::make_unique<::data_service::CloseRoleAdmissionsResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceCloseRoleAdmissionsErased};
    gRpcMethodRegistry[DataServiceAckRoleTradeDrainMessageId] = RpcMethodMeta{
        "DataService", "AckRoleTradeDrain",
        std::make_unique<::data_service::AckRoleTradeDrainRequest>(),
        std::make_unique<::google::protobuf::Empty>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceAckRoleTradeDrainErased};
    gRpcMethodRegistry[DataServiceLoadPlayerDataMessageId] = RpcMethodMeta{
        "DataService", "LoadPlayerData",
        std::make_unique<::data_service::LoadPlayerDataRequest>(),
        std::make_unique<::data_service::LoadPlayerDataResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceLoadPlayerDataErased};
    gRpcMethodRegistry[DataServiceSavePlayerDataMessageId] = RpcMethodMeta{
        "DataService", "SavePlayerData",
        std::make_unique<::data_service::SavePlayerDataRequest>(),
        std::make_unique<::data_service::SavePlayerDataResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceSavePlayerDataErased};
    gRpcMethodRegistry[DataServiceGetPlayerFieldMessageId] = RpcMethodMeta{
        "DataService", "GetPlayerField",
        std::make_unique<::data_service::GetPlayerFieldRequest>(),
        std::make_unique<::data_service::GetPlayerFieldResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceGetPlayerFieldErased};
    gRpcMethodRegistry[DataServiceSetPlayerFieldMessageId] = RpcMethodMeta{
        "DataService", "SetPlayerField",
        std::make_unique<::data_service::SetPlayerFieldRequest>(),
        std::make_unique<::data_service::SetPlayerFieldResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceSetPlayerFieldErased};
    gRpcMethodRegistry[DataServiceRegisterPlayerZoneMessageId] = RpcMethodMeta{
        "DataService", "RegisterPlayerZone",
        std::make_unique<::data_service::RegisterPlayerZoneRequest>(),
        std::make_unique<::google::protobuf::Empty>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceRegisterPlayerZoneErased};
    gRpcMethodRegistry[DataServiceGetPlayerHomeZoneMessageId] = RpcMethodMeta{
        "DataService", "GetPlayerHomeZone",
        std::make_unique<::data_service::GetPlayerHomeZoneRequest>(),
        std::make_unique<::data_service::GetPlayerHomeZoneResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceGetPlayerHomeZoneErased};
    gRpcMethodRegistry[DataServiceBatchGetPlayerHomeZoneMessageId] = RpcMethodMeta{
        "DataService", "BatchGetPlayerHomeZone",
        std::make_unique<::data_service::BatchGetPlayerHomeZoneRequest>(),
        std::make_unique<::data_service::BatchGetPlayerHomeZoneResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceBatchGetPlayerHomeZoneErased};
    gRpcMethodRegistry[DataServiceRemapHomeZoneForMergeMessageId] = RpcMethodMeta{
        "DataService", "RemapHomeZoneForMerge",
        std::make_unique<::data_service::RemapHomeZoneForMergeRequest>(),
        std::make_unique<::data_service::RemapHomeZoneForMergeResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceRemapHomeZoneForMergeErased};
    gRpcMethodRegistry[DataServiceDeletePlayerDataMessageId] = RpcMethodMeta{
        "DataService", "DeletePlayerData",
        std::make_unique<::data_service::DeletePlayerDataRequest>(),
        std::make_unique<::data_service::DeletePlayerDataResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceDeletePlayerDataErased};
    gRpcMethodRegistry[DataServiceCreatePlayerSnapshotMessageId] = RpcMethodMeta{
        "DataService", "CreatePlayerSnapshot",
        std::make_unique<::data_service::CreatePlayerSnapshotRequest>(),
        std::make_unique<::data_service::CreatePlayerSnapshotResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceCreatePlayerSnapshotErased};
    gRpcMethodRegistry[DataServiceListPlayerSnapshotsMessageId] = RpcMethodMeta{
        "DataService", "ListPlayerSnapshots",
        std::make_unique<::data_service::ListPlayerSnapshotsRequest>(),
        std::make_unique<::data_service::ListPlayerSnapshotsResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceListPlayerSnapshotsErased};
    gRpcMethodRegistry[DataServiceGetPlayerSnapshotDiffMessageId] = RpcMethodMeta{
        "DataService", "GetPlayerSnapshotDiff",
        std::make_unique<::data_service::GetPlayerSnapshotDiffRequest>(),
        std::make_unique<::data_service::GetPlayerSnapshotDiffResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceGetPlayerSnapshotDiffErased};
    gRpcMethodRegistry[DataServiceRollbackPlayerMessageId] = RpcMethodMeta{
        "DataService", "RollbackPlayer",
        std::make_unique<::data_service::RollbackPlayerRequest>(),
        std::make_unique<::data_service::RollbackPlayerResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceRollbackPlayerErased};
    gRpcMethodRegistry[DataServiceRollbackZoneMessageId] = RpcMethodMeta{
        "DataService", "RollbackZone",
        std::make_unique<::data_service::RollbackZoneRequest>(),
        std::make_unique<::data_service::RollbackZoneResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceRollbackZoneErased};
    gRpcMethodRegistry[DataServiceRollbackAllMessageId] = RpcMethodMeta{
        "DataService", "RollbackAll",
        std::make_unique<::data_service::RollbackAllRequest>(),
        std::make_unique<::data_service::RollbackAllResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceRollbackAllErased};
    gRpcMethodRegistry[DataServiceBatchRecallItemsMessageId] = RpcMethodMeta{
        "DataService", "BatchRecallItems",
        std::make_unique<::data_service::BatchRecallItemsRequest>(),
        std::make_unique<::data_service::BatchRecallItemsResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceBatchRecallItemsErased};
    gRpcMethodRegistry[DataServiceQueryTransactionLogMessageId] = RpcMethodMeta{
        "DataService", "QueryTransactionLog",
        std::make_unique<::data_service::QueryTransactionLogRequest>(),
        std::make_unique<::data_service::QueryTransactionLogResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceQueryTransactionLogErased};
    gRpcMethodRegistry[DataServiceCreateEventSnapshotMessageId] = RpcMethodMeta{
        "DataService", "CreateEventSnapshot",
        std::make_unique<::data_service::CreateEventSnapshotRequest>(),
        std::make_unique<::data_service::CreateEventSnapshotResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceCreateEventSnapshotErased};
    gRpcMethodRegistry[DataServiceAllocateIdSegmentMessageId] = RpcMethodMeta{
        "DataService", "AllocateIdSegment",
        std::make_unique<::data_service::AllocateIdSegmentRequest>(),
        std::make_unique<::data_service::AllocateIdSegmentResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceAllocateIdSegmentErased};
    gRpcMethodRegistry[DataServiceReservePlayerNameMessageId] = RpcMethodMeta{
        "DataService", "ReservePlayerName",
        std::make_unique<::data_service::ReservePlayerNameRequest>(),
        std::make_unique<::data_service::ReservePlayerNameResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceReservePlayerNameErased};
    gRpcMethodRegistry[DataServiceReleasePlayerNameMessageId] = RpcMethodMeta{
        "DataService", "ReleasePlayerName",
        std::make_unique<::data_service::ReleasePlayerNameRequest>(),
        std::make_unique<::google::protobuf::Empty>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceReleasePlayerNameErased};
    gRpcMethodRegistry[DataServiceBatchGetPlayerNameMessageId] = RpcMethodMeta{
        "DataService", "BatchGetPlayerName",
        std::make_unique<::data_service::BatchGetPlayerNameRequest>(),
        std::make_unique<::data_service::BatchGetPlayerNameResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceBatchGetPlayerNameErased};
    gRpcMethodRegistry[DataServiceGetPlayerAssetOpLedgerMessageId] = RpcMethodMeta{
        "DataService", "GetPlayerAssetOpLedger",
        std::make_unique<::data_service::GetPlayerAssetOpLedgerRequest>(),
        std::make_unique<::data_service::GetPlayerAssetOpLedgerResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceGetPlayerAssetOpLedgerErased};
    gRpcMethodRegistry[DataServiceGetPlayerInboxLedgerMessageId] = RpcMethodMeta{
        "DataService", "GetPlayerInboxLedger",
        std::make_unique<::data_service::GetPlayerInboxLedgerRequest>(),
        std::make_unique<::data_service::GetPlayerInboxLedgerResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceGetPlayerInboxLedgerErased};

    // --- KV ---
    gRpcMethodRegistry[KVRangeMessageId] = RpcMethodMeta{
        "KV", "Range",
        std::make_unique<::etcdserverpb::RangeRequest>(),
        std::make_unique<::etcdserverpb::RangeResponse>(),
        0, common::base::eNodeType::EtcdNodeService, etcdserverpb::SendKVRangeErased};
    gRpcMethodRegistry[KVPutMessageId] = RpcMethodMeta{
        "KV", "Put",
        std::make_unique<::etcdserverpb::PutRequest>(),
        std::make_unique<::etcdserverpb::PutResponse>(),
        0, common::base::eNodeType::EtcdNodeService, etcdserverpb::SendKVPutErased};
    gRpcMethodRegistry[KVDeleteRangeMessageId] = RpcMethodMeta{
        "KV", "DeleteRange",
        std::make_unique<::etcdserverpb::DeleteRangeRequest>(),
        std::make_unique<::etcdserverpb::DeleteRangeResponse>(),
        0, common::base::eNodeType::EtcdNodeService, etcdserverpb::SendKVDeleteRangeErased};
    gRpcMethodRegistry[KVTxnMessageId] = RpcMethodMeta{
        "KV", "Txn",
        std::make_unique<::etcdserverpb::TxnRequest>(),
        std::make_unique<::etcdserverpb::TxnResponse>(),
        0, common::base::eNodeType::EtcdNodeService, etcdserverpb::SendKVTxnErased};
    gRpcMethodRegistry[KVCompactMessageId] = RpcMethodMeta{
        "KV", "Compact",
        std::make_unique<::etcdserverpb::CompactionRequest>(),
        std::make_unique<::etcdserverpb::CompactionResponse>(),
        0, common::base::eNodeType::EtcdNodeService, etcdserverpb::SendKVCompactErased};

    // --- Watch ---
    gRpcMethodRegistry[WatchWatchMessageId] = RpcMethodMeta{
        "Watch", "Watch",
        std::make_unique<::etcdserverpb::WatchRequest>(),
        std::make_unique<::etcdserverpb::WatchResponse>(),
        0, common::base::eNodeType::EtcdNodeService};

    // --- Lease ---
    gRpcMethodRegistry[LeaseLeaseGrantMessageId] = RpcMethodMeta{
        "Lease", "LeaseGrant",
        std::make_unique<::etcdserverpb::LeaseGrantRequest>(),
        std::make_unique<::etcdserverpb::LeaseGrantResponse>(),
        0, common::base::eNodeType::EtcdNodeService, etcdserverpb::SendLeaseLeaseGrantErased};
    gRpcMethodRegistry[LeaseLeaseRevokeMessageId] = RpcMethodMeta{
        "Lease", "LeaseRevoke",
        std::make_unique<::etcdserverpb::LeaseRevokeRequest>(),
        std::make_unique<::etcdserverpb::LeaseRevokeResponse>(),
        0, common::base::eNodeType::EtcdNodeService, etcdserverpb::SendLeaseLeaseRevokeErased};
    gRpcMethodRegistry[LeaseLeaseKeepAliveMessageId] = RpcMethodMeta{
        "Lease", "LeaseKeepAlive",
        std::make_unique<::etcdserverpb::LeaseKeepAliveRequest>(),
        std::make_unique<::etcdserverpb::LeaseKeepAliveResponse>(),
        0, common::base::eNodeType::EtcdNodeService};
    gRpcMethodRegistry[LeaseLeaseTimeToLiveMessageId] = RpcMethodMeta{
        "Lease", "LeaseTimeToLive",
        std::make_unique<::etcdserverpb::LeaseTimeToLiveRequest>(),
        std::make_unique<::etcdserverpb::LeaseTimeToLiveResponse>(),
        0, common::base::eNodeType::EtcdNodeService, etcdserverpb::SendLeaseLeaseTimeToLiveErased};
    gRpcMethodRegistry[LeaseLeaseLeasesMessageId] = RpcMethodMeta{
        "Lease", "LeaseLeases",
        std::make_unique<::etcdserverpb::LeaseLeasesRequest>(),
        std::make_unique<::etcdserverpb::LeaseLeasesResponse>(),
        0, common::base::eNodeType::EtcdNodeService, etcdserverpb::SendLeaseLeaseLeasesErased};

    // --- ClientPlayerFriend ---
    gRpcMethodRegistry[ClientPlayerFriendAddFriendMessageId] = RpcMethodMeta{
        "ClientPlayerFriend", "AddFriend",
        std::make_unique<::friendpb::AddFriendRequest>(),
        std::make_unique<::friendpb::AddFriendResponse>(),
        1, common::base::eNodeType::FriendNodeService, friendpb::SendClientPlayerFriendAddFriendErased};
    gRpcMethodRegistry[ClientPlayerFriendAcceptFriendMessageId] = RpcMethodMeta{
        "ClientPlayerFriend", "AcceptFriend",
        std::make_unique<::friendpb::AcceptFriendRequest>(),
        std::make_unique<::friendpb::AcceptFriendResponse>(),
        1, common::base::eNodeType::FriendNodeService, friendpb::SendClientPlayerFriendAcceptFriendErased};
    gRpcMethodRegistry[ClientPlayerFriendRejectFriendMessageId] = RpcMethodMeta{
        "ClientPlayerFriend", "RejectFriend",
        std::make_unique<::friendpb::RejectFriendRequest>(),
        std::make_unique<::friendpb::RejectFriendResponse>(),
        1, common::base::eNodeType::FriendNodeService, friendpb::SendClientPlayerFriendRejectFriendErased};
    gRpcMethodRegistry[ClientPlayerFriendRemoveFriendMessageId] = RpcMethodMeta{
        "ClientPlayerFriend", "RemoveFriend",
        std::make_unique<::friendpb::RemoveFriendRequest>(),
        std::make_unique<::friendpb::RemoveFriendResponse>(),
        1, common::base::eNodeType::FriendNodeService, friendpb::SendClientPlayerFriendRemoveFriendErased};
    gRpcMethodRegistry[ClientPlayerFriendGetFriendListMessageId] = RpcMethodMeta{
        "ClientPlayerFriend", "GetFriendList",
        std::make_unique<::friendpb::GetFriendListRequest>(),
        std::make_unique<::friendpb::GetFriendListResponse>(),
        1, common::base::eNodeType::FriendNodeService, friendpb::SendClientPlayerFriendGetFriendListErased};
    gRpcMethodRegistry[ClientPlayerFriendGetPendingRequestsMessageId] = RpcMethodMeta{
        "ClientPlayerFriend", "GetPendingRequests",
        std::make_unique<::friendpb::GetPendingRequestsRequest>(),
        std::make_unique<::friendpb::GetPendingRequestsResponse>(),
        1, common::base::eNodeType::FriendNodeService, friendpb::SendClientPlayerFriendGetPendingRequestsErased};
    gRpcMethodRegistry[ClientPlayerFriendBlockMessageId] = RpcMethodMeta{
        "ClientPlayerFriend", "Block",
        std::make_unique<::friendpb::BlockRequest>(),
        std::make_unique<::friendpb::BlockResponse>(),
        1, common::base::eNodeType::FriendNodeService, friendpb::SendClientPlayerFriendBlockErased};
    gRpcMethodRegistry[ClientPlayerFriendUnblockMessageId] = RpcMethodMeta{
        "ClientPlayerFriend", "Unblock",
        std::make_unique<::friendpb::UnblockRequest>(),
        std::make_unique<::friendpb::UnblockResponse>(),
        1, common::base::eNodeType::FriendNodeService, friendpb::SendClientPlayerFriendUnblockErased};
    gRpcMethodRegistry[ClientPlayerFriendListBlocksMessageId] = RpcMethodMeta{
        "ClientPlayerFriend", "ListBlocks",
        std::make_unique<::friendpb::ListBlocksRequest>(),
        std::make_unique<::friendpb::ListBlocksResponse>(),
        1, common::base::eNodeType::FriendNodeService, friendpb::SendClientPlayerFriendListBlocksErased};
    gRpcMethodRegistry[ClientPlayerFriendRecommendFriendsMessageId] = RpcMethodMeta{
        "ClientPlayerFriend", "RecommendFriends",
        std::make_unique<::friendpb::RecommendFriendsRequest>(),
        std::make_unique<::friendpb::RecommendFriendsResponse>(),
        1, common::base::eNodeType::FriendNodeService, friendpb::SendClientPlayerFriendRecommendFriendsErased};
    gRpcMethodRegistry[ClientPlayerFriendNotifyFriendEventMessageId] = RpcMethodMeta{
        "ClientPlayerFriend", "NotifyFriendEvent",
        std::make_unique<::friendpb::FriendEventS2C>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::FriendNodeService, friendpb::SendClientPlayerFriendNotifyFriendEventErased};

    // --- GateClientSpectate ---
    gRpcMethodRegistry[GateClientSpectateSubscribeSpectateMessageId] = RpcMethodMeta{
        "GateClientSpectate", "SubscribeSpectate",
        std::make_unique<::SubscribeSpectateRequest>(),
        std::make_unique<::SubscribeSpectateResponse>(),
        0, common::base::eNodeType::GateNodeService};
    gRpcMethodRegistry[GateClientSpectateUnsubscribeSpectateMessageId] = RpcMethodMeta{
        "GateClientSpectate", "UnsubscribeSpectate",
        std::make_unique<::UnsubscribeSpectateRequest>(),
        std::make_unique<::UnsubscribeSpectateResponse>(),
        0, common::base::eNodeType::GateNodeService};

    // --- Gate ---
    gRpcMethodRegistry[GateSendMessageToPlayerMessageId] = RpcMethodMeta{
        "Gate", "SendMessageToPlayer",
        std::make_unique<::NodeRouteMessageRequest>(),
        std::make_unique<::Empty>(),
        0, common::base::eNodeType::GateNodeService};
    gRpcMethodRegistry[GateBroadcastToPlayersMessageId] = RpcMethodMeta{
        "Gate", "BroadcastToPlayers",
        std::make_unique<::BroadcastToPlayersRequest>(),
        std::make_unique<::Empty>(),
        0, common::base::eNodeType::GateNodeService};
    gRpcMethodRegistry[GateBroadcastToSceneMessageId] = RpcMethodMeta{
        "Gate", "BroadcastToScene",
        std::make_unique<::BroadcastToSceneRequest>(),
        std::make_unique<::Empty>(),
        0, common::base::eNodeType::GateNodeService};
    gRpcMethodRegistry[GateBroadcastToAllMessageId] = RpcMethodMeta{
        "Gate", "BroadcastToAll",
        std::make_unique<::BroadcastToAllRequest>(),
        std::make_unique<::Empty>(),
        0, common::base::eNodeType::GateNodeService};
    gRpcMethodRegistry[GateNodeHandshakeMessageId] = RpcMethodMeta{
        "Gate", "NodeHandshake",
        std::make_unique<::NodeHandshakeRequest>(),
        std::make_unique<::NodeHandshakeResponse>(),
        0, common::base::eNodeType::GateNodeService};
    gRpcMethodRegistry[GateGmGracefulShutdownMessageId] = RpcMethodMeta{
        "Gate", "GmGracefulShutdown",
        std::make_unique<::GmGracefulShutdownRequest>(),
        std::make_unique<::GmGracefulShutdownResponse>(),
        0, common::base::eNodeType::GateNodeService};

    // --- GuildService ---
    gRpcMethodRegistry[GuildServiceCreateGuildMessageId] = RpcMethodMeta{
        "GuildService", "CreateGuild",
        std::make_unique<::guildpb::CreateGuildRequest>(),
        std::make_unique<::guildpb::CreateGuildResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceCreateGuildErased};
    gRpcMethodRegistry[GuildServiceGetGuildMessageId] = RpcMethodMeta{
        "GuildService", "GetGuild",
        std::make_unique<::guildpb::GetGuildRequest>(),
        std::make_unique<::guildpb::GetGuildResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceGetGuildErased};
    gRpcMethodRegistry[GuildServiceGetPlayerGuildMessageId] = RpcMethodMeta{
        "GuildService", "GetPlayerGuild",
        std::make_unique<::guildpb::GetPlayerGuildRequest>(),
        std::make_unique<::guildpb::GetPlayerGuildResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceGetPlayerGuildErased};
    gRpcMethodRegistry[GuildServiceLeaveGuildMessageId] = RpcMethodMeta{
        "GuildService", "LeaveGuild",
        std::make_unique<::guildpb::LeaveGuildRequest>(),
        std::make_unique<::guildpb::LeaveGuildResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceLeaveGuildErased};
    gRpcMethodRegistry[GuildServiceDisbandGuildMessageId] = RpcMethodMeta{
        "GuildService", "DisbandGuild",
        std::make_unique<::guildpb::DisbandGuildRequest>(),
        std::make_unique<::guildpb::DisbandGuildResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceDisbandGuildErased};
    gRpcMethodRegistry[GuildServiceSetAnnouncementMessageId] = RpcMethodMeta{
        "GuildService", "SetAnnouncement",
        std::make_unique<::guildpb::SetAnnouncementRequest>(),
        std::make_unique<::guildpb::SetAnnouncementResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceSetAnnouncementErased};
    gRpcMethodRegistry[GuildServiceSetGuildMemberRoleMessageId] = RpcMethodMeta{
        "GuildService", "SetGuildMemberRole",
        std::make_unique<::guildpb::SetGuildMemberRoleRequest>(),
        std::make_unique<::guildpb::SetGuildMemberRoleResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceSetGuildMemberRoleErased};
    gRpcMethodRegistry[GuildServiceKickGuildMemberMessageId] = RpcMethodMeta{
        "GuildService", "KickGuildMember",
        std::make_unique<::guildpb::KickGuildMemberRequest>(),
        std::make_unique<::guildpb::KickGuildMemberResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceKickGuildMemberErased};
    gRpcMethodRegistry[GuildServiceTransferGuildLeaderMessageId] = RpcMethodMeta{
        "GuildService", "TransferGuildLeader",
        std::make_unique<::guildpb::TransferGuildLeaderRequest>(),
        std::make_unique<::guildpb::TransferGuildLeaderResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceTransferGuildLeaderErased};
    gRpcMethodRegistry[GuildServiceApplyJoinGuildMessageId] = RpcMethodMeta{
        "GuildService", "ApplyJoinGuild",
        std::make_unique<::guildpb::ApplyJoinGuildRequest>(),
        std::make_unique<::guildpb::ApplyJoinGuildResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceApplyJoinGuildErased};
    gRpcMethodRegistry[GuildServiceCancelGuildApplicationMessageId] = RpcMethodMeta{
        "GuildService", "CancelGuildApplication",
        std::make_unique<::guildpb::CancelGuildApplicationRequest>(),
        std::make_unique<::guildpb::CancelGuildApplicationResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceCancelGuildApplicationErased};
    gRpcMethodRegistry[GuildServiceListMyGuildApplicationsMessageId] = RpcMethodMeta{
        "GuildService", "ListMyGuildApplications",
        std::make_unique<::guildpb::ListMyGuildApplicationsRequest>(),
        std::make_unique<::guildpb::ListMyGuildApplicationsResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceListMyGuildApplicationsErased};
    gRpcMethodRegistry[GuildServiceListGuildApplicationsMessageId] = RpcMethodMeta{
        "GuildService", "ListGuildApplications",
        std::make_unique<::guildpb::ListGuildApplicationsRequest>(),
        std::make_unique<::guildpb::ListGuildApplicationsResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceListGuildApplicationsErased};
    gRpcMethodRegistry[GuildServiceReviewGuildApplicationMessageId] = RpcMethodMeta{
        "GuildService", "ReviewGuildApplication",
        std::make_unique<::guildpb::ReviewGuildApplicationRequest>(),
        std::make_unique<::guildpb::ReviewGuildApplicationResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceReviewGuildApplicationErased};
    gRpcMethodRegistry[GuildServiceNotifyGuildChangedMessageId] = RpcMethodMeta{
        "GuildService", "NotifyGuildChanged",
        std::make_unique<::guildpb::GuildChangedS2C>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceNotifyGuildChangedErased};
    gRpcMethodRegistry[GuildServiceUpdateGuildScoreMessageId] = RpcMethodMeta{
        "GuildService", "UpdateGuildScore",
        std::make_unique<::guildpb::UpdateGuildScoreRequest>(),
        std::make_unique<::guildpb::UpdateGuildScoreResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceUpdateGuildScoreErased};
    gRpcMethodRegistry[GuildServiceGetGuildRankMessageId] = RpcMethodMeta{
        "GuildService", "GetGuildRank",
        std::make_unique<::guildpb::GetGuildRankRequest>(),
        std::make_unique<::guildpb::GetGuildRankResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceGetGuildRankErased};
    gRpcMethodRegistry[GuildServiceGetGuildRankByGuildMessageId] = RpcMethodMeta{
        "GuildService", "GetGuildRankByGuild",
        std::make_unique<::guildpb::GetGuildRankByGuildRequest>(),
        std::make_unique<::guildpb::GetGuildRankByGuildResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceGetGuildRankByGuildErased};
    gRpcMethodRegistry[GuildServiceGetGuildDonateOptionsMessageId] = RpcMethodMeta{
        "GuildService", "GetGuildDonateOptions",
        std::make_unique<::guildpb::GetGuildDonateOptionsRequest>(),
        std::make_unique<::guildpb::GetGuildDonateOptionsResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceGetGuildDonateOptionsErased};
    gRpcMethodRegistry[GuildServiceDonateToGuildMessageId] = RpcMethodMeta{
        "GuildService", "DonateToGuild",
        std::make_unique<::guildpb::DonateToGuildRequest>(),
        std::make_unique<::guildpb::DonateToGuildResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceDonateToGuildErased};
    gRpcMethodRegistry[GuildServiceUpgradeGuildMessageId] = RpcMethodMeta{
        "GuildService", "UpgradeGuild",
        std::make_unique<::guildpb::UpgradeGuildRequest>(),
        std::make_unique<::guildpb::UpgradeGuildResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceUpgradeGuildErased};
    gRpcMethodRegistry[GuildServiceGetGuildShopMessageId] = RpcMethodMeta{
        "GuildService", "GetGuildShop",
        std::make_unique<::guildpb::GetGuildShopRequest>(),
        std::make_unique<::guildpb::GetGuildShopResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceGetGuildShopErased};
    gRpcMethodRegistry[GuildServiceBuyGuildShopGoodsMessageId] = RpcMethodMeta{
        "GuildService", "BuyGuildShopGoods",
        std::make_unique<::guildpb::BuyGuildShopGoodsRequest>(),
        std::make_unique<::guildpb::BuyGuildShopGoodsResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceBuyGuildShopGoodsErased};
    gRpcMethodRegistry[GuildServiceGetGuildActivitiesMessageId] = RpcMethodMeta{
        "GuildService", "GetGuildActivities",
        std::make_unique<::guildpb::GetGuildActivitiesRequest>(),
        std::make_unique<::guildpb::GetGuildActivitiesResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceGetGuildActivitiesErased};
    gRpcMethodRegistry[GuildServiceLightGuildLanternMessageId] = RpcMethodMeta{
        "GuildService", "LightGuildLantern",
        std::make_unique<::guildpb::LightGuildLanternRequest>(),
        std::make_unique<::guildpb::LightGuildLanternResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceLightGuildLanternErased};
    gRpcMethodRegistry[GuildServiceClaimGuildReunionMessageId] = RpcMethodMeta{
        "GuildService", "ClaimGuildReunion",
        std::make_unique<::guildpb::ClaimGuildReunionRequest>(),
        std::make_unique<::guildpb::ClaimGuildReunionResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceClaimGuildReunionErased};
    gRpcMethodRegistry[GuildServiceStartGuildTrialMessageId] = RpcMethodMeta{
        "GuildService", "StartGuildTrial",
        std::make_unique<::guildpb::StartGuildTrialRequest>(),
        std::make_unique<::guildpb::StartGuildTrialResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceStartGuildTrialErased};
    gRpcMethodRegistry[GuildServiceRespondGuildTrialInviteMessageId] = RpcMethodMeta{
        "GuildService", "RespondGuildTrialInvite",
        std::make_unique<::guildpb::RespondGuildTrialInviteRequest>(),
        std::make_unique<::guildpb::RespondGuildTrialInviteResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceRespondGuildTrialInviteErased};

    // --- GuildInternal ---
    gRpcMethodRegistry[GuildInternalListAppliedAssetOpsSinceMessageId] = RpcMethodMeta{
        "GuildInternal", "ListAppliedAssetOpsSince",
        std::make_unique<::guildpb::ListAppliedAssetOpsSinceRequest>(),
        std::make_unique<::guildpb::ListAppliedAssetOpsSinceResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildInternalListAppliedAssetOpsSinceErased};

    // --- ClientPlayerLogin ---
    gRpcMethodRegistry[ClientPlayerLoginLoginMessageId] = RpcMethodMeta{
        "ClientPlayerLogin", "Login",
        std::make_unique<::loginpb::LoginRequest>(),
        std::make_unique<::loginpb::LoginResponse>(),
        1, common::base::eNodeType::LoginNodeService, loginpb::SendClientPlayerLoginLoginErased};
    gRpcMethodRegistry[ClientPlayerLoginCreatePlayerMessageId] = RpcMethodMeta{
        "ClientPlayerLogin", "CreatePlayer",
        std::make_unique<::loginpb::CreatePlayerRequest>(),
        std::make_unique<::loginpb::CreatePlayerResponse>(),
        1, common::base::eNodeType::LoginNodeService, loginpb::SendClientPlayerLoginCreatePlayerErased};
    gRpcMethodRegistry[ClientPlayerLoginEnterGameMessageId] = RpcMethodMeta{
        "ClientPlayerLogin", "EnterGame",
        std::make_unique<::loginpb::EnterGameRequest>(),
        std::make_unique<::loginpb::EnterGameResponse>(),
        1, common::base::eNodeType::LoginNodeService, loginpb::SendClientPlayerLoginEnterGameErased};
    gRpcMethodRegistry[ClientPlayerLoginLeaveGameMessageId] = RpcMethodMeta{
        "ClientPlayerLogin", "LeaveGame",
        std::make_unique<::loginpb::LeaveGameRequest>(),
        std::make_unique<::loginpb::LoginEmptyResponse>(),
        1, common::base::eNodeType::LoginNodeService, loginpb::SendClientPlayerLoginLeaveGameErased};
    gRpcMethodRegistry[ClientPlayerLoginDisconnectMessageId] = RpcMethodMeta{
        "ClientPlayerLogin", "Disconnect",
        std::make_unique<::loginpb::LoginNodeDisconnectRequest>(),
        std::make_unique<::loginpb::LoginEmptyResponse>(),
        1, common::base::eNodeType::LoginNodeService, loginpb::SendClientPlayerLoginDisconnectErased};
    gRpcMethodRegistry[ClientPlayerLoginRefreshTokenMessageId] = RpcMethodMeta{
        "ClientPlayerLogin", "RefreshToken",
        std::make_unique<::loginpb::RefreshTokenRequest>(),
        std::make_unique<::loginpb::RefreshTokenResponse>(),
        1, common::base::eNodeType::LoginNodeService, loginpb::SendClientPlayerLoginRefreshTokenErased};

    // --- LoginPreGate ---
    gRpcMethodRegistry[LoginPreGateAssignGateMessageId] = RpcMethodMeta{
        "LoginPreGate", "AssignGate",
        std::make_unique<::loginpb::AssignGateRequest>(),
        std::make_unique<::loginpb::AssignGateResponse>(),
        1, common::base::eNodeType::LoginNodeService, loginpb::SendLoginPreGateAssignGateErased};
    gRpcMethodRegistry[LoginPreGateQueryQueueStatusMessageId] = RpcMethodMeta{
        "LoginPreGate", "QueryQueueStatus",
        std::make_unique<::loginpb::QueryQueueStatusRequest>(),
        std::make_unique<::loginpb::QueryQueueStatusResponse>(),
        1, common::base::eNodeType::LoginNodeService, loginpb::SendLoginPreGateQueryQueueStatusErased};

    // --- LoginAdmin ---
    gRpcMethodRegistry[LoginAdminRemovePlayersFromAccountsMessageId] = RpcMethodMeta{
        "LoginAdmin", "RemovePlayersFromAccounts",
        std::make_unique<::loginpb::RemovePlayersFromAccountsRequest>(),
        std::make_unique<::loginpb::RemovePlayersFromAccountsResponse>(),
        1, common::base::eNodeType::LoginNodeService, loginpb::SendLoginAdminRemovePlayersFromAccountsErased};

    // --- ClientPlayerMail ---
    gRpcMethodRegistry[ClientPlayerMailListMailsMessageId] = RpcMethodMeta{
        "ClientPlayerMail", "ListMails",
        std::make_unique<::mailpb::ListMailsRequest>(),
        std::make_unique<::mailpb::ListMailsResponse>(),
        1, common::base::eNodeType::MailNodeService, mailpb::SendClientPlayerMailListMailsErased};
    gRpcMethodRegistry[ClientPlayerMailReadMailMessageId] = RpcMethodMeta{
        "ClientPlayerMail", "ReadMail",
        std::make_unique<::mailpb::ReadMailRequest>(),
        std::make_unique<::mailpb::ReadMailResponse>(),
        1, common::base::eNodeType::MailNodeService, mailpb::SendClientPlayerMailReadMailErased};
    gRpcMethodRegistry[ClientPlayerMailClaimMailAttachmentsMessageId] = RpcMethodMeta{
        "ClientPlayerMail", "ClaimMailAttachments",
        std::make_unique<::mailpb::ClaimMailAttachmentsRequest>(),
        std::make_unique<::mailpb::ClaimMailAttachmentsResponse>(),
        1, common::base::eNodeType::MailNodeService, mailpb::SendClientPlayerMailClaimMailAttachmentsErased};
    gRpcMethodRegistry[ClientPlayerMailDeleteMailsMessageId] = RpcMethodMeta{
        "ClientPlayerMail", "DeleteMails",
        std::make_unique<::mailpb::DeleteMailsRequest>(),
        std::make_unique<::mailpb::DeleteMailsResponse>(),
        1, common::base::eNodeType::MailNodeService, mailpb::SendClientPlayerMailDeleteMailsErased};
    gRpcMethodRegistry[ClientPlayerMailClearReadMailsMessageId] = RpcMethodMeta{
        "ClientPlayerMail", "ClearReadMails",
        std::make_unique<::mailpb::ClearReadMailsRequest>(),
        std::make_unique<::mailpb::ClearReadMailsResponse>(),
        1, common::base::eNodeType::MailNodeService, mailpb::SendClientPlayerMailClearReadMailsErased};
    gRpcMethodRegistry[ClientPlayerMailNotifyMailChangedMessageId] = RpcMethodMeta{
        "ClientPlayerMail", "NotifyMailChanged",
        std::make_unique<::mailpb::MailChangedEvent>(),
        std::make_unique<::mailpb::MailChangedEvent>(),
        1, common::base::eNodeType::MailNodeService, mailpb::SendClientPlayerMailNotifyMailChangedErased};

    // --- MailAdmin ---
    gRpcMethodRegistry[MailAdminSendPersonalMailMessageId] = RpcMethodMeta{
        "MailAdmin", "SendPersonalMail",
        std::make_unique<::mailpb::SendMailRequest>(),
        std::make_unique<::mailpb::SendMailResponse>(),
        1, common::base::eNodeType::MailNodeService, mailpb::SendMailAdminSendPersonalMailErased};
    gRpcMethodRegistry[MailAdminSendSystemMailMessageId] = RpcMethodMeta{
        "MailAdmin", "SendSystemMail",
        std::make_unique<::mailpb::SendMailRequest>(),
        std::make_unique<::mailpb::SendMailResponse>(),
        1, common::base::eNodeType::MailNodeService, mailpb::SendMailAdminSendSystemMailErased};
    gRpcMethodRegistry[MailAdminSendGuildMailMessageId] = RpcMethodMeta{
        "MailAdmin", "SendGuildMail",
        std::make_unique<::mailpb::SendMailRequest>(),
        std::make_unique<::mailpb::SendMailResponse>(),
        1, common::base::eNodeType::MailNodeService, mailpb::SendMailAdminSendGuildMailErased};
    gRpcMethodRegistry[MailAdminImportMailRecipientsMessageId] = RpcMethodMeta{
        "MailAdmin", "ImportMailRecipients",
        std::make_unique<::mailpb::ImportMailRecipientsRequest>(),
        std::make_unique<::mailpb::ImportMailRecipientsResponse>(),
        1, common::base::eNodeType::MailNodeService, mailpb::SendMailAdminImportMailRecipientsErased};
    gRpcMethodRegistry[MailAdminSealMailCampaignMessageId] = RpcMethodMeta{
        "MailAdmin", "SealMailCampaign",
        std::make_unique<::mailpb::SealMailCampaignRequest>(),
        std::make_unique<::mailpb::SendMailResponse>(),
        1, common::base::eNodeType::MailNodeService, mailpb::SendMailAdminSealMailCampaignErased};

    // --- MatchInternal ---
    gRpcMethodRegistry[MatchInternalStartActivityBattleMessageId] = RpcMethodMeta{
        "MatchInternal", "StartActivityBattle",
        std::make_unique<::match::StartActivityBattleRequest>(),
        std::make_unique<::match::StartActivityBattleResponse>(),
        1, common::base::eNodeType::MatchNodeService, match::SendMatchInternalStartActivityBattleErased};
    gRpcMethodRegistry[MatchInternalCancelActivityBattleMessageId] = RpcMethodMeta{
        "MatchInternal", "CancelActivityBattle",
        std::make_unique<::match::CancelActivityBattleRequest>(),
        std::make_unique<::match::CancelActivityBattleResponse>(),
        1, common::base::eNodeType::MatchNodeService, match::SendMatchInternalCancelActivityBattleErased};
    gRpcMethodRegistry[MatchInternalCommitBattleOutcomeMessageId] = RpcMethodMeta{
        "MatchInternal", "CommitBattleOutcome",
        std::make_unique<::match::CommitBattleOutcomeRequest>(),
        std::make_unique<::match::CommitBattleOutcomeResponse>(),
        1, common::base::eNodeType::MatchNodeService, match::SendMatchInternalCommitBattleOutcomeErased};
    gRpcMethodRegistry[MatchInternalSetSpectateFeaturedMessageId] = RpcMethodMeta{
        "MatchInternal", "SetSpectateFeatured",
        std::make_unique<::match::SetSpectateFeaturedRequest>(),
        std::make_unique<::match::SetSpectateFeaturedResponse>(),
        1, common::base::eNodeType::MatchNodeService, match::SendMatchInternalSetSpectateFeaturedErased};

    // --- MatchService ---
    gRpcMethodRegistry[MatchServiceGetNetworkProbePlanMessageId] = RpcMethodMeta{
        "MatchService", "GetNetworkProbePlan",
        std::make_unique<::match::GetNetworkProbePlanRequest>(),
        std::make_unique<::match::GetNetworkProbePlanResponse>(),
        1, common::base::eNodeType::MatchNodeService, match::SendMatchServiceGetNetworkProbePlanErased};
    gRpcMethodRegistry[MatchServiceCommitNetworkProbeMessageId] = RpcMethodMeta{
        "MatchService", "CommitNetworkProbe",
        std::make_unique<::match::CommitNetworkProbeRequest>(),
        std::make_unique<::match::CommitNetworkProbeResponse>(),
        1, common::base::eNodeType::MatchNodeService, match::SendMatchServiceCommitNetworkProbeErased};
    gRpcMethodRegistry[MatchServiceJoinQueueMessageId] = RpcMethodMeta{
        "MatchService", "JoinQueue",
        std::make_unique<::match::JoinQueueRequest>(),
        std::make_unique<::match::JoinQueueResponse>(),
        1, common::base::eNodeType::MatchNodeService, match::SendMatchServiceJoinQueueErased};
    gRpcMethodRegistry[MatchServiceCancelQueueMessageId] = RpcMethodMeta{
        "MatchService", "CancelQueue",
        std::make_unique<::match::CancelQueueRequest>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::MatchNodeService, match::SendMatchServiceCancelQueueErased};
    gRpcMethodRegistry[MatchServiceGetQueueStatusMessageId] = RpcMethodMeta{
        "MatchService", "GetQueueStatus",
        std::make_unique<::match::GetQueueStatusRequest>(),
        std::make_unique<::match::GetQueueStatusResponse>(),
        1, common::base::eNodeType::MatchNodeService, match::SendMatchServiceGetQueueStatusErased};
    gRpcMethodRegistry[MatchServiceChallengePlayerMessageId] = RpcMethodMeta{
        "MatchService", "ChallengePlayer",
        std::make_unique<::match::ChallengePlayerRequest>(),
        std::make_unique<::match::ChallengePlayerResponse>(),
        1, common::base::eNodeType::MatchNodeService, match::SendMatchServiceChallengePlayerErased};
    gRpcMethodRegistry[MatchServiceRespondChallengeMessageId] = RpcMethodMeta{
        "MatchService", "RespondChallenge",
        std::make_unique<::match::RespondChallengeRequest>(),
        std::make_unique<::match::RespondChallengeResponse>(),
        1, common::base::eNodeType::MatchNodeService, match::SendMatchServiceRespondChallengeErased};
    gRpcMethodRegistry[MatchServiceNotifyChallengeInviteMessageId] = RpcMethodMeta{
        "MatchService", "NotifyChallengeInvite",
        std::make_unique<::match::ChallengeInviteS2C>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::MatchNodeService, match::SendMatchServiceNotifyChallengeInviteErased};
    gRpcMethodRegistry[MatchServiceNotifyChallengeResultMessageId] = RpcMethodMeta{
        "MatchService", "NotifyChallengeResult",
        std::make_unique<::match::ChallengeResultS2C>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::MatchNodeService, match::SendMatchServiceNotifyChallengeResultErased};
    gRpcMethodRegistry[MatchServiceWatchBattleMessageId] = RpcMethodMeta{
        "MatchService", "WatchBattle",
        std::make_unique<::match::WatchBattleRequest>(),
        std::make_unique<::match::WatchBattleResponse>(),
        1, common::base::eNodeType::MatchNodeService, match::SendMatchServiceWatchBattleErased};
    gRpcMethodRegistry[MatchServiceRequestBattleTicketMessageId] = RpcMethodMeta{
        "MatchService", "RequestBattleTicket",
        std::make_unique<::RequestBattleTicketRequest>(),
        std::make_unique<::RequestBattleTicketResponse>(),
        1, common::base::eNodeType::MatchNodeService, match::SendMatchServiceRequestBattleTicketErased};
    gRpcMethodRegistry[MatchServiceListWatchableBattlesMessageId] = RpcMethodMeta{
        "MatchService", "ListWatchableBattles",
        std::make_unique<::match::ListWatchableBattlesRequest>(),
        std::make_unique<::match::ListWatchableBattlesResponse>(),
        1, common::base::eNodeType::MatchNodeService, match::SendMatchServiceListWatchableBattlesErased};

    // --- PlayerInbox ---
    gRpcMethodRegistry[PlayerInboxFetchItemsMessageId] = RpcMethodMeta{
        "PlayerInbox", "FetchItems",
        std::make_unique<::player_inbox::FetchPlayerInboxRequest>(),
        std::make_unique<::player_inbox::FetchPlayerInboxResponse>(),
        1, common::base::eNodeType::PlayerInboxNodeService, player_inbox::SendPlayerInboxFetchItemsErased};
    gRpcMethodRegistry[PlayerInboxAckItemsMessageId] = RpcMethodMeta{
        "PlayerInbox", "AckItems",
        std::make_unique<::player_inbox::AckPlayerInboxRequest>(),
        std::make_unique<::player_inbox::AckPlayerInboxResponse>(),
        1, common::base::eNodeType::PlayerInboxNodeService, player_inbox::SendPlayerInboxAckItemsErased};
    gRpcMethodRegistry[PlayerInboxEnqueueBattleSettlementMessageId] = RpcMethodMeta{
        "PlayerInbox", "EnqueueBattleSettlement",
        std::make_unique<::player_inbox::BattleSettlementInboxRequest>(),
        std::make_unique<::player_inbox::BattleSettlementInboxResponse>(),
        1, common::base::eNodeType::PlayerInboxNodeService, player_inbox::SendPlayerInboxEnqueueBattleSettlementErased};
    gRpcMethodRegistry[PlayerInboxLookupBattleSettlementMessageId] = RpcMethodMeta{
        "PlayerInbox", "LookupBattleSettlement",
        std::make_unique<::player_inbox::BattleSettlementInboxRequest>(),
        std::make_unique<::player_inbox::BattleSettlementInboxResponse>(),
        1, common::base::eNodeType::PlayerInboxNodeService, player_inbox::SendPlayerInboxLookupBattleSettlementErased};

    // --- PlayerInboxAdmin ---
    gRpcMethodRegistry[PlayerInboxAdminGmSanctionPlayerMessageId] = RpcMethodMeta{
        "PlayerInboxAdmin", "GmSanctionPlayer",
        std::make_unique<::player_inbox::GmSanctionPlayerRequest>(),
        std::make_unique<::player_inbox::GmSanctionPlayerResponse>(),
        1, common::base::eNodeType::PlayerInboxNodeService, player_inbox::SendPlayerInboxAdminGmSanctionPlayerErased};
    gRpcMethodRegistry[PlayerInboxAdminGetSummaryMessageId] = RpcMethodMeta{
        "PlayerInboxAdmin", "GetSummary",
        std::make_unique<::player_inbox::GetPlayerInboxSummaryRequest>(),
        std::make_unique<::player_inbox::GetPlayerInboxSummaryResponse>(),
        1, common::base::eNodeType::PlayerInboxNodeService, player_inbox::SendPlayerInboxAdminGetSummaryErased};
    gRpcMethodRegistry[PlayerInboxAdminListItemsMessageId] = RpcMethodMeta{
        "PlayerInboxAdmin", "ListItems",
        std::make_unique<::player_inbox::ListPlayerInboxItemsRequest>(),
        std::make_unique<::player_inbox::ListPlayerInboxItemsResponse>(),
        1, common::base::eNodeType::PlayerInboxNodeService, player_inbox::SendPlayerInboxAdminListItemsErased};
    gRpcMethodRegistry[PlayerInboxAdminLookupByRequestMessageId] = RpcMethodMeta{
        "PlayerInboxAdmin", "LookupByRequest",
        std::make_unique<::player_inbox::LookupPlayerInboxRequest>(),
        std::make_unique<::player_inbox::LookupPlayerInboxResponse>(),
        1, common::base::eNodeType::PlayerInboxNodeService, player_inbox::SendPlayerInboxAdminLookupByRequestErased};
    gRpcMethodRegistry[PlayerInboxAdminVoidItemMessageId] = RpcMethodMeta{
        "PlayerInboxAdmin", "VoidItem",
        std::make_unique<::player_inbox::VoidPlayerInboxItemRequest>(),
        std::make_unique<::player_inbox::VoidPlayerInboxItemResponse>(),
        1, common::base::eNodeType::PlayerInboxNodeService, player_inbox::SendPlayerInboxAdminVoidItemErased};
    gRpcMethodRegistry[PlayerInboxAdminReconcilePlayerMessageId] = RpcMethodMeta{
        "PlayerInboxAdmin", "ReconcilePlayer",
        std::make_unique<::player_inbox::ReconcilePlayerInboxRequest>(),
        std::make_unique<::player_inbox::ReconcilePlayerInboxResponse>(),
        1, common::base::eNodeType::PlayerInboxNodeService, player_inbox::SendPlayerInboxAdminReconcilePlayerErased};

    // --- PlayerLocator ---
    gRpcMethodRegistry[PlayerLocatorSetLocationMessageId] = RpcMethodMeta{
        "PlayerLocator", "SetLocation",
        std::make_unique<::playerlocator::PlayerLocation>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::PlayerLocatorNodeService, playerlocator::SendPlayerLocatorSetLocationErased};
    gRpcMethodRegistry[PlayerLocatorGetLocationMessageId] = RpcMethodMeta{
        "PlayerLocator", "GetLocation",
        std::make_unique<::playerlocator::PlayerId>(),
        std::make_unique<::playerlocator::PlayerLocation>(),
        1, common::base::eNodeType::PlayerLocatorNodeService, playerlocator::SendPlayerLocatorGetLocationErased};
    gRpcMethodRegistry[PlayerLocatorMarkOfflineMessageId] = RpcMethodMeta{
        "PlayerLocator", "MarkOffline",
        std::make_unique<::playerlocator::MarkOfflineRequest>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::PlayerLocatorNodeService, playerlocator::SendPlayerLocatorMarkOfflineErased};
    gRpcMethodRegistry[PlayerLocatorSetSessionMessageId] = RpcMethodMeta{
        "PlayerLocator", "SetSession",
        std::make_unique<::playerlocator::SetSessionRequest>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::PlayerLocatorNodeService, playerlocator::SendPlayerLocatorSetSessionErased};
    gRpcMethodRegistry[PlayerLocatorGetSessionMessageId] = RpcMethodMeta{
        "PlayerLocator", "GetSession",
        std::make_unique<::playerlocator::GetSessionRequest>(),
        std::make_unique<::playerlocator::GetSessionResponse>(),
        1, common::base::eNodeType::PlayerLocatorNodeService, playerlocator::SendPlayerLocatorGetSessionErased};
    gRpcMethodRegistry[PlayerLocatorSetDisconnectingMessageId] = RpcMethodMeta{
        "PlayerLocator", "SetDisconnecting",
        std::make_unique<::playerlocator::SetDisconnectingRequest>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::PlayerLocatorNodeService, playerlocator::SendPlayerLocatorSetDisconnectingErased};
    gRpcMethodRegistry[PlayerLocatorReconnectMessageId] = RpcMethodMeta{
        "PlayerLocator", "Reconnect",
        std::make_unique<::playerlocator::ReconnectRequest>(),
        std::make_unique<::playerlocator::ReconnectResponse>(),
        1, common::base::eNodeType::PlayerLocatorNodeService, playerlocator::SendPlayerLocatorReconnectErased};

    // --- ClientPlayerRank ---
    gRpcMethodRegistry[ClientPlayerRankGetRankPageMessageId] = RpcMethodMeta{
        "ClientPlayerRank", "GetRankPage",
        std::make_unique<::rankpb::GetRankPageRequest>(),
        std::make_unique<::rankpb::GetRankPageResponse>(),
        1, common::base::eNodeType::RankNodeService, rankpb::SendClientPlayerRankGetRankPageErased};
    gRpcMethodRegistry[ClientPlayerRankGetRankAroundMessageId] = RpcMethodMeta{
        "ClientPlayerRank", "GetRankAround",
        std::make_unique<::rankpb::GetRankAroundRequest>(),
        std::make_unique<::rankpb::GetRankAroundResponse>(),
        1, common::base::eNodeType::RankNodeService, rankpb::SendClientPlayerRankGetRankAroundErased};

    // --- RankAdmin ---
    gRpcMethodRegistry[RankAdminOpenRankGenerationMessageId] = RpcMethodMeta{
        "RankAdmin", "OpenRankGeneration",
        std::make_unique<::rankpb::OpenRankGenerationRequest>(),
        std::make_unique<::rankpb::OpenRankGenerationResponse>(),
        1, common::base::eNodeType::RankNodeService, rankpb::SendRankAdminOpenRankGenerationErased};
    gRpcMethodRegistry[RankAdminSubmitRankScoresMessageId] = RpcMethodMeta{
        "RankAdmin", "SubmitRankScores",
        std::make_unique<::rankpb::SubmitRankScoresRequest>(),
        std::make_unique<::rankpb::SubmitRankScoresResponse>(),
        1, common::base::eNodeType::RankNodeService, rankpb::SendRankAdminSubmitRankScoresErased};
    gRpcMethodRegistry[RankAdminFreezeRankGenerationMessageId] = RpcMethodMeta{
        "RankAdmin", "FreezeRankGeneration",
        std::make_unique<::rankpb::FreezeRankGenerationRequest>(),
        std::make_unique<::rankpb::FreezeRankGenerationResponse>(),
        1, common::base::eNodeType::RankNodeService, rankpb::SendRankAdminFreezeRankGenerationErased};
    gRpcMethodRegistry[RankAdminRemoveRankEntryMessageId] = RpcMethodMeta{
        "RankAdmin", "RemoveRankEntry",
        std::make_unique<::rankpb::RemoveRankEntryRequest>(),
        std::make_unique<::rankpb::RankAdminResponse>(),
        1, common::base::eNodeType::RankNodeService, rankpb::SendRankAdminRemoveRankEntryErased};
    gRpcMethodRegistry[RankAdminDeleteRankGenerationMessageId] = RpcMethodMeta{
        "RankAdmin", "DeleteRankGeneration",
        std::make_unique<::rankpb::RemoveRankEntryRequest>(),
        std::make_unique<::rankpb::RankAdminResponse>(),
        1, common::base::eNodeType::RankNodeService, rankpb::SendRankAdminDeleteRankGenerationErased};

    // --- SceneClientPlayerCommon ---
    gRpcMethodRegistry[SceneClientPlayerCommonSendTipToClientMessageId] = RpcMethodMeta{
        "SceneClientPlayerCommon", "SendTipToClient",
        std::make_unique<::TipInfoMessage>(),
        std::make_unique<::Empty>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneClientPlayerCommonKickPlayerMessageId] = RpcMethodMeta{
        "SceneClientPlayerCommon", "KickPlayer",
        std::make_unique<::GameKickPlayerRequest>(),
        std::make_unique<::Empty>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneClientPlayerCommonRedirectToGateMessageId] = RpcMethodMeta{
        "SceneClientPlayerCommon", "RedirectToGate",
        std::make_unique<::RedirectToGateNotify>(),
        std::make_unique<::Empty>(),
        0, common::base::eNodeType::SceneNodeService};

    // --- SceneActivityClientPlayer ---
    gRpcMethodRegistry[SceneActivityClientPlayerGetActivityListMessageId] = RpcMethodMeta{
        "SceneActivityClientPlayer", "GetActivityList",
        std::make_unique<::GetActivityListRequest>(),
        std::make_unique<::GetActivityListResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneActivityClientPlayerClaimActivityVitalityRewardMessageId] = RpcMethodMeta{
        "SceneActivityClientPlayer", "ClaimActivityVitalityReward",
        std::make_unique<::ClaimActivityVitalityRewardRequest>(),
        std::make_unique<::ClaimActivityVitalityRewardResponse>(),
        0, common::base::eNodeType::SceneNodeService};

    // --- SceneAttributeClientPlayer ---
    gRpcMethodRegistry[SceneAttributeClientPlayerGetAttributePanelMessageId] = RpcMethodMeta{
        "SceneAttributeClientPlayer", "GetAttributePanel",
        std::make_unique<::GetAttributePanelRequest>(),
        std::make_unique<::GetAttributePanelResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneAttributeClientPlayerAllocateAttributePointsMessageId] = RpcMethodMeta{
        "SceneAttributeClientPlayer", "AllocateAttributePoints",
        std::make_unique<::AllocateAttributePointsRequest>(),
        std::make_unique<::AllocateAttributePointsResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneAttributeClientPlayerResetAttributePointsMessageId] = RpcMethodMeta{
        "SceneAttributeClientPlayer", "ResetAttributePoints",
        std::make_unique<::ResetAttributePointsRequest>(),
        std::make_unique<::ResetAttributePointsResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneAttributeClientPlayerAutoAllocateAttributePointsMessageId] = RpcMethodMeta{
        "SceneAttributeClientPlayer", "AutoAllocateAttributePoints",
        std::make_unique<::AutoAllocateAttributePointsRequest>(),
        std::make_unique<::AutoAllocateAttributePointsResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneAttributeClientPlayerCreateAttributeSchemeMessageId] = RpcMethodMeta{
        "SceneAttributeClientPlayer", "CreateAttributeScheme",
        std::make_unique<::CreateAttributeSchemeRequest>(),
        std::make_unique<::CreateAttributeSchemeResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneAttributeClientPlayerSwitchAttributeSchemeMessageId] = RpcMethodMeta{
        "SceneAttributeClientPlayer", "SwitchAttributeScheme",
        std::make_unique<::SwitchAttributeSchemeRequest>(),
        std::make_unique<::SwitchAttributeSchemeResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneAttributeClientPlayerRenameAttributeSchemeMessageId] = RpcMethodMeta{
        "SceneAttributeClientPlayer", "RenameAttributeScheme",
        std::make_unique<::RenameAttributeSchemeRequest>(),
        std::make_unique<::RenameAttributeSchemeResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneAttributeClientPlayerNotifyAttributePanelChangedMessageId] = RpcMethodMeta{
        "SceneAttributeClientPlayer", "NotifyAttributePanelChanged",
        std::make_unique<::AttributePanelChangedS2C>(),
        std::make_unique<::Empty>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneAttributeClientPlayerGmSetPlayerLevelMessageId] = RpcMethodMeta{
        "SceneAttributeClientPlayer", "GmSetPlayerLevel",
        std::make_unique<::GmSetPlayerLevelRequest>(),
        std::make_unique<::GmSetPlayerLevelResponse>(),
        0, common::base::eNodeType::SceneNodeService};

    // --- SceneBagClientPlayer ---
    gRpcMethodRegistry[SceneBagClientPlayerGetBagMessageId] = RpcMethodMeta{
        "SceneBagClientPlayer", "GetBag",
        std::make_unique<::GetBagRequest>(),
        std::make_unique<::GetBagResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneBagClientPlayerSortBagMessageId] = RpcMethodMeta{
        "SceneBagClientPlayer", "SortBag",
        std::make_unique<::SortBagRequest>(),
        std::make_unique<::SortBagResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneBagClientPlayerEquipItemMessageId] = RpcMethodMeta{
        "SceneBagClientPlayer", "EquipItem",
        std::make_unique<::EquipItemRequest>(),
        std::make_unique<::EquipItemResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneBagClientPlayerUnequipItemMessageId] = RpcMethodMeta{
        "SceneBagClientPlayer", "UnequipItem",
        std::make_unique<::UnequipItemRequest>(),
        std::make_unique<::UnequipItemResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneBagClientPlayerGmGrantItemMessageId] = RpcMethodMeta{
        "SceneBagClientPlayer", "GmGrantItem",
        std::make_unique<::GmGrantItemRequest>(),
        std::make_unique<::GmGrantItemResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneBagClientPlayerEnhanceEquipMessageId] = RpcMethodMeta{
        "SceneBagClientPlayer", "EnhanceEquip",
        std::make_unique<::EnhanceEquipRequest>(),
        std::make_unique<::EnhanceEquipResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneBagClientPlayerInlayGemMessageId] = RpcMethodMeta{
        "SceneBagClientPlayer", "InlayGem",
        std::make_unique<::InlayGemRequest>(),
        std::make_unique<::InlayGemResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneBagClientPlayerRemoveGemMessageId] = RpcMethodMeta{
        "SceneBagClientPlayer", "RemoveGem",
        std::make_unique<::RemoveGemRequest>(),
        std::make_unique<::RemoveGemResponse>(),
        0, common::base::eNodeType::SceneNodeService};

    // --- SceneCurrencyClientPlayer ---
    gRpcMethodRegistry[SceneCurrencyClientPlayerGmAddCurrencyMessageId] = RpcMethodMeta{
        "SceneCurrencyClientPlayer", "GmAddCurrency",
        std::make_unique<::GmAddCurrencyRequest>(),
        std::make_unique<::GmAddCurrencyResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneCurrencyClientPlayerGmDeductCurrencyMessageId] = RpcMethodMeta{
        "SceneCurrencyClientPlayer", "GmDeductCurrency",
        std::make_unique<::GmDeductCurrencyRequest>(),
        std::make_unique<::GmDeductCurrencyResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneCurrencyClientPlayerGetCurrencyListMessageId] = RpcMethodMeta{
        "SceneCurrencyClientPlayer", "GetCurrencyList",
        std::make_unique<::GetCurrencyListRequest>(),
        std::make_unique<::GetCurrencyListResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneCurrencyClientPlayerGmBlockCurrencyMessageId] = RpcMethodMeta{
        "SceneCurrencyClientPlayer", "GmBlockCurrency",
        std::make_unique<::GmBlockCurrencyRequest>(),
        std::make_unique<::GmBlockCurrencyResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneCurrencyClientPlayerGmUnblockCurrencyMessageId] = RpcMethodMeta{
        "SceneCurrencyClientPlayer", "GmUnblockCurrency",
        std::make_unique<::GmUnblockCurrencyRequest>(),
        std::make_unique<::GmUnblockCurrencyResponse>(),
        0, common::base::eNodeType::SceneNodeService};

    // --- ScenePlayer ---
    gRpcMethodRegistry[ScenePlayerGateLoginNotifyMessageId] = RpcMethodMeta{
        "ScenePlayer", "GateLoginNotify",
        std::make_unique<::GateLoginNotifyRequest>(),
        std::make_unique<::google::protobuf::Empty>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[ScenePlayerExitGameMessageId] = RpcMethodMeta{
        "ScenePlayer", "ExitGame",
        std::make_unique<::GameNodeExitGameRequest>(),
        std::make_unique<::google::protobuf::Empty>(),
        0, common::base::eNodeType::SceneNodeService};

    // --- SceneMissionClientPlayer ---
    gRpcMethodRegistry[SceneMissionClientPlayerGetMissionListMessageId] = RpcMethodMeta{
        "SceneMissionClientPlayer", "GetMissionList",
        std::make_unique<::GetMissionListRequest>(),
        std::make_unique<::GetMissionListResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneMissionClientPlayerAcceptMissionMessageId] = RpcMethodMeta{
        "SceneMissionClientPlayer", "AcceptMission",
        std::make_unique<::MissionActionRequest>(),
        std::make_unique<::GetMissionListResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneMissionClientPlayerClaimMissionRewardMessageId] = RpcMethodMeta{
        "SceneMissionClientPlayer", "ClaimMissionReward",
        std::make_unique<::MissionActionRequest>(),
        std::make_unique<::GetMissionListResponse>(),
        0, common::base::eNodeType::SceneNodeService};

    // --- SceneMovementClientPlayer ---
    gRpcMethodRegistry[SceneMovementClientPlayerMoveStartMessageId] = RpcMethodMeta{
        "SceneMovementClientPlayer", "MoveStart",
        std::make_unique<::MoveStartC2S>(),
        std::make_unique<::Empty>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneMovementClientPlayerMoveStopMessageId] = RpcMethodMeta{
        "SceneMovementClientPlayer", "MoveStop",
        std::make_unique<::MoveStopC2S>(),
        std::make_unique<::Empty>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneMovementClientPlayerMoveSyncMessageId] = RpcMethodMeta{
        "SceneMovementClientPlayer", "MoveSync",
        std::make_unique<::MoveSyncC2S>(),
        std::make_unique<::Empty>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneMovementClientPlayerTeleportRequestMessageId] = RpcMethodMeta{
        "SceneMovementClientPlayer", "TeleportRequest",
        std::make_unique<::TeleportRequestC2S>(),
        std::make_unique<::TeleportRequestC2SResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneMovementClientPlayerNotifyMoveAckMessageId] = RpcMethodMeta{
        "SceneMovementClientPlayer", "NotifyMoveAck",
        std::make_unique<::MoveAckS2C>(),
        std::make_unique<::Empty>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneMovementClientPlayerNotifyActorMoveMessageId] = RpcMethodMeta{
        "SceneMovementClientPlayer", "NotifyActorMove",
        std::make_unique<::ActorMoveS2C>(),
        std::make_unique<::Empty>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneMovementClientPlayerNotifyActorMoveListMessageId] = RpcMethodMeta{
        "SceneMovementClientPlayer", "NotifyActorMoveList",
        std::make_unique<::ActorMoveListS2C>(),
        std::make_unique<::Empty>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneMovementClientPlayerNotifyTeleportMessageId] = RpcMethodMeta{
        "SceneMovementClientPlayer", "NotifyTeleport",
        std::make_unique<::TeleportS2C>(),
        std::make_unique<::Empty>(),
        0, common::base::eNodeType::SceneNodeService};

    // --- SceneMysteryMerchantClientPlayer ---
    gRpcMethodRegistry[SceneMysteryMerchantClientPlayerGetMysteryMerchantInfoMessageId] = RpcMethodMeta{
        "SceneMysteryMerchantClientPlayer", "GetMysteryMerchantInfo",
        std::make_unique<::GetMysteryMerchantInfoRequest>(),
        std::make_unique<::GetMysteryMerchantInfoResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneMysteryMerchantClientPlayerBuyMysteryMerchantGoodsMessageId] = RpcMethodMeta{
        "SceneMysteryMerchantClientPlayer", "BuyMysteryMerchantGoods",
        std::make_unique<::BuyMysteryMerchantGoodsRequest>(),
        std::make_unique<::BuyMysteryMerchantGoodsResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneMysteryMerchantClientPlayerNotifyMysteryMerchantMessageId] = RpcMethodMeta{
        "SceneMysteryMerchantClientPlayer", "NotifyMysteryMerchant",
        std::make_unique<::MysteryMerchantNotify>(),
        std::make_unique<::Empty>(),
        0, common::base::eNodeType::SceneNodeService};

    // --- ScenePetClientPlayer ---
    gRpcMethodRegistry[ScenePetClientPlayerGetPetListMessageId] = RpcMethodMeta{
        "ScenePetClientPlayer", "GetPetList",
        std::make_unique<::GetPetListRequest>(),
        std::make_unique<::GetPetListResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[ScenePetClientPlayerSummonPetMessageId] = RpcMethodMeta{
        "ScenePetClientPlayer", "SummonPet",
        std::make_unique<::SummonPetRequest>(),
        std::make_unique<::SummonPetResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[ScenePetClientPlayerRecallPetMessageId] = RpcMethodMeta{
        "ScenePetClientPlayer", "RecallPet",
        std::make_unique<::RecallPetRequest>(),
        std::make_unique<::RecallPetResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[ScenePetClientPlayerAllocatePetPointsMessageId] = RpcMethodMeta{
        "ScenePetClientPlayer", "AllocatePetPoints",
        std::make_unique<::AllocatePetPointsRequest>(),
        std::make_unique<::AllocatePetPointsResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[ScenePetClientPlayerResetPetPointsMessageId] = RpcMethodMeta{
        "ScenePetClientPlayer", "ResetPetPoints",
        std::make_unique<::ResetPetPointsRequest>(),
        std::make_unique<::ResetPetPointsResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[ScenePetClientPlayerAutoAllocatePetPointsMessageId] = RpcMethodMeta{
        "ScenePetClientPlayer", "AutoAllocatePetPoints",
        std::make_unique<::AutoAllocatePetPointsRequest>(),
        std::make_unique<::AutoAllocatePetPointsResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[ScenePetClientPlayerRenamePetMessageId] = RpcMethodMeta{
        "ScenePetClientPlayer", "RenamePet",
        std::make_unique<::RenamePetRequest>(),
        std::make_unique<::RenamePetResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[ScenePetClientPlayerNotifyPetListChangedMessageId] = RpcMethodMeta{
        "ScenePetClientPlayer", "NotifyPetListChanged",
        std::make_unique<::PetListChangedS2C>(),
        std::make_unique<::Empty>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[ScenePetClientPlayerGmGrantPetMessageId] = RpcMethodMeta{
        "ScenePetClientPlayer", "GmGrantPet",
        std::make_unique<::GmGrantPetRequest>(),
        std::make_unique<::GmGrantPetResponse>(),
        0, common::base::eNodeType::SceneNodeService};

    // --- SceneRollbackClientPlayer ---
    gRpcMethodRegistry[SceneRollbackClientPlayerGmAttachDebtMessageId] = RpcMethodMeta{
        "SceneRollbackClientPlayer", "GmAttachDebt",
        std::make_unique<::GmAttachDebtRequest>(),
        std::make_unique<::GmAttachDebtResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneRollbackClientPlayerGmWaiveDebtMessageId] = RpcMethodMeta{
        "SceneRollbackClientPlayer", "GmWaiveDebt",
        std::make_unique<::GmWaiveDebtRequest>(),
        std::make_unique<::GmWaiveDebtResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneRollbackClientPlayerGmAdjustDebtMessageId] = RpcMethodMeta{
        "SceneRollbackClientPlayer", "GmAdjustDebt",
        std::make_unique<::GmAdjustDebtRequest>(),
        std::make_unique<::GmAdjustDebtResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneRollbackClientPlayerGmFreezeDebtMessageId] = RpcMethodMeta{
        "SceneRollbackClientPlayer", "GmFreezeDebt",
        std::make_unique<::GmFreezeDebtRequest>(),
        std::make_unique<::GmFreezeDebtResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneRollbackClientPlayerGmQueryDebtMessageId] = RpcMethodMeta{
        "SceneRollbackClientPlayer", "GmQueryDebt",
        std::make_unique<::GmQueryDebtRequest>(),
        std::make_unique<::GmQueryDebtResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneRollbackClientPlayerGmCreateSnapshotMessageId] = RpcMethodMeta{
        "SceneRollbackClientPlayer", "GmCreateSnapshot",
        std::make_unique<::GmCreateSnapshotRequest>(),
        std::make_unique<::GmCreateSnapshotResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneRollbackClientPlayerGmListSnapshotsMessageId] = RpcMethodMeta{
        "SceneRollbackClientPlayer", "GmListSnapshots",
        std::make_unique<::GmListSnapshotsRequest>(),
        std::make_unique<::GmListSnapshotsResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneRollbackClientPlayerGmPreviewRollbackMessageId] = RpcMethodMeta{
        "SceneRollbackClientPlayer", "GmPreviewRollback",
        std::make_unique<::GmPreviewRollbackRequest>(),
        std::make_unique<::GmPreviewRollbackResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneRollbackClientPlayerGmExecuteRollbackMessageId] = RpcMethodMeta{
        "SceneRollbackClientPlayer", "GmExecuteRollback",
        std::make_unique<::GmExecuteRollbackRequest>(),
        std::make_unique<::GmExecuteRollbackResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneRollbackClientPlayerGmQueryTransactionLogMessageId] = RpcMethodMeta{
        "SceneRollbackClientPlayer", "GmQueryTransactionLog",
        std::make_unique<::GmQueryTransactionLogRequest>(),
        std::make_unique<::GmQueryTransactionLogResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneRollbackClientPlayerGmTraceItemMessageId] = RpcMethodMeta{
        "SceneRollbackClientPlayer", "GmTraceItem",
        std::make_unique<::GmTraceItemRequest>(),
        std::make_unique<::GmTraceItemResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneRollbackClientPlayerGmClawbackItemMessageId] = RpcMethodMeta{
        "SceneRollbackClientPlayer", "GmClawbackItem",
        std::make_unique<::GmClawbackItemRequest>(),
        std::make_unique<::GmClawbackItemResponse>(),
        0, common::base::eNodeType::SceneNodeService};

    // --- SceneSceneClientPlayer ---
    gRpcMethodRegistry[SceneSceneClientPlayerEnterSceneMessageId] = RpcMethodMeta{
        "SceneSceneClientPlayer", "EnterScene",
        std::make_unique<::EnterSceneC2SRequest>(),
        std::make_unique<::EnterSceneC2SResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneSceneClientPlayerNotifyEnterSceneMessageId] = RpcMethodMeta{
        "SceneSceneClientPlayer", "NotifyEnterScene",
        std::make_unique<::EnterSceneS2C>(),
        std::make_unique<::Empty>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneSceneClientPlayerSceneInfoC2SMessageId] = RpcMethodMeta{
        "SceneSceneClientPlayer", "SceneInfoC2S",
        std::make_unique<::SceneInfoRequest>(),
        std::make_unique<::Empty>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneSceneClientPlayerNotifySceneInfoMessageId] = RpcMethodMeta{
        "SceneSceneClientPlayer", "NotifySceneInfo",
        std::make_unique<::SceneInfoS2C>(),
        std::make_unique<::Empty>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneSceneClientPlayerNotifyActorCreateMessageId] = RpcMethodMeta{
        "SceneSceneClientPlayer", "NotifyActorCreate",
        std::make_unique<::ActorCreateS2C>(),
        std::make_unique<::Empty>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneSceneClientPlayerNotifyActorDestroyMessageId] = RpcMethodMeta{
        "SceneSceneClientPlayer", "NotifyActorDestroy",
        std::make_unique<::ActorDestroyS2C>(),
        std::make_unique<::Empty>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneSceneClientPlayerNotifyActorListCreateMessageId] = RpcMethodMeta{
        "SceneSceneClientPlayer", "NotifyActorListCreate",
        std::make_unique<::ActorListCreateS2C>(),
        std::make_unique<::Empty>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneSceneClientPlayerNotifyActorListDestroyMessageId] = RpcMethodMeta{
        "SceneSceneClientPlayer", "NotifyActorListDestroy",
        std::make_unique<::ActorListDestroyS2C>(),
        std::make_unique<::Empty>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneSceneClientPlayerTravelToZoneMessageId] = RpcMethodMeta{
        "SceneSceneClientPlayer", "TravelToZone",
        std::make_unique<::TravelToZoneRequest>(),
        std::make_unique<::TravelToZoneResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneSceneClientPlayerGetSyncDigestMessageId] = RpcMethodMeta{
        "SceneSceneClientPlayer", "GetSyncDigest",
        std::make_unique<::GetSyncDigestRequest>(),
        std::make_unique<::Empty>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneSceneClientPlayerNotifySyncDigestMessageId] = RpcMethodMeta{
        "SceneSceneClientPlayer", "NotifySyncDigest",
        std::make_unique<::SyncDigestS2C>(),
        std::make_unique<::Empty>(),
        0, common::base::eNodeType::SceneNodeService};

    // --- SceneSkillClientPlayer ---
    gRpcMethodRegistry[SceneSkillClientPlayerReleaseSkillMessageId] = RpcMethodMeta{
        "SceneSkillClientPlayer", "ReleaseSkill",
        std::make_unique<::ReleaseSkillRequest>(),
        std::make_unique<::ReleaseSkillResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneSkillClientPlayerNotifySkillUsedMessageId] = RpcMethodMeta{
        "SceneSkillClientPlayer", "NotifySkillUsed",
        std::make_unique<::SkillUsedS2C>(),
        std::make_unique<::Empty>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneSkillClientPlayerNotifySkillInterruptedMessageId] = RpcMethodMeta{
        "SceneSkillClientPlayer", "NotifySkillInterrupted",
        std::make_unique<::SkillInterruptedS2C>(),
        std::make_unique<::Empty>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneSkillClientPlayerListSkillsMessageId] = RpcMethodMeta{
        "SceneSkillClientPlayer", "ListSkills",
        std::make_unique<::ListSkillsRequest>(),
        std::make_unique<::ListSkillsResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneSkillClientPlayerUpgradeSkillMessageId] = RpcMethodMeta{
        "SceneSkillClientPlayer", "UpgradeSkill",
        std::make_unique<::UpgradeSkillRequest>(),
        std::make_unique<::UpgradeSkillResponse>(),
        0, common::base::eNodeType::SceneNodeService};

    // --- ScenePlayerSync ---
    gRpcMethodRegistry[ScenePlayerSyncSyncBaseAttributeMessageId] = RpcMethodMeta{
        "ScenePlayerSync", "SyncBaseAttribute",
        std::make_unique<::ActorBaseAttributesS2C>(),
        std::make_unique<::Empty>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[ScenePlayerSyncSyncAttribute2FramesMessageId] = RpcMethodMeta{
        "ScenePlayerSync", "SyncAttribute2Frames",
        std::make_unique<::AttributeDelta2FramesS2C>(),
        std::make_unique<::Empty>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[ScenePlayerSyncSyncAttribute5FramesMessageId] = RpcMethodMeta{
        "ScenePlayerSync", "SyncAttribute5Frames",
        std::make_unique<::AttributeDelta5FramesS2C>(),
        std::make_unique<::Empty>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[ScenePlayerSyncSyncAttribute10FramesMessageId] = RpcMethodMeta{
        "ScenePlayerSync", "SyncAttribute10Frames",
        std::make_unique<::AttributeDelta10FramesS2C>(),
        std::make_unique<::Empty>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[ScenePlayerSyncSyncAttribute30FramesMessageId] = RpcMethodMeta{
        "ScenePlayerSync", "SyncAttribute30Frames",
        std::make_unique<::AttributeDelta30FramesS2C>(),
        std::make_unique<::Empty>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[ScenePlayerSyncSyncAttribute60FramesMessageId] = RpcMethodMeta{
        "ScenePlayerSync", "SyncAttribute60Frames",
        std::make_unique<::AttributeDelta60FramesS2C>(),
        std::make_unique<::Empty>(),
        0, common::base::eNodeType::SceneNodeService};

    // --- SceneScenePlayer ---
    gRpcMethodRegistry[SceneScenePlayerEnterSceneMessageId] = RpcMethodMeta{
        "SceneScenePlayer", "EnterScene",
        std::make_unique<::GsEnterSceneRequest>(),
        std::make_unique<::google::protobuf::Empty>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneScenePlayerLeaveSceneMessageId] = RpcMethodMeta{
        "SceneScenePlayer", "LeaveScene",
        std::make_unique<::GsLeaveSceneRequest>(),
        std::make_unique<::google::protobuf::Empty>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneScenePlayerEnterSceneS2CMessageId] = RpcMethodMeta{
        "SceneScenePlayer", "EnterSceneS2C",
        std::make_unique<::EnterSceneS2CRequest>(),
        std::make_unique<::EnterSceneS2CResponse>(),
        0, common::base::eNodeType::SceneNodeService};

    // --- Scene ---
    gRpcMethodRegistry[ScenePlayerEnterGameNodeMessageId] = RpcMethodMeta{
        "Scene", "PlayerEnterGameNode",
        std::make_unique<::PlayerEnterGameNodeRequest>(),
        std::make_unique<::Empty>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneProcessClientPlayerMessageMessageId] = RpcMethodMeta{
        "Scene", "ProcessClientPlayerMessage",
        std::make_unique<::ProcessClientPlayerMessageRequest>(),
        std::make_unique<::ProcessClientPlayerMessageResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneUpdateSessionDetailMessageId] = RpcMethodMeta{
        "Scene", "UpdateSessionDetail",
        std::make_unique<::RegisterPlayerSessionRequest>(),
        std::make_unique<::Empty>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneCreateSceneMessageId] = RpcMethodMeta{
        "Scene", "CreateScene",
        std::make_unique<::CreateSceneRequest>(),
        std::make_unique<::CreateSceneResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneDestroySceneMessageId] = RpcMethodMeta{
        "Scene", "DestroyScene",
        std::make_unique<::DestroySceneRequest>(),
        std::make_unique<::Empty>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[ScenePrepareBattleMessageId] = RpcMethodMeta{
        "Scene", "PrepareBattle",
        std::make_unique<::PrepareBattleRequest>(),
        std::make_unique<::PrepareBattleResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneCancelBattlePrepareMessageId] = RpcMethodMeta{
        "Scene", "CancelBattlePrepare",
        std::make_unique<::CancelBattlePrepareRequest>(),
        std::make_unique<::Empty>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneNodeHandshakeMessageId] = RpcMethodMeta{
        "Scene", "NodeHandshake",
        std::make_unique<::NodeHandshakeRequest>(),
        std::make_unique<::NodeHandshakeResponse>(),
        0, common::base::eNodeType::SceneNodeService};

    // --- SceneScene ---
    gRpcMethodRegistry[SceneSceneTestMessageId] = RpcMethodMeta{
        "SceneScene", "Test",
        std::make_unique<::GameSceneTest>(),
        std::make_unique<::Empty>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneSceneGmGracefulShutdownMessageId] = RpcMethodMeta{
        "SceneScene", "GmGracefulShutdown",
        std::make_unique<::GmGracefulShutdownRequest>(),
        std::make_unique<::GmGracefulShutdownResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneSceneGmDescribeEntityMessageId] = RpcMethodMeta{
        "SceneScene", "GmDescribeEntity",
        std::make_unique<::GmDescribeEntityRequest>(),
        std::make_unique<::GmDescribeEntityResponse>(),
        0, common::base::eNodeType::SceneNodeService};

    // --- SceneManager ---
    gRpcMethodRegistry[SceneManagerCreateSceneMessageId] = RpcMethodMeta{
        "SceneManager", "CreateScene",
        std::make_unique<::scene_manager::CreateSceneRequest>(),
        std::make_unique<::scene_manager::CreateSceneResponse>(),
        1, common::base::eNodeType::SceneManagerNodeService, scene_manager::SendSceneManagerCreateSceneErased};
    gRpcMethodRegistry[SceneManagerDestroySceneMessageId] = RpcMethodMeta{
        "SceneManager", "DestroyScene",
        std::make_unique<::scene_manager::DestroySceneRequest>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::SceneManagerNodeService, scene_manager::SendSceneManagerDestroySceneErased};
    gRpcMethodRegistry[SceneManagerEnterSceneMessageId] = RpcMethodMeta{
        "SceneManager", "EnterScene",
        std::make_unique<::scene_manager::EnterSceneRequest>(),
        std::make_unique<::scene_manager::EnterSceneResponse>(),
        1, common::base::eNodeType::SceneManagerNodeService, scene_manager::SendSceneManagerEnterSceneErased};
    gRpcMethodRegistry[SceneManagerLeaveSceneMessageId] = RpcMethodMeta{
        "SceneManager", "LeaveScene",
        std::make_unique<::scene_manager::LeaveSceneRequest>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::SceneManagerNodeService, scene_manager::SendSceneManagerLeaveSceneErased};

    // --- SceneNodeGrpc ---
    gRpcMethodRegistry[SceneNodeGrpcCreateSceneMessageId] = RpcMethodMeta{
        "SceneNodeGrpc", "CreateScene",
        std::make_unique<::CreateSceneRequest>(),
        std::make_unique<::CreateSceneResponse>(),
        1, common::base::eNodeType::SceneManagerNodeService, scene_node::SendSceneNodeGrpcCreateSceneErased};
    gRpcMethodRegistry[SceneNodeGrpcDestroySceneMessageId] = RpcMethodMeta{
        "SceneNodeGrpc", "DestroyScene",
        std::make_unique<::DestroySceneRequest>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::SceneManagerNodeService, scene_node::SendSceneNodeGrpcDestroySceneErased};
    gRpcMethodRegistry[SceneNodeGrpcReleasePlayerMessageId] = RpcMethodMeta{
        "SceneNodeGrpc", "ReleasePlayer",
        std::make_unique<::scene_node::ReleasePlayerRequest>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::SceneManagerNodeService, scene_node::SendSceneNodeGrpcReleasePlayerErased};
    gRpcMethodRegistry[SceneNodeGrpcPrepareBattleMessageId] = RpcMethodMeta{
        "SceneNodeGrpc", "PrepareBattle",
        std::make_unique<::PrepareBattleRequest>(),
        std::make_unique<::PrepareBattleResponse>(),
        1, common::base::eNodeType::SceneManagerNodeService, scene_node::SendSceneNodeGrpcPrepareBattleErased};
    gRpcMethodRegistry[SceneNodeGrpcCancelBattlePrepareMessageId] = RpcMethodMeta{
        "SceneNodeGrpc", "CancelBattlePrepare",
        std::make_unique<::CancelBattlePrepareRequest>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::SceneManagerNodeService, scene_node::SendSceneNodeGrpcCancelBattlePrepareErased};
    gRpcMethodRegistry[SceneNodeGrpcSealAccountRoleTradeMessageId] = RpcMethodMeta{
        "SceneNodeGrpc", "SealAccountRoleTrade",
        std::make_unique<::SealAccountRoleTradeRequest>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::SceneManagerNodeService, scene_node::SendSceneNodeGrpcSealAccountRoleTradeErased};
    gRpcMethodRegistry[SceneNodeGrpcAssetDebitMessageId] = RpcMethodMeta{
        "SceneNodeGrpc", "AssetDebit",
        std::make_unique<::AssetOpRequest>(),
        std::make_unique<::AssetOpResponse>(),
        1, common::base::eNodeType::SceneManagerNodeService, scene_node::SendSceneNodeGrpcAssetDebitErased};
    gRpcMethodRegistry[SceneNodeGrpcAssetAbortDebitMessageId] = RpcMethodMeta{
        "SceneNodeGrpc", "AssetAbortDebit",
        std::make_unique<::AssetOpRequest>(),
        std::make_unique<::AssetOpResponse>(),
        1, common::base::eNodeType::SceneManagerNodeService, scene_node::SendSceneNodeGrpcAssetAbortDebitErased};
    gRpcMethodRegistry[SceneNodeGrpcAssetCreditMessageId] = RpcMethodMeta{
        "SceneNodeGrpc", "AssetCredit",
        std::make_unique<::AssetOpRequest>(),
        std::make_unique<::AssetOpResponse>(),
        1, common::base::eNodeType::SceneManagerNodeService, scene_node::SendSceneNodeGrpcAssetCreditErased};
    gRpcMethodRegistry[SceneNodeGrpcAssetAckReceiptMessageId] = RpcMethodMeta{
        "SceneNodeGrpc", "AssetAckReceipt",
        std::make_unique<::AssetOpRequest>(),
        std::make_unique<::AssetOpResponse>(),
        1, common::base::eNodeType::SceneManagerNodeService, scene_node::SendSceneNodeGrpcAssetAckReceiptErased};

    // --- ClientPlayerTeam ---
    gRpcMethodRegistry[ClientPlayerTeamCreateTeamMessageId] = RpcMethodMeta{
        "ClientPlayerTeam", "CreateTeam",
        std::make_unique<::teampb::CreateTeamRequest>(),
        std::make_unique<::teampb::TeamResponse>(),
        1, common::base::eNodeType::TeamNodeService, teampb::SendClientPlayerTeamCreateTeamErased};
    gRpcMethodRegistry[ClientPlayerTeamGetMyTeamMessageId] = RpcMethodMeta{
        "ClientPlayerTeam", "GetMyTeam",
        std::make_unique<::teampb::GetMyTeamRequest>(),
        std::make_unique<::teampb::TeamResponse>(),
        1, common::base::eNodeType::TeamNodeService, teampb::SendClientPlayerTeamGetMyTeamErased};
    gRpcMethodRegistry[ClientPlayerTeamApplyJoinTeamMessageId] = RpcMethodMeta{
        "ClientPlayerTeam", "ApplyJoinTeam",
        std::make_unique<::teampb::ApplyJoinTeamRequest>(),
        std::make_unique<::teampb::TeamResponse>(),
        1, common::base::eNodeType::TeamNodeService, teampb::SendClientPlayerTeamApplyJoinTeamErased};
    gRpcMethodRegistry[ClientPlayerTeamHandleApplicationMessageId] = RpcMethodMeta{
        "ClientPlayerTeam", "HandleApplication",
        std::make_unique<::teampb::HandleApplicationRequest>(),
        std::make_unique<::teampb::TeamResponse>(),
        1, common::base::eNodeType::TeamNodeService, teampb::SendClientPlayerTeamHandleApplicationErased};
    gRpcMethodRegistry[ClientPlayerTeamInviteToTeamMessageId] = RpcMethodMeta{
        "ClientPlayerTeam", "InviteToTeam",
        std::make_unique<::teampb::InviteToTeamRequest>(),
        std::make_unique<::teampb::TeamResponse>(),
        1, common::base::eNodeType::TeamNodeService, teampb::SendClientPlayerTeamInviteToTeamErased};
    gRpcMethodRegistry[ClientPlayerTeamRespondInviteMessageId] = RpcMethodMeta{
        "ClientPlayerTeam", "RespondInvite",
        std::make_unique<::teampb::RespondInviteRequest>(),
        std::make_unique<::teampb::TeamResponse>(),
        1, common::base::eNodeType::TeamNodeService, teampb::SendClientPlayerTeamRespondInviteErased};
    gRpcMethodRegistry[ClientPlayerTeamListMyInvitesMessageId] = RpcMethodMeta{
        "ClientPlayerTeam", "ListMyInvites",
        std::make_unique<::teampb::ListMyInvitesRequest>(),
        std::make_unique<::teampb::ListMyInvitesResponse>(),
        1, common::base::eNodeType::TeamNodeService, teampb::SendClientPlayerTeamListMyInvitesErased};
    gRpcMethodRegistry[ClientPlayerTeamLeaveTeamMessageId] = RpcMethodMeta{
        "ClientPlayerTeam", "LeaveTeam",
        std::make_unique<::teampb::LeaveTeamRequest>(),
        std::make_unique<::teampb::TeamResponse>(),
        1, common::base::eNodeType::TeamNodeService, teampb::SendClientPlayerTeamLeaveTeamErased};
    gRpcMethodRegistry[ClientPlayerTeamKickMemberMessageId] = RpcMethodMeta{
        "ClientPlayerTeam", "KickMember",
        std::make_unique<::teampb::KickMemberRequest>(),
        std::make_unique<::teampb::TeamResponse>(),
        1, common::base::eNodeType::TeamNodeService, teampb::SendClientPlayerTeamKickMemberErased};
    gRpcMethodRegistry[ClientPlayerTeamTransferLeaderMessageId] = RpcMethodMeta{
        "ClientPlayerTeam", "TransferLeader",
        std::make_unique<::teampb::TransferLeaderRequest>(),
        std::make_unique<::teampb::TeamResponse>(),
        1, common::base::eNodeType::TeamNodeService, teampb::SendClientPlayerTeamTransferLeaderErased};
    gRpcMethodRegistry[ClientPlayerTeamDisbandTeamMessageId] = RpcMethodMeta{
        "ClientPlayerTeam", "DisbandTeam",
        std::make_unique<::teampb::DisbandTeamRequest>(),
        std::make_unique<::teampb::TeamResponse>(),
        1, common::base::eNodeType::TeamNodeService, teampb::SendClientPlayerTeamDisbandTeamErased};
    gRpcMethodRegistry[ClientPlayerTeamStartTeamMatchMessageId] = RpcMethodMeta{
        "ClientPlayerTeam", "StartTeamMatch",
        std::make_unique<::teampb::StartTeamMatchRequest>(),
        std::make_unique<::teampb::TeamResponse>(),
        1, common::base::eNodeType::TeamNodeService, teampb::SendClientPlayerTeamStartTeamMatchErased};
    gRpcMethodRegistry[ClientPlayerTeamNotifyTeamSnapshotMessageId] = RpcMethodMeta{
        "ClientPlayerTeam", "NotifyTeamSnapshot",
        std::make_unique<::teampb::TeamSnapshotS2C>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::TeamNodeService, teampb::SendClientPlayerTeamNotifyTeamSnapshotErased};
    gRpcMethodRegistry[ClientPlayerTeamNotifyTeamInviteMessageId] = RpcMethodMeta{
        "ClientPlayerTeam", "NotifyTeamInvite",
        std::make_unique<::teampb::TeamInviteS2C>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::TeamNodeService, teampb::SendClientPlayerTeamNotifyTeamInviteErased};
    gRpcMethodRegistry[ClientPlayerTeamNotifyTeamEventMessageId] = RpcMethodMeta{
        "ClientPlayerTeam", "NotifyTeamEvent",
        std::make_unique<::teampb::TeamEventS2C>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::TeamNodeService, teampb::SendClientPlayerTeamNotifyTeamEventErased};

    // --- TeamAuthority ---
    gRpcMethodRegistry[TeamAuthorityReadMembershipMessageId] = RpcMethodMeta{
        "TeamAuthority", "ReadMembership",
        std::make_unique<::teampb::ReadTeamMembershipRequest>(),
        std::make_unique<::teampb::ReadTeamMembershipResponse>(),
        1, common::base::eNodeType::TeamNodeService, teampb::SendTeamAuthorityReadMembershipErased};

    // --- ClientPlayerJubaozhai ---
    gRpcMethodRegistry[ClientPlayerJubaozhaiBrowseListingsMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "BrowseListings",
        std::make_unique<::trade::BrowseListingsRequest>(),
        std::make_unique<::trade::BrowseListingsResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiBrowseListingsErased};
    gRpcMethodRegistry[ClientPlayerJubaozhaiGetListingDetailMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "GetListingDetail",
        std::make_unique<::trade::GetListingDetailRequest>(),
        std::make_unique<::trade::GetListingDetailResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiGetListingDetailErased};
    gRpcMethodRegistry[ClientPlayerJubaozhaiSetFavoriteMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "SetFavorite",
        std::make_unique<::trade::SetFavoriteRequest>(),
        std::make_unique<::trade::SetFavoriteResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiSetFavoriteErased};
    gRpcMethodRegistry[ClientPlayerJubaozhaiGetMyShelfMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "GetMyShelf",
        std::make_unique<::trade::GetMyShelfRequest>(),
        std::make_unique<::trade::GetMyShelfResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiGetMyShelfErased};
    gRpcMethodRegistry[ClientPlayerJubaozhaiListOfficialSkusMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "ListOfficialSkus",
        std::make_unique<::trade::ListOfficialSkusRequest>(),
        std::make_unique<::trade::ListOfficialSkusResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiListOfficialSkusErased};
    gRpcMethodRegistry[ClientPlayerJubaozhaiCreateOfficialOrderMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "CreateOfficialOrder",
        std::make_unique<::trade::CreateOfficialOrderRequest>(),
        std::make_unique<::trade::CommerceOrderResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiCreateOfficialOrderErased};
    gRpcMethodRegistry[ClientPlayerJubaozhaiGetCommerceOrderMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "GetCommerceOrder",
        std::make_unique<::trade::GetCommerceOrderRequest>(),
        std::make_unique<::trade::CommerceOrderResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiGetCommerceOrderErased};
    gRpcMethodRegistry[ClientPlayerJubaozhaiCancelCommerceOrderMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "CancelCommerceOrder",
        std::make_unique<::trade::CancelCommerceOrderRequest>(),
        std::make_unique<::trade::CommerceOrderResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiCancelCommerceOrderErased};
    gRpcMethodRegistry[ClientPlayerJubaozhaiListCommerceOrdersMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "ListCommerceOrders",
        std::make_unique<::trade::ListCommerceOrdersRequest>(),
        std::make_unique<::trade::ListCommerceOrdersResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiListCommerceOrdersErased};
    gRpcMethodRegistry[ClientPlayerJubaozhaiCreatePlayerTradeMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "CreatePlayerTrade",
        std::make_unique<::trade::CreatePlayerTradeRequest>(),
        std::make_unique<::trade::CommerceOrderResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiCreatePlayerTradeErased};
    gRpcMethodRegistry[ClientPlayerJubaozhaiConfirmPlayerTradeMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "ConfirmPlayerTrade",
        std::make_unique<::trade::ConfirmPlayerTradeRequest>(),
        std::make_unique<::trade::CommerceOrderResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiConfirmPlayerTradeErased};
    gRpcMethodRegistry[ClientPlayerJubaozhaiCreateConsignmentMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "CreateConsignment",
        std::make_unique<::trade::CreateConsignmentRequest>(),
        std::make_unique<::trade::CommerceOrderResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiCreateConsignmentErased};
    gRpcMethodRegistry[ClientPlayerJubaozhaiBuyConsignmentMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "BuyConsignment",
        std::make_unique<::trade::BuyConsignmentRequest>(),
        std::make_unique<::trade::CommerceOrderResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiBuyConsignmentErased};
    gRpcMethodRegistry[ClientPlayerJubaozhaiCancelConsignmentMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "CancelConsignment",
        std::make_unique<::trade::CancelConsignmentRequest>(),
        std::make_unique<::trade::CommerceOrderResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiCancelConsignmentErased};
    gRpcMethodRegistry[ClientPlayerJubaozhaiPlaceMarketOrderMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "PlaceMarketOrder",
        std::make_unique<::trade::PlaceMarketOrderRequest>(),
        std::make_unique<::trade::MarketOrderResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiPlaceMarketOrderErased};
    gRpcMethodRegistry[ClientPlayerJubaozhaiCancelMarketOrderMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "CancelMarketOrder",
        std::make_unique<::trade::CancelMarketOrderRequest>(),
        std::make_unique<::trade::MarketOrderResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiCancelMarketOrderErased};
    gRpcMethodRegistry[ClientPlayerJubaozhaiListMarketOrdersMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "ListMarketOrders",
        std::make_unique<::trade::ListMarketOrdersRequest>(),
        std::make_unique<::trade::ListMarketOrdersResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiListMarketOrdersErased};
    gRpcMethodRegistry[ClientPlayerJubaozhaiGetMarketDepthMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "GetMarketDepth",
        std::make_unique<::trade::GetMarketDepthRequest>(),
        std::make_unique<::trade::GetMarketDepthResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiGetMarketDepthErased};
    gRpcMethodRegistry[ClientPlayerJubaozhaiCreateAuctionLotMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "CreateAuctionLot",
        std::make_unique<::trade::CreateAuctionLotRequest>(),
        std::make_unique<::trade::AuctionLotResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiCreateAuctionLotErased};
    gRpcMethodRegistry[ClientPlayerJubaozhaiPlaceAuctionBidMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "PlaceAuctionBid",
        std::make_unique<::trade::PlaceAuctionBidRequest>(),
        std::make_unique<::trade::AuctionLotResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiPlaceAuctionBidErased};
    gRpcMethodRegistry[ClientPlayerJubaozhaiGetAuctionLotMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "GetAuctionLot",
        std::make_unique<::trade::GetAuctionLotRequest>(),
        std::make_unique<::trade::AuctionLotResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiGetAuctionLotErased};
    gRpcMethodRegistry[ClientPlayerJubaozhaiCancelAuctionLotMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "CancelAuctionLot",
        std::make_unique<::trade::CancelAuctionLotRequest>(),
        std::make_unique<::trade::AuctionLotResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiCancelAuctionLotErased};
    gRpcMethodRegistry[ClientPlayerJubaozhaiBrowseCommerceOffersMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "BrowseCommerceOffers",
        std::make_unique<::trade::BrowseCommerceOffersRequest>(),
        std::make_unique<::trade::BrowseCommerceOffersResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiBrowseCommerceOffersErased};
    gRpcMethodRegistry[ClientPlayerJubaozhaiListTradeMarketsMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "ListTradeMarkets",
        std::make_unique<::trade::ListTradeMarketsRequest>(),
        std::make_unique<::trade::ListTradeMarketsResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiListTradeMarketsErased};
    gRpcMethodRegistry[ClientPlayerJubaozhaiCreateRmbConsignmentMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "CreateRmbConsignment",
        std::make_unique<::trade::CreateRmbConsignmentRequest>(),
        std::make_unique<::trade::RmbConsignmentResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiCreateRmbConsignmentErased};
    gRpcMethodRegistry[ClientPlayerJubaozhaiGetRmbConsignmentMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "GetRmbConsignment",
        std::make_unique<::trade::GetRmbConsignmentRequest>(),
        std::make_unique<::trade::RmbConsignmentResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiGetRmbConsignmentErased};
    gRpcMethodRegistry[ClientPlayerJubaozhaiListRmbConsignmentsMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "ListRmbConsignments",
        std::make_unique<::trade::ListRmbConsignmentsRequest>(),
        std::make_unique<::trade::ListRmbConsignmentsResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiListRmbConsignmentsErased};
    gRpcMethodRegistry[ClientPlayerJubaozhaiBuyRmbConsignmentMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "BuyRmbConsignment",
        std::make_unique<::trade::BuyRmbConsignmentRequest>(),
        std::make_unique<::trade::CommerceOrderResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiBuyRmbConsignmentErased};
    gRpcMethodRegistry[ClientPlayerJubaozhaiCancelRmbConsignmentMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "CancelRmbConsignment",
        std::make_unique<::trade::CancelRmbConsignmentRequest>(),
        std::make_unique<::trade::RmbConsignmentResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiCancelRmbConsignmentErased};
    gRpcMethodRegistry[ClientPlayerJubaozhaiGetRmbOrderMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "GetRmbOrder",
        std::make_unique<::trade::GetCommerceOrderRequest>(),
        std::make_unique<::trade::CommerceOrderResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiGetRmbOrderErased};
    gRpcMethodRegistry[ClientPlayerJubaozhaiListRmbOrdersMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "ListRmbOrders",
        std::make_unique<::trade::ListCommerceOrdersRequest>(),
        std::make_unique<::trade::ListCommerceOrdersResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiListRmbOrdersErased};
    gRpcMethodRegistry[ClientPlayerJubaozhaiCancelRmbOrderMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "CancelRmbOrder",
        std::make_unique<::trade::CancelCommerceOrderRequest>(),
        std::make_unique<::trade::CommerceOrderResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiCancelRmbOrderErased};
    gRpcMethodRegistry[ClientPlayerJubaozhaiListSellerIncomesMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "ListSellerIncomes",
        std::make_unique<::trade::ListSellerIncomesRequest>(),
        std::make_unique<::trade::ListSellerIncomesResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiListSellerIncomesErased};
    gRpcMethodRegistry[ClientPlayerJubaozhaiCreateWithdrawalMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "CreateWithdrawal",
        std::make_unique<::trade::CreateWithdrawalRequest>(),
        std::make_unique<::trade::WithdrawalResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiCreateWithdrawalErased};
    gRpcMethodRegistry[ClientPlayerJubaozhaiGetWithdrawalMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "GetWithdrawal",
        std::make_unique<::trade::GetWithdrawalRequest>(),
        std::make_unique<::trade::WithdrawalResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiGetWithdrawalErased};

    // --- TradeAdmin ---
    gRpcMethodRegistry[TradeAdminSeedListingMessageId] = RpcMethodMeta{
        "TradeAdmin", "SeedListing",
        std::make_unique<::trade::SeedListingRequest>(),
        std::make_unique<::trade::SeedListingResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendTradeAdminSeedListingErased};
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
	case ClientPlayerFriendAddFriendMessageId:
	case ClientPlayerFriendAcceptFriendMessageId:
	case ClientPlayerFriendRejectFriendMessageId:
	case ClientPlayerFriendRemoveFriendMessageId:
	case ClientPlayerFriendGetFriendListMessageId:
	case ClientPlayerFriendGetPendingRequestsMessageId:
	case ClientPlayerFriendBlockMessageId:
	case ClientPlayerFriendUnblockMessageId:
	case ClientPlayerFriendListBlocksMessageId:
	case ClientPlayerFriendRecommendFriendsMessageId:
	case ClientPlayerFriendNotifyFriendEventMessageId:
	case GateClientSpectateSubscribeSpectateMessageId:
	case GateClientSpectateUnsubscribeSpectateMessageId:
	case GuildServiceCreateGuildMessageId:
	case GuildServiceGetGuildMessageId:
	case GuildServiceGetPlayerGuildMessageId:
	case GuildServiceLeaveGuildMessageId:
	case GuildServiceDisbandGuildMessageId:
	case GuildServiceSetAnnouncementMessageId:
	case GuildServiceSetGuildMemberRoleMessageId:
	case GuildServiceKickGuildMemberMessageId:
	case GuildServiceTransferGuildLeaderMessageId:
	case GuildServiceApplyJoinGuildMessageId:
	case GuildServiceCancelGuildApplicationMessageId:
	case GuildServiceListMyGuildApplicationsMessageId:
	case GuildServiceListGuildApplicationsMessageId:
	case GuildServiceReviewGuildApplicationMessageId:
	case GuildServiceNotifyGuildChangedMessageId:
	case GuildServiceUpdateGuildScoreMessageId:
	case GuildServiceGetGuildRankMessageId:
	case GuildServiceGetGuildRankByGuildMessageId:
	case GuildServiceGetGuildDonateOptionsMessageId:
	case GuildServiceDonateToGuildMessageId:
	case GuildServiceUpgradeGuildMessageId:
	case GuildServiceGetGuildShopMessageId:
	case GuildServiceBuyGuildShopGoodsMessageId:
	case GuildServiceGetGuildActivitiesMessageId:
	case GuildServiceLightGuildLanternMessageId:
	case GuildServiceClaimGuildReunionMessageId:
	case GuildServiceStartGuildTrialMessageId:
	case GuildServiceRespondGuildTrialInviteMessageId:
	case ClientPlayerLoginLoginMessageId:
	case ClientPlayerLoginCreatePlayerMessageId:
	case ClientPlayerLoginEnterGameMessageId:
	case ClientPlayerLoginLeaveGameMessageId:
	case ClientPlayerLoginDisconnectMessageId:
	case ClientPlayerLoginRefreshTokenMessageId:
	case ClientPlayerMailListMailsMessageId:
	case ClientPlayerMailReadMailMessageId:
	case ClientPlayerMailClaimMailAttachmentsMessageId:
	case ClientPlayerMailDeleteMailsMessageId:
	case ClientPlayerMailClearReadMailsMessageId:
	case ClientPlayerMailNotifyMailChangedMessageId:
	case MatchServiceGetNetworkProbePlanMessageId:
	case MatchServiceCommitNetworkProbeMessageId:
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
	case ClientPlayerRankGetRankPageMessageId:
	case ClientPlayerRankGetRankAroundMessageId:
	case SceneClientPlayerCommonSendTipToClientMessageId:
	case SceneClientPlayerCommonKickPlayerMessageId:
	case SceneClientPlayerCommonRedirectToGateMessageId:
	case SceneActivityClientPlayerGetActivityListMessageId:
	case SceneActivityClientPlayerClaimActivityVitalityRewardMessageId:
	case SceneAttributeClientPlayerGetAttributePanelMessageId:
	case SceneAttributeClientPlayerAllocateAttributePointsMessageId:
	case SceneAttributeClientPlayerResetAttributePointsMessageId:
	case SceneAttributeClientPlayerAutoAllocateAttributePointsMessageId:
	case SceneAttributeClientPlayerCreateAttributeSchemeMessageId:
	case SceneAttributeClientPlayerSwitchAttributeSchemeMessageId:
	case SceneAttributeClientPlayerRenameAttributeSchemeMessageId:
	case SceneAttributeClientPlayerNotifyAttributePanelChangedMessageId:
	case SceneAttributeClientPlayerGmSetPlayerLevelMessageId:
	case SceneBagClientPlayerGetBagMessageId:
	case SceneBagClientPlayerSortBagMessageId:
	case SceneBagClientPlayerEquipItemMessageId:
	case SceneBagClientPlayerUnequipItemMessageId:
	case SceneBagClientPlayerGmGrantItemMessageId:
	case SceneBagClientPlayerEnhanceEquipMessageId:
	case SceneBagClientPlayerInlayGemMessageId:
	case SceneBagClientPlayerRemoveGemMessageId:
	case SceneCurrencyClientPlayerGmAddCurrencyMessageId:
	case SceneCurrencyClientPlayerGmDeductCurrencyMessageId:
	case SceneCurrencyClientPlayerGetCurrencyListMessageId:
	case SceneCurrencyClientPlayerGmBlockCurrencyMessageId:
	case SceneCurrencyClientPlayerGmUnblockCurrencyMessageId:
	case SceneMissionClientPlayerGetMissionListMessageId:
	case SceneMissionClientPlayerAcceptMissionMessageId:
	case SceneMissionClientPlayerClaimMissionRewardMessageId:
	case SceneMovementClientPlayerMoveStartMessageId:
	case SceneMovementClientPlayerMoveStopMessageId:
	case SceneMovementClientPlayerMoveSyncMessageId:
	case SceneMovementClientPlayerTeleportRequestMessageId:
	case SceneMovementClientPlayerNotifyMoveAckMessageId:
	case SceneMovementClientPlayerNotifyActorMoveMessageId:
	case SceneMovementClientPlayerNotifyActorMoveListMessageId:
	case SceneMovementClientPlayerNotifyTeleportMessageId:
	case SceneMysteryMerchantClientPlayerGetMysteryMerchantInfoMessageId:
	case SceneMysteryMerchantClientPlayerBuyMysteryMerchantGoodsMessageId:
	case SceneMysteryMerchantClientPlayerNotifyMysteryMerchantMessageId:
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
	case SceneSceneClientPlayerTravelToZoneMessageId:
	case SceneSceneClientPlayerGetSyncDigestMessageId:
	case SceneSceneClientPlayerNotifySyncDigestMessageId:
	case SceneSkillClientPlayerReleaseSkillMessageId:
	case SceneSkillClientPlayerNotifySkillUsedMessageId:
	case SceneSkillClientPlayerNotifySkillInterruptedMessageId:
	case SceneSkillClientPlayerListSkillsMessageId:
	case SceneSkillClientPlayerUpgradeSkillMessageId:
	case ClientPlayerTeamCreateTeamMessageId:
	case ClientPlayerTeamGetMyTeamMessageId:
	case ClientPlayerTeamApplyJoinTeamMessageId:
	case ClientPlayerTeamHandleApplicationMessageId:
	case ClientPlayerTeamInviteToTeamMessageId:
	case ClientPlayerTeamRespondInviteMessageId:
	case ClientPlayerTeamListMyInvitesMessageId:
	case ClientPlayerTeamLeaveTeamMessageId:
	case ClientPlayerTeamKickMemberMessageId:
	case ClientPlayerTeamTransferLeaderMessageId:
	case ClientPlayerTeamDisbandTeamMessageId:
	case ClientPlayerTeamStartTeamMatchMessageId:
	case ClientPlayerTeamNotifyTeamSnapshotMessageId:
	case ClientPlayerTeamNotifyTeamInviteMessageId:
	case ClientPlayerTeamNotifyTeamEventMessageId:
	case ClientPlayerJubaozhaiBrowseListingsMessageId:
	case ClientPlayerJubaozhaiGetListingDetailMessageId:
	case ClientPlayerJubaozhaiSetFavoriteMessageId:
	case ClientPlayerJubaozhaiGetMyShelfMessageId:
	case ClientPlayerJubaozhaiListOfficialSkusMessageId:
	case ClientPlayerJubaozhaiCreateOfficialOrderMessageId:
	case ClientPlayerJubaozhaiGetCommerceOrderMessageId:
	case ClientPlayerJubaozhaiCancelCommerceOrderMessageId:
	case ClientPlayerJubaozhaiListCommerceOrdersMessageId:
	case ClientPlayerJubaozhaiCreatePlayerTradeMessageId:
	case ClientPlayerJubaozhaiConfirmPlayerTradeMessageId:
	case ClientPlayerJubaozhaiCreateConsignmentMessageId:
	case ClientPlayerJubaozhaiBuyConsignmentMessageId:
	case ClientPlayerJubaozhaiCancelConsignmentMessageId:
	case ClientPlayerJubaozhaiPlaceMarketOrderMessageId:
	case ClientPlayerJubaozhaiCancelMarketOrderMessageId:
	case ClientPlayerJubaozhaiListMarketOrdersMessageId:
	case ClientPlayerJubaozhaiGetMarketDepthMessageId:
	case ClientPlayerJubaozhaiCreateAuctionLotMessageId:
	case ClientPlayerJubaozhaiPlaceAuctionBidMessageId:
	case ClientPlayerJubaozhaiGetAuctionLotMessageId:
	case ClientPlayerJubaozhaiCancelAuctionLotMessageId:
	case ClientPlayerJubaozhaiBrowseCommerceOffersMessageId:
	case ClientPlayerJubaozhaiListTradeMarketsMessageId:
	case ClientPlayerJubaozhaiCreateRmbConsignmentMessageId:
	case ClientPlayerJubaozhaiGetRmbConsignmentMessageId:
	case ClientPlayerJubaozhaiListRmbConsignmentsMessageId:
	case ClientPlayerJubaozhaiBuyRmbConsignmentMessageId:
	case ClientPlayerJubaozhaiCancelRmbConsignmentMessageId:
	case ClientPlayerJubaozhaiGetRmbOrderMessageId:
	case ClientPlayerJubaozhaiListRmbOrdersMessageId:
	case ClientPlayerJubaozhaiCancelRmbOrderMessageId:
	case ClientPlayerJubaozhaiListSellerIncomesMessageId:
	case ClientPlayerJubaozhaiCreateWithdrawalMessageId:
	case ClientPlayerJubaozhaiGetWithdrawalMessageId:
		return true;
	default:
		return false;
	}
}

bool IsValidEventId(uint32_t eventId)
{
	return eventId < kMaxEventCount;
}
