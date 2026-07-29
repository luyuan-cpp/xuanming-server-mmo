#include <gtest/gtest.h>
#include <algorithm>
#include <cstdlib>
#include <array>
#include <functional>
#include <iostream>
#include <limits>
#include <mutex>
#include <thread>
#include <unordered_set>
#include <vector>
#define ENABLE_SNOWFLAKE_TESTING

#include "core/utils/id/snow_flake.h"

using Guid = uint64_t;

using GuidVector = std::vector<Guid>;
using GuidSet = std::unordered_set<Guid>;

// 规模由环境变量控制。原来硬编码 40'000'000:单个用例就是分钟级,`UniqueIds_*`
// 那几个还要乘以 5 个 node —— 整套根本跑不完,于是这套单测事实上从来没人跑。
// 默认给一个秒级的冒烟量,压测口径用 SNOWFLAKE_TEST_TOTAL=40000000 显式打开。
inline size_t SnowFlakeTestTotal()
{
	if (const char* env = std::getenv("SNOWFLAKE_TEST_TOTAL"); env != nullptr && env[0] != '\0') {
		const auto parsed = std::strtoull(env, nullptr, 10);
		if (parsed > 0) {
			return static_cast<size_t>(parsed);
		}
	}
	return 200'000;
}

const size_t kTotal = SnowFlakeTestTotal();
constexpr size_t kThreadCount = 3;

SnowFlakeAtomic idGenAtomic;

void GenerateThreadIds(SnowFlakeAtomic& generator, GuidVector& out)
{
	out.resize(kTotal);
	for (size_t i = 0; i < kTotal; ++i) {
		out[i] = generator.Generate();
	}
}


TEST(TestSnowFlakeThreadSafe, generate)
{
	SnowFlakeAtomic generator;
	generator.set_node_id(1);
	generator.set_epoch(kEpoch);

	std::array<GuidVector, kThreadCount> thread_ids;

	std::thread firstThread(GenerateThreadIds, std::ref(generator), std::ref(thread_ids[0]));
	std::thread secondThread(GenerateThreadIds, std::ref(generator), std::ref(thread_ids[1]));
	std::thread thirdThread(GenerateThreadIds, std::ref(generator), std::ref(thread_ids[2]));

	firstThread.join();
	secondThread.join();
	thirdThread.join();

	for (auto& ids : thread_ids) {
		ASSERT_EQ(ids.size(), kTotal);
		std::sort(ids.begin(), ids.end());
		auto dup = std::adjacent_find(ids.begin(), ids.end());
		ASSERT_TRUE(dup == ids.end()) << "single-thread buffer contains duplicate ID";
	}

	std::array<size_t, kThreadCount> pos{ 0, 0, 0 };
	Guid last = 0;
	bool has_last = false;
	size_t merged_count = 0;

	while (true) {
		int32_t min_idx = -1;
		Guid min_id = 0;

		for (size_t i = 0; i < kThreadCount; ++i) {
			if (pos[i] >= thread_ids[i].size()) {
				continue;
			}

			Guid current = thread_ids[i][pos[i]];
			if (min_idx == -1 || current < min_id) {
				min_id = current;
				min_idx = static_cast<int32_t>(i);
			}
		}

		if (min_idx < 0) {
			break;
		}

		if (has_last) {
			ASSERT_NE(min_id, last) << "duplicate ID detected across threads";
		}

		last = min_id;
		has_last = true;
		++merged_count;
		++pos[min_idx];
	}

	EXPECT_EQ(merged_count, kTotal * kThreadCount);
}

TEST(SnowFlakeTest, ClockRollback_SingleThread) {
	SnowFlake sf;
	sf.set_node_id(1);
	sf.set_mock_now(1000);  // current time is 1000

	auto id1 = sf.Generate();
	auto c1 = ParseGuid(id1);

	sf.set_mock_now(990);  // simulate clock rollback
	auto id2 = sf.Generate();
	auto c2 = ParseGuid(id2);

	EXPECT_GE(c2.timestamp, c1.timestamp);  // ID timestamp must not be earlier than previous
}

TEST(SnowFlakeTest, ClockRollbackSingleThread) {
	SnowFlake sf;
	sf.set_node_id(1);

	sf.set_mock_now(1000);
	Guid id1 = sf.Generate();
	auto c1 = ParseGuid(id1);

	sf.set_mock_now(990); // simulate clock rollback
	Guid id2 = sf.Generate();
	auto c2 = ParseGuid(id2);

	// ID generated after rollback must not be less than previous
	EXPECT_GE(c2.timestamp, c1.timestamp);
	EXPECT_EQ(c2.node_id, c1.node_id);
}

TEST(SnowFlakeTest, Generate100MillionGUIDs_UniqueInSingleNode)
{
	std::unordered_set<Guid> ids;
	ids.reserve(kTotal);

	SnowFlake sf;
	sf.set_node_id(1);
	sf.set_epoch(kEpoch);

	for (size_t i = 0; i < kTotal; ++i) {
		Guid id = sf.Generate();

		// check for duplicates
		auto result = ids.insert(id);
		ASSERT_TRUE(result.second) << "duplicate ID at index " << i << ", ID=" << id;

		// optional: print progress every 10M
		if ((i + 1) % 10'000'000 == 0) {
			std::cout << "Generated " << (i + 1) << " IDs..." << std::endl;
		}
	}

	EXPECT_EQ(ids.size(), kTotal);
}

// Verify batch ID generation
TEST(SnowFlakeTest, GenerateBatch)
{
	SnowFlake sf;
	sf.set_node_id(2);
	sf.set_epoch(kEpoch);

	size_t count = 1000;
	auto ids = sf.GenerateBatch(count);

	EXPECT_EQ(ids.size(), count);

	// check uniqueness
	std::unordered_set<Guid> id_set(ids.begin(), ids.end());
	EXPECT_EQ(id_set.size(), count);
}

// Verify different nodes produce different IDs
TEST(SnowFlakeTest, NodeIDAffectsGeneratedID)
{
	SnowFlake sf1;
	sf1.set_node_id(1);
	sf1.set_epoch(kEpoch);

	SnowFlake sf2;
	sf2.set_node_id(2);
	sf2.set_epoch(kEpoch);

	Guid id1 = sf1.Generate();
	Guid id2 = sf2.Generate();

	EXPECT_NE(id1, id2);
}

// Single-thread + normal generation (SnowFlake::Generate)
TEST(SnowFlakeTest, UniqueIds_SingleThread_NormalGenerate) {
	constexpr int32_t kNodeCount = 5;

	std::unordered_set<Guid> all_ids;
	all_ids.reserve(kNodeCount * kTotal);

	for (int32_t node = 0; node < kNodeCount; ++node) {
		SnowFlake sf;
		sf.set_node_id(static_cast<uint32_t>(node));

		for (int32_t i = 0; i < kTotal; ++i) {
			Guid id = sf.Generate();
			auto [_, inserted] = all_ids.insert(id);
			ASSERT_TRUE(inserted) << "Duplicate ID from node " << node;
		}
	}

	EXPECT_EQ(all_ids.size(), kNodeCount * kTotal);
}

// Single-thread + batch generation (SnowFlake::GenerateBatch)
TEST(SnowFlakeTest, UniqueIds_SingleThread_BatchGenerate) {
	constexpr int32_t kNodeCount = 5;

	std::unordered_set<Guid> all_ids;
	all_ids.reserve(kNodeCount * kTotal);

	for (int32_t node = 0; node < kNodeCount; ++node) {
		SnowFlake sf;
		sf.set_node_id(static_cast<uint32_t>(node));

		auto ids = sf.GenerateBatch(kTotal);
		for (const auto& id : ids) {
			auto [_, inserted] = all_ids.insert(id);
			ASSERT_TRUE(inserted) << "Duplicate ID from node " << node;
		}
	}

	EXPECT_EQ(all_ids.size(), kNodeCount * kTotal);
}

// Multi-thread + normal generation (SnowFlakeAtomic::Generate)
TEST(SnowFlakeTest, UniqueIds_MultiThread_NormalGenerate) {
	constexpr int32_t kNodeCount = 5;

	std::unordered_set<Guid> all_ids;
	std::mutex id_mutex;

	std::vector<std::thread> threads;

	for (int32_t node = 0; node < kNodeCount; ++node) {
		threads.emplace_back([node, &all_ids, &id_mutex]() {
			SnowFlakeAtomic sf;
			sf.set_node_id(static_cast<uint32_t>(node));

			for (int32_t i = 0; i < kTotal; ++i) {
				Guid id = sf.Generate();
				std::lock_guard<std::mutex> lock(id_mutex);
				auto [_, inserted] = all_ids.insert(id);
				ASSERT_TRUE(inserted) << "Duplicate ID in thread node " << node;
			}
			});
	}

	for (auto& t : threads) {
		t.join();
	}

	EXPECT_EQ(all_ids.size(), kNodeCount * kTotal);
}

// Multi-thread + batch generation (SnowFlakeAtomic::GenerateBatch)
TEST(SnowFlakeTest, UniqueIds_MultiThread_BatchGenerate) {
	constexpr int32_t kNodeCount = 5;

	std::unordered_set<Guid> all_ids;
	std::mutex id_mutex;

	std::vector<std::thread> threads;

	for (int32_t node = 0; node < kNodeCount; ++node) {
		threads.emplace_back([node, &all_ids, &id_mutex]() {
			SnowFlakeAtomic sf;
			sf.set_node_id(static_cast<uint32_t>(node));

			auto ids = sf.GenerateBatch(kTotal);
			std::lock_guard<std::mutex> lock(id_mutex);
			for (const auto& id : ids) {
				auto [_, inserted] = all_ids.insert(id);
				ASSERT_TRUE(inserted) << "Duplicate ID in thread node " << node;
			}
			});
	}

	for (auto& t : threads) {
		t.join();
	}

	EXPECT_EQ(all_ids.size(), kNodeCount * kTotal);
}


TEST(TestSnowFlakeThreadSafe, justGenerateTime)
{
	auto start = std::chrono::high_resolution_clock::now();
	Guid id = idGenAtomic.Generate();
	auto end = std::chrono::high_resolution_clock::now();
	std::chrono::duration<double> duration = end - start;

	std::cout << "Single generation time: " << duration.count() << " seconds\n";
	std::cout << "Generated ID: " << id << std::endl;
}



TEST(SnowFlakeTest, SequentialOrder) {
	SnowFlake gen;
	gen.set_node_id(1);
	Guid last = gen.Generate();

	for (int32_t i = 0; i < 1000; ++i) {
		Guid next = gen.Generate();
		ASSERT_GT(next, last) << "ID should be monotonically increasing";
		last = next;
	}
}

TEST(SnowFlakeTest, MultiThreadUniqueness) {
	constexpr int32_t THREAD_COUNT = 8;
	constexpr int32_t IDS_PER_THREAD = 5000;

	SnowFlakeAtomic gen;
	gen.set_node_id(2);
	std::unordered_set<Guid> ids;
	std::mutex mutex;

	auto worker = [&]() {
		std::unordered_set<Guid> local_ids;
		for (int32_t i = 0; i < IDS_PER_THREAD; ++i) {
			local_ids.insert(gen.Generate());
		}

		std::lock_guard<std::mutex> lock(mutex);
		for (const auto& id : local_ids) {
			auto [_, inserted] = ids.insert(id);
			ASSERT_TRUE(inserted) << "Duplicate ID detected in multithreaded context";
		}
		};

	std::vector<std::thread> threads;
	for (int32_t i = 0; i < THREAD_COUNT; ++i) {
		threads.emplace_back(worker);
	}

	for (auto& t : threads) {
		t.join();
	}

	ASSERT_EQ(ids.size(), THREAD_COUNT * IDS_PER_THREAD);
}

TEST(SnowFlakeTest, IdParsing) {
	SnowFlake gen;
	gen.set_node_id(3);
	Guid id = gen.Generate();
	auto components = ParseGuid(id);
	auto real_time = GetRealTimeFromGuid(id);

	ASSERT_EQ(components.node_id, 3);
	ASSERT_LT(components.sequence, (1ULL << kStepBits));
	ASSERT_GT(real_time, 1600000000); // Unix timestamp sanity check
}

// Test batch generation uniqueness
TEST(SnowFlakeTest, GenerateBatchUnique)
{
	SnowFlake generator;
	generator.set_node_id(1);

	const size_t batch_size = 10000;
	std::vector<Guid> ids = generator.GenerateBatch(batch_size);

	ASSERT_EQ(ids.size(), batch_size);

	std::unordered_set<Guid> unique_ids(ids.begin(), ids.end());
	EXPECT_EQ(unique_ids.size(), ids.size()) << "IDs are not unique";
}

// Test correctness of multiple batch generations
TEST(SnowFlakeTest, MultipleBatchGeneration)
{
	SnowFlakeAtomic generator;
	generator.set_node_id(123);

	const size_t batch1 = 5000;
	const size_t batch2 = 8000;

	std::vector<Guid> ids1 = generator.GenerateBatch(batch1);
	std::vector<Guid> ids2 = generator.GenerateBatch(batch2);

	std::unordered_set<Guid> all(ids1.begin(), ids1.end());
	all.insert(ids2.begin(), ids2.end());

	EXPECT_EQ(all.size(), batch1 + batch2) << "Duplicate IDs found across batches";
}

// Test parsing ID components
TEST(SnowFlakeTest, GuidParsing)
{
	SnowFlake generator;
	generator.set_node_id(42);
	Guid id = generator.Generate();

	auto parsed = ParseGuid(id);

	EXPECT_EQ(parsed.node_id, 42);
	EXPECT_LT(parsed.sequence, (1U << kStepBits));
	EXPECT_GT(parsed.timestamp, 0);
}

TEST(SnowFlakeTest, AtomicGuidParsingUsesDefaultEpoch)
{
	SnowFlakeAtomic generator;
	generator.set_node_id(7);
	generator.set_epoch(kEpoch);

	Guid id = generator.Generate();
	auto parsed = ParseGuid(id);
	auto real_time = GetRealTimeFromGuid(id);

	EXPECT_EQ(parsed.node_id, 7);
	EXPECT_LT(parsed.sequence, (1U << kStepBits));
	EXPECT_GE(real_time, static_cast<std::time_t>(kEpoch));
}

TEST(SnowFlakeTest, AtomicSupportsBeyondUint32Timestamp)
{
	SnowFlakeAtomic generator;
	generator.set_node_id(9);
	generator.set_epoch(kEpoch);

	const uint64_t beyond = kEpoch + static_cast<uint64_t>(std::numeric_limits<uint32_t>::max()) + 1ULL;
	generator.set_mock_static_time(beyond);

	Guid id1 = generator.Generate();
	Guid id2 = generator.Generate();

	auto p1 = ParseGuid(id1);
	auto p2 = ParseGuid(id2);

	EXPECT_NE(id1, id2);
	EXPECT_EQ(p1.timestamp, p2.timestamp);
	EXPECT_LT(p1.sequence, p2.sequence);
	EXPECT_EQ(p1.timestamp, beyond - kEpoch);
}

// Concurrent stress test: multiple threads batch-generate, ensuring uniqueness
TEST(SnowFlakeTest, ConcurrentBatchGeneration)
{
	constexpr size_t kThreads = 8;
	constexpr size_t kIDsPerThread = 10000;
	constexpr size_t kTotalIDs = kThreads * kIDsPerThread;

	SnowFlakeAtomic generator;
	generator.set_node_id(2);

	std::vector<std::thread> threads;
	std::vector<Guid> all_ids;
	all_ids.reserve(kTotalIDs);

	std::mutex mutex;  // protects all_ids

	auto worker = [&]() {
		auto ids = generator.GenerateBatch(kIDsPerThread);
		std::lock_guard<std::mutex> lock(mutex);
		all_ids.insert(all_ids.end(), ids.begin(), ids.end());
		};

	for (size_t i = 0; i < kThreads; ++i) {
		threads.emplace_back(worker);
	}

	for (auto& t : threads) {
		t.join();
	}

	// verify total count
	ASSERT_EQ(all_ids.size(), kTotalIDs);

	// verify uniqueness
	std::unordered_set<Guid> unique_ids(all_ids.begin(), all_ids.end());
	EXPECT_EQ(unique_ids.size(), all_ids.size()) << "concurrent generation produced duplicate IDs";
}

// Verify different node IDs do not produce duplicate GUIDs
TEST(SnowFlakeTest, DifferentNodeNoDuplicate)
{
	SnowFlake genA;
	genA.set_node_id(3);

	SnowFlake genB;
	genB.set_node_id(4);

	std::unordered_set<Guid> all_ids;
	constexpr size_t kCount = 10000;

	for (size_t i = 0; i < kCount; ++i) {
		auto [_, ok1] = all_ids.insert(genA.Generate());
		ASSERT_TRUE(ok1) << "Duplicate from node A at i=" << i;

		auto [__, ok2] = all_ids.insert(genB.Generate());
		ASSERT_TRUE(ok2) << "Duplicate from node B at i=" << i;
	}

	EXPECT_EQ(all_ids.size(), kCount * 2);
}

// ---------------------------------------------------------------------------
// 回归用例:唯一性 / 单调性在异常时钟与混用接口下仍然成立
// ---------------------------------------------------------------------------

// Generate 与 GenerateBatch 必须共用同一条序列。
// 修复前:Generate 里 step_ 表示"最后用掉的",GenerateBatch 里当成"下一个可用的",
// 于是批量的第一个 ID 与上一次 Generate 的返回值逐位相同。
TEST(SnowFlakeRegression, GenerateAndGenerateBatchShareOneSequence)
{
	SnowFlake sf;
	sf.set_node_id(4);

	std::unordered_set<Guid> ids;
	Guid prev = 0;

	for (int32_t i = 0; i < 8; ++i) {
		const Guid id = sf.Generate();
		ASSERT_TRUE(ids.insert(id).second) << "Generate replayed an ID at i=" << i;
		ASSERT_GT(id, prev);
		prev = id;
	}

	for (const Guid id : sf.GenerateBatch(8)) {
		ASSERT_TRUE(ids.insert(id).second)
			<< "GenerateBatch replayed an ID already handed out by Generate: " << id;
		ASSERT_GT(id, prev);
		prev = id;
	}

	for (int32_t i = 0; i < 8; ++i) {
		const Guid id = sf.Generate();
		ASSERT_TRUE(ids.insert(id).second) << "Generate replayed an ID after a batch at i=" << i;
		ASSERT_GT(id, prev);
		prev = id;
	}
}

// 时钟停摆(或大幅回拨)时,高水位秒绝不能被写回更小的值 ——
// 写回就等于把已经发出去的 (秒, step) 组合重新发一遍。
// 修复前:WaitNextTime 耗尽重试后 LOG_FATAL(muduo 的 FATAL 直接 abort 进程),
// 而它写的 `break` 路径会让调用方执行 last_time_ = now(更小)。
TEST(SnowFlakeRegression, StalledClockNeverReplaysIds)
{
	SnowFlake sf;
	sf.set_node_id(3);
	sf.set_epoch(0);
	sf.set_mock_static_time(1000); // 时钟冻在第 1000 秒
	sf.SetGuardTime(1000);         // 这一秒的 step 池直接置满,下一次 Generate 必然走等待分支

	Guid prev = 0;
	for (int32_t i = 0; i < 64; ++i) {
		const Guid id = sf.Generate();
		ASSERT_GT(id, prev) << "ID regressed at iteration " << i;
		prev = id;
	}

	// 借了逻辑秒继续发号,时间段只能前进,不能退回 1000。
	EXPECT_GT(prev >> kTimeShift, 1000ULL);
}

// guard 时间早于 epoch 时,uint64 减法会下溢成天文数字,把 last_time_ 顶到远未来,
// 之后所有 ID 的时间段都是错的且再也不推进。必须被识别并忽略。
TEST(SnowFlakeRegression, GuardTimeBeforeEpochIsIgnored)
{
	SnowFlake sf;
	sf.set_node_id(5);
	sf.set_epoch(1000);
	sf.set_mock_static_time(1005); // now_epoch == 5

	sf.SetGuardTime(10); // 早于 epoch,非法

	const Guid id = sf.Generate();
	EXPECT_EQ(id >> kTimeShift, 5ULL) << "guard underflow poisoned the time segment";
}

// 原子版同样不允许把高水位往回写。
TEST(SnowFlakeRegression, AtomicStalledClockNeverReplaysIds)
{
	SnowFlakeAtomic sf;
	sf.set_node_id(6);
	sf.set_epoch(0);
	sf.set_mock_static_time(2000);

	Guid prev = 0;
	for (int32_t i = 0; i < 64; ++i) {
		const Guid id = sf.Generate();
		ASSERT_GT(id, prev) << "ID regressed at iteration " << i;
		prev = id;
	}
	EXPECT_GE(prev >> kTimeShift, 2000ULL);
}

int32_t main(int32_t argc, char** argv)
{
	testing::InitGoogleTest(&argc, argv);
	return RUN_ALL_TESTS();
}
