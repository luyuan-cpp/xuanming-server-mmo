#include "agones/agones_gameserver_lifecycle.h"

#include <algorithm>
#include <map>
#include <string>
#include <utility>

#include "agones/agones_gameserver_status.h"
// muduo 头放在最后:Windows 适配头会 #define pid_t / off_t 等名字(见 muduo_test_loop.h 的说明)。
#include "muduo/base/Logging.h"
#include "muduo/net/EventLoop.h"

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

		// 把不可拷贝的 permit 随回调一起装进 shared_ptr 盒子,让 queueInLoop 的闭包可拷贝(muduo Functor 是 std::function)。
		struct PermitDeliveryBox
		{
			std::shared_ptr<GameServerLifecycle::AllocationPermit> permit;
			GameServerLifecycle::AllocationPermitCallback done;
		};
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
		if (granted)
		{
			owner.FinishAllocationPermit();
		}
	}

	GameServerLifecycle::AllocationPermit::AllocationPermit(AllocationPermit&& other) noexcept
		: owner(other.owner), granted(std::exchange(other.granted, false))
	{
	}

	GameServerLifecycle& GameServerLifecycle::Instance()
	{
		static GameServerLifecycle instance;
		return instance;
	}

	GameServerLifecycle::~GameServerLifecycle()
	{
		// 先让超时定时器的弱引用失效:此后到点的定时器不再回到本对象。
		aliveToken.reset();
		// 静态析构时 loop 多半已经不在:只丢弃等待者,不投递。
		StopImpl(false);
	}

	void GameServerLifecycle::StartDisabled()
	{
		std::lock_guard<std::mutex> lock(mutex);
		state = LifecycleState::Disabled;
		draining = false;
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
			std::lock_guard<std::mutex> lock(mutex);
			if (worker.joinable())
			{
				LOG_WARN << "[Agones] lifecycle already started, ignoring duplicate Start()";
				return;
			}

			this->options = std::move(options);
			this->transport = std::move(transport);
			client = std::make_unique<AgonesRestClient>(std::move(baseUrl), *this->transport, this->options.timeouts);
			state = LifecycleState::Starting;
			stopRequested = false;
			// stop_source 单次有效:每次启动换新的取消源。worker 线程尚未创建,这里写 workerStopToken_ 没有竞争。
			workerStopSource = std::stop_source{};
			workerStopToken = workerStopSource.get_token();
			draining = false;
			// 心跳记为"刚更新":EventLoop 的定时 Touch 还没开始跑,给它 loopStaleAfter 的宽限。
			// worker 线程尚未创建,这里写 worker 专属的 loopStaleReported_ 没有竞争。
			lastLoopHeartbeatNs.store(SteadyNowNs(), std::memory_order_relaxed);
			loopStaleReported = false;

			LOG_INFO << "[Agones] lifecycle starting: sdk=" << client->BaseUrl()
					 << " health_interval_ms=" << this->options.healthInterval.count()
					 << " ready_max_attempts=" << this->options.readyMaxAttempts
					 << " require_loop_heartbeat=" << (this->options.requireLoopHeartbeat ? "true" : "false")
					 << " loop_stale_after_ms=" << this->options.loopStaleAfter.count()
					 << " drain_label=" << (this->options.drainLabelKey.empty() ? "<disabled>" : this->options.drainLabelKey);
		}

		worker = std::thread([this] { WorkerMain(); });
	}

	void GameServerLifecycle::Stop()
	{
		StopImpl(true);
	}

	void GameServerLifecycle::StopImpl(bool deliverRejections)
	{
		std::vector<PermitDelivery> rejections;
		bool workerRunning = false;
		{
			std::lock_guard<std::mutex> lock(mutex);
			// 与 worker 结算点竞争:谁先在锁内摘下等待者谁裁决,每个等待者只裁决一次。
			ResolveAsyncWaitersLocked(rejections, true);
			workerRunning = worker.joinable();
			if (workerRunning)
			{
				stopRequested = true;
			}
		}
		if (deliverRejections)
		{
			DeliverPermits(std::move(rejections));
		}
		// 不投递时 rejections 随作用域析构:回调不被调用,拒绝的 permit 也还没构造,不涉及记账。
		if (!workerRunning)
		{
			return;
		}
		// 中止 worker 在途的可放弃 HTTP(见 workerStopSource_),/shutdown 不受影响;等待、退避与主循环由
		// stopRequested_ + notify 唤醒。放在锁外:request_stop() 会同步执行传输层登记的 stop_callback,
		// 不在持有 mutex_ 时调用外部代码。
		workerStopSource.request_stop();
		cv.notify_all();

		// 有界:在途请求被中止、重试与退避立即退出,join 不再等满 allocate 的重试链(见头文件 Stop())。
		// 唯一多等的是挂起或在途的 /shutdown:worker 退出前送达它,最多多 timeouts.total。
		worker.join();

		std::lock_guard<std::mutex> lock(mutex);
		state = LifecycleState::Stopped;
		client.reset();
		transport.reset();
		LOG_INFO << "[Agones] lifecycle worker stopped and joined";
	}

	bool GameServerLifecycle::EnsureAllocatedBlocking()
	{
		return EnsureAllocatedBlockingImpl(false);
	}

	std::optional<bool> GameServerLifecycle::DecideWithoutWaitLocked(bool reservePermit)
	{
		if (state == LifecycleState::Disabled)
		{
			if (reservePermit)
			{
				++pendingPermits;
			}
			return true;
		}
		if (stopRequested || state == LifecycleState::Stopped)
		{
			LOG_WARN << "[Agones] refusing allocation permit: lifecycle already stopped";
			return false;
		}
		if (draining)
		{
			// D83:排空中一律拒绝,即使本地还是 Allocated(已有单元照常跑完)。
			LOG_WARN << "[Agones] refusing allocation permit: draining (label " << options.drainLabelKey << ")";
			return false;
		}
		if (state == LifecycleState::Allocated)
		{
			if (reservePermit)
			{
				++pendingPermits;
				readyRequested = false;
			}
			return true;
		}
		if (state == LifecycleState::ShuttingDown)
		{
			LOG_WARN << "[Agones] refusing allocation permit: process is shutting down";
			return false;
		}
		return std::nullopt;
	}

	bool GameServerLifecycle::WaitSatisfiedLocked(std::uint64_t startGeneration) const
	{
		// ShuttingDown 是终态(不走 Stop 时一直停在这里):挂起的 allocate 会被 worker 丢弃,不会再有结果,
		// 等下去只会等满上限再误报"allocate 未确认"。
		return state == LifecycleState::Allocated
			|| state == LifecycleState::ShuttingDown
			|| allocateResultGeneration != startGeneration
			|| stopRequested
			|| draining;
	}

	bool GameServerLifecycle::FinishWaitLocked(bool reservePermit)
	{
		if (stopRequested || state == LifecycleState::Stopped)
		{
			// 等待期间 Stop():与 DecideWithoutWaitLocked「已停即拒绝」、异步等待方由 Stop() 统一拒绝同一口径 ——
			// 停止标志置位之后不再签发任何许可。worker 抢先拿锁写入 Allocated(在途 /allocate 恰在中止生效前成功)
			// 时也一样拒绝:实例随 Pod 删除作废,放行只会让一个创建落进正在停机的进程,并留下在途许可。
			// 判定放在排空与状态之前:醒来的原因是停机,就按停机报,不误报成"allocate 未确认"的 ERROR。
			// 异步等待方走不到这里:Stop() 在置位 stopRequested_ 的同一临界区里已摘下全部异步等待者,
			// 此后的异步调用由 DecideWithoutWaitLocked 当场拒绝,不再登记。
			LOG_WARN << "[Agones] refusing allocation permit: lifecycle stopped while waiting for allocate";
			return false;
		}
		if (state == LifecycleState::ShuttingDown)
		{
			// 等待期间进程决定自我终止(RequestShutdown,不走 Stop):与 DecideWithoutWaitLocked 同一口径拒绝。
			// worker 取走 /shutdown 时与写入 ShuttingDown 同一临界区结算等待者,挂起的 allocate 随后被丢弃。
			LOG_WARN << "[Agones] refusing allocation permit: process started shutting down while waiting for allocate";
			return false;
		}
		if (draining)
		{
			// 等待期间进入排空:通常是 worker 在处理挂起的 allocate 之前先读到了标签(allocate 随后被
			// 丢弃);罕见时是 allocate 已成功、等待方醒来之前下一轮轮询读到了标签。两种情况都不在
			// 这里做零单元收口:DoPollDrain 置位 draining_ 与"排空且零单元即回 Ready"的复核在同一
			// 临界区内完成,走到这里时已经收口过了。
			LOG_WARN << "[Agones] refusing allocation permit: drain started while waiting for allocate";
			return false;
		}
		if (state != LifecycleState::Allocated)
		{
			LOG_ERROR << "[Agones] allocate not confirmed within "
					  << options.allocateWaitTimeout.count() << "ms, state=" << ToString(state);
			return false;
		}
		if (reservePermit)
		{
			++pendingPermits;
			readyRequested = false;
		}
		return true;
	}

	bool GameServerLifecycle::EnsureAllocatedBlockingImpl(bool reservePermit)
	{
		std::unique_lock<std::mutex> lock(mutex);

		const std::optional<bool> verdict = DecideWithoutWaitLocked(reservePermit);
		if (verdict.has_value())
		{
			return *verdict;
		}

		const std::uint64_t startGeneration = allocateResultGeneration;
		allocateRequested = true;
		cv.notify_all();

		// 有界等待。超时不是"假设成功",是拒绝这次创建让调用方重试。
		// 等待期间计入 allocateWaiters_(加减都在 mutex_ 内,wait_for 返回时已重新持锁):
		// DoAllocate 据此判断结果落地时是否还有人来取;超时离开后 allocate 才晚到的,由它收口回 Ready。
		++allocateWaiters;
		cv.wait_for(lock, options.allocateWaitTimeout, [this, startGeneration] {
			return WaitSatisfiedLocked(startGeneration);
		});
		--allocateWaiters;

		return FinishWaitLocked(reservePermit);
	}

	bool GameServerLifecycle::EnsureAllocatedNonBlocking()
	{
		return EnsureAllocatedNonBlockingImpl(false);
	}

	bool GameServerLifecycle::EnsureAllocatedNonBlockingImpl(bool reservePermit)
	{
		std::lock_guard<std::mutex> lock(mutex);

		if (state == LifecycleState::Disabled)
		{
			if (reservePermit)
			{
				++pendingPermits;
			}
			return true;
		}
		if (stopRequested || state == LifecycleState::Stopped)
		{
			return false;
		}
		if (draining)
		{
			// 排空中不踢 allocate:回 Ready 之后再 allocate 等于把自己又占回来。
			return false;
		}
		if (state == LifecycleState::Allocated)
		{
			if (reservePermit)
			{
				++pendingPermits;
				readyRequested = false;
			}
			return true;
		}
		if (state == LifecycleState::ShuttingDown)
		{
			return false;
		}

		allocateRequested = true;
		// 调用方会在稍后重试时取走 Allocated,这次结果不做晚到收口(见 DoAllocate)。
		nonBlockingAllocateKicked = true;
		cv.notify_all();
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

	void GameServerLifecycle::AcquireAllocationPermitAsync(muduo::net::EventLoop& loop, AllocationPermitCallback done)
	{
		loop.assertInLoopThread();
		if (!done)
		{
			LOG_ERROR << "[Agones] AcquireAllocationPermitAsync called with an empty callback; ignored";
			return;
		}

		std::vector<PermitDelivery> immediate;
		std::uint64_t waiterId = 0;
		std::chrono::steady_clock::duration waitTimeout{};
		{
			std::lock_guard<std::mutex> lock(mutex);
			const std::optional<bool> verdict = DecideWithoutWaitLocked(true);
			if (verdict.has_value())
			{
				// 当场可判定的结论也经 queueInLoop 投递,保证永不在调用方栈内回调。
				immediate.push_back(PermitDelivery{ loop, *verdict, std::move(done) });
			}
			else
			{
				// 与阻塞版相同:踢一次 allocate,计入 allocateWaiters_,由结算点 / 超时定时器 / Stop() 裁决。
				allocateRequested = true;
				waiterId = nextAsyncWaiterId++;
				waitTimeout = options.allocateWaitTimeout;
				asyncWaiters.emplace(waiterId, AsyncPermitWaiter{ loop, allocateResultGeneration,
					std::chrono::steady_clock::now() + waitTimeout, std::move(done) });
				++allocateWaiters;
				cv.notify_all();
			}
		}

		if (!immediate.empty())
		{
			DeliverPermits(std::move(immediate));
			return;
		}
		ArmAsyncWaiterTimer(loop, waiterId, waitTimeout);
	}

	void GameServerLifecycle::ResolveAsyncWaitersLocked(std::vector<PermitDelivery>& out, bool rejectAll)
	{
		if (asyncWaiters.empty())
		{
			return;
		}
		if (rejectAll)
		{
			LOG_WARN << "[Agones] refusing " << asyncWaiters.size()
					 << " pending async allocation permit(s): lifecycle stopping";
		}
		for (auto it = asyncWaiters.begin(); it != asyncWaiters.end();)
		{
			AsyncPermitWaiter& waiter = it->second;
			if (!rejectAll && !WaitSatisfiedLocked(waiter.startGeneration))
			{
				++it;
				continue;
			}
			--allocateWaiters;
			// 放行时 FinishWaitLocked 已在本临界区内 ++pendingPermits_:随后的晚到收口复核自然看到在途许可。
			const bool granted = !rejectAll && FinishWaitLocked(true);
			out.push_back(PermitDelivery{ waiter.loop, granted, std::move(waiter.done) });
			it = asyncWaiters.erase(it);
		}
	}

	void GameServerLifecycle::ArmAsyncWaiterTimer(muduo::net::EventLoop& loop,
		std::uint64_t waiterId,
		std::chrono::steady_clock::duration delay)
	{
		// muduo 定时器已按单调时钟到期,ExpireAsyncWaiter 仍按 steady 复核;下限 1ms,避免秒→微秒截断让定时器空转。
		const std::chrono::steady_clock::duration clamped =
			std::max<std::chrono::steady_clock::duration>(delay, std::chrono::milliseconds(1));
		loop.runAfter(std::chrono::duration<double>(clamped).count(),
			[alive = std::weak_ptr<int>(aliveToken), this, waiterId] {
				// 只弱持有 aliveToken_:lifecycle 已析构时 lock 失败,不碰 this。
				if (alive.lock())
				{
					ExpireAsyncWaiter(waiterId);
				}
			});
	}

	void GameServerLifecycle::ExpireAsyncWaiter(std::uint64_t waiterId)
	{
		AllocationPermitCallback done;
		std::chrono::milliseconds::rep waitTimeoutMs = 0;
		LifecycleState stateAtExpiry = LifecycleState::Disabled;
		{
			std::unique_lock<std::mutex> lock(mutex);
			const auto it = asyncWaiters.find(waiterId);
			if (it == asyncWaiters.end())
			{
				return; // 已被结算点或 Stop() 裁决,结果已投递
			}
			const auto now = std::chrono::steady_clock::now();
			if (now < it->second.deadline)
			{
				// 定时器比 steady 截止点早到(秒→微秒截断等):按剩余 steady 时长重新上弦。
				muduo::net::EventLoop& loop = it->second.loop.get();
				const std::chrono::steady_clock::duration remaining = it->second.deadline - now;
				lock.unlock();
				ArmAsyncWaiterTimer(loop, waiterId, remaining);
				return;
			}
			done = std::move(it->second.done);
			asyncWaiters.erase(it);
			--allocateWaiters;
			waitTimeoutMs = options.allocateWaitTimeout.count();
			stateAtExpiry = state;
		}

		// 出表后 allocate 才晚到成功的,由 DoAllocate 的晚到收口送回 Ready(allocateWaiters_ 已减)。
		LOG_ERROR << "[Agones] allocate not confirmed within " << waitTimeoutMs
				  << "ms (async), state=" << ToString(stateAtExpiry);
		done(AllocationPermit(*this, false));
	}

	void GameServerLifecycle::DeliverPermits(std::vector<PermitDelivery> deliveries)
	{
		for (PermitDelivery& delivery : deliveries)
		{
			// 私有构造只能在成员函数里调用;先在锁外构造,再经公开的移动构造装盒。
			AllocationPermit permit(*this, delivery.granted);
			auto box = std::make_shared<PermitDeliveryBox>(PermitDeliveryBox{
				std::make_shared<AllocationPermit>(std::move(permit)), std::move(delivery.done) });
			// 本线程(可能是 worker)交出盒子后不再持有回调的引用:回调只在 loop 线程上被调用与析构。
			// 闭包没执行就随 ~EventLoop 销毁时,permit 照常析构记账。
			delivery.loop.get().queueInLoop([box = std::move(box)] {
				AllocationPermitCallback callback;
				callback.swap(box->done);
				callback(std::move(*box->permit));
			});
		}
	}

	bool GameServerLifecycle::RequestReadyIfIdleLocked()
	{
		if (state != LifecycleState::Allocated || !activeUnits.empty() || pendingPermits != 0)
		{
			return false;
		}
		readyRequested = true;
		cv.notify_all();
		return true;
	}

	void GameServerLifecycle::FinishAllocationPermit()
	{
		std::lock_guard<std::mutex> lock(mutex);
		if (pendingPermits == 0)
		{
			LOG_ERROR << "[Agones] unbalanced AllocationPermit release";
			return;
		}

		--pendingPermits;
		if (RequestReadyIfIdleLocked())
		{
			LOG_WARN << "[Agones] allocation permit finished with zero units, returning to Ready";
		}
	}

	void GameServerLifecycle::OnUnitCreated(std::uint64_t unitKey)
	{
		std::lock_guard<std::mutex> lock(mutex);

		const auto inserted = activeUnits.insert(unitKey);
		if (!inserted.second)
		{
			// 上游创建是按业务 id 幂等的,正常不会走到这里。
			// 真走到了说明有路径重复触发事件,记下来但不重复计数。
			LOG_WARN << "[Agones] duplicate OnUnitCreated for key=" << unitKey
					 << ", unit count stays " << activeUnits.size();
			return;
		}

		LOG_INFO << "[Agones] unit created key=" << unitKey
				 << " unit_count=" << activeUnits.size() << " state=" << ToString(state);
	}

	void GameServerLifecycle::OnUnitDestroyed(std::uint64_t unitKey)
	{
		std::lock_guard<std::mutex> lock(mutex);

		if (activeUnits.erase(unitKey) == 0)
		{
			// 销毁一个没记过账的单元:不减计数,更不能让计数变负。
			LOG_WARN << "[Agones] OnUnitDestroyed for unknown key=" << unitKey
					 << ", unit count stays " << activeUnits.size();
			return;
		}

		LOG_INFO << "[Agones] unit destroyed key=" << unitKey
				 << " unit_count=" << activeUnits.size() << " state=" << ToString(state);

		if (!activeUnits.empty())
		{
			return; // 还有单元,保持 Allocated
		}
		if (pendingPermits != 0)
		{
			LOG_INFO << "[Agones] last unit destroyed with " << pendingPermits
					 << " allocation permit(s) still in flight; staying Allocated";
			return;
		}
		if (state == LifecycleState::Disabled || state == LifecycleState::Stopped
			|| state == LifecycleState::ReturningToReady
			|| state == LifecycleState::ShuttingDown)
		{
			return;
		}

		readyRequested = true;
		cv.notify_all();
	}

	void GameServerLifecycle::ReconcileIdleAfterCreate()
	{
		std::lock_guard<std::mutex> lock(mutex);

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
			std::lock_guard<std::mutex> lock(mutex);
			if (state == LifecycleState::Disabled)
			{
				return;
			}
			if (stopRequested || state == LifecycleState::Stopped)
			{
				// 与 Stop() 在同一把锁上线性化:Stop() 之前收下的请求一定发出(worker 退出前先发 /shutdown,
				// 见 WorkerMain 的优先级);Stop() 开始之后才来的一律忽略,不让发不发取决于 worker 是否已退出。
				LOG_WARN << "[Agones] RequestShutdown ignored: lifecycle already stopping";
				return;
			}
			if (shutdownRequested || state == LifecycleState::ShuttingDown)
			{
				// 幂等:/shutdown 挂起或已取走(在途 / 已发出)时再调不补发第二次。worker 取走请求与写入 ShuttingDown
				// 在同一临界区,这里两个条件之间没有空档。/shutdown 单次尝试,失败只打 ERROR,不靠重复调用重试。
				LOG_WARN << "[Agones] RequestShutdown ignored: shutdown already "
						 << (shutdownRequested ? "pending" : "issued");
				return;
			}
			shutdownRequested = true;
		}
		cv.notify_all();
	}

	void GameServerLifecycle::TouchLoopHeartbeat()
	{
		lastLoopHeartbeatNs.store(SteadyNowNs(), std::memory_order_relaxed);
	}

	bool GameServerLifecycle::IsDraining() const
	{
		std::lock_guard<std::mutex> lock(mutex);
		return draining;
	}

	LifecycleState GameServerLifecycle::State() const
	{
		std::lock_guard<std::mutex> lock(mutex);
		return state;
	}

	int GameServerLifecycle::UnitCount() const
	{
		std::lock_guard<std::mutex> lock(mutex);
		return static_cast<int>(activeUnits.size());
	}

	std::size_t GameServerLifecycle::PendingPermitCount() const
	{
		std::lock_guard<std::mutex> lock(mutex);
		return pendingPermits;
	}

	bool GameServerLifecycle::WaitForState(LifecycleState expected, std::chrono::milliseconds timeout)
	{
		std::unique_lock<std::mutex> lock(mutex);
		return cv.wait_for(lock, timeout, [this, expected] { return state == expected; });
	}

	bool GameServerLifecycle::SleepInterruptible(std::chrono::milliseconds duration)
	{
		std::unique_lock<std::mutex> lock(mutex);
		cv.wait_for(lock, duration, [this] { return stopRequested; });
		return !stopRequested;
	}

	bool GameServerLifecycle::AbandonedByStop(const HttpResponse& response) const
	{
		// workerStopToken_ 只在 Start() 里、worker 线程创建之前写,worker 读它无需加锁。
		// 成功的响应照常处理(中止前已完成);只有失败且已停才算被放弃。
		return !response.Ok() && workerStopToken.stop_requested();
	}

	bool GameServerLifecycle::LoopHeartbeatFresh() const
	{
		// options_ 只在 Start() 里、worker 线程创建之前写,worker 读它无需加锁。
		if (!options.requireLoopHeartbeat)
		{
			return true;
		}
		const std::int64_t ageNs = SteadyNowNs() - lastLoopHeartbeatNs.load(std::memory_order_relaxed);
		return std::chrono::nanoseconds(ageNs) <= options.loopStaleAfter;
	}

	void GameServerLifecycle::RunInitialReady()
	{
		auto backoff = options.readyInitialBackoff;
		// 快速重试用完之后每隔这么多次失败才打一行 ERROR,免得 sidecar 长时间不就绪时每个退避间隔刷一行。
		constexpr int kSlowRetryLogEvery = 30;

		// 快速重试用完之后不放弃(docs/design/infra-reconnect-overview.md §4 R10):sidecar 晚于约 1 分钟才就绪时,
		// 放弃就让实例永远停在 Starting、永远不可分配。此后按 readyMaxBackoff 一直重试,直到成功或 Stop();
		// 期间状态仍是 Starting,任何创建照旧 fail-closed,不会假装 Ready。
		for (int attempt = 1;; ++attempt)
		{
			{
				std::lock_guard<std::mutex> lock(mutex);
				if (stopRequested)
				{
					return;
				}
			}

			const HttpResponse response = client->Ready(workerStopToken);
			if (AbandonedByStop(response))
			{
				// 被 Stop() 中止:留在 Starting,不重试,也不按失败打日志。
				return;
			}
			if (response.Ok())
			{
				std::lock_guard<std::mutex> lock(mutex);
				// 罕见但可能:Ready 还在重试时已经有人排队要 allocate。
				// 这里只负责把状态推到 Ready,allocate 由主循环消费挂起请求
				// (排空启用时,WorkerMain 在进入主循环前先同步读一次排空标签)。
				state = LifecycleState::Ready;
				LOG_INFO << "[Agones] READY confirmed after " << attempt << " attempt(s)";
				cv.notify_all();
				return;
			}

			if (attempt < options.readyMaxAttempts)
			{
				LOG_WARN << "[Agones] POST /ready attempt " << attempt << "/" << options.readyMaxAttempts
						 << " failed (status=" << response.statusCode << " error=" << response.error
						 << "), retrying in " << backoff.count() << "ms";
			}
			else if (attempt == options.readyMaxAttempts || attempt % kSlowRetryLogEvery == 0)
			{
				LOG_ERROR << "[Agones] POST /ready still failing after " << attempt << " attempts (status="
						  << response.statusCode << " error=" << response.error
						  << "); staying in Starting, unit creation keeps failing closed, retrying every "
						  << options.readyMaxBackoff.count() << "ms";
			}

			if (!SleepInterruptible(backoff))
			{
				return;
			}
			backoff = std::min(backoff * 2, options.readyMaxBackoff);
		}
	}

	void GameServerLifecycle::DoAllocate(bool nonBlockingRetryExpected)
	{
		bool allocated = false;
		// 停机放弃:在途的 /allocate 被 Stop() 的取消令牌中止,或退避被 Stop() 打断。之后不再发起新的 allocate。
		bool abandoned = false;

		for (int attempt = 1; attempt <= options.allocateMaxAttempts && !allocated; ++attempt)
		{
			const HttpResponse response = client->Allocate(workerStopToken);
			if (response.Ok())
			{
				allocated = true;
				break;
			}
			if (AbandonedByStop(response))
			{
				abandoned = true;
				break;
			}

			LOG_WARN << "[Agones] POST /allocate attempt " << attempt << "/"
					 << options.allocateMaxAttempts << " failed (status=" << response.statusCode
					 << " error=" << response.error << ")";

			if (attempt < options.allocateMaxAttempts && !SleepInterruptible(options.allocateRetryBackoff))
			{
				abandoned = true;
				break;
			}
		}

		std::vector<PermitDelivery> deliveries;
		{
			std::lock_guard<std::mutex> lock(mutex);
			state = allocated ? LifecycleState::Allocated : LifecycleState::Ready;
			++allocateResultGeneration;
			// 结算点:必须在 ++allocateResultGeneration_ 之后、晚到收口复核之前。放行的异步等待方已在此
			// ++pendingPermits_,下面的 RequestReadyIfIdleLocked 自然返回假;失败结果按拒绝出表。
			ResolveAsyncWaitersLocked(deliveries, false);
			// 这里不需要排空分支:draining_ 只可能由 worker 在 DoPollDrain 里置为 true,worker 取
			// allocate 时已判过未排空才走到这里,而 DoAllocate 同在 worker 上执行,期间 draining_ 不会变。
			if (allocated)
			{
				// 已 Stop() 而在途 /allocate 恰在中止生效前成功时也照常写入 Allocated(与 sidecar 一致):
				// 不会因此签发许可 —— 阻塞等待方醒来按已停拒绝(FinishWaitLocked),异步等待方早已由 Stop() 摘表;
				// worker 回到主循环即按停止退出(有挂起的 /shutdown 时先送达)。
				LOG_INFO << "[Agones] ALLOCATED";
				// 非阻塞调用方稍后会来取:触发本次 allocate 的那次踢(快照),或 HTTP 在途期间新踢的
				// (成员;它们的挂起请求下面就地清掉,由这次成功满足)。失败分支不碰成员:在途期间新踢的
				// 挂起 allocate 还会再跑一次,worker 取它时再转存为快照。
				// 先无条件清零成员再合并,不能写进 || 的右侧(短路会跳过清零,标记残留)。
				const bool kickedWhileInFlight = std::exchange(nonBlockingAllocateKicked, false);
				const bool retryExpected = nonBlockingRetryExpected || kickedWhileInFlight;
				// HTTP 在途期间到来的调用方留下的挂起 allocate 都由这次成功满足,就地清掉:否则 worker
				// 下一轮先取到它(allocate 优先于回 Ready),在"已 Allocated"分支里把下面的收口请求冲掉。
				// 同理清掉此前残留的回 Ready 请求,保持"allocate 压过回 Ready"的原语义 —— 还在等的
				// 调用方醒来之前 pendingPermits_ 仍为 0,残留请求会抢先把刚分配的实例送回 Ready。
				// 回不回 Ready 只由下面的收口决定。
				allocateRequested = false;
				readyRequested = false;
				// 晚到分配收口:阻塞等待方的上限(allocateWaitTimeout,默认 3s)短于本函数最坏耗时
				// (默认约 6.4s)。等待方都已超时离开、也没有非阻塞调用方等着重试时,没有任何人会再来
				// 取这次分配 —— 零单元、零在途许可的 Allocated 永远不会被 OnUnitDestroyed /
				// FinishAllocationPermit 收口,实例会一直占着 Agones 容量。与写入 Allocated 在同一临界区
				// 内复核,判断依据(等待方数、单元数、在途许可)与状态是同一快照;此后新来的调用方拿许可
				// 会清掉 readyRequested_,worker 真正回 Ready 前也会再复核一次零单元零许可。
				if (allocateWaiters == 0 && !retryExpected && RequestReadyIfIdleLocked())
				{
					LOG_WARN << "[Agones] allocate confirmed after every waiter gave up (wait timeout "
							 << options.allocateWaitTimeout.count()
							 << "ms), zero units and no permit in flight; returning to Ready";
				}
			}
			else if (abandoned)
			{
				// 停机路径:异步等待方已由 Stop() 摘表拒绝,阻塞等待方随 stopRequested_ 醒来拒绝;不按故障报错。
				LOG_WARN << "[Agones] allocate abandoned: lifecycle stopping";
			}
			else
			{
				LOG_ERROR << "[Agones] allocate failed, staying Ready; unit creation will fail closed";
			}
		}
		cv.notify_all();
		DeliverPermits(std::move(deliveries));
	}

	void GameServerLifecycle::DoBackToReady()
	{
		const HttpResponse response = client->Ready(workerStopToken);
		const bool abandoned = AbandonedByStop(response);

		std::vector<PermitDelivery> deliveries;
		{
			std::lock_guard<std::mutex> lock(mutex);
			if (!response.Ok())
			{
				// /ready 没成功,sidecar 仍是 Allocated。恢复本地状态并唤醒
				// 等待中的创建;它们无需再发一次 /allocate。
				state = LifecycleState::Allocated;
				if (abandoned)
				{
					// 被 Stop() 中止:停机时回不回 Ready 已无意义(Pod 正在删除),不按故障报错。
					LOG_WARN << "[Agones] POST /ready (drain) abandoned: lifecycle stopping";
				}
				else
				{
					LOG_ERROR << "[Agones] POST /ready (drain) failed status=" << response.statusCode
							  << " error=" << response.error << "; staying " << ToString(state);
				}
			}
			else
			{
				state = LifecycleState::Ready;
				LOG_INFO << "[Agones] back to READY (0 units)";
			}
			// 结算点(两个分支都有):回到 Allocated 时放行异步等待方;回到 Ready 时谓词不成立,等待方继续等挂起的 allocate。
			ResolveAsyncWaitersLocked(deliveries, false);
			cv.notify_all();
		}
		DeliverPermits(std::move(deliveries));
	}

	void GameServerLifecycle::DoShutdown()
	{
		// ShuttingDown 已由 WorkerMain 在取走请求的同一临界区写入并结算等待者(见那里的注释)。
		// 刻意不带停止令牌:/shutdown 是进程决定自我终止时必须送达的最终请求,Stop() 不中止它。
		// 在途时 Stop() 的 join 等它完成;Stop() 之前已请求、worker 还没取走的,worker 退出前先发出它
		// (WorkerMain 里 shutdown 优先于停止)。两种情况 Stop() 都多等最多 timeouts.total。
		const HttpResponse response = client->Shutdown();
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
			if (!loopStaleReported)
			{
				loopStaleReported = true;
				LOG_ERROR << "[Agones] EventLoop heartbeat older than " << options.loopStaleAfter.count()
						  << "ms; withholding /health so Agones marks this GameServer Unhealthy";
			}
			return;
		}
		if (loopStaleReported)
		{
			loopStaleReported = false;
			LOG_WARN << "[Agones] EventLoop heartbeat recovered; resuming /health";
		}

		const HttpResponse response = client->Health(workerStopToken);
		if (!response.Ok() && !AbandonedByStop(response))
		{
			LOG_WARN << "[Agones] POST /health failed status=" << response.statusCode
					 << " error=" << response.error;
		}
	}

	void GameServerLifecycle::DoPollDrain()
	{
		const HttpResponse response = client->GameServer(workerStopToken);
		if (AbandonedByStop(response))
		{
			return; // 被 Stop() 中止:不翻转判定,也不按读取失败打日志
		}
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
		const bool labelPresent = labels.count(options.drainLabelKey) > 0;

		// draining_ 置位与下面"排空且零单元即回 Ready"的复核必须在同一临界区内:
		// 许可等待方看到 draining_ 时,零单元收口已经做过,它自己不用再收口。
		std::vector<PermitDelivery> deliveries;
		{
			std::lock_guard<std::mutex> lock(mutex);
			if (labelPresent != draining)
			{
				draining = labelPresent;
				if (draining)
				{
					LOG_WARN << "[Agones] drain label '" << options.drainLabelKey
							 << "' present: refusing new units; unit_count=" << activeUnits.size()
							 << " state=" << ToString(state);
					// 结算点:排空开始,异步等待方一律按"等待期间进入排空"拒绝。
					ResolveAsyncWaitersLocked(deliveries, false);
				}
				else
				{
					LOG_INFO << "[Agones] drain label '" << options.drainLabelKey
							 << "' cleared: accepting new units again; state=" << ToString(state);
				}
				// 唤醒 EnsureAllocatedBlocking 的等待方(谓词含 draining_)。
				cv.notify_all();
			}

			// 每轮都复核一次"排空且零单元":上一次 /ready 失败留在 Allocated 时,在这里收敛。
			if (draining && RequestReadyIfIdleLocked())
			{
				LOG_INFO << "[Agones] draining with zero units, returning to Ready";
			}
		}
		DeliverPermits(std::move(deliveries));
	}

	void GameServerLifecycle::WorkerMain()
	{
		RunInitialReady();

		// 排空启用时,进入主循环之前先同步读一次标签。容器重启后 GameServer 对象上的标签还在;
		// 而 RunInitialReady 要一直重试到 sidecar 认下 /ready 为止,期间调用方挂起的 allocate 请求在主循环里优先级高于
		// 排空轮询 —— 不先读这一次,就会先 /allocate、接下单元,再发现自己在排空。
		// 这一次读取失败时按"保持上一次判定"处理(初值为未排空),由下一轮轮询纠正;不改成
		// "判定未知即拒绝许可":GET /gameserver 的形状一旦与预期不符,实例就永远接不了单元。
		const bool drainEnabled = !options.drainLabelKey.empty();
		bool stopping = false;
		{
			std::lock_guard<std::mutex> lock(mutex);
			stopping = stopRequested;
		}
		if (drainEnabled && !stopping)
		{
			DoPollDrain();
		}

		// 两个周期任务按各自的截止时间调度,互不重置:挂起请求(allocate / 回 Ready)再频繁,
		// 到期的 health 也会在两次请求之间的空档里立刻执行,而不是每次唤醒都重新计时。
		using SteadyTimePoint = std::chrono::steady_clock::time_point;
		const SteadyTimePoint loopStart = std::chrono::steady_clock::now();
		SteadyTimePoint nextHealth = loopStart + options.healthInterval;
		SteadyTimePoint nextDrainPoll = loopStart + options.drainPollInterval;

		for (;;)
		{
			WorkerAction action = WorkerAction::None;
			// 取走 allocate 时转存的非阻塞标记快照,交给 DoAllocate(见头文件 nonBlockingAllocateKicked_)。
			bool allocateKickedByNonBlocking = false;
			// 取走 /shutdown 时结算出的异步等待者,解锁后、发 HTTP 之前投递。
			std::vector<PermitDelivery> shutdownDeliveries;

			{
				std::unique_lock<std::mutex> lock(mutex);
				const SteadyTimePoint wakeAt = drainEnabled ? std::min(nextHealth, nextDrainPoll) : nextHealth;
				cv.wait_until(lock, wakeAt, [this] {
					return stopRequested || allocateRequested || readyRequested || shutdownRequested;
				});

				// 优先级:shutdown > 停止 > allocate > 回 Ready > 排空轮询 > health。
				// shutdown 排在停止前面:Stop() 之前已 RequestShutdown()、worker 还没取走时(典型是 worker 正在
				// 一次可放弃请求里,被 Stop() 中止后回到这里),退出前先送达 /shutdown,下一轮再按停止退出。
				// 这是 worker 唯一的退出点,所以挂起的 /shutdown 不会被 Stop() 吞掉;RequestShutdown() 与 Stop()
				// 在同一把锁上线性化,Stop() 之后的请求被拒,不会在这里多发。
				// allocate 排在回 Ready 前面,避免"最后一个销毁 + 立刻又创建"时
				// 先发一个多余的 /ready 再发 /allocate。
				if (shutdownRequested)
				{
					shutdownRequested = false;
					// 状态必须在释放 mutex、发 HTTP 之前切换(与 Allocating / ReturningToReady 同理):
					//  - RequestShutdown 看到 ShuttingDown 即忽略,/shutdown 在途时不会再排第二次;
					//  - 新的许可请求当场拒绝,不再置位 allocateRequested_。
					// ShuttingDown 是终态(不走 Stop 时一直停在这里,Stop 后转 Stopped)。结算点:等待方的谓词
					// 把 ShuttingDown 视为成立、裁决一律拒绝;阻塞等待方由 notify_all 唤醒。此前挂起的 allocate
					// 留到下一轮,由 allocate 分支丢弃。
					state = LifecycleState::ShuttingDown;
					ResolveAsyncWaitersLocked(shutdownDeliveries, false);
					cv.notify_all();
					action = WorkerAction::Shutdown;
				}
				else if (stopRequested)
				{
					return;
				}
				else if (allocateRequested)
				{
					allocateRequested = false;
					if (state == LifecycleState::ShuttingDown)
					{
						// 取走 /shutdown 之前就挂起的 allocate(shutdown 优先,先被取走):进程正在自我终止,
						// 不得再把状态改回 Allocating / Allocated。等待方已在取走 /shutdown 时被拒绝;
						// 丢弃的请求不再有结果,非阻塞标记一并清掉。回 Ready 请求留给下面的分支照常丢弃。
						nonBlockingAllocateKicked = false;
						LOG_WARN << "[Agones] pending allocate discarded: process is shutting down";
						continue;
					}
					if (draining)
					{
						// 等待方已因排空被拒绝。保留挂起的回 Ready 请求,下一轮照常处理。
						// 被丢弃的请求不再有结果,非阻塞标记一并清掉,免得残留到下一次 allocate。
						nonBlockingAllocateKicked = false;
						continue;
					}
					readyRequested = false;
					if (state == LifecycleState::Starting)
					{
						// Ready 还没成功,现在 allocate 一定失败。丢回去等下一轮,
						// 调用方的有界等待会超时并 fail-closed。非阻塞调用方重试时会重新置位标记。
						nonBlockingAllocateKicked = false;
						continue;
					}
					if (state == LifecycleState::Allocated)
					{
						// 典型来源:回 Ready 途中被踢、随后 /ready 失败留在 Allocated。请求就此满足,
						// 不会有 DoAllocate 结果,标记同 Starting / 排空分支一并清掉。
						nonBlockingAllocateKicked = false;
						// 结算点:异步等待方通常已在 DoBackToReady 里放行,这里兜底;解锁后再投递。
						std::vector<PermitDelivery> deliveries;
						ResolveAsyncWaitersLocked(deliveries, false);
						lock.unlock();
						DeliverPermits(std::move(deliveries));
						continue;
					}
					// 标记转存为本次在途的快照并清零:此后成员里只剩 HTTP 在途期间新踢的。
					allocateKickedByNonBlocking = std::exchange(nonBlockingAllocateKicked, false);
					state = LifecycleState::Allocating;
					action = WorkerAction::Allocate;
				}
				else if (readyRequested)
				{
					readyRequested = false;
					if (!activeUnits.empty() || pendingPermits != 0
						|| state != LifecycleState::Allocated)
					{
						continue;
					}
					// 状态必须在释放 mutex、发 HTTP 之前切换。新的创建请求
					// 看到 ReturningToReady 后不能再把本地 Allocated 当成可用。
					state = LifecycleState::ReturningToReady;
					action = WorkerAction::BackToReady;
				}
				else
				{
					const auto now = std::chrono::steady_clock::now();
					if (drainEnabled && now >= nextDrainPoll)
					{
						nextDrainPoll = now + options.drainPollInterval;
						action = WorkerAction::PollDrain;
					}
					else if (now >= nextHealth)
					{
						nextHealth = now + options.healthInterval;
						action = WorkerAction::Health;
					}
					// 否则是虚假唤醒,回到循环顶部按截止时间继续等。
				}
			}
			DeliverPermits(std::move(shutdownDeliveries));

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
