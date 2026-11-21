#include "network/codec/websocket.h"

#include <cstring>
#include <memory>
#include <unordered_map>

#include <openssl/sha.h>

#include "muduo/base/Logging.h"
#include "utils/encode/base64.h"

namespace websocket
{

namespace
{

// RFC 6455 §1.3 规定的固定 GUID:拼在 Sec-WebSocket-Key 后面取 SHA-1。
constexpr std::string_view kAcceptGuid = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11";

constexpr std::string_view kBadRequestResponse =
    "HTTP/1.1 400 Bad Request\r\nConnection: close\r\nContent-Length: 0\r\n\r\n";
constexpr std::string_view kBadVersionResponse =
    "HTTP/1.1 426 Upgrade Required\r\nSec-WebSocket-Version: 13\r\nConnection: close\r\nContent-Length: 0\r\n\r\n";

// 操作码(RFC 6455 §5.2)。
constexpr uint8_t kOpContinuation = 0x0;
constexpr uint8_t kOpText = 0x1;
constexpr uint8_t kOpBinary = 0x2;
constexpr uint8_t kOpClose = 0x8;
constexpr uint8_t kOpPing = 0x9;
constexpr uint8_t kOpPong = 0xA;

// 控制帧的载荷上限(RFC 6455 §5.5)。
constexpr std::size_t kMaxControlPayload = 125;
constexpr std::size_t kMaskLen = 4;

// 一条已完成升级握手的连接。
struct Peer
{
    std::weak_ptr<muduo::net::TcpConnection> conn; // 只核对身份,不持有连接
    muduo::net::Buffer payload{0};                 // 去掉帧头的字节,等上层按客户端帧解析;从 0 起按需增长
    bool fragmented{false};                        // 上一个数据帧 FIN=0:下一个数据帧必须是续帧
    bool closePending{false};                      // 本批解析完后要回 Close 并半关
    bool closed{false};                            // 已回 Close:之后的输入一律丢弃
    uint16_t closeCode{0};                         // 要回的关闭码;0 = 不带状态码
};

thread_local std::unordered_map<const muduo::net::TcpConnection *, Peer> tlsPeers;
thread_local Counters tlsCounters;

// 握手被拒与协议错误的频率由公网对端控制:第一条必打,之后每 1024 条一行。
bool Sampled(uint64_t total)
{
    return (total & 0x3FF) == 1;
}

Peer *Find(const muduo::net::TcpConnectionPtr &conn)
{
    if (tlsPeers.empty())
    {
        return nullptr; // 没有 WebSocket 客户端时,每次收发只多这一次判空
    }
    const auto it = tlsPeers.find(conn.get());
    if (it == tlsPeers.end())
    {
        return nullptr;
    }
    if (it->second.conn.lock() != conn)
    {
        tlsPeers.erase(it); // 旧连接的表项没删,地址又被新连接用上:新连接重新判定
        return nullptr;
    }
    return &it->second;
}

// 按字节比较,不走 <cctype>:它受 locale 影响,对负值 char 是未定义行为。
char LowerAscii(char c)
{
    return (c >= 'A' && c <= 'Z') ? static_cast<char>(c - 'A' + 'a') : c;
}

bool EqualsIgnoreCase(std::string_view a, std::string_view b)
{
    if (a.size() != b.size())
    {
        return false;
    }
    for (std::size_t i = 0; i < a.size(); ++i)
    {
        if (LowerAscii(a[i]) != LowerAscii(b[i]))
        {
            return false;
        }
    }
    return true;
}

std::string_view Trim(std::string_view text)
{
    while (!text.empty() && (text.front() == ' ' || text.front() == '\t'))
    {
        text.remove_prefix(1);
    }
    while (!text.empty() && (text.back() == ' ' || text.back() == '\t'))
    {
        text.remove_suffix(1);
    }
    return text;
}

// 逗号分隔的头部值里是否有这个词(不区分大小写),例如 "Connection: keep-alive, Upgrade"。
bool HasToken(std::string_view list, std::string_view token)
{
    while (!list.empty())
    {
        const std::size_t comma = list.find(',');
        if (EqualsIgnoreCase(Trim(list.substr(0, comma)), token))
        {
            return true;
        }
        if (comma == std::string_view::npos)
        {
            break;
        }
        list.remove_prefix(comma + 1);
    }
    return false;
}

Handshake Reject(std::string_view httpResponse, std::string &response)
{
    response.assign(httpResponse);
    return Handshake::kRejected;
}

// 发一个控制帧(Pong / Close)。len 不超过 kMaxControlPayload,调用方保证。
void SendControl(const muduo::net::TcpConnectionPtr &conn, uint8_t opcode, const char *payload, std::size_t len)
{
    char frame[2 + kMaxControlPayload];
    frame[0] = static_cast<char>(0x80 | opcode);
    frame[1] = static_cast<char>(len);
    if (len > 0)
    {
        std::memcpy(frame + 2, payload, len);
    }
    conn->send(frame, static_cast<int>(2 + len));
}

void FailProtocol(const muduo::net::TcpConnectionPtr &conn, Peer &peer, uint16_t closeCode)
{
    peer.closeCode = closeCode;
    peer.closePending = true;
    if (Sampled(++tlsCounters.protocolClosed))
    {
        LOG_WARN << "WebSocket frames rejected (sampled), close_code=" << closeCode
                 << " rejected_total=" << tlsCounters.protocolClosed << " peer=" << conn->peerAddress().toIpPort();
    }
}

// 把 in 里完整的帧逐个处理掉:数据帧去掉掩码追加进 peer.payload,Ping 回 Pong。
// 遇到对端的 Close 帧或协议错误就记下并停下,剩余输入由调用方丢弃。
void DecodeFrames(const muduo::net::TcpConnectionPtr &conn, Peer &peer, muduo::net::Buffer *in,
                  std::size_t maxFramePayload)
{
    while (in->readableBytes() >= 2)
    {
        const auto *bytes = reinterpret_cast<const uint8_t *>(in->peek());
        const bool fin = (bytes[0] & 0x80) != 0;
        const uint8_t opcode = bytes[0] & 0x0F;
        const bool control = (opcode & 0x08) != 0;
        uint64_t payloadLen = bytes[1] & 0x7F;
        // 没协商任何扩展,RSV 位必须为 0;客户端发来的帧必须加掩码(RFC 6455 §5.1、§5.2)。
        if ((bytes[0] & 0x70) != 0 || (bytes[1] & 0x80) == 0)
        {
            return FailProtocol(conn, peer, kCloseProtocolError);
        }
        std::size_t headerLen = 2;
        if (payloadLen == 126)
        {
            headerLen += 2;
        }
        else if (payloadLen == 127)
        {
            headerLen += 8;
        }
        // 控制帧不能分片,载荷至多 125 字节(RFC 6455 §5.5)。
        if (control && (!fin || headerLen != 2))
        {
            return FailProtocol(conn, peer, kCloseProtocolError);
        }
        if (in->readableBytes() < headerLen + kMaskLen)
        {
            return; // 帧头还没收全
        }
        if (headerLen > 2)
        {
            payloadLen = 0;
            for (std::size_t i = 2; i < headerLen; ++i)
            {
                payloadLen = (payloadLen << 8) | bytes[i];
            }
        }
        if (payloadLen > maxFramePayload)
        {
            return FailProtocol(conn, peer, kCloseMessageTooBig);
        }
        const auto length = static_cast<std::size_t>(payloadLen);
        const std::size_t frameLen = headerLen + kMaskLen + length;
        if (in->readableBytes() < frameLen)
        {
            return; // 载荷还没收全
        }
        const uint8_t *mask = bytes + headerLen;
        const uint8_t *masked = mask + kMaskLen;

        switch (opcode)
        {
        case kOpContinuation:
        case kOpBinary:
        {
            // 续帧只能跟在没结束的消息后面;没结束的消息中间不能再开新消息(RFC 6455 §5.4)。
            if ((opcode == kOpContinuation) != peer.fragmented)
            {
                return FailProtocol(conn, peer, kCloseProtocolError);
            }
            peer.payload.ensureWritableBytes(length);
            char *out = peer.payload.beginWrite();
            for (std::size_t i = 0; i < length; ++i)
            {
                out[i] = static_cast<char>(masked[i] ^ mask[i & 3]);
            }
            peer.payload.hasWritten(length);
            peer.fragmented = !fin;
            break;
        }
        case kOpText:
            return FailProtocol(conn, peer, kCloseUnsupportedData);
        case kOpPing:
        {
            char pong[kMaxControlPayload];
            for (std::size_t i = 0; i < length; ++i)
            {
                pong[i] = static_cast<char>(masked[i] ^ mask[i & 3]);
            }
            SendControl(conn, kOpPong, pong, length);
            break;
        }
        case kOpPong:
            break; // 本端不发 Ping,收到的 Pong 忽略
        case kOpClose:
            // 回显对端的状态码(载荷的前两个字节);没带就回不带状态码的 Close。
            peer.closeCode = 0;
            if (length >= 2)
            {
                peer.closeCode = static_cast<uint16_t>(((masked[0] ^ mask[0]) << 8) | (masked[1] ^ mask[1]));
            }
            peer.closePending = true;
            return;
        default:
            return FailProtocol(conn, peer, kCloseProtocolError);
        }
        in->retrieve(frameLen);
    }
}

// 处理升级握手。成功返回新建的表项;没收全、被拒返回 nullptr。
Peer *Upgrade(const muduo::net::TcpConnectionPtr &conn, muduo::net::Buffer *in)
{
    std::size_t consumed = 0;
    std::string response;
    switch (ParseHandshake(std::string_view(in->peek(), in->readableBytes()), consumed, response))
    {
    case Handshake::kIncomplete:
        return nullptr; // 等更多字节;一直不发完由上层的握手期限关掉
    case Handshake::kAccepted:
    {
        in->retrieve(consumed);
        conn->send(response.data(), static_cast<int>(response.size()));
        ++tlsCounters.accepted;
        Peer &peer = tlsPeers[conn.get()];
        peer = Peer{};
        peer.conn = conn;
        return &peer;
    }
    case Handshake::kRejected:
        break;
    }
    in->retrieveAll();
    conn->send(response.data(), static_cast<int>(response.size()));
    conn->shutdown();
    conn->forceCloseWithDelay(0.1);
    if (Sampled(++tlsCounters.rejected))
    {
        LOG_WARN << "WebSocket upgrade requests rejected (sampled), rejected_total=" << tlsCounters.rejected
                 << " peer=" << conn->peerAddress().toIpPort();
    }
    return nullptr;
}

} // namespace

std::string AcceptKey(std::string_view clientKey)
{
    std::string input(clientKey);
    input.append(kAcceptGuid);
    unsigned char digest[SHA_DIGEST_LENGTH];
    SHA1(reinterpret_cast<const unsigned char *>(input.data()), input.size(), digest);
    return Base64Encode(std::string(reinterpret_cast<const char *>(digest), sizeof(digest)));
}

Handshake ParseHandshake(std::string_view data, std::size_t &consumed, std::string &response)
{
    consumed = 0;
    response.clear();

    constexpr std::string_view kMethod = "GET ";
    if (data.substr(0, kMethod.size()) != kMethod.substr(0, data.size()))
    {
        return Reject(kBadRequestResponse, response); // 开头就不是 GET,不必等收全
    }
    const std::size_t headEnd = data.find("\r\n\r\n");
    if (headEnd == std::string_view::npos)
    {
        return data.size() < kMaxHandshakeBytes ? Handshake::kIncomplete : Reject(kBadRequestResponse, response);
    }
    if (headEnd + 4 > kMaxHandshakeBytes)
    {
        return Reject(kBadRequestResponse, response);
    }

    std::string_view head = data.substr(0, headEnd);
    const std::size_t requestLineEnd = head.find("\r\n");
    const std::string_view requestLine = head.substr(0, requestLineEnd);
    constexpr std::string_view kVersion = " HTTP/1.1";
    if (requestLine.size() <= kMethod.size() + kVersion.size() ||
        requestLine.substr(requestLine.size() - kVersion.size()) != kVersion)
    {
        return Reject(kBadRequestResponse, response);
    }
    head = requestLineEnd == std::string_view::npos ? std::string_view{} : head.substr(requestLineEnd + 2);

    bool upgradeWebSocket = false;
    bool connectionUpgrade = false;
    std::string_view key;
    std::string_view version;
    while (!head.empty())
    {
        const std::size_t lineEnd = head.find("\r\n");
        const std::string_view line = head.substr(0, lineEnd);
        head = lineEnd == std::string_view::npos ? std::string_view{} : head.substr(lineEnd + 2);
        const std::size_t colon = line.find(':');
        if (colon == std::string_view::npos)
        {
            continue;
        }
        const std::string_view name = line.substr(0, colon);
        const std::string_view value = Trim(line.substr(colon + 1));
        if (EqualsIgnoreCase(name, "Upgrade"))
        {
            upgradeWebSocket = HasToken(value, "websocket");
        }
        else if (EqualsIgnoreCase(name, "Connection"))
        {
            connectionUpgrade = HasToken(value, "upgrade");
        }
        else if (EqualsIgnoreCase(name, "Sec-WebSocket-Key"))
        {
            key = value;
        }
        else if (EqualsIgnoreCase(name, "Sec-WebSocket-Version"))
        {
            version = value;
        }
    }

    // Key 必须是 16 字节随机数的 Base64(24 个字符)。
    if (!upgradeWebSocket || !connectionUpgrade || key.size() != 24 || Base64Decode(std::string(key)).size() != 16)
    {
        return Reject(kBadRequestResponse, response);
    }
    if (version != "13")
    {
        return Reject(kBadVersionResponse, response);
    }

    response = "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: ";
    response.append(AcceptKey(key));
    response.append("\r\n\r\n");
    consumed = headEnd + 4;
    return Handshake::kAccepted;
}

std::size_t EncodeBinaryHeader(std::size_t payloadLen, char out[10])
{
    out[0] = static_cast<char>(0x80 | kOpBinary);
    if (payloadLen <= 125)
    {
        out[1] = static_cast<char>(payloadLen);
        return 2;
    }
    if (payloadLen <= 0xFFFF)
    {
        out[1] = 126;
        out[2] = static_cast<char>(payloadLen >> 8);
        out[3] = static_cast<char>(payloadLen & 0xFF);
        return 4;
    }
    out[1] = 127;
    for (int i = 0; i < 8; ++i)
    {
        out[2 + i] = static_cast<char>((static_cast<uint64_t>(payloadLen) >> (8 * (7 - i))) & 0xFF);
    }
    return 10;
}

muduo::net::Buffer *Unwrap(const muduo::net::TcpConnectionPtr &conn, muduo::net::Buffer *in,
                           std::size_t maxFramePayload)
{
    Peer *peer = Find(conn);
    if (peer == nullptr)
    {
        // 原始客户端帧以 32 位大端长度开头,长度上限远小于 16MB,首字节恒为 0;'G' 只会是升级请求 "GET ..." 的开头。
        // 已经在跑原始帧的连接在帧边界发来 'G',以前因长度非法被断开,现在等于改走握手,只影响它自己。
        if (in->readableBytes() == 0 || *in->peek() != 'G')
        {
            return in;
        }
        peer = Upgrade(conn, in);
        if (peer == nullptr)
        {
            return nullptr;
        }
    }
    if (!peer->closed && !peer->closePending)
    {
        DecodeFrames(conn, *peer, in, maxFramePayload);
    }
    if (peer->closed || peer->closePending)
    {
        in->retrieveAll();
    }
    // closePending 时仍返回载荷:排在 Close 之前的数据先交给上层,随后由 FinishRead 关闭。
    return peer->closed ? nullptr : &peer->payload;
}

void FinishRead(const muduo::net::TcpConnectionPtr &conn)
{
    Peer *peer = Find(conn);
    if (peer == nullptr || !peer->closePending)
    {
        return;
    }
    peer->closePending = false;
    peer->closed = true;
    // 上层处理本批数据时可能已经强关了连接;send / shutdown 对非连接态都是空操作。
    const char code[2] = {static_cast<char>(peer->closeCode >> 8), static_cast<char>(peer->closeCode & 0xFF)};
    SendControl(conn, kOpClose, code, peer->closeCode == 0 ? 0 : sizeof(code));
    conn->shutdown();
    conn->forceCloseWithDelay(0.1);
}

bool IsUpgraded(const muduo::net::TcpConnectionPtr &conn)
{
    return Find(conn) != nullptr;
}

void Frame(muduo::net::Buffer *frame)
{
    char header[10];
    const std::size_t headerLen = EncodeBinaryHeader(frame->readableBytes(), header);
    if (frame->prependableBytes() >= headerLen)
    {
        frame->prepend(header, headerLen);
        return;
    }
    // 预留区放不下帧头(载荷超过 65535 字节才要 10 字节帧头):换一块缓冲重排。
    muduo::net::Buffer framed(headerLen + frame->readableBytes());
    framed.append(header, headerLen);
    framed.append(frame->peek(), frame->readableBytes());
    frame->swap(framed);
}

void Forget(const muduo::net::TcpConnectionPtr &conn)
{
    if (!tlsPeers.empty())
    {
        tlsPeers.erase(conn.get());
    }
}

const Counters &GetCounters()
{
    return tlsCounters;
}

} // namespace websocket
