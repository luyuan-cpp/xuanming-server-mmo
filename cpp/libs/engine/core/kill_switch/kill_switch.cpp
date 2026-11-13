#include "kill_switch/kill_switch.h"

#include <algorithm>
#include <array>
#include <cctype>
#include <cmath>
#include <limits>
#include <utility>

#include <google/protobuf/struct.pb.h>
#include <google/protobuf/util/json_util.h>
#include <muduo/base/Logging.h>

#include "metrics/metrics_registry.h"
#include "rpc/service_metadata/battle_node_service_metadata.h"
#include "rpc/service_metadata/gate_client_spectate_service_metadata.h"
#include "rpc/service_metadata/gate_service_service_metadata.h"
#include "rpc/service_metadata/login_service_metadata.h"
#include "rpc/service_metadata/player_battle_service_metadata.h"
#include "rpc/service_metadata/player_lifecycle_service_metadata.h"
#include "rpc/service_metadata/rpc_event_registry.h"
#include "rpc/service_metadata/scene_admin_service_metadata.h"
#include "rpc/service_metadata/scene_node_service_service_metadata.h"
#include "rpc/service_metadata/scene_service_metadata.h"

namespace kill_switch
{
namespace
{
// gRPC 状态码的上界(UNAUTHENTICATED = 16)。
constexpr uint32_t kMaxGrpcStatusCode = 16;

constexpr std::string_view kTypeScopeSegment = "type/";
constexpr std::string_view kInstanceScopeSegment = "instance/";
constexpr std::string_view kAllMethods = "*";

// 受保护的方法(设计文档 §8):只认精确到方法的规则。热关停只挡新工作,在途的工作必须收得了尾。
// 新增一项要写明它挡的是哪种收不了尾的情形;用消息号常量,方法改名或删除时这里编译失败,不会静默失效。
constexpr std::array kProtectedMessageIds{
    // 连接层身份握手:关停期间节点间链路不断,解除后不用重连。
    GateNodeHandshakeMessageId,
    SceneNodeHandshakeMessageId,
    // 停机:被整体关停的节点还要能被优雅停机(先止血、再停机是标准处置,§10)。
    GateGmGracefulShutdownMessageId,
    SceneSceneGmGracefulShutdownMessageId,
    // gate→scene 的转发信封:RPC0 层不按它判,scene 内层按真实方法判(否则 `*` 会连 ExitGame 一起挡掉)。
    SceneProcessClientPlayerMessageMessageId,
    // 会话收尾:玩家下线存盘、交接释放旧节点、客户端离开游戏、退订观战。挡住会让玩家卡在旧节点上、重登被拒。
    ScenePlayerExitGameMessageId,
    SceneNodeGrpcReleasePlayerMessageId,
    ClientPlayerLoginLeaveGameMessageId,
    BattleClientPlayerStopWatchBattleMessageId,
    GateClientSpectateUnsubscribeSpectateMessageId,
    BattleNodeRemoveObserverMessageId,
    // 资源收尾:拆场景、拆战斗房间。挡住会让已结束的场景 / 房间一直占着节点。
    SceneDestroySceneMessageId,
    SceneNodeGrpcDestroySceneMessageId,
    BattleNodeDestroyBattleMessageId,
    // 在途事务的补偿与回执:解冻参战玩家、退回资产预留、释放场景收据。挡住会让冻结 / 预留一直悬着。
    SceneCancelBattlePrepareMessageId,
    SceneNodeGrpcCancelBattlePrepareMessageId,
    SceneNodeGrpcAssetAbortDebitMessageId,
    SceneNodeGrpcAssetAckReceiptMessageId,
};

std::string_view TrimAsciiSpace(std::string_view text)
{
    const auto isSpace = [](char c) { return std::isspace(static_cast<unsigned char>(c)) != 0; };
    while (!text.empty() && isSpace(text.front()))
    {
        text.remove_prefix(1);
    }
    while (!text.empty() && isSpace(text.back()))
    {
        text.remove_suffix(1);
    }
    return text;
}

std::string ToLowerAscii(std::string_view text)
{
    std::string lower(text);
    std::transform(lower.begin(), lower.end(), lower.begin(),
                   [](unsigned char c) { return static_cast<char>(std::tolower(c)); });
    return lower;
}

// JSON 形式:只认 deny(布尔)、reason(字符串)、code(非负整数),值为 null 等于没写;类型不对算写坏,别的键忽略。
// 与 Go json.Unmarshal 进 Rule 结构体、Java KillSwitch.parseJson 同口径。
std::optional<Rule> ParseJsonRule(std::string_view json)
{
    google::protobuf::Struct object;
    if (!google::protobuf::util::JsonStringToMessage(std::string(json), &object).ok())
    {
        return std::nullopt;
    }
    using google::protobuf::Value;
    Rule rule;
    for (const auto &[name, value] : object.fields())
    {
        if (value.kind_case() == Value::kNullValue)
        {
            continue;
        }
        if (name == "deny")
        {
            if (value.kind_case() != Value::kBoolValue)
            {
                return std::nullopt;
            }
            rule.deny = value.bool_value();
        }
        else if (name == "reason")
        {
            if (value.kind_case() != Value::kStringValue)
            {
                return std::nullopt;
            }
            rule.reason = value.string_value();
        }
        else if (name == "code")
        {
            if (value.kind_case() != Value::kNumberValue)
            {
                return std::nullopt;
            }
            const double number = value.number_value();
            if (!std::isfinite(number) || number < 0 || number != std::floor(number) ||
                number > static_cast<double>(std::numeric_limits<uint32_t>::max()))
            {
                return std::nullopt;
            }
            rule.code = static_cast<uint32_t>(number);
        }
    }
    return rule;
}

std::string JoinTarget(std::string_view service, std::string_view method)
{
    std::string target;
    target.reserve(service.size() + 1 + method.size());
    target.append(service);
    target.push_back('/');
    target.append(method);
    return target;
}

std::string_view ShortServiceName(std::string_view service)
{
    const auto dot = service.rfind('.');
    return dot == std::string_view::npos ? service : service.substr(dot + 1);
}

bool Contains(const std::vector<std::string> &values, const std::string &value)
{
    return std::find(values.begin(), values.end(), value) != values.end();
}

bool AppliesTo(const RuleKey &key, const Identity &identity)
{
    switch (key.scope)
    {
    case Scope::kAll:
        return true;
    case Scope::kNodeType:
        return Contains(identity.nodeTypes, key.scopeValue);
    case Scope::kInstance:
        return Contains(identity.instances, key.scopeValue);
    default:
        return false;
    }
}

// 进程级实例的被关停计数采集器(设计文档 §11)。
void CollectMetrics(prometheus_text::Writer &writer)
{
    using prometheus_text::MetricType;
    const KillSwitch &killSwitch = KillSwitch::Process();
    if (writer.BeginFamily("killswitch_rules", "Kill switch rules that apply to this process (normally 0).",
                           MetricType::kGauge))
    {
        writer.Sample({}, killSwitch.ApplicableRuleCount());
    }
    if (writer.BeginFamily("killswitch_synced",
                           "1 when this process holds a fresh kill switch snapshot from etcd; 0 means every request is allowed.",
                           MetricType::kGauge))
    {
        writer.Sample({}, killSwitch.Synced() ? 1 : 0);
    }
    if (writer.BeginFamily("killswitch_blocked_total", "Requests short-circuited by kill switch rules.",
                           MetricType::kCounter))
    {
        for (const auto &[method, count] : killSwitch.BlockedCounts())
        {
            writer.Sample({{"method", method}}, count);
        }
    }
}
} // namespace

uint32_t Rule::GrpcStatusCode() const
{
    return code == 0 || code > kMaxGrpcStatusCode ? kGrpcUnavailable : code;
}

std::optional<Rule> ParseRule(std::string_view raw)
{
    const std::string_view text = TrimAsciiSpace(raw);
    if (text.empty())
    {
        return Rule{}; // 空值 = 显式不关停(先建键占位)
    }
    if (text.front() == '{')
    {
        return ParseJsonRule(text);
    }
    const std::string word = ToLowerAscii(text);
    if (word == "1" || word == "true" || word == "on" || word == "yes" || word == "deny")
    {
        Rule rule;
        rule.deny = true;
        return rule;
    }
    if (word == "0" || word == "false" || word == "off" || word == "no" || word == "allow")
    {
        return Rule{};
    }
    return std::nullopt;
}

std::optional<RuleKey> ParseRuleKey(std::string_view relativeKey)
{
    std::string_view key = TrimAsciiSpace(relativeKey);
    while (!key.empty() && key.front() == '/')
    {
        key.remove_prefix(1); // 容忍运维多敲的斜杠:.../killswitch//Svc/Method
    }
    if (key.empty())
    {
        return std::nullopt;
    }
    const auto scoped = [](Scope scope, std::string_view rest) -> std::optional<RuleKey>
    {
        const auto slash = rest.find('/');
        if (slash == std::string_view::npos || slash == 0 || slash + 1 == rest.size())
        {
            return std::nullopt; // 范围值或目标缺失
        }
        return RuleKey{scope, std::string(rest.substr(0, slash)), std::string(rest.substr(slash + 1))};
    };
    if (key.starts_with(kTypeScopeSegment))
    {
        return scoped(Scope::kNodeType, key.substr(kTypeScopeSegment.size()));
    }
    if (key.starts_with(kInstanceScopeSegment))
    {
        return scoped(Scope::kInstance, key.substr(kInstanceScopeSegment.size()));
    }
    return RuleKey{Scope::kAll, std::string(), std::string(key)};
}

std::optional<MethodName> ParseFullMethod(std::string_view fullMethod)
{
    if (fullMethod.starts_with('/'))
    {
        fullMethod.remove_prefix(1);
    }
    const auto slash = fullMethod.rfind('/');
    if (slash == std::string_view::npos || slash == 0 || slash + 1 == fullMethod.size())
    {
        return std::nullopt;
    }
    return MethodName{fullMethod.substr(0, slash), fullMethod.substr(slash + 1)};
}

std::vector<std::string> MatchTargets(const MethodName &method, Coverage coverage)
{
    std::vector<std::string> targets;
    if (method.service.empty() || method.method.empty())
    {
        if (coverage == Coverage::kAllRules)
        {
            targets.emplace_back(kAllMethods);
        }
        return targets;
    }
    const std::string_view fullName = method.service;
    const std::string_view shortName = ShortServiceName(fullName);
    const bool hasShortName = !shortName.empty() && shortName != fullName;
    targets.reserve(5);
    targets.push_back(JoinTarget(fullName, method.method));
    if (hasShortName)
    {
        targets.push_back(JoinTarget(shortName, method.method));
    }
    if (coverage == Coverage::kExactRulesOnly)
    {
        return targets;
    }
    targets.push_back(JoinTarget(fullName, kAllMethods));
    if (hasShortName)
    {
        targets.push_back(JoinTarget(shortName, kAllMethods));
    }
    targets.emplace_back(kAllMethods);
    return targets;
}

KillSwitch::KillSwitch(std::chrono::seconds staleAfter, Clock clock)
    : staleAfter_(staleAfter), clock_(std::move(clock))
{
}

KillSwitch &KillSwitch::Process()
{
    static KillSwitch instance;
    return instance;
}

std::chrono::steady_clock::time_point KillSwitch::Now() const
{
    return clock_ ? clock_() : std::chrono::steady_clock::now();
}

bool KillSwitch::StaleLocked(std::chrono::steady_clock::time_point now) const
{
    if (syncedAt_ == std::chrono::steady_clock::time_point{} || staleAfter_.count() <= 0)
    {
        return false;
    }
    return now - syncedAt_ > staleAfter_;
}

void KillSwitch::SetIdentity(Identity identity)
{
    std::lock_guard lock(mutex_);
    identity_ = std::move(identity);
    RebuildLocked();
}

void KillSwitch::ReplaceAll(std::map<std::string, std::string> rawRules)
{
    std::lock_guard lock(mutex_);
    syncedAt_ = Now();
    if (rawRules == raw_ && snapshot_)
    {
        return; // 定期复核没有变化:只续新鲜度
    }
    raw_ = std::move(rawRules);
    RebuildLocked();
}

void KillSwitch::Put(const std::string &relativeKey, const std::string &rawValue)
{
    std::lock_guard lock(mutex_);
    syncedAt_ = Now();
    const auto found = raw_.find(relativeKey);
    if (found != raw_.end() && found->second == rawValue && snapshot_)
    {
        return;
    }
    raw_[relativeKey] = rawValue;
    RebuildLocked();
}

void KillSwitch::Erase(const std::string &relativeKey)
{
    std::lock_guard lock(mutex_);
    syncedAt_ = Now();
    if (raw_.erase(relativeKey) == 0 && snapshot_)
    {
        return;
    }
    RebuildLocked();
}

void KillSwitch::MarkSynced()
{
    std::lock_guard lock(mutex_);
    syncedAt_ = Now();
}

void KillSwitch::RebuildLocked()
{
    auto next = std::make_shared<Snapshot>();
    std::set<std::string> badKeys;
    for (const auto &[key, value] : raw_)
    {
        const auto ruleKey = ParseRuleKey(key);
        if (!ruleKey)
        {
            badKeys.insert(key); // 范围段残缺:不是规则,记一笔让运维看见
            continue;
        }
        if (!AppliesTo(*ruleKey, identity_))
        {
            continue; // 别的节点类型 / 别的进程的规则:不解析、不记日志
        }
        const auto rule = ParseRule(value);
        if (!rule)
        {
            badKeys.insert(key);
            continue;
        }
        auto &layer = ruleKey->scope == Scope::kInstance   ? next->instanceRules
                      : ruleKey->scope == Scope::kNodeType ? next->typeRules
                                                           : next->allRules;
        const auto [slot, inserted] = layer.emplace(ruleKey->target, *rule);
        if (!inserted && rule->deny && !slot->second.deny)
        {
            slot->second = *rule; // 同一层里本进程的两个身份写了相反的规则:关停优先
        }
        next->byKey.emplace(key, *rule);
    }

    for (const auto &key : badKeys)
    {
        if (!badKeys_.contains(key))
        {
            LOG_ERROR << "[KillSwitch] Malformed rule ignored (requests stay allowed): key=" << key << " value=\""
                      << raw_[key] << "\"";
        }
    }
    for (const auto &key : badKeys_)
    {
        if (!badKeys.contains(key))
        {
            LOG_INFO << "[KillSwitch] Previously malformed rule is gone or fixed: key=" << key;
        }
    }
    badKeys_ = std::move(badKeys);

    static const std::map<std::string, Rule> kNoRules;
    const auto &previous = snapshot_ ? snapshot_->byKey : kNoRules;
    for (const auto &[key, rule] : next->byKey)
    {
        const auto old = previous.find(key);
        if (old == previous.end() || !(old->second == rule))
        {
            LOG_INFO << "[KillSwitch] Rule applied: key=" << key << " deny=" << (rule.deny ? "true" : "false")
                     << " code=" << rule.GrpcStatusCode() << " reason=\"" << rule.reason << "\"";
        }
    }
    for (const auto &[key, rule] : previous)
    {
        if (!next->byKey.contains(key))
        {
            LOG_INFO << "[KillSwitch] Rule removed: key=" << key;
        }
    }

    hasRules_.store(!next->byKey.empty(), std::memory_order_release);
    snapshot_ = std::move(next);
}

std::optional<Rule> KillSwitch::Check(const MethodName &method, Coverage coverage) const
{
    if (!hasRules_.load(std::memory_order_acquire))
    {
        return std::nullopt; // 常态:没有适用规则
    }
    std::shared_ptr<const Snapshot> snapshot;
    {
        std::lock_guard lock(mutex_);
        if (StaleLocked(Now()))
        {
            return std::nullopt; // 与 etcd 失联太久:快照作废,整体放行
        }
        snapshot = snapshot_;
    }
    if (!snapshot)
    {
        return std::nullopt;
    }
    const auto targets = MatchTargets(method, coverage);
    for (const auto *layer : {&snapshot->instanceRules, &snapshot->typeRules, &snapshot->allRules})
    {
        for (const auto &target : targets)
        {
            const auto found = layer->find(target);
            if (found == layer->end())
            {
                continue;
            }
            // 第一条命中的规则定谳:更具体的规则可以用 deny=false 豁免更宽的规则。
            if (found->second.deny)
            {
                return found->second;
            }
            return std::nullopt;
        }
    }
    return std::nullopt;
}

size_t KillSwitch::ApplicableRuleCount() const
{
    std::lock_guard lock(mutex_);
    if (!snapshot_ || StaleLocked(Now()))
    {
        return 0;
    }
    return snapshot_->byKey.size();
}

bool KillSwitch::Synced() const
{
    std::lock_guard lock(mutex_);
    return syncedAt_ != std::chrono::steady_clock::time_point{} && !StaleLocked(Now());
}

void KillSwitch::RecordBlocked(const MethodName &method)
{
    const std::string label = JoinTarget(ShortServiceName(method.service), method.method);
    std::lock_guard lock(mutex_);
    ++blocked_[label];
}

std::map<std::string, uint64_t> KillSwitch::BlockedCounts() const
{
    std::lock_guard lock(mutex_);
    return blocked_;
}

bool IsProtectedMessageId(uint32_t messageId)
{
    return std::find(kProtectedMessageIds.begin(), kProtectedMessageIds.end(), messageId) != kProtectedMessageIds.end();
}

bool IsProtectedGrpcMethod(std::string_view fullMethod)
{
    // 名单里只有十几项,逐项比注册表里登记的 gRPC 路径;只在有适用规则时才走到这里(CheckGrpcMethod)。
    for (const uint32_t messageId : kProtectedMessageIds)
    {
        if (messageId >= gRpcMethodRegistry.size())
        {
            continue;
        }
        const char *path = gRpcMethodRegistry[messageId].grpcFullMethod;
        if (path != nullptr && fullMethod == path)
        {
            return true;
        }
    }
    return false;
}

std::optional<Rule> CheckMessageId(uint32_t messageId)
{
    if (messageId >= gRpcMethodRegistry.size())
    {
        return std::nullopt;
    }
    const RpcMethodMeta &meta = gRpcMethodRegistry[messageId];
    if (meta.serviceName == nullptr || meta.methodName == nullptr)
    {
        return std::nullopt; // 注册表空槽
    }
    // gRPC 方法用 proto 声明的带包名全名(与 Go 侧 info.FullMethod 一致);RPC0 方法没有包名。
    MethodName name{meta.serviceName, meta.methodName};
    if (meta.grpcFullMethod != nullptr)
    {
        if (const auto parsed = ParseFullMethod(meta.grpcFullMethod))
        {
            name = *parsed;
        }
    }
    const Coverage coverage = IsProtectedMessageId(messageId) ? Coverage::kExactRulesOnly : Coverage::kAllRules;
    KillSwitch &killSwitch = KillSwitch::Process();
    auto rule = killSwitch.Check(name, coverage);
    if (rule)
    {
        killSwitch.RecordBlocked(name);
    }
    return rule;
}

std::optional<Rule> CheckGrpcMethod(std::string_view fullMethod)
{
    const auto name = ParseFullMethod(fullMethod);
    if (!name)
    {
        return std::nullopt;
    }
    KillSwitch &killSwitch = KillSwitch::Process();
    // 常态没有规则:先走 Check 内部的原子量快速判断,再查保护名单。
    auto rule = killSwitch.Check(*name);
    if (rule && IsProtectedGrpcMethod(fullMethod))
    {
        rule = killSwitch.Check(*name, Coverage::kExactRulesOnly);
    }
    if (rule)
    {
        killSwitch.RecordBlocked(*name);
    }
    return rule;
}

std::string DenyMessage(std::string_view method, const Rule &rule)
{
    std::string message = "killswitch: method ";
    message.append(method);
    message.append(" is disabled");
    if (!rule.reason.empty())
    {
        message.append(", reason: ");
        message.append(rule.reason);
    }
    return message;
}

void RegisterMetrics()
{
    static std::once_flag registered;
    std::call_once(registered, []
    {
        if (!GlobalMetricsRegistry().Register("kill_switch", &CollectMetrics))
        {
            LOG_ERROR << "[Metrics] collector kill_switch is already registered";
        }
    });
}
} // namespace kill_switch
