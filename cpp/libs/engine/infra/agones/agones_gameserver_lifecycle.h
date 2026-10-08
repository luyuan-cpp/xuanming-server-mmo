#pragma once

#include <atomic>
#include <chrono>
#include <cstddef>
#include <cstdint>
#include <condition_variable>
#include <memory>
#include <mutex>
#include <string>
#include <thread>
#include <unordered_set>
#include <utility>

#include "agones/agones_rest_client.h"

// 一个 Agones GameServer = 一个节点进程 = N 个动态创建的"单元"。
// 单元是节点自己的业务房间:scene 节点是 ECS Scene,battle 节点是战斗房间。
// 这个类把"进程里现在有几个单元"翻译成 Agones 的 Ready / Allocated 状态。
//
// 2026-09-29 由 cpp/nodes/scene/agones/agones_scene_lifecycle 下沉并泛化(集群外入口 D85):
//   SceneLifecycle -> GameServerLifecycle,CreatePermit -> AllocationPermit,
//   OnScene* -> OnUnit*,SceneCount -> UnitCount。状态机只有这一份,scene / battle 共用。
//
// 状态机:
//
//   Disabled                   本地/非 Agones 构建,全程不发 HTTP
//
//   Starting                   进程起来了,但 sidecar 可能还没就绪
//     -> POST /ready           有上限的退避重试
//
//   Ready                      当前实际单元数 == 0
//     -> POST /allocate        第一个单元**创建之前**
//   Allocating
//   Allocated                  当前实际单元数 > 0
//     -> POST /ready           最后一个单元销毁**之后**
//   Ready
//
// 两条硬约束:
//  1. 不能先创建第一个单元再异步 allocate。许可(AllocationPermit)必须在实体创建
//     之前拿到,拿不到就必须拒绝这次创建(fail-closed)。
//  2. HTTP 一律跑在 lifecycle worker 线程,绝不进 muduo EventLoop。因此
//     AcquireAllocationPermitBlocking() 阻塞的是**调用线程**(gRPC 线程),
//     跑在 EventLoop 上的调用方必须改用 AcquireAllocationPermitNonBlocking()。
//
// 两个可选能力(默认关闭;scene 本批不开,battle 开 —— D83 / D84):
//  - 排空(drainLabelKey 非空):worker 每 drainPollInterval 调 GET /gameserver 读自己的
//    labels。标签存在(不看值)即进入 draining:一律拒绝新许可;单元归零后回 Ready,
//    由 Fleet 回收;标签被移除即解除 draining、恢复接单元。标签只能由 Fleet
//    allocationOverflow 或运维 `kubectl label` 打上(SDK SetLabel 会强加 agones.dev/sdk- 前缀),
//    进程只读。/ready 成功后、处理任何挂起的 allocate 之前,worker 先同步读一次标签。
//    读取失败(GET 失败、响应里没有元数据段、labels 形状不符)保持上一次的判定,不翻转;
//    status 段的形状问题不影响标签判定。
//  - EventLoop 心跳(requireLoopHeartbeat):EventLoop 定时调 TouchLoopHeartbeat();
//    心跳超过 loopStaleAfter 没更新,worker 就停发 /health,让 Agones 判 Unhealthy 并替换实例
//    (EventLoop 卡死时进程还活着、worker 还在发 health,Agones 看不出来)。

namespace agones
{

	enum class LifecycleState
	{
		Disabled,
		Starting,
		Ready,
		Allocating,
		Allocated,
		ReturningToReady,
		ShuttingDown,
		Stopped,
	};

	const char* ToString(LifecycleState state);

	struct LifecycleOptions
	{
		HttpTimeouts timeouts{};

		// Health 心跳间隔。Agones Fleet 模板里的 health.periodSeconds 必须比这个大。
		std::chrono::milliseconds healthInterval{ 2000 };

		// sidecar 可能晚于游戏进程启动,Ready 需要有上限的退避重试。
		// 30 次 * 最多 2s ≈ 最坏 1 分钟,超过就放弃并留在 Starting:
		// 此时任何创建都会 fail-closed,而不是假装自己 Ready 了。
		int readyMaxAttempts{ 30 };
		std::chrono::milliseconds readyInitialBackoff{ 200 };
		std::chrono::milliseconds readyMaxBackoff{ 2000 };

		// 调用方(gRPC 线程)等 allocate 的有界上限。超时即拒绝本次创建。
		// 注意它比 DoAllocate 的最坏耗时短(allocateMaxAttempts × timeouts.total + 退避,默认约 6.4s):
		// 等待方全部超时离开后 allocate 仍可能晚到成功,此时零单元、零在途许可,
		// DoAllocate 在写入 Allocated 的同一临界区里请求回 Ready(晚到分配收口),不留"Allocated 但空转"。
		// 例外:非阻塞调用方(AcquireAllocationPermitNonBlocking)踢起的分配留给它的重试来取,不收口;
		// 调用方不来重试时仍可能停在 Allocated 且零单元(沿用改动前的 scene 行为)。
		std::chrono::milliseconds allocateWaitTimeout{ 3000 };
		int allocateMaxAttempts{ 3 };
		std::chrono::milliseconds allocateRetryBackoff{ 200 };

		// D84:true 时 /health 绑 EventLoop 心跳。Start() 时心跳记为"刚更新",
		// 给 EventLoop 留 loopStaleAfter 的启动宽限。
		bool requireLoopHeartbeat{ false };
		std::chrono::milliseconds loopStaleAfter{ 10000 };

		// D83:排空标签键,为空 = 不启用(不发 GET /gameserver)。battle 传 "mmorpg.io/drain"。
		std::string drainLabelKey;
		std::chrono::milliseconds drainPollInterval{ 5000 };
	};

	class GameServerLifecycle
	{
	public:
		// 创建请求从通过许可到 EventLoop 真正完成创建之间可能有排队窗口。
		// permit 把这段在途创建也纳入生命周期判断,防止最后一个旧单元销毁后
		// worker 抢先把 sidecar 切回 Ready。
		//
		// 只能由 Acquire* 产生。持有签发它的 lifecycle 的引用(非拥有),
		// 因此 permit 不得活得比签发它的 lifecycle 久(进程内用的是静态单例,天然满足)。
		class AllocationPermit
		{
		public:
			~AllocationPermit();

			AllocationPermit(const AllocationPermit&) = delete;
			AllocationPermit& operator=(const AllocationPermit&) = delete;
			AllocationPermit(AllocationPermit&& other) noexcept;
			AllocationPermit& operator=(AllocationPermit&&) = delete;

			explicit operator bool() const { return granted_; }

		private:
			friend class GameServerLifecycle;
			AllocationPermit(GameServerLifecycle& owner, bool granted) : owner_(owner), granted_(granted) {}

			GameServerLifecycle& owner_;
			bool granted_ = false;
		};

		static GameServerLifecycle& Instance();

		GameServerLifecycle() = default;
		~GameServerLifecycle();

		GameServerLifecycle(const GameServerLifecycle&) = delete;
		GameServerLifecycle& operator=(const GameServerLifecycle&) = delete;

		// 显式进入 Disabled:不起线程、不发任何 HTTP、所有许可直接放行。
		void StartDisabled();

		// transport 为空时自动退化为 StartDisabled()。
		// 应该在 RPC server / etcd 注册完成之后调用(Node::SetAfterStart)。
		void Start(std::unique_ptr<HttpTransport> transport, std::string baseUrl, LifecycleOptions options);

		// 停止并 join worker。幂等。刻意**不**调用 /shutdown ——
		// 在 SIGTERM 路径里调 /shutdown 会反过来触发 Pod 删除,形成递归。
		void Stop();

		// 第一个单元真正创建之前调用,阻塞调用线程直到 Allocated 或超时。
		// 只能从非 EventLoop 线程调用(当前是 gRPC 线程)。
		// Disabled 直接返回 true;draining 一律返回 false。
		bool EnsureAllocatedBlocking();

		// 给跑在 EventLoop 上的调用方用:不阻塞,未 Allocated 时踢一次异步
		// allocate 并返回 false,让本次创建 fail-closed、调用方重试。draining 时不踢 allocate。
		bool EnsureAllocatedNonBlocking();

		// handler 应优先使用 permit 版本。成功返回的 permit 必须活到本次
		// 创建请求完成;析构时自动释放在途创建计数并触发零单元收口。
		AllocationPermit AcquireAllocationPermitBlocking();
		AllocationPermit AcquireAllocationPermitNonBlocking();

		// 单元确实创建成功 / 确实销毁之后调用。按 key 幂等:
		//   - 同一个 key 重复 created 不重复计数
		//   - 未知 key 的 destroyed 不减计数
		//   - 计数永远不会为负
		// key 由节点选定,只要求活跃单元之间唯一(scene 用 ECS entity id,battle 用 battle_id)。
		void OnUnitCreated(std::uint64_t unitKey);
		void OnUnitDestroyed(std::uint64_t unitKey);

		// 一次创建请求结束后调用。如果这次请求实际上没建出实体
		// (参数非法 / 幂等命中)且当前一个单元都没有,就退回 Ready,
		// 避免进程停在"Allocated 但零单元"占着 Agones 容量。
		void ReconcileIdleAfterCreate();

		// 只有进程明确决定自我终止时才调用。不要挂在信号处理里。
		void RequestShutdown();

		// D84:由 EventLoop 线程定时调用(battle 每 1s)。无锁,任何状态下都可调。
		void TouchLoopHeartbeat();

		// D83:当前是否处于排空。未启用排空时恒为 false。
		bool IsDraining() const;

		LifecycleState State() const;
		int UnitCount() const;

		// 测试辅助:等待状态机到达目标状态。
		bool WaitForState(LifecycleState expected, std::chrono::milliseconds timeout);

	private:
		enum class WorkerAction
		{
			None,
			Health,
			PollDrain,
			Allocate,
			BackToReady,
			Shutdown,
		};

		void WorkerMain();
		void RunInitialReady();
		// nonBlockingRetryExpected:worker 取走这次 allocate 时转存的 nonBlockingAllocateKicked_ 快照。
		void DoAllocate(bool nonBlockingRetryExpected);
		void DoBackToReady();
		void DoShutdown();
		void DoHealth();
		void DoPollDrain();
		bool LoopHeartbeatFresh() const;
		bool EnsureAllocatedBlockingImpl(bool reservePermit);
		bool EnsureAllocatedNonBlockingImpl(bool reservePermit);
		void FinishAllocationPermit();
		// 调用方须持有 mutex_:Allocated、零单元、无在途许可时请求回 Ready,返回是否发起了请求。
		bool RequestReadyIfIdleLocked();
		// 可被 Stop() 打断的睡眠。
		bool SleepInterruptible(std::chrono::milliseconds duration);

		mutable std::mutex mutex_;
		std::condition_variable cv_;

		LifecycleState state_ = LifecycleState::Disabled;
		std::unordered_set<std::uint64_t> activeUnits_;
		std::size_t pendingPermits_ = 0;

		bool allocateRequested_ = false;
		bool readyRequested_ = false;
		bool shutdownRequested_ = false;
		bool stopRequested_ = false;
		bool draining_ = false;

		// 每次 allocate 出结果就 +1,让等待方能区分"还没结果"和"失败了"。
		std::uint64_t allocateResultGeneration_ = 0;

		// 晚到分配收口(DoAllocate)要区分"还有人要这次结果"和"发起者都走了":
		//  - allocateWaiters_:正在 EnsureAllocatedBlockingImpl 里有界等待 allocate 结果的调用方数。
		//    它们醒来后才 ++pendingPermits_,结果落地时 pendingPermits_==0 不代表没人要 ——
		//    只看 pendingPermits_ 会让 worker 抢在等待方之前把刚分配的实例送回 Ready。
		//  - nonBlockingAllocateKicked_:挂起的 allocate 请求里有非阻塞调用方(EventLoop 上的
		//    scene CreateScene)踢的那一份。它们约定先 fail-closed、稍后重试时取走 Allocated;
		//    对这样的结果做收口会让重试永远撞上 Ready,形成 allocate / ready 活锁。
		//    标记跟着挂起请求走,不跨结果残留:worker 取走 allocate 时把它转存为本次在途的快照
		//    (DoAllocate 的参数)并清零,之后成员里只剩 HTTP 在途期间新踢的;成功结果按
		//    "快照 || 成员"判定并清零成员,失败结果只丢快照(成员对应的挂起 allocate 还会再跑一次);
		//    worker 丢弃挂起 allocate(Starting / 排空 / 已 Allocated)时一并清零。
		//    残留的旧标记会让之后某次"阻塞等待方全部超时、allocate 晚到成功"被误判为有人重试而跳过收口。
		std::size_t allocateWaiters_ = 0;
		bool nonBlockingAllocateKicked_ = false;

		// EventLoop 最近一次心跳(steady_clock 纪元起的纳秒数)。EventLoop 写、worker 读,
		// 用原子量避免 EventLoop 线程去抢 mutex_。
		std::atomic<std::int64_t> lastLoopHeartbeatNs_{ 0 };
		// 只由 worker 线程读写:心跳过期日志只在状态翻转时打一次。
		bool loopStaleReported_ = false;

		LifecycleOptions options_{};
		std::unique_ptr<HttpTransport> transport_;
		std::unique_ptr<AgonesRestClient> client_;
		std::thread worker_;
	};

} // namespace agones
