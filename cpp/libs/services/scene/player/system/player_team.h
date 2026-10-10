#pragma once

// 组队场景跟随与队友 AOI(设计文档 docs/design/team-system.md §F)。
//
// 职责:scene 只是组队数据的消费方。队伍成员关系的唯一真值是 SharedRedis(team 服务,
// match 进程内,只经 Lua 原子写),scene 自己读 Redis 刷新玩家实体上的 TeamId 组件;
// Kafka 的 PlayerTeamRefreshEvent 只是"去刷新一下"的信号,不带权威数据。
//
// Redis key / value 契约(必须与 Go 侧 §C.1 投影完全一致,改一边必须同步改另一边):
//   team:player:<player_id>  hash  tid   = 十进制 team_id,无队伍为 "0"(离队后不删 key,只置 0)
//                                   epoch = 十进制 membership_epoch,仅在该玩家 tid 变化时 +1;
//                                           key 缺失后重建时用 Redis TIME 毫秒起种(必然大于旧值)
//   team:<team_id>           string TeamInfo pb(proto/common/component/team_comp.proto),
//                                   members 是集合语义,不依赖顺序
//   team:rec:<team_id>       hash  权威记录(scene 只做 EXISTS,判断投影缺失时"未知/无队")
//   player:<player_id>:location string storage::PlayerLocation pb(scene_manager 写)
//   battle:lock:<player_id>  string battle_id(scene PrepareBattle 写,咨询性战斗锁)
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

#include <charconv>
#include <system_error>

#include <hiredis/hiredis.h>

#include "entt/src/entt/entity/entity.hpp"

class PlayerTeamRefreshEvent;

// 队员离队长超过这个水平距离(服务器坐标单位)就瞬移归队。
// 客户端 5 人纵队最长约 5.6(4 个槽位 x 1.4),取 8:队形内的正常落后不触发瞬移;
// 同时小于 kMaxViewRadius(10),保证"不触发瞬移"的队员一定在队长的可见半径内。
constexpr double kTeamRegroupDistance = 8.0;

// HMGET team:player:<id> tid epoch 回复的三态判定结果。
enum class TeamIndexReplyKind : uint8_t
{
	kUnknown = 0,    // 回复为空 / 错误 / 形状不对 / 数字非法:不改组件,也不继续
	kKeyMissing = 1, // 两个字段都是 NIL:key 不存在(过期或被淘汰),与 Go 侧"索引缺失即无队"口径一致
	kPresent = 2,    // 两个字段都是合法十进制字符串(teamId 可以为 0,表示已离队但保留 epoch)
};

struct TeamIndexReply
{
	TeamIndexReplyKind kind = TeamIndexReplyKind::kUnknown;
	uint64_t teamId = 0;
	uint64_t epoch = 0;
};

class PlayerTeamSystem
{
public:
	// ---- 纯函数(无 I/O、无 ECS,头文件内联,供 cpp/tests/aoi_test 直接单测)----

	// 是否把一次读到的成员关系写进 TeamId 组件。
	//   keyMissing:无条件应用(按 tid=0 清除)。旧 epoch 已无从比较;key 重建时 epoch 用
	//               Redis TIME 起种,必然大于旧值,所以之后的正常读不会被误判为回退。
	//   没有组件:  应用(组件在 tid=0 时被移除,旧 epoch 随之丢失,无从比较)。
	//   有组件:    只有 incomingEpoch 严格大于当前 epoch 才应用;相等或更小视为重复/乱序,忽略。
	static bool ShouldApplyMembership(uint64_t currentEpoch, bool hasComponent, uint64_t incomingEpoch,
									  bool keyMissing)
	{
		if (keyMissing)
		{
			return true;
		}
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

	// 解析 HMGET team:player:<id> tid epoch 的回复。逐个元素先判 type 再读 str,
	// 绝不对 NIL 元素读 str/len。
	static TeamIndexReply ParseTeamIndexReply(const redisReply* reply)
	{
		TeamIndexReply result;
		if (reply == nullptr || reply->type != REDIS_REPLY_ARRAY || reply->elements != 2 ||
			reply->element == nullptr)
		{
			return result;
		}
		const redisReply* tidElement = reply->element[0];
		const redisReply* epochElement = reply->element[1];
		if (tidElement == nullptr || epochElement == nullptr)
		{
			return result;
		}
		if (tidElement->type == REDIS_REPLY_NIL && epochElement->type == REDIS_REPLY_NIL)
		{
			result.kind = TeamIndexReplyKind::kKeyMissing;
			return result;
		}
		// 半个 hash(一个字段 NIL、另一个有值)不是 Go 侧 Lua 能写出的形状,按未知处理
		uint64_t teamId = 0;
		uint64_t epoch = 0;
		if (!ParseDecimalElement(tidElement, teamId) || !ParseDecimalElement(epochElement, epoch))
		{
			return result;
		}
		result.kind = TeamIndexReplyKind::kPresent;
		result.teamId = teamId;
		result.epoch = epoch;
		return result;
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

	static bool ParseDecimalElement(const redisReply* element, uint64_t& out)
	{
		if (element->type != REDIS_REPLY_STRING || element->str == nullptr || element->len == 0)
		{
			return false;
		}
		const char* begin = element->str;
		const char* end = element->str + element->len;
		const auto [ptr, ec] = std::from_chars(begin, end, out, 10);
		return ec == std::errc{} && ptr == end;
	}

	// 刷新并跟随(不扇出)。
	static void RefreshAndFollow(entt::entity player);

	// HMGET team:player:<id> tid epoch -> 三态判定 -> ApplyMembership -> 按 mode 继续。
	static void RefreshMembership(entt::entity player, FollowMode mode);

	// 把读到的成员关系写进 TeamId 组件(按 epoch 去乱序),tid 变化时修正队友 AOI。
	// 事件驱动路径,不在 per-tick 上(AGENTS §7 #5)。
	static void ApplyMembership(entt::entity player, uint64_t teamId, uint64_t epoch, bool keyMissing);

	// GET team:<tid> 读投影;缺失时 EXISTS team:rec:<tid> 区分"未知 / 无队"。
	static void LoadTeamInfo(entt::entity player, uint64_t teamId, FollowMode mode);

	// 非队长跟随队长:战斗 / 会话 / battle:lock / zone / 节点守卫全部通过之后,
	// 队长在别的场景 -> 请求 SceneManager 切过去;已在同一场景 -> RegroupToLeader。
	static void CheckFollowLeader(entt::entity player, uint64_t leaderId);

	// 同一场景实例内的归队:按 ShouldRegroup 判定,需要时把 player 瞬移到队长脚下。
	// 只由 CheckFollowLeader 在全部守卫通过后调用;队长实体不在本节点 / 不在同一场景 / 归属交接在途时不动。
	static void RegroupToLeader(entt::entity player, uint64_t leaderId);
};
