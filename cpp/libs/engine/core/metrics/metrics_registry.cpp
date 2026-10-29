#include "metrics/metrics_registry.h"

bool MetricsRegistry::Register(std::string name, Collector collector)
{
	if (!collector)
	{
		return false;
	}
	std::lock_guard<std::mutex> lock(mutex_);
	for (const auto &[existing, _] : collectors_)
	{
		if (existing == name)
		{
			return false;
		}
	}
	collectors_.emplace_back(std::move(name), std::move(collector));
	return true;
}

std::string MetricsRegistry::Render() const
{
	std::vector<std::pair<std::string, Collector>> snapshot;
	{
		std::lock_guard<std::mutex> lock(mutex_);
		snapshot = collectors_;
	}
	prometheus_text::Writer writer;
	for (const auto &[_, collector] : snapshot)
	{
		collector(writer);
	}
	return writer.Text();
}

MetricsRegistry &GlobalMetricsRegistry()
{
	static MetricsRegistry registry;
	return registry;
}
