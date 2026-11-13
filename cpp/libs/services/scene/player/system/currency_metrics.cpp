#include "currency_metrics.h"

#include "muduo/base/Logging.h"

#include "engine/core/metrics/metrics_registry.h"
#include "modules/currency/system/currency_clawback_stats.h"

namespace
{
	// 补缴因版本错位被跳过(currency_clawback_stats)。健康时恒 0;非 0 说明有 scene 停在旧版本、被回滚,或发布次序被打破,
	// 这段时间对应玩家这一币种的补缴暂停(欠款不丢)。
	void CollectCurrencyClawback(prometheus_text::Writer &writer)
	{
		writer.BeginFamily("scene_currency_clawback_version_skew_total",
						   "Deferred clawbacks skipped on this scene node since process start because the debt carries fields "
						   "unknown to this version (same event as the metric=persist_version_skew where=currency_clawback log line); "
						   "should stay 0.",
						   prometheus_text::MetricType::kCounter);
		writer.Sample({}, currency_clawback_stats::ReadVersionSkewSkipped());
	}
} // namespace

void RegisterCurrencyMetrics()
{
	if (!GlobalMetricsRegistry().Register("scene_currency_clawback", &CollectCurrencyClawback))
	{
		LOG_ERROR << "[Metrics] collector scene_currency_clawback is already registered";
	}
}
