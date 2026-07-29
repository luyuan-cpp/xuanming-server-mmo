#pragma once

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

// 一个 Agones GameServer = 一个 Scene Node 进程 = N 个动态创建的 ECS Scene 房间。
// 这个类把"进程里现在有几个 Scene"翻译成 Agones 的 Ready / Allocated 状态。
//
// 状态机:
//
//   Disabled                   本地/非 Agones 构建,全程不发 HTTP
//
//   Starting                   进程起来了,但 sidecar 可能还没就绪
//     -> POST /ready           有上限的退避重试
//
//   Ready                      当前实际 Scene 数 == 0
//     -> POST /allocate        第一个 Scene **创建之前**
//   Allocating
//   Allocated                  当前实际 Scene 数 > 0
//     -> POST /ready           最后一个 Scene 销毁**之后**
//   Ready
//
// 两条硬约束:
//  1. 不能先创建第一个 Scene 再异步 allocate。EnsureAllocatedBlocking() 必须在
//     实体创建之前返回 true,返回 false 就必须拒绝这次创建(fail-closed)。
//  2. HTTP 一律跑在 lifecycle worker 线程,绝不进 muduo EventLoop。因此
//     EnsureAllocatedBlocking() 阻塞的是**调用线程**(gRPC 线程),
//     跑在 EventLoop 上的调用方必须改用 EnsureAllocatedNonBlocking()。

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
		// 此时任何 CreateScene 都会 fail-closed,而不是假装自己 Ready 了。
		int readyMaxAttempts{ 30 };
		std::chrono::milliseconds readyInitialBackoff{ 200 };
		std::chrono::milliseconds readyMaxBackoff{ 2000 };

		// 调用方(gRPC 线程)等 allocate 的有界上限。超时即拒绝本次创建。
		std::chrono::milliseconds allocateWaitTimeout{ 3000 };
		int allocateMaxAttempts{ 3 };
		std::chrono::milliseconds allocateRetryBackoff{ 200 };
	};

	class SceneLifecycle
	{
	public:
		// CreateScene 从通过 gate 到 EventLoop 真正完成创建之间可能有排队窗口。
		// permit 把这段在途创建也纳入生命周期判断,防止最后一个旧 Scene 销毁后
		// worker 抢先把 sidecar 切回 Ready。
		class CreatePermit
		{
		public:
			CreatePermit() = default;
			~CreatePermit();

			CreatePermit(const CreatePermit&) = delete;
			CreatePermit& operator=(const CreatePermit&) = delete;
			CreatePermit(CreatePermit&& other) noexcept;
			CreatePermit& operator=(CreatePermit&&) = delete;

			explicit operator bool() const { return granted_; }

		private:
			friend class SceneLifecycle;
			CreatePermit(SceneLifecycle* owner, bool granted) : owner_(owner), granted_(granted) {}

			SceneLifecycle* owner_ = nullptr;
			bool granted_ = false;
		};

		static SceneLifecycle& Instance();

		SceneLifecycle() = default;
		~SceneLifecycle();

		SceneLifecycle(const SceneLifecycle&) = delete;
		SceneLifecycle& operator=(const SceneLifecycle&) = delete;

		// 显式进入 Disabled:不起线程、不发任何 HTTP、所有 gate 直接放行。
		void StartDisabled();

		// transport 为空时自动退化为 StartDisabled()。
		// 应该在 RPC server / etcd 注册完成之后调用(Node::SetAfterStart)。
		void Start(std::unique_ptr<HttpTransport> transport, std::string baseUrl, LifecycleOptions options);

		// 停止并 join worker。幂等。刻意**不**调用 /shutdown ——
		// 在 SIGTERM 路径里调 /shutdown 会反过来触发 Pod 删除,形成递归。
		void Stop();

		// 第一个 Scene 真正创建之前调用,阻塞调用线程直到 Allocated 或超时。
		// 只能从非 EventLoop 线程调用(当前是 gRPC 线程)。
		// Disabled 直接返回 true。
		bool EnsureAllocatedBlocking();

		// 给跑在 EventLoop 上的调用方用:不阻塞,未 Allocated 时踢一次异步
		// allocate 并返回 false,让本次创建 fail-closed、调用方重试。
		bool EnsureAllocatedNonBlocking();

		// handler 应优先使用 permit 版本。成功返回的 permit 必须活到本次
		// CreateScene 请求完成;析构时自动释放在途创建计数并触发零房间收口。
		CreatePermit AcquireCreatePermitBlocking();
		CreatePermit AcquireCreatePermitNonBlocking();

		// 实体确实创建成功 / 确实销毁之后调用。按 key 幂等:
		//   - 同一个 key 重复 created 不重复计数
		//   - 未知 key 的 destroyed 不减计数
		//   - 计数永远不会为负
		// key 用 ECS entity id:两个事件都直接带,且活跃实体之间唯一。
		void OnSceneCreated(std::uint64_t sceneKey);
		void OnSceneDestroyed(std::uint64_t sceneKey);

		// 一次 CreateScene 请求结束后调用。如果这次请求实际上没建出实体
		// (参数非法 / 幂等命中)且当前一个 Scene 都没有,就退回 Ready,
		// 避免进程停在"Allocated 但零房间"占着 Agones 容量。
		void ReconcileIdleAfterCreate();

		// 只有进程明确决定自我终止时才调用。不要挂在信号处理里。
		void RequestShutdown();

		LifecycleState State() const;
		int SceneCount() const;

		// 测试辅助:等待状态机到达目标状态。
		bool WaitForState(LifecycleState expected, std::chrono::milliseconds timeout);

	private:
		enum class WorkerAction
		{
			Health,
			Allocate,
			BackToReady,
			Shutdown,
		};

		void WorkerMain();
		void RunInitialReady();
		void DoAllocate();
		void DoBackToReady();
		void DoShutdown();
		bool EnsureAllocatedBlockingImpl(bool reserveCreate);
		bool EnsureAllocatedNonBlockingImpl(bool reserveCreate);
		void FinishCreatePermit();
		// 可被 Stop() 打断的睡眠。
		bool SleepInterruptible(std::chrono::milliseconds duration);

		mutable std::mutex mutex_;
		std::condition_variable cv_;

		LifecycleState state_ = LifecycleState::Disabled;
		std::unordered_set<std::uint64_t> activeScenes_;
		std::size_t pendingCreates_ = 0;

		bool allocateRequested_ = false;
		bool readyRequested_ = false;
		bool shutdownRequested_ = false;
		bool stopRequested_ = false;

		// 每次 allocate 出结果就 +1,让等待方能区分"还没结果"和"失败了"。
		std::uint64_t allocateResultGeneration_ = 0;

		LifecycleOptions options_{};
		std::unique_ptr<HttpTransport> transport_;
		std::unique_ptr<AgonesRestClient> client_;
		std::thread worker_;
	};

} // namespace agones
