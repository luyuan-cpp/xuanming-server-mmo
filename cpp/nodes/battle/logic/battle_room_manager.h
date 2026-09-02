#pragma once

#include <cstdint>
#include <map>
#include <memory>
#include <string>
#include <unordered_map>

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
// 出站(全走 Kafka producer,设计文档 D6):
//   S2C     → GateCommand{PushToPlayerEvent},topic=gate-{gate_node_id},
//             key=player_id,target_instance_id=BattleRouting.gate_instance_id;
//   绑定     → BindBattleEvent / UnbindBattleEvent(同上走 GateCommand 信封);
//   结算     → SceneCommand{DispatchEvent, BattleSettlementEvent},
//             topic=scene-{scene_node_id},key=player_id,
//             target_instance_id=BattleRouting.scene_instance_id。
class BattleRoomManager
{
public:
    static BattleRoomManager &Instance();

    // ---- match → battle 内部 gRPC(生成骨架守护段一行委托到这里) ----

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

private:
    BattleRoomManager() = default;

    struct BattleRoom
    {
        uint64_t battleId = 0;
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
    };

    BattleRoom *FindRoom(uint64_t battleId);

    // 装填下一回合行动收集窗口并记录截止时间。
    void ArmRoundTimer(BattleRoom &room);

    // 结算当前回合:全员就绪或回合超时触发。广播 TurnResult,
    // 胜负已分则走 FinishBattle。按 battle_id 重查房间,防 timer 迟到打到已销毁房间。
    void ResolveRound(uint64_t battleId);

    // 整场 deadline 到期:引擎仍未分出胜负按平局强制收尾(battle_node.proto 契约)。
    void OnBattleDeadline(uint64_t battleId);

    // 战斗收尾:每参与者 BattleEndS2C → BattleSettlementEvent → UnbindBattleEvent;
    // 观众按 spectateReason 推 SpectateEndS2C + 解绑(§10.5)。
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

    std::unordered_map<uint64_t, std::unique_ptr<BattleRoom>> rooms_;
};
