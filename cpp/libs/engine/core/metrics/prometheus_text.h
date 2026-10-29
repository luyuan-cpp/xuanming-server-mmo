#pragma once

#include <cstdint>
#include <initializer_list>
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
//   - 样本必须写在 BeginFamily 之后,名字即族名(counter 的 _total 后缀由调用方写进族名)。
// 标签只放低基数值(node_type / zone / 事件名),玩家号等高基数值不许做标签(AGENTS.md §9)。
namespace prometheus_text
{
	enum class MetricType : uint8_t
	{
		kCounter,
		kGauge,
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
		// 写一条样本到当前族。
		void Sample(std::initializer_list<Label> labels, uint64_t value);

		const std::string &Text() const { return text_; }
		uint32_t DroppedSamples() const { return droppedSamples_; }

	private:
		std::string text_;
		std::string family_;
		bool familyValid_{false};
		uint32_t droppedSamples_{0};
	};
} // namespace prometheus_text
