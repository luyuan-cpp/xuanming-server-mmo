#pragma once

#include <charconv>
#include <cstdint>
#include <optional>
#include <string_view>
#include <system_error>

// C++ 节点日志级别(部署键 LogLevel → BaseDeployConfig.log_level)的启动判定,
// docs/design/gate-connection-admission-control.md P0-3。
//
// 数值直接就是 muduo Logger::LogLevel 的枚举值,没有映射表:0=TRACE 1=DEBUG 2=INFO 3=WARN 4=ERROR 5=FATAL。
// LOG_TRACE / LOG_DEBUG / LOG_INFO 按级别过滤,LOG_WARN 及以上任何取值都会打。
// Windows(cpp/libs/engine/muduo_windows)与 Linux(third_party/muduo-linux)两份 muduo 的枚举顺序相同;
// Node::InitLogSystem 用 static_assert 把 kMaxValue 钉在 muduo::Logger::FATAL 上。
//
// 为什么读配置时就拒启,不回落默认值:
//   - 缺键:proto3 的 uint32 没有 presence,以前缺键 = 0 = TRACE(最吵),不报任何错;
//   - 越界(> 5):以前原样 static_cast 成枚举,不是任何枚举项;
//   - 非整数(如 INFO):以前 yaml-cpp 的 as<uint32_t>() 直接抛异常,启动栈上没人接,看不到是哪个键错了。
// 回落 INFO 会把「配置生成器漏键」这类错误再藏一次(K8s 的 node ConfigMap 写死 1 = DEBUG 跑了很久没人发现,就是同一类静默)。
//
// 本头只做纯判定:不读文件、不打日志、不依赖 muduo,Windows 上直接单测
// (cpp/tests/client_endpoint_test/node_log_level_test.cpp)。拒启在 config.cpp::readBaseDeployConfig
// (只有那里分得清「缺键」与「写了 0」)。
// 这里只核结构,不核下界:0 / 1 在本机临时看 TRACE / DEBUG 是合法用法;生产下界 2 是发布策略,
// 由 tools/scripts/release_preflight.ps1 的 debug.cpp.loglevel 判。
namespace node_log_level
{

// 最后一个可用级别 muduo::Logger::FATAL。
inline constexpr uint32_t kMaxValue = 5;

// 已存进 BaseDeployConfig 的数值是否是枚举项。
[[nodiscard]] constexpr bool IsValidValue(uint32_t value)
{
    return value <= kMaxValue;
}

// 解析 yaml 标量原文:只认十进制整数 0..kMaxValue(允许前导 0)。
// 空串(调用方把缺键、YAML 空值、序列 / 映射都折成空串)、符号、空白、小数、十六进制、越界一律 nullopt,
// 调用方必须拒绝启动,不能自行回落。
[[nodiscard]] inline std::optional<uint32_t> Parse(std::string_view raw)
{
    if (raw.empty())
    {
        return std::nullopt;
    }
    uint32_t value = 0;
    const char *const last = raw.data() + raw.size();
    const auto [end, ec] = std::from_chars(raw.data(), last, value);
    if (ec != std::errc() || end != last || !IsValidValue(value))
    {
        return std::nullopt;
    }
    return value;
}

} // namespace node_log_level
