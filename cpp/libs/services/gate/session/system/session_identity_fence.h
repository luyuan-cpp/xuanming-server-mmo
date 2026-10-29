#pragma once

// gate 写客户端 socket 之前的**身份栅栏**(docs/design/routing-identity-audit-20260908.md
// R13 / R14)。纯函数,零依赖,故意从 gate_service_handler.cpp 里拆出来 —— 和
// node_kafka_command_filter.h 同一条理由:要测它就不该把 SessionManager、
// muduo TcpConnection、整条 RPC 注册链拖进测试工程。
//
// 为什么需要栅栏:
//   scene 只凭 session_id 反查 gate(session_id 高位嵌 gate 的 routing node_id),
//   而 routing node_id 是**立刻复用**的:gate G(node_id=3)重启后继任者 G' 仍拿 3 号,
//   scene 用旧 session_id 反查会命中 G' —— 于是 A 玩家的结算/提示/广播被写进 G' 上
//   **另一个玩家**的 socket。session_id 的低 17 位序号在单个 gate 实例内累计 131071 条
//   连接后还会回绕,所以只带 gate 实例 uuid 的代次栅栏挡不住"同一代次内的 session 复用"。
//   两种情况都只有一个共同解:把**目标玩家**带在消息里,写 socket 前和会话当前绑定的
//   玩家比对一次。
//
// 三态而不是两态:灰度窗口里老发送方不带 player_id(0),必须放行,否则升级顺序一反
// 就是全服推送静默消失。但"放行"和"校验通过"必须能被区分,否则迁移完成与否无从观测 ——
// kDeliverUnfenced 就是给这一条用的,调用方只在**首次**打一行 INFO。
// 灰度一个版本之后本枚举删掉 kDeliverUnfenced,0 直接归 kDrop。

#include <cstdint>
#include <string_view>

namespace gate_session_fence
{
	enum class PushVerdict
	{
		// 目标玩家与会话当前绑定一致:正常投递。
		kDeliver,
		// 发送方没带 player_id(老版本):放行,但调用方应记一次 INFO 供迁移观测。
		kDeliverUnfenced,
		// 目标玩家与会话当前绑定不一致:**丢弃**。这正是 node_id 复用 / session 序号回绕
		// 之后本该发生的事 —— 宁可丢一条推送,也不能把它写进别人的 socket。
		kDrop,
	};

	// sessionBoundPlayerId:gate 本地会话当前绑定的玩家(SessionInfo::playerId)。
	//   BindSession 之前是 kInvalidGuid(UINT64_MAX);会话存在但尚未绑定玩家。
	// targetPlayerId:消息里携带的目标玩家(0 = 发送方没带)。
	inline PushVerdict ClassifyPush(uint64_t sessionBoundPlayerId, uint64_t targetPlayerId)
	{
		if (targetPlayerId == 0)
		{
			return PushVerdict::kDeliverUnfenced;
		}
		if (sessionBoundPlayerId == targetPlayerId)
		{
			return PushVerdict::kDeliver;
		}
		// 会话尚未绑定玩家(kInvalidGuid / 0)同样丢弃,不留"未绑定就放行"的口子 ——
		// 那正是 R13 的失败形态本身:gate 重启后继任者把 session 5 发给一条刚连上、
		// 还没登录的新连接,而 scene 仍在用旧 session 5 推 A 的结算。放行等于把战斗
		// 结算写进一个陌生人的 socket。
		//
		// 反过来"会话未绑定却收到指名推送"在正常时序下不可能发生:scene 只会给
		// 已进场景的玩家推送,而进场景必然晚于 gate 侧 BindSession 落地。
		return PushVerdict::kDrop;
	}

	// ---------------------------------------------------------------------------------------------
	// 踢线身份栅栏(docs/design/scene-enter-chain-residuals.md §2.2 ④,余项 a)。
	//
	// login 在 ReplaceLogin 的会话记录提交之后,经 KickPlayerEvent 踢掉被替换的旧会话。旧 gate 只按 session_id 踢,
	// 两种情况会踢到不该踢的连接:session_id 序号回绕 / node_id 复用后被别的玩家复用(T3),以及同一连接上已经承载了
	// 比踢线者更新的登录(T4)。所以踢线也要先核对玩家、再比代次。
	//
	// 判定顺序:先过 ClassifyPush(玩家维度,与推送栅栏同一口径),再比登录代次。
	//
	// - 未绑定会话 fail-closed(kSkipOtherPlayer):与 ClassifyPush 的 R13 口径一致。login 按 gate 节点号显式选分区,
	//   同一 gate 的命令在同一分区内全序;被替换的链在 Bind 之后才释放玩家锁,所以正常时序下被踢会话一定已被 Bind 过,
	//   未绑定只会出现在会话号复用或失败路径上。代价:失锁导致 Bind 被拖到踢线之后时旧连接暂时不被踢,之后由 scene
	//   入场栅栏拒掉陈旧入场、推 3023 收口。
	// - 代次只用 ≥ 比较、只读:会话代次只来自已提交登录的 Bind 与路由(都经 ApplyLoginBinding / ApplyRouteIdentity
	//   取 max)。等号表示这条连接承载的正是踢线者本次登录,只会出现在同一会话上,而同一会话 login 本来就不发踢线,
	//   判「不踢」是安全方向。踢线者代次为 0(旧版 login)不比较;会话代次为 0(没见过代次)时 0 ≥ gen 不成立,照踢。
	// - 按值传参、不写会话:loginGeneration 的写入点仍只有 scene_route_helper.h 的那两个 helper(不变量 4),
	//   本判定因此放在这里而不是 scene_route_helper.h。
	enum class KickVerdict : uint8_t
	{
		// 玩家相符、会话没见过不小于踢线者的代次:推 kLoginBeKickByAnOtherAccount 后 shutdown。
		kKick,
		// 踢线事件没带 player_id(旧版 login):按 session_id 踢(旧行为),调用方首次记一行 INFO 供迁移观测。
		kKickUnfenced,
		// 会话绑定的是别的玩家,或会话尚未绑定玩家:不踢。
		kSkipOtherPlayer,
		// 会话已承载不小于踢线者代次的登录(同一连接上更新的重登):不踢。
		kSkipNewerLogin,
	};

	// sessionBoundPlayerId:gate 本地会话当前绑定的玩家(SessionInfo::playerId,未绑定为 kInvalidGuid)。
	// sessionLoginGeneration:会话上记的登录代次(SessionInfo::loginGeneration,0 = 没见过代次)。
	// targetPlayerId:踢线事件里的被踢玩家(KickPlayerEvent.player_id,0 = 旧版 login)。
	// kickerLoginGeneration:发起踢线、已提交的那条登录链的代次(KickPlayerEvent.login_generation,0 = 不比较)。
	inline KickVerdict ClassifyKick(uint64_t sessionBoundPlayerId, uint64_t sessionLoginGeneration,
	                                uint64_t targetPlayerId, uint64_t kickerLoginGeneration)
	{
		switch (ClassifyPush(sessionBoundPlayerId, targetPlayerId))
		{
		case PushVerdict::kDeliverUnfenced:
			return KickVerdict::kKickUnfenced;
		case PushVerdict::kDrop:
			return KickVerdict::kSkipOtherPlayer;
		case PushVerdict::kDeliver:
			break;
		}
		if (kickerLoginGeneration != 0 && sessionLoginGeneration >= kickerLoginGeneration)
		{
			return KickVerdict::kSkipNewerLogin;
		}
		return KickVerdict::kKick;
	}

	// 只用于日志。
	inline const char *KickVerdictName(KickVerdict verdict)
	{
		switch (verdict)
		{
		case KickVerdict::kKick:
			return "kick";
		case KickVerdict::kKickUnfenced:
			return "kick_unfenced";
		case KickVerdict::kSkipOtherPlayer:
			return "skip_other_player";
		case KickVerdict::kSkipNewerLogin:
			return "skip_newer_login";
		}
		return "unknown";
	}

	// ---------------------------------------------------------------------------------------------
	// scene 推来的踢线(KickPlayer 34)来源栅栏(docs/design/cross-zone-scene-travel.md §13.2「上限到期一律踢线」残余、
	// §12.5.6 残余 1)。
	//
	// scene 在交接收口(冻结上限 / 晚发闸 / 标记已发出之后的出口)先推 tip 再推 34,让客户端回选服重登。同 zone 交接时
	// scene_manager 可能其实已经放行、路由已把会话改绑到目标节点,只是源 scene 没收到应答 —— 源的 34 会断掉这条合法会话。
	// 会话此刻绑在哪个 scene 进程只有 gate 知道,所以由 gate 核对:34 只认会话当前绑定的那个 scene 进程。
	//
	// 只在两边都能正面认出、且确实不同时才丢:
	// - 任一侧认不出(发送连接不在 scene 注册表里 / 注册不带 uuid、会话还没绑 scene、绑定的节点实体已失效):照发,与旧行为相同。
	//   34 是让客户端脱困的出口,没有证据就丢,会让一个实体已被销毁的客户端挂在原地;
	// - 比进程实例号(NodeInfo.node_uuid),不比 node_id、也不比实体句柄:node_id 会被重启的新进程复用,
	//   实体句柄在节点重连 / 重注册后会换。
	// tip 不过这道栅栏:多显示一条「传送未成」只是提示不准,不断连接。
	enum class SceneKickSourceVerdict : uint8_t
	{
		// 发送方就是会话当前绑定的 scene 进程。
		kDeliver,
		// 任一侧认不出:照发。
		kDeliverUnverified,
		// 会话已绑定到另一个 scene 进程:丢弃这条 34。
		kDropNotBoundScene,
	};

	// boundSceneInstance:会话当前绑定的 scene 进程实例号(空 = 未绑定 / 认不出)。
	// senderSceneInstance:本次推送所在连接对应的 scene 进程实例号(空 = 认不出)。
	inline SceneKickSourceVerdict ClassifySceneKickSource(std::string_view boundSceneInstance,
	                                                       std::string_view senderSceneInstance)
	{
		if (boundSceneInstance.empty() || senderSceneInstance.empty())
		{
			return SceneKickSourceVerdict::kDeliverUnverified;
		}
		return boundSceneInstance == senderSceneInstance ? SceneKickSourceVerdict::kDeliver
		                                                  : SceneKickSourceVerdict::kDropNotBoundScene;
	}
} // namespace gate_session_fence
