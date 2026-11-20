#pragma once

#include <array>
#include <atomic>
#include <cstddef>
#include <cstdint>
#include <iterator>
#include <span>
#include <string>
#include <string_view>

#include "engine/core/type_define/type_define.h" // SessionId / kInvalidSessionId
// PlayerLoginIdentityComp(实体上已放行的最高登录身份):定义在引擎层,推送也按它记下的 gate 实例找 gate。
#include "engine/core/type_define/player_login_identity_comp.h"
// IsBoundSession:会话号是否指向一个会话(0 与 kInvalidSessionId 都不算)。写全路径:本头经 player_lifecycle.h
// 被 modules 库包含,那个工程的包含目录里只有 cpp/libs。
#include "services/scene/player/system/player_exit_intent.h"

// 入场会话栅栏:登录代次(login_generation)。设计:docs/design/scene-enter-session-fence.md §5。
//
// 为什么需要:同一角色两台设备经两个 gate 入场、落在同一个 scene 节点时,两条 gate→scene 连接之间没有统一的 FIFO,
// 先登录(已被踢)那一方的 PlayerEnterGameNode 可能晚于顶号方到达。修复前入口无条件擦掉当前会话、把会话绑回旧会话,
// 随后旧连接上紧跟的 ExitGame(旧会话)就把实体退出销毁 —— 新会话在 gate 与 player_locator 上都在线,scene 上没有实体。
// owner_epoch 只在持有节点换了时铸造,同节点顶号两次路由的 epoch 相同,区分不了;只能比"第几次登录"。
//
// 本头只放组件、纯判定与计数,不含 entt / redis / muduo / protobuf,不读任何时钟(先例:player_exit_intent.h)。
// 粘合代码在 player_lifecycle.cpp(PlayerLifecycleSystem::HandleEnterGameNode / JudgeEnter / EnterScene 的绑定复核 /
// RecordLoginIdentity / IsStaleGateInstance),以及 scene_handler.cpp 的 PlayerEnterGameNode(读入站连接的 gate 实例)、
// ProcessClientPlayerMessage(跨实例的客户端消息不派发)与 UpdateSessionDetail。单测:cpp/tests/cross_zone_test 的 EnterFence*、
// GateInstanceFence*。
// 另有一个只读用途:ActingLoginGeneration 给发往 scene_manager 的请求盖章(scene-enter-chain-residuals.md §5.2 ④),
// 单测 ActingLoginGeneration*。
//
// 不变量(改任何一条先重过设计 §3.1 与 §5):
//   * 只比较"随请求携带的代次"与"本节点已放行的最高代次",不与铸造键、会话记录做相等校验(G3 / G4)。
//   * 同一会话一律放行。同一会话 = 同一 (gate 实例, session_id):会话号相同,且两边的 gate 实例都已知时实例也相同
//     (SameGateInstance)。会话号由 gate 的 node_id 派生,同一 node_id 短暂有两个 gate 进程时同号可以出自两个进程;
//     gate 实例取自请求所走入站连接的握手身份(RpcController::HandshakePeerUuid),不取消息体自报的值。这样"同一会话只经
//     同一条 gate→scene 连接到达,FIFO 之下到达序就是 gate 的决策序"由定义保证(scene-enter-chain-residuals.md §14)。
//   * 栅栏只读、零副作用,是入口第一步:被拒的请求不碰当前会话、待入场上下文、改派票据与待确认条目。连接身份不是栅栏之前的
//     另一道判定,而是请求身份的一部分,在组请求上下文时读出(与读请求字段同类)。
//   * 登录代次不进存盘(G5):PlayerLoginIdentityComp 是纯运行时组件,不进 PlayerAllData、不序列化。

namespace enter_fence
{
	// 一处"当前身份"。exists = false 表示这一处没有这名玩家(不参与判定)。
	// gateInstanceId 借用来源(身份组件 / 待入场上下文 / 待确认条目)里的字符串,只在同一次只读判定里用,不存。空 = 未知。
	struct Identity
	{
		bool exists{false};
		SessionId sessionId{kInvalidSessionId};
		uint64_t loginGeneration{0};
		std::string_view gateInstanceId{};
	};

	// 两个会话号相同的会话是否出自同一个 gate 进程:两边实例都已知且不同 = 不是同一会话;任一边未知(空)不比较,按会话号认
	// (与改动前同一口径,也与 player_locator 按实例号忽略断线上报的规则一致:scene-enter-chain-residuals.md §1.4)。
	constexpr bool SameGateInstance(std::string_view current, std::string_view request)
	{
		return current.empty() || request.empty() || current == request;
	}

	// 一名玩家在本节点上最多有三处当前身份(设计 §5.1),存在的都要判,任一拒即拒。下标即顺序,也是 DecideAll 的"第一处"。
	//   kEntity        实体:PlayerLoginIdentityComp;没有该组件时退回快照会话、代次 0
	//   kPending       待入场上下文:PlayerEnterContext{enterInfo.session_id, loginGeneration}
	//   kRelocateEntry 改派待确认条目:relocate_confirm::Registration{sessionId, loginGeneration}。实体销毁之后、落回之前
	//                  只有它记着这名玩家;不纳入的话陈旧入场会把条目误结成 superseded
	// 底层类型 uint8_t 数的是来源种类数(现 3 种)。kCount 固定为最后一项,同时是名表长度。
	enum class IdentitySource : uint8_t
	{
		kEntity,
		kPending,
		kRelocateEntry,

		kCount
	};

	constexpr std::size_t kIdentitySourceCount = static_cast<std::size_t>(IdentitySource::kCount);

	inline constexpr const char *kIdentitySourceNames[] = {
		"entity",
		"pending",
		"relocate_entry",
	};
	static_assert(std::size(kIdentitySourceNames) == kIdentitySourceCount,
				  "kIdentitySourceNames 必须与 IdentitySource 一一对应");

	constexpr const char *IdentitySourceName(IdentitySource source)
	{
		const auto index = static_cast<std::size_t>(source);
		return index < kIdentitySourceCount ? kIdentitySourceNames[index] : "?";
	}

	// 判定结论(设计 §5.2 判定表)。前四项放行,后三项拒绝。名字同时是 [EnterFence] 汇总行里的 key,改名要同步运维检索。
	enum class Verdict : uint8_t
	{
		kAdmitFresh,        // 这一处(或三处都)没有身份
		kAdmitSameSession,  // 同一会话(任意代次;写身份时代次取 max),或代次相等而当前没有会话可比
		kAdmitNewer,        // 请求代次更大,或当前代次为 0 而请求带了代次
		kAdmitLegacy,       // 双方都没有代次、会话不同:沿用旧行为(后到者胜),计数;login 全量铸造后应恒为 0
		kRejectStale,       // 双方都有代次,请求更小
		kRejectConflict,    // 双方代次相等、会话不同(违反 G2 一登一号,只可能是并发冲突或数据异常)
		kRejectUnversioned, // 当前有代次、请求没有、会话不同:不给无代次请求留默认放行的窗口

		kCount
	};

	constexpr std::size_t kVerdictCount = static_cast<std::size_t>(Verdict::kCount);

	inline constexpr const char *kVerdictNames[] = {
		"admit_fresh",
		"admit_same_session",
		"admit_newer",
		"admit_legacy",
		"reject_stale",
		"reject_conflict",
		"reject_unversioned",
	};
	static_assert(std::size(kVerdictNames) == kVerdictCount, "kVerdictNames 必须与 Verdict 一一对应");

	constexpr const char *VerdictName(Verdict verdict)
	{
		const auto index = static_cast<std::size_t>(verdict);
		return index < kVerdictCount ? kVerdictNames[index] : "?";
	}

	// 越界值按拒绝处理(fail-closed)。
	constexpr bool Admits(Verdict verdict)
	{
		switch (verdict)
		{
		case Verdict::kAdmitFresh:
		case Verdict::kAdmitSameSession:
		case Verdict::kAdmitNewer:
		case Verdict::kAdmitLegacy:
			return true;
		default:
			return false;
		}
	}

	// 一处当前身份对一个入场请求的结论(设计 §5.2):
	//   不存在                              → kAdmitFresh
	//   同一会话(会话号相同且有效、gate 实例相同或有一边未知) → kAdmitSameSession(任意代次)
	//   当前代次 0:请求代次 0 / 非 0         → kAdmitLegacy / kAdmitNewer
	//   当前代次非 0:请求代次 0              → kRejectUnversioned
	//                请求更大 / 更小          → kAdmitNewer / kRejectStale
	//                相等:当前有会话 / 没有   → kRejectConflict / kAdmitSameSession
	// "同一会话"要求会话有效:0 与 kInvalidSessionId 不是会话,两边都没有会话时按代次裁决。
	// 会话号相同而 gate 实例不同(同一 node_id 上的两个 gate 进程)不是同一会话,按代次裁决:老进程迟到的入场代次更小,拒。
	// 代次相等而当前没有会话可比时按同一会话放行:G2 保证一个非 0 代次只属于一条会话。
	// requestGateInstance 缺省为空 = 未知,只给纯函数单测用;生产调用点(JudgeEnter、EnterScene 的绑定复核)一律显式传。
	constexpr Verdict Decide(const Identity &current, SessionId requestSession, uint64_t requestGeneration,
							 std::string_view requestGateInstance = {})
	{
		if (!current.exists)
		{
			return Verdict::kAdmitFresh;
		}
		const bool currentBound = player_exit::IsBoundSession(current.sessionId);
		if (currentBound && current.sessionId == requestSession &&
			SameGateInstance(current.gateInstanceId, requestGateInstance))
		{
			return Verdict::kAdmitSameSession;
		}
		if (current.loginGeneration == 0)
		{
			return requestGeneration == 0 ? Verdict::kAdmitLegacy : Verdict::kAdmitNewer;
		}
		if (requestGeneration == 0)
		{
			return Verdict::kRejectUnversioned;
		}
		if (requestGeneration > current.loginGeneration)
		{
			return Verdict::kAdmitNewer;
		}
		if (requestGeneration < current.loginGeneration)
		{
			return Verdict::kRejectStale;
		}
		return currentBound ? Verdict::kRejectConflict : Verdict::kAdmitSameSession;
	}

	// 多处身份的合并:逐个判,遇到拒即返回(返回的是第一处拒绝的结论);全放行时返回第一处**存在的**身份的结论
	// (不存在的那几处恒为 kAdmitFresh,不该盖掉真正比过的结论);一处都不存在返回 kAdmitFresh。
	constexpr Verdict DecideAll(std::span<const Identity> identities, SessionId requestSession, uint64_t requestGeneration,
								std::string_view requestGateInstance = {})
	{
		bool haveFirst = false;
		Verdict first = Verdict::kAdmitFresh;
		for (const Identity &identity : identities)
		{
			if (!identity.exists)
			{
				continue;
			}
			const Verdict verdict = Decide(identity, requestSession, requestGeneration, requestGateInstance);
			if (!Admits(verdict))
			{
				return verdict;
			}
			if (!haveFirst)
			{
				haveFirst = true;
				first = verdict;
			}
		}
		return first;
	}

	// scene 自发、发往 scene_manager 的 EnterScene(客户端换图 / 镜像自动进场 / 队伍跟随 / 交接重发 / 疏散改派)
	// 所代表的登录代次,填进 EnterSceneRequest.login_generation(12),供 scene_manager 做落点会话身份裁决
	// (docs/design/scene-enter-chain-residuals.md §5.2 ④)。
	// 只有实体有身份组件、请求会话有效且与组件记下的会话相同时才取组件的代次;否则 0(scene_manager 按 legacy 处理,
	// 不降级它已记下的身份)。代次是实体已放行的值,可以落后于会话记录与 gate 会话值(G4),下游一律只用 ≥ 比较。
	// 只读、纯函数,**只供给发往 scene_manager 的请求盖章**,不进入场判定(入场判定用 Decide / DecideAll)。
	constexpr uint64_t ActingLoginGeneration(const PlayerLoginIdentityComp *identity, SessionId requestSession)
	{
		if (identity == nullptr || !player_exit::IsBoundSession(requestSession) || identity->sessionId != requestSession)
		{
			return 0;
		}
		return identity->loginGeneration;
	}
} // namespace enter_fence

// 栅栏计数。写法同 player_lifecycle.h 的 exit_release_stats(relaxed atomic、不接 Prometheus、player_id 不做 label、
// 只在 scene 逻辑线程上写),由 RedisSystem 的 30s 快照定时器在计数有变化时打成一行 [EnterFence](进程启动以来的累计值;
// 有拒绝类计数增长时 WARN,否则 INFO)。
//
//   verdicts[]                 入口(PlayerLifecycleSystem::HandleEnterGameNode)每一次判定按结论计数,key 见
//                              enter_fence::kVerdictNames。admit_legacy 在 login 全量铸造后应恒为 0;reject_* 非 0 =
//                              确实挡下过跨会话的陈旧入场(或混版窗口里的无代次顶号,见设计 §6)
//   bind_rejected              EnterScene 绑定时的复核拒绝。入口已判过,应恒为 0;非 0 = 有绕过入口的 EnterScene 调用方
//   session_update_rejected    UpdateSessionDetail 按代次 0 过栅栏被拒(设计 §5.8)。该 RPC 没有已知发送方,应恒为 0
//   stale_gate_instance_rejected 客户端消息(含 gate 代发的 ExitGame)会话号与玩家都对得上,却出自另一个 gate 进程,不派发
//                              (按"会话不在"答复:能换节点重投的答稍后重投,其余空应答;PlayerLifecycleSystem::IsStaleGateInstance,
//                              scene-enter-chain-residuals.md §14)。只在同一 node_id 短暂有两个 gate 进程时出现,平时恒为 0
namespace enter_fence_stats
{
	struct Counters
	{
		std::array<std::atomic<uint64_t>, enter_fence::kVerdictCount> verdicts{};
		std::atomic<uint64_t> bindRejected{0};
		std::atomic<uint64_t> sessionUpdateRejected{0};
		std::atomic<uint64_t> staleGateInstanceRejected{0};
	};

	inline Counters &Get()
	{
		static Counters g_counters;
		return g_counters;
	}

	inline void Inc(std::atomic<uint64_t> &counter) { counter.fetch_add(1, std::memory_order_relaxed); }

	// 越界值不计(Decide 产不出越界值;防御写法,不读数组外)。
	inline void IncVerdict(enter_fence::Verdict verdict)
	{
		const auto index = static_cast<std::size_t>(verdict);
		if (index < enter_fence::kVerdictCount)
		{
			Inc(Get().verdicts[index]);
		}
	}

	struct Snapshot
	{
		std::array<uint64_t, enter_fence::kVerdictCount> verdicts{};
		uint64_t bindRejected{0};
		uint64_t sessionUpdateRejected{0};
		uint64_t staleGateInstanceRejected{0};

		bool operator==(const Snapshot &) const = default;

		// 拒绝类计数之和:30s 汇总行据此判断要不要升 WARN。
		uint64_t Rejected() const
		{
			uint64_t total = bindRejected + sessionUpdateRejected + staleGateInstanceRejected;
			for (std::size_t i = 0; i < verdicts.size(); ++i)
			{
				if (!enter_fence::Admits(static_cast<enter_fence::Verdict>(i)))
				{
					total += verdicts[i];
				}
			}
			return total;
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
		snapshot.bindRejected = counters.bindRejected.load(std::memory_order_relaxed);
		snapshot.sessionUpdateRejected = counters.sessionUpdateRejected.load(std::memory_order_relaxed);
		snapshot.staleGateInstanceRejected = counters.staleGateInstanceRejected.load(std::memory_order_relaxed);
		return snapshot;
	}
} // namespace enter_fence_stats
