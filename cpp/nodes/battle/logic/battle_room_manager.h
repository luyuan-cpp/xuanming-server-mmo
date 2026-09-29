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
#include "proto/contracts/kafka/match_event.pb.h"

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
// 出站(除客户端直连外全走 Kafka producer,设计文档 D6;寻址方式见
// docs/design/control-plane-topic-partitioning-20260908.md,由 ResolveCommandRoute 统一给出):
//   大厅公告 → 无活直连时 GateCommand{PushToPlayerEvent},topic=gate-cmd_g<N> 的
//             gate_node_id % P 号分区,key=player_id,
//             target_gate_id=gate_node_id + target_instance_id=BattleRouting.gate_instance_id;
//             只有 NotifyBattleAssigned / NotifyBattleStart 走这里(turn-based §22 D68),
//             战斗帧不回落。battle 不再发任何 gate 绑定事件(§22 D67:gate 不中继战斗);
//   结算     → SceneCommand{DispatchEvent, BattleSettlementEvent},
//             topic=scene-cmd_g<N> 的 scene_node_id % P 号分区,key=player_id,
//             target_scene_id=scene_node_id + target_instance_id=BattleRouting.scene_instance_id;
//   确认     → SceneCommand{DispatchEvent, BattleConfirmedEvent}(CreateBattle 成功后每玩家一条,
//             scene 据此 PREPARING→FIGHTING 并把作废期限切到正式 deadline,同上信封);
//   对局结果 → contracts.kafka.BattleResultEvent,topic=match-results(全局无 zone 段),
//             key=battle_id;只在真实打完(FinishBattle)时发,Destroy/Abort 作废不发。
//             带活动上下文(帮会同道历练)的局先落 zone Redis battle:activity_result:{battle_id}
//             再发,guild 销账前每 10s 重发(docs/design/guild-phase2/06-activities.md §6.19–§6.21)。
//
// 客户端直连(设计文档 §18,D23-D28;收缩见 turn-based §22):直连是战斗唯一通路 ——
// 上行只从直连面(BattleClientEdge)进来,下行分两个显式出口:战斗帧 PushBattleFrame
// 只走直连、无活直连即丢弃;大厅公告 PushLobbyAnnouncement 有活直连直发、否则回落上面的
// Kafka→gate 路径(判定在 battle_push_policy.h)。房间为每个参战者 / 观众最多保留一条
// 已验证的直连(directConnByPlayer)。票据由本节点签发(BuildAssignment),开局 / 观战接入
// 在任何副作用之前预签,签不出即拒绝(§22 D70 fail-closed);观众首帧随直连握手下发(§22 D69)。
//
// 配表指纹(cross-zone-matchmaking.md §10):CreateBattle 时把 request/players[i] 携带的
// 指纹与本节点七张战斗表指纹比对,不一致按 game_config.yaml battle_table_fingerprint_mode
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
    // (合成 SessionDetails 用,仅日志对齐),返回 false = 拒绝握手。
    bool AttachDirectConnection(uint64_t battleId, uint64_t playerId, ::eBattleTicketRole role,
                                const muduo::net::TcpConnectionPtr &conn, uint32_t *gateSessionId);

    // 直连握手成功应答写出之后调用(snapshot on connect,turn-based §22 D69):
    // 观众立刻经直连收一份当前 SpectateStateS2C 作为首帧 —— 观战首帧只随直连下发,
    // AddObserver 时直连还不存在。参战者不在这里推快照,由客户端在直连就绪时
    // GetBattleState 补拉。必须晚于握手应答:客户端的握手读的第一个包必须是应答。
    // 房间已不存在 / 观众已被移出名单时 no-op。
    void OnDirectConnectionVerified(uint64_t battleId, uint64_t playerId, ::eBattleTicketRole role);

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
    // fail-closed(turn-based §22 D70):插表、装定时器、发任何事件之前先为全体参战者预签票据,
    // 任一人签不出即回 kServiceUnavailable —— 此时保证没有建房、没有推送、没有确认事件,
    // match 走通用补偿(DestroyBattle 幂等 → CancelBattlePrepare)。
    void HandleCreateBattle(const ::CreateBattleRequest &request, ::CreateBattleResponse &response);

    // 幂等:房间不存在视为已销毁。销毁不发结算(补偿/回滚路径),推观众终局并关闭直连。
    void HandleDestroyBattle(const ::DestroyBattleRequest &request);

    // 观战接入(设计文档 §10.5)。错误 tip 约定:房间不存在=kEntityIsNull
    // (match 据此懒剔除 Redis 索引并换场重试)、观众满=kRateLimitExceeded、
    // 观众是参战者=kInvalidParameter、签不出票据=kServiceUnavailable(turn-based §22 D70:
    // 新观众不登记;幂等重推路径把已登记的观众摘除并关其直连 —— match 收到任何错误都会
    // DEL 观战标记,房间里不能留着这名观众)。幂等:同 observer 重复 Add 只重推落点分配
    // (有活直连时连同观战首帧)。
    void HandleAddObserver(const ::AddObserverRequest &request, ::AddObserverResponse &response);

    // 幂等移除(match 互斥清退路径):经直连向该观众推 SpectateEnd(REMOVED)并关直连。
    void HandleRemoveObserver(const ::RemoveObserverRequest &request);

    // ---- 客户端直连消息(BattleClientEdge 派发;权威身份来自已验证的票据,不信请求体) ----
    //
    // 入参保持 SessionDetails(turn-based D28:直连面合成、Handle* 零改动),只用 player_id;
    // session_id 是登记时的大厅会话号,仅供日志对齐。BattleRouting 只在开局快照 / AddObserver
    // 时写入,不再按会话刷新(§22 D71)。

    void HandleSubmitBattleAction(const ::SessionDetails &sessionDetails,
                                  const ::SubmitBattleActionRequest &request,
                                  ::SubmitBattleActionResponse &response);

    void HandleGetBattleState(const ::SessionDetails &sessionDetails,
                              const ::GetBattleStateRequest &request,
                              ::BattleStateS2C &response);

    // 观众主动退出:移出名单并关直连,不推 SpectateEnd(是客户端自己发起的,再推是回声);
    // 观众不存在也回成功(幂等,重复点退出/晚到的退出包都无害)。
    void HandleStopWatchBattle(const ::SessionDetails &sessionDetails,
                               const ::StopWatchBattleRequest &request,
                               ::StopWatchBattleResponse &response);

    // 自动战斗开关(设计文档 §11.2):落引擎 is_auto;开启后若全员就绪,
    // 走与 SubmitBattleAction 相同的提前结算路径。
    void HandleSetAutoBattle(const ::SessionDetails &sessionDetails,
                             const ::SetAutoBattleRequest &request,
                             ::SetAutoBattleResponse &response);

    // 停机收尾:全部房间作废(不结算;观众收 ABORTED,关闭房间直连),供 SetBeforeShutdown 调用。
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
        // CreateBattleRequest.activity_context 原样副本(缺省 = kind NONE = 普通对局)。
        // battle 不解释业务字段,只在 FinishBattle 回显进 BattleResultEvent 并据 kind 选结果通道。
        ::BattleActivityContext activityContext;
        turnbattle::TurnBattleEngine engine;
        // player_id → 路由信息副本(快照携带,battle 不查 etcd 定位对端)。
        // 有序容器:广播与结算的遍历顺序稳定,日志/回放可复现。
        std::map<uint64_t, ::BattleRouting> routingByPlayer;
        // 观众路由(设计文档 §10.5)。routing 的 scene 字段恒为 0:观众零写权、
        // 无结算,绝不向观众的 scene 发任何事件(不变量 6);gate 字段只给无直连时的
        // 大厅公告(落点分配包)回落用。
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
        // player_id(参战者或观众)→ 已验证的客户端直连;缺项 = 该玩家没有活直连:
        // 战斗帧丢弃,大厅公告回落 Kafka→gate(battle_push_policy.h)。
        // 按 player_id 只有一个槽位,参战与观战共用 —— 这就是参战者不能观战自己这局的原因。
        // 有序容器:与 routingByPlayer 同口径,收尾遍历顺序稳定。
        // weak_ptr(AGENTS.md §11.7:应用层不拥有连接):连接由 edge 的 TcpServer 拥有,这里只是
        // "谁有直连"的索引;对端断开后 lock() 失败 == 缺项,不会钉住死连接和 fd。
        std::map<uint64_t, std::weak_ptr<muduo::net::TcpConnection>> directConnByPlayer;
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

    // 战斗收尾:每参与者 BattleEndS2C(战斗帧,只走直连)→ BattleSettlementEvent(先落库后投递);
    // 观众按 spectateReason 推 SpectateEndS2C(§10.5);终局包排队后关闭房间全部直连;
    // 最后向 match-results 发一条 BattleResultEvent(评分回流;带活动上下文的局改走
    // DispatchActivityResultDurably 的持久化通道)。
    // 只组装与发送,不动 rooms_(房间由调用方随后移除)。
    void FinishBattle(BattleRoom &room, ::eBattleOutcome outcome,
                      ::eSpectateEndReason spectateReason);

    void BroadcastTurnResult(const BattleRoom &room, const ::TurnResultS2C &result);

    // 按收信人裁剪战斗状态(G7)。引擎出的 BuildStateSnapshot 是"全知"版本:
    // 含全员 skill_cooldown_rounds,直接广播等于把对手每个技能还差几回合明着告诉玩家。
    // viewerPlayerId = 0 表示观众视角(全员冷却都剔除)。
    // 只动出站拷贝,不碰引擎状态,确定性与回放不受影响(宪法 §7 不变量 5)。
    // 注意:buff 保留不裁 —— 客户端名牌要画 buff 图标,且"谁被控了几回合"本就写在
    // 回合事件流里,裁了只是让自己人也看不见。
    void RedactStateForViewer(::BattleStateS2C &state, uint64_t viewerPlayerId) const;

    // 填入收信人本人的剩余战斗道具(观众与他人收到的恒为空)。
    void FillSelfItems(const BattleRoom &room, ::BattleStateS2C &state,
                       uint64_t viewerPlayerId) const;

    // 观战快照:全量快照 + 房间 timer 回填的行动截止 + 当前观众数。战斗帧(只走直连):
    // 首帧由直连握手触发(OnDirectConnectionVerified),AddObserver 同会话幂等重推时
    // 有活直连才会真正送达。
    void PushSpectateState(const BattleRoom &room, uint64_t observerId) const;

    // 收尾/作废统一出口:经直连向全部观众推 SpectateEndS2C 并关其直连,
    // 然后清空观众表(此后任何广播都不会再打到观众)。
    void NotifySpectateEndAndClose(BattleRoom &room, ::eSpectateEndReason reason,
                                   ::eBattleOutcome outcome);

    // ---- 客户端直连(设计文档 §18;下行出口见 turn-based §22 D68) ----

    // 该玩家在房间里已验证、仍 connected 的直连;没有 / 已断 / 未装配直连面时返回空。
    muduo::net::TcpConnectionPtr LiveDirectConnOf(const BattleRoom &room, uint64_t playerId) const;

    // 战斗帧出口:只走直连。无活直连即丢弃,打按 messageId 采样的
    // LOG_INFO metric=battle_frame_dropped_no_direct(battle 无 Prometheus,采样防日志放大);
    // 客户端直连就绪后 GetBattleState 补拉。用于 TurnResult / BattleEnd / SpectateState /
    // SpectateTurnResult / SpectateEnd 及直连建立后的一切战斗推送。
    // const:PushSpectateState 是 const 方法。
    void PushBattleFrame(const BattleRoom &room, uint64_t playerId, uint32_t messageId,
                         const ::google::protobuf::Message &message) const;

    // 大厅公告出口:有活直连直发,否则回落 Kafka→gate(routing 只在回落时用)。
    // **只**用于 NotifyBattleAssigned / NotifyBattleStart —— 它们发生在直连建立之前,
    // 客户端拿到落点分配才能建直连。新增调用点前先确认它确实属于这一类。
    void PushLobbyAnnouncement(const BattleRoom &room, uint64_t playerId, const ::BattleRouting &routing,
                               uint32_t messageId, const ::google::protobuf::Message &message) const;

    // 签票据并组装落点分配包。false = 签不出(空密钥 + prod / OpenSSL 失败 / 本节点
    // endpoint 未就绪),调用方必须拒绝开局 / 拒绝观战(turn-based §22 D70 fail-closed),
    // 不能放玩家进一场连不上的战斗。expire_at_ms = room.deadlineMs(调用前必须已落到房间上)。
    bool BuildAssignment(const BattleRoom &room, uint64_t playerId, ::eBattleTicketRole role,
                         ::BattleAssignedS2C &out) const;

    // 下发调用方预签好的落点分配包(NotifyBattleAssigned,大厅公告出口)。
    // 签票与失败处置在调用方(CreateBattle / AddObserver),这里不再签。
    void PushAssignment(const BattleRoom &room, uint64_t playerId, const ::BattleRouting &routing,
                        const ::BattleAssignedS2C &assigned) const;

    // 关闭并摘除一个玩家的直连(观众退出 / 被清退 / 换会话 / 签票失败)。立刻从表里摘除,但 shutdown() +
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

    // ---- 活动结果发件箱(帮会同道历练,docs/design/guild-phase2/06-activities.md §6.19–§6.21)----
    //
    // 与上面的 R07 结算发件箱同一纪律(先落库、后投递、未销账则重投),区别只在销账方:
    // 这里由 guild 在该局进入终态后 DEL battle:activity_result:{battle_id}。
    // 判定与常量在 services/battle/system/battle_result_activity.h(纯函数,单测直接盯)。
    struct PendingActivityResult
    {
        // 序列化好的 BattleResultEvent:与 Redis 里的记录逐字节相同,重发原样再发、不重算。
        std::string payload;
        // 已重发的次数(首发不计)。
        uint32_t attempts = 0;
    };

    // 活动局结果的唯一出口。顺序是硬要求:SET 记录 → (成功回调里)发 Kafka → 登记待销账。
    // 反过来先发后写,guild 可能先结算并 DEL(此刻记录还不存在,DEL 是空操作),迟到的 SET
    // 落地后就留下一条孤儿记录,battle 会对一局已结算的对局一直重发到次数用尽。
    // Redis 不可用 / SET 失败:降级为只发一次并报 ERROR(§6.21 R6,与改造前行为一致)。
    void DispatchActivityResultDurably(const contracts::kafka::BattleResultEvent &result);
    // SET 成功后登记一条待销账记录,并保证重发定时器已装填。
    void EnqueuePendingActivityResult(uint64_t battleId, std::string payload);
    // 定时器回调:对每条未销账的记录探测一次。
    void RetryPendingActivityResults();
    // 单条记录的一轮处理:EXISTS → 按判定重发 / 摘除。
    void ProbeActivityResultOne(uint64_t battleId);
    // outbox 空了就停表(常态下没有进行中的活动局结果)。
    void StopActivityResultRetryTimerIfIdle();

    // 绑进定时器 / hiredis 回调的 weak_ptr<自己>(AGENTS.md §11.7)。
    // 本类是进程级单例(Instance() 里的函数静态对象),不由 shared_ptr 持有,拿不到 weak_from_this();
    // 这里用别名构造把 this 挂在 lifeToken_ 的控制块上:lifeToken_ 随本对象析构,之后 lock() 必然失败。
    // 实际上单例寿命覆盖整个 loop(tlsRedis 是 thread_local,先于静态对象析构),这层是按 §11.7
    // "只绑 weak_ptr<自己>"写的防线,不是在修某个已知的悬垂路径。R07 结算发件箱沿用裸 this,本批不动。
    std::weak_ptr<BattleRoomManager> WeakSelf() { return std::shared_ptr<BattleRoomManager>(lifeToken_, this); }

    BattleClientEdge *edge_ = nullptr;

    std::unordered_map<uint64_t, std::unique_ptr<BattleRoom>> rooms_;

    // key = (battle_id, player_id);量级 = 未销账的结算条数,常态为 0。
    std::map<std::pair<uint64_t, uint64_t>, PendingSettlement> settlementOutbox_;
    TimerTaskComp settlementRetryTimer_;

    // key = battle_id;量级 = 未被 guild 销账的活动局结果数,常态为 0。
    // 有序容器:与 settlementOutbox_ 同口径,重发遍历与日志顺序稳定。
    std::map<uint64_t, PendingActivityResult> activityResultOutbox_;
    TimerTaskComp activityResultRetryTimer_;

    // WeakSelf() 的存活令牌。声明在最后 = 最先析构:析构期间任何迟到回调都已 lock 不到本对象。
    std::shared_ptr<char> lifeToken_ = std::make_shared<char>();
};
