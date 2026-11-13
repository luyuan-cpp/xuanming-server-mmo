#pragma once

#include <cassert>
#include <cstddef>
#include <cstdint>
#include <unordered_map>
#include <utility>
#include <vector>

#include "entt/src/entt/entity/entity.hpp"
#include "engine/core/type_define/type_define.h" // EntityVector

class Location;
class Rotation;
class Vector3;

// TeleportS2C.reason 的取值。0..4 沿用 proto/scene/player_movement.proto 注释里的既有约定,
// 5 起是服务器主动瞬移新增的原因(客户端目前只把它写进日志)。
constexpr uint32_t kTeleportReasonTeamRegroup = 5; // 组队:把队员带到队长身边

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

    // 把 entity 此刻的权威位置 / 速度广播成 ActorMoveS2C。这是已经在看他的玩家"看到他在走 / 停下"的通道:
    // 客户端只按 ActorMoveS2C 驱动远端角色的插值与外推(ActorBaseAttributesS2C 那一路的移动脏位不带
    // entity_id,观察者无从归属,客户端也没有接)。后来才看见他的玩家由 AOI 在创建之后补一条同样内容的消息
    // (AoiSystem::SendVisibilityChanges);消息内容与 move_state 的取值在 spatial/system/actor_motion.h。
    // 接收者 = 客户端正在显示 entity 的玩家:AOI 反向索引(AoiWatchersComp)中可见、且有网关会话的观察者。
    // 关注是单向的(docs/design/aoi-directed-interest.md):接收与否只看对方是否关注并显示我,
    // 与我是否看得见对方无关;网格范围外的显式关注者同样收到。隐身期间没有侦测授权的观察者不在名单里。
    // 调用时机:服务器接受一条移动上报之后,服务器自己把速度清零的时刻(撞墙,以及 HaltMotion:挂机、备战、
    // 交接冻结、离场前停步)—— 后一类不广播的话,观察者会按最后一次上报的速度一直外推下去 —— 以及同场景瞬移之后。
    // includeSelf:同时发给 entity 自己的客户端(组队跟随中由服务器驱动走位的队员,本人的位置也以这条为准,
    // 见 TeamFollowSystem);普通移动的本人以自己的预测为准,不发。
    // 没有接收者或没有 Transform 时直接返回。只读组件、不增删组件,可以在 view 遍历中调用。
    static void BroadcastMove(entt::entity entity, bool includeSelf = false);

    // 服务器让 entity 停在原地:速度非零时清零、置速度脏位,并给正在显示他的观察者补一条停步(BroadcastMove,不含本人)。
    // 已经停着的(速度为零)什么都不改、不发;没有 Velocity 组件的不补建。
    // 进入 MovementSystem 不再积分位移的状态时调用(挂机、备战与战斗冻结、归属交接冻结,目录见 actor_motion.h 的
    // kNotIntegratedByMovement);退出与断线保留的停步也是它(PlayerLifecycleSystem::StopMotionForExit)。
    // 不停的话:观察者按最后一条移动一直外推;状态结束后服务器也会从原地按旧速度接着积分。
    // 组队随从:TeamFollowSystem 下一 tick 按自己的规则接手(暂停驱动时补停步;继续驱动时把这次当作"被别处停下",立刻补起步)。
    // 不增删 Velocity,会按需建脏位组件;只能在 scene 节点 loop 线程调用。
    static void HaltMotion(entt::entity entity);

    // 把实体瞬移到**同一场景内**的另一点(服务器权威)。调用方保证 location 是该场景导航上的合法点。
    //   1. 写 Transform、清零 Velocity(否则 Update 会从新位置按瞬移前的速度继续积分);
    //   2. 挂 TeleportSettleComp,挡住瞬移之前发出的移动上报(见该组件注释);
    //   3. 实体是玩家时给它自己的客户端发 TeleportS2C(客户端收到即吸附);
    //   4. AoiSystem::ResetEntityVisibility:旧位置的空间观察者收到销毁,新位置的可见性在下一次
    //      AoiSystem::Update 重建;
    //   5. BroadcastMove:仍显示它的显式关注者(网格范围外)立即收到新位置。
    // 严格节庆地图要求导航可用、终点合法且同域可达；拒绝时返回 false 且不写任何位置状态。
    // 没有 Transform 的实体返回 false。只能在 scene 节点 loop 线程调用。
    static bool TeleportWithinScene(entt::entity entity, const Vector3& location, const Rotation& rotation,
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

// 一批移动广播里"谁收哪几条"的分组(ActorMoveBatch 用)。纯逻辑,头文件内联,供 cpp/tests/aoi_test 直接单测。
//   receiversPerMove[i] = 第 i 条移动的接收者,条数不超过 64。
//   要收的条目集合相同的接收者分在一组;moveMask 第 i 位为 1 表示这一组收第 i 条。
//   组按其第一个接收者第一次出现的先后排,组内接收者也按第一次出现的先后;同一接收者重复出现只算一次。
struct MoveReceiverGroup
{
	uint64_t moveMask{0};
	EntityVector receivers;
};

inline std::vector<MoveReceiverGroup> GroupMoveReceivers(const std::vector<EntityVector>& receiversPerMove)
{
	assert(receiversPerMove.size() <= 64);
	// 接收者 -> 他要收的条目(位掩码),按第一次出现的先后。
	std::vector<std::pair<entt::entity, uint64_t>> wanted;
	std::unordered_map<entt::entity, std::size_t> wantedIndex;
	for (std::size_t move = 0; move < receiversPerMove.size(); ++move)
	{
		for (const auto receiver : receiversPerMove[move])
		{
			const auto [it, inserted] = wantedIndex.try_emplace(receiver, wanted.size());
			if (inserted)
			{
				wanted.emplace_back(receiver, 0);
			}
			wanted[it->second].second |= uint64_t{1} << move;
		}
	}

	std::vector<MoveReceiverGroup> groups;
	std::unordered_map<uint64_t, std::size_t> groupIndex;
	for (const auto& [receiver, moveMask] : wanted)
	{
		const auto [it, inserted] = groupIndex.try_emplace(moveMask, groups.size());
		if (inserted)
		{
			groups.push_back(MoveReceiverGroup{moveMask, {}});
		}
		groups[it->second].receivers.push_back(receiver);
	}
	return groups;
}

// 同一 tick 里一批实体的移动广播,按接收者合并发出。
//
// 用途:组队跟随 —— 同一队形的队员常在同一 tick 起步、停步、拐弯、补校正,分开发时每个观察者每 tick 收好几条。
// 合并后每个接收者每批只收一条:只收一个实体时是 ActorMoveS2C,收多个时是 ActorMoveListS2C
// (客户端逐条按 ActorMoveS2C 处理,结果与分开发相同)。要收同一组实体的接收者共用一条消息:
// 每组序列化一次、每个 gate 一个请求,与 MovementSystem::BroadcastMove 同一个形状。
// 接收者口径、消息内容都与 BroadcastMove 相同,内容取 Flush 那一刻的 Transform / Velocity。
//
// 先后:批里的消息到 Flush 才发。同一实体在 Add 之后、Flush 之前另外直接发出的消息(瞬移、跟随状态)会跑到它前面 ——
// 与那类消息有先后要求时不要进批,直接调 BroadcastMove(TeamFollowSystem 停止驱动一个队员时就是这样)。
// 线程模型:scene 节点 loop 线程独占,在一次调用栈里 Add ... Flush。
class ActorMoveBatch
{
public:
	// 一批最多这么多个实体(分组用 64 位掩码)。登记满了再 Add 时,先把已登记的发出去。
	static constexpr std::size_t kMaxEntities = 64;

	// 登记 entity 这一批要广播移动;includeSelf 同 BroadcastMove。同一实体登记多次只发一条,includeSelf 取或。
	void Add(entt::entity entity, bool includeSelf);
	// 把全部登记发出并清空;没有登记时什么都不做。
	void Flush();
	[[nodiscard]] bool Empty() const { return entries_.empty(); }

private:
	struct Entry
	{
		entt::entity entity{entt::null};
		bool includeSelf{false};
	};
	std::vector<Entry> entries_;
};
