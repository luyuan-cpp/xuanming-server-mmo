#include "battle_room_manager.h"

#include <limits>
#include <cstdlib>
#include <chrono>
#include <map>
#include <memory>
#include <string>
#include <string_view>
#include <unordered_map>
#include <utility>
#include <vector>

#include <yaml-cpp/yaml.h>
#include <google/protobuf/io/coded_stream.h>
#include <google/protobuf/io/zero_copy_stream_impl_lite.h>

#include "muduo/base/Logging.h"

#include "engine/infra/messaging/kafka/kafka_producer.h"
#include "node/system/node/client_endpoint.h"
#include "node/system/node/node.h"
#include "node/system/node/node_command_route.h"
#include "node_config_manager.h"
#include "thread_context/node_context_manager.h"
#include "core/utils/random/random.h"
#include "node/system/node/muduo_grpc_channel_slot.h"
#include "muduo_grpc/channel.h"
#include "core/security/service_call_auth.h"
#include "grpc_client/match/match_internal_grpc_client.h"
#include "proto/match/match_internal.pb.h"
#include "time/system/time.h"

#include "battle_push_policy.h"
#include "battle_security.h"
#include "client/battle_client_edge.h"
#include "client/network_probe_security.h"

#include "constants/turn_battle_constants.h"
#include "data/battle_table_fingerprint.h"
// 活动上下文与逃跑 / 阵亡名单的回显组装(帮会同道历练,guild-phase2 §6.19–§6.21)。
#include "system/battle_result_activity.h"

#include "proto/common/base/message.pb.h"
#include "proto/common/event/battle_event.pb.h"
#include "proto/contracts/kafka/gate_command.pb.h"
#include "proto/contracts/kafka/gate_event.pb.h"
#include "proto/contracts/kafka/match_event.pb.h"
#include "proto/contracts/kafka/scene_command.pb.h"

#include "table/proto/tip/common_error_tip.pb.h"
// kMatchSpectateFull(观众席满;battle→match 与 match→客户端共用)
#include "table/proto/tip/match_error_tip.pb.h"

// 事件 id 注册表常量:
//   contracts_kafka_gate_event_event_id.h  → ContractsKafkaPushToPlayerEventEventId(大厅公告回落)
//   common_event_battle_event_event_id.h   → BattleConfirmedEventEventId
#include "rpc/service_metadata/contracts_kafka_gate_event_event_id.h"
#include "rpc/service_metadata/common_event_battle_event_event_id.h"
// 客户端消息 id 常量(proto/battle/player_battle.proto 重生成后出现):
//   BattleClientPlayerNotifyBattleStartMessageId / NotifyTurnResult / NotifyBattleEnd
//   二期新增:NotifySpectateState / NotifySpectateTurnResult / NotifySpectateEnd
#include "rpc/service_metadata/player_battle_service_metadata.h"

namespace
{

    // 单房间观众席上限 kMaxObserversPerRoom 是 BattleRoomManager 的公开常量(头文件),测试直接引用。
    constexpr size_t kMaxSpectatorOperations = 100000;
    using battle_security::ValidSpectatorOperation;

    // 客户端 S2C 的信封(MessageContent,与大厅连接同一线协议)。直连单发、观众帧编码一次与中继 / 广播的 client_frame
    // 共用这一处,几条出口的字节一致。
    ::MessageContent ClientEnvelope(const uint32_t messageId, const ::google::protobuf::Message &message)
    {
        ::MessageContent content;
        content.set_message_id(messageId);
        content.set_serialized_message(message.SerializeAsString());
        return content;
    }

    // ---- Kafka 出站(设计文档 D6;不变量:target_instance_id 必填 + key=业务实体 ID) ----

    // 发一条 GateCommand 到目标 gate 的 topic。
    // key=player_id:同一玩家的大厅公告在 gate 侧保序(宪法 §7 不变量 3)。
    // battle 发往 gate 的命令只有给某个玩家的大厅公告,信封一律带目标 player_id。
    void SendGateCommand(const ::BattleRouting &routing, const uint64_t playerId,
                         const uint32_t eventId, std::string payload)
    {
        // 目标玩家身份栅栏 fail-closed(gate_event.proto PushToPlayerEvent.player_id):gate 对 0 放行不校验,
        // 等于把栅栏关掉,生产者一侧拒发。唯一调用方 PushMessageViaGate 把同一个值填进事件,这里一处同时拦住
        // 信封与事件。
        if (playerId == 0)
        {
            LOG_ERROR << "battle outbound GateCommand rejected: player_id is 0, event_id=" << eventId
                      << " gate_node_id=" << routing.gate_node_id();
            return;
        }
        // 不变量 2 fail-closed:空 instance id 会关闭 gate 侧防僵尸过滤,宁可丢弃并报错。
        if (routing.gate_instance_id().empty())
        {
            LOG_ERROR << "battle outbound GateCommand rejected: gate_instance_id is empty, player_id=" << playerId
                      << " event_id=" << eventId << " gate_node_id=" << routing.gate_node_id();
            return;
        }

        contracts::kafka::GateCommand command;
        command.set_event_id(eventId);
        command.set_player_id(playerId);
        // target_gate_id 是共享分区之后的**第一级**过滤:一个分区上坐着几百个 gate,
        // 数字比对先把绝大多数命令挡掉,再轮到 target_instance_id 的字符串比对
        // (node_kafka_command_filter.h)。以前一节点一 topic 时留 0 也能跑,现在留 0
        // 等于把便宜那一级关掉。
        command.set_target_gate_id(routing.gate_node_id());
        command.set_target_instance_id(routing.gate_instance_id());
        command.set_payload(std::move(payload));

        // topic + 分区必须一起算,不能再自己拼 "gate-"+id(见 node_command_route.h)。
        const auto route = node::kafka::ResolveCommandRoute(GateNodeService, routing.gate_node_id());
        const auto err = KafkaProducer::Instance().send(route.topic, command.SerializeAsString(),
                                                        std::to_string(playerId), route.partition);
        if (err != RdKafka::ERR_NO_ERROR)
        {
            LOG_ERROR << "battle outbound GateCommand send failed: topic=" << route.topic
                      << " partition=" << route.partition
                      << " player_id=" << playerId << " event_id=" << eventId << " err=" << err;
        }
    }

    // 大厅公告回落(Kafka→gate):MessageContent 包客户端协议消息,经 PushToPlayerEvent
    // 送到玩家的大厅 TCP 连接。只服务直连建立之前的大厅公告(NotifyBattleAssigned /
    // NotifyBattleStart,turn-based §22 D68);唯一调用方是 BattleRoomManager::PushLobbyAnnouncement,
    // 战斗帧绝不走这里。
    void PushMessageViaGate(const ::BattleRouting &routing, const uint64_t playerId,
                            const uint32_t messageId, const google::protobuf::Message &message)
    {
        contracts::kafka::PushToPlayerEvent event;
        event.set_session_id(routing.session_id());
        // session_id 的高位是会立即复用的 gate node_id、低位序号也会回绕,gate 投递前按它核对会话当前玩家
        // (routing-identity-audit-20260908.md R13);0 由 SendGateCommand 拒发。
        event.set_player_id(playerId);
        auto *content = event.mutable_message_content();
        content->set_message_id(messageId);
        content->set_serialized_message(message.SerializeAsString());

        SendGateCommand(routing, playerId, ContractsKafkaPushToPlayerEventEventId,
                        event.SerializeAsString());
    }

    // 战斗帧无活直连被丢弃时的采样日志(turn-based §22 D68)。battle 无 Prometheus 端点,
    // 由日志侧按 metric= 提取计数;每个 messageId 首次必打,之后每 1024 次打一行,防掉线玩家
    // 每回合一条把日志放大(与直连面 LogRejectionSampled 同口径)。messageId 只可能是
    // BattleClientPlayer 的几条 Notify 号(进程内常量),键集合有界;单 loop 线程访问,无锁。
    void LogBattleFrameDroppedSampled(const uint64_t battleId, const uint64_t playerId,
                                      const uint32_t messageId)
    {
        static std::unordered_map<uint32_t, uint64_t> droppedByMessageId;
        uint64_t &count = droppedByMessageId[messageId];
        if ((count++ & 0x3FF) == 0)
        {
            LOG_INFO << "metric=battle_frame_dropped_no_direct message_id=" << messageId
                     << " dropped_total=" << count
                     << " latest_battle_id=" << battleId << " latest_player_id=" << playerId
                     << ",player has no live direct connection;battle frames do not fall back to gate(client requests GetBattleState after direct connection is ready)";
        }
    }

    // 发一条进程内事件到玩家所在 scene:SceneCommand{DispatchEvent, eventId, message}。
    // topic=scene-cmd_g<N> 的 scene_node_id % P 号分区,key=player_id
    // (同一玩家的确认保序 —— 分区已由 node_id 定死,key 只影响同分区内的批次,
    //  不影响落点,不变量 3 仍然成立)。
    // target_instance_id=BattleRouting.scene_instance_id(不变量 2 fail-closed)。
    void SendSceneEvent(const ::BattleRouting &routing, const uint64_t playerId,
                        const uint32_t eventId, const google::protobuf::Message &message,
                        const uint64_t battleIdForLog)
    {
        if (routing.scene_instance_id().empty())
        {
            LOG_ERROR << "battle outbound scene event rejected: scene_instance_id is empty, player_id=" << playerId
                      << " battle_id=" << battleIdForLog << " event_id=" << eventId;
            return;
        }
        const uint32_t sceneNodeId = routing.scene_node_id();
        if (sceneNodeId == 0)
        {
            LOG_ERROR << "battle outbound scene event rejected: scene_node_id is 0, player_id=" << playerId
                      << " battle_id=" << battleIdForLog << " event_id=" << eventId;
            return;
        }

        contracts::kafka::SceneCommand command;
        command.set_command_type(contracts::kafka::SceneCommand::DispatchEvent);
        command.set_player_id(playerId);
        command.set_target_scene_id(sceneNodeId);
        command.set_payload(message.SerializeAsString());
        command.set_target_instance_id(routing.scene_instance_id());
        command.set_event_id(eventId);

        const auto route = node::kafka::ResolveCommandRoute(SceneNodeService, sceneNodeId);
        const auto err = KafkaProducer::Instance().send(route.topic, command.SerializeAsString(),
                                                        std::to_string(playerId), route.partition);
        if (err != RdKafka::ERR_NO_ERROR)
        {
            LOG_ERROR << "battle outbound scene event send failed: topic=" << route.topic
                      << " partition=" << route.partition
                      << " player_id=" << playerId << " battle_id=" << battleIdForLog
                      << " event_id=" << eventId << " err=" << err;
        }
    }

    // 确认事件补发策略(与 scene 侧 player_battle.h 的锁保留期对齐):
    //   scene 备战期锁 EX = prepare_deadline 剩余秒数 + PlayerBattleSystem::kLockExtraTtlSec(60s),
    //   备战到期只摘组件不删锁,锁在期间迟到的确认仍可重建冻结。
    //   prepare_deadline = gather 起点 + match 的 matchedTicketTTLFor(组大小)(go/match queue.go),
    //   现行公式 ⌈N×6 + 2×5 + 3⌉ + 10 → 1/2/5/10 人依次 30/35/53/83s;最大组 10 人(5v5),
    //   所以锁最晚在 gather 起点 +83+60=143s 过期。补发窗口从建房起算,建房晚于 gather 起点,
    //   取 180s ≥ 143s,多出的 37s 吸收 match / scene / battle 三方墙钟偏差与锁 EX 的秒级取整。
    //   依赖:match 改 matchedTicketTTLFor 公式、把 MatchedTicketTTLSeconds 配到公式值之上、放大组
    //   上限,或 scene 改 kLockExtraTtlSec 时,本窗口必须同步保持 ≥ 最大 TTL + kLockExtraTtlSec。
    //   窗口内每 10s 一次(scene 已 FIGHTING 时幂等忽略,开销是每玩家一条小消息;收尾即停表)。
    constexpr uint64_t kConfirmResendWindowMs = 180 * 1000;
    constexpr double kConfirmResendIntervalSec = 10.0;

    // 确认:CreateBattle 成功后每玩家一条 BattleConfirmedEvent,scene 据此 PREPARING→FIGHTING
    // 并把作废期限从 prepare_deadline_ms 切到正式 deadline_ms(cross-zone-matchmaking.md §10)。
    // 首发 + 开局后窗口内周期补发(ResendBattleConfirmed);produce 失败只记日志,由下一次补发覆盖。
    void SendBattleConfirmedEvent(const ::BattleRouting &routing, const uint64_t playerId,
                                  const uint64_t battleId, const uint64_t deadlineMs)
    {
        ::BattleConfirmedEvent event;
        event.set_battle_id(battleId);
        event.set_player_id(playerId);
        event.set_deadline_ms(deadlineMs);
        SendSceneEvent(routing, playerId, BattleConfirmedEventEventId, event, battleId);
    }

    // ---- 配表指纹校验开关(cross-zone-matchmaking.md §10) ----

    enum class FingerprintMode
    {
        Off,     // 不校验
        Warn,    // 不一致只记 metric=battle_table_fingerprint_mismatch,照常开局(默认)
        Enforce, // 不一致拒绝 CreateBattle,metric=battle_table_fingerprint_reject
    };

    const char *FingerprintModeName(const FingerprintMode mode)
    {
        switch (mode)
        {
        case FingerprintMode::Off:
            return "off";
        case FingerprintMode::Enforce:
            return "enforce";
        case FingerprintMode::Warn:
        default:
            return "warn";
        }
    }

    bool ParseFingerprintMode(const std::string &text, FingerprintMode &out)
    {
        if (text == "off")
        {
            out = FingerprintMode::Off;
            return true;
        }
        if (text == "warn")
        {
            out = FingerprintMode::Warn;
            return true;
        }
        if (text == "enforce")
        {
            out = FingerprintMode::Enforce;
            return true;
        }
        return false;
    }

    // 配置载体:GameConfig proto 只承载 scene_node_type/zone_id/zone_redis 且 proto 已冻结
    // (本轮不改 proto),故这里直接用 yaml-cpp 读同一份 etc/game_config.yaml 的
    // `battle_table_fingerprint_mode` 键(与 Node::LoadConfigs 同路径规则:GAME_CONFIG_PATH
    // 环境变量优先),再允许 BATTLE_TABLE_FINGERPRINT_MODE 环境变量覆盖(K8s 灰度用)。
    // 进程内只读一次;缺键/读失败一律回默认 warn(灰度期宁可多告警不误拒开局)。
    FingerprintMode LoadFingerprintModeOnce()
    {
        FingerprintMode mode = FingerprintMode::Warn;

        std::string configPath = "etc/game_config.yaml";
        if (const char *envPath = std::getenv("GAME_CONFIG_PATH"); envPath != nullptr && envPath[0] != '\0')
        {
            configPath = envPath;
        }
        try
        {
            const YAML::Node root = YAML::LoadFile(configPath);
            if (root["battle_table_fingerprint_mode"])
            {
                const auto text = root["battle_table_fingerprint_mode"].as<std::string>();
                if (!ParseFingerprintMode(text, mode))
                {
                    LOG_WARN << "Invalid battle_table_fingerprint_mode;using default warn: value=" << text
                             << " path=" << configPath;
                }
            }
        }
        catch (const std::exception &ex)
        {
            LOG_WARN << "Failed to read battle_table_fingerprint_mode;using default warn: path=" << configPath
                     << " err=" << ex.what();
        }

        if (const char *envMode = std::getenv("BATTLE_TABLE_FINGERPRINT_MODE");
            envMode != nullptr && envMode[0] != '\0')
        {
            if (ParseFingerprintMode(envMode, mode))
            {
                LOG_INFO << "BATTLE_TABLE_FINGERPRINT_MODE env override applied: " << envMode;
            }
            else
            {
                LOG_WARN << "Invalid BATTLE_TABLE_FINGERPRINT_MODE;ignored: value=" << envMode;
            }
        }

        LOG_INFO << "battle table fingerprint validation mode: mode=" << FingerprintModeName(mode);
        return mode;
    }

    FingerprintMode CurrentFingerprintMode()
    {
        // 单 loop 线程访问;首次 CreateBattle 时读一次配置并缓存
        static const FingerprintMode mode = LoadFingerprintModeOnce();
        return mode;
    }

    // 校验 CreateBattleRequest 携带的指纹(request.table_fingerprint + 每个快照的
    // table_fingerprint,非空才比)与本节点指纹一致。返回 false = 按 enforce 拒绝开局。
    bool CheckTableFingerprint(const ::CreateBattleRequest &request)
    {
        const auto mode = CurrentFingerprintMode();
        if (mode == FingerprintMode::Off)
        {
            return true;
        }
        const auto &self = turnbattle::BattleTableFingerprint::Current();

        // 收集所有不一致来源(request 本身 + 各玩家快照),一次日志说清楚
        std::string mismatches;
        if (!request.table_fingerprint().empty() && request.table_fingerprint() != self)
        {
            mismatches += " request=" + request.table_fingerprint();
        }
        for (const auto &snapshot : request.players())
        {
            if (!snapshot.table_fingerprint().empty() && snapshot.table_fingerprint() != self)
            {
                mismatches += " player_" + std::to_string(snapshot.player_id()) + "=" +
                              snapshot.table_fingerprint();
            }
        }
        if (mismatches.empty())
        {
            return true;
        }

        // 结构化 metric 日志(battle 进程无 Prometheus 端点,与 scene reaper 同款口径,
        // 由日志侧提取计数);不含 player_id 标签之外的高基数字段
        if (mode == FingerprintMode::Enforce)
        {
            LOG_ERROR << "metric=battle_table_fingerprint_reject battle_id=" << request.battle_id()
                      << " match_mode=" << request.match_mode()
                      << " self=" << self << " mismatch:" << mismatches
                      << ",table fingerprint mismatch;battle creation rejected(mode=enforce)";
            return false;
        }
        LOG_WARN << "metric=battle_table_fingerprint_mismatch battle_id=" << request.battle_id()
                 << " match_mode=" << request.match_mode()
                 << " self=" << self << " mismatch:" << mismatches
                 << ",table fingerprint mismatch;battle creation continues(mode=warn)";
        return true;
    }

} // namespace

BattleRoomManager &BattleRoomManager::Instance()
{
    // 单 loop 线程访问,无并发构造问题
    static BattleRoomManager instance;
    return instance;
}

BattleRoomManager::BattleRoom *BattleRoomManager::FindRoom(const uint64_t battleId)
{
    return rooms_.Find(battleId);
}

BattleRoomManager::BattleRoom *BattleRoomManager::EmplaceRoom(std::unique_ptr<BattleRoom> room)
{
    const auto battleId = room->battleId;
    // 先插表后回调(房间表保证):回调方(Agones 单元计数)看到的是"确实已创建"的房间
    auto *inserted = rooms_.Emplace(battleId, std::move(room));
    if (inserted == nullptr)
    {
        // 表里那间保持原样;按值接管的新房间随形参销毁(插表前没有任何副作用)
        LOG_ERROR << "EmplaceRoom duplicate insertion(caller idempotency check failed);new room object discarded: battle_id=" << battleId;
    }
    return inserted;
}

void BattleRoomManager::EraseRoom(const uint64_t battleId)
{
    // 先移除后回调(房间表保证):最后一个房间移除后 Agones 才能回 Ready,顺序反了会在房间还在时放出容量
    rooms_.Erase(battleId);
}

bool BattleRoomManager::OutcomeSigningKeyReady(std::string_view secret)
{
    if (token_security::TrimAscii(secret).size() >= 32) return true;
    if (!outcomeKeyUnavailableLogged_) {
        outcomeKeyUnavailableLogged_ = true;
        LOG_ERROR << "metric=battle_sql_outcome_key_unavailable protocol_version=1 disabled=1";
    }
    return false;
}

void BattleRoomManager::HandleCreateBattle(const ::CreateBattleRequest &request,
                                           ::CreateBattleResponse &response)
{
    const auto battleId = request.battle_id();
    response.set_battle_id(battleId);
    // 只有终局协议 v1:放置操作与目标实例是终局原件的身份,缺一不可;其它版本一律拒绝(player-data-tidb-scaling-plan.md §5.4)。
    if (request.outcome_protocol_version() != turnbattle::kOutcomeProtocolVersion ||
        request.placement_operation_id().empty() || request.target_instance_id().empty()) {
        response.mutable_error_message()->set_id(kInvalidParameter); return;
    }

    std::string canonical;
    {google::protobuf::io::StringOutputStream sink(&canonical);google::protobuf::io::CodedOutputStream output(&sink);
     output.SetSerializationDeterministic(true);if(!request.SerializeToCodedStream(&output)){response.mutable_error_message()->set_id(kInvalidParameter);return;}}
    const auto currentMs=TimeSystem::NowMillisecondsUTC();
    // 墓碑保留覆盖所有合法迟到请求；过窗请求仍受admit_before硬拒绝。
    std::erase_if(creationOperations_,[currentMs,&rooms=rooms_](const auto& entry){return turnbattle::CreationReceiptExpired(entry.second.expiresMs,currentMs,rooms.Find(entry.first)!=nullptr);});
    if(!gNode||request.required_physical_region()==0||request.required_physical_region()!=network_probe::PhysicalRegion()||
       request.target_instance_id()!=gNode->GetNodeInfo().node_uuid())
    {response.mutable_error_message()->set_id(kInvalidParameter);return;}
    const auto old=creationOperations_.find(battleId);
    if(old!=creationOperations_.end())
    {
        const auto* originalRoom = FindRoom(battleId);
        if(old->second.aborted||old->second.operationId!=request.placement_operation_id()||
           old->second.canonicalRequest!=canonical||originalRoom==nullptr)
            response.mutable_error_message()->set_id(kInvalidParameter);
        else {
            response.set_adopted_outcome_protocol_version(originalRoom->outcomeProtocol);
            response.set_target_instance_id(originalRoom->targetInstance);
        }
        return;
    }
    if(request.admit_before_ms()==0||request.admit_before_ms()<=currentMs)
    {response.mutable_error_message()->set_id(kInvalidParameter);return;}

    // 既有操作在上方仍按原房间回显 adopted；缺密钥只禁止创建新房间，不降级。
    const char* outcomeSecret = std::getenv("MMORPG_MATCH_OUTCOME_SECRET_BATTLE");
    if (!OutcomeSigningKeyReady(outcomeSecret ? outcomeSecret : "")) {
        response.mutable_error_message()->set_id(kFeatureUnavailable);
        return;
    }

    // 幂等:match 补偿路径可能重试 CreateBattle
    if (FindRoom(battleId) != nullptr)
    {
        LOG_INFO << "CreateBattle idempotent hit: battle_id=" << battleId;
        return;
    }

    if (battleId == 0 || request.players_size() == 0)
    {
        LOG_ERROR << "CreateBattle invalid arguments: battle_id=" << battleId
                  << " players=" << request.players_size();
        response.mutable_error_message()->set_id(kInvalidParameter);
        return;
    }

    // 路由信息 fail-closed 校验:instance id 缺失意味着出站防僵尸过滤会被关闭,
    // 这样的战斗打完也送不回结果,直接拒绝开局让 match 走补偿。
    for (const auto &snapshot : request.players())
    {
        const auto &routing = snapshot.routing();
        if (routing.gate_instance_id().empty() || routing.scene_instance_id().empty())
        {
            LOG_ERROR << "CreateBattle incomplete routing information: battle_id=" << battleId
                      << " player_id=" << snapshot.player_id()
                      << " gate_instance_id=" << routing.gate_instance_id()
                      << " scene_instance_id=" << routing.scene_instance_id();
            response.mutable_error_message()->set_id(kInvalidParameter);
            return;
        }
    }

    // 配表指纹:出快照的 scene / 编排的 match 与本节点读的不是同一份战斗表时,
    // 确定性引擎的结果对各方不再可复现;按配置 warn/enforce(cross-zone-matchmaking.md §10)
    if (!CheckTableFingerprint(request))
    {
        // TipInfoMessage 只有 id + parameters:说明文本放 parameters[0],match 日志可直接打出来
        response.mutable_error_message()->set_id(kFeatureUnavailable);
        response.mutable_error_message()->add_parameters(
            "battle table fingerprint mismatch: node=" +
            turnbattle::BattleTableFingerprint::Current() +
            " request=" + request.table_fingerprint());
        return;
    }

    auto room = std::make_unique<BattleRoom>(dataProvider_);
    room->battleId = battleId;
    room->matchMode = request.match_mode();
    room->battleConfigId = request.battle_config_id();
    // 活动上下文原样保存(未填 = kind NONE);BuildResult 回显进终局原件,结果通道在 match(§6.20)
    room->activityContext = request.activity_context();
    // 观战可见性随原件冻结,只在这里赋值一次(AddObserver 的第二道闸据此执法;未填 = 不可观战)
    room->spectateVisibility = request.spectate_visibility();
    room->outcomeProtocol = request.outcome_protocol_version();
    room->placementOperation = request.placement_operation_id();
    room->targetInstance = request.target_instance_id();
    if (!room->engine.Initialize(request))
    {
        LOG_ERROR << "CreateBattle engine initialization failed(invalid snapshot/table data): battle_id=" << battleId
                  << " battle_config_id=" << request.battle_config_id();
        response.mutable_error_message()->set_id(kInvalidTableData);
        return;
    }

    for (const auto &snapshot : request.players())
    {
        room->routingByPlayer.emplace(snapshot.player_id(), snapshot.routing());
    }

    // 整场 deadline:超时强制平局收尾(battle_node.proto 契约)。
    // match 没填时按"回合上限 + 2 回合余量"兜底 —— 房间是纯内存对象,
    // 没有整场 deadline 的房间在玩家全部掉线后会靠回合超时一直空转到打满,
    // 这里的兜底保证任何房间的生存期都有界。
    const auto nowMs = TimeSystem::NowMillisecondsUTC();
    uint64_t deadlineMs = request.deadline_ms();
    if (deadlineMs <= nowMs)
    {
        deadlineMs = nowMs + static_cast<uint64_t>(turnbattle::kDefaultMaxRounds + 2) *
                                 turnbattle::kRoundDurationMs;
    }
    // 票据 expire_at_ms 取房间作废期限,必须在签票之前落到房间上
    room->deadlineMs = deadlineMs;

    // fail-closed 预签(turn-based §22 D70):直连是战斗唯一通路,签不出票的玩家连不上这局,
    // 放他进来只会让他全程挂机被默认普攻。所以在插表、装定时器、推送、发确认事件这些副作用
    // **之前**为全体参战者签好票;任一人签不出就整局拒绝 —— 此刻房间还是没插表的局部对象,
    // 拒绝等于什么都没发生,match 的通用补偿(DestroyBattle 幂等 → CancelBattlePrepare)即可收尾。
    std::map<uint64_t, ::BattleAssignedS2C> assignmentByPlayer;
    for (const auto &[playerId, routing] : room->routingByPlayer)
    {
        if (!BuildAssignment(*room, playerId, ::BATTLE_TICKET_ROLE_PARTICIPANT,
                             assignmentByPlayer[playerId]))
        {
            LOG_ERROR << "metric=battle_ticket_issue_failed battle_id=" << battleId
                      << " player_id=" << playerId << " role=participant"
                      << ",participant ticket signing failed;battle creation rejected(kServiceUnavailable)";
            response.mutable_error_message()->set_id(kServiceUnavailable);
            return;
        }
    }

    // 纯构建/签票也可能耗尽准入预算，提交前再次拒绝过期的原操作。
    if(request.admit_before_ms()<=TimeSystem::NowMillisecondsUTC())
    {response.mutable_error_message()->set_id(kInvalidParameter);return;}

    // 插表 = 房间生命周期起点(EmplaceRoom 随即通知 Agones 单元已创建);此前的拒绝路径都没有副作用
    auto *roomPtr = EmplaceRoom(std::move(room));
    if (roomPtr == nullptr)
    {
        // 开头已判过幂等且同一 loop 任务内无人插表,走到这里是程序缺陷(EmplaceRoom 已打 ERROR)。
        // 按幂等命中处理:回 OK、不设 error_message。表里那间同 battle_id 的房间是真的在打,
        // 回错误会诱发 match 的补偿 DestroyBattle(battle_id),把它一起销毁。
        return;
    }
    // canonical 已在任何插表/通知副作用之前成功序列化，失败路径不会留下房间。
    creationOperations_[battleId]={request.placement_operation_id(),std::move(canonical),request.deadline_ms()+60000,false};

    roomPtr->battleTimer.RunAfter(static_cast<double>(deadlineMs - nowMs) / 1000.0,
                                  [this, battleId]
                                  { OnBattleDeadline(battleId); });

    // 第一回合行动收集窗口
    ArmRoundTimer(*roomPtr);

    ::BattleStartS2C start;
    start.set_battle_id(battleId);
    *start.mutable_state() = roomPtr->engine.BuildStateSnapshot();
    start.mutable_state()->set_battle_id(battleId);
    // 引擎无时钟,行动截止由节点按房间 timer 回填
    start.mutable_state()->set_action_deadline_ms(roomPtr->actionDeadlineMs);

    for (const auto &[playerId, routing] : roomPtr->routingByPlayer)
    {
        // 落点分配先于开战包(同 key 保序):两条都是大厅公告 —— 此刻还没有直连,
        // 经 Kafka→gate 回落下发;客户端拿到票据后建直连,此后的战斗帧只走直连
        PushAssignment(*roomPtr, playerId, routing, assignmentByPlayer.at(playerId));
        // 每人一份:剔除他人冷却、填本人道具余量(G7)。开局冷却本就是空的,
        // 这里主要是把 self_items 带给客户端,让道具面板一开局就有数
        ::BattleStartS2C personalStart = start;
        RedactStateForViewer(*personalStart.mutable_state(), playerId);
        FillSelfItems(*roomPtr, *personalStart.mutable_state(), playerId);
        PushLobbyAnnouncement(*roomPtr, playerId, routing, BattleClientPlayerNotifyBattleStartMessageId,
                              personalStart);
        // 向玩家所在 scene 确认开局:PREPARING→FIGHTING,作废期限切到本房间的正式 deadline
        // (与 battleTimer 同一个值,scene reaper 与房间强制收尾的时限口径一致)
        SendBattleConfirmedEvent(routing, playerId, battleId, deadlineMs);
    }

    // 确认事件有界周期补发:单次投递丢失/迟到时 scene 仍能在锁保留期内把冻结升级/重建
    roomPtr->confirmResendUntilMs = nowMs + kConfirmResendWindowMs;
    roomPtr->confirmResendTimer.RunEvery(kConfirmResendIntervalSec,
                                         [this, battleId]
                                         { ResendBattleConfirmed(battleId); });

    response.set_adopted_outcome_protocol_version(roomPtr->outcomeProtocol);
    response.set_target_instance_id(roomPtr->targetInstance);
    LOG_INFO << "CreateBattle succeeded: battle_id=" << battleId
             << " players=" << request.players_size()
             << " battle_config_id=" << request.battle_config_id()
             << " match_mode=" << request.match_mode()
             << " deadline_ms=" << deadlineMs
             << " spectate_visibility=" << ::eSpectateVisibility_Name(request.spectate_visibility())
             << " activity_kind=" << ::eBattleActivityKind_Name(request.activity_context().kind())
             << " activity_id=" << request.activity_context().activity_id()
             << " guild_id=" << request.activity_context().guild_id();
}

void BattleRoomManager::HandleDestroyBattle(const ::DestroyBattleRequest &request)
{
    const auto battleId = request.battle_id();
    // 房间只由 Match 的持久决定收尾。终局原件可能已提交,尚未收到决定不能先把相同对局宣布为作废;
    // 迟到的旧 worker 也不能销毁新 worker 已确认的同一房间。
    // 本检查与下面立墓碑同在 battle 逻辑线程,CreateBattle 不可能插在二者之间。
    if (FindRoom(battleId) != nullptr)
    {
        LOG_INFO << "DestroyBattle ignored: room still exists and only a match decision can end it, battle_id=" << battleId
                 << " reason=" << request.reason();
        return;
    }
    if(!request.placement_operation_id().empty())
    {
        if(!gNode||request.target_instance_id()!=gNode->GetNodeInfo().node_uuid())return;
        const auto old=creationOperations_.find(battleId);
        if(old!=creationOperations_.end()&&old->second.operationId!=request.placement_operation_id())return;
        auto& operation=creationOperations_[battleId];operation.operationId=request.placement_operation_id();operation.aborted=true;
        operation.expiresMs=std::max(operation.expiresMs,TimeSystem::NowMillisecondsUTC()+360000);
    }
    // 幂等:已销毁/从未建成都视为成功
    LOG_INFO << "DestroyBattle idempotent hit: battle_id=" << battleId
             << " reason=" << request.reason();
}

void BattleRoomManager::HandleQueryBattleCreation(const ::QueryBattleCreationRequest& request,::QueryBattleCreationResponse& response)
{
    if(!gNode||request.target_instance_id()!=gNode->GetNodeInfo().node_uuid()||request.placement_operation_id().empty())return;
    response.set_target_instance_id(gNode->GetNodeInfo().node_uuid());
    const char* secret = std::getenv("MMORPG_MATCH_OUTCOME_SECRET_BATTLE");
    response.set_supported_outcome_protocol_version(OutcomeSigningKeyReady(secret ? secret : "") ? turnbattle::kOutcomeProtocolVersion : 0);
    const auto found=creationOperations_.find(request.battle_id());
    if(found==creationOperations_.end()){response.set_state(BATTLE_CREATION_ABSENT);return;}
    if(found->second.operationId!=request.placement_operation_id())return;
    const auto* room = FindRoom(request.battle_id());
    if (room != nullptr) response.set_adopted_outcome_protocol_version(room->outcomeProtocol);
    response.set_state(found->second.aborted?BATTLE_CREATION_ABORTED:(room?BATTLE_CREATION_PRESENT:BATTLE_CREATION_ABSENT));
}

bool BattleRoomManager::AcceptsPlacementOperation(uint64_t id,const std::string& operation,const std::string& instance) const
{
    if(operation.empty())return instance.empty()&&!creationOperations_.contains(id);
    if(!gNode||instance!=gNode->GetNodeInfo().node_uuid())return false;
    const auto old=creationOperations_.find(id);
    return old==creationOperations_.end()||old->second.operationId==operation;
}

void BattleRoomManager::HandleSubmitBattleAction(const ::SessionDetails &sessionDetails,
                                                 const ::SubmitBattleActionRequest &request,
                                                 ::SubmitBattleActionResponse &response)
{
    // 权威身份来自已验证的票据(直连面合成的 SessionDetails,turn-based D28),
    // 请求体里没有也不允许有 player_id
    const auto playerId = sessionDetails.player_id();
    if (playerId == 0)
    {
        LOG_WARN << "SubmitBattleAction session has no player identity: session_id=" << sessionDetails.session_id();
        response.mutable_error_message()->set_id(kPlayerNotFoundInSession);
        return;
    }

    auto *room = FindRoom(request.battle_id());
    if (room == nullptr)
    {
        // 常见竞态:战斗刚结束,客户端的行动包晚到
        LOG_DEBUG << "SubmitBattleAction room does not exist: battle_id=" << request.battle_id()
                  << " player_id=" << playerId;
        response.mutable_error_message()->set_id(kInvalidParameter);
        return;
    }

    // 防串房:battle_id 可被客户端伪造,必须校验参战名单
    if (room->routingByPlayer.find(playerId) == room->routingByPlayer.end())
    {
        LOG_WARN << "SubmitBattleAction player is not in this battle: battle_id=" << request.battle_id()
                 << " player_id=" << playerId;
        response.mutable_error_message()->set_id(kInvalidParameter);
        return;
    }

    if (TerminalPending(*room)) { response.mutable_error_message()->set_id(kServiceUnavailable); return; }
    // 先查后提交:校验码回给提交者(G2)。原先"一律回 OK"会让玩家点了吃药、
    // 客户端显示成功、回合结算却变成默认普攻,连原因都看不到 —— 玩家资产路径
    // 不许静默降级(AGENTS §11.3)。ValidateAction 零副作用,与 SubmitAction 的
    // 接受条件逐条同源。
    const uint32_t validation = room->engine.ValidateAction(playerId, request.action());
    if (validation != kSuccess)
    {
        LOG_DEBUG << "SubmitBattleAction validation failed: battle_id=" << request.battle_id()
                  << " player_id=" << playerId << " action=" << request.action().action_type()
                  << " code=" << validation;
        // 0 不是任何一个 tip 码(kCommon_errorOK=0),写进去客户端会读成"成功"却没落账。
        // 引擎已把坏表格子转成 kInvalidTableData,这里是最后一道兜底。
        if (validation == 0)
        {
            LOG_ERROR << "SubmitBattleAction validation returned 0(invalid table or engine contract violation): battle_id="
                      << request.battle_id() << " player_id=" << playerId;
        }
        response.mutable_error_message()->set_id(validation != 0 ? validation : kInvalidParameter);
        return;
    }

    // 非法行动引擎不落账,按未提交处理,回合超时的默认普攻兜底(引擎契约)
    const bool allReady = room->engine.SubmitAction(playerId, request.action());
    if (allReady)
    {
        // 全员就绪提前结算,不等 action_deadline。
        // 任何依赖 room 的响应字段都在这一行之前填完:结算可能把房间带进终局提交
        ResolveRound(room->battleId);
    }
}

void BattleRoomManager::HandleGetBattleState(const ::SessionDetails &sessionDetails,
                                             const ::GetBattleStateRequest &request,
                                             ::BattleStateS2C &response)
{
    // 权威身份来自已验证的票据(turn-based D28)。参战者直连就绪后的第一条包就是它
    // (§22 D69:参战者不在握手时推快照,由这里补拉)
    const auto playerId = sessionDetails.player_id();
    auto *room = FindRoom(request.battle_id());
    // 参战者与观众都放行(观众重进观战画面补拉同一份快照,§10.5)
    const bool isMember =
        room != nullptr && playerId != 0 &&
        (room->routingByPlayer.find(playerId) != room->routingByPlayer.end() ||
         room->routingByObserver.find(playerId) != room->routingByObserver.end());
    if (!isMember)
    {
        // 战斗已不存在(结束/作废)或既非参战者也非观众:回默认状态,battle_id 置 0,
        // 客户端据此丢弃本地战斗 UI(重连补拉的失败分支)
        LOG_DEBUG << "GetBattleState room missing or player unrelated: battle_id=" << request.battle_id()
                  << " player_id=" << playerId;
        response.Clear();
        return;
    }

    response = room->pendingVisibleState ? *room->pendingVisibleState : room->engine.BuildStateSnapshot();
    response.set_battle_id(room->battleId);
    // 引擎无时钟,行动截止由节点回填
    response.set_action_deadline_ms(room->actionDeadlineMs);
    // 补拉路径同样要脱敏 + 带本人道具余量(D39:收缩后客户端丢帧只能靠这条补,
    // 只改广播不改这里,重连一次就把对手冷却全看见了)
    const bool isObserver = room->routingByPlayer.find(playerId) == room->routingByPlayer.end();
    RedactStateForViewer(response, isObserver ? 0 : playerId);
    if (!isObserver)
    {
        FillSelfItems(*room, response, playerId);
    }
}

void BattleRoomManager::PruneSpectatorOperations(uint64_t now)
{
    // 每次准入/取消最多处理128个过期项，不能每次遍历全部活动观战。
    for (size_t count = 0; count < 128 && !spectatorDeadlines_.empty(); ++count) {
        const auto deadline = spectatorDeadlines_.begin();
        if (deadline->first > now) break;
        const auto operation = spectatorOperations_.find(deadline->second);
        if (operation != spectatorOperations_.end() && operation->second.cancelled)
            spectatorOperations_.erase(operation);
        spectatorDeadlines_.erase(deadline);
    }
}
void BattleRoomManager::HandleQueryObserver(const ::QueryObserverRequest &request, ::QueryObserverResponse &response)
{
    if (!gNode || request.target_instance_id() != gNode->GetNodeInfo().node_uuid()
        || request.battle_id() == 0 || request.observer_player_id() == 0
        || !ValidSpectatorOperation(request.spectator_operation_id())) return;
    response.set_spectator_operation_id(request.spectator_operation_id());
    response.set_target_instance_id(request.target_instance_id());
    response.set_state(OBSERVER_ADMISSION_ABSENT);
    if (const auto* room = FindRoom(request.battle_id()); room != nullptr) {
        const auto original = room->observerOperations.find(request.observer_player_id());
        if (original != room->observerOperations.end() && original->second == request.spectator_operation_id()
            && room->routingByObserver.count(request.observer_player_id()) != 0) response.set_state(OBSERVER_ADMISSION_ACTIVE);
    }
}

void BattleRoomManager::HandleAddObserver(const ::AddObserverRequest &request,
                                          ::AddObserverResponse &response)
{
    const auto battleId = request.battle_id();
    const auto observerId = request.observer_player_id();

    const uint64_t now = TimeSystem::NowMillisecondsUTC();
    if (!gNode || request.target_instance_id() != gNode->GetNodeInfo().node_uuid()
        || !ValidSpectatorOperation(request.spectator_operation_id()) || request.admit_before_ms() <= now
        || request.admit_before_ms() - now > 30000 || observerId == 0 || battleId == 0) {
        response.mutable_error_message()->set_id(kInvalidParameter);
        return;
    }
    // 操作表仅本loop拥有；过了原准入截止的Add会在上面拒绝，才可回收该操作墓碑。
    PruneSpectatorOperations(now);
    const auto key = std::make_tuple(battleId, observerId, request.spectator_operation_id());
    const auto canonical = request.SerializeAsString();
    auto operation = spectatorOperations_.find(key);
    if (operation != spectatorOperations_.end()) {
        if (operation->second.admitBeforeMs != request.admit_before_ms()
            || operation->second.canonicalRequest != canonical) {
            response.mutable_error_message()->set_id(kInvalidParameter);
            return;
        }
        if (operation->second.cancelled) {
            // 同 op、同请求的重放回首次拒绝码:match 的前台请求与后台恢复可能并发发同一个 op,先到的回满员,
            // 后到的若回 kInvalidParameter,这次观战会被永久记成「无法观战」,而不是转中继档。
            const uint32_t firstReject = operation->second.rejectTip;
            response.mutable_error_message()->set_id(firstReject != 0 ? firstReject : kInvalidParameter);
            return;
        }
    } else {
        if (spectatorOperations_.size() >= kMaxSpectatorOperations) {
            // 操作墓碑表满是节点级过载保护(表按原准入截止过期回收,稍后重试即可),不是观众席满员,
            // 不能回 kMatchSpectateFull 让客户端误以为这场人满。
            response.mutable_error_message()->set_id(kRateLimitExceeded);
            return;
        }
        operation = spectatorOperations_.emplace(key, SpectatorOperation{request.admit_before_ms(), canonical, true}).first;
        spectatorDeadlines_.emplace(request.admit_before_ms(), key);
    }
    // 所有失败都封闭原意图；只在完成准入/精确重放的成功分支恢复为active。
    operation->second.cancelled = true;
    // 下面有多个拒绝出口,首次拒绝码在退出时统一记进墓碑。按 key 重查、不握迭代器:拒绝路径上的 RetireObserver 可能已删掉这条墓碑。
    struct FirstRejectRecorder
    {
        std::map<SpectatorKey, SpectatorOperation> &operations;
        const SpectatorKey &key;
        const ::AddObserverResponse &response;
        ~FirstRejectRecorder()
        {
            const auto recorded = operations.find(key);
            if (recorded != operations.end() && recorded->second.cancelled && recorded->second.rejectTip == 0)
            {
                recorded->second.rejectTip = response.error_message().id();
            }
        }
    } recordFirstReject{spectatorOperations_, key, response};
    auto *room = FindRoom(battleId);
    if (room == nullptr)
    {
        // kEntityIsNull 是 match 懒剔除 Redis 观战索引的信号(§10.2),别换 tip
        LOG_INFO << "AddObserver room does not exist: battle_id=" << battleId
                 << " observer=" << observerId;
        response.mutable_error_message()->set_id(kEntityIsNull);
        return;
    }

    // fail-closed:落点分配包是大厅公告,观众还没有直连时只能经 Kafka→gate 回落送达;
    // gate_instance_id 缺失会被防僵尸校验丢弃,观众永远拿不到票据、建不了直连,
    // 登记进来也是聋子观众,直接拒绝让 match 重组路由
    if (observerId == 0 || request.routing().gate_instance_id().empty())
    {
        LOG_ERROR << "AddObserver invalid arguments: battle_id=" << battleId
                  << " observer=" << observerId
                  << " gate_instance_id=" << request.routing().gate_instance_id();
        response.mutable_error_message()->set_id(kInvalidParameter);
        return;
    }

    // 参战者不能观战自己所在的战斗:directConnByPlayer 按 player_id 只有一个槽位,
    // 参战和观战共用,放行会让观战直连顶掉参战直连(D11 互斥的房间侧兜底)
    if (room->routingByPlayer.find(observerId) != room->routingByPlayer.end())
    {
        LOG_WARN << "AddObserver observer is a participant: battle_id=" << battleId
                 << " observer=" << observerId;
        response.mutable_error_message()->set_id(kInvalidParameter);
        return;
    }

    // 幂等:match gRPC 超时重试命中存量登记。但 match 超时回滚(DEL 观战标记)后
    // 玩家可能换会话重试(重连换 gate/session),存量路由指向的是旧会话:
    // 不比对就重推,落点分配包打到旧会话,新会话永远拿不到票据。
    if (const auto it = room->routingByObserver.find(observerId);
        it != room->routingByObserver.end())
    {
        const auto active = room->observerOperations.find(observerId);
        if (active == room->observerOperations.end() || active->second != request.spectator_operation_id()) {
            response.mutable_error_message()->set_id(kInvalidParameter);
            return;
        }
        const bool sameSession =
            it->second.session_id() == request.routing().session_id() &&
            it->second.gate_node_id() == request.routing().gate_node_id() &&
            it->second.gate_instance_id() == request.routing().gate_instance_id();

        // 两条幂等路径都先重签(turn-based §22 D70 fail-closed)。签不出票 = 这名观众连不上直连、
        // 看不到任何战斗帧;而 match 收到任何错误都会 DEL 观战标记(watchbattlelogic),
        // 房间侧必须同步摘除并关其直连,否则 match 认为他已不在观战、房间里却还挂着他。
        ::BattleAssignedS2C assigned;
        if (!BuildAssignment(*room, observerId, ::BATTLE_TICKET_ROLE_OBSERVER, assigned, request.spectator_operation_id()))
        {
            LOG_ERROR << "metric=battle_ticket_issue_failed battle_id=" << battleId
                      << " player_id=" << observerId << " role=observer"
                      << " path=" << (sameSession ? "idempotent_same_session" : "idempotent_session_changed")
                      << ",spectator ticket signing failed;observer removed and request rejected(kServiceUnavailable)";
            RetireObserver(*room, observerId, "observer_ticket_issue_failed");
            response.mutable_error_message()->set_id(kServiceUnavailable);
            return;
        }

        if (sameSession)
        {
            // 同会话重试:重推落点分配;观战快照是战斗帧,已有活直连时照常直发,
            // 否则丢弃,由客户端建直连时握手快照补上(§22 D69)
            LOG_INFO << "AddObserver idempotent hit;assignment pushed again: battle_id=" << battleId
                     << " observer=" << observerId;
            PushAssignment(*room, observerId, it->second, assigned);
            PushSpectateState(*room, observerId);
            operation->second.cancelled = false;
            return;
        }
        LOG_INFO << "AddObserver idempotent hit but session changed;route refreshed: battle_id=" << battleId
                 << " observer=" << observerId
                 << " old_session_id=" << it->second.session_id()
                 << " new_session_id=" << request.routing().session_id();
        it->second = request.routing();
        room->observerNames[observerId] = request.observer_name();
        // 会话变了 = 旧客户端实例已不在(崩溃 / 换网):它留下的直连很可能半开,内核尚未
        // 感知、connected() 仍为 true,落点分配包会被写进死 socket 而不是回落到新会话。
        // 先关旧直连,让分配包走 Kafka→gate 到新会话;新客户端凭新票重新挂接,
        // 观战首帧随握手下发(§22 D69),这里不推 —— 直连刚关,推了也只会被战斗帧出口丢弃。
        CloseDirectConnectionOf(*room, observerId, "observer_session_changed");
        // 防御性计时:同 op 原件比对(canonicalRequest)已包含 routing,会话变了的同 op 请求在函数开头
        // 就回 kInvalidParameter,本分支在现有契约下走不到,只在以后放宽原件比对时才会生效。
        // 一旦走到:上面已把直连摘出表,旧连接随后的断开回调走 DetachDirectConnection 时什么都摘不到,
        // 不会替它计重连宽限;这里必须自己重新计时,否则新会话一直不来挂接,这个席位就永远不回收。
        StartObserverSeatGrace(*room, observerId, false);
        PushAssignment(*room, observerId, it->second, assigned);
        operation->second.cancelled = false;
        return;
    }

    // 可见性第二道闸(battle-spectate-tiered-fanout.md §10 第 6 条):match 已按同一份冻结的可见性拦过一道,
    // battle 再拦一次,match 漏判或绕过 match 的 Add 都进不了私密局。默认拒绝:只放行 PUBLIC,未填与 PRIVATE
    // 一样拒。回 kInvalidParameter:房间还在,不能回 kEntityIsNull(match 会懒剔除观战索引);也不是满员
    // (满员会驱动 match 转中继档)。墓碑沿用上面已封闭的状态,同 op 迟到的 Add 不会翻成准入。
    if (room->spectateVisibility != ::SPECTATE_VISIBILITY_PUBLIC)
    {
        LOG_WARN << "AddObserver rejected: battle is not open to spectators, battle_id=" << battleId
                 << " observer=" << observerId;
        response.mutable_error_message()->set_id(kInvalidParameter);
        return;
    }

    // 席位按名单人数计:名单含已挂直连、待连接(登记后宽限内)与断线宽限中的观众,
    // 后两类到期由 ReclaimExpiredObserverSeats 回收腾位。幂等重放已在上面放行,不受满员影响。
    if (room->routingByObserver.size() >= kMaxObserversPerRoom)
    {
        LOG_INFO << "AddObserver spectator capacity reached: battle_id=" << battleId
                 << " observer=" << observerId << " cap=" << kMaxObserversPerRoom;
        response.mutable_error_message()->set_id(kMatchSpectateFull);
        return;
    }
    // 节点级观众席配额(口径同上按名单人数,跨全部房间求和):观众与参战者共用直连面的连接上限,
    // 全节点观众席封顶在注入的配额。与单房间满员同码,对 match 都是「直连席已满」。
    if (const auto nodeSeats = RegisteredObserverSeats(); nodeSeats >= observerSeatQuota_)
    {
        LOG_INFO << "AddObserver node spectator quota reached: battle_id=" << battleId
                 << " observer=" << observerId << " node_observers=" << nodeSeats
                 << " quota=" << observerSeatQuota_;
        response.mutable_error_message()->set_id(kMatchSpectateFull);
        return;
    }

    // fail-closed 预签(turn-based §22 D70):登记之前签票,签不出不登记 ——
    // 观众连不上直连就看不到任何战斗帧,登记进来只会占一个观众名额
    ::BattleAssignedS2C assigned;
    if (!BuildAssignment(*room, observerId, ::BATTLE_TICKET_ROLE_OBSERVER, assigned, request.spectator_operation_id()))
    {
        LOG_ERROR << "metric=battle_ticket_issue_failed battle_id=" << battleId
                  << " player_id=" << observerId << " role=observer path=new"
                  << ",spectator ticket signing failed;spectating rejected(kServiceUnavailable)";
        response.mutable_error_message()->set_id(kServiceUnavailable);
        return;
    }

    room->routingByObserver.emplace(observerId, request.routing());
    room->observerNames.emplace(observerId, request.observer_name());
    room->observerOperations.emplace(observerId, request.spectator_operation_id());
    operation->second.cancelled = false;
    // 从登记起计连接宽限:到期仍未挂上直连就回收席位,不让拿了票不来的观众一直占座
    StartObserverSeatGrace(*room, observerId, false);

    // 只下发落点分配(大厅公告)。观战首帧随直连握手下发(§22 D69 snapshot on connect):
    // 此刻观众一定还没有直连,在这里推首帧只会被战斗帧出口丢弃
    PushAssignment(*room, observerId, request.routing(), assigned);

    LOG_INFO << "AddObserver succeeded: battle_id=" << battleId
             << " observer=" << observerId << " name=" << request.observer_name()
             << " observers=" << room->routingByObserver.size();
}

void BattleRoomManager::HandleRemoveObserver(const ::RemoveObserverRequest &request, ::RemoveObserverResponse &response)
{
    const auto battleId = request.battle_id();
    const auto observerId = request.observer_player_id();

    const uint64_t now = TimeSystem::NowMillisecondsUTC();
    if (!gNode || request.target_instance_id() != gNode->GetNodeInfo().node_uuid()
        || !ValidSpectatorOperation(request.spectator_operation_id()) || request.admit_before_ms() == 0
        || (request.admit_before_ms() > now && request.admit_before_ms() - now > 30000)
        || battleId == 0 || observerId == 0) return;
    PruneSpectatorOperations(now);
    const auto key = std::make_tuple(battleId, observerId, request.spectator_operation_id());
    auto operation = spectatorOperations_.find(key);
    if (operation != spectatorOperations_.end() && operation->second.admitBeforeMs != request.admit_before_ms()) return;
    if (request.admit_before_ms() > now) {
        if (operation == spectatorOperations_.end()) {
            if (spectatorOperations_.size() >= kMaxSpectatorOperations) return;
            operation = spectatorOperations_.emplace(key, SpectatorOperation{request.admit_before_ms(), {}, true}).first;
            spectatorDeadlines_.emplace(request.admit_before_ms(), key);
        }
        operation->second.cancelled = true;
    }
    else if (operation != spectatorOperations_.end()) spectatorOperations_.erase(operation);
    response.set_cancelled(true);
    response.set_spectator_operation_id(request.spectator_operation_id());
    response.set_target_instance_id(request.target_instance_id());
    auto *room = FindRoom(battleId);
    if (room == nullptr)
    {
        // 幂等:房间已结束时观众早已随 FinishBattle/作废路径清退
        return;
    }
    const auto it = room->routingByObserver.find(observerId);
    const auto active = room->observerOperations.find(observerId);
    if (active == room->observerOperations.end() || active->second != request.spectator_operation_id()) return;
    if (it == room->routingByObserver.end())
    {
        // 原op已精确匹配：同时清掉失败准入遗留的私有槽，旧op不能清新op。
        RetireObserver(*room, observerId, "observer_removed");
        return;
    }

    // 被动清退(去排队互斥等):先告知原因再关直连,客户端据 reason 关观战 UI。
    // 战斗帧只走直连:观众没有直连时不回落,客户端只能靠自身超时 / 直连 FIN 收起观战 UI
    ::SpectateEndS2C end;
    end.set_battle_id(battleId);
    end.set_outcome(::BATTLE_OUTCOME_ONGOING);
    end.set_reason(::SPECTATE_END_REMOVED);
    PushBattleFrame(*room, observerId, BattleClientPlayerNotifySpectateEndMessageId, end);

    // 观众已不在名单,直连没有存在意义;SpectateEnd 已排队,shutdown 让它先 flush
    RetireObserver(*room, observerId, "observer_removed");

    LOG_INFO << "RemoveObserver completed: battle_id=" << battleId
             << " observer=" << observerId << " reason=" << request.reason();
}

void BattleRoomManager::HandleStopWatchBattle(const ::SessionDetails &sessionDetails,
                                              const ::StopWatchBattleRequest &request,
                                              ::StopWatchBattleResponse &response)
{
    // 权威身份来自已验证的票据(请求体里的 battle_id 可伪造,
    // 但只用来查房,身份不经它)
    const auto playerId = sessionDetails.player_id();
    if (playerId == 0)
    {
        LOG_WARN << "StopWatchBattle session has no player identity: session_id=" << sessionDetails.session_id();
        response.mutable_error_message()->set_id(kPlayerNotFoundInSession);
        return;
    }

    auto *room = FindRoom(request.battle_id());
    if (room == nullptr)
    {
        // 幂等:战斗刚结束,退出包晚到,视为成功
        return;
    }
    const auto it = room->routingByObserver.find(playerId);
    if (it == room->routingByObserver.end())
    {
        // 幂等:重复退出/从未观战都回成功
        return;
    }

    // 主动退出不推 SpectateEnd:是客户端自己发起的动作,回包即确认。
    // 退出请求可能就是从这条直连来的,应答由直连面在本函数返回后写出;关闭已推迟到本轮
    // loop 之后(CloseDirectConnectionOf 内部 queueInLoop),应答不会被丢
    RetireObserver(*room, playerId, "stop_watch");

    LOG_INFO << "StopWatchBattle completed: battle_id=" << room->battleId
             << " observer=" << playerId;
}

void BattleRoomManager::HandleSetAutoBattle(const ::SessionDetails &sessionDetails,
                                            const ::SetAutoBattleRequest &request,
                                            ::SetAutoBattleResponse &response)
{
    const auto playerId = sessionDetails.player_id();
    if (playerId == 0)
    {
        LOG_WARN << "SetAutoBattle session has no player identity: session_id=" << sessionDetails.session_id();
        response.mutable_error_message()->set_id(kPlayerNotFoundInSession);
        return;
    }

    auto *room = FindRoom(request.battle_id());
    if (room == nullptr)
    {
        LOG_DEBUG << "SetAutoBattle room does not exist: battle_id=" << request.battle_id()
                  << " player_id=" << playerId;
        response.mutable_error_message()->set_id(kInvalidParameter);
        return;
    }

    // 防串房/防观众冒充:只有参战者能切自动
    if (room->routingByPlayer.find(playerId) == room->routingByPlayer.end())
    {
        LOG_WARN << "SetAutoBattle player is not in this battle: battle_id=" << request.battle_id()
                 << " player_id=" << playerId;
        response.mutable_error_message()->set_id(kInvalidParameter);
        return;
    }

    if (TerminalPending(*room)) { response.mutable_error_message()->set_id(kServiceUnavailable); return; }
    // 置位前先采样就绪态,供下方判断本次置位是否触发"未就绪 -> 全员就绪"的翻转
    const bool wasAllReady = room->engine.AllPlayersReady();

    // 未死/未逃等状态校验在引擎内。注意引擎契约是"0 = 成功、非 0 = tip 错误码"
    // (turn_battle_engine.h SetActorAuto 注释),与 tip 枚举的 kSuccess 不是一回事,
    // 不能拿 kSuccess 比较。
    const uint32_t result = room->engine.SetActorAuto(playerId, request.enabled());
    if (result != 0)
    {
        response.mutable_error_message()->set_id(result);
        return;
    }

    // 仅当本次置位把房间从"未就绪"翻到"全员就绪"时才立即结算(与 SubmitBattleAction
    // 的 allReady 分支同一条路径,ResolveRound 内部先取消回合 timer,不会双重结算);
    // 装填时已全员就绪的房间(全自动)节奏由已按 AutoRoundIntervalMsFor(下限 kAutoRoundIntervalMs,
    // 随出手单位数放宽)装填的 roundTimer 负责,重复置位不触发立即结算,防止客户端以包速率重发击穿 D13 节奏。
    if (request.enabled() && !wasAllReady && room->engine.AllPlayersReady())
    {
        ResolveRound(room->battleId);
    }
}

void BattleRoomManager::ArmRoundTimer(BattleRoom &room)
{
    // 手动行动收集窗口为 25 秒,不影响技能/buff 的时间→回合换算。
    // 全自动例外(D13):装填时 AllPlayersReady() 已为真说明全部存活玩家都挂机,
    // 本窗口内不会再有任何提交把回合"提前"结算,整窗等待纯属空转拖慢节奏;
    // 又不能立即结算(同步递归刷回合,观众/客户端表现跟不上),
    // 故改用 kAutoRoundIntervalMs 固定节奏推进。
    // 全自动间隔随刚结算那一回合的出手单位数放宽(AutoRoundIntervalMsFor):固定 2000ms 时单位一多,
    // 客户端 6 倍速也演不完,会把整回合演出跳过。开局首次装填 LastActionOrder 为空 → 仍是下限 2000ms。
    const uint64_t windowMs = room.engine.AllPlayersReady()
                                  ? turnbattle::AutoRoundIntervalMsFor(room.engine.LastActionOrder().size())
                                  : turnbattle::kActionTimeoutMs;
    room.actionDeadlineMs = TimeSystem::NowMillisecondsUTC() + windowMs;
    const auto battleId = room.battleId;
    // timer 回调按 battle_id 重查房间:房间可能在窗口内被销毁,不能捕获裸指针
    room.roundTimer.RunAfter(static_cast<double>(windowMs) / 1000.0,
                             [this, battleId]
                             { ResolveRound(battleId); });
}

void BattleRoomManager::ResolveRound(const uint64_t battleId)
{
    auto *room = FindRoom(battleId);
    if (room == nullptr)
    {
        return;
    }
    if (TerminalPending(*room)) return;
    auto visibleBefore = room->engine.BuildStateSnapshot();
    visibleBefore.set_battle_id(battleId);

    // 全员就绪提前结算时,回合 timer 还挂着,先取消防止双重结算
    room->roundTimer.Cancel();

    ::TurnResultS2C result = room->engine.ResolveCurrentRound();
    const auto outcome = room->engine.Outcome();

    if (outcome == ::BATTLE_OUTCOME_ONGOING)
    {
        // 先装填下一回合,让本次广播携带新的行动截止
        ArmRoundTimer(*room);
        result.mutable_state()->set_action_deadline_ms(room->actionDeadlineMs);
    }
    else
    {
        result.mutable_state()->set_action_deadline_ms(0);
    }
    result.set_battle_id(battleId);
    result.mutable_state()->set_battle_id(battleId);
    // 出手序透传(表现规格 D3):引擎按速度排定的 actor_id 序,含本回合被跳过者,
    // 客户端据此播"行动预告条";参战者与观众收同一份 payload
    result.clear_action_order();
    for (const auto actorId : room->engine.LastActionOrder())
    {
        result.add_action_order(actorId);
    }

    // 分出胜负:最后一回合帧连同终局一起等 Match 的持久决定,不提前公布。
    if (outcome != ::BATTLE_OUTCOME_ONGOING)
    {
        BeginResultCommit(*room, outcome, ::SPECTATE_END_BATTLE_FINISHED,
                          std::move(result), std::move(visibleBefore));
        return;
    }
    BroadcastTurnResult(*room, result);
}

void BattleRoomManager::OnBattleDeadline(const uint64_t battleId)
{
    auto *room = FindRoom(battleId);
    if (room == nullptr)
    {
        return;
    }
    if (TerminalPending(*room)) return;

    // 整场 deadline 打满仍未分出胜负:强制平局收尾(battle_node.proto 契约)。
    // 引擎无"强制终局"接口,由节点在结算数据上盖平局章。
    auto outcome = room->engine.Outcome();
    if (outcome == ::BATTLE_OUTCOME_ONGOING)
    {
        outcome = ::BATTLE_OUTCOME_DRAW;
    }
    LOG_WARN << "Battle duration limit reached;forcing completion: battle_id=" << battleId
             << " outcome=" << ::eBattleOutcome_Name(outcome);

    room->roundTimer.Cancel();
    // 观众侧 deadline 强制收尾归 ABORTED(player_battle.proto 枚举注释口径);
    // outcome 带上强制盖章结果,观众端可与参战者的平局结算对齐展示
    BeginResultCommit(*room, outcome, ::SPECTATE_END_BATTLE_ABORTED,
                      std::nullopt, room->engine.BuildStateSnapshot());
}

void BattleRoomManager::ResendBattleConfirmed(const uint64_t battleId)
{
    auto *room = FindRoom(battleId);
    if (room == nullptr)
    {
        return;
    }
    const auto nowMs = TimeSystem::NowMillisecondsUTC();
    if (nowMs >= room->confirmResendUntilMs)
    {
        // 窗口已过:scene 侧锁也已过期,再补发无意义,停表
        room->confirmResendTimer.Cancel();
        return;
    }
    for (const auto &[playerId, routing] : room->routingByPlayer)
    {
        SendBattleConfirmedEvent(routing, playerId, battleId, room->deadlineMs);
    }
    LOG_INFO << "BattleConfirmedEvent periodic resend: battle_id=" << battleId
             << " players=" << room->routingByPlayer.size()
             << " remain_ms=" << (room->confirmResendUntilMs - nowMs);
}

contracts::kafka::BattleResultEvent BattleRoomManager::BuildResult(const BattleRoom& room, const ::eBattleOutcome outcome) const
{
    contracts::kafka::BattleResultEvent event;
    event.set_battle_id(room.battleId);
    event.set_match_mode(room.matchMode);
    event.set_battle_config_id(room.battleConfigId);
    event.set_outcome(outcome);
    event.set_winner_team_index(outcome == ::BATTLE_OUTCOME_SIDE_B_WIN ? 1u : 0u);
    std::map<uint32_t, std::vector<uint64_t>> teams;
    std::vector<uint64_t> fled, dead;
    std::vector<::BattleSettlementData> settlements;
    for (const auto& [playerId, routing] : room.routingByPlayer)
    {
        auto settlement = room.engine.BuildSettlement(playerId);
        teams[settlement.player_team_index()].push_back(playerId);
        event.set_total_rounds(settlement.total_rounds());
        if (settlement.fled()) fled.push_back(playerId);
        if (settlement.is_dead()) dead.push_back(playerId);
        settlements.push_back(std::move(settlement));
    }
    for (const auto& [teamIndex, players] : teams)
    {
        auto* team = event.add_teams();
        team->set_team_index(teamIndex);
        for (const auto player : players) team->add_player_ids(player);
    }
    event.set_finished_at_ms(TimeSystem::NowMillisecondsUTC());
    turnbattle::FillBattleResultActivityFields(room.activityContext, std::move(fled), std::move(dead), event);
    if (!turnbattle::FreezeBattleSettlementSource(event, std::move(settlements)))
        LOG_ERROR << "metric=battle_settlement_source_invalid;terminal remains unknown";
    return event;
}

void BattleRoomManager::BeginResultCommit(BattleRoom& room, const ::eBattleOutcome outcome,
    const ::eSpectateEndReason reason, std::optional<::TurnResultS2C> finalTurn, ::BattleStateS2C visibleBefore)
{
    if (TerminalPending(room)) return;
    room.roundTimer.Cancel(); room.battleTimer.Cancel(); room.confirmResendTimer.Cancel();
    room.actionDeadlineMs = 0;
    visibleBefore.set_battle_id(room.battleId); visibleBefore.set_action_deadline_ms(0);
    room.pendingVisibleState = std::move(visibleBefore);
    room.pendingFinalTurn = std::move(finalTurn); room.pendingSpectateReason = reason;
    room.outcomePending = true;
    room.outcomeSince = std::chrono::steady_clock::now();
    room.pendingResult = BuildResult(room, outcome);
    // 原终局只计算一次；未知回执不允许重新取掉落、时间或目标UUID。
    if (room.pendingResult.settlement_format_version() != 1 ||
        !room.pendingResult.SerializeToString(&room.outcomePayload) || room.outcomePayload.empty()) {
        room.outcomeExhausted = true;
        LOG_ERROR << "metric=battle_sql_outcome_source_invalid;terminal remains unknown";
        // 这间房留在节点上排不空,周期提醒要靠滞留定时器(2026-10-22 评审补)。
        EnsureStuckRoomReporter();
        return;
    }
    match::CommitBattleOutcomeRequest original;
    original.set_battle_id(room.battleId); original.set_placement_operation_id(room.placementOperation);
    original.set_target_instance_id(room.targetInstance); original.set_raw_result_payload(room.outcomePayload);
    room.outcomeRequestDigest = service_call_auth::Digest(original);
    if (room.outcomeRequestDigest.empty()) { room.outcomeExhausted = true; EnsureStuckRoomReporter(); return; }
    sqlOutcomeRooms_.insert(room.battleId);
    if (!sqlOutcomeTimer_.IsActive()) sqlOutcomeTimer_.RunEvery(1.0, [weak = WeakSelf()] {
        if (const auto self = weak.lock()) self->PumpSqlOutcomes();
    });
    PumpSqlOutcomes();
}

void BattleRoomManager::PumpSqlOutcomes()
{
    if (outcomeStopping_) return;
    constexpr std::string_view method = "/match.MatchInternal/CommitBattleOutcome";
    const auto now = std::chrono::steady_clock::now();
    const std::vector<uint64_t> ids(sqlOutcomeRooms_.begin(), sqlOutcomeRooms_.end());
    for (const auto battleId : ids) {
        auto* room = FindRoom(battleId);
        if (!room || !room->outcomePending) { sqlOutcomeRooms_.erase(battleId); continue; }
        // 三段节奏见 result_commit.h 的 SqlOutcomeRetryInterval 说明(2026-10-30):快速重放 → 越界后慢速重放 → 上限后本地删房。
        const auto sinceFrozen = now - room->outcomeSince;
        if (turnbattle::SqlOutcomeReplayAbandoned(sinceFrozen)) {
            // 越界后又慢速重放满 kSqlOutcomeSlowReplaySpan 仍无决定(Match 长期不可达):本地删房。这不是 ABORT,
            // 不发布任何终局;终局仍由 Match 凭持久原件恢复裁决。在途回包到来时 FindRoom 落空,直接丢弃。
            LOG_ERROR << "metric=battle_sql_outcome_abandoned battle_id=" << battleId << " attempts=" << room->outcomeAttempt
                      << ",no SQL decision long after the replay span;room erased locally, match decides from its persisted original";
            CloseDirectConnections(*room, "sql_outcome_abandoned");
            sqlOutcomeRooms_.erase(battleId);
            EraseRoom(battleId);
            continue;
        }
        if (!room->outcomeExhausted && turnbattle::SqlOutcomeSpanExceeded(sinceFrozen)) {
            // 快速重放跨度用尽不是 ABORT:保留冻结可见状态,转为慢速重放(不再移出待提交集合,否则房间永远留在节点上)。
            // Match 对同一原件按持久决定幂等回放,到权威截止后的 ABORT 也由 Match 自己落,慢速重放迟早拿到决定。
            room->outcomeExhausted = true;
            LOG_ERROR << "metric=battle_sql_outcome_unconfirmed battle_id=" << battleId;
            // 之后由滞留定时器每 60s 报 sql_unconfirmed=(ReportStuckOutcomeRooms,2026-10-22 评审补)。
            EnsureStuckRoomReporter();
        }
        if (room->outcomeInFlight || now < room->nextOutcomeAttempt) continue;
        room->nextOutcomeAttempt = now + turnbattle::SqlOutcomeRetryInterval(room->outcomeExhausted);
        const char* secret = std::getenv("MMORPG_MATCH_OUTCOME_SECRET_BATTLE");
        if (!OutcomeSigningKeyReady(secret ? secret : "") || room->targetInstance.empty() || room->placementOperation.empty()) continue;
        auto& registry = tlsNodeContextManager.GetRegistry(common::base::MatchNodeService);
        std::vector<entt::entity> ready;
        // 沿Inbox相同的READY候选随机选择；跨Match副本仍只重放同一SQL原件，不固定首个实例。
        for (const auto node : registry.view<NodeInfo, MuduoGrpcChannelPtr>()) {
            const auto& channel = registry.get<MuduoGrpcChannelPtr>(node);
            if (channel && channel->GetState(true) == muduo_grpc::ConnectivityState::kReady) ready.push_back(node);
        }
        if (ready.empty() || room->outcomeAttempt == std::numeric_limits<uint64_t>::max()) continue;
        const auto target = ready[tlsRandom.Rand<std::size_t>(0, ready.size() - 1)];
        match::CommitBattleOutcomeRequest request;
        request.set_battle_id(battleId); request.set_placement_operation_id(room->placementOperation);
        request.set_target_instance_id(room->targetInstance); request.set_raw_result_payload(room->outcomePayload);
        if (service_call_auth::Digest(request) != room->outcomeRequestDigest ||
            !service_call_auth::Sign(method, request, *request.mutable_auth(), "battle", secret, TimeSystem::NowMillisecondsUTC())) continue;
        const auto attempt = ++room->outcomeAttempt;
        const auto op = room->placementOperation, instance = room->targetInstance, digest = room->outcomeRequestDigest;
        room->outcomeInFlight = true;
        match::SendMatchInternalCommitBattleOutcome(registry, target, request,
            [weak = WeakSelf(), battleId, attempt, op, instance, digest](const GrpcCallOutcome& outcome,
                const match::CommitBattleOutcomeResponse* reply) {
                const auto self = weak.lock();
                if (!self) return;
                if (self->outcomeStopping_) {
                    LOG_INFO << "event=battle_outcome_reply_after_stop battle_id=" << battleId
                             << " attempt=" << attempt << " code=" << outcome.code
                             << " reply_present=" << (reply != nullptr)
                             << " digest_matches=" << (reply && reply->request_digest() == digest);
                    return;
                }
                auto* current = self->FindRoom(battleId);
                if (!current || !current->outcomePending || current->outcomeAttempt != attempt ||
                    current->placementOperation != op || current->targetInstance != instance ||
                    current->outcomeRequestDigest != digest) return;
                current->outcomeInFlight = false;
                const char* key = std::getenv("MMORPG_MATCH_OUTCOME_SECRET_BATTLE");
                if (!outcome.ok() || !reply || !key || reply->request_digest() != digest ||
                    !service_call_auth::Verify("/match.MatchInternal/CommitBattleOutcome/reply", *reply,
                        reply->auth(), "match", key, TimeSystem::NowMillisecondsUTC())) return;
                if (reply->decision() == match::BATTLE_OUTCOME_DECISION_COMPLETE) {
                    self->OnSqlOutcomeComplete(*current);
                } else if (reply->decision() == match::BATTLE_OUTCOME_DECISION_ABORT) {
                    self->OnSqlOutcomeAbort(*current);
                } // UNKNOWN只重放同件。
            });
    }
    if (sqlOutcomeRooms_.empty()) sqlOutcomeTimer_.Cancel();
}

void BattleRoomManager::OnSqlOutcomeComplete(BattleRoom &room)
{
    room.outcomeComplete = true;
    if (!FinishBattle(room)) return;
    RetireSqlOutcomeRoom(room.battleId);
}

void BattleRoomManager::OnSqlOutcomeAbort(BattleRoom &room)
{
    // 唯有Match持久ABORT才可宣布作废，绝不生成任何个人效果/来源。
    NotifySpectateEndAndClose(room, ::SPECTATE_END_BATTLE_ABORTED, ::BATTLE_OUTCOME_ONGOING);
    CloseDirectConnections(room, "sql_aborted");
    RetireSqlOutcomeRoom(room.battleId);
}

void BattleRoomManager::RetireSqlOutcomeRoom(const uint64_t battleId)
{
    sqlOutcomeRooms_.erase(battleId);
    EraseRoom(battleId);
    if (sqlOutcomeRooms_.empty()) sqlOutcomeTimer_.Cancel();
}

bool BattleRoomManager::FinishBattle(BattleRoom &room)
{
    if (!room.outcomeComplete) return false;
    room.roundTimer.Cancel();
    room.battleTimer.Cancel();
    room.confirmResendTimer.Cancel();

    // 已确认的完整来源是唯一的终局原件；不得在决定之后再次计算掉落或终值。
    const auto &result = room.pendingResult;
    std::set<uint64_t> settled;
    for (const auto& settlement : result.settlements()) settled.insert(settlement.player_id());
    if (settled.size() != room.routingByPlayer.size())
    {
        LOG_ERROR << "metric=battle_settlement_source_missing;no terminal dispatched";
        return false;
    }
    for (const auto& [playerId, routing] : room.routingByPlayer)
        if (!settled.contains(playerId))
        {
            LOG_ERROR << "metric=battle_settlement_source_member_mismatch;no terminal dispatched";
            return false;
        }
    if (room.pendingFinalTurn) BroadcastTurnResult(room, *room.pendingFinalTurn);

    // 个人 BattleEndS2C 与资产可见性只由 Scene 在同 snapshot 双 SQL 确认之后通知;Battle 只发布公共终局帧。
    NotifySpectateEndAndClose(room, room.pendingSpectateReason, result.outcome());
    // 终局包已全部排队,直连从此没有存在意义:shutdown 让它们先 flush 再 FIN(D23)
    CloseDirectConnections(room, "battle_finished");

    LOG_INFO << "Battle ended: battle_id=" << room.battleId
             << " outcome=" << ::eBattleOutcome_Name(result.outcome())
             << " players=" << room.routingByPlayer.size()
             << " teams=" << result.teams_size()
             << " total_rounds=" << result.total_rounds()
             << " fled=" << result.fled_player_ids_size()
             << " dead=" << result.dead_player_ids_size()
             << " activity_kind=" << ::eBattleActivityKind_Name(room.activityContext.kind());
    return true;
}

void BattleRoomManager::BroadcastTurnResult(const BattleRoom &room, const ::TurnResultS2C &result)
{
    // 战斗帧只走直连(turn-based §22 D68):每玩家一条,同一条 TCP 连接天然保证
    // TurnResult 先于随后的 BattleEnd;没有活直连的玩家这一帧直接丢弃,直连就绪后 GetBattleState 补拉。
    // 每人一份拷贝:事件流(events/action_order)人人相同,只有 state 按视角裁剪(G7)。
    // 本来就是每个收信人各序列化一次,这里只多一次消息对象拷贝。
    for (const auto &[playerId, routing] : room.routingByPlayer)
    {
        ::TurnResultS2C personal = result;
        RedactStateForViewer(*personal.mutable_state(), playerId);
        FillSelfItems(room, *personal.mutable_state(), playerId);
        PushBattleFrame(room, playerId, BattleClientPlayerNotifyTurnResultMessageId, personal);
    }
    // 观众版只做一份(全员冷却都剔除、不带任何人的道具余量),消息号不同
    //(D8:客户端观战/参战两套状态机互不干扰);观众帧与收信人无关,同一份信封写给 L0 观众与中继 / 广播通道
    if (room.HasSpectateAudience())
    {
        ::TurnResultS2C spectate = result;
        RedactStateForViewer(*spectate.mutable_state(), 0);
        const auto envelope = ClientEnvelope(BattleClientPlayerNotifySpectateTurnResultMessageId, spectate);
        PushSpectatorFrame(room, envelope);
        PublishSpectateTurn(room, result.round_index(), envelope);
    }
}

void BattleRoomManager::RedactStateForViewer(::BattleStateS2C &state,
                                             const uint64_t viewerPlayerId) const
{
    // 道具余量是私有信息:统一在这里清掉,再由 FillSelfItems 只给本人回填。
    // 不依赖"BuildStateSnapshot 恰好不填 self_items"这一外部事实 —— 那是别处的实现细节。
    state.clear_self_items();
    // 战斗类属性(装备带来的必杀 / 连击 / 反击 / 反震 / 抗性概率)只供引擎判定:不分视角全员清掉,
    // 自己的看属性面板(equipment-attributes.md §4.5)。所有出站状态(开战包 / 回合结果 / 补拉 /
    // 观战首帧)都经过本函数,新增下发路径必须同样先过这里。
    turnbattle::TurnBattleEngine::StripEngineOnlyState(state);
    for (auto &actor : *state.mutable_actors())
    {
        // "自己的单位" = 本人 + 本人的宝宝(宝宝 actor_id 是局内号,归属看 owner_player_id)
        const bool isOwn = viewerPlayerId != 0 &&
                           (actor.actor_id() == viewerPlayerId ||
                            actor.owner_player_id() == viewerPlayerId);
        if (!isOwn)
        {
            actor.clear_skill_cooldown_rounds();
        }
    }
}

void BattleRoomManager::FillSelfItems(const BattleRoom &room, ::BattleStateS2C &state,
                                      const uint64_t viewerPlayerId) const
{
    state.clear_self_items();
    if (viewerPlayerId == 0)
    {
        return;
    }
    for (const auto &item : room.engine.SelfItems(viewerPlayerId))
    {
        *state.add_self_items() = item;
    }
}

::SpectateStateS2C BattleRoomManager::BuildSpectatorState(const BattleRoom &room, const uint32_t observerCount) const
{
    ::SpectateStateS2C state;
    *state.mutable_state() = room.pendingVisibleState ? *room.pendingVisibleState : room.engine.BuildStateSnapshot();
    state.mutable_state()->set_battle_id(room.battleId);
    // 引擎无时钟,行动截止由节点按房间 timer 回填(与参战者首帧同口径)
    state.mutable_state()->set_action_deadline_ms(room.actionDeadlineMs);
    // 观众视角:全员冷却剔除,不带任何人的道具余量(G7)
    RedactStateForViewer(*state.mutable_state(), 0);
    state.set_observer_count(observerCount);
    return state;
}

void BattleRoomManager::PushSpectateState(const BattleRoom &room, const uint64_t observerId) const
{
    // 只数有活直连的观众:待连接 / 断线宽限中的席位不算"正在看"(满员判定另按名单人数)
    PushBattleFrame(room, observerId, BattleClientPlayerNotifySpectateStateMessageId,
                    BuildSpectatorState(room, LiveObserverCount(room)));
}

uint32_t BattleRoomManager::LiveObserverCount(const BattleRoom &room) const
{
    uint32_t count = 0;
    for (const auto &[observerId, routing] : room.routingByObserver)
    {
        if (LiveDirectConnOf(room, observerId) != nullptr)
        {
            ++count;
        }
    }
    return count;
}

size_t BattleRoomManager::RegisteredObserverSeats() const
{
    size_t seats = 0;
    for (const auto battleId : rooms_.Ids())
    {
        if (const auto *room = rooms_.Find(battleId); room != nullptr)
        {
            seats += room->routingByObserver.size();
        }
    }
    return seats;
}

void BattleRoomManager::NotifySpectateEndAndClose(BattleRoom &room,
                                                  const ::eSpectateEndReason reason,
                                                  const ::eBattleOutcome outcome)
{
    // 房间收尾 / 作废:观众席不再回收(房间对象析构时定时器也会取消,这里显式收口)
    room.observerSeatTimer.Cancel();
    if (!room.HasSpectateAudience())
    {
        return;
    }

    ::SpectateEndS2C end;
    end.set_battle_id(room.battleId);
    end.set_outcome(outcome);
    end.set_reason(reason);
    // 同一帧只序列化一次:中继 / 广播通道与 L0 观众共用这份信封,reason 同值
    const auto envelope = ClientEnvelope(BattleClientPlayerNotifySpectateEndMessageId, end);
    PublishSpectateEnd(room, envelope);
    if (room.routingByObserver.empty())
    {
        return;
    }

    // 先抄名单再逐个摘除:RetireObserver 会改 routingByObserver
    std::vector<uint64_t> observers;
    observers.reserve(room.routingByObserver.size());
    for (const auto &[observerId, routing] : room.routingByObserver)
    {
        observers.push_back(observerId);
    }
    // 先把同一帧终局包推给全部观众(编码一次),再逐个摘除。关闭推迟到本轮 loop 之后(shutdown 等输出缓冲
    // 排空再 FIN),每条直连上仍是终局包先于 FIN,观众先看到结束原因。没有直连的观众收不到它(战斗帧不回落,§22 D68)
    PushSpectatorFrame(room, envelope);
    for (const auto observerId : observers)
    {
        RetireObserver(room, observerId, "spectate_end");
    }

    LOG_INFO << "Spectators removed: battle_id=" << room.battleId
             << " observers=" << observers.size()
             << " reason=" << ::eSpectateEndReason_Name(reason);

    // 防御:各表只经 RetireObserver 成套增删,这里应已为空;清一遍保证此后任何广播都打不到观众
    room.routingByObserver.clear();
    room.observerNames.clear();
    room.observerOperations.clear();
    room.observerSeatDeadlines.clear();
}

void BattleRoomManager::RetireObserver(BattleRoom &room, const uint64_t observerId, const char *reason)
{
    if (const auto active = room.observerOperations.find(observerId); active != room.observerOperations.end())
    {
        // 封墓碑:原准入截止已过 → 迟到的同 op Add 会被 HandleAddObserver 的时间校验挡住,墓碑可删;
        // 未过 → 置 cancelled,同 op 迟到的 Add 不能翻成准入。墓碑不存在(已被删)时无事可做。
        const auto operation = spectatorOperations_.find(std::make_tuple(room.battleId, observerId, active->second));
        if (operation != spectatorOperations_.end())
        {
            if (operation->second.admitBeforeMs <= TimeSystem::NowMillisecondsUTC())
            {
                spectatorOperations_.erase(operation);
            }
            else
            {
                operation->second.cancelled = true;
            }
        }
        room.observerOperations.erase(active);
    }
    room.routingByObserver.erase(observerId);
    room.observerNames.erase(observerId);
    room.observerSeatDeadlines.erase(observerId);
    CloseDirectConnectionOf(room, observerId, reason);
}

void BattleRoomManager::StartObserverSeatGrace(BattleRoom &room, const uint64_t observerId, const bool reconnect)
{
    const auto grace = reconnect ? kObserverReconnectGrace : kObserverConnectGrace;
    room.observerSeatDeadlines[observerId] =
        BattleRoom::ObserverSeatDeadline{std::chrono::steady_clock::now() + grace, reconnect};
    ArmObserverSeatTimer(room);
}

void BattleRoomManager::ArmObserverSeatTimer(BattleRoom &room)
{
    if (room.observerSeatDeadlines.empty())
    {
        room.observerSeatTimer.Cancel();
        return;
    }
    auto earliest = room.observerSeatDeadlines.begin()->second.at;
    for (const auto &[observerId, deadline] : room.observerSeatDeadlines)
    {
        if (deadline.at < earliest)
        {
            earliest = deadline.at;
        }
    }
    // 已过期限按 0 延迟挂(本轮 loop 之后回收)。muduo 定时器的到期时钟与 steady_clock 不一定同源
    // (Linux 版按墙钟判定到期),与单调期限可能有偏差:早到时回收入口按单调时钟判定,没到的留着并重挂,
    // 不会提前回收;墙钟回拨只会让回收迟到。
    const double remainingSec = std::chrono::duration<double>(earliest - std::chrono::steady_clock::now()).count();
    const double delaySec = remainingSec > 0.0 ? remainingSec : 0.0;
    const bool armed = room.observerSeatTimer.RunAfter(
        delaySec,
        [weakSelf = WeakSelf(), battleId = room.battleId]
        {
            if (const auto self = weakSelf.lock())
            {
                self->ReclaimExpiredObserverSeats(battleId, std::chrono::steady_clock::now());
            }
        });
    if (!armed)
    {
        // 只可能是当前线程没有 EventLoop(装配错误):席位不会到期回收,响亮报出来
        LOG_ERROR << "observer seat timer arm failed: battle_id=" << room.battleId
                  << " pending_seats=" << room.observerSeatDeadlines.size();
    }
}

void BattleRoomManager::ReclaimExpiredObserverSeats(const uint64_t battleId,
                                                    const std::chrono::steady_clock::time_point now)
{
    auto *room = FindRoom(battleId);
    if (room == nullptr)
    {
        // 定时器迟到打到已销毁的房间:观众早已随收尾 / 作废路径清退
        return;
    }
    // 先抄到期名单再摘除:RetireObserver 会改期限表
    std::vector<std::pair<uint64_t, bool>> expired;
    for (const auto &[observerId, deadline] : room->observerSeatDeadlines)
    {
        if (deadline.at <= now)
        {
            expired.emplace_back(observerId, deadline.reconnect);
        }
    }
    for (const auto &[observerId, reconnect] : expired)
    {
        // battle 没有 Prometheus 端点,按 metric= 行计数、按 cause 聚合;battle_id / observer 只供排查,不作标签
        LOG_INFO << "metric=battle_observer_seat_reclaimed cause="
                 << (reconnect ? "reconnect_timeout" : "connect_timeout")
                 << " battle_id=" << battleId << " observer=" << observerId;
        RetireObserver(*room, observerId, "observer_seat_reclaimed");
    }
    ArmObserverSeatTimer(*room);
}

void BattleRoomManager::AbortAllRooms(const std::string &reason)
{
    // 终局在这里不停(2026-10-23 修复,审计 r2-battle#2):已经算出、还没拿到 SQL 决定的终局,在停机排空窗口内立即再提交,
    // main.cpp 的 SetShutdownDrainComplete(SqlOutcomesFlushed)等它们拿到 COMPLETE / ABORT,有界于 Node 的排空预算;
    // 停止闸到拆除前钩子(StopSqlOutcomes)才装。原来这里直接装停止闸、清空待提交集合:除了恰好在途的那一次,
    // 停机前一刻打完的局再没有机会提交,只能等 match 截止后判 ABORT,全员白打一局。
    // 不打断在途的那一次:它的回复(或 DefaultCallDeadline 到期的失败)回来之后照常按 5s 节奏续发。
    std::size_t flushing = 0;
    for (const auto battleId : sqlOutcomeRooms_)
    {
        auto *room = FindRoom(battleId);
        if (room == nullptr || !turnbattle::NeedsShutdownOutcomeFlush(room->outcomePending, room->outcomeComplete,
                                                                      room->outcomeExhausted))
            continue;
        if (!room->outcomeInFlight) room->nextOutcomeAttempt = {};
        ++flushing;
    }
    LOG_INFO << "event=battle_outcome_shutdown_flush pending=" << flushing << " reason=" << reason;
    if (flushing != 0) PumpSqlOutcomes();
    if (rooms_.Empty())
    {
        return;
    }

    LOG_INFO << "Closing all battle rooms: rooms=" << rooms_.Size() << " reason=" << reason;
    for (const auto battleId : rooms_.Ids())
    {
        auto *room = FindRoom(battleId);
        if (room == nullptr)
        {
            continue;
        }
        room->roundTimer.Cancel();
        room->battleTimer.Cancel();
        room->confirmResendTimer.Cancel();
        // 正常停机不铸造ABORT，也不把UNKNOWN房间报已移除/Ready；Match凭原preflight恢复裁决。
        // 原loop关闭后在途回包可能失败；保留房间直到manager销毁，不再发布任何终局。
        // 2026-10-23:上面补提交的房间若在排空窗口内拿到 COMPLETE / ABORT,由回复处理照常收尾删房(lifecycle 已在
        // Stop 之后,删房只记账、不回 Ready);还在打的房间没有终局可交,按作废处理,match 在权威截止后判 ABORT。
        CloseDirectConnections(*room, reason.c_str());
    }
}

bool BattleRoomManager::SqlOutcomesFlushed() const
{
    for (const auto battleId : sqlOutcomeRooms_)
    {
        const auto *room = rooms_.Find(battleId);
        if (room != nullptr && turnbattle::NeedsShutdownOutcomeFlush(room->outcomePending, room->outcomeComplete,
                                                                     room->outcomeExhausted))
            return false;
    }
    return true;
}

void BattleRoomManager::StopSqlOutcomes()
{
    outcomeStopping_ = true;
    LOG_INFO << "event=battle_outcome_stop_installed pending=" << sqlOutcomeRooms_.size();
    // 排空预算用完仍没拿到 SQL 决定的终局逐局留痕:battle 这边再没有机会提交,match 在权威截止后原子判 ABORT,
    // 全员作废(除非某次在途提交其实已在 match 落库,那时是 COMPLETE,由 match 发件箱照常补发)。
    // 对账按 battle_id 查 match_battle_completion;battle_id 只进正文,不作 label。
    for (const auto battleId : sqlOutcomeRooms_)
    {
        const auto *room = rooms_.Find(battleId);
        if (room == nullptr || !turnbattle::NeedsShutdownOutcomeFlush(room->outcomePending, room->outcomeComplete,
                                                                      room->outcomeExhausted))
            continue;
        LOG_ERROR << "metric=battle_sql_outcome_unflushed_at_shutdown battle_id=" << battleId
                  << " attempts=" << room->outcomeAttempt
                  << ",terminal not confirmed before exit;match decides after the authority deadline"
                  << "(ABORT for every member unless an in-flight commit already landed)";
    }
    sqlOutcomeTimer_.Cancel();
    sqlOutcomeRooms_.clear();
}

// ---- 客户端直连(设计文档 §18,D23-D28;下行出口 turn-based §22 D68) ----

void BattleRoomManager::SetClientEdge(BattleClientEdge *edge)
{
    if (edge == nullptr)
    {
        edge_.reset();
        return;
    }
    edge_ = std::ref(*edge);
    // 观众配额是直连面连接预算的一部分,随借用直连面注入一次(调用时机见头文件)。
    const size_t effectiveMax = edge->EffectiveMaxConnections();
    SetObserverSeatQuota(ObserverSeatQuotaFor(effectiveMax));
    LOG_INFO << "battle observer seat quota set: quota=" << observerSeatQuota_
             << " effective_max_connections=" << effectiveMax;
}

muduo::net::TcpConnectionPtr BattleRoomManager::LiveDirectConnOf(const BattleRoom &room,
                                                                 const uint64_t playerId) const
{
    if (!edge_)
    {
        return nullptr;
    }
    const auto it = room.directConnByPlayer.find(playerId);
    if (it == room.directConnByPlayer.end())
    {
        return nullptr;
    }
    // 锁不住 == 直连已断 == 缺项(AGENTS.md §11.7:只存 weak,用时 lock)
    auto conn = it->second.lock();
    if (!conn || !conn->connected())
    {
        return nullptr;
    }
    return conn;
}

namespace
{
    // 经已验证的直连写一条 S2C。
    void SendOverDirect(const BattleClientEdge &edge, const muduo::net::TcpConnectionPtr &conn,
                        const uint32_t messageId, const ::google::protobuf::Message &message)
    {
        edge.Send(conn, ClientEnvelope(messageId, message));
    }
} // namespace

muduo::net::TcpConnectionPtr BattleRoomManager::BattleFrameConnOf(const BattleRoom &room, const uint64_t playerId,
                                                                  const uint32_t messageId) const
{
    auto conn = LiveDirectConnOf(room, playerId);
    switch (battle_push_policy::Decide(battle_push_policy::PushCategory::kBattleFrame, conn != nullptr))
    {
    case battle_push_policy::PushRoute::kDirect:
        return conn;
    case battle_push_policy::PushRoute::kViaGate:
        // 战斗帧的判定不会产出 kViaGate(tests/battle_push_policy_test.cpp 钉住);
        // 本出口也没有路由可回落,防御性按丢弃处理
    case battle_push_policy::PushRoute::kDrop:
        break;
    }
    LogBattleFrameDroppedSampled(room.battleId, playerId, messageId);
    return nullptr;
}

void BattleRoomManager::PushBattleFrame(const BattleRoom &room, const uint64_t playerId,
                                        const uint32_t messageId,
                                        const ::google::protobuf::Message &message) const
{
    if (const auto conn = BattleFrameConnOf(room, playerId, messageId))
    {
        SendOverDirect(edge_->get(), conn, messageId, message);
    }
}

void BattleRoomManager::PushSpectatorFrame(const BattleRoom &room, const ::MessageContent &envelope) const
{
    if (room.routingByObserver.empty())
    {
        return;
    }
    // 一帧只分帧一次,逐条直连写出同一份字节(共享字节的写法约束见 BattleClientEdge::SendEncoded)
    const auto frame = BattleClientEdge::EncodeFrame(envelope);
    for (const auto &[observerId, routing] : room.routingByObserver)
    {
        if (const auto conn = BattleFrameConnOf(room, observerId, envelope.message_id()))
        {
            edge_->get().SendEncoded(conn, frame);
        }
    }
}

void BattleRoomManager::PushLobbyAnnouncement(const BattleRoom &room, const uint64_t playerId,
                                              const ::BattleRouting &routing, const uint32_t messageId,
                                              const ::google::protobuf::Message &message) const
{
    const auto conn = LiveDirectConnOf(room, playerId);
    switch (battle_push_policy::Decide(battle_push_policy::PushCategory::kLobbyAnnouncement,
                                       conn != nullptr))
    {
    case battle_push_policy::PushRoute::kDirect:
        SendOverDirect(edge_->get(), conn, messageId, message);
        return;
    case battle_push_policy::PushRoute::kViaGate:
        PushMessageViaGate(routing, playerId, messageId, message);
        return;
    case battle_push_policy::PushRoute::kDrop:
        // 大厅公告的判定不会产出 kDrop(单测钉住);真出现说明判定契约被改坏了,必须响
        LOG_ERROR << "battle lobby announcement marked for discard(battle_push_policy contract violation): battle_id=" << room.battleId
                  << " player_id=" << playerId << " message_id=" << messageId;
        return;
    }
}

bool BattleRoomManager::BuildAssignment(const BattleRoom &room, const uint64_t playerId,
                                        const ::eBattleTicketRole role, ::BattleAssignedS2C &out,
                                        std::string_view spectatorOperation) const
{
    const auto &deploy = gNodeConfigManager.GetBaseDeployConfig();
    const auto &secret = deploy.battle_token_secret();
    const auto verdict = battle_security::ClassifyTokenSecret(secret, battle_security::CurrentRunMode());
    if (verdict == battle_security::TokenSecretVerdict::kRefuse)
    {
        // 启动门禁已拒过一次;这里是纵深防御,不签空签名的票
        LOG_ERROR << "battle ticket issuance rejected: battle_token_secret is empty and run_mode=prod, battle_id="
                  << room.battleId << " player_id=" << playerId;
        return false;
    }
    if (gNode == nullptr)
    {
        return false;
    }
    // NodeInfo 由 Node 持有、只在 loop 线程写;跨线程读取不与写入同步。
    // 本函数仍只在 loop 线程执行(Handle* 由 muduo_grpc 服务端在 loop 线程上派发)。
    const auto &self = gNode->GetNodeInfo();
    // 客户端可达地址(集群外入口 D76/D78,规则唯一权威实现在 client_endpoint::ClientFacing):
    // client_endpoint 可用就用它;否则 required=false 时回落 endpoint(podip 形态),
    // required=true 或 endpoint 本身不可用时拿不到 —— 拒签,调用方按 D70 fail-closed 回 kServiceUnavailable。
    const bool required = gNode->ClientEndpointRequired();
    const auto clientFacing = client_endpoint::ClientFacing(self, required);
    if (!clientFacing)
    {
        LOG_ERROR << "battle ticket issuance rejected: this node has no client-reachable address, battle_id=" << room.battleId
                  << " player_id=" << playerId
                  << " endpoint=" << self.endpoint().ip() << ":" << self.endpoint().port()
                  << " client_endpoint=" << self.client_endpoint().ip() << ":" << self.client_endpoint().port()
                  << " required=" << (required ? 1 : 0);
        return false;
    }

    ::BattleTicketPayload payload;
    payload.set_battle_id(room.battleId);
    payload.set_player_id(playerId);
    payload.set_battle_node_id(self.node_id());
    payload.set_battle_instance_id(self.node_uuid());
    payload.set_expire_at_ms(room.deadlineMs);
    payload.set_role(role);
    if (role == ::BATTLE_TICKET_ROLE_OBSERVER) {
        if (spectatorOperation.empty()) {
            const auto original = room.observerOperations.find(playerId);
            if (original == room.observerOperations.end()) return false;
            spectatorOperation = original->second;
        }
        if (!ValidSpectatorOperation(spectatorOperation)) return false;
        payload.set_spectator_operation_id(std::string(spectatorOperation));
    } else if (!spectatorOperation.empty()) return false;
    const std::string payloadBytes = payload.SerializeAsString();

    std::string signature;
    if (verdict == battle_security::TokenSecretVerdict::kEnforce)
    {
        signature = battle_security::SignTicket(secret, payloadBytes);
        if (signature.empty())
        {
            LOG_ERROR << "battle ticket signing failed(OpenSSL), battle_id=" << room.battleId
                      << " player_id=" << playerId;
            return false;
        }
    }
    // dev/test 空密钥:签名留空,直连面按同一判据跳过比对(battle_client_edge.cpp)

    out.Clear();
    out.set_battle_id(room.battleId);
    out.set_host(clientFacing->ip());
    out.set_port(clientFacing->port());
    out.set_token_payload(payloadBytes);
    out.set_token_signature(signature);
    out.set_expire_at_ms(room.deadlineMs);
    out.set_role(role);
    return true;
}

void BattleRoomManager::PushAssignment(const BattleRoom &room, const uint64_t playerId,
                                       const ::BattleRouting &routing,
                                       const ::BattleAssignedS2C &assigned) const
{
    // 票据已由调用方预签(签不出时调用方已拒绝开局 / 拒绝观战,turn-based §22 D70)
    PushLobbyAnnouncement(room, playerId, routing, BattleClientPlayerNotifyBattleAssignedMessageId,
                          assigned);
}

bool BattleRoomManager::OwnsDirectConnection(uint64_t battleId, uint64_t playerId, ::eBattleTicketRole role,
    const muduo::net::TcpConnectionPtr &conn, std::string_view spectatorOperation)
{
    const auto* room = FindRoom(battleId);
    if (room == nullptr || !conn) return false;
    const auto direct = room->directConnByPlayer.find(playerId);
    if (direct == room->directConnByPlayer.end() || direct->second.lock() != conn) return false;
    if (role == BATTLE_TICKET_ROLE_PARTICIPANT) return spectatorOperation.empty() && room->routingByPlayer.count(playerId) != 0;
    const auto original = room->observerOperations.find(playerId);
    return role == BATTLE_TICKET_ROLE_OBSERVER && original != room->observerOperations.end()
        && original->second == spectatorOperation && room->routingByObserver.count(playerId) != 0;
}

bool BattleRoomManager::AttachDirectConnection(const uint64_t battleId, const uint64_t playerId,
                                               const ::eBattleTicketRole role,
                                               const muduo::net::TcpConnectionPtr &conn,
                                               uint32_t *gateSessionId, std::string_view spectatorOperation)
{
    auto *room = FindRoom(battleId);
    if (room == nullptr || !conn)
    {
        return false;
    }

    // 按票据角色核对名单:参战票只认参战名单,观众票只认观众名单(票不能换角色用)
    const ::BattleRouting *routing = nullptr;
    if (role == ::BATTLE_TICKET_ROLE_PARTICIPANT)
    {
        if (!spectatorOperation.empty()) return false;
        if (const auto it = room->routingByPlayer.find(playerId); it != room->routingByPlayer.end())
        {
            routing = &it->second;
        }
    }
    else if (role == ::BATTLE_TICKET_ROLE_OBSERVER)
    {
        const auto original = room->observerOperations.find(playerId);
        if (original == room->observerOperations.end() || spectatorOperation.empty() || original->second != spectatorOperation) return false;
        if (const auto it = room->routingByObserver.find(playerId); it != room->routingByObserver.end())
        {
            routing = &it->second;
        }
    }
    if (routing == nullptr)
    {
        LOG_WARN << "battle direct connection attachment rejected: player not in roster, battle_id=" << battleId
                 << " player_id=" << playerId << " role=" << ::eBattleTicketRole_Name(role);
        return false;
    }

    auto &slot = room->directConnByPlayer[playerId];
    if (const auto old = slot.lock(); old && old != conn)
    {
        // 重连:先关旧的。forceClose 是 queueInLoop 异步的,旧连接的断开回调在本函数
        // 返回后才跑,那时 slot 已指向新连接,DetachDirectConnection 按身份比对不会误摘。
        LOG_INFO << "battle direct connection reconnected;replacing old connection: battle_id=" << battleId << " player_id=" << playerId
                 << " old_peer=" << old->peerAddress().toIpPort()
                 << " new_peer=" << conn->peerAddress().toIpPort();
        old->forceClose();
    }
    slot = conn;
    if (role == ::BATTLE_TICKET_ROLE_OBSERVER)
    {
        // 挂上直连即停止席位计时(连接宽限 / 重连宽限)。不重挂定时器:早到一次由回收入口重挂
        room->observerSeatDeadlines.erase(playerId);
    }
    if (gateSessionId != nullptr)
    {
        *gateSessionId = routing->session_id();
    }
    return true;
}

void BattleRoomManager::DetachDirectConnection(const uint64_t battleId, const uint64_t playerId,
                                               const muduo::net::TcpConnectionPtr &conn)
{
    auto *room = FindRoom(battleId);
    if (room == nullptr)
    {
        return;
    }
    const auto it = room->directConnByPlayer.find(playerId);
    if (it != room->directConnByPlayer.end() && it->second.lock() == conn)
    {
        room->directConnByPlayer.erase(it);
        // 观众断线:仍在名单上就开始重连宽限,期内没重连即回收席位。
        // 主动摘除的路径(CloseDirectConnectionOf / CloseDirectConnections)先删表项,走不到这里。
        if (room->routingByObserver.count(playerId) != 0)
        {
            StartObserverSeatGrace(*room, playerId, true);
        }
    }
}

void BattleRoomManager::OnDirectConnectionVerified(const uint64_t battleId, const uint64_t playerId,
                                                   const ::eBattleTicketRole role)
{
    // 参战者:客户端直连就绪时自己 GetBattleState 补拉(robot 与 Unity 同口径),这里不推
    if (role != ::BATTLE_TICKET_ROLE_OBSERVER)
    {
        return;
    }
    const auto *room = FindRoom(battleId);
    if (room == nullptr || room->routingByObserver.find(playerId) == room->routingByObserver.end())
    {
        return;
    }
    // snapshot on connect(turn-based §22 D69):观战首帧只随直连下发。刚挂接的直连就在
    // directConnByPlayer 里,战斗帧出口会直发;万一握手应答写出后连接已断,照常按无直连丢弃,
    // 客户端重连握手时再推一次。
    PushSpectateState(*room, playerId);
}

namespace
{
    // 推迟到本轮 loop 之后再关:shutdown() 与 forceCloseWithDelay() 都会**同步**把连接切到
    // kDisconnecting(muduo TcpConnection.cc),之后 connected()==false、send 一律丢弃。而
    // 调用本函数的 Handle*(StopWatchBattle / 打出最后一击的 SubmitBattleAction / SetAutoBattle)
    // 的应答要等 Handle* 返回后由直连面写出。queueInLoop 保证:应答先入输出缓冲 → shutdown
    // 等排空再 FIN → 1s 后强关兜底不配合的对端。终局包(PushBattleFrame)早已在缓冲里,顺序不变。
    void ShutdownDirectConnAfterThisLoop(const muduo::net::TcpConnectionPtr &conn)
    {
        if (!conn)
        {
            return;
        }
        std::weak_ptr<muduo::net::TcpConnection> weak = conn;
        conn->getLoop()->queueInLoop([weak]
                                     {
            if (const auto c = weak.lock(); c && c->connected())
            {
                c->shutdown();
                c->forceCloseWithDelay(1.0);
            } });
    }
} // namespace

void BattleRoomManager::CloseDirectConnectionOf(BattleRoom &room, const uint64_t playerId, const char *reason)
{
    const auto it = room.directConnByPlayer.find(playerId);
    if (it == room.directConnByPlayer.end())
    {
        return;
    }
    if (const auto conn = it->second.lock(); conn && conn->connected())
    {
        LOG_INFO << "battle closing player direct connection: battle_id=" << room.battleId << " player_id=" << playerId
                 << " reason=" << reason;
        ShutdownDirectConnAfterThisLoop(conn);
    }
    // 表里立刻摘除:从此该玩家视为无活直连(战斗帧丢弃、大厅公告回落 Kafka→gate),
    // 不再往一条正在关闭的连接上写
    room.directConnByPlayer.erase(it);
}

void BattleRoomManager::CloseDirectConnections(BattleRoom &room, const char *reason)
{
    if (room.directConnByPlayer.empty())
    {
        return;
    }
    LOG_INFO << "battle closing all room direct connections: battle_id=" << room.battleId
             << " count=" << room.directConnByPlayer.size() << " reason=" << reason;
    for (auto &[playerId, weakConn] : room.directConnByPlayer)
    {
        if (const auto conn = weakConn.lock(); conn && conn->connected())
        {
            ShutdownDirectConnAfterThisLoop(conn);
        }
    }
    room.directConnByPlayer.clear();
}

// ---- 滞留房间提醒 ----

void BattleRoomManager::EnsureStuckRoomReporter()
{
    if (stuckRoomReportTimer_.IsActive())
    {
        return;
    }
    // 只绑 weak_ptr<自己>(AGENTS.md §11.7)。
    stuckRoomReportTimer_.RunEvery(kStuckRoomReportIntervalSec,
                                   [weakSelf = WeakSelf()]
                                   {
                                       if (const auto self = weakSelf.lock())
                                       {
                                           self->ReportStuckOutcomeRooms();
                                       }
                                   });
}

void BattleRoomManager::ReportStuckOutcomeRooms()
{
    // 每轮从房间表现数一遍,不另记集合:房间表是唯一真相,房间被清掉后计数自然归零。一次性告警过了值班查询窗口就再没有信号。
    // battle_id 只放日志正文,不写成 key=value 字段(LogQL 解析字段会把它变成高基数 label)。
    // node_rooms = 本节点房间总数:值班打排空标签后等它降到与 stuck_total 相等,说明其余房间都已打完,再删 GameServer。
    // 房间数受节点容量约束,一行列全即可。metric 名沿用已登记的值班查询。
    std::size_t stuckTotal = 0;
    std::string stuckBattles;
    for (const auto battleId : rooms_.Ids())
    {
        const auto *room = FindRoom(battleId);
        if (room == nullptr || !room->outcomePending || !room->outcomeExhausted)
        {
            continue;
        }
        ++stuckTotal;
        if (!stuckBattles.empty())
        {
            stuckBattles += ' ';
        }
        stuckBattles += std::to_string(battleId);
    }
    if (stuckTotal == 0)
    {
        return;
    }
    LOG_ERROR << "metric=battle_personal_settlement_staging_exhausted_rooms sql_unconfirmed=" << stuckTotal
              << " stuck_total=" << stuckTotal << " node_rooms=" << rooms_.Size()
              << ";no SQL decision yet,this node cannot drain until these rooms are cleared;stuck battles: " << stuckBattles;
}

void BattleRoomManager::HandleIssueBattleTicket(const ::IssueBattleTicketRequest &request,
                                                ::IssueBattleTicketResponse &response)
{
    // match 已从大厅会话 metadata 取得权威 player_id(客户端请求体里没有 player_id),
    // battle 只核对名单;这里的 0 只可能是 match 侧漏填,按参数错误拒绝而非 kPlayerNotFoundInSession
    const auto playerId = request.player_id();
    if (playerId == 0 || !gNode || request.target_instance_id() != gNode->GetNodeInfo().node_uuid())
    {
        LOG_WARN << "IssueBattleTicket request lacks player_id: battle_id=" << request.battle_id();
        response.mutable_error_message()->set_id(kInvalidParameter);
        return;
    }

    auto *room = FindRoom(request.battle_id());
    if (room == nullptr)
    {
        // 房间已结束/作废:match 原样透传,客户端据此丢弃本地战斗 UI(与 GetBattleState 空响应同语义)
        LOG_DEBUG << "IssueBattleTicket room does not exist: battle_id=" << request.battle_id()
                  << " player_id=" << playerId;
        response.mutable_error_message()->set_id(kInvalidParameter);
        return;
    }

    ::eBattleTicketRole role = ::BATTLE_TICKET_ROLE_NONE;
    if (room->routingByPlayer.find(playerId) != room->routingByPlayer.end())
    {
        if (!request.spectator_operation_id().empty()) { response.mutable_error_message()->set_id(kInvalidParameter); return; }
        role = ::BATTLE_TICKET_ROLE_PARTICIPANT;
    }
    else if (room->routingByObserver.find(playerId) != room->routingByObserver.end())
    {
        const auto original = room->observerOperations.find(playerId);
        if (original == room->observerOperations.end() || original->second != request.spectator_operation_id()) {
            response.mutable_error_message()->set_id(kInvalidParameter); return;
        }
        // 补签不碰观众席期限:宽限从登记 / 断线起算,补签若能续期,反复补签就能一直占座
        role = ::BATTLE_TICKET_ROLE_OBSERVER;
    }
    else
    {
        LOG_WARN << "IssueBattleTicket player is not in this battle: battle_id=" << request.battle_id()
                 << " player_id=" << playerId;
        response.mutable_error_message()->set_id(kInvalidParameter);
        return;
    }

    if (!BuildAssignment(*room, playerId, role, *response.mutable_assignment()))
    {
        response.clear_assignment();
        response.mutable_error_message()->set_id(kServiceUnavailable);
        return;
    }
    LOG_INFO << "IssueBattleTicket reissuance succeeded: battle_id=" << request.battle_id()
             << " player_id=" << playerId << " role=" << ::eBattleTicketRole_Name(role);
}

// ---- 观战中继(L1)/ 全服广播(L2)(battle-spectate-tiered-fanout.md §15.4) ----

BattleRoomManager::BattleRoom *BattleRoomManager::FindSpectateChannelRoom(const uint64_t battleId,
                                                                          const std::string &targetInstanceId,
                                                                          uint32_t &rejectTip)
{
    if (!gNode || targetInstanceId != gNode->GetNodeInfo().node_uuid())
    {
        LOG_WARN << "spectate channel request rejected: target instance mismatch, battle_id=" << battleId
                 << " target_instance_id=" << targetInstanceId;
        rejectTip = kInvalidParameter;
        return nullptr;
    }
    auto *room = FindRoom(battleId);
    if (room == nullptr)
    {
        // kEntityIsNull 是 match 懒剔除观战索引的信号(同 AddObserver),别换 tip
        LOG_INFO << "spectate channel request: room does not exist, battle_id=" << battleId;
        rejectTip = kEntityIsNull;
        return nullptr;
    }
    // 可见性闸(同 AddObserver 第二道闸):只放行 PUBLIC,未填与 PRIVATE 一样拒。房间其实还在,不能回 kEntityIsNull
    if (room->spectateVisibility != ::SPECTATE_VISIBILITY_PUBLIC)
    {
        LOG_WARN << "spectate channel request rejected: battle is not open to spectators, battle_id=" << battleId;
        rejectTip = kInvalidParameter;
        return nullptr;
    }
    return room;
}

void BattleRoomManager::HandleAttachRelayGate(const ::AttachRelayGateRequest &request,
                                              ::AttachRelayGateResponse &response)
{
    const auto battleId = request.battle_id();
    uint32_t rejectTip = 0;
    auto *room = FindSpectateChannelRoom(battleId, request.target_instance_id(), rejectTip);
    if (room == nullptr)
    {
        response.mutable_error_message()->set_id(rejectTip);
        return;
    }
    const auto gateNodeId = request.gate_node_id();
    const auto &gateInstanceId = request.gate_instance_id();
    // fail-closed(AGENTS.md §7.2):空目标会关掉 gate 侧的目标过滤,这样的 gate 不能进中继表
    if (gateNodeId == 0 || gateInstanceId.empty())
    {
        LOG_ERROR << "AttachRelayGate invalid gate target: battle_id=" << battleId << " gate_node_id=" << gateNodeId
                  << " gate_instance_id=" << gateInstanceId;
        response.mutable_error_message()->set_id(kInvalidParameter);
        return;
    }

    response.set_battle_instance_id(gNode->GetNodeInfo().node_uuid());
    response.set_deadline_ms(room->deadlineMs);
    if (room->spectateBroadcast)
    {
        response.set_broadcast(true);
        return;
    }
    auto &attachedInstanceId = room->relayGates[gateNodeId];
    if (attachedInstanceId == gateInstanceId)
    {
        return;
    }
    attachedInstanceId = gateInstanceId;
    // 只给这台 gate 补发关键帧:它从这一刻才开始读本场,其它 gate 已有缓存
    auto frame = BuildSpectateStateFrame(*room);
    spectateFramePublisher_.PublishToRelayGate(frame, gateNodeId, gateInstanceId);
    LOG_INFO << "AttachRelayGate succeeded: battle_id=" << battleId << " gate_node_id=" << gateNodeId
             << " gate_instance_id=" << gateInstanceId << " relay_gates=" << room->relayGates.size();
}

void BattleRoomManager::HandlePromoteSpectateBroadcast(const ::PromoteSpectateBroadcastRequest &request,
                                                       ::PromoteSpectateBroadcastResponse &response)
{
    const auto battleId = request.battle_id();
    uint32_t rejectTip = 0;
    auto *room = FindSpectateChannelRoom(battleId, request.target_instance_id(), rejectTip);
    if (room == nullptr)
    {
        response.mutable_error_message()->set_id(rejectTip);
        return;
    }

    response.set_battle_instance_id(gNode->GetNodeInfo().node_uuid());
    response.set_deadline_ms(room->deadlineMs);
    if (room->spectateBroadcast)
    {
        return;
    }
    room->spectateBroadcast = true;
    auto frame = BuildSpectateStateFrame(*room);
    spectateFramePublisher_.PublishToBroadcast(frame);
    LOG_INFO << "PromoteSpectateBroadcast succeeded: battle_id=" << battleId
             << " relay_gates=" << room->relayGates.size();
}

::contracts::kafka::SpectateFrameEvent BattleRoomManager::NewSpectateFrame(const BattleRoom &room,
                                                                           const ::contracts::kafka::eSpectateFrameKind kind,
                                                                           const uint32_t roundIndex,
                                                                           const ::MessageContent &envelope) const
{
    ::contracts::kafka::SpectateFrameEvent frame;
    frame.set_battle_id(room.battleId);
    // 通道都是核对过本实例之后才开的,走到这里 gNode 恒非空;真为空时实例留空,gate 按实例不符丢弃,不会误收
    if (gNode != nullptr)
    {
        frame.set_battle_instance_id(gNode->GetNodeInfo().node_uuid());
    }
    frame.set_round_index(roundIndex);
    frame.set_kind(kind);
    frame.set_client_frame(envelope.SerializeAsString());
    frame.set_deadline_ms(room.deadlineMs);
    return frame;
}

::contracts::kafka::SpectateFrameEvent BattleRoomManager::BuildSpectateStateFrame(const BattleRoom &room) const
{
    // observer_count = 0:中继 / 广播档的观众分散在各 gate,battle 不知道人数,0 表示未知(§15.1)
    const auto state = BuildSpectatorState(room, 0);
    return NewSpectateFrame(room, ::contracts::kafka::SPECTATE_FRAME_STATE, state.state().round_index(),
                            ClientEnvelope(BattleClientPlayerNotifySpectateStateMessageId, state));
}

void BattleRoomManager::PublishSpectateTurn(const BattleRoom &room, const uint32_t roundIndex,
                                            const ::MessageContent &envelope)
{
    if (!room.spectateBroadcast && room.relayGates.empty())
    {
        return;
    }
    auto frame = NewSpectateFrame(room, ::contracts::kafka::SPECTATE_FRAME_TURN, roundIndex, envelope);
    // 升档之后只发广播:每台 gate 都在读它,再逐台中继只会让 battle 的产出继续随 gate 数增长(§3 档位规则)
    if (room.spectateBroadcast)
    {
        spectateFramePublisher_.PublishToBroadcast(frame);
        return;
    }
    for (const auto &[gateNodeId, gateInstanceId] : room.relayGates)
    {
        spectateFramePublisher_.PublishToRelayGate(frame, gateNodeId, gateInstanceId);
    }
}

void BattleRoomManager::PublishSpectateEnd(const BattleRoom &room, const ::MessageContent &envelope)
{
    if (!room.spectateBroadcast && room.relayGates.empty())
    {
        return;
    }
    auto frame = NewSpectateFrame(room, ::contracts::kafka::SPECTATE_FRAME_END, room.engine.BuildStateSnapshot().round_index(),
                                  envelope);
    // 升档之后只发广播:与最后一回合的 TURN 同分区保序。再发给升档前挂的 gate 的话,两条 topic 之间没有顺序,
    // 中继 END 可能先到,gate 收尾后把随后到的广播 TURN 丢掉,观众看不到最后一回合。
    if (room.spectateBroadcast)
    {
        spectateFramePublisher_.PublishToBroadcast(frame);
        return;
    }
    for (const auto &[gateNodeId, gateInstanceId] : room.relayGates)
    {
        spectateFramePublisher_.PublishToRelayGate(frame, gateNodeId, gateInstanceId);
    }
}
