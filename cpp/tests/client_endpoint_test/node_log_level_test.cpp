#include <gtest/gtest.h>

#include <cstdint>
#include <optional>
#include <string>
#include <string_view>

#include "engine/config/node_log_level.h"

// node_log_level 的单元测试(gate 准入设计 P0-3 后半)。
//
// 被测的是 readBaseDeployConfig 对部署键 LogLevel 的纯判定,以及 Node::InitLogSystem 转换前的数值核对。
// 拒启本身(stderr + LOG_FATAL abort)不在单测里跑,由交付说明里的负例冒烟验证。
// 和 open_file_limit 同在一个工程:两者都是「判错即 Node 拒启」的启动纯判定。

using node_log_level::IsValidValue;
using node_log_level::kMaxValue;
using node_log_level::Parse;

TEST(NodeLogLevelParse, AcceptsEveryMuduoLevel)
{
    EXPECT_EQ(5u, kMaxValue); // muduo::Logger::FATAL
    for (uint32_t level = 0; level <= kMaxValue; ++level)
    {
        EXPECT_EQ(std::optional<uint32_t>(level), Parse(std::to_string(level))) << level;
    }
}

TEST(NodeLogLevelParse, LeadingZerosAreDecimal)
{
    EXPECT_EQ(std::optional<uint32_t>(2u), Parse("02"));
    EXPECT_EQ(std::optional<uint32_t>(0u), Parse("000"));
}

// config.cpp 把缺键、YAML 空值、序列 / 映射都折成空串:以前缺键 = 0 = TRACE,现在必须拒启。
TEST(NodeLogLevelParse, MissingOrEmptyIsRejected)
{
    EXPECT_EQ(std::nullopt, Parse(""));
    EXPECT_EQ(std::nullopt, Parse(std::string_view{}));
}

TEST(NodeLogLevelParse, OutOfRangeIsRejected)
{
    for (const std::string_view bad : {"6", "7", "10", "4294967295", "4294967296", "99999999999999999999"})
    {
        EXPECT_EQ(std::nullopt, Parse(bad)) << "'" << bad << "'";
    }
}

TEST(NodeLogLevelParse, NonIntegerIsRejected)
{
    for (const std::string_view bad : {"-1", "+2", " 2", "2 ", "2.0", "2.5", "0x2", "INFO", "info", "DEBUG", "true", "2#"})
    {
        EXPECT_EQ(std::nullopt, Parse(bad)) << "'" << bad << "'";
    }
}

TEST(NodeLogLevelIsValidValue, BoundaryAtFatal)
{
    EXPECT_TRUE(IsValidValue(0));
    EXPECT_TRUE(IsValidValue(kMaxValue));
    EXPECT_FALSE(IsValidValue(kMaxValue + 1));
    EXPECT_FALSE(IsValidValue(UINT32_MAX));
}
