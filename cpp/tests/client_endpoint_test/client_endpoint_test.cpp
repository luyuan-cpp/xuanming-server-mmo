#include <gtest/gtest.h>

#include <cstdint>
#include <map>
#include <memory>
#include <optional>
#include <string>
#include <utility>

#include "node/system/node/client_endpoint.h"
#include "proto/common/base/common.pb.h" // NodeInfo / EndpointComp(无 package,全局命名空间)

// client_endpoint 的单元测试(集群外入口 D76–D79)。
//
// 被测的是生产方(ParseSettings / Resolve)与消费方(ClientFacing)两半的纯判定:
//   - env 经 EnvLookup 注入内存表,不读、不改真实进程环境;
//   - Agones 来源用 FakeSource 顶替,不起 sidecar、不发网络请求。
// 生产方判错时 Node 会 LOG_FATAL,所以这里每一条"非法"用例都对应一次线上 CrashLoop,
// 断言的是"判为非法 + error 点名是哪个 env",而不只是返回值。

namespace
{

    using client_endpoint::Advertised;
    using client_endpoint::ExternalSource;
    using client_endpoint::Settings;
    using client_endpoint::SourceKind;

    // 内存 env 表。未登记的名字返回 nullptr,与 std::getenv 的"未设置"一致。
    class FakeEnv
    {
    public:
        FakeEnv &Set(const std::string &name, const std::string &value)
        {
            values_[name] = value;
            return *this;
        }

        client_endpoint::EnvLookup Lookup() const
        {
            return [this](const char *name) -> const char *
            {
                const auto it = values_.find(name);
                return it == values_.end() ? nullptr : it->second.c_str();
            };
        }

    private:
        std::map<std::string, std::string> values_;
    };

    struct ParseOutcome
    {
        bool ok = false;
        Settings settings;
        std::string error;
    };

    ParseOutcome Parse(const FakeEnv &env)
    {
        ParseOutcome outcome;
        outcome.ok = client_endpoint::ParseSettings(env.Lookup(), outcome.settings, outcome.error);
        return outcome;
    }

    // 可脚本化的外部来源:返回预设结果并记录被调用次数。
    class FakeSource final : public ExternalSource
    {
    public:
        static std::unique_ptr<FakeSource> Succeed(std::string host, uint32_t port)
        {
            auto source = std::make_unique<FakeSource>();
            source->ok_ = true;
            source->advertised_.host = std::move(host);
            source->advertised_.port = port;
            return source;
        }

        static std::unique_ptr<FakeSource> Fail(std::string error)
        {
            auto source = std::make_unique<FakeSource>();
            source->ok_ = false;
            source->error_ = std::move(error);
            return source;
        }

        bool Fetch(Advertised &out, std::string &error) override
        {
            ++calls_;
            if (!ok_)
            {
                error = error_;
                return false;
            }
            out = advertised_;
            return true;
        }

        int Calls() const { return calls_; }

    private:
        bool ok_ = false;
        Advertised advertised_;
        std::string error_;
        int calls_ = 0;
    };

    EndpointComp MakeEndpoint(const std::string &ip, uint32_t port)
    {
        EndpointComp endpoint;
        endpoint.set_ip(ip);
        endpoint.set_port(port);
        return endpoint;
    }

    NodeInfo MakeNode(const EndpointComp &endpoint, const std::optional<EndpointComp> &clientEndpoint)
    {
        NodeInfo node;
        *node.mutable_endpoint() = endpoint;
        if (clientEndpoint)
        {
            *node.mutable_client_endpoint() = *clientEndpoint;
        }
        return node;
    }

    bool Contains(const std::string &haystack, const std::string &needle)
    {
        return haystack.find(needle) != std::string::npos;
    }

} // namespace

// ===========================================================================
// ParseSettings:合法组合
// ===========================================================================

TEST(ClientEndpointParseSettings, AbsentEnvMeansNone)
{
    const auto outcome = Parse(FakeEnv{});
    ASSERT_TRUE(outcome.ok) << outcome.error;
    EXPECT_EQ(outcome.settings.source, SourceKind::kNone);
    EXPECT_TRUE(outcome.settings.host.empty());
    EXPECT_EQ(outcome.settings.port, 0u);
    EXPECT_FALSE(outcome.settings.required);
}

TEST(ClientEndpointParseSettings, EmptyValuesAreTreatedAsAbsent)
{
    FakeEnv env;
    env.Set("CLIENT_ENDPOINT_SOURCE", "").Set("CLIENT_ENDPOINT_REQUIRED", "");
    const auto outcome = Parse(env);
    ASSERT_TRUE(outcome.ok) << outcome.error;
    EXPECT_EQ(outcome.settings.source, SourceKind::kNone);
    EXPECT_FALSE(outcome.settings.required);
}

TEST(ClientEndpointParseSettings, NoneIgnoresHostAndPort)
{
    // podip 形态的 Fleet 模板可能仍带 HOST;none 下不得因此致命,也不得把它当成自报地址。
    FakeEnv env;
    env.Set("CLIENT_ENDPOINT_SOURCE", "none")
        .Set("CLIENT_ENDPOINT_HOST", "127.0.0.1")
        .Set("CLIENT_ENDPOINT_PORT", "not-a-port");
    const auto outcome = Parse(env);
    ASSERT_TRUE(outcome.ok) << outcome.error;
    EXPECT_EQ(outcome.settings.source, SourceKind::kNone);
    EXPECT_TRUE(outcome.settings.host.empty());
    EXPECT_EQ(outcome.settings.port, 0u);
}

TEST(ClientEndpointParseSettings, StaticWithHostAndPort)
{
    FakeEnv env;
    env.Set("CLIENT_ENDPOINT_SOURCE", "static")
        .Set("CLIENT_ENDPOINT_HOST", "gate-1.z1.example.com")
        .Set("CLIENT_ENDPOINT_PORT", "30001")
        .Set("CLIENT_ENDPOINT_REQUIRED", "1");
    const auto outcome = Parse(env);
    ASSERT_TRUE(outcome.ok) << outcome.error;
    EXPECT_EQ(outcome.settings.source, SourceKind::kStatic);
    EXPECT_EQ(outcome.settings.host, "gate-1.z1.example.com");
    EXPECT_EQ(outcome.settings.port, 30001u);
    EXPECT_TRUE(outcome.settings.required);
}

TEST(ClientEndpointParseSettings, StaticPortBoundsAreInclusive)
{
    const std::pair<std::string, uint32_t> cases[] = {{"1", 1u}, {"65535", 65535u}};
    for (const auto &[raw, expected] : cases)
    {
        FakeEnv env;
        env.Set("CLIENT_ENDPOINT_SOURCE", "static").Set("CLIENT_ENDPOINT_HOST", "127.0.0.1").Set("CLIENT_ENDPOINT_PORT", raw);
        const auto outcome = Parse(env);
        ASSERT_TRUE(outcome.ok) << "port=" << raw << " error=" << outcome.error;
        EXPECT_EQ(outcome.settings.port, expected);
    }
}

TEST(ClientEndpointParseSettings, RequiredZeroIsFalse)
{
    FakeEnv env;
    env.Set("CLIENT_ENDPOINT_SOURCE", "static")
        .Set("CLIENT_ENDPOINT_HOST", "127.0.0.1")
        .Set("CLIENT_ENDPOINT_PORT", "20000")
        .Set("CLIENT_ENDPOINT_REQUIRED", "0");
    const auto outcome = Parse(env);
    ASSERT_TRUE(outcome.ok) << outcome.error;
    EXPECT_FALSE(outcome.settings.required);
}

TEST(ClientEndpointParseSettings, AgonesWithoutHostUsesAgonesAddress)
{
    FakeEnv env;
    env.Set("CLIENT_ENDPOINT_SOURCE", "agones").Set("CLIENT_ENDPOINT_REQUIRED", "1");
    const auto outcome = Parse(env);
    ASSERT_TRUE(outcome.ok) << outcome.error;
    EXPECT_EQ(outcome.settings.source, SourceKind::kAgones);
    EXPECT_TRUE(outcome.settings.host.empty());
    EXPECT_EQ(outcome.settings.port, 0u);
    EXPECT_TRUE(outcome.settings.required);
}

TEST(ClientEndpointParseSettings, AgonesWithHostOverride)
{
    FakeEnv env;
    env.Set("CLIENT_ENDPOINT_SOURCE", "agones").Set("CLIENT_ENDPOINT_HOST", "127.0.0.1");
    const auto outcome = Parse(env);
    ASSERT_TRUE(outcome.ok) << outcome.error;
    EXPECT_EQ(outcome.settings.source, SourceKind::kAgones);
    EXPECT_EQ(outcome.settings.host, "127.0.0.1");
}

// ===========================================================================
// ParseSettings:非法组合(线上 = LOG_FATAL)
// ===========================================================================

TEST(ClientEndpointParseSettings, UnknownSourceIsRejected)
{
    for (const std::string raw : {"k8s", "STATIC", "Agones", " static", "static "})
    {
        FakeEnv env;
        env.Set("CLIENT_ENDPOINT_SOURCE", raw).Set("CLIENT_ENDPOINT_HOST", "127.0.0.1").Set("CLIENT_ENDPOINT_PORT", "30000");
        const auto outcome = Parse(env);
        EXPECT_FALSE(outcome.ok) << "source='" << raw << "' 不应被接受";
        EXPECT_TRUE(Contains(outcome.error, "CLIENT_ENDPOINT_SOURCE")) << outcome.error;
    }
}

TEST(ClientEndpointParseSettings, StaticWithoutHostIsRejected)
{
    FakeEnv env;
    env.Set("CLIENT_ENDPOINT_SOURCE", "static").Set("CLIENT_ENDPOINT_PORT", "30000");
    const auto outcome = Parse(env);
    EXPECT_FALSE(outcome.ok);
    EXPECT_TRUE(Contains(outcome.error, "CLIENT_ENDPOINT_HOST")) << outcome.error;
}

TEST(ClientEndpointParseSettings, StaticWithoutPortIsRejected)
{
    FakeEnv env;
    env.Set("CLIENT_ENDPOINT_SOURCE", "static").Set("CLIENT_ENDPOINT_HOST", "127.0.0.1");
    const auto outcome = Parse(env);
    EXPECT_FALSE(outcome.ok);
    EXPECT_TRUE(Contains(outcome.error, "CLIENT_ENDPOINT_PORT")) << outcome.error;
}

TEST(ClientEndpointParseSettings, StaticWithInvalidPortIsRejected)
{
    for (const std::string raw : {"0", "65536", "-1", "+80", " 80", "80 ", "80x", "abc", "4294967296"})
    {
        FakeEnv env;
        env.Set("CLIENT_ENDPOINT_SOURCE", "static").Set("CLIENT_ENDPOINT_HOST", "127.0.0.1").Set("CLIENT_ENDPOINT_PORT", raw);
        const auto outcome = Parse(env);
        EXPECT_FALSE(outcome.ok) << "port='" << raw << "' 不应被接受";
        EXPECT_TRUE(Contains(outcome.error, "CLIENT_ENDPOINT_PORT")) << outcome.error;
    }
}

TEST(ClientEndpointParseSettings, HostWithSchemePortOrPathIsRejected)
{
    for (const std::string raw : {"http://127.0.0.1", "127.0.0.1:30000", "gate.example.com/x", "user@gate.example.com",
                                  "gate example.com", "[::1]"})
    {
        FakeEnv env;
        env.Set("CLIENT_ENDPOINT_SOURCE", "static").Set("CLIENT_ENDPOINT_HOST", raw).Set("CLIENT_ENDPOINT_PORT", "30000");
        const auto outcome = Parse(env);
        EXPECT_FALSE(outcome.ok) << "host='" << raw << "' 不应被接受";
        EXPECT_TRUE(Contains(outcome.error, "CLIENT_ENDPOINT_HOST")) << outcome.error;
    }
}

TEST(ClientEndpointParseSettings, AgonesWithPortIsRejected)
{
    FakeEnv env;
    env.Set("CLIENT_ENDPOINT_SOURCE", "agones").Set("CLIENT_ENDPOINT_PORT", "20000");
    const auto outcome = Parse(env);
    EXPECT_FALSE(outcome.ok);
    EXPECT_TRUE(Contains(outcome.error, "CLIENT_ENDPOINT_PORT")) << outcome.error;
}

TEST(ClientEndpointParseSettings, AgonesWithInvalidHostOverrideIsRejected)
{
    FakeEnv env;
    env.Set("CLIENT_ENDPOINT_SOURCE", "agones").Set("CLIENT_ENDPOINT_HOST", "127.0.0.1:7000");
    const auto outcome = Parse(env);
    EXPECT_FALSE(outcome.ok);
    EXPECT_TRUE(Contains(outcome.error, "CLIENT_ENDPOINT_HOST")) << outcome.error;
}

TEST(ClientEndpointParseSettings, InvalidRequiredIsRejected)
{
    for (const std::string raw : {"2", "true", "yes", "on", " 1"})
    {
        FakeEnv env;
        env.Set("CLIENT_ENDPOINT_SOURCE", "static")
            .Set("CLIENT_ENDPOINT_HOST", "127.0.0.1")
            .Set("CLIENT_ENDPOINT_PORT", "30000")
            .Set("CLIENT_ENDPOINT_REQUIRED", raw);
        const auto outcome = Parse(env);
        EXPECT_FALSE(outcome.ok) << "required='" << raw << "' 不应被接受";
        EXPECT_TRUE(Contains(outcome.error, "CLIENT_ENDPOINT_REQUIRED")) << outcome.error;
    }
}

TEST(ClientEndpointParseSettings, NoneWithRequiredIsContradiction)
{
    for (const std::string source : {"", "none"})
    {
        FakeEnv env;
        env.Set("CLIENT_ENDPOINT_REQUIRED", "1");
        if (!source.empty())
        {
            env.Set("CLIENT_ENDPOINT_SOURCE", source);
        }
        const auto outcome = Parse(env);
        EXPECT_FALSE(outcome.ok) << "source='" << source << "' + REQUIRED=1 必须判为矛盾配置";
        EXPECT_TRUE(Contains(outcome.error, "CLIENT_ENDPOINT_REQUIRED")) << outcome.error;
    }
}

TEST(ClientEndpointParseSettings, FailureLeavesOutputUntouched)
{
    Settings settings;
    settings.source = SourceKind::kStatic;
    settings.host = "sentinel";
    settings.port = 4242;
    settings.required = true;

    FakeEnv env;
    env.Set("CLIENT_ENDPOINT_SOURCE", "bogus");
    std::string error;
    ASSERT_FALSE(client_endpoint::ParseSettings(env.Lookup(), settings, error));
    EXPECT_EQ(settings.source, SourceKind::kStatic);
    EXPECT_EQ(settings.host, "sentinel");
    EXPECT_EQ(settings.port, 4242u);
    EXPECT_TRUE(settings.required);
}

TEST(ClientEndpointParseSettings, EmptyLookupIsRejected)
{
    Settings settings;
    std::string error;
    EXPECT_FALSE(client_endpoint::ParseSettings(client_endpoint::EnvLookup{}, settings, error));
    EXPECT_FALSE(error.empty());
}

// ===========================================================================
// Resolve
// ===========================================================================

TEST(ClientEndpointResolve, NoneYieldsNoClientEndpoint)
{
    Settings settings;
    std::optional<EndpointComp> out = MakeEndpoint("stale", 1);
    std::string error;
    ASSERT_TRUE(client_endpoint::Resolve(settings, nullptr, out, error)) << error;
    EXPECT_FALSE(out.has_value());
}

TEST(ClientEndpointResolve, StaticUsesSettings)
{
    Settings settings;
    settings.source = SourceKind::kStatic;
    settings.host = "127.0.0.1";
    settings.port = 30000;
    std::optional<EndpointComp> out;
    std::string error;
    ASSERT_TRUE(client_endpoint::Resolve(settings, nullptr, out, error)) << error;
    ASSERT_TRUE(out.has_value());
    EXPECT_EQ(out->ip(), "127.0.0.1");
    EXPECT_EQ(out->port(), 30000u);
}

TEST(ClientEndpointResolve, StaticWithUnusableSettingsFails)
{
    // 手工拼的 Settings 绕过 ParseSettings 时,Resolve 仍须复核,不得产出半填地址。
    Settings settings;
    settings.source = SourceKind::kStatic;
    settings.host = "127.0.0.1";
    settings.port = 0;
    std::optional<EndpointComp> out;
    std::string error;
    EXPECT_FALSE(client_endpoint::Resolve(settings, nullptr, out, error));
    EXPECT_FALSE(out.has_value());
    EXPECT_FALSE(error.empty());
}

TEST(ClientEndpointResolve, AgonesUsesFetchedAddress)
{
    Settings settings;
    settings.source = SourceKind::kAgones;
    auto source = FakeSource::Succeed("203.0.113.7", 7003);
    std::optional<EndpointComp> out;
    std::string error;
    ASSERT_TRUE(client_endpoint::Resolve(settings, source.get(), out, error)) << error;
    ASSERT_TRUE(out.has_value());
    EXPECT_EQ(out->ip(), "203.0.113.7");
    EXPECT_EQ(out->port(), 7003u);
    EXPECT_EQ(source->Calls(), 1) << "来源自带有界重试,Resolve 只调用一次";
}

TEST(ClientEndpointResolve, AgonesHostOverrideKeepsAgonesPort)
{
    Settings settings;
    settings.source = SourceKind::kAgones;
    settings.host = "127.0.0.1";
    auto source = FakeSource::Succeed("172.18.0.2", 7005);
    std::optional<EndpointComp> out;
    std::string error;
    ASSERT_TRUE(client_endpoint::Resolve(settings, source.get(), out, error)) << error;
    ASSERT_TRUE(out.has_value());
    EXPECT_EQ(out->ip(), "127.0.0.1");
    EXPECT_EQ(out->port(), 7005u);
}

TEST(ClientEndpointResolve, AgonesSourceFailureFails)
{
    Settings settings;
    settings.source = SourceKind::kAgones;
    auto source = FakeSource::Fail("sidecar unreachable after 30 attempts");
    std::optional<EndpointComp> out;
    std::string error;
    EXPECT_FALSE(client_endpoint::Resolve(settings, source.get(), out, error));
    EXPECT_FALSE(out.has_value());
    EXPECT_TRUE(Contains(error, "sidecar unreachable after 30 attempts")) << error;
}

TEST(ClientEndpointResolve, AgonesWithoutSourceFails)
{
    Settings settings;
    settings.source = SourceKind::kAgones;
    std::optional<EndpointComp> out;
    std::string error;
    EXPECT_FALSE(client_endpoint::Resolve(settings, nullptr, out, error));
    EXPECT_FALSE(out.has_value());
    EXPECT_FALSE(error.empty());
}

TEST(ClientEndpointResolve, AgonesUnusableAddressFails)
{
    Settings settings;
    settings.source = SourceKind::kAgones;
    const std::pair<std::string, uint32_t> unusable[] = {{"", 7000u}, {"203.0.113.7", 0u}, {"203.0.113.7", 70000u}};
    for (const auto &[host, port] : unusable)
    {
        auto source = FakeSource::Succeed(host, port);
        std::optional<EndpointComp> out;
        std::string error;
        EXPECT_FALSE(client_endpoint::Resolve(settings, source.get(), out, error));
        EXPECT_FALSE(out.has_value());
        EXPECT_FALSE(error.empty());
    }
}

TEST(ClientEndpointResolve, StaticWithNonBareHostFails)
{
    // 端口合法、host 带 scheme:手工拼的 Settings 绕过 ParseSettings 时,Resolve 的裸主机复核仍须拦住,
    // 否则 "http://x" 会原样下发给客户端,客户端只会连不上且服务端无报错。
    Settings settings;
    settings.source = SourceKind::kStatic;
    settings.host = "http://x";
    settings.port = 30000;
    std::optional<EndpointComp> out;
    std::string error;
    EXPECT_FALSE(client_endpoint::Resolve(settings, nullptr, out, error));
    EXPECT_FALSE(out.has_value());
    EXPECT_FALSE(error.empty());
}

TEST(ClientEndpointResolve, AgonesNonBareHostOverrideFailsBeforeFetch)
{
    // 先校验、后 I/O:覆盖值非法时不得去打 sidecar(真实来源最坏要有界阻塞约 60s)。
    Settings settings;
    settings.source = SourceKind::kAgones;
    settings.host = "127.0.0.1:7000";
    auto source = FakeSource::Succeed("203.0.113.7", 7003);
    std::optional<EndpointComp> out;
    std::string error;
    EXPECT_FALSE(client_endpoint::Resolve(settings, source.get(), out, error));
    EXPECT_FALSE(out.has_value());
    EXPECT_FALSE(error.empty());
    EXPECT_EQ(source->Calls(), 0) << "host 覆盖值非法必须在 Fetch 之前就拒绝";
}

// ===========================================================================
// SourceKindName(进致命日志;取值必须与 CLIENT_ENDPOINT_SOURCE 的 env 取值一致)
// ===========================================================================

TEST(ClientEndpointSourceKindName, MatchesEnvValuesAndOutOfRangeIsUnknown)
{
    EXPECT_EQ(std::string(client_endpoint::SourceKindName(SourceKind::kNone)), "none");
    EXPECT_EQ(std::string(client_endpoint::SourceKindName(SourceKind::kStatic)), "static");
    EXPECT_EQ(std::string(client_endpoint::SourceKindName(SourceKind::kAgones)), "agones");
    EXPECT_EQ(std::string(client_endpoint::SourceKindName(SourceKind::kCount)), "unknown");
}

// ===========================================================================
// ExternalSourceFactory 注册
// ===========================================================================

namespace
{
    std::unique_ptr<ExternalSource> MakeFakeSourceForFactoryTest()
    {
        return FakeSource::Succeed("127.0.0.1", 7000);
    }
} // namespace

TEST(ClientEndpointFactory, RegisterAndClear)
{
    client_endpoint::SetExternalSourceFactory(&MakeFakeSourceForFactoryTest);
    const auto factory = client_endpoint::GetExternalSourceFactory();
    ASSERT_TRUE(factory != nullptr);
    EXPECT_TRUE(factory() != nullptr);

    client_endpoint::SetExternalSourceFactory(nullptr);
    EXPECT_TRUE(client_endpoint::GetExternalSourceFactory() == nullptr);
}

// ===========================================================================
// IsUsable / ClientFacing(消费方选址,与 Go shared/clientendpoint.Select 同口径)
// ===========================================================================

TEST(ClientEndpointIsUsable, RequiresHostAndPortInRange)
{
    EXPECT_TRUE(client_endpoint::IsUsable(MakeEndpoint("127.0.0.1", 1)));
    EXPECT_TRUE(client_endpoint::IsUsable(MakeEndpoint("gate-0.example.com", 65535)));
    EXPECT_FALSE(client_endpoint::IsUsable(MakeEndpoint("", 30000)));
    EXPECT_FALSE(client_endpoint::IsUsable(MakeEndpoint("127.0.0.1", 0)));
    EXPECT_FALSE(client_endpoint::IsUsable(MakeEndpoint("127.0.0.1", 65536)));
    EXPECT_FALSE(client_endpoint::IsUsable(EndpointComp{}));
}

TEST(ClientEndpointClientFacing, UsableClientEndpointWins)
{
    const auto node = MakeNode(MakeEndpoint("10.244.0.5", 18000), MakeEndpoint("127.0.0.1", 30001));
    for (const bool required : {false, true})
    {
        const auto chosen = client_endpoint::ClientFacing(node, required);
        ASSERT_TRUE(chosen.has_value()) << "required=" << required;
        EXPECT_EQ(chosen->ip(), "127.0.0.1");
        EXPECT_EQ(chosen->port(), 30001u);
    }
}

TEST(ClientEndpointClientFacing, MissingFallsBackToEndpointWhenNotRequired)
{
    const auto node = MakeNode(MakeEndpoint("10.244.0.5", 20000), std::nullopt);
    const auto chosen = client_endpoint::ClientFacing(node, false);
    ASSERT_TRUE(chosen.has_value());
    EXPECT_EQ(chosen->ip(), "10.244.0.5");
    EXPECT_EQ(chosen->port(), 20000u);
}

TEST(ClientEndpointClientFacing, HalfFilledIsTreatedAsMissing)
{
    const EndpointComp endpoint = MakeEndpoint("10.244.0.5", 20000);
    for (const auto &halfFilled : {MakeEndpoint("127.0.0.1", 0), MakeEndpoint("", 30001)})
    {
        const auto node = MakeNode(endpoint, halfFilled);

        const auto fallback = client_endpoint::ClientFacing(node, false);
        ASSERT_TRUE(fallback.has_value());
        EXPECT_EQ(fallback->ip(), "10.244.0.5") << "半填不可下发,必须回落 endpoint";
        EXPECT_EQ(fallback->port(), 20000u);

        EXPECT_FALSE(client_endpoint::ClientFacing(node, true).has_value()) << "required 下半填等同缺失,必须拒绝";
    }
}

TEST(ClientEndpointClientFacing, MissingIsRejectedWhenRequired)
{
    const auto node = MakeNode(MakeEndpoint("10.244.0.5", 20000), std::nullopt);
    EXPECT_FALSE(client_endpoint::ClientFacing(node, true).has_value());
}

TEST(ClientEndpointClientFacing, UnusableEndpointFallbackIsRejected)
{
    const auto node = MakeNode(MakeEndpoint("10.244.0.5", 0), std::nullopt);
    EXPECT_FALSE(client_endpoint::ClientFacing(node, false).has_value());
}
