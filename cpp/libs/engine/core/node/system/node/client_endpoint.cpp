#include "node/system/node/client_endpoint.h"

#include <charconv>
#include <cstddef>
#include <iterator>
#include <system_error>
#include <utility>

namespace client_endpoint {

namespace {

// TCP 端口上限;0 表示"未填",同样不可用。
constexpr uint32_t kMaxPort = 65535;

// DNS 名总长上限(RFC 1035);IPv4 字面量远短于它。
constexpr std::size_t kMaxHostLength = 253;

// 与 SourceKind 逐项对应的 env 取值,顺序必须与枚举项一致(static_assert 只拦数量不一致,
// 拦不住顺序写反)。加来源 = 枚举加一项 + 这里加一行。
constexpr std::string_view kSourceKindNames[] = {
    "none",   // kNone
    "static", // kStatic
    "agones", // kAgones
};
static_assert(std::size(kSourceKindNames) == static_cast<std::size_t>(SourceKind::kCount),
              "kSourceKindNames 必须与 SourceKind 一一对应:加了枚举项就得加取值");

// 进程级外部来源工厂。线程模型:写(预构造钩子)与读(Node 构造期的 InitClientEndpoint)
// 都在启动主线程上、EventLoop 运行之前顺序发生,不存在并发访问,故不加同步。
ExternalSourceFactory gExternalSourceFactory = nullptr;

// 读 env;未设置与空串同义,都返回空 view。
std::string_view ReadEnv(const EnvLookup &env, const char *name)
{
    const char *value = env(name);
    if (value == nullptr)
    {
        return {};
    }
    return std::string_view(value);
}

bool ParseSourceKind(std::string_view raw, SourceKind &out)
{
    if (raw.empty())
    {
        out = SourceKind::kNone;
        return true;
    }
    for (std::size_t i = 0; i < std::size(kSourceKindNames); ++i)
    {
        if (kSourceKindNames[i] == raw)
        {
            out = static_cast<SourceKind>(i);
            return true;
        }
    }
    return false;
}

// 端口:纯十进制(不接受符号、空白、前后缀),范围 1..65535。
bool ParsePort(std::string_view raw, uint32_t &out)
{
    if (raw.empty())
    {
        return false;
    }
    uint32_t value = 0;
    const char *begin = raw.data();
    const char *end = raw.data() + raw.size();
    const auto [ptr, ec] = std::from_chars(begin, end, value, 10);
    if (ec != std::errc{} || ptr != end || value < 1 || value > kMaxPort)
    {
        return false;
    }
    out = value;
    return true;
}

// 按字节判断,不走 <cctype>:std::isalnum 受 locale 影响,且对负值 char 是未定义行为。
bool IsAsciiAlnum(char c)
{
    return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9');
}

// 主机:只接受 IPv4 字面量或 DNS 名这类"裸主机"。带 scheme("http://")、端口(":30000")、
// 路径、userinfo、空白或控制字符的一律拒绝 —— 它们会原样下发给客户端,客户端只会连不上,
// 且服务端没有任何报错。
// 字符白名单与 tools/scripts/lib/k8s_client_entry.ps1 Test-ClientEntryHostName 同口径
// (^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$,总长 ≤ 253);它能拦住 kubelet 未展开的 `$(VAR)`
// 和残留的模板占位符 `{…}`。用白名单而非黑名单:黑名单漏掉的字符会让这些字面量被当成合法地址发布,
// fail-closed 失效。两边改口径必须同步改。
bool IsBareHost(std::string_view host)
{
    if (host.empty() || host.size() > kMaxHostLength)
    {
        return false;
    }
    if (!IsAsciiAlnum(host.front()) || !IsAsciiAlnum(host.back()))
    {
        return false;
    }
    for (const char c : host)
    {
        if (!IsAsciiAlnum(c) && c != '.' && c != '-')
        {
            return false;
        }
    }
    return true;
}

std::string DescribeEndpoint(const std::string &host, uint32_t port)
{
    return "host='" + host + "' port=" + std::to_string(port);
}

} // namespace

bool ParseSettings(const EnvLookup &env, Settings &out, std::string &error)
{
    if (!env)
    {
        error = "client endpoint env lookup is empty";
        return false;
    }

    Settings parsed;

    const std::string_view rawRequired = ReadEnv(env, kEnvRequired);
    if (rawRequired.empty() || rawRequired == "0")
    {
        parsed.required = false;
    }
    else if (rawRequired == "1")
    {
        parsed.required = true;
    }
    else
    {
        error = std::string(kEnvRequired) + " must be 0 or 1, got '" + std::string(rawRequired) + "'";
        return false;
    }

    const std::string_view rawSource = ReadEnv(env, kEnvSource);
    if (!ParseSourceKind(rawSource, parsed.source))
    {
        error = std::string(kEnvSource) + " must be one of none|static|agones, got '" + std::string(rawSource) + "'";
        return false;
    }

    const std::string_view rawHost = ReadEnv(env, kEnvHost);
    const std::string_view rawPort = ReadEnv(env, kEnvPort);

    switch (parsed.source)
    {
    case SourceKind::kNone:
        // podip 形态的 Fleet 模板可能仍带 HOST(D80/WP9),none 下 HOST / PORT 一律忽略、不校验。
        // REQUIRED=1 却不自报地址,消费方会把本节点全部跳过,属于矛盾配置。
        if (parsed.required)
        {
            error = std::string(kEnvRequired) + "=1 contradicts " + kEnvSource + "=none";
            return false;
        }
        break;

    case SourceKind::kStatic:
        if (rawHost.empty())
        {
            error = std::string(kEnvHost) + " is required when " + kEnvSource + "=static";
            return false;
        }
        if (!IsBareHost(rawHost))
        {
            error = std::string(kEnvHost) + " must be a bare IPv4 literal or DNS name without scheme, port or path, got '" +
                    std::string(rawHost) + "'";
            return false;
        }
        if (rawPort.empty())
        {
            error = std::string(kEnvPort) + " is required when " + kEnvSource + "=static";
            return false;
        }
        if (!ParsePort(rawPort, parsed.port))
        {
            error = std::string(kEnvPort) + " must be a decimal integer in 1..65535, got '" + std::string(rawPort) + "'";
            return false;
        }
        parsed.host.assign(rawHost);
        break;

    case SourceKind::kAgones:
        // 端口只能来自 Agones 的 Dynamic 分配;同时再给一个静态端口会让"以谁为准"产生歧义。
        if (!rawPort.empty())
        {
            error = std::string(kEnvPort) + " must not be set when " + kEnvSource + "=agones, got '" +
                    std::string(rawPort) + "'";
            return false;
        }
        if (!rawHost.empty())
        {
            if (!IsBareHost(rawHost))
            {
                error = std::string(kEnvHost) +
                        " must be a bare IPv4 literal or DNS name without scheme, port or path, got '" +
                        std::string(rawHost) + "'";
                return false;
            }
            parsed.host.assign(rawHost);
        }
        break;

    case SourceKind::kCount:
        error = std::string(kEnvSource) + " resolved to an invalid source kind";
        return false;
    }

    out = std::move(parsed);
    return true;
}

void SetExternalSourceFactory(ExternalSourceFactory f)
{
    gExternalSourceFactory = f;
}

ExternalSourceFactory GetExternalSourceFactory()
{
    return gExternalSourceFactory;
}

bool Resolve(const Settings &settings, ExternalSource *external, std::optional<EndpointComp> &out, std::string &error)
{
    out.reset();

    switch (settings.source)
    {
    case SourceKind::kNone:
        return true;

    case SourceKind::kStatic:
    {
        EndpointComp endpoint;
        endpoint.set_ip(settings.host);
        endpoint.set_port(settings.port);
        // Settings 通常来自 ParseSettings,这里仍复核一次:别让手工拼的 Settings 绕过可用性判定。
        if (!IsBareHost(settings.host) || !IsUsable(endpoint))
        {
            error = "static client endpoint is not usable: " + DescribeEndpoint(settings.host, settings.port);
            return false;
        }
        out = std::move(endpoint);
        return true;
    }

    case SourceKind::kAgones:
    {
        if (!settings.host.empty() && !IsBareHost(settings.host))
        {
            error = "agones client endpoint host override is not a bare host: '" + settings.host + "'";
            return false;
        }
        if (external == nullptr)
        {
            error = "agones client endpoint source is not available";
            return false;
        }
        Advertised advertised;
        std::string fetchError;
        if (!external->Fetch(advertised, fetchError))
        {
            error = "agones client endpoint fetch failed: " + fetchError;
            return false;
        }
        EndpointComp endpoint;
        endpoint.set_ip(settings.host.empty() ? advertised.host : settings.host);
        endpoint.set_port(advertised.port);
        if (!IsUsable(endpoint))
        {
            error = "agones returned an unusable client endpoint: " + DescribeEndpoint(advertised.host, advertised.port) +
                    " host_override='" + settings.host + "'";
            return false;
        }
        out = std::move(endpoint);
        return true;
    }

    case SourceKind::kCount:
        break;
    }

    error = "unknown client endpoint source kind";
    return false;
}

bool IsUsable(const EndpointComp &endpoint)
{
    return !endpoint.ip().empty() && endpoint.port() >= 1 && endpoint.port() <= kMaxPort;
}

std::optional<EndpointComp> ClientFacing(const NodeInfo &node, bool required)
{
    if (IsUsable(node.client_endpoint()))
    {
        return node.client_endpoint();
    }
    if (required)
    {
        return std::nullopt;
    }
    if (IsUsable(node.endpoint()))
    {
        return node.endpoint();
    }
    return std::nullopt;
}

std::string_view SourceKindName(SourceKind kind)
{
    const auto index = static_cast<std::size_t>(kind);
    if (index >= std::size(kSourceKindNames))
    {
        return "unknown";
    }
    return kSourceKindNames[index];
}

} // namespace client_endpoint
