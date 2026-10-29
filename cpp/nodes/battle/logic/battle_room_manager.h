#pragma once

#include <cstdint>
#include <functional>
#include <map>
#include <memory>
#include <optional>
#include <set>
#include <string>
#include <string_view>
#include <utility>
#include <vector>
#include <tuple>
#include <unordered_map>

#include "muduo/net/TcpConnection.h"
#include "time/comp/timer_task_comp.h"

#include "battle_room_table.h"

// 回合引擎(cpp/libs/services/battle,纯逻辑库,API 见设计文档 §5.1)
#include "system/turn_battle_engine.h"
#include "system/result_commit.h"
// 结算发件箱的判定与 Redis 契约(PendingIndexSweepCursor 作为成员类型)
#include "settlement/settlement_outbox.h"

#include "proto/battle/battle_data.pb.h"
#include "proto/battle/battle_node.pb.h"
#include "proto/battle/player_battle.pb.h"
#include "proto/common/base/session.pb.h"
#include "proto/contracts/kafka/match_event.pb.h"

// 回合制战斗房间管理器(设计文档 §5.2)。
//
// 线程模型:所有方法只允许在 muduo loop 线程执行(gRPC handler 由 muduo_grpc 服务端
// 在 loop 线程上派发,timer 回调本来就在 loop 线程),
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
    // 房间及重试回调捕获管理器 this,单例的地址必须固定。
    BattleRoomManager(const BattleRoomManager&) = delete;
    BattleRoomManager& operator=(const BattleRoomManager&) = delete;
    BattleRoomManager(BattleRoomManager&&) = delete;
    BattleRoomManager& operator=(BattleRoomManager&&) = delete;

    static BattleRoomManager &Instance();

    // 表加载完成后在本节点 EventLoop 安装一次，房间共享只读配置源。
    void SetDataProvider(std::shared_ptr<turnbattle::BattleDataProvider> provider) {
        dataProvider_ = std::move(provider);
    }

    // ---- 房间生命周期回调(集群外入口 D82 / D85;main.cpp 装配,转发给 Agones GameServerLifecycle) ----
    //
    // 房间 = Agones 的"单元":第一个房间建出之前要 allocate、最后一个房间移除之后回 Ready。
    // 契约:
    //   onCreated(battle_id):房间**已插表之后**调用(单元确实创建成功);
    //   onRemoved(battle_id):房间**已从表里移除之后**调用(单元确实销毁)。
    //   每个 battle_id 在一次插表 / 移除上各恰好调用一次 —— 由房间表类型保证(battle_room_table.h:
    //   底层容器私有,只能经 Emplace / Erase 增删,两者各自触发回调;单测 tests/battle_room_table_test.cpp)。
    //   本类所有插入 / 删除路径(开局、正常结束、整场期限回收、DestroyBattle、AbortAllRooms)
    //   经私有的 EmplaceRoom / EraseRoom 调它。
    // 线程模型:只在 loop 线程调用;回调必须快速返回(GameServerLifecycle 只拿一把短锁,不做 I/O)。
    // 未设置的回调 = 不通知(本地 / 测试)。
    using RoomLifecycleHooks = battle_room_table::Hooks;

    // 只在 Node 构造完成、EventLoop 开始 loop() 之前调用一次(main 的 configure 回调)。
    void SetRoomLifecycleHooks(RoomLifecycleHooks hooks) { rooms_.SetHooks(std::move(hooks)); }

    // ---- 客户端直连面(main.cpp 装配;BattleClientEdge 生命周期由 main 的运行时上下文持有) ----

    void SetClientEdge(BattleClientEdge *edge)
    {
        if (edge == nullptr)
        {
            edge_.reset();
            return;
        }
        edge_ = std::ref(*edge);
    }

    // 票据握手通过后把直连挂到房间上。校验"房间仍存在 + 该玩家按 role 确在名单上";
    // 同一玩家已有直连(重连)则关闭旧连接、换成新的。成功时回填该玩家的 gate 会话号
    // (合成 SessionDetails 用,仅日志对齐),返回 false = 拒绝握手。
    bool AttachDirectConnection(uint64_t battleId, uint64_t playerId, ::eBattleTicketRole role,
                                const muduo::net::TcpConnectionPtr &conn, uint32_t *gateSessionId,
                                std::string_view spectatorOperation = {});
    bool OwnsDirectConnection(uint64_t battleId, uint64_t playerId, ::eBattleTicketRole role,
                             const muduo::net::TcpConnectionPtr &conn, std::string_view spectatorOperation);

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

    // 节点级准入(建房准入闸、Agones 分配许可)在调用方 BattleNodeImpl::CreateBattle,先于本函数;
    // 本函数只剩业务判定。
    // 幂等:同 battle_id 重复创建直接回 OK(match 补偿路径可能重试)。
    // fail-closed(turn-based §22 D70):插表、装定时器、发任何事件之前先为全体参战者预签票据,
    // 任一人签不出即回 kServiceUnavailable —— 此时保证没有建房、没有推送、没有确认事件,
    // match 走通用补偿(DestroyBattle 幂等 → CancelBattlePrepare)。
    void HandleCreateBattle(const ::CreateBattleRequest &request, ::CreateBattleResponse &response);

    // 幂等:房间不存在视为已销毁。销毁不发结算(补偿/回滚路径),推观众终局并关闭直连。
    void HandleDestroyBattle(const ::DestroyBattleRequest &request);
    void HandleQueryBattleCreation(const ::QueryBattleCreationRequest&, ::QueryBattleCreationResponse&);
    bool AcceptsPlacementOperation(uint64_t,const std::string&,const std::string&) const;

    // 观战接入(设计文档 §10.5)。错误 tip 约定:房间不存在=kEntityIsNull
    // (match 据此懒剔除 Redis 索引并换场重试)、观众满=kRateLimitExceeded、
    // 观众是参战者=kInvalidParameter、签不出票据=kServiceUnavailable(turn-based §22 D70:
    // 新观众不登记;幂等重推路径把已登记的观众摘除并关其直连 —— match 收到任何错误都会
    // DEL 观战标记,房间里不能留着这名观众)。幂等:同 observer 重复 Add 只重推落点分配
    // (有活直连时连同观战首帧)。
    void HandleAddObserver(const ::AddObserverRequest &request, ::AddObserverResponse &response);

    // 幂等移除(match 互斥清退路径):经直连向该观众推 SpectateEnd(REMOVED)并关直连。
    void HandleRemoveObserver(const ::RemoveObserverRequest &request, ::RemoveObserverResponse &response);
    void HandleQueryObserver(const ::QueryObserverRequest &request, ::QueryObserverResponse &response);

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
    // 例外是等结果回执的确认模式房间:它的个人结算在结果发出之前已写进 pending,清房不删 pending,
    // 结果无论是否最终到达 broker,都由 scene 登录钩子 / 冻结到期补读应用(审计 social-match-battle#2)。
    // 还在落库或落库轮次已用尽的房间同样清掉:已落定成员的 pending 留着照常应用,没落定的成员这一局按作废处理
    // (scene reaper 按 deadline 解冻),结果不发 —— 同局成员不对称,是审计 r2-battle#2 的既有残余。
    // 轮次用尽的房间不会自然消失,节点因此排不空;值班按 staging_exhausted 告警排除 Redis 故障后,删除该 Pod 走这里收尾。
    // 调用方必须在同一个 loop 任务里先关建房准入闸(BattleNodeImpl::CloseAdmission):gRPC 排空
    // 期间已收完的 CreateBattle 仍会派发(等许可的回调也可能排在关闸之后),不关闸它们会在作废之后照常建房。关闸之后房间表
    // 保持为空,AddObserver / IssueBattleTicket 因房间不存在自然被拒(kEntityIsNull / kInvalidParameter)。
    void AbortAllRooms(const std::string &reason);

    // ---- 结算重投的调参(测试与运维需要看得见,故放公开区)----
    //
    // 重投窗口 = kSettlementRetryIntervalSec × kSettlementRetryMaxAttempts = 120s。
    // 上界取自"要覆盖一次 scene 进程重启 + 服务发现补齐"的时间尺度;再长没有意义:
    // 持久记录仍在(不再过期,landing-recovery-sync-digest.md §7),玩家下次进场景会由登录钩子补应用,
    // 在线玩家战斗冻结到期时 scene reaper 也会补读一次。
    // 原来的 kPendingSettlementTtlSec(与 scene 侧同值的 7 天)随 TTL 一起删除。
    static constexpr double kSettlementRetryIntervalSec = 10.0;
    static constexpr uint32_t kSettlementRetryMaxAttempts = 12;
    // 探测「没问到」(命令发出去了,应答是空应答 / ERROR)的轮次上限:这类轮次不消耗 kSettlementRetryMaxAttempts
    // (一次断线不该烧掉重投窗口),但也不能无限挂着 —— 持续 ERROR(口令轮换后 NOAUTH、ACL 拒绝、长时间 LOADING)时,
    // 每局结算都会进 outbox 却永远到不了次数用尽,payload 随对局数无界增长(AGENTS §11.3 重试必须有上限)。
    // 按条目寿命**累计**计数(问到的轮次不清零;问到的轮次另有 kSettlementRetryMaxAttempts 上限,条目寿命两头都有界)。
    // 60 轮 × 10s ≈ 10 分钟,远大于一次主切换;Redis 未连接的轮次 RetryPendingSettlements 直接跳过,不计在内。
    // 10 分钟只是墙钟时间的上界:连接卡住、等存活探测摘连接时,同一条目可能叠着两条在途探测,摘连接时各拿一次空应答、计数 +2。
    // 到上限摘条目并打 metric=battle_settlement_undelivered reason=probe_undetermined;记录若已落库仍由登录钩子兜底。
    static constexpr uint32_t kSettlementProbeUndeterminedMaxRounds = 60;

    // ---- 待结算积压报数(landing-recovery-sync-digest.md §7)----
    //
    // 每 kSettlementBacklogReportIntervalSec 秒一轮:查积压索引里一批 kSettlementBacklogSweepBatch 个成员、
    // 清掉其中的孤儿(头部与轮转窗口交替,见 settlement_outbox.h 的 PendingIndexSweepCursor),
    // 再打一行 metric=battle_settlement_pending_backlog count= oldest_age_s= orphans_removed=。
    // battle / scene 进程没有 Prometheus 端点,这一行就是积压的唯一信号(值班 LogQL 登记在
    // deploy/k8s/asset-channel-alerts.yaml 文件头,设计 §6.5)。
    static constexpr double kSettlementBacklogReportIntervalSec = 60.0;
    static constexpr uint32_t kSettlementBacklogSweepBatch = 10;

private:
    BattleRoomManager() = default;

    struct BattleRoom
    {
        explicit BattleRoom(std::shared_ptr<turnbattle::BattleDataProvider> provider)
            : engine(std::move(provider)) {}
        uint64_t battleId = 0;
        // 开局参数副本(BattleResultEvent 回流给 match 用;引擎内的请求副本是私有的)
        uint32_t matchMode = 0;
        uint32_t battleConfigId = 0;
        // CreateBattleRequest.activity_context 原样副本(缺省 = kind NONE = 普通对局)。
        // battle 不解释业务字段,只在 FinishBattle 回显进 BattleResultEvent 并据 kind 选结果通道。
        ::BattleActivityContext activityContext;
        bool requiresResultConfirmation = false;
        std::optional<turnbattle::ResultCommit> resultCommit;
        ::contracts::kafka::BattleResultEvent pendingResult;
        // 确认模式下每名成员的 R07 个人结算字节(BuildPersonalSettlementPayloads),BeginResultCommit 时
        // 从冻结原件算一次:结果发送前落 pending、回执后派发都用这一份,两处字节必然相同。
        std::map<uint64_t, std::string> personalSettlementPayloads;
        std::optional<::TurnResultS2C> pendingFinalTurn;
        std::optional<::BattleStateS2C> pendingVisibleState;
        ::eSpectateEndReason pendingSpectateReason = ::SPECTATE_END_BATTLE_FINISHED;
        turnbattle::TurnBattleEngine engine;
        // player_id → 路由信息副本(快照携带,battle 不查 etcd 定位对端)。
        // 有序容器:广播与结算的遍历顺序稳定,日志/回放可复现。
        std::map<uint64_t, ::BattleRouting> routingByPlayer;
        // 观众路由(设计文档 §10.5)。routing 的 scene 字段恒为 0:观众零写权、
        // 无结算,绝不向观众的 scene 发任何事件(不变量 6);gate 字段只给无直连时的
        // 大厅公告(落点分配包)回落用。
        std::map<uint64_t, ::BattleRouting> routingByObserver;
        std::map<uint64_t, std::string> observerOperations;
        // 观众名字(AddObserverRequest.observer_name,仅日志/后续观众列表用)
        std::map<uint64_t, std::string> observerNames;
        TimerTaskComp roundTimer;      // 回合 action_deadline
        TimerTaskComp battleTimer;     // 整场 deadline_ms(强制收尾,防房间泄漏)
        uint64_t actionDeadlineMs = 0; // 当前回合行动截止(Unix 毫秒,GetBattleState 回填)
        uint64_t deadlineMs = 0;       // 整场作废期限(与 battleTimer 同值;补发确认事件时带给 scene)
        // BattleConfirmedEvent 补发:开局后 kConfirmResendWindowMs 内每 kConfirmResendIntervalSec
        // 向全部参战玩家重发一次(scene 幂等)。没有 scene→battle 的确认回执通道(proto 已定),
        // 用有界周期补发覆盖"首发 produce 失败 / Kafka 积压 / scene 消费者 rebalance"这类
        // 单次投递丢失或迟到;窗口按 scene 侧锁保留期取 180s:prepare TTL(match matchedTicketTTLFor,
        // 最长 10 人 96s)+ kLockExtraTtlSec 60s = 156s,再留余量(推导见 .cpp kConfirmResendWindowMs)。
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

    std::shared_ptr<turnbattle::BattleDataProvider> dataProvider_;
    BattleRoom *FindRoom(uint64_t battleId);

    // 房间插表(键 = room->battleId):成功后房间表触发一次 onCreated,返回表内的房间。
    // 所有权:room 按值接管。battle_id 已存在时返回 nullptr、不触发回调、打 LOG_ERROR,
    // 表里那间保持原样,room 随形参一起销毁 —— 调用方必须保证此前没有装定时器、推送或发事件
    // (HandleCreateBattle 在插表之前只做纯计算与预签)。调用方在同一 loop 任务里已判过幂等,
    // 走到这里说明是程序缺陷。
    BattleRoom *EmplaceRoom(std::unique_ptr<BattleRoom> room);

    // 房间移除:房间表移除并销毁房间后触发一次 onRemoved;房间不存在时 no-op(不触发)。
    // 调用之后该房间的引用 / 指针全部悬空。
    void EraseRoom(uint64_t battleId);

    // 确认事件周期补发(confirmResendTimer 回调):窗口已过则停表,否则对全员重发。
    void ResendBattleConfirmed(uint64_t battleId);

    // 装填下一回合行动收集窗口并记录截止时间。
    void ArmRoundTimer(BattleRoom &room);

    // 结算当前回合:全员就绪或回合超时触发。广播 TurnResult,
    // 胜负已分则走 FinishBattle。按 battle_id 重查房间,防 timer 迟到打到已销毁房间。
    void ResolveRound(uint64_t battleId);

    // 整场 deadline 到期:引擎仍未分出胜负按平局强制收尾(battle_node.proto 契约)。
    void OnBattleDeadline(uint64_t battleId);

    // 战斗收尾:确认模式先固定含全部个人结算的结果、把个人结算写进 pending,再取得Kafka all ACK，随后
    // 每参与者 BattleEndS2C(战斗帧,只走直连)→ 旧R07 BattleSettlementEvent(确认模式只投命令,不再改写 pending)。
    // 观众按 spectateReason 推 SpectateEndS2C(§10.5);终局包排队后关闭房间全部直连;
    // 旧模式最后发送评分事件；确认模式复用ACK前原件，不重新计算个人奖励。
    // 只组装与发送,不动 rooms_；返回true才能由调用方EraseRoom，校验失败保留房间。
    bool FinishBattle(BattleRoom &room, ::eBattleOutcome outcome,
                      ::eSpectateEndReason spectateReason,
                      const contracts::kafka::BattleResultEvent* committedResult = nullptr);
    contracts::kafka::BattleResultEvent BuildResult(const BattleRoom&, ::eBattleOutcome) const;
    void BeginResultCommit(BattleRoom&, ::eBattleOutcome, ::eSpectateEndReason,
                           std::optional<::TurnResultS2C>, ::BattleStateS2C);
    void PumpResultCommits();
    void StopResultCommits();
    // 结果第一次发往 broker 之前,把全部成员的个人结算写进 R07 pending(审计 social-match-battle#2)。
    // 由 PumpResultCommits 驱动,每轮只对还没落定的成员(ResultCommit::UnstagedMembers)各 EVAL 一次
    // kStagePendingSettlementOnceScript,回复到齐后经 OnPersonalSettlementsStaged 交给 ResultCommit::FinishStaging;
    // 没落完不发结果。已落定的成员不再重写:scene 可能已应用并销账,重写会让 pending 复活(审计 r2-battle#1)。
    // Redis 未连上时不开新一轮(不消耗退避与轮次),等下一次 pump。与本地模式不同,这里不降级为「不落库直接发」:
    // 不落库就发,恰好重现停机 / 崩溃丢个人结算的缺陷。
    void StagePersonalSettlements(BattleRoom &room, turnbattle::ResultCommit::Clock::time_point now);
    // 一轮落库的回复全部到齐。settledMembers = 本轮拿到确定结论的成员(StageVerdict 前三种)。
    // 房间已不在(停机清房)或轮次过期时丢弃;已写下的 pending 留给 scene 裁决。
    // 全部落定:删本局落库标记(fire-and-forget)。轮次用尽(turnbattle::kMaxStagingRounds,审计 r2-battle#4):
    // 打一条 metric=battle_personal_settlement_staging_exhausted,房间移出 resultRooms_ 不再 pump,
    // 但**保留在房间表里**(终局未知:结果不发、DestroyBattle 不作废、新行动照旧拒绝),直到停机 AbortAllRooms。
    void OnPersonalSettlementsStaged(uint64_t battleId, uint64_t attempt, std::set<uint64_t> settledMembers);

    void BroadcastTurnResult(const BattleRoom &room, const ::TurnResultS2C &result);

    // 按收信人裁剪战斗状态(G7)。引擎出的 BuildStateSnapshot 是"全知"版本:
    // 含全员 skill_cooldown_rounds,直接广播等于把对手每个技能还差几回合明着告诉玩家。
    // viewerPlayerId = 0 表示观众视角(全员冷却都剔除)。
    // 只动出站拷贝,不碰引擎状态,确定性与回放不受影响(宪法 §7 不变量 5)。
    // 注意:buff 保留不裁 —— 客户端名牌要画 buff 图标,且"谁被控了几回合"本就写在
    // 回合事件流里,裁了只是让自己人也看不见。
    // 同时不分视角清空全员 BattleActorState.combat(装备带来的必杀 / 连击 / 反震等概率,
    // equipment-attributes.md §4.5,经 TurnBattleEngine::StripEngineOnlyState):这是"不暴露
    // 对手装备概率"的唯一闸口,任何新增的出站 BattleStateS2C 都必须先经过本函数。
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

    // 签票据并组装落点分配包。false = 签不出(空密钥 + prod / OpenSSL 失败 / 本节点没有
    // 客户端可达地址),调用方必须拒绝开局 / 拒绝观战(turn-based §22 D70 fail-closed),
    // 不能放玩家进一场连不上的战斗。expire_at_ms = room.deadlineMs(调用前必须已落到房间上)。
    // host/port 取 client_endpoint::ClientFacing(集群外入口 D78):client_endpoint 可用就用它,
    // 否则 required=false 时回落 endpoint,required=true(CLIENT_ENDPOINT_REQUIRED=1)时拒签。
    // NodeInfo 由 Node 持有、只在 loop 线程写;跨线程读写不同步,本函数仍只能在 loop 线程调用。
    bool BuildAssignment(const BattleRoom &room, uint64_t playerId, ::eBattleTicketRole role,
                            ::BattleAssignedS2C &out, std::string_view spectatorOperation = {}) const;

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
        // 探测没问到的轮次,条目寿命内累计、不清零(不计入 attempts,上限 kSettlementProbeUndeterminedMaxRounds)。
        uint32_t undeterminedRounds = 0;
    };

    // 本地(非确认)模式结算的**唯一**出口(R07;确认模式见下方 DispatchStagedSettlement,顺序同样成立)。
    // 顺序是硬要求:
    //   Redis 落库 → (落库回调里)投 SceneCommand → 登记进 outbox 等销账。
    // 不能反过来先投再落库:投递可能比 Redis 的 SET 先到,scene 应用结算后销账
    // (此刻记录还不存在,销账是空操作),随后迟到的 SET 落地就留下一条孤儿记录,
    // 玩家下次登录会被**重复发一次奖励**。
    // Redis 不可用或命令未发出时降级为直接投递并报 ERROR；已发出但落库应答不确定时仍登记重投，交探测裁决。
    // 只用于本地(非确认)模式;确认模式的 pending 在发结果之前已由 StagePersonalSettlements 写好,
    // 回执后走 DispatchStagedSettlement。
    void DispatchSettlementDurably(const ::BattleRouting &routing, uint64_t playerId,
                                   const ::BattleSettlementData &settlement);

    // 确认模式回执之后的派发:pending 已在发结果之前落库,这里只投 SceneCommand 并登记 outbox 等销账。
    // **绝不**再写 pending:回执之前 scene 可能已经按登录钩子 / 冻结到期补读应用并销账,
    // 再写一次会让已销账的 pending 复活,玩家下次登录重复发奖(BuildPersonalSettlementPayloads 的说明)。
    void DispatchStagedSettlement(const ::BattleRouting &routing, uint64_t playerId, uint64_t battleId,
                                  const std::string &payload);

    // 落库回调里登记待销账记录并保证重投定时器已装填；落库应答不确定时也登记。
    void EnqueuePendingSettlement(uint64_t battleId, uint64_t playerId, std::string payload,
                                  uint32_t originalSceneNodeId);
    // 定时器回调:对每条未销账的记录探测 ACK、重新解析目标、按判定重投或收尾。
    void RetryPendingSettlements();
    // 单条记录的一轮处理(Redis 回调链的入口)。
    void ProbeAndRetryOne(uint64_t battleId, uint64_t playerId);
    // 一轮探测没问到(两跳任一跳的应答是空应答 / ERROR / 意外类型):不动 attempts,undeterminedRounds 加一;
    // 到 kSettlementProbeUndeterminedMaxRounds 摘条目并响一声。stage 只进日志("ack" / "location")。
    // 只改 outbox,不发 Redis 命令(会在 hiredis 释放连接的回调栈里被调到)。
    void NoteUndeterminedSettlementProbe(uint64_t battleId, uint64_t playerId, const char *stage,
                                         battle_settlement::ReplyShape shape);
    // outbox 空了就停表:battle 常态下没有未销账记录,不该留一个每 10s 空转的定时器。
    void StopSettlementRetryTimerIfIdle();
    // 保证积压报数定时器已装填(幂等)。由 DispatchSettlementDurably 在本节点第一次结算时装填,
    // 之后常驻:积压是全局的(记录不过期),不随本节点的 outbox 清空而停。
    // 本节点从没结算过就不报 —— 多个 battle 节点报的是同一个全局数,有一个在打的节点报就够了。
    void EnsureSettlementBacklogReporter();
    // 定时器回调:按 settlementBacklogCursor_ 选一批索引成员清孤儿,再打一行积压 metric。
    // 回调链只以 weak_ptr<自己> 回到本对象推进游标,其余只捕获值(AGENTS.md §11.7)。
    void ReportPendingSettlementBacklog();

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
    // Redis 不可用 / SET 未发出时只发一次并报 ERROR；已发出但应答不确定时仍登记重投，由 EXISTS 探测裁决。
    void DispatchActivityResultDurably(const contracts::kafka::BattleResultEvent &result, bool brokerCommitted = false);
    // SET 回调里登记待销账记录并保证重发定时器已装填；落库应答不确定时也登记。
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
    // "只绑 weak_ptr<自己>"写的防线,不是在修某个已知的悬垂路径。
    // 2026-10-16 核对过停机顺序:tlsRedis.Shutdown() 在 Node::FinalizeTeardownInLoop 里执行(入口兜底在
    // RunNodeMain 返回前),都早于本函数静态对象析构,所以停机时以空应答进来的在途回调看到的本对象仍然活着。
    // R07 结算发件箱的三个 hiredis 回调同日改绑 weak(与活动结果发件箱同一写法);重投定时器 settlementRetryTimer_
    // 仍绑裸 this:TimerTaskComp 是本类成员,析构时取消定时器,属于 §11.7 的同寿命成员例外。
    std::weak_ptr<BattleRoomManager> WeakSelf() { return std::shared_ptr<BattleRoomManager>(lifeToken_, this); }

    // 仅借用运行时上下文持有的直连面；清空借用不销毁对象。
    std::optional<std::reference_wrapper<BattleClientEdge>> edge_;

    // 房间表:增删与生命周期回调(SetRoomLifecycleHooks)绑死在 RoomTable 里,本类只经
    // EmplaceRoom / EraseRoom 调它的 Emplace / Erase,其余路径只读(FindRoom / AbortAllRooms 抄 id)。
    battle_room_table::RoomTable<BattleRoom> rooms_;
    // 本实例操作墓碑只在loop线程访问；进程重启后精确target_instance使旧RPC失效。
    struct CreationOperation {std::string operationId;std::string canonicalRequest;uint64_t expiresMs=0;bool aborted=false;};
    std::unordered_map<uint64_t,CreationOperation> creationOperations_;
    // 取消先于Add时房间可能尚不存在；墓碑在manager而非room，保留到原准入截止。
    struct SpectatorOperation { uint64_t admitBeforeMs{0}; std::string canonicalRequest; bool cancelled{true}; };
    using SpectatorKey = std::tuple<uint64_t, uint64_t, std::string>;
    std::map<SpectatorKey, SpectatorOperation> spectatorOperations_;
    std::multimap<uint64_t, SpectatorKey> spectatorDeadlines_;
    void PruneSpectatorOperations(uint64_t now);
    // producer listener 只登记回执，pump 在 poll 返回后才执行终局副作用，禁止回执栈递归 send。
    bool resultListenerInstalled_ = false;
    uint64_t nextResultCorrelation_ = 0;
    std::map<uint64_t, uint64_t> resultAttempts_;
    std::set<uint64_t> resultRooms_;
    std::vector<std::pair<uint64_t, bool>> resultReports_;
    TimerTaskComp resultCommitTimer_;

    // key = (battle_id, player_id);量级 = 未销账的结算条数,常态为 0。
    std::map<std::pair<uint64_t, uint64_t>, PendingSettlement> settlementOutbox_;
    TimerTaskComp settlementRetryTimer_;
    // 积压报数(EnsureSettlementBacklogReporter);装填后常驻。
    TimerTaskComp settlementBacklogTimer_;
    // 孤儿清理的扫描窗口(头部 / 轮转交替,见 settlement_outbox.h 的 PendingIndexSweepCursor)。
    // 纯进程内状态:节点重启从头部开始,不影响正确性。
    battle_settlement::PendingIndexSweepCursor settlementBacklogCursor_;

    // key = battle_id;量级 = 未被 guild 销账的活动局结果数,常态为 0。
    // 有序容器:与 settlementOutbox_ 同口径,重发遍历与日志顺序稳定。
    std::map<uint64_t, PendingActivityResult> activityResultOutbox_;
    TimerTaskComp activityResultRetryTimer_;

    // WeakSelf() 的存活令牌。声明在最后 = 最先析构:析构期间任何迟到回调都已 lock 不到本对象。
    std::shared_ptr<char> lifeToken_ = std::make_shared<char>();
};
