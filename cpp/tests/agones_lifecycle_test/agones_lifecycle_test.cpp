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

#include "agones/agones_client_endpoint_source.h"
#include "agones/agones_gameserver_lifecycle.h"
#include "agones/agones_gameserver_status.h"
#include "agones/agones_rest_client.h"

// Agones 生命周期状态机、GameServer JSON 解析与客户端地址来源的单元测试。
//
// 全部通过注入的 FakeTransport 跑,不需要真的起 Agones sidecar,也不发任何
// 真实网络请求。libcurl 的实现(CurlAgonesHttpTransport)只在定义了
// MMORPG_AGONES_CURL 的 Linux 构建里编译,Windows 测试构建里根本不存在。
//
// 2026-09-29:被测代码由 cpp/nodes/scene/agones/ 下沉到 cpp/libs/engine/infra/agones/,
// 并按 D85 改名(SceneLifecycle -> GameServerLifecycle 等);工程名不变。

namespace
{

	using namespace std::chrono_literals;

	// 假 sidecar 的状态:记录每次调用,并允许按端点脚本化返回值。
	// 由测试与 FakeTransport 以 shared_ptr 共享 —— transport 的所有权交给了被测对象,
	// 测试仍要能脚本化与断言,又不能持有指向 transport 的裸指针成员。
	class FakeSidecar
	{
	public:
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

		// 所有请求里见过的总超时的最大 / 最小值(毫秒);还没有请求时都是 -1。
		std::chrono::milliseconds::rep MaxTotalTimeoutMs() const
		{
			std::lock_guard<std::mutex> lock(mutex_);
			return maxTotalTimeoutMs_;
		}

		std::chrono::milliseconds::rep MinTotalTimeoutMs() const
		{
			std::lock_guard<std::mutex> lock(mutex_);
			return minTotalTimeoutMs_;
		}

		// 指定端点成功时的响应体(默认 "{}")。用于脚本化 GET /gameserver。
		void SetBody(const std::string& endpoint, std::string body)
		{
			std::lock_guard<std::mutex> lock(mutex_);
			bodies_[endpoint] = std::move(body);
		}

		agones::HttpResponse Record(const std::string& url, const agones::HttpTimeouts& timeouts)
		{
			const std::string endpoint = EndpointOf(url);

			std::lock_guard<std::mutex> lock(mutex_);
			++calls_[endpoint];
			sawConnectTimeout_ = sawConnectTimeout_ || timeouts.connect.count() > 0;
			sawTotalTimeout_ = sawTotalTimeout_ || timeouts.total.count() > 0;
			const std::chrono::milliseconds::rep totalMs = timeouts.total.count();
			if (totalMs > maxTotalTimeoutMs_)
			{
				maxTotalTimeoutMs_ = totalMs;
			}
			if (minTotalTimeoutMs_ < 0 || totalMs < minTotalTimeoutMs_)
			{
				minTotalTimeoutMs_ = totalMs;
			}

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
			const auto body = bodies_.find(endpoint);
			response.body = body == bodies_.end() ? "{}" : body->second;
			return response;
		}

	private:
		static std::string EndpointOf(const std::string& url)
		{
			const auto pos = url.find_last_of('/');
			return pos == std::string::npos ? url : url.substr(pos);
		}

		mutable std::mutex mutex_;
		std::map<std::string, int> calls_;
		std::map<std::string, int> failFirst_;
		std::set<std::string> failAlways_;
		std::map<std::string, std::string> bodies_;
		bool sawConnectTimeout_ = false;
		bool sawTotalTimeout_ = false;
		std::chrono::milliseconds::rep maxTotalTimeoutMs_ = -1;
		std::chrono::milliseconds::rep minTotalTimeoutMs_ = -1;
	};

	// 被测对象独占的传输层,所有调用转给共享的 FakeSidecar。
	class FakeTransport final : public agones::HttpTransport
	{
	public:
		explicit FakeTransport(std::shared_ptr<FakeSidecar> sidecar)
			: sidecar_(std::move(sidecar))
		{
		}

		agones::HttpResponse Post(const std::string& url,
			const std::string& body,
			const agones::HttpTimeouts& timeouts) override
		{
			return sidecar_->Record(url, timeouts);
		}

		agones::HttpResponse Get(const std::string& url, const agones::HttpTimeouts& timeouts) override
		{
			return sidecar_->Record(url, timeouts);
		}

	private:
		std::shared_ptr<FakeSidecar> sidecar_;
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
	// 需要脚本化失败的用例在 Start() 之前先对 sidecar 下指令。
	struct Harness
	{
		std::shared_ptr<FakeSidecar> sidecar = std::make_shared<FakeSidecar>();
		agones::GameServerLifecycle lifecycle;

		void Start(agones::LifecycleOptions options = FastOptions())
		{
			lifecycle.Start(std::make_unique<FakeTransport>(sidecar), "http://127.0.0.1:9358", options);
		}

		~Harness() { lifecycle.Stop(); }
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
	agones::GameServerLifecycle lifecycle;
	lifecycle.StartDisabled();

	EXPECT_EQ(agones::LifecycleState::Disabled, lifecycle.State());
	EXPECT_TRUE(lifecycle.EnsureAllocatedBlocking());
	EXPECT_TRUE(lifecycle.EnsureAllocatedNonBlocking());

	lifecycle.OnUnitCreated(1);
	lifecycle.OnUnitDestroyed(1);
	EXPECT_EQ(0, lifecycle.UnitCount());

	lifecycle.Stop();
}

// transport 为空(Windows / 本地构建)也必须退化成 Disabled,而不是崩或卡住。
TEST(AgonesLifecycleTest, NullTransportFallsBackToDisabled)
{
	agones::GameServerLifecycle lifecycle;
	lifecycle.Start(nullptr, "http://127.0.0.1:9358", FastOptions());

	EXPECT_EQ(agones::LifecycleState::Disabled, lifecycle.State());
	EXPECT_TRUE(lifecycle.EnsureAllocatedBlocking());
	lifecycle.Stop();
}

// sidecar 晚启动:Ready 必须退避重试,而不是一次失败就永久放弃。
TEST(AgonesLifecycleTest, ReadyRetriesWithBoundedBackoffWhenSidecarIsLate)
{
	Harness h;
	h.sidecar->FailFirst("/ready", 3);
	h.Start();

	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));
	EXPECT_EQ(4, h.sidecar->CallCount("/ready")); // 3 次失败 + 1 次成功
}

// Ready 重试有上限:超过上限必须停在 Starting 并继续 fail-closed,
// 不能"到期后假设成功"。
TEST(AgonesLifecycleTest, ReadyGivesUpAfterMaxAttemptsAndKeepsFailingClosed)
{
	auto options = FastOptions();
	options.readyMaxAttempts = 3;
	options.allocateWaitTimeout = 100ms;

	Harness h;
	h.sidecar->FailAlways("/ready");
	h.Start(options);

	EXPECT_TRUE(WaitUntil([&] { return h.sidecar->CallCount("/ready") >= 3; }, 3000ms));
	std::this_thread::sleep_for(50ms);
	EXPECT_EQ(3, h.sidecar->CallCount("/ready"));
	EXPECT_EQ(agones::LifecycleState::Starting, h.lifecycle.State());

	// 关键:没 Ready 成功就绝不允许创建房间。
	EXPECT_FALSE(h.lifecycle.EnsureAllocatedBlocking());
	EXPECT_EQ(0, h.sidecar->CallCount("/allocate"));
}

// 第一个 Scene 之前必须 Allocate,而且只 Allocate 一次。
TEST(AgonesLifecycleTest, FirstSceneAllocatesExactlyOnce)
{
	Harness h;
	h.Start();
	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));

	ASSERT_TRUE(h.lifecycle.EnsureAllocatedBlocking());
	EXPECT_EQ(agones::LifecycleState::Allocated, h.lifecycle.State());
	h.lifecycle.OnUnitCreated(101);

	// 第二个房间不应再发 allocate。
	ASSERT_TRUE(h.lifecycle.EnsureAllocatedBlocking());
	h.lifecycle.OnUnitCreated(102);

	EXPECT_EQ(1, h.sidecar->CallCount("/allocate"));
	EXPECT_EQ(2, h.lifecycle.UnitCount());
}

// Allocate 失败 -> 必须拒绝创建,不能"先建房间再补 allocate"。
TEST(AgonesLifecycleTest, AllocateFailureFailsClosed)
{
	auto options = FastOptions();
	options.allocateWaitTimeout = 1000ms;

	Harness h;
	h.sidecar->FailAlways("/allocate");
	h.Start(options);
	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));

	EXPECT_FALSE(h.lifecycle.EnsureAllocatedBlocking());
	EXPECT_EQ(agones::LifecycleState::Ready, h.lifecycle.State());
	EXPECT_EQ(0, h.lifecycle.UnitCount());
	EXPECT_EQ(options.allocateMaxAttempts, h.sidecar->CallCount("/allocate"));
}

// 同一个 key 重复上报创建,不重复计数。
TEST(AgonesLifecycleTest, DuplicateCreateDoesNotDoubleCount)
{
	Harness h;
	h.Start();
	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));
	ASSERT_TRUE(h.lifecycle.EnsureAllocatedBlocking());

	h.lifecycle.OnUnitCreated(7);
	h.lifecycle.OnUnitCreated(7);
	EXPECT_EQ(1, h.lifecycle.UnitCount());
}

// 两个房间销毁一个,仍然是 Allocated。
TEST(AgonesLifecycleTest, DestroyingOneOfTwoKeepsAllocated)
{
	Harness h;
	h.Start();
	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));
	ASSERT_TRUE(h.lifecycle.EnsureAllocatedBlocking());

	h.lifecycle.OnUnitCreated(1);
	h.lifecycle.OnUnitCreated(2);
	h.lifecycle.OnUnitDestroyed(1);

	std::this_thread::sleep_for(80ms);
	EXPECT_EQ(1, h.lifecycle.UnitCount());
	EXPECT_EQ(agones::LifecycleState::Allocated, h.lifecycle.State());
	EXPECT_EQ(1, h.sidecar->CallCount("/ready")); // 只有启动那一次
}

// 销毁最后一个房间 -> 回到 Ready。
TEST(AgonesLifecycleTest, DestroyingLastSceneReturnsToReady)
{
	Harness h;
	h.Start();
	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));
	ASSERT_TRUE(h.lifecycle.EnsureAllocatedBlocking());

	h.lifecycle.OnUnitCreated(1);
	h.lifecycle.OnUnitDestroyed(1);

	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));
	EXPECT_EQ(0, h.lifecycle.UnitCount());
	EXPECT_EQ(2, h.sidecar->CallCount("/ready")); // 启动 1 次 + 排空 1 次
}

// 重复 Destroy / 销毁未知 key:不重复减、不为负。
TEST(AgonesLifecycleTest, RepeatedDestroyNeverGoesNegative)
{
	Harness h;
	h.Start();
	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));
	ASSERT_TRUE(h.lifecycle.EnsureAllocatedBlocking());

	h.lifecycle.OnUnitCreated(1);
	h.lifecycle.OnUnitDestroyed(1);
	h.lifecycle.OnUnitDestroyed(1);
	h.lifecycle.OnUnitDestroyed(999);

	EXPECT_EQ(0, h.lifecycle.UnitCount());
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
	EXPECT_EQ(0, h.lifecycle.UnitCount());
}

// gRPC 线程通过 gate 后,CreateScene 还要排队进入 EventLoop。这个窗口也算
// "在途创建":旧的最后一个 Scene 被销毁时不能抢先回 Ready。
TEST(AgonesLifecycleTest, PendingAllocationPermitPreventsPrematureReady)
{
	Harness h;
	h.Start();
	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));

	{
		auto firstPermit = h.lifecycle.AcquireAllocationPermitBlocking();
		ASSERT_TRUE(firstPermit);
		h.lifecycle.OnUnitCreated(1);
	}

	{
		auto queuedPermit = h.lifecycle.AcquireAllocationPermitBlocking();
		ASSERT_TRUE(queuedPermit);

		// 模拟 Destroy 已在 EventLoop 执行,而新 Create 仍在队列里。
		h.lifecycle.OnUnitDestroyed(1);
		std::this_thread::sleep_for(80ms);

		EXPECT_EQ(agones::LifecycleState::Allocated, h.lifecycle.State());
		EXPECT_EQ(1, h.sidecar->CallCount("/ready")); // 只有启动 Ready

		h.lifecycle.OnUnitCreated(2);
	}

	EXPECT_EQ(1, h.lifecycle.UnitCount());
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
	agones::GameServerLifecycle lifecycle;
	lifecycle.Start(std::make_unique<BlockingDrainTransport>(state),
		"http://127.0.0.1:9358", FastOptions());
	ASSERT_TRUE(lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));

	{
		auto permit = lifecycle.AcquireAllocationPermitBlocking();
		ASSERT_TRUE(permit);
		lifecycle.OnUnitCreated(1);
	}
	lifecycle.OnUnitDestroyed(1);

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
	EXPECT_FALSE(lifecycle.AcquireAllocationPermitNonBlocking());

	{
		std::lock_guard<std::mutex> lock(state->mutex);
		state->releaseDrain = true;
	}
	state->cv.notify_all();
	ASSERT_TRUE(lifecycle.WaitForState(agones::LifecycleState::Allocated, 3000ms));

	auto nextPermit = lifecycle.AcquireAllocationPermitBlocking();
	ASSERT_TRUE(nextPermit);
	lifecycle.OnUnitCreated(2);

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
				h.lifecycle.OnUnitCreated(static_cast<std::uint64_t>(1000 + i));
			}
		});
	}
	for (auto& t : threads)
	{
		t.join();
	}

	EXPECT_EQ(kThreads, granted.load());
	EXPECT_EQ(kThreads, h.lifecycle.UnitCount());
	EXPECT_EQ(1, h.sidecar->CallCount("/allocate"));
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
		h.lifecycle.OnUnitCreated(static_cast<std::uint64_t>(i));
	}

	std::vector<std::thread> threads;
	threads.reserve(kScenes);
	for (int i = 0; i < kScenes; ++i)
	{
		threads.emplace_back([&h, i] {
			// 每个 key 故意销毁两次,验证幂等。
			h.lifecycle.OnUnitDestroyed(static_cast<std::uint64_t>(i));
			h.lifecycle.OnUnitDestroyed(static_cast<std::uint64_t>(i));
		});
	}
	for (auto& t : threads)
	{
		t.join();
	}

	EXPECT_EQ(0, h.lifecycle.UnitCount());
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
	agones::GameServerLifecycle lifecycle;
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
	EXPECT_FALSE(h.lifecycle.AcquireAllocationPermitBlocking());
	EXPECT_FALSE(h.lifecycle.AcquireAllocationPermitNonBlocking());
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

	agones::GameServerLifecycle lifecycle;
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
	EXPECT_EQ(1, h.sidecar->CallCount("/allocate"));
}

// 连接超时和总超时都必须传下去,只设一个的话连接阶段挂死仍会吃满预算。
TEST(AgonesLifecycleTest, BothTimeoutsArePropagatedToTransport)
{
	Harness h;
	h.Start();
	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));
	EXPECT_TRUE(h.sidecar->SawNonZeroTimeouts());
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

// ---------------------------------------------------------------------------
// 2026-09-29 集群外入口 D81/D83/D84/D85 新增用例
// ---------------------------------------------------------------------------

namespace
{
	// 排空标签:与 battle main 传入的 drainLabelKey、Fleet allocationOverflow 同一个字面量。
	const char* const kDrainLabel = "mmorpg.io/drain";

	std::string GameServerWithLabels(const std::string& labelsJson)
	{
		return std::string(R"({"object_meta":{"name":"gs-1","labels":)") + labelsJson
			+ R"(},"status":{"state":"Allocated","address":"10.0.0.5","ports":[]}})";
	}

	agones::LifecycleOptions DrainOptions()
	{
		auto options = FastOptions();
		options.drainLabelKey = kDrainLabel;
		options.drainPollInterval = 10ms;
		return options;
	}

	agones::ClientEndpointSourceOptions FastSourceOptions(int maxAttempts)
	{
		agones::ClientEndpointSourceOptions options;
		options.maxAttempts = maxAttempts;
		options.initialBackoff = 1ms;
		options.maxBackoff = 2ms;
		return options;
	}

	const char* const kScheduledGameServerJson =
		R"({"object_meta":{"name":"gs-1"},"status":{"state":"Scheduled","address":"203.0.113.7","ports":[{"name":"client","port":7005}]}})";
} // namespace

// --- GET /gameserver 解析 ---------------------------------------------------

// sidecar REST 网关按 proto 原名输出(object_meta)。
TEST(AgonesGameServerStatusTest, ParsesProtoNameShape)
{
	const std::string json = R"({
		"object_meta": {"name": "gs-1", "creation_timestamp": "1700000000",
			"labels": {"mmorpg.io/drain": "true", "app": "battle"}},
		"status": {"state": "Allocated", "address": "10.0.0.5",
			"ports": [{"name": "client", "port": 7003}, {"name": "metrics", "port": 7004}]}
	})";
	agones::GameServerView view;
	std::string error;
	ASSERT_TRUE(agones::ParseGameServer(json, view, error)) << error;

	EXPECT_EQ("Allocated", view.state);
	EXPECT_EQ("10.0.0.5", view.address);
	ASSERT_EQ(1u, view.ports.count("client"));
	EXPECT_EQ(7003u, view.ports.at("client"));
	EXPECT_EQ(7004u, view.ports.at("metrics"));
	ASSERT_EQ(1u, view.labels.count(kDrainLabel));
	EXPECT_EQ("true", view.labels.at(kDrainLabel));
	EXPECT_EQ("battle", view.labels.at("app"));
}

// 标准 protojson 驼峰输出(objectMeta);端口为数字串也接受,未知字段忽略。
TEST(AgonesGameServerStatusTest, ParsesCamelCaseShape)
{
	const std::string json = R"({
		"objectMeta": {"name": "gs-1", "creationTimestamp": "1700000000", "labels": {"app": "battle"}},
		"spec": {"health": {"periodSeconds": 5}},
		"status": {"state": "Ready", "address": "gs-1.example.com",
			"addresses": [{"type": "ExternalDNS", "address": "gs-1.example.com"}],
			"ports": [{"name": "client", "port": "7009"}]}
	})";
	agones::GameServerView view;
	std::string error;
	ASSERT_TRUE(agones::ParseGameServer(json, view, error)) << error;

	EXPECT_EQ("Ready", view.state);
	EXPECT_EQ("gs-1.example.com", view.address);
	EXPECT_EQ(7009u, view.ports.at("client"));
	EXPECT_EQ("battle", view.labels.at("app"));
	EXPECT_EQ(0u, view.labels.count(kDrainLabel));
}

// 地址为空、没有端口、字段为 null 或整体缺省:解析成功,由调用方判定能不能用。
TEST(AgonesGameServerStatusTest, EmptyAddressAndMissingPortsParseAsEmpty)
{
	agones::GameServerView view;
	std::string error;

	const std::string emptyAddress =
		R"({"object_meta":{"labels":{}},"status":{"state":"Scheduled","address":"","ports":[]}})";
	ASSERT_TRUE(agones::ParseGameServer(emptyAddress, view, error)) << error;
	EXPECT_EQ("Scheduled", view.state);
	EXPECT_TRUE(view.address.empty());
	EXPECT_TRUE(view.ports.empty());
	EXPECT_TRUE(view.labels.empty());

	const std::string nullFields =
		R"({"object_meta":null,"status":{"state":"Scheduled","address":null,"ports":null}})";
	ASSERT_TRUE(agones::ParseGameServer(nullFields, view, error)) << error;
	EXPECT_TRUE(view.address.empty());
	EXPECT_TRUE(view.ports.empty());

	ASSERT_TRUE(agones::ParseGameServer("{}", view, error)) << error;
	EXPECT_TRUE(view.state.empty());
	EXPECT_TRUE(view.address.empty());
}

// 格式错误 / 顶层不是对象 / 已知字段类型不符:返回 false、给出原因,且不改动 view。
TEST(AgonesGameServerStatusTest, MalformedJsonFailsAndKeepsView)
{
	agones::GameServerView view;
	view.address = "keep";

	const char* const badInputs[] = {
		R"({"status": {)",
		"not json at all",
		"[1, 2, 3]",
		R"({"status":{"ports":{}}})",
		R"({"status":{"address":42}})",
		R"({"object_meta":{"labels":["a"]}})",
		R"({"object_meta":{"labels":{"mmorpg.io/drain":true}}})",
		R"({"status":{"ports":[{"name":"client","port":70000}]}})",
		R"({"status":{"ports":[{"name":"client","port":7000.5}]}})",
		R"({"status":{"ports":[{"name":"client"}]}})",
	};
	for (const char* input : badInputs)
	{
		std::string error;
		EXPECT_FALSE(agones::ParseGameServer(input, view, error)) << input;
		EXPECT_FALSE(error.empty()) << input;
		EXPECT_EQ("keep", view.address) << input;
	}
}

// 排空轮询只解析元数据段:status 形状坏掉不拦标签判定;驼峰写法同样认;
// 有元数据段但没有 labels 键是合法的"没有标签"。
TEST(AgonesGameServerStatusTest, LabelsParseIgnoresStatusShape)
{
	std::map<std::string, std::string> labels;
	std::string error;

	const std::string brokenPorts =
		R"({"object_meta":{"labels":{"mmorpg.io/drain":"true"}},"status":{"ports":[{"name":"client"}]}})";
	ASSERT_TRUE(agones::ParseGameServerLabels(brokenPorts, labels, error)) << error;
	ASSERT_EQ(1u, labels.count(kDrainLabel));
	EXPECT_EQ("true", labels.at(kDrainLabel));

	// 对照:完整解析会因同一个坏掉的 ports 项失败。
	agones::GameServerView view;
	EXPECT_FALSE(agones::ParseGameServer(brokenPorts, view, error));

	ASSERT_TRUE(agones::ParseGameServerLabels(
		R"({"objectMeta":{"labels":{"app":"battle"}},"status":42})", labels, error)) << error;
	EXPECT_EQ(0u, labels.count(kDrainLabel));
	EXPECT_EQ("battle", labels.at("app"));

	ASSERT_TRUE(agones::ParseGameServerLabels(R"({"object_meta":{"name":"gs-1"}})", labels, error)) << error;
	EXPECT_TRUE(labels.empty());
}

// 元数据段缺失 / 为 null / 换了键名 / 不是对象、labels 形状不符、JSON 坏掉:返回 false 且不改动 labels。
// 元数据段缺失绝不能等同于"标签被移除"(那会把排空中的实例静默放回接单元)。
TEST(AgonesGameServerStatusTest, LabelsParseFailsWithoutMetadata)
{
	std::map<std::string, std::string> labels{ { kDrainLabel, "true" } };

	const char* const badInputs[] = {
		R"({"status":{"state":"Allocated","address":"10.0.0.5","ports":[]}})",
		R"({"object_meta":null,"status":{"state":"Allocated"}})",
		R"({"metadata":{"labels":{}}})",
		R"({"object_meta":"gs-1"})",
		R"({"object_meta":{"labels":["a"]}})",
		R"({"object_meta":{"labels":{"mmorpg.io/drain":true}}})",
		"{broken",
		"[1, 2, 3]",
	};
	for (const char* input : badInputs)
	{
		std::string error;
		EXPECT_FALSE(agones::ParseGameServerLabels(input, labels, error)) << input;
		EXPECT_FALSE(error.empty()) << input;
		ASSERT_EQ(1u, labels.count(kDrainLabel)) << input;
	}
}

// --- AgonesClientEndpointSource(CLIENT_ENDPOINT_SOURCE=agones) ------------

// sidecar 晚启动:先失败 N 次再成功,拿到 status.address + "client" 端口。
TEST(AgonesClientEndpointSourceTest, RetriesUntilSidecarAnswers)
{
	auto sidecar = std::make_shared<FakeSidecar>();
	sidecar->FailFirst("/gameserver", 3);
	sidecar->SetBody("/gameserver", kScheduledGameServerJson);

	// 经接口调用,与 Node::InitRpcServer 的用法一致。
	std::unique_ptr<client_endpoint::ExternalSource> source =
		std::make_unique<agones::AgonesClientEndpointSource>(
			std::make_unique<FakeTransport>(sidecar), "http://127.0.0.1:9358", FastSourceOptions(10));

	client_endpoint::Advertised advertised;
	std::string error;
	ASSERT_TRUE(source->Fetch(advertised, error)) << error;
	EXPECT_EQ("203.0.113.7", advertised.host);
	EXPECT_EQ(7005u, advertised.port);
	EXPECT_EQ(4, sidecar->CallCount("/gameserver")); // 3 次失败 + 1 次成功
}

// sidecar 一直不可达:有界重试耗尽后失败(调用方据此 LOG_FATAL),不无限等。
TEST(AgonesClientEndpointSourceTest, GivesUpAfterMaxAttempts)
{
	auto sidecar = std::make_shared<FakeSidecar>();
	sidecar->FailAlways("/gameserver");
	agones::AgonesClientEndpointSource source(
		std::make_unique<FakeTransport>(sidecar), "http://127.0.0.1:9358", FastSourceOptions(5));

	client_endpoint::Advertised advertised;
	std::string error;
	EXPECT_FALSE(source.Fetch(advertised, error));
	EXPECT_FALSE(error.empty());
	EXPECT_EQ(5, sidecar->CallCount("/gameserver"));
	EXPECT_TRUE(advertised.host.empty());
	EXPECT_EQ(0u, advertised.port);
}

// 地址为空 / 没有 "client" 端口 / JSON 坏掉:都算失败并重试,耗尽后失败。
TEST(AgonesClientEndpointSourceTest, UnusableStatusIsRetriedThenFails)
{
	const char* const unusableBodies[] = {
		R"({"status":{"state":"Scheduled","address":"","ports":[{"name":"client","port":7005}]}})",
		R"({"status":{"state":"Scheduled","address":"203.0.113.7","ports":[{"name":"default","port":7005}]}})",
		R"({"status":{"state":"Scheduled","address":"203.0.113.7","ports":[{"name":"client","port":0}]}})",
		R"({"status":{"state":"Scheduled","address":"203.0.113.7","ports":[]}})",
		"{broken",
	};
	for (const char* body : unusableBodies)
	{
		auto sidecar = std::make_shared<FakeSidecar>();
		sidecar->SetBody("/gameserver", body);
		agones::AgonesClientEndpointSource source(
			std::make_unique<FakeTransport>(sidecar), "http://127.0.0.1:9358", FastSourceOptions(3));

		client_endpoint::Advertised advertised;
		std::string error;
		EXPECT_FALSE(source.Fetch(advertised, error)) << body;
		EXPECT_FALSE(error.empty()) << body;
		EXPECT_EQ(3, sidecar->CallCount("/gameserver")) << body;
	}
}

// 次数上限形同虚设时,时间预算兜底:按单调时钟截止时间收口,error 写明预算耗尽。
TEST(AgonesClientEndpointSourceTest, GivesUpWhenTimeBudgetIsExhausted)
{
	auto sidecar = std::make_shared<FakeSidecar>();
	sidecar->FailAlways("/gameserver");
	auto options = FastSourceOptions(1000000);
	options.totalBudget = 20ms;
	agones::AgonesClientEndpointSource source(
		std::make_unique<FakeTransport>(sidecar), "http://127.0.0.1:9358", options);

	client_endpoint::Advertised advertised;
	std::string error;
	const auto begin = std::chrono::steady_clock::now();
	EXPECT_FALSE(source.Fetch(advertised, error));
	const auto elapsedMs = std::chrono::duration_cast<std::chrono::milliseconds>(
		std::chrono::steady_clock::now() - begin).count();

	EXPECT_LT(elapsedMs, 1000);
	EXPECT_NE(std::string::npos, error.find("time budget")) << error;
	EXPECT_GE(sidecar->CallCount("/gameserver"), 1);
	EXPECT_LT(sidecar->CallCount("/gameserver"), 1000000);
}

// 单次 GET 的超时截到剩余预算:sidecar 可连却不响应时,不会每次都吃满 timeouts.total。
TEST(AgonesClientEndpointSourceTest, ClampsPerAttemptTimeoutToRemainingBudget)
{
	auto sidecar = std::make_shared<FakeSidecar>();
	sidecar->FailAlways("/gameserver");
	auto options = FastSourceOptions(1000000);
	options.timeouts.connect = 500ms;
	options.timeouts.total = 2000ms;
	options.totalBudget = 100ms;
	agones::AgonesClientEndpointSource source(
		std::make_unique<FakeTransport>(sidecar), "http://127.0.0.1:9358", options);

	client_endpoint::Advertised advertised;
	std::string error;
	EXPECT_FALSE(source.Fetch(advertised, error));

	ASSERT_GE(sidecar->CallCount("/gameserver"), 1);
	EXPECT_LE(sidecar->MaxTotalTimeoutMs(), 100);
	// curl 的 0 表示不限时,截断后也绝不能传 0 下去。
	EXPECT_GE(sidecar->MinTotalTimeoutMs(), 1);
}

// 没有传输层(Windows 构建)不崩,直接失败。
TEST(AgonesClientEndpointSourceTest, NullTransportFailsClosed)
{
	agones::AgonesClientEndpointSource source(nullptr, "http://127.0.0.1:9358", FastSourceOptions(3));

	client_endpoint::Advertised advertised;
	std::string error;
	EXPECT_FALSE(source.Fetch(advertised, error));
	EXPECT_FALSE(error.empty());
}

// 不在 Agones 里跑(无 sidecar 端口)时工厂返回 nullptr,Node 据此 fail-closed。
TEST(AgonesClientEndpointSourceTest, FactoryReturnsNullWhenSdkDisabled)
{
	EnvScope scope;
	ClearEnvVar("AGONES_SDK_HTTP_PORT");
	ClearEnvVar("AGONES_ENABLED");

	EXPECT_TRUE(agones::MakeClientEndpointSourceFromEnv() == nullptr);
}

// --- 排空(D83) -------------------------------------------------------------

// 拒绝许可 -> 零单元回 Ready -> 标签清除后恢复。
TEST(AgonesLifecycleDrainTest, DrainRefusesPermitsReturnsToReadyAndRecovers)
{
	Harness h;
	h.sidecar->SetBody("/gameserver", GameServerWithLabels("{}"));
	h.Start(DrainOptions());
	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));
	EXPECT_FALSE(h.lifecycle.IsDraining());

	ASSERT_TRUE(h.lifecycle.EnsureAllocatedBlocking());
	h.lifecycle.OnUnitCreated(1);

	// Fleet 滚动 / 运维打上排空标签。
	h.sidecar->SetBody("/gameserver", GameServerWithLabels(R"({"mmorpg.io/drain":"true"})"));
	ASSERT_TRUE(WaitUntil([&] { return h.lifecycle.IsDraining(); }, 3000ms));

	// 已有单元照常跑,新许可一律拒绝(阻塞与非阻塞都一样)。
	EXPECT_EQ(agones::LifecycleState::Allocated, h.lifecycle.State());
	EXPECT_FALSE(h.lifecycle.AcquireAllocationPermitNonBlocking());
	EXPECT_FALSE(h.lifecycle.AcquireAllocationPermitBlocking());
	EXPECT_EQ(1, h.lifecycle.UnitCount());

	// 最后一个单元结束 -> 回 Ready,交给 Fleet 回收。
	h.lifecycle.OnUnitDestroyed(1);
	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));
	EXPECT_TRUE(h.lifecycle.IsDraining());

	// 排空中即使回到 Ready 也不能再 allocate(否则等于把自己重新占回来)。
	EXPECT_FALSE(h.lifecycle.AcquireAllocationPermitBlocking());
	EXPECT_FALSE(h.lifecycle.AcquireAllocationPermitNonBlocking());
	std::this_thread::sleep_for(50ms);
	EXPECT_EQ(1, h.sidecar->CallCount("/allocate"));
	EXPECT_EQ(agones::LifecycleState::Ready, h.lifecycle.State());

	// 标签被移除 -> 解除排空,恢复接单元。
	h.sidecar->SetBody("/gameserver", GameServerWithLabels(R"({"app":"battle"})"));
	ASSERT_TRUE(WaitUntil([&] { return !h.lifecycle.IsDraining(); }, 3000ms));
	{
		auto permit = h.lifecycle.AcquireAllocationPermitBlocking();
		ASSERT_TRUE(permit);
		EXPECT_EQ(agones::LifecycleState::Allocated, h.lifecycle.State());
		h.lifecycle.OnUnitCreated(2);
	}
	EXPECT_EQ(2, h.sidecar->CallCount("/allocate"));
	EXPECT_EQ(1, h.lifecycle.UnitCount());
	EXPECT_EQ(agones::LifecycleState::Allocated, h.lifecycle.State());
}

// 读标签失败(sidecar 抖动 / 响应体坏掉)不翻转判定。
TEST(AgonesLifecycleDrainTest, FailedDrainPollKeepsPreviousVerdict)
{
	Harness h;
	h.sidecar->SetBody("/gameserver", GameServerWithLabels(R"({"mmorpg.io/drain":"true"})"));
	h.Start(DrainOptions());
	ASSERT_TRUE(WaitUntil([&] { return h.lifecycle.IsDraining(); }, 3000ms));

	h.sidecar->SetBody("/gameserver", "{broken");
	int before = h.sidecar->CallCount("/gameserver");
	ASSERT_TRUE(WaitUntil([&] { return h.sidecar->CallCount("/gameserver") >= before + 3; }, 3000ms));
	EXPECT_TRUE(h.lifecycle.IsDraining());

	// 响应里没有元数据段(形状漂移):按读取失败处理,不能当成"标签被移除"而解除排空。
	h.sidecar->SetBody("/gameserver", R"({"status":{"state":"Allocated","address":"10.0.0.5","ports":[]}})");
	before = h.sidecar->CallCount("/gameserver");
	ASSERT_TRUE(WaitUntil([&] { return h.sidecar->CallCount("/gameserver") >= before + 3; }, 3000ms));
	EXPECT_TRUE(h.lifecycle.IsDraining());

	h.sidecar->FailAlways("/gameserver");
	before = h.sidecar->CallCount("/gameserver");
	ASSERT_TRUE(WaitUntil([&] { return h.sidecar->CallCount("/gameserver") >= before + 3; }, 3000ms));
	EXPECT_TRUE(h.lifecycle.IsDraining());
}

// 默认不启用排空:scene 行为不变,一次 GET /gameserver 都不发。
TEST(AgonesLifecycleDrainTest, DrainPollingIsOffByDefault)
{
	Harness h;
	h.Start();
	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));
	ASSERT_TRUE(WaitUntil([&] { return h.sidecar->CallCount("/health") >= 3; }, 3000ms));

	EXPECT_EQ(0, h.sidecar->CallCount("/gameserver"));
	EXPECT_FALSE(h.lifecycle.IsDraining());
}

// status 段形状坏掉(某个 ports 项缺 port)不拦标签判定:标签在就进入排空。
TEST(AgonesLifecycleDrainTest, BrokenStatusShapeDoesNotBlockDrainLabel)
{
	Harness h;
	h.sidecar->SetBody("/gameserver",
		R"({"object_meta":{"labels":{"mmorpg.io/drain":"true"}},"status":{"state":"Ready","ports":[{"name":"client"}]}})");
	h.Start(DrainOptions());
	ASSERT_TRUE(WaitUntil([&] { return h.lifecycle.IsDraining(); }, 3000ms));
	EXPECT_FALSE(h.lifecycle.AcquireAllocationPermitNonBlocking());
}

// 容器重启后标签还在,且 Starting 期间已有调用方挂上了 allocate 请求:
// worker 必须在 /ready 成功后先读到标签,再处理挂起的 allocate —— 许可被拒,一次 /allocate 都不发。
TEST(AgonesLifecycleDrainTest, DrainLabelIsReadBeforeAllocateQueuedDuringStarting)
{
	auto options = DrainOptions();
	// /ready 前 25 次失败、每次退避 20ms:Starting 持续约 500ms,调用方有充足时间挂上 allocate 请求。
	options.readyMaxAttempts = 50;
	options.readyInitialBackoff = 20ms;
	options.readyMaxBackoff = 20ms;
	options.allocateWaitTimeout = 3000ms;

	Harness h;
	h.sidecar->SetBody("/gameserver", GameServerWithLabels(R"({"mmorpg.io/drain":"true"})"));
	h.sidecar->FailFirst("/ready", 25);
	h.Start(options);
	ASSERT_EQ(agones::LifecycleState::Starting, h.lifecycle.State());

	// 阻塞到 worker 给出结论:/ready 成功 -> 同步读标签 -> 进入排空 -> 唤醒并拒绝等待方。
	EXPECT_FALSE(h.lifecycle.AcquireAllocationPermitBlocking());
	EXPECT_TRUE(h.lifecycle.IsDraining());

	std::this_thread::sleep_for(50ms);
	EXPECT_EQ(0, h.sidecar->CallCount("/allocate"));
	EXPECT_EQ(agones::LifecycleState::Ready, h.lifecycle.State());
}

// GET /gameserver 在途时有调用方来等 allocate:GET 返回带标签的响应后,等待方被排空唤醒并拒绝,
// 挂起的 allocate 被丢弃(覆盖 EnsureAllocatedBlocking 里"等待期间进入排空"的分支)。
TEST(AgonesLifecycleDrainTest, DrainStartingWhileWaitingForAllocateRefusesPermit)
{
	struct HeldGetState
	{
		std::mutex mutex;
		std::condition_variable cv;
		std::string body;
		bool holdGets = false;
		bool getHeld = false;
		int allocateCalls = 0;
	};

	class HoldableGetTransport final : public agones::HttpTransport
	{
	public:
		explicit HoldableGetTransport(std::shared_ptr<HeldGetState> state)
			: state_(std::move(state))
		{
		}

		agones::HttpResponse Post(const std::string& url,
			const std::string&,
			const agones::HttpTimeouts&) override
		{
			if (url.find("/allocate") != std::string::npos)
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
			std::unique_lock<std::mutex> lock(state_->mutex);
			if (state_->holdGets)
			{
				state_->getHeld = true;
				state_->cv.notify_all();
				state_->cv.wait(lock, [this] { return !state_->holdGets; });
			}

			agones::HttpResponse response;
			response.transportOk = true;
			response.statusCode = 200;
			response.body = state_->body;
			return response;
		}

	private:
		std::shared_ptr<HeldGetState> state_;
	};

	auto state = std::make_shared<HeldGetState>();
	state->body = GameServerWithLabels("{}");
	auto options = DrainOptions();
	options.allocateWaitTimeout = 3000ms;

	agones::GameServerLifecycle lifecycle;
	lifecycle.Start(std::make_unique<HoldableGetTransport>(state), "http://127.0.0.1:9358", options);
	ASSERT_TRUE(lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));
	EXPECT_FALSE(lifecycle.IsDraining());

	// 卡住下一次排空轮询的 GET,让 worker 停在 DoPollDrain 里。
	bool held = false;
	{
		std::unique_lock<std::mutex> lock(state->mutex);
		state->holdGets = true;
		held = state->cv.wait_for(lock, 3000ms, [&] { return state->getHeld; });
		if (!held)
		{
			state->holdGets = false;
		}
	}
	state->cv.notify_all();
	ASSERT_TRUE(held);

	// GET 在途时调用方来要许可:worker 腾不出手处理 allocate,调用方挂在有界等待里。
	std::atomic<int> verdict{ -1 };
	std::thread caller([&] {
		auto permit = lifecycle.AcquireAllocationPermitBlocking();
		verdict = permit ? 1 : 0;
	});
	std::this_thread::sleep_for(50ms);

	// 放行 GET,返回带排空标签的响应。
	{
		std::lock_guard<std::mutex> lock(state->mutex);
		state->body = GameServerWithLabels(R"({"mmorpg.io/drain":"true"})");
		state->holdGets = false;
	}
	state->cv.notify_all();
	caller.join();

	EXPECT_EQ(0, verdict.load());
	EXPECT_TRUE(lifecycle.IsDraining());
	std::this_thread::sleep_for(50ms);
	EXPECT_EQ(agones::LifecycleState::Ready, lifecycle.State());
	std::lock_guard<std::mutex> lock(state->mutex);
	EXPECT_EQ(0, state->allocateCalls);
}

// --- EventLoop 心跳(D84) ---------------------------------------------------

// 心跳新鲜:照常发 health。
TEST(AgonesLifecycleHeartbeatTest, FreshLoopHeartbeatKeepsHealthFlowing)
{
	auto options = FastOptions();
	options.requireLoopHeartbeat = true;
	options.loopStaleAfter = 60s; // Start() 记下的初值在整个用例内都新鲜

	Harness h;
	h.Start(options);
	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));
	EXPECT_TRUE(WaitUntil([&] { return h.sidecar->CallCount("/health") >= 3; }, 3000ms));
}

// 心跳过期:停发 health;EventLoop 恢复 Touch 后重新发。
TEST(AgonesLifecycleHeartbeatTest, StaleLoopHeartbeatWithholdsHealthUntilTouched)
{
	auto options = FastOptions();
	options.requireLoopHeartbeat = true;
	options.loopStaleAfter = 50ms;

	Harness h;
	h.Start(options);
	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));

	// 从不 Touch:启动宽限(50ms)过后 worker 不再发 /health。
	std::this_thread::sleep_for(200ms);
	const int stalled = h.sidecar->CallCount("/health");
	std::this_thread::sleep_for(200ms);
	EXPECT_EQ(stalled, h.sidecar->CallCount("/health"));

	// 模拟 EventLoop 恢复:持续 Touch,health 随之恢复。
	std::atomic<bool> stop{ false };
	std::thread loopThread([&] {
		while (!stop.load())
		{
			h.lifecycle.TouchLoopHeartbeat();
			std::this_thread::sleep_for(5ms);
		}
	});
	const bool resumed = WaitUntil([&] { return h.sidecar->CallCount("/health") >= stalled + 3; }, 3000ms);
	stop = true;
	loopThread.join();
	EXPECT_TRUE(resumed);
}

// 未要求心跳(scene 默认):从不 Touch 也照常发 health。
TEST(AgonesLifecycleHeartbeatTest, HeartbeatNotRequiredByDefault)
{
	Harness h;
	h.Start();
	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));
	std::this_thread::sleep_for(100ms);
	const int before = h.sidecar->CallCount("/health");
	EXPECT_TRUE(WaitUntil([&] { return h.sidecar->CallCount("/health") >= before + 3; }, 3000ms));
}

// --- worker 调度 -------------------------------------------------------------

// 挂起请求(allocate / 回 Ready)频繁到来时,health 仍按 healthInterval 发出:每次唤醒不重置
// health 的截止时间。旧实现每次唤醒都重新计时,请求间隔小于 healthInterval 时 health 会被饿死。
TEST(AgonesLifecycleTest, FrequentRequestsDoNotStarveHealth)
{
	auto options = FastOptions();
	options.healthInterval = 50ms;

	Harness h;
	h.Start(options);
	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));

	const int healthBefore = h.sidecar->CallCount("/health");
	const int allocateBefore = h.sidecar->CallCount("/allocate");
	std::atomic<bool> stop{ false };
	std::thread requester([&] {
		while (!stop.load())
		{
			// Ready 时踢一次 allocate;Allocated 且零单元时请求回 Ready。两者交替,
			// 每轮都给 worker 留一个挂起请求,间隔(Windows 上最多约 16ms)小于 healthInterval。
			h.lifecycle.EnsureAllocatedNonBlocking();
			h.lifecycle.ReconcileIdleAfterCreate();
			std::this_thread::sleep_for(5ms);
		}
	});
	std::this_thread::sleep_for(600ms);
	stop = true;
	requester.join();

	// 确实在持续制造请求。
	EXPECT_GE(h.sidecar->CallCount("/allocate") - allocateBefore, 3);
	// 600ms / 50ms 约 12 次;留足余量,只要求没有被饿死。
	EXPECT_GE(h.sidecar->CallCount("/health") - healthBefore, 4);
}

// --- 晚到分配(allocate 慢于等待方超时) ---------------------------------------

namespace
{

	// /allocate 可被测试卡住的 sidecar:模拟 DoAllocate 最坏耗时(默认约 6.4s)长于等待方上限(默认 3s)。
	struct HeldAllocateState
	{
		std::mutex mutex;
		std::condition_variable cv;
		bool holdAllocate = false;
		bool allocateHeld = false;
		// 接下来的这么多次 /allocate 直接返回传输失败(先于卡住判定),用于脚本化 allocate 失败。
		int failAllocate = 0;
		int allocateCalls = 0;
		int readyCalls = 0;
	};

	class HoldableAllocateTransport final : public agones::HttpTransport
	{
	public:
		explicit HoldableAllocateTransport(std::shared_ptr<HeldAllocateState> state)
			: state_(std::move(state))
		{
		}

		agones::HttpResponse Post(const std::string& url,
			const std::string&,
			const agones::HttpTimeouts&) override
		{
			std::unique_lock<std::mutex> lock(state_->mutex);
			if (url.find("/allocate") != std::string::npos)
			{
				++state_->allocateCalls;
				if (state_->failAllocate > 0)
				{
					--state_->failAllocate;
					agones::HttpResponse failed;
					failed.error = "fake: allocate rejected";
					return failed;
				}
				if (state_->holdAllocate)
				{
					state_->allocateHeld = true;
					state_->cv.notify_all();
					state_->cv.wait(lock, [this] { return !state_->holdAllocate; });
				}
			}
			else if (url.find("/ready") != std::string::npos)
			{
				++state_->readyCalls;
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
		std::shared_ptr<HeldAllocateState> state_;
	};

	// 等 /allocate 卡在 sidecar 里。等不到也会放行,避免断言失败后 worker 卡死、Stop() join 不回来。
	bool WaitAllocateHeld(HeldAllocateState& state)
	{
		std::unique_lock<std::mutex> lock(state.mutex);
		const bool held = state.cv.wait_for(lock, 3000ms, [&state] { return state.allocateHeld; });
		if (!held)
		{
			state.holdAllocate = false;
			state.cv.notify_all();
		}
		return held;
	}

	void ReleaseAllocate(HeldAllocateState& state)
	{
		{
			std::lock_guard<std::mutex> lock(state.mutex);
			state.holdAllocate = false;
		}
		state.cv.notify_all();
	}

	// 让下一次 /allocate 重新卡住(复位 allocateHeld,供 WaitAllocateHeld 等待新的一次)。
	void HoldNextAllocate(HeldAllocateState& state)
	{
		std::lock_guard<std::mutex> lock(state.mutex);
		state.holdAllocate = true;
		state.allocateHeld = false;
	}

	int ReadyCalls(HeldAllocateState& state)
	{
		std::lock_guard<std::mutex> lock(state.mutex);
		return state.readyCalls;
	}

	int AllocateCalls(HeldAllocateState& state)
	{
		std::lock_guard<std::mutex> lock(state.mutex);
		return state.allocateCalls;
	}

} // namespace

// 等待方超时拒绝创建离开后 allocate 才晚到成功:零单元、零在途许可,没有任何人会再来取这次分配。
// 必须自己收口回 Ready,不能停在"Allocated 但空转"一直占着 Agones 容量。
TEST(AgonesLifecycleTest, LateAllocateAfterWaiterTimeoutReturnsToReady)
{
	auto state = std::make_shared<HeldAllocateState>();
	state->holdAllocate = true;
	auto options = FastOptions();
	options.allocateWaitTimeout = 100ms;

	agones::GameServerLifecycle lifecycle;
	lifecycle.Start(std::make_unique<HoldableAllocateTransport>(state), "http://127.0.0.1:9358", options);
	ASSERT_TRUE(lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));

	// allocate 卡在 sidecar,等待方到期拒绝本次创建(fail-closed)。
	EXPECT_FALSE(lifecycle.AcquireAllocationPermitBlocking());

	const bool held = WaitAllocateHeld(*state);
	// 第二个等待方在 allocate 在途时到来,同样超时离开。它留下一个挂起的 allocate 请求:
	// 晚到结果必须一并满足它,不能让 worker 取到它时把收口请求冲掉。
	bool secondGranted = true;
	if (held)
	{
		secondGranted = static_cast<bool>(lifecycle.AcquireAllocationPermitBlocking());
	}
	const agones::LifecycleState stateWhileHeld = lifecycle.State();
	ReleaseAllocate(*state);
	ASSERT_TRUE(held);
	EXPECT_FALSE(secondGranted);
	EXPECT_EQ(agones::LifecycleState::Allocating, stateWhileHeld);

	// allocate 晚到成功 -> 同一临界区发现无人等待、零单元零许可 -> 回 Ready。
	// /ready 共 2 次:启动 1 次 + 晚到收口 1 次。
	ASSERT_TRUE(WaitUntil([&] { return ReadyCalls(*state) >= 2; }, 3000ms));
	ASSERT_TRUE(lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));
	EXPECT_EQ(0, lifecycle.UnitCount());
	EXPECT_EQ(1, AllocateCalls(*state));

	// 收口之后照常接新单元:重新 allocate;这次等待方在场,结果不被收口。
	{
		auto permit = lifecycle.AcquireAllocationPermitBlocking();
		ASSERT_TRUE(permit);
		lifecycle.OnUnitCreated(1);
	}
	std::this_thread::sleep_for(50ms);
	EXPECT_EQ(agones::LifecycleState::Allocated, lifecycle.State());
	EXPECT_EQ(1, lifecycle.UnitCount());
	EXPECT_EQ(2, AllocateCalls(*state));
	EXPECT_EQ(2, ReadyCalls(*state));

	lifecycle.Stop();
}

// 非阻塞调用方(EventLoop 上的 scene CreateScene)踢的 allocate 即使慢,结果也要留给它的重试:
// 晚到收口不能作用于这种结果,否则重试永远撞上 Ready(allocate / ready 活锁)。scene 行为不变。
TEST(AgonesLifecycleTest, NonBlockingKickedSlowAllocateStaysAllocatedForRetry)
{
	auto state = std::make_shared<HeldAllocateState>();
	state->holdAllocate = true;

	agones::GameServerLifecycle lifecycle;
	lifecycle.Start(std::make_unique<HoldableAllocateTransport>(state), "http://127.0.0.1:9358", FastOptions());
	ASSERT_TRUE(lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));

	EXPECT_FALSE(lifecycle.AcquireAllocationPermitNonBlocking());

	const bool held = WaitAllocateHeld(*state);
	ReleaseAllocate(*state);
	ASSERT_TRUE(held);

	ASSERT_TRUE(lifecycle.WaitForState(agones::LifecycleState::Allocated, 3000ms));
	// 给 worker 留出处理挂起请求的时间:若误做收口,这里已发出第 2 次 /ready 并离开 Allocated。
	std::this_thread::sleep_for(80ms);
	EXPECT_EQ(agones::LifecycleState::Allocated, lifecycle.State());
	EXPECT_EQ(1, ReadyCalls(*state));

	// 重试取走 Allocated,不再发 allocate。
	{
		auto permit = lifecycle.AcquireAllocationPermitNonBlocking();
		ASSERT_TRUE(permit);
		lifecycle.OnUnitCreated(1);
	}
	EXPECT_EQ(1, AllocateCalls(*state));
	EXPECT_EQ(agones::LifecycleState::Allocated, lifecycle.State());

	lifecycle.Stop();
}

// 非阻塞调用方踢起的 allocate 失败后,它"稍后会来取"的标记不能残留到下一次 allocate:
// 之后一次阻塞等待方全部超时、allocate 晚到成功的结果仍要收口回 Ready。
// 标记残留时,这次结果会被误判为有人重试而停在 Allocated 且零单元。
TEST(AgonesLifecycleTest, FailedNonBlockingAllocateDoesNotSuppressLaterLateAllocateReturn)
{
	auto options = FastOptions();
	options.allocateWaitTimeout = 100ms;
	auto state = std::make_shared<HeldAllocateState>();
	state->failAllocate = options.allocateMaxAttempts;

	agones::GameServerLifecycle lifecycle;
	lifecycle.Start(std::make_unique<HoldableAllocateTransport>(state), "http://127.0.0.1:9358", options);
	ASSERT_TRUE(lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));

	// 非阻塞调用方踢起 allocate 后不再来重试;这次 allocate 的全部尝试都失败。
	EXPECT_FALSE(lifecycle.AcquireAllocationPermitNonBlocking());
	ASSERT_TRUE(WaitUntil([&] { return AllocateCalls(*state) >= options.allocateMaxAttempts; }, 3000ms));
	// 最后一次失败的 HTTP 已发出,DoAllocate 写回 Ready 之前状态是 Allocating:这里等的是失败结果。
	ASSERT_TRUE(lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));

	// 慢 allocate:阻塞等待方到期拒绝本次创建(fail-closed)之后才晚到成功。
	HoldNextAllocate(*state);
	EXPECT_FALSE(lifecycle.AcquireAllocationPermitBlocking());
	const bool held = WaitAllocateHeld(*state);
	ReleaseAllocate(*state);
	ASSERT_TRUE(held);

	// 无人等待、无人重试、零单元零许可 -> 收口回 Ready。/ready 共 2 次:启动 1 次 + 收口 1 次。
	ASSERT_TRUE(WaitUntil([&] { return ReadyCalls(*state) >= 2; }, 3000ms));
	ASSERT_TRUE(lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));
	EXPECT_EQ(0, lifecycle.UnitCount());
	EXPECT_EQ(options.allocateMaxAttempts + 1, AllocateCalls(*state));

	lifecycle.Stop();
}

// allocate 结果落地时仍有阻塞等待方在场(allocateWaiters_ > 0),就不能做晚到收口:这次分配是它要的。
// 缺了这道门槛时,worker 放下锁后立刻回到循环顶部重新拿锁、处理收口请求,而等待方要先被调度唤醒
// 才能拿许可;只要 worker 先到,等待方就会看到 ReturningToReady 被拒,并多出一次 /ready。
// 单轮是否撞上这个先后取决于调度,因此连跑几轮。
TEST(AgonesLifecycleTest, LateAllocateWithWaiterPresentIsNotReturnedToReady)
{
	auto options = FastOptions();
	options.allocateWaitTimeout = 3000ms;
	auto state = std::make_shared<HeldAllocateState>();

	agones::GameServerLifecycle lifecycle;
	lifecycle.Start(std::make_unique<HoldableAllocateTransport>(state), "http://127.0.0.1:9358", options);
	ASSERT_TRUE(lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));

	constexpr int kRounds = 5;
	for (int round = 0; round < kRounds; ++round)
	{
		const std::uint64_t unitKey = static_cast<std::uint64_t>(round + 1);
		HoldNextAllocate(*state);

		std::atomic<bool> granted{ false };
		std::thread waiter([&lifecycle, &granted, unitKey] {
			auto permit = lifecycle.AcquireAllocationPermitBlocking();
			granted = static_cast<bool>(permit);
			if (permit)
			{
				lifecycle.OnUnitCreated(unitKey);
			}
		});
		// allocate 卡住时等待方一定已在有界等待里:worker 只能在等待方 wait_for 放锁之后取到请求。
		const bool held = WaitAllocateHeld(*state);
		ReleaseAllocate(*state);
		waiter.join();
		ASSERT_TRUE(held) << "round " << round;

		ASSERT_TRUE(granted.load()) << "round " << round;
		EXPECT_EQ(agones::LifecycleState::Allocated, lifecycle.State()) << "round " << round;
		EXPECT_EQ(1, lifecycle.UnitCount()) << "round " << round;
		// 启动 1 次 + 前几轮收尾的正常回 Ready,本轮没有多出来的 /ready。
		EXPECT_EQ(1 + round, ReadyCalls(*state)) << "round " << round;
		EXPECT_EQ(1 + round, AllocateCalls(*state)) << "round " << round;

		// 本轮收尾:最后一个单元销毁 -> 正常回 Ready,下一轮从 Ready 重新 allocate。
		lifecycle.OnUnitDestroyed(unitKey);
		ASSERT_TRUE(WaitUntil([&] { return ReadyCalls(*state) >= 2 + round; }, 3000ms)) << "round " << round;
		ASSERT_TRUE(lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms)) << "round " << round;
	}

	lifecycle.Stop();
}
