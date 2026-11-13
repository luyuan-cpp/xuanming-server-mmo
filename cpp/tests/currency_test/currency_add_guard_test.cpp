// CurrencySystem::AddCurrency 的两道守卫:
//   1) 入账后余额溢出 uint64(数据覆盖审计第一轮 §4 第 5 条):零副作用拒绝,回 kInvalidParameter。
//      溢出按补缴扣还后的净入账额判(与战斗结算整批预检 CanCreditWholeBattleCurrency 同口径),判定排在扣还之前。
//   2) 补缴欠款带更新版本才有的字段(第二轮 r2-ps#6):本版本不扣还也不删除,按冻结处理、全额入账,
//      打一条 metric=persist_version_skew where=currency_clawback 的 WARN
//      (docs/design/mixed-version-persist-field-protection.md §4.4)。
//
// 确定性:
//   * 「写没写流水」看 txlog 号段被要过几次号:每条流水恰好要一次(currency_test.cpp 的 main 已投好号段)。
//   * 「记没记异常检测」看玩家身上有没有长出异常窗口组件(PlayerAnomalyWindowComp),不读墙钟。
//   * 欠款都不设到期时间(0 = 不过期),补缴是否生效与墙钟无关。
//   * 余额直接写进 CurrencyComp,模拟「加载进来就是这么多」:用 AddCurrency 铺余额会先写流水、记异常窗口,
//     污染零副作用断言。
//   * 每个用例在 TearDown 里销毁自己建的实体;抓日志只在作用域内生效,出作用域还原 muduo 的输出与级别。

#include <gtest/gtest.h>

#include <cstddef>
#include <cstdint>
#include <cstdio>
#include <limits>
#include <string>
#include <vector>

#include <google/protobuf/unknown_field_set.h>

#include "muduo/base/Logging.h"
#include "engine/core/metrics/metrics_registry.h"
#include "modules/currency/comp/player_currency_comp.h"
#include "modules/currency/constants/currency.h"
#include "modules/currency/system/currency_clawback_stats.h"
#include "modules/currency/system/currency_system.h"
#include "player/system/currency_metrics.h"
#include "modules/id_segment/guid_segment_registry.h"
#include "modules/transaction_log/anomaly_detector.h"
#include "proto/common/component/currency_comp.pb.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include <thread_context/ecs_context.h>

namespace
{

constexpr uint64_t kMaxBalance = std::numeric_limits<uint64_t>::max();
// CurrencyDebtEntry 上本版本不认识、台账里也没退役的字段号:模拟更新版本给欠款加的字段。
constexpr int kFutureDebtField = 9999;

uint32_t Slot(CurrencyType type)
{
	return static_cast<uint32_t>(type);
}

// 落了几条流水 = TransactionLogSystem 向 txlog 号段要过几次 tx_id(每条流水恰好要一次)。
// issued 与 unavailable 相加:号段意外没号时那条流水会被 fail-closed 丢掉,「要过一次」照样数得到
// (同 bag_test/bag_remove_exact_test.cpp)。
uint64_t LogAttempts()
{
	const auto stats = tlsGuidSegmentRegistry.Get(GuidKind::kTxLog).GetStats();
	return stats.issued + stats.unavailable;
}

// ── 日志抓取:这一行 WARN 就是版本错位的计数口径(告警与值班查询按 metric= / where= 数行)──

std::vector<std::string> g_guardLogLines;

void CaptureGuardLogLine(const char *msg, int len)
{
	g_guardLogLines.emplace_back(msg, static_cast<std::size_t>(len));
	std::fwrite(msg, 1, static_cast<std::size_t>(len), stdout);
}

void WriteGuardLogToStdout(const char *msg, int len)
{
	std::fwrite(msg, 1, static_cast<std::size_t>(len), stdout);
}

// 作用域内把 muduo 的输出接进 g_guardLogLines(照抄到 stdout),级别至少放到 WARN;析构时改回 stdout、还原级别。
class ScopedGuardLogCapture
{
public:
	ScopedGuardLogCapture() : previousLevel_(muduo::Logger::logLevel())
	{
		g_guardLogLines.clear();
		if (previousLevel_ > muduo::Logger::WARN)
		{
			muduo::Logger::setLogLevel(muduo::Logger::WARN);
		}
		muduo::Logger::setOutput(&CaptureGuardLogLine);
	}
	~ScopedGuardLogCapture()
	{
		muduo::Logger::setOutput(&WriteGuardLogToStdout);
		muduo::Logger::setLogLevel(previousLevel_);
	}
	ScopedGuardLogCapture(const ScopedGuardLogCapture &) = delete;
	ScopedGuardLogCapture &operator=(const ScopedGuardLogCapture &) = delete;

private:
	muduo::Logger::LogLevel previousLevel_;
};

// 「补缴因版本错位被跳过」的 WARN 行数。级别字段是定宽的 "WARN  "(muduo Logging.cc 的 LogLevelName)。
std::size_t ClawbackVersionSkewWarnLines()
{
	std::size_t count = 0;
	for (const auto &line : g_guardLogLines)
	{
		if (line.find("WARN  ") != std::string::npos &&
			line.find("metric=persist_version_skew") != std::string::npos &&
			line.find("where=currency_clawback") != std::string::npos)
		{
			++count;
		}
	}
	return count;
}

class CurrencyAddGuardTest : public testing::Test
{
protected:
	void SetUp() override
	{
		// 异常检测按缺省阈值开着:别的用例可能改过某个币种的阈值,阈值全 0 时根本不建窗口,
		// 「没长出窗口」就成了空断言。
		for (uint32_t type = 0; type < Slot(kCurrencyMax); ++type)
		{
			AnomalyDetector::SetCurrencyThreshold(static_cast<CurrencyType>(type), AnomalyThreshold{});
		}

		player_ = tlsEcs.actorRegistry.create();
		auto &currency = tlsEcs.actorRegistry.emplace<CurrencyComp>(player_);
		for (uint32_t type = 0; type < Slot(kCurrencyMax); ++type)
		{
			currency.add_values(0);
		}
		tlsEcs.actorRegistry.emplace<PlayerCurrencyComp>(player_);
	}

	void TearDown() override
	{
		if (tlsEcs.actorRegistry.valid(player_))
		{
			tlsEcs.actorRegistry.destroy(player_); // 异常窗口组件随实体一起销毁
		}
	}

	void SetBalance(CurrencyType type, uint64_t value)
	{
		tlsEcs.actorRegistry.get<CurrencyComp>(player_).set_values(static_cast<int>(Slot(type)), value);
	}

	PlayerCurrencyComp &Debts()
	{
		return tlsEcs.actorRegistry.get<PlayerCurrencyComp>(player_);
	}

	entt::entity player_{entt::null};
};

// 余额离上限只差 5、金币欠款还剩 3,入账 10:扣还 3 之后还要入 7,放不下。
// 修复前先扣还 3(欠款还清删除、写一条补缴流水),再把 7 加上去回绕成 1,还写入账流水、记异常窗口、回 kSuccess。
TEST_F(CurrencyAddGuardTest, BalanceOverflowIsRejectedBeforeAnySideEffect)
{
	SetBalance(kCurrencyGold, kMaxBalance - 5);
	Debts().debts[Slot(kCurrencyGold)].owed = 3;
	SetBalance(kCurrencyDiamond, kMaxBalance);
	const uint64_t logsBefore = LogAttempts();

	EXPECT_EQ(kInvalidParameter, CurrencySystem::AddCurrency(player_, kCurrencyGold, 10));
	EXPECT_EQ(kInvalidParameter, CurrencySystem::AddCurrency(player_, kCurrencyDiamond, 1)) << "没有欠款时同样按余额判";
	EXPECT_EQ(kInvalidParameter,
			  CurrencySystem::AddCurrency(player_, kCurrencyDiamond, std::numeric_limits<int64_t>::max()));

	EXPECT_EQ(kMaxBalance - 5, CurrencySystem::GetBalance(player_, kCurrencyGold)) << "余额回绕了";
	EXPECT_EQ(kMaxBalance, CurrencySystem::GetBalance(player_, kCurrencyDiamond)) << "余额回绕了";
	ASSERT_EQ(1u, Debts().debts.count(Slot(kCurrencyGold))) << "拒绝之前就把欠款扣还删掉了";
	EXPECT_EQ(3u, Debts().debts.at(Slot(kCurrencyGold)).owed);
	EXPECT_EQ(0u, Debts().debts.at(Slot(kCurrencyGold)).paid) << "拒绝之前就扣还了一部分";
	EXPECT_FALSE(Debts().dirty) << "拒绝路径不得标脏";
	EXPECT_EQ(logsBefore, LogAttempts()) << "拒绝路径不得写补缴流水或入账流水";
	EXPECT_FALSE(tlsEcs.actorRegistry.any_of<PlayerAnomalyWindowComp>(player_)) << "拒绝路径不得记异常检测";

	// 对照:放得下的一笔照常入账、写流水、记异常窗口 —— 上面那几条「没变化」不是因为观测手段失灵。
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, kCurrencyBindDiamond, 1));
	EXPECT_EQ(1u, CurrencySystem::GetBalance(player_, kCurrencyBindDiamond));
	EXPECT_TRUE(Debts().dirty);
	EXPECT_EQ(logsBefore + 1, LogAttempts());
	EXPECT_TRUE(tlsEcs.actorRegistry.any_of<PlayerAnomalyWindowComp>(player_));
}

// 溢出按扣还后的净入账额判,与战斗结算整批预检 CanCreditWholeBattleCurrency 同口径:预检放行的那一笔,
// 这里不能再拒(多币种结算会只入一半)。所以 8 = 扣还 3 + 入账 5,恰好装满,照常成功;之后再多 1 才拒。
TEST_F(CurrencyAddGuardTest, OverflowIsJudgedOnTheCreditLeftAfterClawback)
{
	SetBalance(kCurrencyGold, kMaxBalance - 5);
	Debts().debts[Slot(kCurrencyGold)].owed = 3;
	const uint64_t logsBefore = LogAttempts();

	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, kCurrencyGold, 8)) << "边界含:恰好装满不算溢出";
	EXPECT_EQ(kMaxBalance, CurrencySystem::GetBalance(player_, kCurrencyGold));
	EXPECT_EQ(0u, Debts().debts.count(Slot(kCurrencyGold))) << "欠款还清照常删除";
	EXPECT_EQ(logsBefore + 2, LogAttempts()) << "一条补缴流水 + 一条入账流水";

	EXPECT_EQ(kInvalidParameter, CurrencySystem::AddCurrency(player_, kCurrencyGold, 1));
	EXPECT_EQ(kMaxBalance, CurrencySystem::GetBalance(player_, kCurrencyGold)) << "余额回绕了";
	EXPECT_EQ(logsBefore + 2, LogAttempts()) << "拒绝路径不得写流水";
}

// 更新版本写下的欠款(带本版本不认识的字段):收入足以还清剩下的 40。修复前按旧口径扣还 40、扣满整条删除,
// 新字段随欠款一起消失,只入账 60。修复后本版本不扣还也不删除(按冻结处理),全额入账,欠款与未知字段原样保留,
// 打一条版本错位 WARN;普通欠款照常扣还。
TEST_F(CurrencyAddGuardTest, DebtWithNewerVersionFieldsIsNeitherRepaidNorDeletedByGains)
{
	CurrencyComp persisted;
	auto *entry = persisted.add_debts();
	entry->set_currency_type(Slot(kCurrencyGold));
	entry->set_owed(50);
	entry->set_paid(10);
	entry->mutable_unknown_fields()->AddVarint(kFutureDebtField, 7);
	Debts().LoadFromProto(persisted);
	const std::string carriedBefore = Debts().debts.at(Slot(kCurrencyGold)).unknownFields;
	ASSERT_FALSE(carriedBefore.empty());
	ASSERT_TRUE(CurrencySystem::DebtCarriesUnknownFields(player_, kCurrencyGold));
	// 对照:另一币种的普通欠款(不带未知字段)。
	ASSERT_EQ(kSuccess, CurrencySystem::AttachDebt(player_, kCurrencyDiamond, 40));
	const uint64_t logsBefore = LogAttempts();

	ScopedGuardLogCapture capture;
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, kCurrencyGold, 100));
	EXPECT_EQ(100u, CurrencySystem::GetBalance(player_, kCurrencyGold)) << "按冻结处理:全额入账";
	ASSERT_EQ(1u, Debts().debts.count(Slot(kCurrencyGold))) << "带新字段的欠款被扣满删除了";
	{
		const auto &kept = Debts().debts.at(Slot(kCurrencyGold));
		EXPECT_EQ(50u, kept.owed);
		EXPECT_EQ(10u, kept.paid) << "本版本不得按旧口径扣还";
		EXPECT_EQ(carriedBefore, kept.unknownFields) << "未知字段必须原样随欠款保留";
	}
	EXPECT_TRUE(CurrencySystem::DebtCarriesUnknownFields(player_, kCurrencyGold));
	EXPECT_EQ(logsBefore + 1, LogAttempts()) << "只有入账流水,没有补缴流水";
	EXPECT_EQ(1u, ClawbackVersionSkewWarnLines()) << "跳过补缴要打一条版本错位 WARN(计数口径)";

	// 普通欠款照常扣还、扣满即删,不打版本错位 WARN。
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, kCurrencyDiamond, 100));
	EXPECT_EQ(60u, CurrencySystem::GetBalance(player_, kCurrencyDiamond));
	EXPECT_EQ(0u, Debts().debts.count(Slot(kCurrencyDiamond)));
	EXPECT_EQ(1u, ClawbackVersionSkewWarnLines());

	// 存盘照常把未知字段写回这笔欠款。
	CurrencyComp saved;
	ASSERT_TRUE(Debts().SaveToProto(saved));
	ASSERT_EQ(1, saved.debts_size());
	EXPECT_EQ(Slot(kCurrencyGold), saved.debts(0).currency_type());
	EXPECT_EQ(50u, saved.debts(0).owed());
	EXPECT_EQ(10u, saved.debts(0).paid());
	ASSERT_EQ(1, saved.debts(0).unknown_fields().field_count());
	EXPECT_EQ(kFutureDebtField, saved.debts(0).unknown_fields().field(0).number());
	EXPECT_EQ(7u, saved.debts(0).unknown_fields().field(0).varint());
}

// 2026-11-12:跳过补缴除了那一行 WARN,还要计一次进程计数,由 scene 的 Prometheus 出口导出(告警读它)。
// 计数是进程累计的,同一进程里别的用例也可能走到,所以一律比前后差值。只在本版本本来要扣还时计:
// 普通欠款照常扣还不计;冻结的(本来就不扣)、溢出被拒的(什么都没提交)也不计。
TEST_F(CurrencyAddGuardTest, ClawbackSkippedForVersionSkewIsCountedAndExported)
{
	CurrencyComp persisted;
	auto *skewed = persisted.add_debts();
	skewed->set_currency_type(Slot(kCurrencyGold));
	skewed->set_owed(50);
	skewed->mutable_unknown_fields()->AddVarint(kFutureDebtField, 7);
	auto *frozen = persisted.add_debts();
	frozen->set_currency_type(Slot(kCurrencyBindDiamond));
	frozen->set_owed(50);
	frozen->set_frozen(true);
	frozen->mutable_unknown_fields()->AddVarint(kFutureDebtField, 7);
	Debts().LoadFromProto(persisted);
	ASSERT_TRUE(CurrencySystem::DebtCarriesUnknownFields(player_, kCurrencyGold));
	ASSERT_EQ(kSuccess, CurrencySystem::AttachDebt(player_, kCurrencyDiamond, 40)); // 普通欠款,对照

	const uint64_t before = currency_clawback_stats::ReadVersionSkewSkipped();
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, kCurrencyGold, 10));
	EXPECT_EQ(before + 1, currency_clawback_stats::ReadVersionSkewSkipped()) << "跳过补缴计一次";
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, kCurrencyDiamond, 10));
	ASSERT_EQ(kSuccess, CurrencySystem::AddCurrency(player_, kCurrencyBindDiamond, 10));
	EXPECT_EQ(before + 1, currency_clawback_stats::ReadVersionSkewSkipped()) << "普通欠款照常扣还、冻结的本来就不扣,都不计";
	SetBalance(kCurrencyGold, kMaxBalance);
	EXPECT_EQ(kInvalidParameter, CurrencySystem::AddCurrency(player_, kCurrencyGold, 10));
	EXPECT_EQ(before + 1, currency_clawback_stats::ReadVersionSkewSkipped()) << "溢出被拒,什么都没提交,不计";

	// 导出:族名、类型与当前值(启动即以 0 导出,告警的 increase 抓得到第一次)。重复登记被注册表按名字拒绝,只打一行 ERROR。
	RegisterCurrencyMetrics();
	const std::string text = GlobalMetricsRegistry().Render();
	EXPECT_NE(std::string::npos, text.find("# TYPE scene_currency_clawback_version_skew_total counter\n")) << text;
	const std::string sample =
		"\nscene_currency_clawback_version_skew_total " + std::to_string(currency_clawback_stats::ReadVersionSkewSkipped()) + "\n";
	EXPECT_NE(std::string::npos, text.find(sample)) << text;
}

} // namespace
