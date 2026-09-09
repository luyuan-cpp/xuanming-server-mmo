#pragma once

#include <cstdint>
#include <map>
#include <memory>
#include <string>
#include <unordered_map>
#include <utility>
#include <vector>

#include "muduo/net/TcpConnection.h"
#include "time/comp/timer_task_comp.h"

// 回合引擎(cpp/libs/services/battle,纯逻辑库,API 见设计文档 §5.1)
#include "system/turn_battle_engine.h"

#include "proto/battle/battle_data.pb.h"
#include "proto/battle/battle_node.pb.h"
#include "proto/battle/player_battle.pb.h"
#include "proto/common/base/session.pb.h"

// 回合制战斗房间管理器(设计文档 §5.2)。
//
// 线程模型:所有方法只允许在 muduo loop 线程执行(gRPC handler 经
// runInLoop + promise 投递进来,timer 回调本来就在 loop 线程),
// 因此内部完全无锁;TimerTaskComp 也依赖当前线程 loop。
//
// 生命周期:房间是纯内存对象,节点崩溃即战斗作废(设计文档 §8);
// 补偿全部由 scene 侧 reaper 按 InBattleComp.deadline_ms 兜底(§3.2),
// 本类不做任何持久化,对玩家权威数据零写权(宪法 §7 新增不变量 4)。
//
// 出站(全走 Kafka producer,设计文档 D6;寻址方式见
// docs/design/control-plane-topic-partitioning-20260908.md,由 ResolveCommandRoute 统一给出):
//   S2C     → GateCommand{PushToPlayerEvent},topic=gate-cmd_g<N> 的
//             gate_node_id % P 号分区,key=player_id,
//             target_gate_id=gate_node_id + target_instance_id=BattleRouting.gate_instance_id;
//   绑定     → BindBattleEvent / UnbindBattleEvent(同上走 GateCommand 信封);
//   结算     → SceneCommand{DispatchEvent, BattleSettlementEvent},
//             topic=scene-cmd_g<N> 的 scene_node_id % P 号分区,key=player_id,
//             target_scene_id=scene_node_id + target_instance_id=BattleRouting.scene_instance_id;
//   确认     → SceneCommand{DispatchEvent, BattleConfirmedEvent}(CreateBattle 成功后每玩家一条,
//             scene 据此 PREPARING→FIGHTING 并把作废期限切到正式 deadline,同上信封);
//   对局结果 → contracts.kafka.BattleResultEvent,topic=match-results(全局无 zone 段),
//             key=battle_id;只在真实打完(FinishBattle)时发,Destroy/Abort 作废不发。
//
// 客户端直连(设计文档 §18,D23-D28):房间为每个参战者 / 观众最多保留一条已验证的
// 直连(directConnByPlayer)。S2C 有直连即直发(PushToPlayer),否则回落上面的
// Kafka→gate 路径;绑定 / 解绑 / 结算 / 确认事件仍全走 Kafka。票据由本节点签发
// (BuildAssignment),开局 / 观战接入时先推 BattleAssignedS2C 再推首帧。
//
// 配表指纹(cross-zone-matchmaking.md §10):CreateBattle 时把 request/players[i] 携带的
// 指纹与本节点六张战斗表指纹比对,不一致按 game_config.yaml battle_table_fingerprint_mode
// (warn|enforce|off,默认 warn)处理。
class BattleClientEdge;

class BattleRoomManager
{
public:
    static BattleRoomManager &Instance();

    // ---- 客户端直连面(main.cpp 装配;BattleClientEdge 生命周期由 main 的运行时上下文持有) ----

    void SetClientEdge(BattleClientEdge *edge) { edge_ = edge; }

    // 票据握手通过后把直连挂到房间上。校验"房间仍存在 + 该玩家按 role 确在名单上";
    // 同一玩家已有直连(重连)则关闭旧连接、换成新的。成功时回填该玩家的 gate 会话号
    // (合成 SessionDetails 用),返回 false = 拒绝握手。
    bool AttachDirectConnection(uint64_t battleId, uint64_t playerId, ::eBattleTicketRole role,
                                const muduo::net::TcpConnectionPtr &conn, uint32_t *gateSessionId);

    // 直连断开:只摘"仍指向本连接"的记录(晚到的旧连接 FIN 不能摘掉重连后的新连接);
    // 房间已不存在时 no-op。
    void DetachDirectConnection(uint64_t battleId, uint64_t playerId,
                                const muduo::net::TcpConnectionPtr &conn);

    // ---- match → battle 内部 gRPC(生成骨架守护段一行委托到这里) ----

    // 丢票补签(D25;改道见 client-rpc-router.md D33):match 已从大厅会话取得权威 player_id,
    // battle 只核对名单 —— 仅当该玩家仍是房间的参战者 / 观众时才签,否则 error_message 非零
    // (player_id 缺失 / 房间不存在 / 不在名单 = kInvalidParameter,签不出票 = kServiceUnavailable)。
    // 消息类型来自 proto/battle/battle_node.pb.h。
    void HandleIssueBattleTicket(const ::IssueBattleTicketRequest &request,
                                 ::IssueBattleTicketResponse &response);

    // 幂等:同 battle_id 重复创建直接回 OK(match 补偿路径可能重试)。
    void HandleCreateBattle(const ::CreateBattleRequest &request, ::CreateBattleResponse &response);

    // 幂等:房间不存在视为已销毁。销毁只解绑,不发结算(补偿/回滚路径)。
    void HandleDestroyBattle(const ::DestroyBattleRequest &request);

    // 观战接入(设计文档 §10.5)。错误 tip 约定:房间不存在=kEntityIsNull
    // (match 据此懒剔除 Redis 索引并换场重试)、观众满=kRateLimitExceeded、
    // 观众是参战者=kInvalidParameter。幂等:同 observer 重复 Add 只重推首帧。
    void HandleAddObserver(const ::AddObserverRequest &request, ::AddObserverResponse &response);

    // 幂等移除(match 互斥清退路径):向该观众推 SpectateEnd(REMOVED)+ 解绑。
    void HandleRemoveObserver(const ::RemoveObserverRequest &request);

    // ---- gate → battle 客户端消息(权威身份来自会话 metadata,不信请求体) ----

    // 参战/观战路由的**自愈**(routing-identity-audit-20260908.md R10)。
    //
    // BattleRouting 在 CreateBattle 时抓一次就再不刷新,战中换 gate / 换会话之后
    // Kafka→gate 的回落推送就投到旧 gate 的旧 session 上,静默失效。
    // 修法是"用已经在线上的数据自愈":gate 在每一条经它转发的客户端 RPC 上都会把
    // 自己的 session_id / node_id / 实例 uuid 盖进 SessionDetails
    // (client_message_processor.cpp BuildSessionDetails),这就是当前真值。
    // 每个 Handle* 入口顺手比一次、不同就更新,重连后客户端的第一条包(重连流程里
    // 必发的 GetBattleState)就把路由修好了。
    //
    // 只在 gate_instance_id 非空时才采信:客户端**直连面**合成的 SessionDetails
    // (battle_client_edge.cpp)只有 player_id + 从房间里取回的旧 session_id,
    // gate 身份两项为空 —— 拿它去"刷新"等于把旧值再写一遍,更糟的是会掩盖真实变更。
    void RefreshRoutingFromSession(const ::SessionDetails &sessionDetails);

    void HandleSubmitBattleAction(const ::SessionDetails &sessionDetails,
                                  const ::SubmitBattleActionRequest &request,
                                  ::SubmitBattleActionResponse &response);

    void HandleGetBattleState(const ::SessionDetails &sessionDetails,
                              const ::GetBattleStateRequest &request,
                              ::BattleStateS2C &response);

    // 观众主动退出:只解绑不推 SpectateEnd(是客户端自己发起的,再推是回声);
    // 观众不存在也回成功(幂等,重复点退出/晚到的退出包都无害)。
    void HandleStopWatchBattle(const ::SessionDetails &sessionDetails,
                               const ::StopWatchBattleRequest &request,
                               ::StopWatchBattleResponse &response);

    // 自动战斗开关(设计文档 §11.2):落引擎 is_auto;开启后若全员就绪,
    // 走与 SubmitBattleAction 相同的提前结算路径。
    void HandleSetAutoBattle(const ::SessionDetails &sessionDetails,
                             const ::SetAutoBattleRequest &request,
                             ::SetAutoBattleResponse &response);

    // 停机收尾:全部房间作废(仅向 gate 发解绑,不结算),供 SetBeforeShutdown 调用。
    void AbortAllRooms(const std::string &reason);

    // ---- 结算重投的调参(测试与运维需要看得见,故放公开区)----
    //
    // 重投窗口 = kSettlementRetryIntervalSec × kSettlementRetryMaxAttempts = 120s。
    // 上界取自"要覆盖一次 scene 进程重启 + 服务发现补齐"的时间尺度;再长没有意义:
    // 持久记录仍在(TTL 7 天),玩家下次进场景会由登录钩子补应用。
    static constexpr double kSettlementRetryIntervalSec = 10.0;
    static constexpr uint32_t kSettlementRetryMaxAttempts = 12;
    // 与 scene 侧 PlayerBattleSystem::kPendingSettlementTtlSec 同值(设计文档 §6)。
    // 两处必须一致:battle 写、scene 读与销账,TTL 分叉就是"记录先于消费者过期"。
    static constexpr uint32_t kPendingSettlementTtlSec = 7 * 24 * 3600;

private:
    BattleRoomManager() = default;

    struct BattleRoom
    {
        uint64_t battleId = 0;
        // 开局参数副本(BattleResultEvent 回流给 match 用;引擎内的请求副本是私有的)
        uint32_t matchMode = 0;
        uint32_t battleConfigId = 0;
        turnbattle::TurnBattleEngine engine;
        // player_id → 路由信息副本(快照携带,battle 不查 etcd 定位对端)。
        // 有序容器:广播与结算的遍历顺序稳定,日志/回放可复现。
        std::map<uint64_t, ::BattleRouting> routingByPlayer;
        // 观众路由(设计文档 §10.5)。routing 的 scene 字段恒为 0:观众零写权、
        // 无结算,绝不向观众的 scene 发任何事件(不变量 6),出站只走 gate。
        std::map<uint64_t, ::BattleRouting> routingByObserver;
        // 观众名字(AddObserverRequest.observer_name,仅日志/后续观众列表用)
        std::map<uint64_t, std::string> observerNames;
        TimerTaskComp roundTimer;      // 回合 action_deadline
        TimerTaskComp battleTimer;     // 整场 deadline_ms(强制收尾,防房间泄漏)
        uint64_t actionDeadlineMs = 0; // 当前回合行动截止(Unix 毫秒,GetBattleState 回填)
        uint64_t deadlineMs = 0;       // 整场作废期限(与 battleTimer 同值;补发确认事件时带给 scene)
        // BattleConfirmedEvent 补发:开局后 kConfirmResendWindowMs 内每 kConfirmResendIntervalSec
        // 向全部参战玩家重发一次(scene 幂等)。没有 scene→battle 的确认回执通道(proto 已定),
        // 用有界周期补发覆盖"首发 produce 失败 / Kafka 积压 / scene 消费者 rebalance"这类
        // 单次投递丢失或迟到;窗口按 scene 侧锁保留期(prepare TTL 最长 78s + 60s 余量)取整。
        TimerTaskComp confirmResendTimer;
        uint64_t confirmResendUntilMs = 0;
        // player_id(参战者或观众)→ 已验证的客户端直连;缺项 = 该玩家走 Kafka→gate 回落。
        // 有序容器:与 routingByPlayer 同口径,收尾遍历顺序稳定。
        std::map<uint64_t, muduo::net::TcpConnectionPtr> directConnByPlayer;
    };

    BattleRoom *FindRoom(uint64_t battleId);

    // 确认事件周期补发(confirmResendTimer 回调):窗口已过则停表,否则对全员重发。
    void ResendBattleConfirmed(uint64_t battleId);

    // 装填下一回合行动收集窗口并记录截止时间。
    void ArmRoundTimer(BattleRoom &room);

    // 结算当前回合:全员就绪或回合超时触发。广播 TurnResult,
    // 胜负已分则走 FinishBattle。按 battle_id 重查房间,防 timer 迟到打到已销毁房间。
    void ResolveRound(uint64_t battleId);

    // 整场 deadline 到期:引擎仍未分出胜负按平局强制收尾(battle_node.proto 契约)。
    void OnBattleDeadline(uint64_t battleId);

    // 战斗收尾:每参与者 BattleEndS2C → BattleSettlementEvent → UnbindBattleEvent;
    // 观众按 spectateReason 推 SpectateEndS2C + 解绑(§10.5);
    // 最后向 match-results 发一条 BattleResultEvent(评分回流)。
    // 只组装与发送,不动 rooms_(房间由调用方随后移除)。
    void FinishBattle(BattleRoom &room, ::eBattleOutcome outcome,
                      ::eSpectateEndReason spectateReason);

    void BroadcastTurnResult(const BattleRoom &room, const ::TurnResultS2C &result);

    // 观战首帧:全量快照 + 房间 timer 回填的行动截止 + 当前观众数。
    void PushSpectateState(const BattleRoom &room, uint64_t observerId,
                           const ::BattleRouting &routing) const;

    // 收尾/作废统一出口:向全部观众推 SpectateEndS2C + UnbindBattleEvent,
    // 然后清空观众表(此后任何广播都不会再打到观众)。
    void NotifySpectateEndAndUnbind(BattleRoom &room, ::eSpectateEndReason reason,
                                    ::eBattleOutcome outcome);

    // ---- 客户端直连(设计文档 §18) ----

    // S2C 统一出口:该玩家有活着的直连就直发 MessageContent,否则回落 Kafka→gate
    //(routing 只在回落时用)。const:PushSpectateState 是 const 方法。
    void PushToPlayer(const BattleRoom &room, uint64_t playerId, const ::BattleRouting &routing,
                      uint32_t messageId, const ::google::protobuf::Message &message) const;

    // 签票据并组装落点分配包。false = 签不出(空密钥 + prod / OpenSSL 失败 / 本节点
    // endpoint 未就绪),调用方不下发。expire_at_ms = room.deadlineMs。
    bool BuildAssignment(const BattleRoom &room, uint64_t playerId, ::eBattleTicketRole role,
                         ::BattleAssignedS2C &out) const;

    // BuildAssignment + 经 PushToPlayer 下发 NotifyBattleAssigned(开局 / 观战接入首帧之前)。
    void PushAssignment(const BattleRoom &room, uint64_t playerId, const ::BattleRouting &routing,
                        ::eBattleTicketRole role) const;

    // 关闭并摘除一个玩家的直连(观众退出 / 被清退)。立刻从表里摘除,但 shutdown() +
    // 延迟强关**推迟到本轮 loop 之后**(queueInLoop):Handle* 可能就是从这条直连进来的,
    // 直连面要在 Handle* 返回后才写应答;shutdown / forceCloseWithDelay 都会同步把连接切到
    // kDisconnecting,那时再写就被丢。推迟一轮后,应答已在输出缓冲里,shutdown 仍等排空再 FIN,
    // 顺序仍是:终局包 → 应答 → FIN。
    void CloseDirectConnectionOf(BattleRoom &room, uint64_t playerId, const char *reason);

    // 关闭并清空房间全部直连(结束 / 作废 / 销毁;必须在终局包推完之后调用)。
    // 同样推迟到本轮 loop 之后:打完最后一击的 SubmitBattleAction / SetAutoBattle 会同步走到
    // FinishBattle,它的应答此刻还没写。
    void CloseDirectConnections(BattleRoom &room, const char *reason);

    // ---- 结算发件箱(R07:结算必须可重投,且重投要重新解析目标)----
    //
    // 判定逻辑在 services/battle/settlement/settlement_outbox.h(纯函数,单测直接盯)。
    // 这里只放"条目 + 一个节点级定时器",房间在 FinishBattle 之后就被销毁了,
    // 定时器不能挂在房间上。
    struct PendingSettlement
    {
        uint64_t battleId = 0;
        uint64_t playerId = 0;
        // 序列化好的 BattleSettlementEvent(重投时原样再发,不重算 —— 结算必须幂等且一致)。
        std::string payload;
        // 开局时抓到的 scene 落点,只用于日志对照:重投**一律**用重新解析出来的 node_id。
        uint32_t originalSceneNodeId = 0;
        uint32_t attempts = 0;
    };

    // 结算的**唯一**出口(R07)。顺序是硬要求:
    //   Redis 落库 → (落库回调里)投 SceneCommand → 登记进 outbox 等销账。
    // 不能反过来先投再落库:投递可能比 Redis 的 SET 先到,scene 应用结算后销账
    // (此刻记录还不存在,销账是空操作),随后迟到的 SET 落地就留下一条孤儿记录,
    // 玩家下次登录会被**重复发一次奖励**。
    // Redis 不可用时降级为"直接投递"并报 ERROR —— 那是既有行为,不是本次引入的退化。
    void DispatchSettlementDurably(const ::BattleRouting &routing, uint64_t playerId,
                                   const ::BattleSettlementData &settlement);

    // 落库成功后登记一条待销账记录并保证重投定时器已装填。
    void EnqueuePendingSettlement(uint64_t battleId, uint64_t playerId, std::string payload,
                                  uint32_t originalSceneNodeId);
    // 定时器回调:对每条未销账的记录探测 ACK、重新解析目标、按判定重投或收尾。
    void RetryPendingSettlements();
    // 单条记录的一轮处理(Redis 回调链的入口)。
    void ProbeAndRetryOne(uint64_t battleId, uint64_t playerId);
    // outbox 空了就停表:battle 常态下没有未销账记录,不该留一个每 10s 空转的定时器。
    void StopSettlementRetryTimerIfIdle();

    BattleClientEdge *edge_ = nullptr;

    std::unordered_map<uint64_t, std::unique_ptr<BattleRoom>> rooms_;

    // key = (battle_id, player_id);量级 = 未销账的结算条数,常态为 0。
    std::map<std::pair<uint64_t, uint64_t>, PendingSettlement> settlementOutbox_;
    TimerTaskComp settlementRetryTimer_;
};
