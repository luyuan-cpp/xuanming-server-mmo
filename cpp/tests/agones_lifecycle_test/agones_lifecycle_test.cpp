#include <gtest/gtest.h>

#include <cstdlib>

#include <atomic>
#include <chrono>
#include <condition_variable>
#include <functional>
#include <map>
#include <memory>
#include <mutex>
#include <set>
#include <string>
#include <thread>
#include <vector>

#include "agones/agones_rest_client.h"
#include "agones/agones_scene_lifecycle.h"

// Agones 生命周期状态机的单元测试。
//
// 全部通过注入的 FakeTransport 跑,不需要真的起 Agones sidecar,也不发任何
// 真实网络请求。libcurl 的实现(CurlAgonesHttpTransport)只在定义了
// MMORPG_AGONES_CURL 的 Linux 构建里编译,Windows 测试构建里根本不存在。

namespace
{

	using namespace std::chrono_literals;

	// 记录每次调用,并允许按端点脚本化返回值。
	class FakeTransport final : public agones::HttpTransport
	{
	public:
		agones::HttpResponse Post(const std::string& url,
			const std::string& body,
			const agones::HttpTimeouts& timeouts) override
		{
			return Record(url, timeouts);
		}

		agones::HttpResponse Get(const std::string& url, const agones::HttpTimeouts& timeouts) override
		{
			return Record(url, timeouts);
		}

		// 让指定端点的前 n 次调用返回传输失败,模拟 sidecar 还没起来。
		void FailFirst(const std::string& endpoint, int times)
		{
			std::lock_guard<std::mutex> lock(mutex_);
			failFirst_[endpoint] = times;
		}

		// 让指定端点永远失败。
		void FailAlways(const std::string& endpoint)
		{
			std::lock_guard<std::mutex> lock(mutex_);
			failAlways_.insert(endpoint);
		}

		int CallCount(const std::string& endpoint) const
		{
			std::lock_guard<std::mutex> lock(mutex_);
			const auto it = calls_.find(endpoint);
			return it == calls_.end() ? 0 : it->second;
		}

		int TotalCalls() const
		{
			std::lock_guard<std::mutex> lock(mutex_);
			int total = 0;
			for (const auto& entry : calls_)
			{
				total += entry.second;
			}
			return total;
		}

		bool SawNonZeroTimeouts() const
		{
			std::lock_guard<std::mutex> lock(mutex_);
			return sawConnectTimeout_ && sawTotalTimeout_;
		}

	private:
		static std::string EndpointOf(const std::string& url)
		{
			const auto pos = url.find_last_of('/');
			return pos == std::string::npos ? url : url.substr(pos);
		}

		agones::HttpResponse Record(const std::string& url, const agones::HttpTimeouts& timeouts)
		{
			const std::string endpoint = EndpointOf(url);

			std::lock_guard<std::mutex> lock(mutex_);
			++calls_[endpoint];
			sawConnectTimeout_ = sawConnectTimeout_ || timeouts.connect.count() > 0;
			sawTotalTimeout_ = sawTotalTimeout_ || timeouts.total.count() > 0;

			agones::HttpResponse response;
			if (failAlways_.count(endpoint) > 0)
			{
				response.error = "fake: endpoint always fails";
				return response;
			}

			auto it = failFirst_.find(endpoint);
			if (it != failFirst_.end() && it->second > 0)
			{
				--it->second;
				response.error = "fake: sidecar not up yet";
				return response;
			}

			response.transportOk = true;
			response.statusCode = 200;
			response.body = "{}";
			return response;
		}

		mutable std::mutex mutex_;
		std::map<std::string, int> calls_;
		std::map<std::string, int> failFirst_;
		std::set<std::string> failAlways_;
		bool sawConnectTimeout_ = false;
		bool sawTotalTimeout_ = false;
	};

	// 测试用的快节奏参数:退避和心跳都压到毫秒级,避免测试跑几十秒。
	agones::LifecycleOptions FastOptions()
	{
		agones::LifecycleOptions options;
		options.healthInterval = 20ms;
		options.readyMaxAttempts = 5;
		options.readyInitialBackoff = 5ms;
		options.readyMaxBackoff = 10ms;
		options.allocateWaitTimeout = 1000ms;
		options.allocateMaxAttempts = 2;
		options.allocateRetryBackoff = 5ms;
		return options;
	}

	// 每个用例一个独立实例(不用单例),这样用例之间不会串状态。
	struct Harness
	{
		FakeTransport* transport = nullptr;
		agones::SceneLifecycle lifecycle;

		void Start(agones::LifecycleOptions options = FastOptions())
		{
			auto owned = std::make_unique<FakeTransport>();
			transport = owned.get();
			lifecycle.Start(std::move(owned), "http://127.0.0.1:9358", options);
		}

		// 起之前先拿到 transport,以便脚本化失败。
		FakeTransport* PrepareTransport()
		{
			pending_ = std::make_unique<FakeTransport>();
			transport = pending_.get();
			return transport;
		}

		void StartPrepared(agones::LifecycleOptions options = FastOptions())
		{
			lifecycle.Start(std::move(pending_), "http://127.0.0.1:9358", options);
		}

		~Harness() { lifecycle.Stop(); }

	private:
		std::unique_ptr<FakeTransport> pending_;
	};

	bool WaitUntil(const std::function<bool()>& predicate, std::chrono::milliseconds timeout)
	{
		const auto deadline = std::chrono::steady_clock::now() + timeout;
		while (std::chrono::steady_clock::now() < deadline)
		{
			if (predicate())
			{
				return true;
			}
			std::this_thread::sleep_for(1ms);
		}
		return predicate();
	}

} // namespace

// 禁用模式:一个 HTTP 都不能发,所有 gate 直接放行。
TEST(AgonesLifecycleTest, DisabledModeIssuesNoHttp)
{
	agones::SceneLifecycle lifecycle;
	lifecycle.StartDisabled();

	EXPECT_EQ(agones::LifecycleState::Disabled, lifecycle.State());
	EXPECT_TRUE(lifecycle.EnsureAllocatedBlocking());
	EXPECT_TRUE(lifecycle.EnsureAllocatedNonBlocking());

	lifecycle.OnSceneCreated(1);
	lifecycle.OnSceneDestroyed(1);
	EXPECT_EQ(0, lifecycle.SceneCount());

	lifecycle.Stop();
}

// transport 为空(Windows / 本地构建)也必须退化成 Disabled,而不是崩或卡住。
TEST(AgonesLifecycleTest, NullTransportFallsBackToDisabled)
{
	agones::SceneLifecycle lifecycle;
	lifecycle.Start(nullptr, "http://127.0.0.1:9358", FastOptions());

	EXPECT_EQ(agones::LifecycleState::Disabled, lifecycle.State());
	EXPECT_TRUE(lifecycle.EnsureAllocatedBlocking());
	lifecycle.Stop();
}

// sidecar 晚启动:Ready 必须退避重试,而不是一次失败就永久放弃。
TEST(AgonesLifecycleTest, ReadyRetriesWithBoundedBackoffWhenSidecarIsLate)
{
	Harness h;
	h.PrepareTransport()->FailFirst("/ready", 3);
	h.StartPrepared();

	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));
	EXPECT_EQ(4, h.transport->CallCount("/ready")); // 3 次失败 + 1 次成功
}

// Ready 重试有上限:超过上限必须停在 Starting 并继续 fail-closed,
// 不能"到期后假设成功"。
TEST(AgonesLifecycleTest, ReadyGivesUpAfterMaxAttemptsAndKeepsFailingClosed)
{
	auto options = FastOptions();
	options.readyMaxAttempts = 3;
	options.allocateWaitTimeout = 100ms;

	Harness h;
	h.PrepareTransport()->FailAlways("/ready");
	h.StartPrepared(options);

	EXPECT_TRUE(WaitUntil([&] { return h.transport->CallCount("/ready") >= 3; }, 3000ms));
	std::this_thread::sleep_for(50ms);
	EXPECT_EQ(3, h.transport->CallCount("/ready"));
	EXPECT_EQ(agones::LifecycleState::Starting, h.lifecycle.State());

	// 关键:没 Ready 成功就绝不允许创建房间。
	EXPECT_FALSE(h.lifecycle.EnsureAllocatedBlocking());
	EXPECT_EQ(0, h.transport->CallCount("/allocate"));
}

// 第一个 Scene 之前必须 Allocate,而且只 Allocate 一次。
TEST(AgonesLifecycleTest, FirstSceneAllocatesExactlyOnce)
{
	Harness h;
	h.Start();
	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));

	ASSERT_TRUE(h.lifecycle.EnsureAllocatedBlocking());
	EXPECT_EQ(agones::LifecycleState::Allocated, h.lifecycle.State());
	h.lifecycle.OnSceneCreated(101);

	// 第二个房间不应再发 allocate。
	ASSERT_TRUE(h.lifecycle.EnsureAllocatedBlocking());
	h.lifecycle.OnSceneCreated(102);

	EXPECT_EQ(1, h.transport->CallCount("/allocate"));
	EXPECT_EQ(2, h.lifecycle.SceneCount());
}

// Allocate 失败 -> 必须拒绝创建,不能"先建房间再补 allocate"。
TEST(AgonesLifecycleTest, AllocateFailureFailsClosed)
{
	auto options = FastOptions();
	options.allocateWaitTimeout = 1000ms;

	Harness h;
	h.PrepareTransport()->FailAlways("/allocate");
	h.StartPrepared(options);
	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));

	EXPECT_FALSE(h.lifecycle.EnsureAllocatedBlocking());
	EXPECT_EQ(agones::LifecycleState::Ready, h.lifecycle.State());
	EXPECT_EQ(0, h.lifecycle.SceneCount());
	EXPECT_EQ(options.allocateMaxAttempts, h.transport->CallCount("/allocate"));
}

// 同一个 key 重复上报创建,不重复计数。
TEST(AgonesLifecycleTest, DuplicateCreateDoesNotDoubleCount)
{
	Harness h;
	h.Start();
	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));
	ASSERT_TRUE(h.lifecycle.EnsureAllocatedBlocking());

	h.lifecycle.OnSceneCreated(7);
	h.lifecycle.OnSceneCreated(7);
	EXPECT_EQ(1, h.lifecycle.SceneCount());
}

// 两个房间销毁一个,仍然是 Allocated。
TEST(AgonesLifecycleTest, DestroyingOneOfTwoKeepsAllocated)
{
	Harness h;
	h.Start();
	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));
	ASSERT_TRUE(h.lifecycle.EnsureAllocatedBlocking());

	h.lifecycle.OnSceneCreated(1);
	h.lifecycle.OnSceneCreated(2);
	h.lifecycle.OnSceneDestroyed(1);

	std::this_thread::sleep_for(80ms);
	EXPECT_EQ(1, h.lifecycle.SceneCount());
	EXPECT_EQ(agones::LifecycleState::Allocated, h.lifecycle.State());
	EXPECT_EQ(1, h.transport->CallCount("/ready")); // 只有启动那一次
}

// 销毁最后一个房间 -> 回到 Ready。
TEST(AgonesLifecycleTest, DestroyingLastSceneReturnsToReady)
{
	Harness h;
	h.Start();
	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));
	ASSERT_TRUE(h.lifecycle.EnsureAllocatedBlocking());

	h.lifecycle.OnSceneCreated(1);
	h.lifecycle.OnSceneDestroyed(1);

	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));
	EXPECT_EQ(0, h.lifecycle.SceneCount());
	EXPECT_EQ(2, h.transport->CallCount("/ready")); // 启动 1 次 + 排空 1 次
}

// 重复 Destroy / 销毁未知 key:不重复减、不为负。
TEST(AgonesLifecycleTest, RepeatedDestroyNeverGoesNegative)
{
	Harness h;
	h.Start();
	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));
	ASSERT_TRUE(h.lifecycle.EnsureAllocatedBlocking());

	h.lifecycle.OnSceneCreated(1);
	h.lifecycle.OnSceneDestroyed(1);
	h.lifecycle.OnSceneDestroyed(1);
	h.lifecycle.OnSceneDestroyed(999);

	EXPECT_EQ(0, h.lifecycle.SceneCount());
}

// allocate 成功但实体没建出来 -> 退回 Ready,别占着容量。
TEST(AgonesLifecycleTest, AllocatedWithNoSceneReconcilesBackToReady)
{
	Harness h;
	h.Start();
	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));
	ASSERT_TRUE(h.lifecycle.EnsureAllocatedBlocking());
	ASSERT_EQ(agones::LifecycleState::Allocated, h.lifecycle.State());

	h.lifecycle.ReconcileIdleAfterCreate();

	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));
	EXPECT_EQ(0, h.lifecycle.SceneCount());
}

// gRPC 线程通过 gate 后,CreateScene 还要排队进入 EventLoop。这个窗口也算
// "在途创建":旧的最后一个 Scene 被销毁时不能抢先回 Ready。
TEST(AgonesLifecycleTest, PendingCreatePermitPreventsPrematureReady)
{
	Harness h;
	h.Start();
	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));

	{
		auto firstPermit = h.lifecycle.AcquireCreatePermitBlocking();
		ASSERT_TRUE(firstPermit);
		h.lifecycle.OnSceneCreated(1);
	}

	{
		auto queuedPermit = h.lifecycle.AcquireCreatePermitBlocking();
		ASSERT_TRUE(queuedPermit);

		// 模拟 Destroy 已在 EventLoop 执行,而新 Create 仍在队列里。
		h.lifecycle.OnSceneDestroyed(1);
		std::this_thread::sleep_for(80ms);

		EXPECT_EQ(agones::LifecycleState::Allocated, h.lifecycle.State());
		EXPECT_EQ(1, h.transport->CallCount("/ready")); // 只有启动 Ready

		h.lifecycle.OnSceneCreated(2);
	}

	EXPECT_EQ(1, h.lifecycle.SceneCount());
	EXPECT_EQ(agones::LifecycleState::Allocated, h.lifecycle.State());
}

// /ready 已经发出去时,新创建不能再把本地旧的 Allocated 当成授权。必须等
// sidecar 真正 Ready 后重新 allocate,否则本地与 Agones 状态会分叉。
TEST(AgonesLifecycleTest, CreateDuringReadyTransitionMustReallocate)
{
	struct DrainState
	{
		std::mutex mutex;
		std::condition_variable cv;
		int readyCalls = 0;
		int allocateCalls = 0;
		bool drainEntered = false;
		bool releaseDrain = false;
	};

	class BlockingDrainTransport final : public agones::HttpTransport
	{
	public:
		explicit BlockingDrainTransport(std::shared_ptr<DrainState> state)
			: state_(std::move(state))
		{
		}

		agones::HttpResponse Post(const std::string& url,
			const std::string&,
			const agones::HttpTimeouts&) override
		{
			if (url.find("/ready") != std::string::npos)
			{
				std::unique_lock<std::mutex> lock(state_->mutex);
				++state_->readyCalls;
				if (state_->readyCalls == 2)
				{
					state_->drainEntered = true;
					state_->cv.notify_all();
					state_->cv.wait(lock, [this] { return state_->releaseDrain; });
				}
			}
			else if (url.find("/allocate") != std::string::npos)
			{
				std::lock_guard<std::mutex> lock(state_->mutex);
				++state_->allocateCalls;
			}

			agones::HttpResponse response;
			response.transportOk = true;
			response.statusCode = 200;
			return response;
		}

		agones::HttpResponse Get(const std::string&, const agones::HttpTimeouts&) override
		{
			return agones::HttpResponse{};
		}

	private:
		std::shared_ptr<DrainState> state_;
	};

	auto state = std::make_shared<DrainState>();
	agones::SceneLifecycle lifecycle;
	lifecycle.Start(std::make_unique<BlockingDrainTransport>(state),
		"http://127.0.0.1:9358", FastOptions());
	ASSERT_TRUE(lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));

	{
		auto permit = lifecycle.AcquireCreatePermitBlocking();
		ASSERT_TRUE(permit);
		lifecycle.OnSceneCreated(1);
	}
	lifecycle.OnSceneDestroyed(1);

	bool drainEntered = false;
	{
		std::unique_lock<std::mutex> lock(state->mutex);
		drainEntered = state->cv.wait_for(lock, 3000ms, [&] { return state->drainEntered; });
	}
	if (!drainEntered)
	{
		std::lock_guard<std::mutex> lock(state->mutex);
		state->releaseDrain = true;
	}
	state->cv.notify_all();
	ASSERT_TRUE(drainEntered);
	EXPECT_EQ(agones::LifecycleState::ReturningToReady, lifecycle.State());
	EXPECT_FALSE(lifecycle.AcquireCreatePermitNonBlocking());

	{
		std::lock_guard<std::mutex> lock(state->mutex);
		state->releaseDrain = true;
	}
	state->cv.notify_all();
	ASSERT_TRUE(lifecycle.WaitForState(agones::LifecycleState::Allocated, 3000ms));

	auto nextPermit = lifecycle.AcquireCreatePermitBlocking();
	ASSERT_TRUE(nextPermit);
	lifecycle.OnSceneCreated(2);

	std::lock_guard<std::mutex> lock(state->mutex);
	EXPECT_EQ(2, state->allocateCalls);
}

// 并发抢第一个房间:只能有一次 allocate。
TEST(AgonesLifecycleTest, ConcurrentFirstSceneAllocatesOnce)
{
	Harness h;
	h.Start();
	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));

	constexpr int kThreads = 8;
	std::atomic<int> granted{ 0 };
	std::vector<std::thread> threads;
	threads.reserve(kThreads);
	for (int i = 0; i < kThreads; ++i)
	{
		threads.emplace_back([&h, &granted, i] {
			if (h.lifecycle.EnsureAllocatedBlocking())
			{
				++granted;
				h.lifecycle.OnSceneCreated(static_cast<std::uint64_t>(1000 + i));
			}
		});
	}
	for (auto& t : threads)
	{
		t.join();
	}

	EXPECT_EQ(kThreads, granted.load());
	EXPECT_EQ(kThreads, h.lifecycle.SceneCount());
	EXPECT_EQ(1, h.transport->CallCount("/allocate"));
}

// 并发销毁最后一个房间:计数收敛到 0,不为负,最终回到 Ready。
TEST(AgonesLifecycleTest, ConcurrentDestroyLastSceneConvergesToReady)
{
	Harness h;
	h.Start();
	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));
	ASSERT_TRUE(h.lifecycle.EnsureAllocatedBlocking());

	constexpr int kScenes = 16;
	for (int i = 0; i < kScenes; ++i)
	{
		h.lifecycle.OnSceneCreated(static_cast<std::uint64_t>(i));
	}

	std::vector<std::thread> threads;
	threads.reserve(kScenes);
	for (int i = 0; i < kScenes; ++i)
	{
		threads.emplace_back([&h, i] {
			// 每个 key 故意销毁两次,验证幂等。
			h.lifecycle.OnSceneDestroyed(static_cast<std::uint64_t>(i));
			h.lifecycle.OnSceneDestroyed(static_cast<std::uint64_t>(i));
		});
	}
	for (auto& t : threads)
	{
		t.join();
	}

	EXPECT_EQ(0, h.lifecycle.SceneCount());
	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));
}

// health 心跳跑在独立 worker 上,并且 Stop() 之后必须停下来。
TEST(AgonesLifecycleTest, HealthTicksOnWorkerAndStopsCleanly)
{
	class HealthCountingTransport final : public agones::HttpTransport
	{
	public:
		explicit HealthCountingTransport(std::shared_ptr<std::atomic<int>> healthCalls)
			: healthCalls_(std::move(healthCalls))
		{
		}

		agones::HttpResponse Post(const std::string& url,
			const std::string&,
			const agones::HttpTimeouts&) override
		{
			if (url.find("/health") != std::string::npos)
			{
				++(*healthCalls_);
			}

			agones::HttpResponse response;
			response.transportOk = true;
			response.statusCode = 200;
			return response;
		}

		agones::HttpResponse Get(const std::string&, const agones::HttpTimeouts&) override
		{
			return agones::HttpResponse{};
		}

	private:
		std::shared_ptr<std::atomic<int>> healthCalls_;
	};

	auto healthCalls = std::make_shared<std::atomic<int>>(0);
	agones::SceneLifecycle lifecycle;
	lifecycle.Start(std::make_unique<HealthCountingTransport>(healthCalls),
		"http://127.0.0.1:9358", FastOptions());
	ASSERT_TRUE(lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));

	ASSERT_TRUE(WaitUntil([&] { return healthCalls->load() >= 3; }, 3000ms));

	lifecycle.Stop();
	const int afterStop = healthCalls->load();
	std::this_thread::sleep_for(120ms);
	EXPECT_EQ(afterStop, healthCalls->load());
	EXPECT_EQ(agones::LifecycleState::Stopped, lifecycle.State());
}

// 已启用的 lifecycle 一旦开始停止,不能像本地 Disabled 模式一样继续放行创建。
TEST(AgonesLifecycleTest, StoppedLifecycleFailsClosed)
{
	Harness h;
	h.Start();
	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));
	h.lifecycle.Stop();

	EXPECT_EQ(agones::LifecycleState::Stopped, h.lifecycle.State());
	EXPECT_FALSE(h.lifecycle.EnsureAllocatedBlocking());
	EXPECT_FALSE(h.lifecycle.EnsureAllocatedNonBlocking());
	EXPECT_FALSE(h.lifecycle.AcquireCreatePermitBlocking());
	EXPECT_FALSE(h.lifecycle.AcquireCreatePermitNonBlocking());
}

// 调用方线程被阻塞时,不能牵连到别的线程 —— 这里用"卡住的 transport"模拟
// 一次慢 HTTP,验证有界等待到期后调用方能自己回来,而不是永久挂死。
TEST(AgonesLifecycleTest, SlowHttpDoesNotBlockCallerBeyondTimeout)
{
	class SlowTransport final : public agones::HttpTransport
	{
	public:
		agones::HttpResponse Post(const std::string& url,
			const std::string&,
			const agones::HttpTimeouts&) override
		{
			if (url.find("/ready") != std::string::npos)
			{
				agones::HttpResponse ok;
				ok.transportOk = true;
				ok.statusCode = 200;
				return ok;
			}
			// allocate / health 卡住,模拟 sidecar 无响应。
			std::this_thread::sleep_for(2000ms);
			agones::HttpResponse failed;
			failed.error = "fake: timed out";
			return failed;
		}

		agones::HttpResponse Get(const std::string&, const agones::HttpTimeouts&) override
		{
			return agones::HttpResponse{};
		}
	};

	auto options = FastOptions();
	options.allocateWaitTimeout = 150ms;

	agones::SceneLifecycle lifecycle;
	lifecycle.Start(std::make_unique<SlowTransport>(), "http://127.0.0.1:9358", options);
	ASSERT_TRUE(lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));

	const auto begin = std::chrono::steady_clock::now();
	const bool granted = lifecycle.EnsureAllocatedBlocking();
	const auto elapsed = std::chrono::steady_clock::now() - begin;

	EXPECT_FALSE(granted);
	EXPECT_LT(std::chrono::duration_cast<std::chrono::milliseconds>(elapsed).count(), 1500);

	lifecycle.Stop();
}

// EventLoop 上的调用方用非阻塞版本:立刻返回 false,并且踢起一次异步 allocate。
TEST(AgonesLifecycleTest, NonBlockingGateReturnsImmediatelyAndKicksAllocate)
{
	Harness h;
	h.Start();
	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));

	const auto begin = std::chrono::steady_clock::now();
	const bool first = h.lifecycle.EnsureAllocatedNonBlocking();
	const auto elapsed = std::chrono::steady_clock::now() - begin;

	EXPECT_FALSE(first);
	EXPECT_LT(std::chrono::duration_cast<std::chrono::milliseconds>(elapsed).count(), 50);

	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Allocated, 3000ms));
	EXPECT_TRUE(h.lifecycle.EnsureAllocatedNonBlocking());
	EXPECT_EQ(1, h.transport->CallCount("/allocate"));
}

// 连接超时和总超时都必须传下去,只设一个的话连接阶段挂死仍会吃满预算。
TEST(AgonesLifecycleTest, BothTimeoutsArePropagatedToTransport)
{
	Harness h;
	h.Start();
	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));
	EXPECT_TRUE(h.transport->SawNonZeroTimeouts());
}

namespace
{
	void SetEnvVar(const char* name, const char* value)
	{
#if defined(_WIN32)
		_putenv_s(name, value);
#else
		setenv(name, value, 1);
#endif
	}

	void ClearEnvVar(const char* name)
	{
#if defined(_WIN32)
		_putenv_s(name, "");
#else
		unsetenv(name);
#endif
	}

	struct EnvScope
	{
		~EnvScope()
		{
			ClearEnvVar("AGONES_ENABLED");
			ClearEnvVar("AGONES_SDK_HTTP_PORT");
			ClearEnvVar("AGONES_SDK_HOST");
		}
	};
} // namespace

// 没有 AGONES_SDK_HTTP_PORT = 不在 Agones 里跑,必须判定为未启用。
TEST(AgonesEnvTest, MissingSdkPortMeansDisabled)
{
	EnvScope scope;
	ClearEnvVar("AGONES_SDK_HTTP_PORT");
	ClearEnvVar("AGONES_ENABLED");

	EXPECT_FALSE(agones::ReadAgonesEnv().enabled);
}

// 有端口就启用,BaseUrl 拼装正确,host 可覆盖。
TEST(AgonesEnvTest, SdkPortEnablesAndBuildsBaseUrl)
{
	EnvScope scope;
	ClearEnvVar("AGONES_ENABLED");
	SetEnvVar("AGONES_SDK_HTTP_PORT", "9358");

	const agones::AgonesEnv env = agones::ReadAgonesEnv();
	ASSERT_TRUE(env.enabled);
	EXPECT_EQ("http://127.0.0.1:9358", env.BaseUrl());

	SetEnvVar("AGONES_SDK_HOST", "localhost");
	EXPECT_EQ("http://localhost:9358", agones::ReadAgonesEnv().BaseUrl());
}

// AGONES_ENABLED=0 是显式关闭开关,即使 sidecar 端口存在也必须关掉。
TEST(AgonesEnvTest, ExplicitDisableWinsOverPresentSidecar)
{
	EnvScope scope;
	SetEnvVar("AGONES_SDK_HTTP_PORT", "9358");
	SetEnvVar("AGONES_ENABLED", "0");

	EXPECT_FALSE(agones::ReadAgonesEnv().enabled);
}

// 端口非法不能让状态机"猜一个默认值继续",必须退回未启用。
TEST(AgonesEnvTest, InvalidPortFallsBackToDisabled)
{
	EnvScope scope;
	ClearEnvVar("AGONES_ENABLED");

	SetEnvVar("AGONES_SDK_HTTP_PORT", "not-a-number");
	EXPECT_FALSE(agones::ReadAgonesEnv().enabled);

	SetEnvVar("AGONES_SDK_HTTP_PORT", "70000");
	EXPECT_FALSE(agones::ReadAgonesEnv().enabled);
}
