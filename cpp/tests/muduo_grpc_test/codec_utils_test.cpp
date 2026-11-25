// muduo_grpc 工具层单测:状态码与映射、metadata 编码、grpc-timeout(docs/design/grpc-muduo-transport.md §4)。
//
// 全是纯函数,不碰网络。期望值一律按规范 / 对端实现手写,不由被测函数推导:
//   * 状态码数值与名字:gRPC doc/statuscodes.md(与 grpc::StatusCode、grpc-go codes 相同);
//   * HTTP → gRPC:gRPC doc/http-grpc-status-mapping.md;
//   * RST_STREAM → gRPC:PROTOCOL-HTTP2.md「Errors」一节;
//   * base64:RFC 4648 §10 测试向量;
//   * grpc-timeout:grpc-go internal/grpcutil/encode_duration.go 的取单位与向上取整规则。

#include <gtest/gtest.h>

#include <chrono>
#include <cstdint>
#include <optional>
#include <string>
#include <string_view>
#include <vector>

#include "muduo_grpc/grpc_timeout.h"
#include "muduo_grpc/metadata.h"
#include "muduo_grpc/status.h"

namespace
{

using muduo_grpc::Status;
using muduo_grpc::StatusCode;
using namespace std::chrono_literals;

// gate 发出的 x-session-detail-bin 应用层值:go/client_rpc_router/internal/logic/forwardlogic_test.go 的样例,
// 本身已是一段标准 base64(带 padding)。
constexpr std::string_view kSessionAppValue = "CgQIARABEgIIAQ==";
// 上面这个值在线上的样子:按 -bin 规则再编一层,标准字母表、不带 padding。
constexpr std::string_view kSessionWireValue = "Q2dRSUFSQUJFZ0lJQVE9PQ";

std::string Bytes(std::initializer_list<int> values)
{
    std::string out;
    for (const int v : values)
    {
        out.push_back(static_cast<char>(v));
    }
    return out;
}

// ── 状态码 ──────────────────────────────────────────────────────────────

struct StatusCodeCase
{
    StatusCode code;
    uint32_t value;
    std::string_view name;
};

constexpr StatusCodeCase kStatusCodeCases[] = {
    {StatusCode::kOk, 0, "OK"},
    {StatusCode::kCancelled, 1, "CANCELLED"},
    {StatusCode::kUnknown, 2, "UNKNOWN"},
    {StatusCode::kInvalidArgument, 3, "INVALID_ARGUMENT"},
    {StatusCode::kDeadlineExceeded, 4, "DEADLINE_EXCEEDED"},
    {StatusCode::kNotFound, 5, "NOT_FOUND"},
    {StatusCode::kAlreadyExists, 6, "ALREADY_EXISTS"},
    {StatusCode::kPermissionDenied, 7, "PERMISSION_DENIED"},
    {StatusCode::kResourceExhausted, 8, "RESOURCE_EXHAUSTED"},
    {StatusCode::kFailedPrecondition, 9, "FAILED_PRECONDITION"},
    {StatusCode::kAborted, 10, "ABORTED"},
    {StatusCode::kOutOfRange, 11, "OUT_OF_RANGE"},
    {StatusCode::kUnimplemented, 12, "UNIMPLEMENTED"},
    {StatusCode::kInternal, 13, "INTERNAL"},
    {StatusCode::kUnavailable, 14, "UNAVAILABLE"},
    {StatusCode::kDataLoss, 15, "DATA_LOSS"},
    {StatusCode::kUnauthenticated, 16, "UNAUTHENTICATED"},
};

TEST(MuduoGrpcStatus, CodeValuesAndNamesMatchGrpcSpec)
{
    for (const auto &c : kStatusCodeCases)
    {
        EXPECT_EQ(static_cast<uint32_t>(c.code), c.value) << c.name;
        EXPECT_EQ(std::string_view(muduo_grpc::StatusCodeName(c.code)), c.name) << c.value;
        const Status status(c.code, "detail");
        EXPECT_EQ(status.code_value(), c.value);
        EXPECT_EQ(status.ok(), c.value == 0);
    }
    EXPECT_EQ(muduo_grpc::kMaxStatusCodeValue, 16u);
}

TEST(MuduoGrpcStatus, ToStringCarriesNameNumberAndMessage)
{
    EXPECT_EQ(Status(StatusCode::kUnavailable, "connection lost").ToString(), "UNAVAILABLE(14): connection lost");
    EXPECT_EQ(Status::Ok().ToString(), "OK(0)");
    EXPECT_TRUE(Status().ok());
    EXPECT_TRUE(Status().message().empty());
}

TEST(MuduoGrpcStatus, HttpStatusMappingFullTable)
{
    EXPECT_EQ(muduo_grpc::StatusCodeFromHttpStatus(400), StatusCode::kInternal);
    EXPECT_EQ(muduo_grpc::StatusCodeFromHttpStatus(401), StatusCode::kUnauthenticated);
    EXPECT_EQ(muduo_grpc::StatusCodeFromHttpStatus(403), StatusCode::kPermissionDenied);
    EXPECT_EQ(muduo_grpc::StatusCodeFromHttpStatus(404), StatusCode::kUnimplemented);
    EXPECT_EQ(muduo_grpc::StatusCodeFromHttpStatus(429), StatusCode::kUnavailable);
    EXPECT_EQ(muduo_grpc::StatusCodeFromHttpStatus(502), StatusCode::kUnavailable);
    EXPECT_EQ(muduo_grpc::StatusCodeFromHttpStatus(503), StatusCode::kUnavailable);
    EXPECT_EQ(muduo_grpc::StatusCodeFromHttpStatus(504), StatusCode::kUnavailable);
    // 表外一律 UNKNOWN(含 200:能走到这里说明 content-type 不是 gRPC)。
    for (const int other : {0, 100, 200, 204, 301, 302, 405, 418, 500, 501, 505, 599, 999})
    {
        EXPECT_EQ(muduo_grpc::StatusCodeFromHttpStatus(other), StatusCode::kUnknown) << other;
    }
}

TEST(MuduoGrpcStatus, RstStreamMapping)
{
    // RFC 9113 §7 错误码按数值写,不借实现库的枚举。
    const Status refused = muduo_grpc::StatusFromRstStreamError(0x7);
    EXPECT_EQ(refused.Code(), StatusCode::kUnavailable);
    EXPECT_NE(refused.message().find("REFUSED_STREAM"), std::string::npos) << refused.message();

    const Status cancel = muduo_grpc::StatusFromRstStreamError(0x8);
    EXPECT_EQ(cancel.Code(), StatusCode::kCancelled);
    EXPECT_NE(cancel.message().find("CANCEL"), std::string::npos) << cancel.message();

    EXPECT_EQ(muduo_grpc::StatusFromRstStreamError(0xb).Code(), StatusCode::kResourceExhausted); // ENHANCE_YOUR_CALM
    EXPECT_EQ(muduo_grpc::StatusFromRstStreamError(0xc).Code(), StatusCode::kPermissionDenied); // INADEQUATE_SECURITY

    // NO_ERROR / PROTOCOL / INTERNAL / FLOW_CONTROL / SETTINGS_TIMEOUT / STREAM_CLOSED / FRAME_SIZE /
    // COMPRESSION / CONNECT / 未定义值:全部 INTERNAL。
    for (const uint32_t other : {0x0u, 0x1u, 0x2u, 0x3u, 0x4u, 0x5u, 0x6u, 0x9u, 0xau, 0xdu, 0xffu, 0xffffffffu})
    {
        EXPECT_EQ(muduo_grpc::StatusFromRstStreamError(other).Code(), StatusCode::kInternal) << other;
    }
}

TEST(MuduoGrpcStatus, ParseGrpcStatusValueBoundaries)
{
    EXPECT_EQ(muduo_grpc::ParseGrpcStatusValue("0"), StatusCode::kOk);
    EXPECT_EQ(muduo_grpc::ParseGrpcStatusValue("16"), StatusCode::kUnauthenticated);
    for (uint32_t value = 0; value <= 16; ++value)
    {
        EXPECT_EQ(muduo_grpc::ParseGrpcStatusValue(std::to_string(value)), static_cast<StatusCode>(value)) << value;
    }
    for (const std::string_view bad : {"17", "", "-1", "+1", "abc", "1a", " 1", "1 ", "100", "0x1"})
    {
        EXPECT_FALSE(muduo_grpc::ParseGrpcStatusValue(bad).has_value()) << "'" << bad << "'";
    }
}

// ── metadata 键 / 值 ────────────────────────────────────────────────────

TEST(MuduoGrpcMetadata, CustomKeyValidation)
{
    for (const std::string_view ok : {"x-session-detail-bin", "x-trace_id.v2", "abc", "a-bin", "0-9"})
    {
        EXPECT_TRUE(muduo_grpc::IsValidCustomMetadataKey(ok)) << ok;
    }
    for (const std::string_view bad :
         {"", "X-Upper", "x-Upper", ":path", ":authority", "grpc-timeout", "grpc-foo", "te", "content-type",
          "user-agent", "connection", "keep-alive", "proxy-connection", "transfer-encoding", "upgrade", "host",
          "has space", "semi;colon", "x/y"})
    {
        EXPECT_FALSE(muduo_grpc::IsValidCustomMetadataKey(bad)) << "'" << bad << "'";
    }
    // 非 ASCII 字节(UTF-8 的 'é')。
    EXPECT_FALSE(muduo_grpc::IsValidCustomMetadataKey(std::string("x-caf") + Bytes({0xC3, 0xA9})));
}

TEST(MuduoGrpcMetadata, AsciiValueValidation)
{
    EXPECT_TRUE(muduo_grpc::IsValidAsciiMetadataValue(""));
    EXPECT_TRUE(muduo_grpc::IsValidAsciiMetadataValue("plain text ~!@#$%^&*()"));
    EXPECT_TRUE(muduo_grpc::IsValidAsciiMetadataValue(std::string(1, '\x20')));
    EXPECT_TRUE(muduo_grpc::IsValidAsciiMetadataValue(std::string(1, '\x7e')));
    EXPECT_FALSE(muduo_grpc::IsValidAsciiMetadataValue(std::string(1, '\x1f')));
    EXPECT_FALSE(muduo_grpc::IsValidAsciiMetadataValue(std::string(1, '\x7f')));
    EXPECT_FALSE(muduo_grpc::IsValidAsciiMetadataValue("tab\there"));
    EXPECT_FALSE(muduo_grpc::IsValidAsciiMetadataValue("line\r\nbreak"));
    EXPECT_FALSE(muduo_grpc::IsValidAsciiMetadataValue(Bytes({0xE5, 0xA4, 0xB1})));
}

TEST(MuduoGrpcMetadata, BinaryKeySuffix)
{
    EXPECT_TRUE(muduo_grpc::IsBinaryHeaderKey("x-session-detail-bin"));
    EXPECT_TRUE(muduo_grpc::IsBinaryHeaderKey("a-bin"));
    EXPECT_FALSE(muduo_grpc::IsBinaryHeaderKey("-bin")); // 只有后缀不算
    EXPECT_FALSE(muduo_grpc::IsBinaryHeaderKey("bin"));
    EXPECT_FALSE(muduo_grpc::IsBinaryHeaderKey("x-bin-not"));
    EXPECT_FALSE(muduo_grpc::IsBinaryHeaderKey("x-BIN"));
}

TEST(MuduoGrpcMetadata, FindMetadataReturnsFirstMatch)
{
    const muduo_grpc::Metadata metadata = {{"x-a", "1"}, {"x-b", "2"}, {"x-a", "3"}};
    const std::string *first = muduo_grpc::FindMetadata(metadata, "x-a");
    ASSERT_NE(first, nullptr);
    EXPECT_EQ(*first, "1");
    EXPECT_EQ(muduo_grpc::FindMetadata(metadata, "x-missing"), nullptr);
}

// ── base64 ──────────────────────────────────────────────────────────────

struct Base64Vector
{
    std::string_view raw;
    std::string_view padded;
    std::string_view unpadded;
};

// RFC 4648 §10。
constexpr Base64Vector kRfc4648Vectors[] = {
    {"", "", ""},
    {"f", "Zg==", "Zg"},
    {"fo", "Zm8=", "Zm8"},
    {"foo", "Zm9v", "Zm9v"},
    {"foob", "Zm9vYg==", "Zm9vYg"},
    {"fooba", "Zm9vYmE=", "Zm9vYmE"},
    {"foobar", "Zm9vYmFy", "Zm9vYmFy"},
};

TEST(MuduoGrpcBase64, EncodeUnpaddedRfc4648Vectors)
{
    for (const auto &v : kRfc4648Vectors)
    {
        EXPECT_EQ(muduo_grpc::Base64EncodeUnpadded(v.raw), v.unpadded) << "'" << v.raw << "'";
    }
    // 标准字母表的 '+' 与 '/'(不是 URL 安全字母表)。
    EXPECT_EQ(muduo_grpc::Base64EncodeUnpadded(Bytes({0x00, 0xFF, 0xFE})), "AP/+");
}

TEST(MuduoGrpcBase64, DecodeLenientAcceptsPaddedAndUnpadded)
{
    for (const auto &v : kRfc4648Vectors)
    {
        const auto padded = muduo_grpc::Base64DecodeLenient(v.padded);
        ASSERT_TRUE(padded.has_value()) << v.padded;
        EXPECT_EQ(*padded, v.raw);
        const auto unpadded = muduo_grpc::Base64DecodeLenient(v.unpadded);
        ASSERT_TRUE(unpadded.has_value()) << v.unpadded;
        EXPECT_EQ(*unpadded, v.raw);
    }
    const auto binary = muduo_grpc::Base64DecodeLenient("AP/+");
    ASSERT_TRUE(binary.has_value());
    EXPECT_EQ(*binary, Bytes({0x00, 0xFF, 0xFE}));
}

TEST(MuduoGrpcBase64, DecodeLenientRejectsMalformedInput)
{
    for (const std::string_view bad : {
             "Zg=",      // 带 padding 但总长不是 4 的倍数
             "Z",        // 去掉 padding 后长度 % 4 == 1
             "Zm9vY",    // 同上
             "Zm9v!",    // 非字母表字符
             "Zg=a",     // '=' 出现在中间
             "Zg==Zg==", // 同上(拼接的两段)
             "Zm9v===",  // 三个 '='
             "====",
             "-_8",      // URL 安全字母表不接受
             "Zm 9v",    // 中间有空格
         })
    {
        EXPECT_FALSE(muduo_grpc::Base64DecodeLenient(bad).has_value()) << "'" << bad << "'";
    }
}

TEST(MuduoGrpcBase64, BinaryHeaderValueSplitsCommaMergedValues)
{
    const auto merged = muduo_grpc::DecodeBinaryHeaderValue("Zm9v,YmFy");
    ASSERT_TRUE(merged.has_value());
    EXPECT_EQ(*merged, "foobar");

    const auto spaced = muduo_grpc::DecodeBinaryHeaderValue("Zm9v, YmFy");
    ASSERT_TRUE(spaced.has_value());
    EXPECT_EQ(*spaced, "foobar");

    const auto mixedPadding = muduo_grpc::DecodeBinaryHeaderValue("Zg==,Zm8");
    ASSERT_TRUE(mixedPadding.has_value());
    EXPECT_EQ(*mixedPadding, "ffo");

    const auto empty = muduo_grpc::DecodeBinaryHeaderValue("");
    ASSERT_TRUE(empty.has_value());
    EXPECT_TRUE(empty->empty());

    EXPECT_FALSE(muduo_grpc::DecodeBinaryHeaderValue("Zm9v,!!").has_value());
}

// 回归:x-session-detail-bin 的两层 base64(§4.2)。传输层只加 / 解线上那一层,
// 应用层值(本身就是 base64 文本)必须逐字节原样往返 —— 「优化」成一层会让路由服 / login 解不出 SessionDetails。
TEST(MuduoGrpcBase64, SessionDetailBinRoundTripsExactlyOneLayer)
{
    const std::string wire = muduo_grpc::Base64EncodeUnpadded(kSessionAppValue);
    EXPECT_EQ(wire, kSessionWireValue);
    EXPECT_EQ(wire.find('='), std::string::npos) << "发送不带 padding(与 grpc C++ / grpc-go 一致)";

    const auto decoded = muduo_grpc::DecodeBinaryHeaderValue(wire);
    ASSERT_TRUE(decoded.has_value());
    EXPECT_EQ(*decoded, kSessionAppValue);

    // 对端(grpc-go)发回带 padding 的形式也必须解成同一个应用层值。
    const auto decodedPadded = muduo_grpc::DecodeBinaryHeaderValue(std::string(kSessionWireValue) + "==");
    ASSERT_TRUE(decodedPadded.has_value());
    EXPECT_EQ(*decodedPadded, kSessionAppValue);

    // 应用层值再解一层才是 SessionDetails 原始字节 —— 那是 gate 的事,传输层不得代劳。
    const auto raw = muduo_grpc::Base64DecodeLenient(*decoded);
    ASSERT_TRUE(raw.has_value());
    EXPECT_EQ(*raw, Bytes({0x0A, 0x04, 0x08, 0x01, 0x10, 0x01, 0x12, 0x02, 0x08, 0x01}));
    EXPECT_NE(*decoded, *raw);
}

// ── grpc-message 百分号编码 ─────────────────────────────────────────────

TEST(MuduoGrpcMessageEncoding, PercentEncode)
{
    EXPECT_EQ(muduo_grpc::PercentEncodeGrpcMessage(""), "");
    EXPECT_EQ(muduo_grpc::PercentEncodeGrpcMessage("hello world"), "hello world"); // 空格原样
    EXPECT_EQ(muduo_grpc::PercentEncodeGrpcMessage("100%"), "100%25");
    EXPECT_EQ(muduo_grpc::PercentEncodeGrpcMessage("a\nb"), "a%0Ab");
    EXPECT_EQ(muduo_grpc::PercentEncodeGrpcMessage(std::string(1, '\x7f')), "%7F");
    // "失败"的 UTF-8 字节(用转义写,不依赖源文件编码)。
    EXPECT_EQ(muduo_grpc::PercentEncodeGrpcMessage(Bytes({0xE5, 0xA4, 0xB1, 0xE8, 0xB4, 0xA5})),
              "%E5%A4%B1%E8%B4%A5");
}

TEST(MuduoGrpcMessageEncoding, PercentDecode)
{
    EXPECT_EQ(muduo_grpc::PercentDecodeGrpcMessage("hello%20world"), "hello world");
    EXPECT_EQ(muduo_grpc::PercentDecodeGrpcMessage("hello world"), "hello world");
    EXPECT_EQ(muduo_grpc::PercentDecodeGrpcMessage("100%25"), "100%");
    EXPECT_EQ(muduo_grpc::PercentDecodeGrpcMessage("%E5%A4%B1%E8%B4%A5"), Bytes({0xE5, 0xA4, 0xB1, 0xE8, 0xB4, 0xA5}));
    EXPECT_EQ(muduo_grpc::PercentDecodeGrpcMessage("%e5%a4%b1"), Bytes({0xE5, 0xA4, 0xB1})); // 小写十六进制也接受
}

TEST(MuduoGrpcMessageEncoding, PercentDecodeKeepsMalformedSequences)
{
    // 规范:解码永不失败,坏序列原样保留。
    EXPECT_EQ(muduo_grpc::PercentDecodeGrpcMessage("%"), "%");
    EXPECT_EQ(muduo_grpc::PercentDecodeGrpcMessage("abc%"), "abc%");
    EXPECT_EQ(muduo_grpc::PercentDecodeGrpcMessage("%4"), "%4");
    EXPECT_EQ(muduo_grpc::PercentDecodeGrpcMessage("%zz"), "%zz");
    EXPECT_EQ(muduo_grpc::PercentDecodeGrpcMessage("%G1"), "%G1");
    EXPECT_EQ(muduo_grpc::PercentDecodeGrpcMessage("%%41"), "%A");
}

TEST(MuduoGrpcMessageEncoding, RoundTripsEveryByte)
{
    std::string all;
    for (int i = 0; i < 256; ++i)
    {
        all.push_back(static_cast<char>(i));
    }
    const std::string encoded = muduo_grpc::PercentEncodeGrpcMessage(all);
    for (const char c : encoded)
    {
        const auto byte = static_cast<unsigned char>(c);
        EXPECT_TRUE(byte >= 0x20 && byte <= 0x7E) << static_cast<int>(byte);
    }
    EXPECT_EQ(muduo_grpc::PercentDecodeGrpcMessage(encoded), all);
}

// ── grpc-timeout ────────────────────────────────────────────────────────

TEST(MuduoGrpcTimeout, EncodeMatchesGrpcGo)
{
    EXPECT_EQ(muduo_grpc::EncodeGrpcTimeout(800ms), "800000u");
    EXPECT_EQ(muduo_grpc::EncodeGrpcTimeout(10s), "10000000u");
    EXPECT_EQ(muduo_grpc::EncodeGrpcTimeout(12s), "12000000u");
    EXPECT_EQ(muduo_grpc::EncodeGrpcTimeout(100s), "100000m");
    EXPECT_EQ(muduo_grpc::EncodeGrpcTimeout(1ns), "1n");
    EXPECT_EQ(muduo_grpc::EncodeGrpcTimeout(99999999ns), "99999999n");
    EXPECT_EQ(muduo_grpc::EncodeGrpcTimeout(100000000ns), "100000u");
    EXPECT_EQ(muduo_grpc::EncodeGrpcTimeout(100000001ns), "100001u"); // 向上取整
    EXPECT_EQ(muduo_grpc::EncodeGrpcTimeout(1500ns), "1500n");
    EXPECT_EQ(muduo_grpc::EncodeGrpcTimeout(0ns), "0n");
    EXPECT_EQ(muduo_grpc::EncodeGrpcTimeout(-5ms), "0n");
    // int64 纳秒上限:前五档都超 8 位,落到小时档(9223372036854775807 / 3.6e12 向上取整)。
    EXPECT_EQ(muduo_grpc::EncodeGrpcTimeout(std::chrono::nanoseconds::max()), "2562048H");
}

TEST(MuduoGrpcTimeout, EncodeNeverExceedsEightDigits)
{
    for (const std::chrono::nanoseconds d : {std::chrono::nanoseconds(1), std::chrono::nanoseconds(99999999),
                                             std::chrono::nanoseconds(100000000), std::chrono::nanoseconds(8000ms),
                                             std::chrono::nanoseconds(std::chrono::hours(30)),
                                             std::chrono::nanoseconds(std::chrono::hours(100000)),
                                             std::chrono::nanoseconds::max()})
    {
        const std::string encoded = muduo_grpc::EncodeGrpcTimeout(d);
        ASSERT_GE(encoded.size(), 2u);
        EXPECT_LE(encoded.size() - 1, 8u) << encoded;
        if (d == std::chrono::nanoseconds::max())
        {
            continue; // 向上取整后超过 int64 纳秒,解回来的钳位行为单独测(见下一个用例)
        }
        const auto parsed = muduo_grpc::ParseGrpcTimeout(encoded);
        ASSERT_TRUE(parsed.has_value()) << encoded;
        EXPECT_GE(*parsed, d) << encoded << " 只能向上取整,不得把截止时间缩短";
    }
}

// 小时档 8 位数乘 3.6e12 会超过 int64 纳秒:grpc-go decodeTimeout 钳到 math.MaxInt64,这里要求同样的结果,
// 而不是有符号溢出(未定义行为,实际会绕成负数,等于「截止时间已过」)。
TEST(MuduoGrpcTimeout, ParseClampsHoursThatOverflowInt64)
{
    EXPECT_EQ(muduo_grpc::ParseGrpcTimeout("2562047H"), std::chrono::nanoseconds(2562047LL * 3600 * 1000000000LL));
    EXPECT_EQ(muduo_grpc::ParseGrpcTimeout("2562048H"), std::chrono::nanoseconds::max());
    EXPECT_EQ(muduo_grpc::ParseGrpcTimeout("99999999H"), std::chrono::nanoseconds::max());
    // 分钟档 8 位上限 6e18 < int64 上限,不需要钳位。
    EXPECT_EQ(muduo_grpc::ParseGrpcTimeout("99999999M"),
              std::chrono::nanoseconds(99999999LL * 60 * 1000000000LL));
}

TEST(MuduoGrpcTimeout, ParseValidValues)
{
    EXPECT_EQ(muduo_grpc::ParseGrpcTimeout("1n"), std::chrono::nanoseconds(1));
    EXPECT_EQ(muduo_grpc::ParseGrpcTimeout("0n"), std::chrono::nanoseconds(0));
    EXPECT_EQ(muduo_grpc::ParseGrpcTimeout("800000u"), std::chrono::nanoseconds(800ms));
    EXPECT_EQ(muduo_grpc::ParseGrpcTimeout("100000m"), std::chrono::nanoseconds(100s));
    EXPECT_EQ(muduo_grpc::ParseGrpcTimeout("10S"), std::chrono::nanoseconds(10s));
    EXPECT_EQ(muduo_grpc::ParseGrpcTimeout("2M"), std::chrono::nanoseconds(120s));
    EXPECT_EQ(muduo_grpc::ParseGrpcTimeout("1H"), std::chrono::nanoseconds(3600s));
    EXPECT_EQ(muduo_grpc::ParseGrpcTimeout("99999999n"), std::chrono::nanoseconds(99999999)); // 8 位上限
}

TEST(MuduoGrpcTimeout, ParseRejectsMalformedValues)
{
    for (const std::string_view bad : {
             "123456789S", // 9 位数字
             "100",        // 没有单位(末位 '0' 不是单位)
             "10x",        // 未知单位
             "1s",         // 单位区分大小写
             "-1S",        // 负号
             "+1S",
             " 1S",
             "S",
             "",
         })
    {
        EXPECT_FALSE(muduo_grpc::ParseGrpcTimeout(bad).has_value()) << "'" << bad << "'";
    }
}

} // namespace
