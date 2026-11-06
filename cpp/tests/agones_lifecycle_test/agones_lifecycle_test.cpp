#include <gtest/gtest.h>

#include <cstdlib>

#include <algorithm>
#include <atomic>
#include <chrono>
#include <condition_variable>
#include <functional>
#include <future>
#include <map>
#include <memory>
#include <mutex>
#include <optional>
#include <set>
#include <stop_token>
#include <string>
#include <thread>
#include <vector>

#include "agones/agones_client_endpoint_source.h"
#include "agones/agones_gameserver_lifecycle.h"
#include "agones/agones_gameserver_status.h"
#include "agones/agones_rest_client.h"
// muduo 头必须最后包含:Windows 适配头会 #define pid_t / off_t 等名字,还会带进 Windows.h。
#include "muduo/net/EventLoop.h"

// Agones 生命周期状态机、GameServer JSON 解析与客户端地址来源的单元测试。
//
// 全部通过注入的 FakeTransport 跑,不需要真的起 Agones sidecar,也不发任何
// 真实网络请求。libcurl 的实现(CurlAgonesHttpTransport)只在定义了
// MMORPG_AGONES_CURL 的 Linux 构建里编译,Windows 测试构建里根本不存在。
//
// 2026-09-29:被测代码由 cpp/nodes/scene/agones/ 下沉到 cpp/libs/engine/infra/agones/,
// 并按 D85 改名(SceneLifecycle -> GameServerLifecycle 等);工程名不变。
//
// 2026-10-16:新增异步许可(AcquireAllocationPermitAsync,grpc-muduo-transport.md §15.9)用例,
// 测试线程自建 muduo EventLoop,用 runAfter 收尾,不起额外线程、不写死循环。
// 同日补 Stop() 有界返回(§15.16 第 1 条)用例:可取消的 fake 传输把 /allocate 卡到取消令牌置位为止。
// 2026-10-17 补停止语义的两处收紧:阻塞等待方与 worker 写入 Allocated 竞争时 Stop() 之后一律拒绝;
// Stop() 之前已 RequestShutdown() 的 /shutdown 在 worker 退出前恰好发出一次。
// 同日补不走 Stop()、单独 RequestShutdown() 的两处边角(grpc-muduo-transport.md §15.17):ShuttingDown 是终态,
// 挂起的 allocate 被丢弃、等待方一律拒绝;RequestShutdown() 幂等,/shutdown 全程最多一次。

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

// Ready 的快速重试用完之后不能放弃:sidecar 晚于快速阶段才就绪时,放弃就让实例永远停在 Starting、
// 永远不可分配(docs/design/infra-reconnect-overview.md §4 R10)。超过 readyMaxAttempts 之后继续重试,
// 期间必须停在 Starting 并继续 fail-closed,不能"到期后假设成功";sidecar 一恢复就进入 Ready。
TEST(AgonesLifecycleTest, ReadyKeepsRetryingPastMaxAttemptsAndStaysFailClosedUntilSidecarRecovers)
{
	auto options = FastOptions();
	options.readyMaxAttempts = 3;
	options.allocateWaitTimeout = 100ms;

	// 100 次失败 × 退避封顶 10ms ≈ 1s:足够在恢复之前核对 fail-closed。
	constexpr int kFailures = 100;
	Harness h;
	h.sidecar->FailFirst("/ready", kFailures);
	h.Start(options);

	// 快速重试的上限过了还在试。
	EXPECT_TRUE(WaitUntil([&] { return h.sidecar->CallCount("/ready") > options.readyMaxAttempts + 1; }, 3000ms));
	EXPECT_EQ(agones::LifecycleState::Starting, h.lifecycle.State());

	// 关键:没 Ready 成功就绝不允许创建房间。
	EXPECT_FALSE(h.lifecycle.EnsureAllocatedBlocking());
	EXPECT_EQ(0, h.sidecar->CallCount("/allocate"));

	// sidecar 恢复后进入 Ready。
	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 5000ms));
	EXPECT_EQ(kFailures + 1, h.sidecar->CallCount("/ready")); // 全部失败 + 1 次成功
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

// 许可放行后,CreateScene 的许可回调还要排队进入 EventLoop(经 queueInLoop 投递)才进 Handle*。
// 这个窗口也算"在途创建":旧的最后一个 Scene 被销毁时不能抢先回 Ready。
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

// EventLoop 上的 RPC0 CreateScene 用非阻塞版本:立刻返回 false,并且踢起一次异步 allocate
// (gRPC 守护段用 AcquireAllocationPermitAsync,见文件末尾的 AgonesAsyncPermitTest)。
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

// ---------------------------------------------------------------------------
// 2026-10-16 第 4 批 §15.9:EventLoop 上的异步许可(AcquireAllocationPermitAsync)
// ---------------------------------------------------------------------------

namespace
{
	using agones::GameServerLifecycle;

	// 在测试线程自建的 loop 上转,直到 done() 为真或超时;返回 done() 的最终结果。
	// runEvery 只做条件检查,runAfter 到点无条件收尾;不起线程、不写死循环。不能在 loop 自己的回调里调用。
	bool RunLoopUntil(muduo::net::EventLoop& loop, const std::function<bool()>& done, std::chrono::milliseconds timeout)
	{
		if (done())
		{
			return true;
		}
		const muduo::net::TimerId check = loop.runEvery(0.005, [&loop, &done] {
			if (done())
			{
				loop.quit();
			}
		});
		const muduo::net::TimerId giveUp = loop.runAfter(std::chrono::duration<double>(timeout).count(),
			[&loop] { loop.quit(); });
		loop.loop();
		loop.cancel(check);
		loop.cancel(giveUp);
		return done();
	}

	// 无条件转满 duration:让已排队的投递跑完,也给"不该再有回调"的断言留出观察窗口。
	void RunLoopFor(muduo::net::EventLoop& loop, std::chrono::milliseconds duration)
	{
		RunLoopUntil(loop, [] { return false; }, duration);
	}

	long long ElapsedMs(std::chrono::steady_clock::time_point begin)
	{
		return std::chrono::duration_cast<std::chrono::milliseconds>(std::chrono::steady_clock::now() - begin).count();
	}

	// 一次异步许可回调的记录。回调只在测试线程(loop 线程)上执行,不需要原子量;
	// 由回调与测试以 shared_ptr 共享,回调闭包晚于用例变量析构也不悬空。
	struct PermitOutcome
	{
		int calls = 0;
		bool granted = false;
	};

	// 只记录、不保留 permit:permit 随回调返回析构(相当于参数非法 / 幂等命中而没建出单元)。
	GameServerLifecycle::AllocationPermitCallback RecordPermit(const std::shared_ptr<PermitOutcome>& outcome)
	{
		return [outcome](GameServerLifecycle::AllocationPermit permit) {
			++outcome->calls;
			outcome->granted = static_cast<bool>(permit);
		};
	}

	// 记录并在持有 permit 期间建出单元(正常的创建路径:permit 活到单元建完)。
	GameServerLifecycle::AllocationPermitCallback RecordPermitAndCreateUnit(
		const std::shared_ptr<PermitOutcome>& outcome, GameServerLifecycle& lifecycle, std::uint64_t unitKey)
	{
		return [outcome, &lifecycle, unitKey](GameServerLifecycle::AllocationPermit permit) {
			++outcome->calls;
			outcome->granted = static_cast<bool>(permit);
			if (permit)
			{
				lifecycle.OnUnitCreated(unitKey);
			}
		};
	}

	bool AllCalled(const std::vector<std::shared_ptr<PermitOutcome>>& outcomes)
	{
		for (const auto& outcome : outcomes)
		{
			if (outcome->calls == 0)
			{
				return false;
			}
		}
		return true;
	}

	// 异步许可用例的夹具:测试线程自建 EventLoop,所有 Acquire 都在这个线程上调。
	// 析构函数体先在 loop 还活着时 Stop()(残留等待者的拒绝排进 loop 队列);随后按声明逆序先销毁 loop
	// (队列里没跑的投递随之析构,放行的 permit 照常记账),最后才销毁 lifecycle ——
	// lifecycle 必须活得比 loop 队列里的 permit 久。
	struct AsyncHarness
	{
		GameServerLifecycle lifecycle;
		muduo::net::EventLoop loop;

		~AsyncHarness() { lifecycle.Stop(); }
	};

	// GET /gameserver 可被卡住、响应体可脚本化的 sidecar:让 worker 停在排空轮询里,
	// 制造"异步等待者已登记、worker 腾不出手处理 allocate"的窗口。
	struct HeldDrainPollState
	{
		std::mutex mutex;
		std::condition_variable cv;
		std::string body;
		bool holdGets = false;
		bool getHeld = false;
		int allocateCalls = 0;
	};

	class HoldableDrainPollTransport final : public agones::HttpTransport
	{
	public:
		explicit HoldableDrainPollTransport(std::shared_ptr<HeldDrainPollState> state)
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
		std::shared_ptr<HeldDrainPollState> state_;
	};

} // namespace

// Disabled:放行;即使当场可判定,也经 queueInLoop 投递,不在调用栈内回调。
TEST(AgonesAsyncPermitTest, DisabledGrantsButNeverCallsBackInline)
{
	AsyncHarness h;
	h.lifecycle.StartDisabled();

	auto outcome = std::make_shared<PermitOutcome>();
	h.lifecycle.AcquireAllocationPermitAsync(h.loop, RecordPermit(outcome));
	EXPECT_EQ(0, outcome->calls);

	ASSERT_TRUE(RunLoopUntil(h.loop, [&] { return outcome->calls > 0; }, 3000ms));
	RunLoopFor(h.loop, 30ms);
	EXPECT_EQ(1, outcome->calls);
	EXPECT_TRUE(outcome->granted);
	EXPECT_EQ(0u, h.lifecycle.PendingPermitCount());
}

// 已 Allocated:立即放行、不再发 allocate,但仍经投递,不在调用栈内回调。
TEST(AgonesAsyncPermitTest, AllocatedGrantsImmediatelyButStillDelivers)
{
	auto sidecar = std::make_shared<FakeSidecar>();
	AsyncHarness h;
	h.lifecycle.Start(std::make_unique<FakeTransport>(sidecar), "http://127.0.0.1:9358", FastOptions());
	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));
	{
		auto permit = h.lifecycle.AcquireAllocationPermitBlocking();
		ASSERT_TRUE(permit);
		h.lifecycle.OnUnitCreated(1);
	}
	ASSERT_EQ(agones::LifecycleState::Allocated, h.lifecycle.State());

	auto outcome = std::make_shared<PermitOutcome>();
	h.lifecycle.AcquireAllocationPermitAsync(h.loop, RecordPermit(outcome));
	EXPECT_EQ(0, outcome->calls);

	ASSERT_TRUE(RunLoopUntil(h.loop, [&] { return outcome->calls > 0; }, 3000ms));
	RunLoopFor(h.loop, 30ms);
	EXPECT_EQ(1, outcome->calls);
	EXPECT_TRUE(outcome->granted);
	EXPECT_EQ(1, sidecar->CallCount("/allocate"));
	EXPECT_EQ(agones::LifecycleState::Allocated, h.lifecycle.State()); // 单元 1 还在
	EXPECT_EQ(0u, h.lifecycle.PendingPermitCount());
}

// 第一个单元之前:两个创建请求都在 Ready 时到来,只 allocate 一次,随后都放行并建出单元。
TEST(AgonesAsyncPermitTest, FirstUnitAllocatesOnceThenGrants)
{
	auto sidecar = std::make_shared<FakeSidecar>();
	auto options = FastOptions();
	options.allocateWaitTimeout = 3000ms;
	AsyncHarness h;
	h.lifecycle.Start(std::make_unique<FakeTransport>(sidecar), "http://127.0.0.1:9358", options);
	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));

	auto first = std::make_shared<PermitOutcome>();
	auto second = std::make_shared<PermitOutcome>();
	h.lifecycle.AcquireAllocationPermitAsync(h.loop, RecordPermitAndCreateUnit(first, h.lifecycle, 1));
	h.lifecycle.AcquireAllocationPermitAsync(h.loop, RecordPermitAndCreateUnit(second, h.lifecycle, 2));
	EXPECT_EQ(0, first->calls);
	EXPECT_EQ(0, second->calls);

	ASSERT_TRUE(RunLoopUntil(h.loop, [&] { return first->calls > 0 && second->calls > 0; }, 3000ms));
	RunLoopFor(h.loop, 30ms);
	EXPECT_EQ(1, first->calls);
	EXPECT_EQ(1, second->calls);
	EXPECT_TRUE(first->granted);
	EXPECT_TRUE(second->granted);
	EXPECT_EQ(1, sidecar->CallCount("/allocate"));
	EXPECT_EQ(2, h.lifecycle.UnitCount());
	EXPECT_EQ(agones::LifecycleState::Allocated, h.lifecycle.State());
	EXPECT_EQ(0u, h.lifecycle.PendingPermitCount());
}

// allocate 失败:由 worker 的失败结果当场拒绝,不等满等待上限。
TEST(AgonesAsyncPermitTest, AllocateFailureRefusesWithoutWaitingForTimeout)
{
	auto sidecar = std::make_shared<FakeSidecar>();
	sidecar->FailAlways("/allocate");
	auto options = FastOptions();
	options.allocateWaitTimeout = 3000ms;
	AsyncHarness h;
	h.lifecycle.Start(std::make_unique<FakeTransport>(sidecar), "http://127.0.0.1:9358", options);
	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));

	auto outcome = std::make_shared<PermitOutcome>();
	const auto begin = std::chrono::steady_clock::now();
	h.lifecycle.AcquireAllocationPermitAsync(h.loop, RecordPermit(outcome));
	ASSERT_TRUE(RunLoopUntil(h.loop, [&] { return outcome->calls > 0; }, 3000ms));
	const long long elapsedMs = ElapsedMs(begin);
	RunLoopFor(h.loop, 30ms);

	EXPECT_EQ(1, outcome->calls);
	EXPECT_FALSE(outcome->granted);
	EXPECT_LT(elapsedMs, 2000);
	EXPECT_EQ(options.allocateMaxAttempts, sidecar->CallCount("/allocate"));
	EXPECT_EQ(agones::LifecycleState::Ready, h.lifecycle.State());
	EXPECT_EQ(0, h.lifecycle.UnitCount());
	EXPECT_EQ(0u, h.lifecycle.PendingPermitCount());
}

// allocate 卡在 sidecar:loop 定时器按时(不早于上限)拒绝;等待者出表后 allocate 才晚到成功,
// 零单元、零在途许可,必须收口回 Ready,且不会再回调一次。
TEST(AgonesAsyncPermitTest, WaitTimeoutRefusesOnTimeAndLateAllocateReturnsToReady)
{
	auto state = std::make_shared<HeldAllocateState>();
	state->holdAllocate = true;
	auto options = FastOptions();
	options.allocateWaitTimeout = 150ms;
	AsyncHarness h;
	h.lifecycle.Start(std::make_unique<HoldableAllocateTransport>(state), "http://127.0.0.1:9358", options);
	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));

	auto outcome = std::make_shared<PermitOutcome>();
	const auto begin = std::chrono::steady_clock::now();
	h.lifecycle.AcquireAllocationPermitAsync(h.loop, RecordPermit(outcome));
	const bool answered = RunLoopUntil(h.loop, [&] { return outcome->calls > 0; }, 3000ms);
	const long long elapsedMs = ElapsedMs(begin);

	// 先放行 allocate 再断言:断言失败时 worker 不会卡在 sidecar 里,Stop() 才 join 得回来。
	const bool held = WaitAllocateHeld(*state);
	const agones::LifecycleState stateWhileHeld = h.lifecycle.State();
	ReleaseAllocate(*state);
	ASSERT_TRUE(answered);
	ASSERT_TRUE(held);
	EXPECT_EQ(1, outcome->calls);
	EXPECT_FALSE(outcome->granted);
	EXPECT_GE(elapsedMs, 150); // steady 复核:不早于上限
	EXPECT_LT(elapsedMs, 1500);
	EXPECT_EQ(agones::LifecycleState::Allocating, stateWhileHeld);

	// /ready 共 2 次:启动 1 次 + 晚到收口 1 次。
	ASSERT_TRUE(WaitUntil([&] { return ReadyCalls(*state) >= 2; }, 3000ms));
	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));
	EXPECT_EQ(1, AllocateCalls(*state));
	EXPECT_EQ(0, h.lifecycle.UnitCount());
	EXPECT_EQ(0u, h.lifecycle.PendingPermitCount());

	RunLoopFor(h.loop, 30ms);
	EXPECT_EQ(1, outcome->calls);
}

// allocate 慢但等待者还在:晚到结果放行等待者,保持 Allocated,不做晚到收口(没有多出来的 /ready)。
TEST(AgonesAsyncPermitTest, LateAllocateWithWaiterPresentGrantsAndStaysAllocated)
{
	auto state = std::make_shared<HeldAllocateState>();
	state->holdAllocate = true;
	auto options = FastOptions();
	options.allocateWaitTimeout = 3000ms;
	AsyncHarness h;
	h.lifecycle.Start(std::make_unique<HoldableAllocateTransport>(state), "http://127.0.0.1:9358", options);
	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));

	auto outcome = std::make_shared<PermitOutcome>();
	h.lifecycle.AcquireAllocationPermitAsync(h.loop, RecordPermitAndCreateUnit(outcome, h.lifecycle, 1));
	// allocate 卡住时等待者一定已登记:Acquire 先登记、再踢 allocate。
	const bool held = WaitAllocateHeld(*state);
	RunLoopFor(h.loop, 30ms);
	const int callsWhileHeld = outcome->calls;
	ReleaseAllocate(*state);
	ASSERT_TRUE(held);
	EXPECT_EQ(0, callsWhileHeld);

	ASSERT_TRUE(RunLoopUntil(h.loop, [&] { return outcome->calls > 0; }, 3000ms));
	// 给 worker 留出处理挂起请求的时间:若误做收口,这里已发出第 2 次 /ready 并离开 Allocated。
	RunLoopFor(h.loop, 80ms);
	EXPECT_EQ(1, outcome->calls);
	EXPECT_TRUE(outcome->granted);
	EXPECT_EQ(agones::LifecycleState::Allocated, h.lifecycle.State());
	EXPECT_EQ(1, h.lifecycle.UnitCount());
	EXPECT_EQ(1, ReadyCalls(*state));
	EXPECT_EQ(1, AllocateCalls(*state));
	EXPECT_EQ(0u, h.lifecycle.PendingPermitCount());
}

// 等待期间开始排空:由排空结算点拒绝(不等满上限),挂起的 allocate 被丢弃,一次 /allocate 都不发。
TEST(AgonesAsyncPermitTest, DrainStartingWhileWaitingRefusesPermit)
{
	auto state = std::make_shared<HeldDrainPollState>();
	state->body = GameServerWithLabels("{}");
	auto options = DrainOptions();
	options.allocateWaitTimeout = 3000ms;
	AsyncHarness h;
	h.lifecycle.Start(std::make_unique<HoldableDrainPollTransport>(state), "http://127.0.0.1:9358", options);
	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));
	EXPECT_FALSE(h.lifecycle.IsDraining());

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

	auto outcome = std::make_shared<PermitOutcome>();
	h.lifecycle.AcquireAllocationPermitAsync(h.loop, RecordPermit(outcome));
	RunLoopFor(h.loop, 30ms);
	const int callsWhileHeld = outcome->calls;

	// 放行 GET,返回带排空标签的响应。先放行再断言,免得 worker 卡死、Stop() join 不回来。
	{
		std::lock_guard<std::mutex> lock(state->mutex);
		state->body = GameServerWithLabels(R"({"mmorpg.io/drain":"true"})");
		state->holdGets = false;
	}
	state->cv.notify_all();
	EXPECT_EQ(0, callsWhileHeld);

	const auto begin = std::chrono::steady_clock::now();
	ASSERT_TRUE(RunLoopUntil(h.loop, [&] { return outcome->calls > 0; }, 3000ms));
	const long long elapsedMs = ElapsedMs(begin);
	RunLoopFor(h.loop, 50ms);

	EXPECT_EQ(1, outcome->calls);
	EXPECT_FALSE(outcome->granted);
	EXPECT_LT(elapsedMs, 2000);
	EXPECT_TRUE(h.lifecycle.IsDraining());
	EXPECT_EQ(agones::LifecycleState::Ready, h.lifecycle.State());
	EXPECT_EQ(0u, h.lifecycle.PendingPermitCount());
	std::lock_guard<std::mutex> lock(state->mutex);
	EXPECT_EQ(0, state->allocateCalls);
}

// 在 loop 线程上 Stop():3 个等待者各拒绝一次(经投递,不在 Stop 栈内回调);之后的 Acquire 立即拒绝(仍经投递)。
TEST(AgonesAsyncPermitTest, StopOnLoopRefusesEveryWaiterOnceAndLaterCallsImmediately)
{
	auto sidecar = std::make_shared<FakeSidecar>();
	sidecar->FailAlways("/ready");
	auto options = FastOptions();
	// /ready 一直失败、退避可被 Stop 打断:worker 停在 Starting,等待者只能由 Stop 或超时裁决。
	options.readyMaxAttempts = 1000;
	options.readyInitialBackoff = 20ms;
	options.readyMaxBackoff = 20ms;
	options.allocateWaitTimeout = 3000ms;
	AsyncHarness h;
	h.lifecycle.Start(std::make_unique<FakeTransport>(sidecar), "http://127.0.0.1:9358", options);
	ASSERT_EQ(agones::LifecycleState::Starting, h.lifecycle.State());

	std::vector<std::shared_ptr<PermitOutcome>> waiters;
	for (int i = 0; i < 3; ++i)
	{
		waiters.push_back(std::make_shared<PermitOutcome>());
		h.lifecycle.AcquireAllocationPermitAsync(h.loop, RecordPermit(waiters.back()));
	}
	RunLoopFor(h.loop, 30ms);
	for (const auto& waiter : waiters)
	{
		EXPECT_EQ(0, waiter->calls); // Starting 下只能等
	}

	h.lifecycle.Stop();
	for (const auto& waiter : waiters)
	{
		EXPECT_EQ(0, waiter->calls); // 拒绝排进 loop 队列,不在 Stop 栈内回调
	}
	EXPECT_EQ(agones::LifecycleState::Stopped, h.lifecycle.State());

	auto late = std::make_shared<PermitOutcome>();
	h.lifecycle.AcquireAllocationPermitAsync(h.loop, RecordPermit(late));
	EXPECT_EQ(0, late->calls);

	std::vector<std::shared_ptr<PermitOutcome>> everyone = waiters;
	everyone.push_back(late);
	const auto begin = std::chrono::steady_clock::now();
	ASSERT_TRUE(RunLoopUntil(h.loop, [&] { return AllCalled(everyone); }, 3000ms));
	const long long elapsedMs = ElapsedMs(begin);
	RunLoopFor(h.loop, 50ms);

	for (const auto& outcome : everyone)
	{
		EXPECT_EQ(1, outcome->calls);
		EXPECT_FALSE(outcome->granted);
	}
	EXPECT_LT(elapsedMs, 2000); // 由 Stop 裁决,不是等满上限
	EXPECT_EQ(0, sidecar->CallCount("/allocate"));
	EXPECT_EQ(0u, h.lifecycle.PendingPermitCount());
}

// allocate 结果与 Stop() 并发:worker 结算点与 Stop 的摘表争同一把锁,谁先摘下谁裁决。
// 不论哪边赢,每个等待者恰好回调一次;同一代的等待者要么全部放行、要么全部拒绝;
// 回调保留的 permit 全部析构后在途计数归 0。先后取决于调度,因此连跑几轮。
TEST(AgonesAsyncPermitTest, AllocateRacingStopResolvesEachWaiterExactlyOnce)
{
	constexpr int kRounds = 5;
	constexpr int kWaiters = 4;
	// 每轮一个新的 lifecycle(Stop 是终态)。全部声明在 loop 之前:loop 先析构,队列里没跑的投递
	// 析构时 permit 记账的对象还在。
	std::vector<std::unique_ptr<GameServerLifecycle>> lifecycles;
	muduo::net::EventLoop loop;

	for (int round = 0; round < kRounds; ++round)
	{
		auto state = std::make_shared<HeldAllocateState>();
		state->holdAllocate = true;
		auto options = FastOptions();
		options.allocateWaitTimeout = 3000ms;
		lifecycles.push_back(std::make_unique<GameServerLifecycle>());
		GameServerLifecycle& lifecycle = *lifecycles.back();
		lifecycle.Start(std::make_unique<HoldableAllocateTransport>(state), "http://127.0.0.1:9358", options);
		ASSERT_TRUE(lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms)) << "round " << round;

		using PermitHolder = std::vector<std::shared_ptr<GameServerLifecycle::AllocationPermit>>;
		auto heldPermits = std::make_shared<PermitHolder>();
		std::vector<std::shared_ptr<PermitOutcome>> outcomes;
		for (int i = 0; i < kWaiters; ++i)
		{
			auto outcome = std::make_shared<PermitOutcome>();
			outcomes.push_back(outcome);
			lifecycle.AcquireAllocationPermitAsync(loop,
				[outcome, heldPermits](GameServerLifecycle::AllocationPermit permit) {
					++outcome->calls;
					outcome->granted = static_cast<bool>(permit);
					// 保留 permit,稍后统一析构,验证在途计数的记账。
					heldPermits->push_back(std::make_shared<GameServerLifecycle::AllocationPermit>(std::move(permit)));
				});
		}

		const bool allocateHeld = WaitAllocateHeld(*state);
		// 放行 allocate 后立刻在 loop 线程上 Stop;Stop 的 join 等 worker 把这次 allocate 收完。
		ReleaseAllocate(*state);
		lifecycle.Stop();
		ASSERT_TRUE(allocateHeld) << "round " << round;

		ASSERT_TRUE(RunLoopUntil(loop, [&] { return AllCalled(outcomes); }, 3000ms)) << "round " << round;
		RunLoopFor(loop, 30ms);

		std::size_t granted = 0;
		for (const auto& outcome : outcomes)
		{
			EXPECT_EQ(1, outcome->calls) << "round " << round;
			if (outcome->granted)
			{
				++granted;
			}
		}
		EXPECT_TRUE(granted == 0 || granted == static_cast<std::size_t>(kWaiters))
			<< "round " << round << " granted=" << granted;
		EXPECT_EQ(granted, lifecycle.PendingPermitCount()) << "round " << round;
		heldPermits->clear();
		EXPECT_EQ(0u, lifecycle.PendingPermitCount()) << "round " << round;
		EXPECT_EQ(agones::LifecycleState::Stopped, lifecycle.State()) << "round " << round;
		EXPECT_EQ(1, AllocateCalls(*state)) << "round " << round;
	}
}

// 回调不保留 permit、也没建出单元(参数非法 / 幂等命中):permit 随回调返回析构,零单元零在途 -> 收口回 Ready。
TEST(AgonesAsyncPermitTest, DroppedPermitWithZeroUnitsReturnsToReady)
{
	auto sidecar = std::make_shared<FakeSidecar>();
	auto options = FastOptions();
	options.allocateWaitTimeout = 3000ms;
	AsyncHarness h;
	h.lifecycle.Start(std::make_unique<FakeTransport>(sidecar), "http://127.0.0.1:9358", options);
	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));

	auto outcome = std::make_shared<PermitOutcome>();
	h.lifecycle.AcquireAllocationPermitAsync(h.loop, RecordPermit(outcome));
	ASSERT_TRUE(RunLoopUntil(h.loop, [&] { return outcome->calls > 0; }, 3000ms));
	EXPECT_EQ(1, outcome->calls);
	EXPECT_TRUE(outcome->granted);

	// /ready 共 2 次:启动 1 次 + permit 析构后的零单元收口 1 次。
	ASSERT_TRUE(WaitUntil([&] { return sidecar->CallCount("/ready") >= 2; }, 3000ms));
	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));
	EXPECT_EQ(0, h.lifecycle.UnitCount());
	EXPECT_EQ(0u, h.lifecycle.PendingPermitCount());
	EXPECT_EQ(1, sidecar->CallCount("/allocate"));
}

// ---------------------------------------------------------------------------
// 2026-10-16 §15.16 第 1 条:Stop() 有界、快速返回 ——
// 在途的可放弃请求随取消令牌中止,退避与重试立即退出;/shutdown 不中止。
// ---------------------------------------------------------------------------

namespace
{
	struct CancellableCounts
	{
		int allocateCalls = 0;
		int allocateCancelled = 0;
		int shutdownCalls = 0;
		int shutdownCompleted = 0;
		// /shutdown 收到的令牌是否可取消。必须恒为 false:Stop() 不得中止它。
		bool shutdownTokenCancellable = false;
		// 可放弃请求(/ready、/health、/allocate、GET /gameserver)收到不可取消令牌的次数。必须为 0。
		int abandonableWithoutToken = 0;
	};

	// 可取消 sidecar 的共享状态。cv 用 condition_variable_any:它的 wait 接 stop_token,令牌置位即醒(C++20),
	// 用来模拟 libcurl 进度回调中止在途请求 —— 卡住点完全由令牌或测试放行控制,不靠墙钟长等待。
	struct CancellableSidecarState
	{
		std::mutex mutex;
		std::condition_variable_any cv;
		// /allocate:failAllocate 时立即返回传输失败;否则 holdAllocate 时卡到令牌置位或测试放行。
		bool failAllocate = false;
		bool holdAllocate = false;
		bool allocateEntered = false;
		// /shutdown:holdShutdown 时卡到测试放行。等待同样监听收到的令牌:实现若误带了可取消令牌,Stop() 会把它中止。
		bool holdShutdown = false;
		bool shutdownEntered = false;
		CancellableCounts counts;
	};

	// 卡住点的兜底上限,只防实现回退(没把令牌传下来)时用例挂死:到点即返回失败,用例随后在耗时断言上失败。
	constexpr auto kFakeHoldSafetyCap = 10s;

	class CancellableTransport final : public agones::HttpTransport
	{
	public:
		explicit CancellableTransport(std::shared_ptr<CancellableSidecarState> state)
			: state_(std::move(state))
		{
		}

		// 不可取消版本按空令牌处理:可放弃请求若走到这里,会被记为"没带令牌"。
		agones::HttpResponse Post(const std::string& url,
			const std::string& body,
			const agones::HttpTimeouts& timeouts) override
		{
			return Post(url, body, timeouts, std::stop_token{});
		}

		agones::HttpResponse Get(const std::string& url, const agones::HttpTimeouts& timeouts) override
		{
			return Get(url, timeouts, std::stop_token{});
		}

		agones::HttpResponse Post(const std::string& url,
			const std::string&,
			const agones::HttpTimeouts&,
			std::stop_token cancel) override
		{
			std::unique_lock<std::mutex> lock(state_->mutex);
			CancellableCounts& counts = state_->counts;
			if (url.find("/shutdown") != std::string::npos)
			{
				++counts.shutdownCalls;
				counts.shutdownTokenCancellable = counts.shutdownTokenCancellable || cancel.stop_possible();
				if (state_->holdShutdown)
				{
					state_->shutdownEntered = true;
					state_->cv.notify_all();
					if (!state_->cv.wait_for(lock, cancel, kFakeHoldSafetyCap, [this] { return !state_->holdShutdown; }))
					{
						return Failed("fake: shutdown aborted");
					}
				}
				++counts.shutdownCompleted;
				return Succeeded();
			}

			if (!cancel.stop_possible())
			{
				++counts.abandonableWithoutToken;
			}
			if (url.find("/allocate") != std::string::npos)
			{
				++counts.allocateCalls;
				if (state_->failAllocate)
				{
					return Failed("fake: allocate rejected");
				}
				if (state_->holdAllocate)
				{
					state_->allocateEntered = true;
					state_->cv.notify_all();
					if (!state_->cv.wait_for(lock, cancel, kFakeHoldSafetyCap, [this] { return !state_->holdAllocate; }))
					{
						if (cancel.stop_requested())
						{
							++counts.allocateCancelled;
							return Failed("fake: allocate cancelled");
						}
						return Failed("fake: hold safety cap reached");
					}
				}
			}
			return Succeeded();
		}

		agones::HttpResponse Get(const std::string&, const agones::HttpTimeouts&, std::stop_token cancel) override
		{
			std::lock_guard<std::mutex> lock(state_->mutex);
			if (!cancel.stop_possible())
			{
				++state_->counts.abandonableWithoutToken;
			}
			return Failed("fake: no gameserver");
		}

	private:
		static agones::HttpResponse Succeeded()
		{
			agones::HttpResponse response;
			response.transportOk = true;
			response.statusCode = 200;
			return response;
		}

		static agones::HttpResponse Failed(const char* error)
		{
			agones::HttpResponse response;
			response.error = error;
			return response;
		}

		std::shared_ptr<CancellableSidecarState> state_;
	};

	CancellableCounts Counts(CancellableSidecarState& state)
	{
		std::lock_guard<std::mutex> lock(state.mutex);
		return state.counts;
	}

	// 等 /allocate 卡在 sidecar 里。等不到就放行,免得断言失败后 worker 卡住。
	bool WaitCancellableAllocateEntered(CancellableSidecarState& state)
	{
		std::unique_lock<std::mutex> lock(state.mutex);
		const bool entered = state.cv.wait_for(lock, 3000ms, [&state] { return state.allocateEntered; });
		if (!entered)
		{
			state.holdAllocate = false;
			state.cv.notify_all();
		}
		return entered;
	}

	// 等 /shutdown 卡在 sidecar 里。等不到就放行。
	bool WaitShutdownEntered(CancellableSidecarState& state)
	{
		std::unique_lock<std::mutex> lock(state.mutex);
		const bool entered = state.cv.wait_for(lock, 3000ms, [&state] { return state.shutdownEntered; });
		if (!entered)
		{
			state.holdShutdown = false;
			state.cv.notify_all();
		}
		return entered;
	}

	void ReleaseShutdown(CancellableSidecarState& state)
	{
		{
			std::lock_guard<std::mutex> lock(state.mutex);
			state.holdShutdown = false;
		}
		state.cv.notify_all();
	}

} // namespace

// allocate 在途时于 loop 线程上 Stop():在途请求随取消令牌中止,Stop 远早于 allocate 最坏耗时返回
// (本例不中止就要等满 fake 的 10s 兜底上限;退避期间 Stop 由 stopRequested_ 立即打断,与令牌无关);
// 异步等待者恰好被拒绝一次,在途许可归零,不再重试。
TEST(AgonesStopCancelTest, StopAbortsInFlightAllocateAndRefusesWaiterOnce)
{
	auto state = std::make_shared<CancellableSidecarState>();
	state->holdAllocate = true;
	auto options = FastOptions();
	options.allocateMaxAttempts = 3;
	options.allocateRetryBackoff = 10000ms;
	options.allocateWaitTimeout = 30000ms; // 等待者只能由 Stop 裁决,不让超时定时器抢先
	AsyncHarness h;
	h.lifecycle.Start(std::make_unique<CancellableTransport>(state), "http://127.0.0.1:9358", options);
	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));

	auto outcome = std::make_shared<PermitOutcome>();
	h.lifecycle.AcquireAllocationPermitAsync(h.loop, RecordPermit(outcome));
	ASSERT_TRUE(WaitCancellableAllocateEntered(*state));
	EXPECT_EQ(agones::LifecycleState::Allocating, h.lifecycle.State());

	const auto begin = std::chrono::steady_clock::now();
	h.lifecycle.Stop();
	const long long stopMs = ElapsedMs(begin);
	EXPECT_EQ(0, outcome->calls); // 拒绝排进 loop 队列,不在 Stop 栈内回调

	ASSERT_TRUE(RunLoopUntil(h.loop, [&] { return outcome->calls > 0; }, 3000ms));
	RunLoopFor(h.loop, 30ms);

	EXPECT_LT(stopMs, 1000);
	EXPECT_EQ(1, outcome->calls);
	EXPECT_FALSE(outcome->granted);
	EXPECT_EQ(agones::LifecycleState::Stopped, h.lifecycle.State());
	EXPECT_EQ(0u, h.lifecycle.PendingPermitCount());
	const CancellableCounts counts = Counts(*state);
	EXPECT_EQ(1, counts.allocateCalls);
	EXPECT_EQ(1, counts.allocateCancelled);
	EXPECT_EQ(0, counts.abandonableWithoutToken);
}

// allocate 失败、进入重试退避时 Stop():退避立即被打断,不再发起新的 /allocate;等待者恰好被拒绝一次。
// Stop 落在"失败返回之后、进入退避之前"还是"退避之中"取决于调度,两种情况结论相同。
TEST(AgonesStopCancelTest, StopDuringAllocateBackoffDoesNotRetry)
{
	auto state = std::make_shared<CancellableSidecarState>();
	state->failAllocate = true;
	auto options = FastOptions();
	options.allocateMaxAttempts = 3;
	options.allocateRetryBackoff = 10000ms; // 远长于断言窗口:只有被 Stop 打断才回得来
	options.allocateWaitTimeout = 30000ms;
	AsyncHarness h;
	h.lifecycle.Start(std::make_unique<CancellableTransport>(state), "http://127.0.0.1:9358", options);
	ASSERT_TRUE(h.lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));

	auto outcome = std::make_shared<PermitOutcome>();
	h.lifecycle.AcquireAllocationPermitAsync(h.loop, RecordPermit(outcome));
	ASSERT_TRUE(WaitUntil([&] { return Counts(*state).allocateCalls >= 1; }, 3000ms));

	const auto begin = std::chrono::steady_clock::now();
	h.lifecycle.Stop();
	const long long stopMs = ElapsedMs(begin);

	ASSERT_TRUE(RunLoopUntil(h.loop, [&] { return outcome->calls > 0; }, 3000ms));
	RunLoopFor(h.loop, 30ms);

	EXPECT_LT(stopMs, 1000);
	EXPECT_EQ(1, outcome->calls);
	EXPECT_FALSE(outcome->granted);
	EXPECT_EQ(agones::LifecycleState::Stopped, h.lifecycle.State());
	EXPECT_EQ(0u, h.lifecycle.PendingPermitCount());
	const CancellableCounts counts = Counts(*state);
	EXPECT_EQ(1, counts.allocateCalls);
	EXPECT_EQ(0, counts.abandonableWithoutToken);
}

// /shutdown 是进程决定自我终止时必须送达的最终请求:不带可取消令牌,Stop() 不中止它,等它完成才返回。
TEST(AgonesStopCancelTest, StopDoesNotAbortInFlightShutdown)
{
	auto state = std::make_shared<CancellableSidecarState>();
	state->holdShutdown = true;
	agones::GameServerLifecycle lifecycle;
	lifecycle.Start(std::make_unique<CancellableTransport>(state), "http://127.0.0.1:9358", FastOptions());
	ASSERT_TRUE(lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));

	lifecycle.RequestShutdown();
	const bool entered = WaitShutdownEntered(*state);

	std::future<void> stopped = std::async(std::launch::async, [&lifecycle] { lifecycle.Stop(); });
	// /shutdown 卡住期间 Stop 不得返回。实现若误给它挂上可取消令牌,fake 的等待会随令牌醒来,Stop 很快就回来;
	// 正确实现在这个窗口里必然还在 join,窗口长短只影响能否抓到回退,不会让正确实现失败。
	const bool stopReturnedWhileHeld = stopped.wait_for(100ms) == std::future_status::ready;
	ReleaseShutdown(*state);
	const bool stopReturned = stopped.wait_for(5000ms) == std::future_status::ready;

	ASSERT_TRUE(entered);
	EXPECT_FALSE(stopReturnedWhileHeld);
	ASSERT_TRUE(stopReturned);
	EXPECT_EQ(agones::LifecycleState::Stopped, lifecycle.State());
	const CancellableCounts counts = Counts(*state);
	EXPECT_EQ(1, counts.shutdownCalls);
	EXPECT_EQ(1, counts.shutdownCompleted);
	EXPECT_FALSE(counts.shutdownTokenCancellable);
	EXPECT_EQ(0, counts.abandonableWithoutToken);
}

// RequestShutdown() 之后、worker 还没取走就 Stop():/shutdown 仍必须送达,且恰好一次。
// worker 卡在一次可放弃的 /allocate 里,RequestShutdown() 时取不走请求;Stop() 经令牌中止 /allocate 后 worker
// 回到主循环,先发 /shutdown(不带可取消令牌)再退出。Stop() 之后的 RequestShutdown() 与重复 Stop() 都不补发。
TEST(AgonesStopCancelTest, ShutdownRequestedBeforeStopIsDeliveredExactlyOnce)
{
	auto state = std::make_shared<CancellableSidecarState>();
	state->holdAllocate = true;
	auto options = FastOptions();
	options.allocateWaitTimeout = 30000ms;
	agones::GameServerLifecycle lifecycle;
	lifecycle.Start(std::make_unique<CancellableTransport>(state), "http://127.0.0.1:9358", options);
	ASSERT_TRUE(lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));

	// 非阻塞踢起 allocate(不等结果),让 worker 卡在 /allocate 里腾不出手取 shutdown 请求。
	EXPECT_FALSE(lifecycle.EnsureAllocatedNonBlocking());
	ASSERT_TRUE(WaitCancellableAllocateEntered(*state));

	lifecycle.RequestShutdown();
	const auto begin = std::chrono::steady_clock::now();
	lifecycle.Stop();
	const long long stopMs = ElapsedMs(begin);

	// /shutdown 不卡:Stop() 的耗时只有中止 /allocate 与一次 /shutdown。
	EXPECT_LT(stopMs, 1000);
	EXPECT_EQ(agones::LifecycleState::Stopped, lifecycle.State());
	CancellableCounts counts = Counts(*state);
	EXPECT_EQ(1, counts.allocateCalls);
	EXPECT_EQ(1, counts.allocateCancelled);
	EXPECT_EQ(1, counts.shutdownCalls);
	EXPECT_EQ(1, counts.shutdownCompleted);
	EXPECT_FALSE(counts.shutdownTokenCancellable);
	EXPECT_EQ(0, counts.abandonableWithoutToken);

	lifecycle.RequestShutdown();
	lifecycle.Stop();
	counts = Counts(*state);
	EXPECT_EQ(1, counts.shutdownCalls);
}

// worker 空闲时 RequestShutdown() 紧接 Stop():worker 可能先取走请求(Stop 等在途 /shutdown 完成),也可能还没取走
// (退出前补发)。先后取决于调度,两种情况都必须恰好一次 /shutdown,因此连跑几轮。
TEST(AgonesStopCancelTest, ShutdownRequestedRightBeforeStopOnIdleWorkerIsDeliveredExactlyOnce)
{
	constexpr int kRounds = 5;
	for (int round = 0; round < kRounds; ++round)
	{
		auto state = std::make_shared<CancellableSidecarState>();
		agones::GameServerLifecycle lifecycle;
		lifecycle.Start(std::make_unique<CancellableTransport>(state), "http://127.0.0.1:9358", FastOptions());
		ASSERT_TRUE(lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms)) << "round " << round;

		lifecycle.RequestShutdown();
		lifecycle.Stop();

		EXPECT_EQ(agones::LifecycleState::Stopped, lifecycle.State()) << "round " << round;
		const CancellableCounts counts = Counts(*state);
		EXPECT_EQ(1, counts.shutdownCalls) << "round " << round;
		EXPECT_EQ(1, counts.shutdownCompleted) << "round " << round;
		EXPECT_FALSE(counts.shutdownTokenCancellable) << "round " << round;
	}
}

namespace
{
	// 阻塞等待方与 Stop() 竞争用的 sidecar。/allocate 卡到 Stop() 置位取消令牌为止,随后照常返回成功 ——
	// 模拟响应恰在中止生效前到达:AbandonedByStop 不成立,worker 照常写入 Allocated。
	// 同时用停止回调把 Stop() 拦在 request_stop() 里直到测试放行:Stop() 唤醒等待方的 notify_all 因此晚于
	// worker 写入 Allocated,阻塞等待方只会被 worker 写入之后的 notify 唤醒,醒来时必然是「已停 + Allocated」——
	// 正是要钉住的竞态,每次都命中,不靠调度碰运气。
	struct StopRaceState
	{
		std::mutex mutex;
		std::condition_variable cv;
		bool allocateEntered = false;
		bool stopObserved = false;
		bool releaseStop = false;
		int allocateCalls = 0;
		// 停止回调必须活过 /allocate 的 Post:回调在 Stop() 线程里阻塞期间,worker 要先返回并写入 Allocated
		// (回调对象放在 Post 栈上的话,析构会等回调跑完,两边互等)。只由 worker 线程 emplace 一次;
		// 测试放行之后回调已经返回,随本对象析构时不阻塞。
		std::optional<std::stop_callback<std::function<void()>>> stopHook;
	};

	class StopRaceAllocateTransport final : public agones::HttpTransport
	{
	public:
		explicit StopRaceAllocateTransport(std::shared_ptr<StopRaceState> state)
			: state_(std::move(state))
		{
		}

		agones::HttpResponse Post(const std::string& url,
			const std::string& body,
			const agones::HttpTimeouts& timeouts) override
		{
			return Post(url, body, timeouts, std::stop_token{});
		}

		agones::HttpResponse Get(const std::string&, const agones::HttpTimeouts&) override
		{
			agones::HttpResponse failed;
			failed.error = "fake: no gameserver";
			return failed;
		}

		agones::HttpResponse Post(const std::string& url,
			const std::string&,
			const agones::HttpTimeouts&,
			std::stop_token cancel) override
		{
			if (url.find("/allocate") == std::string::npos)
			{
				return Succeeded();
			}
			{
				std::lock_guard<std::mutex> lock(state_->mutex);
				if (++state_->allocateCalls > 1)
				{
					return Succeeded(); // 只卡第一次
				}
			}

			// 在 state 锁外登记:令牌若已置位,构造函数会就地调用回调,而回调要拿 state 锁。
			// 回调只弱持有 state,不与 state 里的 stopHook 成环。
			state_->stopHook.emplace(cancel,
				std::function<void()>([weak = std::weak_ptr<StopRaceState>(state_)] {
					const std::shared_ptr<StopRaceState> state = weak.lock();
					if (!state)
					{
						return;
					}
					std::unique_lock<std::mutex> lock(state->mutex);
					state->stopObserved = true;
					state->cv.notify_all();
					// 把 Stop() 拦在这里:放行之前它走不到唤醒等待方的 notify_all。兜底上限只防用例挂死。
					state->cv.wait_for(lock, kFakeHoldSafetyCap, [&state] { return state->releaseStop; });
				}));

			std::unique_lock<std::mutex> lock(state_->mutex);
			state_->allocateEntered = true;
			state_->cv.notify_all();
			if (!state_->cv.wait_for(lock, kFakeHoldSafetyCap, [this] { return state_->stopObserved; }))
			{
				agones::HttpResponse failed;
				failed.error = "fake: hold safety cap reached";
				return failed;
			}
			return Succeeded(); // 响应恰在中止生效前到达
		}

	private:
		static agones::HttpResponse Succeeded()
		{
			agones::HttpResponse response;
			response.transportOk = true;
			response.statusCode = 200;
			return response;
		}

		std::shared_ptr<StopRaceState> state_;
	};

	// 在 state 锁内等 predicate 成立,最多 3s。
	template <typename Predicate>
	bool WaitStopRace(StopRaceState& state, Predicate predicate)
	{
		std::unique_lock<std::mutex> lock(state.mutex);
		return state.cv.wait_for(lock, 3000ms, predicate);
	}

	void ReleaseStopRace(StopRaceState& state)
	{
		{
			std::lock_guard<std::mutex> lock(state.mutex);
			state.releaseStop = true;
		}
		state.cv.notify_all();
	}

} // namespace

// 阻塞等待方与 Stop() 竞争:Stop() 已置停止标志之后,在途 /allocate 恰在中止生效前成功、worker 抢先写入 Allocated,
// 阻塞等待方醒来看到「已停 + Allocated」。必须一律拒绝、不留在途许可 —— 与「已停即拒绝」、异步等待方由 Stop()
// 统一拒绝同一口径:Stop() 之后不再签发任何许可。fake 把 Stop() 拦在 request_stop() 里,竞态每次都命中。
TEST(AgonesStopCancelTest, BlockingWaitersAreRefusedWhenAllocatedRacesStop)
{
	auto state = std::make_shared<StopRaceState>();
	auto options = FastOptions();
	options.allocateWaitTimeout = 5000ms; // 等待方只由 worker 的结果或 Stop() 唤醒,不让超时抢先
	GameServerLifecycle lifecycle;
	lifecycle.Start(std::make_unique<StopRaceAllocateTransport>(state), "http://127.0.0.1:9358", options);
	ASSERT_TRUE(lifecycle.WaitForState(agones::LifecycleState::Ready, 3000ms));

	constexpr int kWaiters = 3;
	// 每个等待方把 permit 存进自己的槽位(不同元素,无数据竞争),Stop() 返回之后再统一析构,验证在途计数。
	std::vector<std::unique_ptr<GameServerLifecycle::AllocationPermit>> held(kWaiters);
	std::atomic<int> answered{ 0 };
	std::vector<std::thread> waiters;
	for (int i = 0; i < kWaiters; ++i)
	{
		waiters.emplace_back([&lifecycle, &held, &answered, i] {
			held[i] = std::make_unique<GameServerLifecycle::AllocationPermit>(lifecycle.AcquireAllocationPermitBlocking());
			++answered;
		});
	}
	// /allocate 卡住时第一个等待方必然已在有界等待里(worker 只能在它 wait_for 放锁之后取到请求);
	// 留一点时间让其余等待方也进入等待。个别晚于 Stop() 到达的走「已停即拒绝」,结论相同。
	const bool entered = WaitStopRace(*state, [&state] { return state->allocateEntered; });
	std::this_thread::sleep_for(30ms);

	std::future<void> stopped = std::async(std::launch::async, [&lifecycle] { lifecycle.Stop(); });
	// Stop() 已置停止标志、正被拦在 request_stop() 里;worker 拿到成功响应、写入 Allocated 并唤醒等待方。
	const bool stopObserved = WaitStopRace(*state, [&state] { return state->stopObserved; });
	const bool allocatedWhileStopping = lifecycle.WaitForState(agones::LifecycleState::Allocated, 3000ms);
	const bool allAnswered = WaitUntil([&answered] { return answered.load() == kWaiters; }, 3000ms);
	// 先放行 Stop() 再断言:任何一步失败时 Stop() 都能返回、等待方都能被唤醒,用例不会挂死。
	ReleaseStopRace(*state);
	for (std::thread& waiter : waiters)
	{
		waiter.join();
	}
	const bool stopReturned = stopped.wait_for(5000ms) == std::future_status::ready;

	ASSERT_TRUE(entered);
	ASSERT_TRUE(stopObserved);
	ASSERT_TRUE(allocatedWhileStopping);
	EXPECT_TRUE(allAnswered); // 等待方由 worker 写入 Allocated 之后的 notify 唤醒,不必等 Stop() 放行
	ASSERT_TRUE(stopReturned);

	int granted = 0;
	for (const auto& permit : held)
	{
		if (permit && *permit)
		{
			++granted;
		}
	}
	EXPECT_EQ(0, granted);
	EXPECT_EQ(0u, lifecycle.PendingPermitCount());
	held.clear();
	EXPECT_EQ(0u, lifecycle.PendingPermitCount());
	EXPECT_EQ(agones::LifecycleState::Stopped, lifecycle.State());
	std::lock_guard<std::mutex> lock(state->mutex);
	EXPECT_EQ(1, state->allocateCalls);
}

// ---------------------------------------------------------------------------
// 2026-10-17 §15.17:不走 Stop()、单独 RequestShutdown() 时 ShuttingDown 是终态,RequestShutdown() 幂等
// ---------------------------------------------------------------------------

namespace
{
	// 按发出顺序记录 POST 端点的 sidecar。启用排空时 worker 进主循环前会同步读一次 GET /gameserver,
	// 卡住它即得到"worker 腾不出手、allocate 与 /shutdown 请求同时挂起"的窗口;/shutdown 可卡住,得到"在途"窗口。
	// GET 的卡住同时监听取消令牌:用例中途失败时 Stop() 仍能中止它。/shutdown 不带令牌,只有兜底上限。
	struct ShutdownOrderState
	{
		std::mutex mutex;
		std::condition_variable_any cv;
		std::vector<std::string> posts;
		bool holdGet = false;
		bool getHeld = false;
		bool holdShutdown = false;
		bool shutdownHeld = false;
	};

	class ShutdownOrderTransport final : public agones::HttpTransport
	{
	public:
		explicit ShutdownOrderTransport(std::shared_ptr<ShutdownOrderState> state)
			: state_(std::move(state))
		{
		}

		agones::HttpResponse Post(const std::string& url,
			const std::string& body,
			const agones::HttpTimeouts& timeouts) override
		{
			return Post(url, body, timeouts, std::stop_token{});
		}

		agones::HttpResponse Get(const std::string& url, const agones::HttpTimeouts& timeouts) override
		{
			return Get(url, timeouts, std::stop_token{});
		}

		agones::HttpResponse Post(const std::string& url,
			const std::string&,
			const agones::HttpTimeouts&,
			std::stop_token) override
		{
			const std::string endpoint = url.substr(url.find_last_of('/'));
			std::unique_lock<std::mutex> lock(state_->mutex);
			state_->posts.push_back(endpoint);
			state_->cv.notify_all();
			if (endpoint == "/shutdown" && state_->holdShutdown)
			{
				state_->shutdownHeld = true;
				state_->cv.notify_all();
				state_->cv.wait_for(lock, kFakeHoldSafetyCap, [this] { return !state_->holdShutdown; });
			}
			agones::HttpResponse response;
			response.transportOk = true;
			response.statusCode = 200;
			return response;
		}

		agones::HttpResponse Get(const std::string&, const agones::HttpTimeouts&, std::stop_token cancel) override
		{
			std::unique_lock<std::mutex> lock(state_->mutex);
			if (state_->holdGet)
			{
				state_->getHeld = true;
				state_->cv.notify_all();
				state_->cv.wait_for(lock, cancel, kFakeHoldSafetyCap, [this] { return !state_->holdGet; });
			}
			// 读失败:排空判定保持初值"未排空",与用例无关。
			agones::HttpResponse failed;
			failed.error = "fake: no gameserver";
			return failed;
		}

	private:
		std::shared_ptr<ShutdownOrderState> state_;
	};

	// 只借排空的那次同步 GET 卡住 worker:轮询间隔远长于用例窗口,之后不再有 GET。
	agones::LifecycleOptions ShutdownOrderOptions()
	{
		agones::LifecycleOptions options = FastOptions();
		options.drainLabelKey = "mmorpg.io/drain";
		options.drainPollInterval = 60000ms;
		options.allocateWaitTimeout = 5000ms; // 等待方只能由 ShuttingDown 裁决,不让超时抢先
		return options;
	}

	// 等 worker 卡在 GET 里(此时 RunInitialReady 已写入 Ready)。等不到就放行。
	bool WaitOrderGetHeld(ShutdownOrderState& state)
	{
		std::unique_lock<std::mutex> lock(state.mutex);
		const bool held = state.cv.wait_for(lock, 3000ms, [&state] { return state.getHeld; });
		if (!held)
		{
			state.holdGet = false;
			state.cv.notify_all();
		}
		return held;
	}

	void ReleaseOrderGet(ShutdownOrderState& state)
	{
		{
			std::lock_guard<std::mutex> lock(state.mutex);
			state.holdGet = false;
		}
		state.cv.notify_all();
	}

	// 等 /shutdown 卡在 sidecar 里。等不到就放行。
	bool WaitOrderShutdownHeld(ShutdownOrderState& state)
	{
		std::unique_lock<std::mutex> lock(state.mutex);
		const bool held = state.cv.wait_for(lock, 3000ms, [&state] { return state.shutdownHeld; });
		if (!held)
		{
			state.holdShutdown = false;
			state.cv.notify_all();
		}
		return held;
	}

	void ReleaseOrderShutdown(ShutdownOrderState& state)
	{
		{
			std::lock_guard<std::mutex> lock(state.mutex);
			state.holdShutdown = false;
		}
		state.cv.notify_all();
	}

	int OrderPostCount(ShutdownOrderState& state, const std::string& endpoint)
	{
		std::lock_guard<std::mutex> lock(state.mutex);
		return static_cast<int>(std::count(state.posts.begin(), state.posts.end(), endpoint));
	}

	// 等 /shutdown 之后出现一次 /health。worker 的优先级是 shutdown > allocate > health:看到这次 /health,
	// 说明取走 /shutdown 时挂起的请求都已处理完(误发的 /allocate 或第二次 /shutdown 都会排在它前面)。
	bool WaitOrderHealthAfterShutdown(ShutdownOrderState& state)
	{
		std::unique_lock<std::mutex> lock(state.mutex);
		return state.cv.wait_for(lock, 3000ms, [&state] {
			const auto shutdown = std::find(state.posts.begin(), state.posts.end(), "/shutdown");
			return shutdown != state.posts.end() && std::find(shutdown, state.posts.end(), "/health") != state.posts.end();
		});
	}

} // namespace

// 不走 Stop()、单独 RequestShutdown():取走 /shutdown 之前就挂起的 allocate 不得把状态改回 Allocating / Allocated。
// worker 卡在进主循环前的 GET 里,期间三类调用方(非阻塞踢、异步等待、阻塞等待)都挂上 allocate,随后 RequestShutdown();
// 放行后 worker 先取 /shutdown(优先级最高)再取挂起的 allocate —— 必须丢弃、不发 /allocate。等待中的许可一律拒绝,
// 且远早于等待上限(不等满再误报"allocate 未确认")。修复前 allocate 照常发出,两类等待方都被放行。
TEST(AgonesShutdownTest, PendingAllocateIsDiscardedOnceShuttingDown)
{
	auto state = std::make_shared<ShutdownOrderState>();
	state->holdGet = true;
	AsyncHarness h;
	h.lifecycle.Start(std::make_unique<ShutdownOrderTransport>(state), "http://127.0.0.1:9358", ShutdownOrderOptions());
	ASSERT_TRUE(WaitOrderGetHeld(*state));
	EXPECT_EQ(agones::LifecycleState::Ready, h.lifecycle.State());

	EXPECT_FALSE(h.lifecycle.EnsureAllocatedNonBlocking());
	auto asyncOutcome = std::make_shared<PermitOutcome>();
	h.lifecycle.AcquireAllocationPermitAsync(h.loop, RecordPermit(asyncOutcome));
	std::atomic<bool> blockingAnswered{ false };
	std::atomic<bool> blockingGranted{ true };
	std::atomic<long long> blockingWaitMs{ -1 };
	std::thread blockingWaiter([&h, &blockingAnswered, &blockingGranted, &blockingWaitMs] {
		const auto begin = std::chrono::steady_clock::now();
		// permit 是临时量,本语句结束即析构:放行时也照常归还在途计数。
		blockingGranted = static_cast<bool>(h.lifecycle.AcquireAllocationPermitBlocking());
		blockingWaitMs = ElapsedMs(begin);
		blockingAnswered = true;
	});
	// 留一点时间让阻塞等待方进入等待;它若晚于 ShuttingDown 才到,走当场拒绝,结论相同。
	std::this_thread::sleep_for(30ms);

	h.lifecycle.RequestShutdown();
	ReleaseOrderGet(*state);

	const bool asyncAnswered = RunLoopUntil(h.loop, [&] { return asyncOutcome->calls > 0; }, 3000ms);
	const bool sawHealthAfterShutdown = WaitOrderHealthAfterShutdown(*state);
	const bool blockingDone = WaitUntil([&] { return blockingAnswered.load(); }, 3000ms);
	blockingWaiter.join();
	RunLoopFor(h.loop, 30ms); // 不该再有第二次回调

	ASSERT_TRUE(asyncAnswered);
	EXPECT_EQ(1, asyncOutcome->calls);
	EXPECT_FALSE(asyncOutcome->granted);
	ASSERT_TRUE(blockingDone);
	EXPECT_FALSE(blockingGranted.load());
	EXPECT_LT(blockingWaitMs.load(), 3000); // 等待上限 5000ms:由 ShuttingDown 裁决,不是等满超时
	ASSERT_TRUE(sawHealthAfterShutdown);
	EXPECT_EQ(0, OrderPostCount(*state, "/allocate"));
	EXPECT_EQ(1, OrderPostCount(*state, "/shutdown"));
	EXPECT_EQ(agones::LifecycleState::ShuttingDown, h.lifecycle.State());
	EXPECT_EQ(0u, h.lifecycle.PendingPermitCount());

	// 之后的调用当场拒绝,不再挂 allocate。
	EXPECT_FALSE(h.lifecycle.EnsureAllocatedNonBlocking());
	EXPECT_FALSE(h.lifecycle.AcquireAllocationPermitBlocking());

	h.lifecycle.Stop();
	EXPECT_EQ(0, OrderPostCount(*state, "/allocate"));
	EXPECT_EQ(agones::LifecycleState::Stopped, h.lifecycle.State());
}

// RequestShutdown() 幂等:挂起时重复调、/shutdown 在途时再调、发出之后再调,都不补发。
// 最后 Stop():Stop() 之前收下的请求会在 worker 退出前送达,任何一次被误收下的重复请求都会在这里多出一次 /shutdown,
// 计数因此是确定的。修复前在途时再调会被收下,/shutdown 发两次。
TEST(AgonesShutdownTest, RepeatedRequestShutdownIssuesShutdownOnce)
{
	auto state = std::make_shared<ShutdownOrderState>();
	state->holdGet = true;
	state->holdShutdown = true;
	agones::GameServerLifecycle lifecycle;
	lifecycle.Start(std::make_unique<ShutdownOrderTransport>(state), "http://127.0.0.1:9358", ShutdownOrderOptions());
	const bool getHeld = WaitOrderGetHeld(*state);

	// worker 卡在 GET 里取不走请求:挂起期间重复调。
	lifecycle.RequestShutdown();
	lifecycle.RequestShutdown();
	ReleaseOrderGet(*state);

	// /shutdown 在途:状态已是 ShuttingDown,再调必须忽略。
	const bool shutdownHeld = WaitOrderShutdownHeld(*state);
	const agones::LifecycleState stateWhileInFlight = lifecycle.State();
	lifecycle.RequestShutdown();
	ReleaseOrderShutdown(*state);

	// 发出之后再调,同样忽略。
	const bool sawHealthAfterShutdown = WaitOrderHealthAfterShutdown(*state);
	lifecycle.RequestShutdown();
	lifecycle.Stop();

	ASSERT_TRUE(getHeld);
	ASSERT_TRUE(shutdownHeld);
	EXPECT_EQ(agones::LifecycleState::ShuttingDown, stateWhileInFlight);
	EXPECT_TRUE(sawHealthAfterShutdown);
	EXPECT_EQ(1, OrderPostCount(*state, "/shutdown"));
	EXPECT_EQ(agones::LifecycleState::Stopped, lifecycle.State());
}
