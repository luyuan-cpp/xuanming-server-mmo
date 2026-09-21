#pragma once

#include <cstddef>
#include <cstdint>
#include <iterator>

#include "engine/core/type_define/type_define.h" // SessionId / kInvalidSessionId

// 玩家退出意图 —— 与 UnregisterPlayer(proto 标记,只带 logout_initiated_ms)成对挂、成对摘的纯运行时组件。
//
// 设计:docs/design/cross-zone-scene-travel.md §12.6.3 第一步(Z1 修复)与 §12.6.4 M1 / M4 / M9 / M15。
//
// 为什么需要它:退出存盘落地时(PlayerLifecycleSystem::HandlePlayerAsyncSaved 的退出分支)要回答
// "这个退出还算不算数"。旧实现拿 PlayerSessionSnapshotComp 里的会话去 SessionMap 里查,查到就判成
// "重连已取代退出" —— 可 HandleExitGameNode 从不解绑会话,查到的恰恰是**退出会话本身**,于是每一次
// 真写盘的正常断线都被判成"被取代",实体成了僵尸(Z1)。判据必须是"当前会话 ≠ 退出那一刻的会话",
// 而退出那一刻的会话只有在退出发起时抄下来才拿得到,这就是 sessionAtExit。
//
// 纯内存:不进 PlayerAllData、不序列化、不跨进程(与 player_ownership_comp.h 的三件套同性质)。
// 放在 player/system/ 而不是 player/comp/:它连带退出判定的纯函数(player_exit::*),不只是一个组件。
//
// 成对约定(改任何一处都要同步另外几处):
//   挂:只在 PlayerLifecycleSystem::HandleExitGameNode 里,紧跟 UnregisterPlayer 之后;
//   摘:EnterScene 第 0 步(PlayerLifecycleSystem::CancelExitOnReconnect,重连取消退出)、
//       HandlePlayerAsyncSaved 退出分支的"被取代"与"意图缺失"两条 fail-closed 出口 —— 都与 UnregisterPlayer
//       一起摘;实体销毁时随实体消失。
//   已在退出中再来一次退出:不重挂,按 player_exit::MergeExitCause 合并原因。

// 退出原因。A1′(断线释放标记)按它判定能不能写(exit_release_mark.h 的 DecideExitReleaseMark,
// §12.6.3 第二步)。默认值 kUnspecified = 来源不明 = 不写标记:漏标只会少写。
//
// 底层类型 uint8_t 数的是原因种类数(现 6 种),与会话号 / 玩家 id 的位宽无关(AGENTS §11.2)。
// kCount 固定为最后一项,同时是名表长度。
enum class ExitCause : uint8_t
{
	kUnspecified,         // 来源不明(s2s LeaveScene 等未标注的调用点)
	kClientDisconnect,    // gate 断线 / 客户端主动退出(ExitGame)
	kNodeShutdown,        // 本节点停机(main.cpp 的停机存盘与 drain 补发)
	kSceneDrain,          // 单场景排空(频道缩容,BeginSceneDrain)
	kIdentityConflict,    // 节点身份失效后的整节点疏散(BeginEmergencyRelocateAll)
	kReleasedByTransfer,  // scene_manager 通知释放(ReleasePlayer:玩家要去别的节点)

	kCount
};

struct PlayerExitIntentComp
{
	// 退出发起那一刻实体绑定的 gate 会话;当时没有会话则为 kInvalidSessionId。
	SessionId sessionAtExit{kInvalidSessionId};
	// 第一次发起退出时的原因。之后再来的退出不改它,只可能置位 releaseMarkSuppressed(见 MergeExitCause)。
	ExitCause cause{ExitCause::kUnspecified};
	// 粘性:一旦置位不再清。A1′ 据此不写断线释放标记(exit_release_mark::DecideExitReleaseMark)。
	bool releaseMarkSuppressed{false};
	// 第一次触发压制的原因(kCount = 未压制),只用于把"不写"的计数按 release / identity / unspecified 拆开。
	ExitCause suppressedBy{ExitCause::kCount};
	// 退出分支"落地内容与当前内存不一致 → 更新快照后重存"已经做过的轮次(M1),上限见
	// player_exit::kMaxExitResaveRounds。只在退出分支里读写。
	uint8_t resaveRounds{0};
	// 退出优先于一次**已写过 handoff 标记**的交接(HandlePlayerAsyncSaved 的"退出优先"分支摘交接组件时置位):
	// 那次交接的 EnterScene 可能还在 scene_manager 排队。此时 A1′ 不写断线释放标记(M11,计
	// skip_handoff_inflight)—— 新标记会被在途的 EnterScene 用掉,放行到一个会话已断的目标节点。
	// FinishExitAfterPersist 自己的"退出优先"分支用局部变量判,不经这里。
	bool travelHandoffMarkIssued{false};
	// 本次退出已打过一次 "save outran reconnect lease" WARN。退出分支现在一次退出可能落地多次(重存轮次、
	// 超限保留后的周期存盘 / rekick),该 WARN 被帮会值班 LogQL 按"事件数"引用(§12.6.9),只许每次退出打一条。
	bool leaseOverrunWarned{false};
};

namespace player_exit
{
	constexpr std::size_t kExitCauseCount = static_cast<std::size_t>(ExitCause::kCount);

	// 只给日志用的名字,与枚举一一对应。数组长度由初始化项推导,漏加 / 多加一行都会被 static_assert 拦下。
	inline constexpr const char *kExitCauseNames[] = {
		"unspecified",
		"client_disconnect",
		"node_shutdown",
		"scene_drain",
		"identity_conflict",
		"released_by_transfer",
	};
	static_assert(std::size(kExitCauseNames) == kExitCauseCount, "kExitCauseNames 必须与 ExitCause 一一对应");

	constexpr const char *ExitCauseName(ExitCause cause)
	{
		const auto index = static_cast<std::size_t>(cause);
		return index < kExitCauseCount ? kExitCauseNames[index] : "?";
	}

	// 退出分支"落地内容与当前内存不一致 → 重存"的轮次上限。退出中的实体已关掉客户端消息与战斗结算
	// 两个改动入口,并在退出发起时把运动学矢量清零(PlayerLifecycleSystem::StopMotionForExit,否则
	// MovementSystem 会逐帧改 Transform、Redis RTT ≥ 1 tick 时永不收敛);已知的逐帧改动源都已关掉,
	// 正常情况下一轮就收敛。超限说明还有未识别的改动源,此时 fail-closed 保留实体(不销毁、不丢差额),
	// 交之后的周期存盘 / 再一次退出请求(HandleExitGameNode 的"已在退出中"分支会补发一次存盘)/
	// 停机看门狗裁决。
	inline constexpr uint8_t kMaxExitResaveRounds = 5;

	// 会话号是否指向一个会话。0 与 kInvalidSessionId 都表示"没有会话"(进场上下文用 0,
	// PlayerSessionSnapshotComp 摘会话后写 kInvalidSessionId)。
	constexpr bool IsBoundSession(SessionId session)
	{
		return session != 0 && session != kInvalidSessionId;
	}

	// 退出是否已被一个更新的会话取代(Z1 的判据)。三个条件全部满足才算:
	//   * 实体当前绑定着一个会话;
	//   * 它不是退出那一刻的会话 —— **同一会话永远不算取代**:HandleExitGameNode 不解绑会话,
	//     退出会话本身一直留在 SessionMap 里、映射到本玩家,旧判据正是栽在这里;
	//   * 它在 SessionMap 里映射到本玩家(currentMappedToPlayer,由调用方查好传入,纯函数不碰全局表)。
	// 正常情况下重连走 EnterScene 第 0 步,UnregisterPlayer 与本组件已被摘掉,落地回调根本进不了退出分支;
	// 这里是纵深防御。
	constexpr bool IsSupersedingSession(SessionId current, SessionId atExit, bool currentMappedToPlayer)
	{
		return IsBoundSession(current) && current != atExit && currentMappedToPlayer;
	}

	// 已在退出中又来一个退出原因时,要不要(粘性地)压制断线释放标记:
	//   kIdentityConflict / kUnspecified → 压制:身份冲突期间写标记可能给接手的新进程开放行口;
	//                                     来源不明按不写处理(漏标只会少写);
	//   kReleasedByTransfer              → 只在 scene 侧 dev 旁路开关(与 scene_manager 的
	//                                     AllowUnsafeCrossNodeHandoff 同步)打开时压制;生产口径不压制,
	//                                     由 A1′ 的 owner_epoch 条件把关(§12.6.4 M6);
	//   其余                             → 不压制,保持原原因。
	// 越界值按 kUnspecified 处理(fail-closed)。
	constexpr bool ShouldSuppressReleaseMarkOnMerge(ExitCause incoming, bool devBypassSuppressesTransfer)
	{
		if (static_cast<std::size_t>(incoming) >= kExitCauseCount)
		{
			return true;
		}
		switch (incoming)
		{
		case ExitCause::kIdentityConflict:
		case ExitCause::kUnspecified:
			return true;
		case ExitCause::kReleasedByTransfer:
			return devBypassSuppressesTransfer;
		default:
			return false;
		}
	}

	// 合并一次"已在退出中"的退出请求:原因保持第一次的,只按上面的规则粘性置位 releaseMarkSuppressed,
	// 并记下第一次触发压制的原因(之后再来的压制不改它)。
	constexpr void MergeExitCause(PlayerExitIntentComp &intent, ExitCause incoming, bool devBypassSuppressesTransfer)
	{
		if (ShouldSuppressReleaseMarkOnMerge(incoming, devBypassSuppressesTransfer))
		{
			if (!intent.releaseMarkSuppressed)
			{
				intent.suppressedBy = incoming;
			}
			intent.releaseMarkSuppressed = true;
		}
	}

	// 退出存盘落地后(已排除"被取代""意图缺失")的去留:
	//   kFinish          刚落地的内容就是当前内存(payloadCurrent),且该 key 没有在途 / 排队中的存盘
	//                    (!hasUnsettledSave)→ 可以销毁;
	//   kResave          不收敛且还没到轮次上限 → 更新快照后重存,等下一次落地再判;
	//   kRetainExhausted 不收敛且已到上限 → fail-closed 保留实体(不销毁、保留 UnregisterPlayer)。
	// 收敛判定排在轮次判定之前:超限之后任何一次落地(例如周期存盘)只要收敛了,照样收尾。
	enum class AfterPersistDecision : uint8_t
	{
		kFinish,
		kResave,
		kRetainExhausted,

		kCount
	};

	constexpr AfterPersistDecision DecideAfterPersist(bool payloadCurrent, bool hasUnsettledSave,
													  uint8_t resaveRounds, uint8_t maxResaveRounds)
	{
		if (payloadCurrent && !hasUnsettledSave)
		{
			return AfterPersistDecision::kFinish;
		}
		return resaveRounds < maxResaveRounds ? AfterPersistDecision::kResave : AfterPersistDecision::kRetainExhausted;
	}

	// 已在退出中又收到一次退出请求(停机 exitAllPlayers、s2s LeaveScene、排空 / 疏散)时,要不要给
	// "重存到上限仍不收敛、被保留"的实体补发一次存盘:只在轮次已到上限、且该 key 没有在途 / 排队中的
	// 存盘时补发(有的话等它落地,由退出分支照常判定)。轮次没到上限说明退出分支自己的重存还在途,
	// 不插手。这是超限保留实体在周期存盘之外的第二个出口 —— 停机时周期存盘已取消,没有它就只剩看门狗。
	constexpr bool ShouldRekickExhaustedExit(uint8_t resaveRounds, uint8_t maxResaveRounds, bool hasUnsettledSave)
	{
		return resaveRounds >= maxResaveRounds && !hasUnsettledSave;
	}

	// 进场路由命中本节点上一个旧实体(仍在退出中的,或不在交接中的活实体)时,它是否已被别的持有者废黜过、
	// 不得复用(复用 = 旧内存成真身,下一次带新 epoch 的存盘覆盖别处的进度,§12.6.1(d) 回档)。
	// 活实体同样适用(复审 ownership-minor):中间有别的持有者时,它未落盘的改动带旧 epoch,本来也会被 CAS 拒。
	//
	// 判据:这次路由带来的 owner_epoch 比实体缓存的值**至少大 2**。推导(实体缓存 = 上一次落在本节点
	// 那次路由给的值 E,本节点离开 location 之后的每一次落点都要铸造):
	//   * 落回本节点的这一次本身至多铸一次(同节点不铸造;location 被租约到期的 LeaveScene 删掉后的
	//     首次落点铸一次)→ 恰好 E+1,中间没有别的持有者,内存仍是真身,
	//     照常复用(旧代际在途写被拒时 HandlePlayerSaveRejected 会用新 epoch 重存);
	//   * 中间有别的持有者 Z:Z 落点铸一次(E+1),再从 Z 回到本节点又铸一次(E+2)→ ≥ E+2。
	//   * incoming 为 0(上游未填)或不大于缓存值(乱序的旧路由):无从判断 / 不新,照常复用。
	//   * 缓存值为 0 同样无从判断,照常复用(与 incoming 为 0 对称,复审 liveness-minor):0 的约定语义是
	//     "未知 / 上游未填"(player_ownership_comp.h),scene_manager 对 epoch 0 的落点一律铸造(§12.6.9 ③),
	//     实体缓存 0 只会出现在新旧 gate / scene_manager 混跑的兼容窗口 —— 那时 Redis 里的真实值可能早已是 5,
	//     拿 0 当基数会把"同节点租约内重登"误判成被废黜、不存盘销毁(在途的无 guard 存盘可能晚于重载的 GET)。
	//     兼容窗口内这类实体的存盘本来就跳过 CAS 并计 owner_epoch_unknown,不在这里另开一条判据。
	// 已知盲区:dev 旁路 AllowUnsafeCrossNodeHandoff 放行不铸造,识别不了(只在开发环境打开)。
	constexpr bool IsDeposedOnReentry(uint64_t cachedEpoch, uint64_t incomingEpoch)
	{
		return cachedEpoch != 0 && incomingEpoch != 0 && incomingEpoch > cachedEpoch && incomingEpoch - cachedEpoch >= 2;
	}
} // namespace player_exit
