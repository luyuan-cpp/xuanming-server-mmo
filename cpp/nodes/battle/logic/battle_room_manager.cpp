#include "battle_room_manager.h"

#include <string>
#include <vector>

#include "muduo/base/Logging.h"

#include "engine/infra/messaging/kafka/kafka_producer.h"
#include "node/system/node/node.h"
#include "time/system/time.h"

#include "constants/turn_battle_constants.h"

#include "proto/common/base/message.pb.h"
#include "proto/common/event/battle_event.pb.h"
#include "proto/contracts/kafka/gate_command.pb.h"
#include "proto/contracts/kafka/gate_event.pb.h"
#include "proto/contracts/kafka/scene_command.pb.h"

#include "table/proto/tip/common_error_tip.pb.h"

// 事件 id 注册表常量(重生成后出现/更新):
//   contracts_kafka_gate_event_event_id.h  → ContractsKafkaBindBattleEventEventId /
//                                            ContractsKafkaUnbindBattleEventEventId /
//                                            ContractsKafkaPushToPlayerEventEventId
//   common_event_battle_event_event_id.h   → BattleSettlementEventEventId
#include "rpc/service_metadata/contracts_kafka_gate_event_event_id.h"
#include "rpc/service_metadata/common_event_battle_event_event_id.h"
// 客户端消息 id 常量(proto/battle/player_battle.proto 重生成后出现):
//   BattleClientPlayerNotifyBattleStartMessageId / NotifyTurnResult / NotifyBattleEnd
#include "rpc/service_metadata/player_battle_service_metadata.h"

namespace
{

    // ---- Kafka 出站(设计文档 D6;不变量:target_instance_id 必填 + key=业务实体 ID) ----

    // 发一条 GateCommand 到目标 gate 的 topic。
    // key=player_id:同一玩家的 S2C/绑定事件在 gate 侧保序(宪法 §7 不变量 3)。
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
        command.set_target_instance_id(routing.gate_instance_id());
        command.set_payload(std::move(payload));

        const std::string topic = "gate-" + std::to_string(routing.gate_node_id());
        const auto err = KafkaProducer::Instance().send(topic, command.SerializeAsString(),
                                                        std::to_string(playerId));
        if (err != RdKafka::ERR_NO_ERROR)
        {
            LOG_ERROR << "battle 出站 GateCommand 发送失败: topic=" << topic
                      << " player_id=" << playerId << " event_id=" << eventId << " err=" << err;
        }
    }

    // S2C 推送:MessageContent 包客户端协议消息,经 PushToPlayerEvent 送到玩家 TCP 连接。
    void PushMessageToPlayer(const ::BattleRouting &routing, const uint64_t playerId,
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

    // 绑定:把 session 的 BattleNodeService 绑定指到本节点(CreateBattle 成功后)。
    void SendBindBattle(const ::BattleRouting &routing, const uint64_t playerId,
                        const uint64_t battleId, const uint32_t battleNodeId)
    {
        contracts::kafka::BindBattleEvent event;
        event.set_session_id(routing.session_id());
        event.set_battle_node_id(battleNodeId);
        event.set_battle_id(battleId);
        event.set_player_id(playerId);

        SendGateCommand(routing, playerId, ContractsKafkaBindBattleEventEventId,
                        event.SerializeAsString());
    }

    // 解绑:战斗结束/作废。gate 侧按 battle_id 匹配当前绑定,迟到的解绑不清新战斗。
    void SendUnbindBattle(const ::BattleRouting &routing, const uint64_t playerId,
                          const uint64_t battleId)
    {
        contracts::kafka::UnbindBattleEvent event;
        event.set_session_id(routing.session_id());
        event.set_battle_id(battleId);
        event.set_player_id(playerId);

        SendGateCommand(routing, playerId, ContractsKafkaUnbindBattleEventEventId,
                        event.SerializeAsString());
    }

    // 结算:每玩家一条 SceneCommand{DispatchEvent, BattleSettlementEvent}。
    // scene 是结算的唯一应用者(宪法 §7 新增不变量 4)。
    void SendSettlementEvent(const ::BattleRouting &routing, const uint64_t playerId,
                             const ::BattleSettlementData &settlement)
    {
        if (routing.scene_instance_id().empty())
        {
            LOG_ERROR << "battle 结算事件被拒: scene_instance_id 为空, player_id=" << playerId
                      << " battle_id=" << settlement.battle_id();
            return;
        }

        ::BattleSettlementEvent event;
        *event.mutable_settlement() = settlement;

        contracts::kafka::SceneCommand command;
        command.set_command_type(contracts::kafka::SceneCommand::DispatchEvent);
        command.set_player_id(playerId);
        command.set_target_scene_id(routing.scene_node_id());
        command.set_payload(event.SerializeAsString());
        command.set_target_instance_id(routing.scene_instance_id());
        command.set_event_id(BattleSettlementEventEventId);

        const std::string topic = "scene-" + std::to_string(routing.scene_node_id());
        const auto err = KafkaProducer::Instance().send(topic, command.SerializeAsString(),
                                                        std::to_string(playerId));
        if (err != RdKafka::ERR_NO_ERROR)
        {
            LOG_ERROR << "battle 结算事件发送失败: topic=" << topic
                      << " player_id=" << playerId << " battle_id=" << settlement.battle_id()
                      << " err=" << err;
        }
    }

    // 本节点 node_id(BindBattleEvent 需要;gNode 在 Node 构造时已就绪)。
    uint32_t SelfNodeId()
    {
        return gNode != nullptr ? gNode->GetNodeId() : 0;
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
    const auto it = rooms_.find(battleId);
    return it == rooms_.end() ? nullptr : it->second.get();
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

    auto room = std::make_unique<BattleRoom>();
    room->battleId = battleId;
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

    auto *roomPtr = room.get();
    rooms_.emplace(battleId, std::move(room));

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
    roomPtr->battleTimer.RunAfter(static_cast<double>(deadlineMs - nowMs) / 1000.0,
                                  [this, battleId]
                                  { OnBattleDeadline(battleId); });

    // 第一回合行动收集窗口
    ArmRoundTimer(*roomPtr);

    // 每参与者:先绑定(后续 SubmitBattleAction 才能经 gate 路由进来),再推开战包
    const auto nodeId = SelfNodeId();
    ::BattleStartS2C start;
    start.set_battle_id(battleId);
    *start.mutable_state() = roomPtr->engine.BuildStateSnapshot();
    start.mutable_state()->set_battle_id(battleId);
    // 引擎无时钟,行动截止由节点按房间 timer 回填
    start.mutable_state()->set_action_deadline_ms(roomPtr->actionDeadlineMs);

    for (const auto &[playerId, routing] : roomPtr->routingByPlayer)
    {
        SendBindBattle(routing, playerId, battleId, nodeId);
        PushMessageToPlayer(routing, playerId, BattleClientPlayerNotifyBattleStartMessageId, start);
    }

    LOG_INFO << "CreateBattle 成功: battle_id=" << battleId
             << " players=" << request.players_size()
             << " battle_config_id=" << request.battle_config_id()
             << " match_mode=" << request.match_mode()
             << " deadline_ms=" << deadlineMs;
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

    // 补偿/回滚路径:只解绑,不发结算(冻结解除由 match 调 scene CancelBattlePrepare
    // 或 scene reaper 按 deadline 兜底,设计文档 §3.2)
    room->roundTimer.Cancel();
    room->battleTimer.Cancel();
    for (const auto &[playerId, routing] : room->routingByPlayer)
    {
        SendUnbindBattle(routing, playerId, battleId);
    }
    rooms_.erase(battleId);

    LOG_INFO << "DestroyBattle 完成: battle_id=" << battleId << " reason=" << request.reason();
}

void BattleRoomManager::HandleSubmitBattleAction(const ::SessionDetails &sessionDetails,
                                                 const ::SubmitBattleActionRequest &request,
                                                 ::SubmitBattleActionResponse &response)
{
    // 权威身份只认 gate 注入的会话 metadata,请求体里没有也不允许有 player_id
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

    // 非法行动(死亡/已逃/校验链不过)引擎不落账,按未提交处理,
    // 回合超时的默认普攻兜底(引擎契约);对客户端一律回 OK,不泄漏校验细节
    const bool allReady = room->engine.SubmitAction(playerId, request.action());
    if (allReady)
    {
        // 全员就绪提前结算,不等 action_deadline
        ResolveRound(room->battleId);
    }
}

void BattleRoomManager::HandleGetBattleState(const ::SessionDetails &sessionDetails,
                                             const ::GetBattleStateRequest &request,
                                             ::BattleStateS2C &response)
{
    const auto playerId = sessionDetails.player_id();
    auto *room = FindRoom(request.battle_id());
    if (room == nullptr || playerId == 0 ||
        room->routingByPlayer.find(playerId) == room->routingByPlayer.end())
    {
        // 战斗已不存在(结束/作废)或非参战者:回默认状态,battle_id 置 0,
        // 客户端据此丢弃本地战斗 UI(重连补拉的失败分支)
        LOG_DEBUG << "GetBattleState 房间不存在或非参战者: battle_id=" << request.battle_id()
                  << " player_id=" << playerId;
        response.Clear();
        return;
    }

    response = room->engine.BuildStateSnapshot();
    response.set_battle_id(room->battleId);
    // 引擎无时钟,行动截止由节点回填
    response.set_action_deadline_ms(room->actionDeadlineMs);
}

void BattleRoomManager::ArmRoundTimer(BattleRoom &room)
{
    // 行动收集窗口 = 一回合等效挂钟时长(v1 策略,与时间→回合换算共用常量)
    room.actionDeadlineMs = TimeSystem::NowMillisecondsUTC() + turnbattle::kRoundDurationMs;
    const auto battleId = room.battleId;
    // timer 回调按 battle_id 重查房间:房间可能在窗口内被销毁,不能捕获裸指针
    room.roundTimer.RunAfter(static_cast<double>(turnbattle::kRoundDurationMs) / 1000.0,
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

    BroadcastTurnResult(*room, result);

    if (outcome != ::BATTLE_OUTCOME_ONGOING)
    {
        FinishBattle(*room, outcome);
        rooms_.erase(battleId);
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
    FinishBattle(*room, outcome);
    rooms_.erase(battleId);
}

void BattleRoomManager::FinishBattle(BattleRoom &room, const ::eBattleOutcome outcome)
{
    room.roundTimer.Cancel();
    room.battleTimer.Cancel();

    for (const auto &[playerId, routing] : room.routingByPlayer)
    {
        ::BattleSettlementData settlement = room.engine.BuildSettlement(playerId);
        // 强制平局路径引擎结算里还是 ONGOING,以节点判定为准统一盖章
        settlement.set_outcome(outcome);
        settlement.set_battle_id(room.battleId);

        // 顺序即协议:客户端先收终局包,scene 再应用结算,最后 gate 解绑(§3.1)
        ::BattleEndS2C end;
        end.set_battle_id(room.battleId);
        end.set_outcome(outcome);
        *end.mutable_settlement() = settlement;
        PushMessageToPlayer(routing, playerId, BattleClientPlayerNotifyBattleEndMessageId, end);

        SendSettlementEvent(routing, playerId, settlement);
        SendUnbindBattle(routing, playerId, room.battleId);
    }

    LOG_INFO << "战斗结束: battle_id=" << room.battleId
             << " outcome=" << ::eBattleOutcome_Name(outcome)
             << " players=" << room.routingByPlayer.size();
}

void BattleRoomManager::BroadcastTurnResult(const BattleRoom &room, const ::TurnResultS2C &result)
{
    // 每玩家一条 PushToPlayerEvent,key=player_id:保证同一玩家的
    // TurnResult 与随后的 BattleEnd 在 gate 侧有序(宪法 §7 不变量 3)。
    for (const auto &[playerId, routing] : room.routingByPlayer)
    {
        PushMessageToPlayer(routing, playerId, BattleClientPlayerNotifyTurnResultMessageId, result);
    }
}

void BattleRoomManager::AbortAllRooms(const std::string &reason)
{
    if (rooms_.empty())
    {
        return;
    }

    LOG_INFO << "作废全部战斗房间: rooms=" << rooms_.size() << " reason=" << reason;
    for (auto &[battleId, room] : rooms_)
    {
        room->roundTimer.Cancel();
        room->battleTimer.Cancel();
        // 只解绑不结算:战斗作废,scene reaper 按 InBattleComp.deadline_ms 解冻(§3.2)
        for (const auto &[playerId, routing] : room->routingByPlayer)
        {
            SendUnbindBattle(routing, playerId, battleId);
        }
    }
    rooms_.clear();
}
