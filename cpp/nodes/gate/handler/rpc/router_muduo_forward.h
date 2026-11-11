#pragma once

// gate → 路由服的 ClientRpcRouter.Forward(路由模式唯一的 gRPC 出站,docs/design/grpc-muduo-transport.md §8、§14)。
//
// 第 3 批起生成客户端也走 muduo_grpc;这条手写 Forward 是第 1 批落地的路径,第 3 批原样保留,没有改用生成的
// SendClientRpcRouterForward(…, GrpcDone, GrpcCallOptions)。它在自己的完成回调里直接进桥。
// 线上字节(:path、metadata 两层 base64、截止取值)与生成客户端一致,改其中任一项须两处同改。对客户端的可见行为
// 与直连模式的生成客户端路径逐条相同:两条路径都只调 gate_rpc_reply_bridge(含路由 lane 的认领)。
//   成功 → 按响应初始 metadata 里回显的 x-session-detail-bin 解出会话,交成功桥;
//          直连模式按发出时的会话,DeliverReply 只用 session_id 与 gate_call_seq,两者效果相同。
//   失败 → 按发出的 SessionDetails 原始字节交失败桥;不重试。
//   没构造出来(签名失败等)→ 不进任何桥,SendForward 返回 false,由调用方当场回执。
// 通道由 NodeConnector::ConnectToGrpcNode 统一建在路由服节点实体上(MuduoGrpcChannelPtr),这里不建通道。

#include <functional>
#include <string>

class MessageContent;

namespace muduo_grpc
{
class Channel;
} // namespace muduo_grpc

namespace client_rpc_router
{
class ForwardRequest;
} // namespace client_rpc_router

namespace gate_router_muduo
{

// 经 muduo_grpc 通道发一次 ClientRpcRouter.Forward。只能在 gate 的 EventLoop 线程调用。
// sessionDetailsBin 是序列化好的 SessionDetails(含路由 lane 的 gate_call_seq 等字段),原样作为
// x-session-detail-bin 的内容 —— 不解析、不重新序列化,路由服原值回写,lane 靠它认领回包。
// 返回 true = 已交给通道,之后至多一次进成功桥或失败桥(都在之后的回调里,不会在本函数里同步发生);成功回包缺
// x-session-detail-bin 回显或回显解析不出时两个桥都不进、回包丢弃,路由 lane 由本地截止兜底(client-request-ordering.md §4.5);
// 返回 false = 调用没构造出来(消息号越界、没有 gRPC 方法名、调用方签名失败),什么都没发、不会有任何回调,
// 只按 1024 采样留一行 ERROR,回执由调用方负责(client_request_lane.h RouterSendResult::kCallNotBuilt)。
bool SendForward(muduo_grpc::Channel &channel, const client_rpc_router::ForwardRequest &request,
                 const std::string &sessionDetailsBin);

// gate 自己要处理应答的转发(排空迁移的 ClientPlayerLogin.RequestGateMigration,docs/design/gate-drain-session-migration.md §4.3)。
// 与 SendForward 同一份请求构造(:path、两层 base64 的会话 metadata、调用方签名、截止),区别只在完成之后:
// 不进回包桥、不推给客户端,把路由服回的 MessageContent 交给 done;调用失败、应答解析失败或签名失败时交 nullptr。
// done 恰好被调用一次,在 gate 的 EventLoop 线程上;签名失败时同步调用。
void SendForwardForInternalReply(muduo_grpc::Channel &channel, const client_rpc_router::ForwardRequest &request,
                                 const std::string &sessionDetailsBin,
                                 std::function<void(const MessageContent *reply)> done);

} // namespace gate_router_muduo
