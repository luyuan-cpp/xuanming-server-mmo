#pragma once

// 测试替身:内存里的 nghttp2 **客户端**会话(docs/design/grpc-muduo-transport.md §15.14:
// ServerSession 单测用它发任意请求 —— 包括 ClientSession 永远不会发的畸形请求)。
//
// 与 TestH2Peer 对称:不碰 socket;Receive 吃服务端发来的字节,TakeOutput 吐出要发给服务端的字节;
// Pump 把它和 ServerSession 直接在内存里对接。用例也可以不用 Pump、手工控制两边字节的交付时机
// (例如让服务端的 GOAWAY 与客户端的新流在「线上」交错)。
//
// 每个 OpenStream / Send* 都**立即**把帧序列化进输出缓冲:线上帧序严格等于脚本调用顺序。
// nghttp2 不校验**发出去**的请求头,所以缺伪头、连接级头、te: gzip 可以交给服务端拒绝;
// 普通 OpenStream 仍会由 nghttp2 把头名转成小写。要测试线上大写头名,必须显式走保留原名的非法输入入口。
// 头部值一律是**线上值**(-bin 头由调用方自己给 base64)。
//
// 记录:每个流的响应头块(含 :status)、trailers 块、DATA 负载、本流上收到的帧序列(含 RST_STREAM 错误码)、
// 流关闭;连接级的 SETTINGS / PING / GOAWAY。
//
// 线程:不加锁,只能在一个线程上使用。nghttp2 回调里只记状态,不回写、不调用测试代码。

#include <cstddef>
#include <cstdint>
#include <map>
#include <memory>
#include <string>
#include <string_view>
#include <vector>

#include "test_h2_peer.h" // HeaderList / SettingEntry / ReceivedGoaway / h2 常量

struct nghttp2_session;

namespace muduo_grpc
{
class ServerSession;
} // namespace muduo_grpc

namespace muduo_grpc_test
{

// test_h2_peer.h 里没有的帧类型与标志(RFC 9113 数值)。
namespace h2
{
inline constexpr uint8_t kFrameRstStream = 0x3;
inline constexpr uint8_t kFramePing = 0x6;
inline constexpr uint8_t kFrameGoaway = 0x7;
inline constexpr uint8_t kFrameWindowUpdate = 0x8;

inline constexpr uint8_t kFlagEndStream = 0x1;
inline constexpr uint8_t kFlagAck = 0x1;
} // namespace h2

// 本流上收到的一帧。
struct ClientFrame
{
    uint8_t type{0};
    uint8_t flags{0};
    uint32_t errorCode{0}; // 只对 RST_STREAM 有意义

    bool endStream() const { return (flags & h2::kFlagEndStream) != 0; }
};

// 客户端看到的一个流(响应方向)。
struct ClientStream
{
    int32_t streamId{0};
    HeaderList responseHeaders; // 第一个头块(含 :status),线上原值与线上顺序
    HeaderList trailers;        // 第二个头块
    std::string body;           // 响应 DATA 负载的拼接(含 5 字节 gRPC 前缀)
    std::vector<ClientFrame> frames; // 本流上收到的 HEADERS / DATA / RST_STREAM,按到达顺序
    bool endStreamReceived{false};
    bool rstReceived{false};
    uint32_t rstErrorCode{0};
    bool closed{false};
    uint32_t closeErrorCode{0};

    const std::string *FindResponseHeader(std::string_view name) const;
    const std::string *FindTrailer(std::string_view name) const;
};

struct TestH2ClientOptions
{
    // 构造时提交的 SETTINGS 项;空 = 一个不带任何项的 SETTINGS 帧(client magic 由 nghttp2 自动先发)。
    std::vector<SettingEntry> initialSettings;
    // > 0:构造时把本端连接级接收窗口设成这个值(nghttp2_session_set_local_window_size,随 SETTINGS 之后发
    // WINDOW_UPDATE);0 = 保持 65535。流级窗口用 initialSettings 里的 INITIAL_WINDOW_SIZE。
    int32_t localConnectionWindowBytes{0};
};

class TestH2Client
{
public:
    TestH2Client();
    explicit TestH2Client(TestH2ClientOptions options);
    ~TestH2Client();
    TestH2Client(const TestH2Client &) = delete;
    TestH2Client &operator=(const TestH2Client &) = delete;

    // ---- 字节进出 ----
    // 喂入服务端发来的字节;false = nghttp2 致命错误(LastError() 说明原因)。
    bool Receive(std::string_view bytes);
    // 取走所有待发给服务端的字节(先把 nghttp2 队列里的帧序列化出来)。
    std::string TakeOutput();
    // 内存对接:本端输出 → server.Receive;server.DrainOutput → 本端 Receive,来回搬到两边都没有新字节为止。
    // false = 某一端报错(服务端 Receive / DrainOutput 返回非 OK 也算;DrainOutput 报错前已产出的字节会先交给本端,
    // 便于用例检查 GOAWAY)或 maxRounds 轮内没静止。
    bool Pump(muduo_grpc::ServerSession &server, int maxRounds = 256);

    // ---- 观察 ----
    const ClientStream *FindStream(int32_t streamId) const;
    // 连接上收到的全部帧类型,按到达顺序。
    const std::vector<uint8_t> &FrameTypes() const { return frameTypes; }
    size_t SettingsFramesReceived() const { return settingsFramesReceived; }
    // 最近一个非 ACK SETTINGS 帧的全部项,顺序与线上一致。
    const std::vector<SettingEntry> &LastPeerSettings() const { return lastPeerSettings; }
    size_t PingsReceived() const { return pingsReceived; }       // 服务端发来的非 ACK PING(ACK 由 nghttp2 自动回)
    size_t PingAcksReceived() const { return pingAcksReceived; }
    const std::vector<ReceivedGoaway> &GoawaysReceived() const { return goawaysReceived; }
    // 客户端视角下服务端的连接级接收窗口(验证服务端放大了连接窗口)。
    int32_t RemoteConnectionWindow() const;
    const std::string &LastError() const { return lastError; }

    // ---- 脚本:每个调用立即序列化成帧 ----
    // 开一个新流发请求头;返回流 id(> 0),失败返回 -1(LastError() 说明原因)。
    int32_t OpenStream(const HeaderList &headers, bool endStream);
    // 仅供非法头名用例:绕过提交时的头名小写化,仍由同一个 nghttp2 会话编码、分配流和接收响应。
    // 头名由本对象深拷贝并持有到会话析构后,不借用调用方或临时字符串;头值照常由 nghttp2 拷贝。
    int32_t OpenStreamWithUnnormalizedNames(const HeaderList &headers, bool endStream);
    // 一次调用 = 一个 DATA 帧(超过 16KiB 时由 nghttp2 拆成多帧)。必须能在当前流控窗口内发完。
    bool SendData(int32_t streamId, std::string_view bytes, bool endStream);
    bool SendRstStream(int32_t streamId, uint32_t errorCode);
    bool SendSettings(const std::vector<SettingEntry> &entries);
    bool SendPing();

private:
    friend struct TestH2ClientCallbacks;

    bool Flush();
    bool Fail(std::string message);
    ClientStream &StreamFor(int32_t streamId);
    int32_t OpenStreamWithNameFlags(const HeaderList &headers, bool endStream, uint8_t nameFlags);

    struct PendingData
    {
        std::string bytes;
        size_t offset{0};
    };

    struct SessionDeleter
    {
        void operator()(nghttp2_session *session) const;
    };

    // NO_COPY_NAME 借用的名称必须活过会话;放在 session_ 前,按逆序析构在会话释放后才销毁。
    // unique_ptr 所有的 HeaderList 不会随外层 vector 扩容移动,提交之后也不再修改。
    std::vector<std::unique_ptr<HeaderList>> retainedHeaderNames;
    std::unique_ptr<nghttp2_session, SessionDeleter> session;
    std::map<int32_t, ClientStream> streams;
    std::map<int32_t, PendingData> pendingData;
    std::string output;
    std::vector<uint8_t> frameTypes;
    size_t settingsFramesReceived{0};
    std::vector<SettingEntry> lastPeerSettings;
    size_t pingsReceived{0};
    size_t pingAcksReceived{0};
    std::vector<ReceivedGoaway> goawaysReceived;
    std::string lastError;
};

} // namespace muduo_grpc_test
