#pragma once

#include <cstdint>
#include <functional>
#include <memory>
#include <optional>
#include <string>
#include <string_view>

#include "proto/common/base/common.pb.h" // NodeInfo / EndpointComp(无 package,全局命名空间)

// 客户端可达地址(advertised address)—— 集群外入口 D76–D79 在 C++ 侧的唯一权威实现。
//
// NodeInfo 有两个地址,语义不同,不能混用:
//   - endpoint:集群内身份。gRPC 绑定、端口 CAS、节点间对端校验、FindNodeByPodIP 只认它。
//   - client_endpoint(字段 11):客户端可达地址,语义同 Kafka advertised.listeners。
//     只由客户端面节点(gate / battle)在 etcd 分配与发布**之前**自报;podip 模式下不填。
//
// 本模块分两半:
//   生产方(Node::InitRpcServer 调用):ParseSettings 读 env → Resolve 得出要自报的地址。
//     任何一步失败都由调用方 fail-closed(LOG_FATAL 非 0 退出,由 K8s / Agones 重建),
//     绝不带着缺失或错误的地址发布到 etcd。
//   消费方(battle BuildAssignment 等"把节点地址下发给客户端"的出口):ClientFacing 选址。
//     Go 侧同一条规则是 go/shared/clientendpoint.Select,两边判定必须一致。
//
// 除 SetExternalSourceFactory / GetExternalSourceFactory 读写的进程级工厂外,全部是纯函数:
// 不打日志、不读真实 env(env 经 EnvLookup 注入)、不做 I/O(I/O 在 ExternalSource 实现里)。
namespace client_endpoint {

// 容器 env 名(D92:只用 CLIENT_ENDPOINT_*;严禁复用 NODE_IP / NODE_PORT,
// 它们是 ResolveNodeIp / TryResolveNodePortFromEnv 的集群内身份输入)。
inline constexpr const char kEnvSource[] = "CLIENT_ENDPOINT_SOURCE";
inline constexpr const char kEnvHost[] = "CLIENT_ENDPOINT_HOST";
inline constexpr const char kEnvPort[] = "CLIENT_ENDPOINT_PORT";
inline constexpr const char kEnvRequired[] = "CLIENT_ENDPOINT_REQUIRED";

// 地址来源。env 取值见 SourceKindName(none / static / agones),缺省为 none。
enum class SourceKind : uint8_t { kNone, kStatic, kAgones, kCount };

// env 解析结果。只有 ParseSettings 成功时的组合才是合法的:
//   kNone   :host / port 恒为空 / 0(env 里即便带了 HOST / PORT 也被忽略);required 恒为 false。
//   kStatic :host 为合法裸主机名,port 在 1..65535。
//   kAgones :host 为空 = 用 Agones status.address;非空 = 覆盖(kind 填 127.0.0.1);port 恒为 0。
struct Settings {
    SourceKind source = SourceKind::kNone;
    std::string host;
    uint32_t port = 0;
    // 消费方纵深防御开关(D79):true 时本节点下发给客户端的地址不许回落 endpoint。
    bool required = false;
};

// 按名字读 env;未设置返回 nullptr。生产用 std::getenv,测试注入假表。
using EnvLookup = std::function<const char *(const char *)>;

// 解析并校验 CLIENT_ENDPOINT_* env。成功返回 true 并整体写 out;
// 失败返回 false、error 写明哪个 env 为什么非法,out 保持不变。
// 空串与未设置同义。以下一律判为非法(调用方据此致命退出):
//   SOURCE 不是 none / static / agones(区分大小写);
//   REQUIRED 不是 0 / 1;none 配 REQUIRED=1(矛盾配置);
//   static 缺 HOST 或 PORT;PORT 不是 1..65535 的纯十进制;
//   agones 设置了 PORT(端口只能来自 Agones 分配,设了就有歧义);
//   HOST 带 scheme / 端口 / 路径 / 空白(只接受 IPv4 字面量或 DNS 名)。
bool ParseSettings(const EnvLookup &env, Settings &out, std::string &error);

// 外部地址来源(Agones sidecar 等)取回的原始地址。
struct Advertised {
    std::string host;
    uint32_t port = 0;
};

// 外部地址来源。实现方在 Fetch 内部自带有界重试(次数 + 退避上限),
// 调用方只调用一次、不再外层重试;Fetch 在调用线程上同步阻塞直到成功或重试耗尽。
// 失败时返回 false 并在 error 写明最后一次失败原因。
class ExternalSource {
public:
    virtual ~ExternalSource() = default;
    virtual bool Fetch(Advertised &out, std::string &error) = 0;
};

// 构造外部地址来源的工厂。返回 nullptr 表示本进程/本构建不支持(调用方据此致命退出)。
using ExternalSourceFactory = std::unique_ptr<ExternalSource> (*)();

// 注册进程级外部来源工厂。只由 node_entry.h 的预构造钩子(THooks::ClientEndpointSourceFactory)
// 在 Node 构造之前调用;传 nullptr 等于注销。线程模型:启动主线程写一次,之后只读。
void SetExternalSourceFactory(ExternalSourceFactory f);

// 取已注册的工厂;未注册返回 nullptr。只由 Node 在 CLIENT_ENDPOINT_SOURCE=agones 时调用。
ExternalSourceFactory GetExternalSourceFactory();

// 由 Settings 得出本节点要自报的 client_endpoint。
//   kNone  :返回 true,out = nullopt(podip 模式,客户端直接用 endpoint)。
//   kStatic:返回 true,out = {host, port}。
//   kAgones:调 external->Fetch 一次;Settings.host 非空时覆盖取回的 host,端口永远取 Agones 的。
// 失败(external 为空、Fetch 失败、结果不可用、Settings 非法)返回 false 并写 error,out = nullopt。
bool Resolve(const Settings &settings, ExternalSource *external, std::optional<EndpointComp> &out, std::string &error);

// 地址可用 = ip 非空且 port 在 1..65535。半填(只有 ip 或只有 port)视为缺失。
bool IsUsable(const EndpointComp &endpoint);

// 选出下发给客户端的地址(D78):
//   1) client_endpoint 可用 → 用它;
//   2) required=true → nullopt(不回落,调用方必须跳过该节点或拒签);
//   3) 否则回落 endpoint;endpoint 本身不可用同样 nullopt。
// 与 go/shared/clientendpoint.Select 同一口径;无 I/O、无副作用,可在热路径调用。
// 线程约束落在实参上:NodeInfo 由 Node 持有、只在 loop 线程写;跨线程读取不与写入同步,
// 所以 node 仍须取自 loop 线程上的 gNode->GetNodeInfo()。required 取 gNode->ClientEndpointRequired()
// (普通成员,可跨线程读)。battle 只能在 loop 线程上的 BuildAssignment / 预签里调用本函数
// (第 4 批起 gRPC 服务端本身就在 loop 上派发,docs/design/grpc-muduo-transport.md §15)。
std::optional<EndpointComp> ClientFacing(const NodeInfo &node, bool required);

// 来源的 env 取值(none / static / agones),供日志使用;越界返回 "unknown"。
std::string_view SourceKindName(SourceKind kind);

// ── 小游戏客户端的 WebSocket 地址(NodeInfo.client_ws_url,docs/design/minigame-websocket-gate.md W7)──
//
// 小游戏平台只许连事先登记的 wss 域名,所以它们经一个固定域名的边缘转进来:
// client_ws_url = <基地址>/<endpoint.ip>/<endpoint.port>,边缘按路径里的地址连到本节点。
// 用 endpoint(集群内地址)而不是 client_endpoint:边缘在集群内,直连 Pod。
// 基地址由部署经 CLIENT_WS_BASE_URL 注入,只给 gate / battle;不设 = 本节点不接小游戏。
inline constexpr const char kEnvWsBaseUrl[] = "CLIENT_WS_BASE_URL";

// 解析并校验 CLIENT_WS_BASE_URL。未设置或空串:返回 true,out 为空。
// 合法值以 wss:// 或 ws:// 开头,后面是主机[:端口][/路径],末尾的 '/' 去掉后写入 out。
// 带查询串、片段、空白、未展开的 $(VAR) 或模板占位符的返回 false 并写 error(调用方据此致命退出)。
bool ParseWsBaseUrl(const EnvLookup &env, std::string &out, std::string &error);

// 是否是 IPv4 字面量(四段 0..255 的十进制)。边缘只按 IPv4 地址转发。
bool IsIpv4Literal(std::string_view text);

// 拼出 client_ws_url。base 为空、ip 不是 IPv4 字面量或 port 不在 1..65535 时返回空串(不自报)。
std::string BuildWsUrl(std::string_view base, const EndpointComp &endpoint);

} // namespace client_endpoint
