#pragma once

// RPC 热关停(docs/design/rpc-kill-switch.md):运维往 etcd /mmorpg/killswitch/ 写一个键,就能把某一类节点、某一个节点进程、
// 某个服务或某个方法上的请求立刻短路掉,不改代码、不重启。键格式、匹配顺序与豁免名单都在设计文档 §4、§5、§8,
// 与 Go 的 go/shared/killswitch 同一套规则。
//
// 本模块只做判定:规则值 / 键解析、匹配顺序、进程级快照、豁免名单与指标。etcd 同步在
// node/system/etcd/kill_switch_etcd_sync.h,它把 Range / watch 的结果交给 KillSwitch::Process()。
//
// 铁律 fail-open:没有规则、规则写坏、从没同步成功、与 etcd 失联超过 StaleAfter,一律放行。
// 它是止血阀,不能拿来判定权限、资格或活动开放。
//
// 线程:全部公开成员可以在任意线程调用(内部一把互斥锁;C++ 节点实际只在节点 loop 线程用)。
// 读路径:没有适用规则时只读一个原子量,不加锁、不分配。

#include <atomic>
#include <chrono>
#include <cstdint>
#include <functional>
#include <map>
#include <memory>
#include <mutex>
#include <optional>
#include <set>
#include <string>
#include <string_view>
#include <unordered_map>
#include <vector>

namespace kill_switch
{
// etcd 前缀,与 Go shared/killswitch.DefaultPrefix 同值。
inline constexpr std::string_view kDefaultPrefix = "/mmorpg/killswitch/";
// 与 etcd 失联多久之后本地快照作废、整体放行(与 Go 缺省同值,设计文档 §9)。
inline constexpr std::chrono::seconds kDefaultStaleAfter{60};
// gRPC UNAVAILABLE。规则没写 code、或 code 越界时用它。
inline constexpr uint32_t kGrpcUnavailable = 14;

// 一条规则 = etcd 一个键的值。
struct Rule
{
    bool deny{false};
    std::string reason;  // 运维填的原因,回给调用方并进日志
    uint32_t code{0};    // gRPC 状态码;0 或超出 1..16 一律按 UNAVAILABLE

    uint32_t GrpcStatusCode() const;
    bool operator==(const Rule &) const = default;
};

// 规则值解析,与 Go killswitch.ParseRule 同口径:
//   空值 → 放行;JSON {"deny":true,"reason":"...","code":9}(字段类型不对算写坏,别的键忽略);
//   裸值 true / 1 / on / yes / deny 关停,false / 0 / off / no / allow 放行(大小写不敏感)。
// nullopt = 值写坏,调用方把这个键当作不存在(fail-open)。
std::optional<Rule> ParseRule(std::string_view raw);

// 规则的作用范围(设计文档 §4)。
enum class Scope : uint8_t
{
    kAll,       // /mmorpg/killswitch/<目标>
    kNodeType,  // /mmorpg/killswitch/type/<eNodeType 名>/<目标>
    kInstance,  // /mmorpg/killswitch/instance/<节点 uuid>/<目标>
    kCount
};

struct RuleKey
{
    Scope scope{Scope::kAll};
    std::string scopeValue;  // 节点类型名 / 节点 uuid;kAll 时为空
    std::string target;      // "服务/方法" | "服务/*" | "*"
};

// 去掉前缀之后的相对键 → 范围与目标。开头多敲的斜杠容忍。
// nullopt = 不是规则键:空,或范围段残缺(type/、type/X、instance//*)。
std::optional<RuleKey> ParseRuleKey(std::string_view relativeKey);

// 一个方法:服务名(gRPC 服务带包名的全名;RPC0 服务没有包名)与方法名。只借用调用方的字符串。
struct MethodName
{
    std::string_view service;
    std::string_view method;
};

// "/pkg.Svc/Method" 或 "pkg.Svc/Method" → MethodName。不是「服务/方法」形状返回 nullopt。
std::optional<MethodName> ParseFullMethod(std::string_view fullMethod);

// 一个方法受哪些规则管(设计文档 §8)。
enum class Coverage : uint8_t
{
    kAllRules,       // 普通方法:精确规则与通配规则(服务/*、*)都认
    kExactRulesOnly, // 受保护的方法(握手 / 停机 / 收尾 / 补偿):只认精确到这个方法的规则,通配关不掉它
};

// 同一范围内的候选目标,从精确到宽泛:包.服务/方法 → 服务/方法 → 包.服务/* → 服务/* → *。
// 服务名没有包名时少掉重复的两项。与 Go MatchKeys 同序。kExactRulesOnly 只给前两项。
std::vector<std::string> MatchTargets(const MethodName &method, Coverage coverage = Coverage::kAllRules);

// 本进程的身份:以哪些节点类型、哪些 uuid 注册。C++ 节点各只有一个;一个进程注册多种类型时(Go match)各算一份。
struct Identity
{
    std::vector<std::string> nodeTypes;  // eNodeType 枚举名,如 "SceneNodeService"
    std::vector<std::string> instances;  // NodeInfo.node_uuid
};

class KillSwitch
{
public:
    using Clock = std::function<std::chrono::steady_clock::time_point()>;

    // clock 留空用 steady_clock::now;单测注入假时钟。
    explicit KillSwitch(std::chrono::seconds staleAfter = kDefaultStaleAfter, Clock clock = {});
    KillSwitch(const KillSwitch &) = delete;
    KillSwitch &operator=(const KillSwitch &) = delete;

    // 本进程身份。C++ 节点在 InitRpcServer 定下节点类型与 uuid 之后调一次。没设之前只认全部范围的规则。
    void SetIdentity(Identity identity);

    // 全量替换(etcd 全量 Range 成功时)。键是去掉前缀的相对键,值是 etcd 里的原样字节。同时刷新新鲜度。
    void ReplaceAll(std::map<std::string, std::string> rawRules);
    // 增量(watch 事件)。同时刷新新鲜度。值写坏的键按不存在处理,与全量替换同一口径。
    void Put(const std::string &relativeKey, const std::string &rawValue);
    void Erase(const std::string &relativeKey);
    // 只刷新新鲜度:真正读到了 etcd,但规则没有变化。
    void MarkSynced();

    // 判定(纯函数,不计数)。返回命中的关停规则;nullopt = 放行。
    std::optional<Rule> Check(const MethodName &method, Coverage coverage = Coverage::kAllRules) const;

    // 本进程适用的有效规则条数(含 deny=false 的豁免规则)。快照作废时为 0。
    size_t ApplicableRuleCount() const;
    // 手上有未过期的快照。
    bool Synced() const;

    // 被关停的请求计数:method 用「短服务名/方法」,取值有界(注册表里的方法)。
    void RecordBlocked(const MethodName &method);
    std::map<std::string, uint64_t> BlockedCounts() const;

    // 进程唯一实例(函数内静态):节点的各派发入口与 etcd 同步共用。单测另建实例。
    static KillSwitch &Process();

private:
    // 适用于本进程的规则,按优先级分三层:实例 → 类型 → 全部。同一层里本进程有多个身份时按目标合并,冲突取关停。
    struct Snapshot
    {
        std::unordered_map<std::string, Rule> instanceRules;
        std::unordered_map<std::string, Rule> typeRules;
        std::unordered_map<std::string, Rule> allRules;
        // 相对键 → 规则,只用于比较前后两份快照、打变更日志。
        std::map<std::string, Rule> byKey;
    };

    std::chrono::steady_clock::time_point Now() const;
    bool StaleLocked(std::chrono::steady_clock::time_point now) const;
    // 用 raw_ 与 identity_ 重建快照,并打变更日志。调用方持锁。
    void RebuildLocked();

    const std::chrono::seconds staleAfter_;
    const Clock clock_;

    mutable std::mutex mutex_;
    Identity identity_;
    std::map<std::string, std::string> raw_;  // 前缀下的全部键(含不适用本进程的),相对键 → 原样值
    std::set<std::string> badKeys_;           // 值写坏的键:只在集合变化时打日志
    std::shared_ptr<const Snapshot> snapshot_;
    std::chrono::steady_clock::time_point syncedAt_{};  // 零值 = 从没同步过
    std::map<std::string, uint64_t> blocked_;
    // 快照里有适用规则:读路径的快速判断(假 = 直接放行,不加锁)。
    std::atomic<bool> hasRules_{false};
};

// ── 节点内各派发入口用(进程级实例 KillSwitch::Process()) ──

// 受保护的方法(设计文档 §8):连接握手、停机、会话收尾、在途工作的补偿与回执,以及 scene 的转发信封
// ProcessClientPlayerMessage(内层由 scene 按真实方法判)。它们只认精确到方法的规则,`*` 与 `服务/*` 关不掉 ——
// 热关停只挡新工作,在途的工作必须收得了尾(玩家下线能存盘、冻结解得开、预留退得回、节点停得了机)。
// 名单只在 kill_switch.cpp 一处。
bool IsProtectedMessageId(uint32_t messageId);
// 同一份名单按 gRPC 路径("/pkg.Svc/Method")查,给 muduo gRPC 服务端用。
bool IsProtectedGrpcMethod(std::string_view fullMethod);

// 按 RPC 注册表里的消息号判:gRPC 方法用带包名的全名,RPC0 方法用注册表里的服务 / 方法名;受保护的方法只认精确规则。
// 越界与空槽一律放行。命中关停时计数并返回规则。
std::optional<Rule> CheckMessageId(uint32_t messageId);

// 按 gRPC 路径 "/pkg.Svc/Method" 判(muduo gRPC 服务端);受保护的方法只认精确规则。命中关停时计数并返回规则。
std::optional<Rule> CheckGrpcMethod(std::string_view fullMethod);

// 给调用方看的拒绝文案(英文,进日志与 gRPC 状态):"killswitch: method <m> is disabled[, reason: <r>]"。
std::string DenyMessage(std::string_view method, const Rule &rule);

// 把进程级实例的指标登记进 GlobalMetricsRegistry(采集器名 kill_switch)。重复调用只登记一次。
void RegisterMetrics();
} // namespace kill_switch
