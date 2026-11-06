#include "core/tracing/log_context_suffix.h"

#include <charconv>
#include <cstring>
#include <limits>

#include "core/tracing/trace_context.h"

namespace tracing {

namespace {

constexpr std::string_view kSuffixOpen = " [ctx";
constexpr std::string_view kTraceIdKey = " trace_id=";
constexpr std::string_view kSpanIdKey = " span_id=";
constexpr std::string_view kPlayerIdKey = " player_id=";
constexpr std::string_view kSuffixClose = "]";
constexpr size_t kMaxUint64DecimalDigits = std::numeric_limits<uint64_t>::digits10 + 1;
constexpr size_t kTraceSpanHexChars = kTraceIdHexChars + kSpanIdHexChars;

static_assert(kMaxLogContextSuffixBytes ==
                  kSuffixOpen.size() + kTraceIdKey.size() + kTraceIdHexChars + kSpanIdKey.size() + kSpanIdHexChars +
                      kPlayerIdKey.size() + kMaxUint64DecimalDigits + kSuffixClose.size() + 1,
              "kMaxLogContextSuffixBytes must equal the total length of the suffix parts");

// 把 text 写到 cursor 处,返回写完之后的位置。容量由调用方事先核过。
char* AppendText(char* cursor, std::string_view text)
{
    std::memcpy(cursor, text.data(), text.size());
    return cursor + text.size();
}

} // namespace

size_t AppendLogContextSuffix(std::string_view line, const LogContextFields& fields, char* out, size_t outCapacity)
{
    const bool hasTrace = fields.traceSpanHex.size() == kTraceSpanHexChars;
    const bool hasPlayer = fields.playerId != 0;
    if (!hasTrace && !hasPlayer) {
        return 0;
    }

    const bool endsWithNewline = !line.empty() && line.back() == '\n';
    const std::string_view body = endsWithNewline ? line.substr(0, line.size() - 1) : line;
    if (out == nullptr || outCapacity < body.size() + kMaxLogContextSuffixBytes) {
        return 0;
    }

    char* cursor = AppendText(out, body);
    cursor = AppendText(cursor, kSuffixOpen);
    if (hasTrace) {
        cursor = AppendText(cursor, kTraceIdKey);
        cursor = AppendText(cursor, fields.traceSpanHex.substr(0, kTraceIdHexChars));
        cursor = AppendText(cursor, kSpanIdKey);
        cursor = AppendText(cursor, fields.traceSpanHex.substr(kTraceIdHexChars, kSpanIdHexChars));
    }
    if (hasPlayer) {
        cursor = AppendText(cursor, kPlayerIdKey);
        // 容量已按最长后缀核过,to_chars 不会失败。
        cursor = std::to_chars(cursor, out + outCapacity, fields.playerId).ptr;
    }
    cursor = AppendText(cursor, kSuffixClose);
    *cursor++ = '\n';
    return static_cast<size_t>(cursor - out);
}

} // namespace tracing
