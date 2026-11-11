#include "metrics/prometheus_text.h"

#include <array>

namespace prometheus_text
{
	namespace
	{
		bool IsAsciiLetter(char c) { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'); }
		bool IsAsciiDigit(char c) { return c >= '0' && c <= '9'; }

		// 类型名与 MetricType 一一对应;数组长度由 kCount 钉住。
		constexpr std::array<std::string_view, static_cast<size_t>(MetricType::kCount)> kTypeNames{
			"counter",
			"gauge",
			"histogram",
		};

		bool LabelsValid(std::initializer_list<Label> labels)
		{
			for (const auto &label : labels)
			{
				if (!IsValidLabelName(label.name))
				{
					return false;
				}
			}
			return true;
		}

		// HELP 文本只转义反斜杠与换行(格式规定)。
		void AppendEscapedHelp(std::string &out, std::string_view help)
		{
			for (const char c : help)
			{
				if (c == '\\')
				{
					out += "\\\\";
				}
				else if (c == '\n')
				{
					out += "\\n";
				}
				else
				{
					out += c;
				}
			}
		}

		// 标签值转义反斜杠、双引号与换行。
		void AppendEscapedLabelValue(std::string &out, std::string_view value)
		{
			for (const char c : value)
			{
				if (c == '\\')
				{
					out += "\\\\";
				}
				else if (c == '"')
				{
					out += "\\\"";
				}
				else if (c == '\n')
				{
					out += "\\n";
				}
				else
				{
					out += c;
				}
			}
		}
	} // namespace

	bool IsValidMetricName(std::string_view name)
	{
		if (name.empty())
		{
			return false;
		}
		for (size_t i = 0; i < name.size(); ++i)
		{
			const char c = name[i];
			const bool ok = IsAsciiLetter(c) || c == '_' || c == ':' || (i > 0 && IsAsciiDigit(c));
			if (!ok)
			{
				return false;
			}
		}
		return true;
	}

	bool IsValidLabelName(std::string_view name)
	{
		if (name.empty() || name.substr(0, 2) == "__")
		{
			return false;
		}
		for (size_t i = 0; i < name.size(); ++i)
		{
			const char c = name[i];
			const bool ok = IsAsciiLetter(c) || c == '_' || (i > 0 && IsAsciiDigit(c));
			if (!ok)
			{
				return false;
			}
		}
		return true;
	}

	bool Writer::BeginFamily(std::string_view name, std::string_view help, MetricType type)
	{
		familyValid_ = IsValidMetricName(name) && type < MetricType::kCount;
		familyType_ = type;
		family_.assign(name);
		if (!familyValid_)
		{
			return false;
		}
		text_ += "# HELP ";
		text_ += name;
		text_ += ' ';
		AppendEscapedHelp(text_, help);
		text_ += "\n# TYPE ";
		text_ += name;
		text_ += ' ';
		text_ += kTypeNames[static_cast<size_t>(type)];
		text_ += '\n';
		return true;
	}

	void Writer::Sample(std::initializer_list<Label> labels, uint64_t value)
	{
		if (!familyValid_ || familyType_ == MetricType::kHistogram || !LabelsValid(labels))
		{
			++droppedSamples_;
			return;
		}
		AppendSample({}, labels, nullptr, value);
	}

	void Writer::Histogram(std::initializer_list<Label> labels, std::span<const uint64_t> upperBounds,
						   std::span<const uint64_t> bucketCounts, uint64_t sum)
	{
		bool valid = familyValid_ && familyType_ == MetricType::kHistogram && LabelsValid(labels) &&
					 bucketCounts.size() == upperBounds.size() + 1;
		for (const auto &label : labels)
		{
			valid = valid && label.name != "le";
		}
		for (size_t i = 1; valid && i < upperBounds.size(); ++i)
		{
			valid = upperBounds[i - 1] < upperBounds[i];
		}
		if (!valid)
		{
			++droppedSamples_;
			return;
		}

		uint64_t cumulative = 0;
		for (size_t i = 0; i < upperBounds.size(); ++i)
		{
			cumulative += bucketCounts[i];
			const std::string bound = std::to_string(upperBounds[i]);
			const Label le{"le", bound};
			AppendSample("_bucket", labels, &le, cumulative);
		}
		cumulative += bucketCounts.back();
		const Label infinity{"le", "+Inf"};
		AppendSample("_bucket", labels, &infinity, cumulative);
		AppendSample("_sum", labels, nullptr, sum);
		AppendSample("_count", labels, nullptr, cumulative);
	}

	void Writer::AppendSample(std::string_view suffix, std::initializer_list<Label> labels, const Label *extra,
							  uint64_t value)
	{
		text_ += family_;
		text_ += suffix;
		if (labels.size() > 0 || extra != nullptr)
		{
			text_ += '{';
			bool first = true;
			const auto appendLabel = [this, &first](const Label &label)
			{
				if (!first)
				{
					text_ += ',';
				}
				first = false;
				text_ += label.name;
				text_ += "=\"";
				AppendEscapedLabelValue(text_, label.value);
				text_ += '"';
			};
			for (const auto &label : labels)
			{
				appendLabel(label);
			}
			if (extra != nullptr)
			{
				appendLabel(*extra);
			}
			text_ += '}';
		}
		text_ += ' ';
		text_ += std::to_string(value);
		text_ += '\n';
	}
} // namespace prometheus_text
