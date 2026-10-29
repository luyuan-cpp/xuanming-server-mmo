#pragma once

#include <array>
#include <atomic>
#include <cstddef>
#include <cstdint>
#include <iterator>

// SessionId / IsBoundSession:会话结束释放的登录身份判定(DecideSessionEndedBinding)用。写全路径,与 enter_fence.h 同理。
#include "services/scene/player/system/player_exit_intent.h"

// scene_manager → scene 的 ReleasePlayer 归属栅栏。设计:docs/design/scene-enter-chain-residuals.md §4(余项 c)。
//
// 为什么需要:ReleasePlayer 原先只带 player_id。scene_manager 的释放有重试(首发失败按 1s / 3s 退避,最坏约 7s 才送达),
// 迟到的那一条会退掉一个仍持有归属的实体:
//   * 交接被 scene_manager 回滚(推路由失败,owner_epoch E → E+2),源节点按回执采纳 E+2 并解冻(B5)之后,迟到的释放到达;
//   * 换到别处后又换回本节点,新载入实体缓存的是更新的一代,迟到的旧释放到达。
// 两种情况下 location 与 gate 会话都还指向本节点,实体被退掉后会话变哑,只能重登。
// 修法:scene_manager 只在本次 EnterScene 提交之后才派发释放,并带上它落点 CAS 用过的观察值 expected_owner_epoch;
// 本节点只在本地缓存恰好等于它时才执行退出这种破坏性动作。
//
// 本头只放纯判定、名表与计数,不含 entt / redis / muduo / protobuf,不读任何时钟(先例:enter_fence.h)。
// 粘合代码在 player_lifecycle.cpp(PlayerLifecycleSystem::HandleReleaseFromSceneManager;会话结束释放走
// HandleSessionEndedRelease,docs/design/location-release-binding.md §3.3),gRPC 入口 scene_node_service.cpp 的
// HandleReleasePlayer 按 session_ended 委托到其中之一。汇总行在 redis.cpp([ReleaseFence])。
// 单测:cpp/tests/cross_zone_test 的 ReleaseFence* / ReleaseFenceEcs*。
//
// 不变量(改任何一条先重过设计 §4.2 与 §4.6):
//   * Decide 只比 owner_epoch(归属代际),不看会话、不看登录代次;登录代次也不进这里的判定(设计 §1.3)。
//     例外只有会话结束释放:Decide 放行之后再过一道登录身份判定 DecideSessionEndedBinding
//     (docs/design/location-release-binding.md §13)。交接释放不过这一道,两道判定互不改对方的结论。
//   * 只读本地缓存 PlayerOwnerEpochComp.epoch,不读 Redis、不采纳 epoch(CZ-3)。
//   * 判定零副作用,且在任何副作用之前做完:被忽略的请求不碰会话、待入场上下文、改派票据、待确认条目与退出意图,
//     不做 MergeExitCause,也不触发超限重踢。
//   * 「交接在途」先于一切比较:交接已发起的实体去留只由 EnterScene 应答与看门狗裁决(现有守卫,语义不变)。
//     依赖 ResolveTravelOutcome → JudgeTravelOutcomeReply 的 B5(kRolledBackToSelf)采纳「先抬 epoch、后摘交接组件」、
//     两步在同一次回调里同步完成的顺序(player_lifecycle.cpp 该处有注释钉住)。
namespace release_fence
{
	// 判定结论(设计 §4.2 判定表,按顺序判)。前三项释放,后三项忽略。名字同时是 [ReleaseFence] 汇总行里的 key,
	// 改名要同步运维检索。底层类型 uint8_t 数的是结论种类数(现 6 种)。kCount 固定为最后一项,同时是名表长度。
	enum class Verdict : uint8_t
	{
		kRelease,             // 缓存 == 期望:释放(现行为)
		kReleaseUnversioned,  // 期望 0(旧版 scene_manager 或尚未铸造):按旧行为释放,计数;scene_manager 全量升级后应恒为 0
		kReleaseCacheUnknown, // 缓存 0(含组件缺失):按旧行为释放,计数并 WARN。不选 fail-closed:缓存为 0 的实体存盘不带
		                      // guard,留着它反而可能用旧状态覆盖 Redis 与 MySQL
		kSkipHandoffInFlight, // 交接已发起:忽略,去留由 EnterScene 应答与看门狗裁决(现有守卫,语义不变)
		kSkipStaleRelease,    // 缓存 > 期望:实体已采纳回滚(E+2)或已在更新的一代重新载入,这是一条迟到的旧释放
		kSkipAheadOfHolder,   // 缓存 < 期望(两者都非 0):生产上构造不出(标记 epoch 就是缓存值,缓存只增不减),只在
		                      // dev 旁路或键异常时出现。退出是破坏性动作,要求严格相等;这样的陈旧实体下一次带 guard 的存盘
		                      // 会被 CAS 拒掉、按废黜流程自毁,不会写坏数据

		kCount
	};

	constexpr std::size_t kVerdictCount = static_cast<std::size_t>(Verdict::kCount);

	inline constexpr const char *kVerdictNames[] = {
		"release",
		"release_unversioned",
		"release_cache_unknown",
		"skip_handoff_in_flight",
		"skip_stale_release",
		"skip_ahead_of_holder",
	};
	static_assert(std::size(kVerdictNames) == kVerdictCount, "kVerdictNames 必须与 Verdict 一一对应");

	constexpr const char *VerdictName(Verdict verdict)
	{
		const auto index = static_cast<std::size_t>(verdict);
		return index < kVerdictCount ? kVerdictNames[index] : "?";
	}

	// 是否执行退出。越界值按不释放处理(fail-closed:退出是破坏性动作)。
	constexpr bool Releases(Verdict verdict)
	{
		switch (verdict)
		{
		case Verdict::kRelease:
		case Verdict::kReleaseUnversioned:
		case Verdict::kReleaseCacheUnknown:
			return true;
		default:
			return false;
		}
	}

	// 出现即值得人看一眼的结论:30s 汇总行在这几项有增长时升 WARN(设计 §4.4 第 4 条)。
	// skip_handoff_in_flight 是生产常态(成功交接时释放到达,源实体多半仍在交接中),release_unversioned 是滚动窗口常态,都不升级。
	constexpr bool Alarming(Verdict verdict)
	{
		switch (verdict)
		{
		case Verdict::kReleaseCacheUnknown:
		case Verdict::kSkipStaleRelease:
		case Verdict::kSkipAheadOfHolder:
			return true;
		default:
			return false;
		}
	}

	// 一条 ReleasePlayer 对本地实体的结论(设计 §4.2,按顺序判):
	//   handoffRequested                  → kSkipHandoffInFlight
	//   expectedOwnerEpoch == 0           → kReleaseUnversioned
	//   cachedOwnerEpoch == 0             → kReleaseCacheUnknown
	//   cached == expected                → kRelease
	//   cached >  expected                → kSkipStaleRelease
	//   cached <  expected                → kSkipAheadOfHolder
	// expectedOwnerEpoch 是 scene_manager 本次 EnterScene 观察到、并用于落点 CAS 的值(ReleasePlayerRequest.expected_owner_epoch);
	// cachedOwnerEpoch 是本地 PlayerOwnerEpochComp.epoch,组件缺失按 0 传入;handoffRequested 即 IsHandoffRequested。
	constexpr Verdict Decide(uint64_t expectedOwnerEpoch, uint64_t cachedOwnerEpoch, bool handoffRequested)
	{
		if (handoffRequested)
		{
			return Verdict::kSkipHandoffInFlight;
		}
		if (expectedOwnerEpoch == 0)
		{
			return Verdict::kReleaseUnversioned;
		}
		if (cachedOwnerEpoch == 0)
		{
			return Verdict::kReleaseCacheUnknown;
		}
		if (cachedOwnerEpoch == expectedOwnerEpoch)
		{
			return Verdict::kRelease;
		}
		return cachedOwnerEpoch > expectedOwnerEpoch ? Verdict::kSkipStaleRelease : Verdict::kSkipAheadOfHolder;
	}

	// ── 会话结束释放(ReleasePlayerRequest.session_ended,docs/design/location-release-binding.md §3.3)──────────────
	//
	// scene_manager 的 LeaveScene 拿不到"持有者已释放"证据时请 location 的持有节点释放。本节点有该玩家实体时,判定与交接释放
	// 共用上面的 Decide(释放 = 按客户端断线退出,存盘落地后由 A1′ 写标记);本节点**完全不持有**时,按下面的判定决定要不要按
	// owner_epoch == expected 条件补写一枚凭证标记,让 scene_manager 的下一次重试拿到证据删 location。
	// 安全性与"改派踢线前的凭证补写"(relocate_confirm::DecideCredential)同理:本节点没有该玩家的任何内存态,盘上就是最终态;
	// 条件写保证不会给更新的一代发证。
	// 2026-10-23 更正(r2-lc#2):只比 owner_epoch 挡不住同 E 的迟到写(同节点短重连重载不铸新 epoch),标记写一律改为
	// save_barrier.handoff_fence 原件的 CAS(handoff_sql_barrier.h)。完全不持有时没有缓存的原 H,所以本判定放行(kWrite)之后
	// 粘合层先 HGET 读现任资格原件,回调里按下面的 DecideCredentialQualification 核对 —— 读回时仍完全不持有、原件是本进程
	// 实例在期望 E 下的资格 —— 才以观察到的原件做 CAS(kCredentialMarkLua)。迟到的凭证帧在任何更新的登记 / 恢复 / 撤销 /
	// 发布之后都 CAS 失败,不会复活凭证。不能拒发了事:location-release-binding.md §15 起等待超时不再授权删 location,
	// 持有节点活着而不持有时,没有凭证 location 就永远删不掉。

	// 凭证判定结论。除 kWrite 外每一项都是一个"不写"的原因。名字同时是 [ReleaseFence] 汇总行里 credential_ 后缀的 key。
	// 底层类型 uint8_t 数的是结论种类数(现 6 种)。kCount 固定为最后一项,同时是名表长度。
	enum class SessionEndedCredential : uint8_t
	{
		kWrite,
		kSkipUnversioned,  // 期望 0:scene_manager 不为 epoch 0 发会话结束释放;防御写法,写了也是"0:t",换手门不认
		kSkipLocalHolder,  // 本节点有实体句柄 / 在途载入 / 改派待确认条目 / 在途入场登记 / 未落地存盘:不是"完全不持有",由那些流程自己收尾
		kSkipMarkDisabled, // A1′ 开关关闭(SCENE_EXIT_RELEASE_MARK=0):回滚期间不发任何新标记(exit_release_mark.h)
		kSkipDevBypass,    // dev 旁路开着(SCENE_DEV_UNSAFE_CROSS_NODE_HANDOFF,与 scene_manager 的旁路同步)
		kSkipIdentity,     // 本节点身份不确认(疏散中 / etcd 租约推定过期,M3):写标记可能给接手的新进程开放行口

		kCount
	};

	constexpr std::size_t kSessionEndedCredentialCount = static_cast<std::size_t>(SessionEndedCredential::kCount);

	inline constexpr const char *kSessionEndedCredentialNames[] = {
		"write",
		"skip_unversioned",
		"skip_local_holder",
		"skip_mark_disabled",
		"skip_dev_bypass",
		"skip_identity",
	};
	static_assert(std::size(kSessionEndedCredentialNames) == kSessionEndedCredentialCount,
				  "kSessionEndedCredentialNames 必须与 SessionEndedCredential 一一对应");

	constexpr const char *SessionEndedCredentialName(SessionEndedCredential decision)
	{
		const auto index = static_cast<std::size_t>(decision);
		return index < kSessionEndedCredentialCount ? kSessionEndedCredentialNames[index] : "?";
	}

	// 凭证判定的输入。默认值全部取"不写"的一侧(fail-closed):漏填任何一项都只会少写。
	struct SessionEndedFacts
	{
		uint64_t expectedOwnerEpoch{0};  // ReleasePlayerRequest.expected_owner_epoch
		// playerList 有该玩家(含无效句柄)/ 有待入场条目 / 有改派待确认条目 / RoleAdmission 的登记还在途 /
		// 有未落地的存盘(后两项 2026-10-23 起:HGET 读到的原件可能正是这条在途登记刚写的 H,不能当成已结束的资格消费)
		bool localHolderPossible{true};
		bool markFeatureEnabled{false};  // A1′ 开关(SCENE_EXIT_RELEASE_MARK)
		bool devUnsafeCrossNode{true};   // dev 旁路开关(SCENE_DEV_UNSAFE_CROSS_NODE_HANDOFF)
		bool identityConfirmed{false};   // 本节点身份当前确认有效
	};

	// 全部满足才写;第一个不满足的条件就是计数的原因。顺序只影响"计到哪个原因",不影响写不写。
	constexpr SessionEndedCredential DecideSessionEndedCredential(const SessionEndedFacts &facts)
	{
		if (facts.expectedOwnerEpoch == 0)
		{
			return SessionEndedCredential::kSkipUnversioned;
		}
		if (facts.localHolderPossible)
		{
			return SessionEndedCredential::kSkipLocalHolder;
		}
		if (!facts.markFeatureEnabled)
		{
			return SessionEndedCredential::kSkipMarkDisabled;
		}
		if (facts.devUnsafeCrossNode)
		{
			return SessionEndedCredential::kSkipDevBypass;
		}
		if (!facts.identityConfirmed)
		{
			return SessionEndedCredential::kSkipIdentity;
		}
		return SessionEndedCredential::kWrite;
	}

	// ── 凭证的资格核对(2026-10-23,r2-lc#2 之后;location-release-binding.md §3.3)───────────────────────────────
	//
	// DecideSessionEndedCredential 判 kWrite 之后,粘合层 HGET player:{id}:save_barrier 的 handoff_fence,在 scene 线程的
	// 回调里按本判定决定能不能拿读到的原件做 CAS。为什么这样不会复活凭证:
	//   * 原件必须是本进程实例(S == 本进程 node_uuid,即 RoleAdmission 登记时写入的 S)在期望 E 下的资格。别的节点 / 别的
	//     进程 / 别的 E 的资格不归本节点处置;跨节点接手先铸 E+1,同节点新持有先登记更新的 N(都会轮换原件)。
	//   * 读回时重采 SessionEndedFacts、仍须判 kWrite:原件若属于本节点一条正在建立的持有(登记在途 / 待入场 / 实体),
	//     这一刻它一定还在内存里(登记回包之前 RoleAdmission 在途,之后 Activate 同步建待入场条目),被这一步挡下。
	//   * CAS 发出之后才开始的任何持有都会先轮换原件,迟到的凭证帧只能回 -2,不依赖连接顺序与墙钟。
	// 原件状态 H / F / R 都认(未写标记就销毁 / 已发布或已释放而标记已过期或被消费 / 登记被撤销),只说明这一代资格怎么结束。
	//
	// 底层类型 uint8_t 数的是结论种类数(现 5 种)。名字同时是 [ReleaseFence] 汇总行里 credential_qualification_ 后缀的 key。
	// kCount 固定为最后一项,同时是名表长度。只有 kQualified 发 CAS;其余都不写。
	enum class CredentialQualification : uint8_t
	{
		kQualified,       // 原件是本进程实例在期望 E 下的资格,读回时仍完全不持有:以原件 CAS
		kReadFailed,      // HGET 没有可用应答(Redis 不可用 / 发不出去 / 空应答 / ERROR / 类型不对):结果未知,不写
		kHolderAppeared,  // 读回时凭证判定已不是 write(本节点又有了该玩家的内存态 / 身份不再确认 / 开关变化):交给那条流程
		kNoQualification, // 没有原件,或原件不是本进程实例在期望 E 下登记的:没有可消费的资格
		kMalformed,       // 原件写坏:fail-closed,不写

		kCount
	};

	constexpr std::size_t kCredentialQualificationCount = static_cast<std::size_t>(CredentialQualification::kCount);

	inline constexpr const char *kCredentialQualificationNames[] = {
		"qualified",
		"read_failed",
		"holder_appeared",
		"no_qualification",
		"malformed",
	};
	static_assert(std::size(kCredentialQualificationNames) == kCredentialQualificationCount,
				  "kCredentialQualificationNames 必须与 CredentialQualification 一一对应");

	constexpr const char *CredentialQualificationName(CredentialQualification qualification)
	{
		const auto index = static_cast<std::size_t>(qualification);
		return index < kCredentialQualificationCount ? kCredentialQualificationNames[index] : "?";
	}

	// 出现即值得人看一眼的结论:no_qualification 意味着 location 指着一个活着却拿不出资格的节点,这条 location 只能等持有
	// 节点死亡或玩家下次登录落回本节点重新登记(location-release-binding.md §3.3 残余)。同号重启顶替(GO-3)只认资格原件
	// 的 E 等于 location 的 E,救不了这里;malformed 是数据写坏。
	constexpr bool QualificationAlarming(CredentialQualification qualification)
	{
		return qualification == CredentialQualification::kNoQualification ||
			   qualification == CredentialQualification::kMalformed;
	}

	// 资格核对的输入,由粘合层在 HGET 回调里采集。默认值全部取"不写"的一侧(fail-closed)。
	struct CredentialQualificationFacts
	{
		bool replyUsable{false};             // HGET 应答是字符串或 nil
		bool stillWritable{false};           // 读回时重采的 SessionEndedFacts 仍判 kWrite
		bool fencePresent{false};            // handoff_fence 字段存在
		bool fenceParsed{false};             // 字段能按 E|S|N|A|K|T 解析
		bool fenceMatchesExpectedEpoch{false}; // 原件 E == ReleasePlayerRequest.expected_owner_epoch
		bool fenceOwnedByThisInstance{false};  // 原件 S == 本进程实例(RoleAdmission 登记时写入的那个值)
	};

	// 按顺序判,第一个不满足的条件就是结论。顺序只影响"计到哪个原因",不影响写不写。
	constexpr CredentialQualification DecideCredentialQualification(const CredentialQualificationFacts &facts)
	{
		if (!facts.replyUsable)
		{
			return CredentialQualification::kReadFailed;
		}
		if (!facts.stillWritable)
		{
			return CredentialQualification::kHolderAppeared;
		}
		if (!facts.fencePresent)
		{
			return CredentialQualification::kNoQualification;
		}
		if (!facts.fenceParsed)
		{
			return CredentialQualification::kMalformed;
		}
		if (!facts.fenceMatchesExpectedEpoch || !facts.fenceOwnedByThisInstance)
		{
			return CredentialQualification::kNoQualification;
		}
		return CredentialQualification::kQualified;
	}

	// ── 会话结束释放的登录身份判定(ReleasePlayerRequest.covered_*,docs/design/location-release-binding.md §13)────────
	//
	// Decide 只回答"实体是不是 expected 那一代的持有者"。同物理节点重登不铸造 owner_epoch,同一个 E 下实体可以先服务旧会话、
	// 再被新登录复用;一条迟到的会话结束释放(登录撞门的同步请求超时后才在本节点执行 / LeaveScene 的后台派发晚于新登录到达)
	// 只比 E 就会把刚绑上新会话的实体按断线退出。所以本节点持有、Decide 判释放之后再比一次登录身份:请求带"这次释放覆盖到的
	// 最新登录身份",实体当前绑定的登录身份(PlayerLoginIdentityComp)比它新就不退出。
	//
	// 先比代次、后比会话号,与入场栅栏"同一会话一律放行"的口径**有意不同**:入场请求与 ExitGame 走同一条 gate→scene 连接,
	// FIFO 让同会话号的到达序就是 gate 的决策序;释放走 scene_manager 的 gRPC,没有这层顺序。同一连接 LeaveGame 后再登录
	// 会话号不变、代次变大(scene-enter-chain-residuals.md §3.2),只比会话号会把这次新登录当成被覆盖的旧会话退掉。
	// 口径与 scene_manager 的离场身份裁决 decideLeaveIdentity 一致(代次大者新,同代异会话算冲突)。
	//
	// 底层类型 uint8_t 数的是结论种类数(现 5 种)。名字同时是 [ReleaseFence] 汇总行里 binding_ 后缀的 key。
	// kCount 固定为最后一项,同时是名表长度。前三项照常退出,后两项不退出。
	enum class SessionEndedBinding : uint8_t
	{
		kCovered,            // 实体代次 < 覆盖代次,或相等且会话相同 / 任一侧没有有效会话:实体服务的就是被覆盖的会话,退出
		kUnversionedRequest, // 覆盖代次 0(旧版 scene_manager):按现行为退出,计数;scene_manager 全量升级后应恒为 0
		kUnversionedEntity,  // 实体没有登录代次(没有 PlayerLoginIdentityComp,或旧版 login 放行的代次 0):证明不了它更新,
		                     // 按现行为退出,计数
		kSkipNewerLogin,     // 实体代次 > 覆盖代次:实体已绑上一次更新的登录,这是一条迟到的释放,不退出
		kSkipConflict,       // 代次相等、两边会话都有效且不同(违反 G2 一登一号):分不清哪一份该退,退出是破坏性动作,不退出

		kCount
	};

	constexpr std::size_t kSessionEndedBindingCount = static_cast<std::size_t>(SessionEndedBinding::kCount);

	inline constexpr const char *kSessionEndedBindingNames[] = {
		"covered",
		"unversioned_request",
		"unversioned_entity",
		"skip_newer_login",
		"skip_conflict",
	};
	static_assert(std::size(kSessionEndedBindingNames) == kSessionEndedBindingCount,
				  "kSessionEndedBindingNames 必须与 SessionEndedBinding 一一对应");

	constexpr const char *SessionEndedBindingName(SessionEndedBinding binding)
	{
		const auto index = static_cast<std::size_t>(binding);
		return index < kSessionEndedBindingCount ? kSessionEndedBindingNames[index] : "?";
	}

	// 是否照常退出。越界值按不退出处理(fail-closed:退出是破坏性动作)。
	constexpr bool BindingReleases(SessionEndedBinding binding)
	{
		switch (binding)
		{
		case SessionEndedBinding::kCovered:
		case SessionEndedBinding::kUnversionedRequest:
		case SessionEndedBinding::kUnversionedEntity:
			return true;
		default:
			return false;
		}
	}

	// 出现即值得人看一眼的结论:30s 汇总行在这几项有增长时升 WARN。skip_newer_login 就是这道判定挡下的那条迟到释放
	// (与 skip_stale_release 同性质);两项 unversioned 是滚动窗口常态,不升级。
	constexpr bool BindingAlarming(SessionEndedBinding binding)
	{
		return binding == SessionEndedBinding::kSkipNewerLogin || binding == SessionEndedBinding::kSkipConflict;
	}

	// 实体当前绑定的登录身份对一条会话结束释放的结论,按顺序判:
	//   coveredGeneration == 0                                    → kUnversionedRequest
	//   entityGeneration == 0                                     → kUnversionedEntity
	//   entityGeneration >  coveredGeneration                     → kSkipNewerLogin
	//   代次相等、两边会话都有效(IsBoundSession)且不同            → kSkipConflict
	//   其余(实体代次更小;代次相等且会话相同或任一侧没有会话)    → kCovered
	// entitySession / entityGeneration 取实体的 PlayerLoginIdentityComp(缺失按 kInvalidSessionId / 0 传入);
	// coveredSession / coveredGeneration 取 ReleasePlayerRequest.covered_session_id / covered_login_generation。
	// 会话号无效只是"没得比",不当作不同;代次相等而有一侧没有会话时按同一登录处理(G2:一个非 0 代次只属于一条会话)。
	constexpr SessionEndedBinding DecideSessionEndedBinding(SessionId entitySession, uint64_t entityGeneration,
															SessionId coveredSession, uint64_t coveredGeneration)
	{
		if (coveredGeneration == 0)
		{
			return SessionEndedBinding::kUnversionedRequest;
		}
		if (entityGeneration == 0)
		{
			return SessionEndedBinding::kUnversionedEntity;
		}
		if (entityGeneration > coveredGeneration)
		{
			return SessionEndedBinding::kSkipNewerLogin;
		}
		if (entityGeneration == coveredGeneration && player_exit::IsBoundSession(entitySession) &&
			player_exit::IsBoundSession(coveredSession) && entitySession != coveredSession)
		{
			return SessionEndedBinding::kSkipConflict;
		}
		return SessionEndedBinding::kCovered;
	}
} // namespace release_fence

// 栅栏计数。写法同 enter_fence.h 的 enter_fence_stats(relaxed atomic、不接 Prometheus、player_id 不做 label、
// 只在 scene 逻辑线程上写),由 RedisSystem 的 30s 快照定时器在计数有变化时打成一行 [ReleaseFence](进程启动以来的累计值;
// release_fence::Alarming 的几项有增长时 WARN,否则 INFO)。
//
//   verdicts[]   HandleReleaseFromSceneManager / HandleSessionEndedRelease 对本地实体的每一次判定按结论计数,
//                key 见 release_fence::kVerdictNames
//   not_found    交接释放时玩家不在本节点(幂等返回)。成功交接后源实体已销毁时就是这一项,数值大不代表异常
//   session_ended            收到的会话结束释放总数(docs/design/location-release-binding.md §3.3)。登出 / 断线后
//                            LeaveScene 拿不到释放证据时才会发,平时低速率;持续升高看 scene_manager 的 pending 计数
//   credential_<结论>        本节点完全不持有时的凭证判定分布,key 见 release_fence::kSessionEndedCredentialNames
//   credential_written / credential_epoch_moved / credential_failed  凭证 CAS(kCredentialMarkLua)的结果
//   credential_superseded    凭证 CAS 返回 -2:读到的原件在 CAS 之前已被轮换(更新的登记 / 撤销 / 发布),不写。追加在行尾
//   credential_qualification_<结论>  判 write 之后 HGET 资格原件的核对分布(2026-10-23),key 见
//                            release_fence::kCredentialQualificationNames。credential_write == 各项之和 + 在途读;
//                            qualified == written + epoch_moved + superseded + failed + 在途 CAS。no_qualification /
//                            malformed 有增长时整行升 WARN(release_fence::QualificationAlarming)
//   binding_<结论>           本节点持有、归属代际已判释放的会话结束释放再过登录身份判定的分布(§13),key 见
//                            release_fence::kSessionEndedBindingNames。会话结束释放在 verdicts[] 里记的只是代际判定:
//                            代际放行而 binding_skip_* 挡下的那几次同样记在 verdicts 的 release 系列里,实际退出次数要相减
namespace release_fence_stats
{
	struct Counters
	{
		std::array<std::atomic<uint64_t>, release_fence::kVerdictCount> verdicts{};
		std::atomic<uint64_t> notFound{0};
		std::atomic<uint64_t> sessionEnded{0};
		std::array<std::atomic<uint64_t>, release_fence::kSessionEndedCredentialCount> credentialDecisions{};
		std::atomic<uint64_t> credentialWritten{0};
		std::atomic<uint64_t> credentialEpochMoved{0};
		std::atomic<uint64_t> credentialFailed{0};
		std::array<std::atomic<uint64_t>, release_fence::kSessionEndedBindingCount> bindingDecisions{};
		std::atomic<uint64_t> credentialSuperseded{0};
		std::array<std::atomic<uint64_t>, release_fence::kCredentialQualificationCount> credentialQualifications{};
	};

	inline Counters &Get()
	{
		static Counters g_counters;
		return g_counters;
	}

	inline void Inc(std::atomic<uint64_t> &counter) { counter.fetch_add(1, std::memory_order_relaxed); }

	// 越界值不计(Decide 产不出越界值;防御写法,不读数组外)。
	inline void IncVerdict(release_fence::Verdict verdict)
	{
		const auto index = static_cast<std::size_t>(verdict);
		if (index < release_fence::kVerdictCount)
		{
			Inc(Get().verdicts[index]);
		}
	}

	// 越界值不计(DecideSessionEndedCredential 产不出越界值;防御写法)。
	inline void IncCredentialDecision(release_fence::SessionEndedCredential decision)
	{
		const auto index = static_cast<std::size_t>(decision);
		if (index < release_fence::kSessionEndedCredentialCount)
		{
			Inc(Get().credentialDecisions[index]);
		}
	}

	// 越界值不计(DecideSessionEndedBinding 产不出越界值;防御写法)。
	inline void IncBindingDecision(release_fence::SessionEndedBinding binding)
	{
		const auto index = static_cast<std::size_t>(binding);
		if (index < release_fence::kSessionEndedBindingCount)
		{
			Inc(Get().bindingDecisions[index]);
		}
	}

	// 越界值不计(DecideCredentialQualification 产不出越界值;防御写法)。
	inline void IncCredentialQualification(release_fence::CredentialQualification qualification)
	{
		const auto index = static_cast<std::size_t>(qualification);
		if (index < release_fence::kCredentialQualificationCount)
		{
			Inc(Get().credentialQualifications[index]);
		}
	}

	struct Snapshot
	{
		std::array<uint64_t, release_fence::kVerdictCount> verdicts{};
		uint64_t notFound{0};
		uint64_t sessionEnded{0};
		std::array<uint64_t, release_fence::kSessionEndedCredentialCount> credentialDecisions{};
		uint64_t credentialWritten{0};
		uint64_t credentialEpochMoved{0};
		uint64_t credentialFailed{0};
		std::array<uint64_t, release_fence::kSessionEndedBindingCount> bindingDecisions{};
		uint64_t credentialSuperseded{0};
		std::array<uint64_t, release_fence::kCredentialQualificationCount> credentialQualifications{};

		bool operator==(const Snapshot &) const = default;

		// 与上一次汇总相比,release_fence::Alarming / BindingAlarming 的几项是否有增长:30s 汇总行据此判断要不要升 WARN。
		bool AlarmingIncreaseSince(const Snapshot &previous) const
		{
			for (std::size_t i = 0; i < verdicts.size(); ++i)
			{
				if (release_fence::Alarming(static_cast<release_fence::Verdict>(i)) && verdicts[i] > previous.verdicts[i])
				{
					return true;
				}
			}
			for (std::size_t i = 0; i < bindingDecisions.size(); ++i)
			{
				if (release_fence::BindingAlarming(static_cast<release_fence::SessionEndedBinding>(i)) &&
					bindingDecisions[i] > previous.bindingDecisions[i])
				{
					return true;
				}
			}
			for (std::size_t i = 0; i < credentialQualifications.size(); ++i)
			{
				if (release_fence::QualificationAlarming(static_cast<release_fence::CredentialQualification>(i)) &&
					credentialQualifications[i] > previous.credentialQualifications[i])
				{
					return true;
				}
			}
			return false;
		}
	};

	inline Snapshot Read()
	{
		const auto &counters = Get();
		Snapshot snapshot;
		for (std::size_t i = 0; i < snapshot.verdicts.size(); ++i)
		{
			snapshot.verdicts[i] = counters.verdicts[i].load(std::memory_order_relaxed);
		}
		snapshot.notFound = counters.notFound.load(std::memory_order_relaxed);
		snapshot.sessionEnded = counters.sessionEnded.load(std::memory_order_relaxed);
		for (std::size_t i = 0; i < snapshot.credentialDecisions.size(); ++i)
		{
			snapshot.credentialDecisions[i] = counters.credentialDecisions[i].load(std::memory_order_relaxed);
		}
		snapshot.credentialWritten = counters.credentialWritten.load(std::memory_order_relaxed);
		snapshot.credentialEpochMoved = counters.credentialEpochMoved.load(std::memory_order_relaxed);
		snapshot.credentialFailed = counters.credentialFailed.load(std::memory_order_relaxed);
		for (std::size_t i = 0; i < snapshot.bindingDecisions.size(); ++i)
		{
			snapshot.bindingDecisions[i] = counters.bindingDecisions[i].load(std::memory_order_relaxed);
		}
		snapshot.credentialSuperseded = counters.credentialSuperseded.load(std::memory_order_relaxed);
		for (std::size_t i = 0; i < snapshot.credentialQualifications.size(); ++i)
		{
			snapshot.credentialQualifications[i] = counters.credentialQualifications[i].load(std::memory_order_relaxed);
		}
		return snapshot;
	}
} // namespace release_fence_stats
