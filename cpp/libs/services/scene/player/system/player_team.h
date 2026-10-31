#pragma once

// 组队场景跟随与队友 AOI(设计文档 docs/design/team-system.md §F)。
//
// 职责:scene消费全球Team SQL权威快照，不把Redis成员索引或Kafka事件当授权。
// 每次查询携SQL已确认的原RoleAdmissionIdentity和scene HMAC；回复签名绑定原请求，
// 回调再次校验原入场世代，并按(membership_epoch,version)拒绝倒序。
// 无队保留epoch墓碑；Redis只用于咨询性的位置和战斗锁，不修复或覆盖SQL成员关系。
// Kafka PlayerTeamRefreshEvent只通知重新查询，不携带权威数据。
//
// 语义(v1.1,2026-10-09 起;v1 是"入队不拉人、只换图不归位"):
//   - 只在同 zone、同 scene 节点内跟随(DV-6);跨节点、跨 zone 只记日志指标。
//   - 非队长进场 -> 查队长位置并请求 SceneManager 把自己切过去;
//     队长进场 -> 对本节点上其他成员逐个"刷新并跟随",不再扇出(防循环)。
//   - 入队即跟随(J-12 修订,team-system.md 文末「组队跟随 v1.1」V.2):组队服务的刷新信号只发给 TeamId 变了的人
//     (新加入者 / 被移出者),收到后除了刷新 TeamId,非队长还会走一遍跟随。
//   - 归队:队员与队长已在同一个场景实例时,若两人互相不在对方视野里、或相距超过 kTeamRegroupDistance,
//     把队员瞬移到队长脚下(MovementSystem::TeleportWithinScene)。"走路时排成一列跟着队长"由客户端做
//     (队员客户端沿队长轨迹自己走并照常上报移动),scene 只负责把人带到队长看得见的地方。
//   - 战斗中(IsInBattle 或 battle:lock 存在)一律不跟随,冻结解除后由 PlayerBattleSystem 回调补一次。
//
// 线程模型:所有方法都必须在 scene 节点 loop 线程调用;hiredis 回调同样在 loop 线程执行。
// 每一跳回调都在发出命令前捕获 player_id,回调里先核对 valid(entity) && Guid == player_id,
// 实体已销毁或槽位被复用时丢弃,绝不让另一个玩家去跟随。

#include <cstdint>

#include "entt/src/entt/entity/entity.hpp"

class PlayerTeamRefreshEvent;

// 队员离队长超过这个水平距离(服务器坐标单位)就瞬移归队。
// 客户端 5 人纵队最长约 5.6(4 个槽位 x 1.4),取 8:队形内的正常落后不触发瞬移;
// 同时小于 kMaxViewRadius(10),保证"不触发瞬移"的队员一定在队长的可见半径内。
constexpr double kTeamRegroupDistance = 8.0;

class PlayerTeamSystem
{
public:
	// ---- 纯函数(无 I/O、无 ECS,头文件内联,供 cpp/tests/aoi_test 直接单测)----

	// 是否把一次读到的成员关系写进 TeamId 组件。
	//   没有组件:应用。
	//   有组件:  只有 incomingEpoch 严格大于当前 epoch 才应用;相等或更小视为重复/乱序,忽略。
	static bool ShouldApplyMembership(uint64_t currentEpoch, bool hasComponent, uint64_t incomingEpoch)
	{
		if (!hasComponent)
		{
			return true;
		}
		return incomingEpoch > currentEpoch;
	}

	// 队员与队长已在同一个场景实例时,要不要把队员瞬移到队长脚下。
	//   互相不可见:必须瞬移。可见性只在跨格时重新评估(AoiSystem::Update),两人同在一个六边形里
	//               却互相看不见时,走到脸贴脸也不会再建立;而客户端要看得见队长才能跟着走。
	//   互相可见:  只有离得比队形还远才瞬移,队形内的落后由客户端自己走回来。
	// mutuallyVisible 由调用方给出;任一方还没被 AoiSystem::Update 评估过(没有 Hex)时按"可见"传入,
	// 理由见 RegroupToLeader。
	static bool ShouldRegroup(double horizontalDistance, bool mutuallyVisible)
	{
		return !mutuallyVisible || horizontalDistance > kTeamRegroupDistance;
	}

	// ---- 场景生命周期入口 ----

	// 玩家进入任意场景之后调用(登录、重连、跨节点加载、同节点换场景)。
	// 调用点:PlayerLifecycleSystem::EnterScene 第 3 步之后、s2s EnterScene 守护段。
	// 不放在 HandleEnterScene 里:它的"已在目标场景"幂等早退会让同场景重连漏刷新(§F.2)。
	// 刷新 TeamId;非队长则检查跟随队长,队长则对本节点其他成员逐个刷新并跟随。
	static void OnEnteredScene(entt::entity player);

	// PlayerTeamRefreshEvent(team 服务经 Kafka scene-cmd 发来的信号,只发给 TeamId 变了的人)。
	// 本节点找不到该玩家实体就丢弃;找到则刷新 TeamId,并让非队长跟随队长(入队即跟随,J-12 修订)。
	// 不扇出:建队者 / 新队长自己收到信号时不会去拉别人。
	static void OnRefreshEvent(const PlayerTeamRefreshEvent& event);

	// PlayerBattleSystem 在确实摘掉 InBattleComp 之后回调:补一次战斗中被跳过的跟随检查。
	static void OnBattleFreezeCleared(entt::entity player);

	// ---- AOI ----

	// 玩家 TeamId 变化后修正队友 AOI 优先级(§F.4)。遍历 player 当前格及邻格的全部实体,
	// 两个方向分别修正:同队 -> 升到 kTeammate;不同队且条目是 kTeammate -> 降回 kNormal。
	// 某一侧条目不存在就什么都不做(下次进入视野时 DetermineAoiPriority 按当前 TeamId 判定)。
	// 实现在 player_team_aoi.cpp:它只依赖 ECS + InterestSystem + GridSystem,
	// 单独成一个编译单元,aoi_test 链接它时不会被连带拉进 Redis/gRPC/战斗依赖。
	static void RefreshTeammateAoi(entt::entity player);

private:
	// 刷新之后要不要继续跟随:
	//   kRefreshOnly           只刷新 TeamId(目前没有调用方,保留给"只对账不动人"的场合)
	//   kFollowLeader          刷新 + 非队长跟随队长(组队服务事件、冻结解除补检查、队长扇出的成员侧)
	//   kFollowLeaderAndFanout 刷新 + 非队长跟随队长 / 队长扇出本节点成员(玩家自己进场)
	enum class FollowMode : uint8_t
	{
		kRefreshOnly = 0,
		kFollowLeader = 1,
		kFollowLeaderAndFanout = 2,
	};

	// 刷新并跟随(不扇出)。
	static void RefreshAndFollow(entt::entity player);

	// 查询签名的原入场世代SQL快照，核回执/代次/排序后ApplyMembership并按mode继续。
	static void RefreshMembership(entt::entity player, FollowMode mode);

	// 把读到的成员关系写进 TeamId 组件(按 epoch 去乱序),tid 变化时修正队友 AOI。
	// 事件驱动路径,不在 per-tick 上(AGENTS §7 #5)。
	static void ApplyMembership(entt::entity player, uint64_t teamId, uint64_t epoch);

	// 非队长跟随队长:战斗 / 会话 / battle:lock / zone / 节点守卫全部通过之后,
	// 队长在别的场景 -> 请求 SceneManager 切过去;已在同一场景 -> RegroupToLeader。
	static void CheckFollowLeader(entt::entity player, uint64_t leaderId);

	// CheckFollowLeader 的第二跳:自己的战斗锁已确认不在,读队长位置并按上面的规则决定切场景或归队。
	static void ReadLeaderLocationForFollow(entt::entity player, uint64_t playerId, uint64_t leaderId);

	// 同一场景实例内的归队:按 ShouldRegroup 判定,需要时把 player 瞬移到队长脚下。
	// 只由 CheckFollowLeader 在全部守卫通过后调用;队长实体不在本节点 / 不在同一场景 / 归属交接在途时不动。
	static void RegroupToLeader(entt::entity player, uint64_t leaderId);
};
