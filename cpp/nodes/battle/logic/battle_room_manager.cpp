#include "battle_room_manager.h"

#include <limits>
#include <map>
#include <string>
#include <unordered_map>
#include <utility>
#include <vector>

#include <yaml-cpp/yaml.h>

#include <muduo/contrib/hiredis/Hiredis.h>
#include <hiredis/hiredis.h>

#include "muduo/base/Logging.h"

#include "engine/infra/messaging/kafka/kafka_producer.h"
#include "node/system/node/client_endpoint.h"
#include "node/system/node/node.h"
#include "node/system/node/node_command_route.h"
#include "node_config_manager.h"
#include "thread_context/redis_manager.h"
#include "time/system/time.h"

#include "battle_push_policy.h"
#include "battle_security.h"
#include "client/battle_client_edge.h"

#include "constants/turn_battle_constants.h"
#include "data/battle_table_fingerprint.h"
// 结算发件箱的纯判定 + Redis key/Lua 契约(R07)。
#include "settlement/settlement_outbox.h"
// 活动局结果的回显组装 + 持久记录键/重发判定(帮会同道历练,guild-phase2 §6.19–§6.21)。
#include "system/battle_result_activity.h"

// player:{id}:location 是 scene_manager 写的跨运行时契约 key(单一共享 Redis,
// cross-zone-matchmaking.md D12),重投时用它重新解析玩家当前所在的 scene。
#include "proto/scene_manager/storage.pb.h"

#include "proto/common/base/message.pb.h"
#include "proto/common/event/battle_event.pb.h"
#include "proto/contracts/kafka/gate_command.pb.h"
#include "proto/contracts/kafka/gate_event.pb.h"
#include "proto/contracts/kafka/match_event.pb.h"
#include "proto/contracts/kafka/scene_command.pb.h"

#include "table/proto/tip/common_error_tip.pb.h"

// 事件 id 注册表常量:
//   contracts_kafka_gate_event_event_id.h  → ContractsKafkaPushToPlayerEventEventId(大厅公告回落)
//   common_event_battle_event_event_id.h   → BattleSettlementEventEventId
#include "rpc/service_metadata/contracts_kafka_gate_event_event_id.h"
#include "rpc/service_metadata/common_event_battle_event_event_id.h"
// 客户端消息 id 常量(proto/battle/player_battle.proto 重生成后出现):
//   BattleClientPlayerNotifyBattleStartMessageId / NotifyTurnResult / NotifyBattleEnd
//   二期新增:NotifySpectateState / NotifySpectateTurnResult / NotifySpectateEnd
#include "rpc/service_metadata/player_battle_service_metadata.h"

namespace
{

    // 单房间观众上限(设计文档 §10.5)。纯内存房间,每观众每回合一条直连推送,
    // 上限约束单房间出站扇出,防观战成为放大器。
    constexpr size_t kMaxObserversPerRoom = 20;

    // ---- Kafka 出站(设计文档 D6;不变量:target_instance_id 必填 + key=业务实体 ID) ----

    // 发一条 GateCommand 到目标 gate 的 topic。
    // key=player_id:同一玩家的大厅公告在 gate 侧保序(宪法 §7 不变量 3)。
    void SendGateCommand(const ::BattleRouting &routing, const uint64_t playerId,
                         const uint32_t eventId, std::string payload)
    {
        // 不变量 2 fail-closed:空 instance id 会关闭 gate 侧防僵尸过滤,宁可丢弃并报错。
        if (routing.gate_instance_id().empty())
        {
            LOG_ERROR << "battle 出站 GateCommand 被拒: gate_instance_id 为空, player_id=" << playerId
                      << " event_id=" << eventId << " gate_node_id=" << routing.gate_node_id();
            return;
        }

        contracts::kafka::GateCommand command;
        command.set_event_id(eventId);
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
            LOG_ERROR << "battle 出站 GateCommand 发送失败: topic=" << route.topic
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
                     << ",玩家无活直连,战斗帧不回落 gate(客户端直连就绪后 GetBattleState 补拉)";
        }
    }

    // 底层发送:显式给出 (scene_node_id, scene_instance_id)。
    // topic=scene-cmd_g<N> 的 scene_node_id % P 号分区,key=player_id
    // (同一玩家的确认/结算保序 —— 分区已由 node_id 定死,key 只影响同分区内的批次,
    //  不影响落点,不变量 3 仍然成立)。
    //
    // sceneInstanceId 允许为空 —— 只有一种情况:结算**重投**时目标是刚从
    // player:{id}:location 重新解析出来的 node_id,那里只有节点号没有实例 uuid。
    // 留空后 gate/scene 侧的 ValidateCommandTarget 会跳过第二级过滤,语义变成
    // "谁现在持有这个 node_id 就谁执行"——这正是重投想要的。为什么这样是安全的:
    // 结算命令**自校验于玩家归属**(scene 侧 ApplySettlement 先 GetPlayer(player_id)),
    // 不持有该玩家的 scene 只会把待结算记录原样再写一遍(幂等),不会误改任何人的数据。
    // 首投仍然带 uuid,防僵尸这一级一点没松。
    void SendSceneCommandTo(const uint32_t sceneNodeId, const std::string &sceneInstanceId,
                            const uint64_t playerId, const uint32_t eventId,
                            const std::string &payload, const uint64_t battleIdForLog)
    {
        if (sceneNodeId == 0)
        {
            LOG_ERROR << "battle 出站 scene 事件被拒: scene_node_id 为 0, player_id=" << playerId
                      << " battle_id=" << battleIdForLog << " event_id=" << eventId;
            return;
        }

        contracts::kafka::SceneCommand command;
        command.set_command_type(contracts::kafka::SceneCommand::DispatchEvent);
        command.set_player_id(playerId);
        command.set_target_scene_id(sceneNodeId);
        command.set_payload(payload);
        command.set_target_instance_id(sceneInstanceId);
        command.set_event_id(eventId);

        const auto route = node::kafka::ResolveCommandRoute(SceneNodeService, sceneNodeId);
        const auto err = KafkaProducer::Instance().send(route.topic, command.SerializeAsString(),
                                                        std::to_string(playerId), route.partition);
        if (err != RdKafka::ERR_NO_ERROR)
        {
            LOG_ERROR << "battle 出站 scene 事件发送失败: topic=" << route.topic
                      << " partition=" << route.partition
                      << " player_id=" << playerId << " battle_id=" << battleIdForLog
                      << " event_id=" << eventId << " err=" << err;
        }
    }

    // 发一条进程内事件到玩家所在 scene:SceneCommand{DispatchEvent, eventId, message}。
    // target_instance_id=BattleRouting.scene_instance_id(不变量 2 fail-closed)。
    // 确认事件走这里;结算走 SendSettlementCommand(重投时目标要重新解析,uuid 无从得知)。
    void SendSceneEvent(const ::BattleRouting &routing, const uint64_t playerId,
                        const uint32_t eventId, const google::protobuf::Message &message,
                        const uint64_t battleIdForLog)
    {
        if (routing.scene_instance_id().empty())
        {
            LOG_ERROR << "battle 出站 scene 事件被拒: scene_instance_id 为空, player_id=" << playerId
                      << " battle_id=" << battleIdForLog << " event_id=" << eventId;
            return;
        }

        SendSceneCommandTo(routing.scene_node_id(), routing.scene_instance_id(), playerId, eventId,
                           message.SerializeAsString(), battleIdForLog);
    }

    // ---- 结算落库(R07):跨进程存储用共享 Redis ----
    //
    // 为什么 battle 用 tlsRedis 就够:zone_redis 在本仓部署里是**全服单一实例**
    // (cross-zone-matchmaking.md D12 把它钉成不变量:player:{id}:location /
    // player:session:{id} / battle:lock:{id} 三类跨运行时契约 key 都以它为准),
    // 所以 battle 写的 battle:settlement:pending:* 与 scene 读写的是同一个库。
    bool RedisReady()
    {
        auto &redis = tlsRedis.GetZoneRedis();
        return redis && redis->connected();
    }

    // 从 player:{id}:location 解析出玩家当前所在的 scene node_id。0 = 解析不到。
    // node_id 在这个 key 里是**十进制字符串**(scene_manager 的 enterscenelogic.go
    // 用 strconv.ParseUint(nodeId, 10, 32) 校验过),不是实体句柄。
    uint32_t ParseSceneNodeIdFromLocationReply(const redisReply *reply)
    {
        if (reply == nullptr || reply->type != REDIS_REPLY_STRING)
        {
            return 0;
        }
        if (reply->len > static_cast<size_t>(std::numeric_limits<int>::max()))
        {
            return 0;
        }
        storage::PlayerLocation location;
        if (!location.ParseFromArray(reply->str, static_cast<int>(reply->len)))
        {
            return 0;
        }
        if (location.node_id().empty())
        {
            return 0;
        }
        try
        {
            const auto parsed = std::stoull(location.node_id());
            if (parsed == 0 || parsed > std::numeric_limits<uint32_t>::max())
            {
                return 0;
            }
            return static_cast<uint32_t>(parsed);
        }
        catch (const std::exception &)
        {
            return 0;
        }
    }

    // 结算命令的一次投递(首投与重投共用),payload 是序列化好的 BattleSettlementEvent。
    void SendSettlementCommand(const uint32_t sceneNodeId, const std::string &sceneInstanceId,
                               const uint64_t playerId, const uint64_t battleId,
                               const std::string &payload)
    {
        SendSceneCommandTo(sceneNodeId, sceneInstanceId, playerId, BattleSettlementEventEventId,
                           payload, battleId);
    }

    // 确认事件补发策略(与 scene 侧 player_battle.h 的锁保留期对齐):
    //   scene 备战期锁 EX = prepare_deadline 剩余秒数 + PlayerBattleSystem::kLockExtraTtlSec(60s),
    //   备战到期只摘组件不删锁,锁在期间迟到的确认仍可重建冻结。
    //   prepare_deadline = gather 起点 + match 的 matchedTicketTTLFor(组大小)(go/match queue.go),
    //   现行公式 ⌈N×6 + 2×(6.1+5) + 3⌉ + 10 → 1/2/5/10 人依次 42/48/66/96s;最大组 10 人(5v5),
    //   所以锁最晚在 gather 起点 +96+60=156s 过期。补发窗口从建房起算,建房晚于 gather 起点,
    //   取 180s ≥ 156s,多出的 24s 吸收 match / scene / battle 三方墙钟偏差与锁 EX 的秒级取整。
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

    // 对局结果回流 match(评分,二期 MMR):topic=match-results(全局,不带 zone 段),
    // key=battle_id(同局保序即可)。payload 直接是 BattleResultEvent,不套 *Command 信封:
    // 该 topic 无目标实例语义(任一 match 实例消费即可),不适用不变量 2 的 target_instance_id。
    constexpr char kMatchResultsTopic[] = "match-results";

    // 发一条已序列化的 BattleResultEvent。活动局首发与重发都走这里,保证 Kafka 上的字节与
    // Redis 持久记录逐字节相同(guild 巡检器从记录结算与消费者从 topic 结算看到的是同一份结果)。
    void SendMatchResultPayload(const uint64_t battleId, const std::string &payload)
    {
        const auto err = KafkaProducer::Instance().send(kMatchResultsTopic, payload,
                                                        std::to_string(battleId));
        if (err != RdKafka::ERR_NO_ERROR)
        {
            LOG_ERROR << "battle 对局结果发送失败: topic=" << kMatchResultsTopic
                      << " battle_id=" << battleId << " err=" << err;
        }
    }

    void SendBattleResultEvent(const contracts::kafka::BattleResultEvent &event)
    {
        SendMatchResultPayload(event.battle_id(), event.SerializeAsString());
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
                    LOG_WARN << "battle_table_fingerprint_mode 取值非法,回默认 warn: value=" << text
                             << " path=" << configPath;
                }
            }
        }
        catch (const std::exception &ex)
        {
            LOG_WARN << "读取 battle_table_fingerprint_mode 失败,回默认 warn: path=" << configPath
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
                LOG_WARN << "BATTLE_TABLE_FINGERPRINT_MODE 取值非法,忽略: value=" << envMode;
            }
        }

        LOG_INFO << "battle 配表指纹校验模式: mode=" << FingerprintModeName(mode);
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
                      << ",配表指纹不一致,拒绝开局(mode=enforce)";
            return false;
        }
        LOG_WARN << "metric=battle_table_fingerprint_mismatch battle_id=" << request.battle_id()
                 << " match_mode=" << request.match_mode()
                 << " self=" << self << " mismatch:" << mismatches
                 << ",配表指纹不一致,照常开局(mode=warn)";
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
        LOG_ERROR << "EmplaceRoom 重复插表(调用方幂等判定失效),新建的房间对象随之丢弃: battle_id=" << battleId;
    }
    return inserted;
}

void BattleRoomManager::EraseRoom(const uint64_t battleId)
{
    // 先移除后回调(房间表保证):最后一个房间移除后 Agones 才能回 Ready,顺序反了会在房间还在时放出容量
    rooms_.Erase(battleId);
}

void BattleRoomManager::HandleCreateBattle(const ::CreateBattleRequest &request,
                                           ::CreateBattleResponse &response)
{
    const auto battleId = request.battle_id();
    response.set_battle_id(battleId);

    // 幂等:match 补偿路径可能重试 CreateBattle
    if (FindRoom(battleId) != nullptr)
    {
        LOG_INFO << "CreateBattle 幂等命中: battle_id=" << battleId;
        return;
    }

    if (battleId == 0 || request.players_size() == 0)
    {
        LOG_ERROR << "CreateBattle 参数非法: battle_id=" << battleId
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
            LOG_ERROR << "CreateBattle 路由信息不完整: battle_id=" << battleId
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

    auto room = std::make_unique<BattleRoom>();
    room->battleId = battleId;
    room->matchMode = request.match_mode();
    room->battleConfigId = request.battle_config_id();
    // 活动上下文原样保存(未填 = kind NONE);FinishBattle 回显并据此选结果通道(§6.20)
    room->activityContext = request.activity_context();
    if (!room->engine.Initialize(request))
    {
        LOG_ERROR << "CreateBattle 引擎初始化失败(快照/表数据非法): battle_id=" << battleId
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
                      << ",参战票据签不出,拒绝开局(kServiceUnavailable)";
            response.mutable_error_message()->set_id(kServiceUnavailable);
            return;
        }
    }

    // 插表 = 房间生命周期起点(EmplaceRoom 随即通知 Agones 单元已创建);此前的拒绝路径都没有副作用
    auto *roomPtr = EmplaceRoom(std::move(room));
    if (roomPtr == nullptr)
    {
        // 开头已判过幂等且同一 loop 任务内无人插表,走到这里是程序缺陷(EmplaceRoom 已打 ERROR)。
        // 按幂等命中处理:回 OK、不设 error_message。表里那间同 battle_id 的房间是真的在打,
        // 回错误会诱发 match 的补偿 DestroyBattle(battle_id),把它一起销毁。
        return;
    }

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

    LOG_INFO << "CreateBattle 成功: battle_id=" << battleId
             << " players=" << request.players_size()
             << " battle_config_id=" << request.battle_config_id()
             << " match_mode=" << request.match_mode()
             << " deadline_ms=" << deadlineMs
             << " activity_kind=" << ::eBattleActivityKind_Name(request.activity_context().kind())
             << " activity_id=" << request.activity_context().activity_id()
             << " guild_id=" << request.activity_context().guild_id();
}

void BattleRoomManager::HandleDestroyBattle(const ::DestroyBattleRequest &request)
{
    const auto battleId = request.battle_id();
    auto *room = FindRoom(battleId);
    if (room == nullptr)
    {
        // 幂等:已销毁/从未建成都视为成功
        LOG_INFO << "DestroyBattle 幂等命中: battle_id=" << battleId
                 << " reason=" << request.reason();
        return;
    }

    // 补偿/回滚路径:不发结算(冻结解除由 match 调 scene CancelBattlePrepare
    // 或 scene reaper 按 deadline 兜底,设计文档 §3.2);推观众终局,并关闭直连。
    // 注意:scene 收到过本房间的确认事件后会拒绝 CancelBattlePrepare(已 FIGHTING 视为过期回滚),
    // 此时玩家冻结要等 reaper 按 deadline_ms 解除 —— 房间已销毁无法再发结算,属已知取舍。
    room->roundTimer.Cancel();
    room->battleTimer.Cancel();
    room->confirmResendTimer.Cancel();
    // 观众同步清退:作废路径无胜负,outcome 留 ONGOING(player_battle.proto 契约)
    NotifySpectateEndAndClose(*room, ::SPECTATE_END_BATTLE_ABORTED, ::BATTLE_OUTCOME_ONGOING);
    // 作废路径没有终局包可等,直接关直连;客户端凭 GetBattleState 空响应 / 断连收 UI
    CloseDirectConnections(*room, "destroyed");
    EraseRoom(battleId);

    LOG_INFO << "DestroyBattle 完成: battle_id=" << battleId << " reason=" << request.reason();
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
        LOG_WARN << "SubmitBattleAction 会话无玩家身份: session_id=" << sessionDetails.session_id();
        response.mutable_error_message()->set_id(kPlayerNotFoundInSession);
        return;
    }

    auto *room = FindRoom(request.battle_id());
    if (room == nullptr)
    {
        // 常见竞态:战斗刚结束,客户端的行动包晚到
        LOG_DEBUG << "SubmitBattleAction 房间不存在: battle_id=" << request.battle_id()
                  << " player_id=" << playerId;
        response.mutable_error_message()->set_id(kInvalidParameter);
        return;
    }

    // 防串房:battle_id 可被客户端伪造,必须校验参战名单
    if (room->routingByPlayer.find(playerId) == room->routingByPlayer.end())
    {
        LOG_WARN << "SubmitBattleAction 玩家不在此战斗: battle_id=" << request.battle_id()
                 << " player_id=" << playerId;
        response.mutable_error_message()->set_id(kInvalidParameter);
        return;
    }

    // 先查后提交:校验码回给提交者(G2)。原先"一律回 OK"会让玩家点了吃药、
    // 客户端显示成功、回合结算却变成默认普攻,连原因都看不到 —— 玩家资产路径
    // 不许静默降级(AGENTS §11.3)。ValidateAction 零副作用,与 SubmitAction 的
    // 接受条件逐条同源。
    const uint32_t validation = room->engine.ValidateAction(playerId, request.action());
    if (validation != kSuccess)
    {
        LOG_DEBUG << "SubmitBattleAction 校验不过: battle_id=" << request.battle_id()
                  << " player_id=" << playerId << " action=" << request.action().action_type()
                  << " code=" << validation;
        // 0 不是任何一个 tip 码(kCommon_errorOK=0),写进去客户端会读成"成功"却没落账。
        // 引擎已把坏表格子转成 kInvalidTableData,这里是最后一道兜底。
        if (validation == 0)
        {
            LOG_ERROR << "SubmitBattleAction 校验返回 0(坏表或引擎契约破坏): battle_id="
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
        // 注意:ResolveRound 可能走到 FinishBattle + EraseRoom,之后 room 悬空 ——
        // 任何依赖 room 的响应字段都必须在这一行之前填完
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
        LOG_DEBUG << "GetBattleState 房间不存在或无关玩家: battle_id=" << request.battle_id()
                  << " player_id=" << playerId;
        response.Clear();
        return;
    }

    response = room->engine.BuildStateSnapshot();
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

void BattleRoomManager::HandleAddObserver(const ::AddObserverRequest &request,
                                          ::AddObserverResponse &response)
{
    const auto battleId = request.battle_id();
    const auto observerId = request.observer_player_id();

    auto *room = FindRoom(battleId);
    if (room == nullptr)
    {
        // kEntityIsNull 是 match 懒剔除 Redis 观战索引的信号(§10.2),别换 tip
        LOG_INFO << "AddObserver 房间不存在: battle_id=" << battleId
                 << " observer=" << observerId;
        response.mutable_error_message()->set_id(kEntityIsNull);
        return;
    }

    // fail-closed:落点分配包是大厅公告,观众还没有直连时只能经 Kafka→gate 回落送达;
    // gate_instance_id 缺失会被防僵尸校验丢弃,观众永远拿不到票据、建不了直连,
    // 登记进来也是聋子观众,直接拒绝让 match 重组路由
    if (observerId == 0 || request.routing().gate_instance_id().empty())
    {
        LOG_ERROR << "AddObserver 参数非法: battle_id=" << battleId
                  << " observer=" << observerId
                  << " gate_instance_id=" << request.routing().gate_instance_id();
        response.mutable_error_message()->set_id(kInvalidParameter);
        return;
    }

    // 参战者不能观战自己所在的战斗:directConnByPlayer 按 player_id 只有一个槽位,
    // 参战和观战共用,放行会让观战直连顶掉参战直连(D11 互斥的房间侧兜底)
    if (room->routingByPlayer.find(observerId) != room->routingByPlayer.end())
    {
        LOG_WARN << "AddObserver 观众是参战者: battle_id=" << battleId
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
        const bool sameSession =
            it->second.session_id() == request.routing().session_id() &&
            it->second.gate_node_id() == request.routing().gate_node_id() &&
            it->second.gate_instance_id() == request.routing().gate_instance_id();

        // 两条幂等路径都先重签(turn-based §22 D70 fail-closed)。签不出票 = 这名观众连不上直连、
        // 看不到任何战斗帧;而 match 收到任何错误都会 DEL 观战标记(watchbattlelogic),
        // 房间侧必须同步摘除并关其直连,否则 match 认为他已不在观战、房间里却还挂着他。
        ::BattleAssignedS2C assigned;
        if (!BuildAssignment(*room, observerId, ::BATTLE_TICKET_ROLE_OBSERVER, assigned))
        {
            LOG_ERROR << "metric=battle_ticket_issue_failed battle_id=" << battleId
                      << " player_id=" << observerId << " role=observer"
                      << " path=" << (sameSession ? "idempotent_same_session" : "idempotent_session_changed")
                      << ",观战票据签不出,摘除观众并拒绝(kServiceUnavailable)";
            room->routingByObserver.erase(it);
            room->observerNames.erase(observerId);
            CloseDirectConnectionOf(*room, observerId, "observer_ticket_issue_failed");
            response.mutable_error_message()->set_id(kServiceUnavailable);
            return;
        }

        if (sameSession)
        {
            // 同会话重试:重推落点分配;观战快照是战斗帧,已有活直连时照常直发,
            // 否则丢弃,由客户端建直连时握手快照补上(§22 D69)
            LOG_INFO << "AddObserver 幂等命中,重推落点分配: battle_id=" << battleId
                     << " observer=" << observerId;
            PushAssignment(*room, observerId, it->second, assigned);
            PushSpectateState(*room, observerId);
            return;
        }
        LOG_INFO << "AddObserver 幂等命中但会话已变,刷新路由: battle_id=" << battleId
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
        PushAssignment(*room, observerId, it->second, assigned);
        return;
    }

    if (room->routingByObserver.size() >= kMaxObserversPerRoom)
    {
        LOG_INFO << "AddObserver 观众已满: battle_id=" << battleId
                 << " observer=" << observerId << " cap=" << kMaxObserversPerRoom;
        response.mutable_error_message()->set_id(kRateLimitExceeded);
        return;
    }

    // fail-closed 预签(turn-based §22 D70):登记之前签票,签不出不登记 ——
    // 观众连不上直连就看不到任何战斗帧,登记进来只会占一个观众名额
    ::BattleAssignedS2C assigned;
    if (!BuildAssignment(*room, observerId, ::BATTLE_TICKET_ROLE_OBSERVER, assigned))
    {
        LOG_ERROR << "metric=battle_ticket_issue_failed battle_id=" << battleId
                  << " player_id=" << observerId << " role=observer path=new"
                  << ",观战票据签不出,拒绝观战(kServiceUnavailable)";
        response.mutable_error_message()->set_id(kServiceUnavailable);
        return;
    }

    room->routingByObserver.emplace(observerId, request.routing());
    room->observerNames.emplace(observerId, request.observer_name());

    // 只下发落点分配(大厅公告)。观战首帧随直连握手下发(§22 D69 snapshot on connect):
    // 此刻观众一定还没有直连,在这里推首帧只会被战斗帧出口丢弃
    PushAssignment(*room, observerId, request.routing(), assigned);

    LOG_INFO << "AddObserver 成功: battle_id=" << battleId
             << " observer=" << observerId << " name=" << request.observer_name()
             << " observers=" << room->routingByObserver.size();
}

void BattleRoomManager::HandleRemoveObserver(const ::RemoveObserverRequest &request)
{
    const auto battleId = request.battle_id();
    const auto observerId = request.observer_player_id();

    auto *room = FindRoom(battleId);
    if (room == nullptr)
    {
        // 幂等:房间已结束时观众早已随 FinishBattle/作废路径清退
        return;
    }
    const auto it = room->routingByObserver.find(observerId);
    if (it == room->routingByObserver.end())
    {
        return;
    }

    // 被动清退(去排队互斥等):先告知原因再关直连,客户端据 reason 关观战 UI。
    // 战斗帧只走直连:观众没有直连时不回落,客户端只能靠自身超时 / 直连 FIN 收起观战 UI
    ::SpectateEndS2C end;
    end.set_battle_id(battleId);
    end.set_outcome(::BATTLE_OUTCOME_ONGOING);
    end.set_reason(::SPECTATE_END_REMOVED);
    PushBattleFrame(*room, observerId, BattleClientPlayerNotifySpectateEndMessageId, end);

    room->routingByObserver.erase(it);
    room->observerNames.erase(observerId);
    // 观众已不在名单,直连没有存在意义;SpectateEnd 已排队,shutdown 让它先 flush
    CloseDirectConnectionOf(*room, observerId, "observer_removed");

    LOG_INFO << "RemoveObserver 完成: battle_id=" << battleId
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
        LOG_WARN << "StopWatchBattle 会话无玩家身份: session_id=" << sessionDetails.session_id();
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

    // 主动退出不推 SpectateEnd:是客户端自己发起的动作,回包即确认
    room->routingByObserver.erase(it);
    room->observerNames.erase(playerId);
    // 退出请求可能就是从这条直连来的,应答由直连面在本函数返回后写出;关闭已推迟到本轮
    // loop 之后(CloseDirectConnectionOf 内部 queueInLoop),应答不会被丢
    CloseDirectConnectionOf(*room, playerId, "stop_watch");

    LOG_INFO << "StopWatchBattle 完成: battle_id=" << room->battleId
             << " observer=" << playerId;
}

void BattleRoomManager::HandleSetAutoBattle(const ::SessionDetails &sessionDetails,
                                            const ::SetAutoBattleRequest &request,
                                            ::SetAutoBattleResponse &response)
{
    const auto playerId = sessionDetails.player_id();
    if (playerId == 0)
    {
        LOG_WARN << "SetAutoBattle 会话无玩家身份: session_id=" << sessionDetails.session_id();
        response.mutable_error_message()->set_id(kPlayerNotFoundInSession);
        return;
    }

    auto *room = FindRoom(request.battle_id());
    if (room == nullptr)
    {
        LOG_DEBUG << "SetAutoBattle 房间不存在: battle_id=" << request.battle_id()
                  << " player_id=" << playerId;
        response.mutable_error_message()->set_id(kInvalidParameter);
        return;
    }

    // 防串房/防观众冒充:只有参战者能切自动
    if (room->routingByPlayer.find(playerId) == room->routingByPlayer.end())
    {
        LOG_WARN << "SetAutoBattle 玩家不在此战斗: battle_id=" << request.battle_id()
                 << " player_id=" << playerId;
        response.mutable_error_message()->set_id(kInvalidParameter);
        return;
    }

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
    // 装填时已全员就绪的房间(全自动)节奏由已按 kAutoRoundIntervalMs 装填的 roundTimer
    // 负责,重复置位不触发立即结算,防止客户端以包速率重发击穿 D13 固定节奏。
    if (request.enabled() && !wasAllReady && room->engine.AllPlayersReady())
    {
        ResolveRound(room->battleId);
    }
}

void BattleRoomManager::ArmRoundTimer(BattleRoom &room)
{
    // 行动收集窗口 = 一回合等效挂钟时长(v1 策略,与时间→回合换算共用常量)。
    // 全自动例外(D13):装填时 AllPlayersReady() 已为真说明全部存活玩家都挂机,
    // 本窗口内不会再有任何提交把回合"提前"结算,整窗等待纯属空转拖慢节奏;
    // 又不能立即结算(同步递归刷回合,观众/客户端表现跟不上),
    // 故改用 kAutoRoundIntervalMs 固定节奏推进。
    const uint64_t windowMs = room.engine.AllPlayersReady() ? turnbattle::kAutoRoundIntervalMs
                                                            : turnbattle::kRoundDurationMs;
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

    BroadcastTurnResult(*room, result);

    if (outcome != ::BATTLE_OUTCOME_ONGOING)
    {
        FinishBattle(*room, outcome, ::SPECTATE_END_BATTLE_FINISHED);
        EraseRoom(battleId);
    }
}

void BattleRoomManager::OnBattleDeadline(const uint64_t battleId)
{
    auto *room = FindRoom(battleId);
    if (room == nullptr)
    {
        return;
    }

    // 整场 deadline 打满仍未分出胜负:强制平局收尾(battle_node.proto 契约)。
    // 引擎无"强制终局"接口,由节点在结算数据上盖平局章。
    auto outcome = room->engine.Outcome();
    if (outcome == ::BATTLE_OUTCOME_ONGOING)
    {
        outcome = ::BATTLE_OUTCOME_DRAW;
    }
    LOG_WARN << "战斗整场超时强制收尾: battle_id=" << battleId
             << " outcome=" << ::eBattleOutcome_Name(outcome);

    room->roundTimer.Cancel();
    // 观众侧 deadline 强制收尾归 ABORTED(player_battle.proto 枚举注释口径);
    // outcome 带上强制盖章结果,观众端可与参战者的平局结算对齐展示
    FinishBattle(*room, outcome, ::SPECTATE_END_BATTLE_ABORTED);
    EraseRoom(battleId);
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
    LOG_INFO << "BattleConfirmedEvent 周期补发: battle_id=" << battleId
             << " players=" << room->routingByPlayer.size()
             << " remain_ms=" << (room->confirmResendUntilMs - nowMs);
}

void BattleRoomManager::FinishBattle(BattleRoom &room, const ::eBattleOutcome outcome,
                                     const ::eSpectateEndReason spectateReason)
{
    room.roundTimer.Cancel();
    room.battleTimer.Cancel();
    room.confirmResendTimer.Cancel();

    // 对局结果回流 match:按 team_index 归组玩家(routingByPlayer 只有玩家,天然不含怪物)
    std::map<uint32_t, std::vector<uint64_t>> playersByTeam;
    uint32_t totalRounds = 0;
    // 结果事件回显的逃跑 / 阵亡名单(所有对局都填;guild 同道历练按 team 0 − 逃跑发奖,U2)
    std::vector<uint64_t> fledPlayerIds;
    std::vector<uint64_t> deadPlayerIds;

    for (const auto &[playerId, routing] : room.routingByPlayer)
    {
        ::BattleSettlementData settlement = room.engine.BuildSettlement(playerId);
        // 强制平局路径引擎结算里还是 ONGOING,以节点判定为准统一盖章
        settlement.set_outcome(outcome);
        settlement.set_battle_id(room.battleId);

        playersByTeam[settlement.player_team_index()].push_back(playerId);
        totalRounds = settlement.total_rounds();
        if (settlement.fled())
        {
            fledPlayerIds.push_back(playerId);
        }
        if (settlement.is_dead())
        {
            deadPlayerIds.push_back(playerId);
        }

        // 顺序:终局包先于关直连(下面 CloseDirectConnections 在全部终局包排队之后)。
        // 终局包是战斗帧,只走直连(turn-based §22 D68);没有直连的玩家收不到它,
        // 但 scene 应用结算后会经大厅再推一份 BattleEndS2C(scene 自己的下行,不归 D39 管),
        // 客户端两条通道都挂 NotifyBattleEnd 并按 battle_id 幂等。
        ::BattleEndS2C end;
        end.set_battle_id(room.battleId);
        end.set_outcome(outcome);
        *end.mutable_settlement() = settlement;
        PushBattleFrame(room, playerId, BattleClientPlayerNotifyBattleEndMessageId, end);

        // R07:先落库、后投递、未销账则重投(重投时重新解析目标)。
        // 投递被推迟到 Redis 落库回调里;真正要保的是"结算在被投递之前已经持久化"。
        DispatchSettlementDurably(routing, playerId, settlement);
    }

    NotifySpectateEndAndClose(room, spectateReason, outcome);
    // 终局包已全部排队,直连从此没有存在意义:shutdown 让它们先 flush 再 FIN(D23)
    CloseDirectConnections(room, "battle_finished");

    // BattleResultEvent(只在真实打完的局发;DestroyBattle/AbortAllRooms 作废路径不进这里)。
    // winner_team_index:SIDE_A_WIN=0 / SIDE_B_WIN=1,平局时 match 忽略该字段。
    contracts::kafka::BattleResultEvent result;
    result.set_battle_id(room.battleId);
    result.set_match_mode(room.matchMode);
    result.set_battle_config_id(room.battleConfigId);
    result.set_outcome(outcome);
    result.set_winner_team_index(outcome == ::BATTLE_OUTCOME_SIDE_B_WIN ? 1u : 0u);
    for (const auto &[teamIndex, playerIds] : playersByTeam)
    {
        auto *team = result.add_teams();
        team->set_team_index(teamIndex);
        for (const auto playerId : playerIds)
        {
            team->add_player_ids(playerId);
        }
    }
    result.set_total_rounds(totalRounds);
    result.set_finished_at_ms(TimeSystem::NowMillisecondsUTC());
    turnbattle::FillBattleResultActivityFields(room.activityContext, std::move(fledPlayerIds),
                                               std::move(deadPlayerIds), result);
    // 活动局(帮会同道历练)丢一条就是整局帮贡 / 资金 / 物品永久丢失,走持久化通道;
    // 普通对局丢一条只影响评分,保持原来的发一次(§6.19)。
    if (result.has_activity_context())
    {
        DispatchActivityResultDurably(result);
    }
    else
    {
        SendBattleResultEvent(result);
    }

    LOG_INFO << "战斗结束: battle_id=" << room.battleId
             << " outcome=" << ::eBattleOutcome_Name(outcome)
             << " players=" << room.routingByPlayer.size()
             << " teams=" << playersByTeam.size()
             << " total_rounds=" << totalRounds
             << " fled=" << result.fled_player_ids_size()
             << " dead=" << result.dead_player_ids_size()
             << " activity_kind=" << ::eBattleActivityKind_Name(room.activityContext.kind());
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
    //(D8:客户端观战/参战两套状态机互不干扰)
    if (!room.routingByObserver.empty())
    {
        ::TurnResultS2C spectate = result;
        RedactStateForViewer(*spectate.mutable_state(), 0);
        for (const auto &[observerId, routing] : room.routingByObserver)
        {
            PushBattleFrame(room, observerId, BattleClientPlayerNotifySpectateTurnResultMessageId,
                            spectate);
        }
    }
}

void BattleRoomManager::RedactStateForViewer(::BattleStateS2C &state,
                                             const uint64_t viewerPlayerId) const
{
    // 道具余量是私有信息:统一在这里清掉,再由 FillSelfItems 只给本人回填。
    // 不依赖"BuildStateSnapshot 恰好不填 self_items"这一外部事实 —— 那是别处的实现细节。
    state.clear_self_items();
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

void BattleRoomManager::PushSpectateState(const BattleRoom &room, const uint64_t observerId) const
{
    ::SpectateStateS2C state;
    *state.mutable_state() = room.engine.BuildStateSnapshot();
    state.mutable_state()->set_battle_id(room.battleId);
    // 引擎无时钟,行动截止由节点按房间 timer 回填(与参战者首帧同口径)
    state.mutable_state()->set_action_deadline_ms(room.actionDeadlineMs);
    // 观众视角:全员冷却剔除,不带任何人的道具余量(G7)
    RedactStateForViewer(*state.mutable_state(), 0);
    state.set_observer_count(static_cast<uint32_t>(room.routingByObserver.size()));

    PushBattleFrame(room, observerId, BattleClientPlayerNotifySpectateStateMessageId, state);
}

void BattleRoomManager::NotifySpectateEndAndClose(BattleRoom &room,
                                                  const ::eSpectateEndReason reason,
                                                  const ::eBattleOutcome outcome)
{
    if (room.routingByObserver.empty())
    {
        return;
    }

    ::SpectateEndS2C end;
    end.set_battle_id(room.battleId);
    end.set_outcome(outcome);
    end.set_reason(reason);

    // 同一条直连先推终局包再关(关闭推迟到本轮 loop 之后,shutdown 等输出缓冲排空再 FIN):
    // 观众先看到结束原因。没有直连的观众收不到它(战斗帧不回落,§22 D68)
    for (const auto &[observerId, routing] : room.routingByObserver)
    {
        PushBattleFrame(room, observerId, BattleClientPlayerNotifySpectateEndMessageId, end);
        CloseDirectConnectionOf(room, observerId, "spectate_end");
    }

    LOG_INFO << "观战清退: battle_id=" << room.battleId
             << " observers=" << room.routingByObserver.size()
             << " reason=" << ::eSpectateEndReason_Name(reason);

    room.routingByObserver.clear();
    room.observerNames.clear();
}

void BattleRoomManager::AbortAllRooms(const std::string &reason)
{
    if (rooms_.Empty())
    {
        return;
    }

    LOG_INFO << "作废全部战斗房间: rooms=" << rooms_.Size() << " reason=" << reason;
    // 先抄 battle_id 再逐个 EraseRoom:移除走唯一删除口,每个房间都触发一次 onRemoved
    // (Agones 单元计数归零);不能边遍历房间表边删。
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
        // 不结算:战斗作废,scene reaper 按 InBattleComp.deadline_ms 解冻(§3.2)。
        // 停机作废无胜负:观众收 ABORTED + ONGOING
        NotifySpectateEndAndClose(*room, ::SPECTATE_END_BATTLE_ABORTED, ::BATTLE_OUTCOME_ONGOING);
        CloseDirectConnections(*room, reason.c_str());
        EraseRoom(battleId);
    }
}

// ---- 客户端直连(设计文档 §18,D23-D28;下行出口 turn-based §22 D68) ----

muduo::net::TcpConnectionPtr BattleRoomManager::LiveDirectConnOf(const BattleRoom &room,
                                                                 const uint64_t playerId) const
{
    if (edge_ == nullptr)
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
    // 经已验证的直连写一条 S2C(MessageContent 信封,与大厅连接同一线协议)。
    void SendOverDirect(const BattleClientEdge &edge, const muduo::net::TcpConnectionPtr &conn,
                        const uint32_t messageId, const ::google::protobuf::Message &message)
    {
        ::MessageContent content;
        content.set_message_id(messageId);
        content.set_serialized_message(message.SerializeAsString());
        edge.Send(conn, content);
    }
} // namespace

void BattleRoomManager::PushBattleFrame(const BattleRoom &room, const uint64_t playerId,
                                        const uint32_t messageId,
                                        const ::google::protobuf::Message &message) const
{
    const auto conn = LiveDirectConnOf(room, playerId);
    switch (battle_push_policy::Decide(battle_push_policy::PushCategory::kBattleFrame, conn != nullptr))
    {
    case battle_push_policy::PushRoute::kDirect:
        SendOverDirect(*edge_, conn, messageId, message);
        return;
    case battle_push_policy::PushRoute::kViaGate:
        // 战斗帧的判定不会产出 kViaGate(tests/battle_push_policy_test.cpp 钉住);
        // 本出口也没有路由可回落,防御性按丢弃处理
    case battle_push_policy::PushRoute::kDrop:
        LogBattleFrameDroppedSampled(room.battleId, playerId, messageId);
        return;
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
        SendOverDirect(*edge_, conn, messageId, message);
        return;
    case battle_push_policy::PushRoute::kViaGate:
        PushMessageViaGate(routing, playerId, messageId, message);
        return;
    case battle_push_policy::PushRoute::kDrop:
        // 大厅公告的判定不会产出 kDrop(单测钉住);真出现说明判定契约被改坏了,必须响
        LOG_ERROR << "battle 大厅公告被判定丢弃(battle_push_policy 契约破坏): battle_id=" << room.battleId
                  << " player_id=" << playerId << " message_id=" << messageId;
        return;
    }
}

bool BattleRoomManager::BuildAssignment(const BattleRoom &room, const uint64_t playerId,
                                        const ::eBattleTicketRole role, ::BattleAssignedS2C &out) const
{
    const auto &deploy = gNodeConfigManager.GetBaseDeployConfig();
    const auto &secret = deploy.battle_token_secret();
    const auto verdict = battle_security::ClassifyTokenSecret(secret, battle_security::CurrentRunMode());
    if (verdict == battle_security::TokenSecretVerdict::kRefuse)
    {
        // 启动门禁已拒过一次;这里是纵深防御,不签空签名的票
        LOG_ERROR << "battle 票据签发被拒: battle_token_secret 为空且 run_mode=prod, battle_id="
                  << room.battleId << " player_id=" << playerId;
        return false;
    }
    if (gNode == nullptr)
    {
        return false;
    }
    // 本函数只在 loop 线程执行(Handle* 经 runInLoop 投递):GetNodeInfo() 读 thread_local,
    // 换到 gRPC 线程会拿到空 NodeInfo、整批静默拒签。
    const auto &self = gNode->GetNodeInfo();
    // 客户端可达地址(集群外入口 D76/D78,规则唯一权威实现在 client_endpoint::ClientFacing):
    // client_endpoint 可用就用它;否则 required=false 时回落 endpoint(podip 形态),
    // required=true 或 endpoint 本身不可用时拿不到 —— 拒签,调用方按 D70 fail-closed 回 kServiceUnavailable。
    const bool required = gNode->ClientEndpointRequired();
    const auto clientFacing = client_endpoint::ClientFacing(self, required);
    if (!clientFacing)
    {
        LOG_ERROR << "battle 票据签发被拒: 本节点没有客户端可达地址, battle_id=" << room.battleId
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
    const std::string payloadBytes = payload.SerializeAsString();

    std::string signature;
    if (verdict == battle_security::TokenSecretVerdict::kEnforce)
    {
        signature = battle_security::SignTicket(secret, payloadBytes);
        if (signature.empty())
        {
            LOG_ERROR << "battle 票据签名失败(OpenSSL), battle_id=" << room.battleId
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

bool BattleRoomManager::AttachDirectConnection(const uint64_t battleId, const uint64_t playerId,
                                               const ::eBattleTicketRole role,
                                               const muduo::net::TcpConnectionPtr &conn,
                                               uint32_t *gateSessionId)
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
        if (const auto it = room->routingByPlayer.find(playerId); it != room->routingByPlayer.end())
        {
            routing = &it->second;
        }
    }
    else if (role == ::BATTLE_TICKET_ROLE_OBSERVER)
    {
        if (const auto it = room->routingByObserver.find(playerId); it != room->routingByObserver.end())
        {
            routing = &it->second;
        }
    }
    if (routing == nullptr)
    {
        LOG_WARN << "battle 直连挂接被拒: 玩家不在名单, battle_id=" << battleId
                 << " player_id=" << playerId << " role=" << ::eBattleTicketRole_Name(role);
        return false;
    }

    auto &slot = room->directConnByPlayer[playerId];
    if (const auto old = slot.lock(); old && old != conn)
    {
        // 重连:先关旧的。forceClose 是 queueInLoop 异步的,旧连接的断开回调在本函数
        // 返回后才跑,那时 slot 已指向新连接,DetachDirectConnection 按身份比对不会误摘。
        LOG_INFO << "battle 直连重连,替换旧连接: battle_id=" << battleId << " player_id=" << playerId
                 << " old_peer=" << old->peerAddress().toIpPort()
                 << " new_peer=" << conn->peerAddress().toIpPort();
        old->forceClose();
    }
    slot = conn;
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
        LOG_INFO << "battle 关闭玩家直连: battle_id=" << room.battleId << " player_id=" << playerId
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
    LOG_INFO << "battle 关闭房间全部直连: battle_id=" << room.battleId
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

// ---- 结算持久化 + 有界重投(R07) ----

void BattleRoomManager::DispatchSettlementDurably(const ::BattleRouting &routing,
                                                  const uint64_t playerId,
                                                  const ::BattleSettlementData &settlement)
{
    const uint64_t battleId = settlement.battle_id();

    ::BattleSettlementEvent event;
    *event.mutable_settlement() = settlement;
    std::string payload;
    if (!event.SerializeToString(&payload))
    {
        LOG_ERROR << "battle 结算序列化失败: battle_id=" << battleId << " player_id=" << playerId;
        return;
    }

    const uint32_t sceneNodeId = routing.scene_node_id();
    const std::string sceneInstanceId = routing.scene_instance_id();

    if (!RedisReady())
    {
        // 降级:落不了库就直接投一次。丢了只能靠 scene reaper 按 deadline 解冻
        // (奖励确实会丢)—— 这与改造前的行为一致,不是新增的退化,但必须响一声。
        LOG_ERROR << "metric=battle_settlement_not_durable battle_id=" << battleId
                  << " player_id=" << playerId
                  << ",Redis 不可用,结算未落库直接投递(投递丢失即奖励丢失)";
        SendSettlementCommand(sceneNodeId, sceneInstanceId, playerId, battleId, payload);
        return;
    }

    // EVAL 两键同 SET 同 TTL;成功回调里才投递,保证"记录先于命令存在"。
    tlsRedis.GetZoneRedis()->command(
        [this, battleId, playerId, payload, sceneNodeId, sceneInstanceId](hiredis::Hiredis *,
                                                                          redisReply *reply)
        {
            const bool stored = reply != nullptr && reply->type != REDIS_REPLY_ERROR;
            if (!stored)
            {
                LOG_ERROR << "metric=battle_settlement_not_durable battle_id=" << battleId
                          << " player_id=" << playerId
                          << ",待结算记录落库失败,仍尝试投递一次(投递丢失即奖励丢失)";
                SendSettlementCommand(sceneNodeId, sceneInstanceId, playerId, battleId, payload);
                return;
            }
            SendSettlementCommand(sceneNodeId, sceneInstanceId, playerId, battleId, payload);
            EnqueuePendingSettlement(battleId, playerId, payload, sceneNodeId);
        },
        (std::string("EVAL %s 2 ") + battle_settlement::kPendingSettlementKeyFmt + " " +
         battle_settlement::kPendingSettlementIdKeyFmt + " %b %llu %u")
            .c_str(),
        battle_settlement::kSetPendingSettlementScript, playerId, playerId,
        payload.data(), payload.size(), battleId, kPendingSettlementTtlSec);
}

void BattleRoomManager::EnqueuePendingSettlement(const uint64_t battleId, const uint64_t playerId,
                                                 std::string payload,
                                                 const uint32_t originalSceneNodeId)
{
    PendingSettlement entry;
    entry.battleId = battleId;
    entry.playerId = playerId;
    entry.payload = std::move(payload);
    entry.originalSceneNodeId = originalSceneNodeId;
    settlementOutbox_[{battleId, playerId}] = std::move(entry);

    if (!settlementRetryTimer_.IsActive())
    {
        settlementRetryTimer_.RunEvery(kSettlementRetryIntervalSec,
                                       [this]
                                       { RetryPendingSettlements(); });
    }
}

void BattleRoomManager::StopSettlementRetryTimerIfIdle()
{
    if (settlementOutbox_.empty() && settlementRetryTimer_.IsActive())
    {
        settlementRetryTimer_.Cancel();
    }
}

void BattleRoomManager::RetryPendingSettlements()
{
    if (settlementOutbox_.empty())
    {
        StopSettlementRetryTimerIfIdle();
        return;
    }
    if (!RedisReady())
    {
        // 探测不了销账就不能判定,更不能盲目重投:等下一轮。次数也不消耗 ——
        // Redis 抖动不该把重投窗口白白烧掉。
        LOG_WARN << "battle 结算重投本轮跳过(Redis 未连接), pending=" << settlementOutbox_.size();
        return;
    }

    // 回调里会改 settlementOutbox_,先把本轮要处理的键抄一份。
    std::vector<std::pair<uint64_t, uint64_t>> keys;
    keys.reserve(settlementOutbox_.size());
    for (const auto &[key, entry] : settlementOutbox_)
    {
        keys.push_back(key);
    }
    for (const auto &[battleId, playerId] : keys)
    {
        ProbeAndRetryOne(battleId, playerId);
    }
}

void BattleRoomManager::ProbeAndRetryOne(const uint64_t battleId, const uint64_t playerId)
{
    // 第一跳:读伴生 id 键探测销账。
    tlsRedis.GetZoneRedis()->command(
        [this, battleId, playerId](hiredis::Hiredis *, redisReply *reply)
        {
            const auto it = settlementOutbox_.find({battleId, playerId});
            if (it == settlementOutbox_.end())
            {
                return; // 回调期间已被别的路径摘掉
            }
            const bool stillOurs = reply != nullptr && reply->type == REDIS_REPLY_STRING &&
                                   std::string(reply->str, reply->len) == std::to_string(battleId);
            if (!stillOurs)
            {
                LOG_INFO << "battle 结算已销账: battle_id=" << battleId << " player_id=" << playerId
                         << " attempts=" << it->second.attempts;
                settlementOutbox_.erase(it);
                StopSettlementRetryTimerIfIdle();
                return;
            }

            // 第二跳:重新解析玩家当前所在的 scene。**绝不复用**开局时抓的 uuid ——
            // R07 的失败序列里,那个 uuid 恰恰属于已经不在了的那个进程。
            if (!RedisReady())
            {
                return;
            }
            tlsRedis.GetZoneRedis()->command(
                [this, battleId, playerId](hiredis::Hiredis *, redisReply *locationReply)
                {
                    const auto entryIt = settlementOutbox_.find({battleId, playerId});
                    if (entryIt == settlementOutbox_.end())
                    {
                        return;
                    }
                    auto &entry = entryIt->second;
                    const uint32_t resolvedNodeId = ParseSceneNodeIdFromLocationReply(locationReply);

                    battle_settlement::RetryInput input;
                    input.pendingRecordStillOurs = true; // 第一跳已确认
                    input.locationResolved = resolvedNodeId != 0;
                    input.attemptsSoFar = entry.attempts;
                    input.maxAttempts = kSettlementRetryMaxAttempts;

                    const auto action = battle_settlement::ClassifyRetry(input);
                    ++entry.attempts;

                    switch (action)
                    {
                    case battle_settlement::RetryAction::kResend:
                        LOG_WARN << "metric=battle_settlement_resend battle_id=" << battleId
                                 << " player_id=" << playerId
                                 << " original_scene_node_id=" << entry.originalSceneNodeId
                                 << " resolved_scene_node_id=" << resolvedNodeId
                                 << " attempt=" << entry.attempts
                                 << ",结算未销账,按重新解析出的 scene 重投";
                        // instance uuid 留空 = "谁现在持有这个 node_id 就谁执行"。
                        SendSettlementCommand(resolvedNodeId, std::string{}, playerId, battleId,
                                              entry.payload);
                        return;
                    case battle_settlement::RetryAction::kSkipNoTarget:
                        LOG_INFO << "battle 结算重投本轮无目标(玩家不在任何场景): battle_id=" << battleId
                                 << " player_id=" << playerId << " attempt=" << entry.attempts
                                 << ",持久记录仍在,等玩家进场景时由登录钩子补应用";
                        return;
                    case battle_settlement::RetryAction::kExhausted:
                        // 响亮但不致命:记录仍在 Redis 里(TTL 7 天),玩家下次进场景补应用。
                        LOG_ERROR << "metric=battle_settlement_undelivered battle_id=" << battleId
                                  << " player_id=" << playerId << " attempts=" << entry.attempts
                                  << ",结算重投次数用尽仍未销账;待结算记录保留,"
                                  << "由 scene 的 OnPlayerEnterScene 登录钩子兜底应用";
                        settlementOutbox_.erase(entryIt);
                        StopSettlementRetryTimerIfIdle();
                        return;
                    case battle_settlement::RetryAction::kDone:
                        settlementOutbox_.erase(entryIt);
                        StopSettlementRetryTimerIfIdle();
                        return;
                    }
                },
                "GET player:%llu:location", playerId);
        },
        (std::string("GET ") + battle_settlement::kPendingSettlementIdKeyFmt).c_str(), playerId);
}

// ---- 活动局结果持久化 + 有界重发(帮会同道历练,guild-phase2 §6.19–§6.21) ----
//
// 崩溃窗口(§6.21):
//   R1/R2 战斗中或 SET 回调之前崩溃 → 无记录,guild 巡检器按超期把该局判 EXPIRED(无人得奖,口径一致);
//   R3 SET 成功、Kafka flush 前崩溃 → 记录在,guild 巡检器 GET 记录直接结算(内存 outbox 随进程丢失无妨);
//   R4 Kafka 投递失败 → 本类每 10s 重发,最多 30 次,之后巡检器兜底;
//   R5 guild 已结算但 DEL 失败 → 继续重发,guild 按 battle_id 判重后再 DEL;最坏 TTL 7 天自然过期;
//   R6 Redis 不可用 → 降级只发一次,metric=battle_activity_result_not_durable,须告警。
// 同一 battle_id 可能被投递多次(重发 / R5),match-results 的消费方必须按 battle_id 幂等(proto 注释已写明)。

void BattleRoomManager::DispatchActivityResultDurably(const contracts::kafka::BattleResultEvent &result)
{
    const uint64_t battleId = result.battle_id();

    std::string payload;
    if (!result.SerializeToString(&payload))
    {
        // 不可恢复:没有字节可落库也没有字节可发。guild 巡检器会把该局判 EXPIRED 并告警。
        LOG_ERROR << "metric=battle_activity_result_serialize_failed battle_id=" << battleId
                  << ",活动局结果序列化失败,既不落库也不投递";
        return;
    }

    if (!RedisReady())
    {
        // R6:落不了库就只发一次 —— 与改造前的行为一致,不是新增的退化,但必须响一声。
        LOG_ERROR << "metric=battle_activity_result_not_durable battle_id=" << battleId
                  << ",Redis 不可用,活动局结果未落库直接投递一次(投递丢失即该局帮会奖励丢失)";
        SendMatchResultPayload(battleId, payload);
        return;
    }

    // 成功回调里才投递,保证"记录先于事件存在"(见头文件 DispatchActivityResultDurably 的说明)。
    const std::string key = turnbattle::ActivityResultKey(battleId);
    const int rc = tlsRedis.GetZoneRedis()->command(
        [weakSelf = WeakSelf(), battleId, payload](hiredis::Hiredis *, redisReply *reply)
        {
            const bool stored = reply != nullptr && reply->type != REDIS_REPLY_ERROR;
            if (!stored)
            {
                // 回复为空(连接断开 / 停机释放)或出错:记录可能落了也可能没落,都不登记重发 ——
                // 没有记录就没有"是否已销账"可探测。发一次,剩下交给 guild 巡检器(有记录则结算,无则 EXPIRED)。
                LOG_ERROR << "metric=battle_activity_result_not_durable battle_id=" << battleId
                          << " redis_error="
                          << (reply != nullptr && reply->str != nullptr ? std::string(reply->str, reply->len)
                                                                        : std::string("null reply"))
                          << ",活动局结果落库失败,仍投递一次(投递丢失即该局帮会奖励丢失)";
                SendMatchResultPayload(battleId, payload);
                return;
            }
            SendMatchResultPayload(battleId, payload);
            if (const auto self = weakSelf.lock())
            {
                self->EnqueuePendingActivityResult(battleId, payload);
            }
        },
        "SET %s %b EX %u", key.c_str(), payload.data(), payload.size(),
        turnbattle::kActivityResultTtlSec);
    if (rc != REDIS_OK)
    {
        // 命令没发出去(连接恰在断开 / 释放中,Hiredis::command 在登记回调前就返回):回调永远不会来,
        // 不在这里补发这局结果就静默丢了。按 R6 降级只发一次。
        LOG_ERROR << "metric=battle_activity_result_not_durable battle_id=" << battleId
                  << ",活动局结果 SET 未能发出,未落库直接投递一次(投递丢失即该局帮会奖励丢失)";
        SendMatchResultPayload(battleId, payload);
    }
}

void BattleRoomManager::EnqueuePendingActivityResult(const uint64_t battleId, std::string payload)
{
    PendingActivityResult entry;
    entry.payload = std::move(payload);
    // 同 battle_id 覆盖:FinishBattle 每房间只走一次,正常不会重复;万一重复,以最新一份为准并重置次数。
    activityResultOutbox_[battleId] = std::move(entry);

    if (!activityResultRetryTimer_.IsActive())
    {
        activityResultRetryTimer_.RunEvery(static_cast<double>(turnbattle::kActivityResultRetryIntervalSec),
                                           [weakSelf = WeakSelf()]
                                           {
                                               if (const auto self = weakSelf.lock())
                                               {
                                                   self->RetryPendingActivityResults();
                                               }
                                           });
    }
}

void BattleRoomManager::StopActivityResultRetryTimerIfIdle()
{
    if (activityResultOutbox_.empty() && activityResultRetryTimer_.IsActive())
    {
        activityResultRetryTimer_.Cancel();
    }
}

void BattleRoomManager::RetryPendingActivityResults()
{
    if (activityResultOutbox_.empty())
    {
        StopActivityResultRetryTimerIfIdle();
        return;
    }
    if (!RedisReady())
    {
        // 探测不了销账就不能判定,更不能盲目重发:等下一轮,次数也不消耗(与 R07 同一取舍)。
        LOG_WARN << "battle 活动局结果重发本轮跳过(Redis 未连接), pending=" << activityResultOutbox_.size();
        return;
    }

    // 回调里会改 activityResultOutbox_,先把本轮要处理的键抄一份。
    std::vector<uint64_t> battleIds;
    battleIds.reserve(activityResultOutbox_.size());
    for (const auto &[battleId, entry] : activityResultOutbox_)
    {
        battleIds.push_back(battleId);
    }
    for (const auto battleId : battleIds)
    {
        ProbeActivityResultOne(battleId);
    }
}

void BattleRoomManager::ProbeActivityResultOne(const uint64_t battleId)
{
    const std::string key = turnbattle::ActivityResultKey(battleId);
    tlsRedis.GetZoneRedis()->command(
        [weakSelf = WeakSelf(), battleId](hiredis::Hiredis *, redisReply *reply)
        {
            const auto self = weakSelf.lock();
            if (!self)
            {
                return;
            }
            auto &outbox = self->activityResultOutbox_;
            const auto it = outbox.find(battleId);
            if (it == outbox.end())
            {
                return; // 回调期间已被别的路径摘掉(例如上一轮的迟到回调)
            }
            auto &entry = it->second;

            if (reply == nullptr)
            {
                // 空回复 = 连接断开 / 停机释放,什么也没问到:本轮不判定、不重发、不消耗次数,
                // 下一轮再探(与 RetryPendingActivityResults 里 Redis 未连接时的取舍一致)。
                LOG_WARN << "battle 活动局结果探测无回复(Redis 连接断开),本轮跳过: battle_id=" << battleId;
                return;
            }
            // 只有明确读到 0 才算已销账;回复出错(或类型不对)一律按"仍在"处理 —— 宁可多发一次
            // (guild 按 battle_id 判重),也不能把没销账的局当成已送达。
            const bool recordGone = reply->type == REDIS_REPLY_INTEGER && reply->integer == 0;
            const auto action = turnbattle::ClassifyActivityResultRetry(
                !recordGone, entry.attempts, turnbattle::kActivityResultRetryMaxAttempts);

            switch (action)
            {
            case turnbattle::ActivityResultRetryAction::kDone:
                LOG_INFO << "battle 活动局结果已销账: battle_id=" << battleId << " resends=" << entry.attempts;
                outbox.erase(it);
                self->StopActivityResultRetryTimerIfIdle();
                return;
            case turnbattle::ActivityResultRetryAction::kResend:
                ++entry.attempts;
                LOG_WARN << "metric=battle_activity_result_resend battle_id=" << battleId
                         << " attempt=" << entry.attempts
                         << ",活动局结果未被 guild 销账,按原字节重发";
                SendMatchResultPayload(battleId, entry.payload);
                return;
            case turnbattle::ActivityResultRetryAction::kExhausted:
                // 响亮但不致命:记录仍在 Redis(TTL 7 天),guild 巡检器超期扫描时从记录直接结算。
                LOG_ERROR << "metric=battle_activity_result_undelivered battle_id=" << battleId
                          << " resends=" << entry.attempts
                          << ",活动局结果重发次数用尽仍未销账;持久记录保留,由 guild 巡检器兜底结算";
                outbox.erase(it);
                self->StopActivityResultRetryTimerIfIdle();
                return;
            }
        },
        "EXISTS %s", key.c_str());
}

void BattleRoomManager::HandleIssueBattleTicket(const ::IssueBattleTicketRequest &request,
                                                ::IssueBattleTicketResponse &response)
{
    // match 已从大厅会话 metadata 取得权威 player_id(客户端请求体里没有 player_id),
    // battle 只核对名单;这里的 0 只可能是 match 侧漏填,按参数错误拒绝而非 kPlayerNotFoundInSession
    const auto playerId = request.player_id();
    if (playerId == 0)
    {
        LOG_WARN << "IssueBattleTicket 请求缺少 player_id: battle_id=" << request.battle_id();
        response.mutable_error_message()->set_id(kInvalidParameter);
        return;
    }

    auto *room = FindRoom(request.battle_id());
    if (room == nullptr)
    {
        // 房间已结束/作废:match 原样透传,客户端据此丢弃本地战斗 UI(与 GetBattleState 空响应同语义)
        LOG_DEBUG << "IssueBattleTicket 房间不存在: battle_id=" << request.battle_id()
                  << " player_id=" << playerId;
        response.mutable_error_message()->set_id(kInvalidParameter);
        return;
    }

    ::eBattleTicketRole role = ::BATTLE_TICKET_ROLE_NONE;
    if (room->routingByPlayer.find(playerId) != room->routingByPlayer.end())
    {
        role = ::BATTLE_TICKET_ROLE_PARTICIPANT;
    }
    else if (room->routingByObserver.find(playerId) != room->routingByObserver.end())
    {
        role = ::BATTLE_TICKET_ROLE_OBSERVER;
    }
    else
    {
        LOG_WARN << "IssueBattleTicket 玩家不在此战斗: battle_id=" << request.battle_id()
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
    LOG_INFO << "IssueBattleTicket 补签成功: battle_id=" << request.battle_id()
             << " player_id=" << playerId << " role=" << ::eBattleTicketRole_Name(role);
}
