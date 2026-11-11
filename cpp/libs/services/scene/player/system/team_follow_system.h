#pragma once

// 组队跟随的 ECS 层:服务器驱动队员沿队长轨迹走位(docs/design/team-follow-server-trail.md)。
//
// 职责:把纯逻辑层 TeamFollowFormation 接到 ECS 上 —— 每 tick 读队长位置、判定每名队员能不能动、
// 把结果写回 Transform / Velocity 并广播;管理队形的建立、加入、移出与解散。
// 不负责:成员关系与"该跟谁"(PlayerTeamSystem 读组队权威后调 TryAttach / OnTeamChanged / OnBecameLeader /
//         OnNotLeader);跨场景 / 跨节点跟随(仍由 PlayerTeamSystem 发 EnterScene)。队长离开本节点导致队形解散时,
//         这里只记下"原随从在等队长去留"(TeamFollowAwaitLeaderComp),何时重新检查由 PlayerTeamSystem 取走结论后决定。
//
// 性能:不寻路。每 tick 每支队伍 O(队员数) 的游标推进,加至多一次回收搜索(只搜队长脚下几米);
// 被驱动的队员不走 MovementSystem 的逐 tick 导航射线,也不再收客户端移动上报。导航只在队员加入时
// 用一次 FindPath、在回收捷径时用一次射线。没有 dtCrowd 那类每代理每 tick 的避让与路径走廊。
//
// 兼容:只驱动客户端声明了跟随(TeamFollowC2S{follow=true})的队员;旧客户端从不声明,
// 走原来的客户端跟随与归队瞬移,行为不变。
//
// 线程模型:全部在 scene 节点 loop 线程调用;不跨 loop、不持有跨 tick 的实体引用以外的状态。

#include <cstdint>
#include <vector>

#include "core/constants/fps.h"
#include "entt/src/entt/entity/entity.hpp"
#include "proto/scene/player_movement.pb.h" // TeamFollowStopReason:停止驱动的原因,会发给客户端

// 队长离开本节点后,原随从最多等这么久(帧)看队长的去留(TeamFollowAwaitLeaderComp)。要盖住一次交接最长的冻结
// (travel_freeze_cap::kFreezeCap 70 秒,超过就由冻结上限收口)和退出存盘的落地,取 90 秒;等满还没结论就放弃,
// 之后靠别的信号(队长回到本节点的扇出、客户端重新声明、战斗结束)再接上。
inline constexpr uint32_t kTeamFollowAwaitLeaderFrames = 90 * kTargetFPS;

class TeamFollowSystem
{
public:
	// World::Update 每 tick 调一次,放在 MovementSystem::Update 之后(读队长本 tick 的最终位置)。
	static void Update(double deltaSeconds);

	// 正被服务器驱动(挂着 TeamFollowerComp)。移动上报 handler 据此整条丢弃客户端的移动上报。
	static bool IsDriven(entt::entity player);

	// 客户端声明跟随(follow=true,挂意图)或暂离(follow=false,摘意图并立刻停止驱动)。
	// 只改意图;声明跟随之后要不要、何时接上,由 PlayerTeamSystem 的跟随检查决定。
	static void SetFollowIntent(entt::entity player, bool follow);
	static bool WantsFollow(entt::entity player);

	// 记下本人入队序号(决定名次)。PlayerTeamSystem 每次应用成员关系时调用。
	static void RememberJoinSeq(entt::entity player, uint32_t joinSeq);

	// 把 follower 接到 leader 身后,由服务器驱动走位。调用方(PlayerTeamSystem::RegroupToLeader)保证:
	// 两人同在一个场景实例、leader 是 follower 当前队伍的队长、交接 / 战斗 / 会话守卫都已通过。
	// walkAllowed:两人互相可见且离得不远,可以走过去;否则直接放进槽位(瞬移)。
	// 落位规则:已站在队长走过的路上 → 原地接上;能走 → 寻路走到队尾;否则瞬移进槽位。
	// 返回 true = 已接管(含本来就在这个队形里),调用方不再按旧规则瞬移;
	// false = 没有声明跟随、两边成员关系还没对齐或队形已满,调用方照旧处理。
	static bool TryAttach(entt::entity follower, entt::entity leader, bool walkAllowed);

	// 停止驱动 follower(不在任何队形里时什么都不做):移出队形、清零速度、补一条停步广播、通知其客户端。
	static void Detach(entt::entity follower, TeamFollowStopReason reason);

	// 客户端刚收到入场通知(新登录接回断线保留 / 托管中的角色,或同一会话重新入场):它把跟随状态清成了
	// "没被驱动",仍接在队长身后的就当场补推一次 driven=true。不等组队读回来 —— 那条读可能失败或被别的
	// 守卫挡住,客户端就会以为自己能走,而服务器其实还在带着他走。换了场景实例的(队形还记着旧场景)不推:
	// 下一 tick 会按换场景停止驱动,接下来的跟随检查再重新接上。
	static void ResendStateAfterEntry(entt::entity player);

	// 队形因"队长离开本节点"解散后在等队长去留的随从(TeamFollowAwaitLeaderComp),这一 tick 有了结论的取走:
	// 队长实体已不在本节点,或队长不再处于离开中(交接作废);等满超时的直接摘掉、不返回。返回的随从仍声明跟随,
	// 调用方(PlayerTeamSystem::FollowAfterLeaderSettled)对他们重新走一遍跟随检查。没有在等的随从时零开销。
	// 返回的引用指向本线程的复用缓冲,下一次调用前有效。
	static const std::vector<entt::entity>& TakeFollowersWithSettledLeader();

	// 成员关系里的 TeamId 变了:作为队员停止驱动,作为队长解散队形。
	static void OnTeamChanged(entt::entity player);
	// 读到自己是队长:不再被驱动。
	static void OnBecameLeader(entt::entity player);
	// 读到自己不是队长:还挂着队形(刚把队长转出去)就解散,原来的队员会在各自的刷新里接到新队长身后。
	static void OnNotLeader(entt::entity player);
};
