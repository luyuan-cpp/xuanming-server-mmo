#pragma once

// 测试替身:内存里的 nghttp2 **服务端**会话(docs/design/grpc-muduo-transport.md §3:
// ClientSession 单测与一个内存中的 nghttp2 服务端会话对接,完全确定性)。
//
// 它不碰 socket:Receive 吃客户端发来的字节,TakeOutput 吐出要发给客户端的字节。
//   * client_session_test 用 Pump 把它和 ClientSession 直接在内存里对接;
//   * channel_test 把它挂到 muduo TcpServer 的连接回调上,做回环端到端。
//
// 记录:每个流的请求头(线上原值、线上顺序)、请求体、是否收到 END_STREAM / RST_STREAM、
// 以及 PING、SETTINGS、GOAWAY。
// 回写:按测试脚本发响应头、DATA(一次调用一帧,连续调用即可把一条消息拆成多帧)、trailers、
// trailers-only、RST_STREAM、GOAWAY(可指定 last_stream_id 与 debug)、SETTINGS、PING。
// 每个 Send* 都**立即**把帧序列化进输出缓冲:线上帧的先后严格等于脚本调用的先后
// (否则 nghttp2 自己的发送队列会让 HEADERS 插到 DATA 前面,trailers 先于消息体到达)。
//
// 线程:不加锁,只能在一个线程上使用。nghttp2 回调里只记状态,不回写、不调用测试代码;
// 测试在 Receive 返回之后用 TakeCompletedRequests 取「请求已收完」的流,再决定怎么应答。
//
// 头部值一律是**线上值**:发 -bin 头时调用方自己给 base64(这正是要验证的那一层)。

#include <cstddef>
#include <cstdint>
#include <map>
#include <memory>
#include <string>
#include <string_view>
#include <utility>
#include <vector>

struct nghttp2_session;

namespace muduo_grpc
{
class ClientSession;
} // namespace muduo_grpc

namespace muduo_grpc_test
{

// RFC 9113 的错误码与 SETTINGS 标识。测试文件用这些常量,而不 include nghttp2.h:
// 一来测试按规范数值断言,不依赖实现库的枚举;二来 nghttp2.h 必须先于 muduo 头文件包含
// (muduo 的 Windows 适配头 #define 了 off_t 等名字),只在 test_h2_peer.cpp 一处包含最安全。
namespace h2
{
inline constexpr uint32_t kNoError = 0x0;
inline constexpr uint32_t kProtocolError = 0x1;
inline constexpr uint32_t kInternalError = 0x2;
inline constexpr uint32_t kRefusedStream = 0x7;
inline constexpr uint32_t kCancel = 0x8;
inline constexpr uint32_t kEnhanceYourCalm = 0xb;

inline constexpr int32_t kSettingsEnablePush = 0x2;
inline constexpr int32_t kSettingsMaxConcurrentStreams = 0x3;
inline constexpr int32_t kSettingsInitialWindowSize = 0x4;
inline constexpr int32_t kSettingsMaxHeaderListSize = 0x6;

inline constexpr uint8_t kFrameData = 0x0;
inline constexpr uint8_t kFrameHeaders = 0x1;
inline constexpr uint8_t kFrameSettings = 0x4;

// 两段式 GOAWAY 第一段用的 last_stream_id(2^31-1)。
inline constexpr int32_t kMaxStreamId = 0x7fffffff;
} // namespace h2

using HeaderList = std::vector<std::pair<std::string, std::string>>;

struct SettingEntry
{
    int32_t id{0};
    uint32_t value{0};

    friend bool operator==(const SettingEntry &, const SettingEntry &) = default;
};

// 服务端看到的一个流。
struct ReceivedStream
{
    int32_t streamId{0};
    HeaderList headers;          // 请求头:线上原值,顺序与线上一致(含伪头)
    std::string body;            // 请求 DATA 负载的拼接(含 5 字节 gRPC 前缀)
    bool requestEnded{false};    // 收到 END_STREAM
    bool rstReceived{false};     // 收到客户端的 RST_STREAM
    uint32_t rstErrorCode{0};
    bool closed{false};          // 服务端会话里这个流已关闭
    uint32_t closeErrorCode{0};

    // 第一个同名头的值;没有返回 nullptr。
    const std::string *FindHeader(std::string_view name) const;
    size_t CountHeader(std::string_view name) const;
};

struct ReceivedGoaway
{
    int32_t lastStreamId{0};
    uint32_t errorCode{0};
    std::string debugData;
};

struct TestH2PeerOptions
{
    // false = 构造时不发服务端 SETTINGS(握手用例要验证「没收到 SETTINGS 就不算 READY」)。
    bool sendInitialSettings{true};
    std::vector<SettingEntry> initialSettings; // 空 = 一个不带任何项的 SETTINGS 帧
};

class TestH2Peer
{
public:
    TestH2Peer();
    explicit TestH2Peer(TestH2PeerOptions options);
    ~TestH2Peer();
    TestH2Peer(const TestH2Peer &) = delete;
    TestH2Peer &operator=(const TestH2Peer &) = delete;

    // ---- 字节进出 ----
    // 喂入客户端发来的字节;false = nghttp2 致命错误(LastError() 说明原因)。
    bool Receive(std::string_view bytes);
    // 取走所有待发给客户端的字节(先把 nghttp2 队列里的帧序列化出来)。
    std::string TakeOutput();
    // 内存对接:客户端 DrainOutput → 服务端 mem_recv2;服务端 mem_send2 → 客户端 Receive,
    // 来回搬到两边都没有新字节为止。false = 某一端报错或 maxRounds 轮内没静止。
    bool Pump(muduo_grpc::ClientSession &client, int maxRounds = 256);

    // ---- 观察 ----
    const ReceivedStream *FindStream(int32_t streamId) const;
    // 按首次出现的先后排列的流 id。
    const std::vector<int32_t> &StreamIds() const { return arrivalOrder; }
    // 自上次调用以来请求已收完(END_STREAM)的流 id,按收完的先后。
    std::vector<int32_t> TakeCompletedRequests();
    size_t PingsReceived() const { return pingsReceived; }
    size_t SettingsFramesReceived() const { return settingsFramesReceived; }
    // 最近一个非 ACK SETTINGS 帧的全部项,顺序与线上一致。
    const std::vector<SettingEntry> &LastPeerSettings() const { return lastPeerSettings; }
    const std::vector<ReceivedGoaway> &GoawaysReceived() const { return goawaysReceived; }
    // 服务端视角下客户端的连接级接收窗口(验证客户端放大了连接窗口)。
    int32_t RemoteConnectionWindow() const;
    const std::string &LastError() const { return lastError; }

    // ---- 脚本:每个调用立即序列化成帧;false = nghttp2 拒绝(LastError() 说明原因) ----
    bool SendSettings(const std::vector<SettingEntry> &entries);
    bool SendHeaders(int32_t streamId, const HeaderList &headers, bool endStream);
    // 一次调用 = 一个 DATA 帧(超过 16KiB 时由 nghttp2 拆成多帧)。必须能在当前流控窗口内发完。
    bool SendData(int32_t streamId, std::string_view bytes, bool endStream);
    bool SendRstStream(int32_t streamId, uint32_t errorCode);
    bool SendGoaway(int32_t lastStreamId, uint32_t errorCode, std::string_view debugData);
    // 两段式 GOAWAY 的第一段:last_stream_id = 2^31-1(与 grpc-go 的优雅关闭一致)。
    bool SendShutdownNotice();
    bool SendPing();

    // ---- 组合 ----
    // 标准成功应答:HEADERS(:status 200、application/grpc、initialMetadata)→ 一条消息 →
    // trailers(grpc-status 0、空 grpc-message、trailingMetadata)。
    bool SendUnaryOk(int32_t streamId, std::string_view message, const HeaderList &initialMetadata = {},
                     const HeaderList &trailingMetadata = {});
    // trailers-only:单个 HEADERS 带 END_STREAM。grpcMessageWire 是线上值(已百分号编码)。
    bool SendTrailersOnly(int32_t streamId, uint32_t grpcStatus, std::string_view grpcMessageWire,
                          const HeaderList &extra = {});

    // 5 字节 gRPC 前缀 + 负载。手工拼装,不复用被测库的 message_framing。
    static std::string Frame(std::string_view payload, uint8_t compressedFlag = 0);
    // 只有前缀、声明长度为 declaredLength(用于「超上限」用例,不必真的发那么多字节)。
    static std::string PrefixOnly(uint32_t declaredLength, uint8_t compressedFlag = 0);

private:
    friend struct TestH2PeerCallbacks;

    bool Flush();
    bool Fail(std::string message);
    ReceivedStream &StreamFor(int32_t streamId);
    void MarkRequestEnded(int32_t streamId);

    struct PendingData
    {
        std::string bytes;
        size_t offset{0};
    };

    struct SessionDeleter
    {
        void operator()(nghttp2_session *session) const;
    };

    std::unique_ptr<nghttp2_session, SessionDeleter> session;
    std::map<int32_t, ReceivedStream> streams;
    std::vector<int32_t> arrivalOrder;
    std::vector<int32_t> completedRequests;
    std::map<int32_t, PendingData> pendingData;
    std::string output;
    size_t pingsReceived{0};
    size_t settingsFramesReceived{0};
    std::vector<SettingEntry> lastPeerSettings;
    std::vector<ReceivedGoaway> goawaysReceived;
    std::string lastError;
};

} // namespace muduo_grpc_test
