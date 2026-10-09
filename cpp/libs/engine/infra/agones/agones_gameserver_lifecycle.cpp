#include "agones/agones_gameserver_lifecycle.h"

#include <algorithm>
#include <map>
#include <string>
#include <utility>

#include "agones/agones_gameserver_status.h"
#include "muduo/base/Logging.h"

namespace agones
{

	namespace
	{
		std::int64_t SteadyNowNs()
		{
			return std::chrono::duration_cast<std::chrono::nanoseconds>(
				std::chrono::steady_clock::now().time_since_epoch())
				.count();
		}
	} // namespace

	const char* ToString(LifecycleState state)
	{
		switch (state)
		{
		case LifecycleState::Disabled: return "Disabled";
		case LifecycleState::Starting: return "Starting";
		case LifecycleState::Ready: return "Ready";
		case LifecycleState::Allocating: return "Allocating";
		case LifecycleState::Allocated: return "Allocated";
		case LifecycleState::ReturningToReady: return "ReturningToReady";
		case LifecycleState::ShuttingDown: return "ShuttingDown";
		case LifecycleState::Stopped: return "Stopped";
		}
		return "Unknown";
	}

	GameServerLifecycle::AllocationPermit::~AllocationPermit()
	{
		if (granted_)
		{
			owner_.FinishAllocationPermit();
		}
	}

	GameServerLifecycle::AllocationPermit::AllocationPermit(AllocationPermit&& other) noexcept
		: owner_(other.owner_), granted_(std::exchange(other.granted_, false))
	{
	}

	GameServerLifecycle& GameServerLifecycle::Instance()
	{
		static GameServerLifecycle instance;
		return instance;
	}

	GameServerLifecycle::~GameServerLifecycle()
	{
		Stop();
	}

	void GameServerLifecycle::StartDisabled()
	{
		std::lock_guard<std::mutex> lock(mutex_);
		state_ = LifecycleState::Disabled;
		draining_ = false;
		LOG_INFO << "[Agones] lifecycle disabled: no HTTP will be issued";
	}

	void GameServerLifecycle::Start(std::unique_ptr<HttpTransport> transport,
		std::string baseUrl,
		LifecycleOptions options)
	{
		if (transport == nullptr)
		{
			// Windows / 本地构建没有 curl 传输层。这不是错误,是设计上的降级。
			StartDisabled();
			return;
		}

		{
			std::lock_guard<std::mutex> lock(mutex_);
			if (worker_.joinable())
			{
				LOG_WARN << "[Agones] lifecycle already started, ignoring duplicate Start()";
				return;
			}

			options_ = std::move(options);
			transport_ = std::move(transport);
			client_ = std::make_unique<AgonesRestClient>(std::move(baseUrl), *transport_, options_.timeouts);
			state_ = LifecycleState::Starting;
			stopRequested_ = false;
			draining_ = false;
			// 心跳记为"刚更新":EventLoop 的定时 Touch 还没开始跑,给它 loopStaleAfter 的宽限。
			// worker 线程尚未创建,这里写 worker 专属的 loopStaleReported_ 没有竞争。
			lastLoopHeartbeatNs_.store(SteadyNowNs(), std::memory_order_relaxed);
			loopStaleReported_ = false;

			LOG_INFO << "[Agones] lifecycle starting: sdk=" << client_->BaseUrl()
					 << " health_interval_ms=" << options_.healthInterval.count()
					 << " ready_max_attempts=" << options_.readyMaxAttempts
					 << " require_loop_heartbeat=" << (options_.requireLoopHeartbeat ? "true" : "false")
					 << " loop_stale_after_ms=" << options_.loopStaleAfter.count()
					 << " drain_label=" << (options_.drainLabelKey.empty() ? "<disabled>" : options_.drainLabelKey);
		}

		worker_ = std::thread([this] { WorkerMain(); });
	}

	void GameServerLifecycle::Stop()
	{
		{
			std::lock_guard<std::mutex> lock(mutex_);
			if (!worker_.joinable())
			{
				return;
			}
			stopRequested_ = true;
		}
		cv_.notify_all();

		worker_.join();

		std::lock_guard<std::mutex> lock(mutex_);
		state_ = LifecycleState::Stopped;
		client_.reset();
		transport_.reset();
		LOG_INFO << "[Agones] lifecycle worker stopped and joined";
	}

	bool GameServerLifecycle::EnsureAllocatedBlocking()
	{
		return EnsureAllocatedBlockingImpl(false);
	}

	bool GameServerLifecycle::EnsureAllocatedBlockingImpl(bool reservePermit)
	{
		std::unique_lock<std::mutex> lock(mutex_);

		if (state_ == LifecycleState::Disabled)
		{
			if (reservePermit)
			{
				++pendingPermits_;
			}
			return true;
		}
		if (stopRequested_ || state_ == LifecycleState::Stopped)
		{
			LOG_WARN << "[Agones] refusing allocation permit: lifecycle already stopped";
			return false;
		}
		if (draining_)
		{
			// D83:排空中一律拒绝,即使本地还是 Allocated(已有单元照常跑完)。
			LOG_WARN << "[Agones] refusing allocation permit: draining (label " << options_.drainLabelKey << ")";
			return false;
		}
		if (state_ == LifecycleState::Allocated)
		{
			if (reservePermit)
			{
				++pendingPermits_;
				readyRequested_ = false;
			}
			return true;
		}
		if (state_ == LifecycleState::ShuttingDown)
		{
			LOG_WARN << "[Agones] refusing allocation permit: process is shutting down";
			return false;
		}

		const std::uint64_t startGeneration = allocateResultGeneration_;
		allocateRequested_ = true;
		cv_.notify_all();

		// 有界等待。超时不是"假设成功",是拒绝这次创建让调用方重试。
		// 等待期间计入 allocateWaiters_(加减都在 mutex_ 内,wait_for 返回时已重新持锁):
		// DoAllocate 据此判断结果落地时是否还有人来取;超时离开后 allocate 才晚到的,由它收口回 Ready。
		++allocateWaiters_;
		cv_.wait_for(lock, options_.allocateWaitTimeout, [this, startGeneration] {
			return state_ == LifecycleState::Allocated
				|| allocateResultGeneration_ != startGeneration
				|| stopRequested_
				|| draining_;
		});
		--allocateWaiters_;

		if (draining_)
		{
			// 等待期间进入排空:通常是 worker 在处理挂起的 allocate 之前先读到了标签(allocate 随后被
			// 丢弃);罕见时是 allocate 已成功、等待方醒来之前下一轮轮询读到了标签。两种情况都不在
			// 这里做零单元收口:DoPollDrain 置位 draining_ 与"排空且零单元即回 Ready"的复核在同一
			// 临界区内完成,走到这里时已经收口过了。
			LOG_WARN << "[Agones] refusing allocation permit: drain started while waiting for allocate";
			return false;
		}
		if (state_ != LifecycleState::Allocated)
		{
			LOG_ERROR << "[Agones] allocate not confirmed within "
					  << options_.allocateWaitTimeout.count() << "ms, state=" << ToString(state_);
			return false;
		}
		if (reservePermit)
		{
			++pendingPermits_;
			readyRequested_ = false;
		}
		return true;
	}

	bool GameServerLifecycle::EnsureAllocatedNonBlocking()
	{
		return EnsureAllocatedNonBlockingImpl(false);
	}

	bool GameServerLifecycle::EnsureAllocatedNonBlockingImpl(bool reservePermit)
	{
		std::lock_guard<std::mutex> lock(mutex_);

		if (state_ == LifecycleState::Disabled)
		{
			if (reservePermit)
			{
				++pendingPermits_;
			}
			return true;
		}
		if (stopRequested_ || state_ == LifecycleState::Stopped)
		{
			return false;
		}
		if (draining_)
		{
			// 排空中不踢 allocate:回 Ready 之后再 allocate 等于把自己又占回来。
			return false;
		}
		if (state_ == LifecycleState::Allocated)
		{
			if (reservePermit)
			{
				++pendingPermits_;
				readyRequested_ = false;
			}
			return true;
		}
		if (state_ == LifecycleState::ShuttingDown)
		{
			return false;
		}

		allocateRequested_ = true;
		// 调用方会在稍后重试时取走 Allocated,这次结果不做晚到收口(见 DoAllocate)。
		nonBlockingAllocateKicked_ = true;
		cv_.notify_all();
		return false;
	}

	GameServerLifecycle::AllocationPermit GameServerLifecycle::AcquireAllocationPermitBlocking()
	{
		const bool granted = EnsureAllocatedBlockingImpl(true);
		return AllocationPermit(*this, granted);
	}

	GameServerLifecycle::AllocationPermit GameServerLifecycle::AcquireAllocationPermitNonBlocking()
	{
		const bool granted = EnsureAllocatedNonBlockingImpl(true);
		return AllocationPermit(*this, granted);
	}

	bool GameServerLifecycle::RequestReadyIfIdleLocked()
	{
		if (state_ != LifecycleState::Allocated || !activeUnits_.empty() || pendingPermits_ != 0)
		{
			return false;
		}
		readyRequested_ = true;
		cv_.notify_all();
		return true;
	}

	void GameServerLifecycle::FinishAllocationPermit()
	{
		std::lock_guard<std::mutex> lock(mutex_);
		if (pendingPermits_ == 0)
		{
			LOG_ERROR << "[Agones] unbalanced AllocationPermit release";
			return;
		}

		--pendingPermits_;
		if (RequestReadyIfIdleLocked())
		{
			LOG_WARN << "[Agones] allocation permit finished with zero units, returning to Ready";
		}
	}

	void GameServerLifecycle::OnUnitCreated(std::uint64_t unitKey)
	{
		std::lock_guard<std::mutex> lock(mutex_);

		const auto inserted = activeUnits_.insert(unitKey);
		if (!inserted.second)
		{
			// 上游创建是按业务 id 幂等的,正常不会走到这里。
			// 真走到了说明有路径重复触发事件,记下来但不重复计数。
			LOG_WARN << "[Agones] duplicate OnUnitCreated for key=" << unitKey
					 << ", unit count stays " << activeUnits_.size();
			return;
		}

		LOG_INFO << "[Agones] unit created key=" << unitKey
				 << " unit_count=" << activeUnits_.size() << " state=" << ToString(state_);
	}

	void GameServerLifecycle::OnUnitDestroyed(std::uint64_t unitKey)
	{
		std::lock_guard<std::mutex> lock(mutex_);

		if (activeUnits_.erase(unitKey) == 0)
		{
			// 销毁一个没记过账的单元:不减计数,更不能让计数变负。
			LOG_WARN << "[Agones] OnUnitDestroyed for unknown key=" << unitKey
					 << ", unit count stays " << activeUnits_.size();
			return;
		}

		LOG_INFO << "[Agones] unit destroyed key=" << unitKey
				 << " unit_count=" << activeUnits_.size() << " state=" << ToString(state_);

		if (!activeUnits_.empty())
		{
			return; // 还有单元,保持 Allocated
		}
		if (pendingPermits_ != 0)
		{
			LOG_INFO << "[Agones] last unit destroyed with " << pendingPermits_
					 << " allocation permit(s) still in flight; staying Allocated";
			return;
		}
		if (state_ == LifecycleState::Disabled || state_ == LifecycleState::Stopped
			|| state_ == LifecycleState::ReturningToReady
			|| state_ == LifecycleState::ShuttingDown)
		{
			return;
		}

		readyRequested_ = true;
		cv_.notify_all();
	}

	void GameServerLifecycle::ReconcileIdleAfterCreate()
	{
		std::lock_guard<std::mutex> lock(mutex_);

		// allocate 成功了但实体没建出来(参数非法 / 幂等命中且本节点无单元)。
		// 不退回 Ready 的话,这个进程会一直占着 Allocated 却零单元。
		if (RequestReadyIfIdleLocked())
		{
			LOG_WARN << "[Agones] allocated but no unit was created, returning to Ready";
		}
	}

	void GameServerLifecycle::RequestShutdown()
	{
		{
			std::lock_guard<std::mutex> lock(mutex_);
			if (state_ == LifecycleState::Disabled || state_ == LifecycleState::Stopped)
			{
				return;
			}
			shutdownRequested_ = true;
		}
		cv_.notify_all();
	}

	void GameServerLifecycle::TouchLoopHeartbeat()
	{
		lastLoopHeartbeatNs_.store(SteadyNowNs(), std::memory_order_relaxed);
	}

	bool GameServerLifecycle::IsDraining() const
	{
		std::lock_guard<std::mutex> lock(mutex_);
		return draining_;
	}

	LifecycleState GameServerLifecycle::State() const
	{
		std::lock_guard<std::mutex> lock(mutex_);
		return state_;
	}

	int GameServerLifecycle::UnitCount() const
	{
		std::lock_guard<std::mutex> lock(mutex_);
		return static_cast<int>(activeUnits_.size());
	}

	bool GameServerLifecycle::WaitForState(LifecycleState expected, std::chrono::milliseconds timeout)
	{
		std::unique_lock<std::mutex> lock(mutex_);
		return cv_.wait_for(lock, timeout, [this, expected] { return state_ == expected; });
	}

	bool GameServerLifecycle::SleepInterruptible(std::chrono::milliseconds duration)
	{
		std::unique_lock<std::mutex> lock(mutex_);
		cv_.wait_for(lock, duration, [this] { return stopRequested_; });
		return !stopRequested_;
	}

	bool GameServerLifecycle::LoopHeartbeatFresh() const
	{
		// options_ 只在 Start() 里、worker 线程创建之前写,worker 读它无需加锁。
		if (!options_.requireLoopHeartbeat)
		{
			return true;
		}
		const std::int64_t ageNs = SteadyNowNs() - lastLoopHeartbeatNs_.load(std::memory_order_relaxed);
		return std::chrono::nanoseconds(ageNs) <= options_.loopStaleAfter;
	}

	void GameServerLifecycle::RunInitialReady()
	{
		auto backoff = options_.readyInitialBackoff;

		for (int attempt = 1; attempt <= options_.readyMaxAttempts; ++attempt)
		{
			{
				std::lock_guard<std::mutex> lock(mutex_);
				if (stopRequested_)
				{
					return;
				}
			}

			const HttpResponse response = client_->Ready();
			if (response.Ok())
			{
				std::lock_guard<std::mutex> lock(mutex_);
				// 罕见但可能:Ready 还在重试时已经有人排队要 allocate。
				// 这里只负责把状态推到 Ready,allocate 由主循环消费挂起请求
				// (排空启用时,WorkerMain 在进入主循环前先同步读一次排空标签)。
				state_ = LifecycleState::Ready;
				LOG_INFO << "[Agones] READY confirmed after " << attempt << " attempt(s)";
				cv_.notify_all();
				return;
			}

			LOG_WARN << "[Agones] POST /ready attempt " << attempt << "/" << options_.readyMaxAttempts
					 << " failed (status=" << response.statusCode << " error=" << response.error
					 << "), retrying in " << backoff.count() << "ms";

			if (!SleepInterruptible(backoff))
			{
				return;
			}
			backoff = std::min(backoff * 2, options_.readyMaxBackoff);
		}

		// 放弃。刻意留在 Starting 而不是假装 Ready —— 这样所有创建
		// 都会 fail-closed,而不是让玩家进到一个 Agones 不认识的进程里。
		LOG_ERROR << "[Agones] gave up on POST /ready after " << options_.readyMaxAttempts
				  << " attempts; unit creation will keep failing closed";
	}

	void GameServerLifecycle::DoAllocate(bool nonBlockingRetryExpected)
	{
		bool allocated = false;

		for (int attempt = 1; attempt <= options_.allocateMaxAttempts && !allocated; ++attempt)
		{
			const HttpResponse response = client_->Allocate();
			if (response.Ok())
			{
				allocated = true;
				break;
			}

			LOG_WARN << "[Agones] POST /allocate attempt " << attempt << "/"
					 << options_.allocateMaxAttempts << " failed (status=" << response.statusCode
					 << " error=" << response.error << ")";

			if (attempt < options_.allocateMaxAttempts && !SleepInterruptible(options_.allocateRetryBackoff))
			{
				break;
			}
		}

		{
			std::lock_guard<std::mutex> lock(mutex_);
			state_ = allocated ? LifecycleState::Allocated : LifecycleState::Ready;
			++allocateResultGeneration_;
			// 这里不需要排空分支:draining_ 只可能由 worker 在 DoPollDrain 里置为 true,worker 取
			// allocate 时已判过未排空才走到这里,而 DoAllocate 同在 worker 上执行,期间 draining_ 不会变。
			if (allocated)
			{
				LOG_INFO << "[Agones] ALLOCATED";
				// 非阻塞调用方稍后会来取:触发本次 allocate 的那次踢(快照),或 HTTP 在途期间新踢的
				// (成员;它们的挂起请求下面就地清掉,由这次成功满足)。失败分支不碰成员:在途期间新踢的
				// 挂起 allocate 还会再跑一次,worker 取它时再转存为快照。
				// 先无条件清零成员再合并,不能写进 || 的右侧(短路会跳过清零,标记残留)。
				const bool kickedWhileInFlight = std::exchange(nonBlockingAllocateKicked_, false);
				const bool retryExpected = nonBlockingRetryExpected || kickedWhileInFlight;
				// HTTP 在途期间到来的调用方留下的挂起 allocate 都由这次成功满足,就地清掉:否则 worker
				// 下一轮先取到它(allocate 优先于回 Ready),在"已 Allocated"分支里把下面的收口请求冲掉。
				// 同理清掉此前残留的回 Ready 请求,保持"allocate 压过回 Ready"的原语义 —— 还在等的
				// 调用方醒来之前 pendingPermits_ 仍为 0,残留请求会抢先把刚分配的实例送回 Ready。
				// 回不回 Ready 只由下面的收口决定。
				allocateRequested_ = false;
				readyRequested_ = false;
				// 晚到分配收口:阻塞等待方的上限(allocateWaitTimeout,默认 3s)短于本函数最坏耗时
				// (默认约 6.4s)。等待方都已超时离开、也没有非阻塞调用方等着重试时,没有任何人会再来
				// 取这次分配 —— 零单元、零在途许可的 Allocated 永远不会被 OnUnitDestroyed /
				// FinishAllocationPermit 收口,实例会一直占着 Agones 容量。与写入 Allocated 在同一临界区
				// 内复核,判断依据(等待方数、单元数、在途许可)与状态是同一快照;此后新来的调用方拿许可
				// 会清掉 readyRequested_,worker 真正回 Ready 前也会再复核一次零单元零许可。
				if (allocateWaiters_ == 0 && !retryExpected && RequestReadyIfIdleLocked())
				{
					LOG_WARN << "[Agones] allocate confirmed after every waiter gave up (wait timeout "
							 << options_.allocateWaitTimeout.count()
							 << "ms), zero units and no permit in flight; returning to Ready";
				}
			}
			else
			{
				LOG_ERROR << "[Agones] allocate failed, staying Ready; unit creation will fail closed";
			}
		}
		cv_.notify_all();
	}

	void GameServerLifecycle::DoBackToReady()
	{
		const HttpResponse response = client_->Ready();

		std::lock_guard<std::mutex> lock(mutex_);
		if (!response.Ok())
		{
			// /ready 没成功,sidecar 仍是 Allocated。恢复本地状态并唤醒
			// 等待中的创建;它们无需再发一次 /allocate。
			state_ = LifecycleState::Allocated;
			LOG_ERROR << "[Agones] POST /ready (drain) failed status=" << response.statusCode
					  << " error=" << response.error << "; staying " << ToString(state_);
			cv_.notify_all();
			return;
		}
		state_ = LifecycleState::Ready;
		LOG_INFO << "[Agones] back to READY (0 units)";
		cv_.notify_all();
	}

	void GameServerLifecycle::DoShutdown()
	{
		{
			std::lock_guard<std::mutex> lock(mutex_);
			state_ = LifecycleState::ShuttingDown;
		}
		const HttpResponse response = client_->Shutdown();
		if (!response.Ok())
		{
			LOG_ERROR << "[Agones] POST /shutdown failed status=" << response.statusCode
					  << " error=" << response.error;
		}
		else
		{
			LOG_INFO << "[Agones] SHUTDOWN requested";
		}
	}

	void GameServerLifecycle::DoHealth()
	{
		if (!LoopHeartbeatFresh())
		{
			// D84:EventLoop 卡死时进程和 worker 都还活着,继续发 health 会骗过 Agones。
			// 停发之后 Agones 按 health.failureThreshold 判 Unhealthy 并替换实例。
			if (!loopStaleReported_)
			{
				loopStaleReported_ = true;
				LOG_ERROR << "[Agones] EventLoop heartbeat older than " << options_.loopStaleAfter.count()
						  << "ms; withholding /health so Agones marks this GameServer Unhealthy";
			}
			return;
		}
		if (loopStaleReported_)
		{
			loopStaleReported_ = false;
			LOG_WARN << "[Agones] EventLoop heartbeat recovered; resuming /health";
		}

		const HttpResponse response = client_->Health();
		if (!response.Ok())
		{
			LOG_WARN << "[Agones] POST /health failed status=" << response.statusCode
					 << " error=" << response.error;
		}
	}

	void GameServerLifecycle::DoPollDrain()
	{
		const HttpResponse response = client_->GameServer();
		if (!response.Ok())
		{
			// 读失败不翻转判定:sidecar 抖一下不应该让正在排空的实例重新接单元,
			// 也不应该让正常实例误入排空。
			LOG_WARN << "[Agones] GET /gameserver (drain poll) failed status=" << response.statusCode
					 << " error=" << response.error << "; keeping draining=" << (IsDraining() ? "true" : "false");
			return;
		}

		// 只解析元数据段的 labels:status 里与排空无关的字段形状漂移(如某个 ports 项缺 port)
		// 不能拦住标签判定;元数据段缺失按读取失败处理 —— 当成"标签被移除"会把正在排空的
		// 实例静默放回接单元(fail-open)。
		std::map<std::string, std::string> labels;
		std::string error;
		if (!ParseGameServerLabels(response.body, labels, error))
		{
			LOG_WARN << "[Agones] drain poll: " << error << "; keeping draining="
					 << (IsDraining() ? "true" : "false");
			return;
		}

		// 只看键是否存在,不看值:解除排空 = 删掉标签(kubectl label gs <name> <key>-)。
		const bool labelPresent = labels.count(options_.drainLabelKey) > 0;

		// draining_ 置位与下面"排空且零单元即回 Ready"的复核必须在同一临界区内:
		// 许可等待方看到 draining_ 时,零单元收口已经做过,它自己不用再收口。
		std::lock_guard<std::mutex> lock(mutex_);
		if (labelPresent != draining_)
		{
			draining_ = labelPresent;
			if (draining_)
			{
				LOG_WARN << "[Agones] drain label '" << options_.drainLabelKey
						 << "' present: refusing new units; unit_count=" << activeUnits_.size()
						 << " state=" << ToString(state_);
			}
			else
			{
				LOG_INFO << "[Agones] drain label '" << options_.drainLabelKey
						 << "' cleared: accepting new units again; state=" << ToString(state_);
			}
			// 唤醒 EnsureAllocatedBlocking 的等待方(谓词含 draining_)。
			cv_.notify_all();
		}

		// 每轮都复核一次"排空且零单元":上一次 /ready 失败留在 Allocated 时,在这里收敛。
		if (draining_ && RequestReadyIfIdleLocked())
		{
			LOG_INFO << "[Agones] draining with zero units, returning to Ready";
		}
	}

	void GameServerLifecycle::WorkerMain()
	{
		RunInitialReady();

		// 排空启用时,进入主循环之前先同步读一次标签。容器重启后 GameServer 对象上的标签还在;
		// 而 RunInitialReady 最长约 1 分钟,期间调用方挂起的 allocate 请求在主循环里优先级高于
		// 排空轮询 —— 不先读这一次,就会先 /allocate、接下单元,再发现自己在排空。
		// 这一次读取失败时按"保持上一次判定"处理(初值为未排空),由下一轮轮询纠正;不改成
		// "判定未知即拒绝许可":GET /gameserver 的形状一旦与预期不符,实例就永远接不了单元。
		const bool drainEnabled = !options_.drainLabelKey.empty();
		bool stopping = false;
		{
			std::lock_guard<std::mutex> lock(mutex_);
			stopping = stopRequested_;
		}
		if (drainEnabled && !stopping)
		{
			DoPollDrain();
		}

		// 两个周期任务按各自的截止时间调度,互不重置:挂起请求(allocate / 回 Ready)再频繁,
		// 到期的 health 也会在两次请求之间的空档里立刻执行,而不是每次唤醒都重新计时。
		using SteadyTimePoint = std::chrono::steady_clock::time_point;
		const SteadyTimePoint loopStart = std::chrono::steady_clock::now();
		SteadyTimePoint nextHealth = loopStart + options_.healthInterval;
		SteadyTimePoint nextDrainPoll = loopStart + options_.drainPollInterval;

		for (;;)
		{
			WorkerAction action = WorkerAction::None;
			// 取走 allocate 时转存的非阻塞标记快照,交给 DoAllocate(见头文件 nonBlockingAllocateKicked_)。
			bool allocateKickedByNonBlocking = false;

			{
				std::unique_lock<std::mutex> lock(mutex_);
				const SteadyTimePoint wakeAt = drainEnabled ? std::min(nextHealth, nextDrainPoll) : nextHealth;
				cv_.wait_until(lock, wakeAt, [this] {
					return stopRequested_ || allocateRequested_ || readyRequested_ || shutdownRequested_;
				});

				if (stopRequested_)
				{
					return;
				}

				// 优先级:shutdown > allocate > 回 Ready > 排空轮询 > health。
				// allocate 排在回 Ready 前面,避免"最后一个销毁 + 立刻又创建"时
				// 先发一个多余的 /ready 再发 /allocate。
				if (shutdownRequested_)
				{
					shutdownRequested_ = false;
					action = WorkerAction::Shutdown;
				}
				else if (allocateRequested_)
				{
					allocateRequested_ = false;
					if (draining_)
					{
						// 等待方已因排空被拒绝。保留挂起的回 Ready 请求,下一轮照常处理。
						// 被丢弃的请求不再有结果,非阻塞标记一并清掉,免得残留到下一次 allocate。
						nonBlockingAllocateKicked_ = false;
						continue;
					}
					readyRequested_ = false;
					if (state_ == LifecycleState::Starting)
					{
						// Ready 还没成功,现在 allocate 一定失败。丢回去等下一轮,
						// 调用方的有界等待会超时并 fail-closed。非阻塞调用方重试时会重新置位标记。
						nonBlockingAllocateKicked_ = false;
						continue;
					}
					if (state_ == LifecycleState::Allocated)
					{
						// 典型来源:回 Ready 途中被踢、随后 /ready 失败留在 Allocated。请求就此满足,
						// 不会有 DoAllocate 结果,标记同 Starting / 排空分支一并清掉。
						nonBlockingAllocateKicked_ = false;
						continue;
					}
					// 标记转存为本次在途的快照并清零:此后成员里只剩 HTTP 在途期间新踢的。
					allocateKickedByNonBlocking = std::exchange(nonBlockingAllocateKicked_, false);
					state_ = LifecycleState::Allocating;
					action = WorkerAction::Allocate;
				}
				else if (readyRequested_)
				{
					readyRequested_ = false;
					if (!activeUnits_.empty() || pendingPermits_ != 0
						|| state_ != LifecycleState::Allocated)
					{
						continue;
					}
					// 状态必须在释放 mutex、发 HTTP 之前切换。新的创建请求
					// 看到 ReturningToReady 后不能再把本地 Allocated 当成可用。
					state_ = LifecycleState::ReturningToReady;
					action = WorkerAction::BackToReady;
				}
				else
				{
					const auto now = std::chrono::steady_clock::now();
					if (drainEnabled && now >= nextDrainPoll)
					{
						nextDrainPoll = now + options_.drainPollInterval;
						action = WorkerAction::PollDrain;
					}
					else if (now >= nextHealth)
					{
						nextHealth = now + options_.healthInterval;
						action = WorkerAction::Health;
					}
					// 否则是虚假唤醒,回到循环顶部按截止时间继续等。
				}
			}

			switch (action)
			{
			case WorkerAction::None:
				break;
			case WorkerAction::Shutdown:
				DoShutdown();
				break;
			case WorkerAction::Allocate:
				DoAllocate(allocateKickedByNonBlocking);
				break;
			case WorkerAction::BackToReady:
				DoBackToReady();
				break;
			case WorkerAction::PollDrain:
				DoPollDrain();
				break;
			case WorkerAction::Health:
				DoHealth();
				break;
			}
		}
	}

} // namespace agones
