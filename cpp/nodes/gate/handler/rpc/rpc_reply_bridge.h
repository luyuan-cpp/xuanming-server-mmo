#pragma once

// 出站 gRPC 调用的结果怎样落到客户端 —— gate 唯一的一份事实源。
//
// 直连模式的每次调用回调(client_session_handler.cpp 的 ForwardDirectGrpcCallResult)与
// 路由模式手写的 Forward 回调(router_muduo_forward.cpp)都只调这里,所以两条出站路径对客户端的可见行为逐条相同,
// 路由 lane 的保序认领(client_request_lane.h,docs/design/client-request-ordering.md §4.5)也对两者同样生效。
// 第 3 批起两条路径底下都是 muduo_grpc 通道(docs/design/grpc-muduo-transport.md §8、§14)。
// 参见 docs/design/grpc-muduo-transport.md §16.5。
//
// 只在 gate 的 EventLoop 线程调用(会话表是 thread_local)。

#include <cstdint>
#include <string>

class SessionDetails;

namespace google::protobuf
{
class Message;
}

namespace gate_rpc_reply_bridge
{

// 直连模式发出时捕获的客户端信封关联号(ClientRequest.id 与 message_id)。回包与失败回执都按它回显,
// 客户端只按 MessageContent.id 认领回包、不再按 message_id 先进先出兜底(docs/design/client-rpc-callback.md §3)。
// 路由模式的调用由 lane 认领(lane 自己保存请求 id),传 nullptr。
struct DirectClientCall
{
    uint64_t requestId = 0;
    uint32_t messageId = 0;
};

// 成功桥。sessionDetails:路由模式是路由服在响应头里原值回显的会话;直连模式是发出时捕获的那份
// (ForwardDirectGrpcCallResult)。本函数只用 session_id 与 gate_call_seq。
//   1. 路由 lane 的调用(gate_call_seq != 0)交给 lane 认领:核对代次与队头、恰好一次下发、放行下一条;
//   2. 否则按 session_id 找会话:MessageContent 原样下发,其它响应类型按 gRpcMethodRegistry 反查 message_id 包一层,
//      directCall 非空时回显它的请求 id;会话已不在、连接已释放就静默丢弃(断线通知的回包本来就没有会话可回)。
void DeliverReply(const SessionDetails &sessionDetails, const ::google::protobuf::Message &reply,
                  const DirectClientCall *directCall);

// 失败桥:调用失败 = 结果未知(下游可能已执行)。rawSessionDetails 是**发出时**的 SessionDetails 原始字节
// (未 base64);为空指针表示这次调用没带会话 metadata,只记 ERROR。
//   1. 失败日志按 1024 条采样(下游整体不可用时失败频率 = 在线玩家的 gRPC 消息频率);
//   2. 路由 lane 的调用交给 lane 认领(回带请求 id 的 1003,放行下一条);
//   3. 否则找会话:directCall 非空时回带请求 id 与原 message_id 的信封 1003(与 lane 同形);
//      为空(路由服回显缺 gate_call_seq 的版本不符)时仍推 23 号 tip。不按状态码区分 tip,不重试。
void NotifyCallFailure(const char *method, int statusCode, const std::string &statusMessage,
                       const std::string *rawSessionDetails, const DirectClientCall *directCall);

} // namespace gate_rpc_reply_bridge
