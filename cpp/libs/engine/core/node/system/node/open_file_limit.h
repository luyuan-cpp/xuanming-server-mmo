#pragma once

#include <algorithm>
#include <cstdint>
#include <limits>
#include <string>
#include <string_view>

#include "proto/common/base/config.pb.h" // BaseDeployConfig(无 package,全局命名空间)
#include "proto/common/base/node.pb.h"   // common::base::eNodeType

// 进程 fd 上限(RLIMIT_NOFILE)的启动判定 —— 容量模型 A0-2、gate 准入设计 G4 / P0-1。
//
// 为什么由进程自己做:K8s 没有按容器声明 ulimit 的字段;镜像(deploy/k8s/Dockerfile.runtime / Dockerfile.cpp)
// 以非 root 运行、没有 ENTRYPOINT 也不设 ulimit;所有启动形态都只是一层 /bin/sh -lc 壳。生效的软限完全取决于
// 节点上的容器运行时(较新的 containerd / Docker 配 systemd 240+ 常见 soft 1024 / hard 524288,外部常识,本仓未实测)。
// 非特权进程能把软限抬到硬限,硬限只能在节点运行时层面改,所以:进程启动时自抬软限,抬完仍不够就拒启。
// 启动壳里不要再加一份 ulimit,那是同一件事的第二份实现。
//
// 本头文件只含纯判定:不做系统调用、不读 env、不打日志,Windows 上也能编译做单测
// (cpp/tests/client_endpoint_test/open_file_limit_test.cpp)。getrlimit / setrlimit、读 env、打印与拒启
// 都在 node.cpp 的 EnsureOpenFileLimitOrDie(Node::Initialize 调用,仅 Linux)。
namespace open_file_limit {

// 运维豁免开关。取值见 ParseMode:缺省 enforce(抬完仍不够就拒启);warn = 只打 WARN、继续启动。
// 与 GATE_RUN_MODE / SCENE_RUN_MODE / BATTLE_RUN_MODE 和 GM 开关无关:scene 的运行模式同时决定 GM 放行,
// 不能让运维为了绕过 fd 检查去打开 GM。warn 只用于「硬限暂时改不了、又必须先起服」的短时应急。
// 风险:gate 的准入闸门在 accept 拿到 fd 之后才判,准入上限没调时客户端连接会先占满 fd 表;此后节点间、Redis / Kafka /
// etcd 重连都会 EMFILE(etcd 租约续不上 → 被摘除),muduo 日志滚动时 FileUtil::AppendFile 的 fopen 失败只有 assert,
// Release 下随即对空指针 setbuffer 崩溃。所以 warn 换来的是「运行到某个时刻被摘除或崩溃」,不只是新连接被拒。
// gate / battle 的应急首选是把准入上限调到 ≤ 软限 − kNonClientReserve,那样本检查直接通过,不需要豁免。
inline constexpr const char kModeEnv[] = "NODE_OPEN_FILE_LIMIT_MODE";

// 「无界」:getrlimit 返回 RLIM_INFINITY 时由调用方映射成它。
inline constexpr uint64_t kUnlimited = std::numeric_limits<uint64_t>::max();

// 非客户端 fd 的余量,只用作拒启门槛,不是进程可用 fd 的上限(进程实际可用的是抬起来之后的软限,通常就是硬限)。
// 覆盖:节点间 TCP / gRPC 连接、Redis / Kafka / etcd 客户端、日志文件、每个 EventLoop 的 epoll / eventfd / timerfd,
// 以及被拒客户端连接在 forceClose 完成前仍占着的 fd。
//
// 取值 4096(2026-11-12 定案,不再等实测):
//   - 2026-10-18 的推算里,最大的一项是「到全服 scene_manager 约 2Z~4Z 条」(Z 是 zone 数),所以当时留了
//     「Z 超过约 900 要重新核」的尾巴。2026-11-05 起 scene_manager 按 zone 收窄(NodeUtils::IsZoneScopedNodeType),
//     gate 到路由服也只连按 zone 取的 k 台(docs/design/connection-fanout-subsetting-20261106.md),这一项不再随 Z 增长;
//   - 剩下随规模增长的只有 zone 内的 gate ↔ scene 全连接(每个节点至多本 zone 的对端台数)与全服池类型的副本数,
//     都远小于 4096;
//   - 这个数只决定「启动时要不要拒」。运行期够不够由指标看:process_open_fds / process_max_fds 超过 80% 告警
//     (process_fd_metrics.h、deploy/k8s/cpp-node-resource-alerts.yaml),不靠这个常数估得准。
inline constexpr uint64_t kNonClientReserve = 4096;

// 软限抬升的上限。硬限是 infinity 或 1073741816 这类巨大值时只抬到这里,
// 避免有代码按 RLIMIT_NOFILE 分配数组或遍历所有 fd。
// 封顶只约束本进程主动抬升的目标:继承来的软限本来就更大(例如运行时 LimitNOFILE=infinity 给出 soft = hard = 1073741816)
// 时按「只抬不降」保持原值,不压回来:本检查只保证下限,不改写运行时给的更大值(启动行 raise=none,soft= 即原值)。
inline constexpr uint64_t kSoftLimitCeiling = 1048576;

struct Limits {
    uint64_t soft = 0;
    uint64_t hard = 0;
};

// 客户端面的准入上限。configKey 为空表示这类节点没有客户端面(scene)。
struct ClientCap {
    uint64_t connections = 0;
    std::string_view configKey;
};

// 按节点类型取客户端面准入上限(etc/base_deploy_config.yaml 的同名键)。
// 新增有客户端面的节点类型时必须在这里加一个 case,否则它只按余量判定。
// 配置为 0 只在 dev/test 合法(gate 回落 session-id 硬上限 131071,battle 回落 65535),prod 由各节点自己的启动门禁
// (gate ValidateGateConnectionLimitOrDie、battle ValidateBattleClientEdgeConfigOrDie)拒绝;这里按 0 计,
// 只要求余量,免得本地调试被十几万的门槛拦住。
inline ClientCap ClientConnectionCapFor(uint32_t nodeType, const BaseDeployConfig &deploy) {
    switch (static_cast<common::base::eNodeType>(nodeType)) {
    case common::base::GateNodeService:
        return {deploy.gate_max_connections(), "GateMaxConnections"};
    case common::base::BattleNodeService:
        return {deploy.battle_max_connections(), "BattleMaxConnections"};
    default:
        return {};
    }
}

constexpr uint64_t RequiredOpenFiles(uint64_t clientConnections) {
    return clientConnections + kNonClientReserve;
}

// 目标软限 = min(硬限, max(上限, 需求)),且永不低于当前软限(只抬不降,硬限不动)。
constexpr uint64_t SoftLimitRaiseTarget(Limits current, uint64_t required) {
    return std::max(current.soft, std::min(current.hard, std::max(kSoftLimitCeiling, required)));
}

enum class Verdict : uint8_t { kOk, kInsufficient };
enum class Mode : uint8_t { kEnforce, kWarn };
// kProceedExempted = 不够但运维显式豁免(warn),继续启动并打 WARN。
enum class Action : uint8_t { kProceed, kProceedExempted, kRefuse };

struct ModeSetting {
    Mode mode = Mode::kEnforce;
    // false = env 给了不认识的值:按 enforce 处理(fail-closed),调用方打一行说明。
    bool recognized = true;
};

// 解析 NODE_OPEN_FILE_LIMIT_MODE。未设置与空串 = enforce;只认小写 enforce / warn(同 NODE_ETCD_TRANSPORT 的写法),
// 其余一律按 enforce 并标记 recognized=false —— 拼错开关不得意外放行。
inline ModeSetting ParseMode(const char *raw) {
    if (raw == nullptr || raw[0] == '\0') {
        return {Mode::kEnforce, true};
    }
    const std::string_view value(raw);
    if (value == "enforce") {
        return {Mode::kEnforce, true};
    }
    if (value == "warn") {
        return {Mode::kWarn, true};
    }
    return {Mode::kEnforce, false};
}

constexpr std::string_view ModeName(Mode mode) {
    return mode == Mode::kWarn ? "warn" : "enforce";
}

constexpr std::string_view VerdictName(Verdict verdict) {
    return verdict == Verdict::kOk ? "ok" : "insufficient";
}

struct Outcome {
    Limits before;
    uint64_t required = 0;
    uint64_t softAfter = 0;
    bool raiseAttempted = false;
    int raiseErrno = 0;
    Verdict verdict = Verdict::kInsufficient;
};

// 目标高于当前软限时才调一次 setSoftLimit(target):返回 0 表示成功,否则返回 errno。
// 成功时 softAfter = target;失败时保持原值并记下 errno。verdict 只看抬升后的软限是否覆盖需求。
template <typename SetSoftLimitFn>
Outcome Evaluate(Limits before, uint64_t required, SetSoftLimitFn &&setSoftLimit) {
    Outcome outcome;
    outcome.before = before;
    outcome.required = required;
    outcome.softAfter = before.soft;
    const uint64_t target = SoftLimitRaiseTarget(before, required);
    if (target > before.soft) {
        outcome.raiseAttempted = true;
        outcome.raiseErrno = setSoftLimit(target);
        if (outcome.raiseErrno == 0) {
            outcome.softAfter = target;
        }
    }
    outcome.verdict = outcome.softAfter >= required ? Verdict::kOk : Verdict::kInsufficient;
    return outcome;
}

// 缺省 fail-closed:不够就拒启;只有显式 warn 才放行。
constexpr Action ActionFor(Verdict verdict, Mode mode) {
    if (verdict == Verdict::kOk) {
        return Action::kProceed;
    }
    return mode == Mode::kWarn ? Action::kProceedExempted : Action::kRefuse;
}

constexpr uint64_t Shortfall(const Outcome &outcome) {
    return outcome.softAfter >= outcome.required ? 0 : outcome.required - outcome.softAfter;
}

inline std::string FormatLimitValue(uint64_t value) {
    return value == kUnlimited ? "unlimited" : std::to_string(value);
}

inline std::string FormatRaise(const Outcome &outcome) {
    if (!outcome.raiseAttempted) {
        return "none";
    }
    if (outcome.raiseErrno == 0) {
        return "ok";
    }
    return "failed(errno=" + std::to_string(outcome.raiseErrno) + ")";
}

// 启动行,stdout 与日志各一行,无论结果。运维、压测与 tools/scripts/deploy-staging.sh 按 "[open_file_limit]" grep,
// 格式由单测逐字锁定;改格式要同步改这些消费方。
inline std::string FormatStartupLine(std::string_view role, const ClientCap &cap, Mode mode, const Outcome &outcome) {
    std::string line = "[open_file_limit] role=";
    line += role;
    line += " verdict=";
    line += VerdictName(outcome.verdict);
    line += " mode=";
    line += ModeName(mode);
    line += " required=";
    line += FormatLimitValue(outcome.required);
    line += " client_cap=";
    line += std::to_string(cap.connections);
    line += " cap_key=";
    line += cap.configKey.empty() ? std::string_view("none") : cap.configKey;
    line += " reserve=";
    line += std::to_string(kNonClientReserve);
    line += " soft_before=";
    line += FormatLimitValue(outcome.before.soft);
    line += " soft=";
    line += FormatLimitValue(outcome.softAfter);
    line += " hard=";
    line += FormatLimitValue(outcome.before.hard);
    line += " raise=";
    line += FormatRaise(outcome);
    return line;
}

// 豁免生效时的 WARN 行(stdout 与日志各一行)。后果要写全(见 kModeEnv 的注释):fd 表满后坏的不只是新连接,
// 运维照这一行判断风险,说轻了会把 warn 当常态配置。格式由单测逐字锁定。
inline std::string FormatExemptionWarning(std::string_view role, const Outcome &outcome) {
    std::string line = "[open_file_limit] WARN exemption active (";
    line += kModeEnv;
    line += "=warn): role=";
    line += role;
    line += " soft=";
    line += FormatLimitValue(outcome.softAfter);
    line += " required=";
    line += FormatLimitValue(outcome.required);
    line += " shortfall=";
    line += std::to_string(Shortfall(outcome));
    line += "; starting anyway. Once the fd table is full, not only new client connections but also reconnects"
            " (peer nodes, Redis, Kafka, etcd) and log file rotation fail with EMFILE, so the node may be"
            " deregistered or crash; for gate / battle lower the client cap to soft - reserve instead";
    return line;
}

// env 给了不认识的值时的说明行。原值不是敏感信息(运维手写的开关),照原样带出便于定位拼写错误。
inline std::string FormatInvalidModeLine(std::string_view raw) {
    std::string line = "[open_file_limit] WARN invalid ";
    line += kModeEnv;
    line += "='";
    line += raw;
    line += "', falling back to enforce (accepted: enforce|warn)";
    return line;
}

// 拒启原因(经 FatalStartupConfig 写 stderr 与日志)。要点:谁、差多少、硬限由谁决定、能调哪几个旋钮。
inline std::string FormatRefusalReason(std::string_view role, const ClientCap &cap, const Outcome &outcome) {
    std::string reason = "role=";
    reason += role;
    reason += " soft=";
    reason += FormatLimitValue(outcome.softAfter);
    reason += " hard=";
    reason += FormatLimitValue(outcome.before.hard);
    reason += " required=";
    reason += FormatLimitValue(outcome.required);
    reason += " (";
    if (cap.configKey.empty()) {
        reason += "no client-facing cap";
    } else {
        reason += cap.configKey;
        reason += "=";
        reason += std::to_string(cap.connections);
    }
    reason += " + reserve ";
    reason += std::to_string(kNonClientReserve);
    reason += ") shortfall=";
    reason += std::to_string(Shortfall(outcome));
    reason += " raise=";
    reason += FormatRaise(outcome);
    reason += "; the soft limit cannot exceed the hard limit, which is set by the container runtime / systemd"
              " (LimitNOFILE of containerd or dockerd, or rlimits in the CRI base_runtime_spec;"
              " Kubernetes has no per-container ulimit field). Raise the node runtime hard limit";
    if (!cap.configKey.empty()) {
        reason += ", or lower ";
        reason += cap.configKey;
    }
    reason += ", or set ";
    reason += kModeEnv;
    reason += "=warn as a temporary operator exemption; see docs/ops/gate-kernel-tuning-runbook.md section 3.1";
    return reason;
}

} // namespace open_file_limit
