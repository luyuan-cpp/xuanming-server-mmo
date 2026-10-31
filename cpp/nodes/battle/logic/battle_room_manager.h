#pragma once

#include <algorithm>
#include <cstdint>
#include <chrono>
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
#include "battle_spectate_relay.h"

// 回合引擎(cpp/libs/services/battle,纯逻辑库,API 见设计文档 §5.1)
#include "system/turn_battle_engine.h"
#include "system/result_commit.h"

#include "proto/battle/battle_data.pb.h"
#include "proto/battle/battle_node.pb.h"
#include "proto/battle/player_battle.pb.h"
#include "proto/common/base/message.pb.h"
#include "proto/common/base/session.pb.h"
#include "proto/contracts/kafka/gate_event.pb.h"
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
// 终局(只有终局协议 v1,player-data-tidb-scaling-plan.md §5.4):打完之后把冻结的完整原件(含全员个人结算)经 gRPC
// CommitBattleOutcome 提交 match SQL,只有拿到持久决定才公布终局、删房。个人结算由 match 发件箱投 player_inbox,
// scene 应用后随存档提交;对局结果(评分、活动局)也由 match 发件箱投 Kafka。battle 不写 Redis,不直接发结果。
//
// 其余出站(除客户端直连外全走 Kafka producer,设计文档 D6;寻址方式见
// docs/design/control-plane-topic-partitioning-20260908.md,由 ResolveCommandRoute 统一给出):
//   大厅公告 → 无活直连时 GateCommand{PushToPlayerEvent},topic=gate-cmd_g<N> 的
//             gate_node_id % P 号分区,key=player_id,
//             target_gate_id=gate_node_id + target_instance_id=BattleRouting.gate_instance_id;
//             GateCommand.player_id 与 PushToPlayerEvent.player_id 都填目标玩家(gate 投递前核对
//             会话当前玩家,session_id 回绕 / gate node_id 复用时不会写进别人的连接),0 在本侧拒发;
//             只有 NotifyBattleAssigned / NotifyBattleStart 走这里(turn-based §22 D68),
//             战斗帧不回落。battle 不再发任何 gate 绑定事件(§22 D67:gate 不中继战斗);
//   确认     → SceneCommand{DispatchEvent, BattleConfirmedEvent}(CreateBattle 成功后每玩家一条,
//             scene 据此 PREPARING→FIGHTING 并把作废期限切到正式 deadline),
//             topic=scene-cmd_g<N> 的 scene_node_id % P 号分区,key=player_id,
//             target_scene_id=scene_node_id + target_instance_id=BattleRouting.scene_instance_id;
//   观战帧   → 直连席之外的观众(battle-spectate-tiered-fanout.md §15.4):SpectateFrameEvent,不套信封。
//             L1 给房间挂接的每台 gate 各一条,L2 只往全服广播 topic 发一条;topic / 分区 / key 与失败语义见
//             battle_spectate_relay.h。这是 §22 D68 的显式例外(D94),参战帧仍然只走直连。
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
    // 单测直接驱动「Match 回了持久决定」并清房,不启动 match(房间按生产合同只由 Match 的决定收尾,DestroyBattle 删不掉)。
    friend class BattleRoomManagerTestAccess;

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

    // 借用直连面;nullptr = 收回借用(不销毁对象,也不动观众席配额)。
    // 借用非空直连面时,按它的有效连接上限算出节点级观众席配额并注入(SetObserverSeatQuota,口径见公开区
    // 「节点级观众席配额」):main.cpp 在启动门禁之后、gRPC 开服之前调用一次,部署配置此刻已加载;
    // 之后准入只比对注入值,请求路径不读配置。
    void SetClientEdge(BattleClientEdge *edge);

    // 票据握手通过后把直连挂到房间上。校验"房间仍存在 + 该玩家按 role 确在名单上";
    // 同一玩家已有直连(重连)则关闭旧连接、换成新的。成功时回填该玩家的 gate 会话号
    // (合成 SessionDetails 用,仅日志对齐),返回 false = 拒绝握手。
    // 观众挂上直连即停止其席位计时(连接宽限 / 重连宽限)。
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
    // 房间已不存在时 no-op。真摘掉了观众的直连且其仍在名单上时,开始 kObserverReconnectGrace 计时,
    // 期内未重连即回收席位。
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
    // 只接受终局协议 v1:outcome_protocol_version 不是 turnbattle::kOutcomeProtocolVersion,或缺放置操作 / 目标实例,
    // 一律回 kInvalidParameter;本节点没有终局签名密钥时回 kFeatureUnavailable。
    // 幂等:同 battle_id 重复创建直接回 OK(match 补偿路径可能重试)。
    // fail-closed(turn-based §22 D70):插表、装定时器、发任何事件之前先为全体参战者预签票据,
    // 任一人签不出即回 kServiceUnavailable —— 此时保证没有建房、没有推送、没有确认事件,
    // match 走通用补偿(DestroyBattle 幂等 → CancelBattlePrepare)。
    void HandleCreateBattle(const ::CreateBattleRequest &request, ::CreateBattleResponse &response);

    // 房间还在就什么都不做:房间只由 Match 的持久决定收尾(COMPLETE / ABORT),过期的补偿不能把一局在打的房间作废。
    // 房间不存在时幂等成功;带放置操作的请求同时立一条操作墓碑,挡住同 battle_id 迟到的 CreateBattle。
    void HandleDestroyBattle(const ::DestroyBattleRequest &request);
    void HandleQueryBattleCreation(const ::QueryBattleCreationRequest&, ::QueryBattleCreationResponse&);
    bool AcceptsPlacementOperation(uint64_t,const std::string&,const std::string&) const;

    // 观战接入(设计文档 §10.5)。错误 tip 约定:房间不存在=kEntityIsNull
    // (match 据此懒剔除 Redis 索引并换场重试)、房间不可观战=kInvalidParameter(可见性第二道闸,
    // battle-spectate-tiered-fanout.md §10 第 6 条:建房时冻结的 spectate_visibility 不是 PUBLIC 即拒,
    // 未填与 PRIVATE 同样拒绝;房间其实还在,不能回 kEntityIsNull)、观众席满=kMatchSpectateFull
    // (match_error 段,battle→match 与 match→客户端共用;单房间席位 kMaxObserversPerRoom 与节点级观众席配额
    // 任一满都回它,语义都是「直连席已满」;席位按名单人数计,含待连接与宽限中的观众)、
    // 观战操作墓碑表满=kRateLimitExceeded(节点级过载,可重试,不是满员)、
    // 观众是参战者=kInvalidParameter、签不出票据=kServiceUnavailable(turn-based §22 D70:
    // 新观众不登记;幂等重推路径把已登记的观众摘除并关其直连 —— match 收到任何错误都会
    // DEL 观战标记,房间里不能留着这名观众)。幂等:同 observer 同 op 重复 Add 只重推落点分配
    // (有活直连时连同观战首帧),且先于可见性闸与满员 / 配额判定(可见性建房后不变,私密局的名单恒为空,
    // 这个顺序放不进私密局的观众)。拒绝即终态只对立起墓碑之后的拒绝成立(房间不存在、不可观战、
    // 满员、签票失败等):同 op 的墓碑保持封闭,迟到的同 op Add 不会翻成准入,回首次拒绝码。两类拒绝不立墓碑、
    // 同一 op 之后仍可能被重新判定:入参校验失败(目标实例 / op 形状 / 准入窗口等)与墓碑表满;
    // 要封住它们只能靠 match 收到错误后发的 RemoveObserver 补立封闭墓碑。
    // 新登记的观众从此刻起计 kObserverConnectGrace,到期未建直连即被回收(ReclaimExpiredObserverSeats)。
    void HandleAddObserver(const ::AddObserverRequest &request, ::AddObserverResponse &response);

    // 幂等移除(match 互斥清退路径):经直连向该观众推 SpectateEnd(REMOVED)并关直连。
    void HandleRemoveObserver(const ::RemoveObserverRequest &request, ::RemoveObserverResponse &response);
    void HandleQueryObserver(const ::QueryObserverRequest &request, ::QueryObserverResponse &response);

    // ---- 观战中继(L1)/ 全服广播(L2)(battle-spectate-tiered-fanout.md §15.4) ----
    //
    // 档位由 match 记账,battle 只认两件事:本场挂了哪些中继 gate、是否已升广播。两者都是房间的内存状态,
    // 随房间消失;节点重启后 target_instance_id 对不上,旧挂接不会被继承。
    // 下面两个接口的前置校验相同,顺序是契约,任何一项不过都没有副作用,回包只带 error_message:
    //   1. target_instance_id 不是本实例 = kInvalidParameter;
    //   2. 房间不存在 = kEntityIsNull(match 据此懒剔除观战索引,别换 tip);
    //   3. 房间不公开 = kInvalidParameter(可见性闸,与 AddObserver 同口径:未填与 PRIVATE 一样拒)。
    // 通过后回包带本实例 UUID 与房间作废期限,match 写进观战凭证:gate 据前者丢僵尸帧,据后者在等不到终局帧时兜底收尾。
    // 补发的 STATE 帧是观众版当前快照(与 L0 首帧同源;observer_count = 0,gate 不知道全服人数),等结果回执期间是冻结的
    // 可见状态,不提前泄露终局。帧没发出去不影响回包(只计数,见 battle_spectate_relay.h),下一回合的帧自愈。

    // 把一台 gate 挂进本场的中继集合:此后每回合给它发一帧,结束时发终局帧;升广播后不再给它发任何帧(TURN 与 END 都只走广播)。
    // gate_node_id 为 0 或 gate_instance_id 为空 = kInvalidParameter(空目标会关掉 gate 侧的目标过滤,AGENTS.md §7.2)。
    // 已是广播档:回 broadcast = true,不挂 gate、不发帧(每台 gate 都在读广播,match 据此把档位同步成 L2)。
    // 新挂接,或同 gate_node_id 换了实例(node_id 会被回收,旧实例已死,直接覆盖):只给这台 gate 补发一帧 STATE。
    // 幂等:同 (gate_node_id, gate_instance_id) 重复挂接只回包、不再补发(match 并发准入与回包丢失后的重试都会走到这里)。
    void HandleAttachRelayGate(const ::AttachRelayGateRequest &request, ::AttachRelayGateResponse &response);

    // 把本场升为全服广播,单向:只升不降。此后每回合与终局帧都只往广播 topic 发一帧,不再逐 gate 中继。
    // 首次升档往广播 topic 发一帧 STATE。幂等:重复调用只回包、不再发帧。
    void HandlePromoteSpectateBroadcast(const ::PromoteSpectateBroadcastRequest &request,
                                        ::PromoteSpectateBroadcastResponse &response);

    // 注入观战帧出口(显式依赖);空 = 默认的真实 Kafka 生产者。给测试用,生产不调用;只能在 loop 线程调用。
    void SetSpectateFrameSink(battle_spectate_relay::FrameSink sink)
    {
        spectateFramePublisher_.SetSink(std::move(sink));
    }

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

    // 停机收尾,供 SetBeforeShutdown 调用:停掉房间定时器、关闭房间直连,不铸造任何终局(观众不收 SpectateEnd:
    // 此刻发 ABORTED 可能与 Match 的真实决定矛盾,中继观众靠 gate 的空闲超时收尾)。
    // 已算出、还没拿到 SQL 决定的终局不在这里作废,而是立即再提交一轮(2026-10-23 修复,审计 r2-battle#2,
    // turnbattle::NeedsShutdownOutcomeFlush),由 SqlOutcomesFlushed / StopSqlOutcomes 在停机排空窗口内收口;
    // 还在打的房间没有终局可交,按作废处理,match 在权威截止后原子判 ABORT,全员作废。
    // 房间保留在表里直到拿到决定或 manager 销毁,不回 Ready(终局未知的房间不能算已移除)。
    // 调用方必须在同一个 loop 任务里先关建房准入闸(BattleNodeImpl::CloseAdmission):gRPC 排空
    // 期间已收完的 CreateBattle 仍会派发(等许可的回调也可能排在关闸之后),不关闸它们会在此之后照常建房。
    //
    // SQL 终局迟迟拿不到决定的房间(滞留提醒里的 sql_unconfirmed)不会自然消失,节点因此排不空。值班按提醒排除 Match / TiDB 故障后
    // 分三步收尾,不要直接删 Pod(会把同节点上正在打的房间一起作废):
    //   1. 给该 GameServer 打排空标签:kubectl -n mmorpg-infra label gs <name> mmorpg.io/drain=true(main.cpp kAgonesDrainLabelKey),
    //      进程 5s 内读到后拒绝新房间;
    //   2. 等其余房间自然打完:提醒行的 node_rooms 降到与 stuck_total 相等(单局上限 = match 的 BattleMaxDurationSeconds,默认 300s);
    //   3. kubectl -n mmorpg-infra delete gs <name>,优雅停机走这里收尾。
    // 非 Agones 的 Deployment 形态(只用于验证 / 回退)没有排空标签,新房间会继续进来,第 2 步等不到;
    // 只能择低峰删 Pod,并接受同节点在打的房间被作废。
    void AbortAllRooms(const std::string &reason);

    // 停机排空谓词(main.cpp 的 Node::SetShutdownDrainComplete):AbortAllRooms 补提交的 v1 终局都拿到 COMPLETE / ABORT
    // 之后为真。只读房间表与待提交集合,每 100ms 被 Node 现场重查;有界于 Node 的排空预算(kShutdownDrainBudget),
    // 预算到了 Node 照常收尾。没有待提交终局的停机(常态)立即为真。
    bool SqlOutcomesFlushed() const;

    // 拆除前钩子(main.cpp 的 Node::SetBeforeTeardown):装终局的停止闸(此后到达的回复只打日志、不产生任何副作用),
    // 停掉重试定时器,并把仍没落定的终局逐局打 metric=battle_sql_outcome_unflushed_at_shutdown。此刻出站通道已关、
    // 在途调用的失败回调已跑完。原来停止闸在 AbortAllRooms 里装,排空窗口内拿不到任何终局(2026-10-23 修复)。
    void StopSqlOutcomes();

    // ---- 滞留房间提醒 ----
    //
    // 每 kStuckRoomReportIntervalSec 秒从房间表数一遍 SQL 终局迟迟没有决定的房间(ReportStuckOutcomeRooms),
    // 不为 0 时打 metric=battle_personal_settlement_staging_exhausted_rooms sql_unconfirmed= stuck_total= node_rooms=。
    // battle 进程没有 Prometheus 端点,这一行就是唯一信号(值班 LogQL 登记在 deploy/k8s/asset-channel-alerts.yaml)。
    static constexpr double kStuckRoomReportIntervalSec = 60.0;

    // ---- 观众席上限与回收(设计文档 §10.5)----
    //
    // 单房间观众席上限。纯内存房间,每观众每回合一条直连推送,上限约束单房间出站扇出,
    // 防观战成为放大器。满员回 kMatchSpectateFull(见 HandleAddObserver)。
    static constexpr size_t kMaxObserversPerRoom = 20;

    // 节点级观众席配额(battle-spectate-tiered-fanout.md §10 第 4 条)。观众直连与参战者共用直连面的连接上限
    // (BattleClientEdge::EffectiveMaxConnections),不给观众划额度,热门节点上的
    // 观众会把参战者挤在直连面之外。全节点已登记的观众席(各房间名单人数之和,口径同单房间满员:含待连接与
    // 宽限中的观众)达到配额即拒新观众,回 kMatchSpectateFull:对 match 与单房间满员同义(「直连席已满」),
    // P1 起据此转 gate 中继档。
    // 配额 = max(kMaxObserversPerRoom, 有效连接上限 / kObserverConnectionShareDivisor):观众最多占四分之一,
    // 连接上限很小时也至少坐得下一个满员房间。
    static constexpr size_t kObserverConnectionShareDivisor = 4;
    static constexpr size_t ObserverSeatQuotaFor(const size_t effectiveMaxConnections)
    {
        return std::max(kMaxObserversPerRoom, effectiveMaxConnections / kObserverConnectionShareDivisor);
    }

    // 注入节点级观众席配额。生产由 SetClientEdge 在装配时注入;测试在借用直连面之后直接注入小值
    // (借用非空直连面会按连接上限重新注入)。只约束此后的新准入:调低后在册人数可能暂时高于配额,
    // 已登记的观众不会因此被摘除,同 op 幂等重放照常成功。只能在 loop 线程调用(与准入同线程,无锁)。
    void SetObserverSeatQuota(const size_t quota) { observerSeatQuota_ = quota; }

    // 满员按名单人数判定(routingByObserver),名单里除了已挂直连的观众,还有登记后尚未建直连、
    // 或直连断开等重连的观众。客户端崩溃 / 放弃观战而 match 没来 Remove 时,后两类会一直占座,
    // 房间满员后新观众全被拒。所以:
    //   新登记(以及防御性的换会话关旧直连,该路径现有契约下走不到)之后 kObserverConnectGrace 内没挂上直连 → 回收;
    //   直连断开之后 kObserverReconnectGrace 内没重连 → 回收。
    // 挂上直连即停表。补签(IssueBattleTicket)与同会话幂等重放都不延长宽限,否则反复补签就能一直占座。
    // 回收 = 封闭该观众的观战操作墓碑、移出名单、关直连,不推 SpectateEnd(此刻没有直连,推了也被丢)。
    // 连接宽限不短于 match 的准入窗口(Add 校验 admit_before ≤ 登记时刻 + 30s):回收时原 op 通常已过
    // 准入截止,墓碑直接删,迟到的同 op Add 由时间校验挡住;未过则封成 cancelled。match 经 QueryObserver
    // 看到 ABSENT 后自行收尾,battle 不主动通知。
    static constexpr std::chrono::seconds kObserverConnectGrace{30};
    static constexpr std::chrono::seconds kObserverReconnectGrace{30};

    // 回收 battleId 房间里期限不晚于 now 的观众席,再按剩余最早期限重挂房间定时器。
    // 房间定时器到期时以 steady_clock::now() 调用;测试以显式时刻驱动,不必等真实宽限。
    // 房间不存在时 no-op;没有到期席位时只重挂定时器(幂等,定时器早到无害)。只能在 loop 线程调用。
    void ReclaimExpiredObserverSeats(uint64_t battleId, std::chrono::steady_clock::time_point now);

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
        // battle 不解释业务字段,只回显进 BattleResultEvent,活动局的结果通道在 match。
        ::BattleActivityContext activityContext;
        // CreateBattleRequest.spectate_visibility 原样副本:建房时赋值一次,整场不变,只供观战准入
        // (AddObserver 的第二道闸与 AttachRelayGate / PromoteSpectateBroadcast)读,只放行 PUBLIC。
        // 缺省 UNSPECIFIED 与 PRIVATE 一样不可观战(默认拒绝)。
        ::eSpectateVisibility spectateVisibility = ::SPECTATE_VISIBILITY_UNSPECIFIED;
        // 原Create采用值，不以当前节点support替代采用证明。建房时已校验等于 kOutcomeProtocolVersion,只回显给 match。
        uint32_t outcomeProtocol = 0;
        std::string placementOperation;
        std::string targetInstance;
        bool outcomePending = false;
        bool outcomeComplete = false;
        bool outcomeInFlight = false;
        bool outcomeExhausted = false;
        uint64_t outcomeAttempt = 0;
        std::string outcomePayload;
        std::string outcomeRequestDigest;
        std::chrono::steady_clock::time_point outcomeSince{};
        std::chrono::steady_clock::time_point nextOutcomeAttempt{};
        ::contracts::kafka::BattleResultEvent pendingResult;
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
        // 观众席回收期限(单调时钟)。只为名单上没挂直连的观众计时:缺项 = 已挂直连或不在名单。
        // reconnect 只区分回收原因(日志 cause),两种宽限的回收动作相同。
        struct ObserverSeatDeadline
        {
            std::chrono::steady_clock::time_point at{};
            bool reconnect = false;
        };
        std::map<uint64_t, ObserverSeatDeadline> observerSeatDeadlines;
        // 只挂「最早期限」的一次性定时器。摘除观众不重挂(期限只会变晚,早到一次由回收入口重挂);
        // 新增期限时重挂。回调只捕获 WeakSelf() 与 battleId(AGENTS.md §11.7),随房间析构取消。
        TimerTaskComp observerSeatTimer;
        // L1 中继:gate_node_id → gate_instance_id(中继帧的 target_instance_id)。只经 HandleAttachRelayGate 写入,
        // 键值都已校验非空;同 node_id 新实例覆盖旧实例。有序容器:发帧顺序稳定。升广播后不再往这里发帧,也不再写入。
        std::map<uint32_t, std::string> relayGates;
        // L2:本场已升全服广播,只升不降。
        bool spectateBroadcast = false;
        // 本场有没有观战帧要发。绝大多数对局一个观众都没有,连观众版状态都不用做。
        bool HasSpectateAudience() const
        {
            return !routingByObserver.empty() || !relayGates.empty() || spectateBroadcast;
        }
        TimerTaskComp roundTimer;      // 回合 action_deadline
        TimerTaskComp battleTimer;     // 整场 deadline_ms(强制收尾,防房间泄漏)
        uint64_t actionDeadlineMs = 0; // 当前回合行动截止(Unix 毫秒,GetBattleState 回填)
        uint64_t deadlineMs = 0;       // 整场作废期限(与 battleTimer 同值;补发确认事件时带给 scene)
        // BattleConfirmedEvent 补发:开局后 kConfirmResendWindowMs 内每 kConfirmResendIntervalSec
        // 向全部参战玩家重发一次(scene 幂等)。没有 scene→battle 的确认回执通道(proto 已定),
        // 用有界周期补发覆盖"首发 produce 失败 / Kafka 积压 / scene 消费者 rebalance"这类
        // 单次投递丢失或迟到;窗口按 scene 侧锁保留期取 180s:prepare TTL(match matchedTicketTTLFor,
        // 最长 10 人 83s)+ kLockExtraTtlSec 60s = 143s,再留余量(推导见 .cpp kConfirmResendWindowMs)。
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

    // 公布公共终局:最后一回合帧 → 观众按 pendingSpectateReason 收 SpectateEndS2C(§10.5)→ 终局包排队后关闭房间全部直连。
    // 只在 Match 持久 COMPLETE 之后调用,终局一律取冻结的原件 pendingResult,不重新计算。
    // 参战者个人的 BattleEndS2C 与资产可见性由 scene 在结算随存档提交之后通知,battle 不发;对局结果由 match 发件箱投递。
    // 只组装与发送,不动 rooms_;返回 true 才能由调用方删房,校验失败保留房间。
    bool FinishBattle(BattleRoom &room);
    contracts::kafka::BattleResultEvent BuildResult(const BattleRoom&, ::eBattleOutcome) const;
    // 打完之后冻结可见状态与终局原件(只计算一次,未知回执不允许重新取掉落、时间或目标 UUID),开始提交 match SQL。
    // 原件无效时置 outcomeExhausted:终局保持未知,房间滞留,由 match 在权威截止后裁决。
    void BeginResultCommit(BattleRoom&, ::eBattleOutcome, ::eSpectateEndReason,
                           std::optional<::TurnResultS2C>, ::BattleStateS2C);
    static bool TerminalPending(const BattleRoom& room) { return room.outcomePending; }
    void PumpSqlOutcomes();
    // Match 回了持久决定之后的收尾(PumpSqlOutcomes 的回复回调调用)。调用之后 room 引用可能已悬空。
    // COMPLETE:FinishBattle 通过才删房,不通过时房间保留、继续重放。ABORT:观众收 ABORTED + ONGOING,不生成任何个人效果。
    void OnSqlOutcomeComplete(BattleRoom &room);
    void OnSqlOutcomeAbort(BattleRoom &room);
    // 把房间移出待提交集合并删房,集合空了就停重放定时器。
    void RetireSqlOutcomeRoom(uint64_t battleId);
    // 与正式请求签名相同的密钥就绪门；只在本节点生命周期首次缺失时报警。
    bool OutcomeSigningKeyReady(std::string_view secret);

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

    // 观众版全量快照:全量快照 + 房间 timer 回填的行动截止 + 观众视角裁剪。L0 首帧与中继 / 广播的 STATE 帧共用这一份,
    // observerCount 由调用方按各自口径给。等结果回执期间取冻结的 pendingVisibleState,不提前泄露终局。
    ::SpectateStateS2C BuildSpectatorState(const BattleRoom &room, uint32_t observerCount) const;

    // 观战快照:全量快照 + 当前观众数(LiveObserverCount)。战斗帧(只走直连):
    // 首帧由直连握手触发(OnDirectConnectionVerified),AddObserver 同会话幂等重推时
    // 有活直连才会真正送达。
    void PushSpectateState(const BattleRoom &room, uint64_t observerId) const;

    // 有活直连的观众数(SpectateStateS2C.observer_count 的口径:真正在看的人)。
    // 与满员判定的口径不同:满员按名单人数,含待连接与宽限中的观众。
    uint32_t LiveObserverCount(const BattleRoom &room) const;

    // 全节点已登记的观众席 = 各房间名单(routingByObserver)人数之和,节点级观众席配额按它判定。
    // 每次现算、不另设计数器:停机清房等路径会连同名单整间删房,计数器得在每条删房路径上对齐,
    // 漏一条就永久漂移;AddObserver 不是热路径,遍历房间表即可。
    size_t RegisteredObserverSeats() const;

    // 收尾/作废统一出口:同一帧 SpectateEndS2C 只序列化一次,先发给中继 gate 或广播(PublishSpectateEnd),再经直连推给
    // 全部观众(PushSpectatorFrame)并逐个 RetireObserver(封墓碑、出名单、关直连),最后取消观众席定时器(此后任何广播都
    // 不会再打到观众)。调用方不调本函数的路径(停机时结果未落定的房间)三个通道一起不发,中继观众靠 gate 的空闲超时收尾。
    void NotifySpectateEndAndClose(BattleRoom &room, ::eSpectateEndReason reason,
                                   ::eBattleOutcome outcome);

    // ---- 观战中继(L1)/ 全服广播(L2)的出站(寻址与失败语义在 battle_spectate_relay.h) ----

    // Attach / Promote 共用的前置校验(顺序与错误码见公开区契约)。通过返回房间;否则返回 nullptr 并写 rejectTip。
    BattleRoom *FindSpectateChannelRoom(uint64_t battleId, const std::string &targetInstanceId, uint32_t &rejectTip);

    // 一帧的公共字段;目标字段由 SpectateFramePublisher 按通道写入。envelope 只在这里序列化一次,gate 加自己的帧头后原样转发。
    ::contracts::kafka::SpectateFrameEvent NewSpectateFrame(const BattleRoom &room,
                                                            ::contracts::kafka::eSpectateFrameKind kind,
                                                            uint32_t roundIndex, const ::MessageContent &envelope) const;

    // 观众版当前快照编成 STATE 帧(161,observer_count = 0),round_index 取快照的回合(§15.4)。
    // 注意快照的回合是「正在收集行动、还没结算」的回合 r,紧随其后的 TURN 同样带 r:gate 不能用 STATE 推进已收回合,
    // 否则这一回合的结算帧会被当成重复丢掉。
    ::contracts::kafka::SpectateFrameEvent BuildSpectateStateFrame(const BattleRoom &room) const;

    // 每回合一帧 TURN:已升广播只发广播 1 条;否则每台中继 gate 各 1 条。两者都没有时什么都不做。
    void PublishSpectateTurn(const BattleRoom &room, uint32_t roundIndex, const ::MessageContent &envelope);

    // 终局帧 END,通道与 TURN 相同:已升广播只发广播 1 条(升档之前挂的 gate 也不再发);否则每台中继 gate 各 1 条。
    // 升档后不能再发中继:两条 topic 之间没有顺序,中继 END 越过广播上的最后一回合 TURN 时,gate 收尾后把那一帧丢掉。
    // 代价:广播上的 END 丢了,这些 gate 上的观众要等空闲超时收尾(与最后一回合之前丢帧同一条兜底)。
    // round_index 取引擎当前回合(最后一回合)。
    void PublishSpectateEnd(const BattleRoom &room, const ::MessageContent &envelope);

    // 观众出房的唯一收口(补签失败 / RemoveObserver / StopWatchBattle / 收尾作废 / 席位回收):
    // 封闭该观众当前观战操作的墓碑(原准入截止已过即删,否则置 cancelled,挡住同 op 迟到的 Add),
    // 移出名单、名字、操作与席位期限,关其直连(CloseDirectConnectionOf,推迟到本轮 loop 之后)。
    // 不推 SpectateEnd:要推的调用方先推再调本函数,同一条直连上终局包先于 FIN。
    // 不重挂观众席定时器(期限只会变晚,早到一次由 ReclaimExpiredObserverSeats 重挂)。观众不在房间里时幂等。
    void RetireObserver(BattleRoom &room, uint64_t observerId, const char *reason);

    // 为名单上的观众(重新)开始席位宽限:期限 = 当前单调时刻 + 对应宽限,并重挂房间定时器。
    // 只在新登记、换会话关旧直连、直连断开三处调用;补签与幂等重放不调,宽限不因它们延长。
    void StartObserverSeatGrace(BattleRoom &room, uint64_t observerId, bool reconnect);

    // 按 observerSeatDeadlines 的最早期限挂一次性定时器(空表即取消)。按真实单调时钟算延迟;
    // 回调只捕获 WeakSelf() 与 battleId,到期重查房间(AGENTS.md §11.7)。
    void ArmObserverSeatTimer(BattleRoom &room);

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

    // 发给全部观众的同一帧(SpectateTurnResult / SpectateEnd):envelope 由调用方序列化好(同一份也用于中继 / 广播),
    // 这里只分帧一次,再对名单上每条活直连写出同一份字节(battle-spectate-tiered-fanout.md §10 第 3 条);逐人的判定与
    // 丢弃日志同 PushBattleFrame。只用于与收信人无关的观众帧;按人裁剪的帧(参战者回合结果、观战首帧)仍走 PushBattleFrame。
    void PushSpectatorFrame(const BattleRoom &room, const ::MessageContent &envelope) const;

    // 战斗帧出口的逐人路由判定(battle_push_policy:只走直连),上面两个出口共用同一判定:
    // 有活直连返回它;否则打采样丢弃日志,返回空。
    muduo::net::TcpConnectionPtr BattleFrameConnOf(const BattleRoom &room, uint64_t playerId,
                                                   uint32_t messageId) const;

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

    // 保证滞留房间提醒的定时器已装填(幂等)。房间第一次滞留时装填(原件无效 / SQL 终局越过快速跨度),之后常驻:
    // 房间被清掉后 ReportStuckOutcomeRooms 自然不再打行。
    void EnsureStuckRoomReporter();
    // 定时器回调:从房间表现数 SQL 终局迟迟没有决定的房间(原件无效、或越过了快速重放跨度),不为 0 就打一行
    // metric=battle_personal_settlement_staging_exhausted_rooms sql_unconfirmed= stuck_total= node_rooms=,
    // battle_id 列表只放正文。metric 名沿用已登记的值班查询。
    void ReportStuckOutcomeRooms();

    // 绑进定时器与 gRPC 回调的 weak_ptr<自己>(AGENTS.md §11.7)。
    // 本类是进程级单例(Instance() 里的函数静态对象),不由 shared_ptr 持有,拿不到 weak_from_this();
    // 这里用别名构造把 this 挂在 lifeToken_ 的控制块上:lifeToken_ 随本对象析构,之后 lock() 必然失败。
    // 实际上单例寿命覆盖整个 loop,这层是按 §11.7 "只绑 weak_ptr<自己>"写的防线,不是在修某个已知的悬垂路径。
    std::weak_ptr<BattleRoomManager> WeakSelf() { return std::shared_ptr<BattleRoomManager>(lifeToken_, this); }

    // 仅借用运行时上下文持有的直连面；清空借用不销毁对象。
    std::optional<std::reference_wrapper<BattleClientEdge>> edge_;
    // 节点级观众席配额(SetObserverSeatQuota)。装配前(还没借到直连面)只留一个满员房间的余量。
    size_t observerSeatQuota_ = kMaxObserversPerRoom;
    // 观战中继 / 广播的出口;默认走真实 Kafka 生产者(SetSpectateFrameSink 可换)。
    battle_spectate_relay::SpectateFramePublisher spectateFramePublisher_;

    // 房间表:增删与生命周期回调(SetRoomLifecycleHooks)绑死在 RoomTable 里,本类只经
    // EmplaceRoom / EraseRoom 调它的 Emplace / Erase,其余路径只读(FindRoom / AbortAllRooms 抄 id)。
    battle_room_table::RoomTable<BattleRoom> rooms_;
    // 本实例操作墓碑只在loop线程访问；进程重启后精确target_instance使旧RPC失效。
    struct CreationOperation {std::string operationId;std::string canonicalRequest;uint64_t expiresMs=0;bool aborted=false;};
    std::unordered_map<uint64_t,CreationOperation> creationOperations_;
    // 取消先于Add时房间可能尚不存在；墓碑在manager而非room，保留到原准入截止。
    // rejectTip:本操作首次被拒的码,同 op、同请求的重放回同一结论(0 = 不是被拒关闭,而是被 Remove / 回收封闭)。
    struct SpectatorOperation { uint64_t admitBeforeMs{0}; std::string canonicalRequest; bool cancelled{true}; uint32_t rejectTip{0}; };
    using SpectatorKey = std::tuple<uint64_t, uint64_t, std::string>;
    std::map<SpectatorKey, SpectatorOperation> spectatorOperations_;
    std::multimap<uint64_t, SpectatorKey> spectatorDeadlines_;
    void PruneSpectatorOperations(uint64_t now);
    bool outcomeStopping_ = false;
    bool outcomeKeyUnavailableLogged_ = false;
    std::set<uint64_t> sqlOutcomeRooms_;
    TimerTaskComp sqlOutcomeTimer_;
    // 滞留房间提醒(EnsureStuckRoomReporter);装填后常驻。
    TimerTaskComp stuckRoomReportTimer_;

    // WeakSelf() 的存活令牌。声明在最后 = 最先析构:析构期间任何迟到回调都已 lock 不到本对象。
    std::shared_ptr<char> lifeToken_ = std::make_shared<char>();
};
