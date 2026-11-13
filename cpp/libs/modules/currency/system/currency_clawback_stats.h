#pragma once

#include <atomic>
#include <cstdint>

// 补缴扣还(CurrencySystem::AddCurrency)的进程级计数,与 owner_epoch_stats 同一套写法(relaxed atomic,进程启动以来的累计值)。
//
//   version_skew_skipped  欠款带着更新版本才有的字段,本版本按冻结处理、不扣还也不删除、收入全额入账的次数
//                         (docs/design/mixed-version-persist-field-protection.md §4.4「欠款自动扣还」)。
//                         只在本版本本来要扣还时(欠款有效、还有余额)计一次,与逐次的
//                         `metric=persist_version_skew where=currency_clawback` WARN 同一个时刻、同一个口径;玩家号只在那一行。
//                         正常应恒 0:更新版本给欠款加的字段要等认识它的版本全量之后才写(两版发布,混版本设计 §13 第 7 条),
//                         旧版节点碰不到;非 0 = 有 scene 停在旧版本、被回滚,或发布次序被打破。这段时间这名玩家这一币种的补缴暂停,
//                         欠款不丢,落到新版本节点后按新口径收。
//
// 由 scene 的 Prometheus 出口导出为 scene_currency_clawback_version_skew_total(cpp/libs/services/scene/player/system/currency_metrics.cpp),
// 告警在 deploy/k8s/persist-version-skew-alerts.yaml。线程模型:只在 scene loop 线程写,出口在同一线程读;用原子是为了与其它计数同一套写法、
// 读写都不加锁。
namespace currency_clawback_stats
{
	inline std::atomic<uint64_t> &VersionSkewSkipped()
	{
		static std::atomic<uint64_t> g_counter{0};
		return g_counter;
	}

	inline void IncVersionSkewSkipped() { VersionSkewSkipped().fetch_add(1, std::memory_order_relaxed); }

	inline uint64_t ReadVersionSkewSkipped() { return VersionSkewSkipped().load(std::memory_order_relaxed); }
} // namespace currency_clawback_stats
