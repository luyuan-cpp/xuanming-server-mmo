#pragma once

#include <cstdint>

#include "entt/src/entt/entity/entity.hpp"

class Location;
class Rotation;
class Vector3;

// TeleportS2C.reason 的取值。0..4 沿用 proto/scene/player_movement.proto 注释里的既有约定,
// 5 起是服务器主动瞬移新增的原因(客户端目前只把它写进日志)。
constexpr uint32_t kTeleportReasonTeamRegroup = 5; // 组队:把队员带到队长身边

// ActorMoveS2C.move_state 的取值(proto/scene/player_movement.proto):0=停止,1=行走。
constexpr uint32_t kMoveStateStopped = 0;
constexpr uint32_t kMoveStateWalking = 1;

// 瞬移后等待客户端落位的最长时间。超过即认为这次瞬移没有被客户端应用,恢复照常裁决。
constexpr uint64_t kTeleportSettleTimeoutMs = 3000;
// 上报点与瞬移目标的水平距离在此之内,视为客户端已经落位(客户端落位后的第一条上报
// 就是从目标点出发的 MoveStart,正常为 0;留出余量给导航吸附与首帧位移)。
constexpr double kTeleportSettleRadius = 3.0;

// 瞬移后收到一条移动上报时的判定。
enum class TeleportReportVerdict : uint8_t
{
	kStale = 0,   // 仍在等待落位,且上报点离目标太远:瞬移之前发出的包,丢弃
	kSettled = 1, // 上报点在目标附近(不论是否已超时):客户端已落位,解除等待并照常裁决
	kExpired = 2, // 超时后仍在远处上报:客户端没有应用这次瞬移,解除等待并照常裁决
};

class MovementSystem
{
public:
    static void Update(double delta);

    // 把 entity 此刻的权威位置 / 速度广播成 ActorMoveS2C。这是别的玩家"看到他在走 / 停下"的唯一通道:
    // 客户端只按 ActorMoveS2C 驱动远端角色的插值与外推(ActorBaseAttributesS2C 那一路的移动脏位不带
    // entity_id,观察者无从归属,客户端也没有接)。
    // 接收者 = 客户端正在显示 entity 的玩家:AOI 反向索引(AoiWatchersComp)中可见、且有网关会话的观察者。
    // 关注是单向的(docs/design/aoi-directed-interest.md):接收与否只看对方是否关注并显示我,
    // 与我是否看得见对方无关;网格范围外的显式关注者同样收到。隐身期间没有侦测授权的观察者不在名单里。
    // 调用时机:服务器接受一条移动上报之后,服务器自己把速度清零的时刻(撞墙、战斗 / 冻结中的停步、
    // 离场前停步)—— 后一类不广播的话,观察者会按最后一次上报的速度一直外推下去 —— 以及同场景瞬移之后。
    // includeSelf:同时发给 entity 自己的客户端(组队跟随中由服务器驱动走位的队员,本人的位置也以这条为准,
    // 见 TeamFollowSystem);普通移动的本人以自己的预测为准,不发。
    // 没有接收者或没有 Transform 时直接返回。只读组件、不增删组件,可以在 view 遍历中调用。
    static void BroadcastMove(entt::entity entity, bool includeSelf = false);

    // 把实体瞬移到**同一场景内**的另一点(服务器权威)。调用方保证 location 是该场景导航上的合法点。
    //   1. 写 Transform、清零 Velocity(否则 Update 会从新位置按瞬移前的速度继续积分);
    //   2. 挂 TeleportSettleComp,挡住瞬移之前发出的移动上报(见该组件注释);
    //   3. 实体是玩家时给它自己的客户端发 TeleportS2C(客户端收到即吸附);
    //   4. AoiSystem::ResetEntityVisibility:旧位置的空间观察者收到销毁,新位置的可见性在下一次
    //      AoiSystem::Update 重建;
    //   5. BroadcastMove:仍显示它的显式关注者(网格范围外)立即收到新位置。
    // 没有 Transform 的实体直接返回。只能在 scene 节点 loop 线程调用。
    static void TeleportWithinScene(entt::entity entity, const Vector3& location, const Rotation& rotation,
                                    uint32_t reason);

    // 移动上报(MoveStart / MoveSync / MoveStop)在裁决之前先问这一句:返回 true 表示这条上报是
    // 瞬移之前发出的,调用方必须整条丢弃(位置与速度都不应用)。没有在等待落位时恒为 false。
    // 判定为已落位 / 已超时的同时摘掉 TeleportSettleComp。
    static bool ShouldDropReportAfterTeleport(entt::entity entity, const Location& reported);

    // 放弃等待瞬移落位(摘 TeleportSettleComp,没有时什么都不做)。
    // 调用方:TeamFollowSystem 停止驱动队员时 —— 驱动期间客户端上报全被丢弃、位置由服务器写,
    // 恢复上报时应以当前位置为准,而不是去比对驱动之前那次瞬移的目标点。
    static void CancelTeleportSettle(entt::entity entity);

    // ---- 纯函数(无 I/O、无 ECS,头文件内联,供 cpp/tests/aoi_test 直接单测)----
    static TeleportReportVerdict JudgeReportAfterTeleport(double horizontalDistanceToTarget, uint64_t nowMs,
                                                          uint64_t expireAtMs)
    {
        // 先看距离再看时间:客户端落位后可能站着不动,过了超时才发第一条上报,
        // 那仍然是一次正常的落位,不该算成"没等到确认"。
        if (horizontalDistanceToTarget <= kTeleportSettleRadius)
        {
            return TeleportReportVerdict::kSettled;
        }
        return nowMs >= expireAtMs ? TeleportReportVerdict::kExpired : TeleportReportVerdict::kStale;
    }
};
