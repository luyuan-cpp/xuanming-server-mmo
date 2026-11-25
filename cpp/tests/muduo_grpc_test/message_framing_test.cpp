// gRPC Length-Prefixed-Message 的拼装与增量拆包(docs/design/grpc-muduo-transport.md §4.1、§4.3)。
//
// 线上格式:1 字节压缩标志 + 4 字节大端长度 + 消息体;DATA 帧边界与消息边界无关。
// 期望字节一律手写,不用被测函数自己拼出来再拆。

#include <gtest/gtest.h>

#include <cstdint>
#include <string>
#include <string_view>
#include <vector>

#include "muduo_grpc/message_framing.h"
#include "muduo_grpc/status.h"

namespace
{

using muduo_grpc::MessageDeframer;
using muduo_grpc::Status;
using muduo_grpc::StatusCode;

std::string Prefix(uint8_t flag, uint32_t length)
{
    std::string out;
    out.push_back(static_cast<char>(flag));
    out.push_back(static_cast<char>((length >> 24) & 0xFF));
    out.push_back(static_cast<char>((length >> 16) & 0xFF));
    out.push_back(static_cast<char>((length >> 8) & 0xFF));
    out.push_back(static_cast<char>(length & 0xFF));
    return out;
}

std::string Framed(std::string_view payload)
{
    return Prefix(0, static_cast<uint32_t>(payload.size())) + std::string(payload);
}

Status Feed(MessageDeframer &deframer, std::string_view bytes, std::vector<std::string> &out)
{
    return deframer.Feed(reinterpret_cast<const uint8_t *>(bytes.data()), bytes.size(), out);
}

TEST(MuduoGrpcFraming, AppendWritesUncompressedBigEndianPrefix)
{
    std::string out;
    muduo_grpc::AppendLengthPrefixedMessage(out, "abc");
    EXPECT_EQ(out, std::string("\x00\x00\x00\x00\x03" "abc", 8));

    std::string empty;
    muduo_grpc::AppendLengthPrefixedMessage(empty, "");
    EXPECT_EQ(empty, std::string(5, '\0')) << "空消息也有 5 字节前缀";

    // 300 = 0x0000012C:验证大端字节序。
    std::string big;
    muduo_grpc::AppendLengthPrefixedMessage(big, std::string(300, 'x'));
    ASSERT_EQ(big.size(), 305u);
    EXPECT_EQ(big.substr(0, 5), std::string("\x00\x00\x00\x01\x2C", 5));
    EXPECT_EQ(big.substr(5), std::string(300, 'x'));

    // 追加,不覆盖已有内容。
    std::string appended = "head";
    muduo_grpc::AppendLengthPrefixedMessage(appended, "z");
    EXPECT_EQ(appended, std::string("head\x00\x00\x00\x00\x01z", 10));
    EXPECT_EQ(muduo_grpc::kMessagePrefixBytes, 5u);
}

TEST(MuduoGrpcFraming, SingleMessage)
{
    MessageDeframer deframer(1024);
    std::vector<std::string> out;
    ASSERT_TRUE(Feed(deframer, Framed("hello"), out).ok());
    ASSERT_EQ(out.size(), 1u);
    EXPECT_EQ(out[0], "hello");
    EXPECT_FALSE(deframer.HasPartialMessage());
}

TEST(MuduoGrpcFraming, EmptyMessageIsStillOneMessage)
{
    MessageDeframer deframer(1024);
    std::vector<std::string> out;
    ASSERT_TRUE(Feed(deframer, Prefix(0, 0), out).ok());
    ASSERT_EQ(out.size(), 1u);
    EXPECT_TRUE(out[0].empty());
    EXPECT_FALSE(deframer.HasPartialMessage());
}

TEST(MuduoGrpcFraming, ByteByByte)
{
    const std::string wire = Framed("payload");
    MessageDeframer deframer(1024);
    std::vector<std::string> out;
    for (size_t i = 0; i < wire.size(); ++i)
    {
        ASSERT_TRUE(Feed(deframer, std::string_view(wire).substr(i, 1), out).ok()) << i;
        if (i + 1 < wire.size())
        {
            EXPECT_TRUE(out.empty()) << i;
            EXPECT_TRUE(deframer.HasPartialMessage()) << i;
        }
    }
    ASSERT_EQ(out.size(), 1u);
    EXPECT_EQ(out[0], "payload");
    EXPECT_FALSE(deframer.HasPartialMessage());
}

TEST(MuduoGrpcFraming, SeveralMessagesInOneFeed)
{
    MessageDeframer deframer(1024);
    std::vector<std::string> out;
    ASSERT_TRUE(Feed(deframer, Framed("one") + Framed("") + Framed("three"), out).ok());
    ASSERT_EQ(out.size(), 3u);
    EXPECT_EQ(out[0], "one");
    EXPECT_TRUE(out[1].empty());
    EXPECT_EQ(out[2], "three");
    EXPECT_FALSE(deframer.HasPartialMessage());
}

TEST(MuduoGrpcFraming, MessagesSpanArbitraryChunks)
{
    const std::string wire = Framed("first-message") + Framed("second");
    // 切在前缀中间、消息体中间、两条消息的边界之后各一刀。
    const std::vector<size_t> cuts = {2, 9, 18, 21, wire.size()};
    MessageDeframer deframer(1024);
    std::vector<std::string> out;
    size_t begin = 0;
    for (const size_t end : cuts)
    {
        ASSERT_TRUE(Feed(deframer, std::string_view(wire).substr(begin, end - begin), out).ok());
        begin = end;
    }
    ASSERT_EQ(out.size(), 2u);
    EXPECT_EQ(out[0], "first-message");
    EXPECT_EQ(out[1], "second");
    EXPECT_FALSE(deframer.HasPartialMessage());
}

TEST(MuduoGrpcFraming, LimitIsInclusive)
{
    MessageDeframer deframer(4);
    std::vector<std::string> out;
    ASSERT_TRUE(Feed(deframer, Framed("1234"), out).ok());
    ASSERT_EQ(out.size(), 1u);
    EXPECT_EQ(out[0], "1234");
}

TEST(MuduoGrpcFraming, OverLimitIsResourceExhaustedAndSticky)
{
    MessageDeframer deframer(4);
    std::vector<std::string> out;
    // 只看前缀就要拒绝,不必等消息体到齐。
    const Status first = Feed(deframer, Prefix(0, 5), out);
    EXPECT_EQ(first.Code(), StatusCode::kResourceExhausted) << first.ToString();
    EXPECT_TRUE(out.empty());

    // 之后一直返回同一个错误,哪怕喂的是合法消息。
    const Status second = Feed(deframer, Framed("ok"), out);
    EXPECT_EQ(second.Code(), StatusCode::kResourceExhausted);
    EXPECT_EQ(second.message(), first.message());
    EXPECT_TRUE(out.empty());
}

TEST(MuduoGrpcFraming, DefaultFourMiBLimitBoundary)
{
    constexpr size_t kLimit = 4 * 1024 * 1024;
    {
        MessageDeframer deframer(kLimit);
        std::vector<std::string> out;
        ASSERT_TRUE(Feed(deframer, Prefix(0, static_cast<uint32_t>(kLimit)), out).ok());
        EXPECT_TRUE(deframer.HasPartialMessage()) << "等消息体";
    }
    {
        MessageDeframer deframer(kLimit);
        std::vector<std::string> out;
        EXPECT_EQ(Feed(deframer, Prefix(0, static_cast<uint32_t>(kLimit + 1)), out).Code(),
                  StatusCode::kResourceExhausted);
    }
}

TEST(MuduoGrpcFraming, CompressedFlagIsInternalAndSticky)
{
    MessageDeframer deframer(1024);
    std::vector<std::string> out;
    const Status first = Feed(deframer, Prefix(1, 3) + "abc", out);
    EXPECT_EQ(first.Code(), StatusCode::kInternal) << first.ToString();
    EXPECT_TRUE(out.empty());
    EXPECT_EQ(Feed(deframer, Framed("ok"), out).Code(), StatusCode::kInternal);
    EXPECT_TRUE(out.empty());
}

TEST(MuduoGrpcFraming, UnknownFlagByteIsInternal)
{
    for (const uint8_t flag : {uint8_t{2}, uint8_t{0x80}, uint8_t{0xFF}})
    {
        MessageDeframer deframer(1024);
        std::vector<std::string> out;
        const Status status = Feed(deframer, Prefix(flag, 1) + "x", out);
        EXPECT_EQ(status.Code(), StatusCode::kInternal) << static_cast<int>(flag);
        EXPECT_TRUE(out.empty());
    }
}

TEST(MuduoGrpcFraming, HasPartialMessageStates)
{
    MessageDeframer deframer(1024);
    std::vector<std::string> out;
    EXPECT_FALSE(deframer.HasPartialMessage()) << "新建";

    const std::string wire = Framed("abcdef");
    // 前缀的 1..4 字节:半截前缀。
    ASSERT_TRUE(Feed(deframer, std::string_view(wire).substr(0, 1), out).ok());
    EXPECT_TRUE(deframer.HasPartialMessage());
    ASSERT_TRUE(Feed(deframer, std::string_view(wire).substr(1, 3), out).ok());
    EXPECT_TRUE(deframer.HasPartialMessage());
    // 前缀齐了,消息体一个字节都没到。
    ASSERT_TRUE(Feed(deframer, std::string_view(wire).substr(4, 1), out).ok());
    EXPECT_TRUE(deframer.HasPartialMessage());
    // 消息体到了一半。
    ASSERT_TRUE(Feed(deframer, std::string_view(wire).substr(5, 3), out).ok());
    EXPECT_TRUE(deframer.HasPartialMessage());
    EXPECT_TRUE(out.empty());
    // 收齐。
    ASSERT_TRUE(Feed(deframer, std::string_view(wire).substr(8), out).ok());
    EXPECT_FALSE(deframer.HasPartialMessage());
    ASSERT_EQ(out.size(), 1u);
    EXPECT_EQ(out[0], "abcdef");

    // 空消息的前缀收齐即完整。
    ASSERT_TRUE(Feed(deframer, Prefix(0, 0), out).ok());
    EXPECT_FALSE(deframer.HasPartialMessage());
    EXPECT_EQ(out.size(), 2u);
}

} // namespace
