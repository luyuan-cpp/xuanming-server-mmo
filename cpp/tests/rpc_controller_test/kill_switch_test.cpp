// RPC 热关停(docs/design/rpc-kill-switch.md)的单测:规则值 / 键解析、匹配顺序、范围与豁免、新鲜度,
// 以及 etcd 同步(传输替身)。不起 etcd、不跑 EventLoop:同步类的定时器在没有 loop 的线程上不武装,
// 用例直接驱动传输回调。进程级实例(KillSwitch::Process)的用例在夹具里清空身份与规则,互不影响。
// gRpcMethodRegistry 由 rpc_channel_loopback_test.cpp 的全局 Environment 在所有用例之前填充。

#include <gtest/gtest.h>

#include <chrono>
#include <map>
#include <memory>
#include <optional>
#include <string>
#include <vector>

#include "kill_switch/kill_switch.h"
#include "node/system/etcd/kill_switch_etcd_sync.h"
#include "rpc/service_metadata/battle_node_service_metadata.h"
#include "rpc/service_metadata/chat_service_metadata.h"
#include "rpc/service_metadata/gate_service_service_metadata.h"
#include "rpc/service_metadata/player_lifecycle_service_metadata.h"
#include "rpc/service_metadata/scene_node_service_service_metadata.h"
#include "rpc/service_metadata/scene_service_metadata.h"

namespace
{
using kill_switch::Identity;
using kill_switch::KillSwitch;
using kill_switch::MethodName;
using kill_switch::Rule;
using kill_switch::Scope;
using SteadyTime = std::chrono::steady_clock::time_point;

constexpr const char *kPrefix = "/mmorpg/killswitch/";
constexpr MethodName kSendChat{"chatpb.ClientPlayerChat", "SendChat"};
constexpr MethodName kUseSkill{"SceneSkillClientPlayer", "ReleaseSkill"};
constexpr MethodName kGetSkills{"SceneSkillClientPlayer", "ListSkills"};

// 可拨动的假时钟:KillSwitch 只经它取时间。
struct FakeClock
{
    std::shared_ptr<SteadyTime> now = std::make_shared<SteadyTime>(std::chrono::steady_clock::now());
    KillSwitch::Clock AsClock() const
    {
        return [now = now] { return *now; };
    }
    void Advance(std::chrono::seconds by) const { *now += by; }
};

Identity SceneIdentity()
{
    return Identity{{"SceneNodeService"}, {"uuid-scene-a"}};
}

// ── 规则值 ──────────────────────────────────────────────────────────────

TEST(KillSwitchRule, EmptyValueIsAnExplicitAllow)
{
    const auto rule = kill_switch::ParseRule("  \n");
    ASSERT_TRUE(rule.has_value());
    EXPECT_FALSE(rule->deny);
}

TEST(KillSwitchRule, BareWordsAreCaseInsensitive)
{
    for (const char *word : {"1", "true", "ON", "Yes", "deny"})
    {
        const auto rule = kill_switch::ParseRule(word);
        ASSERT_TRUE(rule.has_value()) << word;
        EXPECT_TRUE(rule->deny) << word;
    }
    for (const char *word : {"0", "FALSE", "off", "No", "allow"})
    {
        const auto rule = kill_switch::ParseRule(word);
        ASSERT_TRUE(rule.has_value()) << word;
        EXPECT_FALSE(rule->deny) << word;
    }
    EXPECT_FALSE(kill_switch::ParseRule("maybe").has_value());
}

TEST(KillSwitchRule, JsonFieldsAndUnknownKeys)
{
    const auto rule = kill_switch::ParseRule(R"({"deny":true,"reason":"db overload","code":9,"owner":"ops"})");
    ASSERT_TRUE(rule.has_value());
    EXPECT_TRUE(rule->deny);
    EXPECT_EQ(rule->reason, "db overload");
    EXPECT_EQ(rule->code, 9u);

    const auto nulls = kill_switch::ParseRule(R"({"deny":null,"reason":null,"code":null})");
    ASSERT_TRUE(nulls.has_value());
    EXPECT_FALSE(nulls->deny);
    EXPECT_TRUE(nulls->reason.empty());
    EXPECT_EQ(nulls->code, 0u);
}

TEST(KillSwitchRule, JsonWithWrongTypesIsMalformed)
{
    for (const char *raw : {R"({"deny":"yes"})", R"({"reason":5})", R"({"code":-1})", R"({"code":1.5})",
                            R"({"code":"9"})", R"({"deny":true)", R"({"deny":true}})"})
    {
        EXPECT_FALSE(kill_switch::ParseRule(raw).has_value()) << raw;
    }
}

TEST(KillSwitchRule, StatusCodeFallsBackToUnavailable)
{
    Rule rule;
    EXPECT_EQ(rule.GrpcStatusCode(), kill_switch::kGrpcUnavailable);
    rule.code = 9;
    EXPECT_EQ(rule.GrpcStatusCode(), 9u);
    rule.code = 17;
    EXPECT_EQ(rule.GrpcStatusCode(), kill_switch::kGrpcUnavailable);
}

// ── 规则键 ──────────────────────────────────────────────────────────────

TEST(KillSwitchRuleKey, ScopesAndTargets)
{
    const auto all = kill_switch::ParseRuleKey("ClientPlayerChat/SendChat");
    ASSERT_TRUE(all.has_value());
    EXPECT_EQ(all->scope, Scope::kAll);
    EXPECT_EQ(all->target, "ClientPlayerChat/SendChat");

    const auto byType = kill_switch::ParseRuleKey("type/SceneNodeService/*");
    ASSERT_TRUE(byType.has_value());
    EXPECT_EQ(byType->scope, Scope::kNodeType);
    EXPECT_EQ(byType->scopeValue, "SceneNodeService");
    EXPECT_EQ(byType->target, "*");

    const auto byInstance = kill_switch::ParseRuleKey("//instance/uuid-1/SceneSkillClientPlayer/ReleaseSkill");
    ASSERT_TRUE(byInstance.has_value());
    EXPECT_EQ(byInstance->scope, Scope::kInstance);
    EXPECT_EQ(byInstance->scopeValue, "uuid-1");
    EXPECT_EQ(byInstance->target, "SceneSkillClientPlayer/ReleaseSkill");
}

TEST(KillSwitchRuleKey, IncompleteScopesAreNotRules)
{
    for (const char *key : {"", "/", "type/", "type/SceneNodeService", "type/SceneNodeService/", "instance//*"})
    {
        EXPECT_FALSE(kill_switch::ParseRuleKey(key).has_value()) << key;
    }
}

// ── 匹配顺序 ────────────────────────────────────────────────────────────

TEST(KillSwitchMatch, TargetsGoFromExactToWildcard)
{
    const std::vector<std::string> expected{"chatpb.ClientPlayerChat/SendChat", "ClientPlayerChat/SendChat",
                                            "chatpb.ClientPlayerChat/*", "ClientPlayerChat/*", "*"};
    EXPECT_EQ(kill_switch::MatchTargets(kSendChat), expected);
}

TEST(KillSwitchMatch, ServiceWithoutPackageHasNoDuplicates)
{
    const std::vector<std::string> expected{"Scene/CreateScene", "Scene/*", "*"};
    EXPECT_EQ(kill_switch::MatchTargets(MethodName{"Scene", "CreateScene"}), expected);
}

TEST(KillSwitchMatch, ExactRulesOnlyDropsWildcards)
{
    const std::vector<std::string> expected{"chatpb.ClientPlayerChat/SendChat", "ClientPlayerChat/SendChat"};
    EXPECT_EQ(kill_switch::MatchTargets(kSendChat, kill_switch::Coverage::kExactRulesOnly), expected);
}

TEST(KillSwitchMatch, ParseFullMethod)
{
    const auto parsed = kill_switch::ParseFullMethod("/chatpb.ClientPlayerChat/SendChat");
    ASSERT_TRUE(parsed.has_value());
    EXPECT_EQ(parsed->service, "chatpb.ClientPlayerChat");
    EXPECT_EQ(parsed->method, "SendChat");
    EXPECT_FALSE(kill_switch::ParseFullMethod("/NoMethod").has_value());
    EXPECT_FALSE(kill_switch::ParseFullMethod("/Svc/").has_value());
}

// ── 判定 ────────────────────────────────────────────────────────────────

TEST(KillSwitchCheck, NoRulesAllowsEverything)
{
    KillSwitch ks;
    EXPECT_FALSE(ks.Check(kSendChat).has_value());
    ks.ReplaceAll({});
    EXPECT_FALSE(ks.Check(kSendChat).has_value());
    EXPECT_EQ(ks.ApplicableRuleCount(), 0u);
    EXPECT_TRUE(ks.Synced());
}

TEST(KillSwitchCheck, ShortOrFullServiceNameBothMatch)
{
    KillSwitch ks;
    ks.ReplaceAll({{"ClientPlayerChat/SendChat", "true"}});
    EXPECT_TRUE(ks.Check(kSendChat).has_value());
    ks.ReplaceAll({{"chatpb.ClientPlayerChat/*", R"({"deny":true,"reason":"chat storm"})"}});
    const auto rule = ks.Check(kSendChat);
    ASSERT_TRUE(rule.has_value());
    EXPECT_EQ(rule->reason, "chat storm");
}

TEST(KillSwitchCheck, ExactAllowExemptsFromServiceDeny)
{
    KillSwitch ks;
    ks.ReplaceAll({{"SceneSkillClientPlayer/*", "true"}, {"SceneSkillClientPlayer/ListSkills", "false"}});
    EXPECT_TRUE(ks.Check(kUseSkill).has_value());
    EXPECT_FALSE(ks.Check(kGetSkills).has_value());
    EXPECT_EQ(ks.ApplicableRuleCount(), 2u);
}

TEST(KillSwitchCheck, InstanceScopeWinsOverTypeAndAll)
{
    KillSwitch ks;
    ks.SetIdentity(SceneIdentity());
    // 类型整体关停,但本实例豁免。
    ks.ReplaceAll({{"type/SceneNodeService/*", "true"}, {"instance/uuid-scene-a/*", "false"}});
    EXPECT_FALSE(ks.Check(kUseSkill).has_value());

    // 实例整体关停,全部范围的方法豁免救不回来(范围优先于目标精度)。
    ks.ReplaceAll({{"instance/uuid-scene-a/*", "true"}, {"SceneSkillClientPlayer/ReleaseSkill", "false"}});
    EXPECT_TRUE(ks.Check(kUseSkill).has_value());

    // 类型范围先于全部范围。
    ks.ReplaceAll({{"type/SceneNodeService/SceneSkillClientPlayer/ReleaseSkill", "false"}, {"*", "true"}});
    EXPECT_FALSE(ks.Check(kUseSkill).has_value());
    EXPECT_TRUE(ks.Check(kGetSkills).has_value());
}

TEST(KillSwitchCheck, RulesForOtherNodesDoNotApply)
{
    KillSwitch ks;
    ks.SetIdentity(SceneIdentity());
    ks.ReplaceAll({{"type/GateNodeService/*", "true"}, {"instance/uuid-other/*", "true"}});
    EXPECT_FALSE(ks.Check(kUseSkill).has_value());
    EXPECT_EQ(ks.ApplicableRuleCount(), 0u) << "rules scoped to other nodes are not counted";
}

TEST(KillSwitchCheck, IdentitySetLaterActivatesScopedRules)
{
    KillSwitch ks;
    ks.ReplaceAll({{"type/SceneNodeService/*", "true"}});
    EXPECT_FALSE(ks.Check(kUseSkill).has_value()) << "without an identity only global rules apply";
    ks.SetIdentity(SceneIdentity());
    EXPECT_TRUE(ks.Check(kUseSkill).has_value());
}

TEST(KillSwitchCheck, MalformedValueCountsAsAbsent)
{
    KillSwitch ks;
    ks.ReplaceAll({{"SceneSkillClientPlayer/*", R"({"deny":tru})"}});
    EXPECT_FALSE(ks.Check(kUseSkill).has_value());
    EXPECT_EQ(ks.ApplicableRuleCount(), 0u);

    // watch 上把一条生效的规则改坏:与全量快照同口径,当作不存在(不保留旧值)。
    ks.Put("SceneSkillClientPlayer/ReleaseSkill", "true");
    EXPECT_TRUE(ks.Check(kUseSkill).has_value());
    ks.Put("SceneSkillClientPlayer/ReleaseSkill", "nonsense");
    EXPECT_FALSE(ks.Check(kUseSkill).has_value());
}

TEST(KillSwitchCheck, IncrementalPutAndErase)
{
    KillSwitch ks;
    ks.Put("ClientPlayerChat/SendChat", "deny");
    EXPECT_TRUE(ks.Check(kSendChat).has_value());
    ks.Erase("ClientPlayerChat/SendChat");
    EXPECT_FALSE(ks.Check(kSendChat).has_value());
    ks.Erase("ClientPlayerChat/SendChat"); // 不存在的键:无副作用
    EXPECT_FALSE(ks.Check(kSendChat).has_value());
}

TEST(KillSwitchCheck, StaleSnapshotAllowsEverything)
{
    const FakeClock clock;
    KillSwitch ks(std::chrono::seconds(60), clock.AsClock());
    ks.ReplaceAll({{"*", "true"}});
    EXPECT_TRUE(ks.Check(kSendChat).has_value());
    EXPECT_TRUE(ks.Synced());

    clock.Advance(std::chrono::seconds(59));
    EXPECT_TRUE(ks.Check(kSendChat).has_value()) << "a short etcd outage keeps the rules";

    clock.Advance(std::chrono::seconds(2));
    EXPECT_FALSE(ks.Check(kSendChat).has_value()) << "past StaleAfter the snapshot is void";
    EXPECT_FALSE(ks.Synced());
    EXPECT_EQ(ks.ApplicableRuleCount(), 0u);

    ks.MarkSynced(); // 又读到了 etcd
    EXPECT_TRUE(ks.Check(kSendChat).has_value());
}

TEST(KillSwitchCheck, ConflictingRulesAcrossOwnScopesPreferDeny)
{
    KillSwitch ks;
    // 一个进程以两种节点类型注册(Go match = MatchNodeService + TeamNodeService)。
    ks.SetIdentity(Identity{{"MatchNodeService", "TeamNodeService"}, {"uuid-m", "uuid-t"}});
    ks.ReplaceAll({{"type/MatchNodeService/TeamAuthority/*", "false"}, {"type/TeamNodeService/TeamAuthority/*", "true"}});
    EXPECT_TRUE(ks.Check(MethodName{"teampb.TeamAuthority", "GetTeam"}).has_value());
}

// ── 进程级实例与派发入口用的判定 ──────────────────────────────────────────

class ProcessKillSwitch : public ::testing::Test
{
protected:
    void SetUp() override { Reset(); }
    void TearDown() override { Reset(); }
    static void Reset()
    {
        KillSwitch::Process().SetIdentity({});
        KillSwitch::Process().ReplaceAll({});
    }
};

TEST_F(ProcessKillSwitch, ProtectedMethodsIgnoreWildcardRules)
{
    for (const uint32_t id : {GateNodeHandshakeMessageId, SceneNodeHandshakeMessageId, GateGmGracefulShutdownMessageId,
                              SceneProcessClientPlayerMessageMessageId, ScenePlayerExitGameMessageId,
                              SceneNodeGrpcReleasePlayerMessageId, SceneNodeGrpcAssetAbortDebitMessageId,
                              BattleNodeDestroyBattleMessageId})
    {
        EXPECT_TRUE(kill_switch::IsProtectedMessageId(id)) << id;
    }
    EXPECT_FALSE(kill_switch::IsProtectedMessageId(SceneCreateSceneMessageId));

    // 整体关停(`*`、`服务/*`)挡新工作,不挡握手、收尾、补偿。
    KillSwitch::Process().ReplaceAll({{"*", "true"}, {"scene_node.SceneNodeGrpc/*", "true"}});
    EXPECT_FALSE(kill_switch::CheckMessageId(SceneNodeHandshakeMessageId).has_value());
    EXPECT_FALSE(kill_switch::CheckMessageId(ScenePlayerExitGameMessageId).has_value());
    EXPECT_FALSE(kill_switch::CheckMessageId(SceneProcessClientPlayerMessageMessageId).has_value());
    EXPECT_FALSE(kill_switch::CheckMessageId(SceneNodeGrpcAssetAbortDebitMessageId).has_value());
    EXPECT_FALSE(kill_switch::CheckGrpcMethod("/scene_node.SceneNodeGrpc/ReleasePlayer").has_value());
    EXPECT_TRUE(kill_switch::CheckMessageId(SceneCreateSceneMessageId).has_value());
    EXPECT_TRUE(kill_switch::CheckGrpcMethod("/scene_node.SceneNodeGrpc/CreateScene").has_value());

    // 精确到方法的规则仍然关得掉它们(例如补偿调用本身在打爆下游)。
    KillSwitch::Process().ReplaceAll({{"SceneNodeGrpc/AssetAbortDebit", "true"}});
    EXPECT_TRUE(kill_switch::CheckMessageId(SceneNodeGrpcAssetAbortDebitMessageId).has_value());
    EXPECT_TRUE(kill_switch::CheckGrpcMethod("/scene_node.SceneNodeGrpc/AssetAbortDebit").has_value());
}

TEST_F(ProcessKillSwitch, CheckMessageIdMatchesGoStyleNames)
{
    // gRPC 方法按 proto 声明的带包名全名判,与 Go 侧 info.FullMethod 一致:两种写法都命中。
    KillSwitch::Process().ReplaceAll({{"chatpb.ClientPlayerChat/SendChat", "true"}});
    EXPECT_TRUE(kill_switch::CheckMessageId(ClientPlayerChatSendChatMessageId).has_value());
    KillSwitch::Process().ReplaceAll({{"ClientPlayerChat/SendChat", "true"}});
    EXPECT_TRUE(kill_switch::CheckMessageId(ClientPlayerChatSendChatMessageId).has_value());
    // RPC0 方法没有包名。
    KillSwitch::Process().ReplaceAll({{"Scene/CreateScene", "true"}});
    EXPECT_TRUE(kill_switch::CheckMessageId(SceneCreateSceneMessageId).has_value());
    EXPECT_FALSE(kill_switch::CheckMessageId(ClientPlayerChatSendChatMessageId).has_value());
}

TEST_F(ProcessKillSwitch, OutOfRangeMessageIdIsAllowed)
{
    KillSwitch::Process().ReplaceAll({{"*", "true"}});
    EXPECT_FALSE(kill_switch::CheckMessageId(0xFFFFFFFFu).has_value());
}

TEST_F(ProcessKillSwitch, BlockedRequestsAreCountedByShortName)
{
    const auto before = KillSwitch::Process().BlockedCounts();
    const auto countOf = [](const std::map<std::string, uint64_t> &counts, const std::string &key)
    {
        const auto found = counts.find(key);
        return found == counts.end() ? uint64_t{0} : found->second;
    };
    KillSwitch::Process().ReplaceAll({{"ClientPlayerChat/*", "true"}});
    EXPECT_TRUE(kill_switch::CheckGrpcMethod("/chatpb.ClientPlayerChat/SendChat").has_value());
    EXPECT_TRUE(kill_switch::CheckMessageId(ClientPlayerChatSendChatMessageId).has_value());
    const auto after = KillSwitch::Process().BlockedCounts();
    EXPECT_EQ(countOf(after, "ClientPlayerChat/SendChat") - countOf(before, "ClientPlayerChat/SendChat"), 2u);
}

TEST(KillSwitchDenyMessage, CarriesMethodAndReason)
{
    Rule rule;
    rule.deny = true;
    EXPECT_EQ(kill_switch::DenyMessage("/BattleNode/CreateBattle", rule),
              "killswitch: method /BattleNode/CreateBattle is disabled");
    rule.reason = "battle crash loop";
    EXPECT_EQ(kill_switch::DenyMessage("/BattleNode/CreateBattle", rule),
              "killswitch: method /BattleNode/CreateBattle is disabled, reason: battle crash loop");
}

// ── etcd 同步(传输替身) ───────────────────────────────────────────────

class FakeEtcdTransport final : public EtcdTransport
{
public:
    const char *Name() const override { return "fake"; }
    void Range(const etcdserverpb::RangeRequest &request) override { ranges.push_back(request); }
    void Put(const etcdserverpb::PutRequest &) override { ++unexpectedWrites; }
    void DeleteRange(const etcdserverpb::DeleteRangeRequest &) override { ++unexpectedWrites; }
    void Txn(const etcdserverpb::TxnRequest &, EtcdRegistrationCompletion<etcdserverpb::TxnResponse> done) override
    {
        ++unexpectedWrites;
        done(std::nullopt);
    }
    void LeaseGrant(const etcdserverpb::LeaseGrantRequest &,
                    EtcdRegistrationCompletion<etcdserverpb::LeaseGrantResponse> done) override
    {
        ++unexpectedWrites;
        done(std::nullopt);
    }
    void LeaseRevoke(const etcdserverpb::LeaseRevokeRequest &) override { ++unexpectedWrites; }
    void WatchWrite(const etcdserverpb::WatchRequest &request) override { watches.push_back(request); }
    void KeepAliveWrite(const etcdserverpb::LeaseKeepAliveRequest &) override { ++unexpectedWrites; }
    void Shutdown() override { *shutdown = true; }

    std::vector<etcdserverpb::RangeRequest> ranges;
    std::vector<etcdserverpb::WatchRequest> watches;
    int unexpectedWrites{0};
    // 传输随 Shutdown 被释放,用共享标记在释放之后观察。
    std::shared_ptr<bool> shutdown = std::make_shared<bool>(false);
};

etcdserverpb::RangeResponse RangeReply(int64_t revision, const std::map<std::string, std::string> &relativeKeys)
{
    etcdserverpb::RangeResponse reply;
    reply.mutable_header()->set_revision(revision);
    for (const auto &[key, value] : relativeKeys)
    {
        auto *kv = reply.add_kvs();
        kv->set_key(std::string(kPrefix) + key);
        kv->set_value(value);
        kv->set_mod_revision(revision);
    }
    return reply;
}

etcdserverpb::WatchResponse Created(int64_t watchId)
{
    etcdserverpb::WatchResponse response;
    response.set_watch_id(watchId);
    response.set_created(true);
    return response;
}

etcdserverpb::WatchResponse Event(int64_t watchId, mvccpb::Event_EventType type, const std::string &relativeKey,
                                  const std::string &value, int64_t modRevision)
{
    etcdserverpb::WatchResponse response;
    response.set_watch_id(watchId);
    response.mutable_header()->set_revision(modRevision);
    auto *event = response.add_events();
    event->set_type(type);
    event->mutable_kv()->set_key(std::string(kPrefix) + relativeKey);
    event->mutable_kv()->set_value(value);
    event->mutable_kv()->set_mod_revision(modRevision);
    return response;
}

class KillSwitchEtcdSyncTest : public ::testing::Test
{
protected:
    void SetUp() override
    {
        auto transport = std::make_unique<FakeEtcdTransport>();
        transport_ = transport.get();
        sync_.StartWithTransport(std::move(transport));
    }

    KillSwitch ks_;
    KillSwitchEtcdSync sync_{ks_, kPrefix};
    FakeEtcdTransport *transport_{nullptr};
};

TEST_F(KillSwitchEtcdSyncTest, InitialRangeAppliesRulesAndCreatesWatchFromNextRevision)
{
    ASSERT_EQ(transport_->ranges.size(), 1u);
    EXPECT_EQ(transport_->ranges[0].key(), kPrefix);
    EXPECT_EQ(transport_->ranges[0].range_end(), "/mmorpg/killswitch0");

    sync_.OnRange(&transport_->ranges[0], RangeReply(10, {{"ClientPlayerChat/SendChat", "true"}}));
    EXPECT_TRUE(ks_.Check(kSendChat).has_value());
    EXPECT_EQ(sync_.AppliedThroughRevision(), 10);
    ASSERT_EQ(transport_->watches.size(), 1u);
    EXPECT_EQ(transport_->watches[0].create_request().key(), kPrefix);
    EXPECT_EQ(transport_->watches[0].create_request().start_revision(), 11);
    EXPECT_EQ(transport_->unexpectedWrites, 0) << "the kill switch connection only reads";

    sync_.OnWatch(Created(0));
    EXPECT_TRUE(sync_.WatchEstablished());
}

TEST_F(KillSwitchEtcdSyncTest, WatchEventsApplyAndAnOlderRangeDoesNotRollBack)
{
    sync_.OnRange(&transport_->ranges[0], RangeReply(10, {{"ClientPlayerChat/SendChat", "true"}}));
    sync_.OnWatch(Created(0));

    sync_.OnWatch(Event(0, mvccpb::Event_EventType::Event_EventType_DELETE, "ClientPlayerChat/SendChat", "", 12));
    EXPECT_FALSE(ks_.Check(kSendChat).has_value());
    EXPECT_EQ(sync_.AppliedThroughRevision(), 12);

    // 定期复核的 Range 在 revision 11 读到的旧快照晚到:不能把已删的规则加回来。
    sync_.OnRange(&transport_->ranges[0], RangeReply(11, {{"ClientPlayerChat/SendChat", "true"}}));
    EXPECT_FALSE(ks_.Check(kSendChat).has_value());
    EXPECT_EQ(sync_.AppliedThroughRevision(), 12);

    // 已经被更新快照覆盖的 watch 事件同样跳过。
    sync_.OnRange(&transport_->ranges[0], RangeReply(20, {}));
    sync_.OnWatch(Event(0, mvccpb::Event_EventType::Event_EventType_PUT, "ClientPlayerChat/SendChat", "true", 15));
    EXPECT_FALSE(ks_.Check(kSendChat).has_value());

    sync_.OnWatch(Event(0, mvccpb::Event_EventType::Event_EventType_PUT, "ClientPlayerChat/SendChat", "true", 21));
    EXPECT_TRUE(ks_.Check(kSendChat).has_value());
}

TEST_F(KillSwitchEtcdSyncTest, CompactionTriggersAFullRangeAndANewWatch)
{
    sync_.OnRange(&transport_->ranges[0], RangeReply(10, {}));
    sync_.OnWatch(Created(0));

    etcdserverpb::WatchResponse canceled;
    canceled.set_watch_id(0);
    canceled.set_canceled(true);
    canceled.set_compact_revision(50);
    sync_.OnWatch(canceled);
    EXPECT_FALSE(sync_.WatchEstablished());
    ASSERT_EQ(transport_->ranges.size(), 2u) << "compaction re-ranges immediately";

    sync_.OnRange(&transport_->ranges[1], RangeReply(60, {{"*", "true"}}));
    EXPECT_TRUE(ks_.Check(kSendChat).has_value());
    ASSERT_EQ(transport_->watches.size(), 2u);
    EXPECT_EQ(transport_->watches[1].create_request().start_revision(), 61);
}

TEST_F(KillSwitchEtcdSyncTest, StreamRestartReRangesAndRecreatesTheWatchAfterAppliedState)
{
    sync_.OnRange(&transport_->ranges[0], RangeReply(10, {}));
    sync_.OnWatch(Created(0));
    sync_.OnWatch(Event(0, mvccpb::Event_EventType::Event_EventType_PUT, "ClientPlayerChat/SendChat", "true", 14));

    sync_.OnWatchStreamRestarted();
    EXPECT_FALSE(sync_.WatchEstablished());
    ASSERT_EQ(transport_->ranges.size(), 2u);

    // 新流上第一份 Range 比 watch 已经应用到的旧:不替换,从已应用状态之后建 watch。
    sync_.OnRange(&transport_->ranges[1], RangeReply(13, {}));
    EXPECT_TRUE(ks_.Check(kSendChat).has_value());
    ASSERT_EQ(transport_->watches.size(), 2u);
    EXPECT_EQ(transport_->watches[1].create_request().start_revision(), 15);

    // 旧流上 watch 的残留应答不再生效。
    sync_.OnWatch(Event(0, mvccpb::Event_EventType::Event_EventType_DELETE, "ClientPlayerChat/SendChat", "", 16));
    EXPECT_TRUE(ks_.Check(kSendChat).has_value());
}

TEST_F(KillSwitchEtcdSyncTest, ShutdownStopsTheTransportAndIgnoresLateReplies)
{
    const auto shutdownObserved = transport_->shutdown;
    sync_.Shutdown();
    transport_ = nullptr; // 已随 Shutdown 释放
    EXPECT_TRUE(*shutdownObserved);
    sync_.OnRange(nullptr, RangeReply(10, {{"*", "true"}}));
    EXPECT_FALSE(ks_.Check(kSendChat).has_value());
    sync_.Shutdown(); // 幂等
}
} // namespace
