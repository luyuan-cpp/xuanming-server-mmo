#pragma once

// gate 路由 lane 的装配层(docs/design/client-request-ordering.md §4)。
//
// 职责:同一会话经路由服转发的 gRPC 类请求,按收包顺序单飞。上一条观察到完成之后才发下一条;
// 完成有三个来源:成功回包 / gRPC 失败回调 / 本地截止到期。路由服节点摘除不再是单独的来源:node.cpp 先摘
// MuduoGrpcChannelPtr,~Channel 把在途调用以 UNAVAILABLE 排进下一轮,走的就是失败回调。不同会话之间互不影响,
// 路由服与业务服保持无状态、随机分配(client-rpc-router.md D29 不变)。
//
// 只在路由模式(GATE_CLIENT_RPC_ROUTER=1)、只对 PROTOCOL_GRPC 消息启用(O2):
//   - scene 的 TCP 路径本身单连接 FIFO,已经有序;Empty 返回的移动消息不回包,排队会卡死;
//   - 直连模式下 match 不回写会话头,认领不了(O7,等 D61 删除直连路径);
//   - gate 合成的断线通知不进 lane(O8):它带 gate_call_seq = 0、但必须带 session_incarnation,
//     由 login 会话栅栏兜底(代次为 0 时栅栏的 Close 会被跳过)。
//
// 纯逻辑(排队、单飞、认领恰好一次、过期堆)在 services/gate/session/comp/router_lane.h,本文件只做装配与副作用
// (发出、回执、计数、日志)。全部在 gate 唯一的 EventLoop 线程上调用,不加锁,不新增线程。
// 生命周期(AGENTS.md §11.7):排队项、在途索引、定时器回调都不持有 TcpConnectionPtr 或裸 this;
// 每次下发都先按 session_id 找会话、核对会话代次,再 conn.lock()。

#include <chrono>
#include <cstdint>
#include <memory>
#include <string>

#include "muduo/net/TcpConnection.h"
#include "engine/core/type_define/type_define.h"

class ClientRequest;
class SessionDetails;
struct SessionInfo;

namespace google::protobuf
{
class Message;
}

namespace gate_lane
{

// 每会话排队 + 在途的总上限。超限回带请求 id 的 1008(kRateLimitExceeded),不计非法包(O6)。
// 与 client-access-band-routing.md D57 的「每会话在途上限」合并为同一套记账。
inline constexpr std::size_t kMaxLaneDepth = 32;
// 排队等待上限:超过就不发出,回带请求 id 的 1003,明确「没有执行」,客户端重试安全。
// 不变式 I4:gate deadline(路由服,8000)+ 本值 + 1000 ≤ 客户端超时 15000(client-request-ordering.md §5.2)。
inline constexpr std::chrono::milliseconds kMaxLaneWait{5000};
// 在途调用的本地截止 = 发出时刻 + 实际生效的 gRPC deadline + 本余量。余量覆盖 loop 积压:
// 正常情况下通道先以 DEADLINE_EXCEEDED / UNAVAILABLE 回调失败,本地截止只是纵深防御,兜「完成没被认领」:
// 传输层回调丢失(第 3 批前的两个来源 —— 节点摘除 F29、CQ 轮询 !ok —— 已随官方 CQ 删除),或路由服回显缺失 /
// 解析失败 / 不带 gate_call_seq(见 router_lane.h 过期堆一段)。
inline constexpr std::chrono::milliseconds kInflightExpiryMargin{1000};
// 主动过期扫描的间隔(秒)。必须主动扫:队头卡住时客户端在等回包,不会再发新请求触发惰性清理。
inline constexpr double kSweepIntervalSeconds = 0.25;

// 会话代次发号:建连时调用,进程内单调,从 1 开始。与 gate_instance_id 合起来全局唯一。
uint64_t NextIncarnation();

// 带请求 id 与原消息号的错误信封(与路由服的拒绝信封同形,客户端按 id 对上请求)。连接已释放就不发。
// 路由 lane 与 scene 的 TCP(RPC0)回包路径共用(docs/design/muduo-rpc-channel.md §8.1)。
// 只能在 gate 的 EventLoop 线程上调用(会话表是 thread_local)。
void SendEnvelope(SessionInfo &session, uint64_t clientRequestId, uint32_t messageId, uint32_t tipId);

// 按 session_id 找会话并核对会话代次:session_id 被新连接复用之后,旧会话迟到的完成不得落到新会话上(F47)。
// 代次不符计入 [lane] 汇总里的 incarnation_mismatch —— 该计数同时包含路由 lane 与 TCP(RPC0)回包两条路径,
// 不再只是 lane 的。只能在 gate 的 EventLoop 线程上调用。
SessionInfo *FindLiveSession(SessionId sessionId, uint64_t incarnation);

// 发往路由服的发送函数,由 client_session_handler.cpp 在启动时注入(它持有挑路由服副本与生成 sender 的细节)。
// sessionDetailsBin 是序列化好的 SessionDetails,原样作为 x-session-detail-bin。
// 返回是否已发出;本地还没发现任何可用的路由服时返回 false(lane 据此回 1003)。
using RouterSendFn = bool (*)(const ClientRequest &request, const std::string &sessionDetailsBin);
void SetRouterSender(RouterSendFn fn);

// 准入。前置条件:会话存在且已验证;白名单 / 体积 / 限流 / GM / battle 几道闸都已通过;协议是 PROTOCOL_GRPC;路由模式。
// details 是调用方按当前会话构造的身份(BuildSessionDetails,player_id 已在那里归一);本函数复用同一规则
// (session_details_builder.h 的 NormalizePlayerId,幂等)再归一一次作纵深防御,写入会话代次与调用序号后,
// 作为入队时刻的快照保存。拒绝(lane 满 / 已重定向)时直接回带请求 id 的信封。
void Admit(SessionId sessionId, SessionInfo &session, const std::shared_ptr<ClientRequest> &request,
		   SessionDetails details);

// 成功桥:echoed 是路由服在响应头里原值回写的 SessionDetails。gate_call_seq == 0 不归本模块管,返回 false,
// 调用方按旧行为处理;否则由本模块认领:认领成功就下发 reply 并发同一会话的下一条,认领失败(已过期 / 已失败 /
// 会话已换代)就丢弃,保证每个请求恰好一个回执。返回 true 表示本模块已处理完。
bool OnRouterReply(const SessionDetails &echoed, const ::google::protobuf::Message &reply);

// 失败桥:sent 是本次发出时的 SessionDetails。语义同 OnRouterReply;认领成功时回带请求 id 的 1003
// (结果未知:下游可能已经执行,有副作用的请求靠业务侧栅栏与幂等兜底)。
// 路由服节点摘除也走这里:node.cpp 先 remove<MuduoGrpcChannelPtr>(~Channel 把在途调用以 UNAVAILABLE 排进下一轮),
// 再触发 OnNodeRemoveEvent、DestroyEntity;下一轮失败回调到达时被摘实体已销毁,放行下一条时 PickRandomNode
// 要求实体挂着通道,不会再选中它。
bool OnRouterCallFailed(const SessionDetails &sent);

// 主动过期扫描(GateRuntimeContext 上的 TimerTaskComp 驱动,回调不捕获对象)。顺带每 30s 打一行 [lane] 汇总。
void ExpireDue(std::chrono::steady_clock::time_point now);

// 会话即将 erase(断线 / 顶号走同一路径):注销在途调用,丢弃排队项。晚到的完成一律丢弃,不等在途调用。
void OnSessionClosing(SessionInfo &session);

// 会话被重定向到别的 gate:丢弃排队项,之后的准入回 1003;在途那一条的回包照常下发(O9,与 Java 版一致)。
void OnSessionRedirected(SessionInfo &session);

} // namespace gate_lane
