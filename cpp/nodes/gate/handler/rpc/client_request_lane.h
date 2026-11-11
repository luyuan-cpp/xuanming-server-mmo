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
#include <cstddef>
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

// 发送函数的结果。只有 kSent 之后才会有完成(成功桥 / 失败桥 / 本地截止,lane 认领恰好一次);另两种都是「确定没发出」,
// lane 当场回带请求 id 的 1003 并继续取下一条,不登记在途(client-request-ordering.md §4.4)。
enum class RouterSendResult : uint8_t
{
	kSent,		   // 已交给路由服的通道
	kNoRouter,	   // 本地还没有可用的路由服实例,或实例缺通道(启动窗口 / 全部下线)
	kCallNotBuilt, // 调用没构造出来(调用方签名失败、消息号越界、没有 gRPC 方法名),通道没被碰过
};

// 发往路由服的发送函数,由 client_session_handler.cpp 在启动时注入(它持有挑路由服副本与构造 Forward 的细节)。
// sessionDetailsBin 是序列化好的 SessionDetails,原样作为 x-session-detail-bin。
// 契约:返回 kSent 之前不得同步调用成功桥 / 失败桥 —— 此刻序号还没进在途索引,认领会落空、队头永远等不到完成;
// 构造失败只能以 kCallNotBuilt 返回(2026-11-10 修正,原先签名失败同步进失败桥、仍报「已发出」,同会话下一条要卡约 9 s)。
using RouterSendFn = RouterSendResult (*)(const ClientRequest &request, const std::string &sessionDetailsBin);
void SetRouterSender(RouterSendFn fn);

// 发出钩子(client-request-ordering.md §4.4、§4.6):LeaveGame / EnterGame 的会话状态变化要按收包顺序、在请求真正发出的那一刻生效,
// lane 不认识这些方法,由 client_session_handler.cpp 在启动时注入。两个钩子都只在 Pump 里、按收包顺序对每条调用至多一次:
//   - BeforeSendFn:排队超时、登录闸、身份复核都通过之后,交给发送函数之前。返回 0 = 照常发出;非 0 = 不发出,
//     回带请求 id 的该码信封(确定未执行),计 rejected_before_send。只读会话,不得有副作用。
//   - AfterSendFn:发送函数返回 kSent、登记在途之后。没发出的请求(排队超时、被拒、kNoRouter、kCallNotBuilt)一律不调,
//     所以「确定未执行」的回执与会话状态一致;发出之后失败回的 1003 表示结果未知,钩子做过的变化随之保留。
// 钩子里不得 erase 会话、不得重入 lane。未注入时按「放行、无副作用」处理。
using BeforeSendFn = uint32_t (*)(SessionId sessionId, const SessionInfo &session, const ClientRequest &request);
using AfterSendFn = void (*)(SessionId sessionId, SessionInfo &session, const ClientRequest &request);
void SetSendHooks(BeforeSendFn beforeSend, AfterSendFn afterSend);

// 会话的 lane 里有没有收到了、还没发出的 messageId 号请求(在途那一条不算)。客户端入口据此判断 LeaveGame 屏障
// (client_session_handler.cpp)。只能在 gate 的 EventLoop 线程上调用。
bool HasQueuedRequest(const SessionInfo &session, uint32_t messageId);

// 准入。前置条件:会话存在且已验证;白名单 / 体积 / 限流 / GM / battle 几道闸都已通过;协议是 PROTOCOL_GRPC;路由模式。
// details 是调用方按当前会话构造的身份(BuildSessionDetails,player_id 已在那里归一);本函数复用同一规则
// (session_details_builder.h 的 NormalizePlayerId,幂等)再归一一次作纵深防御,写入会话代次与调用序号后,
// 作为入队时刻的快照保存。前面还排着没发出的 LeaveGame 时,快照不带原账号与原修订(见 client_request_lane.cpp Admit)。
// 拒绝(lane 满 / 已重定向)时直接回带请求 id 的信封。
void Admit(SessionId sessionId, SessionInfo &session, const std::shared_ptr<ClientRequest> &request,
		   SessionDetails details);

// 成功桥:echoed 是路由服在响应头里原值回写的 SessionDetails。gate_call_seq == 0 不归本模块管,返回 false,
// 调用方按旧行为处理;否则由本模块认领:认领成功就下发 reply 并发同一会话的下一条,认领失败(已过期 / 已失败 /
// 会话已换代)就丢弃,保证每个请求恰好一个回执。返回 true 表示本模块已处理完。
bool OnRouterReply(const SessionDetails &echoed, const ::google::protobuf::Message &reply);

// 失败桥:sent 是本次发出时的 SessionDetails。语义同 OnRouterReply;认领成功时回带请求 id 的 1003
// (结果未知:下游可能已经执行,有副作用的请求靠业务侧栅栏与幂等兜底)。
// 路由服节点摘除也走这里:node.cpp 先 remove<MuduoGrpcChannelPtr>(~Channel 把在途调用以 UNAVAILABLE 排进下一轮),
// 再触发 OnNodeRemoveEvent、DestroyEntity;下一轮失败回调到达时被摘实体已销毁,放行下一条时 PickRouter
// 要求实体挂着通道,不会再选中它。
bool OnRouterCallFailed(const SessionDetails &sent);

// 主动过期扫描(GateRuntimeContext 上的 TimerTaskComp 驱动,回调不捕获对象)。顺带每 30s 打一行 [lane] 汇总。
void ExpireDue(std::chrono::steady_clock::time_point now);

// 会话即将 erase(断线 / 顶号走同一路径):注销在途调用,丢弃排队项。晚到的完成一律丢弃,不等在途调用。
void OnSessionClosing(SessionInfo &session);

// 会话被重定向到别的 gate:排队项不再发出,逐条回带请求 id 与原 message_id 的 1003(确定未执行,到新 gate 重试安全;
// O4 每个请求恰好一个回执),计 dropped_on_redirect;之后的准入回 1003;在途那一条的回包照常下发(O9,与 Java 版一致)。
// 连接已释放时信封不发(SendEnvelope),计数照记。
void OnSessionRedirected(SessionInfo &session);

// 只用于每 30s 一行的 [lane] 汇总日志(gate 没有 Prometheus 出口)。不按会话、玩家或消息号分维度(AGENTS.md §9)。
// 一条被受理(admitted)的请求最终恰好落进下面一项:sent / expired_in_queue / rejected_not_logged_in /
// rejected_stale_identity / rejected_before_send / no_router / call_not_built / dropped_on_close / dropped_on_redirect,
// 或仍在排队;sent 之后再恰好落进 replied / failed / expired 之一,或随断线注销(late_dropped 计其迟到的完成)。
struct LaneCounters
{
	uint64_t admitted{0};
	uint64_t sent{0};
	uint64_t replied{0};
	uint64_t failed{0}; // 含路由服节点摘除:~Channel 以 UNAVAILABLE 完成在途调用,走同一失败回调
	uint64_t expired{0};
	uint64_t expiredInQueue{0};
	uint64_t noRouter{0};
	uint64_t callNotBuilt{0}; // 发送函数返回 kCallNotBuilt(签名失败等),当场回 1003,不登记在途
	uint64_t rejectedFull{0};
	uint64_t rejectedRedirected{0};
	uint64_t lateDropped{0};
	uint64_t incarnationMismatch{0};
	uint64_t droppedOnClose{0};
	uint64_t droppedOnRedirect{0};
	uint64_t rejectedNotLoggedIn{0};	 // 本连接没有成功 Login 就轮到发 CreatePlayer / EnterGame / LeaveGame(login_admission.h)
	uint64_t rejectedStaleIdentity{0}; // 发出前身份复核失配(离场 / 换角 / 换连接之后,MatchesAuthenticatedBinding),回 kLoginSessionNotFound
	uint64_t rejectedBeforeSend{0};	 // BeforeSendFn 拒绝(EnterGame 换角闸等)
	std::size_t maxDepthSeen{0};
};

// 当前线程(gate 的 EventLoop)的累计计数。单测据此核对分支。
const LaneCounters &GetCounters();

// 只给单测用:清空在途索引与计数,摘掉注入的发送函数与发出钩子。调用序号与会话代次的发号器不回退。
void ResetForTesting();

} // namespace gate_lane
