#pragma once

#include <cstddef>
#include <cstdint>
#include <string_view>

// 日志行尾的上下文后缀(docs/design/distributed-tracing.md §5.1):
//
//   <muduo 原行,去掉行尾换行> [ctx trace_id=<32 位> span_id=<16 位> player_id=<十进制>]\n
//
// 没有 trace 就省掉 trace_id 与 span_id,没有玩家就省掉 player_id;两样都没有时不生成后缀。
// 后缀放在行尾而不是行首:行首的时间、线程号、级别是 Alloy 解析和人工排查的锚点,一个字节都不动;
// 固定的 "[ctx " 前缀加上只在行尾匹配,不会与正文里随手写的 player_id= 混淆。
//
// 纯函数,不读线程局部状态,便于单测;Node::AsyncOutput 负责取当前上下文并把结果一次写进日志。
// 实现里不得打日志:它运行在日志输出函数里面,打日志会递归回到自己。

namespace tracing {

struct LogContextFields {
    std::string_view traceSpanHex;  // 48 个小写十六进制字符(trace_id 32 位紧接 span_id 16 位);空 = 没有 trace
    uint64_t playerId{0};           // 0 = 没有玩家
};

// 后缀最多占的字节数,含补上的换行:
// " [ctx" + " trace_id=" + 32 + " span_id=" + 16 + " player_id=" + 20(uint64 最多 20 位)+ "]" + "\n"。
// 实现里用 static_assert 按各段字面量重新核算一遍。
inline constexpr size_t kMaxLogContextSuffixBytes = 105;

// 把 line 加上后缀写进 out,返回写入的字节数。返回 0 表示原行不用改写:没有上下文,或者 out 放不下
// (out 至少要有「line 去掉行尾换行后的长度 + kMaxLogContextSuffixBytes」字节)。返回 0 时调用方原样输出 line,
// 宁可这一行没有后缀,也不截断正文。
// line 以 '\n' 结尾时后缀插在换行之前;不以换行结尾时(muduo 单行超长会整段丢掉行尾)追加后缀并补一个换行。
// traceSpanHex 长度不是 48 时当作没有 trace。
size_t AppendLogContextSuffix(std::string_view line, const LogContextFields& fields, char* out, size_t outCapacity);

} // namespace tracing
