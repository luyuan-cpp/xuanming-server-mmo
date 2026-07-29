#include "agones/agones_scene_lifecycle.h"

#include <algorithm>
#include <utility>

#include "muduo/base/Logging.h"

namespace agones
{

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

	SceneLifecycle::CreatePermit::~CreatePermit()
	{
		if (owner_ != nullptr && granted_)
		{
			owner_->FinishCreatePermit();
		}
	}

	SceneLifecycle::CreatePermit::CreatePermit(CreatePermit&& other) noexcept
		: owner_(std::exchange(other.owner_, nullptr)), granted_(std::exchange(other.granted_, false))
	{
	}

	SceneLifecycle& SceneLifecycle::Instance()
	{
		static SceneLifecycle instance;
		return instance;
	}

	SceneLifecycle::~SceneLifecycle()
	{
		Stop();
	}

	void SceneLifecycle::StartDisabled()
	{
		std::lock_guard<std::mutex> lock(mutex_);
		state_ = LifecycleState::Disabled;
		LOG_INFO << "[Agones] lifecycle disabled: no HTTP will be issued";
	}

	void SceneLifecycle::Start(std::unique_ptr<HttpTransport> transport,
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

			options_ = options;
			transport_ = std::move(transport);
			client_ = std::make_unique<AgonesRestClient>(std::move(baseUrl), *transport_, options_.timeouts);
			state_ = LifecycleState::Starting;
			stopRequested_ = false;

			LOG_INFO << "[Agones] lifecycle starting: sdk=" << client_->BaseUrl()
					 << " health_interval_ms=" << options_.healthInterval.count()
					 << " ready_max_attempts=" << options_.readyMaxAttempts;
		}

		worker_ = std::thread([this] { WorkerMain(); });
	}

	void SceneLifecycle::Stop()
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

	bool SceneLifecycle::EnsureAllocatedBlocking()
	{
		return EnsureAllocatedBlockingImpl(false);
	}

	bool SceneLifecycle::EnsureAllocatedBlockingImpl(bool reserveCreate)
	{
		std::unique_lock<std::mutex> lock(mutex_);

		if (state_ == LifecycleState::Disabled)
		{
			if (reserveCreate)
			{
				++pendingCreates_;
			}
			return true;
		}
		if (stopRequested_ || state_ == LifecycleState::Stopped)
		{
			LOG_WARN << "[Agones] refusing CreateScene: lifecycle already stopped";
			return false;
		}
		if (state_ == LifecycleState::Allocated)
		{
			if (reserveCreate)
			{
				++pendingCreates_;
				readyRequested_ = false;
			}
			return true;
		}
		if (state_ == LifecycleState::ShuttingDown)
		{
			LOG_WARN << "[Agones] refusing CreateScene: process is shutting down";
			return false;
		}

		const std::uint64_t startGeneration = allocateResultGeneration_;
		allocateRequested_ = true;
		cv_.notify_all();

		// 有界等待。超时不是"假设成功",是拒绝这次创建让调用方重试。
		cv_.wait_for(lock, options_.allocateWaitTimeout, [this, startGeneration] {
			return state_ == LifecycleState::Allocated
				|| allocateResultGeneration_ != startGeneration
				|| stopRequested_;
		});

		if (state_ != LifecycleState::Allocated)
		{
			LOG_ERROR << "[Agones] allocate not confirmed within "
					  << options_.allocateWaitTimeout.count() << "ms, state=" << ToString(state_);
			return false;
		}
		if (reserveCreate)
		{
			++pendingCreates_;
			readyRequested_ = false;
		}
		return true;
	}

	bool SceneLifecycle::EnsureAllocatedNonBlocking()
	{
		return EnsureAllocatedNonBlockingImpl(false);
	}

	bool SceneLifecycle::EnsureAllocatedNonBlockingImpl(bool reserveCreate)
	{
		std::lock_guard<std::mutex> lock(mutex_);

		if (state_ == LifecycleState::Disabled)
		{
			if (reserveCreate)
			{
				++pendingCreates_;
			}
			return true;
		}
		if (stopRequested_ || state_ == LifecycleState::Stopped)
		{
			return false;
		}
		if (state_ == LifecycleState::Allocated)
		{
			if (reserveCreate)
			{
				++pendingCreates_;
				readyRequested_ = false;
			}
			return true;
		}
		if (state_ == LifecycleState::ShuttingDown)
		{
			return false;
		}

		allocateRequested_ = true;
		cv_.notify_all();
		return false;
	}

	SceneLifecycle::CreatePermit SceneLifecycle::AcquireCreatePermitBlocking()
	{
		const bool granted = EnsureAllocatedBlockingImpl(true);
		return CreatePermit(granted ? this : nullptr, granted);
	}

	SceneLifecycle::CreatePermit SceneLifecycle::AcquireCreatePermitNonBlocking()
	{
		const bool granted = EnsureAllocatedNonBlockingImpl(true);
		return CreatePermit(granted ? this : nullptr, granted);
	}

	void SceneLifecycle::FinishCreatePermit()
	{
		std::lock_guard<std::mutex> lock(mutex_);
		if (pendingCreates_ == 0)
		{
			LOG_ERROR << "[Agones] unbalanced CreatePermit release";
			return;
		}

		--pendingCreates_;
		if (pendingCreates_ != 0 || !activeScenes_.empty() || state_ != LifecycleState::Allocated)
		{
			return;
		}

		LOG_WARN << "[Agones] create permit finished with zero scenes, returning to Ready";
		readyRequested_ = true;
		cv_.notify_all();
	}

	void SceneLifecycle::OnSceneCreated(std::uint64_t sceneKey)
	{
		std::lock_guard<std::mutex> lock(mutex_);

		const auto inserted = activeScenes_.insert(sceneKey);
		if (!inserted.second)
		{
			// 上游 CreateScene 是按 scene_id 幂等的,正常不会走到这里。
			// 真走到了说明有路径重复触发事件,记下来但不重复计数。
			LOG_WARN << "[Agones] duplicate OnSceneCreated for key=" << sceneKey
					 << ", scene count stays " << activeScenes_.size();
			return;
		}

		LOG_INFO << "[Agones] scene created key=" << sceneKey
				 << " scene_count=" << activeScenes_.size() << " state=" << ToString(state_);
	}

	void SceneLifecycle::OnSceneDestroyed(std::uint64_t sceneKey)
	{
		std::lock_guard<std::mutex> lock(mutex_);

		if (activeScenes_.erase(sceneKey) == 0)
		{
			// 销毁一个没记过账的 Scene:不减计数,更不能让计数变负。
			LOG_WARN << "[Agones] OnSceneDestroyed for unknown key=" << sceneKey
					 << ", scene count stays " << activeScenes_.size();
			return;
		}

		LOG_INFO << "[Agones] scene destroyed key=" << sceneKey
				 << " scene_count=" << activeScenes_.size() << " state=" << ToString(state_);

		if (!activeScenes_.empty())
		{
			return; // 还有房间,保持 Allocated
		}
		if (pendingCreates_ != 0)
		{
			LOG_INFO << "[Agones] last scene destroyed with " << pendingCreates_
					 << " create permit(s) still in flight; staying Allocated";
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

	void SceneLifecycle::ReconcileIdleAfterCreate()
	{
		std::lock_guard<std::mutex> lock(mutex_);

		if (state_ != LifecycleState::Allocated || !activeScenes_.empty() || pendingCreates_ != 0)
		{
			return;
		}

		// allocate 成功了但实体没建出来(参数非法 / 幂等命中且本节点无房间)。
		// 不退回 Ready 的话,这个进程会一直占着 Allocated 却零房间。
		LOG_WARN << "[Agones] allocated but no scene was created, returning to Ready";
		readyRequested_ = true;
		cv_.notify_all();
	}

	void SceneLifecycle::RequestShutdown()
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

	LifecycleState SceneLifecycle::State() const
	{
		std::lock_guard<std::mutex> lock(mutex_);
		return state_;
	}

	int SceneLifecycle::SceneCount() const
	{
		std::lock_guard<std::mutex> lock(mutex_);
		return static_cast<int>(activeScenes_.size());
	}

	bool SceneLifecycle::WaitForState(LifecycleState expected, std::chrono::milliseconds timeout)
	{
		std::unique_lock<std::mutex> lock(mutex_);
		return cv_.wait_for(lock, timeout, [this, expected] { return state_ == expected; });
	}

	bool SceneLifecycle::SleepInterruptible(std::chrono::milliseconds duration)
	{
		std::unique_lock<std::mutex> lock(mutex_);
		cv_.wait_for(lock, duration, [this] { return stopRequested_; });
		return !stopRequested_;
	}

	void SceneLifecycle::RunInitialReady()
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
				// 这里只负责把状态推到 Ready,allocate 由主循环消费挂起请求。
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

		// 放弃。刻意留在 Starting 而不是假装 Ready —— 这样所有 CreateScene
		// 都会 fail-closed,而不是让玩家进到一个 Agones 不认识的进程里。
		LOG_ERROR << "[Agones] gave up on POST /ready after " << options_.readyMaxAttempts
				  << " attempts; scene creation will keep failing closed";
	}

	void SceneLifecycle::DoAllocate()
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
			if (allocated)
			{
				LOG_INFO << "[Agones] ALLOCATED";
			}
			else
			{
				LOG_ERROR << "[Agones] allocate failed, staying Ready; CreateScene will fail closed";
			}
		}
		cv_.notify_all();
	}

	void SceneLifecycle::DoBackToReady()
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
		LOG_INFO << "[Agones] back to READY (0 scenes)";
		cv_.notify_all();
	}

	void SceneLifecycle::DoShutdown()
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

	void SceneLifecycle::WorkerMain()
	{
		RunInitialReady();

		for (;;)
		{
			WorkerAction action = WorkerAction::Health;

			{
				std::unique_lock<std::mutex> lock(mutex_);
				cv_.wait_for(lock, options_.healthInterval, [this] {
					return stopRequested_ || allocateRequested_ || readyRequested_ || shutdownRequested_;
				});

				if (stopRequested_)
				{
					return;
				}

				// 优先级:shutdown > allocate > 回 Ready > health。
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
					readyRequested_ = false;
					if (state_ == LifecycleState::Starting)
					{
						// Ready 还没成功,现在 allocate 一定失败。丢回去等下一轮,
						// 调用方的有界等待会超时并 fail-closed。
						continue;
					}
					if (state_ == LifecycleState::Allocated)
					{
						continue;
					}
					state_ = LifecycleState::Allocating;
					action = WorkerAction::Allocate;
				}
				else if (readyRequested_)
				{
					readyRequested_ = false;
					if (!activeScenes_.empty() || pendingCreates_ != 0
						|| state_ != LifecycleState::Allocated)
					{
						continue;
					}
					// 状态必须在释放 mutex、发 HTTP 之前切换。新 CreateScene
					// 看到 ReturningToReady 后不能再把本地 Allocated 当成可用。
					state_ = LifecycleState::ReturningToReady;
					action = WorkerAction::BackToReady;
				}
			}

			switch (action)
			{
			case WorkerAction::Shutdown:
				DoShutdown();
				break;
			case WorkerAction::Allocate:
				DoAllocate();
				break;
			case WorkerAction::BackToReady:
				DoBackToReady();
				break;
			case WorkerAction::Health:
			{
				const HttpResponse response = client_->Health();
				if (!response.Ok())
				{
					LOG_WARN << "[Agones] POST /health failed status=" << response.statusCode
							 << " error=" << response.error;
				}
				break;
			}
			}
		}
	}

} // namespace agones
