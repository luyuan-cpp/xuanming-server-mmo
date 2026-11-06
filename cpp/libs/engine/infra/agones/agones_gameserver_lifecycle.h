#pragma once

#include <atomic>
#include <chrono>
#include <cstddef>
#include <cstdint>
#include <condition_variable>
#include <functional>
#include <memory>
#include <mutex>
#include <optional>
#include <stop_token>
#include <string>
#include <thread>
#include <unordered_map>
#include <unordered_set>
#include <utility>
#include <vector>

#include "agones/agones_rest_client.h"

namespace muduo
{
	namespace net
	{
		class EventLoop;
	} // namespace net
} // namespace muduo

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
//   (Disabled 之外)任意状态
//     -> ShuttingDown          RequestShutdown() 被 worker 取走(发 /shutdown 之前);终态,不再回到上面任何状态
//     -> Stopped               Stop() join worker 之后
//
// 两条硬约束:
//  1. 不能先创建第一个单元再异步 allocate。许可(AllocationPermit)必须在实体创建
//     之前拿到,拿不到就必须拒绝这次创建(fail-closed)。
//  2. HTTP 一律跑在 lifecycle worker 线程,绝不进 muduo EventLoop,也绝不阻塞 EventLoop。
//     EventLoop 上的调用方(scene / battle 的 gRPC 守护段)用 AcquireAllocationPermitAsync():
//     有界等待由该 loop 的定时器兑现,结果经 queueInLoop 投回该 loop;
//     RPC0 的 scene CreateScene 仍用 AcquireAllocationPermitNonBlocking()(先 fail-closed、由调用方重试)。
//     AcquireAllocationPermitBlocking() 阻塞的是**调用线程**,仅非 EventLoop 线程 / 测试使用。
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

		// sidecar 可能晚于游戏进程启动,Ready 按指数退避重试,退避封顶 readyMaxBackoff。
		// readyMaxAttempts 是快速重试阶段的次数(30 次 * 最多 2s ≈ 1 分钟),用完之后**不放弃**:打一行 ERROR,
		// 按 readyMaxBackoff 继续重试直到成功或 Stop()(docs/design/infra-reconnect-overview.md §4 R10)。
		// 没成功之前状态一直是 Starting,任何创建都会 fail-closed,而不是假装自己 Ready 了。
		int readyMaxAttempts{ 30 };
		std::chrono::milliseconds readyInitialBackoff{ 200 };
		std::chrono::milliseconds readyMaxBackoff{ 2000 };

		// 调用方等 allocate 的有界上限(阻塞版卡调用线程,异步版由调用方 loop 的定时器兑现)。超时即拒绝本次创建。
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

		// 异步许可的结果回调。permit 不可拷贝,按值移交给回调;回调不保留它时随回调返回析构。
		using AllocationPermitCallback = std::function<void(AllocationPermit permit)>;

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

		// 停止并 join worker。幂等。Stop() 自己刻意**不**发起 /shutdown ——
		// 在 SIGTERM 路径里调 /shutdown 会反过来触发 Pod 删除,形成递归。
		//
		// 停止标志置位之后不再签发任何许可(三类调用方同一口径):
		//  - 异步等待方:与置位同一临界区摘下,锁外经各自的 loop 投递拒绝(见 AcquireAllocationPermitAsync);
		//  - 阻塞等待方:醒来一律拒绝,即使 worker 已抢先写入 Allocated(在途 /allocate 恰在中止生效前成功);
		//  - 之后的 Acquire* / EnsureAllocated*:当场拒绝。
		// 因此 Stop() 之后在途许可只会减少,不会再增加。
		// Disabled(含从未 Start)没有 worker,Stop() 不置停止标志,许可照旧恒放行。
		//
		// 有界、快速返回(scene / battle 在 beforeShutdown hook 里于 loop 线程上调用,loop 冻结期间 gRPC 排空推后):
		// 置停止标志并唤醒 worker,同时经取消令牌中止 worker 在途的可放弃请求(/ready、/health、/allocate、
		// GET /gameserver);退避与重试立即退出,不再发起新的 allocate。join 只等这次中止生效:通常为毫秒级;
		// sidecar 收下请求迟迟不回时,libcurl 空闲期的进度回调约每秒一次,最坏约 1s(不超过 timeouts.total)。
		// 返回即 worker 已结束。
		//
		// 例外是挂起的 /shutdown —— 唯一不中止、必须送达的请求。Stop() 之前收下的 RequestShutdown(),
		// 不论 worker 是否已经取走,都在 worker 退出前恰好发出一次(单次尝试、不重试,失败只打 ERROR):
		// 在途就等它完成;还没取走就先中止在途的可放弃请求、再发出它。/shutdown 不带取消令牌,受 timeouts.total
		// 约束,所以这条路径上 Stop() 的耗时上界多 timeouts.total(默认 2s;最坏约 1s + timeouts.total)。
		// Stop() 开始之后的 RequestShutdown() 被忽略。
		//
		// 停机时可放弃请求不必送达:Pod 正在被删除,/ready /health /allocate 的结果都不再有意义;
		// 被中止的 /allocate 可能已被 sidecar 处理,同样随 GameServer 删除而作废。
		void Stop();

		// 第一个单元真正创建之前调用,阻塞调用线程直到 Allocated 或超时。
		// 仅非 EventLoop 线程 / 测试使用;EventLoop 上的调用方用 AcquireAllocationPermitAsync()。
		// Disabled 直接返回 true;draining 一律返回 false;Stop() 之后(含等待期间 Stop())一律返回 false。
		bool EnsureAllocatedBlocking();

		// 给跑在 EventLoop 上的调用方用:不阻塞,未 Allocated 时踢一次异步
		// allocate 并返回 false,让本次创建 fail-closed、调用方重试。draining 时不踢 allocate。
		bool EnsureAllocatedNonBlocking();

		// handler 应优先使用 permit 版本。成功返回的 permit 必须活到本次
		// 创建请求完成;析构时自动释放在途创建计数并触发零单元收口。
		// Blocking 版与 EnsureAllocatedBlocking 同一判定,仅非 EventLoop 线程 / 测试使用。
		AllocationPermit AcquireAllocationPermitBlocking();
		AllocationPermit AcquireAllocationPermitNonBlocking();

		// EventLoop 上的调用方(scene / battle 的 gRPC 守护段)用的有界异步许可。
		//  * 只能在 loop 线程调用(断言);done 非空(为空时打 ERROR 后直接返回)。
		//  * done 恰好一次,永不在本函数栈内执行:
		//    - 当场可判定的(Disabled / Allocated -> 放行;已停 / 排空 / ShuttingDown -> 拒绝)也经 loop.queueInLoop 投递;
		//    - 有结果时由 worker 线程经 queueInLoop 投递;
		//    - 等待超时由该 loop 上的定时器裁决并就地调用。
		//  * 判定谓词与 Blocking 版相同,共用私有判定函数,日志原样。放行时已 ++pendingPermits_,permit 析构即释放。
		//  * 等待期间计入 allocateWaiters_,晚到分配收口的语义不变。
		//  * 等待上限 options.allocateWaitTimeout;到点按 steady 复核,未到就重新上弦(muduo 定时器已按单调时钟到期)。
		//  * Stop() 在锁内摘下全部等待者,锁外经各自的 loop 投递拒绝;之后的调用立即(仍经投递)拒绝。
		//    Stop() 必须在等待者所属 loop 仍存活时调用:scene / battle 都在 beforeShutdown hook 里调用。
		//    析构(静态析构)只丢弃等待者,不投递。
		//  * 投递闭包没执行就随 ~EventLoop 销毁时,permit 照常析构记账,因此 lifecycle 必须活得比 loop 久
		//    (进程内是静态单例,天然满足)。
		void AcquireAllocationPermitAsync(muduo::net::EventLoop& loop, AllocationPermitCallback done);

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
		// 与 Stop() 在同一把锁上线性化:Stop() 之前收下的请求由 worker 恰好发出一次(Stop() 会等它,见 Stop());
		// Stop() 开始之后的调用被忽略(打 WARN)。Disabled 下什么都不做。
		// 幂等:已有挂起请求、或 worker 已取走(状态为 ShuttingDown)之后再调一律忽略并打 WARN,/shutdown 全程最多发一次。
		// worker 取走请求时(发 HTTP 之前)写入 ShuttingDown。ShuttingDown 是终态:不走 Stop() 时一直停在这里 ——
		// 取走之前挂起的 allocate 被丢弃,等待中的许可请求一律拒绝,之后的 Acquire* / EnsureAllocated* 当场拒绝。
		void RequestShutdown();

		// D84:由 EventLoop 线程定时调用(battle 每 1s)。无锁,任何状态下都可调。
		void TouchLoopHeartbeat();

		// D83:当前是否处于排空。未启用排空时恒为 false。
		bool IsDraining() const;

		LifecycleState State() const;
		int UnitCount() const;
		// 测试与诊断用:当前在途(已签发、尚未析构)的放行许可数。
		std::size_t PendingPermitCount() const;

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

		// 一个在 EventLoop 上等 allocate 结果的异步许可请求。只存 loop 引用,没有 T* 字段。
		struct AsyncPermitWaiter
		{
			std::reference_wrapper<muduo::net::EventLoop> loop;
			std::uint64_t startGeneration = 0;
			std::chrono::steady_clock::time_point deadline;
			AllocationPermitCallback done;
		};

		// 已裁决、待在锁外投递的一份结果。
		struct PermitDelivery
		{
			std::reference_wrapper<muduo::net::EventLoop> loop;
			bool granted = false;
			AllocationPermitCallback done;
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
		// 以下 *Locked 调用方须持有 mutex_。阻塞版与异步版共用这一组判定,日志原样。
		// 不用等待就能下结论时返回结论(放行时按 reservePermit 预占许可);否则返回空,调用方进入有界等待。
		std::optional<bool> DecideWithoutWaitLocked(bool reservePermit);
		// 有界等待的唤醒谓词:Allocated / ShuttingDown(终态)/ 有了新的 allocate 结果 / 已停 / 进入排空。
		bool WaitSatisfiedLocked(std::uint64_t startGeneration) const;
		// 等待结束后的裁决:已停 / ShuttingDown / 排空 / 非 Allocated -> false(打日志);否则按 reservePermit 预占许可并返回 true。
		// 已停优先:Stop() 之后即使状态已是 Allocated 也拒绝,与 DecideWithoutWaitLocked 同一口径。
		bool FinishWaitLocked(bool reservePermit);
		// 结算点:谓词成立的异步等待者出表、--allocateWaiters_,裁决结果追加到 out;rejectAll 时全部按拒绝出表。
		// 凡是改了谓词相关状态并 notify_all 的地方,都在同一临界区里调用它,解锁后再 DeliverPermits。
		void ResolveAsyncWaitersLocked(std::vector<PermitDelivery>& out, bool rejectAll);
		// 在 waiter 所属 loop 上为它上弦超时定时器。定时器只弱持有 aliveToken_。
		void ArmAsyncWaiterTimer(muduo::net::EventLoop& loop, std::uint64_t waiterId,
			std::chrono::steady_clock::duration delay);
		// loop 定时器到点(loop 线程):已裁决则什么都不做;steady 未到则重新上弦;否则出表并就地拒绝。
		void ExpireAsyncWaiter(std::uint64_t waiterId);
		// 只在锁外调用:逐份构造 permit,经各自 loop 的 queueInLoop 投递。
		void DeliverPermits(std::vector<PermitDelivery> deliveries);
		// Stop() = StopImpl(true);~GameServerLifecycle = StopImpl(false)(只丢弃等待者,不投递)。
		void StopImpl(bool deliverRejections);
		// worker 线程用:可放弃请求失败且停止令牌已置位,即"被 Stop() 中止或不必再管",调用方不按故障处理。
		bool AbandonedByStop(const HttpResponse& response) const;
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
		//  - allocateWaiters_:正在有界等待 allocate 结果的调用方数(EnsureAllocatedBlockingImpl 里的阻塞等待方
		//    + asyncWaiters_ 里的异步等待方)。阻塞等待方醒来后才 ++pendingPermits_,结果落地时 pendingPermits_==0
		//    不代表没人要 —— 只看 pendingPermits_ 会让 worker 抢在等待方之前把刚分配的实例送回 Ready。
		//    异步等待方在结算点(与写入状态同一临界区)就已预占许可,再减 allocateWaiters_。
		//  - nonBlockingAllocateKicked_:挂起的 allocate 请求里有非阻塞调用方(EventLoop 上的
		//    scene CreateScene)踢的那一份。它们约定先 fail-closed、稍后重试时取走 Allocated;
		//    对这样的结果做收口会让重试永远撞上 Ready,形成 allocate / ready 活锁。
		//    标记跟着挂起请求走,不跨结果残留:worker 取走 allocate 时把它转存为本次在途的快照
		//    (DoAllocate 的参数)并清零,之后成员里只剩 HTTP 在途期间新踢的;成功结果按
		//    "快照 || 成员"判定并清零成员,失败结果只丢快照(成员对应的挂起 allocate 还会再跑一次);
		//    worker 丢弃挂起 allocate(ShuttingDown / Starting / 排空 / 已 Allocated)时一并清零。
		//    残留的旧标记会让之后某次"阻塞等待方全部超时、allocate 晚到成功"被误判为有人重试而跳过收口。
		std::size_t allocateWaiters_ = 0;
		bool nonBlockingAllocateKicked_ = false;

		// 异步许可等待者(AcquireAllocationPermitAsync)。出表即裁决,每个等待者只会被摘下一次:
		// worker 结算点、loop 超时定时器、Stop() 谁先在 mutex_ 内摘下谁裁决。
		std::unordered_map<std::uint64_t, AsyncPermitWaiter> asyncWaiters_;
		std::uint64_t nextAsyncWaiterId_ = 1;
		// 超时定时器只弱持有它:lifecycle 析构后到点的定时器 lock 失败即返回,不碰已析构的对象。
		std::shared_ptr<int> aliveToken_ = std::make_shared<int>(0);

		// EventLoop 最近一次心跳(steady_clock 纪元起的纳秒数)。EventLoop 写、worker 读,
		// 用原子量避免 EventLoop 线程去抢 mutex_。
		std::atomic<std::int64_t> lastLoopHeartbeatNs_{ 0 };
		// 只由 worker 线程读写:心跳过期日志只在状态翻转时打一次。
		bool loopStaleReported_ = false;

		LifecycleOptions options_{};
		std::unique_ptr<HttpTransport> transport_;
		std::unique_ptr<AgonesRestClient> client_;
		std::thread worker_;

		// Stop() 的取消源。worker 发出的可放弃请求(/ready、/health、/allocate、GET /gameserver)都带
		// workerStopToken_,Stop() 置位即中止在途请求,让 join 有界、快速;/shutdown 刻意不带(见 DoShutdown)。
		// stop_source 单次有效,Start() 在创建 worker 之前换新并取出令牌;worker 只读 workerStopToken_,
		// 与 stopRequested_ 并存:后者是锁内谓词(等待 / 退避 / 主循环),令牌只管锁外的在途 HTTP。
		std::stop_source workerStopSource_;
		std::stop_token workerStopToken_;
	};

} // namespace agones
