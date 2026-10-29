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
		};

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
		if (!familyValid_)
		{
			++droppedSamples_;
			return;
		}
		for (const auto &label : labels)
		{
			if (!IsValidLabelName(label.name))
			{
				++droppedSamples_;
				return;
			}
		}
		text_ += family_;
		if (labels.size() > 0)
		{
			text_ += '{';
			bool first = true;
			for (const auto &label : labels)
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
			}
			text_ += '}';
		}
		text_ += ' ';
		text_ += std::to_string(value);
		text_ += '\n';
	}
} // namespace prometheus_text
