#pragma once

#include <cstddef>
#include <cstdint>
#include <string>
#include <string_view>

#include "muduo/net/Buffer.h"
#include "muduo/net/TcpConnection.h"

// 客户端面的 WebSocket 层(RFC 6455 的服务端一侧),只给 ProtobufCodec 用。
//
// 小游戏(微信 / 抖音)的长连接只有 WebSocket。gate / battle 的客户端端口同时接两种连接:原始 TCP 照旧;
// 首字节是 'G' 的先走 HTTP 升级握手,之后每条二进制消息承载的就是原来的客户端帧字节
// (docs/design/minigame-websocket-gate.md W1、W3 ~ W5)。本层只做握手、拆帧与包帧,不认识任何业务消息。
// TLS 不在这里:wss 由前面的边缘 nginx 终止(同文 W2)。
//
// 线程模型:「连接部分」的函数只能在连接所属的 loop 线程调用。每条 WebSocket 连接的状态存在线程局部表里,
// 换一个线程查到的是另一张空表,会把 WebSocket 连接当成原始 TCP、发出没有帧头的字节。
// gate / battle 的客户端连接都在节点主 loop 上(全仓没有 setThreadNum)。
//
// 生命周期(AGENTS.md §11.7):表以连接指针为键,表项只存 weak_ptr 用来核对身份,不持有连接;
// 连接断开时由连接回调调 Forget 删表项。漏调时 weak 核对保证地址被新连接复用也不会认错,只是多留一个表项。
namespace websocket
{

// 握手请求头的上限,超过仍没收全就拒绝。小游戏与 nginx 发来的升级请求不到 1KB。
inline constexpr std::size_t kMaxHandshakeBytes = 8 * 1024;

// 关闭码(RFC 6455 §7.4.1),只列本层会发的。
inline constexpr uint16_t kCloseProtocolError = 1002;   // 帧不合协议
inline constexpr uint16_t kCloseUnsupportedData = 1003; // 文本帧:客户端帧只走二进制消息
inline constexpr uint16_t kCloseMessageTooBig = 1009;   // 单帧载荷超过上限

// ── 纯函数:不碰连接,单测直接调 ──────────────────────────────────────

// 由请求的 Sec-WebSocket-Key 算应答的 Sec-WebSocket-Accept(RFC 6455 §4.2.2)。
std::string AcceptKey(std::string_view clientKey);

enum class Handshake : uint8_t
{
    kIncomplete, // 请求头还没收全,等更多字节
    kAccepted,   // 合法的升级请求
    kRejected,   // 不是合法的升级请求,或请求头超过上限
};

// 解析升级请求。data 是连接输入缓冲里的全部可读字节。
//   kAccepted:consumed = 请求头长度(含结尾空行),response = 101 应答。
//   kRejected:response = 要回给对端的 HTTP 错误应答(400;版本不是 13 时为 426)。
// 请求路径不校验:连接该去哪台节点由边缘按路径决定,到了这里已经是目标节点。
// 不查 Origin:身份靠帧内的验票,不靠 cookie。不协商扩展与子协议。
Handshake ParseHandshake(std::string_view data, std::size_t &consumed, std::string &response);

// 写出承载 payloadLen 字节的二进制消息帧头(FIN=1,服务端帧不加掩码),返回帧头长度(2、4 或 10)。
std::size_t EncodeBinaryHeader(std::size_t payloadLen, char out[10]);

// ── 连接部分:只能在连接所属的 loop 线程调用 ────────────────────────

// 入站分流,ProtobufCodec::onMessage 的第一步。返回要按客户端帧解析的缓冲:
//   原始 TCP 连接:原样返回 in;
//   WebSocket 连接:把 in 里完整的数据帧去掉帧头与掩码,追加进这条连接的载荷缓冲并返回它
//                  (分片消息、一条消息里装多个客户端帧都按字节流接上);
//   握手没收全、握手被拒、连接已在关闭:返回 nullptr。
// maxFramePayload 是单个数据帧的载荷上限,超过按 1009 关闭。
// 对端的 Close 帧与协议错误在这里只记下,由 FinishRead 执行:同一批里排在它前面的数据要先交给上层。
muduo::net::Buffer *Unwrap(const muduo::net::TcpConnectionPtr &conn, muduo::net::Buffer *in,
                           std::size_t maxFramePayload);

// 本批数据解析完之后调:给 Unwrap 记下要关的连接回 Close 帧并半关。顺序与 TCP「先处理数据、后处理 FIN」一致。
// 之后这条连接的输入一律丢弃;对端迟迟不关时由上层的握手期限与空闲超时收尾。
void FinishRead(const muduo::net::TcpConnectionPtr &conn);

// 这条连接是否已完成升级握手(之后发给它的客户端帧要先过 Frame)。
bool IsUpgraded(const muduo::net::TcpConnectionPtr &conn);

// 给一个已编码的客户端帧原地加上二进制消息帧头。
void Frame(muduo::net::Buffer *frame);

// 连接断开时调,删掉它的表项。不是 WebSocket 连接时是空操作。
void Forget(const muduo::net::TcpConnectionPtr &conn);

// 本线程的累计计数。低基数:不带连接或玩家维度。
struct Counters
{
    uint64_t accepted{0};       // 握手成功
    uint64_t rejected{0};       // 握手被拒
    uint64_t protocolClosed{0}; // 帧不合协议,由本端关闭
};
const Counters &GetCounters();

} // namespace websocket
