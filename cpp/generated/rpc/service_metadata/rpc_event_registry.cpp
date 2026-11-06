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
#include "proto/player_store/player_store.pb.h"
#include "proto/rank/rank.pb.h"
#include "proto/rank/rank_admin.pb.h"
#include "proto/scene/client_player_common.pb.h"
#include "proto/scene/player_activity.pb.h"
#include "proto/scene/player_attribute.pb.h"
#include "proto/scene/player_bag.pb.h"
#include "proto/scene/player_battle_result.pb.h"
#include "proto/scene/player_currency.pb.h"
#include "proto/scene/player_lifecycle.pb.h"
#include "proto/scene/player_mission.pb.h"
#include "proto/scene/player_movement.pb.h"
#include "proto/scene/player_mystery_merchant.pb.h"
#include "proto/scene/player_pet.pb.h"
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
#include "rpc/service_metadata/player_store_service_metadata.h"
#include "rpc/service_metadata/rank_service_metadata.h"
#include "rpc/service_metadata/rank_admin_service_metadata.h"
#include "rpc/service_metadata/client_player_common_service_metadata.h"
#include "rpc/service_metadata/player_activity_service_metadata.h"
#include "rpc/service_metadata/player_attribute_service_metadata.h"
#include "rpc/service_metadata/player_bag_service_metadata.h"
#include "rpc/service_metadata/player_battle_result_service_metadata.h"
#include "rpc/service_metadata/player_currency_service_metadata.h"
#include "rpc/service_metadata/player_lifecycle_service_metadata.h"
#include "rpc/service_metadata/player_mission_service_metadata.h"
#include "rpc/service_metadata/player_movement_service_metadata.h"
#include "rpc/service_metadata/player_mystery_merchant_service_metadata.h"
#include "rpc/service_metadata/player_pet_service_metadata.h"
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
namespace data_service{void SendDataServiceCheckAccountRoleSlotErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceGetRoleFenceStateErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceVerifyRoleAdmissionErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceRegisterRoleAdmissionErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceCloseRoleAdmissionsErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceAckRoleTradeDrainErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceReleaseDeadPlayerStoreHolderErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceReadPlayerStoreHolderErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceRegisterPlayerZoneErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceGetPlayerHomeZoneErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceBatchGetPlayerHomeZoneErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceGetMergedZonesErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceGetPlayerRouteErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceListPlayerSnapshotsErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceBatchRecallItemsErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceGetRecallBatchErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceCancelRecallBatchErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceResolveRecallOpErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceQueryTransactionLogErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceAllocateIdSegmentErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceReservePlayerNameErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceReleasePlayerNameErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceBatchGetPlayerNameErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace data_service{void SendDataServiceBatchGetPlayerDisplayErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
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
namespace loginpb{void SendClientPlayerLoginRequestGateMigrationErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
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
namespace match{void SendMatchInternalAcquireTrialOccupancyErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace match{void SendMatchInternalReleaseTrialOccupancyErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace match{void SendMatchInternalGetBattleCompletionErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace match{void SendMatchInternalGetBattleTerminalDecisionErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace match{void SendMatchInternalListStrandedFormationsErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace match{void SendMatchInternalResolveStrandedFormationErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
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
namespace match{void SendMatchServiceListBattleReportsErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace match{void SendMatchServiceGetBattleReportErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
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
namespace playerstore{void SendPlayerStoreLoadAndClaimErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
namespace playerstore{void SendPlayerStoreSavePlayerErased(entt::registry& , entt::entity , const google::protobuf::Message& , GrpcMessageDone , const GrpcCallOptions& );}
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
static_assert(kMaxRpcMethodCount == 383,
    "kMaxRpcMethodCount out of sync with this generation run - rerun the full proto generator");
std::array<RpcMethodMeta, kMaxRpcMethodCount> gRpcMethodRegistry;

void InitMessageInfo()
{
    // --- BattleClientPlayer ---
    gRpcMethodRegistry[BattleClientPlayerSubmitBattleActionMessageId] = RpcMethodMeta{
        "BattleClientPlayer", "SubmitBattleAction",
        std::make_unique<::SubmitBattleActionRequest>(),
        std::make_unique<::SubmitBattleActionResponse>(),
        1, common::base::eNodeType::BattleNodeService, ::SendBattleClientPlayerSubmitBattleActionErased, "/BattleClientPlayer/SubmitBattleAction"};
    gRpcMethodRegistry[BattleClientPlayerGetBattleStateMessageId] = RpcMethodMeta{
        "BattleClientPlayer", "GetBattleState",
        std::make_unique<::GetBattleStateRequest>(),
        std::make_unique<::BattleStateS2C>(),
        1, common::base::eNodeType::BattleNodeService, ::SendBattleClientPlayerGetBattleStateErased, "/BattleClientPlayer/GetBattleState"};
    gRpcMethodRegistry[BattleClientPlayerNotifyBattleStartMessageId] = RpcMethodMeta{
        "BattleClientPlayer", "NotifyBattleStart",
        std::make_unique<::BattleStartS2C>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::BattleNodeService, ::SendBattleClientPlayerNotifyBattleStartErased, "/BattleClientPlayer/NotifyBattleStart"};
    gRpcMethodRegistry[BattleClientPlayerNotifyTurnResultMessageId] = RpcMethodMeta{
        "BattleClientPlayer", "NotifyTurnResult",
        std::make_unique<::TurnResultS2C>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::BattleNodeService, ::SendBattleClientPlayerNotifyTurnResultErased, "/BattleClientPlayer/NotifyTurnResult"};
    gRpcMethodRegistry[BattleClientPlayerNotifyBattleEndMessageId] = RpcMethodMeta{
        "BattleClientPlayer", "NotifyBattleEnd",
        std::make_unique<::BattleEndS2C>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::BattleNodeService, ::SendBattleClientPlayerNotifyBattleEndErased, "/BattleClientPlayer/NotifyBattleEnd"};
    gRpcMethodRegistry[BattleClientPlayerNotifyBattleReconnectMessageId] = RpcMethodMeta{
        "BattleClientPlayer", "NotifyBattleReconnect",
        std::make_unique<::BattleReconnectS2C>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::BattleNodeService, ::SendBattleClientPlayerNotifyBattleReconnectErased, "/BattleClientPlayer/NotifyBattleReconnect"};
    gRpcMethodRegistry[BattleClientPlayerStopWatchBattleMessageId] = RpcMethodMeta{
        "BattleClientPlayer", "StopWatchBattle",
        std::make_unique<::StopWatchBattleRequest>(),
        std::make_unique<::StopWatchBattleResponse>(),
        1, common::base::eNodeType::BattleNodeService, ::SendBattleClientPlayerStopWatchBattleErased, "/BattleClientPlayer/StopWatchBattle"};
    gRpcMethodRegistry[BattleClientPlayerSetAutoBattleMessageId] = RpcMethodMeta{
        "BattleClientPlayer", "SetAutoBattle",
        std::make_unique<::SetAutoBattleRequest>(),
        std::make_unique<::SetAutoBattleResponse>(),
        1, common::base::eNodeType::BattleNodeService, ::SendBattleClientPlayerSetAutoBattleErased, "/BattleClientPlayer/SetAutoBattle"};
    gRpcMethodRegistry[BattleClientPlayerNotifySpectateStateMessageId] = RpcMethodMeta{
        "BattleClientPlayer", "NotifySpectateState",
        std::make_unique<::SpectateStateS2C>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::BattleNodeService, ::SendBattleClientPlayerNotifySpectateStateErased, "/BattleClientPlayer/NotifySpectateState"};
    gRpcMethodRegistry[BattleClientPlayerNotifySpectateTurnResultMessageId] = RpcMethodMeta{
        "BattleClientPlayer", "NotifySpectateTurnResult",
        std::make_unique<::TurnResultS2C>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::BattleNodeService, ::SendBattleClientPlayerNotifySpectateTurnResultErased, "/BattleClientPlayer/NotifySpectateTurnResult"};
    gRpcMethodRegistry[BattleClientPlayerNotifySpectateEndMessageId] = RpcMethodMeta{
        "BattleClientPlayer", "NotifySpectateEnd",
        std::make_unique<::SpectateEndS2C>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::BattleNodeService, ::SendBattleClientPlayerNotifySpectateEndErased, "/BattleClientPlayer/NotifySpectateEnd"};
    gRpcMethodRegistry[BattleClientPlayerNotifyBattleAssignedMessageId] = RpcMethodMeta{
        "BattleClientPlayer", "NotifyBattleAssigned",
        std::make_unique<::BattleAssignedS2C>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::BattleNodeService, ::SendBattleClientPlayerNotifyBattleAssignedErased, "/BattleClientPlayer/NotifyBattleAssigned"};

    // --- BattleNode ---
    gRpcMethodRegistry[BattleNodeQueryObserverMessageId] = RpcMethodMeta{
        "BattleNode", "QueryObserver",
        std::make_unique<::QueryObserverRequest>(),
        std::make_unique<::QueryObserverResponse>(),
        1, common::base::eNodeType::BattleNodeService, ::SendBattleNodeQueryObserverErased, "/BattleNode/QueryObserver"};
    gRpcMethodRegistry[BattleNodeQueryBattleCreationMessageId] = RpcMethodMeta{
        "BattleNode", "QueryBattleCreation",
        std::make_unique<::QueryBattleCreationRequest>(),
        std::make_unique<::QueryBattleCreationResponse>(),
        1, common::base::eNodeType::BattleNodeService, ::SendBattleNodeQueryBattleCreationErased, "/BattleNode/QueryBattleCreation"};
    gRpcMethodRegistry[BattleNodeCreateBattleMessageId] = RpcMethodMeta{
        "BattleNode", "CreateBattle",
        std::make_unique<::CreateBattleRequest>(),
        std::make_unique<::CreateBattleResponse>(),
        1, common::base::eNodeType::BattleNodeService, ::SendBattleNodeCreateBattleErased, "/BattleNode/CreateBattle"};
    gRpcMethodRegistry[BattleNodeDestroyBattleMessageId] = RpcMethodMeta{
        "BattleNode", "DestroyBattle",
        std::make_unique<::DestroyBattleRequest>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::BattleNodeService, ::SendBattleNodeDestroyBattleErased, "/BattleNode/DestroyBattle"};
    gRpcMethodRegistry[BattleNodeAddObserverMessageId] = RpcMethodMeta{
        "BattleNode", "AddObserver",
        std::make_unique<::AddObserverRequest>(),
        std::make_unique<::AddObserverResponse>(),
        1, common::base::eNodeType::BattleNodeService, ::SendBattleNodeAddObserverErased, "/BattleNode/AddObserver"};
    gRpcMethodRegistry[BattleNodeRemoveObserverMessageId] = RpcMethodMeta{
        "BattleNode", "RemoveObserver",
        std::make_unique<::RemoveObserverRequest>(),
        std::make_unique<::RemoveObserverResponse>(),
        1, common::base::eNodeType::BattleNodeService, ::SendBattleNodeRemoveObserverErased, "/BattleNode/RemoveObserver"};
    gRpcMethodRegistry[BattleNodeIssueBattleTicketMessageId] = RpcMethodMeta{
        "BattleNode", "IssueBattleTicket",
        std::make_unique<::IssueBattleTicketRequest>(),
        std::make_unique<::IssueBattleTicketResponse>(),
        1, common::base::eNodeType::BattleNodeService, ::SendBattleNodeIssueBattleTicketErased, "/BattleNode/IssueBattleTicket"};
    gRpcMethodRegistry[BattleNodeAttachRelayGateMessageId] = RpcMethodMeta{
        "BattleNode", "AttachRelayGate",
        std::make_unique<::AttachRelayGateRequest>(),
        std::make_unique<::AttachRelayGateResponse>(),
        1, common::base::eNodeType::BattleNodeService, ::SendBattleNodeAttachRelayGateErased, "/BattleNode/AttachRelayGate"};
    gRpcMethodRegistry[BattleNodePromoteSpectateBroadcastMessageId] = RpcMethodMeta{
        "BattleNode", "PromoteSpectateBroadcast",
        std::make_unique<::PromoteSpectateBroadcastRequest>(),
        std::make_unique<::PromoteSpectateBroadcastResponse>(),
        1, common::base::eNodeType::BattleNodeService, ::SendBattleNodePromoteSpectateBroadcastErased, "/BattleNode/PromoteSpectateBroadcast"};

    // --- ClientPlayerChat ---
    gRpcMethodRegistry[ClientPlayerChatSendChatMessageId] = RpcMethodMeta{
        "ClientPlayerChat", "SendChat",
        std::make_unique<::chatpb::SendChatRequest>(),
        std::make_unique<::chatpb::SendChatResponse>(),
        1, common::base::eNodeType::ChatNodeService, chatpb::SendClientPlayerChatSendChatErased, "/chatpb.ClientPlayerChat/SendChat"};
    gRpcMethodRegistry[ClientPlayerChatPullChatHistoryMessageId] = RpcMethodMeta{
        "ClientPlayerChat", "PullChatHistory",
        std::make_unique<::chatpb::PullChatHistoryRequest>(),
        std::make_unique<::chatpb::PullChatHistoryResponse>(),
        1, common::base::eNodeType::ChatNodeService, chatpb::SendClientPlayerChatPullChatHistoryErased, "/chatpb.ClientPlayerChat/PullChatHistory"};

    // --- ClientRpcRouter ---
    gRpcMethodRegistry[ClientRpcRouterForwardMessageId] = RpcMethodMeta{
        "ClientRpcRouter", "Forward",
        std::make_unique<::client_rpc_router::ForwardRequest>(),
        std::make_unique<::MessageContent>(),
        1, common::base::eNodeType::ClientRpcRouterNodeService, client_rpc_router::SendClientRpcRouterForwardErased, "/client_rpc_router.ClientRpcRouter/Forward"};

    // --- DataService ---
    gRpcMethodRegistry[DataServiceReadAccountDirectoryMessageId] = RpcMethodMeta{
        "DataService", "ReadAccountDirectory",
        std::make_unique<::data_service::ReadAccountDirectoryRequest>(),
        std::make_unique<::data_service::AccountDirectoryResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceReadAccountDirectoryErased, "/data_service.DataService/ReadAccountDirectory"};
    gRpcMethodRegistry[DataServiceCreateAccountRoleMessageId] = RpcMethodMeta{
        "DataService", "CreateAccountRole",
        std::make_unique<::data_service::CreateAccountRoleRequest>(),
        std::make_unique<::data_service::AccountDirectoryResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceCreateAccountRoleErased, "/data_service.DataService/CreateAccountRole"};
    gRpcMethodRegistry[DataServiceLookupAccountRoleMessageId] = RpcMethodMeta{
        "DataService", "LookupAccountRole",
        std::make_unique<::data_service::LookupAccountRoleRequest>(),
        std::make_unique<::data_service::AccountRoleOwnership>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceLookupAccountRoleErased, "/data_service.DataService/LookupAccountRole"};
    gRpcMethodRegistry[DataServiceRepairAccountRoleMessageId] = RpcMethodMeta{
        "DataService", "RepairAccountRole",
        std::make_unique<::data_service::RepairAccountRoleRequest>(),
        std::make_unique<::data_service::AccountDirectoryResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceRepairAccountRoleErased, "/data_service.DataService/RepairAccountRole"};
    gRpcMethodRegistry[DataServiceRemoveAccountRolesMessageId] = RpcMethodMeta{
        "DataService", "RemoveAccountRoles",
        std::make_unique<::data_service::RemoveAccountRolesRequest>(),
        std::make_unique<::google::protobuf::Empty>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceRemoveAccountRolesErased, "/data_service.DataService/RemoveAccountRoles"};
    gRpcMethodRegistry[DataServiceBeginAccountRoleTradeMessageId] = RpcMethodMeta{
        "DataService", "BeginAccountRoleTrade",
        std::make_unique<::data_service::BeginAccountRoleTradeRequest>(),
        std::make_unique<::data_service::AccountRoleTradeReceipt>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceBeginAccountRoleTradeErased, "/data_service.DataService/BeginAccountRoleTrade"};
    gRpcMethodRegistry[DataServiceConfirmAccountRoleTradeFreezeMessageId] = RpcMethodMeta{
        "DataService", "ConfirmAccountRoleTradeFreeze",
        std::make_unique<::data_service::ConfirmAccountRoleTradeFreezeRequest>(),
        std::make_unique<::data_service::AccountRoleTradeReceipt>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceConfirmAccountRoleTradeFreezeErased, "/data_service.DataService/ConfirmAccountRoleTradeFreeze"};
    gRpcMethodRegistry[DataServiceCancelAccountRoleTradeMessageId] = RpcMethodMeta{
        "DataService", "CancelAccountRoleTrade",
        std::make_unique<::data_service::CancelAccountRoleTradeRequest>(),
        std::make_unique<::data_service::AccountRoleTradeReceipt>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceCancelAccountRoleTradeErased, "/data_service.DataService/CancelAccountRoleTrade"};
    gRpcMethodRegistry[DataServiceTransferAccountRoleMessageId] = RpcMethodMeta{
        "DataService", "TransferAccountRole",
        std::make_unique<::data_service::TransferAccountRoleRequest>(),
        std::make_unique<::data_service::AccountRoleTransferReceipt>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceTransferAccountRoleErased, "/data_service.DataService/TransferAccountRole"};
    gRpcMethodRegistry[DataServiceCheckAccountRoleSlotMessageId] = RpcMethodMeta{
        "DataService", "CheckAccountRoleSlot",
        std::make_unique<::data_service::CheckAccountRoleSlotRequest>(),
        std::make_unique<::data_service::CheckAccountRoleSlotResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceCheckAccountRoleSlotErased, "/data_service.DataService/CheckAccountRoleSlot"};
    gRpcMethodRegistry[DataServiceGetRoleFenceStateMessageId] = RpcMethodMeta{
        "DataService", "GetRoleFenceState",
        std::make_unique<::data_service::GetRoleFenceStateRequest>(),
        std::make_unique<::data_service::GetRoleFenceStateResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceGetRoleFenceStateErased, "/data_service.DataService/GetRoleFenceState"};
    gRpcMethodRegistry[DataServiceVerifyRoleAdmissionMessageId] = RpcMethodMeta{
        "DataService", "VerifyRoleAdmission",
        std::make_unique<::data_service::VerifyRoleAdmissionRequest>(),
        std::make_unique<::data_service::VerifyRoleAdmissionResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceVerifyRoleAdmissionErased, "/data_service.DataService/VerifyRoleAdmission"};
    gRpcMethodRegistry[DataServiceRegisterRoleAdmissionMessageId] = RpcMethodMeta{
        "DataService", "RegisterRoleAdmission",
        std::make_unique<::data_service::RegisterRoleAdmissionRequest>(),
        std::make_unique<::data_service::RegisterRoleAdmissionResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceRegisterRoleAdmissionErased, "/data_service.DataService/RegisterRoleAdmission"};
    gRpcMethodRegistry[DataServiceCloseRoleAdmissionsMessageId] = RpcMethodMeta{
        "DataService", "CloseRoleAdmissions",
        std::make_unique<::data_service::CloseRoleAdmissionsRequest>(),
        std::make_unique<::data_service::CloseRoleAdmissionsResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceCloseRoleAdmissionsErased, "/data_service.DataService/CloseRoleAdmissions"};
    gRpcMethodRegistry[DataServiceAckRoleTradeDrainMessageId] = RpcMethodMeta{
        "DataService", "AckRoleTradeDrain",
        std::make_unique<::data_service::AckRoleTradeDrainRequest>(),
        std::make_unique<::google::protobuf::Empty>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceAckRoleTradeDrainErased, "/data_service.DataService/AckRoleTradeDrain"};
    gRpcMethodRegistry[DataServiceReleaseDeadPlayerStoreHolderMessageId] = RpcMethodMeta{
        "DataService", "ReleaseDeadPlayerStoreHolder",
        std::make_unique<::data_service::ReleaseDeadPlayerStoreHolderRequest>(),
        std::make_unique<::google::protobuf::Empty>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceReleaseDeadPlayerStoreHolderErased, "/data_service.DataService/ReleaseDeadPlayerStoreHolder"};
    gRpcMethodRegistry[DataServiceReadPlayerStoreHolderMessageId] = RpcMethodMeta{
        "DataService", "ReadPlayerStoreHolder",
        std::make_unique<::data_service::ReadPlayerStoreHolderRequest>(),
        std::make_unique<::data_service::ReadPlayerStoreHolderResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceReadPlayerStoreHolderErased, "/data_service.DataService/ReadPlayerStoreHolder"};
    gRpcMethodRegistry[DataServiceRegisterPlayerZoneMessageId] = RpcMethodMeta{
        "DataService", "RegisterPlayerZone",
        std::make_unique<::data_service::RegisterPlayerZoneRequest>(),
        std::make_unique<::google::protobuf::Empty>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceRegisterPlayerZoneErased, "/data_service.DataService/RegisterPlayerZone"};
    gRpcMethodRegistry[DataServiceGetPlayerHomeZoneMessageId] = RpcMethodMeta{
        "DataService", "GetPlayerHomeZone",
        std::make_unique<::data_service::GetPlayerHomeZoneRequest>(),
        std::make_unique<::data_service::GetPlayerHomeZoneResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceGetPlayerHomeZoneErased, "/data_service.DataService/GetPlayerHomeZone"};
    gRpcMethodRegistry[DataServiceBatchGetPlayerHomeZoneMessageId] = RpcMethodMeta{
        "DataService", "BatchGetPlayerHomeZone",
        std::make_unique<::data_service::BatchGetPlayerHomeZoneRequest>(),
        std::make_unique<::data_service::BatchGetPlayerHomeZoneResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceBatchGetPlayerHomeZoneErased, "/data_service.DataService/BatchGetPlayerHomeZone"};
    gRpcMethodRegistry[DataServiceGetMergedZonesMessageId] = RpcMethodMeta{
        "DataService", "GetMergedZones",
        std::make_unique<::google::protobuf::Empty>(),
        std::make_unique<::data_service::GetMergedZonesResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceGetMergedZonesErased, "/data_service.DataService/GetMergedZones"};
    gRpcMethodRegistry[DataServiceGetPlayerRouteMessageId] = RpcMethodMeta{
        "DataService", "GetPlayerRoute",
        std::make_unique<::data_service::GetPlayerRouteRequest>(),
        std::make_unique<::data_service::GetPlayerRouteResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceGetPlayerRouteErased, "/data_service.DataService/GetPlayerRoute"};
    gRpcMethodRegistry[DataServiceListPlayerSnapshotsMessageId] = RpcMethodMeta{
        "DataService", "ListPlayerSnapshots",
        std::make_unique<::data_service::ListPlayerSnapshotsRequest>(),
        std::make_unique<::data_service::ListPlayerSnapshotsResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceListPlayerSnapshotsErased, "/data_service.DataService/ListPlayerSnapshots"};
    gRpcMethodRegistry[DataServiceBatchRecallItemsMessageId] = RpcMethodMeta{
        "DataService", "BatchRecallItems",
        std::make_unique<::data_service::BatchRecallItemsRequest>(),
        std::make_unique<::data_service::BatchRecallItemsResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceBatchRecallItemsErased, "/data_service.DataService/BatchRecallItems"};
    gRpcMethodRegistry[DataServiceGetRecallBatchMessageId] = RpcMethodMeta{
        "DataService", "GetRecallBatch",
        std::make_unique<::data_service::GetRecallBatchRequest>(),
        std::make_unique<::data_service::BatchRecallItemsResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceGetRecallBatchErased, "/data_service.DataService/GetRecallBatch"};
    gRpcMethodRegistry[DataServiceCancelRecallBatchMessageId] = RpcMethodMeta{
        "DataService", "CancelRecallBatch",
        std::make_unique<::data_service::CancelRecallBatchRequest>(),
        std::make_unique<::data_service::BatchRecallItemsResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceCancelRecallBatchErased, "/data_service.DataService/CancelRecallBatch"};
    gRpcMethodRegistry[DataServiceResolveRecallOpMessageId] = RpcMethodMeta{
        "DataService", "ResolveRecallOp",
        std::make_unique<::data_service::ResolveRecallOpRequest>(),
        std::make_unique<::data_service::ResolveRecallOpResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceResolveRecallOpErased, "/data_service.DataService/ResolveRecallOp"};
    gRpcMethodRegistry[DataServiceQueryTransactionLogMessageId] = RpcMethodMeta{
        "DataService", "QueryTransactionLog",
        std::make_unique<::data_service::QueryTransactionLogRequest>(),
        std::make_unique<::data_service::QueryTransactionLogResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceQueryTransactionLogErased, "/data_service.DataService/QueryTransactionLog"};
    gRpcMethodRegistry[DataServiceAllocateIdSegmentMessageId] = RpcMethodMeta{
        "DataService", "AllocateIdSegment",
        std::make_unique<::data_service::AllocateIdSegmentRequest>(),
        std::make_unique<::data_service::AllocateIdSegmentResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceAllocateIdSegmentErased, "/data_service.DataService/AllocateIdSegment"};
    gRpcMethodRegistry[DataServiceReservePlayerNameMessageId] = RpcMethodMeta{
        "DataService", "ReservePlayerName",
        std::make_unique<::data_service::ReservePlayerNameRequest>(),
        std::make_unique<::data_service::ReservePlayerNameResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceReservePlayerNameErased, "/data_service.DataService/ReservePlayerName"};
    gRpcMethodRegistry[DataServiceReleasePlayerNameMessageId] = RpcMethodMeta{
        "DataService", "ReleasePlayerName",
        std::make_unique<::data_service::ReleasePlayerNameRequest>(),
        std::make_unique<::google::protobuf::Empty>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceReleasePlayerNameErased, "/data_service.DataService/ReleasePlayerName"};
    gRpcMethodRegistry[DataServiceBatchGetPlayerNameMessageId] = RpcMethodMeta{
        "DataService", "BatchGetPlayerName",
        std::make_unique<::data_service::BatchGetPlayerNameRequest>(),
        std::make_unique<::data_service::BatchGetPlayerNameResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceBatchGetPlayerNameErased, "/data_service.DataService/BatchGetPlayerName"};
    gRpcMethodRegistry[DataServiceBatchGetPlayerDisplayMessageId] = RpcMethodMeta{
        "DataService", "BatchGetPlayerDisplay",
        std::make_unique<::data_service::BatchGetPlayerDisplayRequest>(),
        std::make_unique<::data_service::BatchGetPlayerDisplayResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceBatchGetPlayerDisplayErased, "/data_service.DataService/BatchGetPlayerDisplay"};
    gRpcMethodRegistry[DataServiceGetPlayerAssetOpLedgerMessageId] = RpcMethodMeta{
        "DataService", "GetPlayerAssetOpLedger",
        std::make_unique<::data_service::GetPlayerAssetOpLedgerRequest>(),
        std::make_unique<::data_service::GetPlayerAssetOpLedgerResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceGetPlayerAssetOpLedgerErased, "/data_service.DataService/GetPlayerAssetOpLedger"};
    gRpcMethodRegistry[DataServiceGetPlayerInboxLedgerMessageId] = RpcMethodMeta{
        "DataService", "GetPlayerInboxLedger",
        std::make_unique<::data_service::GetPlayerInboxLedgerRequest>(),
        std::make_unique<::data_service::GetPlayerInboxLedgerResponse>(),
        1, common::base::eNodeType::DataServiceNodeService, data_service::SendDataServiceGetPlayerInboxLedgerErased, "/data_service.DataService/GetPlayerInboxLedger"};

    // --- KV ---
    gRpcMethodRegistry[KVRangeMessageId] = RpcMethodMeta{
        "KV", "Range",
        std::make_unique<::etcdserverpb::RangeRequest>(),
        std::make_unique<::etcdserverpb::RangeResponse>(),
        0, common::base::eNodeType::EtcdNodeService, etcdserverpb::SendKVRangeErased, "/etcdserverpb.KV/Range"};
    gRpcMethodRegistry[KVPutMessageId] = RpcMethodMeta{
        "KV", "Put",
        std::make_unique<::etcdserverpb::PutRequest>(),
        std::make_unique<::etcdserverpb::PutResponse>(),
        0, common::base::eNodeType::EtcdNodeService, etcdserverpb::SendKVPutErased, "/etcdserverpb.KV/Put"};
    gRpcMethodRegistry[KVDeleteRangeMessageId] = RpcMethodMeta{
        "KV", "DeleteRange",
        std::make_unique<::etcdserverpb::DeleteRangeRequest>(),
        std::make_unique<::etcdserverpb::DeleteRangeResponse>(),
        0, common::base::eNodeType::EtcdNodeService, etcdserverpb::SendKVDeleteRangeErased, "/etcdserverpb.KV/DeleteRange"};
    gRpcMethodRegistry[KVTxnMessageId] = RpcMethodMeta{
        "KV", "Txn",
        std::make_unique<::etcdserverpb::TxnRequest>(),
        std::make_unique<::etcdserverpb::TxnResponse>(),
        0, common::base::eNodeType::EtcdNodeService, etcdserverpb::SendKVTxnErased, "/etcdserverpb.KV/Txn"};
    gRpcMethodRegistry[KVCompactMessageId] = RpcMethodMeta{
        "KV", "Compact",
        std::make_unique<::etcdserverpb::CompactionRequest>(),
        std::make_unique<::etcdserverpb::CompactionResponse>(),
        0, common::base::eNodeType::EtcdNodeService, etcdserverpb::SendKVCompactErased, "/etcdserverpb.KV/Compact"};

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
        0, common::base::eNodeType::EtcdNodeService, etcdserverpb::SendLeaseLeaseGrantErased, "/etcdserverpb.Lease/LeaseGrant"};
    gRpcMethodRegistry[LeaseLeaseRevokeMessageId] = RpcMethodMeta{
        "Lease", "LeaseRevoke",
        std::make_unique<::etcdserverpb::LeaseRevokeRequest>(),
        std::make_unique<::etcdserverpb::LeaseRevokeResponse>(),
        0, common::base::eNodeType::EtcdNodeService, etcdserverpb::SendLeaseLeaseRevokeErased, "/etcdserverpb.Lease/LeaseRevoke"};
    gRpcMethodRegistry[LeaseLeaseKeepAliveMessageId] = RpcMethodMeta{
        "Lease", "LeaseKeepAlive",
        std::make_unique<::etcdserverpb::LeaseKeepAliveRequest>(),
        std::make_unique<::etcdserverpb::LeaseKeepAliveResponse>(),
        0, common::base::eNodeType::EtcdNodeService};
    gRpcMethodRegistry[LeaseLeaseTimeToLiveMessageId] = RpcMethodMeta{
        "Lease", "LeaseTimeToLive",
        std::make_unique<::etcdserverpb::LeaseTimeToLiveRequest>(),
        std::make_unique<::etcdserverpb::LeaseTimeToLiveResponse>(),
        0, common::base::eNodeType::EtcdNodeService, etcdserverpb::SendLeaseLeaseTimeToLiveErased, "/etcdserverpb.Lease/LeaseTimeToLive"};
    gRpcMethodRegistry[LeaseLeaseLeasesMessageId] = RpcMethodMeta{
        "Lease", "LeaseLeases",
        std::make_unique<::etcdserverpb::LeaseLeasesRequest>(),
        std::make_unique<::etcdserverpb::LeaseLeasesResponse>(),
        0, common::base::eNodeType::EtcdNodeService, etcdserverpb::SendLeaseLeaseLeasesErased, "/etcdserverpb.Lease/LeaseLeases"};

    // --- ClientPlayerFriend ---
    gRpcMethodRegistry[ClientPlayerFriendAddFriendMessageId] = RpcMethodMeta{
        "ClientPlayerFriend", "AddFriend",
        std::make_unique<::friendpb::AddFriendRequest>(),
        std::make_unique<::friendpb::AddFriendResponse>(),
        1, common::base::eNodeType::FriendNodeService, friendpb::SendClientPlayerFriendAddFriendErased, "/friendpb.ClientPlayerFriend/AddFriend"};
    gRpcMethodRegistry[ClientPlayerFriendAcceptFriendMessageId] = RpcMethodMeta{
        "ClientPlayerFriend", "AcceptFriend",
        std::make_unique<::friendpb::AcceptFriendRequest>(),
        std::make_unique<::friendpb::AcceptFriendResponse>(),
        1, common::base::eNodeType::FriendNodeService, friendpb::SendClientPlayerFriendAcceptFriendErased, "/friendpb.ClientPlayerFriend/AcceptFriend"};
    gRpcMethodRegistry[ClientPlayerFriendRejectFriendMessageId] = RpcMethodMeta{
        "ClientPlayerFriend", "RejectFriend",
        std::make_unique<::friendpb::RejectFriendRequest>(),
        std::make_unique<::friendpb::RejectFriendResponse>(),
        1, common::base::eNodeType::FriendNodeService, friendpb::SendClientPlayerFriendRejectFriendErased, "/friendpb.ClientPlayerFriend/RejectFriend"};
    gRpcMethodRegistry[ClientPlayerFriendRemoveFriendMessageId] = RpcMethodMeta{
        "ClientPlayerFriend", "RemoveFriend",
        std::make_unique<::friendpb::RemoveFriendRequest>(),
        std::make_unique<::friendpb::RemoveFriendResponse>(),
        1, common::base::eNodeType::FriendNodeService, friendpb::SendClientPlayerFriendRemoveFriendErased, "/friendpb.ClientPlayerFriend/RemoveFriend"};
    gRpcMethodRegistry[ClientPlayerFriendGetFriendListMessageId] = RpcMethodMeta{
        "ClientPlayerFriend", "GetFriendList",
        std::make_unique<::friendpb::GetFriendListRequest>(),
        std::make_unique<::friendpb::GetFriendListResponse>(),
        1, common::base::eNodeType::FriendNodeService, friendpb::SendClientPlayerFriendGetFriendListErased, "/friendpb.ClientPlayerFriend/GetFriendList"};
    gRpcMethodRegistry[ClientPlayerFriendGetPendingRequestsMessageId] = RpcMethodMeta{
        "ClientPlayerFriend", "GetPendingRequests",
        std::make_unique<::friendpb::GetPendingRequestsRequest>(),
        std::make_unique<::friendpb::GetPendingRequestsResponse>(),
        1, common::base::eNodeType::FriendNodeService, friendpb::SendClientPlayerFriendGetPendingRequestsErased, "/friendpb.ClientPlayerFriend/GetPendingRequests"};
    gRpcMethodRegistry[ClientPlayerFriendBlockMessageId] = RpcMethodMeta{
        "ClientPlayerFriend", "Block",
        std::make_unique<::friendpb::BlockRequest>(),
        std::make_unique<::friendpb::BlockResponse>(),
        1, common::base::eNodeType::FriendNodeService, friendpb::SendClientPlayerFriendBlockErased, "/friendpb.ClientPlayerFriend/Block"};
    gRpcMethodRegistry[ClientPlayerFriendUnblockMessageId] = RpcMethodMeta{
        "ClientPlayerFriend", "Unblock",
        std::make_unique<::friendpb::UnblockRequest>(),
        std::make_unique<::friendpb::UnblockResponse>(),
        1, common::base::eNodeType::FriendNodeService, friendpb::SendClientPlayerFriendUnblockErased, "/friendpb.ClientPlayerFriend/Unblock"};
    gRpcMethodRegistry[ClientPlayerFriendListBlocksMessageId] = RpcMethodMeta{
        "ClientPlayerFriend", "ListBlocks",
        std::make_unique<::friendpb::ListBlocksRequest>(),
        std::make_unique<::friendpb::ListBlocksResponse>(),
        1, common::base::eNodeType::FriendNodeService, friendpb::SendClientPlayerFriendListBlocksErased, "/friendpb.ClientPlayerFriend/ListBlocks"};
    gRpcMethodRegistry[ClientPlayerFriendRecommendFriendsMessageId] = RpcMethodMeta{
        "ClientPlayerFriend", "RecommendFriends",
        std::make_unique<::friendpb::RecommendFriendsRequest>(),
        std::make_unique<::friendpb::RecommendFriendsResponse>(),
        1, common::base::eNodeType::FriendNodeService, friendpb::SendClientPlayerFriendRecommendFriendsErased, "/friendpb.ClientPlayerFriend/RecommendFriends"};
    gRpcMethodRegistry[ClientPlayerFriendNotifyFriendEventMessageId] = RpcMethodMeta{
        "ClientPlayerFriend", "NotifyFriendEvent",
        std::make_unique<::friendpb::FriendEventS2C>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::FriendNodeService, friendpb::SendClientPlayerFriendNotifyFriendEventErased, "/friendpb.ClientPlayerFriend/NotifyFriendEvent"};

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
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceCreateGuildErased, "/guildpb.GuildService/CreateGuild"};
    gRpcMethodRegistry[GuildServiceGetGuildMessageId] = RpcMethodMeta{
        "GuildService", "GetGuild",
        std::make_unique<::guildpb::GetGuildRequest>(),
        std::make_unique<::guildpb::GetGuildResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceGetGuildErased, "/guildpb.GuildService/GetGuild"};
    gRpcMethodRegistry[GuildServiceGetPlayerGuildMessageId] = RpcMethodMeta{
        "GuildService", "GetPlayerGuild",
        std::make_unique<::guildpb::GetPlayerGuildRequest>(),
        std::make_unique<::guildpb::GetPlayerGuildResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceGetPlayerGuildErased, "/guildpb.GuildService/GetPlayerGuild"};
    gRpcMethodRegistry[GuildServiceLeaveGuildMessageId] = RpcMethodMeta{
        "GuildService", "LeaveGuild",
        std::make_unique<::guildpb::LeaveGuildRequest>(),
        std::make_unique<::guildpb::LeaveGuildResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceLeaveGuildErased, "/guildpb.GuildService/LeaveGuild"};
    gRpcMethodRegistry[GuildServiceDisbandGuildMessageId] = RpcMethodMeta{
        "GuildService", "DisbandGuild",
        std::make_unique<::guildpb::DisbandGuildRequest>(),
        std::make_unique<::guildpb::DisbandGuildResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceDisbandGuildErased, "/guildpb.GuildService/DisbandGuild"};
    gRpcMethodRegistry[GuildServiceSetAnnouncementMessageId] = RpcMethodMeta{
        "GuildService", "SetAnnouncement",
        std::make_unique<::guildpb::SetAnnouncementRequest>(),
        std::make_unique<::guildpb::SetAnnouncementResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceSetAnnouncementErased, "/guildpb.GuildService/SetAnnouncement"};
    gRpcMethodRegistry[GuildServiceSetGuildMemberRoleMessageId] = RpcMethodMeta{
        "GuildService", "SetGuildMemberRole",
        std::make_unique<::guildpb::SetGuildMemberRoleRequest>(),
        std::make_unique<::guildpb::SetGuildMemberRoleResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceSetGuildMemberRoleErased, "/guildpb.GuildService/SetGuildMemberRole"};
    gRpcMethodRegistry[GuildServiceKickGuildMemberMessageId] = RpcMethodMeta{
        "GuildService", "KickGuildMember",
        std::make_unique<::guildpb::KickGuildMemberRequest>(),
        std::make_unique<::guildpb::KickGuildMemberResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceKickGuildMemberErased, "/guildpb.GuildService/KickGuildMember"};
    gRpcMethodRegistry[GuildServiceTransferGuildLeaderMessageId] = RpcMethodMeta{
        "GuildService", "TransferGuildLeader",
        std::make_unique<::guildpb::TransferGuildLeaderRequest>(),
        std::make_unique<::guildpb::TransferGuildLeaderResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceTransferGuildLeaderErased, "/guildpb.GuildService/TransferGuildLeader"};
    gRpcMethodRegistry[GuildServiceApplyJoinGuildMessageId] = RpcMethodMeta{
        "GuildService", "ApplyJoinGuild",
        std::make_unique<::guildpb::ApplyJoinGuildRequest>(),
        std::make_unique<::guildpb::ApplyJoinGuildResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceApplyJoinGuildErased, "/guildpb.GuildService/ApplyJoinGuild"};
    gRpcMethodRegistry[GuildServiceCancelGuildApplicationMessageId] = RpcMethodMeta{
        "GuildService", "CancelGuildApplication",
        std::make_unique<::guildpb::CancelGuildApplicationRequest>(),
        std::make_unique<::guildpb::CancelGuildApplicationResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceCancelGuildApplicationErased, "/guildpb.GuildService/CancelGuildApplication"};
    gRpcMethodRegistry[GuildServiceListMyGuildApplicationsMessageId] = RpcMethodMeta{
        "GuildService", "ListMyGuildApplications",
        std::make_unique<::guildpb::ListMyGuildApplicationsRequest>(),
        std::make_unique<::guildpb::ListMyGuildApplicationsResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceListMyGuildApplicationsErased, "/guildpb.GuildService/ListMyGuildApplications"};
    gRpcMethodRegistry[GuildServiceListGuildApplicationsMessageId] = RpcMethodMeta{
        "GuildService", "ListGuildApplications",
        std::make_unique<::guildpb::ListGuildApplicationsRequest>(),
        std::make_unique<::guildpb::ListGuildApplicationsResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceListGuildApplicationsErased, "/guildpb.GuildService/ListGuildApplications"};
    gRpcMethodRegistry[GuildServiceReviewGuildApplicationMessageId] = RpcMethodMeta{
        "GuildService", "ReviewGuildApplication",
        std::make_unique<::guildpb::ReviewGuildApplicationRequest>(),
        std::make_unique<::guildpb::ReviewGuildApplicationResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceReviewGuildApplicationErased, "/guildpb.GuildService/ReviewGuildApplication"};
    gRpcMethodRegistry[GuildServiceNotifyGuildChangedMessageId] = RpcMethodMeta{
        "GuildService", "NotifyGuildChanged",
        std::make_unique<::guildpb::GuildChangedS2C>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceNotifyGuildChangedErased, "/guildpb.GuildService/NotifyGuildChanged"};
    gRpcMethodRegistry[GuildServiceUpdateGuildScoreMessageId] = RpcMethodMeta{
        "GuildService", "UpdateGuildScore",
        std::make_unique<::guildpb::UpdateGuildScoreRequest>(),
        std::make_unique<::guildpb::UpdateGuildScoreResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceUpdateGuildScoreErased, "/guildpb.GuildService/UpdateGuildScore"};
    gRpcMethodRegistry[GuildServiceGetGuildRankMessageId] = RpcMethodMeta{
        "GuildService", "GetGuildRank",
        std::make_unique<::guildpb::GetGuildRankRequest>(),
        std::make_unique<::guildpb::GetGuildRankResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceGetGuildRankErased, "/guildpb.GuildService/GetGuildRank"};
    gRpcMethodRegistry[GuildServiceGetGuildRankByGuildMessageId] = RpcMethodMeta{
        "GuildService", "GetGuildRankByGuild",
        std::make_unique<::guildpb::GetGuildRankByGuildRequest>(),
        std::make_unique<::guildpb::GetGuildRankByGuildResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceGetGuildRankByGuildErased, "/guildpb.GuildService/GetGuildRankByGuild"};
    gRpcMethodRegistry[GuildServiceGetGuildDonateOptionsMessageId] = RpcMethodMeta{
        "GuildService", "GetGuildDonateOptions",
        std::make_unique<::guildpb::GetGuildDonateOptionsRequest>(),
        std::make_unique<::guildpb::GetGuildDonateOptionsResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceGetGuildDonateOptionsErased, "/guildpb.GuildService/GetGuildDonateOptions"};
    gRpcMethodRegistry[GuildServiceDonateToGuildMessageId] = RpcMethodMeta{
        "GuildService", "DonateToGuild",
        std::make_unique<::guildpb::DonateToGuildRequest>(),
        std::make_unique<::guildpb::DonateToGuildResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceDonateToGuildErased, "/guildpb.GuildService/DonateToGuild"};
    gRpcMethodRegistry[GuildServiceUpgradeGuildMessageId] = RpcMethodMeta{
        "GuildService", "UpgradeGuild",
        std::make_unique<::guildpb::UpgradeGuildRequest>(),
        std::make_unique<::guildpb::UpgradeGuildResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceUpgradeGuildErased, "/guildpb.GuildService/UpgradeGuild"};
    gRpcMethodRegistry[GuildServiceGetGuildShopMessageId] = RpcMethodMeta{
        "GuildService", "GetGuildShop",
        std::make_unique<::guildpb::GetGuildShopRequest>(),
        std::make_unique<::guildpb::GetGuildShopResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceGetGuildShopErased, "/guildpb.GuildService/GetGuildShop"};
    gRpcMethodRegistry[GuildServiceBuyGuildShopGoodsMessageId] = RpcMethodMeta{
        "GuildService", "BuyGuildShopGoods",
        std::make_unique<::guildpb::BuyGuildShopGoodsRequest>(),
        std::make_unique<::guildpb::BuyGuildShopGoodsResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceBuyGuildShopGoodsErased, "/guildpb.GuildService/BuyGuildShopGoods"};
    gRpcMethodRegistry[GuildServiceGetGuildActivitiesMessageId] = RpcMethodMeta{
        "GuildService", "GetGuildActivities",
        std::make_unique<::guildpb::GetGuildActivitiesRequest>(),
        std::make_unique<::guildpb::GetGuildActivitiesResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceGetGuildActivitiesErased, "/guildpb.GuildService/GetGuildActivities"};
    gRpcMethodRegistry[GuildServiceLightGuildLanternMessageId] = RpcMethodMeta{
        "GuildService", "LightGuildLantern",
        std::make_unique<::guildpb::LightGuildLanternRequest>(),
        std::make_unique<::guildpb::LightGuildLanternResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceLightGuildLanternErased, "/guildpb.GuildService/LightGuildLantern"};
    gRpcMethodRegistry[GuildServiceClaimGuildReunionMessageId] = RpcMethodMeta{
        "GuildService", "ClaimGuildReunion",
        std::make_unique<::guildpb::ClaimGuildReunionRequest>(),
        std::make_unique<::guildpb::ClaimGuildReunionResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceClaimGuildReunionErased, "/guildpb.GuildService/ClaimGuildReunion"};
    gRpcMethodRegistry[GuildServiceStartGuildTrialMessageId] = RpcMethodMeta{
        "GuildService", "StartGuildTrial",
        std::make_unique<::guildpb::StartGuildTrialRequest>(),
        std::make_unique<::guildpb::StartGuildTrialResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceStartGuildTrialErased, "/guildpb.GuildService/StartGuildTrial"};
    gRpcMethodRegistry[GuildServiceRespondGuildTrialInviteMessageId] = RpcMethodMeta{
        "GuildService", "RespondGuildTrialInvite",
        std::make_unique<::guildpb::RespondGuildTrialInviteRequest>(),
        std::make_unique<::guildpb::RespondGuildTrialInviteResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildServiceRespondGuildTrialInviteErased, "/guildpb.GuildService/RespondGuildTrialInvite"};

    // --- GuildInternal ---
    gRpcMethodRegistry[GuildInternalListAppliedAssetOpsSinceMessageId] = RpcMethodMeta{
        "GuildInternal", "ListAppliedAssetOpsSince",
        std::make_unique<::guildpb::ListAppliedAssetOpsSinceRequest>(),
        std::make_unique<::guildpb::ListAppliedAssetOpsSinceResponse>(),
        1, common::base::eNodeType::GuildNodeService, guildpb::SendGuildInternalListAppliedAssetOpsSinceErased, "/guildpb.GuildInternal/ListAppliedAssetOpsSince"};

    // --- ClientPlayerLogin ---
    gRpcMethodRegistry[ClientPlayerLoginLoginMessageId] = RpcMethodMeta{
        "ClientPlayerLogin", "Login",
        std::make_unique<::loginpb::LoginRequest>(),
        std::make_unique<::loginpb::LoginResponse>(),
        1, common::base::eNodeType::LoginNodeService, loginpb::SendClientPlayerLoginLoginErased, "/loginpb.ClientPlayerLogin/Login"};
    gRpcMethodRegistry[ClientPlayerLoginCreatePlayerMessageId] = RpcMethodMeta{
        "ClientPlayerLogin", "CreatePlayer",
        std::make_unique<::loginpb::CreatePlayerRequest>(),
        std::make_unique<::loginpb::CreatePlayerResponse>(),
        1, common::base::eNodeType::LoginNodeService, loginpb::SendClientPlayerLoginCreatePlayerErased, "/loginpb.ClientPlayerLogin/CreatePlayer"};
    gRpcMethodRegistry[ClientPlayerLoginEnterGameMessageId] = RpcMethodMeta{
        "ClientPlayerLogin", "EnterGame",
        std::make_unique<::loginpb::EnterGameRequest>(),
        std::make_unique<::loginpb::EnterGameResponse>(),
        1, common::base::eNodeType::LoginNodeService, loginpb::SendClientPlayerLoginEnterGameErased, "/loginpb.ClientPlayerLogin/EnterGame"};
    gRpcMethodRegistry[ClientPlayerLoginLeaveGameMessageId] = RpcMethodMeta{
        "ClientPlayerLogin", "LeaveGame",
        std::make_unique<::loginpb::LeaveGameRequest>(),
        std::make_unique<::loginpb::LoginEmptyResponse>(),
        1, common::base::eNodeType::LoginNodeService, loginpb::SendClientPlayerLoginLeaveGameErased, "/loginpb.ClientPlayerLogin/LeaveGame"};
    gRpcMethodRegistry[ClientPlayerLoginDisconnectMessageId] = RpcMethodMeta{
        "ClientPlayerLogin", "Disconnect",
        std::make_unique<::loginpb::LoginNodeDisconnectRequest>(),
        std::make_unique<::loginpb::LoginEmptyResponse>(),
        1, common::base::eNodeType::LoginNodeService, loginpb::SendClientPlayerLoginDisconnectErased, "/loginpb.ClientPlayerLogin/Disconnect"};
    gRpcMethodRegistry[ClientPlayerLoginRefreshTokenMessageId] = RpcMethodMeta{
        "ClientPlayerLogin", "RefreshToken",
        std::make_unique<::loginpb::RefreshTokenRequest>(),
        std::make_unique<::loginpb::RefreshTokenResponse>(),
        1, common::base::eNodeType::LoginNodeService, loginpb::SendClientPlayerLoginRefreshTokenErased, "/loginpb.ClientPlayerLogin/RefreshToken"};
    gRpcMethodRegistry[ClientPlayerLoginRequestGateMigrationMessageId] = RpcMethodMeta{
        "ClientPlayerLogin", "RequestGateMigration",
        std::make_unique<::loginpb::GateMigrationRequest>(),
        std::make_unique<::loginpb::GateMigrationResponse>(),
        1, common::base::eNodeType::LoginNodeService, loginpb::SendClientPlayerLoginRequestGateMigrationErased, "/loginpb.ClientPlayerLogin/RequestGateMigration"};

    // --- LoginPreGate ---
    gRpcMethodRegistry[LoginPreGateAssignGateMessageId] = RpcMethodMeta{
        "LoginPreGate", "AssignGate",
        std::make_unique<::loginpb::AssignGateRequest>(),
        std::make_unique<::loginpb::AssignGateResponse>(),
        1, common::base::eNodeType::LoginNodeService, loginpb::SendLoginPreGateAssignGateErased, "/loginpb.LoginPreGate/AssignGate"};
    gRpcMethodRegistry[LoginPreGateQueryQueueStatusMessageId] = RpcMethodMeta{
        "LoginPreGate", "QueryQueueStatus",
        std::make_unique<::loginpb::QueryQueueStatusRequest>(),
        std::make_unique<::loginpb::QueryQueueStatusResponse>(),
        1, common::base::eNodeType::LoginNodeService, loginpb::SendLoginPreGateQueryQueueStatusErased, "/loginpb.LoginPreGate/QueryQueueStatus"};

    // --- LoginAdmin ---
    gRpcMethodRegistry[LoginAdminRemovePlayersFromAccountsMessageId] = RpcMethodMeta{
        "LoginAdmin", "RemovePlayersFromAccounts",
        std::make_unique<::loginpb::RemovePlayersFromAccountsRequest>(),
        std::make_unique<::loginpb::RemovePlayersFromAccountsResponse>(),
        1, common::base::eNodeType::LoginNodeService, loginpb::SendLoginAdminRemovePlayersFromAccountsErased, "/loginpb.LoginAdmin/RemovePlayersFromAccounts"};

    // --- ClientPlayerMail ---
    gRpcMethodRegistry[ClientPlayerMailListMailsMessageId] = RpcMethodMeta{
        "ClientPlayerMail", "ListMails",
        std::make_unique<::mailpb::ListMailsRequest>(),
        std::make_unique<::mailpb::ListMailsResponse>(),
        1, common::base::eNodeType::MailNodeService, mailpb::SendClientPlayerMailListMailsErased, "/mailpb.ClientPlayerMail/ListMails"};
    gRpcMethodRegistry[ClientPlayerMailReadMailMessageId] = RpcMethodMeta{
        "ClientPlayerMail", "ReadMail",
        std::make_unique<::mailpb::ReadMailRequest>(),
        std::make_unique<::mailpb::ReadMailResponse>(),
        1, common::base::eNodeType::MailNodeService, mailpb::SendClientPlayerMailReadMailErased, "/mailpb.ClientPlayerMail/ReadMail"};
    gRpcMethodRegistry[ClientPlayerMailClaimMailAttachmentsMessageId] = RpcMethodMeta{
        "ClientPlayerMail", "ClaimMailAttachments",
        std::make_unique<::mailpb::ClaimMailAttachmentsRequest>(),
        std::make_unique<::mailpb::ClaimMailAttachmentsResponse>(),
        1, common::base::eNodeType::MailNodeService, mailpb::SendClientPlayerMailClaimMailAttachmentsErased, "/mailpb.ClientPlayerMail/ClaimMailAttachments"};
    gRpcMethodRegistry[ClientPlayerMailDeleteMailsMessageId] = RpcMethodMeta{
        "ClientPlayerMail", "DeleteMails",
        std::make_unique<::mailpb::DeleteMailsRequest>(),
        std::make_unique<::mailpb::DeleteMailsResponse>(),
        1, common::base::eNodeType::MailNodeService, mailpb::SendClientPlayerMailDeleteMailsErased, "/mailpb.ClientPlayerMail/DeleteMails"};
    gRpcMethodRegistry[ClientPlayerMailClearReadMailsMessageId] = RpcMethodMeta{
        "ClientPlayerMail", "ClearReadMails",
        std::make_unique<::mailpb::ClearReadMailsRequest>(),
        std::make_unique<::mailpb::ClearReadMailsResponse>(),
        1, common::base::eNodeType::MailNodeService, mailpb::SendClientPlayerMailClearReadMailsErased, "/mailpb.ClientPlayerMail/ClearReadMails"};
    gRpcMethodRegistry[ClientPlayerMailNotifyMailChangedMessageId] = RpcMethodMeta{
        "ClientPlayerMail", "NotifyMailChanged",
        std::make_unique<::mailpb::MailChangedEvent>(),
        std::make_unique<::mailpb::MailChangedEvent>(),
        1, common::base::eNodeType::MailNodeService, mailpb::SendClientPlayerMailNotifyMailChangedErased, "/mailpb.ClientPlayerMail/NotifyMailChanged"};

    // --- MailAdmin ---
    gRpcMethodRegistry[MailAdminSendPersonalMailMessageId] = RpcMethodMeta{
        "MailAdmin", "SendPersonalMail",
        std::make_unique<::mailpb::SendMailRequest>(),
        std::make_unique<::mailpb::SendMailResponse>(),
        1, common::base::eNodeType::MailNodeService, mailpb::SendMailAdminSendPersonalMailErased, "/mailpb.MailAdmin/SendPersonalMail"};
    gRpcMethodRegistry[MailAdminSendSystemMailMessageId] = RpcMethodMeta{
        "MailAdmin", "SendSystemMail",
        std::make_unique<::mailpb::SendMailRequest>(),
        std::make_unique<::mailpb::SendMailResponse>(),
        1, common::base::eNodeType::MailNodeService, mailpb::SendMailAdminSendSystemMailErased, "/mailpb.MailAdmin/SendSystemMail"};
    gRpcMethodRegistry[MailAdminSendGuildMailMessageId] = RpcMethodMeta{
        "MailAdmin", "SendGuildMail",
        std::make_unique<::mailpb::SendMailRequest>(),
        std::make_unique<::mailpb::SendMailResponse>(),
        1, common::base::eNodeType::MailNodeService, mailpb::SendMailAdminSendGuildMailErased, "/mailpb.MailAdmin/SendGuildMail"};
    gRpcMethodRegistry[MailAdminImportMailRecipientsMessageId] = RpcMethodMeta{
        "MailAdmin", "ImportMailRecipients",
        std::make_unique<::mailpb::ImportMailRecipientsRequest>(),
        std::make_unique<::mailpb::ImportMailRecipientsResponse>(),
        1, common::base::eNodeType::MailNodeService, mailpb::SendMailAdminImportMailRecipientsErased, "/mailpb.MailAdmin/ImportMailRecipients"};
    gRpcMethodRegistry[MailAdminSealMailCampaignMessageId] = RpcMethodMeta{
        "MailAdmin", "SealMailCampaign",
        std::make_unique<::mailpb::SealMailCampaignRequest>(),
        std::make_unique<::mailpb::SendMailResponse>(),
        1, common::base::eNodeType::MailNodeService, mailpb::SendMailAdminSealMailCampaignErased, "/mailpb.MailAdmin/SealMailCampaign"};

    // --- MatchInternal ---
    gRpcMethodRegistry[MatchInternalStartActivityBattleMessageId] = RpcMethodMeta{
        "MatchInternal", "StartActivityBattle",
        std::make_unique<::match::StartActivityBattleRequest>(),
        std::make_unique<::match::StartActivityBattleResponse>(),
        1, common::base::eNodeType::MatchNodeService, match::SendMatchInternalStartActivityBattleErased, "/match.MatchInternal/StartActivityBattle"};
    gRpcMethodRegistry[MatchInternalCancelActivityBattleMessageId] = RpcMethodMeta{
        "MatchInternal", "CancelActivityBattle",
        std::make_unique<::match::CancelActivityBattleRequest>(),
        std::make_unique<::match::CancelActivityBattleResponse>(),
        1, common::base::eNodeType::MatchNodeService, match::SendMatchInternalCancelActivityBattleErased, "/match.MatchInternal/CancelActivityBattle"};
    gRpcMethodRegistry[MatchInternalCommitBattleOutcomeMessageId] = RpcMethodMeta{
        "MatchInternal", "CommitBattleOutcome",
        std::make_unique<::match::CommitBattleOutcomeRequest>(),
        std::make_unique<::match::CommitBattleOutcomeResponse>(),
        1, common::base::eNodeType::MatchNodeService, match::SendMatchInternalCommitBattleOutcomeErased, "/match.MatchInternal/CommitBattleOutcome"};
    gRpcMethodRegistry[MatchInternalSetSpectateFeaturedMessageId] = RpcMethodMeta{
        "MatchInternal", "SetSpectateFeatured",
        std::make_unique<::match::SetSpectateFeaturedRequest>(),
        std::make_unique<::match::SetSpectateFeaturedResponse>(),
        1, common::base::eNodeType::MatchNodeService, match::SendMatchInternalSetSpectateFeaturedErased, "/match.MatchInternal/SetSpectateFeatured"};
    gRpcMethodRegistry[MatchInternalAcquireTrialOccupancyMessageId] = RpcMethodMeta{
        "MatchInternal", "AcquireTrialOccupancy",
        std::make_unique<::match::AcquireTrialOccupancyRequest>(),
        std::make_unique<::match::AcquireTrialOccupancyResponse>(),
        1, common::base::eNodeType::MatchNodeService, match::SendMatchInternalAcquireTrialOccupancyErased, "/match.MatchInternal/AcquireTrialOccupancy"};
    gRpcMethodRegistry[MatchInternalReleaseTrialOccupancyMessageId] = RpcMethodMeta{
        "MatchInternal", "ReleaseTrialOccupancy",
        std::make_unique<::match::ReleaseTrialOccupancyRequest>(),
        std::make_unique<::match::ReleaseTrialOccupancyResponse>(),
        1, common::base::eNodeType::MatchNodeService, match::SendMatchInternalReleaseTrialOccupancyErased, "/match.MatchInternal/ReleaseTrialOccupancy"};
    gRpcMethodRegistry[MatchInternalGetBattleCompletionMessageId] = RpcMethodMeta{
        "MatchInternal", "GetBattleCompletion",
        std::make_unique<::match::GetBattleCompletionRequest>(),
        std::make_unique<::match::GetBattleCompletionResponse>(),
        1, common::base::eNodeType::MatchNodeService, match::SendMatchInternalGetBattleCompletionErased, "/match.MatchInternal/GetBattleCompletion"};
    gRpcMethodRegistry[MatchInternalGetBattleTerminalDecisionMessageId] = RpcMethodMeta{
        "MatchInternal", "GetBattleTerminalDecision",
        std::make_unique<::match::GetBattleTerminalDecisionRequest>(),
        std::make_unique<::match::GetBattleTerminalDecisionResponse>(),
        1, common::base::eNodeType::MatchNodeService, match::SendMatchInternalGetBattleTerminalDecisionErased, "/match.MatchInternal/GetBattleTerminalDecision"};
    gRpcMethodRegistry[MatchInternalListStrandedFormationsMessageId] = RpcMethodMeta{
        "MatchInternal", "ListStrandedFormations",
        std::make_unique<::match::ListStrandedFormationsRequest>(),
        std::make_unique<::match::ListStrandedFormationsResponse>(),
        1, common::base::eNodeType::MatchNodeService, match::SendMatchInternalListStrandedFormationsErased, "/match.MatchInternal/ListStrandedFormations"};
    gRpcMethodRegistry[MatchInternalResolveStrandedFormationMessageId] = RpcMethodMeta{
        "MatchInternal", "ResolveStrandedFormation",
        std::make_unique<::match::ResolveStrandedFormationRequest>(),
        std::make_unique<::match::ResolveStrandedFormationResponse>(),
        1, common::base::eNodeType::MatchNodeService, match::SendMatchInternalResolveStrandedFormationErased, "/match.MatchInternal/ResolveStrandedFormation"};

    // --- MatchService ---
    gRpcMethodRegistry[MatchServiceGetNetworkProbePlanMessageId] = RpcMethodMeta{
        "MatchService", "GetNetworkProbePlan",
        std::make_unique<::match::GetNetworkProbePlanRequest>(),
        std::make_unique<::match::GetNetworkProbePlanResponse>(),
        1, common::base::eNodeType::MatchNodeService, match::SendMatchServiceGetNetworkProbePlanErased, "/match.MatchService/GetNetworkProbePlan"};
    gRpcMethodRegistry[MatchServiceCommitNetworkProbeMessageId] = RpcMethodMeta{
        "MatchService", "CommitNetworkProbe",
        std::make_unique<::match::CommitNetworkProbeRequest>(),
        std::make_unique<::match::CommitNetworkProbeResponse>(),
        1, common::base::eNodeType::MatchNodeService, match::SendMatchServiceCommitNetworkProbeErased, "/match.MatchService/CommitNetworkProbe"};
    gRpcMethodRegistry[MatchServiceJoinQueueMessageId] = RpcMethodMeta{
        "MatchService", "JoinQueue",
        std::make_unique<::match::JoinQueueRequest>(),
        std::make_unique<::match::JoinQueueResponse>(),
        1, common::base::eNodeType::MatchNodeService, match::SendMatchServiceJoinQueueErased, "/match.MatchService/JoinQueue"};
    gRpcMethodRegistry[MatchServiceCancelQueueMessageId] = RpcMethodMeta{
        "MatchService", "CancelQueue",
        std::make_unique<::match::CancelQueueRequest>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::MatchNodeService, match::SendMatchServiceCancelQueueErased, "/match.MatchService/CancelQueue"};
    gRpcMethodRegistry[MatchServiceGetQueueStatusMessageId] = RpcMethodMeta{
        "MatchService", "GetQueueStatus",
        std::make_unique<::match::GetQueueStatusRequest>(),
        std::make_unique<::match::GetQueueStatusResponse>(),
        1, common::base::eNodeType::MatchNodeService, match::SendMatchServiceGetQueueStatusErased, "/match.MatchService/GetQueueStatus"};
    gRpcMethodRegistry[MatchServiceChallengePlayerMessageId] = RpcMethodMeta{
        "MatchService", "ChallengePlayer",
        std::make_unique<::match::ChallengePlayerRequest>(),
        std::make_unique<::match::ChallengePlayerResponse>(),
        1, common::base::eNodeType::MatchNodeService, match::SendMatchServiceChallengePlayerErased, "/match.MatchService/ChallengePlayer"};
    gRpcMethodRegistry[MatchServiceRespondChallengeMessageId] = RpcMethodMeta{
        "MatchService", "RespondChallenge",
        std::make_unique<::match::RespondChallengeRequest>(),
        std::make_unique<::match::RespondChallengeResponse>(),
        1, common::base::eNodeType::MatchNodeService, match::SendMatchServiceRespondChallengeErased, "/match.MatchService/RespondChallenge"};
    gRpcMethodRegistry[MatchServiceNotifyChallengeInviteMessageId] = RpcMethodMeta{
        "MatchService", "NotifyChallengeInvite",
        std::make_unique<::match::ChallengeInviteS2C>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::MatchNodeService, match::SendMatchServiceNotifyChallengeInviteErased, "/match.MatchService/NotifyChallengeInvite"};
    gRpcMethodRegistry[MatchServiceNotifyChallengeResultMessageId] = RpcMethodMeta{
        "MatchService", "NotifyChallengeResult",
        std::make_unique<::match::ChallengeResultS2C>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::MatchNodeService, match::SendMatchServiceNotifyChallengeResultErased, "/match.MatchService/NotifyChallengeResult"};
    gRpcMethodRegistry[MatchServiceWatchBattleMessageId] = RpcMethodMeta{
        "MatchService", "WatchBattle",
        std::make_unique<::match::WatchBattleRequest>(),
        std::make_unique<::match::WatchBattleResponse>(),
        1, common::base::eNodeType::MatchNodeService, match::SendMatchServiceWatchBattleErased, "/match.MatchService/WatchBattle"};
    gRpcMethodRegistry[MatchServiceRequestBattleTicketMessageId] = RpcMethodMeta{
        "MatchService", "RequestBattleTicket",
        std::make_unique<::RequestBattleTicketRequest>(),
        std::make_unique<::RequestBattleTicketResponse>(),
        1, common::base::eNodeType::MatchNodeService, match::SendMatchServiceRequestBattleTicketErased, "/match.MatchService/RequestBattleTicket"};
    gRpcMethodRegistry[MatchServiceListWatchableBattlesMessageId] = RpcMethodMeta{
        "MatchService", "ListWatchableBattles",
        std::make_unique<::match::ListWatchableBattlesRequest>(),
        std::make_unique<::match::ListWatchableBattlesResponse>(),
        1, common::base::eNodeType::MatchNodeService, match::SendMatchServiceListWatchableBattlesErased, "/match.MatchService/ListWatchableBattles"};
    gRpcMethodRegistry[MatchServiceListBattleReportsMessageId] = RpcMethodMeta{
        "MatchService", "ListBattleReports",
        std::make_unique<::match::ListBattleReportsRequest>(),
        std::make_unique<::match::ListBattleReportsResponse>(),
        1, common::base::eNodeType::MatchNodeService, match::SendMatchServiceListBattleReportsErased, "/match.MatchService/ListBattleReports"};
    gRpcMethodRegistry[MatchServiceGetBattleReportMessageId] = RpcMethodMeta{
        "MatchService", "GetBattleReport",
        std::make_unique<::match::GetBattleReportRequest>(),
        std::make_unique<::match::GetBattleReportResponse>(),
        1, common::base::eNodeType::MatchNodeService, match::SendMatchServiceGetBattleReportErased, "/match.MatchService/GetBattleReport"};

    // --- PlayerInbox ---
    gRpcMethodRegistry[PlayerInboxFetchItemsMessageId] = RpcMethodMeta{
        "PlayerInbox", "FetchItems",
        std::make_unique<::player_inbox::FetchPlayerInboxRequest>(),
        std::make_unique<::player_inbox::FetchPlayerInboxResponse>(),
        1, common::base::eNodeType::PlayerInboxNodeService, player_inbox::SendPlayerInboxFetchItemsErased, "/player_inbox.PlayerInbox/FetchItems"};
    gRpcMethodRegistry[PlayerInboxAckItemsMessageId] = RpcMethodMeta{
        "PlayerInbox", "AckItems",
        std::make_unique<::player_inbox::AckPlayerInboxRequest>(),
        std::make_unique<::player_inbox::AckPlayerInboxResponse>(),
        1, common::base::eNodeType::PlayerInboxNodeService, player_inbox::SendPlayerInboxAckItemsErased, "/player_inbox.PlayerInbox/AckItems"};
    gRpcMethodRegistry[PlayerInboxEnqueueBattleSettlementMessageId] = RpcMethodMeta{
        "PlayerInbox", "EnqueueBattleSettlement",
        std::make_unique<::player_inbox::BattleSettlementInboxRequest>(),
        std::make_unique<::player_inbox::BattleSettlementInboxResponse>(),
        1, common::base::eNodeType::PlayerInboxNodeService, player_inbox::SendPlayerInboxEnqueueBattleSettlementErased, "/player_inbox.PlayerInbox/EnqueueBattleSettlement"};
    gRpcMethodRegistry[PlayerInboxLookupBattleSettlementMessageId] = RpcMethodMeta{
        "PlayerInbox", "LookupBattleSettlement",
        std::make_unique<::player_inbox::BattleSettlementInboxRequest>(),
        std::make_unique<::player_inbox::BattleSettlementInboxResponse>(),
        1, common::base::eNodeType::PlayerInboxNodeService, player_inbox::SendPlayerInboxLookupBattleSettlementErased, "/player_inbox.PlayerInbox/LookupBattleSettlement"};

    // --- PlayerInboxAdmin ---
    gRpcMethodRegistry[PlayerInboxAdminGmSanctionPlayerMessageId] = RpcMethodMeta{
        "PlayerInboxAdmin", "GmSanctionPlayer",
        std::make_unique<::player_inbox::GmSanctionPlayerRequest>(),
        std::make_unique<::player_inbox::GmSanctionPlayerResponse>(),
        1, common::base::eNodeType::PlayerInboxNodeService, player_inbox::SendPlayerInboxAdminGmSanctionPlayerErased, "/player_inbox.PlayerInboxAdmin/GmSanctionPlayer"};
    gRpcMethodRegistry[PlayerInboxAdminGetSummaryMessageId] = RpcMethodMeta{
        "PlayerInboxAdmin", "GetSummary",
        std::make_unique<::player_inbox::GetPlayerInboxSummaryRequest>(),
        std::make_unique<::player_inbox::GetPlayerInboxSummaryResponse>(),
        1, common::base::eNodeType::PlayerInboxNodeService, player_inbox::SendPlayerInboxAdminGetSummaryErased, "/player_inbox.PlayerInboxAdmin/GetSummary"};
    gRpcMethodRegistry[PlayerInboxAdminListItemsMessageId] = RpcMethodMeta{
        "PlayerInboxAdmin", "ListItems",
        std::make_unique<::player_inbox::ListPlayerInboxItemsRequest>(),
        std::make_unique<::player_inbox::ListPlayerInboxItemsResponse>(),
        1, common::base::eNodeType::PlayerInboxNodeService, player_inbox::SendPlayerInboxAdminListItemsErased, "/player_inbox.PlayerInboxAdmin/ListItems"};
    gRpcMethodRegistry[PlayerInboxAdminLookupByRequestMessageId] = RpcMethodMeta{
        "PlayerInboxAdmin", "LookupByRequest",
        std::make_unique<::player_inbox::LookupPlayerInboxRequest>(),
        std::make_unique<::player_inbox::LookupPlayerInboxResponse>(),
        1, common::base::eNodeType::PlayerInboxNodeService, player_inbox::SendPlayerInboxAdminLookupByRequestErased, "/player_inbox.PlayerInboxAdmin/LookupByRequest"};
    gRpcMethodRegistry[PlayerInboxAdminVoidItemMessageId] = RpcMethodMeta{
        "PlayerInboxAdmin", "VoidItem",
        std::make_unique<::player_inbox::VoidPlayerInboxItemRequest>(),
        std::make_unique<::player_inbox::VoidPlayerInboxItemResponse>(),
        1, common::base::eNodeType::PlayerInboxNodeService, player_inbox::SendPlayerInboxAdminVoidItemErased, "/player_inbox.PlayerInboxAdmin/VoidItem"};
    gRpcMethodRegistry[PlayerInboxAdminReconcilePlayerMessageId] = RpcMethodMeta{
        "PlayerInboxAdmin", "ReconcilePlayer",
        std::make_unique<::player_inbox::ReconcilePlayerInboxRequest>(),
        std::make_unique<::player_inbox::ReconcilePlayerInboxResponse>(),
        1, common::base::eNodeType::PlayerInboxNodeService, player_inbox::SendPlayerInboxAdminReconcilePlayerErased, "/player_inbox.PlayerInboxAdmin/ReconcilePlayer"};

    // --- PlayerLocator ---
    gRpcMethodRegistry[PlayerLocatorSetLocationMessageId] = RpcMethodMeta{
        "PlayerLocator", "SetLocation",
        std::make_unique<::playerlocator::PlayerLocation>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::PlayerLocatorNodeService, playerlocator::SendPlayerLocatorSetLocationErased, "/playerlocator.PlayerLocator/SetLocation"};
    gRpcMethodRegistry[PlayerLocatorGetLocationMessageId] = RpcMethodMeta{
        "PlayerLocator", "GetLocation",
        std::make_unique<::playerlocator::PlayerId>(),
        std::make_unique<::playerlocator::PlayerLocation>(),
        1, common::base::eNodeType::PlayerLocatorNodeService, playerlocator::SendPlayerLocatorGetLocationErased, "/playerlocator.PlayerLocator/GetLocation"};
    gRpcMethodRegistry[PlayerLocatorMarkOfflineMessageId] = RpcMethodMeta{
        "PlayerLocator", "MarkOffline",
        std::make_unique<::playerlocator::MarkOfflineRequest>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::PlayerLocatorNodeService, playerlocator::SendPlayerLocatorMarkOfflineErased, "/playerlocator.PlayerLocator/MarkOffline"};
    gRpcMethodRegistry[PlayerLocatorSetSessionMessageId] = RpcMethodMeta{
        "PlayerLocator", "SetSession",
        std::make_unique<::playerlocator::SetSessionRequest>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::PlayerLocatorNodeService, playerlocator::SendPlayerLocatorSetSessionErased, "/playerlocator.PlayerLocator/SetSession"};
    gRpcMethodRegistry[PlayerLocatorGetSessionMessageId] = RpcMethodMeta{
        "PlayerLocator", "GetSession",
        std::make_unique<::playerlocator::GetSessionRequest>(),
        std::make_unique<::playerlocator::GetSessionResponse>(),
        1, common::base::eNodeType::PlayerLocatorNodeService, playerlocator::SendPlayerLocatorGetSessionErased, "/playerlocator.PlayerLocator/GetSession"};
    gRpcMethodRegistry[PlayerLocatorSetDisconnectingMessageId] = RpcMethodMeta{
        "PlayerLocator", "SetDisconnecting",
        std::make_unique<::playerlocator::SetDisconnectingRequest>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::PlayerLocatorNodeService, playerlocator::SendPlayerLocatorSetDisconnectingErased, "/playerlocator.PlayerLocator/SetDisconnecting"};
    gRpcMethodRegistry[PlayerLocatorReconnectMessageId] = RpcMethodMeta{
        "PlayerLocator", "Reconnect",
        std::make_unique<::playerlocator::ReconnectRequest>(),
        std::make_unique<::playerlocator::ReconnectResponse>(),
        1, common::base::eNodeType::PlayerLocatorNodeService, playerlocator::SendPlayerLocatorReconnectErased, "/playerlocator.PlayerLocator/Reconnect"};

    // --- PlayerStore ---
    gRpcMethodRegistry[PlayerStoreLoadAndClaimMessageId] = RpcMethodMeta{
        "PlayerStore", "LoadAndClaim",
        std::make_unique<::playerstore::LoadAndClaimRequest>(),
        std::make_unique<::playerstore::LoadAndClaimResponse>(),
        1, common::base::eNodeType::PlayerStoreNodeService, playerstore::SendPlayerStoreLoadAndClaimErased, "/playerstore.PlayerStore/LoadAndClaim"};
    gRpcMethodRegistry[PlayerStoreSavePlayerMessageId] = RpcMethodMeta{
        "PlayerStore", "SavePlayer",
        std::make_unique<::playerstore::SavePlayerRequest>(),
        std::make_unique<::playerstore::SavePlayerResponse>(),
        1, common::base::eNodeType::PlayerStoreNodeService, playerstore::SendPlayerStoreSavePlayerErased, "/playerstore.PlayerStore/SavePlayer"};

    // --- ClientPlayerRank ---
    gRpcMethodRegistry[ClientPlayerRankGetRankPageMessageId] = RpcMethodMeta{
        "ClientPlayerRank", "GetRankPage",
        std::make_unique<::rankpb::GetRankPageRequest>(),
        std::make_unique<::rankpb::GetRankPageResponse>(),
        1, common::base::eNodeType::RankNodeService, rankpb::SendClientPlayerRankGetRankPageErased, "/rankpb.ClientPlayerRank/GetRankPage"};
    gRpcMethodRegistry[ClientPlayerRankGetRankAroundMessageId] = RpcMethodMeta{
        "ClientPlayerRank", "GetRankAround",
        std::make_unique<::rankpb::GetRankAroundRequest>(),
        std::make_unique<::rankpb::GetRankAroundResponse>(),
        1, common::base::eNodeType::RankNodeService, rankpb::SendClientPlayerRankGetRankAroundErased, "/rankpb.ClientPlayerRank/GetRankAround"};

    // --- RankAdmin ---
    gRpcMethodRegistry[RankAdminOpenRankGenerationMessageId] = RpcMethodMeta{
        "RankAdmin", "OpenRankGeneration",
        std::make_unique<::rankpb::OpenRankGenerationRequest>(),
        std::make_unique<::rankpb::OpenRankGenerationResponse>(),
        1, common::base::eNodeType::RankNodeService, rankpb::SendRankAdminOpenRankGenerationErased, "/rankpb.RankAdmin/OpenRankGeneration"};
    gRpcMethodRegistry[RankAdminSubmitRankScoresMessageId] = RpcMethodMeta{
        "RankAdmin", "SubmitRankScores",
        std::make_unique<::rankpb::SubmitRankScoresRequest>(),
        std::make_unique<::rankpb::SubmitRankScoresResponse>(),
        1, common::base::eNodeType::RankNodeService, rankpb::SendRankAdminSubmitRankScoresErased, "/rankpb.RankAdmin/SubmitRankScores"};
    gRpcMethodRegistry[RankAdminFreezeRankGenerationMessageId] = RpcMethodMeta{
        "RankAdmin", "FreezeRankGeneration",
        std::make_unique<::rankpb::FreezeRankGenerationRequest>(),
        std::make_unique<::rankpb::FreezeRankGenerationResponse>(),
        1, common::base::eNodeType::RankNodeService, rankpb::SendRankAdminFreezeRankGenerationErased, "/rankpb.RankAdmin/FreezeRankGeneration"};
    gRpcMethodRegistry[RankAdminRemoveRankEntryMessageId] = RpcMethodMeta{
        "RankAdmin", "RemoveRankEntry",
        std::make_unique<::rankpb::RemoveRankEntryRequest>(),
        std::make_unique<::rankpb::RankAdminResponse>(),
        1, common::base::eNodeType::RankNodeService, rankpb::SendRankAdminRemoveRankEntryErased, "/rankpb.RankAdmin/RemoveRankEntry"};
    gRpcMethodRegistry[RankAdminDeleteRankGenerationMessageId] = RpcMethodMeta{
        "RankAdmin", "DeleteRankGeneration",
        std::make_unique<::rankpb::RemoveRankEntryRequest>(),
        std::make_unique<::rankpb::RankAdminResponse>(),
        1, common::base::eNodeType::RankNodeService, rankpb::SendRankAdminDeleteRankGenerationErased, "/rankpb.RankAdmin/DeleteRankGeneration"};

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
    gRpcMethodRegistry[SceneBagClientPlayerRetrieveTemporaryItemsMessageId] = RpcMethodMeta{
        "SceneBagClientPlayer", "RetrieveTemporaryItems",
        std::make_unique<::RetrieveTemporaryItemsRequest>(),
        std::make_unique<::RetrieveTemporaryItemsResponse>(),
        0, common::base::eNodeType::SceneNodeService};

    // --- SceneBattleResultClientPlayer ---
    gRpcMethodRegistry[SceneBattleResultClientPlayerGetRecentBattleResultsMessageId] = RpcMethodMeta{
        "SceneBattleResultClientPlayer", "GetRecentBattleResults",
        std::make_unique<::GetRecentBattleResultsRequest>(),
        std::make_unique<::GetRecentBattleResultsResponse>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneBattleResultClientPlayerAckBattleResultsMessageId] = RpcMethodMeta{
        "SceneBattleResultClientPlayer", "AckBattleResults",
        std::make_unique<::AckBattleResultsRequest>(),
        std::make_unique<::AckBattleResultsResponse>(),
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
    gRpcMethodRegistry[SceneMovementClientPlayerTeamFollowMessageId] = RpcMethodMeta{
        "SceneMovementClientPlayer", "TeamFollow",
        std::make_unique<::TeamFollowC2S>(),
        std::make_unique<::Empty>(),
        0, common::base::eNodeType::SceneNodeService};
    gRpcMethodRegistry[SceneMovementClientPlayerNotifyTeamFollowStateMessageId] = RpcMethodMeta{
        "SceneMovementClientPlayer", "NotifyTeamFollowState",
        std::make_unique<::TeamFollowStateS2C>(),
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
        1, common::base::eNodeType::SceneManagerNodeService, scene_manager::SendSceneManagerCreateSceneErased, "/scene_manager.SceneManager/CreateScene"};
    gRpcMethodRegistry[SceneManagerDestroySceneMessageId] = RpcMethodMeta{
        "SceneManager", "DestroyScene",
        std::make_unique<::scene_manager::DestroySceneRequest>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::SceneManagerNodeService, scene_manager::SendSceneManagerDestroySceneErased, "/scene_manager.SceneManager/DestroyScene"};
    gRpcMethodRegistry[SceneManagerEnterSceneMessageId] = RpcMethodMeta{
        "SceneManager", "EnterScene",
        std::make_unique<::scene_manager::EnterSceneRequest>(),
        std::make_unique<::scene_manager::EnterSceneResponse>(),
        1, common::base::eNodeType::SceneManagerNodeService, scene_manager::SendSceneManagerEnterSceneErased, "/scene_manager.SceneManager/EnterScene"};
    gRpcMethodRegistry[SceneManagerLeaveSceneMessageId] = RpcMethodMeta{
        "SceneManager", "LeaveScene",
        std::make_unique<::scene_manager::LeaveSceneRequest>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::SceneManagerNodeService, scene_manager::SendSceneManagerLeaveSceneErased, "/scene_manager.SceneManager/LeaveScene"};

    // --- SceneNodeGrpc ---
    gRpcMethodRegistry[SceneNodeGrpcCreateSceneMessageId] = RpcMethodMeta{
        "SceneNodeGrpc", "CreateScene",
        std::make_unique<::CreateSceneRequest>(),
        std::make_unique<::CreateSceneResponse>(),
        1, common::base::eNodeType::SceneManagerNodeService, scene_node::SendSceneNodeGrpcCreateSceneErased, "/scene_node.SceneNodeGrpc/CreateScene"};
    gRpcMethodRegistry[SceneNodeGrpcDestroySceneMessageId] = RpcMethodMeta{
        "SceneNodeGrpc", "DestroyScene",
        std::make_unique<::DestroySceneRequest>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::SceneManagerNodeService, scene_node::SendSceneNodeGrpcDestroySceneErased, "/scene_node.SceneNodeGrpc/DestroyScene"};
    gRpcMethodRegistry[SceneNodeGrpcReleasePlayerMessageId] = RpcMethodMeta{
        "SceneNodeGrpc", "ReleasePlayer",
        std::make_unique<::scene_node::ReleasePlayerRequest>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::SceneManagerNodeService, scene_node::SendSceneNodeGrpcReleasePlayerErased, "/scene_node.SceneNodeGrpc/ReleasePlayer"};
    gRpcMethodRegistry[SceneNodeGrpcPrepareBattleMessageId] = RpcMethodMeta{
        "SceneNodeGrpc", "PrepareBattle",
        std::make_unique<::PrepareBattleRequest>(),
        std::make_unique<::PrepareBattleResponse>(),
        1, common::base::eNodeType::SceneManagerNodeService, scene_node::SendSceneNodeGrpcPrepareBattleErased, "/scene_node.SceneNodeGrpc/PrepareBattle"};
    gRpcMethodRegistry[SceneNodeGrpcCancelBattlePrepareMessageId] = RpcMethodMeta{
        "SceneNodeGrpc", "CancelBattlePrepare",
        std::make_unique<::CancelBattlePrepareRequest>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::SceneManagerNodeService, scene_node::SendSceneNodeGrpcCancelBattlePrepareErased, "/scene_node.SceneNodeGrpc/CancelBattlePrepare"};
    gRpcMethodRegistry[SceneNodeGrpcSealAccountRoleTradeMessageId] = RpcMethodMeta{
        "SceneNodeGrpc", "SealAccountRoleTrade",
        std::make_unique<::SealAccountRoleTradeRequest>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::SceneManagerNodeService, scene_node::SendSceneNodeGrpcSealAccountRoleTradeErased, "/scene_node.SceneNodeGrpc/SealAccountRoleTrade"};
    gRpcMethodRegistry[SceneNodeGrpcAssetDebitMessageId] = RpcMethodMeta{
        "SceneNodeGrpc", "AssetDebit",
        std::make_unique<::AssetOpRequest>(),
        std::make_unique<::AssetOpResponse>(),
        1, common::base::eNodeType::SceneManagerNodeService, scene_node::SendSceneNodeGrpcAssetDebitErased, "/scene_node.SceneNodeGrpc/AssetDebit"};
    gRpcMethodRegistry[SceneNodeGrpcAssetAbortDebitMessageId] = RpcMethodMeta{
        "SceneNodeGrpc", "AssetAbortDebit",
        std::make_unique<::AssetOpRequest>(),
        std::make_unique<::AssetOpResponse>(),
        1, common::base::eNodeType::SceneManagerNodeService, scene_node::SendSceneNodeGrpcAssetAbortDebitErased, "/scene_node.SceneNodeGrpc/AssetAbortDebit"};
    gRpcMethodRegistry[SceneNodeGrpcAssetCreditMessageId] = RpcMethodMeta{
        "SceneNodeGrpc", "AssetCredit",
        std::make_unique<::AssetOpRequest>(),
        std::make_unique<::AssetOpResponse>(),
        1, common::base::eNodeType::SceneManagerNodeService, scene_node::SendSceneNodeGrpcAssetCreditErased, "/scene_node.SceneNodeGrpc/AssetCredit"};
    gRpcMethodRegistry[SceneNodeGrpcAssetAckReceiptMessageId] = RpcMethodMeta{
        "SceneNodeGrpc", "AssetAckReceipt",
        std::make_unique<::AssetOpRequest>(),
        std::make_unique<::AssetOpResponse>(),
        1, common::base::eNodeType::SceneManagerNodeService, scene_node::SendSceneNodeGrpcAssetAckReceiptErased, "/scene_node.SceneNodeGrpc/AssetAckReceipt"};

    // --- ClientPlayerTeam ---
    gRpcMethodRegistry[ClientPlayerTeamCreateTeamMessageId] = RpcMethodMeta{
        "ClientPlayerTeam", "CreateTeam",
        std::make_unique<::teampb::CreateTeamRequest>(),
        std::make_unique<::teampb::TeamResponse>(),
        1, common::base::eNodeType::TeamNodeService, teampb::SendClientPlayerTeamCreateTeamErased, "/teampb.ClientPlayerTeam/CreateTeam"};
    gRpcMethodRegistry[ClientPlayerTeamGetMyTeamMessageId] = RpcMethodMeta{
        "ClientPlayerTeam", "GetMyTeam",
        std::make_unique<::teampb::GetMyTeamRequest>(),
        std::make_unique<::teampb::TeamResponse>(),
        1, common::base::eNodeType::TeamNodeService, teampb::SendClientPlayerTeamGetMyTeamErased, "/teampb.ClientPlayerTeam/GetMyTeam"};
    gRpcMethodRegistry[ClientPlayerTeamApplyJoinTeamMessageId] = RpcMethodMeta{
        "ClientPlayerTeam", "ApplyJoinTeam",
        std::make_unique<::teampb::ApplyJoinTeamRequest>(),
        std::make_unique<::teampb::TeamResponse>(),
        1, common::base::eNodeType::TeamNodeService, teampb::SendClientPlayerTeamApplyJoinTeamErased, "/teampb.ClientPlayerTeam/ApplyJoinTeam"};
    gRpcMethodRegistry[ClientPlayerTeamHandleApplicationMessageId] = RpcMethodMeta{
        "ClientPlayerTeam", "HandleApplication",
        std::make_unique<::teampb::HandleApplicationRequest>(),
        std::make_unique<::teampb::TeamResponse>(),
        1, common::base::eNodeType::TeamNodeService, teampb::SendClientPlayerTeamHandleApplicationErased, "/teampb.ClientPlayerTeam/HandleApplication"};
    gRpcMethodRegistry[ClientPlayerTeamInviteToTeamMessageId] = RpcMethodMeta{
        "ClientPlayerTeam", "InviteToTeam",
        std::make_unique<::teampb::InviteToTeamRequest>(),
        std::make_unique<::teampb::TeamResponse>(),
        1, common::base::eNodeType::TeamNodeService, teampb::SendClientPlayerTeamInviteToTeamErased, "/teampb.ClientPlayerTeam/InviteToTeam"};
    gRpcMethodRegistry[ClientPlayerTeamRespondInviteMessageId] = RpcMethodMeta{
        "ClientPlayerTeam", "RespondInvite",
        std::make_unique<::teampb::RespondInviteRequest>(),
        std::make_unique<::teampb::TeamResponse>(),
        1, common::base::eNodeType::TeamNodeService, teampb::SendClientPlayerTeamRespondInviteErased, "/teampb.ClientPlayerTeam/RespondInvite"};
    gRpcMethodRegistry[ClientPlayerTeamListMyInvitesMessageId] = RpcMethodMeta{
        "ClientPlayerTeam", "ListMyInvites",
        std::make_unique<::teampb::ListMyInvitesRequest>(),
        std::make_unique<::teampb::ListMyInvitesResponse>(),
        1, common::base::eNodeType::TeamNodeService, teampb::SendClientPlayerTeamListMyInvitesErased, "/teampb.ClientPlayerTeam/ListMyInvites"};
    gRpcMethodRegistry[ClientPlayerTeamLeaveTeamMessageId] = RpcMethodMeta{
        "ClientPlayerTeam", "LeaveTeam",
        std::make_unique<::teampb::LeaveTeamRequest>(),
        std::make_unique<::teampb::TeamResponse>(),
        1, common::base::eNodeType::TeamNodeService, teampb::SendClientPlayerTeamLeaveTeamErased, "/teampb.ClientPlayerTeam/LeaveTeam"};
    gRpcMethodRegistry[ClientPlayerTeamKickMemberMessageId] = RpcMethodMeta{
        "ClientPlayerTeam", "KickMember",
        std::make_unique<::teampb::KickMemberRequest>(),
        std::make_unique<::teampb::TeamResponse>(),
        1, common::base::eNodeType::TeamNodeService, teampb::SendClientPlayerTeamKickMemberErased, "/teampb.ClientPlayerTeam/KickMember"};
    gRpcMethodRegistry[ClientPlayerTeamTransferLeaderMessageId] = RpcMethodMeta{
        "ClientPlayerTeam", "TransferLeader",
        std::make_unique<::teampb::TransferLeaderRequest>(),
        std::make_unique<::teampb::TeamResponse>(),
        1, common::base::eNodeType::TeamNodeService, teampb::SendClientPlayerTeamTransferLeaderErased, "/teampb.ClientPlayerTeam/TransferLeader"};
    gRpcMethodRegistry[ClientPlayerTeamDisbandTeamMessageId] = RpcMethodMeta{
        "ClientPlayerTeam", "DisbandTeam",
        std::make_unique<::teampb::DisbandTeamRequest>(),
        std::make_unique<::teampb::TeamResponse>(),
        1, common::base::eNodeType::TeamNodeService, teampb::SendClientPlayerTeamDisbandTeamErased, "/teampb.ClientPlayerTeam/DisbandTeam"};
    gRpcMethodRegistry[ClientPlayerTeamStartTeamMatchMessageId] = RpcMethodMeta{
        "ClientPlayerTeam", "StartTeamMatch",
        std::make_unique<::teampb::StartTeamMatchRequest>(),
        std::make_unique<::teampb::TeamResponse>(),
        1, common::base::eNodeType::TeamNodeService, teampb::SendClientPlayerTeamStartTeamMatchErased, "/teampb.ClientPlayerTeam/StartTeamMatch"};
    gRpcMethodRegistry[ClientPlayerTeamNotifyTeamSnapshotMessageId] = RpcMethodMeta{
        "ClientPlayerTeam", "NotifyTeamSnapshot",
        std::make_unique<::teampb::TeamSnapshotS2C>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::TeamNodeService, teampb::SendClientPlayerTeamNotifyTeamSnapshotErased, "/teampb.ClientPlayerTeam/NotifyTeamSnapshot"};
    gRpcMethodRegistry[ClientPlayerTeamNotifyTeamInviteMessageId] = RpcMethodMeta{
        "ClientPlayerTeam", "NotifyTeamInvite",
        std::make_unique<::teampb::TeamInviteS2C>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::TeamNodeService, teampb::SendClientPlayerTeamNotifyTeamInviteErased, "/teampb.ClientPlayerTeam/NotifyTeamInvite"};
    gRpcMethodRegistry[ClientPlayerTeamNotifyTeamEventMessageId] = RpcMethodMeta{
        "ClientPlayerTeam", "NotifyTeamEvent",
        std::make_unique<::teampb::TeamEventS2C>(),
        std::make_unique<::Empty>(),
        1, common::base::eNodeType::TeamNodeService, teampb::SendClientPlayerTeamNotifyTeamEventErased, "/teampb.ClientPlayerTeam/NotifyTeamEvent"};

    // --- TeamAuthority ---
    gRpcMethodRegistry[TeamAuthorityReadMembershipMessageId] = RpcMethodMeta{
        "TeamAuthority", "ReadMembership",
        std::make_unique<::teampb::ReadTeamMembershipRequest>(),
        std::make_unique<::teampb::ReadTeamMembershipResponse>(),
        1, common::base::eNodeType::TeamNodeService, teampb::SendTeamAuthorityReadMembershipErased, "/teampb.TeamAuthority/ReadMembership"};

    // --- ClientPlayerJubaozhai ---
    gRpcMethodRegistry[ClientPlayerJubaozhaiBrowseListingsMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "BrowseListings",
        std::make_unique<::trade::BrowseListingsRequest>(),
        std::make_unique<::trade::BrowseListingsResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiBrowseListingsErased, "/trade.ClientPlayerJubaozhai/BrowseListings"};
    gRpcMethodRegistry[ClientPlayerJubaozhaiGetListingDetailMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "GetListingDetail",
        std::make_unique<::trade::GetListingDetailRequest>(),
        std::make_unique<::trade::GetListingDetailResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiGetListingDetailErased, "/trade.ClientPlayerJubaozhai/GetListingDetail"};
    gRpcMethodRegistry[ClientPlayerJubaozhaiSetFavoriteMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "SetFavorite",
        std::make_unique<::trade::SetFavoriteRequest>(),
        std::make_unique<::trade::SetFavoriteResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiSetFavoriteErased, "/trade.ClientPlayerJubaozhai/SetFavorite"};
    gRpcMethodRegistry[ClientPlayerJubaozhaiGetMyShelfMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "GetMyShelf",
        std::make_unique<::trade::GetMyShelfRequest>(),
        std::make_unique<::trade::GetMyShelfResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiGetMyShelfErased, "/trade.ClientPlayerJubaozhai/GetMyShelf"};
    gRpcMethodRegistry[ClientPlayerJubaozhaiListOfficialSkusMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "ListOfficialSkus",
        std::make_unique<::trade::ListOfficialSkusRequest>(),
        std::make_unique<::trade::ListOfficialSkusResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiListOfficialSkusErased, "/trade.ClientPlayerJubaozhai/ListOfficialSkus"};
    gRpcMethodRegistry[ClientPlayerJubaozhaiCreateOfficialOrderMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "CreateOfficialOrder",
        std::make_unique<::trade::CreateOfficialOrderRequest>(),
        std::make_unique<::trade::CommerceOrderResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiCreateOfficialOrderErased, "/trade.ClientPlayerJubaozhai/CreateOfficialOrder"};
    gRpcMethodRegistry[ClientPlayerJubaozhaiGetCommerceOrderMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "GetCommerceOrder",
        std::make_unique<::trade::GetCommerceOrderRequest>(),
        std::make_unique<::trade::CommerceOrderResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiGetCommerceOrderErased, "/trade.ClientPlayerJubaozhai/GetCommerceOrder"};
    gRpcMethodRegistry[ClientPlayerJubaozhaiCancelCommerceOrderMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "CancelCommerceOrder",
        std::make_unique<::trade::CancelCommerceOrderRequest>(),
        std::make_unique<::trade::CommerceOrderResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiCancelCommerceOrderErased, "/trade.ClientPlayerJubaozhai/CancelCommerceOrder"};
    gRpcMethodRegistry[ClientPlayerJubaozhaiListCommerceOrdersMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "ListCommerceOrders",
        std::make_unique<::trade::ListCommerceOrdersRequest>(),
        std::make_unique<::trade::ListCommerceOrdersResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiListCommerceOrdersErased, "/trade.ClientPlayerJubaozhai/ListCommerceOrders"};
    gRpcMethodRegistry[ClientPlayerJubaozhaiCreatePlayerTradeMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "CreatePlayerTrade",
        std::make_unique<::trade::CreatePlayerTradeRequest>(),
        std::make_unique<::trade::CommerceOrderResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiCreatePlayerTradeErased, "/trade.ClientPlayerJubaozhai/CreatePlayerTrade"};
    gRpcMethodRegistry[ClientPlayerJubaozhaiConfirmPlayerTradeMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "ConfirmPlayerTrade",
        std::make_unique<::trade::ConfirmPlayerTradeRequest>(),
        std::make_unique<::trade::CommerceOrderResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiConfirmPlayerTradeErased, "/trade.ClientPlayerJubaozhai/ConfirmPlayerTrade"};
    gRpcMethodRegistry[ClientPlayerJubaozhaiCreateConsignmentMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "CreateConsignment",
        std::make_unique<::trade::CreateConsignmentRequest>(),
        std::make_unique<::trade::CommerceOrderResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiCreateConsignmentErased, "/trade.ClientPlayerJubaozhai/CreateConsignment"};
    gRpcMethodRegistry[ClientPlayerJubaozhaiBuyConsignmentMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "BuyConsignment",
        std::make_unique<::trade::BuyConsignmentRequest>(),
        std::make_unique<::trade::CommerceOrderResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiBuyConsignmentErased, "/trade.ClientPlayerJubaozhai/BuyConsignment"};
    gRpcMethodRegistry[ClientPlayerJubaozhaiCancelConsignmentMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "CancelConsignment",
        std::make_unique<::trade::CancelConsignmentRequest>(),
        std::make_unique<::trade::CommerceOrderResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiCancelConsignmentErased, "/trade.ClientPlayerJubaozhai/CancelConsignment"};
    gRpcMethodRegistry[ClientPlayerJubaozhaiPlaceMarketOrderMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "PlaceMarketOrder",
        std::make_unique<::trade::PlaceMarketOrderRequest>(),
        std::make_unique<::trade::MarketOrderResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiPlaceMarketOrderErased, "/trade.ClientPlayerJubaozhai/PlaceMarketOrder"};
    gRpcMethodRegistry[ClientPlayerJubaozhaiCancelMarketOrderMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "CancelMarketOrder",
        std::make_unique<::trade::CancelMarketOrderRequest>(),
        std::make_unique<::trade::MarketOrderResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiCancelMarketOrderErased, "/trade.ClientPlayerJubaozhai/CancelMarketOrder"};
    gRpcMethodRegistry[ClientPlayerJubaozhaiListMarketOrdersMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "ListMarketOrders",
        std::make_unique<::trade::ListMarketOrdersRequest>(),
        std::make_unique<::trade::ListMarketOrdersResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiListMarketOrdersErased, "/trade.ClientPlayerJubaozhai/ListMarketOrders"};
    gRpcMethodRegistry[ClientPlayerJubaozhaiGetMarketDepthMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "GetMarketDepth",
        std::make_unique<::trade::GetMarketDepthRequest>(),
        std::make_unique<::trade::GetMarketDepthResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiGetMarketDepthErased, "/trade.ClientPlayerJubaozhai/GetMarketDepth"};
    gRpcMethodRegistry[ClientPlayerJubaozhaiCreateAuctionLotMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "CreateAuctionLot",
        std::make_unique<::trade::CreateAuctionLotRequest>(),
        std::make_unique<::trade::AuctionLotResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiCreateAuctionLotErased, "/trade.ClientPlayerJubaozhai/CreateAuctionLot"};
    gRpcMethodRegistry[ClientPlayerJubaozhaiPlaceAuctionBidMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "PlaceAuctionBid",
        std::make_unique<::trade::PlaceAuctionBidRequest>(),
        std::make_unique<::trade::AuctionLotResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiPlaceAuctionBidErased, "/trade.ClientPlayerJubaozhai/PlaceAuctionBid"};
    gRpcMethodRegistry[ClientPlayerJubaozhaiGetAuctionLotMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "GetAuctionLot",
        std::make_unique<::trade::GetAuctionLotRequest>(),
        std::make_unique<::trade::AuctionLotResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiGetAuctionLotErased, "/trade.ClientPlayerJubaozhai/GetAuctionLot"};
    gRpcMethodRegistry[ClientPlayerJubaozhaiCancelAuctionLotMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "CancelAuctionLot",
        std::make_unique<::trade::CancelAuctionLotRequest>(),
        std::make_unique<::trade::AuctionLotResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiCancelAuctionLotErased, "/trade.ClientPlayerJubaozhai/CancelAuctionLot"};
    gRpcMethodRegistry[ClientPlayerJubaozhaiBrowseCommerceOffersMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "BrowseCommerceOffers",
        std::make_unique<::trade::BrowseCommerceOffersRequest>(),
        std::make_unique<::trade::BrowseCommerceOffersResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiBrowseCommerceOffersErased, "/trade.ClientPlayerJubaozhai/BrowseCommerceOffers"};
    gRpcMethodRegistry[ClientPlayerJubaozhaiListTradeMarketsMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "ListTradeMarkets",
        std::make_unique<::trade::ListTradeMarketsRequest>(),
        std::make_unique<::trade::ListTradeMarketsResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiListTradeMarketsErased, "/trade.ClientPlayerJubaozhai/ListTradeMarkets"};
    gRpcMethodRegistry[ClientPlayerJubaozhaiCreateRmbConsignmentMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "CreateRmbConsignment",
        std::make_unique<::trade::CreateRmbConsignmentRequest>(),
        std::make_unique<::trade::RmbConsignmentResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiCreateRmbConsignmentErased, "/trade.ClientPlayerJubaozhai/CreateRmbConsignment"};
    gRpcMethodRegistry[ClientPlayerJubaozhaiGetRmbConsignmentMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "GetRmbConsignment",
        std::make_unique<::trade::GetRmbConsignmentRequest>(),
        std::make_unique<::trade::RmbConsignmentResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiGetRmbConsignmentErased, "/trade.ClientPlayerJubaozhai/GetRmbConsignment"};
    gRpcMethodRegistry[ClientPlayerJubaozhaiListRmbConsignmentsMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "ListRmbConsignments",
        std::make_unique<::trade::ListRmbConsignmentsRequest>(),
        std::make_unique<::trade::ListRmbConsignmentsResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiListRmbConsignmentsErased, "/trade.ClientPlayerJubaozhai/ListRmbConsignments"};
    gRpcMethodRegistry[ClientPlayerJubaozhaiBuyRmbConsignmentMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "BuyRmbConsignment",
        std::make_unique<::trade::BuyRmbConsignmentRequest>(),
        std::make_unique<::trade::CommerceOrderResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiBuyRmbConsignmentErased, "/trade.ClientPlayerJubaozhai/BuyRmbConsignment"};
    gRpcMethodRegistry[ClientPlayerJubaozhaiCancelRmbConsignmentMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "CancelRmbConsignment",
        std::make_unique<::trade::CancelRmbConsignmentRequest>(),
        std::make_unique<::trade::RmbConsignmentResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiCancelRmbConsignmentErased, "/trade.ClientPlayerJubaozhai/CancelRmbConsignment"};
    gRpcMethodRegistry[ClientPlayerJubaozhaiGetRmbOrderMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "GetRmbOrder",
        std::make_unique<::trade::GetCommerceOrderRequest>(),
        std::make_unique<::trade::CommerceOrderResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiGetRmbOrderErased, "/trade.ClientPlayerJubaozhai/GetRmbOrder"};
    gRpcMethodRegistry[ClientPlayerJubaozhaiListRmbOrdersMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "ListRmbOrders",
        std::make_unique<::trade::ListCommerceOrdersRequest>(),
        std::make_unique<::trade::ListCommerceOrdersResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiListRmbOrdersErased, "/trade.ClientPlayerJubaozhai/ListRmbOrders"};
    gRpcMethodRegistry[ClientPlayerJubaozhaiCancelRmbOrderMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "CancelRmbOrder",
        std::make_unique<::trade::CancelCommerceOrderRequest>(),
        std::make_unique<::trade::CommerceOrderResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiCancelRmbOrderErased, "/trade.ClientPlayerJubaozhai/CancelRmbOrder"};
    gRpcMethodRegistry[ClientPlayerJubaozhaiListSellerIncomesMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "ListSellerIncomes",
        std::make_unique<::trade::ListSellerIncomesRequest>(),
        std::make_unique<::trade::ListSellerIncomesResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiListSellerIncomesErased, "/trade.ClientPlayerJubaozhai/ListSellerIncomes"};
    gRpcMethodRegistry[ClientPlayerJubaozhaiCreateWithdrawalMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "CreateWithdrawal",
        std::make_unique<::trade::CreateWithdrawalRequest>(),
        std::make_unique<::trade::WithdrawalResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiCreateWithdrawalErased, "/trade.ClientPlayerJubaozhai/CreateWithdrawal"};
    gRpcMethodRegistry[ClientPlayerJubaozhaiGetWithdrawalMessageId] = RpcMethodMeta{
        "ClientPlayerJubaozhai", "GetWithdrawal",
        std::make_unique<::trade::GetWithdrawalRequest>(),
        std::make_unique<::trade::WithdrawalResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendClientPlayerJubaozhaiGetWithdrawalErased, "/trade.ClientPlayerJubaozhai/GetWithdrawal"};

    // --- TradeAdmin ---
    gRpcMethodRegistry[TradeAdminSeedListingMessageId] = RpcMethodMeta{
        "TradeAdmin", "SeedListing",
        std::make_unique<::trade::SeedListingRequest>(),
        std::make_unique<::trade::SeedListingResponse>(),
        1, common::base::eNodeType::TradeNodeService, trade::SendTradeAdminSeedListingErased, "/trade.TradeAdmin/SeedListing"};
}

bool IsClientMessageId(uint32_t messageId)
{
	switch (messageId) {
	case BattleClientPlayerSubmitBattleActionMessageId:
	case BattleClientPlayerGetBattleStateMessageId:
	case BattleClientPlayerStopWatchBattleMessageId:
	case BattleClientPlayerSetAutoBattleMessageId:
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
	case ClientPlayerLoginRefreshTokenMessageId:
	case ClientPlayerMailListMailsMessageId:
	case ClientPlayerMailReadMailMessageId:
	case ClientPlayerMailClaimMailAttachmentsMessageId:
	case ClientPlayerMailDeleteMailsMessageId:
	case ClientPlayerMailClearReadMailsMessageId:
	case MatchServiceGetNetworkProbePlanMessageId:
	case MatchServiceCommitNetworkProbeMessageId:
	case MatchServiceJoinQueueMessageId:
	case MatchServiceCancelQueueMessageId:
	case MatchServiceGetQueueStatusMessageId:
	case MatchServiceChallengePlayerMessageId:
	case MatchServiceRespondChallengeMessageId:
	case MatchServiceWatchBattleMessageId:
	case MatchServiceRequestBattleTicketMessageId:
	case MatchServiceListWatchableBattlesMessageId:
	case MatchServiceListBattleReportsMessageId:
	case MatchServiceGetBattleReportMessageId:
	case ClientPlayerRankGetRankPageMessageId:
	case ClientPlayerRankGetRankAroundMessageId:
	case SceneActivityClientPlayerGetActivityListMessageId:
	case SceneActivityClientPlayerClaimActivityVitalityRewardMessageId:
	case SceneAttributeClientPlayerGetAttributePanelMessageId:
	case SceneAttributeClientPlayerAllocateAttributePointsMessageId:
	case SceneAttributeClientPlayerResetAttributePointsMessageId:
	case SceneAttributeClientPlayerAutoAllocateAttributePointsMessageId:
	case SceneAttributeClientPlayerCreateAttributeSchemeMessageId:
	case SceneAttributeClientPlayerSwitchAttributeSchemeMessageId:
	case SceneAttributeClientPlayerRenameAttributeSchemeMessageId:
	case SceneAttributeClientPlayerGmSetPlayerLevelMessageId:
	case SceneBagClientPlayerGetBagMessageId:
	case SceneBagClientPlayerSortBagMessageId:
	case SceneBagClientPlayerEquipItemMessageId:
	case SceneBagClientPlayerUnequipItemMessageId:
	case SceneBagClientPlayerGmGrantItemMessageId:
	case SceneBagClientPlayerEnhanceEquipMessageId:
	case SceneBagClientPlayerInlayGemMessageId:
	case SceneBagClientPlayerRemoveGemMessageId:
	case SceneBagClientPlayerRetrieveTemporaryItemsMessageId:
	case SceneBattleResultClientPlayerGetRecentBattleResultsMessageId:
	case SceneBattleResultClientPlayerAckBattleResultsMessageId:
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
	case SceneMovementClientPlayerTeamFollowMessageId:
	case SceneMysteryMerchantClientPlayerGetMysteryMerchantInfoMessageId:
	case SceneMysteryMerchantClientPlayerBuyMysteryMerchantGoodsMessageId:
	case ScenePetClientPlayerGetPetListMessageId:
	case ScenePetClientPlayerSummonPetMessageId:
	case ScenePetClientPlayerRecallPetMessageId:
	case ScenePetClientPlayerAllocatePetPointsMessageId:
	case ScenePetClientPlayerResetPetPointsMessageId:
	case ScenePetClientPlayerAutoAllocatePetPointsMessageId:
	case ScenePetClientPlayerRenamePetMessageId:
	case ScenePetClientPlayerGmGrantPetMessageId:
	case SceneSceneClientPlayerEnterSceneMessageId:
	case SceneSceneClientPlayerSceneInfoC2SMessageId:
	case SceneSceneClientPlayerTravelToZoneMessageId:
	case SceneSceneClientPlayerGetSyncDigestMessageId:
	case SceneSkillClientPlayerReleaseSkillMessageId:
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
