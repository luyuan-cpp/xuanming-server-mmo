#pragma once

#include <cstdint>
#include <initializer_list>
#include <span>
#include <string>
#include <string_view>

// Prometheus 文本暴露格式 0.0.4 的写出器(https://prometheus.io/docs/instrumenting/exposition_formats/)。
//
// 只管格式,不持有任何指标:一次抓取里调用方按族写出,写完取 Text()。纯逻辑、零依赖(不碰 muduo / ECS),
// 单测在 cpp/tests/rpc_controller_test/prometheus_text_test.cpp。
//
// 约束(违反时整条样本丢弃并计入 DroppedSamples,不让一处坏名字污染整页):
//   - 指标族名 [a-zA-Z_:][a-zA-Z0-9_:]*;标签名 [a-zA-Z_][a-zA-Z0-9_]*,且不以 "__" 开头(Prometheus 保留);
//   - 标签值任意字节,按格式转义 \ " 与换行;
//   - 样本必须写在 BeginFamily 之后,名字即族名(counter 的 _total 后缀由调用方写进族名);
//     histogram 族只能用 Histogram 写,样本名是族名加 _bucket / _sum / _count。
// 标签只放低基数值(node_type / zone / 事件名),玩家号等高基数值不许做标签(AGENTS.md §9)。
namespace prometheus_text
{
	enum class MetricType : uint8_t
	{
		kCounter,
		kGauge,
		kHistogram,
		kCount,
	};

	struct Label
	{
		std::string_view name;
		std::string_view value;
	};

	bool IsValidMetricName(std::string_view name);
	bool IsValidLabelName(std::string_view name);

	class Writer
	{
	public:
		// 开始一个指标族:写 # HELP 与 # TYPE。名字非法或类型越界时返回 false,该族之后的样本一律丢弃。
		bool BeginFamily(std::string_view name, std::string_view help, MetricType type);
		// 写一条样本到当前族(counter / gauge)。当前族是 histogram 时丢弃:直方图的样本只能用 Histogram 写。
		void Sample(std::initializer_list<Label> labels, uint64_t value);
		// 写一组直方图样本到当前族(BeginFamily 的类型须是 kHistogram):每个上界一条累计计数 <族名>_bucket{le="上界"},
		// 再加 le="+Inf"、<族名>_sum 与 <族名>_count。upperBounds 严格升序;bucketCounts 是各桶自己的计数(不累计),
		// 比 upperBounds 多一个,最后一个是大于最后一个上界的部分。labels 是这一组的公共标签,不能含 le。
		// 任一条件不满足时整组不写,计一次 DroppedSamples。
		void Histogram(std::initializer_list<Label> labels, std::span<const uint64_t> upperBounds,
					   std::span<const uint64_t> bucketCounts, uint64_t sum);

		const std::string &Text() const { return text_; }
		uint32_t DroppedSamples() const { return droppedSamples_; }

	private:
		// 写一行 <族名><suffix>{labels[,extra]} value。调用方已校验族与标签。
		void AppendSample(std::string_view suffix, std::initializer_list<Label> labels, const Label *extra,
						  uint64_t value);

		std::string text_;
		std::string family_;
		MetricType familyType_{MetricType::kCounter};
		bool familyValid_{false};
		uint32_t droppedSamples_{0};
	};
} // namespace prometheus_text
