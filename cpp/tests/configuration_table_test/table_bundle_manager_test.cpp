// 配置表热更的进程内半边(docs/design/config-table-hot-reload.md §8):清单与指针解析、逐表差异,
// 表包管理器的启动加载、分片准备、校验拒绝、定时提交、撤销与旧表退役,外加表达式的只读求值。
//
// 表数据用仓里真实导出的那一套:拷进临时目录当「表包」。做新包时往某张表的 json 末尾补一个换行
// (内容不变、文件摘要变),或把整张表换成给定内容;清单里对应的 size / sha256 与 content_digest 同步改。
// 不改仓库里的任何文件。
//
// 时间只走注入的假时钟,用 PollOnce 推进,不 sleep;事件循环只负责把 queueInLoop 接力的分片跑完。
// 管理器会整批换掉进程里的现役表,所以每条用例结束时重新 LoadTables(),把全局表状态还给同一个可执行文件里的其他用例。
#include <chrono>
#include <cstdint>
#include <cstdlib>
#include <filesystem>
#include <fstream>
#include <functional>
#include <memory>
#include <optional>
#include <sstream>
#include <string>
#include <string_view>
#include <system_error>
#include <utility>
#include <vector>

#include "gmock/gmock.h"
#include "gtest/gtest.h"

#include "google/protobuf/struct.pb.h"
#include "google/protobuf/util/json_util.h"
#include "muduo/net/EventLoop.h"

#include "engine/config/config.h"
#include "metrics/prometheus_text.h"
#include "table/code/messagelimiter_table.h"
#include "table/code/table_loader.h"
#include "table_bundle/table_bundle_manager.h"
#include "table_expression.h"
#include "utils/encode/sha256.h"

namespace
{

namespace fs = std::filesystem;
using table_bundle::BundleManifest;
using table_bundle::Hooks;
using table_bundle::ManifestArtifact;
using table_bundle::ManifestTableEntry;
using table_bundle::PointerLine;
using table_bundle::TableBundleManager;
using ::testing::HasSubstr;

// MessageLimiter 在可热更名单里,World 不在(生成的 table_loader.cpp 里的 kHotReloadableTables);夹具 SetUp 会复核。
constexpr std::string_view kHotReloadableFile = "messagelimiter.json";
constexpr std::string_view kStructuralFile = "world.json";
// 一张合法的空表:换上它之后行数是 0,用来分辨「读到的是哪一套表」。
constexpr std::string_view kEmptyTableJson = "{\"data\":[]}";

void SetEnvVar(const char *name, const std::string &value)
{
#ifdef _WIN32
	_putenv_s(name, value.c_str());
#else
	if (value.empty())
	{
		unsetenv(name);
	}
	else
	{
		setenv(name, value.c_str(), 1);
	}
#endif
}

std::string ReadAll(const fs::path &path)
{
	std::ifstream in(path, std::ios::binary);
	std::ostringstream content;
	content << in.rdbuf();
	return content.str();
}

void WriteAll(const fs::path &path, const std::string &content)
{
	std::ofstream out(path, std::ios::binary | std::ios::trunc);
	out.write(content.data(), static_cast<std::streamsize>(content.size()));
}

// 读出 dir 里的清单,交给 mutate 改,再写回去。
void MutateManifest(const fs::path &dir, const std::function<void(google::protobuf::Struct &)> &mutate)
{
	const fs::path path = dir / table_bundle::kManifestFilename;
	google::protobuf::Struct doc;
	ASSERT_TRUE(google::protobuf::util::JsonStringToMessage(ReadAll(path), &doc).ok());
	mutate(doc);
	std::string out;
	ASSERT_TRUE(google::protobuf::util::MessageToJsonString(doc, &out).ok());
	WriteAll(path, out);
}

// 文件改过之后让清单跟上:tableFile 的 json 产物换成文件现状的 size / sha256,content_digest 换成 newDigest。
void RepointManifest(const fs::path &dir, const std::string &tableFile, const std::string &newDigest)
{
	const std::string bytes = ReadAll(dir / tableFile);
	bool patched = false;
	MutateManifest(dir, [&tableFile, &newDigest, &bytes, &patched](google::protobuf::Struct &doc)
				   {
					   auto &fields = *doc.mutable_fields();
					   fields["content_digest"].set_string_value(newDigest);
					   for (auto &table : *fields["tables"].mutable_list_value()->mutable_values())
					   {
						   auto &tableFields = *table.mutable_struct_value()->mutable_fields();
						   for (auto &artifact : *tableFields["artifacts"].mutable_list_value()->mutable_values())
						   {
							   auto &artifactFields = *artifact.mutable_struct_value()->mutable_fields();
							   if (artifactFields["kind"].string_value() == "json" &&
								   artifactFields["file"].string_value() == tableFile)
							   {
								   artifactFields["size"].set_number_value(static_cast<double>(bytes.size()));
								   artifactFields["sha256"].set_string_value(Sha256::HashToHex(bytes));
								   patched = true;
							   }
						   }
					   }
				   });
	ASSERT_TRUE(patched) << "manifest has no json artifact for " << tableFile;
}

struct NextTarget
{
	std::string digest;
	uint64_t activateAtMs = 0;
	bool immediate = false;
};

// sidecar 写出的 POINTER.json 的形状:lines 按 schema_digest 分线,每条线一个 current、一个可选的 next。
std::string PointerJson(const std::string_view schemaDigest,
						const std::string &current,
						const std::optional<NextTarget> &next)
{
	std::ostringstream json;
	json << "{\"track\":\"stable\",\"lines\":{\"" << schemaDigest << "\":{"
		 << "\"current\":{\"content_digest\":\"" << current << "\",\"version\":1}";
	if (next)
	{
		json << ",\"next\":{\"content_digest\":\"" << next->digest << "\",\"version\":2"
			 << ",\"activate_at_ms\":" << next->activateAtMs
			 << ",\"immediate\":" << (next->immediate ? "true" : "false") << "}";
	}
	json << ",\"updated_at_ms\":1,\"updated_by\":\"unit-test\",\"reason\":\"unit test\"}}}";
	return json.str();
}

// 在测试线程自建的 loop 上转,直到 done() 为真或超时;返回 done() 的最终结果。
// 只用来把 queueInLoop 接力的分片跑完:runEvery 只做条件检查,runAfter 是到点无条件收尾的保险,不是用例的时间来源。
// 写法同 agones_lifecycle_test.cpp 的同名函数。不能在 loop 自己的回调里调用。
bool RunLoopUntil(muduo::net::EventLoop &loop, const std::function<bool()> &done)
{
	if (done())
	{
		return true;
	}
	const muduo::net::TimerId check = loop.runEvery(0.005, [&loop, &done]
													{
														if (done())
														{
															loop.quit();
														}
													});
	const muduo::net::TimerId giveUp = loop.runAfter(30.0, [&loop]
													 { loop.quit(); });
	loop.loop();
	loop.cancel(check);
	loop.cancel(giveUp);
	return done();
}

std::string MetricsText(const TableBundleManager &manager)
{
	prometheus_text::Writer writer;
	manager.WriteMetrics(writer);
	return writer.Text();
}

int LiveMessageLimiterRows()
{
	return MessageLimiterTableManager::Instance().FindAll().data_size();
}

// 两个假时钟,只由用例手动推进。起点是固定值,与真实时间无关。
struct FakeClock
{
	uint64_t wallMs = 1'800'000'000'000;
	std::chrono::steady_clock::time_point steady{};
};

class TableBundleManagerTest : public ::testing::Test
{
protected:
	void SetUp() override
	{
		ASSERT_TRUE(IsTableHotReloadable("MessageLimiter")) << "pick another hot-reloadable table for these tests";
		ASSERT_FALSE(IsTableHotReloadable("World")) << "pick another structural table for these tests";
		ASSERT_GT(LiveMessageLimiterRows(), 0) << "these tests tell table sets apart by MessageLimiter row count";

		const auto nonce = std::chrono::steady_clock::now().time_since_epoch().count();
		root = fs::temp_directory_path() / ("tbm_" + std::to_string(nonce));
		statusRoot = root / "status";
		ASSERT_TRUE(fs::create_directories(root / "bundles"));

		// 仓里导出的那一套就是第一包:目录名取它清单里的 content_digest。
		BundleManifest shipped;
		std::string err;
		ASSERT_TRUE(table_bundle::VerifyTableDirManifest(GetConfigDir(), false, kTableSchemaDigest, shipped, err)) << err;
		baseDigest = shipped.contentDigest;
		fs::copy(fs::path(GetConfigDir()), BundleDir(baseDigest), fs::copy_options::recursive);

		WritePointer(baseDigest, std::nullopt);
		SetEnvVar(table_bundle::kBundleRootEnv, root.string());
		SetEnvVar(table_bundle::kStatusRootEnv, statusRoot.string());
	}

	void TearDown() override
	{
		TableBundleManager::SetProcessHooks({});
		SetEnvVar(table_bundle::kBundleRootEnv, "");
		SetEnvVar(table_bundle::kStatusRootEnv, "");
		// 管理器换过进程里的现役表:按配置重新加载一遍,连同身份一起还原,不把本用例的表包带给后面的用例。
		LoadTables();
		SetCurrentTableBundleIdentity(TableBundleIdentity{});
		std::error_code ignored;
		fs::remove_all(root, ignored);
	}

	fs::path BundleDir(const std::string &digest) const { return root / "bundles" / digest; }

	void WritePointer(const std::string &current, const std::optional<NextTarget> &next) const
	{
		WriteAll(root / table_bundle::kPointerFilename, PointerJson(kTableSchemaDigest, current, next));
	}

	std::string StatusText() const { return ReadAll(statusRoot / table_bundle::kStatusFilename); }

	// 以 fromDigest 那一包为底做一包新表,返回新包的 content_digest(由 label 派生,同一条用例里各不相同)。
	// replacement 为空:往 tableFile 末尾补一个换行;否则把整个文件换成 replacement。
	// 里面用了 ASSERT,调用之后要查 HasFatalFailure()。
	std::string MakeCandidate(const std::string &fromDigest,
							  const std::string_view tableFile,
							  const std::string &label,
							  const std::optional<std::string> &replacement = std::nullopt) const
	{
		const std::string file(tableFile);
		const std::string digest = Sha256::HashToHex(fromDigest + "/" + label);
		const fs::path dir = BundleDir(digest);
		fs::copy(BundleDir(fromDigest), dir, fs::copy_options::recursive);
		if (replacement)
		{
			WriteAll(dir / file, *replacement);
		}
		else
		{
			std::ofstream out(dir / file, std::ios::binary | std::ios::app);
			out << '\n';
		}
		RepointManifest(dir, file, digest);
		return digest;
	}

	// 按 POINTER.json 启动一个表包模式的管理器,时间读夹具里的假时钟。
	std::shared_ptr<TableBundleManager> StartManager()
	{
		TableBundleManager::Clock clock;
		clock.wallUnixMs = [fake = fakeClock]
		{ return fake->wallMs; };
		clock.steadyNow = [fake = fakeClock]
		{ return fake->steady; };
		auto manager = std::make_shared<TableBundleManager>(loop, std::move(clock));
		manager->LoadAtStartup(GetConfigDir(), false, "table-bundle-test");
		return manager;
	}

	// loop 先于用例里的管理器构造、后于它析构:管理器的轮询定时器要在 loop 还活着时取消。
	muduo::net::EventLoop loop;
	std::shared_ptr<FakeClock> fakeClock = std::make_shared<FakeClock>();
	fs::path root;
	fs::path statusRoot;
	std::string baseDigest;
};

// ---------------------------------------------------------------------------
// 清单、指针、逐表差异
// ---------------------------------------------------------------------------

TEST_F(TableBundleManagerTest, ManifestVerificationAcceptsShippedTablesAndRejectsOtherSchema)
{
	const std::string dir = BundleDir(baseDigest).string();
	BundleManifest manifest;
	std::string err;
	ASSERT_TRUE(table_bundle::VerifyTableDirManifest(dir, false, kTableSchemaDigest, manifest, err)) << err;
	EXPECT_EQ(manifest.schemaDigest, std::string(kTableSchemaDigest));
	EXPECT_EQ(manifest.contentDigest, baseDigest);
	EXPECT_TRUE(manifest.tables.contains("MessageLimiter"));
	EXPECT_TRUE(manifest.tables.contains("World"));

	err.clear();
	EXPECT_FALSE(table_bundle::VerifyTableDirManifest(dir, false, "0123456789abcdef", manifest, err));
	EXPECT_THAT(err, HasSubstr("schema_digest mismatch"));
}

TEST_F(TableBundleManagerTest, ManifestVerificationRejectsTamperedFileAndOldManifestVersion)
{
	const fs::path dir = BundleDir(baseDigest);
	BundleManifest manifest;
	std::string err;

	// 文件多了一个字节,清单没跟着改
	{
		std::ofstream out(dir / kHotReloadableFile, std::ios::binary | std::ios::app);
		out << '\n';
	}
	EXPECT_FALSE(table_bundle::VerifyTableDirManifest(dir.string(), false, kTableSchemaDigest, manifest, err));
	EXPECT_THAT(err, HasSubstr(std::string(kHotReloadableFile)));

	// 第 1 版清单(没有 schema_digest 的年代)一律不认,先于文件核对报错
	MutateManifest(dir, [](google::protobuf::Struct &doc)
				   { (*doc.mutable_fields())["schema_version"].set_number_value(1); });
	ASSERT_FALSE(HasFatalFailure());
	err.clear();
	EXPECT_FALSE(table_bundle::VerifyTableDirManifest(dir.string(), false, kTableSchemaDigest, manifest, err));
	EXPECT_THAT(err, HasSubstr("schema_version"));
}

TEST_F(TableBundleManagerTest, PointerLineIsReadPerSchemaLine)
{
	const std::string pointerPath = (root / table_bundle::kPointerFilename).string();
	const std::string nextDigest = Sha256::HashToHex("pointer-next");
	std::optional<PointerLine> line;
	std::string err;

	WritePointer(baseDigest, NextTarget{nextDigest, 123456, true});
	ASSERT_TRUE(table_bundle::ReadPointerLine(pointerPath, kTableSchemaDigest, line, err)) << err;
	ASSERT_TRUE(line.has_value());
	ASSERT_TRUE(line->current.has_value());
	EXPECT_EQ(line->current->contentDigest, baseDigest);
	ASSERT_TRUE(line->next.has_value());
	EXPECT_EQ(line->next->contentDigest, nextDigest);
	EXPECT_EQ(line->next->activateAtMs, 123456u);
	EXPECT_TRUE(line->next->immediate);

	// 文件里没有本二进制这条 schema 线:读成功,结果为空(调用方回落到镜像自带的表)
	ASSERT_TRUE(table_bundle::ReadPointerLine(pointerPath, "ffffffffffffffff", line, err)) << err;
	EXPECT_FALSE(line.has_value());

	WriteAll(root / table_bundle::kPointerFilename, "{not json");
	EXPECT_FALSE(table_bundle::ReadPointerLine(pointerPath, kTableSchemaDigest, line, err));
	EXPECT_FALSE(line.has_value());
}

// 启动时本线不在 lines 里,靠 sync_errors 分清「etcd 里没有这条线」和「有,但表包没取到」:后一种拒绝启动。
TEST_F(TableBundleManagerTest, SyncErrorIsReadPerSchemaLine)
{
	const std::string pointerPath = (root / table_bundle::kPointerFilename).string();
	std::string reason = "stale";

	// sidecar 一切正常时不写 sync_errors
	EXPECT_FALSE(table_bundle::ReadPointerSyncError(pointerPath, kTableSchemaDigest, reason));
	EXPECT_TRUE(reason.empty());

	WriteAll(root / table_bundle::kPointerFilename,
			 "{\"track\":\"stable\",\"lines\":{},\"sync_errors\":{\"" + std::string(kTableSchemaDigest) +
				 "\":\"download tables/x.tar.gz: status 503\"}}");
	EXPECT_TRUE(table_bundle::ReadPointerSyncError(pointerPath, kTableSchemaDigest, reason));
	EXPECT_EQ(reason, "download tables/x.tar.gz: status 503");

	// 别的线没同步下来,不影响本线
	EXPECT_FALSE(table_bundle::ReadPointerSyncError(pointerPath, "ffffffffffffffff", reason));
	EXPECT_TRUE(reason.empty());

	WriteAll(root / table_bundle::kPointerFilename, "{not json");
	EXPECT_FALSE(table_bundle::ReadPointerSyncError(pointerPath, kTableSchemaDigest, reason));
}

ManifestTableEntry EntryWithDigests(const std::string &name, const std::string &jsonSha, const std::string &binarySha)
{
	ManifestTableEntry entry;
	entry.name = name;
	entry.artifacts.push_back(ManifestArtifact{"json", name + ".json", 1, jsonSha});
	entry.artifacts.push_back(ManifestArtifact{"binary", name + ".pb", 1, binarySha});
	return entry;
}

TEST(TableBundleManifestDiffTest, ListsChangedAddedAndRemovedTablesSortedForTheActiveFormat)
{
	BundleManifest current;
	current.tables.emplace("Alpha", EntryWithDigests("Alpha", "j1", "b1"));
	current.tables.emplace("Beta", EntryWithDigests("Beta", "j2", "b2"));
	current.tables.emplace("Gamma", EntryWithDigests("Gamma", "j3", "b3"));

	BundleManifest candidate;
	candidate.tables.emplace("Alpha", EntryWithDigests("Alpha", "j1-changed", "b1")); // 只有 json 变了
	candidate.tables.emplace("Beta", EntryWithDigests("Beta", "j2", "b2"));           // 没变
	candidate.tables.emplace("Delta", EntryWithDigests("Delta", "j4", "b4"));         // 新增;Gamma 被删

	EXPECT_EQ(table_bundle::DiffChangedTables(current, candidate, false),
			  (std::vector<std::string>{"Alpha", "Delta", "Gamma"}));
	EXPECT_EQ(table_bundle::DiffChangedTables(current, candidate, true),
			  (std::vector<std::string>{"Delta", "Gamma"}));
}

// ---------------------------------------------------------------------------
// 管理器
// ---------------------------------------------------------------------------

TEST_F(TableBundleManagerTest, StartupLoadsTheBundleThePointerNames)
{
	const auto manager = StartManager();

	const auto current = CurrentTableSet();
	EXPECT_EQ(current->identity.contentDigest, baseDigest);
	EXPECT_EQ(current->identity.source, "bundle");
	const std::string metrics = MetricsText(*manager);
	EXPECT_THAT(metrics, HasSubstr("source=\"bundle\"} 1\n"));
	// 告警按「轨道 + schema 线」比较各实例手上的包,两边(C++ / Go)都要带 release_track;单测里没设环境变量 = stable
	EXPECT_THAT(metrics, HasSubstr("table_bundle_info{release_track=\"stable\","));
	EXPECT_THAT(metrics, HasSubstr("table_bundle_needs_restart 0\n"));

	// STATUS.json 的字段名是和 sidecar、管理接口之间的约定(Go 的 tablebundle.StatusReport),写错一个对方就读成空
	const std::string status = StatusText();
	EXPECT_THAT(status, HasSubstr("\"service\":\"table-bundle-test\""));
	EXPECT_THAT(status, HasSubstr("\"schema_digest\":\"" + std::string(kTableSchemaDigest) + "\""));
	EXPECT_THAT(status, HasSubstr("\"source\":\"bundle\""));
	EXPECT_THAT(status, HasSubstr("\"loaded_digest\":\"" + baseDigest + "\""));
	EXPECT_THAT(status, HasSubstr("\"loaded_at_ms\":" + std::to_string(fakeClock->wallMs) + ","));
	EXPECT_THAT(status, HasSubstr("\"pending_state\":\"\""));
	EXPECT_THAT(status, HasSubstr("\"pending_digest\":\"\""));
}

TEST_F(TableBundleManagerTest, HotReloadableChangeIsPreparedInSlicesThenCommittedWhole)
{
	const auto manager = StartManager();
	const auto before = CurrentTableSet();
	const std::string next = MakeCandidate(baseDigest, kHotReloadableFile, "commit", std::string(kEmptyTableJson));
	ASSERT_FALSE(HasFatalFailure());
	WritePointer(baseDigest, NextTarget{next, 0, true});

	manager->PollOnce(fakeClock->wallMs);
	EXPECT_EQ(CurrentTableSet()->identity.contentDigest, baseDigest) << "准备是分片做的,这一拍还不该切换";
	EXPECT_GT(LiveMessageLimiterRows(), 0) << "准备期间现役表不能被碰";

	ASSERT_TRUE(RunLoopUntil(loop, [&next]
							 { return CurrentTableSet()->identity.contentDigest == next; }));
	EXPECT_EQ(LiveMessageLimiterRows(), 0) << "提交后各表管理器读到的是新包";
	EXPECT_NE(CurrentTableSet(), before);
	EXPECT_EQ(before->identity.contentDigest, baseDigest) << "还被拿着的旧一套照样能读";

	const std::string status = StatusText();
	EXPECT_THAT(status, HasSubstr("\"loaded_digest\":\"" + next + "\""));
	EXPECT_THAT(status, HasSubstr("\"pending_state\":\"\""));
	const std::string metrics = MetricsText(*manager);
	EXPECT_THAT(metrics, HasSubstr("table_reload_total{result=\"committed\"} 1\n"));
	EXPECT_THAT(metrics, HasSubstr("table_sets_retained 1\n"));
}

TEST_F(TableBundleManagerTest, StructuralTableChangeNeedsRestartAndIsNotRetried)
{
	const auto manager = StartManager();
	const std::string next = MakeCandidate(baseDigest, kStructuralFile, "structural");
	ASSERT_FALSE(HasFatalFailure());
	WritePointer(baseDigest, NextTarget{next, 0, true});

	// 逐表比对在这一拍里同步做完,改到不可热更的表就不进分片准备
	manager->PollOnce(fakeClock->wallMs);
	EXPECT_EQ(CurrentTableSet()->identity.contentDigest, baseDigest);
	const std::string status = StatusText();
	EXPECT_THAT(status, HasSubstr("\"pending_state\":\"needs_restart\""));
	EXPECT_THAT(status, HasSubstr("\"pending_digest\":\"" + next + "\""));
	EXPECT_THAT(status, HasSubstr("World"));
	EXPECT_THAT(MetricsText(*manager), HasSubstr("table_reload_total{result=\"needs_restart\"} 1\n"));
	EXPECT_THAT(MetricsText(*manager), HasSubstr("table_bundle_needs_restart 1\n")) << "24 小时没重启的告警读的是这个状态量";

	manager->PollOnce(fakeClock->wallMs);
	EXPECT_THAT(MetricsText(*manager), HasSubstr("table_reload_total{result=\"needs_restart\"} 1\n")) << "指针没变,不重试";

	// 整条线从 etcd 删掉(POINTER.json 里不再有本线):「要重启」的结论跟着撤,进程继续用手上的表
	WriteAll(root / table_bundle::kPointerFilename, "{\"track\":\"stable\",\"lines\":{}}");
	manager->PollOnce(fakeClock->wallMs);
	const std::string cleared = StatusText();
	EXPECT_THAT(cleared, HasSubstr("\"pending_state\":\"\""));
	EXPECT_THAT(cleared, HasSubstr("\"pending_digest\":\"\""));
	EXPECT_THAT(cleared, HasSubstr("\"loaded_digest\":\"" + baseDigest + "\""));
	EXPECT_THAT(MetricsText(*manager), HasSubstr("table_bundle_needs_restart 0\n"));
}

TEST_F(TableBundleManagerTest, ValidationRunsOnStagedTablesAndRejectionKeepsCurrentOnes)
{
	const auto rowsSeenByValidation = std::make_shared<int>(-1);
	Hooks hooks;
	hooks.validate = [rowsSeenByValidation](std::string &err)
	{
		*rowsSeenByValidation = LiveMessageLimiterRows();
		err = "unit test rejects this bundle";
		return false;
	};
	TableBundleManager::SetProcessHooks(std::move(hooks));

	const auto manager = StartManager();
	const std::string next = MakeCandidate(baseDigest, kHotReloadableFile, "rejected", std::string(kEmptyTableJson));
	ASSERT_FALSE(HasFatalFailure());
	WritePointer(baseDigest, NextTarget{next, 0, true});

	manager->PollOnce(fakeClock->wallMs);
	ASSERT_TRUE(RunLoopUntil(loop, [&manager]
							 { return MetricsText(*manager).find("table_reload_total{result=\"rejected\"} 1\n") != std::string::npos; }));

	EXPECT_EQ(*rowsSeenByValidation, 0) << "校验钩子里读表单例,读到的应该是暂存的新包";
	EXPECT_GT(LiveMessageLimiterRows(), 0) << "校验结束后现役表原样还原";
	EXPECT_EQ(CurrentTableSet()->identity.contentDigest, baseDigest);
	const std::string status = StatusText();
	EXPECT_THAT(status, HasSubstr("\"pending_state\":\"rejected\""));
	EXPECT_THAT(status, HasSubstr("\"pending_digest\":\"" + next + "\""));
	EXPECT_THAT(status, HasSubstr("\"pending_reason\":\"validation failed: unit test rejects this bundle\""));

	manager->PollOnce(fakeClock->wallMs);
	EXPECT_THAT(MetricsText(*manager), HasSubstr("table_reload_total{result=\"rejected\"} 1\n")) << "指针没变,不重试";

	// 运营撤掉这一包:拒绝记录跟着撤。不撤的话管理接口会一直把这个进程算在「拒绝」里,
	// 后面发的包哪怕它还没看到,也会被这条旧记录判成发布失败。
	WritePointer(baseDigest, std::nullopt);
	manager->PollOnce(fakeClock->wallMs);
	const std::string cleared = StatusText();
	EXPECT_THAT(cleared, HasSubstr("\"pending_state\":\"\""));
	EXPECT_THAT(cleared, HasSubstr("\"pending_digest\":\"\""));
	EXPECT_THAT(cleared, HasSubstr("\"pending_reason\":\"\""));

	// 同一包重新发布:重新走一遍准备与校验,不被上一次的拒绝记录挡住
	WritePointer(baseDigest, NextTarget{next, 0, true});
	manager->PollOnce(fakeClock->wallMs);
	ASSERT_TRUE(RunLoopUntil(loop, [&manager]
							 { return MetricsText(*manager).find("table_reload_total{result=\"rejected\"} 2\n") != std::string::npos; }));
}

TEST_F(TableBundleManagerTest, UnparsableTableRejectsTheBundle)
{
	const auto manager = StartManager();
	const std::string next = MakeCandidate(baseDigest, kHotReloadableFile, "unparsable", std::string("{not json"));
	ASSERT_FALSE(HasFatalFailure());
	WritePointer(baseDigest, NextTarget{next, 0, true});

	manager->PollOnce(fakeClock->wallMs);
	ASSERT_TRUE(RunLoopUntil(loop, [&manager]
							 { return MetricsText(*manager).find("table_reload_total{result=\"rejected\"} 1\n") != std::string::npos; }));

	EXPECT_EQ(CurrentTableSet()->identity.contentDigest, baseDigest);
	EXPECT_GT(LiveMessageLimiterRows(), 0);
	EXPECT_THAT(StatusText(), HasSubstr("table parse failed"));
}

TEST_F(TableBundleManagerTest, ScheduledBundleWaitsPreparedUntilActivateAt)
{
	const auto manager = StartManager();
	const std::string next = MakeCandidate(baseDigest, kHotReloadableFile, "scheduled");
	ASSERT_FALSE(HasFatalFailure());
	const uint64_t activateAtMs = fakeClock->wallMs + 60'000;
	WritePointer(baseDigest, NextTarget{next, activateAtMs, false});

	manager->PollOnce(fakeClock->wallMs);
	ASSERT_TRUE(RunLoopUntil(loop, [this]
							 { return StatusText().find("\"pending_state\":\"prepared\"") != std::string::npos; }));
	EXPECT_EQ(CurrentTableSet()->identity.contentDigest, baseDigest) << "没到点,只准备不提交";

	fakeClock->wallMs = activateAtMs - 1;
	manager->PollOnce(fakeClock->wallMs);
	EXPECT_EQ(CurrentTableSet()->identity.contentDigest, baseDigest) << "差 1 毫秒也不提交";

	fakeClock->wallMs = activateAtMs;
	manager->PollOnce(fakeClock->wallMs);
	EXPECT_EQ(CurrentTableSet()->identity.contentDigest, next) << "到点的这一拍同步提交";
	EXPECT_THAT(MetricsText(*manager), HasSubstr("table_reload_total{result=\"committed\"} 1\n"));
}

TEST_F(TableBundleManagerTest, WithdrawnNextDropsThePreparedBundle)
{
	const auto manager = StartManager();
	const std::string next = MakeCandidate(baseDigest, kHotReloadableFile, "withdrawn");
	ASSERT_FALSE(HasFatalFailure());
	const uint64_t activateAtMs = fakeClock->wallMs + 60'000;
	WritePointer(baseDigest, NextTarget{next, activateAtMs, false});

	manager->PollOnce(fakeClock->wallMs);
	ASSERT_TRUE(RunLoopUntil(loop, [this]
							 { return StatusText().find("\"pending_state\":\"prepared\"") != std::string::npos; }));

	// 到点之前撤掉 next(管理接口的 DELETE /admin/tables/next)
	WritePointer(baseDigest, std::nullopt);
	manager->PollOnce(fakeClock->wallMs);
	EXPECT_THAT(StatusText(), HasSubstr("\"pending_state\":\"\""));

	fakeClock->wallMs = activateAtMs;
	manager->PollOnce(fakeClock->wallMs);
	EXPECT_EQ(CurrentTableSet()->identity.contentDigest, baseDigest) << "撤掉之后到点也不切换";
	EXPECT_THAT(MetricsText(*manager), HasSubstr("table_reload_total{result=\"committed\"} 0\n"));
}

TEST_F(TableBundleManagerTest, RetiredSetIsReleasedAfterTheRetentionWindow)
{
	const auto manager = StartManager();
	std::shared_ptr<const TableSet> held = CurrentTableSet();
	const std::weak_ptr<const TableSet> retired = held;
	const std::string next = MakeCandidate(baseDigest, kHotReloadableFile, "retire");
	ASSERT_FALSE(HasFatalFailure());
	WritePointer(baseDigest, NextTarget{next, 0, true});

	manager->PollOnce(fakeClock->wallMs);
	ASSERT_TRUE(RunLoopUntil(loop, [&next]
							 { return CurrentTableSet()->identity.contentDigest == next; }));
	EXPECT_THAT(MetricsText(*manager), HasSubstr("table_sets_retained 1\n"));

	fakeClock->steady += TableBundleManager::kRetainDuration - std::chrono::milliseconds(1);
	manager->PollOnce(fakeClock->wallMs);
	EXPECT_THAT(MetricsText(*manager), HasSubstr("table_sets_retained 1\n")) << "差 1 毫秒不到期";

	// 到期:管理器放手,外面还拿着的那一份照样能读(战斗房间靠这个把开局那一套留到终局)
	fakeClock->steady += std::chrono::milliseconds(1);
	manager->PollOnce(fakeClock->wallMs);
	EXPECT_THAT(MetricsText(*manager), HasSubstr("table_sets_retained 0\n"));
	ASSERT_FALSE(retired.expired());
	EXPECT_EQ(held->identity.contentDigest, baseDigest);

	// 最后一个持有者放手,旧的一套才真正释放
	held.reset();
	EXPECT_TRUE(retired.expired());
}

// ---------------------------------------------------------------------------
// 表达式列的只读求值(多间战斗房间共用同一套不可变表,不能把参数存在快照里)
// ---------------------------------------------------------------------------

TEST(TableExpressionEvaluateTest, ConstEvaluateTakesParamsPerCallAndLeavesTheStatefulPathAlone)
{
	ExcelExpression<double> expression;
	ASSERT_TRUE(expression.Init({"level"}));
	const ExcelExpression<double> &readOnly = expression;

	EXPECT_DOUBLE_EQ(readOnly.Evaluate("level*2+1", {3.0}), 7.0);
	EXPECT_DOUBLE_EQ(readOnly.Evaluate("level*2+1", {10.0}), 21.0);
	EXPECT_DOUBLE_EQ(readOnly.Evaluate("level*2+1", {}), 0.0) << "参数个数对不上返回 0";
	EXPECT_DOUBLE_EQ(readOnly.Evaluate("level*(", {3.0}), 0.0) << "表达式写坏返回 0";

	// 旧的两步路径(SetParam + Value)照常工作,且不受 Evaluate 影响
	expression.SetParam({5.0});
	EXPECT_DOUBLE_EQ(expression.Value("level*2+1"), 11.0);
	EXPECT_DOUBLE_EQ(readOnly.Evaluate("level*2+1", {1.0}), 3.0);
	EXPECT_DOUBLE_EQ(expression.Value("level*2+1"), 11.0);
}

} // namespace
